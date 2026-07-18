#!/usr/bin/env bash
# Real-staging E2E for the concierge user_tasks primitive (Feature 3 of the
# concierge / platform-agent set). Exercises the FULL agent→user "ask" contract
# both surfaces expose, END-TO-END against a real local-Docker staging tenant:
#
#   REST (per-workspace, tenant-admin-token authenticated):
#     POST   /workspaces/:id/user-tasks              create an ask
#     GET    /workspaces/:id/user-tasks              this workspace's asks
#     GET    /user-tasks/pending          (AdminAuth) org-wide pending asks
#     PATCH  /workspaces/:id/user-tasks/:taskId      edit (scoped by ws id)
#     DELETE /workspaces/:id/user-tasks/:taskId      remove (scoped by ws id)
#     POST   /workspaces/:id/user-tasks/:taskId/resolve   done|dismissed
#
#   MCP a2a-bridge tools (POST /workspaces/:id/mcp, JSON-RPC tools/call):
#     request_user_action(title, detail?)   list_user_tasks()
#     update_user_task(user_task_id, …)      delete_user_task(user_task_id)
#
#   Cross-workspace authz: workspace B cannot PATCH/DELETE workspace A's task
#   (the user_tasks handler scopes every mutation by the URL :id, so a B-path
#   call against an A-owned task 404s — the same scoping the local
#   test_user_tasks_e2e.sh pins, here proven over the real tenant ws-server).
#
# Why a real-staging sibling to the LOCAL test_user_tasks_e2e.sh: the local one
# runs against a dev workspace-server with external/in-memory workspaces. This
# one provisions a REAL throwaway org + tenant (same CP-admin scaffolding as
# test_staging_full_saas.sh) and drives the user_tasks surfaces through the live
# tenant auth chain (TenantGuard + WorkspaceAuth + Cloudflare edge) — the exact
# path a canvas concierge agent hits in production. It REUSES the staging
# harness's env contract, org-provision/teardown shape, _lib.sh helpers, and the
# exact CP purge-receipt verifier, so lifecycle proof is shared, not duplicated.
#
# NOTE: user_tasks is a pure DB/handler primitive — no LLM container is needed.
# We DO NOT wait for any workspace to boot online (no MINIMAX/ANTHROPIC key
# required), which keeps this test fast and decoupled from runtime cold-boot flake.
# Workspaces are created in 'external' mode so the tenant ws-server registers
# the row without provisioning a worker container.
#
# Required env (same contract as test_staging_full_saas.sh):
#   MOLECULE_CP_URL        default: https://staging-api.moleculesai.app
#   MOLECULE_ADMIN_TOKEN   staging CP admin bearer from Infisical /shared/controlplane-admin
#
# Optional env:
#   E2E_PROVISION_TIMEOUT_SECS   default 900 (15 min cold tenant budget)
#   E2E_KEEP_ORG                 1 → skip teardown (debugging only)
#   E2E_RUN_ID                   slug suffix; CI: ${GITHUB_RUN_ID}-${RUN_ATTEMPT}
#   E2E_INFRA_BACKEND            required: local-docker (only active backend)
#
# Exit codes:
#   0  happy path
#   1  generic / assertion failure
#   2  missing required env
#   3  provisioning timed out
#   4  teardown receipt/audit/org-absence proof failed
set -euo pipefail

# _lib.sh gives us sanitize/admin-auth conventions shared across the suite.
# shellcheck disable=SC1091
# shellcheck source=_lib.sh
source "$(dirname "$0")/_lib.sh"
# Exact synchronous CP purge receipt + exact-org absence verifier.
# shellcheck disable=SC1091
# shellcheck source=lib/cp_purge_receipt.sh
source "$(dirname "$0")/lib/cp_purge_receipt.sh"
e2e_cp_require_local_backend || exit 2

CP_URL="${MOLECULE_CP_URL:-https://staging-api.moleculesai.app}"
e2e_cp_require_staging_origin "$CP_URL" || exit 2
ADMIN_TOKEN="${MOLECULE_ADMIN_TOKEN:?MOLECULE_ADMIN_TOKEN required — load staging CP_ADMIN_API_TOKEN from Infisical /shared/controlplane-admin}"
PROVISION_TIMEOUT_SECS="${E2E_PROVISION_TIMEOUT_SECS:-900}"
# RUN_ID_SUFFIX removed (core#2782 follow-up shellcheck): the slug now
# comes from make_collision_proof_slug below; the old suffix var is dead.

# Collision-proof slug (core#2782). The prior `head -c 32` truncation
# dropped the run_attempt suffix and let two parallel/retry runs
# collide (POST /cp/admin/orgs 409). The helper appends a random
# 8-char uuid so every run gets a unique slug regardless of how
# the workflow composes E2E_RUN_ID. The `source` + `assert` run
# AFTER log/fail/ok are defined below so the assert can call `fail`
# on mismatch. Slug MUST start with 'e2e-' so sweep-stale-e2e-orgs.yml
# + lint_cleanup_traps.sh reap any orphan. (The lint requires a
# quoted SLUG=... with a literal e2e-/rt-e2e- head.)
# shellcheck source=lib/collision-proof-slug.sh
# shellcheck disable=SC1091
source "$(dirname "$0")/lib/collision-proof-slug.sh"

log()  { echo "[$(date +%H:%M:%S)] $*"; }
fail() { echo "[$(date +%H:%M:%S)] ❌ $*" >&2; exit 1; }
ok()   { echo "[$(date +%H:%M:%S)] ✅ $*"; }

# SLUG construction runs after log/fail/ok so the assert can call `fail`.
SLUG="e2e-cncrg-$(make_collision_proof_slug_suffix "${E2E_RUN_ID:-}" 10)"
assert_collision_proof_slug "$SLUG" || fail "Bug in make_collision_proof_slug: produced non-collision-proof slug '$SLUG'"

PASS=0
FAIL=0
check() {  # <desc> <expected-substr> <actual>
  if echo "$3" | grep -qF -- "$2"; then echo "  PASS: $1"; PASS=$((PASS + 1));
  else echo "  FAIL: $1"; echo "    expected to contain: $2"; echo "    got: $(echo "$3" | head -c 300)"; FAIL=$((FAIL + 1)); fi
}
check_not() {  # <desc> <unexpected-substr> <actual>
  if echo "$3" | grep -qF -- "$2"; then echo "  FAIL: $1 (should NOT contain: $2)"; FAIL=$((FAIL + 1));
  else echo "  PASS: $1"; PASS=$((PASS + 1)); fi
}
check_code() {  # <desc> <expected> <actual>
  if [ "$3" = "$2" ]; then echo "  PASS: $1 (HTTP $3)"; PASS=$((PASS + 1));
  else echo "  FAIL: $1 (expected HTTP $2, got HTTP $3)"; FAIL=$((FAIL + 1)); fi
}

CURL_COMMON=(-sS --max-time 30)
TMPDIR_E2E=$(mktemp -d -t cncrg-staging-XXXXXX)

# ─── teardown trap (exact purge receipt/audit + exact-tenant HTTP 404) ────────
CLEANUP_DONE=0
ORG_ID=""
ORG_CREATION_VERIFIED=0
cleanup_org() {
  local entry_rc=$?
  [ "$CLEANUP_DONE" = "1" ] && return 0
  CLEANUP_DONE=1
  rm -rf "$TMPDIR_E2E" 2>/dev/null || true

  if [ "${E2E_KEEP_ORG:-0}" = "1" ]; then
    log "E2E_KEEP_ORG=1 — skipping teardown. Manually delete $SLUG when done."
    return 0
  fi
  if [ "$ORG_CREATION_VERIFIED" != "1" ]; then
    log "No verified creation-returned org identity for $SLUG — skipping destructive org teardown."
    case "$entry_rc" in 0|1|2|3|4) return 0 ;; *) exit 1 ;; esac
  fi
  log "🧹 Tearing down org $SLUG..."
  local purge_verify_rc=0
  e2e_cp_delete_and_verify_purge \
    "$CP_URL" "$ADMIN_TOKEN" "$SLUG" "$ORG_ID" || purge_verify_rc=$?
  case "$purge_verify_rc" in
    0) ;;
    2) exit 2 ;;
    *) exit 4 ;;
  esac
  case "$entry_rc" in 0|1|2|3|4) ;; *) exit 1 ;; esac
}
trap cleanup_org EXIT INT TERM

admin_call() {  # <method> <path> [curl args…]
  local method="$1" path="$2"; shift 2
  curl "${CURL_COMMON[@]}" -X "$method" "$CP_URL$path" \
    -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" "$@"
}

# ─── 0. Preflight ────────────────────────────────────────────────────────────
log "═══ Staging concierge user_tasks E2E ═══  CP=$CP_URL  Slug=$SLUG"
curl "${CURL_COMMON[@]}" "$CP_URL/health" >/dev/null || fail "CP health check failed"
ok "CP reachable"

# ─── 1+2. Create org + await provisioning (bounded retry on transient failure) ─
# A tenant can rarely land in instance_status=failed within seconds of create —
# a transient CP/host hiccup under concurrent-provision load. This was verified
# NOT to be systemic: 6 tenants provisioned concurrently against staging all
# reached running (the failure did not reproduce), so a single failed provision
# is a rare hiccup, not a real product break. One such hiccup must NOT red a whole
# post-merge lane. Re-provision a FRESH org up to E2E_PROVISION_ATTEMPTS times;
# a genuinely-broken provisioner exhausts the attempts and still fails LOUD (no
# masking). A provisioning TIMEOUT (capacity stall, never reaches a terminal
# state) is a different, non-transient signal and is NOT retried (exit 3).
PROVISION_ATTEMPTS="${E2E_PROVISION_ATTEMPTS:-3}"
provision_attempt=0
while : ; do
  provision_attempt=$(( provision_attempt + 1 ))

  # ─── 1. Create org ─────────────────────────────────────────────────────────
  log "1/6 Creating org $SLUG (attempt ${provision_attempt}/${PROVISION_ATTEMPTS})..."
  CREATE_BODYFILE="$TMPDIR_E2E/create-org-response.json"
  set +e
  CREATE_HTTP_CODE=$(admin_call POST /cp/admin/orgs \
    -o "$CREATE_BODYFILE" -w '%{http_code}' \
    -d "{\"slug\":\"$SLUG\",\"name\":\"E2E $SLUG\",\"owner_user_id\":\"e2e-runner:$SLUG\"}")
  CREATE_CURL_RC=$?
  set -e
  CREATE_RESP=$(cat "$CREATE_BODYFILE" 2>/dev/null || true)
  if [ "$CREATE_CURL_RC" != "0" ]; then
    fail "Org create request failed (curl_rc=$CREATE_CURL_RC http=${CREATE_HTTP_CODE:-000}); raw body: $CREATE_RESP"
  fi
  case "$CREATE_HTTP_CODE" in
    2[0-9][0-9]) ;;
    *) fail "Org create returned non-2xx (http=${CREATE_HTTP_CODE:-000}); raw body: $CREATE_RESP" ;;
  esac
  echo "$CREATE_RESP" | python3 -m json.tool >/dev/null || fail "Org create non-JSON: $CREATE_RESP"
  ORG_ID=$(echo "$CREATE_RESP" | python3 -c "import json,sys; print(json.load(sys.stdin).get('id',''))")
  e2e_cp_validate_org_id "$ORG_ID" \
    || fail "Org create response missing a valid UUID 'id': $CREATE_RESP"
  ORG_CREATION_VERIFIED=1
  e2e_cp_publish_creation_identity "$SLUG" "$ORG_ID" \
    || fail "Could not publish the verified org creation identity for teardown"
  ok "Org created (id=$ORG_ID)"

  # ─── 2. Wait for tenant provisioning ───────────────────────────────────────
  log "2/6 Waiting for tenant provisioning (up to ${PROVISION_TIMEOUT_SECS}s)..."
  DEADLINE=$(( $(date +%s) + PROVISION_TIMEOUT_SECS ))
  LAST_STATUS=""
  PROVISION_RESULT=""
  while true; do
    [ "$(date +%s)" -gt "$DEADLINE" ] && { PROVISION_RESULT="timeout"; break; }
    LIST_JSON=$(admin_call GET /cp/admin/orgs 2>/dev/null || echo '{"orgs":[]}')
    STATUS=$(echo "$LIST_JSON" | python3 -c "
import json, sys
d = json.load(sys.stdin)
for o in d.get('orgs', []):
    if o.get('slug') == '$SLUG':
        print(o.get('instance_status', '')); sys.exit(0)
print('')" 2>/dev/null || echo "")
    if [ "$STATUS" != "$LAST_STATUS" ]; then log "    status → $STATUS"; LAST_STATUS="$STATUS"; fi
    case "$STATUS" in
      running) PROVISION_RESULT="running"; break ;;
      failed)  PROVISION_RESULT="failed";  break ;;
      *)       sleep 15 ;;
    esac
  done

  [ "$PROVISION_RESULT" = "running" ] && break
  # A capacity/latency stall (never reached a terminal state) is a genuine,
  # non-transient signal — preserve the original exit 3 rather than masking it.
  [ "$PROVISION_RESULT" = "timeout" ] && exit 3

  # instance_status=failed. Out of attempts → fail loud (persistent, not a hiccup).
  if [ "$provision_attempt" -ge "$PROVISION_ATTEMPTS" ]; then
    fail "Tenant provisioning failed for $SLUG after ${provision_attempt} attempt(s) — persistent provisioning failure, not a transient hiccup"
  fi
  log "⚠ Tenant provisioning FAILED for $SLUG (attempt ${provision_attempt}/${PROVISION_ATTEMPTS}) — rare transient CP/host hiccup; purging the dead org and re-provisioning a FRESH one"
  # Best-effort fire-and-forget purge of the dead org (it never served). If it
  # lingers, the sweep-stale-e2e-orgs janitor reaps it; the eventual successful
  # org is torn down by the EXIT trap. Then mint a FRESH slug (never reuse one
  # the CP may still be deprovisioning) and loop.
  admin_call DELETE "/cp/admin/tenants/$SLUG" -d "{\"confirm\":\"$SLUG\"}" >/dev/null 2>&1 || true
  ORG_ID=""
  ORG_CREATION_VERIFIED=0
  SLUG="e2e-cncrg-$(make_collision_proof_slug_suffix "${E2E_RUN_ID:-}" 10)"
  assert_collision_proof_slug "$SLUG" \
    || fail "Bug in make_collision_proof_slug: produced non-collision-proof slug '$SLUG' on retry"
done
ok "Tenant provisioning complete"

# Derive tenant domain from CP hostname (prod vs staging).
CP_HOST=$(echo "$CP_URL" | sed -E 's#^https?://##; s#/.*$##')
case "$CP_HOST" in
  api.*)         DERIVED_DOMAIN="${CP_HOST#api.}" ;;
  staging-api.*) DERIVED_DOMAIN="staging.${CP_HOST#staging-api.}" ;;
  *)             DERIVED_DOMAIN="$CP_HOST" ;;
esac
TENANT_DOMAIN="${MOLECULE_TENANT_DOMAIN:-$DERIVED_DOMAIN}"
# MOLECULE_TENANT_URL override — the EPHEMERAL-CP path (mirrors
# test_staging_full_saas.sh). Staging front-doors each tenant at its own
# slug.<domain> subdomain, so the Host alone routes. An ephemeral CP is one
# throwaway container whose wildcard proxy resolves the tenant by SLUG, so the
# ephemeral runner points this at the CP base URL; default (unset) keeps the
# exact staging subdomain behavior.
TENANT_URL="${MOLECULE_TENANT_URL:-https://$SLUG.$TENANT_DOMAIN}"
log "    TENANT_URL=$TENANT_URL"

# Ephemeral-CP tenant ROUTING headers (mirrors test_staging_full_saas.sh): when
# MOLECULE_TENANT_URL points at the CP base URL, carry the routing slug via
# Host + X-Molecule-Org-Slug (the CP resolves the tenant by the Host-derived slug
# or this fallback). Default unset ⇒ no extra headers ⇒ exact staging behavior.
TENANT_ROUTE_HOST="${MOLECULE_TENANT_ROUTE_HOST:-}"
if [ -z "$TENANT_ROUTE_HOST" ] && [ -n "${MOLECULE_TENANT_ROUTE_DOMAIN:-}" ]; then
  TENANT_ROUTE_HOST="$SLUG.$MOLECULE_TENANT_ROUTE_DOMAIN"
fi
TENANT_ROUTE_HDRS=()
if [ -n "$TENANT_ROUTE_HOST" ]; then
  TENANT_ROUTE_HDRS=(-H "Host: $TENANT_ROUTE_HOST" -H "X-Molecule-Org-Slug: $SLUG")
  log "    tenant routing via Host=$TENANT_ROUTE_HOST + X-Molecule-Org-Slug=$SLUG (ephemeral-CP slug routing)"
fi

# Origin header: the tenant's gin-contrib/cors allows exactly ONE origin — its own
# public front-door (CORS_ORIGINS). In staging that IS $TENANT_URL
# (https://$SLUG.$domain), so the default below reproduces staging byte-for-byte.
# Under the ephemeral CP, $TENANT_URL is the CP base URL (NOT a tenant origin), so
# a same-value Origin would be cross-origin → an empty-body 403 from cors before
# any handler runs (the exact failure this port fixed). Precedence:
#
#   1. MOLECULE_TENANT_ORIGIN_TEMPLATE set → the SAME template the CP turns into the
#      tenant's CORS_ORIGINS, substituted with this run's slug so the Origin we
#      present is byte-identical to the origin the tenant allows. Always wins.
#   2. template UNSET but ephemeral slug-routing is active (TENANT_ROUTE_HDRS
#      non-empty ⇒ MOLECULE_TENANT_ROUTE_DOMAIN / _ROUTE_HOST in play) → $TENANT_URL
#      is the CP base URL, NOT a tenant origin, so using it verbatim would 403.
#      Instead DERIVE a tenant-scoped origin from the route host — the same
#      slug.<route-domain> the CP forms CORS_ORIGINS from (PublicURLForSlug) —
#      grounding scheme+port in the existing $TENANT_URL (or an explicit
#      MOLECULE_TENANT_ROUTE_PORT) rather than blindly reusing the CP base URL host.
#      This makes the Origin safe by construction even when a caller overrides
#      MOLECULE_TENANT_URL (e.g. to the CP base URL) but forgets the origin template.
#   3. no ephemeral routing (staging) → Origin=$TENANT_URL (the tenant's own
#      subdomain, which IS its CORS_ORIGINS) ⇒ exact staging behavior.
TENANT_ORIGIN="$TENANT_URL"
if [ -n "${MOLECULE_TENANT_ORIGIN_TEMPLATE:-}" ]; then
  TENANT_ORIGIN="${MOLECULE_TENANT_ORIGIN_TEMPLATE//\{slug\}/$SLUG}"
  log "    tenant CORS origin = $TENANT_ORIGIN (from MOLECULE_TENANT_ORIGIN_TEMPLATE)"
elif [ ${#TENANT_ROUTE_HDRS[@]} -gt 0 ]; then
  # Ephemeral slug-routing without an explicit origin template. $TENANT_URL points
  # at the CP base URL here, so it is NOT the origin the tenant's cors allows.
  # Rebuild the tenant-scoped origin from the route host + the scheme/port of the
  # existing $TENANT_URL (the loopback route domain resolves to the same host:port
  # the CP publishes on). Prefer an explicit MOLECULE_TENANT_ROUTE_PORT if given.
  ORIGIN_SCHEME="${TENANT_URL%%://*}"
  case "$ORIGIN_SCHEME" in http|https) ;; *) ORIGIN_SCHEME="" ;; esac
  ORIGIN_PORT="${MOLECULE_TENANT_ROUTE_PORT:-}"
  if [ -z "$ORIGIN_PORT" ]; then
    TU_HOSTPORT="${TENANT_URL#*://}"; TU_HOSTPORT="${TU_HOSTPORT%%/*}"
    case "$TU_HOSTPORT" in *:*) ORIGIN_PORT="${TU_HOSTPORT##*:}" ;; esac
  fi
  if [ -z "$ORIGIN_SCHEME" ] || [ -z "$TENANT_ROUTE_HOST" ]; then
    fail "Cannot derive a tenant CORS Origin for ephemeral slug-routing (scheme='$ORIGIN_SCHEME' route_host='$TENANT_ROUTE_HOST'). Set MOLECULE_TENANT_ORIGIN_TEMPLATE to the CP's LOCAL_TENANT_URL_TEMPLATE (with {slug})."
  fi
  case "$TENANT_ROUTE_HOST" in
    *:*) TENANT_ORIGIN="${ORIGIN_SCHEME}://${TENANT_ROUTE_HOST}" ;;               # route host already carries a port
    *)   TENANT_ORIGIN="${ORIGIN_SCHEME}://${TENANT_ROUTE_HOST}${ORIGIN_PORT:+:$ORIGIN_PORT}" ;;
  esac
  log "    tenant CORS origin = $TENANT_ORIGIN (derived from route host; MOLECULE_TENANT_ORIGIN_TEMPLATE unset)"
fi

# ─── 3. Per-tenant admin token + TLS readiness ───────────────────────────────
log "3/6 Fetching per-tenant admin token..."
TENANT_TOKEN=$(admin_call GET "/cp/admin/orgs/$SLUG/admin-token" \
  | python3 -c "import json,sys; print(json.load(sys.stdin).get('admin_token',''))" 2>/dev/null || echo "")
[ -z "$TENANT_TOKEN" ] && fail "Could not retrieve per-tenant admin token for $SLUG"
ok "Tenant admin token retrieved (len=${#TENANT_TOKEN})"

log "    Waiting for tenant TLS / DNS propagation..."
TLS_DEADLINE=$(( $(date +%s) + 15 * 60 ))
while true; do
  # Under ephemeral slug-routing, /health is answered by the CP's OWN global
  # handler for ANY Host, so probing it with the route headers proves NOTHING —
  # it 2xx's on iteration 1 without the tenant route ever being up (vacuous
  # readiness gate). Probe /org/identity — a tenant-owned handler the CP proxies
  # through — WITH the routing headers + X-Molecule-Org-Id, so a not-yet-routable
  # tenant is actually caught. Staging (empty TENANT_ROUTE_HDRS) keeps the global
  # /health check. Mirrors test_staging_full_saas.sh step 4.
  if [ ${#TENANT_ROUTE_HDRS[@]} -gt 0 ]; then
    curl -sSfk --max-time 5 "${TENANT_ROUTE_HDRS[@]}" -H "X-Molecule-Org-Id: $ORG_ID" "$TENANT_URL/org/identity" >/dev/null 2>&1 && break
  else
    curl -sSfk --max-time 5 "$TENANT_URL/health" >/dev/null 2>&1 && break
  fi
  [ "$(date +%s)" -gt "$TLS_DEADLINE" ] && fail "Tenant never became routable within 15m (probe: $( [ ${#TENANT_ROUTE_HDRS[@]} -gt 0 ] && echo '/org/identity via route headers' || echo '/health' ))"
  sleep 5
done
ok "Tenant reachable at $TENANT_URL"

# tenant_call: Authorization (tenant admin token, valid for every workspace) +
# X-Molecule-Org-Id (TenantGuard 404s without it) + Origin (Cloudflare edge).
tenant_call() {  # <method> <path> [curl args…]
  local method="$1" path="$2"; shift 2
  curl "${CURL_COMMON[@]}" -X "$method" "$TENANT_URL$path" \
    "${TENANT_ROUTE_HDRS[@]}" \
    -H "Authorization: Bearer $TENANT_TOKEN" \
    -H "X-Molecule-Org-Id: $ORG_ID" \
    -H "Origin: $TENANT_ORIGIN" "$@"
}

# Create an external workspace row (no worker container). Echoes its id.
#
# Bounded retry around the external-row create only. The external create still
# runs a DB transaction + post-commit token/status work before returning 201,
# so under staging control-plane latency the one-shot curl could exit rc=28
# (CURL_COMMON --max-time 30 -> "curl: (28) Operation timed out") and the helper
# parsed no id, hard-failing user_tasks before any assertion (issue #2743).
# This is provisioning-latency flake, not a user_tasks contract failure -- so we
# retry transient cases (rc=28 / connection error -> http 000, or 2xx-but-no-id)
# with a longer per-call timeout (mirroring the teardown DELETE --max-time 120)
# and short backoff. Semantic 4xx/5xx stay hard-red with the response body.
CREATE_WS_ATTEMPTS=${CREATE_WS_ATTEMPTS:-5}
CREATE_WS_MAX_TIME=${CREATE_WS_MAX_TIME:-90}
create_external_ws() {  # <name>
  local name="$1" attempt body code id rc
  for attempt in $(seq 1 "$CREATE_WS_ATTEMPTS"); do
    body=$(mktemp "$TMPDIR_E2E/ws_create.XXXXXX")
    # Longer --max-time wins over CURL_COMMON's 30s (later flag); capture rc so
    # rc=28 is classified as transient latency rather than a no-id hard fail.
    set +e
    code=$(tenant_call POST /workspaces --max-time "$CREATE_WS_MAX_TIME" \
      -H "Content-Type: application/json" \
      -d "{\"name\":\"$name\",\"tier\":1,\"runtime\":\"external\",\"external\":true}" \
      -o "$body" -w "%{http_code}" 2>/dev/null)
    rc=$?
    set -e
    id=$(python3 -c "import sys,re
b=open('$body',encoding='utf-8').read()
m=re.search(r'\"id\"\s*:\s*\"([^\"]+)\"', b)
print(m.group(1) if m else '')" 2>/dev/null || echo '')
    if [ -n "$id" ]; then echo "$id"; rm -f "$body"; return 0; fi
    # Semantic failure (got an HTTP response in 4xx/5xx) -> hard-red immediately.
    case "$code" in
      4??|5??)
        fail "external ws create '$name' failed HTTP $code: $(head -c 500 "$body")" ;;
    esac
    # Transient: rc=28 (timeout), connection error (code 000), or 2xx-with-no-id.
    log "    ws create '$name' transient (attempt $attempt/$CREATE_WS_ATTEMPTS: curl rc=$rc http=$code) -- retrying"
    rm -f "$body"
    sleep $(( attempt * 3 ))
  done
  fail "external ws create '$name' returned no id after $CREATE_WS_ATTEMPTS attempts (last curl rc=$rc http=$code; staging control-plane latency, rc=28 class)"
}

# MCP JSON-RPC tools/call against /workspaces/:id/mcp. Echoes the result text
# (result.content[].text). Persists HTTP code to a file (runs in $()).
MCP_CODE_FILE="$TMPDIR_E2E/mcp_code"
mcp_call() {  # <wsid> <tool> <args-json>
  local wsid="$1" tool="$2" args="$3" out code
  out="$TMPDIR_E2E/mcp_out"
  set +e
  code=$(tenant_call POST "/workspaces/$wsid/mcp" -H "Content-Type: application/json" \
    -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"$tool\",\"arguments\":$args}}" \
    -o "$out" -w "%{http_code}" 2>/dev/null)
  set -e
  printf '%s' "$code" > "$MCP_CODE_FILE"
  python3 -c "
import sys, json
try: d = json.load(open('$out'))
except Exception: print(''); sys.exit(0)
res = d.get('result') if isinstance(d, dict) else None
print(''.join(c.get('text','') for c in res.get('content', [])) if isinstance(res, dict) else '')"
}
mcp_http_code() { cat "$MCP_CODE_FILE" 2>/dev/null || echo ''; }

# ─── 4. Provision two workspaces (A raises asks, B probes cross-ws authz) ─────
log "4/6 Creating two tenant workspaces (external rows; no worker containers)..."
WS_A=$(create_external_ws "Concierge-UT-A-$$")
[ -n "$WS_A" ] || fail "ws-A create returned no id"
WS_B=$(create_external_ws "Concierge-UT-B-$$")
[ -n "$WS_B" ] || fail "ws-B create returned no id"
ok "ws-A=$WS_A  ws-B=$WS_B"

# ─── 5. user_tasks REST + MCP + authz ────────────────────────────────────────
log "5/6 user_tasks contract (REST + MCP + cross-ws authz)..."

# 5.1 REST create → 201, status pending
R=$(tenant_call POST "/workspaces/$WS_A/user-tasks" -H "Content-Type: application/json" \
  -d '{"title":"Review the Q3 draft","detail":"Need your sign-off before send"}' \
  -o "$TMPDIR_E2E/c.json" -w "%{http_code}" 2>/dev/null || echo "000")
BODY=$(cat "$TMPDIR_E2E/c.json" 2>/dev/null || echo "")
check_code "REST create user-task" "201" "$R"
check "create returns status pending" '"status":"pending"' "$BODY"
TASK_ID=$(echo "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('user_task_id',''))" 2>/dev/null || echo "")
[ -n "$TASK_ID" ] || fail "no user_task_id returned: $BODY"
log "    TASK_ID=$TASK_ID"

# 5.2 REST read (this workspace + admin org-wide pending)
R=$(tenant_call GET "/workspaces/$WS_A/user-tasks")
check "GET ws-A user-tasks contains the task" "$TASK_ID" "$R"
check "GET ws-A user-tasks shows title" 'Review the Q3 draft' "$R"
R=$(tenant_call GET "/user-tasks/pending")
check "GET /user-tasks/pending (admin) contains the task" "$TASK_ID" "$R"
check "pending entry carries workspace_name" "Concierge-UT-A-$$" "$R"

# 5.3 REST PATCH title/detail → 200, applied
R=$(tenant_call PATCH "/workspaces/$WS_A/user-tasks/$TASK_ID" -H "Content-Type: application/json" \
  -d '{"title":"Review the Q3 draft (URGENT)","detail":"Sign-off needed by EOD"}' \
  -o /dev/null -w "%{http_code}" 2>/dev/null || echo "000")
check_code "REST PATCH user-task" "200" "$R"
R=$(tenant_call GET "/workspaces/$WS_A/user-tasks")
check "PATCH applied new title" '(URGENT)' "$R"
check "PATCH applied new detail" 'Sign-off needed by EOD' "$R"

# 5.4 REST resolve done → 200, gone from pending
R=$(tenant_call POST "/workspaces/$WS_A/user-tasks/$TASK_ID/resolve" -H "Content-Type: application/json" \
  -d '{"status":"done","resolved_by":"cto"}' -o "$TMPDIR_E2E/r.json" -w "%{http_code}" 2>/dev/null || echo "000")
BODY=$(cat "$TMPDIR_E2E/r.json" 2>/dev/null || echo "")
check_code "REST resolve done" "200" "$R"
check "resolve echoes status done" '"status":"done"' "$BODY"
R=$(tenant_call GET "/user-tasks/pending")
check_not "resolved task no longer pending (admin feed)" "$TASK_ID" "$R"

# 5.5 MCP request_user_action → new pending task surfaces on the admin feed
TEXT=$(mcp_call "$WS_A" "request_user_action" '{"title":"Provide the staging API key","detail":"Blocked on it for the deploy"}')
check_code "MCP request_user_action HTTP" "200" "$(mcp_http_code)"
check "MCP request_user_action success text" 'Asked the user' "$TEXT"
R=$(tenant_call GET "/user-tasks/pending")
check "MCP-created ask appears in pending feed" 'Provide the staging API key' "$R"
MCP_TASK_ID=$(echo "$R" | python3 -c "
import sys, json
for t in json.load(sys.stdin):
    if t.get('title') == 'Provide the staging API key':
        print(t.get('id','')); break" 2>/dev/null || echo "")
log "    MCP_TASK_ID=$MCP_TASK_ID"

# 5.6 MCP list_user_tasks returns ws-A's task(s)
TEXT=$(mcp_call "$WS_A" "list_user_tasks" '{}')
check_code "MCP list_user_tasks HTTP" "200" "$(mcp_http_code)"
check "list_user_tasks contains the MCP task" 'Provide the staging API key' "$TEXT"
check "list_user_tasks shows it pending" '"status":"pending"' "$TEXT"

# 5.7 MCP update_user_task changes it
if [ -n "$MCP_TASK_ID" ]; then
  TEXT=$(mcp_call "$WS_A" "update_user_task" "{\"user_task_id\":\"$MCP_TASK_ID\",\"title\":\"Provide the PROD API key\"}")
  check_code "MCP update_user_task HTTP" "200" "$(mcp_http_code)"
  check "MCP update_user_task success text" 'User task updated' "$TEXT"
  TEXT=$(mcp_call "$WS_A" "list_user_tasks" '{}')
  check "update applied (new title)" 'Provide the PROD API key' "$TEXT"
  check_not "update applied (old title gone)" 'staging API key' "$TEXT"

  # 5.8 MCP delete_user_task → gone from list
  TEXT=$(mcp_call "$WS_A" "delete_user_task" "{\"user_task_id\":\"$MCP_TASK_ID\"}")
  check_code "MCP delete_user_task HTTP" "200" "$(mcp_http_code)"
  check "MCP delete_user_task success text" 'User task deleted' "$TEXT"
  TEXT=$(mcp_call "$WS_A" "list_user_tasks" '{}')
  check_not "deleted task gone from list" 'Provide the PROD API key' "$TEXT"
else
  echo "  FAIL: could not resolve MCP_TASK_ID — MCP update/delete steps skipped"
  FAIL=$((FAIL + 1))
fi

# 5.9 Cross-workspace authz: ws-B cannot mutate ws-A's task (scoped by URL :id)
SCOPE_ID=$(tenant_call POST "/workspaces/$WS_A/user-tasks" -H "Content-Type: application/json" \
  -d '{"title":"Scope probe task"}' | python3 -c "import sys,json; print(json.load(sys.stdin).get('user_task_id',''))" 2>/dev/null || echo "")
[ -n "$SCOPE_ID" ] || fail "scope-probe task create failed"
log "    SCOPE_ID=$SCOPE_ID (owned by ws-A)"
# ws-B PATCHes ws-A's task → 404 (workspace_id scope).
R=$(tenant_call PATCH "/workspaces/$WS_B/user-tasks/$SCOPE_ID" -H "Content-Type: application/json" \
  -d '{"title":"hijack"}' -o /dev/null -w "%{http_code}" 2>/dev/null || echo "000")
check_code "ws-B PATCH of ws-A's task scoped out" "404" "$R"
# ws-B DELETEs ws-A's task → 404.
R=$(tenant_call DELETE "/workspaces/$WS_B/user-tasks/$SCOPE_ID" -o /dev/null -w "%{http_code}" 2>/dev/null || echo "000")
check_code "ws-B DELETE of ws-A's task scoped out" "404" "$R"
# Task survived unchanged on ws-A.
R=$(tenant_call GET "/workspaces/$WS_A/user-tasks")
check "ws-A's task survived cross-ws attempts" "$SCOPE_ID" "$R"
check_not "ws-A's task title was NOT hijacked" 'hijack' "$R"
# ws-B's own list must NOT see ws-A's task at all.
R=$(tenant_call GET "/workspaces/$WS_B/user-tasks")
check_not "ws-B list excludes ws-A's task (read isolation)" "$SCOPE_ID" "$R"

# 5.10 Validation contracts
R=$(tenant_call POST "/workspaces/$WS_A/user-tasks" -H "Content-Type: application/json" \
  -d '{"detail":"no title here"}' -o /dev/null -w "%{http_code}" 2>/dev/null || echo "000")
check_code "create without title → 400" "400" "$R"
R=$(tenant_call POST "/workspaces/$WS_A/user-tasks/$SCOPE_ID/resolve" -H "Content-Type: application/json" \
  -d '{"status":"banana"}' -o /dev/null -w "%{http_code}" 2>/dev/null || echo "000")
check_code "resolve with invalid status → 400" "400" "$R"
R=$(tenant_call PATCH "/workspaces/$WS_A/user-tasks/$SCOPE_ID" -H "Content-Type: application/json" \
  -d '{"status":"banana"}' -o /dev/null -w "%{http_code}" 2>/dev/null || echo "000")
check_code "PATCH with invalid status → 400" "400" "$R"

# ─── 6. Results ──────────────────────────────────────────────────────────────
log "6/6 Results: $PASS passed, $FAIL failed (teardown runs via EXIT trap)"
[ "$FAIL" -eq 0 ] || fail "$FAIL user_tasks assertion(s) failed"
ok "═══ STAGING CONCIERGE user_tasks E2E PASSED ($PASS checks) ═══"
