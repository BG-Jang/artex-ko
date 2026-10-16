package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/notify"
)

// 이 파일은 알림 발송 기능의 end-to-end 동작을 검증한다: 취약점 저장 → 이벤트 → 분배 → 실제 HTTP 전송.
//
// 보안상 주의할 점: 이 케이스들은 **전역 Notifier.step() 을 호출하지 않고**, 자신이 만든
// 채널에 대해서만 stepRealtime/stepDigest 를 호출한다. step() 은 DB 의 활성 채널을 전부 순회하므로,
// 실제 DingTalk/WeCom 봇이 설정된 개발 DB 에서 테스트를 돌리면, 전역 step 이 테스트 도중에
// 생긴 취약점을 실제 그룹으로 발송해 버린다. 채널별로 호출해야 영향 범위가 테스트가 만든 가짜 수신 서버로 한정된다.
//
// 정리: 케이스 종료 시 해당 케이스가 만든 이벤트(연쇄 삭제로 전달 기록 포함)와 채널을 삭제해, 실제 채널에 적체가 남지 않게 한다.
//
// 단언 기준: stepRealtime/stepDigest 는 반환값이 없고 내부에서 로그만 남기므로, 여기서는
// **관찰 가능한 외부 동작**(가짜 수신 서버가 받은 내용, 전달 행이 도달한 상태)을 단언한다. 함수의
// 반환값을 단언하는 것보다 실제 호출 경로에 가깝다 — 반환값에 스텁을 거는 방식보다 낫다.

// notifyFixture 는 이 파일 케이스들의 공통 픽스처다.
type notifyFixture struct {
	s       *Server
	pg      *db.DB
	request func(method, path, body string) *httptest.ResponseRecorder
	n       *Notifier
	// 직접 만든 task/exploration: 케이스가 취약점을 기록하는 곳으로, 다른 케이스와 데이터를 격리한다.
	taskID int64
	expID  int64
	// cleanupMark 이후에 생성된 이벤트는 정리 시 함께 삭제한다.
	cleanupMark int64
}

func newNotifyFixture(t *testing.T) *notifyFixture {
	t.Helper()
	// 이 파일의 가짜 수신 서버는 모두 127.0.0.1 에서 실행되는데, 발송은 기본적으로 루프백 주소를 거부한다
	// (동일 머신의 서비스와 클라우드 메타데이터를 노리는 SSRF 를 막기 위한 것). 테스트는 이 스위치를 명시적으로 켠다.
	// 「기본 거부」 동작에 대한 검증은 notify 패키지의 ssrf_test.go 가 담당한다.
	t.Setenv(notify.AllowLocalTargetsEnv, "1")
	// 백그라운드 전달 루프(3초 tick)를 끈다. 이 파일의 케이스들은 stepRealtime·
	// stepDigest 를 직접 호출해 한 번의 분배 결과(앞 K건 송달·나머지 보류)를 검증하는데,
	// 백그라운드 루프가 같은 채널을 동시에 처리하면 집계가 타이밍에 따라 흔들려(느린 CI
	// 에서 간헐 실패) 결정성이 깨진다. 이 변수는 trafficEvidenceServer 가 서버를 세우기
	// 전에 설정돼야 효과가 있다.
	t.Setenv(notifyBackgroundDisabledEnv, "1")
	s, _, request := trafficEvidenceServer(t)
	pg := s.m.pg

	// 직접 만든 task: 공유 픽스처 trafficEvidenceServer 가 만든 task 는 exploration id 를 얻을 수 없는데,
	// 취약점을 기록하려면 이 값이 반드시 필요하다.
	task, err := s.m.CreateTask("通知推送测试", "推送行为验证", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := strconv.ParseInt(task.ID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pg.Exec(`DELETE FROM tasks WHERE id=$1`, taskID) })

	var mark int64
	if err := pg.QueryRow(`SELECT COALESCE(max(id),0) FROM notification_events`).Scan(&mark); err != nil {
		t.Fatal(err)
	}
	// 픽스처가 스스로 닫힌 상태가 되도록, 픽스처 생성 이전에 이미 존재하던 이벤트를 분배 완료로 한꺼번에 표시한다.
	//
	// 이렇게 하는 이유: FanOutPendingEvents 는 **전역**이라, DB 의 미분배 이벤트를 전부
	// 일치하는 모든 채널로 펼친다. 그런데 공유 픽스처 trafficEvidenceServer 는 스스로 취약점 하나를
	// (바로 그것이 반환하는 초기 finding) 기록하고, 다른 케이스의 잔여 데이터도 있을 수 있다. 격리하지 않으면
	// 이런 잡다한 이벤트가 이 케이스의 채널로 분배되어, 「N 건 전달이어야 한다」 같은 단언이
	// 시점에 따라 맞았다 틀렸다 한다. 게다가 실패 방식이 케이스 실행 순서에 따라 달라 바로 실패하는 것보다 찾기 어렵다.
	if _, err := pg.Exec(`UPDATE notification_events SET fanned_out = true WHERE id <= $1 AND NOT fanned_out`, mark); err != nil {
		t.Fatal(err)
	}

	f := &notifyFixture{s: s, pg: pg, request: request, n: newNotifier(s), taskID: taskID, expID: task.ExpID, cleanupMark: mark}
	t.Cleanup(func() {
		if _, err := pg.Exec(`DELETE FROM notification_events WHERE id > $1`, f.cleanupMark); err != nil {
			t.Logf("清理通知事件失败: %v", err)
		}
	})
	// 전체 스위치는 켜져 있어야 한다(다른 케이스가 끈 적이 있을 수 있다).
	if err := pg.SetBool(settingNotifyEnabled, true); err != nil {
		t.Fatal(err)
	}
	return f
}

// record 는 실제 증거 기록 경로로 취약점 하나를 저장하고, finding id 를 반환한다.
// 이 경로는 **같은 트랜잭션** 안에서 알림 이벤트를 등록한다 — 이 기능이 걸리는 지점이다.
func (f *notifyFixture) record(t *testing.T, vulnclass, severity string) int64 {
	t.Helper()
	out, err := f.s.evidenceStore().Record(context.Background(), db.RecordFindingInput{
		TaskID:        f.taskID,
		ExplorationID: f.expID,
		Worker:        "test",
		VulnClass:     vulnclass,
		Name:          vulnclass,
		Severity:      severity,
		Summary:       vulnclass + " 的摘要",
		Evidence:      "poc",
	}, nil)
	if err != nil {
		t.Fatalf("记录漏洞失败: %v", err)
	}
	return out.FindingID
}

// channel 은 채널 설정을 다시 읽는다(채널별 stepX 호출에 사용).
func (f *notifyFixture) channel(t *testing.T, id int64) *db.NotificationChannel {
	t.Helper()
	ch, err := f.pg.NotificationChannelByID(context.Background(), id)
	if err != nil {
		t.Fatalf("读渠道失败: %v", err)
	}
	return ch
}

// deliver 는 이벤트를 분배한 뒤 지정 채널에 대해서만 한 라운드 전달을 실행한다.
func (f *notifyFixture) deliver(t *testing.T, chID int64, baseURL string) {
	t.Helper()
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatalf("分派失败: %v", err)
	}
	f.n.stepRealtime(ctx, f.channel(t, chID), 50, baseURL)
}

// createChannel 은 HTTP 인터페이스로 채널을 만들며, 인터페이스 자체의 검증 경로도 함께 커버한다.
func (f *notifyFixture) createChannel(t *testing.T, payload map[string]any) int64 {
	t.Helper()
	raw, _ := json.Marshal(payload)
	r := f.request("POST", "/api/notify/channels", string(raw))
	if r.Code != 200 {
		t.Fatalf("建渠道失败 %d: %s", r.Code, r.Body)
	}
	var res struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &res); err != nil || res.ID == 0 {
		t.Fatalf("建渠道返回异常: %s (%v)", r.Body, err)
	}
	t.Cleanup(func() { f.pg.Exec(`DELETE FROM notification_channels WHERE id=$1`, res.ID) })
	return res.ID
}

// fakeWebhook 은 수신한 요청 본문을 기록하는 가짜 수신 서버다.
type fakeWebhook struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []map[string]any
}

func newFakeWebhook(t *testing.T) *fakeWebhook {
	t.Helper()
	f := &fakeWebhook{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		f.mu.Lock()
		f.bodies = append(f.bodies, body)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeWebhook) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

func (f *fakeWebhook) body(t *testing.T, i int) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.bodies) {
		t.Fatalf("假接收端只收到 %d 条请求，取不到第 %d 条", len(f.bodies), i)
	}
	return f.bodies[i]
}

func (f *fakeWebhook) last(t *testing.T) map[string]any {
	t.Helper()
	if f.count() == 0 {
		t.Fatal("假接收端没有收到任何请求")
	}
	return f.body(t, f.count()-1)
}

// markdownText 는 요청 본문에서 텍스트를 꺼낸다. 플랫폼마다 다른 필드명을 흡수한다:
// DingTalk markdown 은 `text`, ActionCard 도 `text`, WeCom markdown 은 `content` 를 쓴다.
func markdownText(t *testing.T, body map[string]any) string {
	t.Helper()
	for _, key := range []string{"markdown", "actionCard"} {
		section, ok := body[key].(map[string]any)
		if !ok {
			continue
		}
		for _, field := range []string{"text", "content"} {
			if s, ok := section[field].(string); ok && s != "" {
				return s
			}
		}
	}
	t.Fatalf("请求体里没有可识别的正文: %v", body)
	return ""
}

// agePendingBatch 는 해당 채널의 대기 중 전달을 오래된 것으로 만들어 요약 배치 만료를 테스트한다.
func (f *notifyFixture) agePendingBatch(t *testing.T, chID int64) {
	t.Helper()
	if _, err := f.pg.Exec(`UPDATE notification_deliveries SET created_at = now() - interval '2 hours'
WHERE channel_id=$1 AND state=$2`, chID, db.NotifyStatePending); err != nil {
		t.Fatal(err)
	}
}

func TestNotifyEndToEndRealtimeDelivery(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "实时推送",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	f.record(t, "SQL注入", "high")
	f.deliver(t, chID, "")

	if hook.count() != 1 {
		t.Fatalf("应发出 1 条消息，实际 %d", hook.count())
	}
	text := markdownText(t, hook.last(t))
	// "SQL注入" 은 입력으로 넣은 취약점 제목이라 카드에 그대로 에코된다(사용자 데이터,
	// 번역 대상 아님). "높음"·"개요" 는 렌더 라벨이다 — 심각도는 notify.SeverityLabel("high")
	// 가 내는 "🟠 높음", 요약 머리글은 writeItem 이 붙이는 "**개요**:" 에 각각 들어 있다.
	for _, want := range []string{"SQL注入", "높음", "개요"} {
		if !strings.Contains(text, want) {
			t.Fatalf("消息正文缺少 %q:\n%s", want, text)
		}
	}
	// 전달 결과는 sent 로 전환되어야 한다.
	var pending int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1 AND state <> $2`,
		chID, db.NotifyStateSent).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("投递后仍有 %d 条未标记 sent", pending)
	}
}

func TestNotifyChannelAPIMasksSecretsAndPreservesOnUpdate(t *testing.T) {
	f := newNotifyFixture(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "掩码用例",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": "https://oapi.dingtalk.com/robot/send?access_token=abc123456", "secret": "SECabcdef123456"},
	})

	r := f.request("GET", "/api/notify/channels", "")
	if r.Code != 200 {
		t.Fatalf("列渠道失败 %d: %s", r.Code, r.Body)
	}
	if strings.Contains(r.Body.String(), "abc123456") || strings.Contains(r.Body.String(), "SECabcdef123456") {
		t.Fatalf("接口回显泄露了凭据: %s", r.Body)
	}
	var listed struct {
		Channels []struct {
			ID         int64          `json:"id"`
			Config     map[string]any `json:"config"`
			SecretKeys []string       `json:"secret_keys"`
		} `json:"channels"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	var mine *struct {
		ID         int64          `json:"id"`
		Config     map[string]any `json:"config"`
		SecretKeys []string       `json:"secret_keys"`
	}
	for i := range listed.Channels {
		if listed.Channels[i].ID == chID {
			mine = &listed.Channels[i]
		}
	}
	if mine == nil {
		t.Fatal("新建的渠道未出现在列表里")
	}
	if !notify.IsMasked(fmt.Sprint(mine.Config["webhook"])) || !notify.IsMasked(fmt.Sprint(mine.Config["secret"])) {
		t.Fatalf("凭据字段应为掩码值: %v", mine.Config)
	}
	if len(mine.SecretKeys) == 0 {
		t.Fatal("接口应告知前端哪些字段是凭据")
	}

	// PATCH 는 이름만 바꾸고 마스킹된 자격 증명을 되돌려 보낸다: 실제 자격 증명은 그대로 보존되어야 한다.
	body, _ := json.Marshal(map[string]any{
		"name":   "改名后",
		"config": map[string]any{"webhook": fmt.Sprint(mine.Config["webhook"]), "secret": fmt.Sprint(mine.Config["secret"])},
	})
	if r := f.request("PATCH", fmt.Sprintf("/api/notify/channels/%d", chID), string(body)); r.Code != 200 {
		t.Fatalf("更新失败 %d: %s", r.Code, r.Body)
	}
	cfg := f.channelConfig(t, chID)
	if cfg["webhook"] != "https://oapi.dingtalk.com/robot/send?access_token=abc123456" {
		t.Fatalf("掩码回传把真凭据覆盖了: %v", cfg["webhook"])
	}
	if cfg["secret"] != "SECabcdef123456" {
		t.Fatalf("掩码回传把 secret 覆盖了: %v", cfg["secret"])
	}
	if f.channel(t, chID).Name != "改名后" {
		t.Fatal("名字未更新")
	}

	// secret 을 명시적으로 비우는 것은 적용되어야 한다(「마스킹 값 회신 = 변경 없음」과는 구별된다).
	body, _ = json.Marshal(map[string]any{"config": map[string]any{"secret": ""}})
	if r := f.request("PATCH", fmt.Sprintf("/api/notify/channels/%d", chID), string(body)); r.Code != 200 {
		t.Fatalf("清空 secret 失败 %d: %s", r.Code, r.Body)
	}
	if _, still := f.channelConfig(t, chID)["secret"]; still {
		t.Fatal("空串应清空 secret")
	}
}

func (f *notifyFixture) channelConfig(t *testing.T, id int64) map[string]any {
	t.Helper()
	var cfg map[string]any
	if err := json.Unmarshal(f.channel(t, id).Config, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestNotifyChannelAPICreateValidation(t *testing.T) {
	f := newNotifyFixture(t)
	cases := []struct {
		name    string
		payload map[string]any
		wantSub string
	}{
		// wantSub 는 server/notify_api.go 가 내보내는 한국어 검증 오류와 일치시킨다
		// (notifyErrKindInvalidFmt·notifyErrNameMissing·notifyErrModeInvalid 의 안정 문구).
		// webhook 두 건은 notify/dingtalk.go 의 Validate 가 내는 오류인데, 그 webhook 주소
		// 검증 문구는 이미 한국어로 현지화됐으므로(notify/dingtalk_feishu_wecom_localized_test.go
		// 가 "Webhook 주소가 없습니다"·"Webhook 주소가 올바르지 않습니다" 로 검증한다) 기대
		// 문자열도 한국어 안정 문구로 맞춘다.
		{"类型非法", map[string]any{"name": "x", "kind": "nope", "config": map[string]any{}}, "채널 유형이 올바르지 않습니다"},
		{"缺名称", map[string]any{"kind": notify.KindDingTalk, "config": map[string]any{"webhook": "https://e.com/h"}}, "채널 이름을 입력하세요"},
		{"缺 webhook", map[string]any{"name": "x", "kind": notify.KindDingTalk, "config": map[string]any{}}, "Webhook 주소가 없습니다"},
		{"webhook 协议非法", map[string]any{"name": "x", "kind": notify.KindDingTalk, "config": map[string]any{"webhook": "file:///etc/passwd"}}, "Webhook 주소가 올바르지 않습니다"},
		{"模式非法", map[string]any{"name": "x", "kind": notify.KindDingTalk, "mode": "sometimes", "config": map[string]any{"webhook": "https://e.com/h"}}, "발송 모드가 올바르지 않습니다"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(tc.payload)
			r := f.request("POST", "/api/notify/channels", string(raw))
			if r.Code != 400 {
				t.Fatalf("应返回 400，得到 %d: %s", r.Code, r.Body)
			}
			if !strings.Contains(r.Body.String(), tc.wantSub) {
				t.Fatalf("错误信息应提到 %q，得到 %s", tc.wantSub, r.Body)
			}
		})
	}
	if r := f.request("DELETE", "/api/notify/channels/99999999", ""); r.Code != 404 {
		t.Fatalf("删除不存在的渠道应 404，得到 %d", r.Code)
	}
}

func TestNotifyFilterBlocksBelowThreshold(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "仅严重",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
		"filter": map[string]any{"min_severity": "critical"},
	})
	f.record(t, "低危问题", "low")
	if _, _, err := f.pg.FanOutPendingEvents(context.Background(), 500); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("低于阈值的漏洞不该产生投递，得到 %d 条", n)
	}
	f.n.stepRealtime(context.Background(), f.channel(t, chID), 50, "")
	if hook.count() != 0 {
		t.Fatal("被过滤的漏洞不应发出消息")
	}
}

func TestNotifyDigestBatchesMultipleFindingsIntoOneMessage(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "汇总推送",
		"kind":   notify.KindDingTalk,
		"mode":   db.NotifyModeDigest,
		"config": map[string]any{"webhook": hook.URL},
	})
	for i := 0; i < 3; i++ {
		f.record(t, fmt.Sprintf("汇总漏洞%d", i+1), "high")
	}
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatal(err)
	}
	ch := f.channel(t, chID)

	// 만료 전: 발송하지 않는다.
	f.n.stepDigest(ctx, ch, 50, "")
	if hook.count() != 0 {
		t.Fatal("汇总批次未到期就发了")
	}

	// 배치를 오래되게 만든 뒤: 세 건이 하나의 메시지로 합쳐진다.
	f.agePendingBatch(t, chID)
	f.n.stepDigest(ctx, ch, 50, "")
	if got := hook.count(); got != 1 {
		t.Fatalf("三条应汇总成一条消息，实际发了 %d 条", got)
	}
	text := markdownText(t, hook.last(t))
	// 시간창이 있는 다이제스트 머리말은 markdown.go 가 "**최근 N분간 신규 취약점 N건**"
	// 으로 렌더한다(digestInterval 기본 30분이라 WindowMinutes>0).
	if !strings.Contains(text, "최근") || !strings.Contains(text, "신규 취약점 3건") {
		t.Fatalf("汇总消息缺少条数/时间窗文案:\n%s", text)
	}
	for i := 1; i <= 3; i++ {
		if !strings.Contains(text, fmt.Sprintf("汇总漏洞%d", i)) {
			t.Fatalf("汇总消息缺少第 %d 条:\n%s", i, text)
		}
	}
	// 같은 배치는 batch_id 를 공유해야 한다.
	var distinct, total int
	if err := f.pg.QueryRow(`SELECT count(DISTINCT batch_id), count(*) FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&distinct, &total); err != nil {
		t.Fatal(err)
	}
	if total != 3 || distinct != 1 {
		t.Fatalf("三条投递应共享一个 batch_id，得到 distinct=%d total=%d", distinct, total)
	}
}

func TestNotifyDisabledChannelDoesNotSend(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":    "停用渠道",
		"kind":    notify.KindDingTalk,
		"enabled": false,
		"config":  map[string]any{"webhook": hook.URL},
	})
	f.record(t, "停用期间的漏洞", "critical")
	if _, _, err := f.pg.FanOutPendingEvents(context.Background(), 500); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("停用渠道不该产生投递，得到 %d 条", n)
	}
}

func TestNotifyStatusChangeDelivery(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "状态变更订阅",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
		"filter": map[string]any{"on_status_change": true},
	})
	finding := f.record(t, "状态变更用例", "high")
	r := f.request("PATCH", fmt.Sprintf("/api/exploration/findings/%d", finding), `{"status":"fixed"}`)
	if r.Code != 200 {
		t.Fatalf("改状态失败 %d: %s", r.Code, r.Body)
	}
	f.deliver(t, chID, "")

	// 두 건이어야 한다: fixed 건은 상태 변경이고, finding_created 건도 같은 라운드에서 발송될 수 있다.
	// 상태 변경 건은 실제로는 더 나중에 생성되지만, 순서에 의존하지 않도록 전부 검색한다.
	found := false
	for i := 0; i < hook.count(); i++ {
		text := markdownText(t, hook.body(t, i))
		// 상태 변경 카드는 markdown.go 가 "**상태 변경**: %s → %s" 로, 상태값은
		// notify.StatusLabel 이 렌더한다(fixed → "수정됨").
		if strings.Contains(text, "상태 변경") && strings.Contains(text, "수정됨") {
			found = true
		}
	}
	if !found {
		t.Fatalf("没有收到含「状态变更 → 已修复」的消息（共 %d 条）", hook.count())
	}
}

func TestNotifyStatusChangeSuppressedByDefault(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "不订阅状态变更",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	finding := f.record(t, "不订阅变更", "high")
	if r := f.request("PATCH", fmt.Sprintf("/api/exploration/findings/%d", finding), `{"status":"false_positive"}`); r.Code != 200 {
		t.Fatalf("改状态失败 %d: %s", r.Code, r.Body)
	}
	if _, _, err := f.pg.FanOutPendingEvents(context.Background(), 500); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries d
JOIN notification_events e ON e.id = d.event_id
WHERE d.channel_id=$1 AND e.kind=$2`, chID, notify.EventFindingStatusChanged).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("未订阅状态变更的渠道不该收到状态变更投递，得到 %d 条", n)
	}
}

func TestNotifyTestMessageEndpoint(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "测试发送",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	if r := f.request("POST", fmt.Sprintf("/api/notify/channels/%d/test", chID), ""); r.Code != 200 {
		t.Fatalf("测试发送失败 %d: %s", r.Code, r.Body)
	}
	if hook.count() != 1 {
		t.Fatalf("假接收端应收到 1 条测试消息，得到 %d", hook.count())
	}
	// 테스트 메시지는 한눈에 테스트임을 알 수 있어야 하며, 실제 취약점으로 오인되면 안 된다.
	// 테스트 메시지 제목은 notify_api.go 의 notifyTestName("테스트 메시지 · 채널 설정 정상").
	if text := markdownText(t, hook.last(t)); !strings.Contains(text, "테스트") {
		t.Fatalf("测试消息应标明是测试: %s", text)
	}
	// 설정이 잘못되었을 때는 채널의 원래 오류를 그대로 사용자에게 돌려줘야 한다.
	badID := f.createChannel(t, map[string]any{
		"name":   "坏地址",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": "http://127.0.0.1:1/hook"},
	})
	if r := f.request("POST", fmt.Sprintf("/api/notify/channels/%d/test", badID), ""); r.Code != 502 {
		t.Fatalf("投递失败应回 502，得到 %d: %s", r.Code, r.Body)
	}
}

func TestNotifyDeliveriesHistoryAndRetry(t *testing.T) {
	f := newNotifyFixture(t)
	// 반드시 실패하는 주소를 가리켜 failed 전달을 만든다.
	chID := f.createChannel(t, map[string]any{
		"name":   "失败重试",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": "http://127.0.0.1:1/hook"},
	})
	f.record(t, "会失败的推送", "high")
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatal(err)
	}
	ch := f.channel(t, chID)
	// 재시도 예산이 소진될 때까지 연속으로 전달한다.
	for i := 0; i < db.MaxNotifyAttempts; i++ {
		f.n.stepRealtime(ctx, ch, 50, "")
		if _, err := f.pg.Exec(`UPDATE notification_deliveries SET next_attempt_at = now() - interval '1 minute' WHERE channel_id=$1`, chID); err != nil {
			t.Fatal(err)
		}
	}
	var state string
	if err := f.pg.QueryRow(`SELECT state FROM notification_deliveries WHERE channel_id=$1`, chID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != db.NotifyStateFailed {
		t.Fatalf("重试耗尽后应为 failed，得到 %s", state)
	}

	r := f.request("GET", fmt.Sprintf("/api/notify/deliveries?channel_id=%d&state=failed", chID), "")
	if r.Code != 200 {
		t.Fatalf("查历史失败 %d: %s", r.Code, r.Body)
	}
	var hist struct {
		Deliveries []struct {
			ID        int64  `json:"id"`
			State     string `json:"state"`
			LastError string `json:"last_error"`
			Attempts  int    `json:"attempts"`
			Title     string `json:"title"`
		} `json:"deliveries"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &hist); err != nil {
		t.Fatal(err)
	}
	if hist.Total != 1 || len(hist.Deliveries) != 1 {
		t.Fatalf("应查到 1 条失败投递，得到 total=%d len=%d", hist.Total, len(hist.Deliveries))
	}
	if hist.Deliveries[0].LastError == "" {
		t.Fatal("历史里应带上失败原因，否则用户无法排查")
	}
	if hist.Deliveries[0].Attempts < db.MaxNotifyAttempts {
		t.Fatalf("尝试次数应被记录，得到 %d", hist.Deliveries[0].Attempts)
	}
	if hist.Deliveries[0].Title != "会失败的推送" {
		t.Fatalf("历史应带出漏洞标题，得到 %q", hist.Deliveries[0].Title)
	}

	// 수동 재발송: pending 으로 돌아가고 카운트가 0 으로 초기화되어야 한다.
	if r := f.request("POST", fmt.Sprintf("/api/notify/deliveries/%d/retry", hist.Deliveries[0].ID), ""); r.Code != 200 {
		t.Fatalf("重发失败 %d: %s", r.Code, r.Body)
	}
	var attempts int
	if err := f.pg.QueryRow(`SELECT state, attempts FROM notification_deliveries WHERE id=$1`, hist.Deliveries[0].ID).Scan(&state, &attempts); err != nil {
		t.Fatal(err)
	}
	if state != db.NotifyStatePending || attempts != 0 {
		t.Fatalf("重发后应为 pending 且 attempts=0，得到 %s/%d", state, attempts)
	}
}

func TestNotifyMetaAndSettingsRoundTrip(t *testing.T) {
	f := newNotifyFixture(t)
	r := f.request("GET", "/api/notify/meta", "")
	if r.Code != 200 {
		t.Fatalf("meta 失败: %s", r.Body)
	}
	var meta struct {
		Kinds []struct {
			Kind       string   `json:"kind"`
			SecretKeys []string `json:"secret_keys"`
		} `json:"kinds"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &meta); err != nil {
		t.Fatal(err)
	}
	if len(meta.Kinds) != len(notify.Kinds()) {
		t.Fatalf("meta 应列出全部 %d 个渠道，得到 %d", len(notify.Kinds()), len(meta.Kinds))
	}
	for _, k := range meta.Kinds {
		if len(k.SecretKeys) == 0 {
			t.Errorf("渠道 %s 未上报凭据字段", k.Kind)
		}
	}

	// 설정 3 개 항목의 왕복 검증. 끝의 슬래시는 정규화되어야 한다. 그렇지 않으면 링크가 "//function/..." 로 조립된다.
	if r := f.request("PUT", "/api/settings", `{"notify_public_base_url":"https://artex.example.com/","notify_digest_interval_min":15,"notify_enabled":true}`); r.Code != 200 {
		t.Fatalf("写设置失败 %d: %s", r.Code, r.Body)
	}
	t.Cleanup(func() {
		f.pg.Exec(`DELETE FROM settings WHERE key IN ($1,$2)`, settingNotifyPublicBaseURL, settingNotifyDigestMinutes)
	})
	payload := f.s.settingsPayload()
	if payload["notify_public_base_url"] != "https://artex.example.com" {
		t.Fatalf("回链地址未规范化: %v", payload["notify_public_base_url"])
	}
	if payload["notify_digest_interval_min"] != 15 {
		t.Fatalf("汇总周期未生效: %v", payload["notify_digest_interval_min"])
	}

	// 잘못된 값은 거부되어야 한다.
	for _, body := range []string{
		`{"notify_public_base_url":"ftp://x"}`,
		`{"notify_digest_interval_min":0}`,
		`{"notify_digest_interval_min":99999}`,
	} {
		if r := f.request("PUT", "/api/settings", body); r.Code != 400 {
			t.Errorf("%s 应返回 400，得到 %d", body, r.Code)
		}
	}
}

// TestNotifyDeepLinkUsesPublicBaseURL 은 링크 조립을 검증한다: public_base_url 이 설정되면
// 단일 메시지는 버튼이 있는 ActionCard 를 써야 하며, 링크는 취약점 상세 페이지를 가리켜야 한다.
func TestNotifyDeepLinkUsesPublicBaseURL(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "回链",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	finding := f.record(t, "带回链的漏洞", "high")
	f.deliver(t, chID, "https://artex.example.com")

	body := hook.last(t)
	card, _ := body["actionCard"].(map[string]any)
	if card == nil {
		t.Fatalf("有回链时应用 ActionCard，得到 msgtype=%v", body["msgtype"])
	}
	want := fmt.Sprintf("https://artex.example.com/function/findings/detail?id=%d", finding)
	if card["singleURL"] != want {
		t.Fatalf("回链不对\n期望 %s\n得到 %v", want, card["singleURL"])
	}
}

// TestNotifyNoDeepLinkWithoutBaseURL 은 반대 방향 검증이다: 외부 주소가 없을 때 잘못된 링크
// (예: localhost 나 상대 경로)가 만들어지면 안 되며, 순수 markdown 으로 폴백해야 한다.
func TestNotifyNoDeepLinkWithoutBaseURL(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	chID := f.createChannel(t, map[string]any{
		"name":   "无回链",
		"kind":   notify.KindDingTalk,
		"config": map[string]any{"webhook": hook.URL},
	})
	f.record(t, "无回链的漏洞", "high")
	f.deliver(t, chID, "")

	body := hook.last(t)
	if body["msgtype"] != "markdown" {
		t.Fatalf("未配外部地址时应发 markdown，得到 %v", body["msgtype"])
	}
	// 상세 링크 머리글은 markdown.go 가 "[상세 보기](URL)" 로 렌더한다 — 외부 주소가
	// 없으면 이 링크가 아예 나오지 않아야 한다(옛 중국어 "查看详情" 를 검사하면 라벨이
	// 한국어로 바뀐 지금은 항상 통과해 회귀를 못 잡는다).
	if text := markdownText(t, body); strings.Contains(text, "상세 보기") {
		t.Fatalf("未配外部地址时不该出现详情链接:\n%s", text)
	}
}

// TestNotifyDigestSegmentsAndDefersRemainder 는 「조용한 손실」 수정의 end-to-end 증거다.
//
// 요약 메시지는 채널의 길이 상한(WeCom 4096 바이트)을 받는다. 한 배치가 다 들어가지 않으면 **항목 단위로** 잘라야 한다.
// 이 메시지에 들어간 항목은 전달 완료로 표시하고, 나머지는 다음 메시지를 위해 큐에 되돌린다. 과거 구현은 배치 전체를
// 성공으로 표시했다 — 잘린 항목은 메시지에도 실패 목록에도 없었는데, 전달 이력은 성공으로 보여서
// 취약점이 그대로 사라졌다.
//
// 단언 네 가지: ① 실제로 들어간 건수만 표시 ② 나머지는 여전히 대기 ③ 연기된 항목은
// **재시도 횟수를 소비하지 않음** ④ 한 번 더 돌리면 나머지도 발송되어야 함(교착되지 않음).
func TestNotifyDigestSegmentsAndDefersRemainder(t *testing.T) {
	f := newNotifyFixture(t)
	hook := newFakeWebhook(t)
	// 기업 WeChat 을 사용한다: markdown 상한이 4096 바이트로, 여섯 개 채널 중 가장 엄격하다.
	chID := f.createChannel(t, map[string]any{
		"name":   "分段汇总",
		"kind":   notify.KindWeCom,
		"mode":   db.NotifyModeDigest,
		"config": map[string]any{"webhook": hook.URL},
	})
	const total = 60
	// 제목을 길게 잡아 60 건이 4096 바이트를 확실히 넘어서, 반드시 분할되게 한다.
	longName := strings.Repeat("超长漏洞名称", 6)
	for i := 0; i < total; i++ {
		f.record(t, longName+strconv.Itoa(i+1), "high")
	}
	ctx := context.Background()
	if _, _, err := f.pg.FanOutPendingEvents(ctx, 500); err != nil {
		t.Fatal(err)
	}
	f.agePendingBatch(t, chID)
	ch := f.channel(t, chID)

	f.n.stepDigest(ctx, ch, 50, "")
	if hook.count() != 1 {
		t.Fatalf("应只发出一条消息，得到 %d", hook.count())
	}

	var sent, pending int
	if err := f.pg.QueryRow(`SELECT
    count(*) FILTER (WHERE state=$2),
    count(*) FILTER (WHERE state=$3)
  FROM notification_deliveries WHERE channel_id=$1`, chID, db.NotifyStateSent, db.NotifyStatePending).
		Scan(&sent, &pending); err != nil {
		t.Fatal(err)
	}
	if sent == 0 {
		t.Fatal("应有条目被标记为已送达")
	}
	if pending == 0 {
		t.Fatalf("一批 %d 条不可能全装进 4096 字节，应有剩余待发；sent=%d", total, sent)
	}
	if sent+pending != total {
		t.Fatalf("条目数对不上：sent=%d pending=%d total=%d（既没送达也没待发=丢失）", sent, pending, total)
	}
	// 메시지 본문에는 이 메시지에 포함되지 않은 항목이 몇 건 남았는지 사실대로 알려야 한다.
	// 분절 안내는 markdown.go 가 "(이 메시지에는 앞 N건만 … 나머지 N건은 다음 메시지에서 …)" 로 렌더한다.
	if text := markdownText(t, hook.last(t)); !strings.Contains(text, "나머지") {
		t.Fatalf("消息应说明还有条目未包含在本条:\n%.400s", text)
	}

	// 연기된 항목은 재시도 예산을 소비하면 안 된다: 가져올 때 attempts 를 낙관적으로 +1 했으므로 연기 시 되돌려야 한다.
	var maxAttempts int
	if err := f.pg.QueryRow(`SELECT COALESCE(max(attempts),0) FROM notification_deliveries
WHERE channel_id=$1 AND state=$2`, chID, db.NotifyStatePending).Scan(&maxAttempts); err != nil {
		t.Fatal(err)
	}
	if maxAttempts > 0 {
		t.Fatalf("被推迟的条目不该消耗重试次数（否则几条之后就会被判失败），得到 attempts=%d", maxAttempts)
	}

	// 수렴할 때까지 반복 실행한다. 단언하는 것은 **최종적으로 모두 전달되며** 중간에 실제로 여러 라운드로 나뉘었다는 점이다.
	// 이는 「두 번째 라운드에 전부 발송됨」보다 강한 검증이다: 분할이 교착되지 않고 남은 항목을 잃지 않음을 증명한다.
	rounds := 0
	for {
		var undelivered int
		if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries
WHERE channel_id=$1 AND state <> $2 AND state <> $3`, chID, db.NotifyStateSent, db.NotifyStateFailed).
			Scan(&undelivered); err != nil {
			t.Fatal(err)
		}
		if undelivered == 0 {
			break
		}
		rounds++
		if rounds > total+5 {
			t.Fatalf("分段投递不收敛：跑了 %d 轮仍有 %d 条悬而未决", rounds, undelivered)
		}
		before := hook.count()
		f.n.stepDigest(ctx, ch, 50, "")
		if hook.count() == before {
			t.Fatalf("第 %d 轮没有任何进展，剩余 %d 条会永久卡住", rounds, undelivered)
		}
	}
	if rounds < 2 {
		t.Fatalf("一条 4096 字节的消息装不下 %d 条长标题漏洞，应分多轮发出，实际只用了 %d 轮", total, rounds)
	}
	// 첫 라운드 이후의 모든 라운드는 **순수 이어 보내기**여야 하며, 채널이 거부한 항목은 있으면 안 된다.
	var failed int
	if err := f.pg.QueryRow(`SELECT count(*) FROM notification_deliveries WHERE channel_id=$1 AND state=$2`,
		chID, db.NotifyStateFailed).Scan(&failed); err != nil {
		t.Fatal(err)
	}
	if failed != 0 {
		t.Fatalf("假接收端始终返回成功，不该有失败条目，得到 %d", failed)
	}
}

// TestNotifyBackoffTableMatchesAttemptBudget 은 드리프트 방지 단언이다.
//
// 재시도 예산(db.MaxNotifyAttempts)과 백오프 시퀀스 표(notifyBackoff)는 서로 다른 두 패키지에 있다:
// 전자는 상태 머신의 정책, 후자는 엔진의 실행 간격이다. 둘 중 하나만 바꾸면 — 예컨대 예산을 5 회로 올리고
// 백오프 단계 추가를 잊으면 — 코드는 에러 없이 돌아가지만 4·5 번째 재시도가 마지막 단계의 간격을 그대로 쓰게 된다.
// 그 결과 「재시도 간격이 이유 없이 느려진다」로 나타나서, 원인이 여기라고 떠올리기 어렵다.
// 두 길이가 일치하는지 단언해, 이런 드리프트가 CI 에서 바로 드러나게 한다.
func TestNotifyBackoffTableMatchesAttemptBudget(t *testing.T) {
	if len(notifyBackoff) != db.MaxNotifyAttempts {
		t.Fatalf("退避档位数(%d)与最大尝试次数(%d)不一致——改一个必须同时改另一个",
			len(notifyBackoff), db.MaxNotifyAttempts)
	}
	// 백오프 간격은 단조 비감소여야 한다. 그렇지 않으면 재시도가 점점 빨라져 오히려 레이트 리밋을 가중시킨다.
	for i := 1; i < len(notifyBackoff); i++ {
		if notifyBackoff[i] < notifyBackoff[i-1] {
			t.Fatalf("退避间隔必须单调不减：第 %d 档 %v < 第 %d 档 %v",
				i, notifyBackoff[i], i-1, notifyBackoff[i-1])
		}
	}
}

// TestNotifyRateLimitDoesNotConsumeRetryBudget 은 「토큰을 먼저 얻고 나서 가져온다」는 순서를 고정한다.
// 순서가 뒤바뀌면(먼저 가져오고 나중에 버리면), 레이트 리밋에 막힌 전달이 이미 attempts 를 한 번 소비해
// 예산이 단순 대기로 소진되고 결국 failed 로 떨어진다.
func TestNotifyRateLimitDoesNotConsumeRetryBudget(t *testing.T) {
	// 토큰 버킷 자체만 테스트한다. Server 는 필요 없으며, 굳이 만들 필요도 없다.
	n := &Notifier{buckets: map[int64]*notifyBucket{}}
	now := time.Now()
	// 분당 1 건: 가득 찬 버킷에서는 최대 1 건이다.
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now); got != 1 {
		t.Fatalf("满桶时每分钟 1 条应取 1 个令牌，得到 %d", got)
	}
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(time.Millisecond)); got != 0 {
		t.Fatalf("令牌耗尽后应立即返回 0，得到 %d", got)
	}
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(30*time.Second)); got != 0 {
		t.Fatalf("半程不应补满一个令牌，得到 %d", got)
	}
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(time.Minute)); got != 1 {
		t.Fatalf("满一个周期应补回 1 个令牌，得到 %d", got)
	}
	// 레이트 리밋이 없는 채널은 유한한 상한을 쓰게 하여, 한 라운드가 쌓인 적체에 끌려다니지 않게 한다.
	if got := n.takeTokens(2, 0, notifyUnlimitedBurstPerTick+10, now); got != notifyUnlimitedBurstPerTick {
		t.Fatalf("不限流应返回每轮上限 %d，得到 %d", notifyUnlimitedBurstPerTick, got)
	}
	// 채널 간 토큰 버킷은 서로 독립이다.
	if got := n.takeTokens(1, 1, notifyMaxSendsPerChannelPerTick, now.Add(time.Millisecond)); got != 0 {
		t.Fatalf("渠道 1 的桶应仍然为空，得到 %d", got)
	}
}

// TestNotifyTakeTokensKeepsUnusedTokens 는 「want 개만 가져온다」는 의미를 고정한다.
//
// 과거 구현은 버킷을 완전히 비운 뒤 호출자가 잘라내는 방식이었다. 그래서 rate=100/min 채널이 버킷을 가득 채우고
// 한 라운드에 5 건만 쓰면 나머지 95 개 토큰이 그대로 버려졌다. 채널에 이번 라운드 대기 전달이 없을 때도 똑같이 차감됐다.
// 결과적으로 주석이 주장하던 「적체 시 rate_per_min 건을 한 번에 밀어낼 수 있다」는 어떤 경우에도 불가능했다.
func TestNotifyTakeTokensKeepsUnusedTokens(t *testing.T) {
	n := &Notifier{buckets: map[int64]*notifyBucket{}}
	now := time.Now()
	// 버킷은 처음에 가득 차 있고(100), 이번 라운드에는 5 개만 필요하다.
	if got := n.takeTokens(1, 100, 5, now); got != 5 {
		t.Fatalf("want=5 时应恰好取 5 个令牌，得到 %d", got)
	}
	// 핵심 단언: 남은 95 개는 버킷에 그대로 있어야 하며, 버려져서는 안 된다.
	// 시간을 진행시키지 않아, 가져온 토큰이 보충분이 아니라 기존 재고에서 나왔음을 보장한다.
	if got := n.takeTokens(1, 100, 95, now); got != 95 {
		t.Fatalf("剩余令牌应仍可取用（期望 95），得到 %d——桶被整轮抽空了", got)
	}
	if got := n.takeTokens(1, 100, 1, now); got != 0 {
		t.Fatalf("桶已取尽，应返回 0，得到 %d", got)
	}
	// want<=0 이면 토큰을 전혀 차감하지 않는다(빈 라운드는 과금하지 않는다).
	n2 := &Notifier{buckets: map[int64]*notifyBucket{}}
	if got := n2.takeTokens(1, 20, 0, now); got != 0 {
		t.Fatalf("want=0 应返回 0，得到 %d", got)
	}
	if got := n2.takeTokens(1, 20, 20, now); got != 20 {
		t.Fatalf("want=0 的那次不该消耗令牌，应仍可取满 20，得到 %d", got)
	}
}

// TestDigestTickPlanDecouplesBatchSizeFromSendBudget 은 요약 모드의 두 가지 차원을 고정한다.
//
// 요약 배치 크기가 라운드당 요청 예산에 묶이면, rate_per_min=20 채널은 매 3 초 tick 마다
// 토큰을 1 개만 얻게 되어 요약 메시지마다 취약점이 1 건씩만 들어간다 — 기능상 요약이 없는 것과 같다.
// 메시지 머리말에는 「최근 30 분간 신규 취약점 1 건」이라고 적히는데도 이 퇴화는 오류를 내지 않는다.
// 기존 end-to-end 케이스도 이를 잡아내지 못한다(stepDigest 에 충분히 큰 limit 을 직접 넘겨
// step 안의 할당량 계산을 우회하기 때문이다). 그래서 여기서는 결정 자체를 직접 단언한다.
func TestDigestTickPlanDecouplesBatchSizeFromSendBudget(t *testing.T) {
	tokens, claimLimit := digestTickPlan()
	// 한 배치 = 한 메시지 = 한 요청 = 한 토큰. 토큰의 단위는 메시지이지 취약점이 아니다.
	if tokens != 1 {
		t.Fatalf("汇总一批只发一条消息，应恰好消耗 1 个令牌，得到 %d", tokens)
	}
	if claimLimit != db.MaxDigestBatchSize {
		t.Fatalf("汇总批次大小应为内存上界 db.MaxDigestBatchSize=%d，得到 %d",
			db.MaxDigestBatchSize, claimLimit)
	}
	// 핵심 관계: 배치 크기는 라운드당 요청 예산보다 훨씬 커야 한다. 둘이 같은 자릿수면
	// 「메시지를 몇 건 보내는가」와 「한 배치에 취약점을 몇 건 담는가」를 하나의 수로 섞은 것이다.
	if claimLimit <= notifyMaxSendsPerChannelPerTick {
		t.Fatalf("汇总批次大小 %d 不应受每轮请求预算 %d 约束——"+
			"请求预算是由租约倒推的「发几次请求」，与「一批装几条漏洞」是两个量纲",
			claimLimit, notifyMaxSendsPerChannelPerTick)
	}
}

// TestNotifyTickBudgetFitsWithinLease 는 또 하나의 드리프트 방지 단언이다.
//
// 단일 채널의 라운드당 전달 건수 상한(notifyMaxSendsPerChannelPerTick)은 리스 시간으로부터 역산된 값이다:
// 한 라운드에서 직렬로 전달하는 최악의 소요 시간은 리스보다 작아야 한다. 그렇지 않으면 뒤쪽 몇 건을 다 보내기 전에 리스가 만료되어
// 다중 인스턴스 배포에서는 상대가 이 항목을 다시 가져가 중복 발송된다. 이 세 상수는 서로 다른 위치에 있어서
// 어느 하나를 바꿔도 관계가 깨질 수 있으며 아무 오류도 나지 않는다 — 그래서 여기서 못 박아 둔다.
func TestNotifyTickBudgetFitsWithinLease(t *testing.T) {
	worst := time.Duration(notifyMaxSendsPerChannelPerTick) * notifySendTimeout
	if worst >= notifyLease {
		t.Fatalf("单渠道一轮的最坏耗时 %v 不应达到或超过租约 %v"+
			"（notifyMaxSendsPerChannelPerTick=%d × notifySendTimeout=%v）——"+
			"改这三个常量中的任意一个都要同步检查另外两个",
			worst, notifyLease, notifyMaxSendsPerChannelPerTick, notifySendTimeout)
	}
}
