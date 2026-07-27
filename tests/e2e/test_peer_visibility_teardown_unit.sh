#!/usr/bin/env bash
# Offline reproduction of the peer-visibility gate's teardown failure.
#
# WHAT THIS PINS
# --------------
# The e2e-ephemeral happy path reds on `peer_visibility` AFTER its own gate has
# already printed "GATE PASSED": the failure is entirely in teardown. The
# control plane fences org teardown behind claimOrgPurge, which answers an
# IMMEDIATE HTTP 409 "organization has an active lifecycle operation" while any
# org-scoped mutation lease is still open — and this gate deletes its org ~11s
# after creating it, while the CP's own asynchronous concierge/platform-agent
# workspace provision for that org is still in flight. A 409 writes no purge
# audit row, so a teardown that issues ONE fire-and-forget DELETE and then polls
# for the org to vanish can never converge: nothing re-issues the DELETE.
#
# Observed on main 3be1d6e8 (run 578856) and PR #4906 (run 578755). In that same
# 578755 job the concierge lane took the identical 409 (1.5ms) and RECOVERED,
# because it tears down through lib/cp_purge_receipt.sh, whose bounded 409 retry
# re-issued the DELETE — which then returned 200 in 2.1s.
#
# This test drives the REAL test_peer_visibility_mcp_staging.sh against a mocked
# curl that reproduces that exact CP behaviour, and asserts on the DEFAULT
# teardown path (no knob injected):
#
#   A. transient 409 then 200  → the org is provably purged, the script's own
#      exit code survives, and the DELETE was re-issued (>1 attempt).
#   B. persistent 409          → still FAILS CLOSED with exit 4 and says so.
#      A retry that could never fail would be a worse defect than the bug.
#
# Fail direction is negative-controlled: against the pre-fix single-shot
# teardown, case A exits 4 with the leak warning, so this file is RED before the
# fix and GREEN after. It needs no docker, no network and no credentials.

set -uo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
TARGET="$HERE/test_peer_visibility_mcp_staging.sh"
[ -f "$TARGET" ] || { echo "FAIL: $TARGET not found" >&2; exit 1; }

PASS=0
FAIL=0
check() {  # <desc> <expected> <actual>
  if [ "$3" = "$2" ]; then
    echo "  PASS: $1"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $1 (expected '$2', got '$3')"
    FAIL=$((FAIL + 1))
  fi
}
check_contains() {  # <desc> <needle> <haystack>
  if printf '%s' "$3" | grep -qF -- "$2"; then
    echo "  PASS: $1"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $1 (missing: $2)"
    FAIL=$((FAIL + 1))
  fi
}
check_not_contains() {  # <desc> <needle> <haystack>
  if printf '%s' "$3" | grep -qF -- "$2"; then
    echo "  FAIL: $1 (should NOT contain: $2)"
    FAIL=$((FAIL + 1))
  else
    echo "  PASS: $1"
    PASS=$((PASS + 1))
  fi
}

WORK=$(mktemp -d -t pv-teardown-unit-XXXXXX)
trap 'rm -rf "$WORK"' EXIT INT TERM

SHIM="$WORK/shim"
mkdir -p "$SHIM"

# `sleep` is shimmed to a no-op so the poll/retry budgets do not make this a
# two-minute unit test. It intentionally does NOT change any decision the
# scripts make — only wall-clock.
cat > "$SHIM/sleep" <<'FAKE_SLEEP'
#!/usr/bin/env bash
exit 0
FAKE_SLEEP

# Mock control plane. Serves only what this gate's diagnostic-only path and its
# teardown touch; anything else is an explicit 404 so a silently-changed request
# shape shows up as a failure rather than a false green.
cat > "$SHIM/curl" <<'FAKE_CURL'
#!/usr/bin/env bash
set -uo pipefail

STATE="${FAKE_STATE_DIR:?}"
ORG_ID="11111111-2222-4333-8444-555555555555"
PURGE_ID="99999999-8888-4777-8666-555555555555"

out=""
url=""
method="GET"
data=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o|--output)                out="$2"; shift 2 ;;
    -X|--request)               method="$2"; shift 2 ;;
    -d|--data|--data-raw)       data="$2"; shift 2 ;;
    -w|--write-out)             shift 2 ;;
    -H|--header)                shift 2 ;;
    -m|--max-time|--max-time=*) shift 2 ;;
    -A|--user-agent)            shift 2 ;;
    http://*|https://*)         url="$1"; shift ;;
    *)                          shift ;;
  esac
done

printf '%s %s\n' "$method" "$url" >> "$STATE/calls.log"

slug_file="$STATE/slug"
purged_file="$STATE/purged"
delete_count_file="$STATE/delete_count"

emit() {  # <http_code> <body>
  local code="$1" body="$2"
  if [ -n "$out" ]; then
    printf '%s' "$body" > "$out"
    printf '%s' "$code"
  else
    printf '%s' "$body"
  fi
  case "$code" in
    2*) exit 0 ;;
    *)  exit 0 ;;   # curl itself succeeded; the CALLER inspects the code
  esac
}

slug() { cat "$slug_file" 2>/dev/null || echo ""; }

case "$url" in
  */cp/admin/orgs)
    if [ "$method" = "POST" ]; then
      printf '%s' "$data" \
        | sed -n 's/.*"slug":"\([^"]*\)".*/\1/p' > "$slug_file"
      emit 201 "{\"id\":\"$ORG_ID\",\"slug\":\"$(slug)\"}"
    fi
    emit 200 "{\"orgs\":[]}"
    ;;
  */cp/admin/orgs\?*)
    if [ -f "$purged_file" ]; then
      emit 200 '{"orgs":[]}'
    fi
    emit 200 "{\"orgs\":[{\"slug\":\"$(slug)\",\"instance_status\":\"running\"}]}"
    ;;
  */cp/admin/orgs/*/admin-token)
    emit 200 '{"admin_token":"tenant-admin-token"}'
    ;;
  */cp/admin/tenants/*/boot-events\?*)
    if [ -f "$purged_file" ]; then
      emit 404 "{\"error\":\"org not found\",\"slug\":\"$(slug)\"}"
    fi
    emit 200 "{\"slug\":\"$(slug)\",\"org_id\":\"$ORG_ID\"}"
    ;;
  */cp/admin/tenants/*)
    if [ "$method" != "DELETE" ]; then
      emit 404 '{"error":"unexpected method"}'
    fi
    n=$(( $(cat "$delete_count_file" 2>/dev/null || echo 0) + 1 ))
    printf '%s' "$n" > "$delete_count_file"
    # The CP's org-lifecycle fence: an immediate 409, no purge audit row.
    if [ "${FAKE_SCENARIO:?}" = "persistent-409" ] || [ "$n" -le 1 ]; then
      emit 409 '{"error":"organization has an active lifecycle operation"}'
    fi
    : > "$purged_file"
    emit 200 "{\"deleted\":true,\"slug\":\"$(slug)\",\"org_id\":\"$ORG_ID\",\"purge_id\":\"$PURGE_ID\"}"
    ;;
  */cp/admin/purges\?*)
    if [ -f "$purged_file" ]; then
      emit 200 "{\"purges\":[{\"id\":\"$PURGE_ID\",\"org_slug\":\"$(slug)\",\"org_id\":\"$ORG_ID\",\"status\":\"completed\",\"last_step\":\"completed\",\"completed_at\":\"2026-07-27T09:00:00+00:00\"}]}"
    fi
    emit 200 '{"purges":[]}'
    ;;
  */org/identity)
    emit 200 '{"org":"ready"}'
    ;;
  */buildinfo)
    exit 22
    ;;
  */workspaces)
    n=$(( $(cat "$STATE/ws_count" 2>/dev/null || echo 0) + 1 ))
    printf '%s' "$n" > "$STATE/ws_count"
    emit 201 "{\"id\":\"aaaaaaaa-bbbb-4ccc-8ddd-00000000000$n\",\"auth_token\":\"ws-token-$n\"}"
    ;;
  *)
    emit 404 '{"error":"unmocked request"}'
    ;;
esac
FAKE_CURL

chmod +x "$SHIM/curl" "$SHIM/sleep"

run_gate() {  # <scenario> <state-dir>
  local scenario="$1" state="$2"
  mkdir -p "$state"
  PATH="$SHIM:$PATH" \
  FAKE_SCENARIO="$scenario" \
  FAKE_STATE_DIR="$state" \
  MOLECULE_CP_URL="http://127.0.0.1:9099" \
  MOLECULE_TENANT_URL="http://127.0.0.1:9099" \
  MOLECULE_TENANT_ROUTE_DOMAIN="lvh.me" \
  MOLECULE_ADMIN_TOKEN="fake-admin-token" \
  E2E_INFRA_BACKEND="local-docker" \
  E2E_CP_ALLOW_EPHEMERAL_LOOPBACK=1 \
  E2E_CP_PURGE_POLL_SECS=10 \
  E2E_CP_PURGE_POLL_INTERVAL=1 \
  E2E_CP_PURGE_DELETE_RETRY_SECS=10 \
  PV_PROVISION_MODE="external" \
  PV_RUNTIMES="hermes" \
  PV_TOKEN_DIAGNOSTIC_ONLY=1 \
  E2E_RUN_ID="tdunit" \
    bash "$TARGET" > "$state/stdout.txt" 2> "$state/stderr.txt"
  echo $?
}

echo "── A. transient 409 then 200 — the CP's own in-flight lifecycle op drains ──"
A="$WORK/a"
rc=$(run_gate transient-409 "$A")
out_a=$(cat "$A/stdout.txt" "$A/stderr.txt" 2>/dev/null)
deletes_a=$(grep -c '^DELETE ' "$A/calls.log" 2>/dev/null || echo 0)
check "the gate's own exit code survives teardown" "0" "$rc"
check_not_contains "no leak warning" "was not provably purged" "$out_a"
check_not_contains "no still-present warning" "still present after" "$out_a"
check_contains "teardown reports the purge" "purged" "$out_a"
if [ "$deletes_a" -gt 1 ]; then
  echo "  PASS: DELETE was re-issued after the 409 (${deletes_a} attempts)"
  PASS=$((PASS + 1))
else
  echo "  FAIL: DELETE was issued ${deletes_a}x — a 409 that is never retried leaks the tenant"
  FAIL=$((FAIL + 1))
fi
if [ -f "$A/purged" ]; then
  echo "  PASS: the org was actually purged on the mock CP (no leaked tenant)"
  PASS=$((PASS + 1))
else
  echo "  FAIL: the org was NEVER purged — this is the leak the gate reds on"
  FAIL=$((FAIL + 1))
fi

echo ""
echo "── B. persistent 409 — must still FAIL CLOSED (a retry that can't fail is worse) ──"
B="$WORK/b"
rc=$(run_gate persistent-409 "$B")
out_b=$(cat "$B/stdout.txt" "$B/stderr.txt" 2>/dev/null)
check "unpurgeable org exits 4 (teardown left orphan resources)" "4" "$rc"
check_contains "the persistent 409 is REPORTED, not swallowed" "409" "$out_b"
if [ -f "$B/purged" ]; then
  echo "  FAIL: mock CP purged the org in the persistent-409 case — bad negative control"
  FAIL=$((FAIL + 1))
else
  echo "  PASS: org genuinely never purged (negative control holds)"
  PASS=$((PASS + 1))
fi

echo ""
echo "── RESULT: $PASS passed, $FAIL failed ──"
[ "$FAIL" -eq 0 ] || exit 1
exit 0
