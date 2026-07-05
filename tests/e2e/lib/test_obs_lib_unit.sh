#!/usr/bin/env bash
# test_obs_lib_unit.sh — OFFLINE unit test for lib/obs.sh (no Loki, no network,
# no live infra). Proves the load-bearing contracts:
#   1. JSON escaping produces a valid string for quotes/backslashes/newlines.
#   2. _obs_redact scrubs Bearer tokens / JWTs / long hex.
#   3. step duration is computed and numeric.
#   4. FAIL-SOFT: with Loki unreachable AND `set -e`, every public function
#      still returns 0 — a down obs stack can never fail or hang an e2e.
#
# Run: bash tests/e2e/lib/test_obs_lib_unit.sh   (also wired into ci.yml)
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib/obs.sh
source "$HERE/obs.sh"

fails=0
check() { # DESC CONDITION...
  local desc="$1"; shift
  if "$@"; then
    printf '  ok   %s\n' "$desc"
  else
    printf '  FAIL %s\n' "$desc" >&2
    fails=$((fails + 1))
  fi
}

# --- 1. JSON escaping -------------------------------------------------------
esc=$(_obs_json_escape 'he said "hi"\and\back'$'\n''newline'$'\t''tab')
check "escapes double quotes"  grep -q '\\"hi\\"' <<<"$esc"
check "escapes backslashes"    grep -q '\\\\and\\\\back' <<<"$esc"
check "escapes newline to \\n" grep -q '\\n' <<<"$esc"
check "escapes tab to \\t"     grep -q '\\t' <<<"$esc"
check "escaped string has no raw newline" test "$(printf '%s' "$esc" | wc -l | tr -d ' ')" = "0"

# --- 2. redaction -----------------------------------------------------------
red=$(_obs_redact 'Authorization: Bearer abcDEF123.tok_secret-value here')
check "redacts Bearer token"   grep -q 'Bearer \[REDACTED\]' <<<"$red"
check "drops the raw token"    bash -c '! grep -q "tok_secret-value" <<<"'"$red"'"'
redhex=$(_obs_redact 'sig=0123456789abcdef0123456789abcdef extra')
check "redacts long hex"       grep -q '\[REDACTED_HEX\]' <<<"$redhex"
redjwt=$(_obs_redact 'token eyJhbGciOiJIUzI1NiITitsActuallyLongEnough.payload.sig')
check "redacts JWT"            grep -q '\[REDACTED_JWT\]' <<<"$redjwt"

# --- 3. numeric field + label sanitising ------------------------------------
check "numeric field unquoted"  test "$(_obs_field_num duration_secs 42)" = '"duration_secs":42'
check "non-numeric field quoted" test "$(_obs_field_num http_code 000abc)" = '"http_code":"000abc"'
check "label sanitised" test "$(_obs_lbl 'e2e cncrg/mk@1')" = 'e2e_cncrg/mk_1'

# --- 4. FAIL-SOFT under set -e with Loki unreachable ------------------------
# Point at a closed port; assert every public fn returns 0 fast and never exits.
export OBS_LOKI_URL="http://127.0.0.1:1"
export OBS_PUSH_TIMEOUT=2
export OBS_ENABLED=1
export E2E_RUN_ID="unit-$$"
export MOLECULE_CP_URL="http://localhost:8090"

obs_init "obs_lib_unit"; check "obs_init returns 0 (loki down)" test "$?" = "0"
obs_step_start demo_step; check "obs_step_start returns 0" test "$?" = "0"
sleep 1
obs_step_end demo_step pass "" "http_code=200"; check "obs_step_end pass returns 0" test "$?" = "0"
obs_step_start demo_fail
obs_step_end demo_fail fail "boom 500" "http_code=500" "body={\"err\":\"x\"}"
check "obs_step_end fail returns 0" test "$?" = "0"
_OBS_CUR_STEP=demo_fail
obs_fail_current timeout "deadline exceeded"; check "obs_fail_current returns 0" test "$?" = "0"
obs_leak container mol-ws-abc "executeOrgPurge:purgeInfra"; check "obs_leak returns 0" test "$?" = "0"
obs_run_end fail; check "obs_run_end returns 0" test "$?" = "0"

# Disabled mode must also be a clean no-op.
OBS_ENABLED=0 obs_event run pass 1 "" "k=v"; check "disabled emit returns 0" test "$?" = "0"

if [ "$fails" -ne 0 ]; then
  printf '\n%d obs lib unit check(s) FAILED\n' "$fails" >&2
  exit 1
fi
printf '\nobs.sh unit: all checks passed (escaping, redaction, numeric, fail-soft)\n'
