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

# Injected readers: print whatever the scenario staged, so no container needed.
fake_health() { cat "$TMP/health.json" 2>/dev/null; }
export TRIGGER_DAEMON_HEALTH_CMD=fake_health

# The DELIVERY-LANE signal: the daemon's durable attempt log. Staged as a plain
# file so a scenario can freeze it (wedged lane) or advance it (progressing lane)
# poll by poll, which is what makes the both-directions mutation possible offline.
fake_history() { cat "$TMP/history.json" 2>/dev/null; }
export TRIGGER_DAEMON_PROGRESS_CMD=fake_history

# $1 = number of attempt-log entries, or "none" for an absent/unreadable log.
stage_history() {
  if [ "$1" = "none" ]; then rm -f "$TMP/history.json"; return; fi
  N="$1" OUT="$TMP/history.json" python3 -c '
import json, os
n = int(os.environ["N"])
json.dump([{"name": "probe", "at": "2026-08-09T00:%02d:00+00:00" % (i % 60),
            "status": "fired", "cause": "completed"} for i in range(n)],
          open(os.environ["OUT"], "w"))
'
}

# A stall window big enough that the tests above (which are about the TICK
# signal) never trip the delivery detector by accident.
NOSTALL=100000

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

# A readable, FROZEN attempt log for every tick-signal scenario below. Frozen is
# the harder default on purpose: paired with $NOSTALL it proves the delivery
# detector runs on every poll (SAMPLES climbs) without ever changing a verdict
# that belongs to the tick signal.
stage_history 1

# --- evidence wins immediately, even if the heartbeat looks frozen -----------
stage_health 5
trigger_daemon_wait c 600 10 60 "$NOSTALL" probe_hit; check "evidence present -> pass" 0 $?

stage_health 4000
trigger_daemon_wait c 600 10 60 "$NOSTALL" probe_hit
check "evidence present, heartbeat frozen -> STILL pass (evidence beats liveness)" 0 $?

# --- wedged daemon fails fast ------------------------------------------------
stage_health 400
trigger_daemon_wait c 600 10 60 "$NOSTALL" probe_never; check "frozen heartbeat, no evidence -> fail fast" 2 $?
[ -n "$TRIGGER_DAEMON_LAST_AGE" ] || { echo "FAIL: TRIGGER_DAEMON_LAST_AGE not reported"; FAILED=1; }

stage_health 61
trigger_daemon_wait c 600 10 60 "$NOSTALL" probe_never; check "age 61s (just over threshold) -> fail fast" 2 $?

# --- live daemon that simply has not fired yet -> backstop, NOT a wedge ------
stage_health 59
# Short backstop (2 polls) on purpose: `stage_health` fixes an ABSOLUTE
# timestamp, so the observed age grows with the test's own real elapsed time.
# A 1s-under-threshold case that loops seven times ages past the threshold
# mid-wait and reports a wedge — a self-inflicted flake, not a lib defect.
trigger_daemon_wait c 10 10 60 "$NOSTALL" probe_never; check "age 59s (just under threshold) -> backstop" 1 $?

stage_health 5
trigger_daemon_wait c 60 10 60 "$NOSTALL" probe_never; check "fresh heartbeat, no evidence -> backstop" 1 $?

# --- an unreadable/absent heartbeat must NOT be reported as a wedged daemon --
stage_health none
trigger_daemon_wait c 60 10 60 "$NOSTALL" probe_never; check "no heartbeat file -> backstop, never 'wedged'" 1 $?

printf 'not json' > "$TMP/health.json"
trigger_daemon_wait c 60 10 60 "$NOSTALL" probe_never; check "corrupt heartbeat -> backstop, never 'wedged'" 1 $?

python3 -c 'import json,os; json.dump({"armed":1,"errors":{},"last_tick":None}, open(os.environ["OUT"],"w"))' OUT="$TMP/health.json" 2>/dev/null \
  || OUT="$TMP/health.json" python3 -c 'import json,os; json.dump({"armed":1,"errors":{},"last_tick":None}, open(os.environ["OUT"],"w"))'
trigger_daemon_wait c 60 10 60 "$NOSTALL" probe_never; check "null last_tick (pre-first-tick) -> backstop" 1 $?

# --- usage errors are refused, never silently treated as success -------------
trigger_daemon_wait "" 60 10 60 "$NOSTALL" probe_never;  check "missing container -> usage error" 3 $?
trigger_daemon_wait c 60 0 60 "$NOSTALL" probe_never;    check "zero poll -> usage error" 3 $?
trigger_daemon_wait c abc 10 60 "$NOSTALL" probe_never;  check "non-numeric backstop -> usage error" 3 $?
trigger_daemon_wait c 60 10 60 "$NOSTALL" "";            check "missing probe -> usage error" 3 $?
# The stall window has NO "off" value. A 0 that meant "disabled" would produce a
# run indistinguishable from an armed-and-correct one — the vacuous pass — so it
# is a usage error like any other missing argument.
trigger_daemon_wait c 60 10 60 0 probe_never;     check "zero stall (no 'disabled' value) -> usage error" 3 $?
trigger_daemon_wait c 60 10 60 abc probe_never;   check "non-numeric stall -> usage error" 3 $?
trigger_daemon_wait c 60 10 60 "" probe_never;    check "missing stall -> usage error" 3 $?
# Arity is fail-CLOSED: an un-migrated 5-arg call passes the probe name where the
# stall belongs, which is non-numeric, so it refuses instead of silently running
# with the detector off and the probe never invoked.
trigger_daemon_wait c 60 10 60 probe_never;       check "old 5-arg call shape -> usage error, not a silent unarmed run" 3 $?

# --- derived thresholds ------------------------------------------------------
#
# THE TICK-WEDGE THRESHOLD DERIVES FROM THE DAEMON'S TICK, NOT OUR POLL.
# Both defaulted to 10 in CI, so `6 x observation-poll` and `6 x tick` were the
# same number and the old derivation looked right. They are independent knobs
# (E2E_SCHEDULER_POLL_SECS vs E2E_TRIGGER_POLL_SECONDS), and the mutation that
# separates them is the point: at a 120s daemon tick, a threshold derived from
# the observation poll stays 60s and declares a perfectly healthy daemon wedged.
[ "$(trigger_daemon_stale_secs 10)" = "60" ]  || { echo "FAIL: stale(10) != 60 at the default 10s tick"; FAILED=1; }
[ "$(trigger_daemon_stale_secs 1)" = "60" ]   || { echo "FAIL: stale floor not 60"; FAILED=1; }
[ "$(TRIGGER_DAEMON_TICK_INTERVAL_SECS=30 trigger_daemon_stale_secs 10)" = "180" ] \
  || { echo "FAIL: stale does not track the DAEMON tick (30s tick should give 180s)"; FAILED=1; }
if [ "$(TRIGGER_DAEMON_TICK_INTERVAL_SECS=120 trigger_daemon_stale_secs 10)" -gt 120 ]; then
  echo "PASS [stale=$(TRIGGER_DAEMON_TICK_INTERVAL_SECS=120 trigger_daemon_stale_secs 10)s at a 120s daemon tick] the wedge threshold cannot fire on a healthy slow-ticking daemon"
else
  echo "FAIL: at a 120s daemon tick the stale threshold is $(TRIGGER_DAEMON_TICK_INTERVAL_SECS=120 trigger_daemon_stale_secs 10)s — below one tick, so EVERY healthy poll reads as a wedged daemon"; FAILED=1
fi
# The observation poll still FLOORS it (we cannot conclude a wedge from fewer
# reads than that), it just no longer sets it.
[ "$(TRIGGER_DAEMON_TICK_INTERVAL_SECS=1 trigger_daemon_stale_secs 40)" = "120" ] \
  || { echo "FAIL: observation poll does not floor the stale threshold"; FAILED=1; }

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
# Three keys exactly: the two cap keys above plus the daemon tick cadence the
# wedge threshold is derived from. Pinning the COUNT (not just presence) is what
# catches a fourth key sneaking onto the workspace without a reader here.
[ "$(printf '%s\n' "$cap_env" | grep -c .)" = "3" ] \
  || { echo "FAIL: trigger_daemon_delivery_cap_env emitted $(printf '%s\n' "$cap_env" | grep -c .) keys, expected exactly 3 (2 cap keys + MOLECULE_TRIGGER_POLL_SECONDS)"; FAILED=1; }

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

# ═══ THE DELIVERY-LANE STALL DETECTOR ════════════════════════════════════════
#
# The watchdog this file is really about. `last_tick` proves the TICK LOOP is
# alive and nothing more: scheduler.py's `tick()` never awaits a delivery, so the
# heartbeat advances at full cadence while `_delivery_worker` sits in an await
# that never returns. A heartbeat-only watchdog therefore CANNOT fire on that
# wedge, and the only thing that ever did was elapsed wall-clock — which is also
# the only thing that reds a slow-but-healthy delivery. Both failures, one
# number.
#
# The replacement is the daemon's durable ATTEMPT LOG. Every terminal outcome in
# scheduler.py appends exactly one entry, and nothing else can: not the tick, not
# the cron, not a held connection. So the verdict is "has the log advanced",
# never "how long has this taken", and the deadline RESETS on every advancement.
#
# BOTH DIRECTIONS ARE DEMONSTRATED BELOW, not argued:
#   A. a genuinely wedged lane (log frozen, heartbeat healthy)  -> MUST fire
#   B. a slow-but-progressing lane (attempts landing 600s apart,
#      run for 1800s — nearly 3x the stall window)              -> MUST NOT fire
# plus the boundary either side, the judgement order against the tick signal, and
# the vacuous-pass shape where the detector never armed at all.

STALL=$(trigger_daemon_progress_stall_secs 10)
CAP=$(trigger_daemon_watchdog_secs)
BACKSTOP=$(trigger_daemon_backstop_secs)
# The scenarios below run at the REAL derived bounds. Only the OBSERVATION poll
# is coarsened, to 300s, so a 1800s scenario is six loop iterations instead of a
# hundred and eighty — the wait's arithmetic is in seconds, not in iterations, so
# this changes nothing about which verdict is reached, only how many `cksum`
# spawns it costs. The boundary cases further down use a fine poll against a
# small window, which is where iteration granularity actually matters.
SIMPOLL=300
echo "PASS [cap=${CAP}s stall=${STALL}s backstop=${BACKSTOP}s, simulated at a ${SIMPOLL}s observation poll] derived bounds under test"

# Advancing probes. These mutate the staged attempt log the way the daemon would
# — one line per finished attempt — and never report evidence, so the ONLY thing
# that can end the wait is a watchdog verdict.
_pollN=0
probe_never_advancing() {        # an attempt finishes every poll: fastest healthy lane
  _pollN=$((_pollN + 1)); printf '{"e":%s}\n' "$_pollN" >> "$TMP/history.json"; return 1
}
probe_never_advancing_slowly() { # an attempt finishes every 2 polls = 600 simulated s,
  _pollN=$((_pollN + 1))         # i.e. AT the delivery cancel bound: as slow as a
  [ "$((_pollN % 2))" = "0" ] && printf '{"e":%s}\n' "$_pollN" >> "$TMP/history.json"
  return 1                       # delivery can legitimately be, and still healthy.
}

progress_check() { # $1 desc, $2 min-samples, $3 min-transitions, $4 max-transitions
  local s="$TRIGGER_DAEMON_PROGRESS_SAMPLES" t="$TRIGGER_DAEMON_PROGRESS_TRANSITIONS"
  if [ "$s" -ge "$2" ] && [ "$t" -ge "$3" ] && [ "$t" -le "$4" ]; then
    echo "PASS [samples=$s transitions=$t] $1"
  else
    echo "FAIL [samples=$s transitions=$t, wanted samples>=$2 and ${3}<=transitions<=$4] $1"; FAILED=1
  fi
}

# ── DIRECTION A: a WEDGED lane must fire ─────────────────────────────────────
# Heartbeat perfectly healthy (5s old, against a 60s wedge threshold) — so the
# tick signal says "fine, keep waiting" and would have burned the whole 1800s
# backstop. The attempt log is frozen. rc=4 at the stall window.
stage_health 5
stage_history 3
trigger_daemon_wait c "$BACKSTOP" "$SIMPOLL" 60 "$STALL" probe_never
check "WEDGED lane (log frozen, heartbeat healthy) -> stall verdict" 4 $?
# ARMED, and the count is reported: a run where the detector never engaged
# returns a different rc, but it would be indistinguishable in a bare pass/fail.
progress_check "the detector was ARMED and saw NO advancement on the wedged lane" 1 0 0
if [ "${TRIGGER_DAEMON_PROGRESS_AGE:-0}" -gt "$STALL" ]; then
  echo "PASS [progress age ${TRIGGER_DAEMON_PROGRESS_AGE}s > ${STALL}s] the verdict is reported with the silence that produced it"
else
  echo "FAIL: stall fired but TRIGGER_DAEMON_PROGRESS_AGE='${TRIGGER_DAEMON_PROGRESS_AGE:-}' does not exceed the ${STALL}s window — the number in the operator's message would be wrong"; FAILED=1
fi

# ── DIRECTION B: a SLOW-BUT-PROGRESSING lane must NOT fire ───────────────────
# Attempts land 600s apart — at the delivery cancel bound itself, i.e. as slow as
# a delivery can legitimately be — and the wait runs the full 1800s backstop,
# nearly THREE stall windows. Elapsed time is enormous; absence of progress never
# occurs. Anything other than rc=1 here is the false positive this replaces.
stage_health 5
stage_history 1
_pollN=0
trigger_daemon_wait c "$BACKSTOP" "$SIMPOLL" 60 "$STALL" probe_never_advancing_slowly
_slow_rc=$?
check "SLOW-BUT-PROGRESSING lane (attempts 600s apart, run for ${BACKSTOP}s) -> NOT a stall" 1 $_slow_rc
[ "$_slow_rc" = "4" ] && { echo "FAIL: the watchdog fired on a HEALTHY lane — this is the false positive a fixed wall-clock cap produces"; FAILED=1; }
progress_check "the detector was ARMED and RESET on each of the lane's advancements" 1 2 999

# The same lane, but the marker finally lands long after the stall window would
# have expired had it been a stopwatch. A slow delivery still PASSES.
stage_health 5
stage_history 1
_pollN=0
_late=0
probe_late() {  # advances the log every 2 polls, then succeeds at poll 5 (1500s)
  _pollN=$((_pollN + 1))
  [ "$((_pollN % 2))" = "0" ] && printf '{"e":%s}\n' "$_pollN" >> "$TMP/history.json"
  _late=$_pollN
  [ "$_pollN" -ge 5 ]
}
trigger_daemon_wait c "$BACKSTOP" "$SIMPOLL" 60 "$STALL" probe_late
check "slow lane that DELIVERS at ~1500s (>2x the ${STALL}s stall window) -> pass" 0 $?
[ "$_late" -ge 5 ] || { echo "FAIL: probe_late ended at poll $_late, so the slow-success path was not exercised"; FAILED=1; }

# ── the BOUNDARY either side of the stall window ─────────────────────────────
# A threshold asserted only far from its edge is a threshold nobody has checked.
stage_health 5
stage_history 2
trigger_daemon_wait c 50 10 60 50 probe_never
check "frozen for exactly the stall window (backstop == stall) -> NOT yet a stall" 1 $?
stage_history 2
trigger_daemon_wait c 60 10 60 50 probe_never
check "frozen for one poll PAST the stall window -> stall" 4 $?

# ── judgement ORDER: a dead tick is named before a stalled lane ──────────────
# A frozen tick freezes the attempt log as a CONSEQUENCE. Reporting that as a
# delivery stall would name the symptom and send the operator at the wrong lane.
stage_health 400
stage_history 2
trigger_daemon_wait c "$BACKSTOP" "$SIMPOLL" 60 "$STALL" probe_never
check "tick dead AND log frozen -> reported as the TICK wedge, not the delivery stall" 2 $?

# ── evidence still beats every watchdog ──────────────────────────────────────
stage_health 5
stage_history 2
trigger_daemon_wait c "$BACKSTOP" "$SIMPOLL" 60 1 probe_hit
check "evidence present, stall window of 1s -> STILL pass (evidence beats both signals)" 0 $?

# ── THE VACUOUS PASS, made visible ───────────────────────────────────────────
# An unreadable attempt log leaves the detector UNARMED. That must not be
# reported as a stall (an unexecable container is not a wedged lane) — but it
# also must not look like a clean run, because it carries no evidence about the
# delivery lane at all. The two are told apart by SAMPLES, which is exactly why
# the harness prints it on every leg.
stage_health 5
stage_history none
trigger_daemon_wait c "$BACKSTOP" "$SIMPOLL" 60 "$STALL" probe_never
check "attempt log unreadable -> backstop, NEVER a stall verdict" 1 $?
if [ "$TRIGGER_DAEMON_PROGRESS_SAMPLES" = "0" ]; then
  echo "PASS [samples=0] the UNARMED run is distinguishable from an armed one; a caller asserting samples>0 catches it"
else
  echo "FAIL: samples=$TRIGGER_DAEMON_PROGRESS_SAMPLES with no readable attempt log — the unarmed run is being counted as armed, which is the vacuous pass"; FAILED=1
fi
# ...and the armed-vs-unarmed distinction is REAL, not an artefact of the rc:
# both of these return 1, and only the counter separates them.
stage_history 1
_pollN=0
trigger_daemon_wait c 30 10 60 "$STALL" probe_never_advancing
check "readable+advancing log, short backstop -> backstop (same rc as the unarmed run)" 1 $?
if [ "$TRIGGER_DAEMON_PROGRESS_SAMPLES" -gt 0 ]; then
  echo "PASS [samples=$TRIGGER_DAEMON_PROGRESS_SAMPLES vs 0 above, identical rc=1] armed and unarmed are separable ONLY by the counter"
else
  echo "FAIL: an armed run reported samples=0 — the counter cannot distinguish the vacuous case"; FAILED=1
fi

# ── "NOT POLLED" AND "BLIND" ARE DIFFERENT STATES ────────────────────────────
#
# `samples == 0` conflated two outcomes that mean opposite things, and the live
# log printed the identical "carries NO evidence" line for both on every green
# run. TRIGGER_DAEMON_PROGRESS_READS separates them: reads counts ATTEMPTS,
# samples counts READABLE attempts.
#
#   reads == 0              returned on the probe before any read. Benign — the
#                           evidence arrived before the detector looked.
#   reads > 0, samples == 0 polled and read nothing. The real blind spot.
stage_health 5
stage_history 2
trigger_daemon_wait c "$BACKSTOP" "$SIMPOLL" 60 "$STALL" probe_hit
if [ "$TRIGGER_DAEMON_PROGRESS_READS" = "0" ] && [ "$TRIGGER_DAEMON_PROGRESS_SAMPLES" = "0" ]; then
  echo "PASS [reads=0 samples=0] evidence on the first probe -> NOT POLLED, distinguishable from blind"
else
  echo "FAIL: probe_hit gave reads=$TRIGGER_DAEMON_PROGRESS_READS samples=$TRIGGER_DAEMON_PROGRESS_SAMPLES, expected 0/0 — a fast pass is being counted as a poll"; FAILED=1
fi

stage_history none   # unreadable log: polls happen, reads return nothing
trigger_daemon_wait c 30 10 60 "$STALL" probe_never
if [ "$TRIGGER_DAEMON_PROGRESS_READS" -gt 0 ] && [ "$TRIGGER_DAEMON_PROGRESS_SAMPLES" = "0" ]; then
  echo "PASS [reads=$TRIGGER_DAEMON_PROGRESS_READS samples=0] polled but unreadable -> BLIND, the state that genuinely carries no evidence"
else
  echo "FAIL: unreadable log gave reads=$TRIGGER_DAEMON_PROGRESS_READS samples=$TRIGGER_DAEMON_PROGRESS_SAMPLES, expected reads>0 and samples=0"; FAILED=1
fi

stage_history 2      # readable: reads and samples both climb, and agree
trigger_daemon_wait c 30 10 60 "$STALL" probe_never
if [ "$TRIGGER_DAEMON_PROGRESS_SAMPLES" -gt 0 ] && [ "$TRIGGER_DAEMON_PROGRESS_READS" -eq "$TRIGGER_DAEMON_PROGRESS_SAMPLES" ]; then
  echo "PASS [reads=$TRIGGER_DAEMON_PROGRESS_READS samples=$TRIGGER_DAEMON_PROGRESS_SAMPLES] readable log -> ARMED, every attempt readable"
else
  echo "FAIL: readable log gave reads=$TRIGGER_DAEMON_PROGRESS_READS samples=$TRIGGER_DAEMON_PROGRESS_SAMPLES; both should climb together"; FAILED=1
fi

# ── the harness's REPORTER is DRIVEN, not grepped ────────────────────────────
#
# Grepping the harness for the three state strings proved they were TYPED, not
# that any input reaches them — and that gap shipped: the counter normalisation
# added with the three-state split had NO assertion behind it, so deleting its
# `case` lines left the suite fully green. A guard with no test is the same
# species as a guard that cannot fire, which is what this whole file is about.
#
# So the real function is extracted from the harness and CALLED with each input
# class. `log` is stubbed to capture the line.
if [ ! -f "$HARNESS" ]; then
  echo "FAIL: harness not found at $HARNESS (cannot drive scheduler_progress_report)"; FAILED=1
else
  _rep_src=$(sed -n '/^scheduler_progress_report()/,/^}/p' "$HARNESS")
  if [ -z "$_rep_src" ]; then
    echo "FAIL: could not extract scheduler_progress_report from the harness — the reporter is untested again"; FAILED=1
  else
    _REPORTED=""
    log() { _REPORTED="$*"; }
    eval "$_rep_src"

    report_check() { # $1 desc, $2 want-substring, $3 samples, $4 reads
      _REPORTED=""
      TRIGGER_DAEMON_PROGRESS_SAMPLES="$3" TRIGGER_DAEMON_PROGRESS_READS="$4" \
        TRIGGER_DAEMON_PROGRESS_TRANSITIONS=1 TRIGGER_DAEMON_PROGRESS_AGE=0 \
        scheduler_progress_report leg
      case "$_REPORTED" in
        *"$2"*) echo "PASS [$2] $1" ;;
        *) echo "FAIL: $1 — wanted '$2', got: ${_REPORTED:-<nothing>}"; FAILED=1 ;;
      esac
    }

    # The three legitimate states.
    report_check "samples>0 -> ARMED"                     "ARMED"       6 6
    report_check "reads>0, samples=0 -> BLIND"            "BLIND"       0 6
    report_check "reads=0 -> NOT POLLED"                  "NOT POLLED"  0 0
    # Empty is a legitimate zero (`${…:-0}`), NOT garbage: it is the true starting
    # value of both counters, so it must read as NOT POLLED rather than corrupt.
    report_check "empty counters -> NOT POLLED (legitimate zero)" "NOT POLLED" "" ""
    # Present-but-unusable is its OWN state. It must NOT degrade to the most
    # reassuring label — "the evidence arrived faster than the detector needed to
    # look" asserted about a run where the instrumentation is corrupt is a lie.
    report_check "samples=abc -> COUNTERS UNREADABLE"     "COUNTERS UNREADABLE" abc 6
    report_check "reads=abc -> COUNTERS UNREADABLE"       "COUNTERS UNREADABLE" 0 abc
    report_check "negative -1 -> COUNTERS UNREADABLE"     "COUNTERS UNREADABLE" -1 6
    report_check "trailing junk 3x -> COUNTERS UNREADABLE" "COUNTERS UNREADABLE" 3x 6
    # ...and garbage must not be reported as the benign case.
    _REPORTED=""
    TRIGGER_DAEMON_PROGRESS_SAMPLES=abc TRIGGER_DAEMON_PROGRESS_READS=abc scheduler_progress_report leg
    case "$_REPORTED" in
      *"NOT POLLED"*) echo "FAIL: unparseable counters were reported as the BENIGN 'NOT POLLED' state"; FAILED=1 ;;
      *) echo "PASS [not benign] unparseable counters do not read as 'NOT POLLED'" ;;
    esac

    # MUTATION: strip the normalisation `case` lines and the suite must RED.
    # Without this the whole block above could pass against an implementation
    # that never normalises anything.
    _rep_mut=$(printf '%s\n' "$_rep_src" | grep -v '^  case "\$_samples"' | grep -v '^  case "\$_reads"')
    (
      _REPORTED=""
      log() { _REPORTED="$*"; }
      eval "$_rep_mut"
      TRIGGER_DAEMON_PROGRESS_SAMPLES=abc TRIGGER_DAEMON_PROGRESS_READS=abc scheduler_progress_report leg 2>/dev/null
      case "$_REPORTED" in
        *"COUNTERS UNREADABLE"*) exit 1 ;;   # mutant survived -> the case lines do nothing
        *) exit 0 ;;                          # mutant died -> the case lines are load-bearing
      esac
    )
    if [ "$?" = "0" ]; then
      echo "PASS [mutant died] deleting the normalisation 'case' lines changes the label — they are load-bearing"
    else
      echo "FAIL: deleting the normalisation 'case' lines left the label unchanged — the normalisation is untested decoration"; FAILED=1
    fi

    unset -f scheduler_progress_report report_check log
  fi
fi

# ── EVERY BOUND ON THE PATH IS REACHABLE ─────────────────────────────────────
#
# The 210-vs-600 defect was an ordering defect nobody could see: the outer bound
# expired before the inner one it existed to accommodate could fire, so the retry
# was unreachable, and it parsed and read as deliberate for four months because
# the two numbers lived in different files. The inventory is now data and the
# ordering is asserted, in BOTH directions.
if out=$(trigger_daemon_timeout_ordering_check 10 2>&1); then
  echo "PASS [ordering] every bound in the inventory can be reached"
else
  echo "FAIL: the default timeout inventory is not reachable end to end: $out"; FAILED=1
fi
echo "       inventory: $(trigger_daemon_timeout_ledger 10 | tr '\n' ' ')"

# Mutation 1 — the DIRECT re-creation of the 210 defect: an outer bound that
# cuts before the inner cancel it is meant to outlast. The check must REFUSE it.
_bad_stall() { printf '%s' 210; }
if out=$(trigger_daemon_progress_stall_secs() { _bad_stall; }; trigger_daemon_timeout_ordering_check 10 2>&1); then
  echo "FAIL: a 210s outer window against a ${CAP}s cancel bound was ACCEPTED — the exact inversion this guard exists to refuse"; FAILED=1
else
  echo "PASS [refused] a 210s outer window against the ${CAP}s cancel bound (the original defect, re-created)"
fi

# Mutation 2 — equality. Two bounds expiring at the same instant race, and the
# outer wins often enough that the inner is unreachable in practice.
if out=$(trigger_daemon_progress_stall_secs() { trigger_daemon_watchdog_secs; }; trigger_daemon_timeout_ordering_check 10 2>&1); then
  echo "FAIL: an outer window EQUAL to the cancel bound was accepted"; FAILED=1
else
  echo "PASS [refused] an outer window exactly EQUAL to the cancel bound"
fi

# Mutation 3 — the subsumption claim is checked, not trusted. Raise the cap past
# the runtime's 900s idle TTL and `CAUSE_IDLE` becomes reachable again, so the
# ledger's "subsumed" disposition is no longer true and must fail.
if out=$(TRIGGER_DAEMON_WATCHDOG_SECS=1000 trigger_daemon_timeout_ordering_check 10 2>&1); then
  echo "FAIL: a 1000s cap above the ${TRIGGER_DAEMON_RUNTIME_IDLE_TTL_SECS}s idle TTL was accepted while the ledger still calls the TTL subsumed"; FAILED=1
else
  echo "PASS [refused] a cap above the idle TTL, which would silently make CAUSE_IDLE reachable again"
fi

# ── the CAP IS DERIVED, not typed ────────────────────────────────────────────
# The whole objection to the old 600 was that it was a literal somebody has to
# find and re-tune. It is now the repo's 10x margin on the system's own fire
# interval, so it FOLLOWS the configuration instead of being maintained beside
# it. That the derivation reproduces exactly the 600 previously chosen by
# measurement is the corroboration, not the definition.
[ "$(trigger_daemon_fire_interval_secs)" = "60" ] || { echo "FAIL: '* * * * *' does not derive a 60s fire interval"; FAILED=1; }
[ "$(trigger_daemon_fire_interval_secs '*/5 * * * *')" = "300" ] || { echo "FAIL: '*/5 * * * *' does not derive 300s"; FAILED=1; }
for bad in '0 3 * * *' '* * * *' 'garbage' '*/0 * * * *' '*/99 * * * *' ''; do
  if out=$(trigger_daemon_fire_interval_secs "$bad" 2>&1); then
    echo "FAIL: cron '$bad' was accepted and derived '$out' — a bound from a misparsed cron describes nothing"; FAILED=1
  else
    echo "PASS [refused] uninterpretable cron '$bad'"
  fi
done
# The cap tracks the cron. A `*/5` probe must not leave a 600s cap sized for a
# 60s cadence — that is precisely how a literal rots.
_c5=$(TRIGGER_DAEMON_PROBE_CRON='*/5 * * * *' trigger_daemon_watchdog_secs)
if [ "$_c5" = "3000" ]; then
  echo "PASS [cap=${_c5}s at a */5 cron] the cancel bound follows the configured fire cadence"
else
  echo "FAIL: a */5 probe cron gave cap=$_c5, not 3000 — the cap is not derived from the fire interval"; FAILED=1
fi
# ...and the STALL window follows it too, so the cap/stall PAIR keeps its
# relative ordering at the new cadence. If only one of them scaled, the
# inversion would come back silently.
#
# NOTE WHAT THIS DOES AND DOES NOT CLAIM. It proves the DERIVATION rescales, not
# that `*/5` is a usable configuration — it is not, and the next assertion is the
# proof. An earlier version of this test was labelled "the ordering survives a
# change of fire cadence", which over-claimed: the pair rescales, the LEDGER does
# not survive.
_s5=$(TRIGGER_DAEMON_PROBE_CRON='*/5 * * * *' trigger_daemon_progress_stall_secs 10)
if [ "$_s5" -gt "$_c5" ]; then
  echo "PASS [cap=${_c5}s < stall=${_s5}s] cap and stall rescale TOGETHER at a */5 cadence (the pair keeps its ordering)"
else
  echo "FAIL: at a */5 cron the stall window $_s5 does not exceed the cap $_c5 — the inversion returns as soon as the cadence moves"; FAILED=1
fi
# The whole-ledger verdict at */5 is a REFUSAL, and that is correct, not a gap:
# a 3000s cap sits above the runtime's 900s idle TTL, so `runtime_idle_ttl` is no
# longer subsumed and its disposition would be a lie. Asserting the refusal here
# stops someone "fixing" the cadence knob without noticing the ledger rejects it.
if TRIGGER_DAEMON_PROBE_CRON='*/5 * * * *' trigger_daemon_timeout_ordering_check 10 >/dev/null 2>&1; then
  echo "FAIL: the ledger ACCEPTED a */5 cadence, whose ${_c5}s cap exceeds the ${TRIGGER_DAEMON_RUNTIME_IDLE_TTL_SECS}s idle TTL — the subsumption claim would be false and unchecked"; FAILED=1
else
  echo "PASS [refused] the whole ledger REJECTS a */5 cadence (cap ${_c5}s > idle TTL ${TRIGGER_DAEMON_RUNTIME_IDLE_TTL_SECS}s) — the derivation rescales, this cadence does not survive"
fi

# --- the ordering check is NOT vacuous on an empty or truncated ledger -------
#
# The negative control this guard was missing. Every rule in
# trigger_daemon_timeout_ordering_check is a rule ABOUT ROWS, so zero rows
# satisfies all of them and the function reported "every bound can be reached"
# having examined nothing — the exact defect class the rest of this PR closes,
# inside the guard that closes it. Three shapes, all of which used to pass:
_ledger_orig=$(declare -f trigger_daemon_timeout_ledger)

# 1. EMPTY.
trigger_daemon_timeout_ledger() { printf ''; }
if out=$(trigger_daemon_timeout_ordering_check 10 2>&1); then
  echo "FAIL: an EMPTY ledger returned success — the guard asserted nothing and said so was fine"; FAILED=1
else
  echo "PASS [refused] an EMPTY ledger (0 rows) cannot report a reachable path"
fi

# 2. TRUNCATED — rows present, but fewer than the path has.
trigger_daemon_timeout_ledger() { printf 'a|10|d|reachable\nb|60|h|reachable\nc|600|d|reachable\n'; }
if out=$(trigger_daemon_timeout_ordering_check 10 2>&1); then
  echo "FAIL: a TRUNCATED ledger (3 of $(trigger_daemon_ledger_rows) rows) returned success — bounds silently fell off the path unchecked"; FAILED=1
else
  echo "PASS [refused] a TRUNCATED ledger (3 of $(trigger_daemon_ledger_rows) rows)"
fi

# 3. RIGHT COUNT, no chain — all-but-one subsumed. The row count is satisfied and
#    the loop runs, but the strictly-increasing comparison never fires even once.
trigger_daemon_timeout_ledger() {
  printf 'x|600|d|reachable\n'
  for _i in 1 2 3 4 5; do printf 's%s|900|r|subsumed-by:x\n' "$_i"; done
}
if out=$(trigger_daemon_timeout_ordering_check 10 2>&1); then
  echo "FAIL: a ledger with the right row count but only ONE reachable bound returned success — the ordering chain was never compared against anything"; FAILED=1
else
  echo "PASS [refused] right row count but no reachable CHAIN (1 reachable, 5 subsumed)"
fi

# 4. THE COUNT GUARD MUST NOT BE BYPASSABLE BY AN ENV KNOB.
#    It shipped for one commit as `${TRIGGER_DAEMON_LEDGER_ROWS:-6}` — the only
#    numeric knob in the lib with no validator — consumed by a bare `[ -ne ]`.
#    A non-numeric value makes that test ERROR (rc=2), not return false; the `if`
#    reads the error as false, the count check is SKIPPED, and a truncated ledger
#    passes at rc=0. `set -e` is exempt inside an `if`, so nothing aborts.
#
#    Three vectors, because the first fix (a hard `TRIGGER_DAEMON_LEDGER_ROWS=6`
#    constant) closed only the first and STILL bypassed on the other two when
#    actually run. The count now comes from a FUNCTION, which a variable
#    assignment cannot shadow.
trigger_daemon_timeout_ledger() { printf 'a|10|d|reachable\nb|60|h|reachable\nc|600|d|reachable\n'; }
_env_bypass() { # $1 = description, then the command that must still REFUSE
  local desc="$1"; shift
  if "$@" >/dev/null 2>&1; then
    echo "FAIL: $desc — a 3-of-6 truncated ledger PASSED; the count guard was bypassed"; FAILED=1
  else
    echo "PASS [refused] $desc"
  fi
}
_check_prefix() { TRIGGER_DAEMON_LEDGER_ROWS=abc trigger_daemon_timeout_ordering_check 10; }
# shellcheck disable=SC2034
#   "TRIGGER_DAEMON_LEDGER_ROWS appears unused" is the ASSERTION, not a lint
#   miss: nothing reads this variable any more, which is precisely why setting it
#   can no longer weaken the guard. shellcheck agreeing that it is inert is
#   corroboration; the runtime refusal below is the proof.
_check_after()  { TRIGGER_DAEMON_LEDGER_ROWS=3;   trigger_daemon_timeout_ordering_check 10; }
_env_bypass "command-prefix shadow TRIGGER_DAEMON_LEDGER_ROWS=abc" _check_prefix
_env_bypass "assignment after sourcing TRIGGER_DAEMON_LEDGER_ROWS=3 (would legitimise truncation)" _check_after
# (nothing to unset: the knob is retired; these assignments were inert)

# 5. ...and a REDEFINED count function returning garbage must FAIL CLOSED, not
#    skip. This is the residual the function form cannot prevent, so it is
#    validated at the point of use instead.
_rows_orig=$(declare -f trigger_daemon_ledger_rows)
trigger_daemon_ledger_rows() { printf 'abc'; }
if out=$(trigger_daemon_timeout_ordering_check 10 2>&1); then
  echo "FAIL: a non-numeric expected row count returned SUCCESS — the count check was skipped rather than failing closed"; FAILED=1
else
  case "$out" in
    *"not a positive integer"*) echo "PASS [refused, fail-closed] a non-numeric expected row count is diagnosed, not skipped" ;;
    *) echo "FAIL: refused but with the wrong diagnosis ('$out')"; FAILED=1 ;;
  esac
fi
eval "$_rows_orig"

# 6. ALIAS SHADOW. The one vector the function form does NOT close on its own:
#    `alias trigger_daemon_ledger_rows='printf 3'` under `shopt -s
#    expand_aliases` replaces the call at parse time. Low reachability, but
#    INVISIBLE to anyone grepping for the function definition — which is exactly
#    the audit a maintainer performs — so the call site is `\`-prefixed to
#    suppress alias expansion. Run in a subshell because enabling
#    expand_aliases and defining an alias must not leak into the rest of this
#    suite.
_alias_rc=0
(
  shopt -s expand_aliases
  # No `shellcheck disable` here on purpose. An earlier revision carried
  # `disable=SC2262,SC2263` "for the alias"; those codes fire at NO version
  # available to check — not CI's 0.9.0, not 0.10.0, not 0.11.0. A directive that
  # suppresses nothing tells the next reader there is a known lint issue at this
  # line when there is not, which is the same species as every other claim this
  # file has had to walk back. If a future shellcheck flags it, the gate reds and
  # someone adds the disable back with a reason that is true at the time.
  alias trigger_daemon_ledger_rows='printf 3'
  trigger_daemon_timeout_ledger() { printf 'a|10|d|reachable\nb|60|h|reachable\nc|600|d|reachable\n'; }
  trigger_daemon_timeout_ordering_check 10 >/dev/null 2>&1
) || _alias_rc=$?
if [ "$_alias_rc" = "0" ]; then
  echo "FAIL: an ALIAS shadow of trigger_daemon_ledger_rows let a 3-of-6 truncated ledger pass — the call site is not alias-proof"; FAILED=1
else
  echo "PASS [refused] alias shadow of the row-count function (backslash-prefixed call site)"
fi
eval "$_ledger_orig"   # restore the real ledger for everything below

# ...and the alias must not break the REAL ledger either. This half MUST run
# after the restore above: the previous blocks leave a truncated ledger
# installed, and against that any correct implementation refuses — so running it
# earlier would "prove" alias-proofing from a refusal that had nothing to do with
# the alias. (It did exactly that on the first run of this test.)
_alias_ok_rc=0
(
  shopt -s expand_aliases
  alias trigger_daemon_ledger_rows='printf 3'
  trigger_daemon_timeout_ordering_check 10 >/dev/null 2>&1
) || _alias_ok_rc=$?
if [ "$_alias_ok_rc" = "0" ]; then
  echo "PASS [inert] an alias cannot make the REAL six-row ledger fail either"
else
  echo "FAIL: the real ledger was refused merely because an alias existed (rc=$_alias_ok_rc)"; FAILED=1
fi

# 7. A SUBSUMER WITH A MALFORMED BOUND must name ITS OWN cause. `sub_secs` is
#    read from a DIFFERENT row than the one being validated, so it reaches a
#    `-ge` unvalidated — the same shape as the count-guard bypass. It must not
#    be reported as "not in the ledger" (a different edit to make).
trigger_daemon_timeout_ledger() {
  printf 'a|10|d|reachable\nb|60|h|reachable\nc|600|d|reachable\n'
  printf 'x|abc|r|reachable\ny|900|r|subsumed-by:x\nz|1800|h|reachable\n'
}
_sub_out=$(trigger_daemon_timeout_ordering_check 10 2>&1) && _sub_rc=0 || _sub_rc=$?
if [ "$_sub_rc" = "0" ]; then
  echo "FAIL: a subsumer with a non-numeric bound was ACCEPTED — the -ge errored into a skip"; FAILED=1
else
  case "$_sub_out" in
    *"whose bound is"*"not a positive integer"*)
      echo "PASS [refused, own cause] a subsumer with a malformed bound is diagnosed as malformed, not as missing" ;;
    *"is not in the ledger"*)
      echo "FAIL: a malformed subsumer bound was reported as 'not in the ledger' — wrong remedy for the next reader"; FAILED=1 ;;
    *) echo "FAIL: refused but with an unexpected diagnosis ('$_sub_out')"; FAILED=1 ;;
  esac
fi
eval "$_ledger_orig"

# 8. THE RETIRED KNOB MUST NOT BE NAMED AS THE REMEDY. It has zero reads
#    repo-wide, so a message telling an operator to bump it sends them to edit a
#    variable nothing consults. The remedy must name the function.
_trunc_out=$(trigger_daemon_ledger_rows() { printf 8; }; trigger_daemon_timeout_ordering_check 10 2>&1) || true
case "$_trunc_out" in
  *"update trigger_daemon_ledger_rows"*) echo "PASS [remedy names the function] the row-count mismatch message points at live code" ;;
  *"bump TRIGGER_DAEMON_LEDGER_ROWS"*)   echo "FAIL: the mismatch message still names the retired TRIGGER_DAEMON_LEDGER_ROWS knob (zero reads repo-wide)"; FAILED=1 ;;
  *) echo "FAIL: unexpected mismatch message ('$_trunc_out')"; FAILED=1 ;;
esac

# The env knob must be INERT against the real ledger too — set to garbage, the
# check still passes and still reports 6 rows. (A "fix" that made every run fail
# would also have refused the truncated case.)
if out=$(TRIGGER_DAEMON_LEDGER_ROWS=zzz trigger_daemon_timeout_ordering_check 10 2>&1); then
  case "$out" in
    *"checked 6 row(s)"*) echo "PASS [$out] the retired env knob is INERT: real ledger still passes and still counts 6" ;;
    *) echo "FAIL: passed but did not report 6 rows ('$out')"; FAILED=1 ;;
  esac
else
  echo "FAIL: the real ledger was refused merely because a stale env knob was set: $out"; FAILED=1
fi

# ...and the restored, real ledger must still PASS and must REPORT its count —
# a guard that only ever refuses is as useless as one that only ever passes.
if out=$(trigger_daemon_timeout_ordering_check 10 2>&1); then
  case "$out" in
    *"checked $(trigger_daemon_ledger_rows) row(s)"*)
      echo "PASS [$out] the real ledger passes AND reports how many rows it checked" ;;
    *)
      echo "FAIL: the real ledger passed but did not report its row count ('$out') — an armed check must be distinguishable from an empty one"; FAILED=1 ;;
  esac
else
  echo "FAIL: the real ledger was REFUSED after restore: $out"; FAILED=1
fi
# No literal 600 left in the lib's derivation path: the default must come out of
# the fire interval, so overriding the interval must move it.
if [ "$(TRIGGER_DAEMON_CAP_FIRE_MULTIPLE=3 trigger_daemon_watchdog_secs)" = "180" ]; then
  echo "PASS [3x fire interval = 180s] the cap is a MULTIPLE of a configured interval, not a stored number"
else
  echo "FAIL: changing the fire multiple did not change the cap — it is still a literal"; FAILED=1
fi

# The injected env must carry the daemon tick too, from the SAME call: the
# tick-wedge threshold is derived from it, and it used to be typed at the
# provision site while the threshold came from a different knob entirely.
if trigger_daemon_delivery_cap_env | grep -qx "MOLECULE_TRIGGER_POLL_SECONDS=$(trigger_daemon_tick_interval_secs)"; then
  echo "PASS [MOLECULE_TRIGGER_POLL_SECONDS=$(trigger_daemon_tick_interval_secs)] the daemon tick is injected from the same place the wedge threshold derives from"
else
  echo "FAIL: trigger_daemon_delivery_cap_env does not emit MOLECULE_TRIGGER_POLL_SECONDS — the provisioned tick and the derived threshold can drift apart again"; FAILED=1
fi

# The harness must ROUTE the stall window and the cron through the lib, for the
# same reason as every other bound: a literal at the call site parses, runs, and
# reads as deliberate.
if [ -f "$HARNESS" ]; then
  for fn in trigger_daemon_progress_stall_secs trigger_daemon_timeout_ordering_check trigger_daemon_probe_cron; do
    if grep -q "$fn" "$HARNESS"; then
      echo "PASS [routed] the harness uses $fn"
    else
      echo "FAIL: test_staging_full_saas.sh never calls $fn — the delivery-lane signal or the cron SSOT is not actually wired into the run"; FAILED=1
    fi
  done
  # Every trigger_daemon_wait call site must pass SIX arguments before the probe.
  # A five-argument leftover would hand the probe name in as the stall window and
  # be refused at runtime, but only when that leg actually executes — catch it here.
  _bad_arity=$(grep -n 'trigger_daemon_wait "' "$HARNESS" \
    | grep -Ev 'trigger_daemon_wait "[^"]*" "[^"]*" "[^"]*" +"[^"]*" "[^"]*" [A-Za-z_]' || true)
  if [ -z "$_bad_arity" ]; then
    echo "PASS [arity] every trigger_daemon_wait call site passes container/backstop/poll/stale/stall before the probe"
  else
    echo "FAIL: trigger_daemon_wait call site(s) missing the stall argument: $_bad_arity"; FAILED=1
  fi
  # The harness must ASSERT the detector engaged rather than assume it.
  if grep -q "TRIGGER_DAEMON_PROGRESS_SAMPLES" "$HARNESS"; then
    echo "PASS [anti-vacuous] the harness reports whether the stall detector armed"
  else
    echo "FAIL: the harness never reads TRIGGER_DAEMON_PROGRESS_SAMPLES — an unarmed run would be indistinguishable from an armed, correct one"; FAILED=1
  fi
fi

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
trigger_daemon_wait c "$REAL_BACKSTOP" 5 60 "$NOSTALL" probe_hit; rc=$?
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
trigger_daemon_wait c "$REAL_BACKSTOP" 5 60 "$NOSTALL" probe_third; rc=$?
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
    trigger_daemon_wait c "$REAL_BACKSTOP" 5 60 "$NOSTALL" probe_hit; rc=$?
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
