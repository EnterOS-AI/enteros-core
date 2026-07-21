#!/usr/bin/env bash
# Unit test for ephemeral_cp_happy_path.sh's extra-scenario dispatch/gating.
#
# Pins the hardening from the #4406 follow-up (findings #2/#3):
#   * an UNKNOWN/typo'd scenario key (run_one_extra_scenario → reserved sentinel 97)
#     is a MISCONFIG that fails the gate UNCONDITIONALLY — even under
#     E2E_EPHEMERAL_EXTRA_ADVISORY=1 (a never-ran scenario must never read as an
#     advisory-suppressible green), and
#   * a non-empty value that tokenizes to ZERO scenarios (",", whitespace) is a
#     MISCONFIG too (not a silent green), and
#   * a scenario that genuinely RAN and FAILED — INCLUDING one that exits with the
#     concierge script's own code 2 (cleanup_org trap / env guards) — stays
#     advisory-suppressible and is NEVER misclassified as a never-ran misconfig.
#
# Sources the script (its dispatch is guarded so sourcing does NOT boot a CP) and
# stubs run_one_extra_scenario so nothing real is provisioned.
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# shellcheck source=ephemeral_cp_happy_path.sh disable=SC1091
source "$HERE/ephemeral_cp_happy_path.sh"

fails=0
pass() { echo "  PASS: $1"; }
failc() { echo "  FAIL: $1"; fails=$((fails + 1)); }

# Deterministic stub: 'ok_*' keys pass, 'bad_*' keys RAN-and-FAILED (rc 1),
# 'exit2_*' keys RAN-and-FAILED with the concierge script's own code 2 (its
# cleanup_org trap / env guards) — which must NOT be read as the unknown-key
# sentinel; every other key falls through to the real unknown-key arm (rc 97).
run_one_extra_scenario() {
  case "$1" in
    ok_*)    return 0 ;;
    bad_*)   return 1 ;;
    exit2_*) return 2 ;;
    *) echo "[stub][extra] unknown key '$1'" >&2; return 97 ;;
  esac
}

expect_gate() {  # <desc> <expected-rc> <advisory> <list>
  local desc="$1" want="$2" advisory="$3" list="$4" got
  EXTRA_MISCONFIG=0
  E2E_EPHEMERAL_EXTRA_ADVISORY="$advisory" E2E_EPHEMERAL_EXTRA_SCENARIOS="$list" \
    E2E_EPHEMERAL_EXTRA_GATING="" \
    gate_extra_scenarios >/dev/null 2>&1
  got=$?
  if [ "$got" = "$want" ]; then pass "$desc (gate rc=$got)"; else failc "$desc (want gate rc=$want, got $got)"; fi
}

# Same as expect_gate but with an explicit E2E_EPHEMERAL_EXTRA_GATING allowlist —
# the per-scenario graduation list. A gating-listed scenario's failure fails the
# gate REGARDLESS of the global advisory soak (this is the #4543 coverage-hole fix:
# peer_visibility + concierge_creates_workspace gate even while other extras soak).
expect_gate_g() {  # <desc> <expected-rc> <advisory> <gating-list> <scenario-list>
  local desc="$1" want="$2" advisory="$3" gating="$4" list="$5" got
  EXTRA_MISCONFIG=0
  E2E_EPHEMERAL_EXTRA_ADVISORY="$advisory" E2E_EPHEMERAL_EXTRA_GATING="$gating" \
    E2E_EPHEMERAL_EXTRA_SCENARIOS="$list" \
    gate_extra_scenarios >/dev/null 2>&1
  got=$?
  if [ "$got" = "$want" ]; then pass "$desc (gate rc=$got)"; else failc "$desc (want gate rc=$want, got $got)"; fi
}

echo "── extra-scenario gating unit ──"

# All good → gate passes.
expect_gate "all-pass, advisory off → gate 0" 0 0 "ok_concierge"
expect_gate "all-pass, advisory on  → gate 0" 0 1 "ok_a,ok_b"

# A ran-and-failed scenario: gates when advisory off, suppressed when advisory on.
expect_gate "ran-and-failed, advisory off → gate 1" 1 0 "bad_x"
expect_gate "ran-and-failed, advisory on  → gate 0 (suppressed)" 0 1 "bad_x"

# OVER-CORRECTION GUARD: a scenario that RAN and exited with the concierge script's
# own code 2 (cleanup_org trap / env guards) is a ran-and-failed, NOT a never-ran
# misconfig — so it stays advisory-suppressible, exactly like any other real red.
expect_gate "ran-and-failed exit-2, advisory off → gate 1" 1 0 "exit2_concierge"
expect_gate "ran-and-failed exit-2, advisory on  → gate 0 (suppressed, NOT misconfig)" 0 1 "exit2_concierge"

# UNKNOWN key (finding #2): misconfig — fails the gate even under advisory=1.
expect_gate "unknown key, advisory off → gate 1" 1 0 "typo_key"
expect_gate "unknown key, advisory on  → gate 1 (NOT suppressible)" 1 1 "typo_key"
expect_gate "unknown key alongside a pass, advisory on → gate 1" 1 1 "ok_a,typo_key"

# Zero-token list (finding #3): non-empty but no scenarios → misconfig, never silent green.
expect_gate "comma-only list, advisory on → gate 1" 1 1 ","
expect_gate "whitespace list, advisory on → gate 1" 1 1 "   "

# Empty list → truly nothing requested → gate passes (no extras is legal).
expect_gate "empty list → gate 0" 0 0 ""

# ── GRADUATION: E2E_EPHEMERAL_EXTRA_GATING (#4543 coverage-hole fix) ──────────
# A scenario named in E2E_EPHEMERAL_EXTRA_GATING GATES on failure even under the
# global advisory soak (E2E_EPHEMERAL_EXTRA_ADVISORY=1). This is the property that
# makes peer_visibility + concierge_creates_workspace merge-blocking again after
# #4543 moved their journeys onto this gate under the (suppressing) soak.

# THE INVARIANT: a graduated scenario that RAN and FAILED gates DESPITE the soak.
# NEGATIVE CONTROL: drop it from the gating list (as it was, advisory-only) and the
# SAME failure is suppressed to gate 0 — i.e. leaving these two advisory reopens the
# exact hole. The two asserts below are that negative control, side by side.
expect_gate_g "graduated scenario fails, soak on, IN gating list → gate 1 (blocks)" \
  1 1 "bad_peer_visibility" "bad_peer_visibility"
expect_gate   "same scenario fails, soak on, NOT in gating list → gate 0 (the #4543 hole)" \
  0 1 "bad_peer_visibility"

# A graduated scenario that PASSES does not gate (obviously), even with the soak on.
expect_gate_g "graduated scenario passes, soak on → gate 0" \
  0 1 "ok_concierge" "ok_concierge"

# MIXED, one run: a still-soaking red (not listed) is suppressed while a graduated
# red (listed) still blocks — the whole point of per-scenario graduation.
expect_gate_g "graduated red + soaking red together, soak on → gate 1 (graduated one blocks)" \
  1 1 "bad_peer_visibility" "bad_soaker,bad_peer_visibility"
# And with ONLY the still-soaking red present, the soak suppresses → gate 0.
expect_gate_g "only a soaking red (graduated one absent), soak on → gate 0" \
  0 1 "bad_peer_visibility" "bad_soaker"

# Two graduated scenarios (the real config shape: peer_visibility + concierge_creates_workspace).
expect_gate_g "both graduated, one fails, soak on → gate 1" \
  1 1 "bad_peer_visibility,bad_concierge_creates_workspace" "ok_x,bad_concierge_creates_workspace"

# A gating-listed scenario is still advisory-IRRELEVANT when the soak is OFF: it
# gates because the soak is off, listed or not — the list only MATTERS under soak.
expect_gate_g "gating list is a no-op when soak already off → gate 1" \
  1 0 "bad_peer_visibility" "bad_peer_visibility"

# Direct run_extra_scenarios contract: unknown key sets EXTRA_MISCONFIG even though
# its ran-and-failed count can be 0.
EXTRA_MISCONFIG=0
E2E_EPHEMERAL_EXTRA_SCENARIOS="typo_key" run_extra_scenarios >/dev/null 2>&1; rc=$?
if [ "$rc" = "0" ] && [ "${EXTRA_MISCONFIG}" = "1" ]; then
  pass "run_extra_scenarios: unknown key → failed-count 0 but EXTRA_MISCONFIG=1"
else
  failc "run_extra_scenarios: unknown key (failed-count=$rc EXTRA_MISCONFIG=${EXTRA_MISCONFIG})"
fi

# Over-correction guard, direct: a scenario that RAN and exited 2 must land in the
# failed-COUNT (advisory-suppressible), NOT flip EXTRA_MISCONFIG.
EXTRA_MISCONFIG=0
E2E_EPHEMERAL_EXTRA_SCENARIOS="exit2_x" run_extra_scenarios >/dev/null 2>&1; rc=$?
if [ "$rc" = "1" ] && [ "${EXTRA_MISCONFIG}" = "0" ]; then
  pass "run_extra_scenarios: ran-and-failed exit-2 → failed-count 1, EXTRA_MISCONFIG=0 (not misclassified as misconfig)"
else
  failc "run_extra_scenarios: exit-2 scenario misclassified (failed-count=$rc EXTRA_MISCONFIG=${EXTRA_MISCONFIG})"
fi

echo "──"
if [ "$fails" -eq 0 ]; then echo "✅ extra-scenario gating unit PASSED"; exit 0
else echo "❌ extra-scenario gating unit FAILED ($fails)"; exit 1; fi
