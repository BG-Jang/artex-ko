// Package notify는 취약점 발견 사실을 IM / 이메일로 푸시하는 채널 어댑터 계층을 구현한다.
//
// 계층 구조: 이 패키지는 **리프 패키지**이며 표준 라이브러리에만 의존한다. DB도, server도 알지 못한다.
// 채널 설정은 map[string]any(notification_channels.config JSONB 컬럼에 대응)로 전달되고,
// 푸시할 내용은 Message로 전달된다. 이렇게 분리한 이유는, 서명 계산·UTF-8 절단·필터 매칭처럼
// 실제로 실수하기 쉬운 부분을 PostgreSQL 없이 단위 테스트할 수 있게 하기 위해서다. 호스트는
// server 쪽에서만 오케스트레이션하면 된다.
//
// 동시성 규약: Channel 구현은 **무상태**여야 한다. 같은 Channel 인스턴스가 여러 채널 설정
// (같은 채널의 여러 봇 인스턴스 포함)에서 동시에 재사용되므로, 자격 증명은 모두 cfg 매개변수에서
// 가져와야 하며, webhook URL 같은 값을 구현체 자신의 필드에 캐시해서는 안 된다.
package notify

// 채널 종류 식별자. 이 값들은 notification_channels.kind의 유효한 집합이기도 하며, server 쪽
// 화이트리스트로 검증한다(findings.status와 같은 이유로 DB CHECK 제약은 쓰지 않는다. 나중에 채널을
// 추가하기 쉽게 하기 위해서다).
const (
	KindDingTalk = "dingtalk" // 딩톡 사용자 정의 로봇
	KindFeishu   = "feishu"   // 페이슈(Lark 포함) 사용자 정의 로봇
	KindWeCom    = "wecom"    // 기업 위챗 그룹 로봇
	KindWebhook  = "webhook"  // 범용 Webhook: 사용자 정의 메서드/헤더/JSON 템플릿
	KindTelegram = "telegram" // Telegram Bot API
	KindEmail    = "email"    // SMTP 이메일
)

// 이벤트 종류. notification_events.kind에 대응한다.
const (
	EventFindingCreated       = "finding_created"
	EventFindingStatusChanged = "finding_status_changed"
)

// InitKind는 config에서 kind가 비어 있을 때 쓰는 기본값이다.
const InitKind = KindDingTalk

// severityRank는 취약점 심각도를 비교 가능한 서수로 매핑한다. 알 수 없는 심각도는 0을 반환하므로,
// 어떤 min_severity 설정이든 알 수 없는 심각도는 걸러낸다. 의심스러우면 보내지 않아, 잘못된 알림이
// 도배되는 것을 막는다.
var severityRank = map[string]int{
	"low":      1,
	"medium":   2,
	"high":     3,
	"critical": 4,
}

// SeverityRank는 심각도의 서수를 반환한다. 알 수 없는 심각도는 0을 반환한다.
func SeverityRank(severity string) int { return severityRank[severity] }

// Lang은 server가 연결해 주면 이 리프 패키지가 푸시/이메일 메시지에 렌더링하는 문자열(심각도 및
// 상태 레이블)의 현재 사용자 노출 언어("en"/"ko"/"zh"/"es")를 반환한다. 패키지가 표준 라이브러리만
// 쓰도록 주입 함수로 두었으며, config/db/server를 import하지 않는다. nil이면(단위 테스트나 단독 실행)
// labelLang은 "ko"로 대체되어, 한국어 현지화 골든 테스트가 그대로 유지된다.
var Lang func() string

func labelLang() string {
	if Lang != nil {
		switch v := Lang(); v {
		case "en", "ko", "zh", "es":
			return v
		}
	}
	return "ko"
}

// severityLabels / statusLabels는 언어별 표시 문자열을 담는다. "ko"는 한국어 현지화 골든 테스트가
// 고정하고 있는 값을 그대로 유지한다.
var severityLabels = map[string]map[string]string{
	"ko": {"critical": "🔴 심각", "high": "🟠 높음", "medium": "🟡 중간", "low": "🔵 낮음"},
	"en": {"critical": "🔴 Critical", "high": "🟠 High", "medium": "🟡 Medium", "low": "🔵 Low"},
	"zh": {"critical": "🔴 严重", "high": "🟠 高危", "medium": "🟡 中危", "low": "🔵 低危"},
	"es": {"critical": "🔴 Crítico", "high": "🟠 Alto", "medium": "🟡 Medio", "low": "🔵 Bajo"},
}

var statusLabels = map[string]map[string]string{
	"ko": {"pending": "처리 대기", "in_progress": "처리 중", "confirmed": "확인됨", "resolved": "처리됨", "fixed": "수정됨", "false_positive": "오탐", "ignored": "무시", "duplicate": "중복", "risk_accepted": "위험 수용"},
	"en": {"pending": "Pending", "in_progress": "In progress", "confirmed": "Confirmed", "resolved": "Resolved", "fixed": "Fixed", "false_positive": "False positive", "ignored": "Ignored", "duplicate": "Duplicate", "risk_accepted": "Risk accepted"},
	"zh": {"pending": "待处理", "in_progress": "处理中", "confirmed": "已确认", "resolved": "已处理", "fixed": "已修复", "false_positive": "误报", "ignored": "忽略", "duplicate": "重复", "risk_accepted": "接受风险"},
	"es": {"pending": "Pendiente", "in_progress": "En curso", "confirmed": "Confirmado", "resolved": "Resuelto", "fixed": "Corregido", "false_positive": "Falso positivo", "ignored": "Ignorado", "duplicate": "Duplicado", "risk_accepted": "Riesgo aceptado"},
}

// SeverityLabel은 이모지가 붙은 심각도 이름을 현재 출력 언어로 반환한다. 메시지 제목과 카드 색상에 쓰인다.
// 알 수 없는 심각도는 그대로 되돌려주며, 임의로 만들어 내지 않는다.
func SeverityLabel(severity string) string {
	if v, ok := severityLabels[labelLang()][severity]; ok {
		return v
	}
	return severity
}

// StatusLabel은 처리 상태를 현재 출력 언어로 번역한다. 상태 변경 메시지에 쓰인다. 알 수 없는 상태는 그대로 되돌려준다.
func StatusLabel(status string) string {
	if v, ok := statusLabels[labelLang()][status]; ok {
		return v
	}
	return status
}

// AtLeast는 severity가 min 기준에 도달하는지 판단한다. min이 비어 있으면 기준이 없다는 뜻이므로 항상 통과한다.
// 알 수 없는 severity의 서수는 0이므로, 비어 있지 않은 min이면 어떤 값이든 거부된다(severityRank 주석 참고).
func AtLeast(severity, min string) bool {
	if min == "" {
		return true
	}
	return SeverityRank(severity) >= SeverityRank(min)
}
