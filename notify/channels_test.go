package notify

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

// singleMsg는 따옴표와 줄바꿈이 들어간 단건 메시지를 만든다. 일부러 `"`와 `\n`이 포함된 제목/요약을 사용한다:
// 템플릿 보간이 가장 쉽게 잘못된 JSON을 만들어내는 입력이 바로 이것이다.
func singleMsg() Message {
	return Message{
		Items: []Item{{
			FindingID: 42,
			Name:      `登录处 "SQL注入" 风险`,
			VulnClass: "SQL注入",
			Severity:  "high",
			Summary:   "参数 id\n未过滤 导致注入",
			Assets:    []string{"a.example.com", "b.example.com"},
			DetailURL: "https://artex.local/function/findings/detail?id=42",
		}},
	}
}

// batchMsg는 여러 건을 묶은 요약 메시지를 만든다.
func batchMsg(n int) Message {
	m := Message{Batch: true, WindowMinutes: 30, HomeURL: "https://artex.local/function/findings"}
	for i := 0; i < n; i++ {
		m.Items = append(m.Items, Item{
			FindingID: int64(i + 1),
			Name:      "漏洞" + itoa(i+1),
			VulnClass: "XSS",
			Severity:  "medium",
			Summary:   "反射型跨站脚本",
			Assets:    []string{"target.example.com"},
		})
	}
	return m
}

// capturePost는 가짜 수신 서버를 띄워, 받은 요청 본문과 헤더를 검증 함수에 되돌려준다.
func capturePost(t *testing.T, respBody string, assert func(t *testing.T, body map[string]any, r *http.Request)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("请求体不是合法 JSON: %v\n原文: %s", err, raw)
			}
		}
		if assert != nil {
			assert(t, body, r)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respBody)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDingTalkSendsActionCardWhenLinkPresent(t *testing.T) {
	srv := capturePost(t, `{"errcode":0,"errmsg":"ok"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["msgtype"] != "actionCard" {
			t.Fatalf("有回链时应发 actionCard，得到 %v", body["msgtype"])
		}
		card, _ := body["actionCard"].(map[string]any)
		if card["singleURL"] != "https://artex.local/function/findings/detail?id=42" {
			t.Errorf("回链丢失: %v", card["singleURL"])
		}
	})
	if _, err := (dingTalkChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, singleMsg()); err != nil {
		t.Fatalf("投递失败: %v", err)
	}
}

func TestDingTalkFallsBackToMarkdownForBatch(t *testing.T) {
	srv := capturePost(t, `{"errcode":0,"errmsg":"ok"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["msgtype"] != "markdown" {
			t.Fatalf("汇总消息应发 markdown，得到 %v", body["msgtype"])
		}
		md, _ := body["markdown"].(map[string]any)
		if !strings.Contains(md["text"].(string), "최근 30분간") {
			t.Errorf("汇总正文缺少时间窗: %v", md["text"])
		}
	})
	if _, err := (dingTalkChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, batchMsg(3)); err != nil {
		t.Fatalf("投递失败: %v", err)
	}
}

// TestDingTalkBusinessErrorIsPermanent는 「HTTP 200이지만 errcode가 0이 아닌」 판정을 고정한다.
// errcode를 확인하지 않으면 발송 실패를 성공으로 기록하게 된다. 이는 국내 IM 플랫폼들에 공통된 함정이다.
func TestDingTalkBusinessErrorIsPermanent(t *testing.T) {
	srv := capturePost(t, `{"errcode":310000,"errmsg":"keywords not in content"}`, nil)
	_, err := (dingTalkChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, singleMsg())
	if err == nil {
		t.Fatal("errcode 非 0 应报错")
	}
	if !IsPermanent(err) {
		t.Fatalf("关键词不匹配属于配置错误，应标记为永久失败，得到 %v", err)
	}
	if !strings.Contains(err.Error(), "310000") {
		t.Errorf("错误信息应带上平台错误码，得到 %v", err)
	}
}

func TestWeComTruncatesCJKWithinByteLimit(t *testing.T) {
	var contentLen int
	srv := capturePost(t, `{"errcode":0,"errmsg":"ok"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		md, _ := body["markdown"].(map[string]any)
		content, _ := md["content"].(string)
		contentLen = len(content)
		if !utf8.ValidString(content) {
			t.Fatal("截断后不是合法 UTF-8——企微会整条拒收")
		}
	})
	// 4096바이트를 확실히 넘도록 충분히 긴 중국어 요약을 만든다.
	m := batchMsg(200)
	if _, err := (weComChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, m); err != nil {
		t.Fatalf("投递失败: %v", err)
	}
	if contentLen > weComMarkdownLimit {
		t.Fatalf("正文 %d 字节超出企微上限 %d", contentLen, weComMarkdownLimit)
	}
	if contentLen == 0 {
		t.Fatal("正文为空")
	}
}

func TestWeComRateLimitIsRetryableButKeyErrorIsPermanent(t *testing.T) {
	limited := capturePost(t, `{"errcode":45009,"errmsg":"api freq out of limit"}`, nil)
	_, err := (weComChannel{}).Send(context.Background(), map[string]any{"webhook": limited.URL}, singleMsg())
	if err == nil || IsPermanent(err) {
		t.Fatalf("45009 是滚动窗口限流，应可重试，得到 %v", err)
	}

	badKey := capturePost(t, `{"errcode":93000,"errmsg":"invalid webhook url"}`, nil)
	_, err = (weComChannel{}).Send(context.Background(), map[string]any{"webhook": badKey.URL}, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("93000 是 key 无效，重试不会自愈，应为永久失败，得到 %v", err)
	}
}

func TestFeishuCardStructureAndSign(t *testing.T) {
	const secret = "SECtest123"
	srv := capturePost(t, `{"code":0,"msg":"success"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["msg_type"] != "interactive" {
			t.Fatalf("应发交互式卡片，得到 %v", body["msg_type"])
		}
		card, _ := body["card"].(map[string]any)
		header, _ := card["header"].(map[string]any)
		if header["template"] != "orange" {
			t.Errorf("high 级别应为 orange 配色，得到 %v", header["template"])
		}
		// secret을 설정했다면 서명 파라미터를 반드시 포함해야 한다. 그렇지 않으면 Feishu가 19021 코드로 거부한다.
		if body["sign"] == nil || body["timestamp"] == nil {
			t.Fatalf("缺少加签参数: %v", body)
		}
		// 카드 요소에는 취약점 상세 페이지를 가리키는 url을 가진 버튼이 하나 포함되어야 한다.
		elements, _ := card["elements"].([]any)
		foundButton := false
		for _, e := range elements {
			em, _ := e.(map[string]any)
			if em["tag"] != "action" {
				continue
			}
			actions, _ := em["actions"].([]any)
			for _, a := range actions {
				am, _ := a.(map[string]any)
				if am["url"] == "https://artex.local/function/findings/detail?id=42" {
					foundButton = true
				}
			}
		}
		if !foundButton {
			t.Fatal("卡片里没有指向详情页的按钮")
		}
	})
	cfg := map[string]any{"webhook": srv.URL, "secret": secret}
	if _, err := (feishuChannel{}).Send(context.Background(), cfg, singleMsg()); err != nil {
		t.Fatalf("投递失败: %v", err)
	}
}

func TestFeishuWithoutSecretOmitsSign(t *testing.T) {
	srv := capturePost(t, `{"code":0,"msg":"success"}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["sign"] != nil || body["timestamp"] != nil {
			t.Fatalf("未配置 secret 时不应带加签参数: %v", body)
		}
	})
	if _, err := (feishuChannel{}).Send(context.Background(), map[string]any{"webhook": srv.URL}, singleMsg()); err != nil {
		t.Fatalf("投递失败: %v", err)
	}
}

func TestTelegramEscapesHTMLInUntrustedContent(t *testing.T) {
	var text string
	srv := capturePost(t, `{"ok":true}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		text, _ = body["text"].(string)
		if body["parse_mode"] != "HTML" {
			t.Fatalf("应使用 HTML 解析模式，得到 %v", body["parse_mode"])
		}
	})
	m := Message{Items: []Item{{
		Severity: "high",
		// 제목과 요약은 테스트 대상/모델 출력에서 오는 신뢰할 수 없는 내용이다.
		Name:    `<script>alert(1)</script>`,
		Summary: "a & b < c",
	}}}
	if _, err := (telegramChannel{}).Send(context.Background(),
		map[string]any{"bot_token": "tok", "chat_id": "1", "base_url": srv.URL}, m); err != nil {
		t.Fatalf("投递失败: %v", err)
	}
	if strings.Contains(text, "<script>") {
		t.Fatalf("未转义 HTML，存在注入: %q", text)
	}
	if !strings.Contains(text, "&lt;script&gt;") {
		t.Fatalf("期望转义后的实体，得到 %q", text)
	}
	if !strings.Contains(text, "a &amp; b") {
		t.Fatalf("& 未转义，得到 %q", text)
	}
}

func TestTelegramErrorClassification(t *testing.T) {
	rateLimited := capturePost(t, `{"ok":false,"error_code":429,"description":"Too Many Requests"}`, nil)
	_, err := (telegramChannel{}).Send(context.Background(),
		map[string]any{"bot_token": "tok", "chat_id": "1", "base_url": rateLimited.URL}, singleMsg())
	if err == nil || IsPermanent(err) {
		t.Fatalf("429 应可重试，得到 %v", err)
	}

	forbidden := capturePost(t, `{"ok":false,"error_code":403,"description":"bot was blocked by the user"}`, nil)
	_, err = (telegramChannel{}).Send(context.Background(),
		map[string]any{"bot_token": "tok", "chat_id": "1", "base_url": forbidden.URL}, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("403 是配置问题，应为永久失败，得到 %v", err)
	}
}

func TestWebhookDefaultTemplateProducesValidJSON(t *testing.T) {
	// 이 케이스가 기본 템플릿이 존재하는 이유다: 제목에 따옴표와 줄바꿈이 있을 때, 단순한
	// `"title": "{{.Title}}"` 형태의 작성은 모두 잘못된 JSON을 만든다. {{json .}}만이 이를 막는다.
	srv := capturePost(t, `{"ok":true}`, func(t *testing.T, body map[string]any, _ *http.Request) {
		if body["title"] != `[🟠 높음] 登录处 "SQL注入" 风险` {
			t.Errorf("标题未正确还原: %v", body["title"])
		}
		items, _ := body["items"].([]any)
		if len(items) != 1 {
			t.Fatalf("items 数量应为 1，得到 %d", len(items))
		}
		it, _ := items[0].(map[string]any)
		if it["summary"] != "参数 id\n未过滤 导致注入" {
			t.Errorf("摘要未正确还原: %v", it["summary"])
		}
		// 값은 문자열이 아니라 JSON 숫자여야 한다 (json:"...,string" 같은 태그가 이 함정을 밟는다).
		if _, ok := it["finding_id"].(float64); !ok {
			t.Errorf("finding_id 应为数字，得到 %T", it["finding_id"])
		}
	})
	if _, err := (webhookChannel{}).Send(context.Background(), map[string]any{"url": srv.URL}, singleMsg()); err != nil {
		t.Fatalf("投递失败: %v", err)
	}
}

func TestWebhookCustomTemplateAndHeaders(t *testing.T) {
	srv := capturePost(t, `{"ok":true}`, func(t *testing.T, body map[string]any, r *http.Request) {
		if r.Header.Get("X-Token") != "s3cret" {
			t.Errorf("自定义头丢失: %v", r.Header)
		}
		if body["msg"] != "3 条" {
			t.Errorf("自定义模板渲染有误: %v", body["msg"])
		}
		if body["first"] != "漏洞1" {
			t.Errorf("range 提取有误: %v", body["first"])
		}
	})
	cfg := map[string]any{
		"url":           srv.URL,
		"headers":       map[string]any{"X-Token": "s3cret"},
		"body_template": `{"msg": {{json (printf "%d 条" .Count)}}, "first": {{json (index .Items 0).Name}}}`,
	}
	if _, err := (webhookChannel{}).Send(context.Background(), cfg, batchMsg(3)); err != nil {
		t.Fatalf("投递失败: %v", err)
	}
}

func TestWebhookRejectsNonJSONRenderResult(t *testing.T) {
	cfg := map[string]any{"url": "https://example.com/hook", "body_template": `not json at all`}
	_, err := (webhookChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("渲染结果非 JSON 应为永久失败（模板写错了，重试无用），得到 %v", err)
	}
}

func TestWebhookValidateCatchesBadConfigEarly(t *testing.T) {
	bad := []map[string]any{
		{},
		{"url": "file:///etc/passwd"},
		{"url": "https://example.com", "method": "DELETE"},
		{"url": "https://example.com", "body_template": `{{.Items.`},
	}
	for i, cfg := range bad {
		if err := (webhookChannel{}).Validate(cfg); err == nil {
			t.Errorf("第 %d 组配置应被拒绝: %v", i, cfg)
		}
	}
}

func TestEmailMessageIsWellFormed(t *testing.T) {
	msg, err := buildEmailMessage("artex@example.com", []string{"a@example.com", "b@example.com"}, singleMsg())
	if err != nil {
		t.Fatalf("组装邮件失败: %v", err)
	}
	if !strings.HasPrefix(msg, "From: artex@example.com\r\n") {
		t.Fatalf("From 头有误:\n%s", msg)
	}
	if !strings.Contains(msg, "To: a@example.com, b@example.com\r\n") {
		t.Fatalf("To 头有误:\n%s", msg)
	}
	// 중국어 제목은 RFC 2047로 인코딩되어야 한다. 그렇지 않으면 클라이언트에서 깨져 보인다.
	if !strings.Contains(msg, "Subject: =?utf-8?") {
		t.Fatalf("主题未做 RFC 2047 编码:\n%s", msg)
	}
	if dec, err := new(mime.WordDecoder).DecodeHeader(mustExtractHeader(t, msg, "Subject")); err != nil {
		t.Fatalf("主题无法解码: %v", err)
	} else if !strings.Contains(dec, "SQL注入") {
		t.Fatalf("主题解码后内容有误: %q", dec)
	}

	// 본문은 base64이며, 디코딩하면 유효한 HTML이어야 한다.
	parts := strings.SplitN(msg, "\r\n\r\n", 2)
	if len(parts) != 2 {
		t.Fatal("邮件缺少头/体分隔")
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(strings.TrimSpace(parts[1]), "\r\n", ""))
	if err != nil {
		t.Fatalf("正文 base64 解码失败: %v", err)
	}
	html := string(decoded)
	if !strings.HasPrefix(html, "<div") {
		t.Fatalf("正文不是 HTML: %.80s", html)
	}
	// 제목이 텍스트 위치에 그대로 나타나야 한다: HTML 텍스트 내용의 큰따옴표는 유효한 문자라 이스케이프가 필요 없다.
	// 여기서 '그대로 보존'을 단언하는 이유는, 나중에 누군가 실수로 이스케이프 한 겹을 추가해 중국어 따옴표가
	// &quot;로 표시되는 것을 막기 위함이다.
	if !strings.Contains(html, `"SQL注入"`) {
		t.Fatalf("标题中的引号在文本位置应原样保留: %.200s", html)
	}
}

// TestEmailEscapesStructuralInjection은 이메일 본문에서 실제로 막아야 할 구조 주입을 검증한다:
// 취약점 제목과 요약은 테스트 대상과 모델 출력에서 오는 신뢰할 수 없는 내용이다. 텍스트 위치에서는 & < >를
// 반드시 이스케이프해야 하며(그렇지 않으면 태그를 주입할 수 있음), 속성 위치에서는 따옴표도 이스케이프해야 한다(그렇지 않으면 href를 닫을 수 있음).
func TestEmailEscapesStructuralInjection(t *testing.T) {
	m := Message{
		Items: []Item{{
			Severity:  "high",
			Name:      `<script>alert(1)</script>`,
			Summary:   "a & b > c",
			DetailURL: `https://artex.local/x?a="onmouseover=alert(1)`,
		}},
	}
	html := htmlBody(m, 0)
	if strings.Contains(html, "<script>") {
		t.Fatalf("标题未转义，可注入标签: %s", html)
	}
	if !strings.Contains(html, "&lt;script&gt;") {
		t.Fatalf("期望转义后的实体: %s", html)
	}
	if !strings.Contains(html, "a &amp; b &gt; c") {
		t.Fatalf("& 与 > 未转义: %s", html)
	}
	// 콜백 링크는 관리자가 설정하는 public_base_url이라 신뢰도가 비교적 높지만, 속성 위치에서는 여전히
	// 따옴표를 이스케이프해야 한다. 그렇지 않으면 따옴표가 든 주소가 href를 닫고 이벤트 핸들러를 주입할 수 있다.
	if strings.Contains(html, `onmouseover=alert(1)">`) {
		t.Fatalf("href 属性未正确转义: %s", html)
	}
	if !strings.Contains(html, "&quot;") {
		t.Fatalf("属性位置的引号应被转义: %s", html)
	}
}

func mustExtractHeader(t *testing.T, msg, name string) string {
	t.Helper()
	for _, line := range strings.Split(msg, "\r\n") {
		if strings.HasPrefix(line, name+": ") {
			return strings.TrimPrefix(line, name+": ")
		}
	}
	t.Fatalf("未找到 %s 头", name)
	return ""
}

func TestChannelValidateReportsMissingFields(t *testing.T) {
	// 검증 오류는 설정자에게 그대로 노출되므로, '설정이 잘못됨'처럼 뭉뚱그리지 말고 무엇이 빠졌는지 명확히 알려야 한다.
	cases := []struct {
		kind   string
		cfg    map[string]any
		substr string
	}{
		{KindDingTalk, map[string]any{}, "Webhook"},
		{KindFeishu, map[string]any{}, "Webhook"},
		{KindWeCom, map[string]any{}, "Webhook"},
		{KindTelegram, map[string]any{}, "Bot Token"},
		{KindTelegram, map[string]any{"bot_token": "t"}, "Chat ID"},
		{KindEmail, map[string]any{}, "SMTP"},
		{KindEmail, map[string]any{"host": "h"}, "포트"},
		{KindEmail, map[string]any{"host": "h", "port": 587, "from": "f"}, "수신자"},
	}
	for _, tc := range cases {
		ch, ok := Get(tc.kind)
		if !ok {
			t.Fatalf("渠道 %s 未注册", tc.kind)
		}
		err := ch.Validate(tc.cfg)
		if err == nil {
			t.Errorf("%s 配置 %v 应校验失败", tc.kind, tc.cfg)
			continue
		}
		if !strings.Contains(err.Error(), tc.substr) {
			t.Errorf("%s 的错误信息应提到 %q，得到 %q", tc.kind, tc.substr, err.Error())
		}
	}
}

// TestEmailSMTPErrorClassification은 SMTP 4xx/5xx의 의미 차이를 고정한다.
// 4xx까지 영구 실패로 판정하면, 그레이리스트를 사용하는 메일 서버에서는 모든 발송이 첫 시도 후
// failed 상태로 떨어진다. 그런데 그레이리스트야말로 자동 재시도가 가장 필요한 상황이다.
func TestEmailSMTPErrorClassification(t *testing.T) {
	cases := []struct {
		reply     string
		permanent bool
	}{
		{"450 4.7.1 Greylisting in action, please come back later", false},
		{"451 4.3.0 Temporary system failure", false},
		{"452 4.2.2 Mailbox full", false},
		{"550 5.1.1 User unknown", true},
		{"553 5.1.3 Bad address syntax", true},
		{"554 5.7.1 Relay access denied", true},
		// 응답 코드를 얻지 못하면 '재시도 가능'으로 처리한다. 일시적 장애를
		// 영구 실패로 확정하지 않기 위해서다. 더 시도하는 편이 낫기 때문이다.
		{"unexpected EOF", false},
		{"", false},
	}
	for _, tc := range cases {
		err := smtpStageError("收件人被拒", errors.New(tc.reply))
		if got := IsPermanent(err); got != tc.permanent {
			t.Errorf("应答 %q: 期望 permanent=%v 得到 %v", tc.reply, tc.permanent, got)
		}
		// 분류 방식과 관계없이 원문은 사용자가 원인을 파악할 수 있도록 반드시 보존한다.
		if tc.reply != "" && !strings.Contains(err.Error(), tc.reply) {
			t.Errorf("应答 %q 的原文被丢弃: %v", tc.reply, err)
		}
	}
}

func TestRegistryCoversAllKinds(t *testing.T) {
	// 여섯 개 채널은 하나라도 빠지면 안 된다. 하나라도 빠지면 UI 드롭다운에서 조용히 사라진다.
	want := []string{KindDingTalk, KindEmail, KindFeishu, KindTelegram, KindWebhook, KindWeCom}
	got := Kinds()
	if len(got) != len(want) {
		t.Fatalf("渠道数量应为 %d，得到 %d: %v", len(want), len(got), got)
	}
	for _, k := range want {
		if !ValidKind(k) {
			t.Errorf("渠道 %s 未注册", k)
		}
		if ch, ok := Get(k); !ok || ch.Kind() != k {
			t.Errorf("渠道 %s 的 Kind() 与注册键不一致", k)
		}
	}
	if ValidKind("nope") {
		t.Error("未注册的类型不应通过校验")
	}
}

func TestPermanentErrorUnwrap(t *testing.T) {
	base := &permanentSentinel{}
	err := Permanent(base)
	if !IsPermanent(err) {
		t.Fatal("应识别为永久失败")
	}
	if !strings.Contains(err.Error(), "sentinel") {
		t.Fatalf("错误信息应透传底层: %v", err)
	}
	if Permanent(nil) != nil {
		t.Fatal("Permanent(nil) 必须返回 nil")
	}
	if IsPermanent(nil) {
		t.Fatal("nil 不是永久失败")
	}
}

type permanentSentinel struct{}

func (*permanentSentinel) Error() string { return "sentinel" }
