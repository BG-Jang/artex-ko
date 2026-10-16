package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/Autumn-27/artex/notify"
)

// 이 파일은 IM 푸시의 채널 설정과 이벤트 계층이다. 전송 작업의 클레임과 상태 전이는
// db/notification_delivery.go 에 있다.
//
// 아래 두 가지 불변식은 이 파일을 수정할 때 반드시 지켜야 한다:
//
//  1. 취약점을 기록하는 트랜잭션(RecordFindingTx)은 InsertNotificationEventTx 를 한 번 호출하는 블라인드 INSERT 만 수행한다.
//     알림 관련 테이블을 읽지 않고 필터 매칭도 하지 않는다. 여기에 들어오는 어떤 읽기 연산이든 사용자가
//     잘못 설정한 필터 조건 때문에 취약점 쓰기 트랜잭션을 오염시키거나 중단시킬 수 있다.
//  2. 필터 매칭은 절대 에러를 내지 않는다. 잘못된 설정은 모두 “일치”로 처리한다(notify.Match 참고). 누락 없이 과하게 보내는 편이
//     낫다.

// ErrNotificationChannelNotFound 는 채널이 존재하지 않을 때 반환된다.
var ErrNotificationChannelNotFound = errors.New("알림 채널을 찾을 수 없습니다")

// 전송 상태.
const (
	NotifyStatePending = "pending" // 발송 대기
	NotifyStateSending = "sending" // 어떤 dispatcher 가 클레임했고, 리스가 아직 만료되지 않음
	NotifyStateSent    = "sent"    // 전송 완료
	NotifyStateFailed  = "failed"  // 재시도 소진 또는 영구 실패. 수동 재전송 가능
	NotifyStateSkipped = "skipped" // 채널이 비활성화되어 더 이상 전송하지 않음
)

// 푸시 모드.
const (
	NotifyModeRealtime = "realtime"
	NotifyModeDigest   = "digest"
)

// ValidNotifyMode 는 푸시 모드를 허용 목록으로 검증한다(findings.status 와 같은 이유로 DB CHECK 는 쓰지 않고,
// 이후 확장하기 쉽게 한다).
func ValidNotifyMode(m string) bool {
	return m == NotifyModeRealtime || m == NotifyModeDigest
}

// NotificationChannel 은 채널 인스턴스 설정이다. Config 와 Filter 는 원본 JSON 을 그대로 보관하고,
// 파싱은 notify 패키지에 맡긴다. db 계층은 그 필드의 의미를 이해하지 않는다.
type NotificationChannel struct {
	ID     int64           `json:"id"`
	Name   string          `json:"name"`
	Kind   string          `json:"kind"`
	Mode   string          `json:"mode"`
	Config json.RawMessage `json:"config"`
	Filter json.RawMessage `json:"filter"`
	// Enabled 를 포인터로 둔 이유는 “필드가 전달되지 않음”과 “명시적으로 false 전달”을 구분하기 위해서다 —
	// 프론트엔드 토글 컨트롤은 변경된 필드만 제출한다.
	Enabled    *bool     `json:"enabled,omitempty"`
	RatePerMin int       `json:"rate_per_min"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// IsEnabled 는 채널 활성화 여부를 반환한다. Enabled 가 nil(미로드)이면 활성화된 것으로 처리한다.
func (c *NotificationChannel) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

// NotificationEvent 는 이벤트 사실 한 건이다.
type NotificationEvent struct {
	ID        int64           `json:"id"`
	Kind      string          `json:"kind"`
	FindingID int64           `json:"finding_id"`
	Snapshot  json.RawMessage `json:"snapshot"`
	CreatedAt time.Time       `json:"created_at"`
}

const notificationChannelCols = `id, name, kind, enabled, config, mode, filter, rate_per_min, created_at, updated_at`

func scanNotificationChannel(sc interface{ Scan(...any) error }) (*NotificationChannel, error) {
	var c NotificationChannel
	var enabled bool
	if err := sc.Scan(&c.ID, &c.Name, &c.Kind, &enabled, &c.Config, &c.Mode, &c.Filter, &c.RatePerMin, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	c.Enabled = &enabled
	return &c, nil
}

// ListNotificationChannels 는 모든 채널 인스턴스를 반환한다. 활성화된 채널이 앞에 오고, 같은 레벨에서는 id 순이다.
// 정렬을 SQL 에 둔 이유는 UI 와 dispatcher 가 같은 안정적인 순서를 보도록 하기 위함이다.
func (d *DB) ListNotificationChannels(ctx context.Context) ([]*NotificationChannel, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+notificationChannelCols+` FROM notification_channels
ORDER BY enabled DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*NotificationChannel{}
	for rows.Next() {
		c, err := scanNotificationChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// NotificationChannelByID 는 채널 하나를 가져온다.
func (d *DB) NotificationChannelByID(ctx context.Context, id int64) (*NotificationChannel, error) {
	row := d.QueryRowContext(ctx, `SELECT `+notificationChannelCols+` FROM notification_channels WHERE id=$1`, id)
	c, err := scanNotificationChannel(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotificationChannelNotFound
	}
	return c, err
}

// SaveNotificationChannel 은 채널을 새로 만들거나 갱신한다.
//
// 갱신 시에는 호출자가 명시적으로 준 필드(nil 이 아니고 빈 값이 아닌 것)만 덮어쓴다. 그래야 프론트엔드가 부분 수정된
// 드로어 폼을 제출할 수 있고, 화면에 표시하지 않은 config 필드를 되돌려 보낼 필요가 없다. 되돌려 보내면 오히려
// “마스킹된 값이 실제 시크릿을 덮어쓰는” 사고가 생긴다.
func (d *DB) SaveNotificationChannel(ctx context.Context, c *NotificationChannel) (int64, error) {
	if c.Mode == "" {
		c.Mode = NotifyModeRealtime
	}
	// 여기서는 의도적으로 0 을 **가공하지 않는다**. 0 은 유효한 설정이며 의미는 “제한 없음”이다.
	//
	// 예전에는 `if c.RatePerMin <= 0 { c.RatePerMin = 기본값 }` 로 작성되어 있었다. 본래 의도는 “지정되지 않았을 때
	// 안전한 기본값을 준다”는 것이었으나, 그 과정에서 “명시적으로 0 으로 설정한 경우”까지 삼켜 버렸다. 문서, UI 안내와
	// takeTokens 는 모두 0 을 제한 없음으로 해석하는데, 이곳에서만 몰래 20(DingTalk/WeCom/Telegram)
	// 또는 100(Feishu)으로 바꾸고 있었다. 운영자는 제한이 풀렸다고 생각하지만 실제로는 분당 20 건에 막혀 있었고 아무 안내도 없었다.
	//
	// “지정되지 않음”과 “명시적 0”의 차이는 호출자만 알 수 있다(요청 본문에서 필드가 생략된 경우와 0 을 명시한 경우).
	// 따라서 기본값은 필드가 생략되었을 때 server 계층에서 채운다. notifyCreateChannel 을 참고하라.
	if c.RatePerMin < 0 {
		return 0, errors.New("전송 제한 값은 음수일 수 없습니다")
	}
	if c.Config == nil {
		c.Config = json.RawMessage(`{}`)
	}
	if c.Filter == nil {
		c.Filter = json.RawMessage(`{}`)
	}
	enabled := c.IsEnabled()

	if c.ID == 0 {
		var id int64
		err := d.QueryRowContext(ctx, `INSERT INTO notification_channels(name,kind,enabled,config,mode,filter,rate_per_min)
VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
			c.Name, c.Kind, enabled, string(c.Config), c.Mode, string(c.Filter), c.RatePerMin).Scan(&id)
		return id, err
	}
	res, err := d.ExecContext(ctx, `UPDATE notification_channels
SET name=$2, kind=$3, enabled=$4, config=$5, mode=$6, filter=$7, rate_per_min=$8
WHERE id=$1`,
		c.ID, c.Name, c.Kind, enabled, string(c.Config), c.Mode, string(c.Filter), c.RatePerMin)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrNotificationChannelNotFound
	}
	return c.ID, nil
}

// SetNotificationChannelEnabled 는 활성화 여부를 전환한다.
//
// 채널을 비활성화할 때는 아직 나가지 않은 전송을 함께 skipped 로 표시한다. 그렇지 않으면 다시 활성화했을 때
// “비활성화 기간 동안 쌓인” 오래된 취약점들이 한꺼번에 도착하고, 시효가 지났는데도 신규로 오인되기 쉽다.
func (d *DB) SetNotificationChannelEnabled(ctx context.Context, id int64, enabled bool) error {
	return d.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE notification_channels SET enabled=$2 WHERE id=$1`, id, enabled)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotificationChannelNotFound
		}
		if !enabled {
			if _, err := tx.ExecContext(ctx, `UPDATE notification_deliveries SET state=$2, last_error=$3
WHERE channel_id=$1 AND state IN ($4,$5)`,
				id, NotifyStateSkipped, "채널이 비활성화됨", NotifyStatePending, NotifyStateSending); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteNotificationChannel 은 채널을 삭제한다. 전송 이력은 외래 키 CASCADE 로 함께 삭제된다
// (채널 설정이 없어지면 이력을 해석할 방법이 없기 때문이다).
func (d *DB) DeleteNotificationChannel(ctx context.Context, id int64) error {
	res, err := d.ExecContext(ctx, `DELETE FROM notification_channels WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotificationChannelNotFound
	}
	return nil
}

// RecordNotificationEventTx 는 호출자의 트랜잭션 안에서 푸시 이벤트 한 건을 **최선을 다해** 기록한다.
//
// 이것이 취약점 쓰기 경로에서 알림과 관련된 유일한 변경이다. INSERT 한 번만 수행하며, 어떤 테이블도 읽지 않고, 채널을 알지 않으며,
// 필터도 실행하지 않는다. 트랜잭션 커밋으로 “취약점 저장”과 “푸시 작업 존재”가 원자적으로 일치함이 보장되어,
// 커밋은 성공했지만 큐에 들어가지 않아 메시지가 영구 유실되는 구간이 존재하지 않는다.
//
// 두 가지 핵심 설계는 아무렇게나 작성된 것이 아니다:
//
//  1. **왜 SAVEPOINT 를 쓰는가**: PostgreSQL 에서는 트랜잭션 안의 어느 한 문장이라도 에러가 나면 트랜잭션 전체가
//     aborted 상태가 되고, 이후 모든 문장(COMMIT 포함)이 실패한다. 따라서 “이 INSERT 의 에러를 무시하고
//     호출자가 계속 커밋하도록 하는” 것은 PG 에서 불가능하다. 세이브포인트로 에러를 이 문장 하나에 격리하지 않는 한.
//     세이브포인트가 없으면 “트랜잭션 전체 롤백”이라는 선택지만 남는다.
//
//  2. **왜 전체 롤백이 잘못인가**: 푸시는 편의 기능이고, 취약점 기록이야말로 제품 그 자체다. 알림
//     테이블의 문제(마이그레이션되지 않은 구 DB, 일시적인 디스크 장애)로 고위험 취약점이 저장되지 못하면 안 된다. 그래서 여기서는 에러를 격리하고
//     로그를 남긴 뒤 false 를 반환하여 취약점 쓰기는 평소처럼 커밋되게 한다. 대가는 이 푸시 한 건을 잃는 것이다.
//     error 가 아니라 bool 을 반환하는 것은 의도적이다. 호출자가 이를 쓰기 성공 여부에 영향을 주는 에러로 취급해서는 안 된다.
func RecordNotificationEventTx(ctx context.Context, tx *sql.Tx, kind string, findingID int64, snap notify.Snapshot) bool {
	raw, err := json.Marshal(snap)
	if err != nil {
		log.Printf("[notify] 푸시 이벤트 직렬화 실패 finding=%d: %v", findingID, err)
		return false
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT notify_event`); err != nil {
		log.Printf("[notify] 세이브포인트 생성 실패 finding=%d: %v", findingID, err)
		return false
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO notification_events(kind,finding_id,snapshot) VALUES($1,$2,$3)`,
		kind, findingID, string(raw)); err != nil {
		log.Printf("[notify] 푸시 이벤트 쓰기 실패 finding=%d (취약점 기록에는 영향 없음): %v", findingID, err)
		// 세이브포인트까지 롤백하여 트랜잭션을 aborted 상태에서 복구한다.
		if _, rbErr := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT notify_event`); rbErr != nil {
			log.Printf("[notify] 세이브포인트 롤백 실패 finding=%d: %v", findingID, rbErr)
		}
		return false
	}
	// 세이브포인트를 해제하여, 긴 트랜잭션에 쓸모없는 세이브포인트가 쌓이지 않도록 한다.
	_, _ = tx.ExecContext(ctx, `RELEASE SAVEPOINT notify_event`)
	return true
}

// AddNotificationEvent 는 InsertNotificationEventTx 의 독립 트랜잭션 버전으로, 기존 트랜잭션 안에 있지 않은
// 호출 지점에서 사용한다(예: 채널의 “테스트 메시지 발송”. 실제 finding 이 없다).
func (d *DB) AddNotificationEvent(ctx context.Context, kind string, findingID int64, snap notify.Snapshot) (int64, error) {
	raw, err := json.Marshal(snap)
	if err != nil {
		return 0, fmt.Errorf("알림 이벤트 스냅샷 직렬화 실패: %w", err)
	}
	var id int64
	err = d.QueryRowContext(ctx, `INSERT INTO notification_events(kind,finding_id,snapshot) VALUES($1,$2,$3) RETURNING id`,
		kind, findingID, string(raw)).Scan(&id)
	return id, err
}

// FanOutPendingEvents 는 아직 분배되지 않은 취약점 이벤트를 현재 활성화된 채널 기준으로 전송 작업으로 펼친다.
// 이번 라운드에서 처리한 이벤트 수와 새로 만든 전송 수를 반환한다.
//
// 라운드 전체 작업은 하나의 트랜잭션 안에서 수행된다. 이벤트는 FOR UPDATE SKIP LOCKED 로 클레임하므로, 여러 프로세스가 동시에 실행되더라도
// 각자 서로 다른 행을 클레임한다(프로젝트의 아카이브 큐 클레임도 같은 방식을 쓴다. 참고:
// db/task_archives.go 의 completeNextArchiveJob).
//
// 필터 매칭은 의도적으로 SQL 이 아니라 Go 쪽에서 수행한다. 채널의 필터 조건은 선택적 필드들로 이루어진 JSONB 이고,
// 여섯 가지 조합의 매칭을 SQL 로 표현하면 쿼리 유지보수가 어려워진다. 채널 수는 “사람이 직접 설정한 몇 개” 수준이므로,
// 전체를 로드한 뒤 메모리에서 하나씩 비교하는 편이 더 빠르고 테스트하기도 쉽다.
//
// 어떤 채널에도 일치하지 않은 이벤트도 똑같이 fanned_out 으로 표시한다. 그렇지 않으면 분배 대기 집합에 영원히 남아
// 매 tick 마다 다시 스캔된다.
func (d *DB) FanOutPendingEvents(ctx context.Context, limit int) (eventCount, deliveryCount int, err error) {
	if limit <= 0 {
		limit = 200
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback() //nolint:errcheck // 커밋 성공 후에는 no-op

	channels, err := listEnabledNotificationChannelsTx(ctx, tx)
	if err != nil {
		return 0, 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, kind, finding_id, snapshot FROM notification_events
WHERE NOT fanned_out ORDER BY id FOR UPDATE SKIP LOCKED LIMIT $1`, limit)
	if err != nil {
		return 0, 0, err
	}
	var (
		events      []NotificationEvent
		parsedSnaps []notify.Snapshot
	)
	for rows.Next() {
		var ev NotificationEvent
		if err := rows.Scan(&ev.ID, &ev.Kind, &ev.FindingID, &ev.Snapshot); err != nil {
			rows.Close()
			return 0, 0, err
		}
		var snap notify.Snapshot
		// 스냅샷은 우리가 직접 기록한 것이므로 이론상 반드시 파싱 가능하다. 파싱에 실패해도 전송 흐름은 막지 않되,
		// 이 이벤트는 필드가 모두 비어 있어 필터 조건이 있는 모든 채널에서 건너뛰어진다. 한 건을 덜 보내는 것이
		// 잘못된 행 하나가 큐 전체를 멈추게 하는 것보다 낫다.
		_ = json.Unmarshal(ev.Snapshot, &snap)
		// kind 는 행의 값을 기준으로 한다. 스냅샷 안의 값은 렌더링용 복사본이라 구 버전이 기록했을 수 있다.
		snap.Kind = ev.Kind
		events = append(events, ev)
		parsedSnaps = append(parsedSnaps, snap)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	if len(events) == 0 {
		return 0, 0, tx.Commit()
	}

	type pending struct {
		eventID   int64
		channelID int64
	}
	var toInsert []pending
	for i, snap := range parsedSnaps {
		for _, ch := range channels {
			if !notify.Match(notify.ParseFilter(ch.Filter), snap) {
				continue
			}
			toInsert = append(toInsert, pending{eventID: events[i].ID, channelID: ch.ID})
		}
	}
	if len(toInsert) > 0 {
		var (
			vals []string
			args []any
		)
		for _, p := range toInsert {
			vals = append(vals, fmt.Sprintf("($%d,$%d)", len(args)+1, len(args)+2))
			args = append(args, p.eventID, p.channelID)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO notification_deliveries(event_id,channel_id) VALUES `+strings.Join(vals, ","), args...); err != nil {
			return 0, 0, err
		}
	}

	// 이번 라운드의 이벤트를 분배 완료로 표시한다. 어떤 채널에도 일치하지 않은 이벤트도 함께 표시한다(함수 주석 참고).
	ids := make([]string, 0, len(events))
	markArgs := make([]any, 0, len(events))
	for _, ev := range events {
		markArgs = append(markArgs, ev.ID)
		ids = append(ids, fmt.Sprintf("$%d", len(markArgs)))
	}
	if _, err := tx.ExecContext(ctx, `UPDATE notification_events SET fanned_out=true WHERE id IN (`+strings.Join(ids, ",")+`)`, markArgs...); err != nil {
		return 0, 0, err
	}
	return len(events), len(toInsert), tx.Commit()
}

// listEnabledNotificationChannelsTx 는 트랜잭션 안에서 활성화된 채널을 가져온다. 개수가 적어서
// 페이지네이션도 캐시도 하지 않는다. 캐시를 두면 “설정을 바꾸면 언제 적용되는가”라는 추가 타이밍 문제가 생긴다.
func listEnabledNotificationChannelsTx(ctx context.Context, tx *sql.Tx) ([]*NotificationChannel, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, name, kind, config, mode, filter, rate_per_min
FROM notification_channels WHERE enabled ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*NotificationChannel{}
	for rows.Next() {
		var c NotificationChannel
		if err := rows.Scan(&c.ID, &c.Name, &c.Kind, &c.Config, &c.Mode, &c.Filter, &c.RatePerMin); err != nil {
			return nil, err
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

// NotificationAssetNames 는 자산 id 를 짧은 표시 이름으로 변환하여 푸시 메시지에 사용한다.
//
// 반환 순서는 입력과 같으며, 길이는 입력보다 작을 수 있다(존재하지 않는 id 는 건너뛴다). 입력 순서를 유지하는 것은
// 같은 취약점의 메시지가 여러 번 전송되어도 자산 순서가 안정적이도록 하기 위함이다. 그렇지 않으면 재시도 후 받은 메시지에서
// 자산 순서가 바뀌어 “자산이 바뀌었다”고 오해될 수 있다.
func (d *DB) NotificationAssetNames(ctx context.Context, ids []int64) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ph, args := placeholders(1, ids)
	rows, err := d.QueryContext(ctx, `SELECT id, type, domain, ip, url, app_name, bundle_id FROM assets WHERE id IN (`+ph+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	labels := map[int64]string{}
	for rows.Next() {
		var (
			id                int64
			typ               string
			domain, ip, url   sql.NullString
			appName, bundleID sql.NullString
		)
		if err := rows.Scan(&id, &typ, &domain, &ip, &url, &appName, &bundleID); err != nil {
			return nil, err
		}
		labels[id] = assetDisplayName(typ, domain.String, ip.String, url.String, appName.String, bundleID.String)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ids))
	seen := map[int64]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if label, ok := labels[id]; ok && label != "" {
			out = append(out, label)
		}
	}
	return out, nil
}

// assetDisplayName 은 자산 유형에 따라 가장 식별력 있는 식별자를 고른다.
// 기본값으로 빈 문자열을 반환하며, “이름을 가져올 수 없는 자산”을 어떻게 표시할지는 호출자가 결정한다. 이 함수는 임의의 플레이스홀더를 만들지 않는다.
// 그렇지 않으면 “자산#42” 같은 노이즈가 푸시 메시지에 섞여 들어가고, 독자는 그것을 실제 도메인으로 오인하게 된다.
func assetDisplayName(typ, domain, ip, url, appName, bundleID string) string {
	pick := func(vals ...string) string {
		for _, v := range vals {
			if strings.TrimSpace(v) != "" {
				return v
			}
		}
		return ""
	}
	switch typ {
	case "root_domain", "subdomain":
		return domain
	case "ip":
		return ip
	case "app":
		return pick(appName, bundleID)
	case "service", "endpoint":
		return pick(url, domain, ip)
	default:
		return pick(domain, ip, url, appName)
	}
}

// SetFindingStatusWithNotify 는 취약점 처리 상태를 갱신하고, 같은 트랜잭션에서 상태 변경
// 푸시 이벤트를 한 건 등록한다.
//
// 변경 전 상태(from), 취약점 존재 여부(found), 이벤트 등록 성공 여부(notified)를 반환한다.
//
// 의도적으로 설계한 동작 세 가지:
//   - 상태가 실제로 변하지 않으면 이벤트를 등록하지 않는다. 프론트엔드 드로어가 같은 값을 반복 제출하거나 자동화 스크립트가
//     멱등하게 재실행하더라도 푸시 노이즈가 생겨서는 안 된다.
//   - 취약점이 없으면 found=false 를 반환하고 어떤 쓰기도 하지 않는다. 호출자가 이를 404 로 변환한다.
//   - 이벤트 등록 실패는 상태 갱신에 영향을 주지 않는다(RecordNotificationEventTx 의 세이브포인트 설명 참고).
//     따라서 notified=false 여도 상태는 이미 성공적으로 바뀐 상태이며, 호출자는 이로 인해 에러를 내서는 안 된다.
func (d *DB) SetFindingStatusWithNotify(ctx context.Context, id int64, status string) (from string, found bool, notified bool, err error) {
	err = d.WithEvidenceTx(ctx, func(tx *sql.Tx) error {
		var txErr error
		from, found, _, notified, txErr = SetFindingStatusTx(ctx, tx, id, status)
		return txErr
	})
	return from, found, notified, err
}

// SetFindingStatusTx 는 **호출자의 트랜잭션** 안에서 취약점 상태를 갱신하고 상태 변경 푸시 이벤트를 등록한다.
//
// 트랜잭션 단위 함수로 분리한 이유는 상태를 바꾸는 모든 경로가 같은 의미 체계를 공유하도록 하기 위함이다. 이전에는
// patchFinding 만 알림을 포함한 버전을 사용했고, **재테스트 결론이 “수정됨”일 때**(finding_retests
// 의 `UPDATE findings SET status=...` 문)는 DB 에 직접 쓰고 있었다. 그래서 `on_status_change` 를 설정한
// 채널은 이런 상태 전이에 대해 푸시를 전혀 받지 못했다. 화면에서는 상태가 몰래 바뀌었고,
// 운영자는 플랫폼을 열어 봐야 알 수 있었다.
//
// 변경 전 상태(from), 취약점 존재 여부(found), 상태가 실제로 바뀌었는지(changed),
// 이벤트 등록 성공 여부(notified)를 반환한다. 등록 실패가 상태 갱신에는 영향을 주지 않는다(RecordNotificationEventTx 참고).
func SetFindingStatusTx(ctx context.Context, tx *sql.Tx, id int64, status string) (from string, found bool, changed bool, notified bool, err error) {
	var (
		vulnclass, name, severity, summary string
		taskID                             sql.NullInt64
		assetIDs                           []byte
	)
	scanErr := tx.QueryRowContext(ctx, `SELECT vulnclass, name, severity, summary, task_id, asset_ids, status
FROM findings WHERE id=$1 FOR UPDATE`, id).
		Scan(&vulnclass, &name, &severity, &summary, &taskID, &assetIDs, &from)
	if scanErr == sql.ErrNoRows {
		return "", false, false, false, nil
	}
	if scanErr != nil {
		return "", false, false, false, scanErr
	}
	found = true
	if from == status {
		// 상태가 실제로 바뀌지 않았다면 이벤트를 등록하지 않는다. 같은 값의 반복 제출이나 멱등 재실행이
		// 푸시 노이즈를 만들어서는 안 된다.
		return from, true, false, false, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE findings SET status=$2 WHERE id=$1`, id, status); err != nil {
		return from, true, false, false, err
	}
	var assets []int64
	_ = json.Unmarshal(assetIDs, &assets)
	notified = RecordNotificationEventTx(ctx, tx, notify.EventFindingStatusChanged, id, notify.Snapshot{
		Kind:       notify.EventFindingStatusChanged,
		FindingID:  id,
		TaskID:     taskID.Int64,
		VulnClass:  vulnclass,
		Name:       name,
		Severity:   severity,
		Summary:    summary,
		AssetIDs:   assets,
		FromStatus: from,
		ToStatus:   status,
	})
	return from, true, true, notified, nil
}

// NotificationStats 는 알림 페이지 상단의 개요 집계다.
type NotificationStats struct {
	Channels     int   `json:"channels"`
	ChannelsOn   int   `json:"channels_on"`
	Pending      int   `json:"pending"`
	Failed       int   `json:"failed"`
	SentToday    int   `json:"sent_today"`
	BacklogAgeMS int64 `json:"backlog_age_ms"` // 가장 오래된 대기 전송까지의 경과 밀리초
}

// NotificationStatsSnapshot 은 알림 시스템의 상태를 집계한다.
// BacklogAgeMS 는 “푸시가 멈췄는가”를 가장 직접적으로 보여 주는 지표다. pending 개수보다 훨씬 유용한데,
// 3 건이 쌓인 경우라도 그 차이가 3 초일 수도 3 시간일 수도 있기 때문이다.
func (d *DB) NotificationStatsSnapshot(ctx context.Context) (*NotificationStats, error) {
	var s NotificationStats
	if err := d.QueryRowContext(ctx, `SELECT
    (SELECT count(*) FROM notification_channels),
    (SELECT count(*) FROM notification_channels WHERE enabled),
    (SELECT count(*) FROM notification_deliveries WHERE state IN ($1,$2)),
    (SELECT count(*) FROM notification_deliveries WHERE state=$3),
    (SELECT count(*) FROM notification_deliveries WHERE state=$4 AND sent_at >= date_trunc('day', now())),
    COALESCE((SELECT EXTRACT(EPOCH FROM (now() - min(created_at))) * 1000 FROM notification_deliveries WHERE state=$1), 0)::bigint`,
		NotifyStatePending, NotifyStateSending, NotifyStateFailed, NotifyStateSent).
		Scan(&s.Channels, &s.ChannelsOn, &s.Pending, &s.Failed, &s.SentToday, &s.BacklogAgeMS); err != nil {
		return nil, err
	}
	return &s, nil
}
