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
#   A. transient 409 then 200  → the org is provably purged and the DELETE was
#      re-issued (>1 attempt). This is the fix itself.
#   B. persistent 409, STAGING → FAILS CLOSED with exit 4 and names the durable-
#      leak remediation. A retry that could never fail would be a worse defect
#      than the bug.
#   C. persistent 409, EPHEMERAL CP → warns, keeps the [cp-purge] diagnosis, and
#      does NOT fail the scenario. main 9fc3d764c established this carve-out:
#      that control plane dies with the job so nothing durable leaks, and
#      peer_visibility is in E2E_EPHEMERAL_EXTRA_GATING, so escalating turned an
#      already-printed "GATE PASSED" into a merge-block (core#4923 run 584794).
#
# B and C are the two arms of the SAME failure, split only by topology; both are
# driven so neither arm can rot into a no-op. Note E2E_CP_ALLOW_EPHEMERAL_LOOPBACK
# does double duty (ephemeral discriminator AND loopback-origin permit), which is
# why the staging arm is reached by pointing MOLECULE_CP_URL at the literal staging
# origin — see run_gate.
#
# Fail direction is negative-controlled, each arm independently (verified by
# running the neutralised variant): killing the helper's 409 retry reds A; removing
# the ephemeral carve-out reds C; removing the staging rc escalation reds B.
# It needs no docker, no network and no credentials — all egress is via curl, and
# curl is shimmed.

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

# NOTE: `sleep` is deliberately NOT shimmed. Every retry/poll budget below is
# turned down to single-digit seconds instead. A no-op sleep turns the helper's
# deadline loop into a hot spin that emits its retry line thousands of times,
# which is both a false picture of the real timing and enough output to make the
# assertions themselves flaky.

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

chmod +x "$SHIM/curl"

run_gate() {  # <scenario> <state-dir>
  local scenario="$1" state="$2" cp_mode="${3:-ephemeral}"
  local cp_url ephemeral
  # The teardown's rc-escalation arm is topology-dependent (see the MERGE NOTE in
  # the target): on an EPHEMERAL CP a teardown that cannot prove the purge warns
  # and preserves the scenario rc, because that control plane dies with the job;
  # on STAGING it escalates to 4, because a leaked tenant there is durable and has
  # caused a real provisioning outage. Both arms must be exercised or one of them
  # is a no-op that nothing would catch regressing.
  #
  # E2E_CP_ALLOW_EPHEMERAL_LOOPBACK does double duty: it is both that discriminator
  # AND what lets e2e_cp_require_staging_origin accept a numeric-loopback CP base.
  # So the staging arm cannot be reached through a 127.0.0.1 mock. It is reached by
  # pointing MOLECULE_CP_URL at the literal staging origin instead — the curl shim
  # matches on request PATH, not host, so the same mock CP serves both modes and no
  # real network call is made either way (verified: the shim's catch-all answers an
  # explicit 404 for anything unmocked, so a wrong request shape reds).
  case "$cp_mode" in
    ephemeral) cp_url="http://127.0.0.1:9099";                 ephemeral=1 ;;
    staging)   cp_url="https://staging-api.moleculesai.app";   ephemeral=0 ;;
    *) echo "FAIL: unknown cp_mode '$cp_mode'" >&2; exit 1 ;;
  esac
  mkdir -p "$state"
  PATH="$SHIM:$PATH" \
  FAKE_SCENARIO="$scenario" \
  FAKE_STATE_DIR="$state" \
  MOLECULE_CP_URL="$cp_url" \
  MOLECULE_TENANT_URL="http://127.0.0.1:9099" \
  MOLECULE_TENANT_ROUTE_DOMAIN="lvh.me" \
  MOLECULE_ADMIN_TOKEN="fake-admin-token" \
  E2E_INFRA_BACKEND="local-docker" \
  E2E_CP_ALLOW_EPHEMERAL_LOOPBACK="$ephemeral" \
  E2E_CP_PURGE_POLL_SECS=5 \
  E2E_CP_PURGE_POLL_INTERVAL=1 \
  E2E_CP_PURGE_DELETE_RETRY_SECS=3 \
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
out_a=$(tail -c 200000 "$A/stdout.txt" 2>/dev/null; tail -c 200000 "$A/stderr.txt" 2>/dev/null)
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
echo "── B. persistent 409 on STAGING — must FAIL CLOSED (a retry that can't fail is worse) ──"
B="$WORK/b"
rc=$(run_gate persistent-409 "$B" staging)
out_b=$(tail -c 200000 "$B/stdout.txt" 2>/dev/null; tail -c 200000 "$B/stderr.txt" 2>/dev/null)
check "unpurgeable org exits 4 (teardown left orphan resources)" "4" "$rc"
check_contains "the persistent 409 is REPORTED, not swallowed" \
  "DELETE still 409 (active lifecycle operation)" "$out_b"
check_contains "staging names the durable-leak remediation" \
  "sweep-stale-e2e-orgs retries on a later main push" "$out_b"
if [ -f "$B/purged" ]; then
  echo "  FAIL: mock CP purged the org in the persistent-409 case — bad negative control"
  FAIL=$((FAIL + 1))
else
  echo "  PASS: org genuinely never purged (negative control holds)"
  PASS=$((PASS + 1))
fi

# The other arm of the same failure. main 9fc3d764c deliberately stopped an
# unprovable purge from failing the scenario on the EPHEMERAL CP (that control
# plane is destroyed with the job, and peer_visibility is in
# E2E_EPHEMERAL_EXTRA_GATING, so escalating turned a printed "GATE PASSED" into a
# merge-block — core#4923 run 584794). Pin that the carve-out is scoped to the rc
# only: the [cp-purge] diagnosis must still reach the log, and it must NOT claim a
# sweeper that never sees an ephemeral org. Without this case the ephemeral arm is
# untested and a later edit could silently make it swallow the diagnosis too.
echo ""
echo "── C. same persistent 409 on the EPHEMERAL CP — warn, keep the diagnosis, do NOT fail ──"
C="$WORK/c"
rc=$(run_gate persistent-409 "$C" ephemeral)
out_c=$(tail -c 200000 "$C/stdout.txt" 2>/dev/null; tail -c 200000 "$C/stderr.txt" 2>/dev/null)
check "a disposable CP does not fail the passing scenario" "0" "$rc"
check_contains "still says it was not provably purged" "was not provably purged" "$out_c"
check_contains "names the ephemeral reason" "EPHEMERAL CP" "$out_c"
check_contains "the persistent 409 diagnosis survives the carve-out" \
  "DELETE still 409 (active lifecycle operation)" "$out_c"
check_not_contains "does NOT point at a sweeper that never sees this org" \
  "sweep-stale-e2e-orgs retries on a later main push" "$out_c"
if [ -f "$C/purged" ]; then
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
