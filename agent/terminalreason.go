package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Autumn-27/norma/harness"
)

// runTrace retains the latest tool call so an interrupted run can identify the
// operation that was still in flight.
type runTrace struct {
	startedAt time.Time
	id        string
	name      string
	input     string
	at        time.Time
	pending   bool
}

func (t *runTrace) start(id, name, input string) {
	t.id, t.name, t.input, t.at, t.pending = id, name, input, time.Now(), true
}

func (t *runTrace) done(id string) {
	if id == t.id {
		t.pending = false
	}
}

var reasonHint = map[harness.TerminalReason]string{
	harness.ReasonCompleted:         "모델이 이번 턴을 정상적으로 마쳤지만 텍스트 요약을 남기지 않았습니다. 사실과 자산은 이번 턴의 도구 호출 기록을 기준으로 봅니다",
	harness.ReasonMaxTurns:          "단계 수 상한(MaxTurns)에 도달했습니다. SDK가 마무리를 수행해 사실과 자산을 기록했고, 의도는 exhausted로 표시되어 플래너가 방향을 바꿔 계속 진행할 수 있습니다. 실패로 처리하지 않습니다",
	harness.ReasonTimeout:           "단일 실행의 벽시계 예산(MaxDuration)에 도달했습니다. 시간이 되면 실행 중인 도구를 중단하고 그 자리에서 마무리를 수행해 확인된 사실과 자산을 기록하며, 의도는 exhausted로 표시됩니다",
	harness.ReasonModelError:        "모델 또는 API 호출에 실패했습니다(네트워크, 인증, 레이트 리밋, 공급자 5xx 등). 재시도가 소진되어 의도는 blocked로 표시됩니다. 전송 계층 장애로 이 의도는 사실상 탐색되지 않았으므로, 실행 과정(get_worker_trace)을 확인한 뒤 재배정할지 방법을 바꿀지 결정하세요",
	harness.ReasonBlockingLimit:     "컨텍스트 길이가 하드 상한에 도달해 요청이 전송 전에 차단되었습니다. 의도 단위를 더 잘게 나누거나 도구 반환을 압축해야 합니다",
	harness.ReasonPromptTooLong:     "프롬프트가 너무 길고 컨텍스트 압축 재시도도 모두 소진되어 실행을 계속할 수 없습니다",
	harness.ReasonImageError:        "현재 모델은 이번 턴의 멀티모달 콘텐츠를 지원하지 않습니다. 비전을 지원하는 모델로 바꾸거나 도구가 이미지를 반환하지 않도록 하세요",
	harness.ReasonStopHookPrevented: "Stop 훅이 이번 턴의 종료를 막았지만 이후 계속 진행하지 못했습니다. 태스크 Guard 규칙이 지나치게 엄격한지 확인하세요",
	harness.ReasonHookStopped:       "도구 또는 훅이 범위를 벗어난 목표나 비활성화된 명령 등으로 실행을 능동적으로 중단했습니다. 마지막 tool_result의 차단 설명을 확인하세요",
	harness.ReasonAbortedStreaming:  "모델 출력이 스트리밍 생성 단계에 있을 때 실행이 취소되었습니다",
	harness.ReasonAbortedTools:      "도구 실행 단계에서 실행이 취소되었습니다",
}

// terminalText renders a terminal event with no final text into a compact summary
// and a Markdown detail block.
func terminalText(ctx context.Context, term *harness.Terminal, tr *runTrace) (string, string) {
	reason := term.Reason
	aborted := reason == harness.ReasonAbortedStreaming || reason == harness.ReasonAbortedTools
	// Prompt may return ctx.Err directly without a terminal event. Preserve the
	// cancellation cause instead of falling back to an empty/unknown terminal reason.
	if reason == "" && ctx.Err() != nil {
		aborted = true
	}

	var sum string
	if aborted {
		_, short, _, ok := AbortReason(ctx)
		if !ok {
			short = "취소 원인을 확인하지 못함"
		}
		stage := "실행 과정"
		switch reason {
		case harness.ReasonAbortedStreaming:
			stage = "모델 출력 단계"
		case harness.ReasonAbortedTools:
			stage = "도구 실행 단계"
		}
		sum = "（실행이 중단됨: " + short + "; " + stage + "에서 멈춰 미완료" + progressSuffix(term, tr) + "）"
	} else if reason == harness.ReasonMaxTurns || reason == harness.ReasonTimeout {
		sum = "（실행 예산 상한 도달(" + string(reason) + "), 마무리하며 사실 기록 완료" + progressSuffix(term, tr) + "; 이번에는 텍스트 요약 없음）"
	} else {
		hint := terminalReasonHint(reason)
		sum = "（텍스트 요약 없음, 종료 상태 " + terminalReasonLabel(reason) + ": " + firstLine(hint, 80) + "）"
	}

	var b strings.Builder
	b.WriteString(sum)
	b.WriteString("\n\n")
	displayReason := terminalReasonLabel(reason)
	fmt.Fprintf(&b, "- **종료 상태**: `%s` - %s\n", displayReason, terminalReasonHint(reason))
	if aborted {
		code, _, why, ok := AbortReason(ctx)
		if ok {
			fmt.Fprintf(&b, "- **중단 원인** (`%s`): %s\n", code, why)
		} else {
			b.WriteString("- **중단 원인**: 확인할 수 없음. 취소한 쪽이 context.WithCancelCause로 이름 있는 원인을 붙이지 않았을 수 있습니다\n")
		}
	}
	if term.Err != nil {
		fmt.Fprintf(&b, "- **하위 오류**: `%v`\n", term.Err)
	}
	if aborted && strings.TrimSpace(term.Text) != "" {
		b.WriteString("- **취소 전에 생성된 부분 출력**:\n\n")
		b.WriteString(term.Text)
		b.WriteString("\n\n")
	}
	if term.Turns > 0 {
		fmt.Fprintf(&b, "- **모델 턴 수**: %d 턴\n", term.Turns)
	}
	if !tr.startedAt.IsZero() {
		fmt.Fprintf(&b, "- **이번 실행 소요 시간**: %s\n", roundDur(time.Since(tr.startedAt)))
	}
	if u := term.Usage; u.InputTokens+u.OutputTokens+u.CacheReadTokens+u.CacheWriteTokens > 0 {
		fmt.Fprintf(&b, "- **누적 token**: 입력 %d / 출력 %d / 캐시 읽기 %d / 캐시 쓰기 %d\n",
			u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens)
	}
	if tr.name == "" {
		b.WriteString("- **도구 호출**: 이번 실행은 도구를 호출하기 전에 종료되었습니다\n")
	} else if tr.pending {
		fmt.Fprintf(&b, "- **중단 시 실행 중이던 도구**: `%s` (%s 동안 실행됨, **결과 미반환**)\n\n  ```json\n  %s\n  ```\n",
			tr.name, roundDur(time.Since(tr.at)), firstLine(tr.input, 300))
	} else {
		fmt.Fprintf(&b, "- **중단 직전 마지막 도구**: `%s` (정상 반환됨)\n", tr.name)
	}
	return sum, b.String()
}

func terminalReasonLabel(reason harness.TerminalReason) string {
	if reason == "" {
		return "context_canceled"
	}
	return string(reason)
}

func terminalReasonHint(reason harness.TerminalReason) string {
	if hint := reasonHint[reason]; hint != "" {
		return hint
	}
	if reason == "" {
		return "실행의 context는 취소되었지만 하위 계층에서 Terminal 이벤트가 발생하지 않았습니다"
	}
	return "알 수 없는 종료 상태입니다. harness에 TerminalReason이 새로 추가되었을 수 있으니 reasonHint를 보완하세요"
}

func progressSuffix(term *harness.Terminal, tr *runTrace) string {
	var parts []string
	if term.Turns > 0 {
		parts = append(parts, fmt.Sprintf("%d 턴", term.Turns))
	}
	if !tr.startedAt.IsZero() {
		parts = append(parts, roundDur(time.Since(tr.startedAt)))
	}
	if len(parts) == 0 {
		return ""
	}
	return ", 실행됨: " + strings.Join(parts, " / ")
}

func roundDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return d.Round(100 * time.Millisecond).String()
	case d < time.Hour:
		return d.Round(time.Second).String()
	default:
		return d.Round(time.Minute).String()
	}
}
