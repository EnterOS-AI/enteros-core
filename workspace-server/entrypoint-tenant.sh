#!/bin/sh
# Tenant entrypoint — starts both Go platform (API) and Canvas (UI).
#
# Container runs as non-root 'canvas' user (USER directive in Dockerfile.tenant).
# Both processes start as non-root. SIGTERM propagates to child processes via the
# shell's trap + wait -n pattern below.
#
# Go platform listens on :8080 (deployment health checks hit this port).
# Canvas Node.js listens on :3000 (internal only).
# The Go platform's fallback handler proxies non-API routes to :3000
# so the browser only ever talks to :8080.
#
# If either process dies, we kill the other and exit non-zero so the
# container supervisor restarts the service.

set -e

# Start Canvas in background
cd /canvas
PORT=3000 HOSTNAME=0.0.0.0 node server.js &
CANVAS_PID=$!

# Memory v2 sidecar (built-in postgres plugin). See Dockerfile entrypoint
# comment for rationale.
#
# Spawn-gating: start the sidecar when MEMORY_PLUGIN_URL is set.
# Without it, the sidecar adds zero value and risks aborting tenant
# boot via the 30s health gate when the tenant Postgres lacks
# pgvector. Caught on staging redeploy 2026-05-05:
#   pq: extension "vector" is not available
#
# Defaults (when sidecar IS spawned): MEMORY_PLUGIN_DATABASE_URL
# falls back to the tenant's DATABASE_URL.
#
# Phase A3 (#1792): MEMORY_V2_CUTOVER acceptance removed. The variable
# was deprecated by #1747 (binary stopped reading it) and only kept
# alive here as a synonym to bridge old CP user-data templates. With
# A3 dropping the entire v1 surface, the synonym is gone too. CP
# user-data sets MEMORY_PLUGIN_URL directly; if a stale template
# without that var ships, the sidecar simply doesn't start and the
# tenant boots without memory — loud but recoverable, same posture as
# any other required env missing.
# >>> memory-sidecar-boot (extracted verbatim by entrypoint_tenant_memory_test.go
# — keep these markers; the test drives THIS code, not a copy of it)
MEMORY_PLUGIN_PID=""

# ── ON BY DEFAULT (task #4114) ───────────────────────────────────────────────
# The sidecar is BUNDLED IN THIS IMAGE and its address is a CONSTANT — loopback
# :9100. Nothing about it is deployment-specific, so there is nothing for an
# environment to configure. Requiring every provisioner to re-declare that
# constant is drift by construction, and it drifted: the cloud user-data paths
# set it (CP sharedTenantEnvLines -> bootstrap.go, guarded by a test across the
# three rendered user-datas), while the local-docker provisioner never did. Those
# tenants booted with NO memory sidecar, and since #1747 removed the silent
# agent_memories SQL fallback, every memory call answers
# 503 "memory plugin is not configured". That is the staging E2E red.
#
# Defaulting it HERE — in the one place that owns the sidecar — means no
# provisioner, and no environment (staging included), has to know the constant.
# The CP-side injections stay valid; they now just re-state the same value.
#
# Note the pgvector prerequisite is already satisfied on the paths that matter:
# LocalDocker.pgImage() defaults to pgvector/pgvector:pg16, and hosted tenants use
# the same requirement. The 2026-05-05 "extension vector is not available" incident is handled by
# the DEGRADE arm below, not by leaving memory off everywhere.
MEMORY_PLUGIN_BUNDLED_URL="http://localhost:9100"
if [ -z "$MEMORY_PLUGIN_URL" ]; then
  MEMORY_PLUGIN_URL="$MEMORY_PLUGIN_BUNDLED_URL"
fi
export MEMORY_PLUGIN_URL

# ── Which sidecar is this, and who owns its health? ──────────────────────────
# BUNDLED (loopback URL): the plugin binary in THIS image, which we start below.
# EXTERNAL (any other URL): a plugin the operator runs elsewhere — we must not
# start a sidecar nobody dials, and must not touch their URL.
#
# Note what this is NOT keyed on: whether anyone SET MEMORY_PLUGIN_URL. That was
# the first cut of this change and it was wrong. The control plane sets it on
# EVERY cloud tenant already (controlplane tenant_container_env.go:46 and
# ec2.go:2811 both emit MEMORY_PLUGIN_URL='http://localhost:9100'), so set-ness
# carries no operator intent — it is this same constant, restated. Keying
# fatal-vs-degrade on it would have made the DEGRADE arm below DEAD CODE on
# precisely the hosted fleet that motivated it: multiple provider backends saw the 2026-05-05
# `extension "vector" is not available` crash-loop actually happened.
memory_plugin_is_bundled=""
case "$MEMORY_PLUGIN_URL" in
  http://localhost:*|http://127.0.0.1:*|https://localhost:*|https://127.0.0.1:*)
    memory_plugin_is_bundled=1 ;;
esac

# memory_plugin_off: make the platform see "not configured" rather than let it
# dial a sidecar that is not there. /platform reads MEMORY_PLUGIN_URL at boot
# (internal/memory/wiring.Build) — empty means the bundle is nil and the memory
# endpoints answer a clean 503 with a clear reason, instead of failing on a
# refused connection to :9100. Only ever called for a BUNDLED sidecar — blanking
# an EXTERNAL url would silently disable an operator's own memory service.
memory_plugin_off() {
  MEMORY_PLUGIN_URL=""
  export MEMORY_PLUGIN_URL
  memory_plugin_is_bundled=""
}

# EXTERNAL is tested FIRST, before MEMORY_PLUGIN_DISABLE. MEMORY_PLUGIN_DISABLE
# only ever means "do not start the BUNDLED sidecar" — when the URL points at an
# external plugin there is no sidecar to skip, and blanking the URL would
# silently disable the operator's own memory service. That combination is not
# hypothetical: it is the DOCUMENTED way to run an external plugin (Dockerfile:
# "Set MEMORY_PLUGIN_DISABLE=1 to force-skip the sidecar even with cutover env
# set (e.g. running the plugin externally on a separate host)"). Testing DISABLE
# first would violate the invariant stated on memory_plugin_off above.
if [ -z "$memory_plugin_is_bundled" ]; then
  echo "memory-plugin: MEMORY_PLUGIN_URL=$MEMORY_PLUGIN_URL is not loopback — treating it as an EXTERNAL plugin: no sidecar started, URL left untouched." >&2
elif [ -n "$MEMORY_PLUGIN_DISABLE" ]; then
  echo "memory-plugin: MEMORY_PLUGIN_DISABLE set — bundled sidecar skipped; memory endpoints will 503." >&2
  memory_plugin_off
elif [ -z "$DATABASE_URL" ]; then
  echo "memory-plugin: DATABASE_URL is empty — sidecar skipped; memory endpoints will 503." >&2
  memory_plugin_off
fi

if [ -n "$memory_plugin_is_bundled" ]; then
  # Schema isolation (issue #1733): when defaulting from the tenant
  # DATABASE_URL we co-locate the plugin's tables under a dedicated
  # `memory_plugin` schema so they never collide with platform-tenant
  # tables in `public`. The plugin's 000_schema_bootstrap migration
  # creates the schema; search_path here directs every subsequent CREATE
  # TABLE / SELECT to land in it.
  #
  # The search_path includes `public` as a fallback so the `vector` type
  # resolves regardless of which schema pgvector was installed into.
  # Fresh tenants (no prior `CREATE EXTENSION vector`) install the
  # extension into `memory_plugin` (first writable schema in the path),
  # keeping the SSOT intent. Tenants where pgvector was already
  # installed into `public` by a prior boot or operator action keep the
  # extension where it is and resolve `vector(1536)` via the public
  # fallback — without this fallback those tenants would crash the
  # plugin boot with "type vector does not exist" once the migrations
  # try to create memory_records (#1742 review finding).
  #
  # Operators who explicitly set MEMORY_PLUGIN_DATABASE_URL (separate DB
  # entirely) keep full control — search_path is only injected when we
  # default from DATABASE_URL.
  if [ -z "$MEMORY_PLUGIN_DATABASE_URL" ]; then
    case "$DATABASE_URL" in
      *\?*) MEMORY_PLUGIN_DATABASE_URL="${DATABASE_URL}&search_path=memory_plugin,public" ;;
      *)    MEMORY_PLUGIN_DATABASE_URL="${DATABASE_URL}?search_path=memory_plugin,public" ;;
    esac
  fi
  : "${MEMORY_PLUGIN_LISTEN_ADDR:=:9100}"
  export MEMORY_PLUGIN_DATABASE_URL MEMORY_PLUGIN_LISTEN_ADDR
  echo "memory-plugin: starting sidecar on $MEMORY_PLUGIN_LISTEN_ADDR" >&2
  # MEMORY_PLUGIN_BIN is an indirection for tests only (the regression test in
  # entrypoint_tenant_memory_test.go points it at a stub). Prod is /memory-plugin.
  "${MEMORY_PLUGIN_BIN:-/memory-plugin}" &
  MEMORY_PLUGIN_PID=$!
  # Wait up to 30s for /v1/health. Boot failure is fatal so a misconfigured
  # tenant crash-loops instead of silently serving cutover traffic against
  # a dead plugin.
  #
  # Probe via node: this image is node:20-alpine-based and node is
  # guaranteed on PATH, while busybox wget is unreliable on slim
  # alpine variants (some pin to the busybox applet which lacks
  # --timeout=2 and exits non-zero on a successful HTTP 200 with an
  # empty body, causing a false negative). node's stdlib http.get
  # is portable across all our runtime adapter templates.
  health_port=${MEMORY_PLUGIN_LISTEN_ADDR#:}
  ready=0
  for _ in $(seq 1 30); do
    if HEALTH_PORT="$health_port" node -e '
const http = require("http");
const port = process.env.HEALTH_PORT;
const req = http.get({ host: "localhost", port, path: "/v1/health", timeout: 2000 }, (res) => {
  process.exit(res.statusCode === 200 ? 0 : 1);
});
req.on("error", () => process.exit(1));
req.on("timeout", () => { req.destroy(); process.exit(1); });
' >/dev/null 2>&1; then
      ready=1
      break
    fi
    sleep 1
  done
  if [ "$ready" != "1" ]; then
    # A sick sidecar is fatal only if someone SAID memory is required.
    #
    # MEMORY_PLUGIN_REQUIRED=1 — an operator declared memory load-bearing for
    # this deployment, so a broken plugin must crash-loop rather than quietly
    # serve without it.
    #
    # Otherwise — memory is on because WE turned it on, not because the tenant
    # asked. Do not brick it: kill the sidecar, blank the URL so /platform
    # reports a clean "not configured" 503, and boot. A tenant serving
    # everything-but-memory beats a tenant that will not start at all. This is
    # what makes on-by-default safe on a Postgres without pgvector (the
    # 2026-05-05 `extension "vector" is not available` incident).
    #
    # The requirement is a SEPARATE flag on purpose: MEMORY_PLUGIN_URL cannot
    # express it, because the CP already sets that URL on every cloud tenant (see
    # above), so inferring "required" from it would fire on the whole fleet.
    kill "$MEMORY_PLUGIN_PID" 2>/dev/null || true
    MEMORY_PLUGIN_PID=""
    if [ -n "${MEMORY_PLUGIN_REQUIRED:-}" ]; then
      echo "memory-plugin: ❌ /v1/health never returned 200 after 30s and MEMORY_PLUGIN_REQUIRED is set — aborting boot. Check DATABASE_URL reachability + pgvector extension + migrations." >&2
      kill "$CANVAS_PID" 2>/dev/null || true
      exit 1
    fi
    echo "memory-plugin: ⚠️  /v1/health never returned 200 after 30s — DEGRADING: tenant boots WITHOUT memory and memory endpoints will 503. Check DATABASE_URL reachability + pgvector extension + migrations. (Set MEMORY_PLUGIN_REQUIRED=1 to make this fatal, or MEMORY_PLUGIN_DISABLE=1 to skip the sidecar entirely.)" >&2
    memory_plugin_off
  else
    echo "memory-plugin: ✅ sidecar healthy on :$health_port" >&2
  fi
fi
# <<< memory-sidecar-boot

# Start Go platform in foreground-ish (we trap signals)
# CANVAS_PROXY_URL tells the platform to proxy unmatched routes to Canvas.
# CONTAINER_BACKEND: empty = Docker (default for current hosted and self-hosted
# deployments). The legacy "flyio" adapter remains available for deployments
# that still explicitly configure it.
export CANVAS_PROXY_URL="${CANVAS_PROXY_URL:-http://localhost:3000}"
cd /
/platform &
PLATFORM_PID=$!

# If any process exits, kill the others
cleanup() {
  kill $CANVAS_PID 2>/dev/null || true
  kill $PLATFORM_PID 2>/dev/null || true
  [ -n "$MEMORY_PLUGIN_PID" ] && kill $MEMORY_PLUGIN_PID 2>/dev/null || true
}
trap cleanup EXIT SIGTERM SIGINT

# Wait for any to exit — whichever exits first triggers cleanup
if [ -n "$MEMORY_PLUGIN_PID" ]; then
  wait -n $CANVAS_PID $PLATFORM_PID $MEMORY_PLUGIN_PID
else
  wait -n $CANVAS_PID $PLATFORM_PID
fi
EXIT_CODE=$?
cleanup
exit $EXIT_CODE
