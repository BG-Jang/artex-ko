package agent

import (
	"context"
	"errors"
	"fmt"
)

// AbortCause names why an agent run's context was cancelled. Every cancellation
// site should attach one so the activity trace can report the real initiator.
type AbortCause struct {
	Code  string
	Short string
	Text  string
}

func (c *AbortCause) Error() string { return c.Text }

func cause(code, short, text string) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: text}
}

// Causef builds a cause that includes runtime-specific detail.
func Causef(code, short, format string, args ...any) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: fmt.Sprintf(format, args...)}
}

var (
	// Task-level execution context.
	AbortPausedByUser = cause("paused_by_user", "사용자가 태스크를 일시정지함",
		"사용자가 태스크 제어 인터페이스(POST /api/tasks/{id}/control, action=pause)로 태스크를 일시정지했습니다. 이번 Planner/Worker 실행은 능동적으로 취소되며, 실행 중이던 의도는 frontier(open)로 되돌아갑니다. 태스크를 재개하면 해당 의도를 다시 가져와 처음부터 실행합니다")
	AbortPausedByOrchestrator = cause("paused_by_orchestrator", "오케스트레이션 Agent가 태스크를 일시정지함",
		"오케스트레이션 Agent가 pause_task 도구를 호출해 이 태스크를 일시정지했습니다. 이번 Planner/Worker 실행은 능동적으로 취소되며, 실행 중이던 의도는 frontier(open)로 되돌아갑니다. 재개 후 다시 처음부터 실행합니다")
	AbortTaskDeleted = cause("task_deleted", "태스크가 삭제됨",
		"태스크를 삭제하는 중입니다(DELETE /api/tasks/{id}). 삭제 장벽이 이 태스크에서 실행 중이던 Planner, Worker, 메인 Agent를 취소했으므로 이번 실행 결과는 더 이상 사용되지 않습니다")
	AbortPausedOnReload = cause("paused_on_reload", "백엔드가 태스크의 일시정지 상태를 복원함",
		"백엔드 시작 시 데이터베이스에 저장된 상태에 따라 태스크의 일시정지가 복원되었습니다. 이번 실행은 취소되었습니다. 정상적인 경우 복원 단계에서는 실행 중인 Agent가 없습니다")
	AbortGoalMet = cause("goal_met", "플래너가 태스크 목표 달성을 판정함",
		"플래너가 태스크 목표가 달성되었다고 판정해 태스크를 done으로 전환했고, 이어서 아직 실행 중인 Worker를 취소했습니다. 이 의도들은 실패가 아니라 stopped로 표시됩니다")
	AbortSettleDrainTimeout = cause("settle_drain_timeout", "태스크 시간 초과 마무리 대기 시간이 소진됨",
		"태스크가 timeout에 도달한 뒤 실행 중인 Worker의 정상 마무리를 기다렸지만 90초 drain 유예 시간으로는 부족해 강제 취소를 수행했습니다. 의도는 exhausted로 표시되며, 마무리 단계에서 이미 기록된 사실과 자산은 유지됩니다")

	// Per-work context.
	AbortKilledByPlanner = cause("killed_by_planner", "플래너가 이 의도를 종료함",
		"플래너가 kill_work를 호출해 이 의도를 능동적으로 종료했습니다. 보통 방향이 빗나갔거나 더 진행할 가치가 없다는 뜻입니다. 의도는 stopped로 표시되며 자동으로 다시 가져오지 않습니다")
	AbortWorkPausedByUser = cause("work_paused_by_user", "사용자가 이 Worker 의도를 일시정지함",
		"사용자가 실행 중이던 Worker를 일시정지했습니다. 이번 호출은 취소되었고 의도는 paused 상태로 바뀝니다. 이미 등록된 의도, 사실, 취약점, 활동 기록은 모두 유지되며, 재개하면 처음부터 다시 실행합니다")
	AbortWorkCancelledByUser = cause("work_cancelled_by_user", "사용자가 이 Worker 의도를 삭제함",
		"사용자가 실행 중이던 Worker를 삭제했습니다. 이번 호출은 취소되었습니다. Worker가 쓰기 구간을 빠져나온 뒤 서버는 사용자가 선택한 삭제 모드에 따라 이 의도를 처리합니다. 소프트 삭제는 삭제 표시만 하고 모든 산출물을 유지하며, 하드 삭제는 이 의도와 이에만 의존하는 하위 노드를 연쇄적으로 제거합니다")
	AbortWorkFinished = cause("work_finished", "Worker가 정상 종료되어 context를 해제함",
		"Worker가 정상 종료되어 엔진이 detachWork에서 해당 context 리소스를 해제했습니다. 이는 실행 중단이 아닙니다. 중단 메시지에 나타난 경우에는 취소와 종료 이벤트가 경합한 것입니다")
	AbortPausedRaceGuard = cause("paused_race_guard", "태스크 일시정지 중 새 실행 시작을 거부함",
		"태스크가 일시정지 상태일 때 엔진이 새 실행 context 발급을 거부했습니다. claim과 일시정지 사이의 경합으로 Worker가 계속 시작되는 것을 막기 위한 조치이며, 이미 claim된 의도는 frontier로 되돌아갑니다")

	// Main Agent and standalone conversation contexts.
	AbortChatStoppedByUser = cause("chat_stopped_by_user", "사용자가 이번 대화를 중지함",
		"사용자가 중지 버튼을 눌러 이번 메인 Agent 또는 세션 Agent 실행을 중단했습니다. 이미 생성된 활동 기록은 유지되며, 다음 메시지를 계속 보낼 수 있습니다")
	AbortChatPausedWithTask = cause("chat_paused_with_task", "태스크 일시정지로 메인 Agent 대화가 중단됨",
		"사용자가 태스크를 일시정지할 때 실행 중이던 메인 Agent 대화도 함께 취소되었습니다. 이미 생성된 활동 기록은 유지되며, 태스크를 재개해도 이번 메시지는 자동으로 다시 실행되지 않습니다")
	AbortChatTurnFinished = cause("chat_turn_finished", "이번 대화 턴이 정상 종료되어 context를 해제함",
		"이번 대화 턴이 정상 종료되어 서버가 해당 턴의 context 리소스를 해제하고 있습니다. 이는 실행 중단이 아닙니다. 중단 메시지에 나타난 경우에는 취소와 종료 이벤트가 경합한 것입니다")

	// Process-level and per-run hard backstop.
	AbortShutdown = cause("shutdown", "백엔드 프로세스가 종료 중",
		"백엔드 프로세스가 SIGINT 또는 SIGTERM을 받아 재시작, 업데이트 또는 종료 중입니다. 실행 중이던 모든 Agent가 취소됩니다. 재시작 후 남아 있던 running 의도는 open으로 초기화되어 다시 실행됩니다")
	AbortRunHardTimeout = cause("run_hard_timeout", "단일 실행의 하드 타임아웃 안전장치가 발동됨",
		"단일 실행이 소프트 실행 시간 예산과 추가 유예를 넘겼습니다. 모델 요청이나 특정 도구가 오랫동안 반환하지 않아 정상적인 턴 경계의 마무리를 수행할 수 없었다는 뜻입니다. 중단 직전 마지막으로 반환되지 않은 도구 호출을 중점적으로 확인하세요")
)

// AbortReason resolves the named cause attached to a cancelled run context.
func AbortReason(ctx context.Context) (code, short, text string, ok bool) {
	c := context.Cause(ctx)
	if c == nil {
		return "", "", "", false
	}
	var ac *AbortCause
	if errors.As(c, &ac) {
		return ac.Code, ac.Short, ac.Text, true
	}
	switch {
	case errors.Is(c, context.DeadlineExceeded):
		return "deadline_exceeded", "상위 context의 deadline에 도달함",
			"상위 context가 deadline에 도달했지만 설정한 쪽이 WithTimeoutCause로 이름 있는 원인을 붙이지 않았습니다: " + c.Error(), true
	case errors.Is(c, context.Canceled):
		return "canceled_no_cause", "취소한 쪽이 이름 있는 원인을 붙이지 않음",
			"상위 context가 취소되었지만 취소한 쪽이 context.WithCancelCause로 이름 있는 원인을 붙이지 않았습니다. agent/cancelcause.go에 원인을 등록하고 해당 취소 지점에 연결하세요", true
	default:
		return "other", firstLine(c.Error(), 80), c.Error(), true
	}
}
