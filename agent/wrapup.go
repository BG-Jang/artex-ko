package agent

import (
	"strings"

	"github.com/Autumn-27/norma/harness"
)

// 마무리 프롬프트(wrap-up / settlement prompt): agent 가 【턴 수 소진(MaxTurns)】 또는
// 【시간 초과(run_seconds/MaxDuration)】로 종료될 때 SDK 의 settlement 단계에서 이 프롬프트가 주입되어,
// agent 가 식별했지만 아직 기록하지 않은 내용을 먼저 저장하고 한 문장 요약을 내도록 하여 흐지부지 끝나지 않게 한다.
//
// 각 agent 의 마무리 프롬프트는 백엔드에서 필요에 따라 덮어쓸 수 있으며(agents.wrapup_prompt 에 저장), 비워 두면 여기의
// 내장 기본값을 쓴다. 【프롬프트 본문】만 편집할 수 있으며, 어떤 도구를 비활성화할지와 마무리 단계 자체의 턴 예산은 코드에 고정된 정책이다.

// WrapupOverride, if set, returns the stored wrap-up prompt for an agent key and
// whether a non-empty one exists. Wired by the server to the agents table (like
// PromptOverride for system prompts). nil / empty → the built-in default is used.
var WrapupOverride func(agentKey string) (string, bool)

// WrapupMaxTurnsOverride, if set, returns the admin-configured turn budget for the
// wrap-up phase of an agent and whether a positive one exists. Wired to the agents
// table. nil / ≤0 → the built-in per-agent default (wrapupTurnDefaults) is used.
var WrapupMaxTurnsOverride func(agentKey string) (int, bool)

// 내장 기본 마무리 프롬프트이며 agent key 로 찾는다. worker 는 과거에 하드코딩되어 있던 settleWrapUpPrompt 를 재사용하고
// (worker.go 에 정의됨), planner/mainagent 는 각각 한 벌씩 가진다. 매칭되지 않는(커스텀 agent) 것은 공통 기본값을 쓴다.
var wrapupDefaults = map[string]string{
	"worker":    settleWrapUpPrompt,
	"planner":   plannerWrapUpDefault,
	"mainagent": mainAgentWrapUpDefault,
}

// wrapupTurnDefaults: 각 agent 마무리 단계【자체】의 턴 예산 내장 기본값(백엔드에서 0 보다 큰 값으로 덮어쓸 수 있음).
// 모두 10 턴을 주어 마무리 단계에서 저장할 충분한 단계를 보장한다. 매칭되지 않으면 genericWrapupTurns 를 쓴다.
var wrapupTurnDefaults = map[string]int{
	"worker":    10,
	"planner":   10,
	"mainagent": 10,
}

const genericWrapupTurns = 10

const plannerWrapUpDefault = "이번 계획 라운드의 단계 예산이 곧 소진됩니다. 다만 【이번 라운드】만 끝나는 것이며, 시스템은 이후에도 상황 변화에 따라 당신을 다시 깨워 계획을 이어가게 합니다. 작업 자체가 끝나는 것이 아니므로 여기서 전체 계획을 마무리 지을 필요는 없습니다. 이번 라운드에서 이미 명확히 판단한 결론은 실제로 반영하여 이번 라운드가 헛되지 않게 하되, 【마무리를 위해 억지로 의도를 지어내지는】 마십시오(이번 라운드에 의도가 0개인 것도 완전히 정상적인 결과입니다). (1) 【지금 바로 파견해야 할】 탐색 방향을 이미 판단했다면 add_intent 한 번으로 묶어서 제출합니다(이미 정한 것은 묵히지 말고 바로 보냅니다). (2) 어떤 발견이나 사실로 이미 달성이 증명된 목표는 prove_goal 로 met 표시합니다(빠뜨리지 마십시오). (3) 단계를 나눠야 하는 직렬 익스플로잇 체인을 식별했다면 TodoWrite 로 기록하여 다음에 깨어났을 때 이어서 파견할 수 있게 합니다. 다 끝냈으면 이번 라운드를 바로 종료하며, 요약 텍스트는 출력하지 않습니다."

const mainAgentWrapUpDefault = "단계 예산이 곧 소진되어 이번 상호작용이 끝나려 합니다. 더 이상 새로운 탐색이나 조작을 시작하지 마십시오. **한 문장짜리 순수 텍스트로만** 현재 진행 상황, 핵심 결론, 그리고 권장하는 다음 단계를 사용자에게 한국어로 요약하십시오."

const genericWrapUpDefault = "예산 소진으로 곧 종료됩니다. 먼저 완료했지만 아직 저장하지 않은 결과를 기록한 뒤, **한 문장짜리 순수 텍스트로만** 무엇을 했고 어떤 핵심 결론을 얻었는지 한국어로 요약하십시오(이 한 문장이 이번 실행의 결과로 표시됩니다)."

// WrapupDefault returns the built-in default wrap-up prompt for an agent key —
// used by the admin UI as the "restore default" value and empty-field placeholder.
func WrapupDefault(agentKey string) string {
	if d, ok := wrapupDefaults[agentKey]; ok {
		return d
	}
	return genericWrapUpDefault
}

// WrapupTurnsDefault returns the built-in wrap-up turn budget for an agent key —
// used by the admin UI as the "0 = default N" hint.
func WrapupTurnsDefault(agentKey string) int {
	if n, ok := wrapupTurnDefaults[agentKey]; ok {
		return n
	}
	return genericWrapupTurns
}

// resolveWrapup returns the effective wrap-up prompt: the DB override (if set and
// non-empty) over the built-in default.
func resolveWrapup(agentKey string) string {
	if WrapupOverride != nil {
		if t, ok := WrapupOverride(agentKey); ok && strings.TrimSpace(t) != "" {
			return t
		}
	}
	return WrapupDefault(agentKey)
}

// resolveWrapupTurns returns the effective wrap-up turn budget: a positive DB
// override over the built-in per-agent default.
func resolveWrapupTurns(agentKey string) int {
	if WrapupMaxTurnsOverride != nil {
		if v, ok := WrapupMaxTurnsOverride(agentKey); ok && v > 0 {
			return v
		}
	}
	return WrapupTurnsDefault(agentKey)
}

// wrapupSettlement builds the settlement config for an agent's run. Prompt and the
// turn budget are admin-editable per agent; disabled tools are code-owned policy so
// a user can't edit away the "stop probing" guardrail. Resolved fresh each run
// (reads DB live), so edits apply on the next run without a restart.
func wrapupSettlement(agentKey string, disabledTools []string) *harness.Settlement {
	return &harness.Settlement{
		Prompt:        withWrapupLang(resolveWrapup(agentKey)),
		DisabledTools: disabledTools,
		MaxTurns:      resolveWrapupTurns(agentKey),
	}
}

// withWrapupLang 은 마무리 프롬프트 뒤에 출력 언어 지시(langDirective)를 덧붙인다.
// 마무리 프롬프트는 시스템 프롬프트와 달리 langDirective 꼬리가 자동으로 붙지 않으므로,
// 이렇게 직접 붙여야 사용자에게 보이는 마지막 한 문장 요약이 선택한 표시 언어를 따른다.
// 마무리 산문 자체는 모델이 읽는 운영 지시문(한국어 유지)이고, 이 꼬리가 최고 우선순위로
// 사용자 노출 텍스트의 언어만 강제한다. 요약을 내지 않는 planner 계열에는 무해하다.
// 기본 언어 ko 에서는 꼬리를 붙이지 않는다. 시스템 프롬프트의 langDirective 가 이미
// 한국어 출력을 강제하고 있어, 기본 경로의 모델 입력을 이전 버전과 같게 둔다(벤치마크 드리프트 방지).
func withWrapupLang(prompt string) string {
	if strings.TrimSpace(prompt) == "" {
		return prompt
	}
	lang := resolveOutputLang()
	if lang == "ko" {
		return prompt
	}
	return prompt + langDirective(lang)
}

// ---------- 작업 단위 시간 초과 마무리 문구(docs/任务级超时与收尾设计.md 참고)----------
//
// per-run 마무리 문구와는【별개 두 벌】이다: per-run 은 "이번 run 의 예산이 다 떨어졌다"이고, 작업 시간 초과는
// "작업 전체 시한이 도달해 곧 끝난다"이다. 의미가 종종 정반대다(특히 planner: per-run 은 "멈추지 말고 계속 계획하라"고 하고,
// 작업 시간 초과는 "시한이 되었으니 계획을 멈추고 최종 판정을 하라"고 한다). worker/planner 에만 설정한다.

// WrapupTaskTimeoutOverride / …TurnsOverride: 작업 시간 초과 마무리 문구와 턴 수의 DB 덮어쓰기
// (agents.task_timeout_wrapup_prompt / _max_turns 에 연결, worker/planner 만 해당).
var (
	WrapupTaskTimeoutOverride      func(agentKey string) (string, bool)
	WrapupTaskTimeoutTurnsOverride func(agentKey string) (int, bool)
)

var taskTimeoutWrapupDefaults = map[string]string{
	"worker":  workerTaskTimeoutDefault,
	"planner": plannerTaskTimeoutDefault,
}

const workerTaskTimeoutDefault = "**전체 작업이 시간 제한 상한에 도달하여 곧 종료됩니다**(이번 run 의 예산이 아니라 탐색 전체가 끝나는 시점입니다). 이것이 마지막 기회입니다. (1) 이미 식별했지만 아직 기록하지 않은 내용을 【전부】 저장합니다. 새 자산은 insert_assets, 탐색 결론과 사실은 record_fact, 확인된 취약점은 report_finding 으로 기록합니다. (2) 더 이상 어떤 새 명령이나 탐지도 시작하지 마십시오. (3) **맨 마지막에 한 문장짜리 순수 텍스트로만** 이번 의도에서 얻은 핵심 결론을 한국어로 요약하십시오."

const plannerTaskTimeoutDefault = "**전체 작업이 시간 제한 상한에 도달하여 곧 종료됩니다**(이번 라운드가 아니라 작업 전체가 종료됩니다). 현재까지의 【모든】 사실과 발견을 근거로 마지막 목표 판정을 수행하십시오. 증거로 달성이 증명된 목표는 prove_goal 로 met 표시합니다(빠뜨리지 마십시오). **더 이상 어떤 새 의도도 생성하지 마십시오**(지금 의도를 파견해도 더 이상 실행되지 않습니다). 판정을 마치면 바로 종료하며, 요약 텍스트는 출력하지 않습니다."

// TaskTimeoutWrapupDefault 는 특정 agent 의 작업 시간 초과 내장 기본 마무리 문구를 반환한다(백엔드 플레이스홀더/기본값 복원용).
func TaskTimeoutWrapupDefault(agentKey string) string {
	return taskTimeoutWrapupDefaults[agentKey] // 미설정(mainagent/chat)이면 빈 문자열 반환
}

// resolveTaskTimeoutWrapup: DB 덮어쓰기(비어 있지 않음) > 내장 기본값. 빈 문자열은 해당 agent 에 작업 시간 초과 문구가 없음을 뜻한다
// (worker/planner 가 아님). 이때 호출자는 per-run 문구로 되돌아가야 한다.
func resolveTaskTimeoutWrapup(agentKey string) string {
	if WrapupTaskTimeoutOverride != nil {
		if t, ok := WrapupTaskTimeoutOverride(agentKey); ok && strings.TrimSpace(t) != "" {
			return t
		}
	}
	return TaskTimeoutWrapupDefault(agentKey)
}

func resolveTaskTimeoutTurns(agentKey string) int {
	if WrapupTaskTimeoutTurnsOverride != nil {
		if v, ok := WrapupTaskTimeoutTurnsOverride(agentKey); ok && v > 0 {
			return v
		}
	}
	return resolveWrapupTurns(agentKey) // 기본값은 per-run 턴 수를 그대로 사용
}

// wrapupSettlementForTask builds settlement for a worker/planner run that is aware
// of the task deadline. See §5 of the design doc:
//   - clamped=true  → 이번 run 이 작업 deadline 에 조여짐: Timeout 마무리 = 작업 시한 도달 → 작업 시간 초과 문구;
//     MaxTurns 마무리 = 조임 창 안에서 턴이 먼저 소진, 작업이 몇 분 남음 → per-run 문구로 되돌아감.
//   - clamped=false → 작업 시한이 아직 멀다: 두 reason 모두 per-run 문구를 쓴다(즉 wrapupSettlement 와 같아짐).
//
// harness 의 PromptByReason 은 마무리 시 【실제】 reason 에 따라 그 자리에서 고르므로, build 시점에 어긋나지 않는다.
func wrapupSettlementForTask(agentKey string, disabledTools []string, clamped bool) *harness.Settlement {
	perRun := withWrapupLang(resolveWrapup(agentKey))
	st := &harness.Settlement{
		Prompt:        perRun, // 폴백(또한 clamped 가 아닐 때 두 reason 의 값)
		DisabledTools: disabledTools,
		MaxTurns:      resolveWrapupTurns(agentKey),
	}
	if clamped {
		if tt := resolveTaskTimeoutWrapup(agentKey); tt != "" {
			st.PromptByReason = map[harness.TerminalReason]string{
				harness.ReasonTimeout:  withWrapupLang(tt), // 작업 시한 도달
				harness.ReasonMaxTurns: perRun,             // 턴 수가 먼저 소진, 작업 시간은 남아 있음
			}
			st.MaxTurns = resolveTaskTimeoutTurns(agentKey)
		}
	}
	return st
}
