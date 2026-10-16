package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"
)

// dingTalkChannel은 딩톡 사용자 정의 로봇을 구현한다.
//
// 플랫폼 특성(아래 구현의 선택을 결정한다):
//   - 단일 로봇의 제한은 분당 20건이며, 초과 전송은 조용히 버려진다(HTTP는 여전히 200일 수 있다).
//     따라서 제한은 클라이언트에서 해야 한다. DefaultRatePerMin을 참고하라.
//   - 보안 설정은 세 가지 중 하나를 고른다: 서명 / 사용자 정의 키워드 / IP 화이트리스트.
//     서명은 메시지 내용에 의존하지 않는 유일한 방식이므로, 서명만 지원한다(세 가지를 모두 끈 순수 webhook도 지원한다).
//   - 성공/실패 모두 HTTP 200을 반환하며, 본문의 errcode로 구분한다. errcode를 확인하지 않으면
//     전달 실패를 성공으로 기록하게 된다.
type dingTalkChannel struct{}

func (dingTalkChannel) Kind() string { return KindDingTalk }

func (dingTalkChannel) DefaultRatePerMin() int { return 20 }

// 딩톡의 Webhook 주소에는 access_token이 들어 있어 그 자체가 자격 증명이므로, 전체를 마스킹한다.
func (dingTalkChannel) SecretKeys() []string { return []string{"webhook", "secret"} }

// 목적지는 딩톡 Webhook 주소 자체이며, 주소를 바꾸면 새 주소에 대해서도 서명 키를 다시 명시해야 한다.
func (dingTalkChannel) DestinationKeys() []string { return []string{"webhook"} }

func (dingTalkChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("Webhook 주소가 없습니다")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("Webhook 주소가 올바르지 않습니다: %w", err)
	}
	return nil
}

// Send는 메시지를 한 번 전달한다. 상세 링크가 있고 단건일 때는 ActionCard(버튼 포함)를, 그 밖에는 markdown을 쓴다.
func (c dingTalkChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	hook := cfgString(cfg, "webhook")
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	endpoint, err := dingTalkSignedURL(hook, cfgString(cfg, "secret"), time.Now())
	if err != nil {
		return 0, Permanent(err)
	}

	title := markdownTitle(m)
	// 딩톡 markdown 본문에는 명시적인 바이트 상한이 없지만, 증거 필드가 비정상적으로 커지는 것을 막기 위해 상한을 둔다.
	text, kept := markdownBody(m, 20000)

	var payload any
	if !m.Batch && len(m.Items) == 1 && m.Items[0].DetailURL != "" {
		payload = map[string]any{
			"msgtype": "actionCard",
			"actionCard": map[string]any{
				"title":          title,
				"text":           text,
				"btnOrientation": "0",
				"singleTitle":    "상세 보기",
				"singleURL":      m.Items[0].DetailURL,
			},
		}
	} else {
		payload = map[string]any{
			"msgtype":  "markdown",
			"markdown": map[string]any{"title": title, "text": text},
		}
	}

	raw, err := doJSON(ctx, "POST", endpoint, nil, payload)
	if err != nil {
		return 0, err
	}
	// 딩톡은 업무 오류를 200 응답 안에 숨긴다.
	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("DingTalk 응답을 해석하지 못했습니다: %w (%s)", err, snippet(raw))
	}
	if res.ErrCode != 0 {
		// 301000은 서명 검증 실패, 310000은 키워드 불일치이다. 둘 다 설정 오류이므로,
		// 재시도해도 저절로 회복되지 않는다.
		return 0, Permanent(fmt.Errorf("DingTalk에서 오류가 발생했습니다 (%d): %s", res.ErrCode, res.ErrMsg))
	}
	return kept, nil
}

// dingTalkSignedURL은 공식 서명 규칙에 따라 webhook에 timestamp와 sign 매개변수를 덧붙인다.
//
// 규칙: 서명 대상 문자열 = timestamp + "\n" + secret이며, HMAC-SHA256의 **키도 secret**이다.
// 결과는 base64 인코딩 후 URL 인코딩한다. timestamp는 밀리초이다. secret이 비어 있으면 그대로 반환하며,
// 서명을 켜지 않은 로봇을 지원하기 위함이다.
func dingTalkSignedURL(hook, secret string, now time.Time) (string, error) {
	if secret == "" {
		return hook, nil
	}
	ts := strconv.FormatInt(now.UnixMilli(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "\n" + secret))
	sign := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	u, err := url.Parse(hook)
	if err != nil {
		// err를 그대로 전달하지 않는다. url.Parse의 오류 문자열에는 전체 주소(access_token 포함)가 들어 있다.
		return "", fmt.Errorf("Webhook 주소를 해석하지 못했습니다: %s", redactRequestTarget(hook))
	}
	q := u.Query()
	q.Set("timestamp", ts)
	q.Set("sign", sign)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// validateHTTPURL은 주소가 사용 가능하고 프로토콜이 지원되는지 검증하며, 리터럴 IP 대상에 대해서는 내부망 여부를 판단한다.
//
// 두 가지를 신경 써서 처리한다.
//
//  1. **오류 정보는 반드시 마스킹해야 한다.** url.Parse가 직접 반환하는 것은 *url.Error이며, 그 Error()에는
//     **원래 주소 전체**가 들어 있다. 이 기능의 해당 서비스 주소에는 자격 증명이 들어 있다(딩톡 access_token,
//     기업 위챗 key, Telegram bot token, 페이슈 hook id). 과거에는 여기서 `return err`를 그대로 했고,
//     「주소 형식이 잘못됨」 오류가 자격 증명을 실어 테스트 API의 400 응답, 매 전달마다 DB에 기록되는 last_error,
//     서버 로그, 전달 이력 API로 흘러갔다.
//
//  2. **리터럴 IP는 여기서 내부망 여부를 바로 판단하고, 도메인은 다이얼 단계에 맡긴다**(blockInternalDial이
//     최종 적용 지점이며 DNS 리바인딩도 막는다). 여기서 한 번 검사하는 이유는, 첫 전달이 실패할 때까지 기다리지 않고
//     설정을 저장할 때 바로 안내를 받기 위해서다.
//
// 프로토콜을 제한하는 것은 방어적 조치이다. file:///gopher:// 같은 주소는 http.Client에서 예기치 않은 동작을 일으킬 수 있다.
// scheme 검사로 막혀 있지만, 이 표면을 열어 둘 이유는 없다.
func validateHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("주소를 해석할 수 없습니다 (%s)", redactRequestTarget(raw))
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("http 또는 https 만 지원합니다. 받은 값: %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("호스트 이름이 없습니다")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && isBlockedDialIP(ip) && !allowLocalTargets() {
		return fmt.Errorf("로컬 또는 링크 로컬 주소 %s 로는 전송하지 않습니다 (로컬 서비스로 보내야 하면 %s=1 로 설정하세요)", ip, AllowLocalTargetsEnv)
	}
	return nil
}
