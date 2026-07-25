#!/bin/sh
# dev-start.sh — one-command local development environment.
#
# What it does (in order):
#   1. Generates ADMIN_TOKEN into .env if missing (closes #684 fail-open)
#   2. Runs infra/scripts/setup.sh (postgres + redis + minio + langfuse +
#      clickhouse + populates template/plugin registry from manifest.json)
#   3. Starts the platform (Go :8080), waits for /health
#   4. Starts the canvas (Next.js :3000), waits for HTTP 200
#   5. Prints a readiness banner with API-key add instructions
#   6. On Ctrl-C, kills both background processes and tears down infra
#
# Prerequisites:
#   - Docker + Docker Compose v2  (for postgres/redis/langfuse/etc)
#   - Go 1.25+                     (for the platform binary)
#   - Node.js 20+                  (for the canvas)
#   - jq                           (for setup.sh's manifest clone — optional;
#                                   without it, template palette will be
#                                   empty until you run clone-manifest.sh
#                                   manually)
#
# Usage:
#   ./scripts/dev-start.sh            # incremental (data + images preserved)
#   ./scripts/dev-start.sh --fresh    # FULL reset: wipe DB/object-store +
#                                     # onboarding state, delete locally-built
#                                     # template images + build cache, rebuild
#                                     # from scratch. Re-onboard from zero.
#   ./scripts/dev-start.sh --help
#   # Open http://localhost:3000, add your model API key in
#   # Config → Secrets & API Keys, then create your first workspace.
#
# Idempotent (without --fresh): re-running picks up where the last run left off
# (existing .env is preserved, npm install skipped if node_modules present,
# local DB/object-store state survives, already-built images are reused).

set -e

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
# Shared docker-teardown helpers (mol_rm_by_filter / mol_purge_ws_objects,
# UUID-scoped + xargs-free) — also used by scripts/nuke-and-rebuild.sh.
# shellcheck source=scripts/lib/docker-reset.sh
# shellcheck disable=SC1091
. "$ROOT/scripts/lib/docker-reset.sh"
ENV_FILE="$ROOT/.env"
COMPOSE_PROJECT_NAME="${COMPOSE_PROJECT_NAME:-molecule-core}"
export COMPOSE_PROJECT_NAME
# Compose derives container names from the project; every by-name docker
# inspect/rm/logs below must use this, never a hardcoded molecule-core-…
LANGFUSE_CONTAINER="${COMPOSE_PROJECT_NAME}-langfuse-1"
PLATFORM_PID=
CANVAS_PID=
FRESH=0

print_usage() {
    cat <<'USAGE'
Usage: ./scripts/dev-start.sh [--fresh]

  (no args)   Incremental start — reuses the existing .env, local DB /
              object-store state, and any already-built runtime images.
              Idempotent: safe to re-run.

  --fresh     (alias: --remove-volumes) FULL reset before starting.
              This is DESTRUCTIVE:
                • tears down the compose stack AND its named volumes
                  (WIPES local Postgres / MinIO / Langfuse state and your
                  onboarding config — you re-onboard from scratch);
                • removes dynamically-spawned ws-* workspace containers/volumes;
                • deletes locally-built molecule-local/workspace-template-*
                  images and clears the template build-clone cache, so every
                  runtime image (hermes, etc.) rebuilds from current source
                  instead of reusing a cached tag;
                • on the docker-compose platform fallback, rebuilds the core
                  image with --no-cache (on the default `go run` path the core
                  always recompiles from source anyway).

  -h, --help  Show this help.
USAGE
}

for arg in "$@"; do
    case "$arg" in
        --fresh|--remove-volumes) FRESH=1 ;;
        -h|--help) print_usage; exit 0 ;;
        # Fail loud on unknown flags — the whole point of this parser is that a
        # mistyped/unsupported flag (e.g. --fresh before it existed) must NOT be
        # silently swallowed and leave the dev thinking it took effect.
        *) echo "dev-start.sh: unknown option '$arg' (try --help)" >&2; exit 2 ;;
    esac
done

process_cwd() {
    # $1 = pid. Prints the process cwd, or nothing when the process vanished.
    lsof -a -p "$1" -d cwd -Fn 2>/dev/null | sed -n 's/^n//p' | head -1
}

repo_dev_pid_matches() {
    # $1 = pid. Only processes whose cwd is one of dev-start's app dirs belong
    # to this repo-local stack; do not kill unrelated localhost services.
    cwd=$(process_cwd "$1" || true)
    case "$cwd" in
        "$ROOT/workspace-server"|"$ROOT/canvas")
            return 0
            ;;
        *)
            return 1
            ;;
    esac
}

cleanup_repo_host_processes() {
    # Stop stale host-side dev-start children from prior runs. Docker cleanup
    # alone is insufficient: `go run ./cmd/server` and `next dev` are host
    # processes, and stale listeners on :8080/:3000 can make a new dynamic stack
    # look ready while the browser is still pointed at yesterday's server.
    command -v lsof >/dev/null 2>&1 || return 0

    pids=$(
        {
            lsof -nP -iTCP -sTCP:LISTEN -Fp 2>/dev/null | sed -n 's/^p//p'
            if command -v pgrep >/dev/null 2>&1; then
                pgrep -f 'go run ./cmd/server|next dev --turbopack|next dev' 2>/dev/null || true
            fi
        } | sort -u
    )
    [ -n "$pids" ] || return 0

    killed=0
    for pid in $pids; do
        [ "$pid" != "$$" ] || continue
        if repo_dev_pid_matches "$pid"; then
            pkill -TERM -P "$pid" 2>/dev/null || true
            kill -TERM "$pid" 2>/dev/null || true
            killed=1
        fi
    done

    [ "$killed" -eq 0 ] && return 0
    sleep 1

    for pid in $pids; do
        [ "$pid" != "$$" ] || continue
        if kill -0 "$pid" 2>/dev/null && repo_dev_pid_matches "$pid"; then
            pkill -KILL -P "$pid" 2>/dev/null || true
            kill -KILL "$pid" 2>/dev/null || true
        fi
    done
}

cleanup_dev_stack() {
    cleanup_repo_host_processes
    # Stop containers from prior dev-start runs before port selection. Keep
    # named volumes so the local DB/object-store state survives restarts.
    docker compose -f "$ROOT/docker-compose.yml" down --remove-orphans >/dev/null 2>&1 || true
    docker compose -f "$ROOT/docker-compose.infra.yml" down --remove-orphans >/dev/null 2>&1 || true
    docker rm -f "$LANGFUSE_CONTAINER" >/dev/null 2>&1 || true
}

fresh_reset() {
    # --fresh (opt-in only; NEVER runs on the exit trap — see cleanup()). Unlike
    # cleanup_dev_stack — which keeps named volumes so DB/object-store state
    # survives an ordinary restart — this WIPES that state plus the locally-built
    # runtime images, so onboarding and every template image rebuild from a clean
    # slate. Shares the ws-* purge helper (mol_purge_ws_objects) with
    # scripts/nuke-and-rebuild.sh and adds the template-image + build-clone-cache
    # purge (the piece that otherwise strands a stale
    # molecule-local/workspace-template-*:<sha> image behind a green boot).
    echo "==> --fresh: FULL reset — WIPING local DB / object-store / onboarding state"
    cleanup_repo_host_processes
    # Compose stack + named volumes (postgres/redis/minio/langfuse/clickhouse).
    # Surface a failure rather than silently claiming a clean slate below: if a
    # `down -v` errors (e.g. a broken compose plugin on the go-run boot path), the
    # named volumes are NOT wiped and the operator must not be told otherwise.
    docker compose -f "$ROOT/docker-compose.yml" down -v --remove-orphans >/dev/null 2>&1 \
        || echo "    WARN: 'docker compose -f docker-compose.yml down -v' failed — local DB/Redis volumes may persist" >&2
    docker compose -f "$ROOT/docker-compose.infra.yml" down -v --remove-orphans >/dev/null 2>&1 \
        || echo "    WARN: 'docker compose -f docker-compose.infra.yml down -v' failed — MinIO/Langfuse volumes may persist" >&2
    docker rm -f "$LANGFUSE_CONTAINER" >/dev/null 2>&1 || true
    # Dynamically-spawned ws-<uuid> workspace containers + volumes (UUID-scoped so
    # an unrelated project's ws-* object on the same host is never touched).
    mol_purge_ws_objects
    docker network rm molecule-core-net >/dev/null 2>&1 || true
    # Locally-built runtime template images (the whole molecule-local/ namespace is
    # synthetic local-build output) so the next provision rebuilds each template
    # from current source rather than reusing a stale :<sha> tag.
    mol_rm_by_filter "template images" "docker images --format '{{.Repository}}:{{.Tag}}'" "^molecule-local/" "docker rmi -f"
    # Clone/build cache. localbuild.go uses MOLECULE_LOCAL_BUILD_CACHE verbatim when
    # set, else ${XDG_CACHE_HOME:-$HOME/.cache}/molecule/… , else $TMPDIR/molecule/… —
    # clear every branch so an operator who set the override (or runs without HOME)
    # still gets a real clean slate instead of the stale-image trap --fresh exists
    # to eliminate.
    for _cache in \
        "${MOLECULE_LOCAL_BUILD_CACHE:-}" \
        "${XDG_CACHE_HOME:-$HOME/.cache}/molecule/workspace-template-build" \
        "${TMPDIR:-/tmp}/molecule/workspace-template-build"; do
        [ -n "$_cache" ] && rm -rf "$_cache" 2>/dev/null || true
    done
    echo "    reset complete — re-onboarding from a clean slate"
}

cleanup() {
    echo ""
    echo "==> Shutting down..."
    if [ -n "${PLATFORM_PID:-}" ]; then
        kill "$PLATFORM_PID" 2>/dev/null || true
    fi
    if [ -n "${CANVAS_PID:-}" ]; then
        kill "$CANVAS_PID" 2>/dev/null || true
    fi
    cleanup_dev_stack
    echo "    Done."
}
trap cleanup EXIT INT TERM

# ─────────────────────────────────────────────── 1. dev-mode auth posture
#
# SECURITY (harden/no-fail-open-auth): the workspace-server auth chain is
# now fail-CLOSED in EVERY environment, dev included. There is NO dev-mode
# fail-open escape hatch anymore — AdminAuth / WorkspaceAuth / discovery all
# require a real credential. So local dev must AUTHENTICATE, not run open.
#
# The clean way to keep the canvas working locally is to provision a
# deterministic ADMIN_TOKEN and hand the matching NEXT_PUBLIC_ADMIN_TOKEN to
# the canvas bundle. The canvas already attaches `Authorization: Bearer
# $NEXT_PUBLIC_ADMIN_TOKEN` on every platform call (canvas/src/lib/api.ts),
# and next.config.ts warns if the pair is half-set. We set BOTH here.
#
#   MOLECULE_ENV=development   — dev conveniences (loopback bind, relaxed
#                                rate limit). NOT an auth lever.
#   ADMIN_TOKEN=<dev value>    — server-side bearer AdminAuth/WorkspaceAuth
#                                enforce (Tier-2b). Real credential.
#   NEXT_PUBLIC_ADMIN_TOKEN    — same value, baked into the canvas bundle so
#                                the browser sends the matching bearer.
#
# This matching `NEXT_PUBLIC_ADMIN_TOKEN` pair is local-development-only.
# Production/SaaS Canvas must not bake the tenant `ADMIN_TOKEN` into public
# JavaScript. Browser requests authenticate with a verified control-plane
# session or a deliberately supplied org/admin credential; the server still
# runs with `MOLECULE_ENV=production` and fail-closed route middleware.
if [ -f "$ENV_FILE" ] && grep -q '^MOLECULE_ENV=' "$ENV_FILE"; then
    echo "==> Reusing MOLECULE_ENV from existing .env"
else
    echo "==> Setting MOLECULE_ENV=development in .env"
    {
        if [ -f "$ENV_FILE" ]; then
            cat "$ENV_FILE"
            echo ""
        fi
        echo "# Generated by scripts/dev-start.sh on $(date -u +%Y-%m-%dT%H:%M:%SZ)"
        echo "# Local-dev conveniences (loopback bind, relaxed rate limit)."
        echo "# Auth is fail-closed even in dev — see ADMIN_TOKEN below."
        echo "MOLECULE_ENV=development"
    } > "$ENV_FILE.tmp"
    mv "$ENV_FILE.tmp" "$ENV_FILE"
    echo "    Saved to $ENV_FILE"
fi

# Provision a deterministic dev ADMIN_TOKEN (idempotent — preserved across
# re-runs). This is the credential the canvas authenticates with locally; it
# is NOT a secret (it only guards your own localhost stack), so a fixed,
# well-known value is fine and keeps re-runs reproducible.
DEV_ADMIN_TOKEN="dev-local-admin-token"
if [ -f "$ENV_FILE" ] && grep -q '^ADMIN_TOKEN=' "$ENV_FILE"; then
    echo "==> Reusing ADMIN_TOKEN from existing .env"
else
    echo "==> Provisioning dev ADMIN_TOKEN in .env (fail-closed auth, authenticated canvas)"
    {
        cat "$ENV_FILE"
        echo ""
        echo "# Dev ADMIN_TOKEN — the canvas authenticates with this locally."
        echo "# Auth is fail-closed; without a matching bearer the canvas 401s."
        echo "# Fixed value is fine: it only guards your localhost stack."
        echo "ADMIN_TOKEN=$DEV_ADMIN_TOKEN"
    } > "$ENV_FILE.tmp"
    mv "$ENV_FILE.tmp" "$ENV_FILE"
    echo "    Saved to $ENV_FILE"
fi

# Source .env so the platform inherits ADMIN_TOKEN (and anything else
# the user has added — e.g. ANTHROPIC_API_KEY for skipping the canvas
# Secrets UI). `set -a` exports every assignment in the sourced file
# without us having to know the var names.
set -a
# shellcheck disable=SC1090
. "$ENV_FILE"
set +a

# The canvas reads NEXT_PUBLIC_ADMIN_TOKEN at build/dev time and attaches it
# as the bearer on every platform call. Mirror the server-side ADMIN_TOKEN
# into it so the matched-pair guard in canvas/next.config.ts is satisfied and
# the browser authenticates. Exported for the `npm run dev` child below.
export NEXT_PUBLIC_ADMIN_TOKEN="$ADMIN_TOKEN"

# Local private repo access for template/plugin bootstrap. This stays in the
# process env only: do not persist it to .env and do not print the value. The
# token lets setup.sh clone private manifest entries and lets the platform pass
# scoped GIT_HTTP_* credentials to the org concierge's management-MCP plugin
# boot-install path. Contributors without this file still get the existing
# reduced public-template bootstrap.
if [ -z "${MOLECULE_GITEA_TOKEN:-}" ] && [ -r "$HOME/.molecule-ai/gitea-token" ]; then
    MOLECULE_GITEA_TOKEN=$(tr -d '\r\n' < "$HOME/.molecule-ai/gitea-token")
    export MOLECULE_GITEA_TOKEN
    echo "==> Using local Molecule Gitea token for private template/plugin repos"
fi
if [ -z "${MOLECULE_TEMPLATE_REPO_TOKEN:-}" ] && [ -n "${MOLECULE_GITEA_TOKEN:-}" ]; then
    export MOLECULE_TEMPLATE_REPO_TOKEN="$MOLECULE_GITEA_TOKEN"
fi

if [ "$FRESH" -eq 1 ]; then
    fresh_reset
else
    echo "==> Cleaning up previous local dev containers"
    cleanup_dev_stack
fi

# ─────────────────────────────────────────── dynamic host ports (no hijack)
#
# A FIXED host port is a landmine on a dev's own machine. If they already run
# Postgres/Redis on the conventional port, publishing our container there does
# NOT fail — Docker's wildcard bind coexists with their service, and our app,
# connecting to `localhost:<port>`, silently lands on THEIR Postgres (their
# private DB) instead of ours. No error, just wrong — and possibly written-to —
# data. (On macOS it's worse: `localhost` resolves to ::1 first, so even a
# v6-only local Postgres wins.)
#
# So we allocate a free host port PER published service — preferring the
# conventional port when it is genuinely free on BOTH IPv4 and IPv6, else an
# OS-assigned ephemeral port — publish Docker there, and build every
# connection URL from the chosen port using 127.0.0.1 (never `localhost`).
# dev-start.sh OWNS these URLs for the local stack; export any of the
# MOLECULE_*_HOST_PORT vars (or a non-local DATABASE_URL/REDIS_URL) yourself to
# override.
pick_port() {
    # $1 = preferred port. Echoes it when free on IPv4 AND IPv6, else a free
    # ephemeral port. Degrades to the preferred port if python3 is absent.
    if command -v python3 >/dev/null 2>&1; then
        python3 - "$1" <<'PY'
import socket, sys
pref = int(sys.argv[1])
def free(port):
    for fam, addr in ((socket.AF_INET, "127.0.0.1"), (socket.AF_INET6, "::1")):
        s = socket.socket(fam, socket.SOCK_STREAM)
        try:
            s.bind((addr, port))
        except OSError:
            s.close()
            return False
        s.close()
    return True
if free(pref):
    print(pref)
else:
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    print(s.getsockname()[1])
    s.close()
PY
    else
        echo "$1"
    fi
}

MOLECULE_PG_HOST_PORT=${MOLECULE_PG_HOST_PORT:-$(pick_port 5432)}
MOLECULE_REDIS_HOST_PORT=${MOLECULE_REDIS_HOST_PORT:-$(pick_port 6379)}
MOLECULE_MINIO_HOST_PORT=${MOLECULE_MINIO_HOST_PORT:-$(pick_port 9000)}
MOLECULE_MINIO_CONSOLE_HOST_PORT=${MOLECULE_MINIO_CONSOLE_HOST_PORT:-$(pick_port 9001)}
PLATFORM_PORT=$(pick_port 8080)
CANVAS_PORT=$(pick_port 3000)
export MOLECULE_PG_HOST_PORT MOLECULE_REDIS_HOST_PORT \
       MOLECULE_MINIO_HOST_PORT MOLECULE_MINIO_CONSOLE_HOST_PORT

# App→infra connection URLs. Override empty or loopback-local URLs from an
# earlier dev-start run; respect a deliberate non-local URL.
case "${DATABASE_URL:-}" in
    ""|*localhost:*|*127.0.0.1:*|*\[::1\]:*|*::1:*)
        export DATABASE_URL="postgres://${POSTGRES_USER:-dev}:${POSTGRES_PASSWORD:-dev}@127.0.0.1:${MOLECULE_PG_HOST_PORT}/${POSTGRES_DB:-molecule}?sslmode=disable"
        ;;
esac
case "${REDIS_URL:-}" in
    ""|*localhost:*|*127.0.0.1:*|*\[::1\]:*|*::1:*)
        export REDIS_URL="redis://127.0.0.1:${MOLECULE_REDIS_HOST_PORT}"
        ;;
esac

# SDK object-store/workspace-data SSOT local backend. MinIO is the local
# S3-compatible adapter behind MOLECULE_OBJECT_STORE_BACKEND=minio. Keep these
# credentials host/platform-side only; workspace boxes must receive only
# platform auth or presigned/proxy URLs, never object-store credentials.
export MOLECULE_OBJECT_STORE_BACKEND="${MOLECULE_OBJECT_STORE_BACKEND:-minio}"
export MOLECULE_OBJECT_STORE_REGION="${MOLECULE_OBJECT_STORE_REGION:-us-east-1}"
export MOLECULE_WORKSPACE_DATA_BUCKET="${MOLECULE_WORKSPACE_DATA_BUCKET:-molecule-workspace-data}"
export MINIO_ROOT_USER="${MINIO_ROOT_USER:-${MOLECULE_WORKSPACE_DATA_ACCESS_KEY_ID:-molecule-dev-minio}}"
export MINIO_ROOT_PASSWORD="${MINIO_ROOT_PASSWORD:-${MOLECULE_WORKSPACE_DATA_SECRET_ACCESS_KEY:-molecule-dev-minio-password}}"
export MOLECULE_WORKSPACE_DATA_ACCESS_KEY_ID="${MOLECULE_WORKSPACE_DATA_ACCESS_KEY_ID:-$MINIO_ROOT_USER}"
export MOLECULE_WORKSPACE_DATA_SECRET_ACCESS_KEY="${MOLECULE_WORKSPACE_DATA_SECRET_ACCESS_KEY:-$MINIO_ROOT_PASSWORD}"
case "${MOLECULE_WORKSPACE_DATA_ENDPOINT:-}" in
    ""|http://localhost:*|http://127.0.0.1:*|http://\[::1\]:*|http://::1:*)
        export MOLECULE_WORKSPACE_DATA_ENDPOINT="http://127.0.0.1:${MOLECULE_MINIO_HOST_PORT}"
        ;;
esac

# Platform listen port + the canvas's cross-origin view of it. The canvas
# talks to the platform from the browser, so CORS must allow the canvas
# origin and the browser needs the platform's real port (both dynamic).
export NEXT_PUBLIC_PLATFORM_URL="http://127.0.0.1:${PLATFORM_PORT}"
export NEXT_PUBLIC_WS_URL="ws://127.0.0.1:${PLATFORM_PORT}/ws"
export CORS_ORIGINS="http://localhost:${CANVAS_PORT},http://127.0.0.1:${CANVAS_PORT}"

# Agent A2A reachability (macOS): the provisioner publishes each agent
# container's port on 127.0.0.1 (IPv4). Its default advertise host is
# "localhost", which macOS resolves to ::1 (IPv6) FIRST — so the host-run
# platform's A2A proxy dials [::1]:<port> and gets connection-refused (502 on
# chat). Pin the advertise host to 127.0.0.1 so the proxy hits the IPv4 port
# the container actually publishes. Port stays dynamic; only the host is fixed.
export MOLECULE_WORKSPACE_ADVERTISE_HOST=127.0.0.1

echo "==> Host ports (dynamic — conventional when free, else ephemeral):"
echo "    Postgres   127.0.0.1:${MOLECULE_PG_HOST_PORT}"
echo "    Redis      127.0.0.1:${MOLECULE_REDIS_HOST_PORT}"
echo "    MinIO      http://127.0.0.1:${MOLECULE_MINIO_HOST_PORT} (S3 API) / ${MOLECULE_MINIO_CONSOLE_HOST_PORT} (console)"
echo "    Platform   ${NEXT_PUBLIC_PLATFORM_URL}"
echo "    Canvas     http://127.0.0.1:${CANVAS_PORT}"

# ─────────────────────────────────────────── LLM tracing (Langfuse)
# Langfuse is the trace SINK. The platform's /traces proxy READS it (host URL,
# dynamic port); the shared runtime PRODUCES traces and reaches it over the
# Docker network (container URL, stable). Deterministic seeded keys (see the
# langfuse container bootstrap after setup.sh). Keys in the platform env make
# buildContainerEnv inject them into every agent (SSOT producer wiring).
MOLECULE_LANGFUSE_HOST_PORT=${MOLECULE_LANGFUSE_HOST_PORT:-$(pick_port 3001)}
export MOLECULE_LANGFUSE_HOST_PORT
export LANGFUSE_PUBLIC_KEY="${LANGFUSE_PUBLIC_KEY:-pk-lf-0000000000000000000000000000000000000001}"
export LANGFUSE_SECRET_KEY="${LANGFUSE_SECRET_KEY:-sk-lf-0000000000000000000000000000000000000002}"
# Same keys under the names the compose langfuse service seeds the org with.
export LANGFUSE_INIT_PROJECT_PUBLIC_KEY="$LANGFUSE_PUBLIC_KEY"
export LANGFUSE_INIT_PROJECT_SECRET_KEY="$LANGFUSE_SECRET_KEY"
export LANGFUSE_HOST="http://127.0.0.1:${MOLECULE_LANGFUSE_HOST_PORT}"          # platform reader (host)
export MOLECULE_WORKSPACE_LANGFUSE_HOST="http://langfuse-web:3000"             # agent producer (docker net)
echo "    Langfuse   ${LANGFUSE_HOST}  (traces UI + platform /traces proxy)"

# ─────────────────────────────────────────────── 2. infra + templates

# Use setup.sh (not raw docker-compose) so the template registry gets
# populated from manifest.json. Without that, the canvas template
# palette is empty and the user has to manually clone repos — exactly
# the friction this script exists to eliminate.
echo "==> Running infra/scripts/setup.sh (infra + template registry)"
"$ROOT/infra/scripts/setup.sh"

wait_for_langfuse_clickhouse_native() {
    echo "    Waiting for Langfuse ClickHouse native port..."
    i=1
    while [ "$i" -le 60 ]; do
        if docker compose -f "$ROOT/docker-compose.infra.yml" exec -T langfuse-clickhouse \
            clickhouse-client --user langfuse --password "${CLICKHOUSE_PASSWORD:-langfuse-dev}" \
            --query 'SELECT 1' >/dev/null 2>&1; then
            echo "    Langfuse ClickHouse ready (t+${i}s)"
            return 0
        fi
        sleep 1
        i=$((i + 1))
    done
    echo "    ✗ Langfuse ClickHouse native port did not become ready in 60s"
    echo "      Check: docker logs molecule-core-langfuse-clickhouse-1"
    return 1
}

wait_for_langfuse_http() {
    echo "    Waiting for Langfuse HTTP..."
    i=1
    while [ "$i" -le 90 ]; do
        running=$(docker inspect -f '{{.State.Running}}' "$LANGFUSE_CONTAINER" 2>/dev/null || echo false)
        if [ "$running" != "true" ]; then
            echo "    ✗ Langfuse exited before becoming ready — last logs:"
            docker logs --tail 80 "$LANGFUSE_CONTAINER" 2>&1 | sed 's/^/      /'
            return 1
        fi

        code=$(curl -s -o /dev/null -w "%{http_code}" "$LANGFUSE_HOST/" 2>/dev/null || true)
        case "$code" in
            2*|3*)
                echo "    Langfuse ready (t+${i}s)"
                return 0
                ;;
        esac

        sleep 1
        i=$((i + 1))
    done

    echo "    ✗ Langfuse did not respond in 90s — last logs:"
    docker logs --tail 80 "$LANGFUSE_CONTAINER" 2>&1 | sed 's/^/      /'
    return 1
}

# ─────────────────────────────────────────── Langfuse web (trace UI + API)
# setup.sh brings up the Langfuse deps (clickhouse + langfuse-db-init) but not
# the web app. That lives in docker-compose.yml as the ONE Langfuse definition;
# bring it up from there — parameterized by the exported env above — so this
# script and plain `docker compose up` cannot drift apart.
echo "==> Starting Langfuse (trace UI on :${MOLECULE_LANGFUSE_HOST_PORT})"
wait_for_langfuse_clickhouse_native
# Remove a stale non-compose Langfuse container from pre-unification checkouts;
# compose refuses to reuse the name otherwise.
if docker inspect -f '{{index .Config.Labels "com.docker.compose.project"}}' \
    "$LANGFUSE_CONTAINER" 2>/dev/null | grep -Fqvx "$COMPOSE_PROJECT_NAME"; then
    docker rm -f "$LANGFUSE_CONTAINER" >/dev/null 2>&1 || true
fi
if docker compose -f "$ROOT/docker-compose.yml" up -d langfuse; then
    echo "    Langfuse container started (first boot ~15-30s)"
else
    echo "    ✗ Langfuse failed to start"
    exit 1
fi
wait_for_langfuse_http

# ─────────────────────────────────────────────── 3. platform
#
# Three paths, tried in order:
#   (a) usable `go` on PATH (or one this script bootstrapped earlier) →
#       run the platform directly via `go run`. Fast iteration, attaches
#       to /tmp/molecule-platform.log.
#   (b) no usable `go` → bootstrap_go_toolchain downloads the official,
#       checksum-pinned Go tarball for this OS/arch into
#       ~/.molecule-ai/toolchains/ (hermetic: no sudo, no package manager,
#       nothing system-wide; ~70MB once, cached forever) and puts it on
#       PATH for this run only.
#   (c) bootstrap impossible (offline, unsupported OS/arch, checksum
#       mismatch) → fall back to the published platform container image.
#       NOTE: on arm64 hosts this fallback currently fails — the
#       workspace-server/Dockerfile base images are single-arch amd64
#       mirror digests — which is exactly why (b) exists.
#
# The earlier version of this script silently called `go run` and died
# with `go: not found` on dev boxes where Go wasn't installed. This
# ladder makes the failure path either succeed (bootstrap/fallback) or
# fail loud with explicit guidance.

# Pinned bootstrap toolchain. Any go >= 1.21 on PATH is also fine as-is:
# Go's own GOTOOLCHAIN mechanism then auto-fetches the exact version
# workspace-server/go.mod requires. Refresh procedure (all five lines
# together): pick the release from https://go.dev/dl/?mode=json and copy
# its per-platform archive sha256 sums.
GO_BOOTSTRAP_VERSION="go1.26.5"
GO_SHA256_DARWIN_ARM64="efb87ff28af9a188d0536ef5d42e63dd52ba8263cd7344a993cc48dd11dedb6a"
GO_SHA256_DARWIN_AMD64="6231d8d3b8f5552ec6cbf6d685bdd5482e1e703214b120e89b3bf0d7bf1ef725"
GO_SHA256_LINUX_ARM64="fe4789e92b1f33358680864bbe8704289e7bb5fc207d80623c308935bd696d49"
GO_SHA256_LINUX_AMD64="5c2c3b16caefa1d968a94c1daca04a7ca301a496d9b086e17ad77bb81393f053"

# go_is_usable: go on PATH with major.minor >= 1.21 (GOTOOLCHAIN takes it
# from there). Unparseable version output = not usable (fail toward
# bootstrap, which is deterministic).
go_is_usable() {
    command -v go >/dev/null 2>&1 || return 1
    go_ver_mm=$(go version 2>/dev/null | sed -n 's/^go version go\([0-9]*\.[0-9]*\).*/\1/p')
    [ -n "$go_ver_mm" ] || return 1
    [ "$(printf '%s\n' "$go_ver_mm" | cut -d. -f1)" -gt 1 ] 2>/dev/null && return 0
    [ "$(printf '%s\n' "$go_ver_mm" | cut -d. -f1)" -eq 1 ] 2>/dev/null \
        && [ "$(printf '%s\n' "$go_ver_mm" | cut -d. -f2)" -ge 21 ] 2>/dev/null
}

# bootstrap_go_toolchain: hermetic per-user Go install. Returns 0 with the
# toolchain's bin FIRST on PATH, non-zero (with a reason on stderr) when
# this host can't be bootstrapped — the caller then uses the container
# fallback. Download + verify + extract happen in a temp dir with an
# atomic rename at the end, so a killed run never leaves a half-installed
# toolchain that a later run would trust.
bootstrap_go_toolchain() {
    go_os=$(uname -s | tr '[:upper:]' '[:lower:]')
    case "$go_os" in
        darwin|linux) ;;
        *) echo "    bootstrap: unsupported OS '$go_os'" >&2; return 1 ;;
    esac
    case "$(uname -m)" in
        arm64|aarch64) go_arch=arm64 ;;
        x86_64|amd64)  go_arch=amd64 ;;
        *) echo "    bootstrap: unsupported arch '$(uname -m)'" >&2; return 1 ;;
    esac
    case "${go_os}-${go_arch}" in
        darwin-arm64) go_sha="$GO_SHA256_DARWIN_ARM64" ;;
        darwin-amd64) go_sha="$GO_SHA256_DARWIN_AMD64" ;;
        linux-arm64)  go_sha="$GO_SHA256_LINUX_ARM64" ;;
        linux-amd64)  go_sha="$GO_SHA256_LINUX_AMD64" ;;
    esac

    go_dest="$HOME/.molecule-ai/toolchains/${GO_BOOTSTRAP_VERSION}"
    if [ -x "$go_dest/go/bin/go" ]; then
        PATH="$go_dest/go/bin:$PATH"; export PATH
        echo "    Using cached hermetic Go toolchain: $go_dest"
        return 0
    fi

    go_tarball="${GO_BOOTSTRAP_VERSION}.${go_os}-${go_arch}.tar.gz"
    go_url="https://go.dev/dl/${go_tarball}"
    # Stage INSIDE the toolchains dir so the final mv is an atomic same-fs
    # rename — a killed run leaves only a .stage.* dir (never a half-install
    # the cache check above would trust), and a later run redoes it cleanly.
    mkdir -p "$(dirname "$go_dest")" || return 1
    go_tmp=$(mktemp -d "$(dirname "$go_dest")/.stage.XXXXXX") || return 1
    echo "==> Go not found — bootstrapping hermetic ${GO_BOOTSTRAP_VERSION} (${go_os}/${go_arch}, ~70MB, one-time)"
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL --retry 3 -o "$go_tmp/$go_tarball" "$go_url"
    elif command -v wget >/dev/null 2>&1; then
        wget -q -O "$go_tmp/$go_tarball" "$go_url"
    else
        echo "    bootstrap: neither curl nor wget available" >&2
        rm -rf "$go_tmp"; return 1
    fi || { echo "    bootstrap: download failed ($go_url)" >&2; rm -rf "$go_tmp"; return 1; }

    if command -v sha256sum >/dev/null 2>&1; then
        go_got=$(sha256sum "$go_tmp/$go_tarball" | cut -d' ' -f1)
    elif command -v shasum >/dev/null 2>&1; then
        go_got=$(shasum -a 256 "$go_tmp/$go_tarball" | cut -d' ' -f1)
    else
        echo "    bootstrap: no sha256 tool (sha256sum/shasum) — refusing unverified toolchain" >&2
        rm -rf "$go_tmp"; return 1
    fi
    if [ "$go_got" != "$go_sha" ]; then
        echo "    bootstrap: CHECKSUM MISMATCH for $go_tarball (got $go_got) — refusing" >&2
        rm -rf "$go_tmp"; return 1
    fi

    mkdir -p "$go_tmp/extract" \
        && tar -xzf "$go_tmp/$go_tarball" -C "$go_tmp/extract" \
        && mv "$go_tmp/extract" "$go_dest" \
        || { echo "    bootstrap: extract/install failed" >&2; rm -rf "$go_tmp"; return 1; }
    rm -rf "$go_tmp"

    PATH="$go_dest/go/bin:$PATH"; export PATH
    echo "    Hermetic Go installed: $go_dest (system untouched; delete that dir to remove)"
    return 0
}

if ! go_is_usable; then
    bootstrap_go_toolchain || true
fi

if command -v go >/dev/null 2>&1; then
    echo "==> Starting Platform (Go :${PLATFORM_PORT})"
    cd "$ROOT/workspace-server"
    # PORT inline (not exported) so the canvas child below doesn't inherit it.
    PORT="$PLATFORM_PORT" go run ./cmd/server > /tmp/molecule-platform.log 2>&1 &
    PLATFORM_PID=$!
else
    echo "==> Go not found on PATH — falling back to docker-compose platform service"
    echo "    (Install Go 1.25+ for faster iteration: https://go.dev/dl/)"
    cd "$ROOT"
    # Bring up just the platform service from docker-compose.yml. infra/setup.sh
    # already brought up postgres+redis+etc on docker-compose.infra.yml; this
    # adds the platform container on top. docker-compose.yml already reads
    # dynamic ports via ${PLATFORM_PUBLISH_PORT}/${PLATFORM_PORT}; publish the
    # container on the same dynamic host port we chose above so the
    # wait-for-/health loop (127.0.0.1:$PLATFORM_PORT) matches.
    export PLATFORM_PORT PLATFORM_PUBLISH_PORT="$PLATFORM_PORT"
    # Truncate the log once, then APPEND both steps so the --fresh from-scratch
    # build transcript is preserved above the up output (a bare `>` on the up
    # would clobber it) while a normal run still starts from an empty log.
    : > /tmp/molecule-platform.log
    if [ "$FRESH" -eq 1 ]; then
        # --fresh: force a from-scratch image first; the shared `up --build` below
        # is then a fast cache hit rather than a duplicated up + failure handler.
        echo "    --fresh: rebuilding platform image with --no-cache"
        docker compose build --no-cache platform >> /tmp/molecule-platform.log 2>&1 || {
            echo "    ✗ docker compose build --no-cache platform failed — see /tmp/molecule-platform.log"
            exit 1
        }
    fi
    docker compose up -d --build platform >> /tmp/molecule-platform.log 2>&1 || {
        echo "    ✗ docker compose up platform failed — see /tmp/molecule-platform.log"
        echo "    Either install Go 1.25+ (https://go.dev/dl/) and rerun, or fix the docker fallback."
        exit 1
    }
    # PLATFORM_PID is unset on this path; cleanup() handles that with `kill ... 2>/dev/null || true`.
    PLATFORM_PID=
fi

echo "    Waiting for Platform /health..."
PLATFORM_READY=0
for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20 \
         21 22 23 24 25 26 27 28 29 30; do
    if curl -sf "http://127.0.0.1:${PLATFORM_PORT}/health" >/dev/null 2>&1; then
        echo "    Platform ready (t+${i}s)"
        PLATFORM_READY=1
        break
    fi
    sleep 1
done
if [ "$PLATFORM_READY" -ne 1 ]; then
    echo "    ✗ Platform did not respond in 30s — check /tmp/molecule-platform.log"
    exit 1
fi

# ─────────────────────────────────────────────── 4. canvas

echo "==> Starting Canvas (Next.js :${CANVAS_PORT})"
cd "$ROOT/canvas"
if [ ! -d node_modules ]; then
    echo "    First-run: npm install (~30-60s)"
    npm install
fi
# Invoke Next directly with the dynamic port — package.json's dev script pins
# `-p 3000`, which would override a PORT env var. NEXT_PUBLIC_* / CORS_ORIGINS
# were exported above so the browser bundle and the platform's CORS agree.
./node_modules/.bin/next dev --turbopack -p "$CANVAS_PORT" > /tmp/molecule-canvas.log 2>&1 &
CANVAS_PID=$!

echo "    Waiting for Canvas HTTP 200..."
# 180s, liveness-gated: a COLD Next.js/Turbopack compile on a loaded machine
# routinely exceeds 30s, and the old hard 30s gate tore down the whole stack
# (platform included) for a canvas that was seconds from ready. Keep waiting
# as long as the canvas process is alive; bail fast if it DIED.
CANVAS_READY=0
i=0
while [ "$i" -lt 180 ]; do
    i=$((i + 1))
    code=$(curl -sf -o /dev/null -w "%{http_code}" "http://127.0.0.1:${CANVAS_PORT}/" 2>/dev/null || echo "0")
    if [ "$code" = "200" ]; then
        echo "    Canvas ready (t+${i}s)"
        CANVAS_READY=1
        break
    fi
    if [ -n "${CANVAS_PID:-}" ] && ! kill -0 "$CANVAS_PID" 2>/dev/null; then
        echo "    ✗ Canvas process exited during startup — check /tmp/molecule-canvas.log"
        break
    fi
    if [ $((i % 30)) -eq 0 ]; then
        echo "    still compiling canvas — ${i}s elapsed"
    fi
    sleep 1
done
if [ "$CANVAS_READY" -ne 1 ]; then
    echo "    ✗ Canvas did not respond in 180s — check /tmp/molecule-canvas.log"
    exit 1
fi

# ─────────────────────────────────────────────── 5. readiness banner

cat <<EOF

═══════════════════════════════════════════════════════════
  Molecule AI dev environment ready

  ── Open in a browser ─────────────────────────────────────
  Canvas:    http://127.0.0.1:${CANVAS_PORT}
  Langfuse:  ${LANGFUSE_HOST}   (LLM traces)
             login: dev@molecule.local / dev-langfuse-pw

  ── Backing services (dynamic ports; URLs wired for you) ───
  Platform:  ${NEXT_PUBLIC_PLATFORM_URL}   (API, loopback-bound in dev)
  Postgres:  127.0.0.1:${MOLECULE_PG_HOST_PORT}   (\$DATABASE_URL exported)
  Redis:     127.0.0.1:${MOLECULE_REDIS_HOST_PORT}
  MinIO:     127.0.0.1:${MOLECULE_MINIO_HOST_PORT} (S3 API, bucket: ${MOLECULE_WORKSPACE_DATA_BUCKET})

  Auth:      fail-closed — canvas uses the dev ADMIN_TOKEN (see .env)
  Logs:      /tmp/molecule-platform.log · /tmp/molecule-canvas.log
             docker logs ${LANGFUSE_CONTAINER}   (trace sink)
             docker logs molecule-core-minio-1      (object store)

  Ports are dynamic: the conventional port is used when free, else a free
  one is picked so nothing on your machine is hijacked. Every URL above is
  exported for you (DATABASE_URL, REDIS_URL, LANGFUSE_*,
  MOLECULE_OBJECT_STORE_*, MOLECULE_WORKSPACE_DATA_*, CORS_ORIGINS,
  NEXT_PUBLIC_*). psql in with:  psql "\$DATABASE_URL"

  First run:
    1. Open the Canvas. On a fresh self-host DB a fullscreen setup scene
       configures the platform agent (runtime · provider · model · key);
       an already-configured stack goes straight to the workspace.
    2. Chat with your platform agent (the org concierge) in My Chat.
    3. See exactly what's sent to the LLM — consolidated system prompt,
       tool calls, inputs/outputs — in the Langfuse UI above (or the
       canvas Traces tab, per workspace).

  Press Ctrl-C to stop all services.
═══════════════════════════════════════════════════════════
EOF

wait
