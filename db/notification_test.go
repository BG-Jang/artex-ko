package db

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Autumn-27/artex/notify"
)

// 이 파일의 테스트들은 모두 실제 PostgreSQL 에 연결한다(DB 가 없으면 건너뜀). 여기서 쓰는 SQL 은
// FOR UPDATE SKIP LOCKED, make_interval, JSONB, 여러 행 IN(...) 플레이스홀더 연결 등을 쓰므로,
// “컴파일은 되지만 실행 시 에러가 날 수 있는” 형태라서 반드시 실제로 실행해야 검증된 것이다.

func notifyTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// newTestChannel 은 채널을 하나 만들고, 테스트가 끝나면 자동으로 삭제한다.
func newTestChannel(t *testing.T, d *DB, kind, mode string, filter string) *NotificationChannel {
	t.Helper()
	if filter == "" {
		filter = `{}`
	}
	ch := &NotificationChannel{
		Name:       "测试渠道-" + t.Name(),
		Kind:       kind,
		Mode:       mode,
		Config:     json.RawMessage(`{"webhook":"https://example.com/hook"}`),
		Filter:     json.RawMessage(filter),
		RatePerMin: 100,
	}
	id, err := d.SaveNotificationChannel(context.Background(), ch)
	if err != nil {
		t.Fatalf("建渠道失败: %v", err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_channels WHERE id=$1`, id) })
	ch.ID = id
	return ch
}

// addTestEvent 는 이벤트를 직접 기록한다(finding 을 거치지 않음). 분배와 전송 테스트에 쓴다.
func addTestEvent(t *testing.T, d *DB, kind string, findingID int64, snap notify.Snapshot) int64 {
	t.Helper()
	snap.Kind = kind
	snap.FindingID = findingID
	id, err := d.AddNotificationEvent(context.Background(), kind, findingID, snap)
	if err != nil {
		t.Fatalf("写事件失败: %v", err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_events WHERE id=$1`, id) })
	return id
}

func TestNotificationAssetNamesResolvesAndPreservesOrder(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	// 자산 세 종류는 각자 표시 기준이 다르다: 도메인, IP, URL.
	insertAsset := func(query, value string) int64 {
		t.Helper()
		var id int64
		if err := d.QueryRow(query, value).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	domID := insertAsset(`INSERT INTO assets(type, domain) VALUES('subdomain',$1) RETURNING id`, "a.example.com")
	ipID := insertAsset(`INSERT INTO assets(type, ip) VALUES('ip',$1) RETURNING id`, "10.1.2.3")
	svcID := insertAsset(`INSERT INTO assets(type, url) VALUES('service',$1) RETURNING id`, "https://a.example.com/admin")
	t.Cleanup(func() {
		d.Exec(`DELETE FROM assets WHERE id IN ($1,$2,$3)`, domID, ipID, svcID)
	})

	// 입력 순서는 일부러 뒤섞었고, 존재하지 않는 id 도 하나 포함한다.
	got, err := d.NotificationAssetNames(ctx, []int64{svcID, 999999999, domID, ipID, svcID})
	if err != nil {
		t.Fatalf("解析资产名失败: %v", err)
	}
	want := []string{"https://a.example.com/admin", "a.example.com", "10.1.2.3"}
	if len(got) != len(want) {
		t.Fatalf("资产名数量不符，期望 %v 得到 %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("顺序/取值不符，期望 %v 得到 %v", want, got)
		}
	}
}

// TestRecordNotificationEventTxUnwindsOnFailure 는 세이브포인트 메커니즘의 핵심 케이스다:
// 트랜잭션 안에서 먼저 notification_events 쓰기가 반드시 실패하도록 만든다(임시로 항상 false 인 제약 추가).
// 단언: ① 함수가 false 를 반환할 것 ② 트랜잭션이 aborted 상태에 빠지지 않아 이후 문장을 계속 실행할 수 있을 것.
//
// 세이브포인트가 없다면 PostgreSQL 은 트랜잭션 전체를 무효화하고, 이후 모든 문장이
// "current transaction is aborted" 로 실패한다. “알림 테이블 문제 때문에 취약점이 저장되지 못하는”
// 장애 경로가 바로 이것이다.
//
// 여기서는 일부러 COMMIT 이 아니라 **ROLLBACK 으로 마무리한다**: PG 에서 ALTER TABLE 은 트랜잭션 가능한 DDL 이므로,
// 커밋하면 그 임시 제약이 스키마에 영구히 남아 이후 모든 케이스를 함께 실패시킨다.
// 롤백은 DDL 도 자동으로 되돌리므로 수동 정리가 필요 없다. 단언은 “트랜잭션이 아직 살아 있는가”만 확인하면 되며,
// 실제로 커밋할 필요는 없다.
func TestRecordNotificationEventTxUnwindsOnFailure(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	// 방어적 정리: 과거 실행이 이 제약을 남겼다면 먼저 제거한다.
	if _, err := d.Exec(`ALTER TABLE notification_events DROP CONSTRAINT IF EXISTS notify_test_never`); err != nil {
		t.Fatal(err)
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck // 임시 제약 조건을 되돌린다. 함수 주석 참고

	// NOT VALID: 이후에 쓰여지는 행에만 제약을 건다. 기존 DB 에 있는 과거 이벤트는 검증하지 않는다
	// (그렇지 않으면 기존 행 중 위반이 있을 때 제약을 추가할 수 없다).
	if _, err := tx.ExecContext(ctx, `ALTER TABLE notification_events ADD CONSTRAINT notify_test_never CHECK (false) NOT VALID`); err != nil {
		t.Fatalf("加临时约束失败: %v", err)
	}
	if RecordNotificationEventTx(ctx, tx, notify.EventFindingCreated, 1, notify.Snapshot{Severity: "high"}) {
		t.Fatal("在必然失败的约束下仍报告写入成功")
	}
	// 핵심 단언: 트랜잭션을 아직 쓸 수 있다.
	var one int
	if err := tx.QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil {
		t.Fatalf("事务已被污染（保存点未生效）: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	// DDL 이 롤백과 함께 되돌려졌는지 확인하여, 이후 케이스에 지뢰를 남기지 않는다.
	var exists bool
	if err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_constraint WHERE conname='notify_test_never')`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("临时约束未被回滚撤销，会污染后续用例")
	}
}

func TestFanOutRoutesEventsByFilter(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	all := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	onlyCritical := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{"min_severity":"critical"}`)
	sqlOnly := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{"vulnclass_include":["SQL"]}`)

	highSQL := addTestEvent(t, d, notify.EventFindingCreated, 1001, notify.Snapshot{Severity: "high", VulnClass: "SQL注入"})
	lowXSS := addTestEvent(t, d, notify.EventFindingCreated, 1002, notify.Snapshot{Severity: "low", VulnClass: "XSS"})
	criticalXSS := addTestEvent(t, d, notify.EventFindingCreated, 1003, notify.Snapshot{Severity: "critical", VulnClass: "XSS"})

	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatalf("分派失败: %v", err)
	}

	cases := []struct {
		name    string
		eventID int64
		channel int64
		want    bool
	}{
		{"全收渠道收到 high", highSQL, all.ID, true},
		{"全收渠道收到 low", lowXSS, all.ID, true},
		{"仅严重渠道跳过 high", highSQL, onlyCritical.ID, false},
		{"仅严重渠道收到 critical", criticalXSS, onlyCritical.ID, true},
		{"仅SQL渠道收到 SQL", highSQL, sqlOnly.ID, true},
		{"仅SQL渠道跳过 XSS", lowXSS, sqlOnly.ID, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var exists bool
			if err := d.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM notification_deliveries WHERE event_id=$1 AND channel_id=$2)`,
				tc.eventID, tc.channel).Scan(&exists); err != nil {
				t.Fatal(err)
			}
			if exists != tc.want {
				t.Fatalf("投递是否存在: 期望 %v 得到 %v", tc.want, exists)
			}
		})
	}

	// 다시 분배해도 중복 전송이 생기지 않아야 한다(fanned_out 멱등성).
	events, deliveries, err := d.FanOutPendingEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if events != 0 || deliveries != 0 {
		t.Fatalf("已分派的事件不应被再次处理，得到 events=%d deliveries=%d", events, deliveries)
	}
}

// TestFanOutMarksEventsWithNoMatchingChannel 은 “이벤트가 어떤 채널에도 일치하지 않는” 경우를 다룬다.
// 이런 이벤트도 반드시 분배 완료로 표시되어야 한다. 그렇지 않으면 분배 대기 집합에 영원히 남아 매 tick 마다 다시 스캔된다.
func TestFanOutMarksEventsWithNoMatchingChannel(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	pick := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{"vulnclass_include":["绝不匹配的类型"]}`)
	_ = pick

	ev := addTestEvent(t, d, notify.EventFindingCreated, 2001, notify.Snapshot{Severity: "high", VulnClass: "XSS"})
	_, deliveries, err := d.FanOutPendingEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if deliveries != 0 {
		t.Fatalf("不该产生投递，得到 %d", deliveries)
	}
	var fanned bool
	if err := d.QueryRowContext(ctx, `SELECT fanned_out FROM notification_events WHERE id=$1`, ev).Scan(&fanned); err != nil {
		t.Fatal(err)
	}
	if !fanned {
		t.Fatal("未命中渠道的事件也必须标记为已分派，否则会被无限重扫")
	}
}

func TestClaimRealtimeDeliveriesHonorsLeaseAndMode(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	realtime := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	digest := newTestChannel(t, d, notify.KindDingTalk, NotifyModeDigest, `{}`)

	addTestEvent(t, d, notify.EventFindingCreated, 3001, notify.Snapshot{Severity: "high", VulnClass: "XSS"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}

	// 실시간 클레임은 realtime 채널의 것만 가져와야 하며, digest 채널의 것을 건드리면 안 된다.
	got, err := d.ClaimRealtimeDeliveries(ctx, realtime.ID, 10, time.Minute)
	if err != nil {
		t.Fatalf("领取失败: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("应领到 1 条，得到 %d", len(got))
	}
	if got[0].State != NotifyStateSending || got[0].Attempts != 1 {
		t.Fatalf("领取后应为 sending 且 attempts=1，得到 state=%s attempts=%d", got[0].State, got[0].Attempts)
	}
	// 조인으로 로드한 렌더링 컨텍스트가 모두 있어야 한다(채널 설정 + 이벤트 스냅샷 + finding id).
	if got[0].Channel == nil || len(got[0].Channel.Config) == 0 {
		t.Fatal("领取结果缺少渠道配置，渲染会失败")
	}
	if got[0].FindingID != 3001 {
		t.Fatalf("finding id 未从事件带出，得到 %d", got[0].FindingID)
	}

	// 리스가 만료되지 않았으면 두 번째 클레임은 비어 있어야 한다. 이것이 “같은 행이 두 dispatcher 에 동시에 전송되지 않는다”
	// 는 보장이다.
	again, err := d.ClaimRealtimeDeliveries(ctx, realtime.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("租约期内不应重复领取，得到 %d 条", len(again))
	}

	// digest 채널의 전송은 실시간 클레임에 잡히면 안 된다.
	left, err := d.ClaimRealtimeDeliveries(ctx, digest.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("实时领取不应拿到 digest 渠道的投递，得到 %d 条", len(left))
	}
}

// TestClaimExpiredLeaseRecovers 는 크래시 자가 치유를 검증한다: 전송 도중 프로세스가 죽으면 sending 행이 남는데,
// 리스가 만료된 뒤에는 다시 클레임할 수 있어야 한다. 그렇지 않으면 이 전송은 영원히 멈춘다.
func TestClaimExpiredLeaseRecovers(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	addTestEvent(t, d, notify.EventFindingCreated, 4001, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	first, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute)
	if err != nil || len(first) != 1 {
		t.Fatalf("首次领取失败: %v (%d 条)", err, len(first))
	}
	// 리스를 수동으로 과거로 밀어 “리스가 만료됨”을 흉내 낸다.
	if _, err := d.Exec(`UPDATE notification_deliveries SET next_attempt_at = now() - interval '1 minute' WHERE id=$1`, first[0].ID); err != nil {
		t.Fatal(err)
	}
	second, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 {
		t.Fatalf("租约过期的 sending 行应可被重新领取，得到 %d 条", len(second))
	}
	if second[0].Attempts != 2 {
		t.Fatalf("重新领取应累加尝试次数，得到 %d", second[0].Attempts)
	}
}

func TestClaimSkipsDisabledChannel(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	addTestEvent(t, d, notify.EventFindingCreated, 5001, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	// 비활성화하면 기존 대기 전송도 함께 skipped 로 표시된다.
	if err := d.SetNotificationChannelEnabled(ctx, ch.ID, false); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := d.QueryRow(`SELECT state FROM notification_deliveries WHERE channel_id=$1`, ch.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStateSkipped {
		t.Fatalf("停用渠道的存量待发投递应被标记为 skipped，得到 %s", state)
	}
	got, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("已停用渠道不应能被领取，得到 %d 条", len(got))
	}
}

func TestDigestBatchDueAndStableBatchID(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeDigest, `{}`)
	for i := 0; i < 3; i++ {
		addTestEvent(t, d, notify.EventFindingCreated, int64(6000+i), notify.Snapshot{Severity: "high"})
	}
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}

	// 배치가 방금 만들어져 나이가 0 이므로, 30 분 주기에서는 만료되지 않아야 한다.
	due, err := d.DigestBatchDue(ctx, ch.ID, 30*time.Minute)
	if err != nil {
		t.Fatalf("判断批次到期失败: %v", err)
	}
	if due {
		t.Fatal("刚建立的批次不应立即到期")
	}

	// 세 전송의 생성 시각을 모두 과거로 밀어, 주기를 다 채운 배치를 흉내 낸다.
	if _, err := d.Exec(`UPDATE notification_deliveries SET created_at = now() - interval '40 minutes' WHERE channel_id=$1`, ch.ID); err != nil {
		t.Fatal(err)
	}
	due, err = d.DigestBatchDue(ctx, ch.ID, 30*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !due {
		t.Fatal("超过周期的批次应判定为到期")
	}

	batch, err := d.ClaimDigestBatch(ctx, ch.ID, MaxDigestBatchSize, time.Minute)
	if err != nil {
		t.Fatalf("领取汇总批次失败: %v", err)
	}
	if len(batch) != 3 {
		t.Fatalf("汇总应一次领走全部 3 条，得到 %d 条", len(batch))
	}
	if batch[0].BatchID == nil {
		t.Fatal("汇总批次必须写 batch_id，否则历史里看不出这几条是一起发的")
	}
	firstBatchID := *batch[0].BatchID
	for _, dl := range batch {
		if dl.BatchID == nil || *dl.BatchID != firstBatchID {
			t.Fatalf("同一批次应共享 batch_id，得到 %v vs %d", dl.BatchID, firstBatchID)
		}
	}

	// 이 배치 전체를 실패로 재배치한 뒤 다시 클레임할 때 batch_id 는 원래 값을 유지해야 한다(COALESCE 의 역할):
	// 그렇지 않으면 한 번의 재시도로 “이 배치는 함께 발송되었다”는 사실이 지워진다.
	//
	// 한 건만 재배치하는 것이 아니라 배치 전체를 재배치해야 한다. 전송 엔진은 요약 메시지를 보낼 때 그렇게 처리한다
	// (메시지 하나가 배치 전체를 대표하므로 성패를 함께 한다). 한 건만 재배치하면 나머지는 여전히 리스 기간 안에 있어서,
	// 재클레임은 자연히 그 한 건만 가져온다.
	allIDs := make([]int64, 0, len(batch))
	for _, dl := range batch {
		allIDs = append(allIDs, dl.ID)
	}
	if err := d.RescheduleDeliveries(ctx, allIDs, time.Second, "模拟失败"); err != nil {
		t.Fatal(err)
	}
	// 리스를 과거로 밀어 백오프 시간이 지났음을 흉내 낸다.
	if _, err := d.Exec(`UPDATE notification_deliveries SET next_attempt_at = now() - interval '1 minute' WHERE channel_id=$1`, ch.ID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := d.ClaimDigestBatch(ctx, ch.ID, MaxDigestBatchSize, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(reclaimed) != 3 {
		t.Fatalf("重领应拿到全部 3 条，得到 %d", len(reclaimed))
	}
	if reclaimed[0].BatchID == nil || *reclaimed[0].BatchID != firstBatchID {
		t.Fatalf("重试后 batch_id 应保持原值 %d，得到 %v", firstBatchID, reclaimed[0].BatchID)
	}
}

func TestDeliveryStateTransitions(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	addTestEvent(t, d, notify.EventFindingCreated, 7001, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	got, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute)
	if err != nil || len(got) != 1 {
		t.Fatalf("领取失败: %v (%d)", err, len(got))
	}
	id := got[0].ID

	if err := d.RescheduleDeliveries(ctx, []int64{id}, time.Second, "网络抖动"); err != nil {
		t.Fatal(err)
	}
	var state, lastErr string
	if err := d.QueryRow(`SELECT state, last_error FROM notification_deliveries WHERE id=$1`, id).Scan(&state, &lastErr); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStatePending || lastErr != "网络抖动" {
		t.Fatalf("重排后应为 pending 并记录原因，得到 state=%s err=%q", state, lastErr)
	}

	if err := d.FailDeliveries(ctx, []int64{id}, "重试耗尽"); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT state FROM notification_deliveries WHERE id=$1`, id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStateFailed {
		t.Fatalf("应为 failed，得到 %s", state)
	}

	// 수동 재전송은 재시도 카운트를 0 으로 초기화하고 즉시 만료시켜야 한다. 그렇지 않으면 이전 실패 예산을 물려받는다.
	if err := d.RetryNotificationDelivery(ctx, id); err != nil {
		t.Fatalf("重发失败: %v", err)
	}
	var attempts int
	var next time.Time
	if err := d.QueryRow(`SELECT state, attempts, next_attempt_at FROM notification_deliveries WHERE id=$1`, id).Scan(&state, &attempts, &next); err != nil {
		t.Fatal(err)
	}
	if state != NotifyStatePending || attempts != 0 {
		t.Fatalf("重发后应为 pending 且 attempts=0，得到 state=%s attempts=%d", state, attempts)
	}
	if next.After(time.Now().Add(time.Second)) {
		t.Fatal("重发应立即可领")
	}

	// 이미 전달된 전송은 재전송할 수 없어야 한다.
	if err := d.MarkDeliveriesSent(ctx, []int64{id}); err != nil {
		t.Fatal(err)
	}
	if err := d.RetryNotificationDelivery(ctx, id); err == nil {
		t.Fatal("已送达的投递不该允许重发")
	}
}

func TestListNotificationDeliveriesPagingAndFilter(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	for i := 0; i < 5; i++ {
		addTestEvent(t, d, notify.EventFindingCreated, int64(8000+i), notify.Snapshot{Severity: "high", Name: "分页测试"})
	}
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ClaimRealtimeDeliveries(ctx, ch.ID, 10, time.Minute); err != nil {
		t.Fatal(err)
	}

	page1, total, err := d.ListNotificationDeliveries(ctx, NotificationDeliveryFilter{ChannelID: ch.ID, State: NotifyStateSending}, 1, 2)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if total != 5 {
		t.Fatalf("总数应为 5，得到 %d", total)
	}
	if len(page1) != 2 {
		t.Fatalf("每页 2 条，得到 %d", len(page1))
	}
	// 최신 항목이 앞에 온다: 첫 페이지의 첫 id 는 두 번째 페이지의 첫 id 보다 커야 한다.
	page2, _, err := d.ListNotificationDeliveries(ctx, NotificationDeliveryFilter{ChannelID: ch.ID, State: NotifyStateSending}, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 2 || page2[0].ID >= page1[0].ID {
		t.Fatalf("分页顺序应为新的在前，得到 page1[0]=%d page2[0]=%d", page1[0].ID, page2[0].ID)
	}
	// 렌더링 컨텍스트도 이력과 함께 반환되어야 한다. 그렇지 않으면 목록이 “무엇을 보냈는지”를 표시할 수 없다.
	if page1[0].ChannelName == "" || page1[0].FindingID == 0 {
		t.Fatalf("历史项缺少展示字段: %+v", page1[0])
	}

	// 상태 필터: pending 이 없어야 한다.
	pending, totalPending, err := d.ListNotificationDeliveries(ctx, NotificationDeliveryFilter{ChannelID: ch.ID, State: NotifyStatePending}, 1, 50)
	if err != nil {
		t.Fatal(err)
	}
	if totalPending != 0 || len(pending) != 0 {
		t.Fatalf("不该有 pending 投递，得到 %d 条 (total=%d)", len(pending), totalPending)
	}
}

func TestSetFindingStatusWithNotifyOnlyEmitsOnRealChange(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	tk, err := d.CreateTask("通知状态变更测试", "目标", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(tk.ID)
	es := d.Exploration(tk.ExplorationID)
	f, err := es.RecordFinding(ctx, RecordFindingInput{
		TaskID: tk.ID, Worker: "test", VulnClass: "SQL注入", Name: "状态变更用例",
		Severity: "high", Summary: "摘要",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_events WHERE finding_id=$1`, f.FindingID) })

	// 저장 시 finding_created 이벤트 한 건이 이미 등록되어 있으므로, 먼저 그 수를 세어 기준선으로 삼는다.
	var base int
	if err := d.QueryRow(`SELECT count(*) FROM notification_events WHERE finding_id=$1`, f.FindingID).Scan(&base); err != nil {
		t.Fatal(err)
	}
	if base < 1 {
		t.Fatal("漏洞落库应在同一事务里登记一条推送事件")
	}

	// 같은 상태로 바꾸면 이벤트가 생기지 않아야 한다(중복 제출로 푸시 노이즈가 생기지 않도록).
	from, found, notified, err := d.SetFindingStatusWithNotify(ctx, f.FindingID, "pending")
	if err != nil || !found {
		t.Fatalf("状态设置失败: found=%v err=%v", found, err)
	}
	if notified {
		t.Fatal("状态未变化时不应登记推送事件")
	}
	if from != "pending" {
		t.Fatalf("应返回变更前状态 pending，得到 %q", from)
	}

	// 실제 변경: 이벤트가 등록되고 from/to 가 기록되어야 한다.
	from, found, notified, err = d.SetFindingStatusWithNotify(ctx, f.FindingID, "fixed")
	if err != nil || !found {
		t.Fatalf("状态设置失败: found=%v err=%v", found, err)
	}
	if !notified {
		t.Fatal("状态实际变更时应登记推送事件")
	}
	if from != "pending" {
		t.Fatalf("from 应为 pending，得到 %q", from)
	}
	var snapshot []byte
	if err := d.QueryRow(`SELECT snapshot FROM notification_events WHERE finding_id=$1 AND kind=$2`,
		f.FindingID, notify.EventFindingStatusChanged).Scan(&snapshot); err != nil {
		t.Fatalf("未找到状态变更事件: %v", err)
	}
	var snap notify.Snapshot
	if err := json.Unmarshal(snapshot, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.FromStatus != "pending" || snap.ToStatus != "fixed" {
		t.Fatalf("快照里的状态流转不对: %s → %s", snap.FromStatus, snap.ToStatus)
	}
	// 스냅샷에는 렌더링에 필요한 필드가 있어야 한다. 그렇지 않으면 상태 변경 메시지가 빈 껍데기가 된다.
	if snap.VulnClass != "SQL注入" || snap.Severity != "high" || snap.Name != "状态变更用例" {
		t.Fatalf("快照缺少渲染字段: %+v", snap)
	}
	var status string
	if err := d.QueryRow(`SELECT status FROM findings WHERE id=$1`, f.FindingID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "fixed" {
		t.Fatalf("状态应已更新为 fixed，得到 %s", status)
	}

	// 존재하지 않는 취약점: found=false, 에러 없음.
	if _, found, _, err := d.SetFindingStatusWithNotify(ctx, 999999999, "fixed"); err != nil || found {
		t.Fatalf("不存在的漏洞应返回 found=false 且无错，得到 found=%v err=%v", found, err)
	}
}

func TestNotificationStatsSnapshot(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	addTestEvent(t, d, notify.EventFindingCreated, 9001, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	stats, err := d.NotificationStatsSnapshot(ctx)
	if err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if stats.Channels < 1 || stats.ChannelsOn < 1 {
		t.Fatalf("渠道计数不对: %+v", stats)
	}
	if stats.Pending < 1 {
		t.Fatalf("应统计到待发投递: %+v", stats)
	}
	// 방금 만든 전송의 적체 나이는 0 에 가까워야 하며, 음수나 과도한 값이 아니어야 한다.
	if stats.BacklogAgeMS < 0 || stats.BacklogAgeMS > int64(time.Hour/time.Millisecond) {
		t.Fatalf("积压年龄不合法: %d ms", stats.BacklogAgeMS)
	}
	_ = ch
}

func TestNotificationChannelCRUDRoundTrip(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	ch := &NotificationChannel{
		Name:       "CRUD 往返",
		Kind:       notify.KindEmail,
		Mode:       NotifyModeDigest,
		Config:     json.RawMessage(`{"host":"smtp.example.com","port":587,"from":"a@b.c","to":["x@y.z"]}`),
		Filter:     json.RawMessage(`{"min_severity":"medium","on_status_change":true}`),
		RatePerMin: 42,
	}
	id, err := d.SaveNotificationChannel(ctx, ch)
	if err != nil {
		t.Fatalf("新建失败: %v", err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_channels WHERE id=$1`, id) })

	got, err := d.NotificationChannelByID(ctx, id)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if got.Mode != NotifyModeDigest || got.RatePerMin != 42 || got.Name != "CRUD 往返" {
		t.Fatalf("往返字段不一致: %+v", got)
	}
	if !got.IsEnabled() {
		t.Fatal("默认应为启用")
	}
	var cfg map[string]any
	if err := json.Unmarshal(got.Config, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["host"] != "smtp.example.com" {
		t.Fatalf("配置未正确落库: %v", cfg)
	}
	var filter notify.Filter
	if err := json.Unmarshal(got.Filter, &filter); err != nil {
		t.Fatal(err)
	}
	if filter.MinSeverity != "medium" || !filter.OnStatusChange {
		t.Fatalf("过滤条件未正确落库: %+v", filter)
	}

	// 갱신 후 다시 읽는다.
	got.Name = "改名了"
	off := false
	got.Enabled = &off
	if _, err := d.SaveNotificationChannel(ctx, got); err != nil {
		t.Fatal(err)
	}
	after, err := d.NotificationChannelByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if after.Name != "改名了" || after.IsEnabled() {
		t.Fatalf("更新未生效: %+v", after)
	}

	// 삭제 후에는 “존재하지 않음”을 보고해야 하며, 조용히 성공해서는 안 된다.
	if err := d.DeleteNotificationChannel(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := d.NotificationChannelByID(ctx, id); err != ErrNotificationChannelNotFound {
		t.Fatalf("期望 ErrNotificationChannelNotFound，得到 %v", err)
	}
	if err := d.DeleteNotificationChannel(ctx, id); err != ErrNotificationChannelNotFound {
		t.Fatalf("重复删除应报不存在，得到 %v", err)
	}
}

// TestSaveNotificationChannelKeepsExplicitZeroRate 는 과거에 잘못 작성되었던 부분을 고정한다:
// **0 은 유효한 설정이며 의미는 “제한 없음”이므로, db 계층이 “지정되지 않음”으로 보고 기본값으로 덮어써서는 안 된다**.
//
// 과거 버그: SaveNotificationChannel 에 `if RatePerMin <= 0 { 기본값 }` 이 있어서,
// 문서, UI 안내, takeTokens 는 모두 “0=제한 없음”으로 해석하는데, 이 저장 계층만 몰래
// 20 (DingTalk/WeCom/Telegram) 또는 100 (Feishu) 으로 바꿔 버렸다. 운영자는 제한이 풀렸다고 생각하지만 실제로는 막혀 있었고,
// 아무 안내도 없었다. “지정되지 않음”과 “명시적 0”의 차이는 요청 본문만 표현할 수 있으므로,
// 기본값은 server 계층에서 채우고(notifyCreateChannel 참고), db 계층은 저장만 담당한다.
func TestSaveNotificationChannelKeepsExplicitZeroRate(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	// 명시적 0(제한 없음): 그대로 저장되어야 한다.
	unlimited := &NotificationChannel{
		Name: "不限流", Kind: notify.KindDingTalk, RatePerMin: 0,
		Config: json.RawMessage(`{"webhook":"https://example.com/h"}`),
	}
	id, err := d.SaveNotificationChannel(ctx, unlimited)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_channels WHERE id=$1`, id) })
	got, err := d.NotificationChannelByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.RatePerMin != 0 {
		t.Fatalf("显式 0 表示不限流，必须原样保存，得到 %d", got.RatePerMin)
	}
	if got.Mode != NotifyModeRealtime {
		t.Fatalf("默认模式应为 realtime，得到 %s", got.Mode)
	}

	// 음수는 잘못된 입력이므로, 조용히 다른 값으로 바꾸지 말고 거부해야 한다.
	bad := &NotificationChannel{
		Name: "负限流", Kind: notify.KindDingTalk, RatePerMin: -1,
		Config: json.RawMessage(`{"webhook":"https://example.com/h"}`),
	}
	if _, err := d.SaveNotificationChannel(ctx, bad); err == nil {
		t.Fatal("负限流应被拒绝")
	}
}

// TestDeleteChannelCascadesDeliveries 는 외래 키 동작을 고정한다: 채널을 삭제하면 전송 이력도 함께 사라지지만
// (설정이 없어지면 이력을 해석할 수 없으므로), 이벤트 자체는 남아야 한다. 다른 채널이 아직 참조할 수 있기 때문이다.
func TestDeleteChannelCascadesDeliveries(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeRealtime, `{}`)
	ev := addTestEvent(t, d, notify.EventFindingCreated, 9101, notify.Snapshot{Severity: "high"})
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	var before int
	if err := d.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, ch.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before == 0 {
		t.Fatal("前置条件不成立：未产生投递")
	}
	if err := d.DeleteNotificationChannel(ctx, ch.ID); err != nil {
		t.Fatal(err)
	}
	var after int
	if err := d.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, ch.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != 0 {
		t.Fatalf("渠道删除后其投递应级联删除，仍有 %d 条", after)
	}
	var evExists bool
	if err := d.QueryRow(`SELECT EXISTS(SELECT 1 FROM notification_events WHERE id=$1)`, ev).Scan(&evExists); err != nil {
		t.Fatal(err)
	}
	if !evExists {
		t.Fatal("删渠道不应连带删除事件本身")
	}
}

// TestClaimDigestBatchHonorsCallerLimit 은 감사에서 지적된 허점을 다룬다:
// 요약 채널은 이전에 토큰 버킷을 완전히 우회했다. allow 가 takeTokens 에서 차감되지만 아무도 쓰지 않았고,
// rate_per_min 은 digest 모드에 아무 효과가 없었다. 이제 limit 도 제약에 참여한다.
func TestClaimDigestBatchHonorsCallerLimit(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()
	ch := newTestChannel(t, d, notify.KindDingTalk, NotifyModeDigest, `{}`)
	for i := 0; i < 10; i++ {
		addTestEvent(t, d, notify.EventFindingCreated, int64(7000+i), notify.Snapshot{Severity: "high"})
	}
	if _, _, err := d.FanOutPendingEvents(ctx, 100); err != nil {
		t.Fatal(err)
	}
	// limit=3: 3 건만 클레임할 수 있고, 나머지는 DB 에 남는다.
	got, err := d.ClaimDigestBatch(ctx, ch.ID, 3, time.Minute)
	if err != nil {
		t.Fatalf("领取失败: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("应按调用方限流额度只领 3 条，得到 %d", len(got))
	}
	// limit=0 은 이번 라운드의 쿼터가 소진되었음을 뜻한다. 한 건도 클레임하지 않아야 하며, 에러도 나지 않아야 한다.
	if got, err := d.ClaimDigestBatch(ctx, ch.ID, 0, time.Minute); err != nil || len(got) != 0 {
		t.Fatalf("额度为 0 时应领 0 条且不报错，得到 %d 条 err=%v", len(got), err)
	}
}

// TestFinishFindingRetestEmitsStatusChange 는 감사에서 지적된 완전성 결함을 다룬다:
// 재테스트 결론이 “수정됨”이면 상태는 실제로 바뀌지만, 그 UPDATE 가 알림을 포함한 버전을 우회하고 DB 에 직접 쓰였다.
// 그래서 on_status_change 를 설정한 채널은 이런 상태 전이에 대해 푸시를 전혀 받지 못했고, 화면에서는 상태가 몰래 바뀌었으며,
// 운영자는 플랫폼을 열어 봐야 알았다.
//
// 이 케이스는 “모든 상태 변경 경로가 상태 변경 이벤트를 등록해야 한다”는 것을 고정한다.
func TestFinishFindingRetestEmitsStatusChange(t *testing.T) {
	d := notifyTestDB(t)
	ctx := context.Background()

	tk, err := d.CreateTask("复测推送测试", "目标", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(tk.ID)
	es := d.Exploration(tk.ExplorationID)
	f, err := es.RecordFinding(ctx, RecordFindingInput{
		TaskID: tk.ID, Worker: "test", VulnClass: "SQL注入", Name: "复测目标",
		Severity: "high", Summary: "摘要",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Exec(`DELETE FROM notification_events WHERE finding_id=$1`, f.FindingID) })

	// 재테스트 레코드를 하나 만들고 바로 완료 상태로 민다.
	rt, _, _, err := d.CreateFindingRetest(ctx, f.FindingID, "复核")
	if err != nil {
		t.Fatal(err)
	}
	if rt.ConversationID == nil {
		t.Fatal("复测应关联一个会话")
	}
	// 재테스트는 결론을 기록하려면 먼저 running 상태에 들어가야 한다(실제 흐름과 동일하게).
	if ok, err := d.StartFindingRetest(ctx, rt.ID); err != nil || !ok {
		t.Fatalf("启动复测失败: ok=%v err=%v", ok, err)
	}
	if err := d.RecordFindingRetestResult(ctx, *rt.ConversationID, "fixed", "已修复", "证据"); err != nil {
		t.Fatal(err)
	}
	if err := d.FinishFindingRetest(rt.ID, "completed", ""); err != nil {
		t.Fatalf("结束复测失败: %v", err)
	}

	var status string
	if err := d.QueryRow(`SELECT status FROM findings WHERE id=$1`, f.FindingID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != FindingFixed {
		t.Fatalf("复测判已修复后状态应为 fixed，得到 %s", status)
	}

	// 핵심 단언: 상태 변경 이벤트가 정확히 하나 있고, from/to 가 맞아야 한다.
	var snapshot []byte
	err = d.QueryRow(`SELECT snapshot FROM notification_events WHERE finding_id=$1 AND kind=$2 ORDER BY id DESC LIMIT 1`,
		f.FindingID, notify.EventFindingStatusChanged).Scan(&snapshot)
	if err != nil {
		t.Fatalf("复测判已修复应登记状态变更推送事件（否则配了 on_status_change 的渠道收不到）: %v", err)
	}
	var snap notify.Snapshot
	if err := json.Unmarshal(snapshot, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.FromStatus != "pending" || snap.ToStatus != FindingFixed {
		t.Fatalf("快照的状态流转不对: %s → %s", snap.FromStatus, snap.ToStatus)
	}
	// 스냅샷에는 렌더링에 필요한 필드가 있어야 한다. 그렇지 않으면 푸시된 메시지가 빈 껍데기가 된다.
	if snap.Name != "复测目标" || snap.Severity != "high" {
		t.Fatalf("快照缺少渲染字段: %+v", snap)
	}
}
