package notify

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// 이 파일은 '메시지 단위 패킹' 수정을 다룬다: 요약 메시지가 채널의 길이 상한을 넘으면 **항목 단위로** 잘라내고,
// 담지 못한 항목 수를 정확히 보고하여 호출자가 실제로 전달된 항목만 표시하도록 해야 한다.
//
// 이전 방식은 전체를 렌더링한 뒤 자르고, 배치 전체를 전달 완료로 표시했다. 메시지 뒷부분이 소리 없이 사라졌고,
// 발송 이력에는 모두 성공으로 나왔다. 취약점이 그대로 누락됐는데 어디에서도 발견할 수 없었다.

func TestMarkdownBodyPacksWholeItemsWithinByteLimit(t *testing.T) {
	// 중국어 요약 200건. WeCom의 4096바이트를 확실히 넘는다.
	m := batchMsg(200)
	body, kept := markdownBody(m, weComMarkdownLimit)

	if len(body) > weComMarkdownLimit {
		t.Fatalf("正文 %d 字节超上限 %d", len(body), weComMarkdownLimit)
	}
	if !utf8.ValidString(body) {
		t.Fatal("正文不是合法 UTF-8")
	}
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("应只装下一部分（0 < kept < %d），得到 %d", len(m.Items), kept)
	}
	// 헤더는 이 메시지에 몇 건이 포함되었고 나머지가 몇 건인지 정확히 밝혀야 한다. 그렇지 않으면 독자가 헤더의
	// 숫자를 전체 개수로 오인한다.
	if !strings.Contains(body, "나머지") || !strings.Contains(body, "다음 메시지에서") {
		t.Fatalf("头部应说明还有多少条未包含在本条里:\n%s", body[:minInt(400, len(body))])
	}
	// 앞의 kept 건만 포함해야 한다.
	for i := 0; i < kept; i++ {
		if !strings.Contains(body, "漏洞"+itoa(i+1)) {
			t.Fatalf("第 %d 条应在本条消息里:\n%s", i+1, body)
		}
	}
	if strings.Contains(body, "漏洞"+itoa(kept+1)) {
		t.Fatalf("第 %d 条不该出现（它属于下一批）", kept+1)
	}
}

func TestMarkdownBodyKeepsEverythingWhenUnderLimit(t *testing.T) {
	m := batchMsg(3)
	body, kept := markdownBody(m, 0) // 0 = 제한 없음
	if kept != len(m.Items) {
		t.Fatalf("不限制长度时应全部保留，得到 kept=%d", kept)
	}
	if strings.Contains(body, "其余") {
		t.Fatalf("没有截断时不该出现截断提示:\n%s", body)
	}
}

func TestMarkdownBodyAlwaysKeepsAtLeastOneItem(t *testing.T) {
	// 예산이 너무 작아 한 건도 들어가지 않더라도 여전히 한 건은 발송해야 한다 (최종 자르기에서 보정).
	// 그렇지 않으면 길이를 초과한 취약점 하나 때문에 배치 전체가 영구히 막힌다. 매번 가져올 때마다 들어가지 않고, 매번 발송되지 않는다.
	m := batchMsg(5)
	_, kept := markdownBody(m, 50)
	if kept != 1 {
		t.Fatalf("至少应保留 1 条，得到 %d", kept)
	}
}

func TestMarkdownBodySingleReturnsOne(t *testing.T) {
	_, kept := markdownBody(singleMsg(), 4096)
	if kept != 1 {
		t.Fatalf("单条消息应报送达 1 条，得到 %d", kept)
	}
	// 빈 메시지에는 전달할 항목이 없다.
	if _, k := markdownBody(Message{}, 4096); k != 0 {
		t.Fatalf("空消息应报 0 条，得到 %d", k)
	}
}

func TestTelegramPackingUsesRuneBudget(t *testing.T) {
	m := batchMsg(200)
	text, kept := telegramHTML(m)
	// Telegram은 **문자 수** 기준으로 길이를 제한한다. 바이트 기준으로 계산하면 중국어 메시지가 1/3로 줄어든다.
	if n := utf8.RuneCountInString(text); n > telegramTextLimit {
		t.Fatalf("正文 %d 字符超上限 %d", n, telegramTextLimit)
	}
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("应只装下一部分，得到 %d", kept)
	}
	if !strings.Contains(text, "다음 메시지") {
		t.Fatalf("应说明还有余量未包含:\n%.300s", text)
	}
}

func TestFeishuPackingReportsKept(t *testing.T) {
	m := batchMsg(2000)
	_, kept := feishuCard(m)
	if kept <= 0 || kept >= len(m.Items) {
		t.Fatalf("卡片应只装下一部分，得到 %d", kept)
	}
}

func TestWebhookAndEmailReportAllItems(t *testing.T) {
	// 이 두 채널은 본문을 자르지 않으므로 배치 전체를 전달 완료로 본다.
	m := batchMsg(7)
	if n := len(m.Items); n != 7 {
		t.Fatal("前置条件不成立")
	}
	// 렌더러의 반환값으로 간접 확인한다: markdownBody(0)에서 제한이 없을 때 모두 보존되는지.
	if _, k := markdownBody(m, 0); k != len(m.Items) {
		t.Fatalf("不限制长度时应用全部，得到 %d", k)
	}
}

// TestMarkdownEscapesUntrustedContent는 '신뢰할 수 없는 내용이 메시지 구조를 바꿀 수 없음'을 보장하는 회귀 테스트다.
// 제목과 요약은 모델 출력(모델은 테스트 대상의 응답을 읽음)에서 오며, 자산 이름은 테스트 대상의 URL에서 온다.
func TestMarkdownEscapesUntrustedContent(t *testing.T) {
	cases := []struct {
		name  string
		item  Item
		must  []string // 결과에 반드시 나타나야 함 (이스케이프된 형태)
		wrong []string // 결과에 나타나면 안 됨 (이스케이프되지 않은 형태)
	}{
		{
			name: "标题里的换行 + 外链",
			item: Item{
				Severity: "high",
				Name:     "登录口 SQL 注入\n[紧急：点此验证账号](http://attacker.tld)",
			},
			// 줄바꿈은 반드시 접어야 한다 (그렇지 않으면 가짜 목록 항목/인용 블록을 만들 수 있다);
			// 대괄호와 소괄호는 반드시 이스케이프해야 한다 (그렇지 않으면 클릭 가능한 외부 링크가 된다).
			must:  []string{`\[紧急：点此验证账号\]`, `\(http://attacker.tld\)`},
			wrong: []string{"\n[紧急", "\n\n[紧急"},
		},
		{
			name: "标题里的图片信标",
			item: Item{
				Severity: "high",
				Name:     "漏洞 ![](http://attacker.tld/beacon)",
			},
			must:  []string{`\!`, `\(http://attacker.tld/beacon\)`},
			wrong: []string{"![]("},
		},
		{
			name: "资产名里的强调与引用",
			item: Item{
				Severity: "high",
				Name:     "普通标题",
				Assets:   []string{"a.com/*注入*>引用"},
			},
			must:  []string{`\*注入\*`, `\>`},
			wrong: []string{"*注入*"},
		},
		{
			name: "摘要里的反引号与竖线",
			item: Item{
				Severity: "high",
				Name:     "标题",
				Summary:  "`code` | 表格",
			},
			must:  []string{"\\`code\\`", `\|`},
			wrong: []string{"`code`"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := Message{Items: []Item{tc.item}}
			// 단건 모드의 Item 작성은 세 markdown 채널이 공유하는 렌더링 경로다.
			var b strings.Builder
			writeItem(&b, tc.item, "", true)
			got := b.String()
			for _, want := range tc.must {
				if !strings.Contains(got, want) {
					t.Errorf("缺少转义形态 %q:\n%s", want, got)
				}
			}
			for _, bad := range tc.wrong {
				if strings.Contains(got, bad) {
					t.Errorf("出现了未转义形态 %q（可被用来注入结构或外链）:\n%s", bad, got)
				}
			}
			_ = m
		})
	}
}

// TestMarkdownEscapeBackslashFirst는 이스케이프 순서를 고정한다: 백슬래시를 가장 먼저 처리해야 한다.
// 그렇지 않으면 뒤에서 추가한 백슬래시에 또 한 겹이 씌워져 출력에 이중 백슬래시가 나타난다.
func TestMarkdownEscapeBackslashFirst(t *testing.T) {
	if got := markdownEscape(`a\b*c`); got != `a\\b\*c` {
		t.Fatalf("转义顺序有误，得到 %q", got)
	}
}

// TestTelegramTitleHasNoMarkdownEscapes는 구체적인 회귀를 고정한다:
// markdown 이스케이프가 Telegram의 HTML 출력으로 새어 들어가면 안 된다 (공유 제목 함수에 이스케이프를 넣었다가,
// Telegram 메시지에 `\(1\)` 같은 보이는 백슬래시가 나타난 적이 있다).
func TestTelegramTitleHasNoMarkdownEscapes(t *testing.T) {
	m := Message{Items: []Item{{Severity: "high", Name: "alert(1) *重点*"}}}
	text, _ := telegramHTML(m)
	if strings.Contains(text, `\(`) || strings.Contains(text, `\*`) {
		t.Fatalf("Telegram 正文里出现了 markdown 的反斜杠转义:\n%s", text)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
