#!/usr/bin/env bash
# Staging E2E — fresh-provision peer-visibility gate via the LITERAL MCP path.
#
# WHY THIS EXISTS
# ---------------
# Hermes and OpenClaw were repeatedly reported "fleet-verified / cascade-
# complete" because the *proxy* signals were green:
#   - registry-registration + heartbeat (Hermes), and
#   - model round-trip 200 (OpenClaw).
# But a freshly-provisioned workspace, asked on canvas "can you see your
# peers", actually FAILS:
#   - Hermes: 401 on the molecule MCP `list_peers` call,
#   - OpenClaw: falls back to native `sessions_list`, sees no platform peers.
# Tasks #142/#159 were even marked "completed" under this same proxy flaw.
#
# This script codifies the LITERAL user-facing path so it can never silently
# regress: it provisions a brand-new throwaway org + sibling workspaces via
# the real control-plane provisioning path, then for each runtime that should
# have platform peer-visibility it drives the EXACT MCP call the canvas agent
# makes — `POST /workspaces/:id/mcp` JSON-RPC tools/call name=list_peers,
# authenticated by that workspace's own bearer token through the real
# WorkspaceAuth + MCPRateLimiter middleware chain. It then asserts:
#   (1) HTTP 200,
#   (2) JSON-RPC `result` present (NOT an `error` object — a -32000
#       "tool call failed" or a 401 from WorkspaceAuth fails here),
#   (3) the returned peer set CONTAINS the other provisioned sibling
#       workspace IDs — not an empty list, not a native-sessions fallback.
#
# This is NOT a proxy. It does not look at a registry row, /health, the
# heartbeat table, or `GET /registry/:id/peers`. It drives the byte-for-byte
# JSON-RPC envelope that mcp_molecule_list_peers issues from a real agent.
#
# It is written to FAIL on today's broken Hermes/OpenClaw behavior and go
# green only when the in-flight root-cause fixes (Hermes-401, OpenClaw MCP
# wiring) actually land. That is the point: it is the objective proof gate.
#
# AUTH MODEL (mirrors tests/e2e/test_staging_full_saas.sh)
# --------------------------------------------------------
#   Single MOLECULE_ADMIN_TOKEN (= staging CP_ADMIN_API_TOKEN from Infisical)
#   drives: POST /cp/admin/orgs (provision), GET
#   /cp/admin/orgs/:slug/admin-token (per-tenant token), DELETE
#   /cp/admin/tenants/:slug (teardown). The per-tenant admin token drives
#   tenant workspace creation. When a managed runtime create response
#   does not expose a one-time workspace token, the same tenant admin/session
#   bearer drives the MCP call through WorkspaceAuth. No dev-only admin
#   token-mint routes are used in this E2E (feedback_no_dev_only_routes_in_e2e).
#
# Required env:
#   MOLECULE_ADMIN_TOKEN   staging CP admin bearer from Infisical /shared/controlplane-admin
# Optional env:
#   MOLECULE_CP_URL        default https://staging-api.moleculesai.app
#   E2E_RUN_ID             slug suffix; CI passes ${GITHUB_RUN_ID}
#   PV_RUNTIMES            space list; default "hermes openclaw claude-code"
#   E2E_PROVISION_TIMEOUT_SECS  default 1800 (hermes/openclaw cold-provision budget)
#   E2E_MINIMAX_API_KEY / E2E_ANTHROPIC_API_KEY / E2E_OPENAI_API_KEY
#                          DEPRECATED for this script — platform-managed models
#                          use the CP LLM proxy; direct vendor keys are blocked
#                          by PR #2291. Kept in workflow env for other E2Es.
#   PV_TOKEN_DIAGNOSTIC_ONLY
#                          1 -> stop after create/token acquisition. Useful
#                          to classify Hermes-only vs shared auth-route issues.
#   E2E_KEEP_ORG           1 → skip teardown (local debugging only)
#
# Exit codes:
#   0  every runtime saw its peers via the literal MCP call
#   1  generic failure
#   2  missing required env
#   3  provisioning timed out
#   4  teardown left orphan resources
#   10 peer-visibility regression reproduced (the gate firing as designed)

set -uo pipefail

# The literal MCP list_peers assertion lives in the shared, backend-
# agnostic lib so it is BYTE-IDENTICAL between this staging backend and
# the local docker-compose backend (tests/e2e/test_peer_visibility_mcp_
# local.sh). Only provisioning/teardown differs per backend.
# shellcheck source=tests/e2e/lib/peer_visibility_assert.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/peer_visibility_assert.sh"
# Shared ephemeral-topology contract (derive_tenant_topology). Default (all
# MOLECULE_TENANT_* unset) reproduces exact staging routing byte-for-byte; the
# ephemeral runner points MOLECULE_TENANT_URL at the CP base +
# MOLECULE_TENANT_ROUTE_DOMAIN=lvh.me so the CP wildcard proxy resolves the tenant.
# shellcheck source=tests/e2e/lib/tenant_topology.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/tenant_topology.sh"

CP_URL="${MOLECULE_CP_URL:-https://staging-api.moleculesai.app}"
ADMIN_TOKEN="${MOLECULE_ADMIN_TOKEN:?MOLECULE_ADMIN_TOKEN required — load staging CP_ADMIN_API_TOKEN from Infisical /shared/controlplane-admin}"
# RUN_ID_SUFFIX removed (core#2782 follow-up shellcheck): the slug
# now comes from make_collision_proof_slug below; the old suffix
# var is dead.
PV_RUNTIMES="${PV_RUNTIMES:-hermes openclaw claude-code}"
PROVISION_TIMEOUT_SECS="${E2E_PROVISION_TIMEOUT_SECS:-1800}"
# Provisioning mode: `managed` (default, unset) = real runtime workspaces booted
# to `online` (exact staging behaviour). `external` = `external:true` rows,
# awaiting_agent, NO container/LLM boot — the ephemeral-CP secret-free port that
# exercises ONLY the platform-side literal-MCP peer-visibility contract.
PV_PROVISION_MODE="${PV_PROVISION_MODE:-managed}"
# Ephemeral slug-routing headers; set by derive_tenant_topology in step 3. Init
# empty so tenant_call is safe under `set -u` before derivation and on staging.
TENANT_ROUTE_HDRS=()

# Collision-proof slug (core#2782). The prior `head -c 32` truncation
# dropped the run_attempt suffix and let two parallel/retry runs
# collide (POST /cp/admin/orgs 409). The helper appends a random
# 8-char uuid so every run gets a unique slug regardless of how
# the workflow composes E2E_RUN_ID. The `source` + `assert` run
# AFTER log/fail/ok are defined below so the assert can call `fail`
# on mismatch. Slug MUST start with 'e2e-' so the
# sweep-stale-e2e-orgs safety net (EPHEMERAL_PREFIXES) catches any
# leak this run fails to tear down.
# shellcheck source=lib/collision-proof-slug.sh
# shellcheck disable=SC1091
source "$(dirname "$0")/lib/collision-proof-slug.sh"

ORG_ID=""
TENANT_URL=""
TENANT_TOKEN=""

log()  { echo "[$(date +%H:%M:%S)] $*"; }
fail() { echo "[$(date +%H:%M:%S)] ❌ $*" >&2; exit 1; }
ok()   { echo "[$(date +%H:%M:%S)] ✅ $*"; }

# SLUG construction runs after log/fail/ok so the assert can call `fail`.
# core#65: pass prefix_len=7 ("e2e-pv-") so the helper's run_id
# budget is computed precisely against the CP's 31-char org-slug
# cap (the prior 33-char slug like
# `e2e-pv-20260614-364043-2-e560b630` was rejected by the CP
# with HTTP 400 BEFORE the MCP call, breaking the
# core-main "E2E Peer Visibility (push)" lane).
SLUG="e2e-pv-$(make_collision_proof_slug_suffix "${E2E_RUN_ID:-}" 7)"
assert_collision_proof_slug "$SLUG" || fail "Bug in make_collision_proof_slug: produced non-collision-proof slug '$SLUG'"

admin_call() {
  local method="$1" path="$2"; shift 2
  curl -sS -X "$method" "$CP_URL$path" \
    -H "Authorization: Bearer $ADMIN_TOKEN" \
    -H "Content-Type: application/json" "$@"
}
tenant_call() {
  local method="$1" path="$2"; shift 2
  curl -sS -X "$method" "$TENANT_URL$path" \
    ${TENANT_ROUTE_HDRS[@]+"${TENANT_ROUTE_HDRS[@]}"} \
    -H "Authorization: Bearer $TENANT_TOKEN" \
    -H "X-Molecule-Org-Id: $ORG_ID" \
    -H "Content-Type: application/json" "$@"
}

tenant_call_capture() {
  local method="$1" path="$2" out="$3"; shift 3
  curl -sS -o "$out" -w "%{http_code}" -X "$method" "$TENANT_URL$path" \
    ${TENANT_ROUTE_HDRS[@]+"${TENANT_ROUTE_HDRS[@]}"} \
    -H "Authorization: Bearer $TENANT_TOKEN" \
    -H "X-Molecule-Org-Id: $ORG_ID" \
    -H "Content-Type: application/json" "$@"
}

extract_auth_token() {
  python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    print(''); sys.exit(0)
print(d.get('auth_token') or d.get('connection', {}).get('auth_token') or '')
" 2>/dev/null
}

# ─── Scoped teardown ───────────────────────────────────────────────────
# Deletes ONLY the org this run created (DELETE /cp/admin/tenants/$SLUG
# with the {"confirm":$SLUG} fat-finger guard). Never a cluster-wide
# sweep — honors feedback_cleanup_after_each_test and
# feedback_never_run_cluster_cleanup_tests_on_live_platform. The
# workflow's always() step + sweep-stale-e2e-orgs are the outer nets.
teardown() {
  local rc=$?
  set +e
  if [ "${E2E_KEEP_ORG:-0}" = "1" ]; then
    echo ""
    log "[teardown] E2E_KEEP_ORG=1 — leaving $SLUG for debugging (REMEMBER TO DELETE)"
    exit "$rc"
  fi
  echo ""
  log "[teardown] DELETE /cp/admin/tenants/$SLUG (scoped to this run only)"
  admin_call DELETE "/cp/admin/tenants/$SLUG" --max-time 120 \
    -d "{\"confirm\":\"$SLUG\"}" >/dev/null 2>&1
  for j in $(seq 1 24); do
    LIST=$(admin_call GET "/cp/admin/orgs?limit=500" 2>/dev/null)
    LEAK=$(echo "$LIST" | python3 -c "
import sys, json
try: d = json.load(sys.stdin)
except Exception: print(1); sys.exit(0)
orgs = d if isinstance(d, list) else d.get('orgs', [])
print(sum(1 for o in orgs if o.get('slug') == '$SLUG' and o.get('instance_status') not in ('purged',) and o.get('status') != 'purged'))
" 2>/dev/null || echo 1)
    if [ "$LEAK" = "0" ]; then
      log "[teardown] ✓ $SLUG purged (after ${j}x5s)"
      exit "$rc"
    fi
    sleep 5
  done
  echo "::warning::[teardown] $SLUG still present after 120s — investigate now; sweep-stale-e2e-orgs retries on a later main push after its 90m age floor" >&2
  [ "$rc" -eq 0 ] && rc=4
  exit "$rc"
}
trap teardown EXIT INT TERM

# ─── 1. Provision the throwaway org ────────────────────────────────────
log "1/6 POST /cp/admin/orgs — slug=$SLUG"
CREATE=$(admin_call POST /cp/admin/orgs \
  -d "{\"slug\":\"$SLUG\",\"name\":\"E2E peer-visibility $SLUG\",\"owner_user_id\":\"e2e-runner:$SLUG\"}")
ORG_ID=$(echo "$CREATE" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null)
[ -n "$ORG_ID" ] || fail "org creation failed: $(echo "$CREATE" | head -c 300)"
log "    ORG_ID=$ORG_ID"

# ─── 2. Wait for tenant environment + DNS ──────────────────────────────
log "2/6 waiting for tenant instance_status=running (provider + routing)..."
DEADLINE=$(( $(date +%s) + PROVISION_TIMEOUT_SECS ))
while true; do
  [ "$(date +%s)" -gt "$DEADLINE" ] && fail "tenant never came up within ${PROVISION_TIMEOUT_SECS}s"
  STATUS=$(admin_call GET "/cp/admin/orgs?limit=500" 2>/dev/null | python3 -c "
import sys, json
try: d = json.load(sys.stdin)
except Exception: sys.exit(0)
orgs = d if isinstance(d, list) else d.get('orgs', [])
for o in orgs:
    if o.get('slug') == '$SLUG':
        print(o.get('instance_status') or o.get('status') or 'unknown'); break
" 2>/dev/null)
  case "$STATUS" in running|online|ready) break ;; esac
  sleep 10
done
log "    tenant status=$STATUS"

# ─── 3. Per-tenant admin token + tenant URL ────────────────────────────
log "3/6 fetching per-tenant admin token..."
TT_RESP=$(admin_call GET "/cp/admin/orgs/$SLUG/admin-token")
TENANT_TOKEN=$(echo "$TT_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('admin_token',''))" 2>/dev/null)
[ -n "$TENANT_TOKEN" ] || fail "tenant token fetch failed: $(echo "$TT_RESP" | head -c 200)"

# Ephemeral-topology-aware tenant routing (shared contract). Default (all
# MOLECULE_TENANT_* unset) sets TENANT_URL=https://$SLUG.<derived-domain> with an
# EMPTY TENANT_ROUTE_HDRS ⇒ exact staging behaviour byte-for-byte. Under the
# ephemeral CP, MOLECULE_TENANT_URL=$CP_BASE_URL + MOLECULE_TENANT_ROUTE_DOMAIN
# make TENANT_URL the CP base and TENANT_ROUTE_HDRS carry Host + X-Molecule-Org-Slug.
derive_tenant_topology "$SLUG" "$CP_URL" || fail "could not derive tenant topology for $SLUG"
log "    tenant url: $TENANT_URL"
if [ ${#TENANT_ROUTE_HDRS[@]} -gt 0 ]; then
  log "    tenant routing via Host=$TENANT_ROUTE_HOST + X-Molecule-Org-Slug=$SLUG (ephemeral slug routing)"
fi

# Under ephemeral slug-routing the CP's GLOBAL /health answers ANY Host, so a
# /health probe is vacuous (2xx on iteration 1 without the tenant route up).
# Probe a tenant-owned handler WITH the routing headers so a not-yet-routable
# tenant is actually caught. Staging (empty TENANT_ROUTE_HDRS) keeps /health.
log "3b. waiting for tenant readiness (TLS/DNS, up to 10min)..."
for i in $(seq 1 120); do
  if [ ${#TENANT_ROUTE_HDRS[@]} -gt 0 ]; then
    curl -fsSk -m 5 "${TENANT_ROUTE_HDRS[@]}" -H "X-Molecule-Org-Id: $ORG_ID" "$TENANT_URL/org/identity" >/dev/null 2>&1 && { log "    tenant route ready (attempt $i)"; break; }
  else
    curl -fsS "$TENANT_URL/health" -m 5 -k >/dev/null 2>&1 && { log "    /health ok (attempt $i)"; break; }
  fi
  sleep 5
done
BUILDINFO=$(curl -fsS "$TENANT_URL/buildinfo" -m 10 2>/dev/null || true)
if [ -n "$BUILDINFO" ]; then
  log "    tenant buildinfo: $(echo "$BUILDINFO" | head -c 300)"
else
  log "    tenant buildinfo unavailable"
fi

# ─── 4. Provision the parent + one sibling per runtime under test ──────
# Platform-managed models: Molecule owns billing via the CP LLM proxy, so
# the workspace needs NO tenant key. PR #2291 blocks direct vendor key writes
# (ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN, MINIMAX_API_KEY, etc.) for
# platform-managed workspaces. We intentionally keep SECRETS_JSON empty so a
# stray E2E_*_API_KEY in the runner env cannot silently convert this into a
# BYOK run and mask the platform-managed path (mirrors
# test_staging_full_saas.sh's E2E_LLM_PATH=platform branch).
SECRETS_JSON='{}'

# Workspace-create now enforces the MODEL_REQUIRED contract: there is NO
# platform-side default model for a runtime (feedback_workspace_model_required_
# no_platform_default). Every create MUST carry an explicit `model`, or the CP
# rejects it with MODEL_REQUIRED before this gate's peer-visibility assertion
# can run. We pick a PLATFORM-MANAGED id (Molecule owns billing — no tenant key
# needed; this gate only needs the workspace to boot + list peers, not heavy
# LLM work), validated against the controlplane providers SSOT
# (internal/providers/providers.yaml runtimes.<rt>.providers[platform].models):
#   claude-code → anthropic/claude-sonnet-4-6   (platform claude model)
#   hermes/openclaw → moonshot/kimi-k2.6         (their only platform family)
# E2E_MODEL_SLUG overrides for operator-dispatched runs.
pv_platform_model_for_runtime() {
  if [ -n "${E2E_MODEL_SLUG:-}" ]; then printf '%s' "$E2E_MODEL_SLUG"; return 0; fi
  case "$1" in
    claude-code) printf 'anthropic/claude-sonnet-4-6' ;;
    hermes|openclaw) printf 'moonshot/kimi-k2.6' ;;
    *) printf 'moonshot/kimi-k2.6' ;;
  esac
}

log "4/6 provisioning parent + one sibling per runtime under test (mode=${PV_PROVISION_MODE})..."
# EXTERNAL mode (the ephemeral-CP port): create `external:true` rows so the
# literal-MCP peer-visibility contract runs WITHOUT booting runtime containers /
# backed platform models. Default (mode=managed, unset) = exact staging behaviour.
if [ "$PV_PROVISION_MODE" = "external" ]; then
  PARENT_RT="external"; PARENT_MODEL="external:custom"; PARENT_EXTRA=',"external":true'
else
  PARENT_RT="claude-code"; PARENT_MODEL=$(pv_platform_model_for_runtime claude-code); PARENT_EXTRA=""
fi
P_RESP=$(tenant_call POST /workspaces \
  -d "{\"name\":\"pv-parent\",\"runtime\":\"$PARENT_RT\",\"model\":\"$PARENT_MODEL\",\"tier\":3$PARENT_EXTRA,\"secrets\":$SECRETS_JSON}")
PARENT_ID=$(echo "$P_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null)
[ -n "$PARENT_ID" ] || fail "parent create failed: $(echo "$P_RESP" | head -c 300)"
log "    PARENT_ID=$PARENT_ID"

# WS_IDS[runtime]=id ; WS_TOKENS[runtime]=auth_token (the MCP bearer)
declare -A WS_IDS WS_TOKENS
ALL_WS_IDS="$PARENT_ID"
for rt in $PV_RUNTIMES; do
  if [ "$PV_PROVISION_MODE" = "external" ]; then
    CREATE_RT="external"; RT_MODEL="external:custom"; RT_EXTRA=',"external":true'
  else
    CREATE_RT="$rt"; RT_MODEL=$(pv_platform_model_for_runtime "$rt"); RT_EXTRA=""
  fi
  R=$(tenant_call POST /workspaces \
    -d "{\"name\":\"pv-$rt\",\"runtime\":\"$CREATE_RT\",\"model\":\"$RT_MODEL\",\"tier\":2,\"parent_id\":\"$PARENT_ID\"$RT_EXTRA,\"secrets\":$SECRETS_JSON}")
  WID=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null)
  WTOK=$(echo "$R" | extract_auth_token)
  [ -n "$WID" ] || fail "$rt workspace create failed: $(printf '%s' "$R" | head -c 300)"
  if [ -z "$WTOK" ]; then
    log "    $rt create response did not include workspace auth_token; using tenant admin/session bearer for MCP auth"
    WTOK="$TENANT_TOKEN"
  fi
  WS_IDS[$rt]="$WID"
  WS_TOKENS[$rt]="$WTOK"
  ALL_WS_IDS="$ALL_WS_IDS $WID"
  log "    $rt → $WID"
done

if [ "${PV_TOKEN_DIAGNOSTIC_ONLY:-0}" = "1" ]; then
  ok "token diagnostic passed for runtimes: $PV_RUNTIMES"
  exit 0
fi

# ─── 5. Wait for every sibling online ──────────────────────────────────
# EXTERNAL rows are `awaiting_agent` by design (no container ever reaches
# `online`); the literal-MCP peer path is PLATFORM-side, so skip the boot-wait.
if [ "$PV_PROVISION_MODE" = "external" ]; then
  log "5/6 external mode — workspaces are awaiting_agent; skipping the online-wait, driving MCP directly"
else
log "5/6 waiting for all workspaces status=online (up to ${PROVISION_TIMEOUT_SECS}s — cold boot)..."
WS_DEADLINE=$(( $(date +%s) + PROVISION_TIMEOUT_SECS ))
for rt in $PV_RUNTIMES; do
  wid="${WS_IDS[$rt]}"
  LAST=""
  while true; do
    [ "$(date +%s)" -gt "$WS_DEADLINE" ] && fail "$rt ($wid) never reached online (last=$LAST)"
    S=$(tenant_call GET "/workspaces/$wid" 2>/dev/null | python3 -c "
import sys, json
try: d = json.load(sys.stdin)
except Exception: sys.exit(0)
w = d.get('workspace') if isinstance(d.get('workspace'), dict) else d
print(w.get('status') or '')
" 2>/dev/null)
    [ "$S" != "$LAST" ] && { log "    $rt → $S"; LAST="$S"; }
    case "$S" in
      online) break ;;
      failed) sleep 10 ;;   # transient: bootstrap-watcher 5-min deadline, heartbeat recovers
      *)      sleep 10 ;;
    esac
  done
  ok "    $rt online"
done
fi

# ─── 6. THE GATE — literal mcp_molecule_list_peers via POST /:id/mcp ────
# This is the byte-for-byte user-facing call. NOT GET /registry/:id/peers,
# NOT /health, NOT the heartbeat table. JSON-RPC 2.0 tools/call,
# name=list_peers, authenticated through WorkspaceAuth + MCPRateLimiter.
log "6/6 driving the LITERAL list_peers MCP call per runtime..."
echo ""
REGRESSED=0
declare -A VERDICT
# Thread the ephemeral slug-routing headers into the SHARED MCP assertion. The
# literal list_peers curl lives in peer_visibility_assert.sh; under the ephemeral
# CP the MCP call goes to the CP base URL and the tenant is resolved by Host +
# X-Molecule-Org-Slug. pv_assert_runtime reads PV_ROUTE_HDRS in-scope (sourced
# lib). Default (staging/local, empty) ⇒ unchanged.
# shellcheck disable=SC2034  # consumed cross-file in lib/peer_visibility_assert.sh
PV_ROUTE_HDRS=(${TENANT_ROUTE_HDRS[@]+"${TENANT_ROUTE_HDRS[@]}"})

for rt in $PV_RUNTIMES; do
  wid="${WS_IDS[$rt]}"
  wtok="${WS_TOKENS[$rt]}"
  # Byte-identical assertion via the shared lib. Staging passes ORG_ID as
  # the X-Molecule-Org-Id header value; the literal MCP call + every
  # anti-proxy / anti-native-fallback guarantee is the SAME code the
  # local backend runs.
  PV_VERDICT=""
  pv_assert_runtime "$rt" "$wid" "$wtok" "$TENANT_URL" "$ORG_ID" "$ALL_WS_IDS" || REGRESSED=1
  VERDICT[$rt]="$PV_VERDICT"
  echo ""
done

echo "=== SUMMARY — fresh-provision peer-visibility (literal MCP list_peers) ==="
for rt in $PV_RUNTIMES; do
  printf '  %-14s %s\n' "$rt" "${VERDICT[$rt]:-NO_RUN}"
done
echo ""

if [ "$REGRESSED" -ne 0 ]; then
  echo "✗ GATE FAILED — at least one runtime cannot see its peers via the"
  echo "  literal mcp_molecule_list_peers call. This is the real user-facing"
  echo "  failure the proxy signals (registry row / heartbeat / model 200)"
  echo "  were hiding. Expected RED until the Hermes-401 + OpenClaw-MCP-wiring"
  echo "  root-cause fixes land; goes green only when they actually do."
  exit 10
fi

ok "GATE PASSED — every runtime under test sees its platform peers via the literal MCP call."
exit 0
