package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// telegramTextLimit은 Telegram sendMessage의 text 필드 상한이다(글자 수 기준).
const telegramTextLimit = 4096

// telegramChannel은 Telegram Bot API를 구현한다.
//
// 플랫폼 특성:
//   - 인증은 모두 URL 경로에 있다(/bot<token>/sendMessage). 서명은 필요 없다.
//   - MarkdownV2 대신 HTML 파싱 모드를 쓴다. MarkdownV2는 `_*[]()~`>#+-=|{}.!` 등 18개 문자를 모두 이스케이프해야 하며,
//     하나라도 빠지면 메시지 전체가 거부된다. HTML은 & < > 세 개만 이스케이프하면 된다.
//   - 업무 오류도 HTTP 200 안에 숨어 있으므로, ok 필드로 판단한다.
type telegramChannel struct{}

func (telegramChannel) Kind() string { return KindTelegram }

// Telegram 개인 대화는 약 초당 1건, 그룹은 분당 20건이다. 보수적인 값을 취한다.
func (telegramChannel) DefaultRatePerMin() int { return 20 }

// Bot Token은 완전한 자격 증명이다. chat_id는 수신자일 뿐이며 비밀로 보지 않는다(이것만 있어서는 Token 없이 메시지를 보낼 수 없다).
func (telegramChannel) SecretKeys() []string { return []string{"bot_token"} }

// base_url은 Token을 어느 API 엔드포인트로 보낼지 결정한다(예: 자체 구축 리버스 프록시). 이것을 바꾸면 Token을 다시 명시해야 한다.
func (telegramChannel) DestinationKeys() []string { return []string{"base_url"} }

func (telegramChannel) Validate(cfg map[string]any) error {
	if cfgString(cfg, "bot_token") == "" {
		return errors.New("Bot Token이 없습니다")
	}
	if cfgString(cfg, "chat_id") == "" {
		return errors.New("Chat ID가 없습니다")
	}
	if base := cfgString(cfg, "base_url"); base != "" {
		if err := validateHTTPURL(base); err != nil {
			return fmt.Errorf("API 주소가 올바르지 않습니다: %w", err)
		}
	}
	return nil
}

func (c telegramChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	endpoint, err := telegramEndpoint(cfg)
	if err != nil {
		return 0, Permanent(err)
	}
	text, kept := telegramHTML(m)
	payload := map[string]any{
		"chat_id":                  cfgString(cfg, "chat_id"),
		"text":                     text,
		"parse_mode":               "HTML",
		"disable_web_page_preview": false,
	}
	raw, err := doJSON(ctx, "POST", endpoint, nil, payload)
	if err != nil {
		return 0, err
	}
	var res struct {
		OK          bool   `json:"ok"`
		ErrorCode   int    `json:"error_code"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("Telegram 응답을 해석하지 못했습니다: %w (%s)", err, snippet(raw))
	}
	if res.OK {
		return kept, nil
	}
	// 429는 제한이므로 백오프 후 재시도가 효과가 있다. 그 밖(400 매개변수 오류, 401 token 오류, 403 차단됨,
	// 404 chat 없음)은 모두 설정 문제이므로 재시도해도 회복되지 않는다.
	if res.ErrorCode == 429 {
		return 0, fmt.Errorf("Telegram 요청 제한에 걸렸습니다: %s", res.Description)
	}
	return 0, Permanent(fmt.Errorf("Telegram 응답 오류 %d: %s", res.ErrorCode, res.Description))
}

// telegramEndpoint는 sendMessage 주소를 조합한다. base_url이 비어 있으면 공식 API를 쓰고,
// 비어 있지 않으면 자체 구축 Bot API 리버스 프록시에 쓴다(국내 네트워크 환경에서 흔한 요구이다).
func telegramEndpoint(cfg map[string]any) (string, error) {
	base := cfgString(cfg, "base_url")
	if base == "" {
		base = "https://api.telegram.org"
	}
	base = strings.TrimSuffix(base, "/")
	token := cfgString(cfg, "bot_token")
	raw := base + "/bot" + token + "/sendMessage"
	u, err := url.Parse(raw)
	if err != nil {
		// err를 그대로 전달하지 않는다. 주소에는 Bot Token이 들어 있으며, 이 시점에는 addr조차 되돌려주면 안 된다.
		return "", fmt.Errorf("API 주소를 조합하지 못했습니다 (API 주소: %s)", redactRequestTarget(base))
	}
	return u.String(), nil
}

// telegramHTML은 HTML 본문을 렌더링하며, 본문과 실제로 기록된 항목 수를 반환한다(Channel.Send 참고).
func telegramHTML(m Message) (string, int) {
	var b strings.Builder
	b.WriteString("<b>" + telegramEscape(markdownTitle(m)) + "</b>\n")
	if m.Batch {
		// Telegram의 상한은 **글자 수**이므로, 패킹도 글자 기준(runeSize)으로 센다.
		footer := ""
		if m.HomeURL != "" {
			footer = fmt.Sprintf("\n\n<a href=\"%s\">플랫폼에서 전체 보기</a>", telegramEscapeAttr(m.HomeURL))
		}
		kept := packItemCount(m.Items, telegramTextLimit, telegramReservedRunes, footer, runeSize, func(it Item, idx int) string {
			return telegramBatchLine(it, idx+1)
		})
		items := m.Items[:kept]
		b.Reset()
		b.WriteString("<b>" + telegramEscape(telegramBatchTitle(m, items, len(m.Items))) + "</b>")
		for i, it := range items {
			b.WriteString("\n" + telegramEscape(telegramBatchLine(it, i+1)))
		}
		b.WriteString(footer)
		return TruncateHTML(b.String(), telegramTextLimit), kept
	}
	if len(m.Items) == 0 {
		return b.String(), 0
	}
	it := m.Items[0]
	if it.IsStatusChange() {
		b.WriteString(fmt.Sprintf("\n<b>상태 변경</b>: %s → %s",
			telegramEscape(StatusLabel(it.FromStatus)), telegramEscape(StatusLabel(it.ToStatus))))
	}
	if it.VulnClass != "" && it.VulnClass != it.Title() {
		b.WriteString("\n<b>유형</b>: " + telegramEscape(it.VulnClass))
	}
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		b.WriteString("\n<b>자산</b>: " + telegramEscape(a))
	}
	if s := OneLine(it.Summary, maxSummaryRunes); s != "" {
		b.WriteString("\n<b>개요</b>: " + telegramEscape(s))
	}
	if it.DetailURL != "" {
		b.WriteString(fmt.Sprintf("\n\n<a href=\"%s\">상세 보기</a>", telegramEscapeAttr(it.DetailURL)))
	}
	return TruncateHTML(b.String(), telegramTextLimit), 1
}

// telegramReservedRunes는 메시지 제목과 가능한 잘림 안내를 위해 예약하는 크기이다(글자 수 기준).
const telegramReservedRunes = 160

// telegramBatchLine은 요약 안의 항목 한 줄을 렌더링한다(이스케이프되지 않은 상태이며, 호출자가 일괄 이스케이프한다).
func telegramBatchLine(it Item, idx int) string {
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		return fmt.Sprintf("%d. %s · %s — %s", idx, SeverityLabel(it.Severity), it.Title(), a)
	}
	return fmt.Sprintf("%d. %s · %s", idx, SeverityLabel(it.Severity), it.Title())
}

// telegramBatchTitle은 요약 메시지의 제목 줄을 렌더링한다. 건수는 이 메시지에 **실제로 들어간** 건수를 쓰며,
// 배치 전체 건수를 쓰지 않는다. 그렇지 않으면 읽는 사람은 메시지 머리의 숫자를 전체라고 믿게 된다.
func telegramBatchTitle(m Message, items []Item, total int) string {
	title := fmt.Sprintf("취약점 요약 · 총 %d건", total)
	if extra := total - len(items); extra > 0 {
		title += fmt.Sprintf(" (앞 %d건만 표시, 나머지 %d건은 다음 메시지에서 이어집니다)", len(items), extra)
	}
	if m.WindowMinutes > 0 {
		title = fmt.Sprintf("최근 %d분간 · %s", m.WindowMinutes, title)
	}
	return title
}

// telegramEscape는 HTML 텍스트 내용을 이스케이프한다.
// Telegram은 이 세 가지 엔티티만 인식한다. 이미 있는 &amp; 같은 엔티티도 다시 이스케이프되는데, 이것이 바로 원하는 동작이다.
// 우리가 보여 주려는 것은 원본 문자이지, 사용자가 HTML을 주입하게 두는 것이 아니기 때문이다.
func telegramEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// telegramEscapeAttr는 HTML 속성 값을 이스케이프한다. 텍스트 이스케이프에 더해 따옴표도 처리해야 한다.
// URL에 따옴표가 있으면 href 속성이 일찍 닫혀, 뒤의 내용이 주입 지점이 되어 버린다.
func telegramEscapeAttr(s string) string {
	s = telegramEscape(s)
	s = strings.ReplaceAll(s, "\"", "&quot;")
	return s
}
