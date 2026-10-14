package notify

import (
	"fmt"
	"strings"
)

// 이 파일은 이메일 HTML 본문을 렌더링한다. 최신 CSS 대신 인라인 스타일과 단순한 표 레이아웃을 일부러 쓴다.
// 이메일 클라이언트(특히 Outlook과 국내 기업 메일)는 <style> 블록과 flex/grid 지원 차이가 매우 커서,
// 인라인 스타일이 모든 곳에서 올바르게 표시되는 유일한 방법이기 때문이다.

// htmlSeverityColor는 심각도에 해당하는 강조 색을 반환한다. 왼쪽 색 막대와 제목에 쓰인다.
func htmlSeverityColor(severity string) string {
	switch severity {
	case "critical":
		return "#d32029"
	case "high":
		return "#e8830c"
	case "medium":
		return "#d4b106"
	case "low":
		return "#1677ff"
	default:
		return "#8c8c8c"
	}
}

// htmlTitle은 이메일 제목을 반환한다.
func htmlTitle(m Message) string {
	return markdownTitle(m)
}

// htmlBody는 이메일 본문 HTML을 렌더링한다. maxRunes<=0이면 자르지 않는다.
func htmlBody(m Message, maxRunes int) string {
	var b strings.Builder
	b.WriteString(`<div style="font-family:-apple-system,BlinkMacSystemFont,'Segoe UI','PingFang SC','Microsoft YaHei',sans-serif;font-size:14px;color:#262626;line-height:1.6;">`)
	if m.Batch {
		b.WriteString(htmlBatchIntro(m))
		for _, it := range m.Items {
			b.WriteString(htmlItem(it, false))
		}
	} else if len(m.Items) > 0 {
		b.WriteString(htmlItem(m.Items[0], true))
	}
	if m.HomeURL != "" {
		fmt.Fprintf(&b, `<p style="margin:16px 0 0;"><a href="%s" style="color:#1677ff;">플랫폼에서 전체 보기</a></p>`, htmlEscapeAttr(m.HomeURL))
	}
	b.WriteString(`</div>`)
	return TruncateHTML(b.String(), maxRunes)
}

// htmlBatchIntro는 요약 이메일의 머리 부분을 렌더링한다. 건수와 심각도 분포를 보여 준다.
func htmlBatchIntro(m Message) string {
	var b strings.Builder
	if m.WindowMinutes > 0 {
		fmt.Fprintf(&b, `<h2 style="font-size:16px;margin:0 0 4px;">최근 %d분간 신규 취약점 %d건</h2>`, m.WindowMinutes, len(m.Items))
	} else {
		fmt.Fprintf(&b, `<h2 style="font-size:16px;margin:0 0 4px;">신규 취약점 %d건</h2>`, len(m.Items))
	}
	counts := map[string]int{}
	for _, it := range m.Items {
		counts[it.Severity]++
	}
	var parts []string
	for _, sev := range []string{"critical", "high", "medium", "low"} {
		if n := counts[sev]; n > 0 {
			parts = append(parts, fmt.Sprintf(`<span style="color:%s;font-weight:600;">%s %d</span>`,
				htmlSeverityColor(sev), htmlEscape(SeverityLabel(sev)), n))
		}
	}
	if len(parts) > 0 {
		fmt.Fprintf(&b, `<p style="margin:0 0 12px;">%s</p>`, strings.Join(parts, " &middot; "))
	}
	return b.String()
}

// htmlItem은 취약점 항목 하나를 렌더링한다. full=true이면 요약과 상세 링크를 포함하고(단건 푸시),
// false이면 한 줄로 압축한다(요약 목록).
func htmlItem(it Item, full bool) string {
	color := htmlSeverityColor(it.Severity)
	var b strings.Builder
	if full {
		fmt.Fprintf(&b, `<div style="border-left:4px solid %s;padding:8px 0 8px 12px;margin-bottom:12px;">`, color)
	} else {
		fmt.Fprintf(&b, `<div style="border-left:3px solid %s;padding:4px 0 4px 10px;margin-bottom:8px;">`, color)
	}
	fmt.Fprintf(&b, `<div style="font-weight:600;">%s &middot; %s</div>`,
		htmlEscape(SeverityLabel(it.Severity)), htmlEscape(it.Title()))

	if !full {
		var extras []string
		if a := assetLine(it.Assets, maxAssetsShown); a != "" {
			extras = append(extras, htmlEscape(a))
		}
		if it.Summary != "" {
			extras = append(extras, htmlEscape(OneLine(it.Summary, 60)))
		}
		if len(extras) > 0 {
			fmt.Fprintf(&b, `<div style="color:#595959;font-size:13px;">%s</div>`, strings.Join(extras, " &middot; "))
		}
		b.WriteString(`</div>`)
		return b.String()
	}

	if it.IsStatusChange() {
		fmt.Fprintf(&b, `<div><b>상태 변경</b>: %s → %s</div>`,
			htmlEscape(StatusLabel(it.FromStatus)), htmlEscape(StatusLabel(it.ToStatus)))
	}
	if it.VulnClass != "" && it.VulnClass != it.Title() {
		fmt.Fprintf(&b, `<div><b>유형</b>: %s</div>`, htmlEscape(it.VulnClass))
	}
	if a := assetLine(it.Assets, maxAssetsShown); a != "" {
		fmt.Fprintf(&b, `<div><b>자산</b>: %s</div>`, htmlEscape(a))
	}
	if s := OneLine(it.Summary, maxSummaryRunes); s != "" {
		fmt.Fprintf(&b, `<div><b>개요</b>: %s</div>`, htmlEscape(s))
	}
	if it.DetailURL != "" {
		fmt.Fprintf(&b, `<div style="margin-top:6px;"><a href="%s" style="color:#1677ff;">상세 보기</a></div>`, htmlEscapeAttr(it.DetailURL))
	}
	b.WriteString(`</div>`)
	return b.String()
}

// htmlEscape는 HTML 텍스트 내용을 이스케이프한다. 취약점 제목과 요약은 테스트 대상과 모델 출력에서 온 신뢰할 수 없는 내용이다.
// 이스케이프하지 않으면 임의의 HTML(외부 링크 이미지 포함)을 이메일에 주입하는 것을 허용하는 셈이다.
func htmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// htmlEscapeAttr는 HTML 속성 값을 이스케이프한다. 텍스트 이스케이프에 더해 따옴표를 추가로 처리하여,
// URL 안의 따옴표가 href 속성을 일찍 닫지 못하도록 막는다.
func htmlEscapeAttr(s string) string {
	s = htmlEscape(s)
	s = strings.ReplaceAll(s, "\"", "&quot;")
	return s
}
