#!/usr/bin/env bash
# Offline unit test for lib/container_running_wait.sh (no Docker, no network).
#
# The whole value of this lib is that its outcomes are DISTINGUISHABLE — a
# container that came up late (pass), one that started and died (fail FAST), and
# one that never appeared (fail at the bound). A wait helper whose fail arms are
# never exercised is just a sleep, so each arm is asserted separately, including
# the probe COUNT: fast-fail must not burn the budget, and the bounded arm must
# not poll forever.
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=container_running_wait.sh
. "$HERE/container_running_wait.sh"

FAILED=0
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# Injected state reader: replays a staged state per successive probe, so no
# container (and no daemon) is needed. "-" stages an ABSENT container (docker
# prints nothing); past the end of the script the last state is held, so a
# scenario reads as "…and it stays that way". Probes are counted, because the
# bound and the fast-fail are claims about HOW MANY times we looked.
fake_state() {
  local n total line
  n=$(( $(cat "$TMP/probes" 2>/dev/null || echo 0) + 1 ))
  echo "$n" > "$TMP/probes"
  total=$(wc -l < "$TMP/states")
  [ "$n" -le "$total" ] || n="$total"
  line=$(sed -n "${n}p" "$TMP/states")
  [ "$line" = "-" ] && line=""
  printf '%s' "$line"
}
export CONTAINER_WAIT_STATE_CMD=fake_state

stage() {  # stage <state|-> [<state|-> ...]  — one state per successive probe
  : > "$TMP/states"
  local s
  for s in "$@"; do printf '%s\n' "$s" >> "$TMP/states"; done
  : > "$TMP/probes"
}
probes() {  # a freshly staged (truncated) counter reads as 0, not empty
  local n; n=$(cat "$TMP/probes" 2>/dev/null || true); printf '%s' "${n:-0}"
}

# Collapse wall-clock: the loop still counts its budget, tests stay instant.
sleep() { :; }

check() {  # $1 desc, $2 expected rc, $3 actual rc
  if [ "$2" = "$3" ]; then
    echo "PASS [rc=$2] $1"
  else
    echo "FAIL [want rc=$2, got rc=$3] $1"; FAILED=1
  fi
}
check_eq() {  # $1 desc, $2 expected, $3 actual
  if [ "$2" = "$3" ]; then
    echo "PASS [$2] $1"
  else
    echo "FAIL [want '$2', got '$3'] $1"; FAILED=1
  fi
}

# --- already running -> pass immediately, no soak ----------------------------
stage running
wait_container_running ws-x 60 2; check "running on first probe -> pass" 0 $?
check_eq "running pass costs exactly one probe (no residual soak)" 1 "$(probes)"
check_eq "last state reported" running "$CONTAINER_WAIT_LAST_STATE"

# --- THE RACE this exists for: online arrives before the container ------------
# Absent, then created, then running: the old assert (a single docker ps the
# instant CP said online) failed here. It must now WAIT and then PASS.
stage - - created running
wait_container_running ws-x 60 2; check "absent -> created -> running (the #4980 race) -> pass" 0 $?
check_eq "waited exactly until running was observed" 4 "$(probes)"
check_eq "last state reported" running "$CONTAINER_WAIT_LAST_STATE"

# --- a container that started and DIED fails FAST, not at the bound -----------
stage exited
wait_container_running ws-x 600 2; check "exited -> terminal, fail fast" 2 $?
check_eq "fail-fast costs one probe, not the 600s budget" 1 "$(probes)"
check_eq "last state reported" exited "$CONTAINER_WAIT_LAST_STATE"

stage dead
wait_container_running ws-x 600 2; check "dead -> terminal, fail fast" 2 $?
check_eq "fail-fast costs one probe" 1 "$(probes)"

# It stays reachable when the death happens DURING the wait, too.
stage - created exited
wait_container_running ws-x 600 2; check "came up then died mid-wait -> terminal" 2 $?
check_eq "stopped as soon as it died" 3 "$(probes)"

# --- never appears -> bounded fail with the OTHER outcome ---------------------
stage -
wait_container_running ws-x 10 2; check "never appears -> bounded fail (not terminal)" 1 $?
check_eq "polled the bound and stopped (10s / 2s + first probe)" 6 "$(probes)"
check_eq "absent last state is empty, and the caller can say so" "" "$CONTAINER_WAIT_LAST_STATE"

# `created` forever is a real failure too — but NOT the terminal one, because a
# container stuck in created never started and never died.
stage created
wait_container_running ws-x 10 2; check "stuck in 'created' -> bounded fail, never a false pass" 1 $?
check_eq "last state reported" created "$CONTAINER_WAIT_LAST_STATE"

# A flapping container is still coming up; only the bound may end it.
stage restarting
wait_container_running ws-x 4 2; check "restarting -> bounded fail, never a false pass" 1 $?

# The bound is honoured even when the poll interval overshoots it.
stage -
wait_container_running ws-x 3 5; check "poll > timeout still terminates" 1 $?
check_eq "final probe lands at the exact deadline" 2 "$(probes)"

# --- usage errors are refused, never silently treated as success -------------
stage running
wait_container_running "" 60 2;    check "missing container name -> usage error" 3 $?
wait_container_running ws-x 60 0;  check "zero poll -> usage error" 3 $?
wait_container_running ws-x abc 2; check "non-numeric timeout -> usage error" 3 $?
wait_container_running ws-x 60 ""; check "missing poll -> usage error" 3 $?
check_eq "usage errors probe nothing" 0 "$(probes)"

# --- an unreadable daemon must not be reported as a dead container ------------
unreadable() { return 1; }
stage -
CONTAINER_WAIT_STATE_CMD=unreadable
wait_container_running ws-x 4 2
check "docker CLI failure -> retryable, bounded fail (never 'terminal')" 1 $?
CONTAINER_WAIT_STATE_CMD=fake_state

# --- the caller actually uses it (a lib nothing calls proves nothing) ---------
CALLER="$HERE/../test_selfhost_concierge_schedules_e2e.sh"
grep -Fq 'wait_container_running "$CNAME"' "$CALLER" \
  || { echo "FAIL: test_selfhost_concierge_schedules_e2e.sh no longer waits via wait_container_running"; FAILED=1; }
grep -Fq 'source "$(dirname "$0")/lib/container_running_wait.sh"' "$CALLER" \
  || { echo "FAIL: caller does not source lib/container_running_wait.sh"; FAILED=1; }

CI_WORKFLOW="$HERE/../../../.gitea/workflows/ci.yml"
grep -Fq 'bash tests/e2e/lib/test_container_running_wait_unit.sh' "$CI_WORKFLOW" \
  || { echo "FAIL: ci.yml does not invoke this unit test — it would never run"; FAILED=1; }

if [ "$FAILED" = "0" ]; then echo "container_running_wait unit: OK"; else echo "container_running_wait unit: FAILURES"; fi
exit $FAILED
