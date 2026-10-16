// Package agent wires real LLM-driven planner and work agents (on top of the
// agent-core SDK) to the dual SQLite graph. See docs/ARTEX-架构设计.md
// §4.3 (planner) and §4.4 (work agent).
//
// Provider configuration is read from the environment so the system runs with
// any Anthropic- or OpenAI-format endpoint. If no key is configured, FromEnv
// returns ok=false and the exploration engine stays idle (an LLM is required).
package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/Autumn-27/artex/llmrec"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/compaction"
	"github.com/Autumn-27/norma/llm"
	acperm "github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/transcript"
)

// Config describes the LLM backend resolved from the environment.
type Config struct {
	Format  llm.Format
	BaseURL string
	APIKey  string
	Model   string
	// Proxy routes all LLM requests through the given proxy URL (http/https/socks5,
	// optionally with user:pass@ credentials). Empty means direct — it does NOT
	// fall back to the standard *_PROXY environment variables.
	Proxy string
	// RatePerSecond / RatePerMinute cap the shared request rate across ALL agents
	// using the provider (0 = that window unlimited).
	RatePerSecond float64
	RatePerMinute float64
	// ContextWindowK is the model's context window in K tokens (user-configured),
	// used to size compaction thresholds. 0 = default; see CompactionWindow.
	ContextWindowK int
	// ThinkingType은 사고(thinking) 「on/off」 필드(thinking.type)를 독립적으로 제어한다:
	//   "" = 전송하지 않음(기본값, 해당 필드를 지원하지 않는 모델과 호환); "disabled" = 명시적으로 끔;
	//   "enabled" = 켬. ReasoningEffort와 완전히 분리되어 있다. 일부 인터페이스에는 thinking 필드가 없고
	//   강도 파라미터만으로 사고를 활성화할 수 있으므로, 두 값은 각각 따로 설정할 수 있다.
	ThinkingType string
	// ReasoningEffort는 사고 「강도」 필드를 독립적으로 제어한다:
	//   "" = 전송하지 않음(기본값); "low"/"medium"/"high"/"xhigh"/"max" = 해당 강도.
	//   OpenAI에서는 최상위 reasoning_effort로, Anthropic에서는 output_config.effort로 매핑된다.
	ReasoningEffort string
	// Stream은 이 profile이 스트리밍(SSE) 인터페이스를 사용할지 제어한다. true(기본값) = 스트리밍, false = 진짜 비스트리밍
	// (stream:false를 보내 완전한 JSON을 한 번에 받으며 Provider.Complete를 거친다). 비스트리밍은 일부 게이트웨이의
	// 불량한 SSE 구현(빈 프레임, 사고 필드 프레임 누락)을 우회할 수 있지만, 실행 중 실시간 진행 상황과 실시간 token
	// 집계를 잃는다. agentcore.Options.NonStreaming = !Stream으로 매핑된다.
	Stream bool
	// MaxTokens는 단일 응답의 출력 상한(token)이다. 0 = 필드를 보내지 않아 서버 기본값을 따른다(기존 동작).
	// ContextWindowK와 다르다: 후자는 모델의 총 용량이며 로컬에서 압축 임계값을 계산하는 데만 쓰이고
	// 요청에는 나타나지 않는다. 이 값은 요청마다 함께 전송된다. agentcore.Options.MaxTokens로 매핑된다.
	MaxTokens int
	// MaxTokensField는 MaxTokens를 담을 요청 필드 이름을 고르며, format=openai일 때만 적용된다:
	//   "" = max_tokens(기본값); "max_completion_tokens" = 새 필드.
	// OpenAI 추론 모델(o 시리즈/GPT-5)은 후자만 인식하며, max_tokens를 받으면 unsupported_parameter로 바로 실패한다.
	// 반면 대부분의 호환 게이트웨이는 전자만 인식하므로 자동으로 추론하지 않고, 엔드포인트에 따라 사용자가 고르도록 한다.
	MaxTokensField string
	// SessionHeaderKey가 비어 있지 않으면 매 LLM 요청에 커스텀 HTTP 헤더를 붙인다. 헤더 이름은 이 값이고,
	// 헤더 값은 【현재 세션 id】다(chat 세션=conv-<id>, worker=exp<x>-worker-i<intent> 등, WorkerSessionID 참고).
	// session-id 헤더로 프롬프트 캐시나 고정 라우팅을 하는 게이트웨이용이다. 빈 값이면 보내지 않는다.
	// 값은 transcript.WithSessionID로 요청 context에 실리고 RoundTripper가 읽어 채우므로, 같은 공유 provider라도 세션마다 다른 헤더 값을 보낼 수 있다.
	SessionHeaderKey string
	// Retry는 이 설정을 해석한 재시도 파라미터다(profile 재정의 → 전역 정책 → 내장 기본값, server 측에서 해석).
	// 세 계층의 의미는 RetryConfig를 참고한다. 零값 = 내장 기본값을 그대로 따른다.
	Retry RetryConfig
}

// RetryConfig는 하나의 LLM 설정에 딸린 재시도 파라미터다. 각 계층의 「횟수」는 같은 의미를 가진다:
// 0 = 내장 기본 횟수를 사용; 음수 = 해당 계층의 재시도를 끔; >0 = 그 값을 사용. 각 계층의 「간격」은
// 0 = 해당 계층 원래의 지수 백오프를 사용; >0 = 이 고정 간격으로 대체한다.
type RetryConfig struct {
	// ConnectAttempts/ConnectInterval: SDK 연결 재시도(연결 재설정/타임아웃/429/5xx, 스트림 시작 전).
	// llm.Config.MaxRetries / RetryInterval로 직접 매핑된다. 기본 3회, 0.5s부터 지수 증가(상한 8s).
	ConnectAttempts int
	ConnectInterval time.Duration
	// EmptyAttempts/EmptyInterval: SDK 빈 응답 재시도(완료되었지만 content block이 없음, openai
	// 형식에서만 적용). llm.Config.EmptyResponseRetries / EmptyResponseInterval로 매핑된다.
	// 기본 2회, 같은 지수 단계를 따른다.
	EmptyAttempts int
	EmptyInterval time.Duration
	// StreamAttempts/StreamInterval: 같은 provider 안전 창 재시도. 이 프로젝트가 SDK 위에 덧붙인 계층으로,
	// 「아직 호출자에게 어떤 출력도 전달하지 않았을 때」만 끊긴 스트림/과부하/스트림 내 429를 재생한다. SDK는 이를 보지 못하며
	// server/task_llm.go에서 소비한다. 기본 2회, 0.5s부터 지수 증가(상한 4s).
	StreamAttempts int
	StreamInterval time.Duration
}

// compaction window resolution bounds (in K tokens). Below the floor the
// threshold math (window − summary reserve − buffer) would go non-positive and
// compaction would fire every turn; above the cap it would never fire.
const (
	defaultWindowK = 200  // unset → assume a 200K window (Claude default)
	minWindowK     = 32   // floor so effectiveWindow stays comfortably positive
	maxWindowK     = 1000 // cap at 1M tokens (user request)
)

// CompactionWindow returns the model context window in TOKENS for compaction
// thresholds, resolved from the user-configured size (ContextWindowK). 0/unset →
// a 200K default; otherwise clamped to [32K, 1M] so compaction stays effective.
func (c Config) CompactionWindow() int {
	k := c.ContextWindowK
	if k <= 0 {
		k = defaultWindowK
	}
	if k < minWindowK {
		k = minWindowK
	}
	if k > maxWindowK {
		k = maxWindowK
	}
	return k * 1000
}

// compactionConfig builds the agent-core compaction config for a context window
// in tokens. agentcore.NewSession wires the summarizer (same provider) when this
// is set on Options.Compaction.
func compactionConfig(windowTokens int) *compaction.Config {
	if windowTokens <= 0 {
		windowTokens = defaultWindowK * 1000
	}
	return &compaction.Config{ContextWindow: windowTokens}
}

// FromEnv resolves the LLM provider config:
//
//	ARTEX_LLM_PROVIDER = anthropic|openai (default: inferred from keys)
//	ARTEX_LLM_MODEL    = model id        (default: per provider)
//	ARTEX_LLM_BASE_URL = endpoint        (optional)
//	ARTEX_LLM_PROXY    = proxy URL        (optional; http/https/socks5)
//	ANTHROPIC_API_KEY / OPENAI_API_KEY         = credentials
func FromEnv() (Config, bool) {
	prov := os.Getenv("ARTEX_LLM_PROVIDER")
	anthKey := os.Getenv("ANTHROPIC_API_KEY")
	oaiKey := os.Getenv("OPENAI_API_KEY")

	if prov == "" {
		switch {
		case anthKey != "":
			prov = "anthropic"
		case oaiKey != "":
			prov = "openai"
		default:
			return Config{}, false
		}
	}

	c := Config{
		BaseURL: os.Getenv("ARTEX_LLM_BASE_URL"),
		Model:   os.Getenv("ARTEX_LLM_MODEL"),
		Proxy:   strings.TrimSpace(os.Getenv("ARTEX_LLM_PROXY")),
		// 기본값은 스트리밍; ARTEX_LLM_STREAM=false/0/off로 명시적으로 끄면 비스트리밍을 사용한다.
		Stream: !isFalsy(os.Getenv("ARTEX_LLM_STREAM")),
	}
	switch prov {
	case "openai":
		c.Format = llm.FormatOpenAI
		c.APIKey = oaiKey
		if c.Model == "" {
			c.Model = "gpt-4o"
		}
	case "openai-responses":
		c.Format = llm.FormatOpenAIResponses
		c.APIKey = oaiKey
		if c.Model == "" {
			c.Model = "gpt-5"
		}
	default:
		c.Format = llm.FormatAnthropic
		c.APIKey = anthKey
		if c.Model == "" {
			c.Model = "claude-opus-4-8"
		}
	}
	if c.APIKey == "" {
		return Config{}, false
	}
	return c, true
}

// ConfigFrom builds a Config from UI-provided strings (provider defaults to
// anthropic; model defaults per provider). Inputs are trimmed and the base URL
// is normalized to the API base the provider expects (the provider appends the
// endpoint path itself), so a full endpoint URL is tolerated.
func ConfigFrom(provider, model, baseURL, apiKey, proxy string) Config {
	c := Config{
		Model:   strings.TrimSpace(model),
		BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		APIKey:  strings.TrimSpace(apiKey),
		Proxy:   strings.TrimSpace(proxy),
		Stream:  true, // 기본값은 스트리밍; 호출자가 profile별로 덮어쓴다
	}
	switch strings.TrimSpace(provider) {
	case "openai":
		c.Format = llm.FormatOpenAI
		// provider appends "/chat/completions"; tolerate a full endpoint URL.
		c.BaseURL = strings.TrimRight(strings.TrimSuffix(c.BaseURL, "/chat/completions"), "/")
		if c.Model == "" {
			c.Model = "gpt-4o"
		}
	case "openai-responses":
		c.Format = llm.FormatOpenAIResponses
		// provider appends "/responses"; tolerate a full endpoint URL.
		c.BaseURL = strings.TrimRight(strings.TrimSuffix(c.BaseURL, "/responses"), "/")
		if c.Model == "" {
			c.Model = "gpt-5"
		}
	default:
		c.Format = llm.FormatAnthropic
		// provider appends "/v1/messages".
		c.BaseURL = strings.TrimRight(strings.TrimSuffix(c.BaseURL, "/v1/messages"), "/")
		if c.Model == "" {
			c.Model = "claude-opus-4-8"
		}
	}
	return c
}

// isFalsy reports whether an env-var string explicitly requests "off". Empty or
// unrecognized → false (so an unset var keeps the streaming default).
func isFalsy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "0", "false", "off", "no":
		return true
	}
	return false
}

// Provider returns the short provider name ("anthropic"/"openai").
func (c Config) Provider() string {
	switch c.Format {
	case llm.FormatOpenAI:
		return "openai"
	case llm.FormatOpenAIResponses:
		return "openai-responses"
	}
	return "anthropic"
}

// NewProvider builds an llm.Provider from the config. When a rate is set, the
// limiter lives on the single provider instance — so planner + all workers +
// main agent (which share this provider) are bounded by one shared rate limit.
func (c Config) NewProvider() (llm.Provider, error) {
	client, err := quotaAwareHTTPClient(c.Proxy, c.SessionHeaderKey)
	if err != nil {
		return nil, err
	}
	lc := llm.Config{
		Format:     c.Format,
		BaseURL:    c.BaseURL,
		APIKey:     c.APIKey,
		Model:      c.Model,
		HTTPClient: client,
	}
	// 사고 on/off와 강도 두 필드는 각각 그대로 전달한다(빈 값 = 해당 필드를 보내지 않음). 둘은 분리되어 있다:
	// thinking.type만 보내거나, effort만 보내거나, 둘 다 보내거나, 둘 다 보내지 않을 수 있다.
	lc.ThinkingType = c.ThinkingType
	lc.ReasoningEffort = c.ReasoningEffort
	// 출력 상한의 필드 이름 선택(빈 값 = max_tokens 사용). 상한의 「값」은 여기에 없다. 매 턴 agentcore.Options.MaxTokens로 전달되며,
	// provider는 그 값을 어느 키에 넣을지만 결정한다.
	lc.MaxTokensField = c.MaxTokensField
	// 재시도 파라미터는 SDK와 같은 의미를 가지며(횟수 0=기본/음수=끔, 간격 0=지수 백오프/>0=고정) 그대로 전달한다.
	lc.MaxRetries = c.Retry.ConnectAttempts
	lc.RetryInterval = c.Retry.ConnectInterval
	lc.EmptyResponseRetries = c.Retry.EmptyAttempts
	lc.EmptyResponseInterval = c.Retry.EmptyInterval
	if c.RatePerSecond > 0 || c.RatePerMinute > 0 {
		lc.RateLimit = &llm.RateLimit{PerSecond: c.RatePerSecond, PerMinute: c.RatePerMinute}
	}
	return llm.NewProvider(lc)
}

// IsQuotaExhaustedMessage deliberately recognizes only explicit balance,
// billing, credit, or quota-exhaustion signals. Generic 429/rate-limit text,
// authentication failures, network errors, and server failures are excluded.
var nonFailoverHTTPStatus = regexp.MustCompile(`(?:status(?:\s+code)?|http(?:\s+status)?)\s*[=:]?\s*(?:401|403|5\d\d)\b`)
var transientQuotaLimit = regexp.MustCompile(`(?i)(?:\b(?:rpm|tpm|rpd|qps)\b|quota[_\s-]*metric|rate[_\s-]*limit|too many requests|(?:requests?|tokens?)\s+(?:per|/)\s*(?:second|minute)|(?:per|/)\s*(?:second|minute)\s+(?:requests?|tokens?)|generate[_\s-]*requests[_\s-]*per[_\s-]*(?:minute|second)|tokens?[_\s-]*per[_\s-]*(?:minute|second))`)

func IsQuotaExhaustedMessage(message string) bool {
	message = strings.ToLower(message)
	// Authentication/authorization and provider-side 5xx failures never rotate,
	// even when a gateway happens to echo a quota-looking phrase in the body.
	if nonFailoverHTTPStatus.MatchString(message) {
		return false
	}
	// Provider APIs frequently describe an ordinary rate limit as "quota
	// exceeded", especially Google-style responses containing a quota metric.
	// These limits recover with time and must stay on the current provider.
	if transientQuotaLimit.MatchString(message) {
		return false
	}
	markers := []string{
		"insufficient_quota", "quota_exceeded", "quota exceeded", "quota exhausted",
		"exceeded your current quota", "billing_hard_limit_reached",
		"billing hard limit", "billing_not_active", "credit balance", "insufficient credit",
		"insufficient balance", "balance is too low", "payment required", "status 402",
		"余额不足", "额度不足", "额度已用尽", "欠费",
	}
	for _, marker := range markers {
		if strings.Contains(message, marker) {
			return true
		}
	}
	// gRPC RESOURCE_EXHAUSTED is overloaded for both account quota and ordinary
	// request-rate limiting. Preserve it as an explicit exhaustion signal only
	// when the same error does not identify a transient rate limit.
	return strings.Contains(message, "resource_exhausted") &&
		!strings.Contains(message, "rate limit") &&
		!strings.Contains(message, "too many requests")
}

// quotaAwareTransport preserves Norma's normal retry behavior except for a 429
// whose body explicitly says the account quota/balance is exhausted. Norma's
// retry loop treats every 429 as transient; normalizing only that response to
// 402 lets a task router fail over immediately while retaining the original
// response body for provider-specific classification and audit logs.
type quotaAwareTransport struct {
	base http.RoundTripper
	// sessionHeaderKey, when non-empty, is the HTTP header name each request
	// carries; its value is the session id read from the request context. Empty
	// disables it. See Config.SessionHeaderKey.
	sessionHeaderKey string
}

func (t quotaAwareTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Custom session-id header: name is user-configured, value is THIS run's
	// session id (norma stashes it on the context via transcript.WithSessionID).
	// Stable across a session's turns and distinct across sessions — exactly what
	// a session-keyed prompt cache wants. Skipped when no session id is present.
	if t.sessionHeaderKey != "" {
		if sid := transcript.SessionIDFrom(req.Context()); sid != "" {
			req.Header.Set(t.sessionHeaderKey, sid)
		}
	}
	// When LLM recording is on, the Recorder puts a Capture on the context so the
	// raw wire bodies can be persisted. This is the only layer that still sees
	// them: norma builds the request body internally and decodes the SSE response
	// before either reaches the recorder.
	capt := llmrec.CaptureFrom(req.Context())
	capt.SetRequest(requestBodySnapshot(req))

	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	// Tee rather than read: a 200 is an SSE stream that must keep streaming. The
	// 429 branch below reads through this wrapper, so its body lands in the
	// capture before being replaced.
	resp.Body = capt.TeeResponse(resp.StatusCode, resp.Body)

	if resp.StatusCode != http.StatusTooManyRequests {
		return resp, nil
	}
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	if readErr != nil {
		return resp, nil
	}
	if IsQuotaExhaustedMessage(string(body)) {
		resp.StatusCode = http.StatusPaymentRequired
		resp.Status = "402 Payment Required"
	}
	return resp, nil
}

// requestBodySnapshot copies an outgoing request body without consuming it.
// norma builds every model request from a *bytes.Reader, so net/http populates
// GetBody and the copy has no effect on what gets sent.
func requestBodySnapshot(req *http.Request) string {
	if req.GetBody == nil {
		return ""
	}
	rc, err := req.GetBody()
	if err != nil {
		return ""
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return ""
	}
	return string(b)
}

func quotaAwareHTTPClient(proxy, sessionHeaderKey string) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	proxy = strings.TrimSpace(proxy)
	if proxy == "" {
		transport.Proxy = nil // 빈 값 = 직접 연결, HTTP_PROXY/HTTPS_PROXY 환경 변수로 대체하지 않음
	} else {
		proxyURL, err := url.Parse(proxy)
		if err != nil {
			return nil, fmt.Errorf("llm: invalid proxy %q: %w", proxy, err)
		}
		switch proxyURL.Scheme {
		case "http", "https", "socks5":
		case "":
			return nil, fmt.Errorf("llm: proxy %q missing scheme (use http://, https:// or socks5://)", proxy)
		default:
			return nil, fmt.Errorf("llm: unsupported proxy scheme %q (use http, https or socks5)", proxyURL.Scheme)
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	return &http.Client{Transport: quotaAwareTransport{base: transport, sessionHeaderKey: strings.TrimSpace(sessionHeaderKey)}}, nil
}

// logTestConnection prints the raw HTTP status code(s) and response body of a
// connection test to the server log, so clicking "테스트" leaves a diagnosable trail of
// exactly what the gateway returned — 401 bodies, quota text, empty frames — not
// just the collapsed ok/err the UI shows. Bodies are clipped to keep a chatty
// SSE stream from flooding the log.
func logTestConnection(c Config, capt *llmrec.Capture) {
	attempts := capt.Attempts()
	if len(attempts) == 0 {
		log.Printf("[llm-test] %s / %s @ %s — HTTP 요청이 전혀 전송되지 않음(설정 해석 또는 연결 수립 단계에서 실패)",
			c.Provider(), c.Model, c.BaseURL)
		return
	}
	for i, a := range attempts {
		log.Printf("[llm-test] %s / %s @ %s — 시도 %d/%d HTTP %d\n응답 본문: %s",
			c.Provider(), c.Model, c.BaseURL, i+1, len(attempts), a.Status, clipBody(a.Body))
	}
}

// clipBody trims a wire body for logging. 4K is plenty to show an error JSON or
// the head of an SSE stream while bounding a runaway response.
func clipBody(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(비어 있음)"
	}
	const max = 4096
	if len(s) > max {
		return s[:max] + fmt.Sprintf("…(잘림, 총 %d 바이트)", len(s))
	}
	return s
}

// TestConnection makes a minimal real completion to verify the provider/model/
// endpoint/key actually work. Returns the round-trip latency and the model's
// reply text.
func TestConnection(ctx context.Context, c Config) (time.Duration, string, error) {
	prov, err := c.NewProvider()
	if err != nil {
		return 0, "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// 원시 wire 메시지를 캡처한다. 연결 테스트에서 가장 필요한 것은 게이트웨이가 실제로 무엇을 반환했는지(상태 코드+응답 본문)이며,
	// norma는 응답을 StreamEvent로 디코딩한 뒤 이 정보를 버리기 때문이다. quotaAwareTransport가 context에서 이 Capture를 찾아
	// 매 HTTP 시도의 상태 코드와 body를 채운다.
	ctx, capt := llmrec.NewCapture(ctx)
	defer logTestConnection(c, capt)
	// 연결 테스트는 agentcore의 세션 루프를 거치지 않는 단발 경로이므로 context에 session id가 붙지 않는다.
	// SessionHeaderKey를 설정한 엔드포인트(예: opencode zen은 x-opencode-session 헤더를 필수로 요구해 없으면 400 MissingSessionID를 반환)에서는
	// 「대화는 정상인데 테스트 클릭 시 400」이 되는 차이가 생긴다. 여기서 일회용 랜덤 session id를 붙여, 테스트와 실제 대화가 같은
	// 헤더 설정 로직을 타도록 한다. SessionHeaderKey가 없는 엔드포인트는 이 값을 읽지 않으므로 부작용이 없다.
	ctx = transcript.WithSessionID(ctx, "conntest-"+transcript.NewSessionID())
	start := time.Now()
	// MaxTokens는 넉넉하게 줘야 한다. 추론 모델(예: deepseek-v4-pro)은 답을 내기 전에 긴 사고 과정을 먼저 생성한다
	// (실측상 "ping" 한 마디에도 ~2900 token을 소모). 32만 주면 모델이 「사고 단계」에서 출력 상한(finish=length)에 걸려 잘리고,
	// 연결 테스트는 err=nil로 통과하더라도 "중단/length/resume"이 뒤섞여 표시된다. 예산을 충분히 주면 OK를 깔끔하게 출력한다(finish=stop).
	// EscalateMaxTokens는 false로 둔다. 잘림을 이유로 한도를 올려 재시도하지 않으며, resume 루프로 토큰을 낭비하지 않기 위해서다.
	reply, err := agentcore.Run(ctx, agentcore.Options{
		Provider:       prov,
		SystemPrompt:   []string{"你是连接测试。直接输出两个字符 OK 即可，不要思考、不要解释、不要别的。"},
		PermissionMode: acperm.ModeBypass,
		MaxTurns:       1,
		MaxTokens:      8192,
		NonStreaming:   !c.Stream, // 해당 profile의 실제 송수신 모드로 연결 테스트를 수행
	}, "ping")
	lat := time.Since(start)
	if err != nil {
		return lat, "", err
	}
	// err==nil만으로는 부족하다. 요청은 성공했지만 모델이 글자 하나도 내보내지 않는 경우가 실제로 있다(사고가 예산을 모두 소모,
	// 본문이 안전 정책에 의해 삼켜짐, 호환 계층이 content를 누락). 이런 설정은 세션에서 「응답하지 않는」 상태인데
	// 테스트는 성공으로 보고하게 되므로, 이번에 없애려는 차이가 바로 이것이다. 보이는 본문이 없으면 무조건 실패로 판정한다.
	reply = strings.TrimSpace(reply)
	if reply == "" {
		return lat, "", fmt.Errorf("모델이 응답 내용을 반환하지 않음(요청은 성공했지만 텍스트가 반환되지 않음)")
	}
	return lat, reply, nil
}
