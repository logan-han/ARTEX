#!/usr/bin/env bash
# Development mode: runs the backend (:8787) + traffic proxy (:8788) together with the frontend next dev (:5173).
# The frontend reverse-proxies /api to the backend; Ctrl-C stops everything.
#
# For the single binary (frontend embedded), see the README's single binary section; it does not use this script.
set -euo pipefail
cd "$(dirname "$0")"

# On exit, kill every child process in this process group (backend + frontend).
cleanup() { kill 0 2>/dev/null || true; }
trap cleanup EXIT INT TERM

# Backend (plain go run, frontend not embedded); the number of concurrent work agents is set in "System settings".
go run ./cmd/artex -addr :8787 -proxy 127.0.0.1:8788 &

# Frontend with hot reload (Vite/Next dev server, /api reverse-proxied to :8787).
( cd web && npm run dev ) &

echo "[dev] backend :8787 / proxy :8788 / frontend http://localhost:5173  (Ctrl-C to quit)"
wait
