#!/usr/bin/env bash
# Capture curl's HTTP status without appending a fallback to stdout.
#
# Usage: capture_http_status STATUS_FILE command [args...]
# The command must emit only curl's `-w '%{http_code}'` value on stdout and
# direct its response body elsewhere with `-o`. Transport/malformed output is
# normalized once to 000. Results are returned in HTTP_CAPTURE_CODE and
# HTTP_CAPTURE_RC; fixed output names avoid Bash dynamic-scope collisions.

HTTP_CAPTURE_CODE="000"
HTTP_CAPTURE_RC="0"

capture_http_status() {
  local status_file="$1"
  shift

  local _capture_rc _capture_raw

  : > "$status_file"
  if "$@" > "$status_file" 2>/dev/null; then
    _capture_rc=0
  else
    _capture_rc=$?
  fi

  _capture_raw=$(tr -d '[:space:]' < "$status_file" 2>/dev/null || true)
  case "$_capture_raw" in
    [0-9][0-9][0-9]) ;;
    *) _capture_raw="000" ;;
  esac

  # shellcheck disable=SC2034 # sourced-function outputs consumed by caller
  HTTP_CAPTURE_CODE="$_capture_raw"
  # shellcheck disable=SC2034 # sourced-function outputs consumed by caller
  HTTP_CAPTURE_RC="$_capture_rc"
}

http_code_is_exact_success() {
  local code="${1:-}" request_rc="${2:-}" expected
  shift 2
  [ "$request_rc" = "0" ] || return 1
  for expected in "$@"; do
    [ "$code" = "$expected" ] && return 0
  done
  return 1
}

http_code_is_exact_removed_route() {
  [ "${1:-}" = "404" ] && [ "${2:-}" = "22" ]
}
