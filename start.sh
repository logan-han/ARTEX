#!/bin/sh
# ARTEX supervisor start script (Linux / macOS / Docker ENTRYPOINT)
#
# Usage:
#   ./start.sh                       run in the foreground (Ctrl-C to stop)
#   nohup ./start.sh >artex.log 2>&1 &   keep it running in the background
#   ./start.sh -addr :9000           extra arguments are passed through to artex unchanged
#
# It does one thing only: run artex, and when the process exits, use the exit code to decide whether to start it again.
#
#   0      normal stop by the user   → leave the loop
#   75     program requested restart → rerun immediately ("One-click update" or "Roll back" was clicked in the UI)
#   other  crash                     → rerun after a backoff (1→2→4... up to 60 seconds)
#
# Downloading, SHA256 verification and swapping the binary are deliberately not done here: that logic would have to be
# written twice, for sh and bat, and it is exactly the part that must not go wrong. Once a binary that cannot run is
# swapped in, this script would faithfully keep restarting it and the user could only rescue it by hand on the machine.
# So verification/swapping all stays in Go (the selfupdate package), done by artex itself at startup, and the script stays dead simple.
set -u

cd "$(dirname "$0")" || exit 1

BIN=./artex
[ -x "$BIN" ] || { echo "[artex] cannot find the executable $BIN" >&2; exit 1; }

RESTART_CODE=75
MAX_DELAY=60

child=0
stopping=0

# Forward stop signals to the artex process itself.
#
# This is required under Docker: docker stop only sends SIGTERM to PID 1 (that is, this script),
# not to child processes. Without forwarding, artex never receives the signal and cannot shut down gracefully;
# after 10 seconds it is killed hard with SIGKILL and running tasks are cut off midway.
forward() {
	stopping=1
	if [ "$child" -ne 0 ]; then
		kill -TERM "$child" 2>/dev/null || true
	fi
}
trap forward INT TERM

delay=1
while :; do
	"$BIN" "$@" &
	child=$!

	# A signal interrupts wait and makes it return >128. At that point the child is actually still shutting down
	# gracefully, so we must wait again to get its real exit code.
	wait "$child"
	code=$?
	if [ "$code" -gt 128 ]; then
		wait "$child"
		code=$?
	fi
	child=0

	if [ "$stopping" -eq 1 ]; then
		echo "[artex] stopped"
		exit 0
	fi

	case "$code" in
		0)
			echo "[artex] exited normally"
			exit 0
			;;
		"$RESTART_CODE")
			# The update/rollback is staged: on rerun, artex completes the swap at startup (see selfupdate.Bootstrap).
			echo "[artex] restart requested (applying the new version)..."
			delay=1
			;;
		*)
			echo "[artex] exited abnormally (code=$code), restarting in ${delay}s" >&2
			sleep "$delay"
			delay=$((delay * 2))
			[ "$delay" -gt "$MAX_DELAY" ] && delay=$MAX_DELAY
			;;
	esac
done
