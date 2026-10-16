package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// weComMarkdownLimit은 기업 위챗 그룹 로봇 markdown content의 하드 상한이다(문자가 아니라 바이트 단위).
// 여섯 개 채널 가운데 가장 엄격한 제한이며, TruncateBytes가 존재하는 주된 이유이기도 하다.
const weComMarkdownLimit = 4096

// weComChannel은 기업 위챗 그룹 로봇을 구현한다.
//
// 플랫폼 특성:
//   - URL의 key로만 인증하며 서명은 지원하지 않는다. 따라서 webhook 주소 자체가 자격 증명 전체이다.
//   - markdown content 상한은 4096 **바이트**이며, 초과하면 잘리지 않고 메시지 전체가 거부된다. 중국어는 글자당 3바이트이므로,
//     본문은 천 자 남짓만 쓸 수 있고 클라이언트에서 반드시 잘라야 한다.
//   - 제한은 분당 20건이며, 이 역시 클라이언트 제한으로 막는다.
type weComChannel struct{}

func (weComChannel) Kind() string { return KindWeCom }

func (weComChannel) DefaultRatePerMin() int { return 20 }

// 기업 위챗에는 Webhook이라는 자격 증명 하나만 있으며(URL의 key), 서명을 지원하지 않는다.
// 주소 전체가 곧 자격 증명이므로, 마스킹해야 할 다른 필드는 없다.
func (weComChannel) SecretKeys() []string { return []string{"webhook"} }

// 위챗에는 Webhook 필드 하나만 있으며, 이것이 목적지이자 자격 증명이다. 따라서 「주소를 바꾼 뒤에 남는 자격 증명」이라는 문제가 없다.
func (weComChannel) DestinationKeys() []string { return []string{"webhook"} }

func (weComChannel) Validate(cfg map[string]any) error {
	hook := cfgString(cfg, "webhook")
	if hook == "" {
		return errors.New("Webhook 주소가 없습니다")
	}
	if err := validateHTTPURL(hook); err != nil {
		return fmt.Errorf("Webhook 주소가 올바르지 않습니다: %w", err)
	}
	return nil
}

func (c weComChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	// 요약 배치는 매우 길 수 있다(50건 × 한 줄씩 + 머리말). 4096바이트는 쉽게 넘는다.
	// 플랫폼이 오류를 내게 두지 않고 여기서 자른다. 거부되면 이 배치 전체가 유실되지만, 잘라서 보내면 앞부분 몇 건이라도 전달된다.
	content, kept := markdownBody(m, weComMarkdownLimit)
	payload := map[string]any{
		"msgtype":  "markdown",
		"markdown": map[string]any{"content": content},
	}
	raw, err := doJSON(ctx, "POST", cfgString(cfg, "webhook"), nil, payload)
	if err != nil {
		return 0, err
	}
	var res struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, fmt.Errorf("WeCom 응답을 해석하지 못했습니다: %w (%s)", err, snippet(raw))
	}
	if res.ErrCode != 0 {
		// 45009는 인터페이스 호출 횟수 초과이다. 플랫폼의 제한 창은 굴러가므로 백오프 후 재시도가 효과가 있다.
		// 따라서 명시적으로 재시도 가능으로 분류한다. 여기까지 왔다는 것은 클라이언트의 rate_per_min 설정이 너무 공격적이라는 뜻이다.
		// 재시도는 최후의 보루일 뿐이며, 진짜 해결책은 이 채널의 제한 값을 낮추는 것이다.
		if res.ErrCode == 45009 {
			return 0, fmt.Errorf("WeCom 요청이 제한되었습니다 (%d): %s", res.ErrCode, res.ErrMsg)
		}
		// 93000은 webhook key가 유효하지 않다는 뜻이다. 영구적 실패이며, 재시도해도 회복되지 않는다.
		return 0, Permanent(fmt.Errorf("WeCom에서 오류가 발생했습니다 (%d): %s", res.ErrCode, res.ErrMsg))
	}
	return kept, nil
}
