package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// jsonResult marshals v to a JSON tool result.
func jsonResult(v any) (actool.Result, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return actool.Errorf(err.Error()), nil
	}
	return actool.Text(string(b)), nil
}

// This file implements P2, the "cross-task orchestration toolset" (benchmark orchestration doc §2 P2). These are
// host tools: they need access to the Manager (any task's Store), the Engine (pausing) and the task creation flow,
// so they live in the server layer. Read tools redirect "existing per-task tools" to run against the target task's
// store (build a temporary ToolSet and Call the matching tool), reusing exactly the same logic; control tools
// (spawn/pause) call the Manager/Engine directly. Like the traffic tools, they are seeded into the tools table and
// bound per agent (only visible to the orchestration agents they are bound to).

// hostTools is the runtime host-tool provider fed to ToolAugment: traffic tools
// (gated by capture) + cross-task orchestration tools + user-defined custom tools.
// The second return is the names of custom tools flagged `deferred` (schema
// withheld, routed via SearchExtraTools/ExecuteExtraTool). Per-agent binding still
// decides who actually sees any of them.
//
//nolint:unused // used as the hostTools provider in wireAgentAugment
func (s *Server) hostTools() ([]actool.CoreTool, map[string][]string) {
	tools := append(s.m.HostTools(), s.orchestrationTools()...)
	tools = append(tools, s.findingRetestTools()...)
	tools = append(tools, s.platformTools()...) // platform operation tools (create/edit skills/tools/MCP, for Auto)
	custom, err := s.customTools()
	if err != nil {
		log.Printf("[custom-tool] load failed: %v", err)
		return tools, nil
	}
	tools = append(tools, custom...)
	// deferred custom tools → name -> its bound agent keys. ToolAugment turns a
	// name into a deferred entry only for agents it's actually bound to (so we don't
	// advertise a tool the per-agent binding will drop from the callable set).
	deferred := map[string][]string{}
	rows, _ := s.m.pg.ListCustomTools()
	for _, t := range rows {
		if t.Deferred && t.Enabled {
			deferred[t.Key] = t.Agents
		}
	}
	return tools, deferred
}

// orchestrationTools returns the cross-task tool set. Bound per-agent via the
// tools table (default: no binding — opt-in for orchestration agents).
func (s *Server) orchestrationTools() []actool.CoreTool {
	return []actool.CoreTool{
		s.toolListTasks(),
		s.toolListLLMProfiles(),
		s.toolSpawnTask(),
		s.toolPauseTask(),
		s.toolGetTaskGraph(),
		s.toolListTaskFindings(),
		s.toolAddHint(),
		s.toolGetWorkerTrace(),
		s.toolListWorkerTraces(),
		s.toolSearchWorkerTraces(),
		s.toolGetTaskNodeDetail(),
		s.toolUpdateFindingReport(),
		s.toolGetFindingTraffic(),
		s.toolBindFindingTraffic(),
	}
}

// --- schema helpers ---

func strParam(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

// parseProfileID reads an LLM profile id from a tool arg that may arrive as a JSON
// number (5) or a numeric string ("5"); returns 0 when absent/unparseable.
func parseProfileID(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		v, _ := strconv.ParseInt(strings.TrimSpace(str), 10, 64)
		return v
	}
	return 0
}

func objSchema(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		req := make([]any, len(required))
		for i, r := range required {
			req[i] = r
		}
		m["required"] = req
	}
	return m
}

func roTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		ReadOnly:   func(json.RawMessage) bool { return true },
		Concurrent: func(json.RawMessage) bool { return true },
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

func wrTool(name, desc string, schema map[string]any, run func(context.Context, json.RawMessage) (actool.Result, error)) actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: name, Description: desc, Schema: schema,
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			return run(ctx, in)
		},
	})
}

// delegateToTask resolves the `task_id` in the input, builds a ToolSet bound to
// that task's store, strips task_id, and calls the chosen per-task tool — so the
// cross-task read reuses the exact in-task logic against another task.
func (s *Server) delegateToTask(ctx context.Context, in json.RawMessage, pick func(*agent.ToolSet) actool.CoreTool) (actool.Result, error) {
	var head struct {
		TaskID string `json:"task_id"`
	}
	_ = json.Unmarshal(in, &head)
	if strings.TrimSpace(head.TaskID) == "" {
		return actool.Errorf("task_id is required"), nil
	}
	t, ok := s.m.Task(head.TaskID)
	if !ok {
		return actool.Errorf("task not found: " + head.TaskID), nil
	}
	var m map[string]json.RawMessage
	_ = json.Unmarshal(in, &m)
	delete(m, "task_id")
	inner, _ := json.Marshal(m)
	tsx := agent.NewToolSet(t.Store, "orchestrator")
	if s.m.Assets() != nil {
		tsx.SetAssetStore(s.m.Assets(), s.m.Assets().Companies())
	}
	tsx.SetNotify(t.Notify)         // generic wake-up (write ops without a dedicated callback use it; no-op for read tools)
	tsx.SetNotifyHint(t.NotifyHint) // add_hint → records a "human added N strategic hints: ..." trigger and wakes the planner
	return pick(tsx).Call(ctx, inner, nil)
}

// --- tools ---

func (s *Server) toolListTasks() actool.CoreTool {
	return roTool("list_tasks",
		"List all tasks (id/description/goal/status/run time/parent task/LLM config). The orchestration agent uses it to keep the big picture, see which tasks have been stuck too long and which LLM each one uses. Run time: running = creation → now, terminal = creation → last activity (seconds). llm_profile: the config name used by the task's planner/worker; (active config) = follows the global active config.",
		objSchema(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			lastAct, _ := s.m.PG().LastActivityAll()
			// id -> name to resolve each task's pinned LLM profile.
			profName := map[int64]string{}
			if profs, err := s.m.pg.ListProfiles(); err == nil {
				for _, p := range profs {
					profName[p.ID] = p.Name
				}
			}
			out := make([]map[string]any, 0)
			for _, t := range s.m.List() {
				status := s.deriveTaskStatus(t)
				end := lastAct[t.ExpID]
				if live := s.engine.LastActivity(t.ID); live > end {
					end = live
				}
				dur := int64(0)
				if status == "running" {
					dur = time.Now().Unix() - t.CreatedAt
				} else if end > t.CreatedAt {
					dur = end - t.CreatedAt
				}
				row := map[string]any{"id": t.ID, "description": t.Description, "goal": t.Goal, "status": status, "run_seconds": dur}
				if t.ParentRef != "" {
					row["parent_ref"] = t.ParentRef
				}
				llmState := t.llmStateSnapshot()
				if llmState.ProfileID == nil {
					row["llm_profile"] = "(active config)"
				} else if n, ok := profName[*llmState.ProfileID]; ok {
					row["llm_profile"] = n
				} else {
					row["llm_profile"] = fmt.Sprintf("#%d (deleted)", *llmState.ProfileID)
				}
				out = append(out, row)
			}
			return jsonResult(out)
		})
}

// toolListLLMProfiles lists the available LLM profiles (name/model/active) so an
// orchestration agent can pick one for spawn_task's llm_profile. Never leaks keys.
func (s *Server) toolListLLMProfiles() actool.CoreTool {
	return roTool("list_llm_profiles",
		"List the available LLM configs (profiles): id, name, model, format, and whether it is the currently active config. Pass the id to spawn_task's llm_profile_id parameter to give a subtask its own LLM (e.g. a cheap model for recon, a strong model for exploitation). API keys are not included.",
		objSchema(map[string]any{}),
		func(context.Context, json.RawMessage) (actool.Result, error) {
			profs, err := s.m.pg.ListProfiles()
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			out := make([]map[string]any, 0, len(profs))
			for _, p := range profs {
				out = append(out, map[string]any{
					"id": p.ID, "name": p.Name, "model": p.Model, "format": p.Format, "is_active": p.IsDefault,
				})
			}
			return jsonResult(map[string]any{"profiles": out})
		})
}

func (s *Server) toolSpawnTask() actool.CoreTool {
	return wrTool("spawn_task",
		"Create a subtask and start its exploration engine; returns task_id. Use it to dispatch one piece of work (e.g. a challenge or a target) as a standalone task. parent_ref is optional: set it to the parent task id associated with the current orchestration to link parent and child.",
		objSchema(map[string]any{
			"description":            strParam("Task description (short title)"),
			"goal":                   strParam("Task goal (what to achieve)"),
			"parent_ref":             strParam("Optional: parent task id (for the parent/child link)"),
			"source_task_ids":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": fmt.Sprintf("Optional: list of source task ids to inherit read-only (at most %d). The subtask can reference, read-only, the assets/conclusions these tasks have already established as a starting point; unlike parent_ref's plain parent/child pointer, this is content inheritance.", db.MaxTaskSourceCount)},
			"llm_profile_id":         map[string]any{"type": "integer", "description": "Optional: the LLM config id for this subtask's planner/worker (see list_llm_profiles); if empty, inherit the parent task's, then fall back to the global active config"},
			"timeout_seconds":        map[string]any{"type": "integer", "description": "Optional: task-level timeout (seconds). When reached, a graceful wrap-up runs and the task enters the timeout terminal state; empty or 0 = no limit"},
			"plan_heartbeat_seconds": map[string]any{"type": "integer", "description": "Optional: planner heartbeat trigger interval (seconds). Once this long has passed since the last planning round ended/the task started with no trigger in between → trigger a planning round (a fallback against deadlock + a wake-up to supervise in-flight workers). Empty or 0 = default 600 (10min);"},
			"seed_first_intent":      map[string]any{"type": "boolean", "description": "Optional: can be enabled for simple tasks; dispatches a seed intent at creation (content = description + goal) so a worker starts testing right away without waiting for the first planner round; default false (standard plan-then-execute flow)."},
		}, "description", "goal"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Description          string          `json:"description"`
				Goal                 string          `json:"goal"`
				ParentRef            string          `json:"parent_ref"`
				SourceTaskIDs        []string        `json:"source_task_ids"`
				LLMProfileID         json.RawMessage `json:"llm_profile_id"`
				TimeoutSeconds       int             `json:"timeout_seconds"`
				PlanHeartbeatSeconds int             `json:"plan_heartbeat_seconds"`
				SeedFirstIntent      bool            `json:"seed_first_intent"`
			}
			_ = json.Unmarshal(in, &a)
			if strings.TrimSpace(a.Description) == "" {
				a.Description = "Untitled task"
			}
			if strings.TrimSpace(a.Goal) == "" {
				return actool.Errorf("goal is required"), nil
			}
			if a.TimeoutSeconds < 0 {
				a.TimeoutSeconds = 0
			}
			// Read-only inherited source tasks: count limit + every id valid/deduplicated/existing; same validation as HTTP task creation.
			if len(a.SourceTaskIDs) > db.MaxTaskSourceCount {
				return actool.Errorf(fmt.Sprintf("select at most %d linked tasks", db.MaxTaskSourceCount)), nil
			}
			sourceIDs := make([]int64, 0, len(a.SourceTaskIDs))
			seenSources := map[int64]bool{}
			for _, raw := range a.SourceTaskIDs {
				id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
				if err != nil || id <= 0 || seenSources[id] {
					return actool.Errorf("linked task id is invalid or duplicated"), nil
				}
				if _, ok := s.m.Task(strconv.FormatInt(id, 10)); !ok {
					return actool.Errorf(fmt.Sprintf("linked task #%d does not exist", id)), nil
				}
				seenSources[id] = true
				sourceIDs = append(sourceIDs, id)
			}
			// LLM profile resolution: explicit id > inherit parent's pin > active(nil).
			var pin *int64
			if id := parseProfileID(a.LLMProfileID); id > 0 {
				if _, ok := s.loadProfileConfig(id); !ok {
					return actool.Errorf(fmt.Sprintf("LLM config #%d does not exist or has no API key set", id)), nil
				}
				pin = &id
			} else if a.ParentRef != "" {
				if pt, ok := s.m.Task(a.ParentRef); ok {
					pin = pt.LLMProfileID
				}
			}
			var llmIDs []int64
			if pin != nil {
				llmIDs = []int64{*pin}
			}
			t, err := s.m.CreateTaskWithOptions(a.Description, a.Goal, db.TaskCreateOptions{
				SourceTaskIDs:        sourceIDs,
				LLMProfileIDs:        llmIDs,
				TimeoutSeconds:       a.TimeoutSeconds,
				PlanHeartbeatSeconds: a.PlanHeartbeatSeconds,
			})
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if a.ParentRef != "" {
				t.ParentRef = a.ParentRef
				if id, e := strconv.ParseInt(t.ID, 10, 64); e == nil {
					_ = s.m.PG().SetParentRef(id, a.ParentRef)
				}
			}
			// Shared post-create flow, reusing the same launchTask as HTTP task creation (server.go createTask):
			// seed + visible background goal decomposition (round 0/LLM steps/each goal) + engine.Run.
			// seed_first_intent defaults to false (standard plan-then-execute); simple tasks can enable it to dispatch one work item directly for testing.
			s.launchTask(t, a.Description+" "+a.Goal, a.SeedFirstIntent)
			return actool.Text(fmt.Sprintf("task created: %s", t.ID)), nil
		})
}

func (s *Server) toolPauseTask() actool.CoreTool {
	return wrTool("pause_task", "Pause the given task (stops its planner/worker loops).",
		objSchema(map[string]any{"task_id": strParam("id of the task to pause")}, "task_id"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				TaskID string `json:"task_id"`
			}
			_ = json.Unmarshal(in, &a)
			t, ok := s.m.Task(a.TaskID)
			if !ok {
				return actool.Errorf("task not found: " + a.TaskID), nil
			}
			if _, err := s.applyTaskControlWithCause(t, "pause", agent.AbortPausedByOrchestrator); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return actool.Text("task paused: " + a.TaskID), nil
		})
}

func (s *Server) toolGetTaskGraph() actool.CoreTool {
	return roTool("get_task_graph", "Read the given task's exploration graph overview (same as graph_overview: asset counts/frontier/findings/coverage etc.); specify the task with task_id.",
		objSchema(map[string]any{"task_id": strParam("task id")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).GraphOverviewTool)
		})
}

func (s *Server) toolListTaskFindings() actool.CoreTool {
	return roTool("list_task_findings", "Read the given task's confirmed vulnerabilities (including flags/PoCs; each with id/task_id/intent_id/vulnclass/severity/summary/status); specify the task with task_id.",
		objSchema(map[string]any{"task_id": strParam("task id")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).ListFindingsTool)
		})
}

func (s *Server) toolAddHint() actool.CoreTool {
	return wrTool("add_task_hint", "Inject strategic hints into the given task (its planner reads them the next time it generates intents).\n"+
		"★Prefer batching: put multiple hints in the hints array and submit them at once (returns an ids array, same length and order as hints, id=0 for failed items); for a single hint omit hints and give the top-level text directly.",
		objSchema(map[string]any{
			"task_id":      strParam("task id"),
			"hints":        map[string]any{"type": "array", "description": "[PREFERRED] array of hints; each element has the same fields as the top level (text/asset_ids/traffic_refs).", "items": objSchema(map[string]any{"text": strParam("hint content"), "asset_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}}, "traffic_refs": agent.HintTrafficSchema()})},
			"text":         strParam("[single] hint content"),
			"traffic_refs": agent.HintTrafficSchema(),
			"asset_ids":    map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "anchored asset ids (optional, 0/1/many; asset ids within that task)"},
		}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).AddHintTool)
		})
}

func (s *Server) toolGetWorkerTrace() actool.CoreTool {
	return roTool("get_task_worker_trace",
		"View the execution trace of one work item (intent) in the given task: get_task_worker_trace(task_id, intent_id) shows step summaries; then pass step_ids=[...] to fetch those steps in full (at most 5 at a time; if you pass more, only the first 5 are returned).",
		objSchema(map[string]any{
			"task_id":   strParam("task id"),
			"intent_id": map[string]any{"type": "integer", "description": "intent id (a work item in that task)"},
			"step_ids":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "Optional: ids of the steps to fetch in full (at most 5 at a time; if you pass more, only the first 5 are returned and the rest are listed in omitted_step_ids)"},
		}, "task_id", "intent_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).GetWorkerTraceTool)
		})
}

func (s *Server) toolListWorkerTraces() actool.CoreTool {
	return roTool("list_task_worker_traces", "List which work items (intents) have run in the given task + their step counts, to find which ones are worth reviewing (then use get_task_worker_trace).",
		objSchema(map[string]any{"task_id": strParam("task id")}, "task_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).ListWorkerTracesTool)
		})
}

func (s *Server) toolSearchWorkerTraces() actool.CoreTool {
	return roTool("search_task_worker_traces", "Search the execution traces of all work items in the given task by keyword (returns matching step summaries + intent_id).",
		objSchema(map[string]any{"task_id": strParam("task id"), "q": strParam("search keyword")}, "task_id", "q"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).SearchWorkerTracesTool)
		})
}

func (s *Server) toolGetTaskNodeDetail() actool.CoreTool {
	return roTool("get_task_node_detail",
		"Read the full content of one exploration graph node in the given task (finding/fact/intent/goal: summary + details/evidence/PoC). id is the exploration node id (e.g. as returned by report_finding, or the id in list_task_findings). Use it to fetch a vulnerability's full evidence before writing its report.",
		objSchema(map[string]any{
			"task_id": strParam("task id"),
			"id":      map[string]any{"type": "integer", "description": "exploration graph node id (not an asset id)"},
		}, "task_id", "id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			return s.delegateToTask(ctx, in, (*agent.ToolSet).NodeDetailTool)
		})
}

// toolUpdateFindingReport writes/overwrites a finding's detailed Markdown report.
// finding_id is the id report_finding returned ("finding recorded: <id>", the
// finding node id). The write (SetFindingReportByNodeID) is keyed by node_id and
// task-agnostic, so this host tool needs no task_id / exploration store.
func (s *Server) toolUpdateFindingReport() actool.CoreTool {
	return wrTool("update_finding_report",
		"Write/update the **detailed report** for a recorded vulnerability (full Markdown text, replacing the old content entirely). Pass as finding_id the id returned by report_finding (the number in \"finding recorded: <id>\"). The report should cover: vulnerability overview, impact and risk, reproduction steps, evidence/PoC, remediation advice.",
		objSchema(map[string]any{
			"finding_id":       map[string]any{"type": "integer", "description": "target vulnerability id (the id returned by report_finding)"},
			"report":           strParam("full text of the detailed report, in Markdown"),
			"evidence_version": map[string]any{"type": "integer", "description": "the evidence version returned by get_finding_traffic; prevents the report from overwriting newer evidence changes"},
		}, "finding_id", "report"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				EvidenceVersion *int64          `json:"evidence_version"`
				FindingID       json.RawMessage `json:"finding_id"`
				Report          string          `json:"report"`
			}
			_ = json.Unmarshal(in, &a)
			nodeID := parseProfileID(a.FindingID) // reuse the "number or numeric string" parsing
			if nodeID <= 0 {
				return actool.Errorf("invalid finding_id"), nil
			}
			n, err := s.m.pg.SetFindingReportVersionByNodeID(ctx, nodeID, a.Report, a.EvidenceVersion)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if n == 0 {
				return actool.Errorf(fmt.Sprintf("no vulnerability record found for finding_id=%d (record it with report_finding first)", nodeID)), nil
			}
			return actool.Text(fmt.Sprintf("finding %d report updated (%d chars)", nodeID, len(a.Report))), nil
		})
}

// deriveTaskStatus mirrors listTasks' status derivation for the list_tasks tool.
func (s *Server) deriveTaskStatus(t *Task) string {
	lifecycle := t.lifecycleSnapshot()
	switch {
	case isTerminalStatus(lifecycle.Status):
		return lifecycle.Status
	case lifecycle.Paused || s.engine.IsPaused(t.ID):
		return "paused"
	case s.engine.ReadyFor(t) && s.engine.Started(t.ID):
		return "running"
	}
	return "created"
}

// orchestrationToolSeeds seeds the cross-task tools into the tools table so they
// are bindable per-agent (default: bound to nobody — opt-in for orchestration
// agents). First-insert only, like the traffic seeds.
func (s *Server) seedOrchestrationTools() {
	// task-op + platform tools default-bind to the built-in Auto agent (it exists to operate
	// the platform). SeedTool applies on first insert; rows already seeded in old DBs are bound by seedAutoDefaultBindings.
	autoAgents, _ := json.Marshal([]string{"auto"})
	for _, t := range s.orchestrationTools() {
		schema, _ := json.Marshal(t.InputSchema())
		bindings := autoAgents
		if t.Name() == "bind_finding_traffic" {
			bindings = json.RawMessage(`["reporter"]`)
		}
		_ = s.m.PG().SeedTool(t.Name(), t.Description(), schema, bindings)
	}
	for _, t := range s.platformTools() {
		schema, _ := json.Marshal(t.InputSchema())
		_ = s.m.PG().SeedTool(t.Name(), t.Description(), schema, autoAgents)
	}
	s.refreshBuiltinToolSchemas()
	s.seedAutoDefaultBindings()
	s.seedPlannerDefaultBindings()
	s.seedPlannerListAssetsBinding()
	s.seedCompanyScopeRebind()
	s.seedWorkerReadToolsUnbind() // unbind list_facts/list_companies/list_worker_traces from worker by default (one-off)
	s.seedWorkerReadbackRebind()  // repair an old migration's wrong removal: re-bind search_all_worker_traces/get_worker_trace/node_detail to worker (one-off)
	s.seedAutoReportFindingBinding()
	s.unbindGoalMetDefault()
	s.reseedGoalsPrompt()             // goals prompt gains the "extract operating constraints" step → append a new default version for old DBs (one-off)
	s.reseedMainAgentPrompt()         // mainagent prompt gains "after goals are met, add_intent asks whether to create a goal" (one-off)
	s.reseedPlannerPrompt()           // planner prompt: rewrite the valid reasons for "0 intents" + add a quantified acceptance check (one-off)
	s.reseedWorkerPrompt()            // worker prompt: add an evidence bar for negative conclusions (one-off)
	s.seedReporterAgent()             // preset "Report writer" agent + tool bindings + finding trigger (one-off)
	s.upgradeReporterTriggerMessage() // migration for old DBs: make the reporter pass back evidence_version (one-off)
	s.seedFindingTrafficTools()       // add optional evidence parameters and read-only evidence tools, keeping user config
	s.seedFindingWorkflowTools()
	// Note: pentest's default tool bindings need no migration: BuiltinToolSeeds seeds
	// list_assets/insert_assets/report_finding/list_findings/list_companies together with
	// pentest on a fresh init (the project has no old DBs yet, so there is no migration).
}

// refreshBuiltinToolSchemas propagates code schema/description changes on the
// orchestration + platform tools into already-seeded rows ONCE per version flag —
// SeedTool is first-insert-only, so a new param (e.g. spawn_task's llm_profile) never
// reaches an old DB otherwise. Preserves each tool's agent binding + enabled flag.
// Bump the flag whenever these tools' schemas/descriptions change in code.
func (s *Server) refreshBuiltinToolSchemas() {
	const flag = "tool_schema_refresh_v7_list_facts_paging"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	tools := append(s.orchestrationTools(), s.platformTools()...)
	for _, t := range tools {
		schema, _ := json.Marshal(t.InputSchema())
		if err := s.m.pg.RefreshToolDefaults(t.Name(), t.Description(), schema); err != nil {
			log.Printf("[tools] refresh %s schema failed: %v", t.Name(), err)
		}
	}
	// Also refresh some built-in agent tools to the code defaults:
	//   - goal_met: the description seeded in old DBs misleadingly said "end this planning round", so the planner
	//     treated it as a way to "end an empty round" and wrongly judged the whole task complete right after starting.
	//   - insert_assets: new related parameter (marks whether an asset relates to the current task, deciding whether
	//     it counts toward coverage); SeedTool is first-insert only, so schemas already seeded in old DBs would miss it.
	//   - list_facts: now paginated, with new limit/before/q parameters; otherwise the empty schema already seeded in
	//     old DBs shows "no parameters" on the tool management page and the model never sees these parameter docs.
	refreshBuiltin := map[string]bool{"goal_met": true, "insert_assets": true, "list_facts": true}
	for _, sd := range agent.BuiltinToolSeeds() {
		if !refreshBuiltin[sd.Key] {
			continue
		}
		schema, _ := json.Marshal(sd.Schema)
		if err := s.m.pg.RefreshToolDefaults(sd.Key, sd.Desc, schema); err != nil {
			log.Printf("[tools] refresh %s desc failed: %v", sd.Key, err)
		}
	}
	_ = s.m.pg.SetSetting(flag, "true")
	log.Printf("[tools] refreshed orchestration/platform tool schemas to the code defaults (one-off)")
}

// unbindGoalMetDefault removes goal_met's default "planner" binding ONCE (guarded by
// a settings flag), so existing DBs match the new default of NO agent. goal_met bypasses
// per-goal prove_goal to declare the whole task done — powerful/risky and redundant with
// the prove_goal→auto-complete path — so it ships unbound; users can re-bind it per agent
// in the UI. A user's own binding to another agent is untouched (we only strip planner).
func (s *Server) unbindGoalMetDefault() {
	const flag = "goal_met_unbind_default_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.RemoveAgentFromTool("planner", "goal_met"); err != nil {
		log.Printf("[tools] failed to unbind goal_met from planner: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// reseedGoalsPrompt refreshes the goals decomposer's prompt to the CURRENT CODE DEFAULT, because the default body
// gained the "extract operating constraints (set_constraints) before decomposing goals" step, and SeedPromptIfEmpty
// is first-insert only, so the version 1 already in old DBs does not get it. Here versioning APPENDS A NEW VERSION
// and switches to it (ResetPromptToDefault); old versions stay in the history, so users who customised it can
// recover it from the version records. Guarded by a settings flag → runs once; bump the flag when the default
// changes again. Fresh DBs need nothing (SeedPromptIfEmpty already seeds the latest default).
func (s *Server) reseedGoalsPrompt() {
	const flag = "goals_prompt_constraint_step_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // only try once, whether or not it succeeds
	a, err := s.m.pg.GetAgentByKey("goals")
	if err != nil || a == nil {
		return // on a fresh DB with no agent rows yet, seedPrompts seeds the latest default directly; no migration needed
	}
	tmpl := agent.BuiltinPromptSeeds()["goals"]
	if tmpl == "" {
		return
	}
	// Fresh DB: seedPrompts already seeded the latest default → the current version equals the code default; no need to append a duplicate version.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] failed to refresh the goals prompt to the new default: %v", err)
		return
	}
	log.Printf("[prompts] appended a new default version of the goals prompt (adds the constraint extraction step, one-off)")
}

// reseedMainAgentPrompt refreshes the mainagent prompt to the CURRENT CODE DEFAULT: the default body gained guidance
// that "once all goals are met, when add_intent dispatches an intent directly, ask the human whether to register it
// as a formal goal", and SeedPromptIfEmpty is first-insert only, so versions already in old DBs do not get it.
// Versioning APPENDS A NEW VERSION and switches to it (ResetPromptToDefault); old versions stay in the history, so
// users who customised it can recover it from the version records. Guarded by a settings flag → runs once. Fresh
// DBs need nothing (SeedPromptIfEmpty already seeds the latest default). Exactly the same shape as reseedGoalsPrompt.
func (s *Server) reseedMainAgentPrompt() {
	const flag = "mainagent_prompt_goalless_intent_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // only try once, whether or not it succeeds
	a, err := s.m.pg.GetAgentByKey("mainagent")
	if err != nil || a == nil {
		return // on a fresh DB with no agent rows yet, seedPrompts seeds the latest default directly; no migration needed
	}
	tmpl := agent.BuiltinPromptSeeds()["mainagent"]
	if tmpl == "" {
		return
	}
	// Fresh DB: seedPrompts already seeded the latest default → the current version equals the code default; no need to append a duplicate version.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] failed to refresh the mainagent prompt to the new default: %v", err)
		return
	}
	log.Printf("[prompts] appended a new default version of the mainagent prompt (adds asking to create a goal after goals are met, one-off)")
}

// reseedPlannerPrompt refreshes the planner prompt to the CURRENT CODE DEFAULT: the default body was trimmed and
// restructured, "restraint" was downgraded to deduplication only, "depth over coverage" and "hard floor: if goals are
// unmet and no intents are running, you must produce some" were added, and re-checking negative conclusions got an
// upper bound. Bump the flag below (currently v2) whenever the default changes substantively so existing old DBs
// refresh again. SeedPromptIfEmpty is first-insert only, so versions already in old DBs do not get it; hence
// versioning APPENDS A NEW VERSION and switches to it (ResetPromptToDefault); old versions stay in the history, so
// users who customised it can recover it from the version records. Guarded by a settings flag → runs once. Fresh DBs
// need nothing (SeedPromptIfEmpty already seeds the latest default). Exactly the same shape as reseedGoalsPrompt.
func (s *Server) reseedPlannerPrompt() {
	const flag = "planner_prompt_compact_realistic_v2"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // only try once, whether or not it succeeds
	a, err := s.m.pg.GetAgentByKey("planner")
	if err != nil || a == nil {
		return // on a fresh DB with no agent rows yet, seedPrompts seeds the latest default directly; no migration needed
	}
	tmpl := agent.BuiltinPromptSeeds()["planner"]
	if tmpl == "" {
		return
	}
	// Fresh DB: seedPrompts already seeded the latest default → the current version equals the code default; no need to append a duplicate version.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] failed to refresh the planner prompt to the new default: %v", err)
		return
	}
	log.Printf("[prompts] appended a new default version of the planner prompt (trimmed restructure + restraint downgraded to dedup + depth first + bound on negative re-checks, one-off)")
}

// reseedWorkerPrompt refreshes the worker prompt to the CURRENT CODE DEFAULT: the record_fact section of the default
// body dropped the whole sentence about "for negative conclusions, write the observation + a tentative reading", and
// decoupled confidence (observed/inferred) from "whether this intent's means were exhausted" (both easily misled the
// planner), while tightening the split of the facts array into separate entries to the rare exception of "entirely
// independent and impossible to merge". Bump the flag to v3 so existing old DBs refresh again. SeedPromptIfEmpty is
// first-insert only, so versions already in old DBs do not get it; hence versioning APPENDS A NEW VERSION and switches
// to it, and old versions stay recoverable in the history. Guarded by a settings flag → runs once. Fresh DBs need
// nothing. Exactly the same shape as reseedGoalsPrompt.
func (s *Server) reseedWorkerPrompt() {
	const flag = "worker_prompt_compact_v4"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // only try once, whether or not it succeeds
	a, err := s.m.pg.GetAgentByKey("worker")
	if err != nil || a == nil {
		return // on a fresh DB with no agent rows yet, seedPrompts seeds the latest default directly; no migration needed
	}
	tmpl := agent.BuiltinPromptSeeds()["worker"]
	if tmpl == "" {
		return
	}
	// Fresh DB: seedPrompts already seeded the latest default → the current version equals the code default; no need to append a duplicate version.
	if cur, err := s.m.pg.CurrentPrompt(a.ID); err == nil && cur == tmpl {
		return
	}
	if _, err := s.m.pg.ResetPromptToDefault(a.ID, tmpl); err != nil {
		log.Printf("[prompts] failed to refresh the worker prompt to the new default: %v", err)
		return
	}
	log.Printf("[prompts] appended a new default version of the worker prompt (context lookup section narrowed to list_assets/list_findings, dropped list_facts/node_detail/asset_neighbors, one-off)")
}

// reporterToolCallMessage must unconditionally require reading get_finding_traffic once before writing the report.
// That tool is read-only and "does not depend on the capture switch", so it can read manually bound evidence whether
// or not automatic binding is on. If this said "only read when automatic binding is enabled", then under the default
// (off) config the reporter would not pass evidence_version, SetFindingReportVersionByNodeID would write -1 under the
// legacy semantics, and the vulnerability details and Markdown export would permanently show "evidence changed, report
// needs updating", with nothing in the UI able to clear it.
const reporterToolCallMessage = "A vulnerability was just recorded via report_finding above. Read finding_id (the standalone vulnerability record ID) and finding_node_id (the exploration node ID) from the returned JSON, " +
	"then first use get_finding_traffic(finding_id) to read the current evidence list and its version (an empty list is normal; write the report as usual); " +
	"if the run guidance enables automatic binding, verify and link this vulnerability's traffic before reading. Use finding_node_id for node details. " +
	"Finally call update_finding_report(finding_id=finding_node_id, report, evidence_version=<the version actually read>) to save; " +
	"evidence_version is required, otherwise the report is permanently flagged as needing an update. Do not mix up the two IDs."

// Old trigger message (0.3.8 and earlier), kept verbatim in the original Chinese because it must match the rows those
// versions seeded: only records still identical to it word for word are migrated; ones the user edited are left alone.
const reporterToolCallMessageV1 = "上面刚有一个漏洞被 report_finding 登记。请从触发上下文里取出 finding_id" +
	"（工具返回 \"finding recorded: <id>\" 里的数字）与任务 id，按你的职责撰写该漏洞的详细报告，" +
	"最后调用 update_finding_report(finding_id, report) 保存。"

// upgradeReporterTriggerMessage refreshes reporter trigger messages in old DBs that still carry the default text to the
// new version. seedReporterAgent is guarded by reporter_agent_seed_v1 and only writes the trigger when it creates the
// agent, so upgraded DBs never get the new text: seedFindingTrafficTools added evidence_version to the tool schema, but
// nothing tells the reporter to use it. One-off, and only overwrites text that was not modified.
func (s *Server) upgradeReporterTriggerMessage() {
	const flag = "reporter_trigger_evidence_version_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // only try once
	triggers, err := s.m.pg.ListTriggersFor("reporter")
	if err != nil {
		log.Printf("[reporter] failed to read triggers: %v", err)
		return
	}
	for _, t := range triggers {
		if !t.OnToolCall || t.ToolCallMessage != reporterToolCallMessageV1 {
			continue // edited by the user or not a finding trigger; leave it alone.
		}
		t.ToolCallMessage = reporterToolCallMessage
		if err := s.m.pg.UpdateTrigger(t); err != nil {
			log.Printf("[reporter] failed to upgrade the trigger message: %v", err)
			return
		}
		log.Printf("[reporter] trigger message upgraded to read and pass back evidence_version")
	}
}

// seedReporterAgent presets a "Report writer" custom agent (builtin=false, editable/deletable in the UI): it binds
// update_finding_report + the task query tools and attaches a "fires when report_finding is called" trigger, so
// every recorded vulnerability wakes it up to write a detailed report. One-off (settings flag guard): it is not
// recreated after the user deletes it. Dependency: the orchestration tools are SeedTool'd above this function, so the
// bindings resolve.
func (s *Server) seedReporterAgent() {
	const flag = "reporter_agent_seed_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	defer func() { _ = s.m.pg.SetSetting(flag, "true") }() // only try once, whether or not it succeeds

	if exist, _ := s.m.pg.GetAgentByKey("reporter"); exist != nil {
		return // key already taken (the user created it by hand); do not overwrite
	}
	a, err := s.m.pg.CreateAgent("reporter", "Report writer",
		"Detailed vulnerability report writer: triggered automatically when a vulnerability is found; gathers the evidence and execution trace, then writes a Markdown report and saves it back.")
	if err != nil {
		log.Printf("[reporter] failed to create agent: %v", err)
		return
	}
	if err := s.m.pg.SeedPromptIfEmpty(a.ID, agent.ReporterDefaultPrompt); err != nil {
		log.Printf("[reporter] failed to seed prompt: %v", err)
	}
	// Trigger run strategy: parallel + none, i.e. one report per vulnerability, with multiple findings written concurrently.
	// merge must be none: otherwise (default all) a wave of findings is merged into a single run and parallelism is pointless.
	// maxParallel=5: at most 5 report sessions at once, to avoid too many simultaneous LLM calls.
	if err := s.m.pg.SetAgentTriggerBehavior("reporter", "parallel", "none", 5); err != nil {
		log.Printf("[reporter] failed to set the trigger run strategy: %v", err)
	}
	// Bind the tools it needs: write reports + read evidence/execution traces/situation.
	if err := s.m.pg.AddAgentToToolBinding("reporter", []string{
		"update_finding_report", "get_task_node_detail", "list_task_findings",
		"get_task_worker_trace", "list_task_worker_traces", "search_task_worker_traces",
		"get_task_graph",
	}); err != nil {
		log.Printf("[reporter] failed to bind tools: %v", err)
	}
	// Trigger: fires when report_finding is called (the tool returns "finding recorded: <id>" carrying the finding_id,
	// and the task id is in the trigger message too).
	if _, err := s.m.pg.CreateTrigger(&db.AgentTrigger{
		AgentKey:        "reporter",
		Enabled:         true,
		OnToolCall:      true,
		ToolNames:       []string{"report_finding"},
		ToolCallMessage: reporterToolCallMessage,
	}); err != nil {
		log.Printf("[reporter] failed to create trigger: %v", err)
	}
	log.Printf("[reporter] preset the 'Report writer' agent + finding trigger")
}

// seedAutoReportFindingBinding adds "auto" to report_finding's binding ONCE so
// conversation-context agents can call it without requiring an intent_id.
func (s *Server) seedAutoReportFindingBinding() {
	const flag = "auto_report_finding_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("auto", []string{"report_finding"}); err != nil {
		log.Printf("[auto] failed to bind report_finding by default: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedPlannerDefaultBindings adds "planner" to report_finding's binding ONCE
// (guarded by a settings flag), so existing DBs — whose report_finding row was
// seeded as worker-only — also let the planner record findings. Fresh DBs already
// get it via PlannerTools(); this only backfills without overriding a user unbind.
func (s *Server) seedPlannerDefaultBindings() {
	const flag = "planner_report_finding_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"report_finding"}); err != nil {
		log.Printf("[planner] failed to bind report_finding by default: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedPlannerListAssetsBinding adds "planner" to list_assets's binding ONCE
// (guarded by a settings flag), so existing DBs — whose list_assets row was seeded
// as auto/pentest-only — also let the planner query the asset store by DSL. Fresh
// DBs already get it via PlannerTools(); this only backfills without overriding a
// user unbind.
func (s *Server) seedPlannerListAssetsBinding() {
	const flag = "planner_list_assets_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"list_assets"}); err != nil {
		log.Printf("[planner] failed to bind list_assets by default: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedCompanyScopeRebind changes add_company_scope's default binding ONCE on
// existing DBs (guarded by a settings flag): the tool moves off worker and onto
// planner — defining a company's asset scope is a planning/main/auto concern, not
// something a worker does mid-exploration. Fresh DBs already get planner via
// PlannerTools() and lack worker via WorkerTools(); this only backfills old rows.
// One-shot + flag-guarded so a user who later re-binds worker isn't overridden.
func (s *Server) seedCompanyScopeRebind() {
	const flag = "company_scope_rebind_v1" // switch the default binding worker→planner
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("planner", []string{"add_company_scope"}); err != nil {
		log.Printf("[planner] failed to bind add_company_scope by default: %v", err)
		return
	}
	if err := s.m.pg.RemoveAgentFromTool("worker", "add_company_scope"); err != nil {
		log.Printf("[worker] failed to unbind add_company_scope: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedWorkerReadToolsUnbind strips the read-context tools off worker's default
// binding ONCE on existing DBs (guarded by a settings flag): a worker executes one
// intent and writes back — reading facts/companies and listing all workers' traces is
// a planning/main concern, not the executor's. Fresh DBs already lack these via
// WorkerTools(); this only backfills old rows without overriding a user who
// deliberately re-binds worker. Each RemoveAgentFromTool is per-tool +
// membership-guarded, so planner/mainagent bindings of the same tool are untouched.
//
// NOTE: search_all_worker_traces / get_worker_trace / node_detail are intentionally NOT
// unbound — worker owns them for cross-work look-back + node drill-down (see WorkerTools).
// They used to be in this list back when worker lacked them; seedWorkerReadbackRebind
// repairs DBs whose old run stripped them.
func (s *Server) seedWorkerReadToolsUnbind() {
	const flag = "worker_readtools_unbind_v1"
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	for _, k := range []string{
		"list_facts", "list_companies", "list_worker_traces",
	} {
		if err := s.m.pg.RemoveAgentFromTool("worker", k); err != nil {
			log.Printf("[worker] failed to unbind %s from worker: %v", k, err)
			return // on error do not set the flag; retry on next startup
		}
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedWorkerReadbackRebind re-binds the cross-work look-back / drill-down tools onto
// worker ONCE (guarded by a settings flag): an earlier seedWorkerReadToolsUnbind wrongly
// stripped search_all_worker_traces / get_worker_trace / node_detail from worker after
// they had been added to WorkerTools(), so any DB that ran that migration lost them.
// Fresh DBs already have them via WorkerTools() and this is a harmless no-op there.
// One-shot + flag-guarded so a user who later deliberately unbinds them isn't overridden.
func (s *Server) seedWorkerReadbackRebind() {
	const flag = "worker_readback_rebind_v2" // v2: adds node_detail
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	if err := s.m.pg.AddAgentToToolBinding("worker", []string{
		"search_all_worker_traces", "get_worker_trace", "node_detail",
	}); err != nil {
		log.Printf("[worker] failed to re-bind look-back/detail tools: %v", err)
		return // on error do not set the flag; retry on next startup
	}
	_ = s.m.pg.SetSetting(flag, "true")
}

// seedAutoDefaultBindings adds "auto" to the task-op + platform tools' bindings
// ONCE (guarded by a settings flag), so existing DBs whose tool rows were seeded
// before Auto existed still give Auto its default toolset — without re-adding it
// after a user deliberately unbinds.
func (s *Server) seedAutoDefaultBindings() {
	const flag = "auto_default_bindings_v3" // v3: replace old asset tool names, add insert_assets/add_company_scope
	if v, _, _ := s.m.pg.GetSetting(flag); v == "true" {
		return
	}
	keys := make([]string, 0, len(platformToolKeys)+12)
	for _, t := range s.orchestrationTools() {
		keys = append(keys, t.Name())
	}
	keys = append(keys, platformToolKeys...)
	// Asset tools: Auto, operating the platform, often needs to view/register assets and manage company scope.
	keys = append(keys, "insert_assets", "add_company_scope", "list_assets")
	if err := s.m.pg.AddAgentToToolBinding("auto", keys); err != nil {
		log.Printf("[auto] default binding failed: %v", err)
		return
	}
	_ = s.m.pg.SetSetting(flag, "true")
}
