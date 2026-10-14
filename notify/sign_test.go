package notify

import (
	"net/url"
	"testing"
	"time"
)

// 서명 기준 값은 OpenSSL로 독립적으로 계산한 것이며, 이 패키지 자체 구현으로 생성한 것이 아니다.
// 그렇지 않으면 '코드가 바뀌지 않았다'만 증명할 뿐, '알고리즘이 맞다'는 증명할 수 없다.
//
//	TS=1700000000000, SECRET=SECtest123
//	DingTalk: printf '%s\n%s' "$TS" "$SECRET" | openssl dgst -sha256 -hmac "$SECRET" -binary | openssl base64 -A
//	      -> w3RMHXzixTMdzr8OHJUmVLS4IoPJVdu+Ut1LE48MePE=
//	Feishu: printf '' | openssl dgst -sha256 -hmac "$(printf '%s\n%s' "$TS" "$SECRET")" -binary | openssl base64 -A
//	      -> Hd4xFWQU6R6ad4nzy4ETIznzlqebqH7xcTFVmONTudo=
const (
	signTestTSMillis = int64(1700000000000)
	signTestSecret   = "SECtest123"
	dingTalkExpected = "w3RMHXzixTMdzr8OHJUmVLS4IoPJVdu+Ut1LE48MePE="
	feishuExpected   = "Hd4xFWQU6R6ad4nzy4ETIznzlqebqH7xcTFVmONTudo="
)

func TestDingTalkSignMatchesReference(t *testing.T) {
	got, err := dingTalkSignedURL("https://oapi.dingtalk.com/robot/send?access_token=tok", signTestSecret, time.UnixMilli(signTestTSMillis))
	if err != nil {
		t.Fatalf("签名失败: %v", err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("产出地址不可解析: %v", err)
	}
	q := u.Query()
	if q.Get("sign") != dingTalkExpected {
		t.Errorf("签名不符\n期望 %s\n得到 %s", dingTalkExpected, q.Get("sign"))
	}
	if q.Get("timestamp") != "1700000000000" {
		t.Errorf("时间戳应为毫秒且原样带上，得到 %q", q.Get("timestamp"))
	}
	// 기존 query 파라미터(access_token)는 서명에 의해 덮어써지면 안 된다.
	if q.Get("access_token") != "tok" {
		t.Errorf("原有 query 参数丢失，得到 %q", q.Get("access_token"))
	}
}

func TestFeishuSignMatchesReference(t *testing.T) {
	got := feishuSign("1700000000000", signTestSecret)
	if got != feishuExpected {
		t.Errorf("签名不符\n期望 %s\n得到 %s", feishuExpected, got)
	}
}

// TestSignAlgorithmsDiffer는 두 플랫폼의 알고리즘 차이를 고정한다. 이들은 서로 파라미터 순서가 반대다
// (DingTalk는 key=secret, Feishu는 key=서명 대상 문자열). 한쪽을 그대로 베끼면 반드시 검증에 실패하며,
// 이 케이스는 향후 리팩터링에서 두 함수가 하나로 합쳐지지 않도록 보장한다.
func TestSignAlgorithmsDiffer(t *testing.T) {
	ts := "1700000000000"
	dingURL, err := dingTalkSignedURL("https://example.com/hook", signTestSecret, time.UnixMilli(signTestTSMillis))
	if err != nil {
		t.Fatal(err)
	}
	dq, _ := url.Parse(dingURL)
	if dq.Query().Get("sign") == feishuSign(ts, signTestSecret) {
		t.Fatal("钉钉与飞书签名相同，说明其中一家的算法实现错了")
	}
}

func TestDingTalkNoSecretLeavesURLUntouched(t *testing.T) {
	// 서명을 켜지 않은 봇: timestamp/sign 파라미터를 임의로 추가하면 안 된다.
	const hook = "https://oapi.dingtalk.com/robot/send?access_token=tok"
	got, err := dingTalkSignedURL(hook, "", time.UnixMilli(signTestTSMillis))
	if err != nil {
		t.Fatal(err)
	}
	if got != hook {
		t.Fatalf("未配置 secret 时地址不应改动，得到 %q", got)
	}
}

func TestValidateHTTPURL(t *testing.T) {
	ok := []string{"https://example.com/hook", "http://10.0.0.1:8080/x?y=1"}
	for _, s := range ok {
		if err := validateHTTPURL(s); err != nil {
			t.Errorf("%q 应被接受: %v", s, err)
		}
	}
	// file:// 같은 스킴은 허용하면 안 된다. http.Client가 이를 처리하는 방식은 예상 범위를 벗어난다.
	bad := []string{"", "file:///etc/passwd", "ftp://example.com", "https://", "gopher://x"}
	for _, s := range bad {
		if err := validateHTTPURL(s); err == nil {
			t.Errorf("%q 应被拒绝", s)
		}
	}
}
