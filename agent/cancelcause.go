package agent

import (
	"context"
	"errors"
	"fmt"
)

// AbortCause names why an agent run's context was cancelled. Every cancellation
// site should attach one so the activity trace can report the real initiator.
type AbortCause struct {
	Code  string
	Short string
	Text  string
}

func (c *AbortCause) Error() string { return c.Text }

func cause(code, short, text string) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: text}
}

// Causef builds a cause that includes runtime-specific detail.
func Causef(code, short, format string, args ...any) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: fmt.Sprintf(format, args...)}
}

var (
	// Task-level execution context.
	AbortPausedByUser = cause("paused_by_user", "Task paused by the user",
		"The user paused the task through the task control API (POST /api/tasks/{id}/control, action=pause). This Planner/Worker run was cancelled on purpose; running intents return to the frontier (open) and are claimed again and re-run from the start once the task resumes")
	AbortPausedByOrchestrator = cause("paused_by_orchestrator", "Task paused by the orchestration agent",
		"The orchestration agent called the pause_task tool to pause this task. This Planner/Worker run was cancelled on purpose; running intents return to the frontier (open) and run again once the task resumes")
	AbortTaskDeleted = cause("task_deleted", "Task deleted",
		"The task is being deleted (DELETE /api/tasks/{id}) and the deletion barrier has cancelled its running Planner, Worker and main agent; the results of this run will not be used")
	AbortPausedOnReload = cause("paused_on_reload", "Task pause restored on backend startup",
		"On startup the backend restored the task's paused state from the status persisted in the database. This run was cancelled; normally no agent is running during the restore phase")
	AbortGoalMet = cause("goal_met", "Planner judged the task goal met",
		"The planner judged the task goal met and set the task to done, then cancelled the Workers still running; those intents are marked stopped, not failed")
	AbortSettleDrainTimeout = cause("settle_drain_timeout", "Task timeout wrap-up wait ran out",
		"After the task hit its timeout, the engine waited for running Workers to wrap up gracefully, but the 90-second drain grace period was not enough, so it hard-cancelled them; the intents are marked exhausted, and facts and assets already written during wrap-up are kept")

	// Per-work context.
	AbortKilledByPlanner = cause("killed_by_planner", "Intent killed by the planner",
		"The planner called kill_work to terminate this intent on purpose, which usually means the direction drifted or is no longer worth pursuing; the intent is marked stopped and is not claimed again automatically")
	AbortWorkPausedByUser = cause("work_paused_by_user", "Worker intent paused by the user",
		"The user paused the running Worker. This call was cancelled and the intent changes to paused; registered intents, facts, vulnerabilities and activity records are all kept, and it re-runs from the start once resumed")
	AbortWorkCancelledByUser = cause("work_cancelled_by_user", "Worker intent deleted by the user",
		"The user deleted the running Worker. This call was cancelled; once the Worker leaves its write section, the server handles the intent according to the deletion mode the user chose: a soft delete only marks it deleted and keeps all its output, while a hard delete cascades to remove the intent and the downstream nodes supported only by it")
	AbortWorkFinished = cause("work_finished", "Worker finished and released context",
		"The Worker finished normally and the engine released its context resources in detachWork. This is not a run interruption; if it appears in an interruption message, the cancellation raced with the finish event")
	AbortPausedRaceGuard = cause("paused_race_guard", "New run refused while task is paused",
		"While the task is paused, the engine refuses to issue a new execution context, so a race between claim and pause cannot let a Worker start anyway; already claimed intents return to the frontier")

	// Main Agent and standalone conversation contexts.
	AbortChatStoppedByUser = cause("chat_stopped_by_user", "Chat turn stopped by the user",
		"The user clicked stop, deliberately aborting this main agent or conversation agent run. Activity records already produced are kept, and the next message can still be sent")
	AbortChatPausedWithTask = cause("chat_paused_with_task", "Task pause aborted the main agent chat",
		"When the user paused the task, the running main agent chat was cancelled along with it. Activity records already produced are kept; this turn's message is not replayed automatically after the task resumes")
	AbortChatTurnFinished = cause("chat_turn_finished", "Chat turn finished and released context",
		"This chat turn finished normally and the server is releasing the turn's context resources. This is not a run interruption; if it appears in an interruption message, the cancellation raced with the finish event")

	// Process-level and per-run hard backstop.
	AbortShutdown = cause("shutdown", "Backend process shutting down",
		"The backend process received SIGINT or SIGTERM and is restarting, updating or shutting down. All running agents are cancelled; running intents left over are reset to open after the restart and run again")
	AbortRunHardTimeout = cause("run_hard_timeout", "Per-run hard timeout backstop fired",
		"A single run exceeded its soft wall-clock budget plus the extra grace period, which means a model request or a tool did not return for a long time, so the normal wrap-up at a turn boundary could not run. Focus on the last tool call that had not returned before the interruption")
)

// AbortReason resolves the named cause attached to a cancelled run context.
func AbortReason(ctx context.Context) (code, short, text string, ok bool) {
	c := context.Cause(ctx)
	if c == nil {
		return "", "", "", false
	}
	var ac *AbortCause
	if errors.As(c, &ac) {
		return ac.Code, ac.Short, ac.Text, true
	}
	switch {
	case errors.Is(c, context.DeadlineExceeded):
		return "deadline_exceeded", "Upstream context hit its deadline",
			"The upstream context hit its deadline, but whoever set it attached no named cause via WithTimeoutCause: " + c.Error(), true
	case errors.Is(c, context.Canceled):
		return "canceled_no_cause", "Canceller attached no named cause",
			"The upstream context was cancelled, but the canceller attached no named cause via context.WithCancelCause; register a cause in agent/cancelcause.go and wire it into that cancellation site", true
	default:
		return "other", firstLine(c.Error(), 80), c.Error(), true
	}
}
