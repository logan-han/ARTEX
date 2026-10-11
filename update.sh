#!/usr/bin/env bash
# ARTEX update script: ① Docker update (pull or rebuild the image and recreate)  ② Local build update (rebuild the binary)
# Counterpart to install.sh: install handles the first deployment, update upgrades to a new version.
# No manual DB migration is needed: artex re-runs schema.sql idempotently on every start (including ADD COLUMN/CREATE
# INDEX IF NOT EXISTS), so "a restart is a migration". Data (the pgdata volume, ./data, ./skills) is not affected.
set -euo pipefail
cd "$(cd "$(dirname "$0")" && pwd)"

info(){ printf '\033[36m[*]\033[0m %s\n' "$*"; }
ok(){   printf '\033[32m[+]\033[0m %s\n' "$*"; }
warn(){ printf '\033[33m[!]\033[0m %s\n' "$*"; }
die(){  printf '\033[31m[x]\033[0m %s\n' "$*" >&2; exit 1; }
ask(){  local p="$1" d="${2:-}" a; read -rp "$p${d:+ [$d]}: " a; echo "${a:-$d}"; }

# ── Optional: sync the repo to the latest code (this is how compose/scripts/local build sources get updated) ─────
sync_repo(){
  [ -d .git ] && command -v git >/dev/null 2>&1 || { warn "Not a git working copy, skipping git pull"; return; }
  [ "$(ask 'Pull the latest code (git pull --ff-only)? (y/n)' y)" = y ] || return
  if ! git pull --ff-only; then
    warn "git pull could not fast-forward (local changes or diverged branches). Resolve it manually and try again; continuing with the current code for now"
  fi
}

# ── ① Docker update ─────────────────────────────
update_docker(){
  command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1 \
    || die "docker / docker compose not found. Install and deploy with ./install.sh first"
  [ -f .env ] || die ".env not found. Run ./install.sh first to complete the initial deployment"

  # Optional: upgrade to a specific version tag (leave empty to keep ARTEX_TAG from .env, default latest)
  local tag; tag="$(ask 'Target image tag (press Enter to keep .env / latest)' '')"
  if [ -n "$tag" ]; then
    if grep -q '^ARTEX_TAG=' .env; then
      sed -i.bak "s|^ARTEX_TAG=.*|ARTEX_TAG=${tag}|" .env && rm -f .env.bak
    else
      printf '\nARTEX_TAG=%s\n' "$tag" >> .env
    fi
    ok "Set ARTEX_TAG to ${tag}"
  fi

  # Only touch artex: postgres is pinned to 16-alpine and does not need upgrading along with it (pulling it just wastes
  # bandwidth, and a major version change carries compatibility risk). artex declares depends_on postgres, so running
  # up with the service name starts pg automatically if it is not running, and leaves it as is (not recreated) if it is.
  info "Pulling the new image (artex only)..."
  if ! docker compose pull artex; then
    warn "No published artex image to pull, rebuilding it from this checkout instead (takes a few minutes)..."
    docker compose build --pull artex
  fi
  info "Recreating and starting (artex migrates the schema automatically on restart)..."
  docker compose up -d artex
  ok "Update complete → http://localhost:8787"
  info "View logs: docker compose logs -f artex"
  info "Clean up old images (optional): docker image prune -f"
}

# ── ② Local build update ────────────────────────
update_local(){
  command -v go >/dev/null 2>&1 || die "Go (>=1.26) not found: https://go.dev/dl/"
  [ -f config.json ] || warn "config.json not found; for a first deployment use ./install.sh instead"
  ok "Go: $(go version)"

  if command -v npm >/dev/null 2>&1; then
    info "Rebuilding the static frontend..."
    ( cd web && npm ci && npm run build:static )
    rm -rf server/webui/dist && cp -r web/out server/webui/dist
    info "Rebuilding the single binary with the embedded frontend..."
    CGO_ENABLED=0 go build -tags embedui -trimpath -o artex ./cmd/artex
  else
    warn "npm not found: building the backend **without the embedded frontend** (run the frontend separately with npm run dev)"
    CGO_ENABLED=0 go build -o artex ./cmd/artex
  fi
  ok "Build complete → ./artex"
  warn "Restart the running artex process to apply it (the schema migrates automatically on restart)"
}

echo "=============================="
echo "  ARTEX updater"
echo "  1) Docker update (pull or rebuild the image and recreate)"
echo "  2) Local build update (Go rebuild)"
echo "=============================="
case "$(ask 'Choose' 1)" in
  1) sync_repo; update_docker ;;
  2) sync_repo; update_local ;;
  *) die "Invalid choice" ;;
esac
