package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/Autumn-27/artex/selfupdate"
)

// The HTTP surface of one-click update from the web UI. The real download/verify/swap
// logic all lives in the selfupdate package; this file only handles the auth
// boundary, mutual exclusion, progress broadcasting, and telling main "it is time to
// exit".
//
// The restart is not done by this process: once the new version is staged, the
// process exits with selfupdate.ExitRestart and the supervisor script (start.sh /
// start.bat, or the ENTRYPOINT under Docker) starts it again.

// restartCh is closed once the upgrade is ready or a rollback has completed; main then
// exits with ExitRestart.
var (
	restartOnce sync.Once
	restartCh   = make(chan struct{})
)

// RestartRequested returns a channel that is closed when it is time to "exit and let
// the supervisor start me again".
func RestartRequested() <-chan struct{} { return restartCh }

func requestRestart() { restartOnce.Do(func() { close(restartCh) }) }

// bootState is the conclusion selfupdate.Bootstrap reached at this start (upgrade
// succeeded / just rolled back / staged file discarded). main injects it so that
// /api/update/check can tell the frontend truthfully how the last upgrade ended.
var (
	bootStateMu sync.Mutex
	bootState   selfupdate.State
)

// SetBootUpdateState is called once by main at startup.
func SetBootUpdateState(st selfupdate.State) {
	bootStateMu.Lock()
	defer bootStateMu.Unlock()
	bootState = st
}

func bootUpdateState() selfupdate.State {
	bootStateMu.Lock()
	defer bootStateMu.Unlock()
	return bootState
}

// releaseCache caches the result of the latest-release query against GitHub.
//
// The top bar's "new version available" notice queries once on every full page load,
// and unauthenticated GitHub API calls are limited to 60 per IP per hour. Without a
// cache, opening a few tabs or refreshing a few times would use up the quota, and
// then a real attempt to update could no longer query at all. When the user
// explicitly clicks "Check for updates", force can bypass the cache.
type releaseCache struct {
	mu  sync.Mutex
	rel *selfupdate.Release
	err error
	at  time.Time
	// fetch is the fetch function, an injection point kept only for tests; when nil,
	// the real GitHub query is used.
	fetch func(context.Context, *http.Client) (*selfupdate.Release, error)
}

const (
	releaseTTL = 30 * time.Minute
	// Failed results are cached briefly too, otherwise every page load would sit
	// through a timeout while GitHub is unreachable; but the TTL must be short so it
	// recovers on its own soon after the network comes back.
	releaseErrTTL = 2 * time.Minute
	// Timeout for the query. NewClient's 30-minute timeout is for downloading the whole
	// package; a version check cannot wait that long.
	releaseTimeout = 20 * time.Second
)

var relCache = &releaseCache{}

// get returns the latest Release, without touching the network on a cache hit.
//
// The lock is held for the whole fetch: concurrent requests queue up for the result
// of the same query instead of each hitting GitHub (several tabs querying at once
// right after the page loads is exactly when the rate limit is easiest to hit).
func (c *releaseCache) get(ctx context.Context, client *http.Client, force bool) (*selfupdate.Release, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !force {
		ttl := releaseTTL
		if c.err != nil {
			ttl = releaseErrTTL
		}
		if !c.at.IsZero() && time.Since(c.at) < ttl {
			return c.rel, c.err
		}
	}

	fetch := c.fetch
	if fetch == nil {
		fetch = selfupdate.FetchLatest
	}
	ctx, cancel := context.WithTimeout(ctx, releaseTimeout)
	defer cancel()
	rel, err := fetch(ctx, client)
	// A cancelled request (the user closed the tab) does not mean GitHub has a problem,
	// so do not write it into the cache, or the next visitor would get a baffling
	// "cancelled" error.
	if err != nil && ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled) {
		return c.rel, err
	}
	c.rel, c.err, c.at = rel, err, time.Now()
	return rel, err
}

// updateProgress is one progress update pushed to the frontend.
type updateProgress struct {
	Phase   selfupdate.Phase `json:"phase"`
	Percent int              `json:"percent"` // only meaningful during download; -1 otherwise
	Message string           `json:"message"`
	Version string           `json:"version,omitempty"`
	Error   string           `json:"error,omitempty"`
}

// updateHub holds the progress of one upgrade and broadcasts it to SSE subscribers.
//
// running doubles as the mutex: another POST /api/update/apply during an upgrade
// gets a 409 straight away, so two goroutines never write to the same artex.new at
// once.
type updateHub struct {
	mu      sync.Mutex
	running bool
	cur     updateProgress
	subs    map[chan updateProgress]struct{}
}

var updHub = &updateHub{
	cur:  updateProgress{Phase: selfupdate.PhaseIdle, Percent: -1},
	subs: map[chan updateProgress]struct{}{},
}

// begin claims the right to upgrade, returning false if one is already in progress.
func (h *updateHub) begin(version string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.running {
		return false
	}
	h.running = true
	h.cur = updateProgress{Phase: selfupdate.PhaseDownload, Percent: 0, Message: "Preparing...", Version: version}
	h.fanout(h.cur)
	return true
}

// finish ends an upgrade. A nil err means the new version is staged and waiting for the
// restart.
func (h *updateHub) finish(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.running = false
	if err != nil {
		h.cur = updateProgress{Phase: selfupdate.PhaseFailed, Percent: -1, Message: "Update failed", Error: err.Error(), Version: h.cur.Version}
	} else {
		h.cur = updateProgress{Phase: selfupdate.PhaseStaged, Percent: 100, Message: "New version ready, restarting...", Version: h.cur.Version}
	}
	h.fanout(h.cur)
}

func (h *updateHub) publish(ph selfupdate.Phase, pct int, msg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cur = updateProgress{Phase: ph, Percent: pct, Message: msg, Version: h.cur.Version}
	h.fanout(h.cur)
}

// fanout must be called with h.mu held. Subscriber channels are buffered and are
// skipped when full: progress is disposable, transient information, and a stuck SSE
// connection must never block the upgrade itself.
func (h *updateHub) fanout(p updateProgress) {
	for ch := range h.subs {
		select {
		case ch <- p:
		default:
		}
	}
}

func (h *updateHub) snapshot() (updateProgress, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cur, h.running
}

func (h *updateHub) subscribe() (<-chan updateProgress, func()) {
	ch := make(chan updateProgress, 64)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, ch)
			h.mu.Unlock()
			close(ch)
		})
	}
}

// updateCheck queries GitHub for the latest release and compares it with the current
// version.
//
// The frontend also talks to api.github.com directly (GitHub's CORS is *), but **this
// endpoint is authoritative**: the download is done by the backend, so an update is
// only possible if the backend can reach GitHub. It is common for the browser to
// reach it while the server cannot (the server is on an internal network, or a proxy
// is only configured in the browser); an update would then certainly fail, so it is
// better to report the error truthfully at the check stage.
func (s *Server) updateCheck(w http.ResponseWriter, r *http.Request) {
	current := BuildVersion
	mode := "binary"
	if selfupdate.InDocker() {
		mode = "docker"
	}
	boot := bootUpdateState()
	out := map[string]any{
		"current":     current,
		"mode":        mode,
		"os":          runtime.GOOS,
		"arch":        runtime.GOARCH,
		"has_backup":  selfupdate.HasBackup(),
		"repo":        selfupdate.Repo,
		"boot_notice": boot.Detail,
		"rolled_back": boot.RolledBack,
	}

	// The top bar notice uses the cache (the default); when the user clicks "Check for
	// updates", force=1 forces a fresh fetch.
	force := r.URL.Query().Get("force") != ""
	client := selfupdate.NewClient(s.m.GlobalProxy())
	rel, err := relCache.get(r.Context(), client, force)
	if err != nil {
		out["error"] = err.Error()
		writeJSON(w, 200, out)
		return
	}

	latest := rel.TagName
	out["latest"] = latest
	out["notes"] = rel.Body
	out["html_url"] = rel.HTMLURL
	if !rel.PublishedAt.IsZero() {
		out["published_at"] = rel.PublishedAt.Format(time.RFC3339)
	}

	asset := selfupdate.AssetName(latest, runtime.GOOS, runtime.GOARCH)
	out["asset"] = asset
	if a, ok := rel.FindAsset(asset); ok {
		out["asset_available"] = true
		out["size"] = a.Size
	} else {
		out["asset_available"] = false
	}

	cmp, comparable := selfupdate.CompareVersions(current, latest)
	out["comparable"] = comparable
	out["has_update"] = comparable && cmp < 0
	if !comparable {
		// Development builds (dev / git describe with a suffix) have no comparable version.
		// Allowing an update would only overwrite the binary being debugged locally with
		// a release, so no update is offered at all.
		out["reason"] = fmt.Sprintf("Current version %q is not an official release; one-click update is disabled", current)
	}
	writeJSON(w, 200, out)
}

// updateApply downloads and stages the new version, then has the process exit so the
// supervisor script restarts it.
//
// It returns 202 straight away and does the real work on a background goroutine:
// downloading the whole package can take minutes, and hanging it off the request
// would get it cut off by a reverse proxy timeout. Progress goes through
// /api/update/stream.
func (s *Server) updateApply(w http.ResponseWriter, r *http.Request) {
	current := BuildVersion

	// Use the cache: this guarantees that what gets installed is the version the user
	// saw and confirmed in the UI.
	client := selfupdate.NewClient(s.m.GlobalProxy())
	rel, err := relCache.get(r.Context(), client, false)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	cmp, comparable := selfupdate.CompareVersions(current, rel.TagName)
	if !comparable {
		writeErr(w, 400, fmt.Sprintf("Current version %q is not an official release; one-click update is disabled", current))
		return
	}
	if cmp >= 0 {
		writeErr(w, 400, fmt.Sprintf("Already on the latest version %s", current))
		return
	}
	if !updHub.begin(rel.TagName) {
		writeErr(w, 409, "An update is already in progress")
		return
	}

	go func() {
		// s.ctx is used deliberately instead of the request ctx: the request ends as soon
		// as the HTTP response is returned, so a download tied to it would be cancelled
		// immediately.
		err := selfupdate.Stage(s.ctx, client, rel, current, func(ph selfupdate.Phase, pct int, msg string) {
			updHub.publish(ph, pct, msg)
		})
		updHub.finish(err)
		if err != nil {
			log.Printf("[update] update failed: %v", err)
			return
		}
		log.Printf("[update] %s → %s staged, exiting to complete the swap", current, rel.TagName)
		// Leave a little time to push the last progress update to the frontend before
		// triggering the exit.
		time.Sleep(1500 * time.Millisecond)
		requestRestart()
	}()

	writeJSON(w, 202, map[string]any{"ok": true, "target": rel.TagName})
}

// updateRollback deliberately reverts to the previous version (artex.old, backed up
// before the swap).
func (s *Server) updateRollback(w http.ResponseWriter, r *http.Request) {
	if _, running := updHub.snapshot(); running {
		writeErr(w, 409, "Cannot roll back while an update is in progress")
		return
	}
	if err := selfupdate.Rollback(); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	log.Printf("[update] manually rolled back to the previous version, exiting to complete the switch")
	writeJSON(w, 202, map[string]any{"ok": true})
	go func() {
		time.Sleep(500 * time.Millisecond)
		requestRestart()
	}()
}

// updateStream pushes update progress over SSE.
func (s *Server) updateStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch, unsub := updHub.subscribe()
	defer unsub()

	send := func(p updateProgress) {
		b, _ := json.Marshal(p)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}
	// Send the current state first, so a refreshed page immediately sees an upgrade in
	// progress.
	cur, _ := updHub.snapshot()
	send(cur)

	ctx := r.Context()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case p, ok := <-ch:
			if !ok {
				return
			}
			send(p)
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}
