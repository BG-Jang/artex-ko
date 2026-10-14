package notify

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

// 이 파일은 관련된 두 가지 강화 조치를 검증한다:
//   ① 발송 주소가 서버를 발판으로 삼아 내부망이나 클라우드 메타데이터를 공격(SSRF)하는 것을 막는다
//   ② 주소 검증 오류 메시지에 주소 안의 자격 증명이 드러나면 안 된다
//
// 이 패키지의 많은 케이스는 127.0.0.1의 httptest 가짜 수신 서버를 사용하므로, 가드는 기본적으로 이를 차단한다.
// 그래서 TestMain에서 AllowLocalTargetsEnv를 일괄 활성화하고, 아래 각 SSRF 케이스에서는
// 이를 명시적으로 해제하여 **기본 거부** 동작을 단언한다.

func TestMain(m *testing.M) {
	// 일반 케이스가 로컬 가짜 수신 서버에 연결할 수 있도록 한다. SSRF 케이스는 스스로 임시로 비운다.
	_ = os.Setenv(AllowLocalTargetsEnv, "1")
	os.Exit(m.Run())
}

// TestDialGuardRejectsLoopbackByDefault는 SSRF 방어의 핵심 단언이다:
// 기본 설정에서 루프백 주소로의 발송은 **연결 계층**에서 반드시 거부되어야 한다.
func TestDialGuardRejectsLoopbackByDefault(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		_, _ = io.WriteString(w, `{"errcode":0}`)
	}))
	defer srv.Close()

	t.Setenv(AllowLocalTargetsEnv, "") // 탈출구 해제 = 기본 동작
	_, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"}, Message{Items: []Item{{Severity: "high"}}})
	if err == nil {
		t.Fatal("默认不应允许投递到环回地址")
	}
	if hit {
		t.Fatal("请求已经打到了本机服务——守卫没生效")
	}
	// 오류 메시지는 사용자에게 어떻게 허용하는지 안내할 수 있어야 한다 (로컬 SMTP 릴레이는 정당한 구성이다).
	if !strings.Contains(err.Error(), AllowLocalTargetsEnv) {
		t.Errorf("拒绝信息应说明如何显式放开: %v", err)
	}
}

// TestDialGuardAllowsLoopbackWhenOptedIn은 반대 케이스다: 명시적으로 허용하면 반드시 동작해야 한다.
// 그렇지 않으면 로컬 postfix나 내부망 릴레이처럼 정당한 배포가 일괄적으로 막힌다.
func TestDialGuardAllowsLoopbackWhenOptedIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer srv.Close()

	t.Setenv(AllowLocalTargetsEnv, "1")
	if _, err := (dingTalkChannel{}).Send(context.Background(),
		map[string]any{"webhook": srv.URL + "/robot/send"}, Message{Items: []Item{{Severity: "high"}}}); err != nil {
		t.Fatalf("显式放开后应可投递: %v", err)
	}
}

func TestIsBlockedDialIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.1.2.3", "::1",
		"169.254.169.254", // 클라우드 메타데이터 엔드포인트. 이 함수가 존재하는 주된 이유다.
		"169.254.1.1", "fe80::1",
		"0.0.0.0", "::",
		"224.0.0.1", "ff02::1",
		"::ffff:127.0.0.1", // IPv4-mapped 형식은 반드시 원래 형태로 복원한 뒤 판정해야 한다. 그렇지 않으면 우회구멍이 된다.
		"",
	}
	for _, s := range blocked {
		if !isBlockedDialIP(net.ParseIP(s)) {
			t.Errorf("%s 应被拒绝", s)
		}
	}
	// RFC1918 사설망은 **의도적으로** 허용한다. 내부망에 직접 구축한 Mattermost나 SMTP 릴레이는 흔한 정당한 용법이다.
	// 이 단언은 이 트레이드오프를 고정한다. 누군가 사설망 판정을 슬쩍 추가하면 여기서 실패하고,
	// 그 결정을 의식적으로 내리도록 강제한다 (소리 없이 일부 배포를 망가뜨리지 않도록).
	allowed := []string{"10.0.0.5", "172.16.3.4", "192.168.1.10", "8.8.8.8", "2606:4700::1111"}
	for _, s := range allowed {
		if isBlockedDialIP(net.ParseIP(s)) {
			t.Errorf("%s 应被放行（私网是常见的合法投递目标）", s)
		}
	}
}

// TestValidateHTTPURLRejectsLiteralPrivateTargets는 설정 단계의 사전 안내를 검증한다:
// IP 리터럴은 첫 발송 실패를 기다리지 않고 저장 시점에 거부해야 한다.
func TestValidateHTTPURLRejectsLiteralPrivateTargets(t *testing.T) {
	t.Setenv(AllowLocalTargetsEnv, "")
	for _, raw := range []string{
		"http://127.0.0.1:8080/hook",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]:8080/hook",
	} {
		if err := validateHTTPURL(raw); err == nil {
			t.Errorf("%s 应在配置阶段被拒绝", raw)
		}
	}
	// 공인 주소와 사설망 주소는 평소처럼 통과한다 (사설망은 다이얼 단계에 맡기며, 그 단계에서는 차단하지 않는다).
	for _, raw := range []string{"https://oapi.dingtalk.com/robot/send", "http://10.0.0.9/hook"} {
		if err := validateHTTPURL(raw); err != nil {
			t.Errorf("%s 应通过校验: %v", raw, err)
		}
	}
}

// TestValidateHTTPURLErrorNeverLeaksCredentials는 감사에서 지적된, 내가 이전 라운드에서 놓쳤던 분기다.
//
// url.Parse가 **실패**하면 전체 원본 주소를 포함하는 *url.Error를 반환하며, 그 Error()에 전체 원본 주소가 들어 있다.
// 나는 이전에 http.Client.Do의 반환 오류만 마스킹하고 여기를 빠뜨렸다. 당시 추가한 '영구 실패 경로' 케이스(file://, gopher://, ftp://)는
// 모두 url.Parse에 성공하여 scheme 분기로 가기 때문에, 전부 통과해도 이 경로가 안전하다는 것을 증명하지 못했다. 거짓 보장이었다.
// 그래서 전부 통과하더라도 이 경로가 안전하다는 것은 증명되지 않는다 — 거짓 보장이다.
func TestValidateHTTPURLErrorNeverLeaksCredentials(t *testing.T) {
	cases := []string{
		"http://127.0.0.1/%zz?access_token=" + leakProbeToken,         // 잘못된 퍼센트 이스케이프
		"https://a.example.com:port/x?access_token=" + leakProbeToken, // 포트가 숫자가 아님
		"http://[::1?access_token=" + leakProbeToken,                  // 괄호 짝이 맞지 않음
	}
	for _, raw := range cases {
		// 먼저 이 입력이 **실제로** url.Parse를 실패시키는지 확인한다. 이 단계가 없으면 케이스가 눈치채지 못한 채 다른 분기로 갈 수 있다
		// (이전 라운드의 거짓 보장이 바로 이렇게 생겼다).
		if _, err := url.Parse(raw); err == nil {
			t.Errorf("%q 本应解析失败，否则这条用例没有覆盖到目标分支", raw)
			continue
		}
		err := validateHTTPURL(raw)
		if err == nil {
			t.Errorf("%q 应校验失败", raw)
			continue
		}
		assertNoSecret(t, err.Error(), leakProbeToken)
	}
	// 채널 계층의 래핑도 주소를 밖으로 내보내지 않는지 확인한다.
	t.Setenv(AllowLocalTargetsEnv, "")
	err := (dingTalkChannel{}).Validate(map[string]any{"webhook": cases[0]})
	if err == nil {
		t.Fatal("非法地址应校验失败")
	}
	assertNoSecret(t, err.Error(), leakProbeToken)
}

// TestEmailDialGuardRejectsLoopbackByDefault는 SMTP 채널의 다이얼 가드를 검증한다.
//
// 이메일 채널은 과거에 날것의 net.Dialer를 쓰고 있었다. SSRF 방어 전체에서 유일한 구멍이었다. host를
// 169.254.169.254나 127.0.0.1로 설정하면 바로 연결되었고, smtp.NewClient 핸드셰이크가 실패할 때 상대가 돌려준 한 줄이 오류에 담겨
// last_error를 거쳐 발송 이력 API로 그대로 되돌아왔다. 다른 채널들이 이미 막아 둔 '반쯤 눈먼 읽기' 원시 기능과 같은 것이다.
// '연결 거부 vs 타임아웃'의 소요 시간 차이로 포트를 탐지하는 것도 가능했다.
//
// 이 패키지의 TestMain은 AllowLocalTargetsEnv를 전역으로 켠다 (많은 케이스가 127.0.0.1의 가짜 수신 서버를 쓰기 때문).
// 그래서 이 케이스는 스스로 이를 꺼야 한다. 그렇지 않으면 가드가 있든 없든 통과해버린다.
// 이것이 바로 이 허점이 어떤 테스트에서도 발견되지 않았던 이유다.
func TestEmailDialGuardRejectsLoopbackByDefault(t *testing.T) {
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)

	t.Setenv(AllowLocalTargetsEnv, "") // 탈출구 해제 = 기본 동작
	_, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil {
		t.Fatal("默认不应允许把邮件投递到环回地址")
	}
	// 연결 자체가 만들어지지 않아야 한다: 가드가 Control 훅에서 차단하므로 EHLO는 전송되지 않는다.
	if f.sawCommand("EHLO") || f.sawCommand("HELO") {
		t.Fatal("SMTP 会话已经建立——守卫没生效")
	}
	// 오류 메시지는 사용자에게 어떻게 허용하는지 안내할 수 있어야 한다 (로컬 postfix 릴레이는 정당한 구성이다).
	if !strings.Contains(err.Error(), AllowLocalTargetsEnv) {
		t.Errorf("拒绝信息应说明如何显式放开: %v", err)
	}
}

// TestEmailDialGuardAllowsLoopbackWhenOptedIn은 짝을 이루는 반대 케이스다: 명시적으로 허용하면
// 정상적으로 발송되어야 한다. 내부망 자체 구축 SMTP나 로컬 릴레이는 매우 흔한 배포이므로, 가드가 일괄적으로 막아서는 안 된다.
func TestEmailDialGuardAllowsLoopbackWhenOptedIn(t *testing.T) {
	f := newFakeSMTP(t)
	cfg := emailCfg(t, f, nil)

	t.Setenv(AllowLocalTargetsEnv, "1")
	if _, err := (emailChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("显式放开后本机 SMTP 应可投递: %v", err)
	}
	if !f.sawCommand("EHLO") {
		t.Fatal("未看到 EHLO——会话没真正建立")
	}
}
