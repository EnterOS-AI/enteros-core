#!/usr/bin/env bash
# Resolve a molecules-server workspace container from the canonical UUID suffix.
#
# The tenant API can report status=online before it exposes instance_id. The
# live reconciler E2E therefore asks the provider control surface for the exact
# `mol-ws-...-<first 12 UUID hex>` container. A missing container is a normal,
# retryable state; this helper intentionally prints nothing and returns 0 for
# both no-match and a transient `docker ps` failure.

resolve_molecules_server_container() {
  local workspace_id="${1:-}"
  local compact_id ws_fragment docker_names

  compact_id=$(printf '%s' "$workspace_id" | tr '[:upper:]' '[:lower:]')
  compact_id=${compact_id//-/}
  case "$compact_id" in
    ""|*[![:xdigit:]]*) return 0 ;;
  esac
  [ "${#compact_id}" -eq 32 ] || return 0
  ws_fragment=${compact_id:0:12}

  if ! docker_names=$(docker ps --format '{{.Names}}' 2>/dev/null); then
    return 0
  fi

  printf '%s\n' "$docker_names" | awk -v suffix="-$ws_fragment" '
    index($0, "mol-ws-") == 1 &&
    length($0) >= length(suffix) &&
    substr($0, length($0) - length(suffix) + 1) == suffix {
      print
      exit
    }
  '
}
