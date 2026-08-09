#!/usr/bin/env bash
# Offline unit test for lib/trigger_daemon_wait.sh (no Docker, no network).
#
# The three outcomes this lib exists to distinguish (evidence / backstop-with-a
# live daemon / wedged daemon) are the whole point, so each is asserted
# separately, including the boundary either side of the stale threshold. A wait
# helper whose fail-fast arm is never exercised is just a sleep.
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=trigger_daemon_wait.sh
. "$HERE/trigger_daemon_wait.sh"

FAILED=0
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# Injected reader: prints whatever the scenario staged, so no container needed.
fake_health() { cat "$TMP/health.json" 2>/dev/null; }
export TRIGGER_DAEMON_HEALTH_CMD=fake_health

stage_health() { # $1 = age seconds, or "none" for no heartbeat at all
  if [ "$1" = "none" ]; then rm -f "$TMP/health.json"; return; fi
  AGE="$1" OUT="$TMP/health.json" python3 -c '
import datetime as dt, json, os
ts = dt.datetime.now(dt.timezone.utc) - dt.timedelta(seconds=int(os.environ["AGE"]))
json.dump({"armed": 1, "errors": {}, "last_tick": ts.isoformat()}, open(os.environ["OUT"], "w"))
'
}

# Collapse wall-clock: the loop still counts its budget, tests stay instant.
sleep() { :; }

probe_hit()  { return 0; }
probe_never() { return 1; }

check() { # $1 desc, $2 expected rc, $3 actual rc
  if [ "$2" = "$3" ]; then
    echo "PASS [rc=$2] $1"
  else
    echo "FAIL [want rc=$2, got rc=$3] $1"; FAILED=1
  fi
}

# --- evidence wins immediately, even if the heartbeat looks frozen -----------
stage_health 5
trigger_daemon_wait c 600 10 60 probe_hit; check "evidence present -> pass" 0 $?

stage_health 4000
trigger_daemon_wait c 600 10 60 probe_hit
check "evidence present, heartbeat frozen -> STILL pass (evidence beats liveness)" 0 $?

# --- wedged daemon fails fast ------------------------------------------------
stage_health 400
trigger_daemon_wait c 600 10 60 probe_never; check "frozen heartbeat, no evidence -> fail fast" 2 $?
[ -n "$TRIGGER_DAEMON_LAST_AGE" ] || { echo "FAIL: TRIGGER_DAEMON_LAST_AGE not reported"; FAILED=1; }

stage_health 61
trigger_daemon_wait c 600 10 60 probe_never; check "age 61s (just over threshold) -> fail fast" 2 $?

# --- live daemon that simply has not fired yet -> backstop, NOT a wedge ------
stage_health 59
trigger_daemon_wait c 60 10 60 probe_never; check "age 59s (just under threshold) -> backstop" 1 $?

stage_health 5
trigger_daemon_wait c 60 10 60 probe_never; check "fresh heartbeat, no evidence -> backstop" 1 $?

# --- an unreadable/absent heartbeat must NOT be reported as a wedged daemon --
stage_health none
trigger_daemon_wait c 60 10 60 probe_never; check "no heartbeat file -> backstop, never 'wedged'" 1 $?

printf 'not json' > "$TMP/health.json"
trigger_daemon_wait c 60 10 60 probe_never; check "corrupt heartbeat -> backstop, never 'wedged'" 1 $?

python3 -c 'import json,os; json.dump({"armed":1,"errors":{},"last_tick":None}, open(os.environ["OUT"],"w"))' OUT="$TMP/health.json" 2>/dev/null \
  || OUT="$TMP/health.json" python3 -c 'import json,os; json.dump({"armed":1,"errors":{},"last_tick":None}, open(os.environ["OUT"],"w"))'
trigger_daemon_wait c 60 10 60 probe_never; check "null last_tick (pre-first-tick) -> backstop" 1 $?

# --- usage errors are refused, never silently treated as success -------------
trigger_daemon_wait "" 60 10 60 probe_never;  check "missing container -> usage error" 3 $?
trigger_daemon_wait c 60 0 60 probe_never;    check "zero poll -> usage error" 3 $?
trigger_daemon_wait c abc 10 60 probe_never;  check "non-numeric backstop -> usage error" 3 $?
trigger_daemon_wait c 60 10 60 "";            check "missing probe -> usage error" 3 $?

# --- derived thresholds ------------------------------------------------------
[ "$(trigger_daemon_stale_secs 10)" = "60" ]  || { echo "FAIL: stale(10) != 60"; FAILED=1; }
[ "$(trigger_daemon_stale_secs 30)" = "180" ] || { echo "FAIL: stale(30) != 180"; FAILED=1; }
[ "$(trigger_daemon_stale_secs 1)" = "60" ]   || { echo "FAIL: stale floor not 60"; FAILED=1; }

# The backstop MUST exceed the delivery watchdog — the old 360s-vs-600s
# inversion is the bug this guards against.
[ "$(TRIGGER_DAEMON_WATCHDOG_SECS=600 trigger_daemon_backstop_secs)" = "1800" ] \
  || { echo "FAIL: backstop(600) != 1800"; FAILED=1; }
wd=600; bs=$(TRIGGER_DAEMON_WATCHDOG_SECS=$wd trigger_daemon_backstop_secs)
[ "$bs" -gt "$wd" ] || { echo "FAIL: backstop $bs not greater than watchdog $wd"; FAILED=1; }

# The watchdog is the SSOT the inequality is measured against, so it has to be
# readable on its own and has to survive garbage without silently becoming 0
# (a 0 watchdog makes EVERY backstop "greater than" it and the guard vacuous).
[ "$(trigger_daemon_watchdog_secs)" = "600" ] || { echo "FAIL: watchdog default != 600"; FAILED=1; }
[ "$(TRIGGER_DAEMON_WATCHDOG_SECS=900 trigger_daemon_watchdog_secs)" = "900" ] || { echo "FAIL: watchdog override ignored"; FAILED=1; }
[ "$(TRIGGER_DAEMON_WATCHDOG_SECS=0 trigger_daemon_watchdog_secs)" = "600" ]   || { echo "FAIL: watchdog 0 not defaulted"; FAILED=1; }
[ "$(TRIGGER_DAEMON_WATCHDOG_SECS=abc trigger_daemon_watchdog_secs)" = "600" ] || { echo "FAIL: watchdog garbage not defaulted"; FAILED=1; }

# --- the watchdog has a FLOOR ------------------------------------------------
# 0 and garbage were already defaulted; a positive-but-absurd value was not.
# TRIGGER_DAEMON_WATCHDOG_SECS=1 derived a 3s backstop, made the RELATIVE
# refusal wave through any positive override at all, and — now that this number
# is the bound actually written onto the workspace — would tell the daemon to
# abandon every delivery after one second. All three consequences are asserted
# below, not just the clamp: a floor nothing downstream reads is not a floor.
[ "$(TRIGGER_DAEMON_WATCHDOG_SECS=1 trigger_daemon_watchdog_secs)" = "60" ]  || { echo "FAIL: watchdog 1 not floored to 60"; FAILED=1; }
[ "$(TRIGGER_DAEMON_WATCHDOG_SECS=59 trigger_daemon_watchdog_secs)" = "60" ] || { echo "FAIL: watchdog 59 not floored to 60"; FAILED=1; }
[ "$(TRIGGER_DAEMON_WATCHDOG_SECS=60 trigger_daemon_watchdog_secs)" = "60" ] || { echo "FAIL: watchdog 60 (AT the floor) altered"; FAILED=1; }
[ "$(TRIGGER_DAEMON_WATCHDOG_SECS=61 trigger_daemon_watchdog_secs)" = "61" ] || { echo "FAIL: watchdog 61 (just over the floor) clamped"; FAILED=1; }
# The consequence that made the hole exploitable: the refusal is a comparison
# against the watchdog, so a declared `1` used to wave through any positive
# override at all. A 30s backstop against a declared 1s watchdog must now be
# REFUSED, because the bound it is really running against is the 60s floor.
if out=$(TRIGGER_DAEMON_WATCHDOG_SECS=1 trigger_daemon_backstop_resolve 30); then
  echo "FAIL: override 30 HONOURED ('$out') against a declared 1s watchdog — the floor is not being applied to the refusal"; FAILED=1
else
  echo "PASS [refused] override 30 against a declared 1s watchdog (measured against the 60s floor)"
fi
# ...and the derived backstop can no longer be 3s.
[ "$(TRIGGER_DAEMON_WATCHDOG_SECS=1 trigger_daemon_backstop_secs)" = "180" ] || { echo "FAIL: watchdog 1 derives a backstop other than 3x the 60s floor"; FAILED=1; }
# The floored value is what gets INJECTED too, or the workspace would run a
# different bound from the one the backstop was derived against.
[ "$(TRIGGER_DAEMON_WATCHDOG_SECS=1 trigger_daemon_delivery_cap_env | grep -c '=60$')" = "2" ] \
  || { echo "FAIL: the injected caps do not carry the floored value"; FAILED=1; }

# --- the OVERRIDE is held to the same inequality -----------------------------
# Deriving correctly is not the guard. Every call site spells its knob
# `${OVERRIDE:-$(derive)}`, so an override REPLACES the derivation and any
# literal wins outright — which is how 10g's DELIVER leg ran a bare 210 against
# a 600s watchdog from #4568 onward. These are the both-directions mutation: the
# 210 case must be REFUSED, and a legitimately larger value must still be taken.
resolve_check() { # $1 desc, $2 want-stdout ("" = expect refusal), $3 want-rc, $4 override, [$5 watchdog]
  local out rc
  out=$(TRIGGER_DAEMON_WATCHDOG_SECS="${5:-600}" trigger_daemon_backstop_resolve "$4") && rc=0 || rc=$?
  if [ "$rc" = "$3" ] && [ "$out" = "$2" ]; then
    echo "PASS [rc=$rc out='${out}'] $1"
  else
    echo "FAIL [want rc=$3 out='$2', got rc=$rc out='${out}'] $1"; FAILED=1
  fi
}

# REFUSED — below / at the watchdog. `210` is the exact literal 10g shipped.
resolve_check "override 210 vs 600s watchdog (the 10g DELIVER literal) -> REFUSED" "" 3 210
resolve_check "override 360 vs 600s watchdog (the old fire budget)     -> REFUSED" "" 3 360
resolve_check "override 599 (one under)                                -> REFUSED" "" 3 599
resolve_check "override 600 EQUAL to the watchdog                      -> REFUSED" "" 3 600
resolve_check "override 0                                              -> REFUSED" "" 3 0
resolve_check "override non-numeric                                    -> REFUSED" "" 3 abc
resolve_check "override negative                                       -> REFUSED" "" 3 "-300"
# HONOURED — strictly above the watchdog, and the empty case still derives 3x.
resolve_check "override 601 (one over)      -> honoured"        "601"  0 601
resolve_check "override 1800                -> honoured"        "1800" 0 1800
resolve_check "no override                  -> derives 3x"      "1800" 0 ""
# The bar tracks the watchdog, it is not a hardcoded 600: the SAME 1000 flips
# verdict when the watchdog moves. Without this a future watchdog bump would
# leave the guard silently checking the wrong number.
resolve_check "override 1000 vs 900s watchdog  -> honoured"     "1000" 0 1000 900
resolve_check "override 1000 vs 1200s watchdog -> REFUSED"      ""     3 1000 1200
resolve_check "no override vs 900s watchdog    -> derives 2700" "2700" 0 ""   900

# --- the harness must ROUTE its knobs through the resolver -------------------
# The contract above is unenforceable if a call site bypasses it, and bypassing
# it is precisely what happened: a bare literal default reads as deliberate,
# parses, runs, and is never compared to anything. A backstop knob defaulting to
# a NUMBER is therefore the defect signature, independent of which number.
HARNESS="$HERE/../test_staging_full_saas.sh"
if [ ! -f "$HARNESS" ]; then
  echo "FAIL: harness not found at $HARNESS (cannot check backstop call sites)"; FAILED=1
else
  for knob in E2E_SCHEDULER_TIMEOUT_SECS E2E_SCHEDULE_DELIVER_TIMEOUT_SECS E2E_SCHEDULE_DELIVER_FIRE_TIMEOUT_SECS; do
    if grep -Eq "\\\$\{$knob:-[0-9]" "$HARNESS"; then
      echo "FAIL: $knob defaults to a BARE LITERAL in test_staging_full_saas.sh — a scheduler backstop must resolve through trigger_daemon_backstop_resolve so it is checked against the watchdog"; FAILED=1
    else
      echo "PASS [no bare literal] $knob"
    fi
    if grep -q "$knob" "$HARNESS" && ! grep -q "e2e_scheduler_backstop_secs" "$HARNESS"; then
      echo "FAIL: $knob is read but the harness never calls e2e_scheduler_backstop_secs"; FAILED=1
    fi
  done
fi

# --- the bound is CONFIGURED, so the inequality is about a REAL number -------
#
# The backstop exceeding "the watchdog" was true against a number nobody
# enforced: 600 mirrored MOLECULE_TRIGGER_DELIVERY_WATCHDOG_SECONDS, retired at
# scheduler v0.2.2, while the pinned v0.2.3 abandons an unattributable delivery
# at 3600. Rather than grow the backstop to 10800 inside a 75-minute job, the
# harness now WRITES the bound onto the workspace. These pin that contract.
cap_env=$(trigger_daemon_delivery_cap_env)
cap_secs=$(trigger_daemon_watchdog_secs)

# BOTH keys, or the fix is cosmetic. classify_delivery_liveness takes the
# not-attributable branch on every fresh e2e workspace, and there
# _reported_absolute_cap PREFERS the cap the runtime's snapshot carries over the
# daemon's own env ceiling — so shipping only the daemon key leaves that branch
# at the runtime's 3600s default while every derived number claims otherwise.
for k in MOLECULE_TRIGGER_DELIVERY_ABSOLUTE_CAP_SECONDS MOLECULE_MAX_TURN_SECONDS; do
  if printf '%s\n' "$cap_env" | grep -qx "$k=$cap_secs"; then
    echo "PASS [$k=$cap_secs] injected cap key"
  else
    echo "FAIL: trigger_daemon_delivery_cap_env does not emit $k=$cap_secs (got: $(printf '%s' "$cap_env" | tr '\n' ' '))"; FAILED=1
  fi
done
[ "$(printf '%s\n' "$cap_env" | grep -c .)" = "2" ] \
  || { echo "FAIL: trigger_daemon_delivery_cap_env emitted something other than exactly 2 keys"; FAILED=1; }

# ONE number: the injected caps and the derived backstop must move together. A
# second literal anywhere in that chain is how the 210 survived four months.
for wd in 60 120 300 600 900; do
  got_cap=$(TRIGGER_DAEMON_WATCHDOG_SECS=$wd trigger_daemon_delivery_cap_env | sed -n 's/^MOLECULE_MAX_TURN_SECONDS=//p')
  got_bs=$(TRIGGER_DAEMON_WATCHDOG_SECS=$wd trigger_daemon_backstop_secs)
  if [ "$got_cap" = "$wd" ] && [ "$got_bs" = "$((wd * 3))" ] && [ "$got_bs" -gt "$got_cap" ]; then
    echo "PASS [cap=$got_cap backstop=$got_bs] backstop STRICTLY exceeds the injected cap"
  else
    echo "FAIL: watchdog=$wd gave cap=$got_cap backstop=$got_bs — the derived backstop must be 3x the INJECTED cap and strictly greater than it"; FAILED=1
  fi
done

# The other direction: a backstop at or below the cap the workspace is actually
# running must be refused. `1800` is today's derived value and `3600` is the
# scheduler default this change exists to stop measuring against — against a
# workspace configured at 3600 both are unusable, and that must be visible.
resolve_check "override 1800 vs a 3600s configured cap -> REFUSED" "" 3 1800 3600
resolve_check "override 3600 EQUAL to the configured cap -> REFUSED" "" 3 3600 3600
resolve_check "override 1800 vs the 600s configured cap -> honoured" "1800" 0 1800 600

# --- the reading this is all derived from is PINNED to a scheduler version ---
# Every claim above (which env names exist, that the runtime's reported cap
# wins, that there are four dispositions) was read at one tag. A repin can
# invalidate all of it silently, so core's live pin is compared against the tag
# recorded in the lib.
PIN_FILE="$HERE/../../../workspace-server/internal/handlers/plugin_registry_test.go"
VALIDATED=$(trigger_daemon_scheduler_version_validated)
if [ ! -f "$PIN_FILE" ]; then
  echo "FAIL: cannot find the scheduler pin at $PIN_FILE — the version guard is not actually checking anything"; FAILED=1
else
  PINNED=$(sed -n 's|.*molecule-ai-plugin-scheduler#\(v[0-9][0-9.]*\).*|\1|p' "$PIN_FILE" | tail -1)
  if [ -z "$PINNED" ]; then
    echo "FAIL: no molecule-ai-plugin-scheduler pin found in $PIN_FILE"; FAILED=1
  elif [ "$PINNED" = "$VALIDATED" ]; then
    echo "PASS [scheduler $PINNED] the configured cap was read at the version core pins"
  else
    echo "FAIL: core pins molecule-ai-plugin-scheduler $PINNED but lib/trigger_daemon_wait.sh was validated against $VALIDATED — re-read scheduler.py's DEFAULT_DELIVERY_ABSOLUTE_CAP_SECONDS / classify_delivery_liveness and molecule_runtime/turn_lease.py's cap resolution at $PINNED, then update trigger_daemon_scheduler_version_validated"; FAILED=1
  fi
fi

# --- the harness must ROUTE the injection through the lib --------------------
# Same reasoning as the backstop call sites: a literal cap typed at the
# provision site parses, runs, reads as deliberate and is compared to nothing.
if [ ! -f "$HARNESS" ]; then
  echo "FAIL: harness not found at $HARNESS (cannot check the cap injection)"; FAILED=1
else
  if grep -q "trigger_daemon_delivery_cap_env" "$HARNESS"; then
    echo "PASS [routed] the harness injects the delivery cap through the lib"
  else
    echo "FAIL: test_staging_full_saas.sh never calls trigger_daemon_delivery_cap_env — the workspace runs the scheduler's 3600s default and every derived backstop is measured against a number nothing enforces"; FAILED=1
  fi
  for k in MOLECULE_TRIGGER_DELIVERY_ABSOLUTE_CAP_SECONDS MOLECULE_MAX_TURN_SECONDS; do
    # The quote/bracket run matters: the provision site builds the env in a
    # python heredoc, so the literal shape to catch is `s['KEY'] = '3600'`, not
    # just `KEY=3600`. A pattern that only handled the shell shape read clean
    # against the python one — checked by mutation, not by eye.
    # ${k} braced, not $k: bare `$k[` reads as an array expansion (SC1087).
    if grep -Eq "${k}['\"]*\]?[[:space:]]*[=:][[:space:]]*['\"]?[0-9]" "$HARNESS"; then
      echo "FAIL: $k is assigned a BARE LITERAL in test_staging_full_saas.sh — it must come from trigger_daemon_delivery_cap_env so it cannot drift from the backstop derived against it"; FAILED=1
    else
      echo "PASS [no bare literal] $k"
    fi
  done
  # The refusal message is what an operator reads at the exact moment they are
  # about to act, so it may not send them after a knob that does not exist. Both
  # the header and the message used to say to mirror/retune the daemon's
  # delivery timeout; scheduler v0.2.2 removed it, and the bound is now one this
  # harness SETS. Wrong remediation is worse than none.
  if grep -Eqi "retuned daemon|retune the daemon|mirror(ing)? a retuned" "$HARNESS"; then
    echo "FAIL: test_staging_full_saas.sh still tells the operator to mirror/retune the daemon's delivery watchdog — scheduler v0.2.2 retired that knob; the bound is injected by this harness"; FAILED=1
  else
    echo "PASS [no stale remediation] the harness does not point operators at the retired daemon knob"
  fi
fi

# --- grid-landing confirm bound (core routing, NOT daemon liveness) ----------
# The grid write is already acked by core's 201 before the first probe, so this
# bound exists only to absorb docker-exec observation latency. Its CEILING is the
# load-bearing part: the poll is operator-tunable, so without a cap a raised poll
# would silently restore the multi-minute soak this replaced.
[ "$(schedule_grid_confirm_secs 10)" = "30" ] || { echo "FAIL: grid-confirm(10) != 30"; FAILED=1; }
[ "$(schedule_grid_confirm_secs 20)" = "60" ] || { echo "FAIL: grid-confirm(20) != 60"; FAILED=1; }
[ "$(schedule_grid_confirm_secs 1)"  = "30" ] || { echo "FAIL: grid-confirm floor not 30"; FAILED=1; }
[ "$(schedule_grid_confirm_secs 600)" = "120" ] || { echo "FAIL: grid-confirm ceiling not 120"; FAILED=1; }
[ "$(schedule_grid_confirm_secs abc)" = "30" ] || { echo "FAIL: grid-confirm non-numeric poll not defaulted"; FAILED=1; }
[ "$(schedule_grid_confirm_secs)"     = "30" ] || { echo "FAIL: grid-confirm missing poll not defaulted"; FAILED=1; }

# It must stay STRICTLY SHORTER than the fire backstop it replaced at the 10g
# grid step — that inequality IS the fix. A refactor that lets the two converge
# puts a 30-minute wait back on a deterministic routing contradiction.
gc=$(schedule_grid_confirm_secs 10); fb=$(TRIGGER_DAEMON_WATCHDOG_SECS=600 trigger_daemon_backstop_secs)
[ "$gc" -lt "$fb" ] || { echo "FAIL: grid-confirm $gc not shorter than fire backstop $fb"; FAILED=1; }
gc=$(schedule_grid_confirm_secs 600)
[ "$gc" -lt "$fb" ] || { echo "FAIL: grid-confirm $gc (max poll) not shorter than fire backstop $fb"; FAILED=1; }

# --- REAL wall-clock: raising the backstop must not lengthen the happy path ---
#
# Everything above collapses `sleep`, which proves the RETURN CODE but says
# nothing about elapsed time — and elapsed time is the whole objection to a
# large backstop. Enlarging 210 -> 1800 is only safe because the wait is
# signal-driven: it returns when the probe fires, so the backstop is dead
# capacity, never a soak. These two run with the REAL sleep against the DERIVED
# 1800s backstop and time it.
unset -f sleep

elapsed_check() { # $1 desc, $2 max-allowed-secs, $3 min-allowed-secs, $4 elapsed
  if [ "$4" -le "$2" ] && [ "$4" -ge "$3" ]; then
    echo "PASS [elapsed=${4}s, within ${3}..${2}s] $1"
  else
    echo "FAIL [elapsed=${4}s, wanted ${3}..${2}s] $1"; FAILED=1
  fi
}

REAL_BACKSTOP=$(TRIGGER_DAEMON_WATCHDOG_SECS=600 trigger_daemon_backstop_secs)   # 1800
# 10x headroom is the rule the backstop is sized by, so it is also the bar the
# happy path is held to: anything at or under a TENTH of the backstop provably
# returned on the signal rather than waiting the timer out.
HAPPY_CEILING=$((REAL_BACKSTOP / 10))

stage_health 5

# Signal ALREADY present -> returns before the first sleep, so ~0s regardless of
# how large the backstop is.
t0=$(date +%s)
trigger_daemon_wait c "$REAL_BACKSTOP" 5 60 probe_hit; rc=$?
t1=$(date +%s)
check "REAL sleep, signal already present -> pass" 0 $rc
elapsed_check "signal already present returns immediately, not at the ${REAL_BACKSTOP}s backstop" \
  "$HAPPY_CEILING" 0 $((t1 - t0))

# Signal on the 3rd poll -> two 5s sleeps, so ~10s. The load-bearing assertion
# is the ceiling: it must be nowhere near the backstop. The floor proves the
# poll actually happened (a probe that returned instantly would not be
# exercising the loop at all).
_hits=0
probe_third() { _hits=$((_hits + 1)); [ "$_hits" -ge 3 ]; }
t0=$(date +%s)
trigger_daemon_wait c "$REAL_BACKSTOP" 5 60 probe_third; rc=$?
t1=$(date +%s)
check "REAL sleep, signal on the 3rd poll -> pass" 0 $rc
elapsed_check "3rd-poll signal returns at ~10s (2 x 5s poll), not at the ${REAL_BACKSTOP}s backstop" \
  "$HAPPY_CEILING" 10 $((t1 - t0))
[ "$_hits" = "3" ] || { echo "FAIL: probe called $_hits times, expected exactly 3"; FAILED=1; }

# Whole seconds round a 40ms return and a 900ms one to the same "0s", so the
# load-bearing claim — the backstop is DEAD CAPACITY, never spent — is measured
# at millisecond resolution too. The live 10g DELIVER leg returns in ~76ms
# against this same derived 1800s number (7 consecutive green ephemeral runs,
# 2026-08-09); the unit case has to show that shape, not just a rounded zero.
_ms0=$(date +%s%3N 2>/dev/null || echo N)
case "$_ms0" in
  *[!0-9]*) echo "SKIP [no ms-resolution date on this host] sub-second signal-return timing" ;;
  *)
    trigger_daemon_wait c "$REAL_BACKSTOP" 5 60 probe_hit; rc=$?
    _ms1=$(date +%s%3N)
    check "REAL sleep, ms-resolution: signal already present -> pass" 0 $rc
    _ms=$((_ms1 - _ms0))
    if [ "$_ms" -le 2000 ]; then
      echo "PASS [elapsed=${_ms}ms against a ${REAL_BACKSTOP}s backstop] the wait returns ON THE SIGNAL, not on the timer"
    else
      echo "FAIL [elapsed=${_ms}ms] a signal already present must return in milliseconds; the backstop is not a budget"; FAILED=1
    fi
    ;;
esac

if [ "$FAILED" = "0" ]; then echo "trigger_daemon_wait unit: OK"; else echo "trigger_daemon_wait unit: FAILURES"; fi
exit $FAILED
