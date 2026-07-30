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

if [ "$FAILED" = "0" ]; then echo "trigger_daemon_wait unit: OK"; else echo "trigger_daemon_wait unit: FAILURES"; fi
exit $FAILED
