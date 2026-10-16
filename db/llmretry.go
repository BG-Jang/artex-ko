package db

import (
	"encoding/json"
	"time"
)

// LLM 재시도 정책: 5계층 재시도의 「횟수 + 간격」 전역 구성. docs/LLM重试设计.md 참고.
// settings 테이블의 JSON 값 하나에 저장된다 —— 시스템 전체에 하나만 있는 런타임 파라미터이므로 이를 위해 테이블을 따로 만들 가치는 없다;
// 읽을 때는 내장 기본값으로 폴백하므로, 키가 없을 때(새 DB/한 번도 구성하지 않은 경우) 동작은 상수를 하드코딩하던 시절과 완전히 같다.

const settingLLMRetryPolicy = "llm_retry_policy"

// RetryRule is one layer's knob pair. The zero value means "unset":
//
//	Attempts   0 = 내장 기본 횟수 사용; -1 = 해당 계층 재시도 끔; >0 = 해당 값 사용
//	IntervalMS 0 = 해당 계층 원래의 간격 정책 사용(보통 지수 백오프); >0 = 고정 밀리초 간격으로 변경
//
// -1은 「0회」가 아니라 「명시적으로 끔」을 뜻한다. 0은 이미 「미구성」이 차지하고 있기 때문이다.
type RetryRule struct {
	Attempts   int `json:"attempts"`
	IntervalMS int `json:"interval_ms"`
}

// Interval returns the configured fixed interval, or 0 when unset (caller keeps
// its own default ladder).
func (r RetryRule) Interval() time.Duration {
	if r.IntervalMS <= 0 {
		return 0
	}
	return time.Duration(r.IntervalMS) * time.Millisecond
}

// Or returns the rule with each unset field filled in from fallback. Used to
// layer a profile override on top of the global policy field by field, so a
// profile that only pins the interval still inherits the global count.
func (r RetryRule) Or(fallback RetryRule) RetryRule {
	if r.Attempts == 0 {
		r.Attempts = fallback.Attempts
	}
	if r.IntervalMS == 0 {
		r.IntervalMS = fallback.IntervalMS
	}
	return r
}

// retry knob bounds. A count above the cap turns a blip into a token bonfire;
// an interval above an hour outlives any transient failure worth waiting out.
const (
	maxRetryAttempts   = 20
	maxRetryIntervalMS = 3600_000 // 1h
)

// Clamped returns the rule with out-of-range values pulled back into the sane
// band (attempts within [-1, 20], interval within [0, 1h]).
func (r RetryRule) Clamped() RetryRule {
	if r.Attempts < -1 {
		r.Attempts = -1
	}
	if r.Attempts > maxRetryAttempts {
		r.Attempts = maxRetryAttempts
	}
	if r.IntervalMS < 0 {
		r.IntervalMS = 0
	}
	if r.IntervalMS > maxRetryIntervalMS {
		r.IntervalMS = maxRetryIntervalMS
	}
	return r
}

// Clamped bounds a profile's override the same way the global policy is bounded,
// so a hand-crafted API payload can't land a value the CHECK constraint rejects.
func (o RetryOverride) Clamped() RetryOverride {
	o.Connect, o.Empty, o.Stream = o.Connect.Clamped(), o.Empty.Clamped(), o.Stream.Clamped()
	return o
}

// LLMRetryPolicy holds the 5계층 retry configuration. Connect/Empty/Stream are the
// per-request layers (a profile may override them, see LLMProfile.Retry);
// Breaker and Intent are process-wide by nature and live only here.
type LLMRetryPolicy struct {
	// Connect: SDK 연결 수립 재시도(연결 재설정/타임아웃/429/5xx, 스트림 시작 전). 기본 3회, 지수 백오프.
	Connect RetryRule `json:"connect"`
	// Empty: SDK 빈 응답 재시도(완료되었지만 content block이 없음, openai 형식만). 기본 2회, 지수 백오프.
	Empty RetryRule `json:"empty"`
	// Stream: 동일 provider 안전 창 재시도(출력을 전달하기 전 스트림 끊김의 재생). 기본 2회, 0.5s부터 지수 증가(상한 4s).
	Stream RetryRule `json:"stream"`
	// Breaker: 폴링 서킷 브레이커. Attempts=연속 몇 번의 일시적 실패에서 차단할지(기본 3, -1=일시적 실패는 차단하지 않음,
	// 잔액 부족/키 무효 같은 하드 실패는 여전히 즉시 차단); IntervalMS=고정 쿨다운 시간(0=기본 1/5/30분 단계).
	Breaker RetryRule `json:"breaker"`
	// Intent: worker가 model_error로 종료된 뒤 해당 의도 전체를 다시 실행. 기본 2회, 고정 3s.
	Intent RetryRule `json:"intent"`
}

// Clamped returns the policy with every rule clamped.
func (p LLMRetryPolicy) Clamped() LLMRetryPolicy {
	p.Connect, p.Empty, p.Stream = p.Connect.Clamped(), p.Empty.Clamped(), p.Stream.Clamped()
	p.Breaker, p.Intent = p.Breaker.Clamped(), p.Intent.Clamped()
	return p
}

// LLMRetryPolicy reads the global retry policy. A missing or unparseable value
// yields the zero policy — i.e. every layer on its built-in default.
func (d *DB) LLMRetryPolicy() LLMRetryPolicy {
	var p LLMRetryPolicy
	if d == nil {
		return p
	}
	raw, ok, err := d.GetSetting(settingLLMRetryPolicy)
	if err != nil || !ok || raw == "" {
		return p
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return LLMRetryPolicy{}
	}
	return p.Clamped()
}

// SetLLMRetryPolicy persists the global retry policy (values are clamped first).
func (d *DB) SetLLMRetryPolicy(p LLMRetryPolicy) error {
	raw, err := json.Marshal(p.Clamped())
	if err != nil {
		return err
	}
	return d.SetSetting(settingLLMRetryPolicy, string(raw))
}
