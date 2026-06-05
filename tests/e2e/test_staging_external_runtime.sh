#!/bin/bash
# test_staging_external_runtime.sh — E2E regression for the
# external-runtime workspace lifecycle on a real staging tenant.
#
# Why this test exists: the four/five sites that write 'awaiting_agent'
# / 'hibernating' to workspaces.status had been silently failing in
# production for five days (see migration 046) before a static drift
# gate caught the enum gap. Unit tests passed because sqlmock matched
# the SQL by regex but didn't enforce the live enum constraint, and
# every existing E2E exercised hermes (not external) so the silent
# failures never surfaced. This test pins the four awaiting_agent
# transitions in real Postgres on a real staging tenant.
#
# Verification path:
#   1. Provision a fresh tenant (test_staging_full_saas.sh harness shape).
#   2. Create an external-runtime workspace with NO URL → assert
#      response status == 'awaiting_agent' AND GET on the workspace
#      returns the same. (Pre-fix the row stuck on 'provisioning'
#      because the UPDATE in workspace.go:333 silently failed.)
#   3. Register a fake URL via /registry/register → assert transition
#      to 'online'. (Pre-fix this branch worked because it writes
#      'online' which IS in the enum.)
#   4. Stop heartbeating; wait past REMOTE_LIVENESS_STALE_AFTER (90s
#      default) + a sweep interval → assert transition back to
#      'awaiting_agent'. (Pre-fix the sweep UPDATE failed silently and
#      the workspace stuck on 'online' indefinitely.)
#
# Hibernation is intentionally NOT covered here — it has its own timing
# model (idle threshold) and warrants a separate harness.
#
# Required env (mirrors test_staging_full_saas.sh):
#   MOLECULE_CP_URL          default: https://staging-api.moleculesai.app
#   MOLECULE_ADMIN_TOKEN     CP admin bearer (Railway CP_ADMIN_API_TOKEN)
#
# Optional env:
#   E2E_PROVISION_TIMEOUT_SECS  default 900 (15 min cold EC2 budget)
#   E2E_KEEP_ORG                1 → skip teardown (debugging only)
#   E2E_RUN_ID                  Slug suffix; CI: ${GITHUB_RUN_ID}
#   E2E_STALE_WAIT_SECS         default 180 (90s window + 90s buffer)
#   E2E_INTENTIONAL_FAILURE     1 → break a step on purpose to verify
#                               the EXIT trap still tears down (mirrors
#                               the full-saas harness's safety net).
#   E2E_REQUIRE_LIVE            1 → fail-closed if the harness exits 0
#                               WITHOUT having driven all four
#                               awaiting_agent transitions. CI sets this
#                               so a future skip / early-return can never
#                               masquerade as a green run. Mirrors CP
#                               serving-e2e SERVING_E2E_REQUIRE_LIVE.
#   E2E_STALE_POLL_DEADLINE_SECS  default 240. Upper bound for the
#                               heartbeat-staleness READINESS poll (step
#                               6). Replaces the old fixed sleep+one-shot
#                               assert that raced the sweep cadence.
#   E2E_TRANSIENT_RETRIES      default 8. Bounded retries for register /
#                               re-register against transient edge errors
#                               (502/503/504 from Caddy during cold TLS /
#                               agent boot). Mirrors the full-saas
#                               cold-start retry loop — NOT a bare sleep.
#
# Exit codes: 0 happy, 1 generic, 2 missing env, 3 provision timeout,
# 4 teardown leak, 5 REQUIRE_LIVE violation (exited 0 having validated
# nothing).

set -euo pipefail

CP_URL="${MOLECULE_CP_URL:-https://staging-api.moleculesai.app}"
ADMIN_TOKEN="${MOLECULE_ADMIN_TOKEN:?MOLECULE_ADMIN_TOKEN required — Railway staging CP_ADMIN_API_TOKEN}"
PROVISION_TIMEOUT_SECS="${E2E_PROVISION_TIMEOUT_SECS:-900}"
RUN_ID_SUFFIX="${E2E_RUN_ID:-$(date +%H%M%S)-$$}"
STALE_WAIT_SECS="${E2E_STALE_WAIT_SECS:-180}"
# Readiness-poll deadline for the sweep transition (step 6). Must exceed
# STALE_WAIT_SECS (the no-heartbeat window) by at least one sweep
# interval so a slightly-late sweep tick is polled-for, not misread as a
# stuck 'online'. 240 = 180s window + 60s sweep-cadence headroom.
STALE_POLL_DEADLINE_SECS="${E2E_STALE_POLL_DEADLINE_SECS:-240}"
TRANSIENT_RETRIES="${E2E_TRANSIENT_RETRIES:-8}"
REQUIRE_LIVE="${E2E_REQUIRE_LIVE:-0}"

SLUG="e2e-ext-$(date +%Y%m%d)-${RUN_ID_SUFFIX}"
SLUG=$(echo "$SLUG" | tr '[:upper:]' '[:lower:]' | tr -cd 'a-z0-9-' | head -c 32)

log()  { echo "[$(date +%H:%M:%S)] $*"; }
fail() { echo "[$(date +%H:%M:%S)] ❌ $*" >&2; exit 1; }
ok()   { echo "[$(date +%H:%M:%S)] ✅ $*"; }

# REQUIRE_LIVE bookkeeping: count the four awaiting_agent transitions the
# test is contracted to prove. The EXIT trap fails-closed (exit 5) if the
# script reaches a clean exit without all four — so a silent skip, an
# early `return 0`, or a refactor that drops a step can never show green.
TRANSITIONS_VERIFIED=0
EXPECTED_TRANSITIONS=4
require_transition() {  # $1 = human label
  TRANSITIONS_VERIFIED=$((TRANSITIONS_VERIFIED + 1))
  log "    [require-live] transition ${TRANSITIONS_VERIFIED}/${EXPECTED_TRANSITIONS} proven: $1"
}

# Redact bearer tokens from any HTTP body before logging (mirrors the
# full-saas sanitize_http_body so transient-error logs never leak creds).
sanitize_http_body() {
  sed -E 's/(Bearer|token)[[:space:]]+[A-Za-z0-9._-]+/\1 REDACTED/g'
}

# Bounded retry-on-transient for POST /registry/register. The tenant edge
# (Caddy) returns 502/503/504 with an identifiable body while TLS / the
# workspace agent finishes cold-booting — a single shot here was the
# un-named flake (a transient edge error misread as a register failure).
# This mirrors the full-saas cold-start loop (test_staging_full_saas.sh
# ~L780-816): retry ONLY on a transient TRANSPORT class (5xx + body
# match), bounded by TRANSIENT_RETRIES, and FAIL CLOSED (non-zero) once
# the budget is spent. It deliberately does NOT retry on a 4xx — that's a
# real contract bug (e.g. wrong payload field) and must stay red.
# Sets REGISTER_RESP (body + trailing "HTTP_CODE=NNN" line) on success;
# returns non-zero (caller `fail`s) when the bounded budget is exhausted.
register_with_retry() {  # $1 = step label, $2 = request body
  local label="$1" body="$2"
  local attempt code resp safe
  for attempt in $(seq 1 "$TRANSIENT_RETRIES"); do
    set +e
    resp=$(curl -sS --max-time 30 -w "\nHTTP_CODE=%{http_code}" -X POST \
      "$TENANT_URL/registry/register" \
      -H "Authorization: Bearer $WS_AUTH_TOKEN" \
      -H "X-Molecule-Org-Id: $ORG_ID" \
      -H "Content-Type: application/json" \
      -d "$body")
    set -e
    code=$(printf '%s' "$resp" | sed -n 's/^HTTP_CODE=//p' | tail -n1)
    code=${code:-000}
    if [ "$code" = "200" ]; then
      REGISTER_RESP="$resp"
      return 0
    fi
    safe=$(printf '%s' "$resp" | sanitize_http_body | head -c 300)
    # Retry ONLY on a transient transport class; a 4xx is a real bug.
    if echo "$code" | grep -Eq '^(502|503|504)$' \
       && echo "$safe" | grep -Eqi 'Service Unavailable|Bad Gateway|Gateway Timeout|error code: 502|error code: 504|workspace agent unreachable|connection refused|no healthy upstream'; then
      log "    ${label} transient $code attempt ${attempt}/${TRANSIENT_RETRIES}: $safe"
      [ "$attempt" -lt "$TRANSIENT_RETRIES" ] && { sleep 10; continue; }
    fi
    # Non-transient (4xx, or unrecognized 5xx body): stop and fail closed.
    REGISTER_RESP="$resp"
    return 1
  done
  return 1
}

CURL_COMMON=(-sS --fail-with-body --max-time 30)

# ─── cleanup trap (mirrors full-saas) ────────────────────────────────────
CLEANUP_DONE=0
cleanup_org() {
  local entry_rc=$?
  if [ "$CLEANUP_DONE" = "1" ]; then return 0; fi
  CLEANUP_DONE=1

  if [ "${E2E_KEEP_ORG:-0}" = "1" ]; then
    log "E2E_KEEP_ORG=1 → leaving $SLUG behind for inspection"
    return 0
  fi

  log "Cleanup: deleting tenant $SLUG..."
  curl "${CURL_COMMON[@]}" --max-time 120 -X DELETE "$CP_URL/cp/admin/tenants/$SLUG" \
    -H "Authorization: Bearer $ADMIN_TOKEN" \
    -H "Content-Type: application/json" \
    -d "{\"confirm\":\"$SLUG\"}" >/dev/null 2>&1 \
    && ok "Teardown request accepted" \
    || log "Teardown returned non-2xx (may already be gone)"

  local leak_count=1 elapsed=0
  while [ "$elapsed" -lt 60 ]; do
    leak_count=$(curl "${CURL_COMMON[@]}" "$CP_URL/cp/admin/orgs" \
      -H "Authorization: Bearer $ADMIN_TOKEN" 2>/dev/null \
      | python3 -c "import json,sys; d=json.load(sys.stdin); print(sum(1 for o in d.get('orgs', []) if o.get('slug')=='$SLUG' and o.get('status') != 'purged'))" \
      2>/dev/null || echo 1)
    [ "$leak_count" = "0" ] && break
    sleep 5
    elapsed=$((elapsed + 5))
  done

  if [ "$leak_count" != "0" ]; then
    echo "⚠️  LEAK: org $SLUG still present post-teardown (count=$leak_count)" >&2
    exit 4
  fi
  ok "Teardown clean — no orphan resources for $SLUG (${elapsed}s)"

  # REQUIRE_LIVE fail-closed gate. Only meaningful on an OTHERWISE-CLEAN
  # exit (entry_rc==0): a script that completed all steps but somehow did
  # not register all four transitions (a skip, an early return, a dropped
  # assertion in a refactor) must NOT report success. A non-zero entry_rc
  # already carries its own failure semantics — don't mask it with 5.
  if [ "$entry_rc" = "0" ] && [ "${REQUIRE_LIVE}" = "1" ] \
     && [ "$TRANSITIONS_VERIFIED" -lt "$EXPECTED_TRANSITIONS" ]; then
    echo "❌ REQUIRE_LIVE: exited 0 but only ${TRANSITIONS_VERIFIED}/${EXPECTED_TRANSITIONS} awaiting_agent transitions were proven — refusing to report green." >&2
    exit 5
  fi

  case "$entry_rc" in
    0|1|2|3|4|5) ;;
    *) exit 1 ;;
  esac
}
trap cleanup_org EXIT INT TERM

# ─── 0. Preflight ───────────────────────────────────────────────────────
log "═══════════════════════════════════════════════════════════════════"
log " Staging external-runtime E2E (regression for migration 046)"
log "   CP:    $CP_URL"
log "   Slug:  $SLUG"
log "   Stale: ${STALE_WAIT_SECS}s wait window"
log "═══════════════════════════════════════════════════════════════════"

curl "${CURL_COMMON[@]}" "$CP_URL/health" >/dev/null || fail "CP health check failed"
ok "CP reachable"

admin_call() {
  local method="$1"; shift; local path="$1"; shift
  curl "${CURL_COMMON[@]}" -X "$method" "$CP_URL$path" \
    -H "Authorization: Bearer $ADMIN_TOKEN" \
    -H "Content-Type: application/json" "$@"
}

# ─── 1. Create org ──────────────────────────────────────────────────────
log "1/8 Creating org $SLUG..."
CREATE_RESP=$(admin_call POST /cp/admin/orgs \
  -d "{\"slug\":\"$SLUG\",\"name\":\"E2E ext $SLUG\",\"owner_user_id\":\"e2e-runner:$SLUG\"}")
ORG_ID=$(echo "$CREATE_RESP" | python3 -c "import json,sys; print(json.load(sys.stdin).get('id',''))")
[ -z "$ORG_ID" ] && fail "Org create response missing 'id'"
ok "Org created (id=$ORG_ID)"

# ─── 2. Wait for tenant provisioning ────────────────────────────────────
# Terminal status from /cp/admin/orgs is 'running' (org_instances.status),
# NOT 'ready' — same field the full-saas harness polls. 'failed' surfaces
# diagnostic dump and aborts. See test_staging_full_saas.sh step 2 for
# the field-bugfix history (2026-04-21, last_error path).
log "2/8 Waiting for tenant (up to ${PROVISION_TIMEOUT_SECS}s)..."
DEADLINE=$(( $(date +%s) + PROVISION_TIMEOUT_SECS ))
LAST_STATUS=""
while true; do
  if [ "$(date +%s)" -gt "$DEADLINE" ]; then
    fail "Tenant provisioning timed out (last: $LAST_STATUS)"
  fi
  LIST_JSON=$(admin_call GET /cp/admin/orgs 2>/dev/null || echo '{"orgs":[]}')
  STATUS=$(echo "$LIST_JSON" | python3 -c "
import json, sys
d = json.load(sys.stdin)
for o in d.get('orgs', []):
    if o.get('slug') == '$SLUG':
        print(o.get('instance_status', ''))
        sys.exit(0)
print('')
" 2>/dev/null || echo "")
  if [ "$STATUS" != "$LAST_STATUS" ]; then
    log "   instance_status: $STATUS"
    LAST_STATUS="$STATUS"
  fi
  case "$STATUS" in
    running) break ;;
    failed)
      log "── DIAGNOSTIC BURST (step 2 — tenant provisioning failed) ──"
      echo "$LIST_JSON" | python3 -c "
import json, sys
d = json.load(sys.stdin)
for o in d.get('orgs', []):
    if o.get('slug') == '$SLUG':
        print(json.dumps(o, indent=2))
        sys.exit(0)
print('(no org row found for slug=$SLUG — DB drift?)')
" 2>&1 | sed 's/^/  /'
      log "── END DIAGNOSTIC ──"
      fail "Tenant provisioning failed for $SLUG (see diagnostic above)"
      ;;
    *) sleep 15 ;;
  esac
done
ok "Tenant provisioning complete"

# Derive tenant URL the same way the full-saas harness does.
CP_HOST=$(echo "$CP_URL" | sed -E 's#^https?://##; s#/.*$##')
case "$CP_HOST" in
  api.*)         DERIVED_DOMAIN="${CP_HOST#api.}" ;;
  staging-api.*) DERIVED_DOMAIN="staging.${CP_HOST#staging-api.}" ;;
  *)             DERIVED_DOMAIN="$CP_HOST" ;;
esac
TENANT_DOMAIN="${MOLECULE_TENANT_DOMAIN:-$DERIVED_DOMAIN}"
TENANT_URL="https://$SLUG.$TENANT_DOMAIN"
log "    TENANT_URL=$TENANT_URL"

# ─── 3. Per-tenant admin token + TLS readiness ──────────────────────────
log "3/8 Fetching per-tenant admin token..."
TENANT_TOKEN_RESP=$(admin_call GET "/cp/admin/orgs/$SLUG/admin-token")
TENANT_TOKEN=$(echo "$TENANT_TOKEN_RESP" | python3 -c "import json,sys; print(json.load(sys.stdin).get('admin_token',''))")
[ -z "$TENANT_TOKEN" ] && fail "Could not retrieve per-tenant admin token"
ok "Token retrieved (len=${#TENANT_TOKEN})"

log "Waiting for tenant TLS / DNS..."
TLS_DEADLINE=$(( $(date +%s) + 15 * 60 ))
while true; do
  if curl -sSfk --max-time 5 "$TENANT_URL/health" >/dev/null 2>&1; then break; fi
  if [ "$(date +%s)" -gt "$TLS_DEADLINE" ]; then
    fail "Tenant URL never responded 2xx on /health within 15min"
  fi
  sleep 5
done
ok "Tenant reachable"

tenant_call() {
  local method="$1"; shift; local path="$1"; shift
  curl "${CURL_COMMON[@]}" -X "$method" "$TENANT_URL$path" \
    -H "Authorization: Bearer $TENANT_TOKEN" \
    -H "X-Molecule-Org-Id: $ORG_ID" \
    "$@"
}

# ─── 4. Create external workspace (no URL) ──────────────────────────────
# This is the FIRST silent-failure path (workspace.go:333). Pre-migration
# 046, the response would say status=awaiting_agent but the row stuck
# on whatever the create handler set first (typically 'provisioning')
# because the follow-up UPDATE failed the enum cast.
log "4/8 Creating external workspace (no URL — exercises workspace.go:333)..."
WS_CREATE_RESP=$(tenant_call POST /workspaces \
  -d '{"name":"ext-e2e","runtime":"external","external":true}')

WS_ID=$(echo "$WS_CREATE_RESP" | python3 -c "import json,sys; print(json.load(sys.stdin).get('id',''))")
WS_RESP_STATUS=$(echo "$WS_CREATE_RESP" | python3 -c "import json,sys; print(json.load(sys.stdin).get('status',''))")
WS_AUTH_TOKEN=$(echo "$WS_CREATE_RESP" | python3 -c "
import json,sys
try:
    d = json.load(sys.stdin)
    conn = d.get('connection') or {}
    print(conn.get('auth_token','') or d.get('auth_token',''))
except Exception:
    print('')
")
[ -z "$WS_ID" ] && fail "Workspace create missing id: $WS_CREATE_RESP"
[ "$WS_RESP_STATUS" != "awaiting_agent" ] && fail "Expected response status=awaiting_agent, got $WS_RESP_STATUS"
ok "Workspace created (id=$WS_ID, response status=awaiting_agent)"

# This GET is the proof that the row actually has the value (not just
# the response body lying). Pre-migration-046 the UPDATE would have
# silently failed and this would return whatever 'provisioning' the
# initial INSERT left. Post-fix it must be 'awaiting_agent'.
log "    Verifying DB row..."
GET_RESP=$(tenant_call GET "/workspaces/$WS_ID")
DB_STATUS=$(echo "$GET_RESP" | python3 -c "import json,sys; print(json.load(sys.stdin).get('status',''))")
[ "$DB_STATUS" != "awaiting_agent" ] && fail "DB row status=$DB_STATUS (expected awaiting_agent — migration 046 likely not applied)"
ok "DB row stored as awaiting_agent (proof migration 046 applied)"
require_transition "create: provisioning → awaiting_agent (DB-verified)"

# ─── 5. Register the workspace (transitions to online) ──────────────────
# Pre-fix this path was actually fine because it writes 'online', a value
# already in the enum. We exercise it anyway because the registration
# implicitly walks resolveDeliveryMode (registry.go:resolveDeliveryMode),
# which DOES read runtime + apply the new poll-default introduced by
# PR #2382.
log "5/8 Registering workspace via /registry/register..."
[ -z "$WS_AUTH_TOKEN" ] && fail "No workspace auth token returned — register impossible"
# Payload contract (workspace-server/internal/models/workspace.go RegisterPayload):
#   id            — required, the workspace UUID (NOT "workspace_id" — that's the
#                   heartbeat payload field; mixing them yields a 400 from
#                   ShouldBindJSON because `id` has binding:"required").
#   agent_card    — required (binding:"required"); minimal valid card is name+skills.
#   delivery_mode — set explicitly to "poll" so url validation is skipped
#                   regardless of whether the deployed image has the
#                   runtime=external→poll default from PR #2382. Observed
#                   2026-04-30 17:18Z: a freshly-provisioned staging tenant
#                   was running an older workspace-server :latest image
#                   that lacked resolveDeliveryMode's external→poll branch,
#                   so the implicit default was push and validateAgentURL
#                   400'd on example.invalid. Asserting on the implicit
#                   default makes the *register call* itself fragile to
#                   image-tag drift on the fleet — verify the default
#                   separately (step 5b assertion) without depending on it
#                   here.
#   url           — accepted but not dispatched-to in poll mode, so
#                   example.invalid is a valid sentinel.
REGISTER_BODY=$(printf '{"id":"%s","url":"https://example.invalid:443","delivery_mode":"poll","agent_card":{"name":"e2e-ext","skills":[{"id":"echo","name":"Echo"}]}}' "$WS_ID")
# Bounded retry-on-transient (see register_with_retry). The previous
# single-shot here would `fail` on a cold-boot 502 from the tenant edge —
# an un-named transient misread as a register break. The helper retries
# ONLY that class and fails closed on a real 4xx or an exhausted budget.
REGISTER_RESP=""
register_with_retry "register" "$REGISTER_BODY" \
  || fail "register returned non-200 after bounded retries — body: $(printf '%s' "$REGISTER_RESP" | sanitize_http_body | head -c 300)"
log "    register response: $(echo "$REGISTER_RESP" | sanitize_http_body | head -c 300)"

GET_RESP=$(tenant_call GET "/workspaces/$WS_ID")
ONLINE_STATUS=$(echo "$GET_RESP" | python3 -c "import json,sys; print(json.load(sys.stdin).get('status',''))")
[ "$ONLINE_STATUS" != "online" ] && fail "Expected online after register, got $ONLINE_STATUS"
ok "Workspace transitioned to online"
require_transition "register: awaiting_agent → online"

# Confirm the register handler echoed back delivery_mode=poll. We read
# this from the register RESPONSE, not the workspace GET response, because
# the GET handler's SELECT (workspace.go:597) doesn't fetch delivery_mode
# — its column list pre-dates the delivery_mode column from #2339 PR 1.
# Surfacing delivery_mode in GET is tracked separately; not gating on it
# here keeps this test focused on the awaiting_agent transitions.
REGISTER_BODY_JSON=$(echo "$REGISTER_RESP" | head -n 1)
REGISTER_DELIVERY_MODE=$(echo "$REGISTER_BODY_JSON" | python3 -c "import json,sys; print(json.load(sys.stdin).get('delivery_mode',''))")
if [ "$REGISTER_DELIVERY_MODE" = "poll" ]; then
  ok "delivery_mode=poll (register response echoed explicit value)"
else
  fail "Register response delivery_mode=$REGISTER_DELIVERY_MODE (expected poll). Body: $REGISTER_BODY_JSON"
fi

# ─── 6. Stop heartbeating; wait past REMOTE_LIVENESS_STALE_AFTER ────────
# This is the SECOND silent-failure path (registry/healthsweep.go's
# sweepStaleRemoteWorkspaces). Pre-migration-046 the heartbeat-staleness
# UPDATE silently failed and the workspace stuck on 'online' forever
# even though no agent was alive.
#
# FLAKE FIX (named: sweep-cadence race). The old code did a FIXED
# `sleep $STALE_WAIT_SECS` then a SINGLE assert. The staleness sweep is a
# periodic tick (REMOTE_LIVENESS_STALE_AFTER + a sweep interval); if the
# tick that flips the row lands even one second after the fixed sleep, the
# one-shot GET reads 'online' and the test fails — a real transition,
# misread as a flake because the assert was racing the sweep cadence.
# Replace with: sleep through the mandatory no-heartbeat window ONCE (the
# sweep cannot fire before the window elapses, so polling earlier is
# pointless), then READINESS-POLL for the awaiting_agent transition up to
# STALE_POLL_DEADLINE_SECS, hard-failing with a clear message at the
# deadline. Deterministic: a slow-but-working sweep passes; a genuinely
# stuck 'online' still fails (now with how long we actually waited).
log "6/8 Waiting ${STALE_WAIT_SECS}s no-heartbeat window, then polling for sweep (up to ${STALE_POLL_DEADLINE_SECS}s total)..."
[ "$STALE_POLL_DEADLINE_SECS" -le "$STALE_WAIT_SECS" ] && \
  fail "Misconfigured: STALE_POLL_DEADLINE_SECS ($STALE_POLL_DEADLINE_SECS) must exceed STALE_WAIT_SECS ($STALE_WAIT_SECS) by at least one sweep interval"
sleep "$STALE_WAIT_SECS"

STALE_DEADLINE=$(( $(date +%s) + (STALE_POLL_DEADLINE_SECS - STALE_WAIT_SECS) ))
STALE_STATUS=""
while true; do
  GET_RESP=$(tenant_call GET "/workspaces/$WS_ID")
  STALE_STATUS=$(echo "$GET_RESP" | python3 -c "import json,sys; print(json.load(sys.stdin).get('status',''))")
  [ "$STALE_STATUS" = "awaiting_agent" ] && break
  if [ "$(date +%s)" -gt "$STALE_DEADLINE" ]; then
    fail "After ${STALE_POLL_DEADLINE_SECS}s with no heartbeat, status still '$STALE_STATUS' (expected awaiting_agent sweep transition) — migration 046 likely not applied OR sweep not running"
  fi
  sleep 10
done
ok "Heartbeat-staleness sweep transitioned online → awaiting_agent (proof healthsweep.go fix working)"
require_transition "sweep: online → awaiting_agent (no heartbeat)"

# ─── 7. Re-register and confirm we can come back online ─────────────────
# This proves the awaiting_agent state is recoverable (re-registrable),
# which is the whole point of using it instead of 'offline'.
log "7/8 Re-registering after stale → confirming recovery to online..."
# Same payload contract as step 5 (id + agent_card both required). See note
# there for why workspace_id would 400. Same bounded retry-on-transient.
REGISTER_RESP=""
register_with_retry "re-register" "$REGISTER_BODY" \
  || fail "re-register returned non-200 after bounded retries — body: $(printf '%s' "$REGISTER_RESP" | sanitize_http_body | head -c 300)"
log "    re-register response: $(echo "$REGISTER_RESP" | sanitize_http_body | head -c 300)"

GET_RESP=$(tenant_call GET "/workspaces/$WS_ID")
RECOVERED_STATUS=$(echo "$GET_RESP" | python3 -c "import json,sys; print(json.load(sys.stdin).get('status',''))")
[ "$RECOVERED_STATUS" != "online" ] && \
  fail "Expected re-register to return workspace to online, got $RECOVERED_STATUS"
ok "Re-register succeeded — awaiting_agent → online (operator-recoverable)"
require_transition "re-register: awaiting_agent → online (recovery)"

# ─── 8. Done — cleanup runs in the EXIT trap ───────────────────────────
# REQUIRE_LIVE belt-and-braces: assert here too (in addition to the EXIT
# trap) so the failure surfaces in step order, not only post-teardown.
if [ "${REQUIRE_LIVE}" = "1" ] && [ "$TRANSITIONS_VERIFIED" -lt "$EXPECTED_TRANSITIONS" ]; then
  fail "REQUIRE_LIVE: only ${TRANSITIONS_VERIFIED}/${EXPECTED_TRANSITIONS} transitions proven at end of run"
fi
log "8/8 All four awaiting_agent transitions verified."
log "═══════════════════════════════════════════════════════════════════"
ok "External-runtime E2E PASSED on $SLUG"
log "═══════════════════════════════════════════════════════════════════"
