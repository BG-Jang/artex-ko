package server

import (
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// 재시도 정책의 서버 측 해석은 docs/LLM重试设计.md 참고. 다섯 계층 중:
//   - 연결 수립 / 빈 응답 / 동일 provider 안전 구간 은 「엔드포인트를 따라가는」 항목이라, 각 LLM 설정이 덮어쓸 수 있다.
//     전역 기본값을 덮어쓸 수 있다(profile 의 항목을 비워 두면 전역 값을 상속하고, 전역도 없으면 내장 기본값을 쓴다);
//   - 서킷 브레이커 / 의도 재실행 은 프로세스 단위이며, 전역에 하나만 존재한다.
//
// 전역 정책은 DB 의 settings 한 행을 읽으며, 호출 지점은 모두 저빈도 경로다(provider 구성, work 마무리,
// 설정 저장). 캐시 계층을 더 추가할 가치는 없다. 다만 서킷 브레이커 파라미터는 예외다. 실패 경로에서 매번 읽어야 하므로
// applyRetryPolicy 가 Registry 에 넘겨 보관한다.

// retryPolicy reads the global policy; a nil DB yields the zero policy (all
// layers on their built-in defaults).
func (s *Server) retryPolicy() db.LLMRetryPolicy {
	if s.m == nil || s.m.pg == nil {
		return db.LLMRetryPolicy{}
	}
	return s.m.pg.LLMRetryPolicy()
}

// resolveRetry layers one profile's override on top of the global policy and
// converts the result into the form agent.Config carries. Rules combine field by
// field, so a profile that only pins an interval still inherits the global count.
func resolveRetry(o db.RetryOverride, pol db.LLMRetryPolicy) agent.RetryConfig {
	connect := o.Connect.Or(pol.Connect)
	empty := o.Empty.Or(pol.Empty)
	stream := o.Stream.Or(pol.Stream)
	return agent.RetryConfig{
		// 횟수는 여기서 「0=기본값 / 음수=끔」이라는 원래 의미를 유지한다. SDK 의 MaxRetries /
		// EmptyResponseRetries 와 완전히 같은 구조이므로, 그쪽이 스스로 해석하도록 맡기면 된다.
		ConnectAttempts: connect.Attempts, ConnectInterval: connect.Interval(),
		EmptyAttempts: empty.Attempts, EmptyInterval: empty.Interval(),
		StreamAttempts: stream.Attempts, StreamInterval: stream.Interval(),
	}
}

// applyProfileRetry fills cfg.Retry for a profile read from the DB.
func (s *Server) applyProfileRetry(cfg *agent.Config, p *db.LLMProfile) {
	if p == nil {
		return
	}
	cfg.Retry = resolveRetry(p.Retry, s.retryPolicy())
}

// 서킷 브레이커(폴링 쿨다운)의 기본값은 llmpool 내장 값과 같다. 여기서는 「사용자가 값을 설정한 경우」에만 덮어쓴다.
// 의도 재실행의 기본값은 engine.go 의 modelErrorRetries / modelErrorRetryBackoff 를 참고한다.

// applyRetryPolicy pushes the process-wide layers of the policy into the objects
// that consume them on a hot path: the circuit-breaker registry. Called at
// startup and whenever the policy is saved.
func (s *Server) applyRetryPolicy() {
	pol := s.retryPolicy()
	if s.llmHealth != nil {
		s.llmHealth.SetPolicy(pol.Breaker.Attempts, pol.Breaker.Interval())
	}
}

// modelErrorRetryPolicy resolves the intent-level replay knobs (layer ⑤): how
// many times a model_error work is re-run and how long to back off between runs.
func (e *Engine) modelErrorRetryPolicy() (retries int, backoff time.Duration) {
	retries, backoff = modelErrorRetries, modelErrorRetryBackoff
	if e == nil || e.m == nil || e.m.pg == nil {
		return retries, backoff
	}
	rule := e.m.pg.LLMRetryPolicy().Intent
	if rule.Attempts != 0 {
		retries = max(rule.Attempts, 0)
	}
	if d := rule.Interval(); d > 0 {
		backoff = d
	}
	return retries, backoff
}

// emptyTurnNudgeLimit resolves how many empty-turn continuations one work may
// inject (see steerHooks.Stop). It deliberately reuses layer ②'s knob —— 「빈 응답
// 재시도 횟수」: 둘은 같은 일을 하는 두 가지 수단이다. SDK 계층은 「콘텐츠 블록이 하나도 없는」 경우를 다루며, 수단은
// 같은 요청을 그대로 다시 보내는 것이다. 이 계층은 「사고만 있고 본문도 도구도 없는」 경우를 다루며, 수단은 지시를 하나 덧붙여
// 모델이 이미 있는 사고를 이어서 진행하게 하는 것이다(같은 요청을 그대로 재전송하는 것은 컨텍스트 형태에서 비롯된 이런 공회전에는 의미가 없다). 빈 응답 판정 기준이
// 다른 이유는 SDK 가 「이벤트를 yield 했는지」를 기준으로 하지만, 사고 증분 자체가 이벤트이기 때문이다. 그러나 사용자가 설정하는
// 「빈 응답 재시도 횟수」의 의도는 「모델이 실질적인 내용을 내지 못하면 다시 한 번 요청한다」이므로,
// 두 계층이 하나의 횟수를 공유해야 이 의도와 맞는다.
//
// 특정 profile 의 덮어쓰기가 아니라 전역 정책을 읽는다. run 도중 장애 전환으로 profile 이 바뀔 수 있는데, 이것은
// 의도 전체의 총량 게이트이므로 엔드포인트가 바뀐다고 함께 바뀌면 안 된다. 의미는 SDK 의 emptyRetries() 와 같은 구조다:
// 0 = 기본값 defaultEmptyTurnNudges; -1(음수) = 공회전 이어 달리기 끔; >0 = 해당 값 사용.
func (e *Engine) emptyTurnNudgeLimit() int {
	if e == nil || e.m == nil || e.m.pg == nil {
		return defaultEmptyTurnNudges
	}
	switch n := e.m.pg.LLMRetryPolicy().Empty.Attempts; {
	case n == 0:
		return defaultEmptyTurnNudges
	case n < 0:
		return 0
	default:
		return n
	}
}
