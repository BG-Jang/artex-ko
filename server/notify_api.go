package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/notify"
)

// 이 파일은 푸시 기능의 HTTP 인터페이스다. 모든 라우트는 requireAuth 뒤에 마운트된다(Handler() 참고),
// 다른 관리 인터페이스와 같은 방식이다.

// notify_api.go 가 HTTP 응답으로 돌려주는 사용자 노출 오류 문구다. 한국어 UI 에서 알림
// 설정 요청이 실패하면 이 문구가 그대로 토스트로 뜨므로 한국어로 둔다. 식별자(id)·JSON
// 필드명·enum 값(realtime·digest)·채널 종류 키는 사용자가 설정을 고치는 데 쓰는 값이라
// 원문 그대로 둔다. %s·%q 가 든 상수는 fmt.Sprintf 형식 문자열이다. 용어: 채널, 알림 푸시, 전송, 발송 모드.
// 로그·주석은 BRIEF
// 방침상 최하위라 이 묶음 밖이다.
const (
	notifyErrBadJSON         = "요청 본문이 올바른 JSON 형식이 아닙니다: "
	notifyErrKindInvalidFmt  = "채널 유형이 올바르지 않습니다. 가능한 값: %s"
	notifyErrNameMissing     = "채널 이름을 입력하세요"
	notifyErrNameEmpty       = "채널 이름은 비워 둘 수 없습니다"
	notifyErrModeInvalid     = "발송 모드가 올바르지 않습니다. 가능한 값: realtime / digest"
	notifyErrRateNegative    = "전송 제한 값은 음수일 수 없습니다"
	notifyErrChannelID       = "채널 id 가 올바르지 않습니다"
	notifyErrKindUnregFmt    = "채널 유형 %q 는 등록되어 있지 않습니다"
	notifyErrDeliveryID      = "전송 id 가 올바르지 않습니다"
	notifyErrChannelNotFound = "알림 채널을 찾을 수 없습니다"
)

// 채널 연결을 점검할 때 보내는 테스트 메시지 본문이다. 사용자가 등록한 채널로 실제 발송되므로
// 한국어로 두되, 받는 사람이 실제 취약점으로 오해하지 않도록 한눈에 테스트임이 드러나게 한다.
const (
	notifyTestName    = "테스트 메시지 · 채널 설정 정상"
	notifyTestClass   = "연결 테스트"
	notifyTestSummary = "ARTEX 알림 채널 테스트 메시지입니다. 이 메시지를 받으셨다면 채널 설정이 정상입니다."
)

// notifyChannelDTO 는 채널의 외부 표현이다.
//
// Config 는 **마스킹된** 설정이다. 자격 증명 필드는 notify.MaskedPrefix 로 시작하는 값으로 치환된다.
// 프론트엔드가 마스킹 값을 그대로 다시 제출하면 「이 필드는 바뀌지 않았다」는 뜻이며, 서버는 이에 따라 DB 의 원래 값을 유지한다.
// (notify.MergeConfig 참고).
type notifyChannelDTO struct {
	ID         int64          `json:"id"`
	Name       string         `json:"name"`
	Kind       string         `json:"kind"`
	Enabled    bool           `json:"enabled"`
	Mode       string         `json:"mode"`
	Config     map[string]any `json:"config"`
	Filter     notify.Filter  `json:"filter"`
	RatePerMin int            `json:"rate_per_min"`
	CreatedAt  time.Time      `json:"created_at"`
	UpdatedAt  time.Time      `json:"updated_at"`
	// SecretKeys 는 프론트엔드에 어떤 필드가 자격 증명인지 알려주며, 이에 따라 비밀번호 입력란과 「비워 두면 변경 없음」 안내를 렌더링한다.
	// 채널 스스로 선언하며(notify.Channel.SecretKeys), 프론트엔드는 채널 지식을 하드코딩하지 않는다.
	SecretKeys []string `json:"secret_keys"`
}

// notifyDeliveryDTO 는 전달 이력의 외부 표현이다.
type notifyDeliveryDTO struct {
	ID          int64      `json:"id"`
	FindingID   int64      `json:"finding_id,string"`
	EventKind   string     `json:"event_kind"`
	ChannelID   int64      `json:"channel_id"`
	ChannelName string     `json:"channel_name"`
	ChannelKind string     `json:"channel_kind"`
	State       string     `json:"state"`
	Attempts    int        `json:"attempts"`
	LastError   string     `json:"last_error"`
	BatchID     *int64     `json:"batch_id,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	SentAt      *time.Time `json:"sent_at,omitempty"`
	NextAttempt time.Time  `json:"next_attempt_at"`
	// 메시지 제목 요약. 이력 목록을 펼치지 않아도 이 항목이 무엇을 보낸 것인지 알 수 있게 한다.
	Title    string `json:"title"`
	Severity string `json:"severity"`
}

func toNotifyChannelDTO(ch *db.NotificationChannel) notifyChannelDTO {
	var cfg map[string]any
	if len(ch.Config) > 0 {
		_ = json.Unmarshal(ch.Config, &cfg)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	secrets := []string{}
	if c, ok := notify.Get(ch.Kind); ok {
		secrets = c.SecretKeys()
	}
	return notifyChannelDTO{
		ID:         ch.ID,
		Name:       ch.Name,
		Kind:       ch.Kind,
		Enabled:    ch.IsEnabled(),
		Mode:       ch.Mode,
		Config:     notify.MaskConfig(ch.Kind, cfg),
		Filter:     notify.ParseFilter(ch.Filter),
		RatePerMin: ch.RatePerMin,
		CreatedAt:  ch.CreatedAt,
		UpdatedAt:  ch.UpdatedAt,
		SecretKeys: secrets,
	}
}

func toNotifyDeliveryDTO(dl *db.NotificationDelivery) notifyDeliveryDTO {
	snap, _ := parseSnapshot(dl)
	dto := notifyDeliveryDTO{
		ID:          dl.ID,
		FindingID:   dl.FindingID,
		EventKind:   dl.EventKind,
		ChannelID:   dl.ChannelID,
		ChannelName: dl.ChannelName,
		ChannelKind: dl.ChannelKind,
		State:       dl.State,
		Attempts:    dl.Attempts,
		LastError:   dl.LastError,
		BatchID:     dl.BatchID,
		CreatedAt:   dl.CreatedAt,
		SentAt:      dl.SentAt,
		NextAttempt: dl.NextAttemptAt,
		Severity:    snap.Severity,
	}
	if snap.Name != "" {
		dto.Title = snap.Name
	} else {
		dto.Title = snap.VulnClass
	}
	return dto
}

// notifyMeta 는 알림 페이지에 필요한 정적 메타데이터와 전역 설정을 반환한다. 한 번의 요청으로 모두 받도록 하여,
// 드롭다운 하나를 렌더링하려고 프론트엔드가 요청을 세 번 보내는 일을 막는다.
func (s *Server) notifyMeta(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	kinds := make([]map[string]any, 0, len(notify.Kinds()))
	for _, k := range notify.Kinds() {
		ch, _ := notify.Get(k)
		kinds = append(kinds, map[string]any{
			"kind":                 k,
			"default_rate_per_min": ch.DefaultRatePerMin(),
			"secret_keys":          ch.SecretKeys(),
		})
	}
	baseURL, _, _ := pg.GetSetting(settingNotifyPublicBaseURL)
	digest, _, _ := pg.GetSetting(settingNotifyDigestMinutes)
	stats, err := pg.NotificationStatsSnapshot(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"kinds":               kinds,
		"enabled":             pg.GetBool(settingNotifyEnabled, true),
		"public_base_url":     baseURL,
		"digest_interval_min": digest,
		"defaults": map[string]any{
			"digest_interval_min": notifyDefaultDigestMinutes,
		},
		"stats": stats,
	})
}

func (s *Server) notifyListChannels(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	channels, err := pg.ListNotificationChannels(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out := make([]notifyChannelDTO, 0, len(channels))
	for _, ch := range channels {
		out = append(out, toNotifyChannelDTO(ch))
	}
	writeJSON(w, 200, map[string]any{"channels": out})
}

// notifyChannelRequest 는 채널 생성/수정 요청의 본문이다.
//
// 모든 업무 필드는 포인터로 두어 「전달되지 않음」과 「0 값으로 전달됨」을 구분한다. PATCH 시맨틱에서는
// 전달되지 않은 필드는 DB 의 원래 값을 반드시 유지해야 한다.
type notifyChannelRequest struct {
	Name       *string        `json:"name"`
	Kind       *string        `json:"kind"`
	Enabled    *bool          `json:"enabled"`
	Mode       *string        `json:"mode"`
	Config     map[string]any `json:"config"`
	Filter     *notify.Filter `json:"filter"`
	RatePerMin *int           `json:"rate_per_min"`
}

func (s *Server) notifyCreateChannel(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var req notifyChannelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, notifyErrBadJSON+err.Error())
		return
	}
	if req.Kind == nil || !notify.ValidKind(*req.Kind) {
		writeErr(w, 400, fmt.Sprintf(notifyErrKindInvalidFmt, strings.Join(notify.Kinds(), " / ")))
		return
	}
	name := ""
	if req.Name != nil {
		name = strings.TrimSpace(*req.Name)
	}
	if name == "" {
		writeErr(w, 400, notifyErrNameMissing)
		return
	}
	channel, _ := notify.Get(*req.Kind)
	if err := channel.Validate(req.Config); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	ch := &db.NotificationChannel{
		Name:       name,
		Kind:       *req.Kind,
		Enabled:    req.Enabled,
		Mode:       db.NotifyModeRealtime,
		RatePerMin: channel.DefaultRatePerMin(),
	}
	if req.Mode != nil {
		if !db.ValidNotifyMode(*req.Mode) {
			writeErr(w, 400, notifyErrModeInvalid)
			return
		}
		ch.Mode = *req.Mode
	}
	if req.RatePerMin != nil {
		// 명시적으로 값을 준 경우 그대로 쓴다. 0 도 포함하며, 0 은 「속도 제한 없음」을 뜻하는 유효한 설정이다.
		if *req.RatePerMin < 0 {
			writeErr(w, 400, notifyErrRateNegative)
			return
		}
		ch.RatePerMin = *req.RatePerMin
	}
	// 「필드가 생략된 경우」에만 채널 기본값을 적용한다. 기본값은 DB 계층이 아니라 여기서 결정해야 한다.
	// 「이 필드를 보내지 않음」과 「0 을 명시적으로 보냄」은 요청 본문에서만 구분할 수 있으며, 두 경우의 의미는 완전히 다르다.
	// (전자=기본값 사용, 후자=속도 제한 없음). DB 계층이 0 도 미지정으로 취급하면 속도 제한 없음 설정에 도달할 수 없게 된다.
	if req.RatePerMin == nil {
		ch.RatePerMin = channel.DefaultRatePerMin()
	}
	if req.Filter != nil {
		// 쓰기 시 값 범위가 제한된 필터 필드(예: min_severity)를 검증한다. 자세한 내용은 notify.Filter.Validate 참고:
		// 임계값에 오타가 나면 필터가 조용히 무효화되어 전체 푸시가 되므로, 반드시 입구에서 차단해야 한다.
		if err := req.Filter.Validate(); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		raw, _ := json.Marshal(req.Filter)
		ch.Filter = raw
	}
	rawCfg, _ := json.Marshal(req.Config)
	ch.Config = rawCfg

	id, err := pg.SaveNotificationChannel(r.Context(), ch)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": id})
}

func (s *Server) notifyUpdateChannel(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, notifyErrChannelID)
		return
	}
	current, err := pg.NotificationChannelByID(r.Context(), id)
	if err != nil {
		notifyChannelLookupErr(w, err)
		return
	}
	var req notifyChannelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, notifyErrBadJSON+err.Error())
		return
	}

	// kind 는 수정할 수 있지만, 타입을 바꾸면 자격 증명 필드를 통째로 교체해야 하므로 기존 설정과 병합할 수 없다.
	kind := current.Kind
	if req.Kind != nil {
		if !notify.ValidKind(*req.Kind) {
			writeErr(w, 400, fmt.Sprintf(notifyErrKindInvalidFmt, strings.Join(notify.Kinds(), " / ")))
			return
		}
		kind = *req.Kind
	}
	channel, _ := notify.Get(kind)

	var stored map[string]any
	if kind == current.Kind {
		if len(current.Config) > 0 {
			_ = json.Unmarshal(current.Config, &stored)
		}
	}
	if stored == nil {
		stored = map[string]any{}
	}
	// 단순 MergeConfig 가 아니라 PrepareConfigUpdate 를 쓴다. 대상 주소가 바뀌면 운영자가
	// 자격 증명 필드에 대해 다시 의사를 밝혀야 한다. 그렇지 않으면 「주소만 바꾸고 자격 증명은 그대로」인 경우 DB 의 실제 자격 증명이 새 주소로 전송된다.
	merged, err := notify.PrepareConfigUpdate(kind, stored, req.Config)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := channel.Validate(merged); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	rawCfg, _ := json.Marshal(merged)

	ch := &db.NotificationChannel{
		ID:         id,
		Name:       current.Name,
		Kind:       kind,
		Enabled:    current.Enabled,
		Mode:       current.Mode,
		Config:     rawCfg,
		Filter:     current.Filter,
		RatePerMin: current.RatePerMin,
	}
	if req.Name != nil {
		if ch.Name = strings.TrimSpace(*req.Name); ch.Name == "" {
			writeErr(w, 400, notifyErrNameEmpty)
			return
		}
	}
	if req.Enabled != nil {
		ch.Enabled = req.Enabled
	}
	if req.Mode != nil {
		if !db.ValidNotifyMode(*req.Mode) {
			writeErr(w, 400, notifyErrModeInvalid)
			return
		}
		ch.Mode = *req.Mode
	}
	if req.RatePerMin != nil {
		if *req.RatePerMin < 0 {
			writeErr(w, 400, notifyErrRateNegative)
			return
		}
		ch.RatePerMin = *req.RatePerMin
	}
	if req.Filter != nil {
		if err := req.Filter.Validate(); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		raw, _ := json.Marshal(req.Filter)
		ch.Filter = raw
	}

	// SaveNotificationChannel 이 아니라 SetNotificationChannelEnabled 경로를 타는 이유는,
	// 「비활성화」 시 기존 전송 대기 전달도 함께 skipped 로 표시해, 다시 활성화할 때
	// 이미 오래된 적체 메시지가 한꺼번에 도착하지 않도록 하기 위함이다.
	enabledChanged := ch.Enabled != nil && current.Enabled != nil && *ch.Enabled != *current.Enabled
	if enabledChanged {
		// 먼저 설정 변경을 DB 에 저장한다(이때 enabled 는 이전 값을 써서 건너뛰기 로직이 미리 발동하지 않게 한다),
		// 그다음 스위치를 따로 전환한다. 두 단계 사이에 동시성 창은 없다. 이 인터페이스가 두 필드를 바꾸는 유일한 진입점이다.
		prev := ch.Enabled
		ch.Enabled = current.Enabled
		if _, err := pg.SaveNotificationChannel(r.Context(), ch); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if err := pg.SetNotificationChannelEnabled(r.Context(), id, *prev); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"id": id})
		return
	}
	if _, err := pg.SaveNotificationChannel(r.Context(), ch); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": id})
}

func (s *Server) notifyDeleteChannel(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, notifyErrChannelID)
		return
	}
	if err := pg.DeleteNotificationChannel(r.Context(), id); err != nil {
		notifyChannelLookupErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// notifyTestChannel 은 현재 저장된 설정으로 테스트 메시지 한 건을 보낸다.
//
// 전달 큐를 거치지 않고 채널의 Send 를 직접 호출한다. 테스트의 목적은 사용자에게 「이 설정으로 보낼 수 있는지」를 즉시 알려주는 것이다.
// 큐를 거치면 결과가 전달 이력에 묻혀, 사용자가 다시 찾아봐야 성공 여부를 알 수 있게 된다.
// 따라서 이 인터페이스는 **동기** 방식이며, 타임아웃 상한은 notify 패키지의 HTTP 클라이언트가 정한다(15초).
func (s *Server) notifyTestChannel(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, notifyErrChannelID)
		return
	}
	ch, err := pg.NotificationChannelByID(r.Context(), id)
	if err != nil {
		notifyChannelLookupErr(w, err)
		return
	}
	channel, ok := notify.Get(ch.Kind)
	if !ok {
		writeErr(w, 400, fmt.Sprintf(notifyErrKindUnregFmt, ch.Kind))
		return
	}
	var cfg map[string]any
	if len(ch.Config) > 0 {
		_ = json.Unmarshal(ch.Config, &cfg)
	}
	if err := channel.Validate(cfg); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	msg := notifyTestMessage(s.notifierBaseURL(pg))
	start := time.Now()
	// 테스트 메시지는 한 건뿐이므로 전달 건수는 여기서 필요 없다(채널 길이 상한은 단일 메시지에 대해
	// 자르기로 처리되며 분할은 관여하지 않는다).
	if _, err := channel.Send(r.Context(), cfg, msg); err != nil {
		// 채널이 반환한 원본 오류를 그대로 사용자에게 돌려준다. 이것이 사용자가 설정을 디버깅하는 유일한 단서다.
		writeErr(w, 502, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"ok":         true,
		"latency_ms": time.Since(start).Milliseconds(),
	})
}

// notifyTestMessage 는 테스트 메시지를 구성한다. 한눈에 테스트임을 알 수 있는 내용으로 의도적으로 만든다.
// 받은 사람이 이를 실제 취약점으로 오인하면 안 되기 때문이다.
func notifyTestMessage(baseURL string) notify.Message {
	return notify.Message{
		Items: []notify.Item{{
			FindingID: 0,
			Name:      notifyTestName,
			VulnClass: notifyTestClass,
			Severity:  "low",
			Summary:   notifyTestSummary,
			Assets:    []string{"artex.example.com"},
			DetailURL: baseURL,
		}},
		HomeURL: baseURL,
	}
}

// notifierBaseURL 은 링크용 외부 주소를 읽어 온다.
func (s *Server) notifierBaseURL(pg *db.DB) string {
	v, _, _ := pg.GetSetting(settingNotifyPublicBaseURL)
	return trimTrailingSlash(v)
}

func (s *Server) notifyListDeliveries(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	f := db.NotificationDeliveryFilter{
		State:     r.URL.Query().Get("state"),
		EventKind: r.URL.Query().Get("event_kind"),
	}
	if v := r.URL.Query().Get("channel_id"); v != "" {
		f.ChannelID = int64(atoiDefault(v, 0))
	}
	page := queryInt(r, "page", 1)
	pageSize := queryInt(r, "page_size", 50)
	items, total, err := pg.ListNotificationDeliveries(r.Context(), f, page, pageSize)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out := make([]notifyDeliveryDTO, 0, len(items))
	for _, dl := range items {
		out = append(out, toNotifyDeliveryDTO(dl))
	}
	writeJSON(w, 200, map[string]any{"deliveries": out, "total": total, "page": page, "page_size": pageSize})
}

func (s *Server) notifyRetryDelivery(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok {
		writeErr(w, 400, notifyErrDeliveryID)
		return
	}
	if err := pg.RetryNotificationDelivery(r.Context(), id); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// notifyChannelLookupErr 는 「채널 없음」을 404 로, 그 밖의 오류는 500 으로 변환한다.
func notifyChannelLookupErr(w http.ResponseWriter, err error) {
	if errors.Is(err, db.ErrNotificationChannelNotFound) {
		writeErr(w, 404, notifyErrChannelNotFound)
		return
	}
	writeErr(w, 500, err.Error())
}
