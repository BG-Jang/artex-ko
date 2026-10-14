package server

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// 태스크 단위 타임아웃 코디네이터(docs/任务级超时与收尾设计.md §4/§8.5 참조).
// 절대 벽시계 기준: timeout이 설정된 태스크마다 타이머 goroutine 하나가 마감 시각에 맞춰 마무리 절차를 순서대로 진행한다:
//   ① settling → ② worker 신규 의도 수령 중지 / ③ planner 일반 notify 폐기
//   ④ 실행 중인 worker drain 대기(grace 적용) → ⑤ 최종 라운드 planner 판정 → ⑥ 최종 상태 확정(가드 포함)

const (
	settleDrainGrace     = 90 * time.Second // 실행 중인 worker가 우아하게 마무리될 때까지의 상한. 초과 시 강제 cancel
	deadlinePollInterval = 2 * time.Second  // deadline이 찍히지 않았거나 LLM이 준비되지 않았을 때의 폴링 간격
	deadlineMaxSleep     = 30 * time.Second // 1회 최대 대기 시간(주기적으로 최종 상태를 재확인하기 위함)
)

// ---------- settling 상태 ----------

func (e *Engine) isSettling(taskID string) bool {
	v, _ := e.settling.Load(taskID)
	b, _ := v.(bool)
	return b
}

// markSettling flips settling on; returns true only for the first caller.
func (e *Engine) markSettling(taskID string) bool {
	_, loaded := e.settling.LoadOrStore(taskID, true)
	return !loaded
}

// ---------- 실행 중 카운터(worker.Execute + planner.Plan), drain에 사용 ----------

func (e *Engine) inflightCounter(taskID string) *int64 {
	v, _ := e.inflight.LoadOrStore(taskID, new(int64))
	return v.(*int64)
}

// beginTaskOperation atomically registers a task-owned operation unless deletion
// has already installed its barrier. The delete handler can therefore wait for
// inflight==0 without a check-then-start race recreating files after cleanup.
func (e *Engine) beginTaskOperation(taskID string) bool {
	e.deleteMu.RLock()
	defer e.deleteMu.RUnlock()
	if e.IsDeleting(taskID) {
		return false
	}
	atomic.AddInt64(e.inflightCounter(taskID), 1)
	return true
}

func (e *Engine) decInflight(taskID string) { atomic.AddInt64(e.inflightCounter(taskID), -1) }
func (e *Engine) inflightCount(taskID string) int64 {
	return atomic.LoadInt64(e.inflightCounter(taskID))
}

// ---------- deadline ----------

// taskDeadline returns the task's absolute deadline (unix). Prefers the in-process
// map (stamped this session); falls back to the DB-loaded value (restart), seeding
// the map. 0 = no timeout / not yet stamped.
func (e *Engine) taskDeadline(t *Task) int64 {
	if v, ok := e.deadline.Load(t.ID); ok {
		return v.(int64)
	}
	deadlineAt := t.lifecycleSnapshot().DeadlineAt
	if deadlineAt > 0 {
		e.deadline.Store(t.ID, deadlineAt)
		return deadlineAt
	}
	return 0
}

// resetTimeoutRevival clears only the per-run timeout state after PostgreSQL has
// atomically committed timeout -> running and reset first_run_at/deadline_at. The
// configured TimeoutSeconds remains on Task, so the next real Planner/Worker run
// stamps a fresh full budget. coordStarted is reset because the coordinator that
// produced the timeout has already completed (or is in its final return path).
func (e *Engine) resetTimeoutRevival(taskID string) {
	e.settling.Delete(taskID)
	e.deadline.Delete(taskID)
	e.stamped.Delete(taskID)
	e.coordStarted.Delete(taskID)
}

// stampFirstRun records first_run_at + deadline_at on the FIRST real run (LLM ready)
// of a timeout task, once per process. No-op when the task has no timeout.
func (e *Engine) stampFirstRun(t *Task) {
	if t.TimeoutSeconds <= 0 {
		return
	}
	if _, loaded := e.stamped.LoadOrStore(t.ID, true); loaded {
		return
	}
	dl, err := e.m.StampTaskFirstRun(t.ID)
	if err != nil {
		log.Printf("[deadline] task %s first_run 스탬프 기록 실패: %v", t.ID, err)
		e.stamped.Delete(t.ID) // 다음 재시도를 허용
		return
	}
	if dl > 0 {
		e.deadline.Store(t.ID, dl)
		log.Printf("[deadline] task %s 첫 실행, 마감 시각 %s", t.ID, time.Unix(dl, 0).Format("2006-01-02 15:04:05"))
	}
}

// clockCtx layers the task's TaskClock (absolute deadline) onto a run's context so
// worker/planner can clamp their wall-clock budget and pick per-run vs task-timeout
// wrap-up words. final marks the coordinator-driven terminal planner round.
func (e *Engine) clockCtx(base context.Context, t *Task, final bool) context.Context {
	dl := e.taskDeadline(t)
	if dl <= 0 && !final {
		return base // no timeout → unchanged behavior
	}
	return agent.WithTaskClock(base, agent.TaskClock{DeadlineUnix: dl, Final: final})
}

// ---------- 코디네이터 ----------

// startDeadlineCoordinator launches the per-task deadline timer once (idempotent).
// Called from Run() and from the restart reload path, so non-active timeout tasks
// still get settled after their deadline even without live planner/worker loops.
func (e *Engine) startDeadlineCoordinator(ctx context.Context, t *Task) {
	if t == nil || t.TimeoutSeconds <= 0 {
		return
	}
	e.deleteMu.RLock()
	if e.IsDeleting(t.ID) {
		e.deleteMu.RUnlock()
		return
	}
	if _, loaded := e.coordStarted.LoadOrStore(t.ID, true); loaded {
		e.deleteMu.RUnlock()
		return
	}
	rt := e.registerTaskRoutines(ctx, t.ID, 1)
	e.deleteMu.RUnlock()
	runTaskRoutine(rt, func(loopCtx context.Context) { e.deadlineCoordinator(loopCtx, t) })
}

// deadlineCoordinator waits until the task's absolute deadline, then runs the settle
// sequence. Absolute wall-clock: it keeps counting through pauses.
func (e *Engine) deadlineCoordinator(ctx context.Context, t *Task) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if isTerminalStatus(e.m.TaskStatus(t.ID)) {
			return // already finished (goals met / failed) — nothing to time out
		}
		dl := e.taskDeadline(t)
		if dl <= 0 {
			if sleepCtx(ctx, deadlinePollInterval) { // not yet stamped (task hasn't really run)
				return
			}
			continue
		}
		if remaining := time.Until(time.Unix(dl, 0)); remaining > 0 {
			nap := remaining
			if nap > deadlineMaxSleep {
				nap = deadlineMaxSleep
			}
			if sleepCtx(ctx, nap) {
				return
			}
			continue
		}
		e.settleTask(ctx, t)
		return
	}
}

// settleTask runs the ordered settle sequence once (§4 steps ①–⑥).
func (e *Engine) settleTask(ctx context.Context, t *Task) {
	if !e.markSettling(t.ID) {
		return
	}
	log.Printf("[deadline] task %s 타임아웃 상한 도달, 마무리 절차 진입", t.ID)

	// ④ 실행 중인 worker/planner의 drain을 대기한다(실행 중인 run은 클램프된 MaxDuration에 따라 스스로 마무리된다);
	// grace를 넘겨도 비워지지 않으면 → 해당 태스크의 exec ctx를 강제 cancel한다(settling 인지 분기에서 올바르게 분류됨).
	hardStop := time.Now().Add(settleDrainGrace)
	for e.inflightCount(t.ID) > 0 {
		if time.Now().After(hardStop) {
			log.Printf("[deadline] task %s drain 타임아웃(%s), 실행 중인 run 강제 취소", t.ID, settleDrainGrace)
			e.cancelExec(t.ID, agent.AbortSettleDrainTimeout)
			_ = sleepCtx(ctx, 3*time.Second) // worker 분기가 DB 저장과 분류를 할 수 있도록 잠시 대기
			break
		}
		if sleepCtx(ctx, 500*time.Millisecond) {
			return // 엔진 전체 종료
		}
	}

	// ⑤ 최종 라운드 planner를 1회 실행한다(태스크 타임아웃 문구 사용, 최종 목표 판정, 새 의도는 생성하지 않음).
	met := e.runFinalPlannerRound(ctx, t)
	if !e.beginTaskOperation(t.ID) {
		return
	}
	defer e.decInflight(t.ID)

	// ⑥ 최종 상태를 확정한다(가드 포함): met → done(completed), 그렇지 않으면 timeout. 일반 경로에서 이미 done을 기록했다면,
	// 가드(SetTaskStatusGuarded)가 덮어쓰기를 거부하여 completed 의미를 유지한다.
	status := "timeout"
	if met {
		status = "done"
	}
	won, err := e.m.SetTaskStatusGuarded(t.ID, status)
	switch {
	case err != nil:
		log.Printf("[deadline] task %s 최종 상태 기록 실패: %v", t.ID, err)
	case won:
		log.Printf("[deadline] task %s 마무리 완료, 최종 상태=%s", t.ID, status)
	default:
		log.Printf("[deadline] task %s 마무리 시점에 이미 최종 상태, 기존 상태 유지", t.ID)
	}
}

// runFinalPlannerRound drives exactly ONE terminal planner round with the
// task-timeout planner words (final goal judgment; no new intents). Waits for the
// LLM to be ready (bounded by ctx) so a completable task isn't mis-judged timeout.
// timeoutFinalRoundSummaryFmt 는 작업 시간 초과 시 마지막 계획 라운드의 표시 전용
// 활동 요약이다(node_id 없음·전사에만 노출·되먹임 경로 미접촉). [[G132]]
const timeoutFinalRoundSummaryFmt = "작업 시간 초과 마무리·최종 판정(%d차)"

func (e *Engine) runFinalPlannerRound(ctx context.Context, t *Task) (met bool) {
	if e.IsDeleting(t.ID) {
		return false
	}
	planner, _ := e.snapshotFor(t)
	for planner == nil {
		if sleepCtx(ctx, deadlinePollInterval) {
			return false
		}
		if isTerminalStatus(e.m.TaskStatus(t.ID)) {
			return false
		}
		if e.IsDeleting(t.ID) {
			return false
		}
		planner, _ = e.snapshotFor(t)
	}
	// 독립 ctx(execCancel에 연결하지 않아 pause/강제 cancel이 이 마지막 라운드를 중단하지 않도록 함), Final 플래그를 달아 태스크 타임아웃 문구를 주입한다.
	fctx := e.clockCtx(ctx, t, true)
	if !e.beginTaskOperation(t.ID) {
		return false
	}
	defer e.decInflight(t.ID)
	emit := func(r db.Activity) { e.emitActivity(t, r) }
	e.emitActivity(t, db.Activity{Worker: "planner", Kind: "round",
		Summary: fmt.Sprintf(timeoutFinalRoundSummaryFmt, e.nextPlannerRound(t.ID))})
	tTaskID, _ := strconv.ParseInt(t.ID, 10, 64)
	e.BeginLLMCall(t.ID)
	met, reason, err := planner.Plan(fctx, tTaskID, e.m.assets, t.Store, t.Goal, t.drainTriggers(), emit)
	e.EndLLMCall(t.ID)
	if err != nil {
		log.Printf("[deadline] task %s 최종 계획 오류: %v", t.ID, err)
	} else if met {
		log.Printf("[deadline] task %s 최종 판정, 목표 달성: %s", t.ID, reason)
	}
	return met
}
