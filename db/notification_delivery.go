package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// 이 파일은 전송 작업의 클레임과 상태 전이를 담당한다.
//
// 클레임은 긴 트랜잭션이 아니라 “리스(lease)” 방식을 쓴다. 행을 sending 으로 바꾸고 next_attempt_at 을 미래로 밀어
// 리스 만료 시각으로 삼은 뒤 트랜잭션을 커밋하고, 그 다음에 네트워크 전송을 한다. 이렇게 하면 전송 중에 DB 잠금을 쥐고 있지 않는다.
// 네트워크 요청은 수 초가 걸릴 수 있으므로(클라이언트 타임아웃 15 초), 행 잠금을 붙잡고 있으면 같은 DB 의 다른 쓰기 작업을 망가뜨린다.
//
// 대신 전송 도중 프로세스가 죽으면 행이 sending 에 머무를 수 있다. 이 경우는 **자가 치유**된다. 리스가 만료되면
// next_attempt_at 이 과거로 떨어지고, 다음 라운드의 클레임이 같은 행을 다시 집어 올린다(클레임 조건의
// state IN ('pending','sending') 참고). 재시도 횟수는 클레임 시점에 이미 +1 되므로, 크래시가 무한 재시도를 만들지 않는다.
// MaxNotifyAttempts 번의 기회를 모두 쓰면 failed 로 떨어져 사람이 처리하게 된다.

// MaxNotifyAttempts 는 전송 한 건의 최대 시도 횟수(첫 시도 포함)다.
// 이 값은 엔진이 아니라 여기에 정의한다. 상태 머신 자체의 정책이고, 엔진은 그것을 실행할 뿐이기 때문이다.
const MaxNotifyAttempts = 3

// MaxDigestBatchSize 는 단일 요약 배치 한 번에 최대로 합칠 전송 수다.
//
// 이 상한이 필요한 이유는 자원 때문이다. 한 요약 주기 안에 취약점이 수만 건 쌓일 수 있다(전체 스캔 한 번으로도 충분히 가능).
// 상한이 없으면 클레임이 모든 행을 메모리로 읽고 하나의 초장문 메시지로 렌더링한 뒤, 채널의 길이 제한에서 대부분 잘려 버린다.
// 메모리도 낭비되고, 잘린 취약점들은 **조용히 유실된다**.
// 상한을 두면 초과분은 DB 에 남아 다음 배치가 되고, 다음 주기에 자연스럽게 발송되므로 유실되지 않는다.
//
// 500 을 택한 근거: 메시지로 렌더링했을 때 WeCom 의 4096 바이트 제한 안에서 아직 “읽을 내용이 있는” 양의 규모다.
// 이보다 더 크게 잡아도 잘리는 위치만 뒤로 밀릴 뿐이다.
const MaxDigestBatchSize = 500

// NotificationDelivery 는 전송 작업 한 건이며, 렌더링에 필요한 채널 설정과 이벤트 스냅샷을 포함한다.
type NotificationDelivery struct {
	ID            int64           `json:"id"`
	EventID       int64           `json:"event_id"`
	ChannelID     int64           `json:"channel_id"`
	State         string          `json:"state"`
	Attempts      int             `json:"attempts"`
	NextAttemptAt time.Time       `json:"next_attempt_at"`
	LastError     string          `json:"last_error"`
	BatchID       *int64          `json:"batch_id,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	SentAt        *time.Time      `json:"sent_at,omitempty"`
	Snapshot      json.RawMessage `json:"snapshot,omitempty"`
	// 조인으로 로드한 렌더링 컨텍스트. JSON 에는 포함하지 않는다(DTO 조립은 server 계층에서 한다).
	Channel *NotificationChannel `json:"-"`
	// FindingID/EventKind 는 이벤트에서 가져온다. 이력 목록에서 취약점 상세로 바로 이동하기 위함이다.
	FindingID int64  `json:"finding_id,string"`
	EventKind string `json:"event_kind"`
	// ChannelName/ChannelKind 는 목록 표시용 중복 필드다. 프론트엔드의 추가 조회를 생략하기 위함이다.
	ChannelName string `json:"channel_name"`
	ChannelKind string `json:"channel_kind"`
}

const notificationDeliveryCols = `d.id, d.event_id, d.channel_id, d.state, d.attempts, d.next_attempt_at,
       d.last_error, d.batch_id, d.created_at, d.sent_at`

// joinedDeliveryQuery 는 전송 행의 공통 읽기 형태다: 전송 + 이벤트 스냅샷 + 채널 설정.
// 메시지 하나를 렌더링하려면 세 가지가 모두 필요하므로, 따로 조회하면 왕복이 세 번 생긴다.
const joinedDeliveryQuery = `SELECT ` + notificationDeliveryCols + `,
       e.snapshot, e.kind, e.finding_id,
       c.id, c.name, c.kind, c.enabled, c.config, c.mode, c.filter, c.rate_per_min
FROM notification_deliveries d
JOIN notification_events e ON e.id = d.event_id
JOIN notification_channels c ON c.id = d.channel_id`

func scanNotificationDelivery(sc interface{ Scan(...any) error }) (*NotificationDelivery, error) {
	var (
		dl        NotificationDelivery
		lastErr   sql.NullString
		batchID   sql.NullInt64
		sentAt    sql.NullTime
		snapshot  []byte
		eventKind string
		channel   NotificationChannel
		chEnabled bool
	)
	if err := sc.Scan(&dl.ID, &dl.EventID, &dl.ChannelID, &dl.State, &dl.Attempts, &dl.NextAttemptAt,
		&lastErr, &batchID, &dl.CreatedAt, &sentAt,
		&snapshot, &eventKind, &dl.FindingID,
		&channel.ID, &channel.Name, &channel.Kind, &chEnabled, &channel.Config, &channel.Mode, &channel.Filter, &channel.RatePerMin); err != nil {
		return nil, err
	}
	dl.LastError = lastErr.String
	if batchID.Valid {
		dl.BatchID = &batchID.Int64
	}
	if sentAt.Valid {
		dl.SentAt = &sentAt.Time
	}
	dl.Snapshot = json.RawMessage(snapshot)
	dl.EventKind = eventKind
	dl.ChannelName = channel.Name
	dl.ChannelKind = channel.Kind
	channel.Enabled = &chEnabled
	dl.Channel = &channel
	return &dl, nil
}

// claimQuery 는 한 번의 클레임을 기술한다. 먼저 sel 로 후보를 골라 잠그고, 그다음 이들을 sending 으로 바꾸고
// 리스를 연장한다. sel 안의 lease 위치는 호출자가 $n 플레이스홀더로 두고 직접 인자를 넘긴다.
type claimQuery struct {
	sql  string
	args []any
}

// ClaimRealtimeDeliveries 는 특정 채널의 만료된 실시간 전송을 최대 limit 건 클레임한다.
//
// 전역에서 한꺼번에 가져와 골라 보내는 방식이 아니라 **채널 단위**로 클레임하는 이유: 레이트 리미터는 전송 엔진에서 채널별로 관리된다.
// 이 채널이 이번 라운드에 몇 건을 보낼 수 있는지 먼저 알아야만 같은 수만큼 행을 클레임할 수 있고, 그래야 제한이 재시도 횟수를 소모하지 않는다.
// 반대로 먼저 클레임하고 나중에 버리면, 제한에 막힌 행이 이미 attempts 를 한 번 소모하게 되어
// 3 회의 재시도 예산이 단순한 대기만으로 소진되고 결국 failed 로 떨어진다.
//
// 조건에는 “리스가 만료된 sending”이 포함된다. 이것이 크래시 자가 치유의 착지점이다. lease 는 단일 전송의 최악 소요 시간
// (채널 HTTP 클라이언트 타임아웃 15 초)보다 충분히 커야 한다. 그렇지 않으면 같은 행이 두 dispatcher 에 동시에 전송된다.
// 또한 비활성화된 채널도 여기서 걸러낸다. 비활성화 작업이 이미 기존 전송을 skipped 로 표시했지만,
// 비활성화와 클레임이 동시에 일어나는 경우의 누락을 막기 위해 한 번 더 막는다.
func (d *DB) ClaimRealtimeDeliveries(ctx context.Context, channelID int64, limit int, lease time.Duration) ([]*NotificationDelivery, error) {
	if limit <= 0 {
		return nil, nil
	}
	return d.claimDeliveries(ctx, lease, claimQuery{
		sql: `SELECT dd.id FROM notification_deliveries dd
JOIN notification_channels c ON c.id = dd.channel_id
WHERE dd.channel_id = $1 AND dd.state IN ($2,$3) AND dd.next_attempt_at <= now()
  AND c.enabled AND c.mode = $4
ORDER BY dd.next_attempt_at, dd.id
FOR UPDATE OF dd SKIP LOCKED
LIMIT $5`,
		args: []any{channelID, NotifyStatePending, NotifyStateSending, NotifyModeRealtime, limit},
	}, nil)
}

// DigestBatchDue 는 해당 채널의 기한이 찬 배치가 모였는지를 알려준다. 대기 중인 전송이 있고, **가장 오래된 것의**
// 나이가 요약 주기에 도달했는지를 본다.
//
// 판단 기준은 벽시계 시간이 아니라 가장 오래된 전송의 나이다. 그래야 방금 만든 채널이 정각에 맞추느라
// 한 건짜리 “요약”을 바로 내보내지 않고, 오래 쌓인 배치도 쓸데없이 한 라운드를 더 기다리지 않는다.
//
// ClaimDigestBatch 와 분리한 이유는 의미가 다르기 때문이다: 이 함수는 “지금 보내야 하는가”만 답하고,
// 클레임은 해당 채널의 **모든** 대기 행(아직 나이가 차지 않은 것 포함)을 가져가야 한다. 그렇지 않으면 한 주기가
// 여러 메시지로 쪼개져 요약의 의미가 사라진다.
func (d *DB) DigestBatchDue(ctx context.Context, channelID int64, minAge time.Duration) (bool, error) {
	var due bool
	err := d.QueryRowContext(ctx, `SELECT EXISTS (
  SELECT 1 FROM notification_deliveries d
  JOIN notification_channels c ON c.id = d.channel_id
  WHERE d.channel_id = $1 AND d.state IN ($2,$3) AND c.enabled
  GROUP BY d.channel_id
  HAVING min(d.created_at) <= now() - make_interval(secs => $4)
)`, channelID, NotifyStatePending, NotifyStateSending, int64(minAge.Seconds())).Scan(&due)
	return due, err
}

// ClaimDigestBatch 는 해당 채널에서 현재 기한이 찬 대기 전송을 요약 배치 하나로 클레임한다.
// 배치 한 번에 최대 MaxDigestBatchSize 건이다.
//
// 같은 배치의 모든 전송은 batch_id 를 공유하며, 배치 번호로 집합 안의 최소 id 를 쓴다(안정적이고 읽기 쉬우며
// 추가 시퀀스가 필요 없다). 재시도 시 COALESCE 로 원래 배치 번호를 보존하여, “이 N 건은 함께 발송되었다”는 사실이
// 여러 번 재시도한 뒤에도 성립하도록 한다.
//
// id 오름차순으로 앞의 N 건을 취한다. 무작위로 고르지 않는다. 가장 먼저 생성된 전송이 가장 먼저 나가도록 하여,
// 적체 시 “새 취약점이 먼저 나가고 오래된 취약점은 영원히 뒤로 밀리는” 기아 현상이 생기지 않게 한다.
func (d *DB) ClaimDigestBatch(ctx context.Context, channelID int64, limit int, lease time.Duration) ([]*NotificationDelivery, error) {
	if limit <= 0 {
		return nil, nil
	}
	// limit 은 **메모리 상한**이며, 호출자는 MaxDigestBatchSize 를 넘긴다. 여기서도 한 번 더 자르는 이유는
	// 호출자가 더 큰 값을 넘기는 것을 방지하기 위함이다.
	//
	// “레이트 리밋 한도”를 배치 크기로 받아들이지 않는다. 제한의 단위는 메시지 건수다. 배치 하나는 메시지 하나를 보내고 토큰 하나를 쓰며,
	// server 계층의 takeTokens 가 차감한다. 이것은 “배치 하나에 취약점을 몇 건 담을지”와 다른 차원의 양이다.
	// 과거에는 rate_per_min 이 digest 에도 적용되도록 라운드마다의 요청 예산을 배치 크기로 넘겼다.
	// 그 결과 rate=20/min 채널은 배치마다 취약점을 1 건만 담았고, digest 가 요약 문구가 붙은 실시간 푸시로 퇴화했다.
	// 제한을 바꾸려면 takeTokens 의 want 를 수정하라.
	// 여기는 건드리지 마라.
	if limit > MaxDigestBatchSize {
		limit = MaxDigestBatchSize
	}
	out, err := d.claimDeliveries(ctx, lease, claimQuery{
		sql: `SELECT dd.id FROM notification_deliveries dd
JOIN notification_channels c ON c.id = dd.channel_id
WHERE dd.channel_id = $1 AND dd.state IN ($2,$3) AND dd.next_attempt_at <= now() AND c.enabled
ORDER BY dd.id
FOR UPDATE OF dd SKIP LOCKED
LIMIT $4`,
		args: []any{channelID, NotifyStatePending, NotifyStateSending, limit},
	}, func(tx *sql.Tx, ids []int64) error {
		batchID := ids[0]
		for _, id := range ids {
			if id < batchID {
				batchID = id
			}
		}
		ph, idArgs := placeholders(2, ids)
		_, err := tx.ExecContext(ctx, `UPDATE notification_deliveries SET batch_id = COALESCE(batch_id, $1)
WHERE id IN (`+ph+`)`, append([]any{batchID}, idArgs...)...)
		return err
	})
	return out, err
}

// claimDeliveries 는 “선택 + sending 전환 + 리스 연장 + 전체 행 읽기”를 하나의 트랜잭션 안에서 수행한다.
// postClaim 은 선택적 추가 단계다(요약 배치는 여기서 batch_id 를 기록한다).
func (d *DB) claimDeliveries(ctx context.Context, lease time.Duration, cq claimQuery, postClaim func(*sql.Tx, []int64) error) ([]*NotificationDelivery, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // 커밋 성공 후에는 no-op

	ids, err := selectForClaim(ctx, tx, cq.sql, cq.args...)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, tx.Commit()
	}
	// sending 으로 바꾸고 next_attempt_at 을 미래로 민다. 이 미래 시각이 곧 리스 만료 시각이므로,
	// “리스가 아직 만료되지 않음”과 “재시도 시각이 되지 않음”을 같은 조건식으로 표현할 수 있다. 새 컬럼이 필요 없다.
	ph, idArgs := placeholders(3, ids)
	if _, err := tx.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, attempts=attempts+1, next_attempt_at=now()+make_interval(secs => $2)
WHERE id IN (`+ph+`)`,
		append([]any{NotifyStateSending, lease.Seconds()}, idArgs...)...); err != nil {
		return nil, err
	}
	if postClaim != nil {
		if err := postClaim(tx, ids); err != nil {
			return nil, err
		}
	}
	out, err := loadDeliveriesTx(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

func selectForClaim(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func loadDeliveriesTx(ctx context.Context, tx *sql.Tx, ids []int64) ([]*NotificationDelivery, error) {
	ph, args := placeholders(1, ids)
	rows, err := tx.QueryContext(ctx, joinedDeliveryQuery+` WHERE d.id IN (`+ph+`) ORDER BY d.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*NotificationDelivery{}
	for rows.Next() {
		dl, err := scanNotificationDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, dl)
	}
	return out, rows.Err()
}

// MarkDeliveriesSent 는 전송 묶음을 전달 완료로 표시한다.
func (d *DB) MarkDeliveriesSent(ctx context.Context, ids []int64) error {
	ph, args := placeholders(2, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, sent_at=now(), last_error='' WHERE id IN (`+ph+`)`, append([]any{NotifyStateSent}, args...)...)
	return err
}

// RescheduleDeliveries 는 전송 묶음을 pending 으로 되돌리고 재시도 시각을 뒤로 민다.
//
// 중간 상태를 새로 도입하지 않고 pending 으로 되돌리는 이유는, “남은 기회 횟수”를 한 곳(MaxNotifyAttempts)에서만
// 표현하도록 하기 위해서다. 그래야 상태 머신의 분기가 재시도 정책에 따라 불어나지 않는다.
func (d *DB) RescheduleDeliveries(ctx context.Context, ids []int64, delay time.Duration, errMsg string) error {
	ph, args := placeholders(4, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, next_attempt_at=now()+make_interval(secs => $2), last_error=$3
WHERE id IN (`+ph+`)`,
		append([]any{NotifyStatePending, delay.Seconds(), truncateNotifyError(errMsg)}, args...)...)
	return err
}

// DeferDeliveries 는 전송 묶음을 pending 으로 되돌리고 즉시 다시 클레임할 수 있게 하며, **클레임할 때 기록한 시도 한 번을 취소한다**.
//
// 용도는 하나뿐이다. 요약 메시지를 채널의 길이 상한에 맞춰 나눠 보낼 때, 이번 조각에 담기지 못한 항목을 다음 조각으로 넘기기 위함이다.
// 이것은 실패가 아니므로 재시도 예산을 소모해서는 안 된다. 클레임 시 attempts 는 이미 낙관적으로 +1 되어 있으므로,
// 여기서 반드시 다시 빼야 한다. 그렇지 않으면 500 건의 적체가 조각당 20 건씩 25 조각으로 잘릴 때
// 꼬리 항목들은 세 번째 조각에서 MaxNotifyAttempts 로 failed 판정되지만, 실제로는 한 번도 에러가 난 적이 없다.
//
// GREATEST(...,0) 은 “사람이 수동 재전송하면서 attempts 를 0 으로 초기화한 뒤 여기에 도달한” 경우를 막아,
// 카운트가 음수가 되지 않도록 한다.
func (d *DB) DeferDeliveries(ctx context.Context, ids []int64, reason string) error {
	ph, args := placeholders(3, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$1, attempts=GREATEST(attempts-1, 0), next_attempt_at=now(), last_error=$2
WHERE id IN (`+ph+`)`,
		append([]any{NotifyStatePending, truncateNotifyError(reason)}, args...)...)
	return err
}

// FailDeliveries 는 전송 묶음을 최종 실패로 표시한다. 전송 이력에서 사람이 수동 재전송하기를 기다린다.
func (d *DB) FailDeliveries(ctx context.Context, ids []int64, errMsg string) error {
	// 플레이스홀더는 $3 부터 시작한다. $1 은 state, $2 는 last_error 이다.
	ph, args := placeholders(3, ids)
	if len(args) == 0 {
		return nil
	}
	_, err := d.ExecContext(ctx, `UPDATE notification_deliveries SET state=$1, last_error=$2 WHERE id IN (`+ph+`)`,
		append([]any{NotifyStateFailed, truncateNotifyError(errMsg)}, args...)...)
	return err
}

// RetryNotificationDelivery 는 전송 한 건을 수동으로 재전송한다: pending 으로 리셋, 재시도 카운트 0 으로 초기화,
// 즉시 만료 처리. 카운트를 지우는 것은 의도적이다. 사람이 “재전송”을 누른다는 것은 앞선 실패 원인이 처리되었다는 뜻이므로,
// 예전 카운트로 다시 제한할 이유가 없다.
func (d *DB) RetryNotificationDelivery(ctx context.Context, id int64) error {
	res, err := d.ExecContext(ctx, `UPDATE notification_deliveries
SET state=$2, attempts=0, next_attempt_at=now(), last_error=''
WHERE id=$1 AND state IN ($3,$4)`, id, NotifyStatePending, NotifyStateFailed, NotifyStateSkipped)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("전송 %d 이(가) 없거나 현재 상태에서는 재전송할 수 없습니다", id)
	}
	return nil
}

// NotificationDeliveryFilter 는 전송 이력 조회 조건이다.
type NotificationDeliveryFilter struct {
	ChannelID int64
	State     string
	EventKind string
}

func (f NotificationDeliveryFilter) where() (string, []any) {
	var conds []string
	var args []any
	if f.ChannelID > 0 {
		args = append(args, f.ChannelID)
		conds = append(conds, fmt.Sprintf("d.channel_id=$%d", len(args)))
	}
	if f.State != "" {
		args = append(args, f.State)
		conds = append(conds, fmt.Sprintf("d.state=$%d", len(args)))
	}
	if f.EventKind != "" {
		args = append(args, f.EventKind)
		conds = append(conds, fmt.Sprintf("e.kind=$%d", len(args)))
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// ListNotificationDeliveries 는 전송 이력을 페이지 단위로 반환한다. 최신 항목이 앞에 온다.
func (d *DB) ListNotificationDeliveries(ctx context.Context, f NotificationDeliveryFilter, page, pageSize int) ([]*NotificationDelivery, int, error) {
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 || pageSize > 200 {
		pageSize = 50
	}
	where, args := f.where()

	var total int
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM notification_deliveries d
JOIN notification_events e ON e.id = d.event_id`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	q := fmt.Sprintf("%s%s ORDER BY d.id DESC LIMIT $%d OFFSET $%d",
		joinedDeliveryQuery, where, len(args)+1, len(args)+2)
	rows, err := d.QueryContext(ctx, q, append(args, pageSize, (page-1)*pageSize)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []*NotificationDelivery{}
	for rows.Next() {
		dl, err := scanNotificationDelivery(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, dl)
	}
	return out, total, rows.Err()
}

// truncateNotifyError 는 에러 메시지를 열에 들어갈 수 있는 길이로 자른다. 채널이 돌려준 응답 본문은 길 수 있고
// (특히 범용 Webhook 을 자체 서비스로 보낼 때), 자르지 않으면 이력 목록의 페이로드가 부풀어 오른다.
func truncateNotifyError(msg string) string {
	const max = 500
	if len(msg) <= max {
		return msg
	}
	// 문자 경계에서 뒤로 물러나 자른다. UTF-8 문자가 반쪽만 남으면 프론트엔드에서 깨져 보이기 때문이다.
	cut := max
	for cut > 0 && !isUTF8Start(msg[cut]) {
		cut--
	}
	return msg[:cut] + "…"
}

func isUTF8Start(b byte) bool { return b&0xC0 != 0x80 }

// placeholders 는 start 부터 시작하는 $n 플레이스홀더 문자열과 해당 인자를 생성하여 IN (...) 에 사용한다.
// 예: start=3, ids=[7,8] → "$3,$4", [7,8].
func placeholders(start int, ids []int64) (string, []any) {
	ph := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		ph = append(ph, fmt.Sprintf("$%d", start+i))
		args = append(args, id)
	}
	return strings.Join(ph, ","), args
}
