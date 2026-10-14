package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
	acperm "github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// compactIntents distills intents to {id, summary, state, asset_ids, parents,
// yields} so the planner sees both the direction and its LINEAGE — parents (the
// upstream nodes it derived from: facts/intents/findings) and yields (the facts/
// findings it produced) — without pulling full payloads. parentsOf/yieldsOf are
// built from the exploration edges in graph_overview.
func compactIntents(ns []*db.Node, parentsOf, yieldsOf map[int64][]int64) []map[string]any {
	out := make([]map[string]any, 0, len(ns))
	for _, n := range ns {
		var p map[string]any
		_ = json.Unmarshal(n.Payload, &p)
		m := map[string]any{"id": n.ID, "summary": p["summary"], "state": n.State}
		if n.Inherited {
			m["source_task_id"] = n.SourceTaskID
			m["inherited"] = true
		}
		// asset_ids is the structured "which assets this direction covers" signal for
		// dedup; fall back to legacy payload keys (target_ids plural, then target_id
		// single) so intents stored before the rename still surface their anchors.
		if tg, ok := p["asset_ids"]; ok && tg != nil {
			m["asset_ids"] = tg
		} else if tg, ok := p["target_ids"]; ok && tg != nil {
			m["asset_ids"] = tg
		} else if tg, ok := p["target_id"]; ok && tg != nil && tg != "" {
			m["asset_ids"] = []any{tg}
		}
		if ps := parentsOf[n.ID]; len(ps) > 0 {
			m["parents"] = ps // 상위: 이 의도가 파생된 노드들 (여러 사실이 함께 하나의 의도를 낳을 수 있음)
		}
		if ys := yieldsOf[n.ID]; len(ys) > 0 {
			m["yields"] = ys // 하위: 이 의도가 낳은 사실/취약점
		}
		out = append(out, m)
	}
	return out
}

// ToolSet exposes the PG-backed dual graph (asset + exploration) to an LLM agent.
// One ToolSet is created per planner/worker run; per-run signals live here.
type ToolSet struct {
	findingRecorder FindingRecorder
	as              *db.AssetStore   // asset store (optional; nil = asset tools not available)
	cs              *db.CompanyStore // company store (optional)
	ts              *db.ExplorationStore
	worker          string
	taskID          int64 // PG tasks.id; 0 when unknown (tests / orchestrator cross-task reads)
	// coverageDisabled mirrors tasks.coverage_enabled=false. Stored inverted so the
	// zero value (all existing ToolSet constructions) means ENABLED — matching the
	// DB default (true). When true: graphOverviewData drops the coverage block, the
	// auto-scope hook (insertAssets) is skipped, and add_task_scope/list_untested_assets
	// are filtered out of the agent's tool list. The scope field stays regardless.
	coverageDisabled bool
	// ownerNode is the exploration node that writes attach to: assets this run
	// touches get anchored to it as lineage/provenance (NOT visibility — the asset
	// graph is global and shared). Worker = its claimed intent; planner = begin root.
	ownerNode int64
	GoalMet   bool
	Reason    string
	writes    WriteCounts
	// killWork, if set, terminates a running work by intent id (engine callback,
	// wired by the planner). nil = the kill_work tool reports unavailable.
	killWork func(intentID int64) error
	// steerWork, if set, queues a mid-run course-correction for the work running an
	// intent id (engine callback, wired by the planner): the worker injects it before
	// its next tool call and re-plans, without being killed. nil = tool unavailable.
	steerWork func(intentID int64, msg string) error
	// enrich, if set, receives async auto-completion triggers (DNS resolve for a
	// domain, HTTP probe for a site). nil = no engine enrichment.
	enrich EnrichTrigger
	// notify, if set, wakes the task's planner after a graph change that should be
	// re-planned promptly (currently: a new hint). nil = no wake (the hint is still
	// stored and read on the next round triggered by other events). debounced.
	notify func()
	// notifyFinding, if set, wakes the task's planner when this run reports a finding,
	// carrying (intentID, summary) so the round can spell out which intent found what.
	// Wired for workers; nil elsewhere → falls back to notify (bare wake).
	notifyFinding func(intentID int64, summary string)
	// resumeTask, if set, revives the task after a graph change that should make a
	// stopped task run again (currently: set_goals adds a goal). It flips a terminal/
	// paused task back to running and (re)starts the engine loops — a plain notify()
	// can't, because the planner's terminal gate swallows wakes. Wired ONLY for the
	// main agent (human steering); nil for the goals decomposer and workers.
	resumeTask func()
	// notifyGoal, if set, wakes the planner AND records ONE "사람이 N 개의 목표를 추가함: …" trigger
	// for a whole set_goals call (batch-aware — one call, one trigger, not one per goal)
	// so the next round spells out the added goals (instead of the planner having to
	// spot new open goals in the overview). Wired ONLY for the main agent; nil for the
	// goals decomposer (round-0 has no running planner to inform) and workers → those
	// fall back to the bare notify.
	notifyGoal func(texts []string)
	// notifyHint, if set, wakes the planner AND records ONE "사람이 N 개의 전략 힌트를 추가함: …"
	// trigger for a whole add_hint call (batch-aware — one call, one trigger) so the next
	// round is told the round was fired by a new hint and spells the hint out, instead of
	// the planner having to spot it folded into the graph overview. Wired for the main
	// agent + cross-task orchestration; nil elsewhere → falls back to the bare notify.
	notifyHint func(texts []string)
}

// SetNotifyGoal wires the goal-add trigger callback (see ToolSet.notifyGoal). Set only
// by the main-agent chat, so runtime-added goals are announced to the planner by name.
func (t *ToolSet) SetNotifyGoal(fn func([]string)) { t.notifyGoal = fn }

// SetNotifyHint wires the hint-add trigger callback (see ToolSet.notifyHint). Set by
// the main-agent chat and cross-task orchestration, so a runtime-added hint fires a
// planner round announced by name instead of a bare wake.
func (t *ToolSet) SetNotifyHint(fn func([]string)) { t.notifyHint = fn }

// SetResumeTask wires the task-revive callback (see ToolSet.resumeTask). Set only by
// the main-agent chat, so runtime-added goals can pull a finished task back to running.
func (t *ToolSet) SetResumeTask(fn func()) { t.resumeTask = fn }

// SetNotify wires the planner-wake callback (see ToolSet.notify). Set by callers
// that hold the task handle (main-agent chat, cross-task orchestration).
func (t *ToolSet) SetNotify(fn func()) { t.notify = fn }

// SetNotifyFinding wires the finding-wake callback (see ToolSet.notifyFinding).
func (t *ToolSet) SetNotifyFinding(fn func(int64, string)) { t.notifyFinding = fn }

// EnrichTrigger is the enrichment engine seen from the tool layer (see package
// enrich). Kept as an interface here to avoid coupling agent → enrich.
type EnrichTrigger interface {
	ResolveDomain(id int64, host string)
	ProbeSite(id int64, url string)
}

// WriteCounts breaks down what a worker persisted this run, by node kind, so the
// engine can log an accurate "wrote back" summary instead of lumping assets and
// findings under "facts" (record_fact → Facts, insert_assets → Assets,
// report_finding → Findings; each element of a batch counts once).
type WriteCounts struct {
	Facts    int
	Assets   int
	Findings int
}

// Total is every node persisted this run, regardless of kind — the
// "explored but persisted nothing" signal (Total == 0).
func (w WriteCounts) Total() int { return w.Facts + w.Assets + w.Findings }

// String renders the per-kind breakdown for logs, e.g. "사실1 자산25 취약점0".
func (w WriteCounts) String() string {
	return fmt.Sprintf("사실%d 자산%d 취약점%d", w.Facts, w.Assets, w.Findings)
}

// Writes reports what this run wrote back, split by node kind (so the engine can
// tell "explored but persisted nothing" apart from a completed intent, and log an
// honest breakdown instead of calling assets/findings "facts").
func (t *ToolSet) Writes() WriteCounts { return t.writes }

func NewToolSet(ts *db.ExplorationStore, worker string) *ToolSet {
	return &ToolSet{ts: ts, worker: worker}
}

// SetTaskID sets the PG task id on this ToolSet so that report_finding can
// dual-write to the standalone findings table (which survives task deletion).
func (t *ToolSet) SetTaskID(id int64) { t.taskID = id }

// SetCoverageEnabled records whether this task has the asset-coverage feature on
// (default enabled). Passing false makes graphOverviewData omit the coverage block
// and DropCoverageTools filter the two coverage-only tools out of the agent's tool
// list. It does NOT stop scope accumulation: insertAssets' auto-scope hook runs
// either way, because task_scope is the task's range boundary (the filter basis for
// asset queries), not merely a coverage denominator.
func (t *ToolSet) SetCoverageEnabled(enabled bool) { t.coverageDisabled = !enabled }

// CoverageDisabled reports whether the coverage feature is off for this task.
func (t *ToolSet) CoverageDisabled() bool { return t.coverageDisabled }

// coverageOnlyTools are the LLM tools that only make sense when asset coverage is
// on. When the feature is off they are filtered out of the agent's tool list so
// they neither pollute the prompt nor let the model build a disabled denominator.
// add_task_scope is deliberately NOT here: task_scope is the task's range boundary
// (the filter basis for asset queries), not merely a coverage denominator, so the
// agents that own 범위 정의 keep it either way — in lockstep with insertAssets'
// auto-scope hook, which also runs regardless of the switch.
var coverageOnlyTools = map[string]bool{"list_untested_assets": true}

// DropCoverageTools returns tools with the coverage-only ones removed when this
// task has the feature disabled; otherwise it returns tools unchanged.
func (t *ToolSet) DropCoverageTools(tools []actool.CoreTool) []actool.CoreTool {
	if !t.coverageDisabled {
		return tools
	}
	out := tools[:0:0]
	for _, tool := range tools {
		if coverageOnlyTools[tool.Name()] {
			continue
		}
		out = append(out, tool)
	}
	return out
}

// Cross-task reuse: exported accessors returning the per-task tool logic bound to
// THIS ToolSet's store. Host-side orchestration tools build a ToolSet for an
// arbitrary task, then Call these — so cross-task reads/hint reuse the exact
// same logic as the in-task tools. (readTool ignores ToolContext, so Call(…,nil)
// is safe; add_hint is a writeTool but also doesn't deref the context here.)
func (t *ToolSet) GraphOverviewTool() actool.CoreTool      { return t.graphOverview() }
func (t *ToolSet) ListFindingsTool() actool.CoreTool       { return t.listFindings() }
func (t *ToolSet) GetWorkerTraceTool() actool.CoreTool     { return t.getWorkerTrace() }
func (t *ToolSet) ListWorkerTracesTool() actool.CoreTool   { return t.listWorkerTraces() }
func (t *ToolSet) SearchWorkerTracesTool() actool.CoreTool { return t.searchAllWorkerTraces() }
func (t *ToolSet) NodeDetailTool() actool.CoreTool         { return t.nodeDetail() }
func (t *ToolSet) AddHintTool() actool.CoreTool            { return t.addHint() }

// SetEnrich wires the async enrichment engine (DNS/HTTP auto-completion).
func (t *ToolSet) SetEnrich(e EnrichTrigger) { t.enrich = e }

// SetOwnerNode sets the exploration node that writes anchor to (worker: its
// intent node; planner/main: the begin root). Assets created/referenced while
// ownerNode is set are anchored to it as lineage (not visibility).
func (t *ToolSet) SetOwnerNode(id int64) { t.ownerNode = id }

// anchorOwner records a lineage edge from this run's owner node to an asset
// (no-op if unset). Provenance only — the asset graph is global and shared, so
// this no longer affects which assets a task can read.
func (t *ToolSet) anchorOwner(assetID int64) {
	if t.ts != nil && t.ownerNode > 0 && assetID > 0 {
		_ = t.ts.Anchor(t.ownerNode, assetID)
	}
}

// pid parses an id that may arrive as a JSON number or string ("" / 0 → 0).
func pid(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		v, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		return v
	}
	return 0
}

// pidList parses a list of ids (number|string), dropping zeros/invalids.
func pidList(raw []json.RawMessage) []int64 {
	var out []int64
	for _, r := range raw {
		if v := pid(r); v > 0 {
			out = append(out, v)
		}
	}
	return out
}

func obj(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}
func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func intp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}
func idp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

func readTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		ReadOnly:    func(json.RawMessage) bool { return true },
		Concurrent:  func(json.RawMessage) bool { return true },
		Permissions: func(context.Context, json.RawMessage, acperm.Context) acperm.Decision { return acperm.Allowed() },
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

func writeTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		Permissions: func(context.Context, json.RawMessage, acperm.Context) acperm.Decision { return acperm.Allowed() },
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

// readExpTool / writeExpTool build a domain tool whose handler dereferences the
// task-bound ExplorationStore. Two ToolSets carry a nil store: the catalog's
// seed-only shell (never called) and the server-level one behind buildDomainReg,
// which the tools table can bind to ANY agent — including ones that never run
// inside a task (auto/pentest/reporter/사용자 정의 에이전트/사이드 질문). Refusing there
// keeps a mis-bound tool a bad tool call; without the guard it was a nil deref,
// and tool handlers run on the harness's own goroutine, so the panic is out of
// reach of every recover() in the server and kills the whole process.
func (t *ToolSet) readExpTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return readTool(name, desc, schema, t.needExploration(name, run))
}

func (t *ToolSet) writeExpTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return writeTool(name, desc, schema, t.needExploration(name, run))
}

// needExploration wraps a handler so it only runs with an exploration store.
// Tools that degrade more usefully than "unavailable" (report_finding points at
// add_task_hint, set_goals/set_constraints at the task itself) keep their own
// bespoke guard instead.
func (t *ToolSet) needExploration(name string, run func(context.Context, json.RawMessage) (actool.Result, error)) func(context.Context, json.RawMessage) (actool.Result, error) {
	return func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
		if t.ts == nil {
			return actool.Errorf(name + " 은(는) 작업 컨텍스트(탐색 그래프)가 필요합니다. 현재 에이전트가 작업 안에서 실행되지 않아 작업의 탐색 그래프를 가져올 수 없으므로 이 도구를 사용할 수 없습니다. 작업 안에서 사용하거나, task_id를 지정하는 교차 작업 읽기 도구(get_task_node_detail / list_task_findings / get_task_graph 등)를 사용하세요."), nil
		}
		return run(ctx, in)
	}
}

func jsonResult(v any) (actool.Result, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return actool.Errorf(err.Error()), nil
	}
	return actool.Text(string(b)), nil
}

// --- read tools (planner + worker) ---

func (t *ToolSet) graphOverview() actool.CoreTool {
	return t.readExpTool("graph_overview",
		"(탐색 체인 그래프) 탐색 상황 증류 요약: 자산 수, 인터페이스가 없는 사이트, frontier, 취약점, hints(사람/메인 에이전트가 add_hint로 건 전략 힌트, 의도 생성 시 반드시 반영). 계획 수립 시 가장 먼저 호출하세요.",
		obj(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			return jsonResult(t.graphOverviewData())
		})
}

// graphOverviewData computes the distilled situational snapshot shared by the
// graph_overview tool and the planner's wake-up prompt (which pre-injects it so
// the model needn't spend a turn calling the tool — every plan round starts with
// an empty context and always needs this first).
func (t *ToolSet) graphOverviewData() map[string]any {
	out := map[string]any{}
	// goals summary folded in so the planner needn't call list_goals each round.
	goals, _ := t.ts.ListByKind(db.KindGoal, 100)
	gsum := make([]map[string]any, 0, len(goals))
	for _, g := range goals {
		var p map[string]any
		_ = json.Unmarshal(g.Payload, &p)
		gsum = append(gsum, map[string]any{"id": g.ID, "state": g.State, "text": p["text"]})
	}
	out["goals"] = gsum
	// hints: 사람/메인 에이전트가 add_hint로 그래프에 건 전략 힌트이며; folded in so the
	// planner reads them every round when generating intents (그렇지 않으면 기록만 되고 읽히지 않음).
	hints, _ := t.ts.ListByKind(db.KindHint, 50)
	hsum := make([]map[string]any, 0, len(hints))
	for _, h := range hints {
		var p map[string]any
		_ = json.Unmarshal(h.Payload, &p)
		hint := map[string]any{"id": h.ID, "state": h.State, "text": p["text"]}
		if findingTrafficBindingEnabled() && p["traffic_refs"] != nil {
			hint["traffic_refs"] = p["traffic_refs"]
		}
		hsum = append(hsum, hint)
	}
	out["hints"] = hsum
	// lineage from the exploration edges: an intent's parents (what it
	// derived_from — possibly several facts combined) and its yields (the
	// facts/findings it produced). factFrom maps a fact → the intent that
	// produced it. This is the relationship layer the flat lists lacked.
	edges, _ := t.ts.Edges(5000)
	parentsOf := map[int64][]int64{}
	yieldsOf := map[int64][]int64{}
	factFrom := map[int64]int64{}
	for _, e := range edges {
		switch e.Rel {
		case db.RelDerivedFrom, db.RelSpawns: // upstream: derived_from (fact/finding/intent→intent) or spawns (origin fact→goal, legacy begin→intent)
			parentsOf[e.To] = append(parentsOf[e.To], e.From)
		case db.RelYields: // intent --yields--> fact/finding
			yieldsOf[e.From] = append(yieldsOf[e.From], e.To)
			factFrom[e.To] = e.From
		}
	}
	// cold-digest §6: members folded into an active digest are shown via cold_digests
	// (below), not the flat recent_* lists. `covered` maps member id → its digest id.
	// §6 render-time revival check: a covered member that has become hot again (a new
	// intent derived from it) must reappear this round — so `hidden` folds a member out
	// only when it is covered AND still cold.
	covered, _ := t.ts.CoveredMembers()
	// Render-time hot set (ancestor of a live intent / fact under a live intent).
	// §6 revival check: a covered member that revived (now hot) must NOT stay folded
	// — hidden() only folds a member out when it is covered AND still cold. Computed
	// every round (cheap for real graph sizes); nil map degrades safely.
	var hotAtRender map[int64]bool
	if cg, _, err := loadColdGraph(t.ts); err == nil {
		hotAtRender = cg.hotSet()
	}
	hidden := func(id int64) bool { _, c := covered[id]; return c && !hotAtRender[id] }
	const openIntentsCap = 30
	fr, _ := t.ts.Frontier(openIntentsCap) // priority DESC, id ASC — 우선순위가 가장 높은 앞 N 개; 실제 총수는 frontier_open 참조
	out["open_intents"] = compactIntents(fr, parentsOf, yieldsOf)
	all, _ := t.ts.ListByKind(db.KindIntent, 300)
	var running, recentDone []*db.Node
	for _, n := range all {
		switch n.State {
		case "running":
			running = append(running, n)
		case "done", "blocked", "exhausted":
			if hidden(n.ID) {
				continue // in a cold_digest and still cold — shown via cold_digests (§6.2)
			}
			recentDone = append(recentDone, n) // 최신순 (all은 id 내림차순); 접힌 항목은 제외하고, 출력 시 최신 N 개로 자름
		}
	}
	out["running_intents"] = compactIntents(running, parentsOf, yieldsOf)
	// done_intents_total: 종료된 의도(done/blocked/exhausted)의 총수이며, recent_done_intents와
	// 나란히 명명됨 — 후자는 그 최신 윈도우만 잘라 보여주는 뷰일 뿐이다. 두 키를 나란히 두면 스스로 설명된다: "보이는 것은 N/총수".
	// planner가 중복을 판단할 때 "표시되지 않음"을 "한 번도 배정되지 않음"으로 착각하지 않도록, 프롬프트에 별도 설명을 넣을 필요가 없다.
	if dt, err := t.ts.CountFinishedIntents(); err == nil {
		out["done_intents_total"] = dt
	}
	// frontier_open: 열린 의도의 실제 총수 (open_intents는 그중 우선순위가 가장 높은 앞 N 개만 잘라 보여주는 뷰이다).
	if fo, err := t.ts.CountOpenIntents(); err == nil {
		out["frontier_open"] = fo
	} else {
		out["frontier_open"] = len(fr)
	}
	// findings (confirmed vulns) and facts (worker exploration results) are
	// now distinct node kinds. recent_facts surfaces fact summaries (esp.
	// negative results) so the planner sees them in one call; full content
	// via node_detail(id).
	vulnNodes, _ := t.ts.ListByKind(db.KindFinding, 1000)
	factNodes, _ := t.ts.ListByKind(db.KindFact, 1000) // newest first
	out["findings_total"] = len(vulnNodes)             // 확인된 취약점 총수 (목표 판정에 사용); 상세는 finding_list 참조 (최신 윈도우)
	out["facts"] = len(factNodes)                      // 탐색 사실/결론 수 (부정 결론 포함)
	// findings는 작업에서 가장 가치가 높은 산출물이므로 → 개요에는 최신 윈도우(≤10개, vulnNodes는 이미 id 내림차순이라 최신이 앞)를 담고,
	// planner가 매 라운드 목표를 판단할 때 최근 확인된 취약점을 한눈에 볼 수 있게 하고; 전체/더 이전 항목은 list_findings로 가져온다.
	// 각 항목은 {id, summary, from_intent?}만 남긴다: from_intent는 이 취약점을 낳은 의도다.
	// evidence/assets/vulnclass/severity/state 등은 여전히 list_findings / node_detail(id)로 가져올 수 있다.
	const findingListCap = 10
	findingList := make([]map[string]any, 0, findingListCap)
	for _, n := range vulnNodes {
		if len(findingList) >= findingListCap {
			break
		}
		var fp map[string]any
		_ = json.Unmarshal(n.Payload, &fp)
		m := map[string]any{"id": n.ID, "summary": fp["summary"]}
		if from := factFrom[n.ID]; from > 0 {
			m["from_intent"] = from // 이 취약점을 낳은 의도
		}
		findingList = append(findingList, m)
	}
	out["finding_list"] = findingList
	// recent_facts: 접히지 않은 사실 중 최신 윈도우(≤N, factNodes는 id 내림차순이라 최신이 앞). 이미 digest에 접혔지만 여전히 식은(hidden) 항목은
	// cold_digests로 나오므로 여기서는 중복하지 않는다. 각 항목은 {id, summary, from_intent?,
	// confidence?}; evidence 등 상세는 node_detail(id)로 본다. 더 이전 항목은 list_facts로 넘겨 본다.
	const recentFactsCap = 20
	recentFacts := make([]map[string]any, 0, recentFactsCap)
	for _, n := range factNodes {
		if len(recentFacts) >= recentFactsCap {
			break
		}
		if hidden(n.ID) {
			continue // digest에 접혀 여전히 식은 항목 — cold_digests 참조
		}
		m := compactNode(n)
		if from := factFrom[n.ID]; from > 0 {
			m["from_intent"] = from // 이 사실을 낳은 의도
		}
		// confidence는 개요에 포함한다: 계획자가 어떤 결론이 inferred(추론)일 뿐인지 한눈에 보게 하기 위함이다 (특히 부정 결론은
		// 확정 사실로 취급하지 않도록); evidence는 길어서 node_detail(id)에 맡긴다.
		var fp map[string]any
		if json.Unmarshal(n.Payload, &fp) == nil {
			if c, ok := fp["confidence"].(string); ok && c != "" {
				m["confidence"] = c
			}
		}
		recentFacts = append(recentFacts, m)
	}
	out["recent_facts"] = recentFacts
	// recent_done_intents: 접히지 않은 종료 의도 중 최신 윈도우(≤N, recentDone은 이미 id 내림차순).
	// 더 이전 항목은 done_intents_total 개수 + node_detail(id)로 확인한다.
	const recentDoneCap = 12
	if len(recentDone) > recentDoneCap {
		recentDone = recentDone[:recentDoneCap]
	}
	out["recent_done_intents"] = compactIntents(recentDone, parentsOf, yieldsOf)
	// cold-digest §6.1: 접힌 콜드 영역의 digest body를 최신 구성원 시각 내림차순으로 앞 N 개 취함; 잘린 더 오래된 digest
	// 는 id만 제공한다 (expand_digest로 여전히 펼칠 수 있음). 콜드 영역의 유일한 출구가 무한정 길어지는 것을 막기 위함이다.
	const coldDigestsCap = 15
	if cds, more := coldDigestsRecent(t.ts, coldDigestsCap); len(cds) > 0 {
		out["cold_digests"] = cds // [{id, body, member_count}] — body를 직접 읽음 (§6.1)
		if len(more) > 0 {
			out["cold_digests_more"] = more // 잘린 더 오래된 digest의 id; expand_digest(id)로 펼친다
		}
	}
	// the original task (root) so the planner always has it, not just the
	// decomposed goals.
	if description, goal, err := t.ts.Root(); err == nil {
		out["task"] = map[string]any{"description": description, "goal": goal}
	}
	// Direct source tasks are a live, read-only blackboard view. Keep their
	// summaries in a separate field so their intents never enter this task's
	// frontier or get mistaken for locally claimable work.
	out["related_tasks"] = t.relatedTaskOverviews()
	// coverage: 대략적인 자산 테스트 커버리지 참고값 — 범위(task_scope) 내 자산 중 fact가 닿은
	// 비율 + by_type(유형별 총수/테스트 완료). 테스트되지 않은 구체적 자산은 에이전트가 필요에 따라 list_untested_assets를 호출해 직접 판단한다. 작업 컨텍스트에서만 제공된다.
	// 자산 커버리지 기능이 꺼져 있을 때(coverageDisabled): host_count(대상 호스트 수 정보)만 남기고,
	// denominator/tested/pct/by_type/note 등 커버리지 지표는 버려서 컨텍스트를 오염시키지 않고,
	// 이미 숨겨진 add_task_scope/list_untested_assets를 유도하지 않도록 한다.
	if t.as != nil && t.ts != nil && t.taskID > 0 {
		{
			m := map[string]any{}
			if !t.coverageDisabled {
				if cov, err := t.as.TaskCoverageWithSources(t.taskID); err == nil {
					m["denominator"] = cov.Denominator
					m["tested"] = cov.Tested
					m["by_type"] = cov.ByType
					m["note"] = "coverage 자산 테스트 커버리지(인터페이스 등 각종 관련 자산 포함), 대략적 추정치이며 참고용: 현재 작업과 직접 연관된 작업의 scope, fact 앵커를 포함; 연관 scope는 읽기 전용. 컨테이너형 자산/대량 열거는 값을 낮게 만들 수 있으니 이 값으로 테스트가 끝났다고 단정하지 말 것; add_task_scope로 본 작업 범위를 보강하고, list_untested_assets로 미테스트 자산을 확인할 수 있음 [보통 list_untested_assets는 호출하지 않아도 되며, 작업 진행에 따라 진행하면 됨];"
					if cov.Denominator == 0 {
						m["pct"] = nil
						m["status"] = "범위 미고정"
					} else {
						m["pct"] = cov.Pct
					}
				}
			}
			if hosts, err := t.as.HostsByTaskWithSources(t.taskID); err == nil {
				// 호스트 총수만 제공하고, host 목록을 graph_overview에 그대로 펼치지 않는다 (대규모 범위 작업에서는 매 라운드 반복 전달되는 많은 문자열이 되어 계획 판단에 가치가 낮음);
				// 구체적인 호스트는 필요할 때 list_assets로 조회한다.
				m["host_count"] = len(hosts)
			}
			if len(m) > 0 {
				out["coverage"] = m
			}
		}
	}
	return out
}

func inheritedMap(m map[string]any, sourceTaskID int64) map[string]any {
	m["source_task_id"] = sourceTaskID
	m["inherited"] = true
	return m
}

const (
	relatedOverviewTotalTextRunes      = 48_000
	relatedOverviewMaxTextPerSource    = 8_000
	relatedOverviewMaxGoalsPerSource   = 8
	relatedOverviewMaxHintsPerSource   = 6
	relatedOverviewMaxFactsPerSource   = 12
	relatedOverviewMaxFindingsPerTask  = 6
	relatedOverviewMaxIntentsPerTask   = 8
	relatedOverviewMaxScopePerSource   = 12
	relatedOverviewMaxDigestsPerSource = 6
)

// overviewTextBudget bounds inherited prompt text while preserving a fair slice
// for every direct source. Full evidence remains available through the on-demand
// read tools, so truncation here does not discard persisted blackboard data.
type overviewTextBudget struct {
	remaining int
	truncated bool
}

func relatedOverviewBudgetForSources(sourceCount int) int {
	if sourceCount <= 0 {
		return 0
	}
	if sourceCount > db.MaxTaskSourceCount {
		sourceCount = db.MaxTaskSourceCount
	}
	perSource := relatedOverviewTotalTextRunes / sourceCount
	if perSource > relatedOverviewMaxTextPerSource {
		perSource = relatedOverviewMaxTextPerSource
	}
	return perSource
}

func (b *overviewTextBudget) take(value any, fieldLimit int) string {
	var text string
	switch value := value.(type) {
	case string:
		text = strings.TrimSpace(value)
	case nil:
		return ""
	default:
		text = strings.TrimSpace(fmt.Sprint(value))
	}
	if text == "" {
		return ""
	}
	if b.remaining <= 0 || fieldLimit <= 0 {
		b.truncated = true
		return ""
	}
	runes := []rune(text)
	limit := fieldLimit
	if limit > b.remaining {
		limit = b.remaining
	}
	if len(runes) > limit {
		b.truncated = true
		if limit == 1 {
			text = "…"
		} else {
			text = string(runes[:limit-1]) + "…"
		}
		runes = []rune(text)
	}
	b.remaining -= len(runes)
	return text
}

func recentTerminalIntents(store *db.ExplorationStore, limit int) []*db.Node {
	if limit <= 0 {
		return []*db.Node{}
	}
	const batch = 300
	cursor := int64(0)
	out := make([]*db.Node, 0, limit)
	for len(out) < limit {
		page, more, err := store.ListByKindPage(db.KindIntent, cursor, batch)
		if err != nil || len(page) == 0 {
			break
		}
		for _, intent := range page {
			switch intent.State {
			case "done", "blocked", "exhausted", "stopped":
				out = append(out, intent)
			}
			if len(out) >= limit {
				break
			}
		}
		if !more {
			break
		}
		cursor = page[len(page)-1].ID
	}
	return out
}

// relatedTaskOverviews distills persistent blackboard state from direct source
// tasks. It intentionally reads each source's local store methods, never its own
// related sources, so inheritance is one level only.
func (t *ToolSet) relatedTaskOverviews() []map[string]any {
	sources, err := t.ts.DirectSourceStores()
	if err != nil {
		return []map[string]any{}
	}
	if len(sources) > db.MaxTaskSourceCount {
		sources = sources[:db.MaxTaskSourceCount]
	}
	perSourceTextBudget := relatedOverviewBudgetForSources(len(sources))
	out := make([]map[string]any, 0, len(sources))
	for _, source := range sources {
		ts := source.Store
		// §2 cross-task: render the source task's OWN folded view — fold out the
		// members it has already folded, and surface its cold_digests read-only.
		hidden := hiddenMembersFor(ts)
		budget := overviewTextBudget{remaining: perSourceTextBudget}
		item := map[string]any{
			"source_task_id": source.Task.TaskID,
			"inherited":      true,
			"task": map[string]any{
				"description": budget.take(source.Task.Description, 800),
				"goal":        budget.take(source.Task.Goal, 800),
				"status":      source.Task.Status,
			},
		}
		stats, statsErr := ts.Stats()

		edges, _ := ts.Edges(5000)
		parentsOf := map[int64][]int64{}
		yieldsOf := map[int64][]int64{}
		factFrom := map[int64]int64{}
		for _, edge := range edges {
			switch edge.Rel {
			case db.RelDerivedFrom, db.RelSpawns:
				parentsOf[edge.To] = append(parentsOf[edge.To], edge.From)
			case db.RelYields:
				yieldsOf[edge.From] = append(yieldsOf[edge.From], edge.To)
				factFrom[edge.To] = edge.From
			}
		}

		goals, _ := ts.ListByKind(db.KindGoal, relatedOverviewMaxGoalsPerSource)
		goalSummary := make([]map[string]any, 0, len(goals))
		for _, goal := range goals {
			var payload map[string]any
			_ = json.Unmarshal(goal.Payload, &payload)
			goalSummary = append(goalSummary, inheritedMap(map[string]any{
				"id": goal.ID, "state": goal.State, "text": budget.take(payload["text"], 400),
			}, source.Task.TaskID))
		}
		item["goals"] = goalSummary

		hints, _ := ts.ListByKind(db.KindHint, relatedOverviewMaxHintsPerSource)
		hintSummary := make([]map[string]any, 0, len(hints))
		for _, hint := range hints {
			var payload map[string]any
			_ = json.Unmarshal(hint.Payload, &payload)
			hintSummary = append(hintSummary, inheritedMap(map[string]any{
				"id": hint.ID, "state": hint.State, "text": budget.take(payload["text"], 400),
			}, source.Task.TaskID))
		}
		item["hints"] = hintSummary

		facts, _ := ts.ListByKind(db.KindFact, relatedOverviewMaxFactsPerSource)
		findings, _ := ts.ListByKind(db.KindFinding, relatedOverviewMaxFindingsPerTask)
		intentNodes, _ := ts.ListByKind(db.KindIntent, 300)
		terminalIntent := make(map[int64]bool, len(intentNodes))
		for _, intent := range intentNodes {
			terminalIntent[intent.ID] = inheritedIntentSummaryState(intent.State)
		}
		item["facts"] = len(facts)
		item["findings"] = len(findings)
		if statsErr == nil {
			item["facts"] = stats[db.KindFact]
			item["findings"] = stats[db.KindFinding]
			if stats[db.KindGoal] > len(goals) || stats[db.KindHint] > len(hints) ||
				stats[db.KindFact] > len(facts) || stats[db.KindFinding] > len(findings) {
				budget.truncated = true
			}
		}
		recentFindings := make([]map[string]any, 0, len(findings))
		for _, finding := range findings {
			entry := inheritedMap(compactFinding(finding), source.Task.TaskID)
			entry["summary"] = budget.take(entry["summary"], 400)
			recentFindings = append(recentFindings, entry)
		}
		item["recent_findings"] = recentFindings
		recentFacts := make([]map[string]any, 0, len(facts))
		for _, fact := range facts {
			if hidden(fact.ID) {
				continue // folded into this source's cold_digests — shown there (§2/§6.2)
			}
			m := inheritedMap(compactNode(fact), source.Task.TaskID)
			m["summary"] = budget.take(m["summary"], 400)
			if from := factFrom[fact.ID]; from > 0 && terminalIntent[from] {
				m["from_intent"] = from
			}
			var payload map[string]any
			if json.Unmarshal(fact.Payload, &payload) == nil {
				if confidence, ok := payload["confidence"].(string); ok && confidence != "" {
					m["confidence"] = confidence
				}
			}
			recentFacts = append(recentFacts, m)
		}
		item["recent_facts"] = recentFacts

		recentDoneRaw := recentTerminalIntents(ts, relatedOverviewMaxIntentsPerTask)
		recentDone := recentDoneRaw[:0] // in-place filter: drop this source's folded intents (§2)
		for _, intent := range recentDoneRaw {
			if hidden(intent.ID) {
				continue
			}
			recentDone = append(recentDone, intent)
		}
		for _, intent := range recentDone {
			intent.Inherited = true
			intent.SourceTaskID = source.Task.TaskID
		}
		intentResults := compactIntents(recentDone, parentsOf, yieldsOf)
		for i, intent := range recentDone {
			intentResults[i]["summary"] = budget.take(intentResults[i]["summary"], 400)
			acts, _, err := ts.ActivityPageForTerminalIntent(intent.ID, 0, 20)
			if err != nil {
				continue
			}
			var resultSummary, textFallback string
			for _, activity := range acts {
				switch activity.Kind {
				case "result":
					resultSummary = activity.Summary
				case "text":
					textFallback = activity.Summary
				}
			}
			if resultSummary == "" {
				resultSummary = textFallback
			}
			if resultSummary != "" {
				intentResults[i]["result_summary"] = budget.take(resultSummary, 800)
			}
		}
		item["recent_intent_results"] = intentResults
		// §2 cross-task: the source task's folded cold region, read-only, newest-member
		// first & capped like the current task's. Members (and overflow digests) are
		// resolvable via expand_digest(id)/node_detail(id), which search source tasks.
		if cds, more := coldDigestsRecent(ts, relatedOverviewMaxDigestsPerSource); len(cds) > 0 {
			for _, cd := range cds {
				cd["inherited"] = true
				cd["source_task_id"] = source.Task.TaskID
			}
			item["cold_digests"] = cds
			if len(more) > 0 {
				item["cold_digests_more"] = more // 잘린 더 오래된 digest의 id; expand_digest(id)로 펼친다
			}
		}
		if statsErr == nil {
			item["node_stats"] = stats
		}

		if t.as != nil {
			if scopeRows, err := t.as.ListTaskScope(source.Task.TaskID); err == nil && len(scopeRows) > 0 {
				scopeCount := len(scopeRows)
				if len(scopeRows) > relatedOverviewMaxScopePerSource {
					scopeRows = scopeRows[:relatedOverviewMaxScopePerSource]
					budget.truncated = true
				}
				scope := make([]map[string]any, 0, len(scopeRows))
				for _, row := range scopeRows {
					entry := map[string]any{"kind": row.Kind, "source": budget.take(row.Source, 300)}
					switch {
					case row.Domain != "":
						entry["value"] = budget.take(row.Domain, 400)
					case row.Net != "":
						entry["value"] = budget.take(row.Net, 400)
					case row.Value != "":
						entry["value"] = budget.take(row.Value, 400)
					case row.CompanyID != nil:
						entry["company_id"] = *row.CompanyID
					}
					scope = append(scope, entry)
				}
				item["asset_scope"] = scope
				item["asset_scope_count"] = scopeCount
			}
			if coverage, err := t.as.TaskCoverage(source.Task.TaskID, source.Task.ExplorationID); err == nil {
				item["asset_coverage"] = map[string]any{
					"denominator": coverage.Denominator,
					"tested":      coverage.Tested,
					"pct":         coverage.Pct,
					"by_type":     coverage.ByType,
				}
			}
		}
		if budget.truncated {
			item["summary_truncated"] = true
		}
		out = append(out, item)
	}
	return out
}

func inheritedIntentSummaryState(state string) bool {
	switch state {
	case "done", "blocked", "exhausted", "stopped":
		return true
	default:
		return false
	}
}

// compactNode distills any exploration node to id + summary + state, dropping the
// big detail/evidence (fetch that on demand via node_detail).
func compactNode(n *db.Node) map[string]any {
	var p map[string]any
	_ = json.Unmarshal(n.Payload, &p)
	m := map[string]any{"id": n.ID, "state": n.State, "summary": p["summary"]}
	if n.Inherited {
		inheritedMap(m, n.SourceTaskID)
	}
	return m
}

// compactFinding is compactNode plus the vuln-specific vulnclass/severity.
func compactFinding(n *db.Node) map[string]any {
	var p map[string]any
	_ = json.Unmarshal(n.Payload, &p)
	m := map[string]any{"id": n.ID, "state": n.State, "summary": p["summary"]}
	if n.Inherited {
		inheritedMap(m, n.SourceTaskID)
	}
	if vc, ok := p["vulnclass"]; ok && vc != nil && vc != "" {
		m["vulnclass"] = vc
	}
	if sv, ok := p["severity"]; ok && sv != nil && sv != "" {
		m["severity"] = sv
	}
	return m
}

func (t *ToolSet) listFindings() actool.CoreTool {
	return t.readExpTool("list_findings", "본 작업 및 직접 연관된 작업의 【확인된 취약점】 목록(간략: id+task_id+intent_id+vulnclass+severity+요약+상태). 연관 작업 항목은 source_task_id/inherited=true를 달고 읽기 전용이다. 여기에는 취약점만 포함되며, 일반 탐색 사실은 list_facts, 상세는 node_detail(id)를 사용한다.",
		obj(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			f, _ := t.ts.ListByKindWithSources(db.KindFinding, 500)
			if err := t.ts.PopulateFindingTrafficIDs(f); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			intentOf, _ := t.ts.FindingIntentsWithSources() // finding id -> 해당 취약점을 낳은 intent id
			taskID := t.taskID
			if taskID <= 0 {
				taskID, _ = t.ts.TaskID()
			}
			out := make([]map[string]any, 0, len(f))
			for _, n := range f {
				m := compactFinding(n)
				if n.FindingID > 0 {
					m["finding_id"], m["finding_node_id"], m["traffic_count"] = n.FindingID, n.ID, n.TrafficCount
				}
				if n.Inherited {
					m["task_id"] = n.SourceTaskID
				} else {
					m["task_id"] = taskID
				}
				if iid, ok := intentOf[n.ID]; ok {
					m["intent_id"] = iid
				}
				out = append(out, m)
			}
			return jsonResult(out)
		})
}

// factsPageSize is the default page size for list_facts. Facts pile up on long
// tasks; returning all of them at once (the old behaviour) could blow up the
// context, so default to the newest page and let the agent page/filter for more.
const factsPageSize = 20

func (t *ToolSet) listFacts() actool.CoreTool {
	return t.readExpTool("list_facts", "본 작업 및 직접 연관된 작업의 【탐색 사실/결론】을 페이지 단위로 나열한다. 최신순(간략: id+요약+상태, 요약이 길면 잘리며 전문은 node_detail(id)로 확인). 매개변수는 모두 선택: limit(기본 20, 최대 100), before(커서, 이전 페이지의 next_before를 전달하면 더 오래된 페이지를 가져옴; 생략/0=최신 페이지), q(요약 키워드 필터). 반환값은 {facts, total, has_more, next_before}: total은 필터 적용 후 총수이며, has_more=true이면 next_before로 계속 넘긴다. 연관 작업 항목은 source_task_id/inherited=true를 달고 읽기 전용이다. 취약점은 list_findings를 사용한다.",
		obj(map[string]any{
			"limit":  intp("반환 개수, 기본 20, 최대 100"),
			"before": intp("페이지 커서: 이 값보다 id가 작은 더 오래된 사실만 반환; 생략 또는 0 = 최신 페이지"),
			"q":      str("사실 요약의 키워드로 필터링 (대소문자 구분 없음); 생략 = 필터링 안 함"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Limit  int    `json:"limit"`
				Before int64  `json:"before"`
				Q      string `json:"q"`
			}
			_ = json.Unmarshal(in, &a)
			limit := a.Limit
			if limit <= 0 {
				limit = factsPageSize
			}
			if limit > 100 {
				limit = 100
			}
			f, hasMore, total, err := t.ts.ListByKindPageWithSources(db.KindFact, a.Before, limit, strings.TrimSpace(a.Q))
			if err != nil {
				return actool.Result{}, err
			}
			out := make([]map[string]any, 0, len(f))
			for _, n := range f {
				out = append(out, compactFact(n))
			}
			res := map[string]any{"facts": out, "total": total, "has_more": hasMore}
			if hasMore && len(f) > 0 {
				res["next_before"] = f[len(f)-1].ID // 이 값을 전달하면 다음(더 오래된) 페이지를 가져옴
			}
			return jsonResult(res)
		})
}

// factSummaryMax caps a fact summary in list_facts output. Facts carry one-line
// conclusions, but nothing enforces brevity; a runaway summary must not bloat a
// whole page. Full text stays available via node_detail(id).
const factSummaryMax = 160

// compactFact is compactNode with the summary rune-capped for list_facts, so a
// page of facts stays bounded regardless of how long any single summary grew.
func compactFact(n *db.Node) map[string]any {
	m := compactNode(n)
	if s, ok := m["summary"].(string); ok && len([]rune(s)) > factSummaryMax {
		m["summary"] = string([]rune(s)[:factSummaryMax]) + "…"
		m["summary_truncated"] = true
	}
	return m
}

func (t *ToolSet) nodeDetail() actool.CoreTool {
	return t.readExpTool("node_detail", "id로 본 작업 또는 직접 연관된 작업의 【탐색 그래프 노드】 전체 내용을 가져온다. 상속된 노드는 source_task_id/inherited=true를 달고 읽기 전용이다. list_facts/list_findings/graph_overview가 반환한 탐색 노드 id만 사용할 것; 자산은 list_assets/asset_neighbors를 사용한다.",
		obj(map[string]any{"id": idp("탐색 그래프 노드 id(자산 id 아님)")}, "id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				ID json.RawMessage `json:"id"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.ID)
			if id <= 0 {
				return actool.Errorf("id는 필수입니다"), nil
			}
			n, err := t.ts.GetNodeWithSources(id)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if n == nil {
				return actool.Errorf(fmt.Sprintf("탐색 노드 %d을(를) 찾을 수 없습니다. 자산을 조회하려는 경우 list_assets / asset_neighbors를 사용하세요 (자산과 탐색 노드는 서로 다른 id 공간이므로, 자산 id는 node_detail에 전달할 수 없습니다).", id)), nil
			}
			if err := t.ts.PopulateFindingTrafficIDs([]*db.Node{n}); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return jsonResult(n) // full payload incl. detail / evidence, plus explicit finding IDs
		})
}

// --- planner write tools ---

// intentItem은 add_intent의 배치/단건 입력 중 하나의 탐색 방향이다.
type intentItem struct {
	Summary   string            `json:"summary"`
	AssetIDs  []json.RawMessage `json:"asset_ids"`
	ParentIDs []json.RawMessage `json:"parent_ids"`
	Priority  int               `json:"priority"`
}

// addOneIntent는 의도 노드 하나를 생성하고 상위 계보를 연결한 뒤 id를 반환한다.
// 제약: 의도는 확인된 지식에만 앵커링할 수 있다 — 각 parent_id는 이미 존재하는 fact/finding
// 노드여야 한다 (다른 의도/목표/힌트에 매달 수 없음). 최상위의 완전히 새로운 방향은 parent_ids를 비워 두면 되며, 이 경우 origin fact에 연결된다.
// 이렇게 하여 "모든 의도는 fact 노드에 연결되며, 근거 없이 계획하지 않고 발견에 따라 도출된다"는 점이 생성 경로에서부터 강제된다.
func (t *ToolSet) addOneIntent(it intentItem) (int64, error) {
	if strings.TrimSpace(it.Summary) == "" {
		return 0, fmt.Errorf("summary는 비울 수 없습니다")
	}
	// 먼저 앵커를 검증한다 (노드를 만들기 전에, 잘못된 앵커로 고아 의도가 남지 않도록 하기 위함).
	parents := pidList(it.ParentIDs)
	for _, pidv := range parents {
		n, err := t.ts.GetNodeWithSources(pidv)
		if err != nil || n == nil {
			return 0, fmt.Errorf("parent_id %d가 본 작업 또는 직접 연관된 작업에 존재하지 않습니다: parent_ids는 이미 존재하는 【사실(fact)/취약점(finding)】 노드 id여야 하며, 최상위의 완전히 새로운 방향이라면 parent_ids를 비워 두세요", pidv)
		}
		if n.Kind != db.KindFact && n.Kind != db.KindFinding {
			return 0, fmt.Errorf("parent_id %d는 %q 노드이므로 의도의 앵커로 쓸 수 없습니다: 의도는 확인된 【사실(fact)/취약점(finding)】에만 앵커링할 수 있으며, 의도/목표/힌트에 매달 수 없습니다. 최상위의 완전히 새로운 방향이라면 parent_ids를 비워 두세요", pidv, n.Kind)
		}
	}
	priority := it.Priority
	if priority == 0 {
		priority = 5
	}
	anchors := pidList(it.AssetIDs)
	// 자산 차단: 의도에 연결된 자산이 시스템 자산 차단 규칙에 걸리면 해당 의도의 하달을 금지한다.
	if t.as != nil && len(anchors) > 0 {
		hits, err := t.as.CheckAssetsIntercept(t.taskID, anchors)
		if err != nil {
			return 0, fmt.Errorf("자산 차단 검증 실패: %w", err)
		}
		if len(hits) > 0 {
			var b strings.Builder
			fmt.Fprintf(&b, "의도 「%s」에 연결된 자산이 테스트 범위 검증을 통과하지 못했습니다. 관련 자산에 대한 테스트를 중단하세요:", it.Summary)
			for _, h := range hits {
				fmt.Fprintf(&b, "\n - %s", h.Describe())
			}
			return 0, fmt.Errorf("%s", b.String())
		}
	}
	payload := map[string]any{"summary": it.Summary}
	if len(anchors) > 0 {
		payload["asset_ids"] = anchors
	}
	id, err := t.ts.AddIntent(payload, priority, anchors, "planner")
	if err != nil {
		return 0, err
	}
	// upstream lineage: link each (validated) fact/finding parent → this intent, so
	// "multiple facts combine into one new intent" is expressible.
	for _, parent := range parents {
		_ = t.ts.Link(parent, db.RelDerivedFrom, id)
	}
	// a top-level intent (no explicit parent) connects to the origin fact, so every
	// intent still traces back to a fact node — at task start the only fact is the
	// origin, and the first intents derive from it.
	if len(parents) == 0 {
		if origin, _ := t.ts.OriginFactID(); origin > 0 {
			_ = t.ts.Link(origin, db.RelDerivedFrom, id)
		}
	}
	return id, nil
}

func (t *ToolSet) addIntent() actool.CoreTool {
	return t.writeExpTool("add_intent", "【탐색 방향】을 생성해 frontier에 기록하고 탐색 체인에 연결한다. 의도는 열린 탐색 방향이며 고정된 유형이 아니다 — summary 한 문장으로 무엇을 탐색/검증/악용할지 자유롭게 기술한다.\n"+
		"★우선 배치: 한 라운드에서 고른 여러 새 방향을 intents 배열에 담아 한 번에 제출하세요 (건별 호출보다 왕복이 적음). ids 배열을 반환하며 intents와 길이와 순서가 같습니다 (실패 항목은 id=0, 상세는 errors 참조). 단건이면 intents를 생략하고 최상위 summary를 주세요.",
		obj(map[string]any{
			"intents":    map[string]any{"type": "array", "description": "【우선 사용】 추가할 탐색 방향 배열이며 순서대로 처리한다. 각 요소의 필드는 아래 최상위 필드(summary/asset_ids/parent_ids/priority)와 같다. ids는 이 배열과 길이와 순서가 같게 반환된다.", "items": map[string]any{"type": "object"}},
			"summary":    str("[단건] 이 탐색 방향을 한 문장으로 기술: 무엇을 하는지 + 왜 하는지. 방향이 명확히 쓰여 있으면 충분하며, 자산 id에 의존하지 않는다."),
			"asset_ids":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "이 방향에서 테스트/공격할 【대상 자산 id】(**가능한 한 전달**, 0/1/여러 개; list_assets가 반환한 자산 id이며 탐색 노드 id가 아님): 이 탐색 방향이 겨냥하는 자산(사이트/인터페이스/파라미터/호스트 등). 방향이 특정 자산을 중심으로 한다면 반드시 전달하세요 — 이는 「이 탐색이 어떤 대상을 치는지」를 나타내는 구조화된 표식으로, 커버리지 중복 제거와 의도의 자산 체인 연결에 쓰입니다. 순수한 전역 정찰이라 구체적인 대상 자산이 전혀 없을 때만 비워 두세요."},
			"parent_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "상위 앵커 id (선택, 0/1/여러 개): 이 방향이 어떤 【확인된 사실(fact)/취약점(finding)】을 종합해 도출되었는지. **이미 존재하는 fact/finding 노드 id만 입력할 수 있으며, 의도/목표/힌트 id는 입력할 수 없습니다** — 의도는 확인된 지식에 앵커링되어야 하며, 근거 없이 계획하지 않고 발견에 따라 도출되어야 합니다. 여러 사실이 함께 새 의도 하나를 낳는다면 여러 개를 전달하세요; 최상위의 완전히 새로운 정찰 방향이라면 비워 두세요 (작업 시작점 origin fact에 자동으로 연결됨)."},
			"priority":   intp("우선순위 0-10, 기본값 5"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Intents    []intentItem `json:"intents"`
				intentItem              // 단건 모드: 최상위 summary/asset_ids/parent_ids/priority
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Intents) > 0
			items := a.Intents
			if !batch {
				items = []intentItem{a.intentItem}
			}

			ids := make([]int64, len(items))
			errs := map[string]string{}
			createdAny := false
			for i, it := range items {
				id, err := t.addOneIntent(it)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
				createdAny = true
			}

			// 사람이 메인 에이전트를 통해 의도를 직접 투입한 경우 → 작업이 이미 done 상태이면(열린 목표가 없는 goalless 분기), 그것을
			// running으로 되돌려야 worker가 이 의도를 가져가 실행할 수 있다. resumeTask는 메인 에이전트의 Chat에서만 연결되며
			// (SetResumeTask); planner의 ToolSet은 nil이므로, planner가 스스로 add_intent를 호출할 때 이 구간은
			// no-op이며 정상적인 의도 생성에 영향을 주지 않는다. 의도 노드는 위에서 이미 생성되었고(open), 부활 시 잘못 고갈된 것으로 판단되지 않는다.
			if createdAny && t.resumeTask != nil {
				t.resumeTask()
			}

			if !batch { // 단건: 기존 반환값 유지
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				return actool.Text(fmt.Sprintf("intent created: %d", ids[0])), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}

func (t *ToolSet) listGoals() actool.CoreTool {
	return t.readExpTool("list_goals", "본 작업의 목표 노드와 상태(open/met)를 나열하며, 달성 여부를 판단하는 데 사용한다.",
		obj(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			g, _ := t.ts.ListByKind(db.KindGoal, 100)
			return jsonResult(g)
		})
}

func (t *ToolSet) proveGoal() actool.CoreTool {
	return t.writeExpTool("prove_goal", "어떤 취약점/사실이 특정 목표의 달성을 증명한다고 판단되면 호출한다: 증거 노드를 목표 노드에 연결하고 목표를 met으로 표시한다.",
		obj(map[string]any{
			"goal_id":     idp("목표 노드 id"),
			"evidence_id": idp("이를 증명하는 취약점/사실 노드 id"),
			"reason":      str("이 증거가 해당 목표를 충족하는 이유"),
		}, "goal_id", "evidence_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				GoalID     json.RawMessage `json:"goal_id"`
				EvidenceID json.RawMessage `json:"evidence_id"`
				Reason     string          `json:"reason"`
			}
			_ = json.Unmarshal(in, &a)
			goal, ev := pid(a.GoalID), pid(a.EvidenceID)
			if goal == 0 || ev == 0 {
				return actool.Errorf("goal_id와 evidence_id는 필수입니다"), nil
			}
			goalNode, err := t.ts.GetNode(goal)
			if err != nil || goalNode == nil || goalNode.Kind != db.KindGoal {
				return actool.Errorf("goal_id는 본 작업의 목표 노드여야 합니다 (연관 작업의 목표는 읽기 전용)"), nil
			}
			evidenceNode, err := t.ts.GetNodeWithSources(ev)
			if err != nil || evidenceNode == nil || (evidenceNode.Kind != db.KindFact && evidenceNode.Kind != db.KindFinding) {
				return actool.Errorf("evidence_id는 본 작업 또는 직접 연관된 작업의 사실/취약점 노드여야 합니다"), nil
			}
			_ = t.ts.Link(ev, db.RelProves, goal)
			_ = t.ts.SetNodeState(goal, "met")
			// 목표 하나를 met으로 표시할 때마다 본 작업의 【모든 목표】가 met인지 확인하며, 그렇다면 자동으로
			// 작업 완료로 판정한다(GoalMet 설정). 모델이 goal_met을 명시적으로 호출할 필요는 없다.
			if goals, err := t.ts.ListByKind(db.KindGoal, 1000); err == nil && len(goals) > 0 {
				allMet := true
				for _, g := range goals {
					if g.State != "met" {
						allMet = false
						break
					}
				}
				if allMet {
					t.GoalMet = true
					t.Reason = fmt.Sprintf("모든 %d개 목표가 met입니다 (마지막으로 goal %d이(가) 트리거함)", len(goals), goal)
					return actool.Text(fmt.Sprintf("goal %d marked met; 본 작업의 모든 목표가 달성되어 작업이 자동으로 완료 판정되었습니다", goal)), nil
				}
			}
			return actool.Text(fmt.Sprintf("goal %d marked met", goal)), nil
		})
}

func (t *ToolSet) goalMet() actool.CoreTool {
	return writeTool("goal_met", "【즉시 작업 전체를 종료】 — 작업의 【모든 목표가 실제로 달성되어 전체가 마무리되었음】을 확인했을 때만 호출하세요 (작업 【전체】가 완료된 경우에만 해당; 목표/flag/취약점 중 하나만 달성한 것은 【해당되지 않으며】, 그 경우에는 prove_goal로 해당 목표만 표시하면 됩니다). ⚠️이 도구는 '이번 라운드 계획 종료'를 위한 것이 아닙니다: 이번 라운드에 배정할 새 의도가 없거나 worker의 결과를 기다리는 경우에는 【그냥 이번 라운드를 종료하면 되며, 이 도구를 호출하지 마세요】 (의도 0개는 완전히 정상입니다). 일반적인 판정은 prove_goal로 목표를 하나씩 증명하는 것을 우선하며, goal_met은 개별 증명을 건너뛰고 전체를 바로 마무리하는 수단일 뿐입니다.",
		obj(map[string]any{"reason": str("달성 사유 (목표가 실제로 달성되었다는 증거여야 하며, 「이번 라운드에 새 방향 없음」 같은 이번 라운드 종료 사유는 불가)")}, "reason"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct{ Reason string }
			_ = json.Unmarshal(in, &a)
			t.GoalMet = true
			t.Reason = a.Reason
			return actool.Text("acknowledged: goal marked met"), nil
		})
}

// --- worker write tools ---

func (t *ToolSet) addFinding() actool.CoreTool {
	return writeTool("report_finding", "확인된 취약점을 기록한다. evidence에 명령 출력, 로그 등 검증 가능한 증거를 제공한다. 작업 컨텍스트에서는 현재 intent_id를 전달한다. 반환되는 finding_id는 독립된 취약점 기록 ID이고, finding_node_id는 탐색 노드 ID이다 (첫 줄에 이 노드 번호를 유지).", obj(map[string]any{
		"vulnclass": str("취약점 분류"), "name": str("취약점 이름"), "severity": str("critical|high|medium|low"), "summary": str("취약점 요약"),
		"intent_id": idp("현재 작업의 의도 id"), "asset_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "영향받는 자산 id"},
		"evidence":         str("증거/PoC 텍스트"),
		"evidence_hint_id": idp("선택: 본 작업에서 이 취약점에 해당하는 힌트 노드 ID이며, 그 구조화된 traffic_refs를 자동으로 함께 가져온다; 상속된 힌트나 다른 취약점의 힌트는 참조할 수 없다"),
		"traffic_refs": map[string]any{"type": "array", "description": "선택; HTTP/HTTPS 취약점은 먼저 검색하고 요청/응답을 하나씩 검증해 실제로 취약점 결론을 뒷받침하는지 확인한 뒤, 재현 순서대로 실제 ID를 입력하세요. TCP 등 비 HTTP 취약점, 수집되지 않았거나 정확한 기록을 찾을 수 없는 경우에는 생략하거나 []를 전달하면 되며 보고를 막지는 않습니다. evidence에 그 사유를 설명하고 다른 검증 가능한 증거를 제공할 수 있습니다. ID를 추측하거나, 도메인/시간으로 관련성을 추정하거나, 패킷 보충만을 위해 중복 탐색하지 마세요. 용도: baseline 정상 대조 / proof 취약점 증명 / verification 추가 검증 / supporting 보조 증거.",
			"items": obj(map[string]any{"traffic_id": str("traffic_search가 반환한 실제 트래픽 ID"), "role": map[string]any{"type": "string", "enum": []string{"baseline", "proof", "verification", "supporting"}}, "note": str("이 트래픽이 취약점 결론을 어떻게 뒷받침하는지")}, "traffic_id")},
	}, "vulnclass", "severity", "summary"), func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
		var a struct {
			VulnClass, Name, Severity, Summary, Evidence string
			IntentID                                     json.RawMessage   `json:"intent_id"`
			AssetIDs                                     []json.RawMessage `json:"asset_ids"`
			TrafficRefs                                  []db.TrafficRef   `json:"traffic_refs"`
			EvidenceHintID                               json.RawMessage   `json:"evidence_hint_id"`
		}
		if err := json.Unmarshal(in, &a); err != nil {
			return actool.Errorf(err.Error()), nil
		}
		if t.ts == nil {
			return actool.Errorf("report_finding은 작업 컨텍스트가 필요합니다. 플랫폼 대화에서는 add_task_hint로 해당 작업에 취약점을 인계하고, 힌트에 기존 traffic_refs를 함께 담아 작업 에이전트가 등록하도록 하세요. 이미 등록된 취약점은 bind_finding_traffic으로 트래픽을 보충 바인딩할 수 있습니다."), nil
		}
		// Auto-binding off: ignore the evidence params instead of rejecting the call.
		// stripTrafficParameters already removes them from the advertised schema, but
		// models routinely emit fields anyway — failing here would discard a confirmed
		// finding over a stray parameter. The success path below reports evidence_status
		// "not_bound" with the "자동 바인딩이 꺼져 있어 페이지에서 수동으로 연결 가능" note, which is what the caller needs.
		if !findingTrafficBindingEnabled() {
			a.TrafficRefs, a.EvidenceHintID = nil, nil
		}
		if len(a.EvidenceHintID) > 0 && pid(a.EvidenceHintID) <= 0 {
			return actool.Errorf("evidence_hint_id는 유효한 힌트 노드 ID여야 합니다; 인계 힌트가 없으면 생략하세요"), nil
		}
		refs, err := t.findingRefsFromHint(pid(a.EvidenceHintID), a.TrafficRefs)
		if err != nil {
			return actool.Errorf(err.Error()), nil
		}
		input := db.RecordFindingInput{TaskID: t.taskID, ExplorationID: t.ts.ID(), IntentID: pid(a.IntentID), VulnClass: a.VulnClass, Name: a.Name, Severity: a.Severity, Summary: a.Summary, Evidence: a.Evidence, Worker: t.worker, AssetIDs: pidList(a.AssetIDs)}
		var recorded *db.RecordedFinding
		if t.findingRecorder != nil {
			recorded, err = t.findingRecorder.Record(ctx, input, refs)
		} else if len(refs) > 0 {
			return actool.Errorf("트래픽 증거 저장소를 사용할 수 없습니다; 취약점이 등록되지 않았습니다"), nil
		} else {
			recorded, err = t.ts.RecordFinding(ctx, input)
		}
		if err != nil {
			return actool.Errorf(err.Error()), nil
		}
		if t.notifyFinding != nil {
			iid := input.IntentID
			if iid <= 0 {
				iid = t.ownerNode
			}
			t.notifyFinding(iid, a.Summary)
		} else if t.notify != nil {
			t.notify()
		}
		t.writes.Findings++
		// Keep the first line's node-ID contract for existing reporter triggers.
		for i := range recorded.Traffic.Bindings {
			recorded.Traffic.Bindings[i].Snapshot.ReqHead = ""
			recorded.Traffic.Bindings[i].Snapshot.RespHead = ""
		}
		result := struct {
			*db.RecordedFinding
			EvidenceStatus string `json:"evidence_status"`
			EvidenceNote   string `json:"evidence_note,omitempty"`
		}{RecordedFinding: recorded, EvidenceStatus: "bound"}
		if len(recorded.Traffic.Bindings) == 0 {
			result.EvidenceStatus = "not_bound"
			result.EvidenceNote = "취약점은 저장되었으나 트래픽은 바인딩되지 않았습니다. TCP/패킷 없음 상황이면 정상적으로 계속 진행해도 됩니다. 이미 검증된 HTTP 트래픽이 있다면 사용 가능한 bind_finding_traffic 또는 취약점 페이지에서 보충 바인딩한 뒤 증거 인계를 완료하세요. 취약점을 중복 생성하지 마세요."
			if !findingTrafficBindingEnabled() {
				result.EvidenceNote = "취약점이 저장되었습니다. 에이전트의 자동 트래픽 바인딩이 꺼져 있으니 페이지에서 수동으로 트래픽을 연결하세요."
			}
		}
		raw, _ := json.Marshal(result)
		return actool.Text(fmt.Sprintf("finding recorded: %d\n%s", recorded.NodeID, raw)), nil
	})
}

// recordFact writes a general exploration RESULT/conclusion (not a vuln, not a
// new asset) into the EXPLORATION graph, chained to the intent that produced it.
// This is the home for observations and — importantly — negative results
// ("port closed", "param not injectable", "no login found"). Such conclusions
// must NOT be stuffed into the asset graph via upsert_asset.
// factItem은 record_fact의 배치/단건 입력 중 하나의 사실이다.
type factItem struct {
	Summary    string            `json:"summary"`
	Detail     string            `json:"detail"`
	Evidence   string            `json:"evidence"`   // 핵심 증거 한 줄 (명령 + 핵심 출력 줄), 결론을 뒷받침하며 사후 검증에 용이하도록
	Confidence string            `json:"confidence"` // observed(직접 확인) | inferred(현상으로부터 추론)
	IntentID   json.RawMessage   `json:"intent_id"`
	AssetIDs   []json.RawMessage `json:"asset_ids"`
}

// recordOneFact는 fact 노드 하나를 기록하고 의도에 연결한다 (intent→yields→fact). defaultIntent는
// 배치 시의 기본 의도로, 이 항목에 intent_id가 없을 때 사용된다.
func (t *ToolSet) recordOneFact(it factItem, defaultIntent int64) (int64, error) {
	if strings.TrimSpace(it.Summary) == "" {
		return 0, fmt.Errorf("summary는 비울 수 없습니다")
	}
	payload := map[string]any{"summary": it.Summary}
	if it.Detail != "" {
		payload["detail"] = it.Detail
	}
	if e := strings.TrimSpace(it.Evidence); e != "" {
		payload["evidence"] = e
	}
	if c := strings.TrimSpace(it.Confidence); c != "" {
		payload["confidence"] = c
	}
	intent := pid(it.IntentID)
	if intent <= 0 {
		intent = defaultIntent
	}
	if intent > 0 {
		node, err := t.ts.GetNode(intent)
		if err != nil || node == nil || node.Kind != db.KindIntent {
			return 0, fmt.Errorf("intent_id는 본 작업의 의도여야 합니다 (연관 작업의 의도는 읽기 전용)")
		}
	}
	// a fact is its OWN node kind (distinct from a vuln finding).
	id, err := t.ts.AddNode(db.KindFact, payload, 5, "confirmed", t.worker, pidList(it.AssetIDs))
	if err != nil {
		return 0, err
	}
	if intent > 0 {
		_ = t.ts.Link(intent, db.RelYields, id) // chain: intent -> fact
	}
	t.writes.Facts++
	return id, nil
}

func (t *ToolSet) recordFact() actool.CoreTool {
	return t.writeExpTool("record_fact", "탐색 【사실/결론】을 탐색 그래프에 기록하고, 이를 낳은 의도(intent_id)에 연결한다. 탐색 결과를 기록하는 데 사용하며 — 지문/열거 등 【긍정 결론】과, '포트 닫힘' / '파라미터 주입 불가' / '로그인 진입점 미발견' 등 【부정 결론】을 모두 포함한다.\n"+
		"⚠️한 번의 탐색에서 나온 여러 관찰은 【하나의 사실로 요약】하고, 여러 개로 쪼개지 말 것. 하나로 합칠 수 있으면 가능한 한 하나의 사실로 표현하세요: summary=이번 결론에 대한 요약 한 문장, detail=관련 세부 사항(여러 구체 항목 포함 가능). 예: 지문 의도 → 하나의 사실 {summary:'X 사이트의 기술 스택과 응답 특성을 식별함', detail:'nginx 1.25 / Vue3 / 200 / title=.. / body_len=..'}; 상태 코드, 지문, 제목을 각각 따로 기록하지 마세요. 하나의 의도는 보통 하나의 사실만 산출하며, 너무 잘게 쪼개면 그래프가 무한히 팽창합니다.\n"+
		"★facts 배열은 서로 다른 여러 【결론】을 한 번에 기록할 때 사용한다 (각 항목은 intent_id를 생략할 수 있으며, 기본값은 최상위 intent_id). ids 배열을 반환하며 facts와 길이와 순서가 같다.\n"+
		"⚠️도구 출력에서 【실제로 확인한】 결론만 기록하고, 추측으로 채우지 마세요. evidence와 confidence는 부정확한 결론이 그래프를 오염시키는 것을 막기 위해 사용합니다:\n"+
		"  · evidence=이 결론을 뒷받침하는 【한 줄】 핵심 증거(명령 + 가장 잘 증명하는 한두 줄의 출력). **반드시 간결하게** — 세부 내용은 detail에 있으므로 여기에 긴 출력을 붙이지 마세요.\n"+
		"  · confidence=observed(출력에서 직접 확인) | inferred(현상으로부터 추론).\n"+
		"  · **부정 결론**(주입 불가/포트 닫힘/진입점 미발견 등)은 \"관찰 + 잠정적 해석\"만 기록 — 실제로 본 것을 진술하고, 방향을 포기할지는 계획자가 전체 상황을 종합해 결정함. 반드시 evidence를 제공하고, 수단을 다 쓰지 않았거나 증거가 약하면(한 번만 탐색했거나 그럴듯해 보이기만 하는 경우 포함) inferred로 표시하고, 완전히 소진했고 직접 확인한 경우에만 observed로 표시하세요.",
		obj(map[string]any{
			"facts":      map[string]any{"type": "array", "description": "【여러 개의 서로 다른 결론이 있을 때 사용】 사실 배열이며, 요소 필드는 아래 최상위 필드(summary/detail/evidence/confidence/intent_id/asset_ids)와 같다. intent_id를 생략하면 최상위 intent_id를 사용한다. ids는 이 배열과 길이와 순서가 같게 반환된다.", "items": map[string]any{"type": "object"}},
			"summary":    str("이번 탐색 결론에 대한 【요약 한 문장】 (detail을 요약한 것)"),
			"intent_id":  idp("이 사실을 낳은 의도 id (할당받은 의도; 배치 시 각 항목의 기본값)"),
			"detail":     str("이 사실의 관련 세부 사항: 이번 탐색에서 얻은 여러 관찰 사실을 모두 여기에 기록"),
			"evidence":   str("【한 줄】 핵심 증거: 명령 + 결론을 가장 잘 증명하는 한두 줄의 출력. 반드시 간결하게, 긴 출력을 붙이지 말 것 (세부 내용은 detail에)."),
			"confidence": str("observed(출력에서 직접 확인) | inferred(현상으로부터 추론). 부정 결론은 반드시 사실대로 표시할 것."),
			"asset_ids":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "관련 자산 id (선택, 0/1/여러 개): 이 사실이 관련된 자산"},
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Facts    []factItem `json:"facts"`
				factItem            // 단건 모드 + 배치 기본 intent_id
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Facts) > 0
			items := a.Facts
			if !batch {
				items = []factItem{a.factItem}
			}
			defaultIntent := pid(a.factItem.IntentID) // 최상위 intent_id = 배치 기본값

			ids := make([]int64, len(items))
			errs := map[string]string{}
			for i, it := range items {
				id, err := t.recordOneFact(it, defaultIntent)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
			}

			if !batch { // 단건: 기존 반환값 유지
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				return actool.Text(fmt.Sprintf("fact recorded: %d", ids[0])), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}

type hintItem struct {
	Text        string            `json:"text"`
	AssetIDs    []json.RawMessage `json:"asset_ids"`
	TrafficRefs []db.TrafficRef   `json:"traffic_refs"`
}

// addOneHint는 hint 노드 하나(active/human)를 탐색 그래프에 연결하며, 자산에 앵커링할 수 있고 id를 반환한다.
func (t *ToolSet) addOneHint(it hintItem) (int64, error) {
	if len(it.TrafficRefs) > 0 && !findingTrafficBindingEnabled() {
		return 0, fmt.Errorf("에이전트 자동 트래픽 바인딩이 꺼져 있어 traffic_refs를 담은 힌트를 저장하지 않았습니다. 시스템 설정에서 켜거나, 텍스트만 인계하세요")
	}
	if strings.TrimSpace(it.Text) == "" {
		return 0, fmt.Errorf("text는 비울 수 없습니다")
	}
	var anchors []int64
	for _, raw := range it.AssetIDs {
		if tid := pid(raw); tid > 0 {
			anchors = append(anchors, tid)
		}
	}
	refs, err := db.NormalizeTrafficRefs(it.TrafficRefs)
	if err != nil {
		return 0, err
	}
	payload := map[string]any{"text": it.Text}
	if len(refs) > 0 {
		payload["traffic_refs"] = refs
	}
	// planner 깨우기는 여기서 항목별로 하지 않고 — addHint가 배치 전체를 쓴 뒤 한 번에 트리거한다 (힌트 텍스트 포함),
	// 한 번의 add_hint로 들어온 여러 힌트가 planner의 트리거 줄을 항목마다 도배하는 것을 막기 위함이다.
	return t.ts.AddNode(db.KindHint, payload, 0, "active", "human", anchors)
}

type goalItem struct {
	Text      string `json:"text"`
	VulnClass string `json:"vulnclass"`
}

// addOneGoal은 goal 노드 하나(open)를 탐색 그래프에 연결한다: 작업 루트(origin fact, rel spawns)에 연결.
// origin은 t.worker를 사용한다(기본값 system): goals 분해기가 기록할 때는 "goals", 메인 에이전트 실행 시에는
// "human"으로 기록한다. planner 깨우기는 setGoals가 배치 전체를 쓴 뒤 한꺼번에 처리하며(아래 참조), 여기서는 저장만 담당한다.
func (t *ToolSet) addOneGoal(it goalItem) (int64, error) {
	text := strings.TrimSpace(it.Text)
	if text == "" {
		return 0, fmt.Errorf("text는 비울 수 없습니다")
	}
	payload := map[string]any{"text": text}
	if vc := strings.TrimSpace(it.VulnClass); vc != "" {
		payload["vulnclass"] = vc
	}
	origin := t.worker
	if origin == "" {
		origin = "system"
	}
	id, err := t.ts.AddNode(db.KindGoal, payload, 0, "open", origin, nil)
	if err != nil {
		return 0, err
	}
	if of, _ := t.ts.OriginFactID(); of > 0 && id > 0 {
		_ = t.ts.Link(of, db.RelSpawns, id) // goals descend from the task root (origin fact)
	}
	return id, nil
}

// setGoals는 【본 작업】에 탐색 목표(goal 노드)를 추가한다. 목표 분해기의 제출 도구이자, 메인
// 에이전트가 실행 중 목표를 보충하는 도구이기도 하다 — 동일한 관리형 도구로, 웹에서 설명/schema를 수정하고 에이전트별로 바인딩할 수 있다.
func (t *ToolSet) setGoals() actool.CoreTool {
	return writeTool("set_goals",
		"【본 작업】에 탐색 목표(goal)를 추가한다. 목표 = 최종적으로 전달 가능하고 검증 가능한 결과이며, 공격 단계나 정찰 행동이 아니다.\n"+
			"★우선 배치: 여러 목표를 goals 배열에 담아 한 번에 제출하세요. ids를 반환하며 goals와 길이와 순서가 같습니다 (실패 항목은 id=0, 상세는 errors 참조). 단건이면 goals를 생략하고 최상위 text를 주세요.\n"+
			"vulnclass 선택: 해당 취약점 분류(예: SQLi/IDOR), 비즈니스 로직 목표는 비워 둡니다. 목표의 달성 여부는 시스템이 판정해 met으로 표시하며, 이 도구는 추가만 담당합니다.",
		obj(map[string]any{
			"goals":     map[string]any{"type": "array", "description": "【우선 사용】 추가할 목표 배열이며 순서대로 처리한다. 각 요소: text(필수, 독립적으로 검증 가능한 최종 목표) + vulnclass(선택). ids는 이 배열과 길이와 순서가 같게 반환된다.", "items": map[string]any{"type": "object"}},
			"text":      str("[단건] 독립적으로 검증 가능한 최종 목표 하나"),
			"vulnclass": str("[단건] 해당 취약점 분류(명확한 경우), 예: SQLi/IDOR; 비즈니스 로직 목표는 비워 둘 수 있음"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.ts == nil {
				return actool.Errorf("set_goals 사용 불가: ExplorationStore가 초기화되지 않음"), nil
			}
			var a struct {
				Goals    []goalItem `json:"goals"`
				goalItem            // 단건 모드: 최상위 text/vulnclass
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Goals) > 0
			items := a.Goals
			if !batch {
				items = []goalItem{a.goalItem}
			}

			ids := make([]int64, len(items))
			errs := map[string]string{}
			var addedTexts []string
			for i, it := range items {
				id, err := t.addOneGoal(it)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
				addedTexts = append(addedTexts, strings.TrimSpace(it.Text))
			}
			if len(addedTexts) > 0 {
				// planner 깨우기(배치당 한 번). 우선 notifyGoal: 한 번의 set_goals마다 「사람이 N개의 목표를 추가함:
				// …」트리거 하나를 기록하며 항목별로 쏟아내지 않는다. 분해기/worker에는 이 콜백이 없으므로 순수 notify로 되돌아간다(분해기
				// round-0에서는 notify도 연결되어 있지 않아 아무 동작도 하지 않는다. 이때는 planner가 아직 시작되지 않았기 때문이다).
				switch {
				case t.notifyGoal != nil:
					t.notifyGoal(addedTexts)
				case t.notify != nil:
					t.notify()
				}
				// 메인 에이전트 실행 중 목표를 추가하면 → 완료/일시정지된 작업을 running으로 되돌려 계속 실행한다(종료 상태 게이트가
				// 일반 notify를 삼켜버리므로 명시적으로 부활시켜야 한다). mainagent에만 이 콜백이 연결되며, 분해기/worker에서는 nil이다.
				if t.resumeTask != nil {
					t.resumeTask()
				}
			}

			if !batch { // 단건: 기존 반환값 유지
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				return actool.Text(fmt.Sprintf("goal added: %d", ids[0])), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}

type constraintItem struct {
	Text string `json:"text"`
	Type string `json:"type"` // allow | deny
}

// addOneConstraint는 작업 제약 하나를 task_constraints에 저장한다. origin은 t.worker를 사용한다(기본값 system):
// 분해기는 "goals"를, 메인 에이전트는 "human"을 기록한다.
func (t *ToolSet) addOneConstraint(it constraintItem) (int64, error) {
	text := strings.TrimSpace(it.Text)
	if text == "" {
		return 0, fmt.Errorf("text는 비울 수 없습니다")
	}
	kind := strings.TrimSpace(strings.ToLower(it.Type))
	if kind == "" {
		kind = "deny" // 기본값은 거부 처리: 타입이 표기되지 않은 경우 더 보수적으로 동작
	}
	if kind != "allow" && kind != "deny" {
		return 0, fmt.Errorf("type은 allow 또는 deny여야 합니다")
	}
	return t.ts.AddConstraint(kind, text, t.worker)
}

// setConstraints는 【본 작업】에 작업 제약(allow=허용할 작업 / deny=금지할 작업)을 추가한다. 목표
// 분해기 round-0에서 제약을 추출하는 제출 도구이기도 하며, 메인 에이전트가 실행 중 제약을 보충하는 도구이기도 하다 — 동일한 관리형 도구로, 웹에서
// 설명/schema를 수정하고 에이전트별로 바인딩할 수 있다. 제약은 planner/worker의 시스템 프롬프트에 주입되어 탐색 경계를 제한한다.
func (t *ToolSet) setConstraints() actool.CoreTool {
	return writeTool("set_constraints",
		"【본 작업】에 작업 제약을 추가하여 탐색 경계를 정한다: type=allow(허용되는 작업) 또는 deny(금지되는 작업).\n"+
			"제약 = 『무엇을 할 수 있고 없는지』에 대한 규정(예: 『현재 포트만 테스트하고 다른 포트는 스캔하지 않음』 『운영 DB에 쓰기 작업 금지』 『수동 정찰만 허용』). 목표도 아니고 공격 단계도 아니다.\n"+
			"★우선 배치: 여러 제약을 constraints 배열에 담아 한 번에 제출하세요. ids를 반환하며 constraints와 길이와 순서가 같습니다 (실패 항목은 id=0, 상세는 errors 참조). 단건이면 constraints를 생략하고 최상위 text/type을 주세요.\n"+
			"작업 목표/설명에 【명시적으로 적힌】 제약만 등록하고 임의로 지어내지 마세요. 타입이 확실하지 않으면 deny를 사용하세요(더 보수적).",
		obj(map[string]any{
			"constraints": map[string]any{"type": "array", "description": "【우선 사용】 추가할 제약 배열이며 순서대로 처리한다. 각 요소: text(필수, 제약 하나) + type(allow|deny). ids는 이 배열과 길이와 순서가 같게 반환된다.", "items": map[string]any{"type": "object"}},
			"text":        str("[단건] 작업 제약 하나의 내용"),
			"type":        str("[단건] allow(허용) 또는 deny(금지); 생략 시 deny로 처리"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.ts == nil {
				return actool.Errorf("set_constraints 사용 불가: ExplorationStore가 초기화되지 않음"), nil
			}
			var a struct {
				Constraints    []constraintItem `json:"constraints"`
				constraintItem                  // 단건 모드: 최상위 text/type
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Constraints) > 0
			items := a.Constraints
			if !batch {
				items = []constraintItem{a.constraintItem}
			}
			ids := make([]int64, len(items))
			errs := map[string]string{}
			for i, it := range items {
				id, err := t.addOneConstraint(it)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
			}
			if !batch { // 단건: 단순 반환 유지
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				return actool.Text(fmt.Sprintf("constraint added: %d", ids[0])), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}

func (t *ToolSet) addHint() actool.CoreTool {
	return t.writeExpTool("add_hint", "사람/메인 에이전트의 전략 힌트를 탐색 그래프에 연결한다. planner는 다음에 의도를 생성할 때 이를 읽는다.\n"+
		"★우선 배치: 여러 힌트를 hints 배열에 담아 한 번에 제출하세요 (건별 호출보다 왕복이 적음). ids 배열을 반환하며 hints와 길이와 순서가 같습니다 (실패 항목은 id=0, 상세는 errors 참조). 단건이면 hints를 생략하고 최상위 text를 주세요.",
		obj(map[string]any{
			"hints":        map[string]any{"type": "array", "description": "【우선 사용】 추가할 힌트 배열이며 순서대로 처리한다. 각 요소의 필드는 아래 최상위 필드(text/asset_ids/traffic_refs)와 같다. ids는 이 배열과 길이와 순서가 같게 반환된다.", "items": obj(map[string]any{"text": str("힌트 내용"), "asset_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}}, "traffic_refs": HintTrafficSchema()})},
			"text":         str("[단건] 힌트 내용, 예: '인증 후 인터페이스를 집중 공략'"),
			"traffic_refs": HintTrafficSchema(),
			"asset_ids":    map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "앵커링할 자산 id (선택, 0/1/여러 개)"},
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Hints    []hintItem `json:"hints"`
				hintItem            // 단건 모드: 최상위 text/asset_ids
			}
			_ = json.Unmarshal(in, &a)
			batch := len(a.Hints) > 0
			items := a.Hints
			if !batch {
				items = []hintItem{a.hintItem}
			}

			ids := make([]int64, len(items))
			errs := map[string]string{}
			var addedTexts []string
			for i, it := range items {
				id, err := t.addOneHint(it)
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				ids[i] = id
				addedTexts = append(addedTexts, strings.TrimSpace(it.Text))
			}
			if len(addedTexts) > 0 {
				// planner 깨우기(배치당 한 번). 우선 notifyHint: 한 번의 add_hint마다 「사람이 N개의 전략 힌트를 추가함:
				// …」트리거 하나를 기록하여, planner가 "이번 라운드는 새 hint로 트리거됨"을 명확히 알고 힌트 내용을 보게 한다.
				// 이 콜백이 연결되지 않은 경우 순수 notify로 되돌아간다 (bare wake; 힌트는 여전히 그래프에 접혀 있어 스스로 읽을 수 있음).
				switch {
				case t.notifyHint != nil:
					t.notifyHint(addedTexts)
				case t.notify != nil:
					t.notify()
				}
			}

			if !batch { // 단건: 기존 반환값 유지
				if e, bad := errs["0"]; bad {
					return actool.Errorf(e), nil
				}
				return actool.Text(fmt.Sprintf("hint added: %d", ids[0])), nil
			}
			out := map[string]any{"ids": ids}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		})
}

// killWorkTool lets the planner terminate a single running work (by intent id).
func (t *ToolSet) killWorkTool() actool.CoreTool {
	return t.writeExpTool("kill_work", "실행 중인 의도(work) 하나를 종료한다. 빗나가거나 의미 없는 탐색을 중단할 때 사용하며, 종료된 의도는 stopped로 표시되고 자동으로 다시 할당되지 않는다. 결정하기 전에 get_worker_output으로 무엇을 하고 있는지 먼저 확인하세요.",
		obj(map[string]any{"intent_id": idp("종료할 의도 id (= work 핸들)")}, "intent_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.killWork == nil {
				return actool.Errorf("kill_work 현재 사용 불가"), nil
			}
			var a struct {
				IntentID json.RawMessage `json:"intent_id"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.IntentID)
			if id <= 0 {
				return actool.Errorf("intent_id는 필수입니다"), nil
			}
			node, err := t.ts.GetNode(id)
			if err != nil || node == nil || node.Kind != db.KindIntent {
				return actool.Errorf("intent_id는 본 작업의 의도여야 합니다 (연관 작업의 의도는 읽기 전용)"), nil
			}
			if err := t.killWork(id); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text(fmt.Sprintf("의도 %d의 work에 종료 신호를 보냈습니다", id)), nil
		})
}

// steerWorkTool lets the planner inject a mid-run course-correction into a running
// work WITHOUT killing it: the message reaches the worker before its next tool call,
// which re-plans its next step (already-gathered context is kept). For in-intent
// nudges ("X 중단, Y에 집중"); if the whole direction is wrong use kill_work + a new intent.
func (t *ToolSet) steerWorkTool() actool.CoreTool {
	return t.writeExpTool("steer_work", "실행 중인 의도(work)에 교정 지시를 실시간으로 주입한다. 중단시키지 않고 기존 진행도 잃지 않으며, worker는 다음 동작 전에 지시를 받아 그에 맞게 조정한다. '더 이상 X로 가지 말고 Y에 집중'처럼 【의도 내】 교정에 사용하며, 방향 전체가 잘못되었다면 kill_work 후 새 의도를 내리세요. 먼저 get_worker_output으로 무엇을 하고 있는지 확인하는 것을 권장합니다.",
		obj(map[string]any{
			"intent_id": idp("교정할 의도 id (= work 핸들)"),
			"message":   str("worker에 줄 교정 지시: 무엇을 멈추고 무엇으로 방향을 바꿀지 명확히"),
		}, "intent_id", "message"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.steerWork == nil {
				return actool.Errorf("steer_work 현재 사용 불가"), nil
			}
			var a struct {
				IntentID json.RawMessage `json:"intent_id"`
				Message  string          `json:"message"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.IntentID)
			if id <= 0 {
				return actool.Errorf("intent_id는 필수입니다"), nil
			}
			node, err := t.ts.GetNode(id)
			if err != nil || node == nil || node.Kind != db.KindIntent {
				return actool.Errorf("intent_id는 본 작업의 의도여야 합니다 (연관 작업의 의도는 읽기 전용)"), nil
			}
			if strings.TrimSpace(a.Message) == "" {
				return actool.Errorf("message는 필수입니다"), nil
			}
			if err := t.steerWork(id, a.Message); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text(fmt.Sprintf("의도 %d의 work에 교정 지시를 주입했습니다 (다음 단계부터 적용)", id)), nil
		})
}

// getWorkerOutput은 intent id로 work의 최종 (또는 중단 시점까지의) 결론 텍스트를 반환한다.
func (t *ToolSet) getWorkerOutput() actool.CoreTool {
	return t.readExpTool("get_worker_output", "본 작업 또는 직접 연관된 작업의 의도(work) 하나의 최종 출력 결론을 가져온다. 연관 작업 결과는 source_task_id/inherited=true를 달고 읽기 전용이다. 정상 종료 시에는 요약을 반환하고, 종료(stopped)/비정상 종료된 work는 중단 시점까지의 마지막 출력을 반환한다.",
		obj(map[string]any{"intent_id": idp("의도 id (= work 핸들)")}, "intent_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				IntentID json.RawMessage `json:"intent_id"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.IntentID)
			if id <= 0 {
				return actool.Errorf("intent_id는 필수입니다"), nil
			}
			intentNode, err := t.ts.GetNodeWithSources(id)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if intentNode == nil || intentNode.Kind != db.KindIntent {
				return actool.Errorf("intent_id가 본 작업 또는 그 직접 연관된 작업에 속하지 않습니다"), nil
			}
			acts, _, err := t.ts.ActivityListWithSources(id, 0, 1000)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			var chosen, fallback *db.Activity
			for i := range acts {
				switch acts[i].Kind {
				case "result":
					chosen = &acts[i]
					fallback = &acts[i]
				case "text":
					fallback = &acts[i]
				}
			}
			pick := chosen
			if pick == nil {
				pick = fallback
			}
			if pick == nil {
				if intentNode.Inherited {
					return jsonResult(inheritedMap(map[string]any{
						"intent_id": id, "final_text": "(해당 work에는 아직 출력이 없음)",
					}, intentNode.SourceTaskID))
				}
				return actool.Text("(해당 work에는 아직 출력이 없음)"), nil
			}
			detail, _ := t.ts.ActivityDetailWithSources(pick.ID)
			if detail == "" {
				detail = pick.Summary
			}
			result := map[string]any{
				"intent_id": id, "final_text": detail,
				"summary": pick.Summary, "is_error": pick.IsError,
			}
			if intentNode.Inherited {
				inheritedMap(result, intentNode.SourceTaskID)
			}
			return jsonResult(result)
		})
}

// traceSteps renders summary-only trace rows, re-truncating each summary to 100
// chars — the stored summary is capped at 200 for the UI transcript; the trace
// tools want it tighter since a whole work's step list is many rows.
func traceSteps(acts []db.Activity) []map[string]any {
	steps := make([]map[string]any, 0, len(acts))
	for i := range acts {
		step := map[string]any{
			"step_id": acts[i].ID, "kind": acts[i].Kind, "tool": acts[i].Tool,
			"is_error": acts[i].IsError, "summary": firstLine(acts[i].Summary, 100),
		}
		if acts[i].Inherited {
			inheritedMap(step, acts[i].SourceTaskID)
		}
		steps = append(steps, step)
	}
	return steps
}

// getWorkerTrace exposes a work's execution PROCESS (not just its final output):
// list step summaries, keyword-search within one work, or pull full detail of a
// few specific steps. Thinking steps are excluded everywhere.
func (t *ToolSet) getWorkerTrace() actool.CoreTool {
	return t.readExpTool("get_worker_trace",
		"의도(work) 하나의 【실행 과정】을 본다 (get_worker_output이 최종 결론만 주는 것과 달리). 세 가지 사용법:\n"+
			"① intent_id만 전달 → 해당 work의 각 단계 요약 스트림을 반환 (summary≤100자, step_id 포함; 동작 개요일 뿐 전체 출력은 포함하지 않음);\n"+
			"② intent_id + q → 키워드가 일치하는 단계의 요약만 반환 (요약과 전체 출력 모두에서 검색; 여전히 summary만 제공하며, 내용을 보려면 ③을 사용);\n"+
			"③ intent_id + step_ids → 이 단계들의 전체 내용(detail)을 반환; 한 번에 최대 5개를 가져오며, 초과분은 앞의 5개만 반환하고 가져오지 않은 항목은 notice/omitted_step_ids에 알려 준다.\n"+
			"일반적인 흐름: 먼저 ①/②로 의심스러운 단계의 step_id를 찾은 뒤, ③으로 그 전체 출력을 가져온다. 사고(thinking) 단계는 포함하지 않는다. 직접 연관된 작업의 과거 trace도 지원하며, 그 결과에는 source_task_id/inherited=true가 붙고 읽기 전용이다.",
		obj(map[string]any{
			"intent_id": idp("의도 id (= work 핸들)"),
			"q":         str("키워드: 요약/전체 출력에서 일치하는 단계만 반환 (선택; step_ids와 상호 배타적)"),
			"step_ids":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "전체 내용을 가져올 step_id (①/② 반환값에서; 한 번에 최대 5개, 더 많이 넘기면 앞의 5개만 반환되고 나머지는 omitted_step_ids에 나열됨)"},
			"limit":     intp("요약 스트림/검색의 반환 상한 (선택)"),
		}, "intent_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				IntentID json.RawMessage   `json:"intent_id"`
				Q        string            `json:"q"`
				StepIDs  []json.RawMessage `json:"step_ids"`
				Limit    int               `json:"limit"`
			}
			_ = json.Unmarshal(in, &a)
			id := pid(a.IntentID)
			if id <= 0 {
				return actool.Errorf("intent_id는 필수입니다"), nil
			}
			intentNode, nodeErr := t.ts.GetNodeWithSources(id)
			if nodeErr != nil {
				return actool.Errorf(nodeErr.Error()), nil
			}
			if intentNode == nil || intentNode.Kind != db.KindIntent {
				return actool.Errorf("intent_id가 본 작업 또는 그 직접 연관된 작업에 속하지 않습니다"), nil
			}
			// ③ detail drill-down by step ids, thinking excluded by the store.
			if len(a.StepIDs) > 0 {
				// Dedup + drop invalid ids first so garbage/duplicates don't eat into
				// the per-call cap. detail is returned in full (untruncated), so the
				// cap bounds one tool result; over the cap we serve the first N and
				// tell the model exactly which ids were deferred, instead of erroring
				// and forcing it to re-plan the call.
				const maxStepIDs = 5
				var ids []int64
				seen := make(map[int64]bool)
				for _, raw := range a.StepIDs {
					if v := pid(raw); v > 0 && !seen[v] {
						seen[v] = true
						ids = append(ids, v)
					}
				}
				var omitted []int64
				if len(ids) > maxStepIDs {
					omitted = append(omitted, ids[maxStepIDs:]...)
					ids = ids[:maxStepIDs]
				}
				acts, err := t.ts.ActivityByIDsWithSources(ids)
				if err != nil {
					return actool.Errorf(err.Error()), nil
				}
				steps := make([]map[string]any, 0, len(acts))
				for i := range acts {
					if acts[i].NodeID == nil || *acts[i].NodeID != id || acts[i].Inherited != intentNode.Inherited ||
						(acts[i].Inherited && acts[i].SourceTaskID != intentNode.SourceTaskID) {
						continue
					}
					step := map[string]any{
						"step_id": acts[i].ID, "kind": acts[i].Kind, "tool": acts[i].Tool,
						"is_error": acts[i].IsError, "detail": acts[i].Detail,
					}
					if acts[i].Inherited {
						inheritedMap(step, acts[i].SourceTaskID)
					}
					steps = append(steps, step)
				}
				result := map[string]any{"intent_id": id, "steps": steps, "returned_step_ids": ids}
				if len(omitted) > 0 {
					// returned_step_ids/omitted_step_ids let the model decide programmatically
					// whether another call is worth it; the notice states the same in prose.
					result["omitted_step_ids"] = omitted
					result["notice"] = fmt.Sprintf(
						"한 번에 최대 %d개 단계의 전체 내용을 가져오며, 이번에는 앞의 %d개(%v)만 반환했고 가져오지 않은 %d개는 %v입니다."+
							"이 내용으로 충분히 파악된다면 나머지 단계를 더 가져올 필요가 없으며, 꼭 계속해야 할 때는 이 step_id들로 다시 호출하세요.",
						maxStepIDs, len(ids), ids, len(omitted), omitted)
				}
				if intentNode.Inherited {
					inheritedMap(result, intentNode.SourceTaskID)
				}
				return jsonResult(result)
			}
			// ①/② summary stream, optionally keyword-filtered; 100-char summaries.
			var acts []db.Activity
			var err error
			if strings.TrimSpace(a.Q) != "" {
				acts, err = t.ts.ActivityTraceSearchWithSources(id, a.Q, a.Limit)
			} else {
				acts, err = t.ts.ActivityTraceWithSources(id, a.Limit)
			}
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			result := map[string]any{"intent_id": id, "steps": traceSteps(acts)}
			if intentNode.Inherited {
				inheritedMap(result, intentNode.SourceTaskID)
			}
			return jsonResult(result)
		})
}

// searchAllWorkerTraces keyword-searches EVERY work's process in this task — for
// finding what a worker saw but never wrote back as a fact. Returns only matching
// summaries (≤100 chars), each tagged with its intent_id for follow-up drill-down.
func (t *ToolSet) searchAllWorkerTraces() actool.CoreTool {
	return t.readExpTool("search_all_worker_traces",
		"【보통은 사용을 권장하지 않음. 시스템이 이미 대부분의 정보를 제공하고 있기 때문】 【본 작업의 다른 work 실행 과정】에서 키워드(q)로 검색한다 — 어떤 worker가 봤지만 fact로 기록되지 않은 것(특정 경로/token/오류 등)을 되찾을 때 사용한다."+
			"자신의 이 의도 단계는 자동으로 제외된다 (이미 컨텍스트에 있으므로)."+
			"일치한 단계의 요약(summary≤100자)만 반환하며, 각 항목에 intent_id가 붙는다. 이를 바탕으로 get_worker_trace(intent_id, step_ids=[...])를 사용해 전체 내용을 가져오세요.",
		obj(map[string]any{
			"q":     str("키워드 (모든 work 단계의 요약 + 전체 출력에서 검색)"),
			"limit": intp("반환 상한, 기본 100 (선택)"),
		}, "q"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Q     string `json:"q"`
				Limit int    `json:"limit"`
			}
			_ = json.Unmarshal(in, &a)
			if strings.TrimSpace(a.Q) == "" {
				return actool.Errorf("q는 필수입니다"), nil
			}
			// 호출자 자신의 이 의도 단계는 제외한다 (worker의 자체 trace는 이미 그 컨텍스트에 있음).
			acts, err := t.ts.ActivityTraceSearchAllWithSources(t.ownerNode, a.Q, a.Limit)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			hits := make([]map[string]any, 0, len(acts))
			for i := range acts {
				var intent int64
				if acts[i].NodeID != nil {
					intent = *acts[i].NodeID
				}
				hit := map[string]any{
					"intent_id": intent, "step_id": acts[i].ID, "worker": acts[i].Worker,
					"kind": acts[i].Kind, "tool": acts[i].Tool, "is_error": acts[i].IsError,
					"summary": firstLine(acts[i].Summary, 100),
				}
				if acts[i].Inherited {
					inheritedMap(hit, acts[i].SourceTaskID)
				}
				hits = append(hits, hit)
			}
			return jsonResult(map[string]any{"query": a.Q, "hits": hits})
		})
}

// listWorkerTraces gives a worker (which has no graph_overview and can't see the
// intent graph) a lightweight index of the works in this task — intent_id +
// one-line summary + state — so it can DISCOVER which works to inspect via
// get_worker_trace. Without this a worker only knows intent_ids that come back
// from search_all_worker_traces hits. Excludes still-open intents (not yet run →
// no process to inspect).
func (t *ToolSet) listWorkerTraces() actool.CoreTool {
	return t.readExpTool("list_worker_traces",
		"【보통은 사용을 권장하지 않음. 시스템이 이미 대부분의 정보를 제공하고 있기 때문】 본 작업에서 【이미 실행된 work(의도)】 색인을 나열한다: intent_id + 한 문장 방향(summary) + 상태."+
			"당신(worker)은 탐색 그래프를 볼 수 없으므로, 이것으로 들여다볼 가치가 있는 work를 찾으세요 — 그 다음 get_worker_trace(intent_id)로 단계를 보고, get_worker_trace(intent_id, step_ids=[...])로 상세를 가져오세요."+
			"이미 실행된 것만 나열(running/done/exhausted/blocked/stopped)하며, 아직 실행되지 않은 open은 제외한다. 주의: 당신의 작업 경계는 여전히 할당받은 그 의도이며, 다른 work를 보는 것은 관찰을 재사용하고 중복 작업을 피하기 위함일 뿐이다.",
		obj(map[string]any{
			"q":     str("summary 키워드로 필터링 (선택)"),
			"limit": intp("반환 상한, 기본 50 (선택)"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Q     string `json:"q"`
				Limit int    `json:"limit"`
			}
			_ = json.Unmarshal(in, &a)
			limit := a.Limit
			if limit <= 0 {
				limit = 50
			}
			all, err := t.ts.ListByKindWithSources(db.KindIntent, 500)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			q := strings.ToLower(strings.TrimSpace(a.Q))
			out := make([]map[string]any, 0, limit)
			for _, n := range all {
				if n.Inherited && n.State == "running" {
					continue
				}
				switch n.State {
				case "running", "done", "exhausted", "blocked", "stopped": // has run → has a process
				default:
					continue
				}
				var p map[string]any
				_ = json.Unmarshal(n.Payload, &p)
				summary, _ := p["summary"].(string)
				if q != "" && !strings.Contains(strings.ToLower(summary), q) {
					continue
				}
				item := map[string]any{"intent_id": n.ID, "summary": summary, "state": n.State}
				if n.Inherited {
					inheritedMap(item, n.SourceTaskID)
				}
				out = append(out, item)
				if len(out) >= limit {
					break
				}
			}
			return jsonResult(map[string]any{"works": out})
		})
}

// PlannerTools is the read + intent-generation + goal-judgement tool set.
func (t *ToolSet) PlannerTools() []actool.CoreTool {
	return []actool.CoreTool{
		t.graphOverview(), t.listFindings(), t.listFacts(), t.nodeDetail(),
		// cold-digest §6.1: restore folded cold nodes (digest body → members → detail).
		t.expandDigest(),
		t.getWorkerOutput(), t.getWorkerTrace(), t.searchAllWorkerTraces(), t.listGoals(), t.addIntent(), t.proveGoal(), t.goalMet(),
		t.killWorkTool(), t.steerWorkTool(),
		// report_finding: 계획 단계의 상황 판단 중 자신이 이미 취약점을 확인했다면 바로 등록할 수 있음 (worker와 같은 도구).
		t.addFinding(),
		// list_companies: 기업 목록 + scope + 자산 수 조회 (company_id 확보 / 소속 범위 파악).
		t.listCompanies(),
		// list_assets: 계획 시 DSL로 전체 자산 DB를 검색 (list_untested_assets의 "범위 내 미테스트" 관점과 함께 사용하여,
		// "도메인/지문/포트/상태 코드 등 조건으로 전체 DB에서 조회"하는 능력을 보완한다).
		t.listAssets(),
		// add_company_scope: 계획 시 도메인/IP/CIDR/ICP/키워드를 특정 기업의 자산 범위에 포함할 수 있음 (일치하는 자산을 자동으로 인수).
		t.addCompanyScope(),
		// add_task_scope: 루트 도메인 전체/기업 전체/특정 서브도메인/IP를 능동적으로 본 작업의 테스트 범위(커버리지 분모)에 포함한다.
		t.addTaskScope(),
		// list_untested_assets: 필요 시 본 작업 범위 내 미테스트 자산(유형 + 페이지네이션)을 조회하여 추가 테스트 여부를 직접 결정한다.
		t.listUntestedAssets(),
	}
}
