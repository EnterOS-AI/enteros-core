#!/usr/bin/env bash

# Wait for a workspace CONTAINER to actually be running — the thing container
# assertions depend on — instead of trusting the control plane's status as a
# proxy for it (core#4980).
#
# WHY THIS EXISTS. A workspace status of `online` and the container being up are
# SEPARATE events: core flips the row when the agent registers, and on a loaded
# runner the container can still be a moment behind (or, on a restart, a moment
# ahead of its replacement). The self-host concierge schedules lane asserted
# `docker ps` the instant it read `online` and reddened a REQUIRED context on a
# comment-only PR — five seconds after provisioning, container not yet visible.
# Re-running was the only remedy, which trains readers to skip the log.
#
# WHY THIS IS NOT A SLEEP. A bounded wait that can only ever pass is worse than
# the bug it hides, so the states are triaged rather than merely retried:
#   * running          -> pass immediately, no residual soak on a fast run.
#   * exited | dead    -> TERMINAL. The container started and died; more waiting
#                         cannot change that, so fail FAST and say so. This is
#                         the arm that keeps a genuinely dead container red.
#   * created|restarting|paused|absent -> still converging, keep polling to the
#                         bound, then fail with a DISTINCT "never appeared"
#                         message so the two failures are never confused.
#
# Offline-testable: the state is read through CONTAINER_WAIT_STATE_CMD when set,
# so lib/test_container_running_wait_unit.sh drives every arm without Docker.

# The last docker state observed by wait_container_running ("" when the
# container was absent/unreadable). Exported because it is a PUBLIC OUTPUT of
# this lib: callers put it straight into their failure message so the operator
# reads "last state=created" rather than a bare "timed out".
export CONTAINER_WAIT_LAST_STATE="${CONTAINER_WAIT_LAST_STATE:-}"

# container_running_state <container-name>
# Echoes docker's State.Status for the EXACT container name, or "" when the
# container does not exist (or the daemon is transiently unreadable). Absent is
# deliberately not distinguished from unreadable: both are retryable, and
# treating a docker CLI blip as a hard failure is the flake this lib removes.
container_running_state() {
  local name="${1:-}" state
  [ -n "$name" ] || { printf ''; return 0; }

  if [ -n "${CONTAINER_WAIT_STATE_CMD:-}" ]; then
    state=$($CONTAINER_WAIT_STATE_CMD "$name" 2>/dev/null) || state=""
  else
    # `container inspect` (not bare `docker inspect`) so an identically named
    # image can never answer for a container, and the lookup stays exact-name.
    state=$(docker container inspect --format '{{.State.Status}}' "$name" 2>/dev/null) || state=""
  fi
  printf '%s' "$state"
}

# wait_container_running <container-name> <timeout-secs> <poll-secs>
#
#   0 = running (asserted, not assumed)
#   1 = never reached running within <timeout-secs> (absent or still starting)
#   2 = TERMINAL: the container exists but is exited/dead — fail fast
#   3 = usage error
#
# Sets CONTAINER_WAIT_LAST_STATE to the last state seen. The state is probed
# immediately and once more at the exact deadline, so a fast run pays nothing
# and evidence landing in a shortened final interval is not missed.
wait_container_running() {
  local name="${1:-}" timeout_secs="${2:-}" poll_secs="${3:-}"
  local elapsed=0 sleep_secs remaining state
  CONTAINER_WAIT_LAST_STATE=""

  case "$timeout_secs" in
    ''|*[!0-9]*) return 3 ;;
  esac
  case "$poll_secs" in
    ''|*[!0-9]*|0) return 3 ;;
  esac
  [ -n "$name" ] || return 3

  while true; do
    state=$(container_running_state "$name")
    CONTAINER_WAIT_LAST_STATE="$state"
    case "$state" in
      running)    return 0 ;;
      exited|dead) return 2 ;;
    esac

    [ "$elapsed" -ge "$timeout_secs" ] && return 1

    remaining=$((timeout_secs - elapsed))
    sleep_secs=$poll_secs
    [ "$sleep_secs" -le "$remaining" ] || sleep_secs=$remaining
    sleep "$sleep_secs"
    elapsed=$((elapsed + sleep_secs))
  done
}
