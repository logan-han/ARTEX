package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// Planner is the event-driven LLM planner (docs §4.3): each time the asset or
// exploration graph changes (debounced), it reads the exploration route, queries
// assets, judges whether the task goal is met, and emits 0..N exploration intents
// into the frontier. It is the sole intent generator.
type Planner struct {
	findingRecorder   FindingRecorder
	prov              llm.Provider
	model             string
	tx                *transcript.Store                      // raw LLM conversation persistence (nil = off)
	window            int                                    // context window in tokens (for compaction)
	windowFn          func() int                             // optional dynamic task-chain minimum
	maxTurns          int                                    // max agent turns per run (0 = unlimited)
	killWork          func(intentID int64) error             // engine callback to terminate a running work (nil = off)
	steerWork         func(intentID int64, msg string) error // engine callback to steer a running work mid-run (nil = off)
	proxyAddr         string                                 // recording proxy for WebFetch (empty = direct)
	proxyCACert       string                                 // recording proxy's CA cert path (HTTPS verify)
	webSearch         WebSearchOpts                          // web_search tool backend selection (off by default)
	workDir           string                                 // shared work dir (surfaced in prompt as artifact-output target)
	injectConstraints func() bool                            // resolver: inject task operation constraints into system prompt? (nil = yes)
	nonStreamingFn    func() bool                            // resolver: use non-streaming (Complete) path? (nil = streaming)
	noaEnabledFn      func() bool                            // resolver: use experimental noa compaction? (nil = off)
	maxTokensFn       func() int                             // resolver: per-reply output cap (nil/0 = send no cap)
	compactor         *Compactor                             // cold-node compaction (§7); nil = disabled

	// todos keeps ONE plan-scratchpad per task (keyed by exploration id) so the
	// planner's multi-step plan survives across wake-ups — each Plan() is a fresh
	// session, but the shared store lets it record a serial exploit chain once and
	// dispatch it step-by-step over rounds instead of front-loading it in parallel.
	todoMu sync.Mutex
	todos  map[int64]*actool.TodoStore
}

func NewPlanner(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int) *Planner {
	return &Planner{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns, todos: map[int64]*actool.TodoStore{}}
}

func (p *Planner) SetCompactionWindowResolver(fn func() int) { p.windowFn = fn }

// SetCompactor wires the cold-node compactor (cold-digest §7). Called each
// planner wake-up to advance the round counter, maintain cold stamps, and
// (off the hot path) fold cold nodes into digests. nil = feature disabled.
func (p *Planner) SetCompactor(c *Compactor) { p.compactor = c }

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default).
func (p *Planner) SetNonStreaming(fn func() bool) { p.nonStreamingFn = fn }

func (p *Planner) nonStreaming() bool { return p.nonStreamingFn != nil && p.nonStreamingFn() }

// SetNoaEnabled wires a resolver deciding whether runs use the experimental noa
// context-compression mechanism. nil/unset = off (built-in compaction). Read per
// run so the settings toggle takes effect without rebuilding the agent.
func (p *Planner) SetNoaEnabled(fn func() bool) { p.noaEnabledFn = fn }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (p *Planner) SetMaxTokens(fn func() int) { p.maxTokensFn = fn }

func (p *Planner) maxTokens() int {
	if p.maxTokensFn == nil {
		return 0
	}
	return p.maxTokensFn()
}

func (p *Planner) compactionWindow() int {
	if p.windowFn != nil {
		return p.windowFn()
	}
	return p.window
}

// SetProxy points the planner's WebFetch at the recording proxy plus the CA cert
// it trusts to verify HTTPS through it (empty addr = direct).
func (p *Planner) SetProxy(addr, caCert string) { p.proxyAddr, p.proxyCACert = addr, caCert }

// SetWebSearch selects the web_search backend for the planner (off by default).
func (p *Planner) SetWebSearch(o WebSearchOpts) { p.webSearch = o }

// SetConstraintInject wires a resolver deciding whether this task's operation
// constraints get injected into the planner system prompt. Read per round so the
// settings toggle takes effect without rebuilding the agent. nil = inject (default).
func (p *Planner) SetConstraintInject(fn func() bool) { p.injectConstraints = fn }

// wantConstraints reports whether constraint injection is enabled (default yes).
func (p *Planner) wantConstraints() bool { return p.injectConstraints == nil || p.injectConstraints() }

// todoFor returns the task's persistent planning todo store, creating it on first
// use. Shared across all of this task's planner wake-ups.
func (p *Planner) todoFor(expID int64) *actool.TodoStore {
	p.todoMu.Lock()
	defer p.todoMu.Unlock()
	s := p.todos[expID]
	if s == nil {
		s = actool.NewTodoStore()
		p.todos[expID] = s
	}
	return s
}

// SetKillWork wires the engine's per-work terminate callback so the planner's
// kill_work tool can stop a single running worker.
func (p *Planner) SetKillWork(fn func(intentID int64) error) { p.killWork = fn }

// SetSteerWork wires the engine's per-work steering callback so the planner's
// steer_work tool can inject a mid-run course-correction into a running worker.
func (p *Planner) SetSteerWork(fn func(intentID int64, msg string) error) { p.steerWork = fn }

// renderPlannerTodos formats the persistent planning todo for injection into the
// wake-up prompt (empty when there are no todos yet — first wake-up).
func renderPlannerTodos(items []actool.Todo) string {
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n[Your planning todo list (kept across wake-ups, written by you last round)]:\n")
	for _, it := range items {
		mark := map[actool.TodoStatus]string{actool.TodoPending: "☐", actool.TodoInProgress: "▶", actool.TodoCompleted: "✔"}[it.Status]
		if mark == "" {
			mark = "☐"
		}
		b.WriteString(fmt.Sprintf("  %s %s\n", mark, it.Content))
	}
	b.WriteString("Drive from this: only dispatch an intent for the next step whose [prerequisite steps are done / the fact it depends on already exists]; use TodoWrite to update the list (mark steps already satisfied by a fact as completed). Do not re-dispatch steps already pending/in_progress in the list.")
	return b.String()
}

// TriggerEvent describes what concretely caused this planning round to fire, so
// the planner looks first at the actual change instead of re-scanning the whole
// overview. Kind:
//
//	"done"    - a worker finished intent IntentID (its output conclusion is fetched).
//	"finding" - a worker reported a finding on intent IntentID (Detail = summary).
//	"goal"    - the human (via the main agent's set_goals) added one OR MORE goals in a
//	            single call (Goals = the goal text added this time, 1+ entries; set_goals supports batching).
//	"goal_deleted" - the human deleted a goal from the overview's goal management (Detail = deleted goal text).
//	"goal_edited"  - the human edited a goal from the overview's goal management (OldGoal->NewGoal text).
//	"cancelled" - the human deleted intent IntentID (Detail = deletion reason). The intent is
//	            stopped (not deleted) and the reason is attached to it as a fact.
type TriggerEvent struct {
	Kind     string
	IntentID int64
	Detail   string
	Summary  string   // Kind=="cancelled" only: intent summary captured before deletion (after a real delete the node no longer exists and cannot be queried)
	Goals    []string // Kind=="goal" only: goal text added by this set_goals call (one or more)
	OldGoal  string   // Kind=="goal_edited" only: goal text before the edit
	NewGoal  string   // Kind=="goal_edited" only: goal text after the edit
	Hints    []string // Kind=="hint" only: hint text added by this add_hint call (one or more)
}

// renderTriggers spells out the change(s) that fired this round: for a finished
// worker — which intent + its output conclusion; for a finding — which intent +
// what was found. Empty for time/heartbeat wakes. Reads the store (best-effort;
// a blank field never blocks the round).
func renderTriggers(ts *db.ExplorationStore, evs []TriggerEvent) string {
	if len(evs) == 0 || ts == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n[Actual change(s) that fired this round (look here first, then decide whether to add directions)]:")
	for _, ev := range evs {
		switch ev.Kind {
		case "goal":
			if len(ev.Goals) == 1 {
				b.WriteString(fmt.Sprintf("\n- The human (main agent) added a goal: %s. A new goal to achieve; add exploration directions for it accordingly (if no matching intent exists yet).", ev.Goals[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n- The human (main agent) added %d goals: %s. All are new goals to achieve; add an exploration direction one by one for any goal that has no matching intent yet.", len(ev.Goals), strings.Join(ev.Goals, "; ")))
			}
		case "hint":
			if len(ev.Hints) == 1 {
				b.WriteString(fmt.Sprintf("\n- The human (main agent) added a strategic hint: %s. It is attached to the exploration graph; adjust or add exploration directions accordingly (if no matching intent exists yet).", ev.Hints[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n- The human (main agent) added %d strategic hints: %s. All are attached to the exploration graph; adjust or add exploration directions one by one accordingly.", len(ev.Hints), strings.Join(ev.Hints, "; ")))
			}
		case "goal_deleted":
			b.WriteString(fmt.Sprintf("\n- The human deleted this goal: %s. The goal is removed; re-judge the remaining goals/directions accordingly (no need to dispatch intents for it).", ev.Detail))
		case "goal_edited":
			b.WriteString(fmt.Sprintf("\n- The human edited a goal, from \"%s\" to \"%s\". Adjust exploration directions to the new goal (stop dispatching the old direction if it no longer applies).", ev.OldGoal, ev.NewGoal))
		case "finding":
			b.WriteString(fmt.Sprintf("\n- The worker for intent #%d (%s) reported a finding: %s", ev.IntentID, intentSummary(ts, ev.IntentID), ev.Detail))
		case "cancelled":
			// Prefer the Summary captured at deletion time for the intent content (after a real delete the node no longer exists and intentSummary cannot find it).
			sm := ev.Summary
			if sm == "" {
				sm = intentSummary(ts, ev.IntentID)
			}
			b.WriteString(fmt.Sprintf("\n- Intent #%d was deleted by the user. Intent content: %s. Deletion reason: %s. The intent is deleted (will not run); re-plan accordingly.", ev.IntentID, sm, ev.Detail))
		default: // "done"
			b.WriteString(fmt.Sprintf("\n- The worker for intent #%d (%s) finished, output conclusion: %s", ev.IntentID, intentSummary(ts, ev.IntentID), workerOutput(ts, ev.IntentID)))
			if fids := factIDsYielded(ts, ev.IntentID); fids != "" {
				b.WriteString(fmt.Sprintf("; new fact ids produced by this intent: %s ", fids))
			}
		}
	}
	b.WriteString("\n(Full detail is available via node_detail / get_worker_output / list_findings.)")
	return b.String()
}

// factIDsYielded lists the fact ids an intent produced this run as "#12, #15", so the
// planner can jump straight to the round's incremental facts. Empty (best-effort) when
// the intent yielded no facts or the lookup fails.
func factIDsYielded(ts *db.ExplorationStore, id int64) string {
	ids, err := ts.FactsYielded(id)
	if err != nil || len(ids) == 0 {
		return ""
	}
	parts := make([]string, len(ids))
	for i, fid := range ids {
		parts[i] = fmt.Sprintf("#%d", fid)
	}
	return strings.Join(parts, ", ")
}

// intentSummary reads an intent node's one-line summary (best-effort, "?" on miss).
func intentSummary(ts *db.ExplorationStore, id int64) string {
	n, err := ts.GetNode(id)
	if err != nil || n == nil {
		return "?"
	}
	var p map[string]any
	if json.Unmarshal(n.Payload, &p) == nil {
		if s, ok := p["summary"].(string); ok && s != "" {
			return s
		}
	}
	return "?"
}

// workerOutput returns the finished worker's conclusion for an intent — the last
// 'result' (else 'text') activity's full detail, truncated. Same source get_worker_output uses.
func workerOutput(ts *db.ExplorationStore, id int64) string {
	acts, _, err := ts.ActivityList(&id, 0, 1000)
	if err != nil {
		return "(failed to fetch output)"
	}
	var pick *db.Activity
	for i := range acts {
		if acts[i].Kind == "result" {
			pick = &acts[i]
		} else if acts[i].Kind == "text" && pick == nil {
			pick = &acts[i]
		}
	}
	if pick == nil {
		return "(this work has no output record yet)"
	}
	out, _ := ts.ActivityDetail(pick.ID)
	if out == "" {
		out = pick.Summary
	}
	return truncOutput(out, 800)
}

// truncOutput caps a worker-output blob so the trigger context doesn't bloat the
// system prompt every round; full text is one get_worker_output call away.
func truncOutput(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + " ...(truncated, see get_worker_output for the full text)"
}

// renderGraphOverview folds the pre-computed graph_overview snapshot into the
// wake-up prompt so the planner starts each round with the full situation in
// hand — saving the round-trip it would otherwise spend calling the tool. It is
// the exact same JSON graph_overview would return; deeper detail is still one
// tool call away (node_detail / list_facts / …).
func renderGraphOverview(data map[string]any) string {
	b, err := json.Marshal(data)
	if err != nil {
		return "" // fall back to the model calling graph_overview itself
	}
	return "\n\n[This round's situation (graph_overview prefetched, identical to what calling that tool would return; call node_detail/list_facts etc. for more detail as needed)]:\n" + string(b)
}

// plannerDefaultTmpl is the built-in EDITABLE body (section [A]) of the planner prompt,
// seeded into agent_prompts. Goal is a {{.Goal}} template var; the artifact-output-spec
// tail is code-owned (artifactSpec) and appended by plannerSystem after rendering.
const plannerDefaultTmpl = `You are the "planner" of an authorised penetration-testing system on a cyber-security platform, woken frequently (woken the moment the graph changes). Duties: read the situation -> judge the goal -> **only when there is genuinely a new, uncovered direction** add exploration intents. You are the planner, not the executor: everything you produce this round can only be [generating/clarifying intents] or [judging goals]; never do the actual work inside the plan.

Task goal: {{.Goal}}

**How many intents should this round produce (think this through first)**:
- **Hard floor (highest priority)**: as long as [the goal is not met] and [there is currently no open or running intent at all] (frontier_open=0 and running_intents empty), this round **must** produce at least one intent that advances the goal. With no running work to wait on and no queued direction, producing 0 intents = the task stalls; even if every known direction is only in recent_done, open a new one or continue one based on the done/exhausted/blocked judgement below.
- Beyond the hard floor, **producing 0 intents is a normal outcome, but it needs a valid reason** (not a "dispatch less is safer" default): (1) **already covered** - every direction you can think of is already handled by an intent still open/running (re-generating an existing intent with reworded text is a serious error); (2) **waiting on a dependency** - the next step depends on the output of a currently running work that has not come back yet (forcing a dispatch now leaves the downstream step without its prerequisite and spinning; wait and dispatch after the next wake-up once the graph updates).
- Conversely: when there genuinely is a new direction [uncovered and not dependent on running work], or the goal is unmet and untested surface remains within scope, dispatch - do not treat 0 intents as a lazy default.

**Decision flow for each wake-up**:

1. **The full situation is attached below this prompt** (it is graph_overview's return, no need to call it again): task (original title + goal/root node), asset counts, goals + status, open/running/recent_done intents, sites_without_endpoints (sites with no endpoints, hinting at directions still to explore), facts (count of exploration facts, a different class from findings), recent_facts ({id,summary,confidence?}).
   - **Scope**: exploration nodes (goals/intents/facts/findings) are this task only; **the asset graph is globally shared** (one copy across tasks; asset counts are global within scope, not unique to this task) - ignore assets unrelated to this task.
   - **Lineage**: each intent carries parents (upstream: which facts/intents it derives from) and yields (downstream: which facts/findings it produced), and each recent_facts entry carries from_intent; use this to understand "which facts came from which direction, and whether a new direction can be synthesised from them".
   - **Negative/doubtful observations** (things like "port closed"/"not injectable" in recent_facts) are a worker's observation, not a verdict: before trusting one, node_detail(id) to see the evidence - only treat a direction as temporarily closed when the evidence is solid, confidence=observed, and the approaches are exhausted; when evidence is missing, it only "looks like it", it was probed just once, or confidence=inferred, treat it as [not yet determined], and if it is in scope and no other intent covers it, dispatch a re-check intent by default to confirm or refute it (**re-check the same negative direction at most once**; if it is still negative after the re-check and the evidence is reasonable, respect that conclusion and do not dispatch again).
   - **Call for deeper detail only as needed**: list_facts (paginated, newest first, default 20, supports q filtering and before paging, returns total/has_more), list_findings (all findings), node_detail(id) (full evidence/detail; lists and recent_facts give only summaries), list_assets (pull: q search, type/company_id/task_id filters, pagination, or id/ids direct fetch), asset_neighbors. Assets are globally shared, do not pull them all by default.

2. **Judge the goal (core duty)**: the goals field already carries the goals and their status; for an unmet goal that some finding/fact has proven, call prove_goal(goal_id, evidence_id, reason) to mark it met. **When the one you mark happens to be the last unfinished goal, the system automatically judges the whole task complete** - finishing is driven only by proving goals one at a time; there is no other "one-click complete" mechanism.
   - WARNING **Quantitative acceptance check (never stamp early)**: when a goal has a quantifiable condition (coverage reaching X%, capturing N flags, obtaining some privilege), before prove_goal you **must** check the measured values in graph_overview above (coverage.pct, the findings_total count, etc.): if it is not met, prove_goal is **forbidden**; dispatch an intent to close the gap instead; do not mark it met early on the grounds of "mostly done / the core is already taken". Example: required coverage is 100% but measured coverage.pct=40% -> not met, keep dispatching top-up testing intents.

3. **(Optional, opening only, extremely lightweight) probing to understand**: only when the graph still has almost no facts (recent_facts essentially empty, the task just started) and the situation alone is not enough to make the initial intent concrete, use Bash etc. to do a tiny, read-only probe of the target (e.g. 1-2 curls to look at the homepage/fingerprint). **The only legitimate product is a more precise one-sentence intent description** - never discovering/verifying/exploiting a vulnerability, nor an enumeration result of endpoints/directories/parameters (those are the worker's job; write them up as an intent and dispatch). Three hard boundaries:
   - If the graph already has worker-produced facts (facts>0 / recent_facts non-empty) -> probing yourself again is **forbidden**; base every judgement on existing facts, and this round's product can only be "dispatch a new intent" or "end"; to dig into a lead -> dispatch an intent for a worker to investigate, do not curl it yourself.
   - Even at the opening, probe at most <=3 times and then stop, purely to make the initial intent clear; the moment you notice yourself "investigating deeply" rather than "quickly fixing a direction" (enumerating endpoints/directories one by one, trying ids one by one, decoding chains, repeatedly probing the same endpoint, any injection/privilege-escalation/vulnerability testing or verification - all the worker's heavy lifting), stop at once and write it up as an intent.
   - If it can be judged from existing facts/situation, there is no need to probe at all.

4. **Decide which new directions to add**: **the "restraint" here means only [do not duplicate an existing intent], not "dispatch as little as possible"** - while the goal is unmet, the default question is "to close in on the goal, what deeper, harder, still-uncovered approaches are there", not "can we wrap up". An intent is [an open exploration direction] (not a fixed type/menu); judge directions yourself from known facts, assets and goals, and compare each against open + running + recent_done:
   - Already covered by open/running -> do not generate (already being handled).
   - Appeared in recent_done -> **first look at that intent's state (every entry carries it) to tell how it stopped, then decide**:
     - **done (ran to completion normally)**: already covered -> do not re-dispatch verbatim; whether it is a dead end comes from the fact conclusions it yielded, not from state; re-dispatch only when a [substantive new mechanism] appears (new fact/asset/parameter/a clearly different approach), and make the summary spell out how it differs from last time; rewording or "maybe it works if I try again" does not count, retrying is forbidden.
     - **exhausted (budget ran out, cut off halfway, only partial write-back) / blocked (model or network failure, essentially did not run)**: both ended badly midway with incomplete information - first use get_worker_trace / get_worker_output to see how far it actually got and where it stuck, then choose from: close to a breakthrough but cut off by budget -> dispatch "continue from last progress"; purely an external failure that never ran (often blocked) -> re-dispatch the same direction directly; stuck at the same spot every time -> change the approach/direction. The basis is always the real progress in the trace, not state itself.
   - A brand-new direction with no intent coverage at all -> generate.
   - Every known direction is covered by intents still open/running -> do not generate, just end (there is running/queued work; let it advance); but if only recent_done covers them, there is no open/running left and the goal is unmet -> per the hard floor at the top, you must open a new one or continue one.
   - **Depth over coverage**: coverage is a lower bound / acceptance item, not the exploration goal itself; after finding a high-value entry point (one that may lead to RCE/privilege escalation/data exfiltration), prioritise dispatching intents to [drive that path all the way through] rather than spreading wide to level up coverage with shallow per-asset tests.
   - **Keep routes diverse, do not converge too early**: while the goal is unmet, if the existing intents all pile onto the same route/entry point while a [fundamentally different] uncovered direction exists (another attack surface / another class of asset / another exploit chain), prioritise adding that divergent direction rather than adding synonymous intents on the same line (judge by substantive difference, not wording); if that divergent direction is already covered by an existing intent, still do not generate. The ideal is 2-3 mechanistically different routes running in parallel (e.g. "attack via the upload chain" and "attack via auth bypass"), concentrating resources only once one of them hands over evidence of [closing in on the goal]. **But diversity always yields to the [operation constraints] at the top**: for an attack surface/port/host/operation excluded by a constraint, never generate an intent even if it is fundamentally different.

   **Serial exploit chains: dispatch step by step, do not split into parallel.** For a strongly-dependent serial chain (1->2->3, where each step depends on the actual output of the previous one): do not fan them all out at once (a downstream step cannot get a prerequisite that does not exist yet and will only duplicate/spin); use TodoWrite to record the whole chain as todos (one per step), dispatch only the step whose "prerequisite is already satisfied" this round (usually the first), and after it yields a fact, dispatch the next step on the next wake-up (the prompt will carry the todo list) and mark the satisfied one completed. Do not split "the same thing" into two ("confirm the trigger point" and "trigger the trigger point" are the same step); only [parallel, mutually independent] dimensions (e.g. enumerating several unrelated endpoints) use multiple intents in parallel.

5. **Submit**: use **one** add_intent call to batch-submit the new directions you selected (the intents array, at most the 4 highest-value ones, do not call it repeatedly one at a time):
   - **summary**: a one-sentence natural-language description of the direction (full address of the test target + what to do + why), with no fixed category; dedup relies mainly on comparing it against existing intents.
   - **asset_ids**: the target asset ids this direction will test/attack (pass them wherever you can, 0/1/many, from list_assets) - whenever the direction centres on specific assets (site/endpoint/parameter/host) you must pass them, used for coverage dedup and to link into the asset chain; pass all of them when it spans multiple assets; leave empty only for pure global reconnaissance with no specific asset.
   - **parent_ids**: which upstream nodes this direction is synthesised from (optional, 0/1/many) - pass all of them when several facts combine into one intent, pass the id when it derives from some upstream intent/finding, and leave empty for a top-level brand-new direction.

Do not duplicate, do not pad; but when the goal is unmet and there is an uncovered, deeper approach, dispatch when you should. Concise, focused, efficient.`

func plannerSystem(goal, dataDir, workDir string) string {
	body := renderSystem("planner", plannerDefaultTmpl, PlannerVars{Goal: goal, DataDir: dataDir, Now: nowStr()})
	return body + artifactSpec(workDir)
}

// Plan runs one planning round. emit, if non-nil, receives the planner's execution
// steps (so users can see how it reads the situation and judges goals — the
// planner is the intent generator and was previously a black box). Returns whether
// the planner judged the goal met.
// triggers carries the concrete change(s) that fired this round — worker(s) done
// and/or finding(s) reported (may be several — the engine debounces a burst; empty
// for time/heartbeat wakes). They are spelled out at the top of the prompt so the
// planner looks first at the actual change (which intent, its output/finding).
func (p *Planner) Plan(ctx context.Context, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, goal string, triggers []TriggerEvent, emit func(db.Activity)) (met bool, reason string, err error) {
	// cold-digest §2.3/§7: advance this task's planner-round counter, maintain the
	// cold_since_round stamps, and (if a threshold is hit) kick off background
	// compaction. Synchronous part is cheap (a few queries); the LLM compaction
	// runs in a detached goroutine so it never adds latency to this round.
	p.compactor.OnPlannerRound(ctx, ts)
	tsx := NewToolSet(ts, "planner")
	tsx.SetFindingRecorder(p.findingRecorder)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetTaskID(taskID)
	tsx.SetCoverageEnabled(as == nil || as.CoverageEnabled(taskID))
	tsx.killWork = p.killWork   // enable kill_work tool (nil = unavailable)
	tsx.steerWork = p.steerWork // enable steer_work tool (nil = unavailable)
	if origin, _ := ts.OriginFactID(); origin > 0 {
		tsx.SetOwnerNode(origin) // planner-side anchors default to the task root (origin fact)
	}
	// Domain tools + the base default tool set (Read/Write/Edit/MultiEdit/LS/Glob/Grep/Bash).
	// When asset-coverage is disabled, drop add_task_scope/list_untested_assets (kept out of the prompt).
	base := append(tsx.DropCoverageTools(tsx.PlannerTools()), actool.DefaultTools()...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts)})
	tools, def, cleanup := AugmentTools(ctx, "planner", base)
	defer cleanup()
	// The key situation (just-finished intents + the prefetched full graph) is moved
	// into [this round's user input] (see input below); system keeps only the static
	// planning body. Moving it out keeps system stable each round and caches better; the
	// cost is that if a single round grows long, the situation may get compacted (a
	// planner round is usually short, so the risk is low). situational is appended to input below.
	situational := renderTriggers(ts, triggers) + renderGraphOverview(tsx.graphOverviewData())
	// Task-level deadline / endgame mode (injected via ctx, see taskclock.go). On the
	// endgame round, the planner's task-timeout wrap-up text is appended to this round's
	// user input (alongside situational) as [this round's operating instruction], so it
	// only does the final goal judgement and produces no new intents.
	tc := taskClockFrom(ctx)
	if tc.Final {
		situational += "\n\n[Task endgame wrap-up (special instruction for this round, overrides the regular planning flow above)]: " + resolveTaskTimeoutWrapup("planner")
	}
	// This task's working directory <workDir>/tasks/<taskID>; create it first.
	taskDir := ensureRunDir(p.workDir, taskID, 0)
	ctx = intercept.WithReviewContext(ctx, taskDir, intercept.ReviewBackground{})
	sysBody := plannerSystem(goal, p.workDir, taskDir)
	if p.wantConstraints() {
		sysBody += constraintBlock(ts) // inject operation constraints (if any) into the system prompt to frame the exploration boundary
	}
	system, boundary := deferredSystem(sysBody, def)
	// The planner has no wall-clock budget of its own; when there is a deadline, clamp
	// MaxDuration to the remaining time so a running planning round enters wrap-up when
	// the task's time is up (by timeout -> task-timeout text, by step count -> per-run text).
	maxDur, clamped := clampMaxDuration(tc.DeadlineUnix, 0)
	settle := wrapupSettlement("planner", nil)
	if tc.DeadlineUnix > 0 {
		settle = wrapupSettlementForTask("planner", nil, clamped)
	}
	opts := agentcore.Options{
		Provider:        p.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		EnableWebFetch:  true, // route through the recording proxy for an audit trail; load the proxy CA to verify the MITM-resigned HTTPS certs
		WebFetchProxy:   p.proxyAddr,
		WebFetchCACert:  p.proxyCACert,
		// Web search (optional). ddgs needs no key; brave-free needs BraveKey; tavily needs TavilyKey.
		// WebSearchProxy is a separate egress proxy (http/https/socks5), unrelated to the traffic-recording MITM proxy; empty = direct.
		EnableWebSearch:       p.webSearch.Enabled,
		WebSearchBackend:      p.webSearch.Backend,
		BraveSearchAPIKey:     p.webSearch.BraveKey,
		TavilySearchAPIKey:    p.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: p.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  p.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   p.webSearch.DeepSeekModel,
		WebSearchProxy:        p.webSearch.Proxy,
		BashEnv:               proxyEnv(p.proxyAddr, p.proxyCACert), // Bash subcommands default to using the proxy + trusting its CA
		WorkingDir:            taskDir,                              // this task's working directory <workDir>/tasks/<taskID>
		ToolOutputDir:         cmdOutDir(taskDir),
		MaxTurns:              p.maxTurns, // 0 = unlimited (configurable in agent management)
		MaxDuration:           maxDur,     // 0 = unlimited; with a deadline = time remaining until it
		Compaction:            compactionConfig(p.compactionWindow()),
		// Planning todos shared across wake-ups: let a serial chain persist across rounds (the session is new, the store is not).
		Todos: p.todoFor(ts.ID()),
		// Hitting [this round's] step budget -> the SDK runs wrap-up: land the conclusions
		// already worked out this round (the add_intent to dispatch, the prove_goal that can
		// be proven, recording the serial chain with TodoWrite) rather than stopping planning
		// - the planner will keep being woken afterwards.
		// When clamped (squeezed by the task deadline), use PromptByReason instead (see wrapupSettlementForTask).
		Settlement:   settle,
		NonStreaming: p.nonStreaming(), // when this profile selects non-streaming, go via Provider.Complete
		MaxTokens:    p.maxTokens(),    // 0 = send no cap, let the server default decide
	}
	if p.tx != nil { // persist raw LLM conversation; one accumulating file per task's planner
		opts.Transcript = p.tx
		opts.SessionID = fmt.Sprintf("exp%d-planner", ts.ID())
	}
	// Experimental feature: when enabled, noa takes over context compaction (archives
	// are centralised under <workDir>/noa/<SessionID> and persist).
	noaSession := fmt.Sprintf("exp%d-planner", ts.ID())
	enableNoa(&opts, p.noaEnabledFn, p.workDir, noaSession, noaWarn(noaSession))
	// The situation (just-finished intents + the full graph) is now appended to this
	// round's user input (see input below). The user input also carries the instruction
	// + the cross-wake-up todos (a todo is the model's own planning note, regenerable, so
	// the user input is fine for it).
	// The opening line comes in two forms by "whether this round has a concrete change":
	// a change -> point at the [actual change] block below; no change (heartbeat periodic
	// sweep / hint / resume etc.) -> do not falsely claim "the graph changed", instead
	// prompt it to also re-check the running intents.
	lead := "There was just a concrete change (see [Actual change(s) that fired this round] below); plan the next step accordingly: "
	if len(triggers) == 0 {
		lead = "This round is a wake-up from a **periodic sweep (heartbeat tick) / no concrete-change signal** - the graph may not have new changes. Also re-check the running intents: use steer_work to correct one with no progress or drifting off for a long time, and kill_work to cut losses on one whose direction is entirely wrong; then judge the goal and decide whether to add directions: "
		// On a heartbeat/no-change wake-up, if the whole graph has no open or running intent
		// at all -> exploration has stalled (no worker running, no queued direction). Tell the
		// planner clearly and force it to add new directions this round, instead of spinning an
		// idle round after merely re-checking the running intents.
		if active, err := ts.HasActiveIntent(); err == nil && !active {
			lead = "This round is a wake-up from a **periodic sweep (heartbeat tick)**, and there is currently **no open or running intent at all** - no worker running, no queued direction, exploration has stalled. You **must** produce one or more new intents this round that advance the goal and are **mutually distinct** from the existing intents in the graph (producing 0 intents is not allowed); first judge from the situation below whether the goal is already met, and if not, add directions immediately: "
		}
	}
	input := lead + situational + "\n\nFrom the situation above, judge the goal. When a goal is [truly met] (the goal's outcome has been obtained / the goal's vulnerability confirmed), use prove_goal to mark them one by one. **Hard floor: as long as the goal is not yet met and there is currently no open or running intent (frontier_open=0 and running_intents empty), this round must produce at least one intent that advances the goal - with no running work to wait on and no queued direction, producing 0 intents = the task stalls. Only when there are already open/running intents advancing, or the goal is met, may this round produce no new intent.**" +
		renderPlannerTodos(opts.Todos.List())
	// MaxDuration now interrupts running tools when the wall clock is up and enters wrap-up
	// in place (on the live ctx); a stuck single round no longer bypasses wrap-up, so no
	// external hard ctx fallback is needed. ctx only carries pause / kill / shutdown.
	_, _, err = captureRun(ctx, opts, input,
		func(r db.Activity) {
			if emit != nil {
				r.Worker = "planner" // planner activity has no intent_id (it generates them)
				emit(r)
			}
		})
	return tsx.GoalMet, tsx.Reason, err
}
