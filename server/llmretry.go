package server

import (
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// Server-side resolution of the retry policy, see docs/LLM-retry-design.md. Of the five layers:
//   - connect / empty response / same-provider safe window "follow the endpoint": each LLM config can
//     override the global default (a field left empty on the profile inherits the global value, and when the
//     global one is not set either, the built-in default applies);
//   - circuit breaker / intent rerun are process-wide, with a single global copy.
//
// Reading the global policy costs one DB settings row and every call site is on a low-frequency path (building
// a provider, work wrap-up, saving a config), so another cache layer is not worth it; the circuit-breaker
// parameters are the exception: they are read on every pass through the failure path, so applyRetryPolicy
// pushes them to the Registry to keep.

// retryPolicy reads the global policy; a nil DB yields the zero policy (all
// layers on their built-in defaults).
func (s *Server) retryPolicy() db.LLMRetryPolicy {
	if s.m == nil || s.m.pg == nil {
		return db.LLMRetryPolicy{}
	}
	return s.m.pg.LLMRetryPolicy()
}

// resolveRetry layers one profile's override on top of the global policy and
// converts the result into the form agent.Config carries. Rules combine field by
// field, so a profile that only pins an interval still inherits the global count.
func resolveRetry(o db.RetryOverride, pol db.LLMRetryPolicy) agent.RetryConfig {
	connect := o.Connect.Or(pol.Connect)
	empty := o.Empty.Or(pol.Empty)
	stream := o.Stream.Or(pol.Stream)
	return agent.RetryConfig{
		// Counts keep their raw "0 = default / negative = off" semantics here: the SDK's MaxRetries /
		// EmptyResponseRetries work exactly the same way, so let the SDK resolve them itself.
		ConnectAttempts: connect.Attempts, ConnectInterval: connect.Interval(),
		EmptyAttempts: empty.Attempts, EmptyInterval: empty.Interval(),
		StreamAttempts: stream.Attempts, StreamInterval: stream.Interval(),
	}
}

// applyProfileRetry fills cfg.Retry for a profile read from the DB.
func (s *Server) applyProfileRetry(cfg *agent.Config, p *db.LLMProfile) {
	if p == nil {
		return
	}
	cfg.Retry = resolveRetry(p.Retry, s.retryPolicy())
}

// The circuit-breaker (LLM pool cooldown) defaults match the ones built into llmpool; they are only overridden
// here when "the user has configured a value".
// For the intent rerun defaults, see modelErrorRetries / modelErrorRetryBackoff in engine.go.

// applyRetryPolicy pushes the process-wide layers of the policy into the objects
// that consume them on a hot path: the circuit-breaker registry. Called at
// startup and whenever the policy is saved.
func (s *Server) applyRetryPolicy() {
	pol := s.retryPolicy()
	if s.llmHealth != nil {
		s.llmHealth.SetPolicy(pol.Breaker.Attempts, pol.Breaker.Interval())
	}
}

// modelErrorRetryPolicy resolves the intent-level replay knobs (layer ⑤): how
// many times a model_error work is re-run and how long to back off between runs.
func (e *Engine) modelErrorRetryPolicy() (retries int, backoff time.Duration) {
	retries, backoff = modelErrorRetries, modelErrorRetryBackoff
	if e == nil || e.m == nil || e.m.pg == nil {
		return retries, backoff
	}
	rule := e.m.pg.LLMRetryPolicy().Intent
	if rule.Attempts != 0 {
		retries = max(rule.Attempts, 0)
	}
	if d := rule.Interval(); d > 0 {
		backoff = d
	}
	return retries, backoff
}

// emptyTurnNudgeLimit resolves how many empty-turn continuations one work may
// inject (see steerHooks.Stop). It deliberately reuses layer ②'s knob, the "empty
// response retry" count: both are means to the same end. The SDK layer handles "not a single content block",
// and its remedy is to resend the same request as-is; this one handles "thinking only, with neither body text
// nor tools", and its remedy is to append an instruction so the model carries on from the thinking it already
// has (resending as-is is pointless for an empty turn determined by the shape of the context). The emptiness
// criteria differ because the SDK goes by "whether any event was yielded", and thinking deltas are events in
// their own right; but when users configure "how many times to retry an empty response", what they mean is "if
// the model produced nothing substantive, go again", so the two layers share one count to match that mental
// model.
//
// It reads the global policy rather than a profile's override: a run may switch profiles midway due to
// failover, and this is the total cap for the whole intent, which should not change with the endpoint. The
// semantics mirror the SDK's emptyRetries(): 0 = default defaultEmptyTurnNudges; -1 (negative) = turn off
// empty-turn continuation; >0 = use that value.
func (e *Engine) emptyTurnNudgeLimit() int {
	if e == nil || e.m == nil || e.m.pg == nil {
		return defaultEmptyTurnNudges
	}
	switch n := e.m.pg.LLMRetryPolicy().Empty.Attempts; {
	case n == 0:
		return defaultEmptyTurnNudges
	case n < 0:
		return 0
	default:
		return n
	}
}
