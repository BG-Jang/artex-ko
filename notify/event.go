package notify

// Snapshot은 notification_events.snapshot JSONB 컬럼의 계약이다. 쓰는 쪽은 db 계층의 취약점 저장 트랜잭션이고,
// 읽는 쪽은 server 계층의 전송 엔진과 필터 매칭이다. 이 정의를 이 패키지에 둔 이유는, 이것이 「알림 도메인」의
// 페이로드이기 때문이다. db는 직렬화만 담당하고 필드의 의미는 알지 못한다.
//
// 취약점 필드를 렌더링 시점에 다시 조회하지 않고 중복 저장하는 이유: 취약점은 나중에 이름이 바뀌거나, 심각도가
// 바뀌거나, 상태가 바뀔 수 있다. 하지만 푸시 내용은 **사건 발생 당시**의 결론을 반영해야 한다. 다시 조회하면
// 「나중에 low로 바뀐 것」이 보이는 위험한 오해를 낳는다. 또한 이렇게 하면 fan-out과 렌더링이 findings/tasks/assets
// 세 테이블을 JOIN할 필요가 없어진다.
type Snapshot struct {
	// 이벤트 종류: finding_created / finding_status_changed
	Kind      string  `json:"kind"`
	FindingID int64   `json:"finding_id"`
	TaskID    int64   `json:"task_id"`
	VulnClass string  `json:"vulnclass"`
	Name      string  `json:"name"`
	Severity  string  `json:"severity"`
	Summary   string  `json:"summary"`
	AssetIDs  []int64 `json:"asset_ids"`
	// kind=finding_status_changed일 때만 값이 있다.
	FromStatus string `json:"from_status,omitempty"`
	ToStatus   string `json:"to_status,omitempty"`
}

// Item은 채널이 렌더링할 대기 중인 취약점 한 건이다.
type Item struct {
	FindingID int64
	Name      string
	VulnClass string
	Severity  string
	Summary   string
	// Assets는 해석된 자산 표시 이름(예: 도메인/IP)이다. server 계층이 채운다.
	// 이 패키지는 DB를 건드리지 않으므로 이름을 가져올 수 없다.
	Assets []string
	// DetailURL은 취약점 상세 페이지 링크이다. 비어 있으면 public_base_url이 설정되지 않은 것이므로 렌더링 시 생략한다.
	DetailURL string
	// 상태 변경 이벤트 전용이며, 두 값이 모두 비어 있지 않으면 「대기 → 수정됨」 형태로 렌더링한다.
	FromStatus string
	ToStatus   string
}

// IsStatusChange는 이 항목이 상태 변경 이벤트인지 보고한다.
func (i Item) IsStatusChange() bool { return i.FromStatus != "" || i.ToStatus != "" }

// Title은 항목의 표시 제목을 반환한다. 사람이 붙인 name을 우선하고, 없으면 취약점 종류 vulnclass로 대체하며,
// 둘 다 비어 있으면 자리표시자를 쓴다. 빈 제목은 절대 출력하지 않는다.
func (i Item) Title() string {
	if i.Name != "" {
		return i.Name
	}
	if i.VulnClass != "" {
		return i.VulnClass
	}
	return "(이름 없는 취약점)"
}

// Message는 채널 한 번의 전송에 담기는 전체 내용이다.
type Message struct {
	// 단건 푸시일 때 길이는 1이고, 요약 푸시(digest)일 때는 한 배치 전체이다.
	// 빈 슬라이스는 허용되지 않으므로, 호출자는 최소 한 건이 있음을 보장해야 한다.
	Items []Item
	// Batch=true이면 요약 메시지로 렌더링한다(제목을 바꾸고 시간 구간과 건수를 포함한다).
	Batch bool
	// WindowMinutes는 요약 주기(분)이며, Batch=true일 때만 「최근 N분」 문구에 쓰인다.
	// 렌더링 때 time.Since로 계산하지 않고 설정에서 명시적으로 전달받는 이유는, 렌더링을 결정적으로 유지해 테스트하기 쉽게 하기 위해서다.
	WindowMinutes int
	// HomeURL은 플랫폼 대시보드 주소(전역 public_base_url)이다. 비어 있으면 대시보드 진입 링크를 넣지 않는다.
	HomeURL string
}
