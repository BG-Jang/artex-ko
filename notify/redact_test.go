package notify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// 이 파일은 불변식 테스트다: 채널 구현에서 나오는 **어떤** 오류 텍스트도 자격 증명을 담으면 안 된다.
//
// 이 파일을 따로 둔 이유: 최초의 채널 케이스는 성공 경로와 플랫폼 비즈니스 오류만 다뤘고,
// 전송 계층 실패는 전혀 보지 않았다. 그런데 바로 전송 계층 오류(연결 거부, DNS 실패, 타임아웃)가 가장 위험하다.
// http.Client.Do가 반환하는 *url.Error는 **전체 URL**을 오류 텍스트에 넣으며, 이 기능에서 이 채널들의 자격 증명은 바로 URL 안에 있다.
// 자격 증명은 이 경로를 따라 네 개의 출구로 흘러간다:
//
//	notification_deliveries.last_error → 평문으로 DB 저장
//	GET /api/notify/deliveries 응답 → 채널 설정의 마스킹을 우회하여 브라우저에 그대로 노출
//	서버 로그 → 외부로 전송되어 보관되는 경우가 많음
//	테스트 발송 API의 502 응답 → 프런트엔드에 그대로 노출
//
// 그래서 여기서는 함수 하나만 테스트하지 않고, 채널마다 반드시 실패하는 요청을 실제로 보내 보고, 오류 텍스트에서
// 그 자격 증명을 찾을 수 없음을 단언한다.

// credentialCases는 '자격 증명이 URL에 있는' 모든 채널 형태를 다룬다:
// DingTalk와 WeCom은 query에, Feishu는 경로 끝 세그먼트에, Telegram은 경로 중간에 있다.
var credentialCases = []struct {
	name   string
	ch     Channel
	cfg    map[string]any
	secret string
}{
	{
		name:   "钉钉 access_token 在 query",
		ch:     dingTalkChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/robot/send?access_token=" + leakProbeToken},
		secret: leakProbeToken,
	},
	{
		name:   "企业微信 key 在 query",
		ch:     weComChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/cgi-bin/webhook/send?key=" + leakProbeToken},
		secret: leakProbeToken,
	},
	{
		name:   "飞书 hook id 在路径末段",
		ch:     feishuChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/open-apis/bot/v2/hook/" + leakProbeToken},
		secret: leakProbeToken,
	},
	{
		name:   "Telegram bot token 在路径中段",
		ch:     telegramChannel{},
		cfg:    map[string]any{"bot_token": leakProbeToken, "chat_id": "1", "base_url": "http://127.0.0.1:1"},
		secret: leakProbeToken,
	},
	{
		name:   "钉钉加签密钥",
		ch:     dingTalkChannel{},
		cfg:    map[string]any{"webhook": "http://127.0.0.1:1/robot/send", "secret": leakProbeToken},
		secret: leakProbeToken,
	},
}

// leakProbeToken은 절대 실제 자격 증명일 수 없는 센티넬 값이며, 오류 텍스트에서 이를 검색하는 데 쓴다.
const leakProbeToken = "LEAKPROBE0123456789abcdef"

// TestChannelErrorsNeverLeakCredentials는 핵심 불변식이다.
func TestChannelErrorsNeverLeakCredentials(t *testing.T) {
	for _, tc := range credentialCases {
		t.Run(tc.name, func(t *testing.T) {
			// 반드시 실패하는 상대방: 127.0.0.1:1에는 아무도 수신하지 않으므로 연결 거부 경로를 탄다.
			_, err := tc.ch.Send(context.Background(), tc.cfg, Message{
				Items: []Item{{FindingID: 1, Severity: "high", Name: "泄露探针"}},
			})
			if err == nil {
				t.Fatal("对不可达地址应报错")
			}
			assertNoSecret(t, err.Error(), tc.secret)
		})
	}
}

// TestChannelErrorsNeverLeakCredentialsInPermanentPath는 영구 실패 분기를 다룬다:
// URL 검증 실패, 플랫폼 비즈니스 오류 등도 오류 텍스트를 외부로 전달하므로, 마찬가지로 자격 증명을 담으면 안 된다.
func TestChannelErrorsNeverLeakCredentialsInPermanentPath(t *testing.T) {
	cases := []struct {
		name string
		ch   Channel
		cfg  map[string]any
	}{
		// 주소에 자격 증명이 있지만 형식이 잘못됨. validateHTTPURL / url.Parse 분기를 트리거한다.
		{"钉钉地址非法", dingTalkChannel{}, map[string]any{"webhook": "file:///" + leakProbeToken}},
		{"企微地址非法", weComChannel{}, map[string]any{"webhook": "gopher://" + leakProbeToken}},
		{"飞书地址非法", feishuChannel{}, map[string]any{"webhook": "ftp://" + leakProbeToken + "/hook"}},
		{"Telegram API 地址非法", telegramChannel{}, map[string]any{"bot_token": "tok", "chat_id": "1", "base_url": "file://" + leakProbeToken}},
		{"通用 Webhook 地址非法", webhookChannel{}, map[string]any{"url": "javascript:" + leakProbeToken}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.ch.Send(context.Background(), tc.cfg, Message{Items: []Item{{Severity: "high"}}})
			if err == nil {
				t.Fatal("非法配置应报错")
			}
			assertNoSecret(t, err.Error(), leakProbeToken)
		})
	}
}

func assertNoSecret(t *testing.T, text, secret string) {
	t.Helper()
	if strings.Contains(text, secret) {
		t.Fatalf("错误文本泄露了凭据 %q:\n    %s", secret, text)
	}
}

func TestRedactRequestTargetKeepsOnlySchemeAndHost(t *testing.T) {
	cases := map[string]string{
		"https://oapi.dingtalk.com/robot/send?access_token=S1":    "https://oapi.dingtalk.com/…",
		"https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=S2": "https://qyapi.weixin.qq.com/…",
		"https://open.feishu.cn/open-apis/bot/v2/hook/S3":         "https://open.feishu.cn/…",
		"https://api.telegram.org/botS4/sendMessage":              "https://api.telegram.org/…",
		"http://10.0.0.5:8080/hook":                               "http://10.0.0.5:8080/…",
	}
	for in, want := range cases {
		got := redactRequestTarget(in)
		if got != want {
			t.Errorf("redactRequestTarget(%q) = %q，期望 %q", in, got, want)
		}
		// 마스킹 결과 자체에도 원본 주소의 경로/쿼리 조각이 전혀 남아 있으면 안 된다.
		if parts := strings.SplitN(in, "://", 2); len(parts) == 2 {
			if hostAndRest := strings.SplitN(parts[1], "/", 2); len(hostAndRest) == 2 && hostAndRest[1] != "" {
				if strings.Contains(got, hostAndRest[1]) {
					t.Errorf("脱敏后仍含路径/查询片段 %q: %q", hostAndRest[1], got)
				}
			}
		}
	}
	// 파싱할 수 없는 입력은 원래 문자열을 절대 에코하지 않는다.
	for _, bad := range []string{"", "://", "not a url", "http://"} {
		if got := redactRequestTarget(bad); strings.Contains(got, bad) && bad != "" {
			t.Errorf("不可解析输入 %q 被回显为 %q", bad, got)
		}
	}
}

// TestRedactTransportErrorStripsURL은 *url.Error라는 구체 타입을 정확히 겨눈다:
// 이는 http.Client.Do의 반환 타입이며, 누출의 첫 번째 현장이다.
func TestRedactTransportErrorStripsURL(t *testing.T) {
	inner := errors.New("dial tcp 127.0.0.1:1: connect: connection refused")
	uerr := &url.Error{
		Op:  "Post",
		URL: "https://api.telegram.org/bot" + leakProbeToken + "/sendMessage",
		Err: inner,
	}
	got := redactTransportError(uerr)
	assertNoSecret(t, got, leakProbeToken)
	if !strings.Contains(got, "api.telegram.org") {
		t.Errorf("应保留 host 以便排查，得到 %q", got)
	}
	if !strings.Contains(got, "connection refused") {
		t.Errorf("应保留底层原因以便排查，得到 %q", got)
	}
	// Op도 보존해야 한다 (POST인지 GET인지는 원인 파악에 의미가 있다).
	if !strings.Contains(got, "Post") {
		t.Errorf("应保留操作名，得到 %q", got)
	}
}

// TestRedactURLsInTextHandlesFallback은 폴백 경로를 다룬다: *url.Error가 아닌 커스텀 오류
// (예: 리다이렉트 정책이 반환한 오류)에 든 주소도 마찬가지로 제거해야 한다.
func TestRedactURLsInTextHandlesFallback(t *testing.T) {
	in := fmt.Sprintf("拒绝跨主机重定向（a.example → http://b.example/bot%s/send）", leakProbeToken)
	got := redactURLsInText(in)
	assertNoSecret(t, got, leakProbeToken)
	if !strings.Contains(got, "http://b.example/…") {
		t.Errorf("应把地址替换为脱敏形态，得到 %q", got)
	}
	// 주소가 없는 텍스트는 원래 그대로 보존한다.
	if plain := "dial tcp: connection refused"; redactURLsInText(plain) != plain {
		t.Error("不含地址的文本不应被改动")
	}
}

// TestCrossHostRedirectRefused는 '자격 증명이 URL에 있음 + 호스트 간 리다이렉트 추적 = 자격 증명 유출'을 다룬다.
// httptest의 두 서버는 127.0.0.1의 서로 다른 포트에서 수신하므로, 포트가 다르면 Host도 다르다.
// 이것이 바로 호스트 간 리다이렉트를 구성한다.
func TestCrossHostRedirectRefused(t *testing.T) {
	var hit bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/robot/send?access_token="+leakProbeToken, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	_, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": redirector.URL + "/robot/send?access_token=" + leakProbeToken},
		Message{Items: []Item{{Severity: "high"}}})
	if err == nil {
		t.Fatal("跨主机重定向应被拒绝")
	}
	if hit {
		t.Fatal("跳转目标被访问了——凭据已随重定向外泄")
	}
	assertNoSecret(t, err.Error(), leakProbeToken)
}

// TestSameHostRedirectAllowed는 반대 케이스다: 같은 호스트 내 리다이렉트(예: 끝 슬래시 추가)는 반드시 여전히 동작해야 한다.
// 그렇지 않으면 정상적인 워크플로까지 함께 막힌다.
func TestSameHostRedirectAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robot/send" {
			// 같은 호스트, 같은 포트로의 리다이렉트.
			http.Redirect(w, r, "/robot/send/", http.StatusTemporaryRedirect)
			return
		}
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer srv.Close()

	if _, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"},
		Message{Items: []Item{{Severity: "high"}}}); err != nil {
		t.Fatalf("同主机重定向不应被拒绝: %v", err)
	}
}
