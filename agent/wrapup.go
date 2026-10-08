package agent

import (
	"strings"

	"github.com/Autumn-27/norma/harness"
)

// Wrap-up prompt (wrap-up / settlement prompt): when an agent is terminated because it
// ran out of steps (MaxTurns) or timed out (run_seconds/MaxDuration), the SDK's
// settlement phase injects this prompt so the agent first persists what it has
// identified but not yet written back, then outputs a one-sentence summary, instead of
// leaving the work half-finished.
//
// Each agent's wrap-up prompt can be overridden in the admin console as needed (stored
// in agents.wrapup_prompt); left empty, the built-in default here is used. Only the
// prompt body is editable; which tools are disabled and how many turns the wrap-up
// itself gets are code-owned policy.

// WrapupOverride, if set, returns the stored wrap-up prompt for an agent key and
// whether a non-empty one exists. Wired by the server to the agents table (like
// PromptOverride for system prompts). nil / empty → the built-in default is used.
var WrapupOverride func(agentKey string) (string, bool)

// WrapupMaxTurnsOverride, if set, returns the admin-configured turn budget for the
// wrap-up phase of an agent and whether a positive one exists. Wired to the agents
// table. nil / ≤0 → the built-in per-agent default (wrapupTurnDefaults) is used.
var WrapupMaxTurnsOverride func(agentKey string) (int, bool)

// Built-in default wrap-up prompts, keyed by agent key. worker reuses the historically
// hard-coded settleWrapUpPrompt (defined in worker.go) and planner/mainagent each have
// their own; anything else (custom agents) falls back to the generic default.
var wrapupDefaults = map[string]string{
	"worker":    settleWrapUpPrompt,
	"planner":   plannerWrapUpDefault,
	"mainagent": mainAgentWrapUpDefault,
}

// wrapupTurnDefaults: built-in default turn budget for the wrap-up phase itself of each
// agent (an admin value >0 overrides it). All get 10 turns so the wrap-up phase has
// enough steps to persist results. Anything else uses genericWrapupTurns.
var wrapupTurnDefaults = map[string]int{
	"worker":    10,
	"planner":   10,
	"mainagent": 10,
}

const genericWrapupTurns = 10

const plannerWrapUpDefault = "The planning steps for this round are about to run out. Note that only **this round** is ending: the system will wake you again to keep planning as the situation changes, so the task is not ending and you do not need to wrap up the whole plan here. Act on the conclusions you have already worked out this round so the round is not wasted, but **do not force intents just to wrap up** (0 intents this round is still a perfectly normal result): (1) if you have identified exploration directions that **should be dispatched now**, submit them in one batched add_intent call (do not hold back the ones you have already worked out); (2) for goals already proven met by a finding/fact, call prove_goal to mark them met (do not miss any); (3) if you identified a serial exploitation chain that must be done step by step, record it with TodoWrite so you can continue dispatching it on the next wake-up. When done, simply end this round; no summary text is needed."

const mainAgentWrapUpDefault = "Your steps are about to run out and this interaction is about to end. Do not start any new exploration/operations. **In a single standalone sentence of plain text**, summarise for the user the current progress, the key conclusions and the suggested next step."

const genericWrapUpDefault = "You are about to be terminated because your budget is exhausted. First write back any completed results that have not been persisted yet, then **in a single standalone sentence of plain text** summarise what you did and the key conclusions you reached (this sentence is shown as the result of this run)."

// WrapupDefault returns the built-in default wrap-up prompt for an agent key —
// used by the admin UI as the "restore default" value and empty-field placeholder.
func WrapupDefault(agentKey string) string {
	if d, ok := wrapupDefaults[agentKey]; ok {
		return d
	}
	return genericWrapUpDefault
}

// WrapupTurnsDefault returns the built-in wrap-up turn budget for an agent key —
// used by the admin UI as the "0 = default N" hint.
func WrapupTurnsDefault(agentKey string) int {
	if n, ok := wrapupTurnDefaults[agentKey]; ok {
		return n
	}
	return genericWrapupTurns
}

// resolveWrapup returns the effective wrap-up prompt: the DB override (if set and
// non-empty) over the built-in default.
func resolveWrapup(agentKey string) string {
	if WrapupOverride != nil {
		if t, ok := WrapupOverride(agentKey); ok && strings.TrimSpace(t) != "" {
			return t
		}
	}
	return WrapupDefault(agentKey)
}

// resolveWrapupTurns returns the effective wrap-up turn budget: a positive DB
// override over the built-in per-agent default.
func resolveWrapupTurns(agentKey string) int {
	if WrapupMaxTurnsOverride != nil {
		if v, ok := WrapupMaxTurnsOverride(agentKey); ok && v > 0 {
			return v
		}
	}
	return WrapupTurnsDefault(agentKey)
}

// wrapupSettlement builds the settlement config for an agent's run. Prompt and the
// turn budget are admin-editable per agent; disabled tools are code-owned policy so
// a user can't edit away the "stop probing" guardrail. Resolved fresh each run
// (reads DB live), so edits apply on the next run without a restart.
func wrapupSettlement(agentKey string, disabledTools []string) *harness.Settlement {
	return &harness.Settlement{
		Prompt:        resolveWrapup(agentKey),
		DisabledTools: disabledTools,
		MaxTurns:      resolveWrapupTurns(agentKey),
	}
}

// ---------- Task-level timeout wrap-up prompts (see docs/task-level-timeout-and-wrapup-design.md) ----------
//
// These are a separate set from the per-run wrap-up prompts: per-run means "the budget
// for this one run of yours is used up"; task timeout means "the whole task is out of
// time and about to end". Their meanings are often opposite (especially for planner:
// per-run says "don't stop, keep planning", task timeout says "time is up, stop planning
// and make the final call"). Only configured for worker/planner.

// WrapupTaskTimeoutOverride / ...TurnsOverride: DB overrides for the task-timeout
// wrap-up prompt and turn count (wired to agents.task_timeout_wrapup_prompt /
// _max_turns, worker/planner only).
var (
	WrapupTaskTimeoutOverride      func(agentKey string) (string, bool)
	WrapupTaskTimeoutTurnsOverride func(agentKey string) (int, bool)
)

var taskTimeoutWrapupDefaults = map[string]string{
	"worker":  workerTaskTimeoutDefault,
	"planner": plannerTaskTimeoutDefault,
}

const workerTaskTimeoutDefault = "**The whole task has reached its timeout limit and is about to end** (this is not the budget of your current run: the whole exploration is out of time). This is your last chance: (1) persist **everything** you have identified but not yet written back: new assets via insert_assets, exploration conclusions/facts via record_fact, confirmed vulnerabilities via report_finding; (2) do not start any new commands/probes; (3) **finally, in a single standalone sentence of plain text**, summarise your key conclusions on this intent."

const plannerTaskTimeoutDefault = "**The whole task has reached its timeout limit and is about to end** (not just this round: the whole task is terminating). Based on **all** current facts and findings, make one final goal assessment: for goals already proven met by evidence, call prove_goal to mark them met (do not miss any). **Do not generate any new intents** (intents dispatched now will no longer be executed). Once the assessment is done, wrap up; no summary text is needed."

// TaskTimeoutWrapupDefault returns an agent's built-in default task-timeout wrap-up
// prompt (for the admin placeholder / restore default).
func TaskTimeoutWrapupDefault(agentKey string) string {
	return taskTimeoutWrapupDefaults[agentKey] // not configured (mainagent/chat): returns an empty string
}

// resolveTaskTimeoutWrapup: DB override (non-empty) > built-in default. An empty
// string means the agent has no task-timeout prompt (not worker/planner), in which
// case the caller should fall back to the per-run prompt.
func resolveTaskTimeoutWrapup(agentKey string) string {
	if WrapupTaskTimeoutOverride != nil {
		if t, ok := WrapupTaskTimeoutOverride(agentKey); ok && strings.TrimSpace(t) != "" {
			return t
		}
	}
	return TaskTimeoutWrapupDefault(agentKey)
}

func resolveTaskTimeoutTurns(agentKey string) int {
	if WrapupTaskTimeoutTurnsOverride != nil {
		if v, ok := WrapupTaskTimeoutTurnsOverride(agentKey); ok && v > 0 {
			return v
		}
	}
	return resolveWrapupTurns(agentKey) // defaults to the per-run turn count
}

// wrapupSettlementForTask builds settlement for a worker/planner run that is aware
// of the task deadline. See §5 of the design doc:
//   - clamped=true  → this run is squeezed by the task deadline: wrap-up because of
//     Timeout = the task is out of time → task-timeout prompt; wrap-up because of
//     MaxTurns = steps ran out first within the squeezed window and the task still has a
//     few minutes left → fall back to the per-run prompt.
//   - clamped=false → the task still has plenty of time: both reasons use the per-run
//     prompt (i.e. it degrades to wrapupSettlement).
//
// The PromptByReason handed to the harness picks by the actual reason on the spot at
// wrap-up time, so nothing is mismatched at build time.
func wrapupSettlementForTask(agentKey string, disabledTools []string, clamped bool) *harness.Settlement {
	perRun := resolveWrapup(agentKey)
	st := &harness.Settlement{
		Prompt:        perRun, // fallback (also the value for both reasons when not clamped)
		DisabledTools: disabledTools,
		MaxTurns:      resolveWrapupTurns(agentKey),
	}
	if clamped {
		if tt := resolveTaskTimeoutWrapup(agentKey); tt != "" {
			st.PromptByReason = map[harness.TerminalReason]string{
				harness.ReasonTimeout:  tt,     // the task is out of time
				harness.ReasonMaxTurns: perRun, // steps ran out first, the task still has time
			}
			st.MaxTurns = resolveTaskTimeoutTurns(agentKey)
		}
	}
	return st
}
