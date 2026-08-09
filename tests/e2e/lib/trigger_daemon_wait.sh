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
# "THE WATCHDOG" IS NO LONGER ONE FIXED DELIVERY TIMEOUT. It was, up to v0.2.1,
# and the 600s this lib compared against was that timeout's env default. v0.2.2
# retired the env var and v0.2.3 replaced the stopwatch with an activity-aware
# classifier that has FOUR dispositions and TWO distinct bounds (a 900s idle TTL
# and a 3600s absolute cap by default), including an "alive -> never cancelled"
# branch that has no bound at all. The number this lib measures against is
# therefore the ABSOLUTE cap specifically, and the harness CONFIGURES it rather
# than mirroring it — see `trigger_daemon_watchdog_secs` below for which env
# vars carry it and why both are needed.
#
# TICK LIVENESS IS NECESSARY BUT NOT SUFFICIENT — and that gap is the whole
# reason this file needed a second signal. Decoupling delivery from the tick is
# what makes `last_tick` trustworthy, but it is ALSO what makes a fresh
# `last_tick` compatible with a delivery that is wedged forever: `tick()` never
# awaits a delivery, so the heartbeat keeps advancing at full cadence while
# `_delivery_worker` sits in an `await` that will never return. A watchdog
# reading only the heartbeat therefore CANNOT fire on that wedge — it waits out
# the whole backstop and then reports "still ticking", which is true and useless.
# Elapsed wall-clock was the only thing that ever caught it, and a wall-clock cap
# is exactly what reds a slow-but-healthy delivery.
#
# THE SECOND SIGNAL: THE DAEMON'S DURABLE ATTEMPT LOG. `schedule-history.json`
# gains exactly one entry per FINISHED delivery attempt, and every terminal path
# in scheduler.py writes one — `_commit` for fired / unknown / deferred / error,
# and `check_watchdog`'s `_record_history` for a cancellation, each with a
# machine-readable `cause`. Read at v0.2.3 (see
# `trigger_daemon_scheduler_version_validated`), there is no path that finishes
# an attempt without appending. That makes the log a TRUE forward-progress
# signal for the delivery lane specifically:
#
#   - the tick loop cannot advance it (tick() only enqueues and writes health),
#   - a wedged delivery holding a connection open cannot advance it,
#   - and a slow-but-healthy one DOES advance it, once per attempt.
#
# WHY `schedule-state.json` IS DELIBERATELY *NOT* PART OF THE FINGERPRINT, and
# what it actually does on each path — driven at the pinned source, because the
# obvious guess is wrong in BOTH directions and the two paths differ:
#
#   CRON. `evaluate_tick` yields `scheduled_for=armed` — the value already in
#     state (trigger_schedule.py: `elif armed <= now: ... scheduled_for=armed`).
#     `tick()` then writes `persisted[turn.name] = turn.scheduled_for`, i.e. it
#     writes the SAME value back. So during a wedge the file is BYTE-IDENTICAL
#     tick after tick, deliberately: freezing the armed time is what makes a
#     crash mid-delivery re-fire. It advances only when `_commit` recomputes
#     `next_run` on a real completion — which is precisely the event the attempt
#     log already records. On this path state is REDUNDANT, not wrong.
#   POKE. `evaluate_tick` yields `scheduled_for=now`, and `keep_pokes` retains
#     the poke for any schedule still `_inflight`. So a poked schedule whose
#     delivery is wedged is re-poked every tick and writes a FRESH `now` every
#     tick. On this path state advances at full cadence while nothing is being
#     delivered — a FALSE progress signal, exactly the thing this watchdog must
#     not key on.
#
# Either way it must stay out: on one path it adds nothing the attempt log does
# not already give, and on the other it would reset the stall deadline forever
# on a wedged poked delivery. This note exists so nobody re-adopts the file
# after reading only the cron path and concluding it looks like a fine signal.
#
# So the wait now fails on ABSENCE OF ADVANCEMENT, not on elapsed time: the
# stall deadline is reset on every observed change, which means a delivery that
# keeps finishing attempts can run arbitrarily long without ever tripping it.
#
# Callers get five distinguishable outcomes instead of one timeout:
#   0 = evidence observed (pass immediately, no residual sleep)
#   1 = backstop exhausted while BOTH signals kept advancing (not a wedge)
#   2 = tick loop stopped (fail fast, name the wedge)
#   3 = usage error
#   4 = delivery lane stalled: ticking normally, but the attempt log has not
#       advanced for the derived stall window (fail fast, name the lane)
#
# Offline-testable: health JSON is read through TRIGGER_DAEMON_HEALTH_CMD and the
# attempt log through TRIGGER_DAEMON_PROGRESS_CMD when set, so the unit test
# drives both without Docker.

# Last heartbeat age observed by trigger_daemon_wait, in whole seconds ("" when
# unreadable). Exported because it is a PUBLIC OUTPUT of this lib: callers put it
# straight into their failure message so the operator sees how long the daemon
# had been silent, rather than a bare "timed out".
export TRIGGER_DAEMON_LAST_AGE="${TRIGGER_DAEMON_LAST_AGE:-}"

# Delivery-lane progress accounting from the last trigger_daemon_wait. PUBLIC
# OUTPUTS, and the ANTI-VACUOUS-PASS instrumentation: a run in which the stall
# detector was never armed passes identically to one in which it was armed and
# correct, so the caller must be able to assert it engaged and print the count.
#
#   TRIGGER_DAEMON_PROGRESS_SAMPLES     readable attempt-log reads (0 == the
#                                       detector NEVER ARMED — treat as no proof)
#   TRIGGER_DAEMON_PROGRESS_TRANSITIONS observed advancements of the attempt log
#   TRIGGER_DAEMON_PROGRESS_AGE         seconds since the last advancement ("" if
#                                       never armed)
export TRIGGER_DAEMON_PROGRESS_SAMPLES="${TRIGGER_DAEMON_PROGRESS_SAMPLES:-0}"
export TRIGGER_DAEMON_PROGRESS_TRANSITIONS="${TRIGGER_DAEMON_PROGRESS_TRANSITIONS:-0}"
export TRIGGER_DAEMON_PROGRESS_AGE="${TRIGGER_DAEMON_PROGRESS_AGE:-}"

# Path of the daemon heartbeat inside the workspace container.
TRIGGER_DAEMON_HEALTH_PATH="${TRIGGER_DAEMON_HEALTH_PATH:-/configs/schedules/schedule-health.json}"

# Path of the daemon's durable ATTEMPT LOG inside the workspace container — the
# delivery-lane progress signal. scheduler.py HISTORY_FILENAME.
TRIGGER_DAEMON_HISTORY_PATH="${TRIGGER_DAEMON_HISTORY_PATH:-/configs/schedules/schedule-history.json}"

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

# A short, stable fingerprint of the daemon's durable ATTEMPT LOG, or "" when it
# is absent or unreadable. Changes if and only if the delivery lane finished an
# attempt (see the header): one entry per terminal outcome, appended by
# `_commit` or by the watchdog's `_record_history`.
#
# Unreadable is deliberately NOT reported as "no progress", for the same reason
# an absent heartbeat is not reported as a wedge: a workspace whose container is
# briefly unexecable, or one that has simply not completed its first attempt
# yet, is not a stalled lane. An empty fingerprint leaves the stall detector
# UNARMED for that poll, and the caller can see that it was unarmed because
# TRIGGER_DAEMON_PROGRESS_SAMPLES stays 0.
#
# WHOLE-FILE, NOT PER-SCHEDULE, and that is the correct granularity rather than a
# convenient one. A digest covering every schedule on the container could in
# principle mask one wedged schedule behind another that keeps firing — except
# that the daemon runs a SINGLE `_delivery_worker` awaiting a SINGLE `_active`
# delivery, so a wedged delivery blocks the queue for every schedule at once. The
# wedge is lane-wide by construction, so the lane-wide signal is the one that
# matches it; filtering to one schedule's entries would only make the signal
# sparser (one attempt per fire instead of N) without making it more specific.
#
# `cksum` rather than the raw file: the log grows to HISTORY_MAX=200 entries, so
# comparing content directly would hold two multi-kilobyte strings in shell
# variables on every poll for no gain. A collision would have to reproduce both
# the byte count and the CRC of an append-only log whose entries carry
# timestamps; the failure mode of a collision is one missed advancement, which
# the next poll corrects.
trigger_daemon_progress_digest() {
  local container="${1:-}" blob
  [ -n "$container" ] || { printf ''; return 0; }

  if [ -n "${TRIGGER_DAEMON_PROGRESS_CMD:-}" ]; then
    blob=$($TRIGGER_DAEMON_PROGRESS_CMD "$container" 2>/dev/null) || blob=""
  else
    blob=$(docker exec "$container" sh -c "cat $TRIGGER_DAEMON_HISTORY_PATH 2>/dev/null" 2>/dev/null) || blob=""
  fi
  [ -n "$blob" ] || { printf ''; return 0; }

  printf '%s' "$blob" | cksum 2>/dev/null | tr -d ' \t\n' || printf ''
}

# trigger_daemon_wait <container> <backstop-secs> <poll-secs> <stale-secs> <stall-secs> <probe-fn> [probe-args...]
#
# Polls <probe-fn> for evidence. Between polls it checks TWO independent signals
# on the OWNING container — a sibling daemon's state says nothing about the one
# that owns the work (in the incident this was written for, two idle siblings
# ticked normally while the owner was frozen):
#
#   TICK      last_tick advancement    -> rc=2 when silent for <stale-secs>
#   DELIVERY  attempt-log advancement  -> rc=4 when silent for <stall-secs>
#
# Neither is a stopwatch on the work. Both deadlines are reset by OBSERVED
# ADVANCEMENT, so a slow-but-progressing delivery is never cancelled by either,
# however long it takes. <backstop-secs> is the only elapsed-time bound left and
# it is a never-hit outermost net, not a budget.
#
# ORDER OF JUDGEMENT is deliberate and mirrors the daemon's own classifier: a
# dead TICK is judged before a stalled DELIVERY, because a frozen tick loop
# freezes the attempt log as a CONSEQUENCE — reporting that as a delivery stall
# would name the symptom and hide the cause.
#
# Sets TRIGGER_DAEMON_LAST_AGE, TRIGGER_DAEMON_PROGRESS_AGE,
# TRIGGER_DAEMON_PROGRESS_SAMPLES and TRIGGER_DAEMON_PROGRESS_TRANSITIONS so the
# caller can put real numbers in its message AND assert the detector was armed.
trigger_daemon_wait() {
  local container="${1:-}" backstop="${2:-}" poll="${3:-}" stale="${4:-}" stall="${5:-}" probe="${6:-}"
  TRIGGER_DAEMON_LAST_AGE=""
  TRIGGER_DAEMON_PROGRESS_AGE=""
  TRIGGER_DAEMON_PROGRESS_SAMPLES=0
  TRIGGER_DAEMON_PROGRESS_TRANSITIONS=0

  case "$backstop" in ''|*[!0-9]*) return 3 ;; esac
  case "$poll" in ''|*[!0-9]*|0) return 3 ;; esac
  case "$stale" in ''|*[!0-9]*) return 3 ;; esac
  # NO "off" VALUE. `0` is refused rather than treated as "stall detection
  # disabled", because a disabled detector produces a run that is
  # indistinguishable from an armed-and-correct one — the vacuous pass this
  # lib's own instrumentation exists to make visible. A caller that cannot name
  # a window has to fail, not silently opt out of the only signal that catches a
  # wedged delivery under a healthy heartbeat.
  case "$stall" in ''|*[!0-9]*|0) return 3 ;; esac
  [ -n "$container" ] || return 3
  [ -n "$probe" ] || return 3
  shift 6

  local waited=0 age digest last_digest="" since_progress=0
  while true; do
    if "$probe" "$@"; then
      return 0
    fi

    age=$(trigger_daemon_tick_age_secs "$container")
    TRIGGER_DAEMON_LAST_AGE="$age"
    if [ -n "$age" ] && [ "$age" -gt "$stale" ]; then
      return 2
    fi

    digest=$(trigger_daemon_progress_digest "$container")
    if [ -n "$digest" ]; then
      TRIGGER_DAEMON_PROGRESS_SAMPLES=$((TRIGGER_DAEMON_PROGRESS_SAMPLES + 1))
      if [ "$digest" != "$last_digest" ]; then
        # The FIRST readable digest is a baseline, not an advancement: there is
        # nothing before it to have advanced from. Counting it would inflate the
        # transition count that callers assert on, which is the one number that
        # distinguishes "the lane moved" from "the detector merely ran".
        #
        # An `if` rather than `[ ... ] && assign`: on the first digest the test
        # is false, so the `&&` list would carry exit status 1. Every current
        # call site spells `trigger_daemon_wait ... || rc=$?`, which suspends the
        # caller's errexit for the whole body and makes that harmless — but the
        # harness runs `set -euo pipefail` AND is invoked by Gitea Actions as
        # `bash --noprofile --norc -e -o pipefail`, so a future call site that
        # drops the `||` would kill the run inside this loop with no message at
        # all. Not worth leaving to a convention nobody can see from here.
        if [ -n "$last_digest" ]; then
          TRIGGER_DAEMON_PROGRESS_TRANSITIONS=$((TRIGGER_DAEMON_PROGRESS_TRANSITIONS + 1))
        fi
        last_digest="$digest"
        since_progress=0
      fi
      TRIGGER_DAEMON_PROGRESS_AGE="$since_progress"
      if [ "$since_progress" -gt "$stall" ]; then
        return 4
      fi
    fi

    [ "$waited" -ge "$backstop" ] && return 1

    sleep "$poll"
    waited=$((waited + poll))
    since_progress=$((since_progress + poll))
  done
}

# ─── the two intervals every threshold below is derived from ─────────────────
#
# Both are CONFIGURED BY THIS HARNESS, which is what makes deriving from them
# honest rather than a guess about somebody else's default.

# The daemon's own tick cadence, in seconds. scheduler.py `_poll_seconds()` reads
# MOLECULE_TRIGGER_POLL_SECONDS (default 30, floor 1) and the harness injects it
# at provision — see `trigger_daemon_delivery_cap_env`, which emits it from here
# so the value the daemon runs and the value the thresholds are derived from
# cannot be two different numbers.
trigger_daemon_tick_interval_secs() {
  local tick="${TRIGGER_DAEMON_TICK_INTERVAL_SECS:-10}"
  case "$tick" in ''|*[!0-9]*|0) tick=10 ;; esac
  printf '%s' "$tick"
}

# The cron the harness writes on its own probe schedules. Single SSOT so the
# expression sent to the API and the interval every bound is derived from cannot
# drift — the whole failure being fixed is a number that stopped matching the
# thing it was supposed to describe.
trigger_daemon_probe_cron() {
  printf '%s' "${TRIGGER_DAEMON_PROBE_CRON:-* * * * *}"
}

# trigger_daemon_fire_interval_secs [cron]
#
# How often the probe schedule fires, in seconds, derived from its cron. This is
# the system's own statement of how much time one delivery is allotted: the
# daemon refuses to re-enqueue a schedule that is still in flight
# (`if turn.name in self._inflight: continue`), so a delivery that outlives one
# fire interval has already caused the lane to SKIP a fire. That makes the fire
# interval — not a stopwatch reading — the natural unit for every delivery bound.
#
# Only the shapes the harness actually writes are accepted (`*` and `*/N` in the
# minute field, remaining fields all `*`). Anything else is REFUSED with rc=3 and
# nothing on stdout, rather than silently defaulting: a bound derived from a
# misparsed cron is precisely the "number that describes nothing" this file
# exists to eliminate, and a wrong derivation is worse than a loud refusal.
#
# shellcheck disable=SC2120
#   The argument IS optional and IS passed — by lib/test_trigger_daemon_wait_unit.sh,
#   which shellcheck lints as a separate file and so cannot see. The usual way to
#   tell shellcheck an argument is optional is `${1:-default}`, and that is exactly
#   the form this function must NOT use: `:-` would substitute the default for an
#   argument supplied as the EMPTY STRING, and refusing that case is an assertion
#   with a test behind it (a caller that thinks it has a cron and does not must get
#   rc=3, not a 60s interval invented on its behalf).
trigger_daemon_fire_interval_secs() {
  # `${1-...}`, not `${1:-...}`: an OMITTED argument takes the configured probe
  # cron, but an argument supplied as the empty string is a caller that thinks it
  # has a cron and does not. Defaulting that would hand back a 60s interval for a
  # schedule nobody described.
  local cron minute rest
  cron="${1-$(trigger_daemon_probe_cron)}"
  minute="${cron%% *}"
  rest="${cron#* }"
  [ "$rest" = "* * * *" ] || return 3
  case "$minute" in
    '*') printf '%s' 60; return 0 ;;
    '*/'*)
      local n="${minute#*/}"
      case "$n" in ''|*[!0-9]*|0) return 3 ;; esac
      [ "$n" -le 59 ] || return 3
      printf '%s' "$((n * 60))"; return 0 ;;
  esac
  return 3
}

# ─── TICK-WEDGE threshold: absence of tick-sequence advancement ──────────────
#
# trigger_daemon_stale_secs [observation-poll-secs]
#
# Derived from the DAEMON'S tick interval, not from the harness's observation
# poll. That distinction is the bug: both defaulted to 10 in CI, so 6 x poll
# happened to equal 6 x tick and the threshold looked correct — but they are two
# independent knobs (E2E_SCHEDULER_POLL_SECS vs E2E_TRIGGER_POLL_SECONDS). Raise
# the DAEMON's poll to 120s, a supported configuration, and a threshold derived
# from the observation poll stays 60s: every wait then returns rc=2 "the trigger
# daemon STOPPED TICKING" against a daemon that is ticking exactly as configured.
# A false positive by construction, waiting on a knob nobody had reason to think
# was load-bearing.
#
# Six missed ticks, so a loaded box never trips it, floored by three observation
# polls (we cannot conclude anything from fewer reads than that) and by 60s.
trigger_daemon_stale_secs() {
  local poll="${1:-10}" tick stale
  case "$poll" in ''|*[!0-9]*|0) poll=10 ;; esac
  tick=$(trigger_daemon_tick_interval_secs)
  stale=$((tick * 6))
  [ "$stale" -ge $((poll * 3)) ] || stale=$((poll * 3))
  [ "$stale" -ge 60 ] || stale=60
  printf '%s' "$stale"
}

# ─── the delivery cancel bound every backstop is measured against ────────────
#
# WHAT THIS NUMBER IS at the PINNED scheduler, and what it is NOT. There is no
# daemon-side `MOLECULE_TRIGGER_DELIVERY_WATCHDOG_SECONDS` to mirror: v0.2.2
# retired it, and core pins v0.2.3 (workspace-server/internal/handlers/
# plugin_registry_test.go). v0.2.3's watchdog is ACTIVITY-AWARE — each tick it
# reads the runtime's turn-lease snapshot and `classify_delivery_liveness`
# returns one of four dispositions:
#
#   lease alive AND attributable      never cancelled, however long it runs
#   lease attributable, idle-expired  cancel at the lease idle TTL
#   lease attributable, past its cap  cancel at the runtime's ABSOLUTE cap
#   no lease, or a lease NOT
#     attributable to this delivery   cancel at the ABSOLUTE ceiling
#
# The LAST row is the ordinary case for an e2e workspace, not an exotic
# fallback. The turn lease is workspace-GLOBAL (installed at container boot,
# re-armed per turn, and `turn_lease.arm_turn_if_fed` leaves the FIRST turn of a
# fresh container unarmed because nothing has yet proved the activity feed
# works), while `lease_is_attributable` requires `turn_age_seconds < elapsed`.
# On a freshly-provisioned workspace whose only activity is the fire it just
# received, `turn_age` is container uptime PLUS this delivery's own elapsed, so
# it exceeds `elapsed` at EVERY elapsed — the branch is permanent, not a
# transient at the start of the delivery.
#
# So the worst case over all four branches is the ABSOLUTE cap, and that cap is
# configured in TWO places which must BOTH be set or the smaller one is ignored:
#
#   MOLECULE_TRIGGER_DELIVERY_ABSOLUTE_CAP_SECONDS — the DAEMON's own ceiling
#     (scheduler._absolute_cap_seconds; default 3600). Used verbatim only when
#     there is no lease at all.
#   MOLECULE_MAX_TURN_SECONDS — the RUNTIME's absolute per-turn cap
#     (turn_lease._resolve_absolute_cap; default 4.0 x the 900s idle TTL, so
#     3600). REPORTED-CAP-WINS: `_reported_absolute_cap` prefers the cap the
#     snapshot carries over the daemon's env ceiling, so on the not-attributable
#     branch — the ordinary one — setting the daemon env ALONE changes nothing.
#
# Both are injected onto e2e workspaces FROM THIS FUNCTION (see
# `trigger_daemon_delivery_cap_env` below and its call site in
# test_staging_full_saas.sh). That is what makes this a real bound rather than a
# guess about somebody else's default: the harness owns the workspace env, so it
# does not have to track the daemon's shipped cap — it sets it.
#
# THE DEFAULT IS DERIVED, NOT TYPED. It is the repo's ~10x margin rule applied
# to the system's own fire interval:
#
#     cap = TRIGGER_DAEMON_CAP_FIRE_MULTIPLE x trigger_daemon_fire_interval_secs
#         = 10 x 60s = 600s at the harness's `* * * * *` probe cron
#
# WHY THE FIRE INTERVAL IS THE RIGHT BASE UNIT. It is not a proxy for delivery
# latency; it is the system's own declaration of how much time one delivery is
# allotted, enforced by the daemon: a schedule still in flight when its next fire
# comes due is skipped rather than re-enqueued, so outliving one fire interval is
# already, mechanically, falling behind. A cap expressed as "N fire intervals"
# therefore says something about the lane, and it re-derives itself if the probe
# cadence ever changes — where a literal 600 would quietly become 1/5 of a cycle
# against a `*/5 * * * *` probe and nobody would notice until it cancelled every
# delivery.
#
# WHY THE MULTIPLE IS 10 AND WHY IT IS NOT A RETUNABLE LITERAL. 10 is this
# repo's standing margin rule, and it is dimensionless: it survives every change
# to the intervals, which is exactly the property the old 600 lacked. The
# measurement corroborates the unit rather than setting the number — across seven
# consecutive green ephemeral-happy-path runs on 2026-08-09 the whole cron
# boundary -> fire -> turn -> `notify` -> activity row chain completed 18.7s to
# 47.7s after the minute boundary, i.e. 0.31 to 0.79 of ONE fire interval. Real
# deliveries do fit inside a single cycle, which is what makes the cycle a
# meaningful unit; the 10x margin is then the usual headroom on top.
#
# The derivation reproduces exactly the 600 that was previously chosen by
# measurement, so this costs no CI wall-clock and no job budget. What changes is
# that the number now follows the configuration instead of having to be found
# and re-tuned by hand when the configuration moves.
#
# A cap UNDER the real delivery time cancels every delivery and the schedule
# never advances — the v0.2.2 incident, reproduced by configuration. That is what
# the 60s floor below defends.
TRIGGER_DAEMON_CAP_FIRE_MULTIPLE="${TRIGGER_DAEMON_CAP_FIRE_MULTIPLE:-10}"

trigger_daemon_watchdog_secs() {
  local derived watchdog
  # A cron this lib cannot parse must not silently produce a bound. The refusal
  # propagates: fire_interval rc=3 -> empty -> the default branch below is a
  # non-numeric, which falls through to... nothing safe. So refuse loudly here
  # too rather than invent a number.
  derived=$(trigger_daemon_fire_interval_secs) || return 3
  case "$TRIGGER_DAEMON_CAP_FIRE_MULTIPLE" in ''|*[!0-9]*|0) return 3 ;; esac
  derived=$((derived * TRIGGER_DAEMON_CAP_FIRE_MULTIPLE))
  watchdog="${TRIGGER_DAEMON_WATCHDOG_SECS:-$derived}"
  case "$watchdog" in ''|*[!0-9]*|0) watchdog="$derived" ;; esac
  # FLOOR. `0` and garbage already fell back to the default; a positive-but-
  # absurd value did not, and it had three consequences, all of them a guard
  # that passes while measuring against a declared lie:
  #   - the derivation. TRIGGER_DAEMON_WATCHDOG_SECS=1 derived a 3s backstop.
  #   - the refusal, which is a RELATIVE comparison and so waved through any
  #     positive override at all — a 30s DELIVER backstop included.
  #   - and now the injection: this number IS the daemon's and the runtime's
  #     real cancel bound, so a 1 would tell the daemon to abandon every
  #     delivery after one second — the v0.2.2 cancel/retry loop, reproduced by
  #     configuration rather than by a bug.
  # 60s is the floor because no real delivery completes under it; the measured
  # ones take 18.7-47.7s. Clamping rather than refusing keeps this a FLOOR:
  # every reader — derivation, refusal and injection alike — then sees the same
  # honest number instead of three different ones.
  [ "$watchdog" -ge 60 ] || watchdog=60
  printf '%s' "$watchdog"
}

# trigger_daemon_delivery_cap_env
#
# The workspace env, one KEY=VALUE per line, that pins the daemon's AND the
# runtime's delivery cancel bounds to `trigger_daemon_watchdog_secs`. Emitted
# from here rather than typed at the provision call site for the same reason the
# watchdog is a function: a second copy of the number is a second thing to
# forget, and the whole point of this pair is that the number the backstop is
# derived from and the number the daemon actually enforces are THE SAME NUMBER.
#
# Both keys are required — see the REPORTED-CAP-WINS note above. Emitting only
# the daemon key would leave the ordinary (not-attributable) branch bounded at
# the runtime's 3600s default and the inequality would stay false while looking
# fixed.
#
# MOLECULE_MAX_TURN_SECONDS rather than A2A_COMPLETION_IDLE_TIMEOUT_SECONDS is
# deliberate. Both move the runtime's absolute cap, but the idle knob moves it
# only via the 4x multiple, which means dividing the executor's per-event idle
# cap by four as a side effect — at 600s that would put a real LLM turn on a
# 150s leash, ~3x the measured worst case. MOLECULE_MAX_TURN_SECONDS moves the
# absolute cap ALONE and leaves the idle cap at its 900s default (~19x measured).
# Its one side effect is that `turn_is_alive_despite_idle` can no longer rescue a
# turn past the idle cap, since the cap is now below the TTL — reaching that
# needs 900s of runtime-event silence, which every other budget in this harness
# (a 90s A2A turn, a 360s idle digest) already calls a hard failure.
#
# MOLECULE_TRIGGER_POLL_SECONDS rides along for the same reason: the tick-wedge
# threshold is derived from the daemon's tick cadence, so the cadence the daemon
# actually runs and the one the threshold is computed from must be one value
# emitted once. It used to be typed at the provision site while the threshold was
# derived from a DIFFERENT knob entirely (the harness's observation poll) — two
# numbers that agreed only by coincidence at their defaults.
trigger_daemon_delivery_cap_env() {
  local cap tick
  cap=$(trigger_daemon_watchdog_secs) || return 3
  tick=$(trigger_daemon_tick_interval_secs)
  printf 'MOLECULE_TRIGGER_DELIVERY_ABSOLUTE_CAP_SECONDS=%s\n' "$cap"
  printf 'MOLECULE_MAX_TURN_SECONDS=%s\n' "$cap"
  printf 'MOLECULE_TRIGGER_POLL_SECONDS=%s\n' "$tick"
}

# ─── DELIVERY-STALL threshold: absence of attempt-log advancement ────────────
#
# trigger_daemon_progress_stall_secs [observation-poll-secs]
#
# How long the daemon's attempt log may stay frozen before the delivery lane is
# declared stalled. This is NOT a budget for the work: it is reset by every
# observed advancement, so a lane that keeps finishing attempts never approaches
# it however long the run lasts.
#
# It is sized so that the DAEMON always gets first crack at its own wedge, which
# is the correct nesting for any two timeouts on one path — the inner one must be
# able to fire, act, and have its effect OBSERVED before the outer one gives up:
#
#     stall = cap                     the daemon's own absolute cancel bound
#           + fire_interval           slack so a cancel landing just after a tick
#                                     boundary is still inside the window
#           + tick_interval           up to one tick before check_watchdog runs
#           + observation_poll        up to one harness poll before we see the
#                                     history entry the cancellation wrote
#
# At defaults: 600 + 60 + 10 + 10 = 680s. Every term is a configured interval, so
# the ordering cap < stall holds by construction rather than by arithmetic luck —
# there is no pair of settings under which the harness can declare a stall before
# the daemon has had the chance to cancel and re-queue. That is the exact
# inversion the 210-vs-600 pair had, in the opposite direction: there the OUTER
# bound cut first and the inner one it existed to accommodate was unreachable.
trigger_daemon_progress_stall_secs() {
  local poll="${1:-10}" cap fire tick
  case "$poll" in ''|*[!0-9]*|0) poll=10 ;; esac
  cap=$(trigger_daemon_watchdog_secs) || return 3
  fire=$(trigger_daemon_fire_interval_secs) || return 3
  tick=$(trigger_daemon_tick_interval_secs)
  printf '%s' "$((cap + fire + tick + poll))"
}

# The scheduler tag whose source the two env names and the four-branch model
# above were READ AT — not guessed from, not inferred from a version string.
#
# Everything here is only as true as that reading. If core repins the scheduler
# and the new version renames a knob, adds a fifth disposition, or stops
# preferring the runtime's reported cap, the injection silently stops bounding
# anything and the backstop goes back to exceeding a number nobody enforces —
# which is precisely the failure mode being fixed, one version later. So the
# unit test compares this against core's live pin and REDS on a bump, forcing a
# re-read rather than letting the derivation rot in place.
trigger_daemon_scheduler_version_validated() {
  printf '%s' 'v0.2.3'
}

# The backstop is a never-hit safety net, so it must EXCEED the delivery cancel
# bound above — otherwise the wait expires while a wedged fire is still being
# re-queued for its retry, which is exactly how the old 360s budget could never
# observe a recovery. 3x leaves room for the cancel AND at least two full retry
# cycles inside the wait.
trigger_daemon_backstop_secs() {
  local cap
  cap=$(trigger_daemon_watchdog_secs) || return 3
  printf '%s' "$((cap * 3))"
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
  watchdog=$(trigger_daemon_watchdog_secs) || return 3

  if [ -z "$override" ]; then
    trigger_daemon_backstop_secs
    return 0
  fi

  case "$override" in *[!0-9]*) return 3 ;; esac
  [ "$override" -gt "$watchdog" ] || return 3
  printf '%s' "$override"
}

# ─── the timeout inventory, and the proof that every entry can be REACHED ────
#
# The 210-vs-600 defect was not a wrong number. It was an ORDERING defect that no
# reader could see, because the two bounds lived in different files and nothing
# ever compared them: the outer bound (a 210s DELIVER backstop, sized as "about
# three fire cycles plus slack") expired before the inner one (a 600s delivery
# cancel) could fire, so the retry the backstop existed to accommodate was
# unreachable — the extra fire cycles it was bought to allow were exactly what
# its own literal cut off. A timeout that cannot be reached is not a safety net;
# it is dead code shaped like one, and it reads as deliberate forever.
#
# So the inventory is DATA, and the ordering is ASSERTED. Every bound on the
# trigger delivery path is listed here with its owner and its disposition, and
# `trigger_daemon_timeout_ordering_check` refuses any arrangement in which a
# bound cannot be reached.
#
# THE FAILURE THIS CANNOT CATCH, stated plainly rather than papered over: a NEW
# timeout added to the daemon or the runtime and never added here. Nothing in
# this repo can see that directly — the daemon source lives in
# molecule-ai-plugin-scheduler and the runtime in molecule-ai-workspace-runtime,
# so there is no file here to grep for a cap-shaped env name, and an offline
# unit test cannot fetch one.
#
# What DOES cover it is the version pin: `trigger_daemon_scheduler_version_
# validated` is compared against core's live pin in
# plugin_registry_test.go, and the unit test REDS the moment they diverge. A new
# daemon timeout can only reach a workspace through a repin, and a repin cannot
# land without turning this red first, which forces the re-read that adds the
# row. That is a weaker guarantee than a direct grep and it is the honest one:
# it catches the version moving, not the content, so the re-read is the control.
#
# DISPOSITIONS. Exactly two are legal:
#   reachable        nothing on the path cuts before it, so it can actually fire.
#   subsumed-by:NAME a strictly TIGHTER bound at the same layer always fires
#                    first, so this one is unreachable ON THIS PATH by design.
#                    Legal only when the named subsumer is strictly SMALLER —
#                    which makes it the opposite of the 210 defect, where a
#                    LARGER outer bound was cut off by a SMALLER one it was
#                    supposed to contain.
#
# The one subsumed entry is the runtime's 900s idle TTL. On the delivery path
# `classify_delivery_liveness` tests `absolute_cap_exceeded` BEFORE `idle_expired`
# (read at the validated pin), and `idle_seconds <= turn_age_seconds` always
# holds, so idle > 900 implies turn_age > 900 > cap: `CAUSE_IDLE` can never be
# the verdict while the injected cap is below the TTL. That is intentional — the
# absolute cap is the tighter, un-bypassable bound and the harness has no
# business retuning a runtime-wide idle policy — but it must be DECLARED, not
# assumed, or a future reader counts CAUSE_IDLE as live protection it does not
# have. If someone raises the cap above the TTL the subsumption inverts, the
# check below fails, and the re-read is forced.
#
# THE ONE BOUND THIS LEDGER DOES NOT MODEL is the CI job timeout, and it is
# named here rather than left off the list, because "a bound nobody enumerated"
# is how the 210 survived. `.gitea/workflows/e2e-ephemeral-happy-path.yml` sets
# `timeout-minutes: 75` (4500s), which is the true outermost layer: if it were
# ever below the 1800s backstop, the backstop would be unreachable and every
# derived number below it would be decoration.
#
# It is not a ledger ROW because making it one honestly would mean each lane
# passing its own `timeout-minutes` in as env, and there is more than one lane
# running this harness — a row wired from a single workflow would assert the
# bound for one lane and silently assert NOTHING for the others, which is a
# guard that looks like it covers the path and does not. The inequality that
# holds today: at most ONE backstop is reachable per run (every leg `fail`s
# rather than continuing, so reaching a later backstop means the earlier legs
# returned on their signal in seconds), so worst case is 1800s of backstop
# inside 4500s of job. Anyone shortening `timeout-minutes` below ~2x the
# backstop must revisit this.
#
# Read at the validated scheduler pin; the runtime constant is
# molecule_runtime/turn_lease.py `_DEFAULT_IDLE_CAP_SECONDS`.
TRIGGER_DAEMON_RUNTIME_IDLE_TTL_SECS="${TRIGGER_DAEMON_RUNTIME_IDLE_TTL_SECS:-900}"

# trigger_daemon_timeout_ledger [observation-poll-secs]
#
# One row per bound, in the order they sit on the path, as
#   name|seconds|owner|disposition
trigger_daemon_timeout_ledger() {
  local poll="${1:-10}" tick stale cap stall backstop
  case "$poll" in ''|*[!0-9]*|0) poll=10 ;; esac
  tick=$(trigger_daemon_tick_interval_secs)
  stale=$(trigger_daemon_stale_secs "$poll")
  cap=$(trigger_daemon_watchdog_secs) || return 3
  stall=$(trigger_daemon_progress_stall_secs "$poll") || return 3
  backstop=$(trigger_daemon_backstop_secs) || return 3

  printf 'daemon_tick_interval|%s|daemon|reachable\n' "$tick"
  printf 'harness_tick_wedge|%s|harness|reachable\n' "$stale"
  printf 'delivery_absolute_cap|%s|daemon+runtime|reachable\n' "$cap"
  printf 'runtime_idle_ttl|%s|runtime|subsumed-by:delivery_absolute_cap\n' "$TRIGGER_DAEMON_RUNTIME_IDLE_TTL_SECS"
  printf 'harness_delivery_stall|%s|harness|reachable\n' "$stall"
  printf 'harness_backstop|%s|harness|reachable\n' "$backstop"
}

# The number of rows the ledger MUST have. Not a style check — see the counter
# guard in `trigger_daemon_timeout_ordering_check`. Bump it in the same commit
# that adds or removes a row, which is the point: the bump is the review.
TRIGGER_DAEMON_LEDGER_ROWS="${TRIGGER_DAEMON_LEDGER_ROWS:-6}"

# trigger_daemon_timeout_ordering_check [observation-poll-secs]
#
# rc=0 when every ledger entry can be reached; rc=1 otherwise, with the offending
# rows on stdout. Deliberately does NOT use `set -e`-dependent control flow: this
# is called from a harness that runs under errexit from its Gitea Actions
# invocation (`bash --noprofile --norc -e -o pipefail`), where a non-zero
# intermediate would kill the caller before it could print the diagnosis. Every
# failure here accumulates into a report and comes back as one rc.
#
# ─── THE COUNTER GUARD, and why this function needed one at all ──────────────
#
# Every ordering rule below is a rule ABOUT ROWS. Feed it zero rows and every one
# of them is vacuously satisfied: the loop body never runs, `rc` stays 0, and the
# function reports "every bound can be reached" having examined nothing. A
# truncated ledger is the same defect one step milder — the surviving rows are
# checked, the missing ones are not, and the pass looks identical.
#
# That is precisely the failure class this whole change exists to close, so it
# would be absurd to leave it in the guard that closes it. This is the same
# treatment `trigger_daemon_wait` gets from
# TRIGGER_DAEMON_PROGRESS_SAMPLES: assert the thing actually ran on something,
# and REPORT the count so a reader can tell an armed check from an empty one.
#
# The count is printed on stdout on success too (prefixed `timeout-ledger:
# checked N row(s)`) — a silent pass is exactly what let the empty case hide.
trigger_daemon_timeout_ordering_check() {
  local poll="${1:-10}" ledger rc=0 prev_name="" prev_secs=0 name secs owner disp sub sub_secs
  local rows=0 reachable=0

  ledger=$(trigger_daemon_timeout_ledger "$poll") || {
    printf 'timeout-ledger: could not be computed (a bound refused to derive)\n'
    return 1
  }

  # FAIL CLOSED ON EMPTY, before any rule runs. An empty ledger is not "an
  # arrangement with no violations", it is an arrangement nobody described.
  rows=$(printf '%s\n' "$ledger" | grep -c '|' 2>/dev/null || printf 0)
  case "$rows" in ''|*[!0-9]*) rows=0 ;; esac
  if [ "$rows" -eq 0 ]; then
    printf 'timeout-ledger: EMPTY — 0 rows to check, so every ordering rule below passes vacuously and this guard asserted NOTHING. Refusing rather than reporting a green that means "nobody described the path".\n'
    return 1
  fi
  # ...and on TRUNCATED. Rows can only appear/disappear by editing the ledger, so
  # a mismatch is either a deliberate change (bump TRIGGER_DAEMON_LEDGER_ROWS in
  # the same commit) or a bound that silently fell off the path.
  if [ "$rows" -ne "$TRIGGER_DAEMON_LEDGER_ROWS" ]; then
    printf 'timeout-ledger: expected %s row(s), got %s — a bound was added or lost without review. If deliberate, bump TRIGGER_DAEMON_LEDGER_ROWS in the same commit; if not, a timeout on this path is no longer being checked at all.\n' \
      "$TRIGGER_DAEMON_LEDGER_ROWS" "$rows"
    rc=1
  fi

  while IFS='|' read -r name secs owner disp; do
    [ -n "$name" ] || continue
    case "$secs" in ''|*[!0-9]*|0)
      printf 'timeout-ledger: %s has a non-positive/non-numeric bound %s\n' "$name" "$secs"
      rc=1; continue ;;
    esac
    case "$disp" in
      reachable)
        # Reachable bounds must be STRICTLY increasing along the path. Equality
        # is not good enough: two bounds that expire at the same instant race,
        # and the outer one wins often enough that the inner is unreachable in
        # practice while looking fine in the inventory.
        if [ -n "$prev_name" ] && [ "$secs" -le "$prev_secs" ]; then
          printf 'timeout-ledger: %s (%ss, %s) is not STRICTLY greater than the bound before it, %s (%ss) — the inner bound can never fire; this is the 210-vs-600 inversion\n' \
            "$name" "$secs" "$owner" "$prev_name" "$prev_secs"
          rc=1
        fi
        prev_name="$name"; prev_secs="$secs"
        reachable=$((reachable + 1))
        ;;
      subsumed-by:*)
        sub="${disp#subsumed-by:}"
        sub_secs=$(printf '%s\n' "$ledger" | awk -F'|' -v n="$sub" '$1==n{print $2}')
        if [ -z "$sub_secs" ]; then
          printf 'timeout-ledger: %s claims to be subsumed by %s, which is not in the ledger\n' "$name" "$sub"
          rc=1
        elif [ "$sub_secs" -ge "$secs" ]; then
          printf 'timeout-ledger: %s (%ss) claims to be subsumed by %s (%ss), but the subsumer is NOT strictly tighter — %s is now reachable and its disposition is a lie\n' \
            "$name" "$secs" "$sub" "$sub_secs" "$name"
          rc=1
        fi
        ;;
      *)
        printf 'timeout-ledger: %s has disposition %q — only "reachable" or "subsumed-by:NAME" are legal, so a bound nobody classified cannot slip onto the path\n' "$name" "$disp"
        rc=1
        ;;
    esac
  done <<EOF
$ledger
EOF

  # A ledger of nothing but `subsumed-by` rows would clear the loop with the
  # strictly-increasing chain never once evaluated — rows present, ordering still
  # unasserted. The chain needs at least two reachable bounds to BE a chain.
  if [ "$reachable" -lt 2 ]; then
    printf 'timeout-ledger: only %s reachable row(s) — the strictly-increasing ordering was never actually compared against anything. Rows alone are not a checked ordering.\n' "$reachable"
    rc=1
  fi

  if [ "$rc" -eq 0 ]; then
    printf 'timeout-ledger: checked %s row(s), %s reachable, ordering strictly increasing\n' "$rows" "$reachable"
  fi
  return "$rc"
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
