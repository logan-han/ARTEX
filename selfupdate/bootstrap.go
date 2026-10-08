package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

// smokeEnv makes a child process started by the smoke test skip Bootstrap entirely.
//
// Strictly speaking nothing would go wrong without it: the child's os.Executable()
// is artex.new, so every path derived from it carries the .new prefix and never
// touches the real upgrade files. But relying on that coincidence is too fragile; an
// explicit short circuit is obvious at a glance and also saves the child a pointless
// disk probe.
const smokeEnv = "ARTEX_SELFUPDATE_SMOKE"

// Action is Bootstrap's instruction to main.
type Action int

const (
	// Continue: start the server as usual.
	Continue Action = iota
	// Restart: exit immediately with ExitRestart so the supervisor script starts the
	// process again.
	Restart
)

// State describes the upgrade state for this start, so /api/update/check can tell
// the frontend truthfully "whether the last upgrade succeeded or was rolled back".
type State struct {
	Pending     bool   // swapped in but not yet confirmed stable
	RolledBack  bool   // this start has just performed an automatic rollback
	FailedStage bool   // the staged file failed verification/smoke test and was discarded
	Detail      string // one-sentence explanation for the user
}

// Bootstrap runs at the very start of main and must be called before any port is
// opened for listening and before the database is opened.
//
// Three situations:
//
//	(1) staged file artex.new exists → verify + smoke test; if it passes, swap and request a restart; if not, discard it and keep running the old version
//	(2) only the marker file is left  → a swap has just happened; count one attempt, and roll back after enough consecutive failures
//	(3) nothing                       → normal start
func Bootstrap() (Action, State) {
	if os.Getenv(smokeEnv) != "" {
		return Continue, State{}
	}
	p, err := ResolvePaths()
	if err != nil {
		log.Printf("[update] skipping bootstrap: %v", err)
		return Continue, State{}
	}

	if _, err := os.Stat(p.New); err == nil {
		return applyStaged(p)
	}

	m, ok := readMarker(p.Marker)
	if !ok {
		return Continue, State{}
	}
	return confirmOrRollback(p, m)
}

// applyStaged handles the "staged file exists" situation: swap if verification
// passes, discard if it fails.
//
// This is the only place in the whole upgrade path that overwrites the executable,
// and also the last gate: the smoke test stops problems such as a corrupted
// download, the wrong architecture or missing dynamic libraries. Once a binary that
// cannot run is let through, the supervisor script will tirelessly keep restarting
// it while the Go code never gets a chance to run, so automatic rollback is out of
// the question.
func applyStaged(p Paths) (Action, State) {
	m, _ := readMarker(p.Marker)

	if err := verifyStaged(p); err != nil {
		log.Printf("[update] staged new version failed verification and was discarded; continuing with the current version: %v", err)
		cleanStaged(p)
		_ = os.Remove(p.Marker)
		return Continue, State{FailedStage: true, Detail: "New version failed verification and was discarded: " + err.Error()}
	}

	if err := swap(p); err != nil {
		log.Printf("[update] swap failed; continuing with the current version: %v", err)
		cleanStaged(p)
		_ = os.Remove(p.Marker)
		return Continue, State{FailedStage: true, Detail: "Failed to swap in the new version: " + err.Error()}
	}

	// Swap succeeded. Keep the marker so the next start (which runs the new version)
	// can confirm it is stable.
	m.Attempts = 0
	if m.StagedAt == 0 {
		m.StagedAt = time.Now().Unix()
	}
	if err := writeMarker(p.Marker, m); err != nil {
		log.Printf("[update] failed to write the upgrade marker (automatic rollback unavailable): %v", err)
	}
	log.Printf("[update] swapped to %s, exiting to restart (exit %d)", orUnknown(m.To), ExitRestart)
	return Restart, State{Pending: true}
}

// confirmOrRollback handles "a start after a swap": it counts attempts and swaps
// the old version back in once the limit is exceeded.
//
// The count only increments once the Go code is running, so it covers failures of
// the "can execute but crashes during initialisation" kind (incompatible config,
// port in use, DB migration blew up); "cannot exec at all" is caught by the smoke
// test before the swap. Only the two together give complete coverage.
func confirmOrRollback(p Paths, m marker) (Action, State) {
	m.Attempts++
	if m.Attempts > maxAttempts {
		if err := rollback(p); err != nil {
			// If even the rollback failed, stop restarting, or we would loop forever.
			// Clear the marker and let the process start as it is; if it cannot, the
			// user can at least see why in the log.
			log.Printf("[update] new version failed to start %d times in a row, and the rollback failed: %v", maxAttempts, err)
			_ = os.Remove(p.Marker)
			return Continue, State{Detail: "New version failed to start and the rollback failed: " + err.Error()}
		}
		log.Printf("[update] new version failed to start %d times in a row; rolled back to %s, exiting to restart (exit %d)",
			maxAttempts, orUnknown(m.From), ExitRestart)
		_ = os.Remove(p.Marker)
		return Restart, State{RolledBack: true, Detail: fmt.Sprintf("New version failed to start; rolled back to %s", orUnknown(m.From))}
	}
	if err := writeMarker(p.Marker, m); err != nil {
		log.Printf("[update] failed to update the upgrade marker: %v", err)
	}
	log.Printf("[update] new version starting (attempt %d/%d); the upgrade will be confirmed once it runs stably",
		m.Attempts, maxAttempts)
	return Continue, State{Pending: true}
}

// Settle confirms that the new version is running stably and clears the upgrade
// marker.
//
// main calls it after a delay, once the HTTP listener is up: only surviving that
// long counts. Otherwise the marker stays where it is and the next start keeps
// counting attempts until a rollback is triggered.
func Settle() {
	p, err := ResolvePaths()
	if err != nil {
		return
	}
	settle(p)
}

func settle(p Paths) {
	if _, ok := readMarker(p.Marker); !ok {
		return // not a start after an upgrade; nothing to do
	}
	if err := os.Remove(p.Marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("[update] failed to clear the upgrade marker: %v", err)
		return
	}
	log.Printf("[update] new version is running stably; upgrade complete (previous version kept as %s)", p.Old)
}

// SettleDelay is how long the new version must run before it counts as having
// "survived".
const SettleDelay = 30 * time.Second

// verifyStaged verifies the staged file: first compare the SHA256, then actually start
// it once.
func verifyStaged(p Paths) error {
	want, err := os.ReadFile(p.Sum)
	if err != nil {
		return fmt.Errorf("read checksum: %w", err)
	}
	got, err := fileSHA256(p.New)
	if err != nil {
		return fmt.Errorf("compute checksum: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(string(want)), got) {
		return errors.New("SHA256 mismatch (download corrupted or tampered with)")
	}
	return smokeTest(p.New)
}

// smokeTest starts the new binary with -h to confirm it can really execute on this
// system. This catches a whole class of problems such as truncated downloads, the
// wrong architecture (exec format error) and missing dependencies.
func smokeTest(bin string) error {
	if err := os.Chmod(bin, 0o755); err != nil {
		return fmt.Errorf("set executable permission: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "-h")
	cmd.Env = append(os.Environ(), smokeEnv+"=1")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return errors.New("smoke test timed out (new binary not responding)")
	}
	if err != nil {
		snippet := strings.TrimSpace(string(out))
		if len(snippet) > 300 {
			snippet = snippet[:300] + "…"
		}
		return fmt.Errorf("smoke test failed: %v: %s", err, snippet)
	}
	return nil
}

// swap replaces the current binary with the staged new version.
//
// Both Unix and Windows allow renaming a running executable (Windows forbids
// deleting or overwriting it, but renaming is not on that list), so no per-platform
// handling is needed here, and the process does not have to stop itself first.
func swap(p Paths) error {
	// On Windows, rename does not overwrite an existing target, so the .old left by
	// the previous upgrade must be removed first.
	if err := os.Remove(p.Old); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove old backup %s: %w", p.Old, err)
	}
	if err := os.Rename(p.Current, p.Old); err != nil {
		return fmt.Errorf("back up current version: %w", err)
	}
	if err := os.Rename(p.New, p.Current); err != nil {
		// The swap failed but the current version has already been moved away; put it
		// back as it was, or the next start will have no executable.
		if rerr := os.Rename(p.Old, p.Current); rerr != nil {
			return fmt.Errorf("failed to move the new version into place (%v), and restoring the current version failed: %w", err, rerr)
		}
		return fmt.Errorf("move new version into place: %w", err)
	}
	_ = os.Remove(p.Sum)
	return nil
}

// rollback swaps back in the old version that swap backed up.
func rollback(p Paths) error {
	if _, err := os.Stat(p.Old); err != nil {
		return fmt.Errorf("no backup %s to roll back to: %w", p.Old, err)
	}
	// Move the new version that could not start to .failed for troubleshooting
	// instead of deleting it.
	failed := p.Current + ".failed"
	_ = os.Remove(failed)
	if err := os.Rename(p.Current, failed); err != nil {
		return fmt.Errorf("move failed version aside: %w", err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		return fmt.Errorf("restore old version: %w", err)
	}
	return nil
}

// Rollback implements /api/update/rollback: it deliberately reverts to the previous
// version. It only performs the swap; the restart is again left to the supervisor
// script (the caller then exits with ExitRestart).
func Rollback() error {
	p, err := ResolvePaths()
	if err != nil {
		return err
	}
	if _, err := os.Stat(p.Old); err != nil {
		return errors.New("no previous version to roll back to (" + p.Old + " does not exist)")
	}
	cleanStaged(p)
	if err := smokeTest(p.Old); err != nil {
		return fmt.Errorf("previous version cannot execute, refusing to roll back: %w", err)
	}
	// Swap current and backup, so a rollback can itself be rolled back.
	tmp := p.Current + ".swap"
	_ = os.Remove(tmp)
	if err := os.Rename(p.Current, tmp); err != nil {
		return fmt.Errorf("move current version aside: %w", err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		_ = os.Rename(tmp, p.Current)
		return fmt.Errorf("move previous version into place: %w", err)
	}
	if err := os.Rename(tmp, p.Old); err != nil {
		log.Printf("[update] failed to tidy up the backup after the rollback (the running version is unaffected): %v", err)
	}
	_ = os.Remove(p.Marker)
	return nil
}

// HasBackup reports whether a previous version exists to roll back to, so the
// frontend can decide whether to show the rollback button.
func HasBackup() bool {
	p, err := ResolvePaths()
	if err != nil {
		return false
	}
	_, err = os.Stat(p.Old)
	return err == nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown version"
	}
	return s
}
