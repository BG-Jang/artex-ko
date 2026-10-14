package notify

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Filter는 notification_channels.filter JSONB 컬럼의 계약이다. 채널 인스턴스의 필터 조건을 담는다.
// 모든 필드는 선택 사항이며, 기본값은 「필터링하지 않음」이다. 이것이 잘못된 설정에 대한 폴백 의미이며, ParseFilter를 참고하라.
type Filter struct {
	// MinSeverity는 최소 심각도 기준(low/medium/high/critical)이며, 비어 있으면 기준을 두지 않는다.
	MinSeverity string `json:"min_severity"`
	// TaskIDs / AssetIDs가 빈 배열이면 제한이 없고, 비어 있지 않으면 이벤트와 교집합이 있어야 한다.
	TaskIDs  []int64 `json:"task_ids"`
	AssetIDs []int64 `json:"asset_ids"`
	// VulnClassInclude가 비어 있으면 모두 받고, 비어 있지 않으면 vulnclass가 그 키워드 중 하나와 일치해야 한다.
	// VulnClassExclude는 키워드 중 하나와 일치하면 제외한다(제외가 포함보다 우선한다).
	// 매칭 방식은 대소문자를 구분하지 않는 부분 문자열이다. 정규식보다 안전하다. 사용자가 정규식을 잘못 써도 채널이 조용히 무력화되지 않는다.
	VulnClassInclude []string `json:"vulnclass_include"`
	VulnClassExclude []string `json:"vulnclass_exclude"`
	// OnStatusChange는 이 채널이 취약점 상태 변경 이벤트를 받을지 결정한다(realtime 모드에서만 의미가 있다).
	OnStatusChange bool `json:"on_status_change"`
}

// ParseFilter는 채널 필터 설정을 파싱한다.
//
// **절대 error를 반환하지 않는다.** 의도된 설계이다. 필터 조건 설정이 잘못되면 항상 제로값 Filter로 퇴화한다(= 필터링 안 함 = 모두 일치).
// 취약점 알림 시스템에서는 **조금 더 많이 보내는 편이 고위험 항목 하나를 조용히 놓치는 것보다 훨씬 낫기** 때문이다.
// 파싱 실패를 「보내지 않음」으로 바꾸면, 설정은 끝난 것처럼 보이지만 실제로는 아무것도 보내지 않는 채널이 된다. 이것이 가장 나쁜 실패 모드이다.
func ParseFilter(raw []byte) Filter {
	var f Filter
	if len(raw) == 0 {
		return f
	}
	// 파싱에 실패하면 f는 제로값을 유지하므로, 필터링하지 않는다.
	_ = json.Unmarshal(raw, &f)
	return f
}

// ValidMinSeverity는 s가 올바른 심각도 기준인지 보고한다(빈 문자열은 기준을 두지 않음을 뜻한다).
func ValidMinSeverity(s string) bool {
	if s == "" {
		return true
	}
	_, ok := severityRank[s]
	return ok
}

// Validate는 필터 설정에서 **값이 제한된** 필드를 검증하며, 채널을 저장할 때 호출한다.
//
// 쓰기 시점에 막아야 하는 이유: Match는 알 수 없는 기준에 대해 `rank >= 0`으로 판정하므로 항상 참이다.
// 즉 min_severity의 글자 하나를 잘못 쓰면("hgih"), 필터가 **조용히 무력화되어** 「전부 전송」이 된다.
// 이는 이 패키지의 「보내는 것이 누락하는 것보다 낫다」는 방향과 일치하지만(누락은 없다), 그 결과 사용자는 등급별 전송을 하고 있다고 믿으면서
// 실제로는 모든 취약점을 그룹에 쏟아붓게 되고, 설정을 잘못했다는 어떤 신호도 받지 못한다. 이런 「조용한 강등」은 입구에서 막아야 한다.
//
// Validate는 **쓰기** 경로에서만 쓴다. 읽기 경로는 여전히 ParseFilter의 관대한 의미를 따른다.
// 그래야 과거 데이터에 이미 들어 있는 잘못된 값 때문에 채널 전체를 읽지 못하는 일이 없다.
func (f Filter) Validate() error {
	if !ValidMinSeverity(f.MinSeverity) {
		return fmt.Errorf("올바르지 않은 최소 심각도입니다: %q. low / medium / high / critical 중에서 선택하거나, 비워 두면 제한하지 않습니다", f.MinSeverity)
	}
	return nil
}

// Match는 이벤트를 해당 필터 조건이 있는 채널로 전달해야 하는지 판정한다.
//
// **절대 error를 반환하지 않는다.** 이유는 ParseFilter와 같다. 내부의 어떤 예외든 「일치」로 처리한다.
// 판정 순서: 이벤트 종류 → 심각도 기준 → 작업/자산 범위 → 취약점 종류 키워드.
func Match(f Filter, s Snapshot) bool {
	// 상태 변경 이벤트는 명시적으로 켠 채널만 받는다. 기본값은 꺼짐이다. 대부분의 사용자는 「푸시」를 「새 취약점 발견」으로 기대하며,
	// 상태가 바뀔 때마다 모든 흐름을 기록처럼 따라가는 것을 원하지 않기 때문이다.
	if s.Kind == EventFindingStatusChanged && !f.OnStatusChange {
		return false
	}
	if !AtLeast(s.Severity, f.MinSeverity) {
		return false
	}
	if len(f.TaskIDs) > 0 && !slices.Contains(f.TaskIDs, s.TaskID) {
		return false
	}
	if len(f.AssetIDs) > 0 && !intersectsInt(f.AssetIDs, s.AssetIDs) {
		return false
	}
	// 제외가 우선한다. 포함 목록과 동시에 일치하더라도, 제외 키워드 중 하나와 일치하면 탈락한다.
	if len(f.VulnClassExclude) > 0 && containsAnyFold(s.VulnClass, f.VulnClassExclude) {
		return false
	}
	if len(f.VulnClassInclude) > 0 && !containsAnyFold(s.VulnClass, f.VulnClassInclude) {
		return false
	}
	return true
}

func intersectsInt(a, b []int64) bool {
	// 작은 집합이므로 선형 탐색으로 충분하다. 양쪽 모두 사람이 직접 고른 수십 개 규모라서,
	// map을 만드는 비용이 이득보다 크다.
	for _, v := range b {
		if slices.Contains(a, v) {
			return true
		}
	}
	return false
}

// containsAnyFold는 s가 keywords 중 하나를 포함하는지 보고한다(대소문자를 구분하지 않는다).
func containsAnyFold(s string, keywords []string) bool {
	lower := strings.ToLower(s)
	for _, kw := range keywords {
		kw = strings.ToLower(strings.TrimSpace(kw))
		if kw != "" && strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}
