package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/notify"
)

// 전역 설정 키(settings 키-값 테이블에 저장하므로 별도 테이블은 필요 없다).
const (
	// settingNotifyEnabled 는 푸시 전체 스위치다. 기본값은 켜짐이며, 점검 기간에 한 번에 차단하기 위한 용도다.
	// 기능의 활성화 조건이 아니다. 실제 활성화 조건은 「채널이 설정되어 있는지」다.
	settingNotifyEnabled = "notify_enabled"
	// settingNotifyPublicBaseURL 은 취약점 상세 페이지 링크를 생성할 때 쓰는 외부 접근 주소다
	// (예: https://artex.example.com). 비워 두면 메시지에 링크 버튼을 붙이지 않는다.
	// 프로젝트에 재사용할 수 있는 외부 주소 설정이 없어서 여기에 새 항목을 추가했다.
	settingNotifyPublicBaseURL = "notify_public_base_url"
	// settingNotifyDigestMinutes 는 요약 모드의 주기(분)다.
	settingNotifyDigestMinutes = "notify_digest_interval_min"
)

const (
	// notifyTick 은 전달 엔진의 폴링 간격이다. 3초는 이 엔진 실시간성의 상한이며,
	// 「취약점 DB 저장」부터 「메시지가 IM 에 도착」까지의 주된 지연 원인이기도 하다.
	notifyTick = 3 * time.Second
	// notifyBackgroundDisabledEnv 가 설정되면 서버가 백그라운드 전달 루프
	// (newNotifier(s).Run)를 띄우지 않는다. 통합 테스트는 stepRealtime·stepDigest 를
	// 직접 호출해 분배 결과를 결정론적으로 검증하는데, 3초 주기의 백그라운드 루프가
	// 같은 채널을 동시에 처리하면 집계가 타이밍에 따라 흔들린다. 이를 막기 위한
	// 테스트 전용 스위치이며, 변수가 없으면 평소대로 기동한다(프로덕션 동작 불변).
	notifyBackgroundDisabledEnv = "ARTEX_NOTIFY_BACKGROUND_DISABLED"
	// notifyLease 는 전달을 수령할 때 부여하는 임대(lease) 시간이다. 단일 전달의 최악 소요 시간보다
	// 충분히 커야 한다(notify 패키지 HTTP 클라이언트 타임아웃 15초). 그렇지 않으면 같은 행이 두 개의
	// dispatcher 에 의해 동시에 전달될 수 있다.
	notifyLease = 3 * time.Minute
	// notifyFanOutPerTick 은 한 라운드에서 분배하는 이벤트 수를 제한한다. 채널을 처음 활성화할 때
	// 과거에 쌓인 적체를 한꺼번에 전달 태스크로 펼치는 것을 막기 위함이다.
	notifyFanOutPerTick = 200
	// notifyDefaultDigestMinutes 는 요약 주기의 기본값이다.
	notifyDefaultDigestMinutes = 30
	// notifyUnlimitedBurstPerTick 은 채널에 속도 제한이 없을 때의 라운드당 전달 상한이다.
	// 존재 이유는 「채널을 무제한으로 설정했는데 한 번에 수천 건의 취약점이 스캔되는」 경우에
	// 단일 라운드 루프가 장시간 블로킹되는 것을 막기 위함이다.
	notifyUnlimitedBurstPerTick = 50
	// notifyMaxSendsPerChannelPerTick 은 단일 채널이 라운드당 최대 몇 건을 전달할지를 정한다.
	//
	// 이 상한은 **임대 시간**에서 역산한 값이다. 수령 시 행에 부여하는 임대(notifyLease = 3분)에 비해
	// 한 라운드에서 순차 전달하는 건수가 많아 최악 소요 시간이 임대를 넘으면, 뒤쪽 몇 건은 전송이
	// 끝나기 전에 임대가 만료된다. 단일 프로세스 안에서는 문제가 없다(Run 은 단일 goroutine 에서
	// 순차 실행되므로 tick 이 재진입하지 않는다). 그러나 **같은 DB 를 쓰는 프로세스가 둘 이상**이면
	// 상대 측이 임대가 만료된 행을 다시 수령해 중복 전송하고, attempts 도 두 번 증가시켜
	// 원래 프로세스가 아직 전달 중인데도 실패로 판정할 수 있다.
	//
	// 값 산정: 3분 임대 / 30초 단일 타임아웃 = 6 은 **임대를 정확히 다 쓰는** 값이라 여유가 없어
	// 쓸 수 없다. 5 를 택하면 최악 150초에 30초의 여유가 남는다. 이 관계는
	// TestNotifyTickBudgetFitsWithinLease 로 고정되어 있다. notifyLease, notifySendTimeout,
	// 이 값 중 무엇이든 바꾸면 해당 단언이 실패한다.
	notifyMaxSendsPerChannelPerTick = 5
	// notifySendTimeout 은 단일 전달의 타임아웃이다. 앞의 상수 값도 이 값에 따라 정해지며,
	// 두 값을 곱한 결과가 notifyLease 를 넘으면 안 된다. TestNotifyTickBudgetFitsWithinLease 참고.
	notifySendTimeout = 30 * time.Second
)

// notifyBackoff 는 실패 재시도의 백오프 시퀀스이며, 인덱스는 이미 시도한 횟수다.
// 3번의 기회(첫 시도 포함)는 db.MaxNotifyAttempts 와 대응하므로 두 값은 함께 바꿔야 한다.
var notifyBackoff = []time.Duration{
	time.Second,
	5 * time.Second,
	30 * time.Second,
}

// 전달 실패·지연 사유. notification_deliveries.last_error 에 저장되어
// notify_api.go 의 전달 이력 응답을 거쳐 알림 페이지 「전달 기록」 표(delivery-list.tsx)에
// 그대로 표시된다. 따라서 사용자 노출 문구라 한국어로 둔다(진단 로그 log.Printf 는 원문 보존, Z2).
const (
	errDeliveryChannelKindUnregistered = "등록되지 않은 채널 유형입니다: %q"
	errDeliverySnapshotUnrenderable    = "이벤트 스냅샷을 해석하지 못해 이 취약점을 메시지로 만들지 못했습니다"
	errDeliveryChannelLengthCapped     = "이 메시지가 채널 길이 상한에 도달해 앞의 %d건만 전송했고, 나머지는 다음 배치로 미룹니다"
	errDeliveryNoDeliveredCount        = "채널이 전송 건수를 보고하지 않았습니다(delivered=%d)"
	errDeliveryRetryExhausted          = "%d회 재시도 후에도 실패했습니다: %s"
	errDeliverySnapshotEmpty           = "전달 항목 #%d 의 이벤트 스냅샷이 비어 있습니다"
	errDeliverySnapshotParse           = "전달 항목 #%d 의 이벤트 스냅샷을 해석하지 못했습니다: %w"
	errDeliveryBatchAllUnparseable     = "요약 배치의 전달 %d건을 모두 해석하지 못했습니다"
)

// Notifier 는 취약점 푸시의 전달 엔진이다.
//
// Scheduler 와 나란히 독립된 goroutine 으로 실행된다(server.New 참고). 의도적으로
// Scheduler 의 tick 을 재사용하지 않는다. 푸시의 실시간성 요구(3초)는 트리거의 업무 주기와 달라,
// 두 기능의 실패도 서로 무관해야 한다. 푸시가 멈춘다고 agent 트리거에 영향을 주면 안 된다.
type Notifier struct {
	s  *Server
	pg *db.DB

	// mu 는 buckets 를 보호한다. 채널 수가 적고 경합이 낮아 뮤텍스 하나면 충분하므로,
	// 이를 위해 더 세밀한 구조를 도입할 가치는 없다.
	mu      sync.Mutex
	buckets map[int64]*notifyBucket
}

// notifyBucket 은 단일 채널의 토큰 버킷이다.
//
// 「분당 카운트 후 초기화」하는 슬라이딩 윈도우 대신 토큰 버킷을 쓰는 이유는, 후자의 경계 효과가 나쁘기 때문이다.
// 윈도우 끝에서 20건을 보내고 바로 다음 순간 20건을 더 보내면, 플랫폼 입장에서는 1초 안에 40건이 되어
// 제한에 걸린다. 토큰 버킷은 일정한 속도로 보충되므로 이런 순간적인 폭주를 자연스럽게 막는다.
type notifyBucket struct {
	tokens   float64
	lastFill time.Time
}

func newNotifier(s *Server) *Notifier {
	return &Notifier{s: s, pg: s.m.pg, buckets: map[int64]*notifyBucket{}}
}

// Run 은 ctx 가 끝날 때까지 순환한다. server.New 에서 한 번 시작된다.
func (n *Notifier) Run(ctx context.Context) {
	if n.pg == nil {
		return
	}
	t := time.NewTicker(notifyTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n.step(ctx)
		}
	}
}

// step 은 한 라운드를 실행한다. 먼저 새 이벤트를 분배하고, 그다음 기한이 된 태스크를 전달한다.
//
// 어느 단계가 실패하더라도 로그만 남기고 루프는 중단하지 않는다. 알림 시스템의 장애가 프로세스 수준의 문제로 번지면 안 된다.
// 각 tick 은 독립적이므로 다음 라운드에서 자연스럽게 재시도된다.
func (n *Notifier) step(ctx context.Context) {
	if !n.enabled() {
		return
	}
	if _, _, err := n.pg.FanOutPendingEvents(ctx, notifyFanOutPerTick); err != nil {
		log.Printf("[notify] 이벤트 분배 실패: %v", err)
		return
	}
	channels, err := n.pg.ListNotificationChannels(ctx)
	if err != nil {
		log.Printf("[notify] 채널 읽기 실패: %v", err)
		return
	}
	baseURL := n.publicBaseURL()
	for _, ch := range channels {
		if !ch.IsEnabled() {
			continue
		}
		// 토큰 버킷의 계량 단위는 **메시지 건수**(HTTP 요청 수와 동일)이며, 취약점 건수가 아니다.
		// 실시간 모드에서는 둘이 같다(취약점 1건 = 메시지 1건). 요약 모드에서는 한 배치의 취약점들이
		// 메시지 하나로 합쳐지므로 토큰은 하나만 소비한다.
		//
		// 두 모드 모두 먼저 토큰 버킷에 묻고, 그다음 할당량만큼 수령한다. 순서를 바꾸면 안 된다.
		// 그렇지 않으면 제한에 막혀 전달되지 못한 건이 이미 재시도 횟수를 소모하게 된다.
		now := time.Now()
		if ch.Mode == db.NotifyModeDigest {
			tokens, claimLimit := digestTickPlan()
			if n.takeTokens(ch.ID, ch.RatePerMin, tokens, now) <= 0 {
				continue
			}
			n.stepDigest(ctx, ch, claimLimit, baseURL)
			continue
		}
		allow := n.takeTokens(ch.ID, ch.RatePerMin, notifyMaxSendsPerChannelPerTick, now)
		if allow <= 0 {
			continue
		}
		n.stepRealtime(ctx, ch, allow, baseURL)
	}
}

// digestTickPlan 은 요약 채널의 이번 라운드 토큰 소비량과 배치 크기 상한을 반환한다.
//
// 두 반환값은 **서로 다른 단위**이며, 이것이 별도 함수로 분리된 이유다.
//
//   - tokens 는 메시지 건수다. 취약점 배치 하나가 메시지 하나로 합쳐져 HTTP 요청을 한 번만 보내므로 항상 1이다.
//     따라서 rate_per_min 은 여전히 digest 에 적용된다(분당 최대 이만큼의 요약 메시지).
//   - claimLimit 은 이 배치에 최대 몇 건의 취약점을 담을지다. 메모리 상한에만 제약받으며 요청 예산과는 무관하다.
//
// 과거에는 rate_per_min 을 digest 에 적용하려고 라운드당 요청 예산
// (notifyMaxSendsPerChannelPerTick, 임대 시간에서 역산한 값)을 그대로 배치 크기로 넘겼다.
// 그 결과 rate_per_min=20 인 채널은 3초 tick 마다 토큰이 1개씩만 채워졌고, 요약 메시지 하나에
// 취약점이 1건만 담겼다. digest 가 「요약 문구가 붙은 실시간 푸시」로 퇴화해, 수신자는
// 「최근 30분간 신규 취약점 1건」이라는 메시지를 연달아 받았고 db.MaxDigestBatchSize 에는 도달할 수 없었다.
//
// 이 증상은 E2E 테스트에서 쉽게 드러나지 않았다(기존 케이스는 stepDigest 에 충분히 큰 limit 을 직접 넘겨
// step 의 할당량 계산을 우회했다). 그래서 이 결정을 여기에 모으고
// TestDigestTickPlanDecouplesBatchSizeFromSendBudget 으로 직접 고정해 두었다.
func digestTickPlan() (tokens, claimLimit int) {
	return 1, db.MaxDigestBatchSize
}

// stepRealtime 은 단일 채널의 실시간 태스크를 수령해 전달한다. 취약점 1건당 메시지 1건이다.
func (n *Notifier) stepRealtime(ctx context.Context, ch *db.NotificationChannel, allow int, baseURL string) {
	deliveries, err := n.pg.ClaimRealtimeDeliveries(ctx, ch.ID, allow, notifyLease)
	if err != nil {
		log.Printf("[notify] 실시간 전달 수령 실패 channel=%d: %v", ch.ID, err)
		return
	}
	if len(deliveries) == 0 {
		return
	}
	channel, cfg, ok := n.adapt(ch)
	if !ok {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf(trMsg(errDeliveryChannelKindUnregistered), ch.Kind))
		return
	}
	for _, dl := range deliveries {
		msg, err := n.renderSingle(ctx, dl, baseURL)
		if err != nil {
			// 렌더링 실패는 로컬 데이터 문제라서 재시도해도 나아지지 않는다.
			_ = n.pg.FailDeliveries(ctx, []int64{dl.ID}, err.Error())
			continue
		}
		n.send(ctx, channel, cfg, msg, []*db.NotificationDelivery{dl})
	}
}

// stepDigest 는 배치가 기한이 되면 채널의 전송 대기 전달들을 메시지 하나로 합쳐 보낸다.
func (n *Notifier) stepDigest(ctx context.Context, ch *db.NotificationChannel, allow int, baseURL string) {
	window := n.digestInterval()
	due, err := n.pg.DigestBatchDue(ctx, ch.ID, window)
	if err != nil {
		log.Printf("[notify] 요약 배치 판정 실패 channel=%d: %v", ch.ID, err)
		return
	}
	if !due {
		return
	}
	deliveries, err := n.pg.ClaimDigestBatch(ctx, ch.ID, allow, notifyLease)
	if err != nil {
		log.Printf("[notify] 요약 배치 수령 실패 channel=%d: %v", ch.ID, err)
		return
	}
	if len(deliveries) == 0 {
		return
	}
	channel, cfg, ok := n.adapt(ch)
	if !ok {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf(trMsg(errDeliveryChannelKindUnregistered), ch.Kind))
		return
	}
	msg, included, err := n.renderBatch(ctx, deliveries, baseURL, int(window.Minutes()))
	if err != nil {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), err.Error())
		return
	}
	// 스냅샷이 깨져 메시지에 들어가지 못한 전달은 명시적으로 실패 처리해야 한다. 그렇지 않으면
	// included 밖에 남아 메시지에도 실패 목록에도 들어가지 않는다. 발송이 성공하면 이들의 상태가
	// 이후 일괄 표시에서 누락되어, 임대가 만료되어 다시 수령될 때까지 sending 상태에 머문다.
	if skipped := excludeDeliveries(deliveries, included); len(skipped) > 0 {
		reason := errDeliverySnapshotUnrenderable
		if fErr := n.pg.FailDeliveries(ctx, deliveryIDs(skipped), reason); fErr != nil {
			log.Printf("[notify] 깨진 스냅샷 전달 실패 표시 오류 channel=%s ids=%v: %v", ch.Kind, deliveryIDs(skipped), fErr)
		}
		log.Printf("[notify] 스냅샷을 해석할 수 없는 전달 %d건을 건너뜀 channel=%d", len(skipped), ch.ID)
	}
	// 메시지에 담긴 전달만 send 에 넘긴다. included[i] 와 msg.Items[i] 는 엄격히 대응하며,
	// send 는 이 대응 관계를 이용해 「채널이 앞의 K건만 담았다고 회신한 결과」를 올바른 전달 행에 반영한다.
	n.send(ctx, channel, cfg, msg, included)
}

// send 는 전달을 수행하고 결과에 따라 상태를 전이시킨다.
//
// 같은 배치의 전달(요약 모드에서는 수십 건일 수 있음)은 발송 결과를 공유한다. 즉 전부 도달하거나 배치 전체를 재시도한다.
// 건별 재시도는 하지 않는다. 요약 메시지는 하나이므로 그중 일부만 재전송하면 배치 의미가 깨진다.
//
// 유일한 예외는 **채널 길이 상한으로 인한 분할**이다. 채널이 실제로 앞의 K건만 담았다고 회신하면,
// K+1 번째부터는 다음 배치로 넘겨야 하며 함께 성공 처리하면 안 된다. 그렇지 않으면 잘린 취약점들이
// 메시지에도 실패 목록에도 없게 되어 완전히 사라진다.
func (n *Notifier) send(ctx context.Context, channel notify.Channel, cfg map[string]any, msg notify.Message, deliveries []*db.NotificationDelivery) {
	// 단일 전달에 상한을 두어, 특정 채널이 멈춰 이번 라운드의 나머지 채널이 모두 막히지 않게 한다.
	sendCtx, cancel := context.WithTimeout(ctx, notifySendTimeout)
	defer cancel()
	delivered, err := channel.Send(sendCtx, cfg, msg)
	if err == nil && delivered > 0 {
		if delivered > len(deliveries) {
			// 채널이 보고한 건수가 전달 수를 넘을 수는 없다. 실제로 발생했다면 렌더링 계층이 잘못 계산한 것이므로,
			// 전부 전달된 것으로 처리하고 문제를 기록해 두는 편이 기록을 뒤죽박죽으로 만드는 것보다 낫다.
			log.Printf("[notify] 채널이 보고한 전달 건수 %d 가 전달 수 %d 를 초과함 channel=%s, 전부 전달된 것으로 처리",
				delivered, len(deliveries), channel.Kind())
			delivered = len(deliveries)
		}
		sent, rest := deliveries[:delivered], deliveries[delivered:]
		if err := n.pg.MarkDeliveriesSent(ctx, deliveryIDs(sent)); err != nil {
			log.Printf("[notify] 전달 완료 표시 실패 channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(sent), err)
		}
		if len(rest) > 0 {
			// 이 메시지가 채널 길이 상한에 도달했다. 나머지는 즉시 큐로 되돌려 다음 tick 에서 이어서 보낸다.
			// RescheduleDeliveries 가 아니라 DeferDeliveries 를 쓴다. 이는 실패가 아니므로
			// 재시도 예산을 소모하면 안 된다(수령 시 이미 낙관적으로 +1 되었으므로 거기서 되돌린다).
			if err := n.pg.DeferDeliveries(ctx, deliveryIDs(rest),
				fmt.Sprintf(trMsg(errDeliveryChannelLengthCapped), delivered)); err != nil {
				log.Printf("[notify] 분할 이어 보내기 큐 등록 실패 channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(rest), err)
			}
		}
		return
	}
	if err == nil {
		// 채널이 오류도 없이 몇 건이 전달되었는지도 알려주지 않았다. 실패로 처리해(백오프 적용),
		// 이 전달이 반복 수령되면서 영원히 표시되지 못하는 상황을 막는다.
		err = fmt.Errorf(trMsg(errDeliveryNoDeliveredCount), delivered)
	}

	// 실패 처리는 **건별로** 결정하며, 배치 전체의 최대 시도 횟수로 판단하지 않는다.
	//
	// 과거에는 `if maxAttempts(deliveries) >= MaxNotifyAttempts` 로 배치 전체를 한꺼번에 실패 처리했다.
	// 하지만 배치 안의 각 건의 시도 횟수는 서로 다르다. 이미 두 번 재시도한 오래된 전달(attempts=2)이
	// 같은 배치의 새 전달(attempts=1)까지 failed 로 끌고 가, 새 취약점은 재시도 한 번 없이 영구 유실되었다.
	// 「오래된 행이 새 행을 끌어들이지 않게 한다」는 원래 의도와 정반대의 결과였다.
	permanent := notify.IsPermanent(err)
	var failIDs, exhaustedIDs []int64
	byDelay := map[time.Duration][]int64{}
	for _, dl := range deliveries {
		switch {
		case permanent:
			failIDs = append(failIDs, dl.ID)
		case dl.Attempts >= db.MaxNotifyAttempts:
			exhaustedIDs = append(exhaustedIDs, dl.ID)
		default:
			delay := notifyBackoff[min(dl.Attempts, len(notifyBackoff)-1)]
			byDelay[delay] = append(byDelay[delay], dl.ID)
		}
	}

	if len(failIDs) > 0 {
		if fErr := n.pg.FailDeliveries(ctx, failIDs, err.Error()); fErr != nil {
			log.Printf("[notify] 실패 상태 표시 오류 channel=%s ids=%v: %v", channel.Kind(), failIDs, fErr)
		}
	}
	if len(exhaustedIDs) > 0 {
		reason := fmt.Sprintf(trMsg(errDeliveryRetryExhausted), db.MaxNotifyAttempts, err)
		if fErr := n.pg.FailDeliveries(ctx, exhaustedIDs, reason); fErr != nil {
			log.Printf("[notify] 실패 상태 표시 오류 channel=%s ids=%v: %v", channel.Kind(), exhaustedIDs, fErr)
		}
	}
	// 지연 시간별로 묶어 재배치한다. 백오프는 3단계뿐이라 그룹 수가 자연히 작으므로,
	// 건마다 UPDATE 를 따로 보낼 필요가 없다(500건짜리 배치라면 500번 왕복이 생긴다).
	for delay, group := range byDelay {
		if rErr := n.pg.RescheduleDeliveries(ctx, group, delay, err.Error()); rErr != nil {
			log.Printf("[notify] 전달 재배치 실패 channel=%s ids=%v: %v", channel.Kind(), group, rErr)
		}
	}
	if len(failIDs)+len(exhaustedIDs) > 0 {
		log.Printf("[notify] 전달 실패 channel=%d kind=%s 영구 실패=%d 재시도 소진=%d 재시도 대기=%d: %s",
			deliveries[0].ChannelID, channel.Kind(), len(failIDs), len(exhaustedIDs), len(byDelay), err)
	}
}

// excludeDeliveries 는 all 중 keep 에 없는 항목들을 반환한다(포인터 동일성 기준으로 비교).
// 「메시지에 담기지 못한」 전달을 찾아내는 데 쓰며, 이들은 명시적으로 처리해야 하고 회색 지대에 남겨 두면 안 된다.
func excludeDeliveries(all, keep []*db.NotificationDelivery) []*db.NotificationDelivery {
	inKeep := make(map[*db.NotificationDelivery]bool, len(keep))
	for _, dl := range keep {
		inKeep[dl] = true
	}
	var out []*db.NotificationDelivery
	for _, dl := range all {
		if !inKeep[dl] {
			out = append(out, dl)
		}
	}
	return out
}

// adapt 는 채널 구현을 가져오고 그 설정을 파싱한다.
// ok=false 는 타입이 등록되지 않았음을 뜻하며, 이 경우 무한 재시도하지 않고 바로 실패 처리해야 한다.
func (n *Notifier) adapt(ch *db.NotificationChannel) (notify.Channel, map[string]any, bool) {
	channel, ok := notify.Get(ch.Kind)
	if !ok {
		return nil, nil, false
	}
	var cfg map[string]any
	if len(ch.Config) > 0 {
		// 설정 파싱에 실패하면 빈 map 을 준다. 채널 자체의 Validate 가 「어느 필드가 없는지」를 알려주는데,
		// 그 오류가 JSON 파싱 오류보다 사용자가 문제를 고치는 데 더 도움이 된다.
		_ = json.Unmarshal(ch.Config, &cfg)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	return channel, cfg, true
}

// renderSingle 은 취약점 메시지 한 건을 렌더링한다.
func (n *Notifier) renderSingle(ctx context.Context, dl *db.NotificationDelivery, baseURL string) (notify.Message, error) {
	snap, err := parseSnapshot(dl)
	if err != nil {
		return notify.Message{}, err
	}
	item, err := n.itemFor(ctx, snap, baseURL)
	if err != nil {
		return notify.Message{}, err
	}
	return notify.Message{Items: []notify.Item{item}, HomeURL: baseURL}, nil
}

// renderBatch 는 요약 메시지를 렌더링한다. 스냅샷은 건별로 파싱하며, 한 건이 깨져도 그 건만 건너뛰고
// 배치 전체의 요약이 함께 날아가지 않도록 한다.
//
// 반환값 included 와 msg.Items 는 **엄격히 일대일 대응**한다(i번째 전달 ↔ i번째 항목).
// 이 대응은 필수 요건이다. 호출자는 「채널이 앞의 K건을 담았다」는 회신에 따라 앞의 K개 전달을
// 전달 완료로 표시한다. 여기서 깨진 스냅샷을 건너뛰면서 included 에서 제외하지 않으면 인덱스가 어긋나,
// 실패해야 할 깨진 항목이 전달 완료로 표시되고 정상 항목은 미전달로 잘못 판정된다.
// 깨진 건들은 호출자가 명시적으로 실패 처리한다. stepDigest 참고.
func (n *Notifier) renderBatch(ctx context.Context, deliveries []*db.NotificationDelivery, baseURL string, windowMinutes int) (notify.Message, []*db.NotificationDelivery, error) {
	items := make([]notify.Item, 0, len(deliveries))
	included := make([]*db.NotificationDelivery, 0, len(deliveries))
	for _, dl := range deliveries {
		snap, err := parseSnapshot(dl)
		if err != nil {
			// 깨진 스냅샷은 메시지에도 included 에도 넣지 않는다. 처리는 호출자가 맡는다
			// (「전달 완료」에 섞어 넘기지 않고 명시적으로 실패 표시한다).
			log.Printf("[notify] 요약 배치에서 해석할 수 없는 스냅샷 건너뜀 delivery=%d: %v", dl.ID, err)
			continue
		}
		item, err := n.itemFor(ctx, snap, baseURL)
		if err != nil {
			return notify.Message{}, nil, err
		}
		items = append(items, item)
		included = append(included, dl)
	}
	if len(items) == 0 {
		return notify.Message{}, nil, fmt.Errorf(trMsg(errDeliveryBatchAllUnparseable), len(deliveries))
	}
	return notify.Message{
		Items:         items,
		Batch:         true,
		WindowMinutes: windowMinutes,
		HomeURL:       baseURL,
	}, included, nil
}

// itemFor 는 이벤트 스냅샷을 푸시 대기 항목으로 렌더링하며, 자산 이름과 상세 페이지 링크도 함께 해석한다.
func (n *Notifier) itemFor(ctx context.Context, snap notify.Snapshot, baseURL string) (notify.Item, error) {
	assets, err := n.pg.NotificationAssetNames(ctx, snap.AssetIDs)
	if err != nil {
		// 자산 이름 해석 실패로 푸시를 막으면 안 된다. 이름을 못 읽는 것이 알림을 못 받는 것보다 훨씬 가벼우며,
		// 메시지에서 자산 한 줄이 빠질 뿐이다.
		log.Printf("[notify] 자산 이름 해석 실패 finding=%d: %v", snap.FindingID, err)
	}
	item := notify.Item{
		FindingID:  snap.FindingID,
		Name:       snap.Name,
		VulnClass:  snap.VulnClass,
		Severity:   snap.Severity,
		Summary:    snap.Summary,
		Assets:     assets,
		FromStatus: snap.FromStatus,
		ToStatus:   snap.ToStatus,
	}
	if baseURL != "" {
		// 상세 페이지 라우트는 web/src/app/(main)/function/findings/detail/page.tsx 참고.
		// 이 페이지는 query 파라미터 id 에서 취약점 id 를 읽는다.
		item.DetailURL = fmt.Sprintf("%s/function/findings/detail?id=%d", baseURL, snap.FindingID)
	}
	return item, nil
}

// takeTokens 는 채널 토큰 버킷에서 **최대 want 개**의 토큰을 가져가고, 실제로 가져온 개수를 반환한다.
//
// 토큰 1개 = 메시지 1건(HTTP 요청 1회). 실시간 모드에서는 호출자가 필요한 만큼 넘기고,
// 요약 모드에서는 배치 하나가 메시지 하나로 나가므로 1을 넘긴다.
//
// 버킷 용량은 채널의 분당 상한이며 일정한 속도로 보충된다. ratePerMin<=0 은 속도 제한이 없음을 뜻하며,
// 단일 루프가 무한히 쌓인 적체에 끌려가지 않도록 유한하지만 충분히 큰 값을 반환한다.
//
// want 상한은 필수다. 이것이 없으면 버킷을 통째로 비울 수밖에 없는데, 호출자는 라운드당 상한을 따로 두고 있어
// 가져온 토큰 중 일부는 쓰이지 못한 채 다음 보충 전에 사라진다. 그러면 쌓아 둔 버스트 용량에는 영원히 도달할 수 없고,
// 「이번 라운드에 전송 대기 전달이 전혀 없는」 경우에도 토큰이 차감되어 버린다.
func (n *Notifier) takeTokens(channelID int64, ratePerMin, want int, now time.Time) int {
	if want <= 0 {
		return 0
	}
	if ratePerMin <= 0 {
		return min(want, notifyUnlimitedBurstPerTick)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	b := n.buckets[channelID]
	if b == nil {
		b = &notifyBucket{tokens: float64(ratePerMin), lastFill: now}
		n.buckets[channelID] = b
	}
	// 실제 경과 시간에 따라 보충한다. 속도는 초당 ratePerMin/60 이다.
	if elapsed := now.Sub(b.lastFill).Seconds(); elapsed > 0 {
		b.tokens = minF(float64(ratePerMin), b.tokens+elapsed*float64(ratePerMin)/60)
		b.lastFill = now
	}
	// 극소 epsilon 을 더한 뒤 정수로 내림한다. 토큰 수는 부동소수점 누적으로 얻어지므로, 두 번에 나눠 채울 때
	// 0.5 + 0.5 가 0.9999999999 가 될 수 있는데 그대로 int() 를 하면 0 으로 잘린다.
	// 수학적으로는 가득 찬 버킷인데도 토큰을 꺼낼 수 없게 된다. 1e-9 는 토큰 하나보다 훨씬 작아 실제 부족분은 놓치지 않는다.
	take := min(int(b.tokens+1e-9), want)
	if take <= 0 {
		return 0
	}
	b.tokens -= float64(take)
	return take
}

// enabled 는 전체 스위치를 읽는다.
func (n *Notifier) enabled() bool {
	return n.pg.GetBool(settingNotifyEnabled, true)
}

// publicBaseURL 은 링크에 쓰는 외부 주소를 반환하며, 끝의 슬래시는 제거한다.
func (n *Notifier) publicBaseURL() string {
	v, ok, err := n.pg.GetSetting(settingNotifyPublicBaseURL)
	if err != nil || !ok {
		return ""
	}
	return trimTrailingSlash(v)
}

// digestInterval 은 요약 주기를 반환한다. 값이 잘못되었거나 설정되지 않았으면 기본값으로 되돌린다.
func (n *Notifier) digestInterval() time.Duration {
	v, ok, err := n.pg.GetSetting(settingNotifyDigestMinutes)
	if err != nil || !ok {
		return time.Duration(notifyDefaultDigestMinutes) * time.Minute
	}
	m := 0
	if _, err := fmt.Sscanf(v, "%d", &m); err != nil || m <= 0 {
		return time.Duration(notifyDefaultDigestMinutes) * time.Minute
	}
	return time.Duration(m) * time.Minute
}

// parseSnapshot 은 전달에 해당하는 이벤트의 스냅샷을 파싱한다.
func parseSnapshot(dl *db.NotificationDelivery) (notify.Snapshot, error) {
	var snap notify.Snapshot
	if len(dl.Snapshot) == 0 {
		return snap, fmt.Errorf(trMsg(errDeliverySnapshotEmpty), dl.ID)
	}
	if err := json.Unmarshal(dl.Snapshot, &snap); err != nil {
		return snap, fmt.Errorf(trMsg(errDeliverySnapshotParse), dl.ID, err)
	}
	if snap.Kind == "" {
		// 이벤트 타입은 이벤트 행을 기준으로 한다. 스냅샷에 있는 값은 이전 버전이 기록한 것일 수 있다.
		snap.Kind = dl.EventKind
	}
	return snap, nil
}

func deliveryIDs(deliveries []*db.NotificationDelivery) []int64 {
	out := make([]int64, 0, len(deliveries))
	for _, dl := range deliveries {
		out = append(out, dl.ID)
	}
	return out
}

func trimTrailingSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
