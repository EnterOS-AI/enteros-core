#!/usr/bin/env bash
# Bounded 10f create_schedule retry through the scheduler capability-cache race.
#
# The caller supplies two real behavior functions:
#   self_schedule_invoke_create <args-json>  -> sets _SS_CLASS and _SS_ID
#   self_schedule_own_grid_has <name>        -> OWN volume-grid evidence
#
# Return 0 only with grid evidence, 1 after a bounded unresolved race, and 2 for
# invalid budgets or a real tool/auth error. A generated UUID is the only signal
# that permits reinvocation: it proves core accepted the create into the retired
# DB while its capability cache was stale. id==name is volume-routing evidence,
# and any visible grid entry is authoritative, so both switch to poll-only.
self_schedule_create_until_own_grid() {
  local args_json="${1:-}" expected_name="${2:-}"
  local elapsed=0 do_create=1 why="" next="" remaining sleep_secs

  _SS_CREATE_DETAIL=""
  case "${_SS_CREATE_TIMEOUT_SECS:-}" in
    ''|*[!0-9]*) _SS_CREATE_DETAIL="invalid create timeout"; return 2 ;;
  esac
  case "${_SS_POLL_SECS:-}" in
    ''|*[!0-9]*|0) _SS_CREATE_DETAIL="invalid create poll interval"; return 2 ;;
  esac
  if [ -z "$args_json" ] || [ -z "$expected_name" ]; then
    _SS_CREATE_DETAIL="missing create arguments or expected name"
    return 2
  fi

  while true; do
    # Check before every invocation. If a prior attempt became visible during
    # the poll interval, grid truth wins and no duplicate volume create is sent.
    if self_schedule_own_grid_has "$expected_name"; then
      _SS_CREATE_DETAIL="confirmed on OWN volume grid"
      return 0
    fi

    if [ "$do_create" = "1" ]; then
      self_schedule_invoke_create "$args_json"
      if [ "${_SS_CLASS:-}" != "ok" ]; then
        _SS_CREATE_DETAIL="create returned class=${_SS_CLASS:-empty}"
        return 2
      fi
      do_create=0

      # A volume create writes synchronously, but read once immediately so fast
      # evidence completes without a fixed poll and before classifying the id.
      if self_schedule_own_grid_has "$expected_name"; then
        _SS_CREATE_DETAIL="confirmed on OWN volume grid"
        return 0
      fi
    fi

    if [[ "${_SS_ID:-}" =~ ^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$ ]]; then
      why="create mis-routed to the retired DB (id='$_SS_ID' is a UUID) while core's scheduler capability cache was stale"
      do_create=1
      next="reinvoking after capability propagation"
    elif [ "${_SS_ID:-}" = "$expected_name" ]; then
      why="create routed to the volume (id='$_SS_ID') but the OWN grid write is not visible yet"
      do_create=0
      next="re-polling without another create"
    else
      why="routing unclear (id='${_SS_ID:-empty}')"
      do_create=0
      next="re-polling without risking a duplicate create"
    fi
    _SS_CREATE_DETAIL="$why"

    if [ "$elapsed" -ge "$_SS_CREATE_TIMEOUT_SECS" ]; then
      return 1
    fi

    remaining=$((_SS_CREATE_TIMEOUT_SECS - elapsed))
    sleep_secs="$_SS_POLL_SECS"
    [ "$sleep_secs" -le "$remaining" ] || sleep_secs="$remaining"
    log "    10f: OWN grid absent — $why; $next (${elapsed}/${_SS_CREATE_TIMEOUT_SECS}s)."
    sleep "$sleep_secs"
    elapsed=$((elapsed + sleep_secs))
  done
}
