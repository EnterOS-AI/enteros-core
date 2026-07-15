#!/usr/bin/env bash
# Minimal staging boot-to-registration cell.
#
# Four load-bearing assertions:
#   1. The current strict admin API accepts a fresh molecules-server org and the
#      tenant reaches instance_status=running.
#   2. The auto-installed platform root appears online with a routable URL and a
#      heartbeat through the tenant workspace API. That is the externally
#      observable result of the runtime's register + heartbeat path.
#   3. A real A2A message/send turn returns the deterministic token PINEAPPLE;
#      text-shaped runtime errors do not count as completion.
#   4. The EXIT trap confirms the admin purge and verifies the org is absent.
#
# Org creation intentionally sends only the current control-plane contract:
# slug, name, owner_user_id, and provider. Runtime is fetched by the workflow
# from Infisical and verified against GET /workspaces/:id. Model and billing are
# intentionally not sent or claimed: they are not valid org-create fields, and
# this API does not expose an authoritative identity for them. The A2A assertion
# proves the deployed default LLM route completes.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib/collision-proof-slug.sh
# shellcheck disable=SC1091
source "$SCRIPT_DIR/lib/collision-proof-slug.sh"
# shellcheck source=lib/completion_assert.sh
# shellcheck disable=SC1091
source "$SCRIPT_DIR/lib/completion_assert.sh"

CP_URL="${MOLECULE_CP_URL:-https://staging-api.moleculesai.app}"
ADMIN_TOKEN="${MOLECULE_ADMIN_TOKEN:?MOLECULE_ADMIN_TOKEN required — load staging CP_ADMIN_API_TOKEN from Infisical /shared/controlplane-admin}"
RUNTIME="${E2E_RUNTIME:-claude-code}"
PROVIDER="${E2E_PROVIDER:-molecules-server}"
PROVISION_TIMEOUT_SECS="${E2E_PROVISION_TIMEOUT_SECS:-300}"
TENANT_READY_TIMEOUT_SECS="${E2E_TENANT_READY_TIMEOUT_SECS:-300}"
REGISTER_TIMEOUT_SECS="${E2E_REGISTER_TIMEOUT_SECS:-180}"
A2A_TIMEOUT_SECS="${E2E_A2A_TIMEOUT_SECS:-120}"
KEEP_ORG="${E2E_KEEP_ORG:-}"

CURL_COMMON=(-sS -A curl/8.4.0 --max-time 30)

FAIL_CODE=1
EXIT_CODE=0
log()  { echo "[$(date +%H:%M:%S)] $*"; }
fail() {
  local code="${FAIL_CODE:-1}"
  EXIT_CODE="$code"
  echo "[$(date +%H:%M:%S)] ❌ $*" >&2
  exit "$code"
}
ok()   { echo "[$(date +%H:%M:%S)] ✅ $*"; }

# Preserve the historical cp455 identifier while retaining the mandatory
# collision-proof UUID suffix under the control-plane's 32-character slug cap.
SLUG_PREFIX="cp455-${RUNTIME}-"
SLUG="${SLUG_PREFIX}$(make_collision_proof_slug_suffix "${E2E_RUN_ID:-}" ${#SLUG_PREFIX})"
assert_collision_proof_slug "$SLUG" || fail "collision-proof helper produced invalid slug '$SLUG'"

WORKSPACE_ID=""
ORG_ID=""
TENANT_TOKEN=""
TENANT_URL=""
RESULT_JSON="/tmp/cell-result.json"
PROVISION_BODY="/tmp/cell-provision-${SLUG}.json"
TENANT_HEALTH_BODY="/tmp/cell-tenant-health-${SLUG}.txt"
WORKSPACE_LIST_BODY="/tmp/cell-workspaces-${SLUG}.json"
WORKSPACE_DETAIL_BODY="/tmp/cell-workspace-detail-${SLUG}.json"
A2A_BODY="/tmp/cell-a2a-${SLUG}.json"
A2A_QUEUE_BODY="/tmp/cell-a2a-queue-${SLUG}.json"
PROVISION_START_EPOCH="$(date +%s)"
REGISTER_STATUS="not_attempted"
COMPLETION_STATUS="not_attempted"
TEARDOWN_STATUS="not_attempted"

write_result() {
  local elapsed="${1:-0}"
  python3 - "$RESULT_JSON" "$RUNTIME" "$PROVIDER" \
    "$WORKSPACE_ID" "$REGISTER_STATUS" "$COMPLETION_STATUS" \
    "$TEARDOWN_STATUS" "$elapsed" "$EXIT_CODE" <<'PY'
import json, sys
from datetime import datetime, timezone

path, runtime, provider, workspace, registered, completion, teardown, elapsed, code = sys.argv[1:]
with open(path, "w", encoding="utf-8") as fh:
    json.dump({
        "runtime": runtime,
        "provider": provider,
        "workspace_id": workspace,
        "register_status": registered,
        "completion_status": completion,
        "teardown_status": teardown,
        "elapsed_seconds": int(elapsed),
        "exit_code": int(code),
        "ts": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
    }, fh, indent=2)
    fh.write("\n")
PY
}

org_present_count() {
  curl "${CURL_COMMON[@]}" "$CP_URL/cp/admin/orgs?limit=500" \
    -H "Authorization: Bearer ${ADMIN_TOKEN}" 2>/dev/null \
    | SLUG="$SLUG" python3 -c '
import json, os, sys
try:
    rows = json.load(sys.stdin).get("orgs", [])
except Exception:
    print(1); raise SystemExit
slug = os.environ["SLUG"]
print(sum(1 for row in rows if row.get("slug") == slug and row.get("status") != "purged" and row.get("instance_status") != "purged"))
' 2>/dev/null || echo 1
}

on_exit() {
  local entry_code=$?
  EXIT_CODE="$entry_code"
  local elapsed=$(( $(date +%s) - PROVISION_START_EPOCH ))

  if [ -n "$KEEP_ORG" ]; then
    TEARDOWN_STATUS="skipped_keep_org"
  else
    echo "::group::Teardown (trap)"
    local teardown_code
    teardown_code=$(curl "${CURL_COMMON[@]}" --max-time 120 \
      -X DELETE -o /dev/null -w '%{http_code}' \
      -H "Authorization: Bearer ${ADMIN_TOKEN}" \
      -H "Content-Type: application/json" \
      -d "{\"confirm\":\"${SLUG}\"}" \
      "$CP_URL/cp/admin/tenants/$SLUG" 2>/dev/null) || teardown_code=000
    log "DELETE /cp/admin/tenants/$SLUG -> HTTP $teardown_code"

    local remaining=1 waited=0
    while [ "$waited" -lt 60 ]; do
      remaining=$(org_present_count)
      [ "$remaining" = "0" ] && break
      sleep 5
      waited=$((waited + 5))
    done
    if [ "$remaining" = "0" ]; then
      TEARDOWN_STATUS="ok"
      ok "Teardown verified: org absent after ${waited}s"
    else
      TEARDOWN_STATUS="leak_risk_org_present"
      EXIT_CODE=6
      echo "::error::org $SLUG is still present after teardown (${waited}s); leak risk"
    fi
    echo "::endgroup::"
  fi

  write_result "$elapsed"
  echo "Structured results written to $RESULT_JSON"
  python3 -m json.tool "$RESULT_JSON" 2>/dev/null || true
  rm -f "$PROVISION_BODY" "$TENANT_HEALTH_BODY" "$WORKSPACE_LIST_BODY" \
    "$WORKSPACE_DETAIL_BODY" "$A2A_BODY" "$A2A_QUEUE_BODY"
  trap - EXIT
  exit "$EXIT_CODE"
}
trap on_exit EXIT
trap 'echo "::error::Script aborted on signal"; exit 130' INT TERM

admin_call() {
  local method="$1" route="$2"
  shift 2
  curl "${CURL_COMMON[@]}" -X "$method" "$CP_URL$route" \
    -H "Authorization: Bearer ${ADMIN_TOKEN}" \
    -H "Content-Type: application/json" "$@"
}

tenant_call() {
  local method="$1" route="$2"
  shift 2
  curl "${CURL_COMMON[@]}" -X "$method" "$TENANT_URL$route" \
    -H "Authorization: Bearer ${TENANT_TOKEN}" \
    -H "X-Molecule-Org-Id: ${ORG_ID}" \
    -H "Origin: ${TENANT_URL}" "$@"
}

safe_body_preview() {
  local value="$1" limit="${2:-400}"
  { printf '%s' "$value" | redact_secrets | head -c "$limit"; } || true
}

a2a_queue_id_from_response() {
  python3 -c '
import json, sys
try: doc = json.load(sys.stdin)
except Exception: raise SystemExit
queued = doc.get("queued") is True or str(doc.get("status") or "").lower() == "queued"
qid = doc.get("queue_id") or ""
if queued and isinstance(qid, str): print(qid)
' 2>/dev/null
}

poll_a2a_queue_result() {
  local deadline=$(( $(date +%s) + A2A_TIMEOUT_SECS ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    local code resp status
    code=$(tenant_call GET "/workspaces/${WORKSPACE_ID}/a2a/queue/${QUEUE_ID}" \
      --max-time 30 -H "X-Workspace-ID: ${WORKSPACE_ID}" \
      -o "$A2A_QUEUE_BODY" -w '%{http_code}' 2>/dev/null) || code=000
    resp=$(cat "$A2A_QUEUE_BODY" 2>/dev/null || echo "")
    if [ "$code" = "200" ]; then
      status=$(printf '%s' "$resp" | python3 -c '
import json, sys
try: print(json.load(sys.stdin).get("status", ""))
except Exception: print("")
' 2>/dev/null || echo "")
      case "$status" in
        completed)
          printf '%s' "$resp" | python3 -c '
import json, sys
try: body = json.load(sys.stdin).get("response_body")
except Exception: raise SystemExit(1)
if body is None: raise SystemExit(1)
print(json.dumps(body))
' >"$A2A_BODY" || fail "completed queue item omitted response_body"
          return 0
          ;;
        failed|dropped) fail "A2A queue item $QUEUE_ID ended $status: $(safe_body_preview "$resp")" ;;
        queued|dispatched|in_progress|"") ;;
        *) fail "A2A queue item $QUEUE_ID returned unexpected status '$status'" ;;
      esac
    elif [ "$code" != "404" ] && [ "$code" != "000" ]; then
      fail "A2A queue poll returned HTTP $code: $(safe_body_preview "$resp")"
    fi
    sleep 2
  done
  fail "A2A queue item $QUEUE_ID did not complete within ${A2A_TIMEOUT_SECS}s"
}

# Assertion 1: Provision through the current strict admin API.
echo "::group::Assertion 1: provision current admin contract"
log "POST $CP_URL/cp/admin/orgs slug=$SLUG provider=$PROVIDER"
PROVISION_PAYLOAD=$(python3 - "$SLUG" "$PROVIDER" <<'PY'
import json, sys
slug, provider = sys.argv[1:]
print(json.dumps({
    "slug": slug,
    "name": f"E2E {slug}",
    "owner_user_id": f"e2e-runner:{slug}",
    "provider": provider,
}))
PY
)
PROVISION_HTTP_CODE=$(admin_call POST /cp/admin/orgs -d "$PROVISION_PAYLOAD" \
  -o "$PROVISION_BODY" -w '%{http_code}' 2>/dev/null) || PROVISION_HTTP_CODE=000
PROVISION_RESP=$(cat "$PROVISION_BODY" 2>/dev/null || echo "")
if ! [[ "$PROVISION_HTTP_CODE" =~ ^2[0-9][0-9]$ ]]; then
  fail "org create failed HTTP $PROVISION_HTTP_CODE: $(safe_body_preview "$PROVISION_RESP")"
fi
ORG_ID=$(printf '%s' "$PROVISION_RESP" | python3 -c '
import json, sys
try: print(json.load(sys.stdin).get("id", ""))
except Exception: print("")
' 2>/dev/null || echo "")
[ -n "$ORG_ID" ] || fail "org create response missing id: $(safe_body_preview "$PROVISION_RESP")"

deadline=$(( $(date +%s) + PROVISION_TIMEOUT_SECS ))
LAST_STATUS=""
STATUS=""
while [ "$(date +%s)" -lt "$deadline" ]; do
  LIST_JSON=$(admin_call GET '/cp/admin/orgs?limit=500' 2>/dev/null || echo '{"orgs":[]}')
  STATUS=$(printf '%s' "$LIST_JSON" | SLUG="$SLUG" python3 -c '
import json, os, sys
try: rows = json.load(sys.stdin).get("orgs", [])
except Exception: rows = []
slug = os.environ["SLUG"]
print(next((row.get("instance_status", "") for row in rows if row.get("slug") == slug), ""))
' 2>/dev/null || echo "")
  if [ "$STATUS" != "$LAST_STATUS" ]; then log "instance_status -> ${STATUS:-<none>}"; LAST_STATUS="$STATUS"; fi
  case "$STATUS" in
    running) break ;;
    failed) fail "tenant provisioning failed: $(safe_body_preview "$LIST_JSON")" ;;
  esac
  sleep 5
done
[ "$STATUS" = "running" ] || { FAIL_CODE=3; fail "tenant did not reach running within ${PROVISION_TIMEOUT_SECS}s"; }
ok "Tenant provisioned (org_id=$ORG_ID)"
echo "::endgroup::"

# Current tenant URL and auth are separate from the control-plane admin API.
CP_HOST="${CP_URL#*://}"
CP_HOST="${CP_HOST%%/*}"
case "$CP_HOST" in
  staging-api.*) TENANT_DOMAIN="staging.${CP_HOST#staging-api.}" ;;
  api.*) TENANT_DOMAIN="${CP_HOST#api.}" ;;
  *) TENANT_DOMAIN="$CP_HOST" ;;
esac
TENANT_URL="${MOLECULE_TENANT_URL:-https://${SLUG}.${TENANT_DOMAIN}}"
TOKEN_RESP=$(admin_call GET "/cp/admin/orgs/$SLUG/admin-token" 2>/dev/null || echo '{}')
TENANT_TOKEN=$(printf '%s' "$TOKEN_RESP" | python3 -c '
import json, sys
try: print(json.load(sys.stdin).get("admin_token", ""))
except Exception: print("")
' 2>/dev/null || echo "")
[ -n "$TENANT_TOKEN" ] || fail "admin-token response omitted admin_token"

# A running control-plane instance is not sufficient: DNS, TLS, and the tenant
# edge route must answer before a workspace-detail failure can be attributed to
# the workspace server. Keep this phase explicit so a 000/edge error cannot be
# misreported as a runtime registration failure.
echo "::group::Tenant edge readiness"
deadline=$(( $(date +%s) + TENANT_READY_TIMEOUT_SECS ))
TENANT_HEALTH_CODE="000"
LAST_TENANT_HEALTH_CODE=""
while [ "$(date +%s)" -lt "$deadline" ]; do
  TENANT_HEALTH_CODE=$(curl "${CURL_COMMON[@]}" "$TENANT_URL/health" \
    -o "$TENANT_HEALTH_BODY" -w '%{http_code}' 2>/dev/null) || TENANT_HEALTH_CODE=000
  if [ "$TENANT_HEALTH_CODE" != "$LAST_TENANT_HEALTH_CODE" ]; then
    log "tenant /health -> HTTP $TENANT_HEALTH_CODE"
    LAST_TENANT_HEALTH_CODE="$TENANT_HEALTH_CODE"
  fi
  [[ "$TENANT_HEALTH_CODE" =~ ^2[0-9][0-9]$ ]] && break
  sleep 5
done
TENANT_HEALTH_RESP=$(cat "$TENANT_HEALTH_BODY" 2>/dev/null || echo "")
if ! [[ "$TENANT_HEALTH_CODE" =~ ^2[0-9][0-9]$ ]]; then
  FAIL_CODE=4
  fail "tenant edge did not become healthy within ${TENANT_READY_TIMEOUT_SECS}s (http=$TENANT_HEALTH_CODE body=$(safe_body_preview "$TENANT_HEALTH_RESP"))"
fi
ok "Tenant DNS/TLS/edge route is healthy"
echo "::endgroup::"

# Assertion 2: the platform root must have registered and be heartbeat-online.
echo "::group::Assertion 2: platform root registered and online"
deadline=$(( $(date +%s) + REGISTER_TIMEOUT_SECS ))
LAST_WORKSPACE_STATUS=""
LAST_WORKSPACE_LIST_CODE=""
LAST_WORKSPACE_DETAIL_CODE=""
WS_CODE="000"
WS_LIST=""
WS_DETAIL_CODE="000"
WS_DETAIL=""
WS_STATUS=""
WS_URL=""
WS_HEARTBEAT=""
WS_RUNTIME=""
while [ "$(date +%s)" -lt "$deadline" ]; do
  WS_CODE=$(tenant_call GET /workspaces -o "$WORKSPACE_LIST_BODY" -w '%{http_code}' 2>/dev/null) || WS_CODE=000
  WS_LIST=$(cat "$WORKSPACE_LIST_BODY" 2>/dev/null || echo '[]')
  if [ "$WS_CODE" != "$LAST_WORKSPACE_LIST_CODE" ]; then
    log "GET /workspaces -> HTTP $WS_CODE"
    LAST_WORKSPACE_LIST_CODE="$WS_CODE"
  fi
  if [ "$WS_CODE" = "200" ]; then
    WORKSPACE_ID=$(printf '%s' "$WS_LIST" | python3 -c '
import json, sys
try: rows = json.load(sys.stdin)
except Exception: rows = []
for row in rows if isinstance(rows, list) else []:
    if row.get("kind") == "platform" and not row.get("parent_id"):
        print(row.get("id", "")); break
' 2>/dev/null || echo "")
  fi
  if [ -n "$WORKSPACE_ID" ]; then
    WS_DETAIL_CODE=$(tenant_call GET "/workspaces/$WORKSPACE_ID" \
      -o "$WORKSPACE_DETAIL_BODY" -w '%{http_code}' 2>/dev/null) || WS_DETAIL_CODE=000
    WS_DETAIL=$(cat "$WORKSPACE_DETAIL_BODY" 2>/dev/null || echo '{}')
    if [ "$WS_DETAIL_CODE" = "200" ]; then
      eval "$(printf '%s' "$WS_DETAIL" | python3 -c '
import json, shlex, sys
try: row = json.load(sys.stdin)
except Exception: row = {}
for key, field in (("WS_STATUS", "status"), ("WS_URL", "url"), ("WS_HEARTBEAT", "last_heartbeat_at"), ("WS_RUNTIME", "runtime")):
    print(key + "=" + shlex.quote(str(row.get(field) or "")))
' 2>/dev/null)"
    else
      WS_STATUS=""; WS_URL=""; WS_HEARTBEAT=""; WS_RUNTIME=""
    fi
    if [ "$WS_DETAIL_CODE" != "$LAST_WORKSPACE_DETAIL_CODE" ] || [ "${WS_STATUS:-}" != "$LAST_WORKSPACE_STATUS" ]; then
      log "GET /workspaces/$WORKSPACE_ID -> HTTP $WS_DETAIL_CODE status=${WS_STATUS:-<none>}"
      LAST_WORKSPACE_DETAIL_CODE="$WS_DETAIL_CODE"
      LAST_WORKSPACE_STATUS="${WS_STATUS:-}"
    fi
    if [ "${WS_STATUS:-}" = "online" ] && [ -n "${WS_URL:-}" ] && [ -n "${WS_HEARTBEAT:-}" ]; then
      REGISTER_STATUS="ok"
      break
    fi
  fi
  sleep 5
done
if [ "$REGISTER_STATUS" != "ok" ]; then
  FAIL_CODE=4
  fail "platform root did not become online+routable+heartbeat-registered within ${REGISTER_TIMEOUT_SECS}s (id=${WORKSPACE_ID:-none} status=${WS_STATUS:-none} list_http=${WS_CODE:-000} detail_http=${WS_DETAIL_CODE:-000} detail_body=$(safe_body_preview "${WS_DETAIL:-}") list_body=$(safe_body_preview "${WS_LIST:-}" 800))"
fi
[ -n "$WS_RUNTIME" ] || {
  FAIL_CODE=4
  fail "platform root detail omitted runtime"
}
[ "$WS_RUNTIME" = "$RUNTIME" ] || {
  FAIL_CODE=4
  fail "platform root runtime=$WS_RUNTIME, expected SSOT runtime=$RUNTIME"
}
ok "Registration visible through tenant API (workspace_id=$WORKSPACE_ID runtime=$WS_RUNTIME)"
echo "::endgroup::"

# Assertion 3: a real, deterministic A2A completion on the current route.
echo "::group::Assertion 3: real A2A completion"
FAIL_CODE=5
A2A_PAYLOAD=$(python3 -c '
import json, uuid
print(json.dumps({
    "jsonrpc": "2.0",
    "method": "message/send",
    "id": "minimal-cell-known-answer",
    "params": {"message": {
        "role": "user",
        "messageId": f"e2e-{uuid.uuid4().hex[:8]}",
        "parts": [{"kind": "text", "text": "This is a platform wiring check. No tools or memory are needed. Reply with exactly the word PINEAPPLE and nothing else."}],
    }},
}))
')
A2A_OK=0
for attempt in 1 2 3; do
  A2A_CODE=$(tenant_call POST "/workspaces/${WORKSPACE_ID}/a2a" \
    --max-time "$A2A_TIMEOUT_SECS" -H "Content-Type: application/json" \
    -H "X-Workspace-ID: ${WORKSPACE_ID}" -d "$A2A_PAYLOAD" \
    -o "$A2A_BODY" -w '%{http_code}' 2>/dev/null) || A2A_CODE=000
  A2A_RESP=$(cat "$A2A_BODY" 2>/dev/null || echo "")
  if [[ "$A2A_CODE" =~ ^2[0-9][0-9]$ ]]; then A2A_OK=1; break; fi
  if [[ "$A2A_CODE" =~ ^50[234]$ ]] && [ "$attempt" -lt 3 ]; then
    log "A2A cold-start attempt $attempt returned HTTP $A2A_CODE; retrying"
    sleep 5
    continue
  fi
  break
done
[ "$A2A_OK" = "1" ] || fail "A2A POST failed HTTP $A2A_CODE: $(safe_body_preview "$A2A_RESP")"

QUEUE_ID=$(printf '%s' "$A2A_RESP" | a2a_queue_id_from_response || echo "")
if [ -n "$QUEUE_ID" ]; then
  log "A2A queued as $QUEUE_ID; polling durable result"
  poll_a2a_queue_result
  A2A_RESP=$(cat "$A2A_BODY" 2>/dev/null || echo "")
fi
A2A_TEXT=$(printf '%s' "$A2A_RESP" | python3 "$SCRIPT_DIR/lib/a2a_text_extract.py" 2>/dev/null || echo "")
[ -n "$A2A_TEXT" ] || log "A2A extraction empty; response: $(safe_body_preview "$A2A_RESP")"
a2a_assert_real_completion "$A2A_TEXT" "PINEAPPLE" "minimal boot cell"
COMPLETION_STATUS="ok"
ok "Real A2A completion succeeded"
echo "::endgroup::"

FAIL_CODE=1
ok "All four minimal-cell assertions passed for $SLUG"
