#!/usr/bin/env bash

# Poll a caller-supplied evidence function until it succeeds or the bounded
# timeout is exhausted. The evidence function is invoked immediately and once
# more at the exact deadline, so callers neither pay a fixed soak on fast runs
# nor miss evidence that lands during a shortened final interval.
#
# Usage: idle_digest_wait <timeout-seconds> <poll-seconds> <probe-fn> [args...]
idle_digest_wait() {
  local timeout_secs="${1:-}" poll_secs="${2:-}" probe_fn="${3:-}"
  local elapsed=0 sleep_secs remaining

  case "$timeout_secs" in
    ''|*[!0-9]*) return 2 ;;
  esac
  case "$poll_secs" in
    ''|*[!0-9]*|0) return 2 ;;
  esac
  [ -n "$probe_fn" ] || return 2
  shift 3

  while true; do
    if "$probe_fn" "$@"; then
      return 0
    fi
    [ "$elapsed" -ge "$timeout_secs" ] && return 1

    remaining=$((timeout_secs - elapsed))
    sleep_secs=$poll_secs
    [ "$sleep_secs" -le "$remaining" ] || sleep_secs=$remaining
    sleep "$sleep_secs"
    elapsed=$((elapsed + sleep_secs))
  done
}

# The idle-digest assertion reads live container logs and durable goal state.
# When the assertion is enabled, both the Docker CLI and a reachable daemon are
# required; callers must treat a nonzero return as a hard configuration error.
idle_digest_require_docker() {
  command -v docker >/dev/null 2>&1 || return 1
  docker ps >/dev/null 2>&1
}
