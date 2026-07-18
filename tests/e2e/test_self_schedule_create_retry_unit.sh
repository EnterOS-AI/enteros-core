#!/usr/bin/env bash
# Offline behavior proof for 10f's capability-cache DB-to-volume create race.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HARNESS="$HERE/test_staging_full_saas.sh"
CI_WORKFLOW="$HERE/../../.gitea/workflows/ci.yml"
# shellcheck source=lib/self_schedule_create_retry.sh disable=SC1091
source "$HERE/lib/self_schedule_create_retry.sh"

passed=0
failed=0

assert_eq() {
  local description="$1" want="$2" got="$3"
  if [ "$got" = "$want" ]; then
    printf '  PASS: %s\n' "$description"
    passed=$((passed + 1))
  else
    printf '  FAIL: %s — want %q, got %q\n' "$description" "$want" "$got" >&2
    failed=$((failed + 1))
  fi
}

NAME="e2e-self-fire-unit"
UUID="01234567-89ab-4cde-8fab-0123456789ab"
INVOKES=0
PROBES=0
GRID_ON_PROBE=0
SLEEPS=""
LOGS=""
RESULT_CLASSES=()
RESULT_IDS=()

reset_case() {
  _SS_CREATE_TIMEOUT_SECS="$1"
  _SS_POLL_SECS="$2"
  GRID_ON_PROBE="$3"
  INVOKES=0
  PROBES=0
  SLEEPS=""
  LOGS=""
  RESULT_CLASSES=()
  RESULT_IDS=()
  _SS_CLASS=""
  _SS_ID=""
  _SS_CREATE_DETAIL=""
}

self_schedule_own_grid_has() {
  PROBES=$((PROBES + 1))
  [ "$GRID_ON_PROBE" -gt 0 ] && [ "$PROBES" -ge "$GRID_ON_PROBE" ]
}

self_schedule_invoke_create() {
  local index="$INVOKES"
  _SS_CLASS="${RESULT_CLASSES[$index]:-ok}"
  _SS_ID="${RESULT_IDS[$index]:-}"
  INVOKES=$((INVOKES + 1))
}

sleep() {
  SLEEPS="${SLEEPS}${SLEEPS:+,}$1"
}

log() {
  LOGS="${LOGS}${LOGS:+|}$*"
}

run_create() {
  self_schedule_create_until_own_grid '{"name":"e2e-self-fire-unit"}' "$NAME"
}

# The exact main-run failure: UUID + missing OWN grid proves a legacy DB
# misroute. Reinvoke after one bounded poll; stop once the volume create lands.
reset_case 20 10 4
RESULT_CLASSES=(ok ok)
RESULT_IDS=("$UUID" "$NAME")
run_create
rc=$?
assert_eq "UUID misroute retries into the volume" 0 "$rc"
assert_eq "UUID misroute invokes exactly twice" 2 "$INVOKES"
assert_eq "UUID retry waits for capability propagation" 10 "$SLEEPS"

# Grid evidence always wins. If the first DB response is followed by a visible
# OWN-grid entry before the next attempt, do not risk a duplicate volume create.
reset_case 20 10 3
RESULT_CLASSES=(ok)
RESULT_IDS=("$UUID")
run_create
rc=$?
assert_eq "late grid evidence completes the create" 0 "$rc"
assert_eq "late grid evidence prevents reinvocation" 1 "$INVOKES"

# id==name is the deterministic volume-routing witness. A delayed file read is
# poll-only: never re-create a volume entry that may already exist.
reset_case 20 10 3
RESULT_CLASSES=(ok)
RESULT_IDS=("$NAME")
run_create
rc=$?
assert_eq "volume id waits for delayed grid visibility" 0 "$rc"
assert_eq "volume id is never reinvoked" 1 "$INVOKES"

reset_case 20 10 0
RESULT_CLASSES=(ok)
RESULT_IDS=("$NAME")
run_create
rc=$?
assert_eq "missing grid after volume id fails bounded" 1 "$rc"
assert_eq "bounded volume wait still invokes once" 1 "$INVOKES"
assert_eq "bounded volume wait uses exact budget" "10,10" "$SLEEPS"

# A grid entry visible before the helper starts means there is nothing to create.
reset_case 20 10 1
run_create
rc=$?
assert_eq "pre-existing OWN-grid evidence succeeds" 0 "$rc"
assert_eq "pre-existing OWN-grid evidence skips create" 0 "$INVOKES"

# An unrecognized success id is not proof of a DB misroute. Fail closed after a
# bounded poll rather than guessing and duplicating a possibly-volume create.
reset_case 20 10 0
RESULT_CLASSES=(ok)
RESULT_IDS=("unexpected-id")
run_create
rc=$?
assert_eq "unclear id fails bounded" 1 "$rc"
assert_eq "unclear id is poll-only" 1 "$INVOKES"

# A real tool/auth error is never a capability-cache retry signal.
reset_case 20 10 0
RESULT_CLASSES=(tool-error)
RESULT_IDS=("")
run_create
rc=$?
assert_eq "tool error fails immediately" 2 "$rc"
assert_eq "tool error is never reinvoked" 1 "$INVOKES"
assert_eq "tool error does not sleep" "" "$SLEEPS"

# Persistent proven DB misroutes are retried only through the finite deadline.
reset_case 20 10 0
RESULT_CLASSES=(ok ok ok)
RESULT_IDS=("$UUID" "$UUID" "$UUID")
run_create
rc=$?
assert_eq "persistent DB misroute fails bounded" 1 "$rc"
assert_eq "persistent DB misroute has bounded attempts" 3 "$INVOKES"
assert_eq "persistent DB misroute has bounded sleeps" "10,10" "$SLEEPS"

if grep -Fq 'self_schedule_create_until_own_grid "$_SS_ARGS_EXPLICIT" "$_SS_SN"' "$HARNESS" \
    && grep -Fq 'self_schedule_create_until_own_grid "$_SS_ARGS_OMIT" "$_SS_SN_OMIT"' "$HARNESS"; then
  printf '%s\n' "  PASS: both positive 10f legs consume the retry helper"
  passed=$((passed + 1))
else
  printf '%s\n' "  FAIL: both positive 10f legs must consume the retry helper" >&2
  failed=$((failed + 1))
fi

foreign_block=$(sed -n '/LEG B NEG CONTROL:/,/LEG B NC: settling/p' "$HARNESS")
if printf '%s' "$foreign_block" | grep -Fq 'self_schedule_invoke_create "$_SS_ARGS_FOREIGN"' \
    && ! printf '%s' "$foreign_block" | grep -Fq 'self_schedule_create_until_own_grid'; then
  printf '%s\n' "  PASS: foreign-id negative control remains a single direct invocation"
  passed=$((passed + 1))
else
  printf '%s\n' "  FAIL: foreign-id negative control must not use the positive retry path" >&2
  failed=$((failed + 1))
fi

if grep -Fq 'bash tests/e2e/test_self_schedule_create_retry_unit.sh' "$CI_WORKFLOW"; then
  printf '%s\n' "  PASS: self-schedule retry regression is wired pre-pull"
  passed=$((passed + 1))
else
  printf '%s\n' "  FAIL: self-schedule retry regression is not wired into pull-request CI" >&2
  failed=$((failed + 1))
fi

printf 'self-schedule create retry: passed=%d failed=%d\n' "$passed" "$failed"
[ "$failed" -eq 0 ]
