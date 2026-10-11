# syntax=docker/dockerfile:1
#
# Self-contained image build: the static frontend and the single binary are compiled in the builder stages below,
# so a plain checkout is all you need:
#   docker compose build artex        (or: docker build -t artex:local .)
# Both builder stages run on the build machine's own platform and Go cross-compiles for TARGETARCH, so a multi-arch
# build only has to emulate the apt layer of the runtime stage for arm64, not the Next/Go builds.

# ① Static frontend export (architecture independent, built once)
FROM --platform=$BUILDPLATFORM node:22-bookworm-slim AS web
# The build context has no .git, so skip husky's git hook install; also keep Next's telemetry quiet.
ENV HUSKY=0 NEXT_TELEMETRY_DISABLED=1
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build:static

# ② Single binary with the frontend embedded, cross-compiled for the target architecture by build.sh
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS build
ARG TARGETARCH
# Version embedded in the binary (the release workflow passes the tag); local image builds report "dev".
ARG ARTEX_BUILD_VERSION=dev
WORKDIR /src
# third_party/ holds the vendored norma module that go.mod's replace directive points at.
COPY go.mod go.sum ./
COPY third_party/ third_party/
RUN go mod download
COPY . .
COPY --from=web /src/web/out server/webui/dist
RUN ARTEX_SKIP_FRONTEND=1 ARTEX_OUTPUT=/out/artex ARTEX_BUILD_VERSION="$ARTEX_BUILD_VERSION" \
    bash build.sh --target "linux/$TARGETARCH"

# ③ Runtime image: common tools plus the binary from stage ②
FROM python:3.12-slim-bookworm
# Common tools: ripgrep / curl / vim, plus a set of recon staples (add or remove as needed).
# Node 20.x is installed from NodeSource: bookworm's own apt nodejs is 18, and Playwright requires >=20.
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates ripgrep curl wget vim git jq unzip \
      dnsutils iputils-ping netcat-openbsd inetutils-telnet whois nmap \
    && curl -fsSL https://deb.nodesource.com/setup_20.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*
# Preinstall the Playwright MCP and CLI (globally) so nothing is downloaded over the network via npx at runtime.
# @playwright/mcp: the browser MCP runs `npx @playwright/mcp` directly (already installed globally, no -y/@latest needed).
# @playwright/cli: provides playwright-cli; after installing, --help is run to check that it executes.
# Then install playwright (browser management) and use --with-deps to preinstall chromium and its system dependencies,
# so the MCP/CLI in the container work on first start without downloading a browser.
RUN npm install -g @playwright/mcp@latest @playwright/cli@latest playwright@latest \
    && playwright-cli --help \
    && playwright install --with-deps chromium \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=build /out/artex /app/artex
# Supervisor start script: when the process exits it uses the exit code to decide whether to restart it; one-click update in the UI relies on it to swap binaries.
# It also forwards SIGTERM to artex: docker stop only sends the signal to PID 1,
# and without forwarding artex never receives it, cannot shut down gracefully and is killed hard with SIGKILL after 10 seconds.
COPY start.sh /app/start.sh
RUN chmod +x /app/artex /app/start.sh
COPY skills/ /app/skills/
# data/ (SQLite + jwt.key) persistence point
VOLUME ["/app/data"]
EXPOSE 8787 8788
ENTRYPOINT ["/app/start.sh"]
CMD ["-addr", ":8787", "-proxy", ":8788"]
