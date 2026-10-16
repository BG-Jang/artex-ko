package notify

import "testing"

func TestParseFilterMalformedFallsBackToMatchAll(t *testing.T) {
	// 잘못된 JSON, 빈 입력, 타입이 틀린 필드는 모두 제로값 Filter로 퇴화해야 하며,
	// 즉 '필터링하지 않음'이 되어야 한다. 이 불변식은 '차라리 더 보낼지언정 누락하지 않는다'의 귀결이다.
	// 여기서 오류를 내거나 반쯤만 파싱하도록 바뀌면, 사용자가 문자 하나만 잘못 입력해도 고위험 알림이 조용히 사라진다.
	cases := []struct {
		name string
		raw  string
	}{
		{"空输入", ""},
		{"非法 JSON", `{not json`},
		{"截断的 JSON", `{"min_severity":`},
		{"类型不匹配", `{"min_severity": 123, "task_ids": "abc"}`},
		{"顶层是数组", `[1,2,3]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := ParseFilter([]byte(tc.raw))
			if f.MinSeverity != "" || len(f.TaskIDs) != 0 || len(f.AssetIDs) != 0 {
				t.Fatalf("畸形配置应退化为零值 Filter，得到 %+v", f)
			}
			// 제로값 Filter는 임의의 이벤트에 반드시 매칭되어야 한다.
			ev := Snapshot{Kind: EventFindingCreated, Severity: "low", VulnClass: "XSS"}
			if !Match(f, ev) {
				t.Fatal("零值 Filter 应命中所有事件")
			}
		})
	}
}

func TestMatchSeverityThreshold(t *testing.T) {
	ev := func(sev string) Snapshot {
		return Snapshot{Kind: EventFindingCreated, Severity: sev}
	}
	cases := []struct {
		min    string
		sev    string
		expect bool
	}{
		{"", "low", true},
		{"", "critical", true},
		{"high", "critical", true},
		{"high", "high", true},
		{"high", "medium", false},
		{"high", "low", false},
		{"critical", "high", false},
		{"critical", "critical", true},
		// 알 수 없는 레벨의 서수는 0이므로, 비어 있지 않은 모든 임계값에 의해 차단되어야 한다 (의심스러우면 보내지 않음).
		{"low", "", false},
		{"low", "unknown", false},
		{"", "", true},
	}
	for _, tc := range cases {
		got := Match(Filter{MinSeverity: tc.min}, ev(tc.sev))
		if got != tc.expect {
			t.Errorf("min=%q sev=%q: 期望 %v 得到 %v", tc.min, tc.sev, tc.expect, got)
		}
	}
}

func TestMatchScopeRestrictions(t *testing.T) {
	ev := Snapshot{
		Kind:      EventFindingCreated,
		Severity:  "high",
		TaskID:    7,
		AssetIDs:  []int64{10, 20},
		VulnClass: "SQL注入",
	}
	cases := []struct {
		name   string
		filter Filter
		expect bool
	}{
		{"空范围=不限", Filter{}, true},
		{"任务命中", Filter{TaskIDs: []int64{7}}, true},
		{"任务未命中", Filter{TaskIDs: []int64{8}}, false},
		{"任务多选含命中", Filter{TaskIDs: []int64{8, 7}}, true},
		{"资产有交集", Filter{AssetIDs: []int64{20, 99}}, true},
		{"资产无交集", Filter{AssetIDs: []int64{99}}, false},
		{"任务与资产同时命中", Filter{TaskIDs: []int64{7}, AssetIDs: []int64{10}}, true},
		{"任务命中但资产未命中", Filter{TaskIDs: []int64{7}, AssetIDs: []int64{99}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Match(tc.filter, ev); got != tc.expect {
				t.Errorf("期望 %v 得到 %v", tc.expect, got)
			}
		})
	}
}

func TestMatchVulnClassKeywords(t *testing.T) {
	ev := func(class string) Snapshot {
		return Snapshot{Kind: EventFindingCreated, Severity: "high", VulnClass: class}
	}
	cases := []struct {
		name   string
		filter Filter
		class  string
		expect bool
	}{
		{"include 为空=全收", Filter{}, "任意类型", true},
		{"include 命中", Filter{VulnClassInclude: []string{"SQL"}}, "SQL注入", true},
		{"include 未命中", Filter{VulnClassInclude: []string{"命令执行"}}, "SQL注入", false},
		{"include 多词任一命中", Filter{VulnClassInclude: []string{"命令执行", "SQL"}}, "SQL注入", true},
		{"大小写不敏感", Filter{VulnClassInclude: []string{"sql"}}, "SQL注入", true},
		{"exclude 命中即排除", Filter{VulnClassExclude: []string{"信息泄露"}}, "信息泄露", false},
		{"exclude 未命中则放行", Filter{VulnClassExclude: []string{"信息泄露"}}, "SQL注入", true},
		// 제외가 포함보다 우선한다. 동시에 매칭되면 제외되어야 한다.
		{"排除优先于包含", Filter{
			VulnClassInclude: []string{"SQL"},
			VulnClassExclude: []string{"注入"},
		}, "SQL注入", false},
		// 공백만 있는 키워드는 무시해야 한다. 그렇지 않으면 '공백이 있는 모든 문자열과 매칭'으로 퇴화한다.
		{"空白关键词被忽略", Filter{VulnClassInclude: []string{"", "  "}}, "SQL注入", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Match(tc.filter, ev(tc.class)); got != tc.expect {
				t.Errorf("期望 %v 得到 %v", tc.expect, got)
			}
		})
	}
}

func TestMatchStatusChangeRequiresOptIn(t *testing.T) {
	ev := Snapshot{Kind: EventFindingStatusChanged, Severity: "critical", FromStatus: "pending", ToStatus: "fixed"}
	// 기본값은 꺼짐: 대부분 사람들이 말하는 '취약점 알림'은 새로 발견된 취약점을 뜻하지, 상태 변경 로그를 뜻하지 않는다.
	if Match(Filter{MinSeverity: "low"}, ev) {
		t.Fatal("状态变更事件在未开启时应被跳过")
	}
	if !Match(Filter{OnStatusChange: true}, ev) {
		t.Fatal("开启 on_status_change 后状态变更事件应命中")
	}
	// 생성 이벤트는 on_status_change의 영향을 받지 않는다.
	created := Snapshot{Kind: EventFindingCreated, Severity: "critical"}
	if !Match(Filter{MinSeverity: "low"}, created) {
		t.Fatal("创建事件不应依赖 on_status_change")
	}
}
