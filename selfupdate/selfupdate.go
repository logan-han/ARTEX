// Package selfupdate implements ARTEX's one-click update from the web UI: it pulls
// the new binary from a GitHub Release, verifies it, stages it, and swaps it in
// atomically on the next start.
//
// Division of labour (see start.sh / start.bat):
//
//	start script = dumb supervisor loop; its only job is "when the process exits, use the exit code to decide whether to start it again"
//	this package = all the error-prone logic (download / SHA256 verification / smoke test / swap / rollback on failure)
//
// The swap lives in Go rather than in the scripts because SHA256 verification and
// the smoke test would have to be written twice, for sh and for bat (sha256sum /
// shasum / certutil), and this is exactly the step that must not go wrong: swap in a
// binary that cannot run and the supervisor will faithfully keep restarting it,
// leaving the user to log in to the machine and rescue it by hand.
//
// A complete upgrade goes through three process starts:
//
//	(1) the old server receives /api/update/apply → download and verify → stage artex.new → exit 75
//	(2) the script restarts the old version → Bootstrap finds artex.new → verify + smoke test → swap → exit 75
//	(3) the script restarts it, now on the new version → Bootstrap records an attempt → clears the marker once startup succeeds
//
// Any failed step falls back to the old version: if verification fails in (2), the
// staged file is deleted and the old version keeps running; if in (3) the new version
// fails to last until the marker is cleared 3 times in a row (it crashes on startup),
// artex.old is swapped back in automatically.
package selfupdate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ExitRestart is the "supervisor, please start me again" exit code (EX_TEMPFAIL).
// The start script reruns immediately when it sees it, without counting it towards
// crash backoff. 0 means the user stopped the process normally (the script leaves its
// loop); anything else is treated as a crash.
const ExitRestart = 75

// maxAttempts is the number of start attempts allowed after a swap. Every start of
// the new version increments the count, and surviving past settleDelay clears the
// marker; crashing maxAttempts times in a row means the new version simply cannot
// start, so it is rolled back automatically.
const maxAttempts = 3

// Paths is every file involved in one upgrade, all placed in the **directory of the
// executable**. The CWD is deliberately not used: when running as a service the
// working directory may be / or any other path, and using the CWD would put the
// staged file somewhere else and break the swap logic outright.
type Paths struct {
	Dir     string // directory containing the executable
	Current string // currently running binary           artex      / artex.exe
	New     string // staged new version                 artex.new  / artex.new.exe
	Sum     string // sha256 of the new version (hex)    artex.new.sha256 / artex.new.exe.sha256
	Old     string // pre-swap backup of the old version artex.old  / artex.old.exe
	Marker  string // upgrade state marker               artex.upgrade.json
}

// ResolvePaths derives every upgrade path from the current executable.
//
// On Windows, .new/.old must keep the .exe suffix too, otherwise both the smoke test
// and running the binary after the swap fail, so the suffix is stripped before the
// names are built and re-appended afterwards; that keeps the naming symmetrical on
// both platforms.
func ResolvePaths() (Paths, error) {
	exe, err := os.Executable()
	if err != nil {
		return Paths{}, fmt.Errorf("locate executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	dir := filepath.Dir(exe)
	name := filepath.Base(exe)
	ext := filepath.Ext(name) // ".exe" on Windows, usually empty on Unix
	stem := strings.TrimSuffix(name, ext)

	join := func(suffix string) string { return filepath.Join(dir, stem+suffix+ext) }
	return Paths{
		Dir:     dir,
		Current: exe,
		New:     join(".new"),
		Sum:     join(".new") + ".sha256",
		Old:     join(".old"),
		Marker:  filepath.Join(dir, stem+".upgrade.json"),
	}, nil
}

// marker records the progress of one swap, and is used to trigger an automatic
// rollback when the new version cannot start.
type marker struct {
	From     string `json:"from"`     // version before the upgrade
	To       string `json:"to"`       // target version
	Attempts int    `json:"attempts"` // start attempts made since the swap
	StagedAt int64  `json:"staged_at"`
}

func readMarker(path string) (marker, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return marker{}, false
	}
	var m marker
	if json.Unmarshal(b, &m) != nil {
		return marker{}, false
	}
	return m, true
}

func writeMarker(path string, m marker) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// cleanStaged removes the staged files. A successful swap, a failed verification and
// a user cancellation all go through it, so a leftover artex.new is not retried on
// the next start.
func cleanStaged(p Paths) {
	_ = os.Remove(p.New)
	_ = os.Remove(p.Sum)
}

// CompareVersions compares two version numbers and returns -1/0/1 (a<b / a==b / a>b).
// ok=false means at least one side is not a comparable version (for example "dev"
// from a local development build, or "0.3.7-2-gabc1234-dirty" from git describe).
// The caller should then disable one-click update, otherwise a build under
// development would be "upgraded" to a release, overwriting uncommitted changes.
func CompareVersions(a, b string) (int, bool) {
	av, aok := parseVersion(a)
	bv, bok := parseVersion(b)
	if !aok || !bok {
		return 0, false
	}
	for i := range 3 {
		if av[i] != bv[i] {
			if av[i] < bv[i] {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

// parseVersion parses a version of the form "v0.3.7" / "0.3.7" into [3]int.
//
// Only a clean three-part version is accepted: for non-tag builds, build.sh uses git
// describe and produces suffixed versions such as "0.3.7-2-gabc1234". Those must be
// judged not comparable rather than treated as 0.3.7; otherwise a development build
// would be wrongly seen as "already up to date" or be overwritten by a release.
func parseVersion(s string) ([3]int, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return [3]int{}, false
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var out [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

// InDocker reports whether the process is running in a container. Under Docker the
// swap writes to the container's writable layer, and recreating the container with
// `docker compose up -d` reverts to the version shipped in the image. That is
// expected (the user is pulling a new image at that point anyway), but the frontend
// needs to know so it can explain it clearly.
func InDocker() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	b, err := os.ReadFile("/proc/1/cgroup")
	if err != nil {
		return false
	}
	s := string(b)
	return strings.Contains(s, "docker") || strings.Contains(s, "containerd")
}
