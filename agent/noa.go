package agent

import (
	"log"
	"path/filepath"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/noaadapter"
)

// noaWarn returns a diagnostics sink tagging non-fatal noa messages with the
// session, routed through the package logger (agents have no per-instance one).
func noaWarn(session string) func(string) {
	return func(msg string) { log.Printf("[noa] %s: %s", session, msg) }
}

// noa is the "model-driven context compaction" mechanism introduced in norma v0.4.0,
// which the user switches on or off in System settings as an experimental platform
// feature. It is mutually exclusive with the built-in compaction: noaadapter.Enable is
// the only entry point and attaches the context takeover (Compactor), the Compress
// tool and the three resident prompt sections in one go; not calling Enable means it
// is off (the built-in compaction works as usual). The switch is resolved by the
// noaEnabledFn injected into each agent and read once per run, so toggling it only
// affects runs started afterwards, with no need to rebuild the agent.

// enableNoa wires noa into opts when the resolver reports it enabled. archiveRoot is
// the persistent base directory for the compacted originals (the global workDir: every
// agent lands under <workDir>/noa instead of being scattered across task/intent
// directories), and sessionID names the archive subdirectory under it (globally
// unique, so there are no clashes within the same base directory).
//
// noa is experimental: failing to wire it in must not interrupt a real task. On error
// it reports through onWarn and falls back to the built-in compaction. On success it
// clears opts.Compaction so agentcore does not warn about "two context managers set at
// the same time".
func enableNoa(opts *agentcore.Options, enabled func() bool, archiveRoot, sessionID string, onWarn func(string)) {
	if enabled == nil || !enabled() {
		return
	}
	if opts.OnWarn == nil {
		opts.OnWarn = onWarn
	}
	if err := noaadapter.Enable(opts, noaadapter.Options{
		ArchiveBaseDir: filepath.Join(archiveRoot, "noa"),
		SessionID:      sessionID,
		OnWarn:         onWarn,
	}); err != nil {
		if onWarn != nil {
			onWarn("failed to enable noa compaction, falling back to built-in compaction: " + err.Error())
		}
		return
	}
	// Compactor overrides Compaction, but agentcore warns every time both are set; clear it explicitly.
	opts.Compaction = nil
}
