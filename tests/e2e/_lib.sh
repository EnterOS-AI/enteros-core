#!/usr/bin/env bash
# Common E2E helpers. Source this from every tests/e2e/*.sh.
#
# Usage:
#   source "$(dirname "$0")/_lib.sh"
#   e2e_cleanup_all_workspaces   # call at top of script
#   TOKEN=$(echo "$register_response" | e2e_extract_token)
#
# BASE defaults to http://localhost:8080. Set it before sourcing to override.

: "${BASE:=http://localhost:8080}"
export BASE

# Emit the auth_token from a /registry/register response on stdout.
# See _extract_token.py for the exact semantics.
e2e_extract_token() {
  python3 "$(dirname "${BASH_SOURCE[0]}")/_extract_token.py"
}

# Delete every workspace currently on the platform. Use at the top of a
# script so count-based assertions are reproducible across runs.
# Mint a fresh workspace auth token via the real admin endpoint.
#
# Usage:
#   TOKEN=$(e2e_mint_workspace_token "$workspace_id") || exit 1
e2e_mint_workspace_token() {
  local wid="$1"
  if [ -z "$wid" ]; then
    echo "e2e_mint_workspace_token: workspace id required" >&2
    return 2
  fi
  local body
  local admin_bearer="${MOLECULE_ADMIN_TOKEN:-${ADMIN_TOKEN:-}}"
  local admin_auth=()
  [ -n "$admin_bearer" ] && admin_auth=(-H "Authorization: Bearer $admin_bearer")
  body=$(curl -s -X POST -w "\n%{http_code}" "$BASE/admin/workspaces/$wid/tokens" ${admin_auth[@]+"${admin_auth[@]}"})
  local code
  code=$(printf '%s' "$body" | tail -n1)
  local json
  json=$(printf '%s' "$body" | sed '$d')
  if [ "$code" != "201" ]; then
    echo "e2e_mint_workspace_token: got HTTP $code from POST /admin/workspaces/:id/tokens" >&2
    return 1
  fi
  printf '%s' "$json" | python3 -c "import json,sys; print(json.load(sys.stdin)['auth_token'])"
}

e2e_delete_workspace() {
  local wid="$1"
  local name="${2:-}"
  shift 2 || true
  local curl_args=("$@")
  if [ -z "$wid" ]; then
    return 0
  fi
  if [ -z "$name" ]; then
    name=$(curl -s "$BASE/workspaces/$wid" "${curl_args[@]}" | python3 -c "import json,sys
try:
  print(json.load(sys.stdin).get('name',''))
except Exception:
  pass" 2>/dev/null || true)
  fi
  curl -s -X DELETE "$BASE/workspaces/$wid?confirm=true" \
    -H "X-Confirm-Name: $name" "${curl_args[@]}" > /dev/null || true
}

e2e_cleanup_all_workspaces() {
  curl -s "$BASE/workspaces" | python3 -c "import json,sys
try:
  [print(f\"{w.get('id','')}\\t{w.get('name','')}\") for w in json.load(sys.stdin)]
except Exception:
  pass" 2>/dev/null | while IFS=$'\t' read -r _wid _name; do
    e2e_delete_workspace "$_wid" "$_name"
  done
}
