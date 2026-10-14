package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateBytesKeepsValidUTF8(t *testing.T) {
	// 이 패키지에서 가장 중요한 불변식이다. WeCom은 **바이트** 기준으로 길이를 제한하며, 중국어 한 글자는 3바이트이므로
	// 바이트 단위로 강제 자르는 구현은 한자를 반쪽으로 잘라 잘못된 UTF-8을 만들고, 플랫폼에서 거부된다.
	// 길이가 서로소인 여러 중영 혼합 입력을 사용하여 가능한 모든 절단 지점을 검증한다.
	inputs := []string{
		"中文测试内容",
		"混合 mixed 内容 content",
		"a中b文c测d试e",
		"🔴🟠🟡🔵", // 4바이트 이모지, 잘못 자르면 더 쉽게 드러남
		strings.Repeat("漏洞", 100),
	}
	for _, in := range inputs {
		for max := 1; max <= len(in)+2; max++ {
			got := TruncateBytes(in, max)
			if !utf8.ValidString(got) {
				t.Fatalf("输入 %q max=%d: 产出非法 UTF-8 %q", in, max, got)
			}
			if len(got) > max {
				t.Fatalf("输入 %q max=%d: 结果 %d 字节超出上限", in, max, len(got))
			}
			// 잘리지 않았다면 내용을 바꾸면 안 된다.
			if len(in) <= max && got != in {
				t.Fatalf("输入 %q max=%d: 未超限却改动了内容 -> %q", in, max, got)
			}
		}
	}
}

func TestTruncateBytesZeroMeansUnlimited(t *testing.T) {
	long := strings.Repeat("x", 10000)
	if got := TruncateBytes(long, 0); got != long {
		t.Fatal("max=0 应表示不限制")
	}
	if got := TruncateBytes(long, -5); got != long {
		t.Fatal("max<0 应表示不限制")
	}
}

func TestTruncateBytesEllipsisBudget(t *testing.T) {
	// max가 말줄임표 자체보다 작을 때는, 말줄임표를 붙이는 바람에 오히려 초과되면 안 된다.
	got := TruncateBytes("abcdefgh", 1)
	if len(got) > 1 {
		t.Fatalf("max=1 时结果 %q 长度 %d 超限", got, len(got))
	}
	// 정상적인 경우에는 말줄임표가 붙어야 한다.
	if got := TruncateBytes("abcdefgh", 5); !strings.HasSuffix(got, ellipsis) {
		t.Fatalf("期望带省略号，得到 %q", got)
	}
}

func TestTruncateRunesCountsCharactersNotBytes(t *testing.T) {
	// TruncateBytes와의 기준 차이는 반드시 유지해야 한다. Telegram은 문자 수로 제한하므로,
	// 바이트 기준으로 계산하면 중국어 메시지가 1/3 수준으로 잘린다.
	s := "一二三四五六七八九十"
	got := TruncateRunes(s, 5)
	if n := utf8.RuneCountInString(got); n != 5 {
		t.Fatalf("期望 5 个字符，得到 %d 个 (%q)", n, got)
	}
	// 같은 문자열을 바이트 기준으로 계산하면 확실히 더 짧아야 한다.
	if utf8.RuneCountInString(TruncateBytes(s, 5)) >= 5 {
		t.Fatal("字节口径不应产出与字符口径相同的字符数")
	}
}

func TestOneLineCollapsesWhitespace(t *testing.T) {
	got := OneLine("第一行\n\n第二行\t带制表   多空格", 0)
	if strings.ContainsAny(got, "\n\t") {
		t.Fatalf("应折叠所有空白，得到 %q", got)
	}
	if strings.Contains(got, "  ") {
		t.Fatalf("不应保留连续空格，得到 %q", got)
	}
	// 잘린 후에도 여전히 읽을 수 있고 유효해야 한다.
	got = OneLine("一二三四五六七八九十", 4)
	if n := utf8.RuneCountInString(got); n != 4 {
		t.Fatalf("期望 4 字符，得到 %d (%q)", n, got)
	}
}

func TestTruncateHTMLNeverCutsTagInHalf(t *testing.T) {
	// HTML을 직접 자르면 `<a href="htt` 같은 조각이 생기고, 플랫폼이 메시지 전체를 거부한다.
	s := `<b>标题</b>正文正文正文<a href="https://example.com/very/long/path">查看详情</a>`
	for max := 1; max <= utf8.RuneCountInString(s)+2; max++ {
		got := TruncateHTML(s, max)
		if n := utf8.RuneCountInString(got); max > 0 && n > max {
			t.Fatalf("max=%d: 结果 %d 字符超限", max, n)
		}
		// 끝부분에 닫히지 않은 `<`가 남아서는 안 된다 (마지막 조각에 `<`는 있는데 `>`가 없는 경우).
		if lt := strings.LastIndex(got, "<"); lt >= 0 && !strings.Contains(got[lt:], ">") {
			t.Fatalf("max=%d: 尾部标签被切断 -> %q", max, got)
		}
	}
}

func TestAssetLineOmitsExcess(t *testing.T) {
	if got := assetLine(nil, 3); got != "" {
		t.Fatalf("无资产应返回空串，得到 %q", got)
	}
	if got := assetLine([]string{"a", "b"}, 3); got != "a, b" {
		t.Fatalf("未超限应全列，得到 %q", got)
	}
	// 상한을 넘으면 전체 개수를 반드시 표기해야 한다. 그렇지 않으면 독자가 아직 나열되지 않은 자산이 몇 개인지 알 수 없다.
	got := assetLine([]string{"a", "b", "c", "d", "e"}, 2)
	if !strings.Contains(got, "등 5개") {
		t.Fatalf("应标注总数 5，得到 %q", got)
	}
}

func TestSeverityAndStatusLabels(t *testing.T) {
	if AtLeast("", "low") {
		t.Fatal("空级别序数为 0，应被任何门槛挡住")
	}
	if !AtLeast("critical", "") {
		t.Fatal("空门槛应放行")
	}
	if got := StatusLabel("fixed"); got != "수정됨" {
		t.Fatalf("未知状态映射，得到 %q", got)
	}
	// 알 수 없는 상태는 원래 그대로 에코하며, 라벨을 임의로 만들지 않는다.
	if got := StatusLabel("weird_status"); got != "weird_status" {
		t.Fatalf("未知状态应原样回显，得到 %q", got)
	}
}

// TestTruncateHTMLNeverCutsEntity는 감사에서 지적된 누락을 다룬다: 자르기는 반쪽 태그를 피할 뿐만 아니라
// 잘린 HTML 엔티티도 피해야 한다.
//
// `&amp;`가 `&amp`로 잘리면, 엔티티만 인식하는 파서가 **메시지 전체**를 거부할 수 있다.
// 그런데 길이가 초과된 요약 메시지는 흔하므로 그 대가가 너무 크다.
func TestTruncateHTMLNeverCutsEntity(t *testing.T) {
	s := "aaaa&amp;bbbb&lt;cccc&quot;dddd"
	for max := 1; max <= utf8.RuneCountInString(s)+2; max++ {
		got := TruncateHTML(s, max)
		// 끝부분에 '&는 있지만 대응하는 ;가 없는' 엔티티 조각이 나타나면 안 된다.
		if amp := strings.LastIndex(got, "&"); amp >= 0 && !strings.Contains(got[amp:], ";") {
			t.Fatalf("max=%d: 尾部留下实体残片 %q", max, got[amp:])
		}
		if strings.Contains(got, "&amp\x00") {
			t.Fatalf("max=%d: 出现畸形实体", max)
		}
	}
}
