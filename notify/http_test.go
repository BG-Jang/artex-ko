package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// 이 파일은 doJSON의 HTTP 계층 오류 분류를 다룬다.
//
// 따로 테스트하는 이유: 각 채널 어댑터는 플랫폼 자체의 비즈니스 오류 코드만 처리한다(DingTalk errcode,
// Feishu code, Telegram ok 필드). 그런데 **HTTP 계층**의 분류는 doJSON이 공통으로 수행하며,
// 두 방어선은 서로 독립적이다. 이 계층이 없으면 503을 반환하는 중계 게이트웨이가 영구 실패로 간주되어
// 재시도 없이 포기되고, 403은 재시도 가능으로 간주되어 세 라운드나 헛되이 백오프하게 된다.

func replyServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDoJSONClassifiesHTTPStatus(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		permanent bool
	}{
		{"200 成功不算错误", 200, false},
		{"429 限流可重试", 429, false},
		{"408 请求超时可重试", 408, false},
		{"500 服务端错误可重试", 500, false},
		{"502 网关错误可重试", 502, false},
		{"503 服务不可用可重试", 503, false},
		{"400 参数错误永久失败", 400, true},
		{"401 鉴权失败永久失败", 401, true},
		{"403 禁止访问永久失败", 403, true},
		{"404 地址不存在永久失败", 404, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := replyServer(t, tc.status, `{"detail":"upstream says no"}`)
			_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
			if tc.status < 300 {
				if err != nil {
					t.Fatalf("2xx 不应报错: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("非 2xx 应报错")
			}
			if got := IsPermanent(err); got != tc.permanent {
				t.Fatalf("HTTP %d 的 permanent 判定错误：期望 %v 得到 %v (%v)",
					tc.status, tc.permanent, got, err)
			}
			// 상태 코드는 오류에 반드시 나타나야 한다. 그렇지 않으면 사용자가 설정을 잘못한 것인지 상대방이 다운된 것인지 판단할 수 없다.
			// 영어 StatusText가 아니라 숫자를 단언한다. 이 패키지의 문구는 중국어이므로(프로젝트의 다른 부분과 일치),
			// 숫자만이 언어와 무관하게 안정적으로 단언할 수 있는 부분이다.
			if !strings.Contains(err.Error(), strconv.Itoa(tc.status)) {
				t.Errorf("错误信息应带上 HTTP 状态码 %d，得到 %v", tc.status, err)
			}
		})
	}
}

// TestDoJSONIncludesResponseSnippet은 snippet을 검증한다: 상대방이 반환한 오류 설명은 함께 돌려줘야 한다.
// 그렇지 않으면 사용자는 '실패했다'만 알고, 상대가 왜 거부했는지 알 수 없다.
func TestDoJSONIncludesResponseSnippet(t *testing.T) {
	srv := replyServer(t, 400, `{"error":"invalid webhook token"}`)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("应报错")
	}
	if !strings.Contains(err.Error(), "invalid webhook token") {
		t.Errorf("错误信息应带回对端的说明，得到 %v", err)
	}
}

// TestDoJSONSnippetIsSingleLineAndBounded는 snippet의 형태를 제약한다:
// 응답 원문이 last_error 열과 프런트엔드 표에 그대로 들어가므로, 여러 줄이나 과도한 길이는 레이아웃과 페이로드를 깨뜨린다.
func TestDoJSONSnippetIsSingleLineAndBounded(t *testing.T) {
	// 줄바꿈, 탭, 5000자 초과 내용이 포함된 응답.
	long := strings.Repeat("x", 5000)
	srv := replyServer(t, 500, "line1\nline2\r\n\tline3 "+long)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("应报错")
	}
	msg := err.Error()
	if strings.ContainsAny(msg, "\r\n\t") {
		t.Errorf("错误信息应压成单行，得到 %q", msg)
	}
	// snippet 상한은 200자 + 고정 접두사이며, 전체 분량은 원본 응답보다 훨씬 작아야 한다.
	if len(msg) > 400 {
		t.Errorf("错误信息过长（%d 字节），应被 snippet 截断: %q", len(msg), msg)
	}
}

// TestDoJSONRejectsOversizedResponse는 읽기에 상한이 있음을 확인한다: 상대방이 비정상적으로 큰 응답을 반환해도
// 전체 응답을 메모리에 읽어 들이면 안 된다 (발송 이력의 모든 항목이 last_error를 하나씩 저장한다).
func TestDoJSONRejectsOversizedResponse(t *testing.T) {
	huge := strings.Repeat("A", 1<<20) // 1 MiB
	srv := replyServer(t, 400, huge)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("应报错")
	}
	if len(err.Error()) > 400 {
		t.Errorf("超大响应应被限长读取并截断，错误信息长度 %d", len(err.Error()))
	}
}
