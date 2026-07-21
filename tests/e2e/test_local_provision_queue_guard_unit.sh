#!/usr/bin/env bash
# Offline behavior proof for the local-provision queued-response guard.
# No network, Docker, or provisioning: the real guard is sourced and driven
# through the mandatory-stub, advisory-MiniMax, and pollable response shapes.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
GUARD="$HERE/lib/a2a_queue_guard.sh"
DRIVER="$HERE/test_local_provision_lifecycle_e2e.sh"
PASS=0
FAIL=0

pass() { echo "PASS: $*"; PASS=$((PASS + 1)); }
fail_test() { echo "FAIL: $*" >&2; FAIL=$((FAIL + 1)); }

run_case() {
  local lane="$1" response="$2"
  set +e
  CASE_OUTPUT=$(LIFECYCLE_LLM="$lane" A2A_RESPONSE="$response" GUARD="$GUARD" bash -c '
    set -uo pipefail
    source "$GUARD"
    FAIL_COUNT=0
    infra_skip_advisory() {
      local reason="$1"
      if [ "$LIFECYCLE_LLM" != "minimax" ]; then
        return 0
      fi
      echo "SKIP:$reason"
      exit 0
    }
    fail() {
      echo "HARD_FAIL:$1"
      FAIL_COUNT=$((FAIL_COUNT + 1))
    }
    QUEUE_ID=$(printf "%s" "$A2A_RESPONSE" | a2a_queue_id_from_response 2>/dev/null || true)
    if require_a2a_queue_id "$QUEUE_ID"; then
      echo "POLL:$QUEUE_ID"
      exit 0
    fi
    echo "BLOCKED:fail_count=$FAIL_COUNT"
    exit 23
  ' 2>&1)
  CASE_RC=$?
  set -e
}

assert_case() {
  local label="$1" lane="$2" response="$3" expected_rc="$4" required="$5" forbidden="$6"
  run_case "$lane" "$response"
  if [ "$CASE_RC" != "$expected_rc" ]; then
    fail_test "$label rc=$CASE_RC, want $expected_rc; output=$CASE_OUTPUT"
    return
  fi
  if ! printf '%s' "$CASE_OUTPUT" | grep -qF "$required"; then
    fail_test "$label missing '$required'; output=$CASE_OUTPUT"
    return
  fi
  if [ -n "$forbidden" ] && printf '%s' "$CASE_OUTPUT" | grep -qF "$forbidden"; then
    fail_test "$label emitted forbidden '$forbidden'; output=$CASE_OUTPUT"
    return
  fi
  pass "$label"
}

if [ ! -r "$GUARD" ]; then
  echo "FAIL: real queue guard is missing: $GUARD" >&2
  exit 1
fi

VALID_QID="22222222-2222-4222-8222-222222222222"
assert_case "pollable queued response proceeds" "" "{\"queued\":true,\"queue_id\":\"$VALID_QID\"}" 0 "POLL:$VALID_QID" "HARD_FAIL"
assert_case "mandatory stub rejects missing queue_id before polling" "" '{"queued":true}' 23 "BLOCKED:fail_count=1" "POLL:"
assert_case "mandatory stub rejects null queue_id before polling" "" '{"queued":true,"queue_id":null}' 23 "BLOCKED:fail_count=1" "POLL:"
assert_case "mandatory stub rejects blank queue_id before polling" "" '{"queued":true,"queue_id":"   "}' 23 "BLOCKED:fail_count=1" "POLL:"
assert_case "mandatory stub rejects non-string queue_id before polling" "" '{"queued":true,"queue_id":42}' 23 "BLOCKED:fail_count=1" "POLL:"
assert_case "mandatory stub rejects non-UUID queue_id before polling" "" '{"queued":true,"queue_id":"queue-123"}' 23 "BLOCKED:fail_count=1" "POLL:"
assert_case "MiniMax advisory preserves the scoped infra skip" "minimax" '{"queued":true,"queue_id":null}' 0 "SKIP:a2a-queued-no-queue-id" "HARD_FAIL"

python3 - "$DRIVER" <<'PY'
from pathlib import Path
import sys

text = Path(sys.argv[1]).read_text(encoding="utf-8")
source = 'source "$(dirname "$0")/lib/a2a_queue_guard.sh"'
parse = 'a2a_queue_id_from_response'
call = 'if ! require_a2a_queue_id "$A2A_QID"; then'
poll = '"$BASE/workspaces/$WSID/a2a/queue/$A2A_QID"'
for required in (source, parse, call, poll):
    if required not in text:
        raise SystemExit(f"FAIL: local lifecycle driver is not wired to the real queue guard: {required}")
if not text.index(parse) < text.index(call) < text.index(poll):
    raise SystemExit("FAIL: local lifecycle driver can poll before parsing and validating queue_id")
print("PASS: local lifecycle driver parses and validates queue_id before constructing its poll URL")
PY
if [ "$?" -eq 0 ]; then
  PASS=$((PASS + 1))
else
  FAIL=$((FAIL + 1))
fi

echo "Results: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
