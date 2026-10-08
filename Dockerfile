# syntax=docker/dockerfile:1
#
# Runtime image (nothing is compiled in the image): installs common tools only and adds the **prebuilt single Linux binary**.
# The binary is cross-compiled by the CI binaries job (pure Go, no QEMU) and placed, per target architecture, at
# dist/<TARGETARCH>/artex in the build context. That way a multi-arch build only has to emulate the apt layer for arm64,
# not the Next/Go builds, which is much faster.
#
# To build the image locally by hand, prepare the binary yourself first:
#   cd web && npm run build:static && cd ..
#   cp -r web/out server/webui/dist
#   CGO_ENABLED=0 GOARCH=amd64 go build -tags embedui -o dist/amd64/artex ./cmd/artex
#   docker build -t artex:local .
FROM python:3.12-slim-bookworm
ARG TARGETARCH
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
# Prebuilt binary for the matching architecture (dist/amd64/artex or dist/arm64/artex)
COPY dist/${TARGETARCH}/artex /app/artex
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
