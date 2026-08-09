#!/usr/bin/env bash

# Signal-driven waits for the `kind: trigger` scheduler daemon.
#
# WHY THIS EXISTS. The scheduler steps used to sleep a fixed budget and then
# guess at the cause ("the trigger lane never delivered"). That is wrong twice
# over: it burns the whole budget on a daemon that is provably dead, and it
# reports a delivery failure for what is actually a wedged tick loop. Worse, the
# budget (360s) was SHORTER than the daemon's own delivery timeout (600s), so a
# fire landing while the agent was settling could never be observed recovering —
# an automatic red no matter how healthy the system was.
#
# WHAT MAKES A SIGNAL-DRIVEN WAIT POSSIBLE. From molecule-scheduler v0.2.1 /
# molecule-ai-sdk 0.5.6, delivery is DECOUPLED from ticking: the tick only
# enqueues due fires and always advances `last_tick`, and a wedged delivery is
# cancelled and re-queued by the daemon's own watchdog. So `last_tick` is a TRUE
# liveness signal, and "stale heartbeat" means the tick loop is wedged — a
# deterministic failure that more waiting cannot fix. Against an OLDER daemon,
# which delivers inside the tick, a frozen heartbeat is expected during a fire
# and this wait would misreport it; that version floor is the contract.
#
# Callers get three distinguishable outcomes instead of one timeout:
#   0 = evidence observed (pass immediately, no residual sleep)
#   1 = backstop exhausted while the daemon kept ticking (NOT a frozen tick)
#   2 = daemon stopped ticking (fail fast, name the wedge)
#   3 = usage error
#
# Offline-testable: health JSON is read through TRIGGER_DAEMON_HEALTH_CMD when
# set, so the unit test drives it without Docker.

# Last heartbeat age observed by trigger_daemon_wait, in whole seconds ("" when
# unreadable). Exported because it is a PUBLIC OUTPUT of this lib: callers put it
# straight into their failure message so the operator sees how long the daemon
# had been silent, rather than a bare "timed out".
export TRIGGER_DAEMON_LAST_AGE="${TRIGGER_DAEMON_LAST_AGE:-}"

# Path of the daemon heartbeat inside the workspace container.
TRIGGER_DAEMON_HEALTH_PATH="${TRIGGER_DAEMON_HEALTH_PATH:-/configs/schedules/schedule-health.json}"

# Age of the last heartbeat in whole seconds, or "" when it is absent or
# unreadable. Absent is deliberately NOT reported as stale: a daemon that has
# not yet written its first heartbeat is not a wedged one, and treating the two
# alike would turn a slow boot into a hard failure.
trigger_daemon_tick_age_secs() {
  local container="${1:-}" health
  [ -n "$container" ] || { printf ''; return 0; }

  if [ -n "${TRIGGER_DAEMON_HEALTH_CMD:-}" ]; then
    health=$($TRIGGER_DAEMON_HEALTH_CMD "$container" 2>/dev/null) || health=""
  else
    health=$(docker exec "$container" sh -c "cat $TRIGGER_DAEMON_HEALTH_PATH 2>/dev/null" 2>/dev/null) || health=""
  fi
  [ -n "$health" ] || { printf ''; return 0; }

  HEALTH_JSON="$health" python3 -c '
import datetime as dt, json, os
try:
    tick = json.loads(os.environ["HEALTH_JSON"]).get("last_tick")
    if not tick:
        print("")
    else:
        ts = dt.datetime.fromisoformat(tick)
        if ts.tzinfo is None:
            ts = ts.replace(tzinfo=dt.timezone.utc)
        print(int((dt.datetime.now(dt.timezone.utc) - ts).total_seconds()))
except Exception:
    print("")
' 2>/dev/null || printf ''
}

# trigger_daemon_wait <container> <backstop-secs> <poll-secs> <stale-secs> <probe-fn> [probe-args...]
#
# Polls <probe-fn> for evidence. Between polls it checks the OWNING container's
# heartbeat: a sibling daemon's health says nothing about the one that owns the
# work (in the incident this was written for, two idle siblings ticked normally
# while the owner was frozen).
#
# Sets TRIGGER_DAEMON_LAST_AGE to the last observed age ("" if unreadable) so the
# caller can put a real number in its failure message.
trigger_daemon_wait() {
  local container="${1:-}" backstop="${2:-}" poll="${3:-}" stale="${4:-}" probe="${5:-}"
  TRIGGER_DAEMON_LAST_AGE=""

  case "$backstop" in ''|*[!0-9]*) return 3 ;; esac
  case "$poll" in ''|*[!0-9]*|0) return 3 ;; esac
  case "$stale" in ''|*[!0-9]*) return 3 ;; esac
  [ -n "$container" ] || return 3
  [ -n "$probe" ] || return 3
  shift 5

  local waited=0 age
  while true; do
    if "$probe" "$@"; then
      return 0
    fi

    age=$(trigger_daemon_tick_age_secs "$container")
    TRIGGER_DAEMON_LAST_AGE="$age"
    if [ -n "$age" ] && [ "$age" -gt "$stale" ]; then
      return 2
    fi

    [ "$waited" -ge "$backstop" ] && return 1

    sleep "$poll"
    waited=$((waited + poll))
  done
}

# The stale threshold: enough missed polls that a loaded box never trips it,
# short enough to fail in about a minute instead of the whole backstop.
trigger_daemon_stale_secs() {
  local poll="${1:-10}" stale
  case "$poll" in ''|*[!0-9]*|0) poll=10 ;; esac
  stale=$((poll * 6))
  [ "$stale" -ge 60 ] || stale=60
  printf '%s' "$stale"
}

# The daemon's own delivery watchdog, in whole seconds — the ONE number every
# backstop in this lib is measured against. It exists as a function so no caller
# re-types the value: a second copy of it is a second thing to forget when the
# daemon's timeout moves, and the invariant below is only as good as the number
# it compares to.
trigger_daemon_watchdog_secs() {
  local watchdog="${TRIGGER_DAEMON_WATCHDOG_SECS:-600}"
  case "$watchdog" in ''|*[!0-9]*|0) watchdog=600 ;; esac
  printf '%s' "$watchdog"
}

# The backstop is a never-hit safety net, so it must EXCEED the daemon's own
# delivery watchdog — otherwise the wait expires while a wedged fire is still
# being re-queued for its retry, which is exactly how the old 360s budget could
# never observe a recovery.
trigger_daemon_backstop_secs() {
  printf '%s' $(( $(trigger_daemon_watchdog_secs) * 3 ))
}

# trigger_daemon_backstop_resolve [operator-override-secs]
#
# The ONLY supported way to turn an operator knob into a backstop. An empty
# override derives the 3x above; a supplied one is honoured ONLY while it still
# exceeds the watchdog. Anything else is REFUSED (rc=3, nothing on stdout) so the
# caller fails loudly instead of running a wait that cannot observe a recovery.
#
# WHY REFUSING MATTERS MORE THAN DERIVING. Every call site spells its knob
# `${SOME_OVERRIDE:-$(derive)}`, so the override does not adjust the derivation —
# it REPLACES it, and any literal at all wins, including one below the watchdog.
# Deriving correctly in the default branch therefore proves nothing about what
# actually runs. This is not hypothetical: 10g's DELIVER leg passed a bare `210`
# against a 600s watchdog from #4568 until this guard, and it parsed, ran and
# read as deliberate the entire time — the inequality was simply never checked
# anywhere. A knob able to reintroduce the exact defect this lib exists to
# prevent has to be rejected by the code, not warned about in a comment.
#
# Note the strict `>`: equality is not good enough. A backstop exactly equal to
# the watchdog expires at the same instant the daemon cancels the wedged
# delivery, i.e. before the re-queued retry has had any time at all to land, so
# the recovery it is supposed to be able to see is still unobservable.
trigger_daemon_backstop_resolve() {
  local override="${1:-}" watchdog
  watchdog=$(trigger_daemon_watchdog_secs)

  if [ -z "$override" ]; then
    trigger_daemon_backstop_secs
    return 0
  fi

  case "$override" in *[!0-9]*) return 3 ;; esac
  [ "$override" -gt "$watchdog" ] || return 3
  printf '%s' "$override"
}

# ─── the grid-landing wait is deliberately NOT a trigger_daemon_wait ─────────
#
# Confirming that a just-created schedule reached the volume grid is a different
# kind of wait from everything above, and forcing trigger_daemon_wait onto it
# would be wrong twice over:
#
#   - WRONG SIGNAL. The grid file is written by CORE's synchronous create
#     forward (schedules.go Create → createVolume → the runtime's
#     /internal/schedules), not by the daemon's tick. The daemon only READS the
#     grid. So the daemon's heartbeat carries no information about whether the
#     create routed, and a healthy tick would happily mask a dead create path.
#   - STRUCTURALLY IMPOSSIBLE. trigger_daemon_wait needs the OWNING container as
#     its first argument, and at this point the owner is UNKNOWN — identifying it
#     is the OUTPUT of the grid probe, not an input to it.
#
# The real routing signal has already been received: POST /schedules returns 201
# only after the runtime accepted and persisted the entry (createVolume relays a
# 2xx; a still-booting runtime is a retried 502/503/504 and finally a 503, never a
# 201). So on the happy path the grid is ALREADY on disk before the first probe
# runs, and the probe passes on that first poll. What remains is pure OBSERVATION
# latency on our side — `docker ps` enumeration plus a `docker exec cat` — not
# convergence we are waiting on.
#
# That is why this budget is short and derived, instead of the fire backstop the
# step used to reuse: a grid that is still missing well after a 201 is a
# DETERMINISTIC contradiction between core's ack and the volume, and waiting out
# a 30-minute daemon backstop cannot resolve it — it only converts a fast, clear
# failure into half an hour of dead CI time.
#
# schedule_grid_confirm_secs [poll-secs]
#   ~3 observation attempts at the caller's poll interval, with a 30s floor (so a
#   1s poll still gets a usable window) and a 120s ceiling. The CEILING is the
#   load-bearing half: the poll comes from operator-tunable E2E_SCHEDULER_POLL_SECS,
#   so without it a raised poll would silently restore exactly the multi-minute
#   soak this replaces.
schedule_grid_confirm_secs() {
  local poll="${1:-10}" secs
  case "$poll" in ''|*[!0-9]*|0) poll=10 ;; esac
  secs=$((poll * 3))
  [ "$secs" -ge 30 ] || secs=30
  [ "$secs" -le 120 ] || secs=120
  printf '%s' "$secs"
}
