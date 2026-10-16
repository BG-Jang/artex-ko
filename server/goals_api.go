package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
)

// goals_api.go 가 사용자에게 돌려주는 오류 응답 문구. 명령·요청 필드명(text·vulnclass·
// gid)·enum 은 원문 그대로 두고, 사람이 읽는 메시지만 한국어로 둔다. 영어 writeErr
// (task not found·invalid JSON·bad goal id)는 F3b 중국어 전용 범위라 보존한다.
const (
	errGoalTaskDeletingAdd    = "작업을 삭제하는 중이라 목표를 추가할 수 없습니다"
	errGoalTaskDeletingEdit   = "작업을 삭제하는 중이라 목표를 수정할 수 없습니다"
	errGoalTaskDeletingDelete = "작업을 삭제하는 중이라 목표를 삭제할 수 없습니다"
	errGoalTextEmpty          = "목표 내용은 비워 둘 수 없습니다"
	errGoalNotFound           = "목표를 찾을 수 없습니다"
	errGoalReadAfterAdd       = "목표를 저장한 뒤 읽지 못했습니다"
	errGoalReadAfterEdit      = "목표를 수정한 뒤 읽지 못했습니다"
)

// 개요「목표 관리」의 수동 CRUD 인터페이스. 에이전트 측 set_goals 도구와 같은 goal 노드를 쓰지만,
// 입구는 사람이 UI 에서 직접 추가/수정/삭제하는 것이다. 추가/수정 후에는 「작업 부활」 로직(admitTask resume:
// 종료 상태→running, 일시 정지 해제, 필요 시 대기열 등록), 삭제 시에는 부활하지 않는다(제품 결정). 각 변경 handler 는 모두
// beginTaskOperation/decInflight 를 거쳐 작업 삭제와의 경합을 피한다(의도 CRUD 와 동일).

// listGoals 는 이 작업의 모든 목표(text/vulnclass/state 로 분리됨)를 반환하여 목표 관리 카드 렌더링에 쓴다.
func (s *Server) listGoals(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	goals, err := t.Store.ListByKind(db.KindGoal, 10000)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"goals": goalDTOs(goals)})
}

// addGoal 은 사람이 목표를 하나 추가한다: DB 에 저장(작업 루트 spawns 하위에 연결)→ 「목표 추가」 트리거를 기록해 깨움
// planner → 작업 부활, 플래너가 새 목표에 따라 달성 여부를 다시 판단하게 한다.
func (s *Server) addGoal(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, errGoalTaskDeletingAdd)
		return
	}
	defer s.engine.decInflight(t.ID)

	var body struct {
		Text      string `json:"text"`
		VulnClass string `json:"vulnclass"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		writeErr(w, 400, errGoalTextEmpty)
		return
	}
	payload := map[string]any{"text": text}
	if vc := strings.TrimSpace(body.VulnClass); vc != "" {
		payload["vulnclass"] = vc
	}
	id, err := t.Store.AddGoal(payload, "human")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if of, _ := t.Store.OriginFactID(); of > 0 && id > 0 {
		_ = t.Store.Link(of, db.RelSpawns, id) // goal descends from the task root (origin fact)
	}
	t.NotifyGoal([]string{text}) // 「사람이 목표를 추가함: …」 트리거를 기록하고 planner 를 깨움
	s.reviveTask(t)              // 완료/일시 정지된 작업을 실행 상태로 되돌려 계속 실행
	node, _ := t.Store.GetNode(id)
	if node == nil {
		writeErr(w, 500, errGoalReadAfterAdd)
		return
	}
	writeJSON(w, 200, goalDTO(node))
}

// editGoal 은 사람이 목표 텍스트(및 vulnclass)를 수정한다: DB 수정 → 「사람이 목표를 old 에서 new 로 수정함」 기록
// 트리거로 planner 를 깨움 → 작업 부활, 플래너가 새 목표에 따라 방향을 조정하게 한다.
func (s *Server) editGoal(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, errGoalTaskDeletingEdit)
		return
	}
	defer s.engine.decInflight(t.ID)

	gid, err := strconv.ParseInt(r.PathValue("gid"), 10, 64)
	if err != nil || gid <= 0 {
		writeErr(w, 400, "bad goal id")
		return
	}
	var body struct {
		Text      string `json:"text"`
		VulnClass string `json:"vulnclass"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		writeErr(w, 400, errGoalTextEmpty)
		return
	}
	node, err := t.Store.GetNode(gid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if node == nil || node.Kind != db.KindGoal {
		writeErr(w, 404, errGoalNotFound)
		return
	}
	oldText := goalDTO(node).Text
	if err := t.Store.UpdateGoalPayload(gid, text, strings.TrimSpace(body.VulnClass)); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	t.NotifyGoalEdited(oldText, text) // 「사람이 목표를 old 에서 new 로 수정함」 트리거를 기록하고 planner 를 깨움
	s.reviveTask(t)                   // 추가와 동일: 작업을 부활시켜 새 목표에 따라 다시 판단
	updated, _ := t.Store.GetNode(gid)
	if updated == nil {
		writeErr(w, 500, errGoalReadAfterEdit)
		return
	}
	writeJSON(w, 200, goalDTO(updated))
}

// deleteGoal 은 사람이 목표 하나를 삭제한다(하드 삭제, 연결 간선/앵커도 함께 삭제): DB 삭제 → 「사람이 해당 목표 X 를 삭제함」 기록
// 트리거로 planner 를 깨워 남은 목표를 다시 판단하게 한다. 제품 결정에 따라 삭제 시에는 작업을 【부활시키지 않는다】.
func (s *Server) deleteGoal(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, errGoalTaskDeletingDelete)
		return
	}
	defer s.engine.decInflight(t.ID)

	gid, err := strconv.ParseInt(r.PathValue("gid"), 10, 64)
	if err != nil || gid <= 0 {
		writeErr(w, 400, "bad goal id")
		return
	}
	node, err := t.Store.GetNode(gid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if node == nil || node.Kind != db.KindGoal {
		writeErr(w, 404, errGoalNotFound)
		return
	}
	text := goalDTO(node).Text
	if err := t.Store.DeleteGoal(gid); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	t.NotifyGoalDeleted(text) // 「사람이 해당 목표를 삭제함: …」 트리거를 기록하고 planner 를 깨움(작업은 부활하지 않음)
	writeJSON(w, 200, map[string]bool{"ok": true})
}
