#!/usr/bin/env bash
# Unit test for lib/workspace_env_presence.sh — pure/offline (no LLM, no network).
# Stubs tenant_call to feed fixed GET /workspaces/:id JSON and asserts the
# present|absent|unobservable verdict. Locks the hardened contract from
# code-review #4032 (findings 3-5): ambiguous fields dropped, list-of-dicts
# handled, non-bool map values -> unobservable (never a silent absent/present).
set -uo pipefail
DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=workspace_env_presence.sh
source "$DIR/workspace_env_presence.sh"

PASS=0; FAIL=0
# tenant_call stub: ignores args, echoes $WS_JSON (the case fixture).
tenant_call() { printf '%s' "${WS_JSON:-}"; }

check() { # <label> <expected> <json>
  local label="$1" expected="$2"; WS_JSON="$3"
  local got; got=$(workspace_platform_llm_token_presence "ws-test")
  if [ "$got" = "$expected" ]; then
    echo "PASS: $label -> $got"; PASS=$((PASS + 1))
  else
    echo "FAIL: $label -> got '$got' expected '$expected'"; FAIL=$((FAIL + 1))
  fi
}

check "bool present"                 present      '{"llm_usage_token_present": true}'
check "bool absent"                  absent       '{"llm_usage_token_present": false}'
check "has_llm_usage_token present"  present      '{"has_llm_usage_token": true}'
check "ambiguous llm_auth_present dropped (finding7)" unobservable '{"llm_auth_present": true}'
check "ambiguous platform_llm_auth_present dropped"   unobservable '{"platform_llm_auth_present": true}'
check "list-of-strings has"          present      '{"provisioned_env_keys": ["FOO","MOLECULE_LLM_USAGE_TOKEN"]}'
check "list-of-strings absent"       absent       '{"provisioned_env_keys": ["FOO","BAR"]}'
check "list-of-dicts has (finding5)" present      '{"container_env_keys": [{"key":"MOLECULE_LLM_USAGE_TOKEN"}]}'
check "list-of-dicts absent"         absent       '{"container_env_keys": [{"key":"FOO"}]}'
check "list unknown-shape -> unobs"  unobservable '{"env_keys": [123, 456]}'
check "union: token in 2nd field (finding4)" present '{"provisioned_env_keys": ["FOO"], "container_env_keys": ["FOO","MOLECULE_LLM_USAGE_TOKEN"]}'
check "union: token in no field -> absent"   absent '{"provisioned_env_keys": ["FOO"], "container_env_keys": ["BAR"]}'
check "weird element, token still present (finding5)" present     '{"container_env_keys": ["MOLECULE_LLM_USAGE_TOKEN", 123]}'
check "weird element, token absent -> unobs (finding5)" unobservable '{"container_env_keys": ["FOO", 123]}'
check "empty list -> unobs (finding6)" unobservable '{"container_env_keys": []}'
check "env_key_presence bool true"   present      '{"env_key_presence": {"MOLECULE_LLM_USAGE_TOKEN": true}}'
check "env_key_presence bool false"  absent       '{"env_key_presence": {"MOLECULE_LLM_USAGE_TOKEN": false}}'
check "env_key_presence str 'false' -> unobs (finding6)" unobservable '{"env_key_presence": {"MOLECULE_LLM_USAGE_TOKEN": "false"}}'
check "env_key_presence object -> unobs (finding6)"      unobservable '{"env_key_presence": {"MOLECULE_LLM_USAGE_TOKEN": {"injected_at": 1}}}'
check "no fields (current build)"    unobservable '{"id":"ws","status":"running"}'
check "bad json"                     unobservable 'not json at all <html>404'
check "not a dict"                   unobservable '["a","b"]'
check "empty body"                   unobservable ''
check "precedence: bool wins over list" absent    '{"llm_usage_token_present": false, "provisioned_env_keys": ["MOLECULE_LLM_USAGE_TOKEN"]}'

# set-e-safety (finding 1): a --fail-with-body host makes tenant_call exit non-zero
# WHILE writing a body; the probe must return `unobservable` and NOT abort the
# caller under `set -e` (the old bare `tenant_call | python3` propagated curl's 22
# through pipefail and aborted the harness opaquely).
sete=$(
  set -e
  tenant_call() { printf '%s' '{"error":"gone"}'; return 22; }  # simulate curl --fail-with-body
  v=$(workspace_platform_llm_token_presence "ws-x")
  printf 'reached:%s' "$v"   # never printed if the probe aborted the subshell
)
if [ "$sete" = "reached:unobservable" ]; then
  echo "PASS: set-e-safe on --fail-with-body curl exit (finding1)"; PASS=$((PASS + 1))
else
  echo "FAIL: set-e-safety got '$sete' expected 'reached:unobservable'"; FAIL=$((FAIL + 1))
fi

echo "----"
echo "workspace_env_presence unit: PASS=$PASS FAIL=$FAIL"
[ "$FAIL" -eq 0 ] || exit 1
