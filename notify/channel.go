package notify

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
)

// Channel은 알림 채널의 어댑터이다. 구현은 **무상태**여야 한다. 같은 인스턴스가
// 여러 채널 설정에서 동시에 재사용되며, 자격 증명은 모두 cfg 매개변수로 전달한다.
type Channel interface {
	// Kind는 채널 종류 식별자를 반환한다. 레지스트리의 키와 일치해야 한다.
	Kind() string
	// Validate는 설정을 저장할 때 호출되어 필수 필드와 형식을 검증한다. 반환된 오류는
	// 설정자에게 그대로 표시되므로, 「설정이 잘못됨」처럼 뭉뚱그리지 말고 「어떤 필드가
	// 없는지」를 설명해야 한다.
	Validate(cfg map[string]any) error
	// Send는 메시지를 한 번 전달하고, **실제로 전달된 항목 수**와 오류를 반환한다.
	//
	// 항목 수를 반환하는 이유: 각 플랫폼에는 메시지 길이 상한이 있어, 요약 메시지가
	// 배치 전체를 담지 못하면 잘린다. 호출자가 배치 전체를 무조건 전달 완료로 표시하면
	// 잘려 나간 항목은 사라진다. 메시지에서도 보이지 않고 전달 이력에는 성공으로 남아,
	// 누락을 발견할 방법이 없다. kept를 반환받으면 호출자는 앞의 kept개만 표시하고,
	// 나머지는 다음 배치로 넘긴다.
	//
	// 오류를 반환하면 전달 실패이며, 그중 *PermanentError는 재시도하지 않아야 함을 뜻한다.
	// 실패 시 kept는 의미가 없으므로 호출자는 무시해야 한다.
	Send(ctx context.Context, cfg map[string]any, m Message) (int, error)
	// DefaultRatePerMin은 해당 채널이 공식적으로 권장하는 분당 전송 상한을 반환하며,
	// 새 채널 인스턴스를 만들 때의 기본 속도 제한값으로 쓰인다. 0을 반환하면 알려진
	// 제한이 없다는 뜻이다.
	DefaultRatePerMin() int
	// SecretKeys는 해당 채널 설정에서 자격 증명에 해당하는 키 이름을 반환한다. API 응답에서
	// 이 키들의 값은 마스킹되고, 업데이트 시 마스킹된 값을 받으면 DB의 원래 값을 유지한다.
	// 어떤 필드가 자격 증명인지는 구현체만 정확히 알고 있다(기업 위챗은 Webhook 주소 전체가
	// 자격 증명이고, 딩톡은 그중 secret만 해당한다). 따라서 이 정보는 반드시 채널이
	// 제공해야 하며, 상위 계층이 추측해서는 안 된다.
	SecretKeys() []string
	// DestinationKeys는 해당 채널 설정에서 「메시지가 어디로 가는지」를 결정하는 키 이름을
	// 반환한다.
	//
	// SecretKeys와 마찬가지로 보안과 직결된다. 목적지 주소와 자격 증명은 서로 독립된 필드이므로,
	// 「주소만 바꾸고 자격 증명은 그대로 유지」를 허용하면 채널 설정을 수정할 수 있는 누구든
	// DB에 저장된 실제 자격 증명을 자신이 통제하는 서버로 보내게 된다. 그러면 설정 마스킹은
	// 의미를 완전히 잃는다. 자세한 내용은 PrepareConfigUpdate를 참고하라.
	DestinationKeys() []string
}

// registry는 채널 레지스트리이다. init()을 이용한 자기 등록 대신 명시적 리터럴을 쓴다.
// 그래야 「어떤 채널이 있는지」를 한 곳에서 전부 볼 수 있고, 새 채널을 추가했을 때 누락이
// 런타임 부작용이 아니라 컴파일 시점에 드러난다.
var registry = map[string]Channel{
	KindDingTalk: dingTalkChannel{},
	KindFeishu:   feishuChannel{},
	KindWeCom:    weComChannel{},
	KindWebhook:  webhookChannel{},
	KindTelegram: telegramChannel{},
	KindEmail:    emailChannel{},
}

// Get은 종류에 따라 채널 구현을 가져온다.
func Get(kind string) (Channel, bool) {
	c, ok := registry[kind]
	return c, ok
}

// ValidKind는 kind가 지원되는 채널 종류인지 보고한다.
func ValidKind(kind string) bool {
	_, ok := registry[kind]
	return ok
}

// Kinds는 지원되는 모든 채널 종류를 사전순으로 정렬해 반환한다(UI 드롭다운을 안정적으로 표시하기 위함).
func Kinds() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// PermanentError는 재시도하지 않아야 할 전달 실패를 표시한다. 자격 증명 오류, 목적지 거부,
// 잘못된 요청 본문 등이 해당한다. 재시도는 일시적 장애(네트워크 흔들림, 제한, 상대 측 5xx)에만
// 의미가 있다. 영구적 실패를 백오프 재시도하면 성공하지도 않으면서 진짜 오류가 재시도 로그에
// 묻혀 버린다.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent는 err를 영구적 실패로 표시한다. err가 nil이면 nil을 반환하므로,
// `return Permanent(someCheck())` 형태로 쓸 수 있다.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &PermanentError{Err: err}
}

// IsPermanent는 err 체인에 영구적 실패 표시가 있는지 보고한다.
func IsPermanent(err error) bool {
	var pe *PermanentError
	return errors.As(err, &pe)
}

// ---- 설정 읽기 헬퍼 ----
//
// 채널 설정은 DB의 JSONB 컬럼에서 오며, encoding/json으로 역직렬화하면 map[string]any가 된다.
// 숫자는 항상 float64, 배열은 []any이다. 아래 헬퍼들은 이 변환을 통일하고, 사용자가 UI에서
// 비워 두었거나 타입이 어긋난 경우(예: 포트를 문자열로 입력한 경우)도 허용한다.

// cfgString은 문자열 설정 항목을 가져오며, 앞뒤 공백은 모두 잘라낸다. 웹 폼에서 복사해 붙여 넣을 때 쉽게 딸려 오기 때문이다.
func cfgString(cfg map[string]any, key string) string {
	v, ok := cfg[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

// cfgInt는 정수 설정 항목을 가져오며, float64(JSON 기본값)와 문자열 두 출처를 모두 지원한다.
func cfgInt(cfg map[string]any, key string) int {
	switch v := cfg[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}

// cfgBool은 불리언 설정 항목을 가져오며, 문자열 "true"/"1"도 허용한다.
func cfgBool(cfg map[string]any, key string) bool {
	switch v := cfg[key].(type) {
	case bool:
		return v
	case string:
		s := strings.ToLower(strings.TrimSpace(v))
		return s == "true" || s == "1" || s == "yes"
	default:
		return false
	}
}

// cfgStrings는 문자열 배열 설정 항목을 가져오며, 공백을 자동으로 잘라내고 빈 문자열은 버린다.
func cfgStrings(cfg map[string]any, key string) []string {
	raw, ok := cfg[key].([]any)
	if !ok {
		// 값이 하나뿐인 폼 제출을 위해 단일 문자열도 허용한다.
		if s := cfgString(cfg, key); s != "" {
			return []string{s}
		}
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			continue
		}
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// cfgMap은 문자열 맵 설정 항목(예: 사용자 정의 HTTP 헤더)을 가져오며, 키와 값의 공백을 모두 잘라내고 빈 키는 버린다.
func cfgMap(cfg map[string]any, key string) map[string]string {
	raw, ok := cfg[key].(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		s, ok := v.(string)
		if !ok {
			continue
		}
		out[k] = s
	}
	return out
}
