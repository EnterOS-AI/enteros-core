#!/usr/bin/env bash
set -uo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
LIB="$SCRIPT_DIR/cp_purge_receipt.sh"

# shellcheck source=cp_purge_receipt.sh
if ! source "$LIB"; then
  echo "FAIL: unable to source $LIB" >&2
  exit 1
fi

TMPDIR_TEST=$(mktemp -d -t cp-purge-receipt-XXXXXX)
trap 'rm -rf "$TMPDIR_TEST"' EXIT INT TERM

cat > "$TMPDIR_TEST/curl" <<'FAKE_CURL'
#!/usr/bin/env bash
set -euo pipefail

out=""
url=""
method="GET"
write_out=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o|--output)
      out="$2"
      shift 2
      ;;
    -X|--request)
      method="$2"
      shift 2
      ;;
    -w|--write-out)
      write_out=1
      shift 2
      ;;
    -H|--header|-d|--data|--max-time|-A|--user-agent)
      shift 2
      ;;
    http://*|https://*)
      url="$1"
      shift
      ;;
    *)
      shift
      ;;
  esac
done

printf '%s %s\n' "$method" "$url" >> "${FAKE_CALL_LOG:?}"

case "$url" in
  */health)
    case "${FAKE_SCENARIO:?}" in
      create-http-409|create-http-500)
        printf '%s' '{"status":"ok"}'
        ;;
      *)
        exit 7
        ;;
    esac
    ;;
  */cp/admin/tenants/*/boot-events\?limit=1)
    case "${FAKE_SCENARIO:?}" in
      already-absent)
        body='{"error":"org not found","slug":"e2e-receipt-unit"}'
        code=404
        ;;
      absent-generic-html)
        body='<html><body>route not found</body></html>'
        code=404
        ;;
      absent-empty)
        body=''
        code=404
        ;;
      absent-mismatched-json)
        body='{"error":"org not found","slug":"e2e-another-org"}'
        code=404
        ;;
      malformed-identity)
        body='{"slug":"wrong-slug","org_id":"not-a-uuid","count":0,"events":[]}'
        code=200
        ;;
      identity-error)
        body='{"error":"db lookup failed"}'
        code=500
        ;;
      foreign-slug-collision)
        body='{"slug":"e2e-receipt-unit","org_id":"33333333-3333-4333-8333-333333333333","count":0,"events":[]}'
        code=200
        ;;
      create-http-409|create-http-500)
        if grep -q '^deleted$' "${FAKE_STATE_FILE:?}" 2>/dev/null; then
          body='{"error":"org not found"}'
          code=404
        else
          created_slug=$(sed -n 's/^E2E_CREATED_SLUG=//p' "${GITHUB_ENV:?}" | tail -n 1)
          body="{\"slug\":\"$created_slug\",\"org_id\":\"33333333-3333-4333-8333-333333333333\",\"count\":0,\"events\":[]}"
          code=200
        fi
        ;;
      org-present)
        body='{"slug":"e2e-receipt-unit","org_id":"11111111-1111-4111-8111-111111111111","count":0,"events":[]}'
        code=200
        ;;
      *)
        if grep -q '^deleted$' "${FAKE_STATE_FILE:?}" 2>/dev/null; then
          body='{"error":"org not found","slug":"e2e-receipt-unit"}'
          code=404
        else
          body='{"slug":"e2e-receipt-unit","org_id":"11111111-1111-4111-8111-111111111111","count":0,"events":[]}'
          code=200
        fi
        ;;
    esac
    if [ -n "$out" ]; then
      printf '%s' "$body" > "$out"
    else
      printf '%s' "$body"
    fi
    if [ "$write_out" = "1" ]; then
      printf '%s' "$code"
    fi
    ;;
  */cp/admin/tenants/*)
    case "${FAKE_SCENARIO:?}" in
      transport-loss-audit-recovered|transport-loss-audit-missing|transport-loss-stale-audit)
        # Model the live ephemeral-CP failure: the handler completes the purge,
        # but its Docker-network detach drops the response before curl receives
        # an HTTP status/body. The caller may recover only from the exact audit
        # plus exact structured absence proof.
        printf 'deleted\n' > "${FAKE_STATE_FILE:?}"
        exit 52
        ;;
      delete-409-then-200)
        # Transient "organization has an active lifecycle operation": the first
        # DELETE conflicts (org still settling its own workspace op), the retry
        # succeeds. Count prior DELETE calls in the log (this call is already
        # appended) to decide which attempt this is.
        dn=$(grep -c '^DELETE ' "${FAKE_CALL_LOG:?}" 2>/dev/null || printf 0)
        if [ "${dn:-0}" -le 1 ]; then
          printf '%s' '{"error":"organization has an active lifecycle operation"}' > "$out"
          printf '409'
        else
          printf '%s' '{"deleted":true,"slug":"e2e-receipt-unit","org_id":"11111111-1111-4111-8111-111111111111","purge_id":"22222222-2222-4222-8222-222222222222"}' > "$out"
          printf 'deleted\n' > "${FAKE_STATE_FILE:?}"
          printf '200'
        fi
        exit 0
        ;;
      delete-409-persistent)
        # Conflict never clears: the retry loop must give up at the deadline.
        printf '%s' '{"error":"organization has an active lifecycle operation"}' > "$out"
        printf '409'
        exit 0
        ;;
      delete-500-persistent)
        # Negative control: a 500 is NOT a self-resolving conflict. It must fail
        # immediately without any retry (guards that only 409 is retried).
        printf '%s' '{"error":"purge cascade failed; automatic retry scheduled"}' > "$out"
        printf '500'
        exit 0
        ;;
      malformed-receipt) body='{"deleted":true,"slug":"e2e-receipt-unit"}' ;;
      receipt-org-mismatch) body='{"deleted":true,"slug":"e2e-receipt-unit","org_id":"33333333-3333-4333-8333-333333333333","purge_id":"22222222-2222-4222-8222-222222222222"}' ;;
      create-http-409|create-http-500)
        created_slug=$(sed -n 's/^E2E_CREATED_SLUG=//p' "${GITHUB_ENV:?}" | tail -n 1)
        body="{\"deleted\":true,\"slug\":\"$created_slug\",\"org_id\":\"33333333-3333-4333-8333-333333333333\",\"purge_id\":\"22222222-2222-4222-8222-222222222222\"}"
        ;;
      *) body='{"deleted":true,"slug":"e2e-receipt-unit","org_id":"11111111-1111-4111-8111-111111111111","purge_id":"22222222-2222-4222-8222-222222222222"}' ;;
    esac
    printf '%s' "$body" > "$out"
    printf 'deleted\n' > "${FAKE_STATE_FILE:?}"
    printf '200'
    ;;
  */cp/admin/purges*)
    case "${FAKE_SCENARIO:?}" in
      transport-loss-audit-recovered)
        completed_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
        body="{\"purges\":[{\"id\":\"22222222-2222-4222-8222-222222222222\",\"org_id\":\"11111111-1111-4111-8111-111111111111\",\"org_slug\":\"e2e-receipt-unit\",\"status\":\"completed\",\"last_step\":\"completed\",\"completed_at\":\"$completed_at\"}]}"
        ;;
      transport-loss-audit-missing)
        body='{"purges":[]}'
        ;;
      transport-loss-stale-audit)
        body='{"purges":[{"id":"22222222-2222-4222-8222-222222222222","org_id":"11111111-1111-4111-8111-111111111111","org_slug":"e2e-receipt-unit","status":"completed","last_step":"completed","completed_at":"2020-01-01T00:00:00Z"}]}'
        ;;
      create-http-409|create-http-500)
        created_slug=$(sed -n 's/^E2E_CREATED_SLUG=//p' "${GITHUB_ENV:?}" | tail -n 1)
        body="{\"purges\":[{\"id\":\"22222222-2222-4222-8222-222222222222\",\"org_id\":\"33333333-3333-4333-8333-333333333333\",\"org_slug\":\"$created_slug\",\"status\":\"completed\",\"last_step\":\"completed\",\"completed_at\":\"2026-07-15T00:00:00Z\"}]}"
        ;;
      failed-purge)
        body='{"purges":[{"id":"22222222-2222-4222-8222-222222222222","org_id":"11111111-1111-4111-8111-111111111111","org_slug":"e2e-receipt-unit","status":"failed","last_step":"infra"}]}'
        ;;
      audit-anomaly)
        body='{"purges":[{"id":"22222222-2222-4222-8222-222222222222","org_id":"11111111-1111-4111-8111-111111111111","org_slug":"e2e-receipt-unit","status":"completed","last_step":"completed","completed_at":null}]}'
        ;;
      audit-wrong-type)
        body='{"purges":[{"id":"22222222-2222-4222-8222-222222222222","org_id":"11111111-1111-4111-8111-111111111111","org_slug":"e2e-receipt-unit","status":"completed","last_step":"completed","completed_at":{"unexpected":true}}]}'
        ;;
      audit-org-mismatch)
        body='{"purges":[{"id":"22222222-2222-4222-8222-222222222222","org_id":"33333333-3333-4333-8333-333333333333","org_slug":"e2e-receipt-unit","status":"completed","last_step":"completed","completed_at":"2026-07-15T00:00:00Z"}]}'
        ;;
      *)
        body='{"purges":[{"id":"22222222-2222-4222-8222-222222222222","org_id":"11111111-1111-4111-8111-111111111111","org_slug":"e2e-receipt-unit","status":"completed","last_step":"completed","completed_at":"2026-07-15T00:00:00Z"}]}'
        ;;
    esac
    printf '%s' "$body" > "$out"
    printf '200'
    ;;
  */cp/admin/orgs*)
    if [ "$method" = "POST" ]; then
      # Real curl returns zero for HTTP errors when --fail is not set. Keep the
      # response JSON deliberately dangerous: its valid foreign ID reproduced
      # the bug where the harness treated a 409/500 body as a successful create.
      case "${FAKE_SCENARIO:?}" in
        create-http-409) code=409 ;;
        create-http-500) code=500 ;;
        *) echo "unexpected fake org-create scenario: ${FAKE_SCENARIO:?}" >&2; exit 9 ;;
      esac
      body='{"id":"33333333-3333-4333-8333-333333333333","error":"foreign org collision"}'
      if [ -n "$out" ]; then
        printf '%s' "$body" > "$out"
      else
        printf '%s' "$body"
      fi
      if [ "$write_out" = "1" ]; then
        printf '%s' "$code"
      fi
      exit 0
    fi
    case "${FAKE_SCENARIO:?}" in
      create-http-409|create-http-500)
        created_slug=$(sed -n 's/^E2E_CREATED_SLUG=//p' "${GITHUB_ENV:?}" | tail -n 1)
        body="{\"limit\":500,\"offset\":0,\"orgs\":[{\"slug\":\"$created_slug\",\"id\":\"33333333-3333-4333-8333-333333333333\",\"instance_status\":\"failed\"}]}"
        code=200
        ;;
      roster-auth-error)
        body='{"error":"unauthorized"}'
        code=401
        ;;
      roster-malformed)
        body='not-json'
        code=200
        ;;
      *)
        body='{"limit":500,"offset":0,"orgs":[]}'
        code=200
        ;;
    esac
    if [ -n "$out" ]; then
      printf '%s' "$body" > "$out"
    else
      printf '%s' "$body"
    fi
    if [ "$write_out" = "1" ]; then
      printf '%s' "$code"
    fi
    ;;
  *)
    echo "unexpected fake curl URL: $url" >&2
    exit 9
    ;;
esac
FAKE_CURL
chmod +x "$TMPDIR_TEST/curl"

PASS=0
FAIL=0

run_case() {
  local scenario="$1" backend="${2:-local-docker}"
  local cp_url="${3:-https://staging-api.moleculesai.app}"
  local slug="${4:-e2e-receipt-unit}"
  local state_file="${5:-$TMPDIR_TEST/state}"
  local call_log="${6:-$TMPDIR_TEST/calls}"
  local poll_secs="${7:-0}"
  local poll_interval="${8:-0}"
  local expected_org_id="11111111-1111-4111-8111-111111111111"
  if [ "$#" -ge 9 ]; then
    expected_org_id="$9"
  fi
  rm -f "$state_file" "$call_log"
  : > "$call_log"
  PATH="$TMPDIR_TEST:$PATH" \
    FAKE_SCENARIO="$scenario" \
    FAKE_STATE_FILE="$state_file" \
    FAKE_CALL_LOG="$call_log" \
    E2E_INFRA_BACKEND="$backend" \
    E2E_CP_PURGE_POLL_SECS="$poll_secs" \
    E2E_CP_PURGE_POLL_INTERVAL="$poll_interval" \
    E2E_CP_PURGE_DELETE_RETRY_SECS="$poll_secs" \
    e2e_cp_delete_and_verify_purge \
      "$cp_url" "test-token" "$slug" "$expected_org_id"
}

run_roster_case() {
  local scenario="$1"
  local state_file="$TMPDIR_TEST/roster-state"
  local call_log="$TMPDIR_TEST/roster-calls"
  rm -f "$state_file" "$call_log"
  : > "$call_log"
  PATH="$TMPDIR_TEST:$PATH" \
    FAKE_SCENARIO="$scenario" \
    FAKE_STATE_FILE="$state_file" \
    FAKE_CALL_LOG="$call_log" \
    E2E_INFRA_BACKEND=local-docker \
    e2e_cp_fetch_org_roster_json \
      "https://staging-api.moleculesai.app" "test-token"
}

assert_rc() {
  local label="$1" expected="$2" scenario="$3" backend="${4:-local-docker}"
  local output observed
  output=$(run_case "$scenario" "$backend" 2>&1)
  observed=$?
  if [ "$observed" = "$expected" ]; then
    echo "PASS: $label (rc=$observed)"
    PASS=$((PASS + 1))
  else
    echo "FAIL: $label expected rc=$expected observed=$observed" >&2
    printf '%s\n' "$output" | sed 's/^/  /' >&2
    FAIL=$((FAIL + 1))
  fi
}

echo "Test: exact control-plane purge receipt + org absence verifier"

success_output=$(run_case success 2>&1)
success_rc=$?
if [ "$success_rc" = "0" ] \
  && printf '%s' "$success_output" | grep -q 'CP purge completed' \
  && printf '%s' "$success_output" | grep -q 'exact org absent' \
  && ! printf '%s' "$success_output" | grep -qi 'EC2'; then
  echo "PASS: successful local purge reports only the evidence actually checked"
  PASS=$((PASS + 1))
else
  echo "FAIL: truthful success output contract (rc=$success_rc)" >&2
  printf '%s\n' "$success_output" | sed 's/^/  /' >&2
  FAIL=$((FAIL + 1))
fi

transport_recovery_output=$(run_case transport-loss-audit-recovered 2>&1)
transport_recovery_rc=$?
if [ "$transport_recovery_rc" = "0" ] \
  && printf '%s' "$transport_recovery_output" | grep -q 'DELETE response was lost' \
  && printf '%s' "$transport_recovery_output" | grep -q 'recovered exact completed purge audit' \
  && printf '%s' "$transport_recovery_output" | grep -q 'exact org absent'; then
  echo "PASS: lost DELETE response recovers only from exact completed audit plus exact absence"
  PASS=$((PASS + 1))
else
  echo "FAIL: exact audit recovery after lost DELETE response (rc=$transport_recovery_rc)" >&2
  printf '%s\n' "$transport_recovery_output" | sed 's/^/  /' >&2
  FAIL=$((FAIL + 1))
fi

assert_rc "lost DELETE response without exact completed audit fails closed" 4 transport-loss-audit-missing
assert_rc "lost DELETE response cannot reuse a stale completed audit" 4 transport-loss-stale-audit

assert_rc "missing purge_id fails closed" 4 malformed-receipt
assert_rc "receipt org_id must match the pre-delete identity" 4 receipt-org-mismatch
assert_rc "failed exact purge audit fails closed" 4 failed-purge
assert_rc "inconsistent completed purge audit fails closed" 4 audit-anomaly
assert_rc "non-string purge completion marker fails closed" 4 audit-wrong-type
assert_rc "mismatched purge audit org_id fails closed" 4 audit-org-mismatch
assert_rc "exact org still present fails closed" 4 org-present
assert_rc "malformed exact-tenant 200 is inconclusive" 4 malformed-identity
assert_rc "exact-tenant endpoint error is inconclusive" 4 identity-error

# ── transient 409 "active lifecycle operation" is retried until it clears ──
# Reproduces run 539697: the org's own workspace lifecycle op had not drained
# at teardown, so the synchronous DELETE returned 409. It is self-resolving —
# a bounded retry must let it clear rather than hard-failing and leaking.
retry_out=$(run_case delete-409-then-200 local-docker \
  https://staging-api.moleculesai.app e2e-receipt-unit \
  "$TMPDIR_TEST/retry-state" "$TMPDIR_TEST/retry-calls" 5 0 2>&1)
retry_rc=$?
retry_deletes=$(grep -c '^DELETE ' "$TMPDIR_TEST/retry-calls" 2>/dev/null || true)
if [ "$retry_rc" = "0" ] \
  && printf '%s' "$retry_out" | grep -q 'active lifecycle operation' \
  && printf '%s' "$retry_out" | grep -q 'CP purge completed' \
  && [ "${retry_deletes:-0}" -ge 2 ]; then
  echo "PASS: transient 409 active-lifecycle DELETE is retried until it clears (attempts=$retry_deletes)"
  PASS=$((PASS + 1))
else
  echo "FAIL: transient 409 retry (rc=$retry_rc deletes=$retry_deletes)" >&2
  printf '%s\n' "$retry_out" | sed 's/^/  /' >&2
  FAIL=$((FAIL + 1))
fi

# ── a 409 that never clears retries, then fails closed at the deadline ──
persist_out=$(run_case delete-409-persistent local-docker \
  https://staging-api.moleculesai.app e2e-receipt-unit \
  "$TMPDIR_TEST/persist409-state" "$TMPDIR_TEST/persist409-calls" 1 1 2>&1)
persist_rc=$?
persist_deletes=$(grep -c '^DELETE ' "$TMPDIR_TEST/persist409-calls" 2>/dev/null || true)
if [ "$persist_rc" = "4" ] && [ "${persist_deletes:-0}" -ge 2 ]; then
  echo "PASS: persistent 409 retries then fails closed at the deadline (attempts=$persist_deletes)"
  PASS=$((PASS + 1))
else
  echo "FAIL: persistent 409 fail-closed (rc=$persist_rc deletes=$persist_deletes; expected rc=4 deletes>=2)" >&2
  printf '%s\n' "$persist_out" | sed 's/^/  /' >&2
  FAIL=$((FAIL + 1))
fi

# ── NEGATIVE CONTROL: a non-409 (500) is NOT a self-resolving conflict — it
#    must fail closed on the FIRST attempt, with NO retry, even with poll budget.
noretry_out=$(run_case delete-500-persistent local-docker \
  https://staging-api.moleculesai.app e2e-receipt-unit \
  "$TMPDIR_TEST/noretry-state" "$TMPDIR_TEST/noretry-calls" 5 0 2>&1)
noretry_rc=$?
noretry_deletes=$(grep -c '^DELETE ' "$TMPDIR_TEST/noretry-calls" 2>/dev/null || true)
if [ "$noretry_rc" = "4" ] && [ "${noretry_deletes:-0}" = "1" ]; then
  echo "PASS: non-409 DELETE fails closed without retry (attempts=$noretry_deletes)"
  PASS=$((PASS + 1))
else
  echo "FAIL: non-409 must not retry (rc=$noretry_rc deletes=$noretry_deletes; expected rc=4 deletes=1)" >&2
  printf '%s\n' "$noretry_out" | sed 's/^/  /' >&2
  FAIL=$((FAIL + 1))
fi
assert_rc "non-local backend is rejected" 2 success legacy-ec2

collision_calls="$TMPDIR_TEST/collision-calls"
collision_output=$(run_case foreign-slug-collision local-docker \
  "https://staging-api.moleculesai.app" "e2e-receipt-unit" \
  "$TMPDIR_TEST/collision-state" "$collision_calls" 0 0 2>&1)
collision_rc=$?
if [ "$collision_rc" = "4" ] \
  && printf '%s' "$collision_output" | grep -q 'org_id mismatch' \
  && ! grep -q '^DELETE ' "$collision_calls" \
  && ! grep -q '/cp/admin/purges' "$collision_calls"; then
  echo "PASS: foreign org reusing the slug is refused before DELETE"
  PASS=$((PASS + 1))
else
  echo "FAIL: foreign-slug collision must perform zero DELETE/purge audit (rc=$collision_rc)" >&2
  printf '%s\n' "$collision_output" | sed 's/^/  /' >&2
  FAIL=$((FAIL + 1))
fi

missing_identity_calls="$TMPDIR_TEST/missing-identity-calls"
missing_identity_output=$(run_case success local-docker \
  "https://staging-api.moleculesai.app" "e2e-receipt-unit" \
  "$TMPDIR_TEST/missing-identity-state" "$missing_identity_calls" 0 0 "" 2>&1)
missing_identity_rc=$?
if [ "$missing_identity_rc" = "2" ] \
  && printf '%s' "$missing_identity_output" | grep -q 'creation-returned org_id is required' \
  && [ ! -s "$missing_identity_calls" ]; then
  echo "PASS: missing creation identity performs no probe or destructive cleanup"
  PASS=$((PASS + 1))
else
  echo "FAIL: missing creation identity must perform zero control-plane requests (rc=$missing_identity_rc)" >&2
  printf '%s\n' "$missing_identity_output" | sed 's/^/  /' >&2
  FAIL=$((FAIL + 1))
fi

published_identity="$TMPDIR_TEST/published-identity.env"
rm -f "$published_identity"
GITHUB_ENV="$published_identity" \
  e2e_cp_publish_creation_identity \
    "e2e-receipt-unit" "11111111-1111-4111-8111-111111111111" 2>/dev/null
published_identity_rc=$?
if [ "$published_identity_rc" = "0" ] \
  && [ "$(sed -n '1p' "$published_identity")" = "E2E_CREATED_SLUG=e2e-receipt-unit" ] \
  && [ "$(sed -n '2p' "$published_identity")" = "E2E_CREATED_ORG_ID=11111111-1111-4111-8111-111111111111" ] \
  && [ "$(wc -l < "$published_identity" | tr -d ' ')" = "2" ]; then
  echo "PASS: verified creation identity is published exactly for the safety net"
  PASS=$((PASS + 1))
else
  echo "FAIL: verified creation identity publication contract (rc=$published_identity_rc)" >&2
  [ -f "$published_identity" ] && sed 's/^/  /' "$published_identity" >&2
  FAIL=$((FAIL + 1))
fi

absent_state="$TMPDIR_TEST/absent-state"
absent_calls="$TMPDIR_TEST/absent-calls"
absent_output=$(run_case already-absent local-docker \
  "https://staging-api.moleculesai.app" "e2e-receipt-unit" \
  "$absent_state" "$absent_calls" 2>&1)
absent_rc=$?
if [ "$absent_rc" = "0" ] \
  && printf '%s' "$absent_output" | grep -q 'already absent' \
  && ! grep -q '^DELETE ' "$absent_calls" \
  && ! grep -q '/cp/admin/purges' "$absent_calls"; then
  echo "PASS: exact-tenant 404 is idempotent and performs no DELETE/purge audit"
  PASS=$((PASS + 1))
else
  echo "FAIL: already-absent cleanup contract (rc=$absent_rc)" >&2
  printf '%s\n' "$absent_output" | sed 's/^/  /' >&2
  FAIL=$((FAIL + 1))
fi

for rejected_absence in absent-generic-html absent-empty absent-mismatched-json; do
  rejected_state="$TMPDIR_TEST/${rejected_absence}-state"
  rejected_calls="$TMPDIR_TEST/${rejected_absence}-calls"
  rejected_output=$(run_case "$rejected_absence" local-docker \
    "https://staging-api.moleculesai.app" "e2e-receipt-unit" \
    "$rejected_state" "$rejected_calls" 2>&1)
  rejected_rc=$?
  if [ "$rejected_rc" = "4" ] \
    && printf '%s' "$rejected_output" | grep -q 'malformed or mismatched HTTP 404' \
    && ! grep -q '^DELETE ' "$rejected_calls" \
    && ! grep -q '/cp/admin/purges' "$rejected_calls"; then
    echo "PASS: $rejected_absence cannot manufacture authoritative absence"
    PASS=$((PASS + 1))
  else
    echo "FAIL: $rejected_absence must fail closed before DELETE/purge audit (rc=$rejected_rc)" >&2
    printf '%s\n' "$rejected_output" | sed 's/^/  /' >&2
    FAIL=$((FAIL + 1))
  fi
done

assert_precreate_outage_is_non_destructive() {
  local label="$1" harness="$2"
  local call_log="$TMPDIR_TEST/precreate-${label}.calls"
  local state_file="$TMPDIR_TEST/precreate-${label}.state"
  local output rc call_count
  rm -f "$call_log" "$state_file"
  : > "$call_log"
  output=$(PATH="$TMPDIR_TEST:$PATH" \
    FAKE_SCENARIO=already-absent \
    FAKE_STATE_FILE="$state_file" \
    FAKE_CALL_LOG="$call_log" \
    E2E_INFRA_BACKEND=local-docker \
    E2E_REQUIRE_LIVE=1 \
    E2E_RUN_ID=precreate-unit \
    E2E_MODE=smoke \
    OBS_ENABLED=0 \
    MOLECULE_CP_URL=https://staging-api.moleculesai.app \
    MOLECULE_ADMIN_TOKEN=test-token \
    bash "$harness" 2>&1)
  rc=$?
  call_count=$(wc -l < "$call_log" | tr -d ' ')
  if [ "$rc" = "1" ] \
    && [ "$call_count" = "1" ] \
    && grep -q '^GET https://staging-api.moleculesai.app/health$' "$call_log" \
    && printf '%s' "$output" | grep -q 'skipping destructive org teardown'; then
    echo "PASS: $label preserves the pre-create outage failure and performs no cleanup request"
    PASS=$((PASS + 1))
  else
    echo "FAIL: $label pre-create outage trap was destructive or changed rc (rc=$rc calls=$call_count)" >&2
    printf '%s\n' "$output" | sed 's/^/  /' >&2
    sed 's/^/  call: /' "$call_log" >&2
    FAIL=$((FAIL + 1))
  fi
}

E2E_DIR=$(cd "$SCRIPT_DIR/.." && pwd)
assert_precreate_outage_is_non_destructive \
  full-saas "$E2E_DIR/test_staging_full_saas.sh"
assert_precreate_outage_is_non_destructive \
  concierge "$E2E_DIR/test_staging_concierge_e2e.sh"
assert_precreate_outage_is_non_destructive \
  concierge-workspace "$E2E_DIR/test_staging_concierge_creates_workspace_e2e.sh"

assert_failed_create_status_is_non_destructive() {
  local label="$1" harness="$2" http_code="$3"
  local scenario="create-http-$http_code"
  local call_log="$TMPDIR_TEST/create-${label}-${http_code}.calls"
  local state_file="$TMPDIR_TEST/create-${label}-${http_code}.state"
  local github_env="$TMPDIR_TEST/create-${label}-${http_code}.env"
  local output rc
  rm -f "$call_log" "$state_file" "$github_env"
  : > "$call_log"
  : > "$github_env"
  output=$(PATH="$TMPDIR_TEST:$PATH" \
    FAKE_SCENARIO="$scenario" \
    FAKE_STATE_FILE="$state_file" \
    FAKE_CALL_LOG="$call_log" \
    GITHUB_ENV="$github_env" \
    E2E_INFRA_BACKEND=local-docker \
    E2E_REQUIRE_LIVE=1 \
    E2E_RUN_ID="create-${http_code}-unit" \
    OBS_ENABLED=0 \
    MOLECULE_CP_URL=https://staging-api.moleculesai.app \
    MOLECULE_ADMIN_TOKEN=test-token \
    bash "$harness" 2>&1)
  rc=$?
  if [ "$rc" = "1" ] \
    && printf '%s' "$output" | grep -q "http=$http_code" \
    && ! grep -q '^DELETE ' "$call_log" \
    && ! grep -q '/cp/admin/purges' "$call_log" \
    && ! grep -q '/boot-events' "$call_log" \
    && ! grep -q '^E2E_CREATED_' "$github_env"; then
    echo "PASS: $label HTTP $http_code create failure preserves failure and authorizes no teardown"
    PASS=$((PASS + 1))
  else
    echo "FAIL: $label HTTP $http_code JSON response must not authorize teardown (rc=$rc)" >&2
    printf '%s\n' "$output" | sed 's/^/  /' >&2
    sed 's/^/  call: /' "$call_log" >&2
    sed 's/^/  env: /' "$github_env" >&2
    FAIL=$((FAIL + 1))
  fi
}

for create_http_code in 409 500; do
  assert_failed_create_status_is_non_destructive \
    concierge "$E2E_DIR/test_staging_concierge_e2e.sh" "$create_http_code"
  assert_failed_create_status_is_non_destructive \
    concierge-workspace "$E2E_DIR/test_staging_concierge_creates_workspace_e2e.sh" "$create_http_code"
done

assert_poll_rejected() {
  local label="$1" poll_secs="$2" poll_interval="$3"
  local output observed
  output=$(run_case success local-docker \
    "https://staging-api.moleculesai.app" "e2e-receipt-unit" \
    "$TMPDIR_TEST/poll-state" "$TMPDIR_TEST/poll-calls" \
    "$poll_secs" "$poll_interval" 2>&1)
  observed=$?
  if [ "$observed" = "2" ]; then
    echo "PASS: $label"
    PASS=$((PASS + 1))
  else
    echo "FAIL: $label expected rc=2 observed=$observed" >&2
    printf '%s\n' "$output" | sed 's/^/  /' >&2
    FAIL=$((FAIL + 1))
  fi
}

assert_poll_rejected "embedded colon in poll seconds is rejected" "1:2" 0
assert_poll_rejected "octal-invalid poll seconds are rejected" 08 0
assert_poll_rejected "octal-invalid poll interval is rejected independently" 0 08

roster_success=$(run_roster_case success 2>&1)
roster_success_rc=$?
if [ "$roster_success_rc" = "0" ] \
  && printf '%s' "$roster_success" | python3 -c 'import json,sys; assert isinstance(json.load(sys.stdin)["orgs"], list)'; then
  echo "PASS: safety-net roster discovery returns validated JSON"
  PASS=$((PASS + 1))
else
  echo "FAIL: validated safety-net roster discovery (rc=$roster_success_rc)" >&2
  FAIL=$((FAIL + 1))
fi

roster_auth_output=$(run_roster_case roster-auth-error 2>&1)
roster_auth_rc=$?
roster_malformed_output=$(run_roster_case roster-malformed 2>&1)
roster_malformed_rc=$?
if [ "$roster_auth_rc" = "4" ] \
  && [ "$roster_malformed_rc" = "4" ] \
  && printf '%s' "$roster_auth_output" | grep -q 'inconclusive' \
  && printf '%s' "$roster_malformed_output" | grep -q 'inconclusive'; then
  echo "PASS: safety-net auth and malformed-JSON discovery fail visibly inconclusive"
  PASS=$((PASS + 1))
else
  echo "FAIL: safety-net inconclusive discovery contract" >&2
  printf '%s\n%s\n' "$roster_auth_output" "$roster_malformed_output" | sed 's/^/  /' >&2
  FAIL=$((FAIL + 1))
fi

untrusted_output=$(run_case success local-docker "https://attacker.invalid" 2>&1)
untrusted_rc=$?
if [ "$untrusted_rc" = "2" ] \
  && printf '%s' "$untrusted_output" | grep -q 'refusing to send the admin bearer'; then
  echo "PASS: untrusted HTTPS control-plane host is rejected before bearer use"
  PASS=$((PASS + 1))
else
  echo "FAIL: untrusted HTTPS host expected rc=2 observed=$untrusted_rc" >&2
  printf '%s\n' "$untrusted_output" | sed 's/^/  /' >&2
  FAIL=$((FAIL + 1))
fi

malformed_slug_output=$(run_case success local-docker \
  "https://staging-api.moleculesai.app" "e2e-receipt-unit?redirect=1" 2>&1)
malformed_slug_rc=$?
if [ "$malformed_slug_rc" = "2" ]; then
  echo "PASS: malformed E2E slug is rejected before URL construction"
  PASS=$((PASS + 1))
else
  echo "FAIL: malformed E2E slug expected rc=2 observed=$malformed_slug_rc" >&2
  printf '%s\n' "$malformed_slug_output" | sed 's/^/  /' >&2
  FAIL=$((FAIL + 1))
fi

E2E_INFRA_BACKEND=legacy-ec2 e2e_cp_require_local_backend >/dev/null 2>&1
backend_guard_rc=$?
if [ "$backend_guard_rc" = "2" ]; then
  echo "PASS: standalone preflight rejects non-local backend before provisioning"
  PASS=$((PASS + 1))
else
  echo "FAIL: standalone backend preflight expected rc=2 observed=$backend_guard_rc" >&2
  FAIL=$((FAIL + 1))
fi

unset E2E_INFRA_BACKEND
e2e_cp_require_local_backend >/dev/null 2>&1
unset_backend_guard_rc=$?
if [ "$unset_backend_guard_rc" = "2" ]; then
  echo "PASS: standalone preflight rejects an unspecified backend"
  PASS=$((PASS + 1))
else
  echo "FAIL: unspecified backend preflight expected rc=2 observed=$unset_backend_guard_rc" >&2
  FAIL=$((FAIL + 1))
fi

e2e_cp_require_staging_origin "https://attacker.invalid" >/dev/null 2>&1
origin_guard_rc=$?
if [ "$origin_guard_rc" = "2" ] \
  && e2e_cp_require_staging_origin "https://staging-api.moleculesai.app"; then
  echo "PASS: standalone origin preflight pins the staging control plane"
  PASS=$((PASS + 1))
else
  echo "FAIL: standalone staging-origin preflight did not fail closed" >&2
  FAIL=$((FAIL + 1))
fi

e2e_cp_require_staging_origin "http://127.0.0.1:18080" >/dev/null 2>&1
loopback_default_rc=$?
E2E_CP_ALLOW_EPHEMERAL_LOOPBACK=1 \
  e2e_cp_require_staging_origin "http://127.0.0.1:18080" >/dev/null 2>&1
loopback_opt_in_rc=$?
E2E_CP_ALLOW_EPHEMERAL_LOOPBACK=1 \
  e2e_cp_require_staging_origin "https://attacker.invalid" >/dev/null 2>&1
opt_in_attacker_rc=$?
if [ "$loopback_default_rc" = "2" ] \
  && [ "$loopback_opt_in_rc" = "0" ] \
  && [ "$opt_in_attacker_rc" = "2" ]; then
  echo "PASS: explicit ephemeral opt-in allows only numeric loopback origin"
  PASS=$((PASS + 1))
else
  echo "FAIL: ephemeral loopback origin allowlist was not exact" >&2
  FAIL=$((FAIL + 1))
fi

echo "passed=$PASS failed=$FAIL"
[ "$FAIL" = "0" ]
