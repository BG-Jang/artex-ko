package notify

import (
	"fmt"
	"strings"
)

// 이 파일은 「Markdown 계열」 채널(딩톡, 기업 위챗)이 공유하는 메시지 렌더링이다.
// 페이슈는 카드 JSON을, Telegram과 이메일은 HTML을 쓰며, 각 어댑터가 자체적으로 렌더링한다.

// maxAssetsShown은 메시지에 최대 몇 개의 자산을 나열할지 정한다. 하나의 취약점은 수십 개의 자산에 연결될 수 있는데,
// 전부 나열하면 메시지가 넘치고 정보 가치도 없다. 네 번째 이후의 도메인은 IM에서 아무도 보지 않는다.
const maxAssetsShown = 3

// maxSummaryRunes는 요약을 몇 글자로 압축할지 정한다. IM 메시지는 「상세를 보러 가라」는 안내이지,
// 보고서 본문이 아니다. 전체 내용은 플랫폼에 있다.
const maxSummaryRunes = 120

// markdownReservedBytes는 메시지 머리(요약 줄 + 심각도 분포 + 가능한 잘림 안내)와 꼬리(플랫폼 링크)에 예약하는 크기이다.
// 메시지 전체를 패킹할 때 이 부분을 예산에서 미리 빼서 머리와 꼬리가 잘리지 않도록 보장한다.
// 머리나 꼬리가 잘리면 읽는 사람은 「이것이 몇 번째 배치인지, 표시되지 않은 항목이 몇 개 남았는지」조차 알 수 없다.
const markdownReservedBytes = 320

// markdownEscape는 markdown 메타 문자를 이스케이프한다.
//
// 반드시 해야 하는 이유: 취약점 제목, 요약, 유형, 자산 표시 이름은 모두 **신뢰할 수 없는 출처**에서 온다.
// 제목과 요약은 모델의 출력에서 나오고(모델은 테스트 대상의 응답을 읽는다), 자산의 url은 스캔으로 얻은
// 전체 URL이며 대상이 통제하는 쿼리 문자열을 포함할 수 있다. 이스케이프하지 않으면, 제목이 다음과 같은 취약점은
//
//	로그인 입구 SQL 주입\n[긴급: 여기를 눌러 계정 확인](http://attacker.tld)
//
// 보안 엔지니어의 딩톡/페이슈에서 **클릭 가능한 외부 링크**로 렌더링된다. 또 `![](http://attacker.tld/beacon)`은
// 렌더링 시 클라이언트가 가져가므로, 「이 취약점을 봤다」고 알리는 것이며 읽는 사람의 IP도 새어 나간다.
// 악의가 없는 내용이라도, 삽입된 굵은 글씨나 인용 블록이 아래의 심각한 취약점을 접힌 선 밖으로 밀어낼 수 있다.
//
// 이스케이프 대상은 제목/링크/강조/목록/인용/취소선처럼 구조를 바꾸거나 클릭 가능한 요소를 만드는 문자들이다.
// `\`를 가장 먼저 처리해야 한다. 그렇지 않으면 뒤에서 붙인 백슬래시를 다시 이스케이프하게 된다.
func markdownEscape(s string) string {
	replacer := strings.NewReplacer(
		`\`, `\\`,
		"`", "\\`",
		"*", `\*`,
		"_", `\_`,
		"[", `\[`,
		"]", `\]`,
		"(", `\(`,
		")", `\)`,
		"!", `\!`,
		"#", `\#`,
		">", `\>`,
		"|", `\|`,
		"~", `\~`,
	)
	return replacer.Replace(s)
}

// markdownText는 신뢰할 수 없는 텍스트를 한 줄로 줄이고 이스케이프하여 markdown 본문에 쓸 수 있게 한다.
// 한 줄화는 이스케이프와 짝을 이루는 나머지 절반이다. 줄바꿈 자체가 새 목록 항목이나 인용 블록을 위조할 수 있고,
// 이스케이프 문자로는 이를 막을 수 없기 때문이다.
func markdownText(s string, maxRunes int) string {
	return markdownEscape(OneLine(s, maxRunes))
}

// markdownTitle은 메시지 제목(IM 플랫폼의 제목 줄/카드 제목)을 반환하며, 내용은 **이스케이프되지 않은 원문**이다.
//
// 여기서 일부러 이스케이프하지 않는 이유: 이 제목은 네 가지 문맥의 렌더러가 함께 쓴다. markdown 본문, Telegram의 HTML,
// 페이슈 카드의 plain_text, 그리고 범용 Webhook의 JSON과 이메일 제목이다. 문맥마다 이스케이프 규칙이 다르다.
// markdown 이스케이프를 HTML에 넣으면 보이는 백슬래시가 남고, JSON에 넣으면 데이터가 오염된다. 그래서 이스케이프는
// 각 출력 쪽이 책임져야 한다(writeItem / feishuItemLines / telegramEscape 참고). 예전에 공유 함수에 markdown 이스케이프를
// 넣었다가, Telegram 메시지에 `\(1\)` 같은 보이는 백슬래시가 나타난 적이 있다.
func markdownTitle(m Message) string {
	if m.Batch {
		return fmt.Sprintf("취약점 요약 · 총 %d건", len(m.Items))
	}
	if len(m.Items) == 0 {
		return "취약점 알림"
	}
	it := m.Items[0]
	return fmt.Sprintf("[%s] %s", SeverityLabel(it.Severity), OneLine(it.Title(), 0))
}

// markdownBody는 메시지 본문을 렌더링하며, 본문과 **실제로 기록된 항목 수**를 반환한다.
//
// 반환값 kept는 이번 전송에서 실제로 전달된 항목 수이다. 호출자는 이 값에 따라 앞의 kept개만 전달 완료로 표시한다.
// 채널의 길이 상한에 막혀 나가지 못한 항목은 다음 배치에 남겨야 하며, 함께 성공으로 표시하면 안 된다.
// 이것이 바로 「조용한 누락」의 원인이다. 메시지는 잘렸지만 전달 기록에는 전부 전달된 것으로 남아, 뒷부분이 한 번도
// 나가지 않았다는 사실을 아무 곳에서도 알 수 없다.
//
// maxBytes<=0이면 제한이 없다.
func markdownBody(m Message, maxBytes int) (string, int) {
	if !m.Batch {
		if len(m.Items) == 0 {
			return "", 0
		}
		var b strings.Builder
		writeItem(&b, m.Items[0], "", true)
		// 단건 메시지는 길더라도 그대로 보낸다(마지막 자르기로 보호한다). 한 취약점의 일부 정보라도
		// 아무것도 보내지 않는 것보다 낫다.
		return TruncateBytes(b.String(), maxBytes), 1
	}

	footer := ""
	if m.HomeURL != "" {
		footer = fmt.Sprintf("\n[플랫폼에서 전체 보기](%s)\n", m.HomeURL)
	}
	kept := packItemCount(m.Items, maxBytes, markdownReservedBytes, footer, byteSize, func(it Item, idx int) string {
		var b strings.Builder
		writeItem(&b, it, fmt.Sprintf("%d. ", idx+1), false)
		return b.String()
	})

	items := m.Items[:kept]
	var b strings.Builder
	b.WriteString(markdownBatchIntro(m, items, len(m.Items)))
	for i, it := range items {
		writeItem(&b, it, fmt.Sprintf("%d. ", i+1), false)
	}
	b.WriteString(footer)
	return TruncateBytes(b.String(), maxBytes), kept
}

// markdownBatchIntro는 요약 메시지의 머리 부분을 렌더링한다. 시간 구간, 건수, 심각도 분포를 보여 준다.
// 이 정보가 있으면 요약을 받은 사람이 플랫폼에 들어가지 않고도 이 배치를 지금 처리해야 하는지 판단할 수 있다.
//
// items는 **실제로 들어간** 항목이고, total은 이번 배치에 있어야 할 총 개수이다. 두 값이 다르면 반드시
// 「나머지 몇 건은 다음 메시지에 있다」고 명시해야 한다. 그렇지 않으면 읽는 사람은 메시지 머리의 숫자를 전체라고 믿고,
// 한 번도 나가지 않은 뒷부분 항목은 화면에 전혀 나타나지 않는다.
func markdownBatchIntro(m Message, items []Item, total int) string {
	var b strings.Builder
	if m.WindowMinutes > 0 {
		fmt.Fprintf(&b, "**최근 %d분간 신규 취약점 %d건**", m.WindowMinutes, total)
	} else {
		fmt.Fprintf(&b, "**신규 취약점 %d건**", total)
	}
	if extra := total - len(items); extra > 0 {
		fmt.Fprintf(&b, "(이 메시지에는 앞 %d건만 표시하며, 나머지 %d건은 다음 메시지에서 이어서 보냅니다)", len(items), extra)
	}
	// 심각도별 분포를 보여 주어, 읽는 사람이 심각한 항목이 있는지 한눈에 보게 한다. 집계는 **이 메시지에 실제로
	// 들어간 항목만** 대상으로 하여, 「심각 3」이 아래에서 세어지는 항목과 일치하도록 보장한다.
	counts := map[string]int{}
	for _, it := range items {
		counts[it.Severity]++
	}
	var parts []string
	for _, sev := range []string{"critical", "high", "medium", "low"} {
		if n := counts[sev]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", SeverityLabel(sev), n))
		}
	}
	if len(parts) > 0 {
		b.WriteString("\n" + strings.Join(parts, " · "))
	}
	b.WriteString("\n\n")
	return b.String()
}

// writeItem은 취약점 항목 하나를 렌더링한다.
//
// prefix는 요약 목록의 순번이다. single=true이면 전체 버전을 렌더링하고(요약과 상세 링크 포함),
// 요약 목록에서는 한 줄 요약만 렌더링한다. 그렇지 않으면 50건짜리 요약이 장문의 문서가 되어 버린다.
//
// 외부에서 온 모든 내용(제목/유형/자산/요약)은 markdownText를 거친다. 즉 한 줄화 + 이스케이프이다.
// 상세 링크는 관리자가 설정한 public_base_url로 만든 것이라 신뢰할 수 없는 내용이 아니며, 클릭 가능한 링크여야 하므로 그대로 출력한다.
func writeItem(b *strings.Builder, it Item, prefix string, single bool) {
	line := fmt.Sprintf("%s**%s · %s**", prefix, SeverityLabel(it.Severity), markdownText(it.Title(), 0))
	if !single {
		// 요약 모드: 한 줄로 표시하며, 자산과 요약은 압축해서 뒤에 붙인다.
		var extras []string
		if a := assetLine(it.Assets, maxAssetsShown); a != "" {
			extras = append(extras, markdownText(a, 0))
		}
		if it.Summary != "" {
			extras = append(extras, markdownText(it.Summary, 60))
		}
		if len(extras) > 0 {
			line += " — " + strings.Join(extras, " · ")
		}
		b.WriteString(line + "\n")
		return
	}
	b.WriteString(line + "\n")
	if it.IsStatusChange() {
		fmt.Fprintf(b, "**상태 변경**: %s → %s\n",
			markdownText(StatusLabel(it.FromStatus), 0), markdownText(StatusLabel(it.ToStatus), 0))
	}
	if it.VulnClass != "" && it.VulnClass != it.Title() {
		fmt.Fprintf(b, "**유형**: %s\n", markdownText(it.VulnClass, 0))
	}
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		fmt.Fprintf(b, "**자산**: %s\n", markdownText(a, 0))
	}
	if it.Summary != "" {
		if s := markdownText(it.Summary, maxSummaryRunes); s != "" {
			fmt.Fprintf(b, "**개요**: %s\n", s)
		}
	}
	if it.DetailURL != "" {
		fmt.Fprintf(b, "[상세 보기](%s)\n", it.DetailURL)
	}
}
