package notify

import (
	"strings"
	"unicode/utf8"
)

const ellipsis = "…"

// TruncateBytes는 s를 최대 max 바이트까지 잘라내며, 결과가 유효한 UTF-8이고 문자를 중간에서 끊지 않도록 보장한다.
//
// 반드시 문자 경계에서 잘라야 하는 이유: 기업 위챗 그룹 로봇의 markdown은 4096 **바이트** 하드 상한이 있고(문자 수가 아니다),
// 중국어 한 글자는 3바이트이다. 바이트 단위로 바로 슬라이스하면 한자가 두 조각으로 잘려 잘못된 UTF-8이 만들어진다.
// 플랫폼은 그런 메시지를 통째로 거부하거나 깨진 글자로 표시한다. 여기서는 먼저 예산 위치에서 앞쪽으로 되돌아가
// 가장 가까운 rune 시작 바이트를 찾는다(utf8.RuneStart로 연속 바이트 0b10xxxxxx를 판별한다).
//
// max<=0이면 제한이 없다. 잘랐을 때는 생략 기호를 붙인다. 다만 max가 생략 기호조차 담을 수 없을 만큼 작으면 붙이지 않는다.
func TruncateBytes(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	budget := max - len(ellipsis)
	suffix := ellipsis
	if budget < 0 {
		// max가 생략 기호보다도 짧다: 생략 기호를 포기하고 단순히 자르며, 결과가 max를 넘지 않게 한다.
		budget = max
		suffix = ""
	}
	cut := budget
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + suffix
}

// OneLine은 여러 줄 텍스트를 한 줄로 줄인다. 모든 공백을 접은 뒤 문자 수 기준으로 자른다.
// IM 메시지의 제목 줄에 쓰인다. 요약에는 줄바꿈이 자주 들어 있어, 그대로 표나 제목에 넣으면 레이아웃이 깨진다.
// max<=0이면 길이 제한이 없다.
func OneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	return TruncateRunes(s, max)
}

// TruncateRunes는 s를 최대 max 글자(바이트가 아니라)까지 자르며, 잘렸을 때는 생략 기호를 붙인다.
// max<=0이면 제한이 없다.
//
// TruncateBytes와의 차이는 플랫폼의 기준에 있다. 기업 위챗은 바이트 기준으로 길이를 제한하고, Telegram은 글자 수 기준이다.
// 기준을 잘못 쓰면 오류는 나지 않고, 메시지가 예상보다 훨씬 짧게 잘린다(중국어 1글자 = 3바이트이므로,
// 4096을 바이트로 자르면 약 1365글자만 남는다). 그래서 두 함수는 모두 유지하고, 채널에 따라 골라 써야 한다.
func TruncateRunes(s string, max int) string {
	if max <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 1 {
		return string(runes[:max])
	}
	return string(runes[:max-1]) + ellipsis
}

// TruncateHTML은 HTML 조각을 글자 수 기준으로 자르며, 반쪽짜리 태그가 생기지 않도록 보장한다.
//
// HTML을 글자 단위로 바로 자르면 `<a href="htt` 같은 잘린 태그가 나온다. 플랫폼 파서는 이때 메시지 전체를 거부하거나,
// 뒤의 본문을 속성 값으로 삼켜 버린다. 그래서 여기서는 먼저 글자 기준으로 자른 뒤, 꼬리에 닫히지 않은 `<`가 있는지 확인하고,
// 있으면 그 앞으로 되돌아간다.
//
// 태그 짝 맞추기(</b> 같은 것을 보충하는 작업)는 하지 않는다. Telegram의 HTML 파서는 닫히지 않은 태그를 자동으로 닫아 주며,
// 직접 짝 맞추기를 구현하면 속성 안의 따옴표, 주석, 자기 닫힘 태그까지 처리해야 해서, 복잡도에 비해 얻는 것이 적다.
func TruncateHTML(s string, max int) string {
	if max <= 0 || len([]rune(s)) <= max {
		return s
	}
	cut := TruncateRunes(s, max)
	// 꼬리가 `<`로 시작하는 잘린 조각이면(마지막 `<` 뒤에 `>`가 없으면) `<` 앞으로 되돌아간다.
	if lt := strings.LastIndex(cut, "<"); lt >= 0 && !strings.Contains(cut[lt:], ">") {
		cut = cut[:lt]
	}
	// 꼬리가 잘린 HTML 엔티티(예: `&amp;`가 `&amp`로 잘린 것)이면 마찬가지로 되돌아간다.
	// 엔티티 조각은 엔티티만 인식하는 파서에서 **메시지 전체**를 거부하게 만들 수 있다. 길이 상한을 넘는 요약 메시지는
	// 원래 흔하므로, 그 때문에 알림 하나를 통째로 잃을 이유는 없다.
	if amp := strings.LastIndex(cut, "&"); amp >= 0 && !strings.Contains(cut[amp:], ";") {
		cut = cut[:amp]
	}
	return cut
}

// packItemCount는 예산 안에 항목을 **완전히** 담을 수 있는 개수를 계산하며, 요약 메시지를 항목 단위로 패킹할 때 쓴다.
//
// 완성된 전체 본문을 렌더링한 뒤 자르지 않고 항목 단위로 패킹하는 이유: 자르면 뒤쪽 항목이 흔적 없이 사라지는데,
// 그 항목들의 전달 기록은 여전히 전달 완료로 표시된다. 메시지에서도, 전달 이력에서도 드러나지 않으므로 취약점이 그대로 묻힌다.
// 항목 단위로 패킹하면, 들어가지 않은 항목은 DB에 남아 다음 배치가 되고, 호출자가 받는 kept는 이 메시지에서 실제로 전달된 건수가 된다.
//
// 매개변수: maxSize<=0이면 제한이 없다. reserve는 머리/꼬리에 예약하는 양이다.
// size는 계량을 담당한다. 플랫폼마다 기준이 다르다(기업 위챗/딩톡은 바이트, Telegram은 글자 수).
// 기준을 잘못 쓰면 오류는 나지 않고, 중국어 메시지가 상한보다 훨씬 작게 눌린다.
// render는 idx번째 항목을 실제 텍스트로 렌더링한다. 길이는 내용에 따라 다르므로 추정으로 처리할 수 없다.
//
// 최소 1을 반환한다(항목이 남아 있는 한). 단건이 극단적으로 길더라도 이 한 건은 보내야 한다.
// 그렇지 않으면 초장문 취약점 하나 때문에 배치 전체가 영구히 그 자리에 멈춘다. 최종 잘라내기는 호출자가 담당한다.
func packItemCount(items []Item, maxSize, reserve int, footer string, size func(string) int, render func(Item, int) string) int {
	if maxSize <= 0 {
		return len(items)
	}
	budget := maxSize - reserve - size(footer)
	if budget < 0 {
		budget = 0
	}
	used := 0
	for i, it := range items {
		used += size(render(it, i))
		if used > budget && i > 0 {
			return i
		}
	}
	return len(items)
}

// byteSize / runeSize는 packItemCount의 두 가지 계량 기준이다. 이름을 붙여 두어, 호출하는 곳에 날 func(s string) int
// 클로저가 나타나지 않게 한다. 그렇지 않으면 어떤 기준을 쓰는지 한눈에 알기 어렵다.
func byteSize(s string) int { return len(s) }
func runeSize(s string) int { return utf8.RuneCountInString(s) }

// assetLine은 자산 목록을 한 줄짜리 표시 텍스트로 렌더링한다. limit개를 넘으면 나머지는 생략하고 총수를 표기한다.
// 하나의 취약점은 수십 개의 자산에 연결될 수 있으며, 전부 나열하면 메시지가 넘친다.
func assetLine(assets []string, limit int) string {
	if len(assets) == 0 {
		return ""
	}
	if limit <= 0 || len(assets) <= limit {
		return strings.Join(assets, ", ")
	}
	return strings.Join(assets[:limit], ", ") + " 등 " + itoa(len(assets)) + "개"
}

// itoa는 strconv.Itoa의 짧은 별칭이며, 표시 텍스트를 이어 붙일 때만 쓴다. 곳곳에서 strconv를 import하지 않으려고 둔 것이다.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
