#!/usr/bin/env bash
# Offline anti-rot guard for the DELEGATION A2A POST's timeout budget in
# tests/e2e/test_staging_full_saas.sh. No network, no curl, no tenant.
#
# WHY THIS EXISTS
#
# `message/send` is synchronous: the delegation POST's response carries the
# CHILD workspace's finished reply (the step reads result.parts[0].text), so the
# call waits out a full agent turn on a workspace that was just created — cold
# adapter start, TLS to the LLM endpoint, first prompt, first token.
#
# The PARENT leg routes through a2a_send_or_poll_queue and overrides the timeout
# to 90s for exactly that reason. The DELEGATION leg is raw curl, deliberately,
# because it authenticates as the PARENT WORKSPACE token rather than the tenant
# admin — and in being hand-rolled it silently inherited CURL_COMMON's generic
# --max-time 30. That took a REQUIRED gate red twice on 2026-07-30:
#
#   Delegation A2A POST failed after 1 attempt(s) (curl_rc=28, http=000)
#     core#4961 job 879714 @ 08:59:54   core#4958 job 880955 @ 11:08:49
#
# curl_rc=28 is "operation timed out"; http=000 means no response ever arrived.
# The retry loop around the call is scoped to cold-start HTTP statuses, so a
# transport timeout matched no arm and fell through after ONE attempt.
#
# It is fixed as a TIMEOUT, not a retry: rc=28 is maybe-processed (the child may
# be mid-turn), so re-POSTing risks double-delivering the delegation. That is the
# same rule lib/workspace_create_retry.sh encodes as "curl timeout 28 → no
# retry" for the non-idempotent create.
#
# This guard asserts the override is PRESENT and NOT SMALLER than the parent
# leg's budget, so a future edit cannot quietly drop it back to 30s.
#
# Negative control: SENTINEL_BROKEN=1 points the assertions at a fixture that
# reproduces the pre-fix line, so every assertion below has a demonstrated fail
# arm (a test you have not seen fail proves nothing).
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
SAAS="$HERE/test_staging_full_saas.sh"
PARENT_LEG_BUDGET=90   # a2a_send_or_poll_queue's --max-time for the parent leg

E2E_TMPDIR=$(mktemp -d -t delegto-XXXXXX)
trap 'rm -rf "$E2E_TMPDIR"' EXIT INT TERM

if [ "${SENTINEL_BROKEN:-0}" = "1" ]; then
  # The pre-fix shape: the delegation curl with no --max-time override, so it
  # inherits CURL_COMMON's 30s. Every assertion must fail against this.
  SAAS="$E2E_TMPDIR/broken_full_saas.sh"
  cat > "$SAAS" <<'BROKEN'
    DELEG_CODE=$(curl "${CURL_COMMON[@]}" -X POST "$TENANT_URL/workspaces/$CHILD_ID/a2a" \
      -H "Authorization: Bearer $PARENT_WS_TOKEN" \
      -d "$DELEG_PAYLOAD"
BROKEN
fi

pass=0 fail=0
ok() { if eval "$2"; then echo "  PASS: $1"; pass=$((pass+1)); else echo "  FAIL: $1"; fail=$((fail+1)); fi; }

# The delegation POST is identified by its URL shape: it is the only a2a call
# that targets $CHILD_ID.
deleg_line() { grep -n 'workspaces/\$CHILD_ID/a2a' "$SAAS" | head -1 | cut -d: -f1; }

# The override may sit on the same physical line as `curl` or on the preceding
# continuation line, so read the whole logical command: from the `DELEG_CODE=$(`
# assignment through the first line that does not end in a backslash.
deleg_cmd() {
  local start; start=$(grep -n 'DELEG_CODE=\$(curl' "$SAAS" | head -1 | cut -d: -f1)
  [ -n "$start" ] || { printf ''; return; }
  awk -v s="$start" 'NR>=s { print; if ($0 !~ /\\$/) exit }' "$SAAS"
}

# Extract the numeric budget, resolving a ${VAR:-N} default if one is used.
deleg_budget() {
  deleg_cmd | tr ' ' '\n' | grep -A1 -- '--max-time' | tail -1 \
    | sed 's/.*:-//; s/}.*//; s/"//g' | tr -dc '0-9'
}

echo "── delegation A2A timeout budget ──"
ok "the delegation POST exists in the harness"        '[ -n "$(deleg_line)" ]'
ok "its curl command carries an explicit --max-time"  'deleg_cmd | grep -q -- "--max-time"'
B=$(deleg_budget)
ok "the budget parses as a number"                    '[ -n "$B" ]'
ok "budget is at least the parent leg's 90s"           '[ -n "$B" ] && [ "$B" -ge "$PARENT_LEG_BUDGET" ]'
ok "budget is not CURL_COMMON's generic 30s"           '[ -n "$B" ] && [ "$B" -ne 30 ]'

# The fix must NOT have become a retry-on-timeout: that would risk
# double-delivering a non-idempotent delegation. Assert the retry arm is still
# gated on HTTP status, not on the curl exit code.
if [ "${SENTINEL_BROKEN:-0}" != "1" ]; then
  echo "── the fix stayed a timeout fix, not a retry ──"
  ok "retry arm still keys on 502/503/504, not DELEG_RC" \
     'grep -q "DELEG_CODE. | grep -Eq .\\^(502|503|504)\\$" "$SAAS" || grep -Eq "502\\|503\\|504" "$SAAS"'
  ok "no retry-on-curl-exit-28 was introduced" \
     '! grep -Eq "DELEG_RC. *(=|-eq) *.?28" "$SAAS"'
fi

echo
echo "  passed=$pass failed=$fail"
[ "$fail" -eq 0 ] || exit 1
