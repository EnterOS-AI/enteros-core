#!/usr/bin/env bash
# Unit test for ephemeral_cp_happy_path.sh's model guard (require_platform_model).
#
# E2E_PLATFORM_MODEL becomes the throwaway CP's platform default, so it is what
# the concierge and every agent it creates run on. The lane must fail before
# anything boots when that model is unset or is Gemini 4 Hydrogen/Carbon (owner
# 2026-11-17: the cheapest model for any e2e testing), and the boot env must
# always carry it as MOLECULE_LLM_DEFAULT_MODEL (an unset value used to skip the
# line, leaving the CP on its gemini-4-hydrogen fallback).
#
# Sources the script (its dispatch is guarded, so sourcing boots nothing).
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$HERE/ephemeral_cp_happy_path.sh"

# shellcheck source=ephemeral_cp_happy_path.sh disable=SC1091
source "$SCRIPT"

fails=0
pass() { echo "  PASS: $1"; }
failc() { echo "  FAIL: $1"; fails=$((fails + 1)); }

expect() { # expect <desc> <want-rc> <model-or-UNSET>
  local desc="$1" want="$2" model="$3" rc out
  if [ "$model" = "UNSET" ]; then
    out="$( (unset E2E_PLATFORM_MODEL; require_platform_model) 2>&1 )"; rc=$?
  else
    out="$( (E2E_PLATFORM_MODEL="$model"; require_platform_model) 2>&1 )"; rc=$?
  fi
  if [ "$rc" = "$want" ]; then pass "$desc (rc=$rc)"; else failc "$desc: rc=$rc want $want — $out"; fi
  if [ "$want" != "0" ]; then
    case "$out" in
      *"FATAL: E2E_PLATFORM_MODEL refused"*) pass "$desc names the variable" ;;
      *) failc "$desc: no FATAL naming E2E_PLATFORM_MODEL in: $out" ;;
    esac
  fi
}

echo "require_platform_model"
expect "unset is refused"                2 UNSET
expect "empty is refused"                2 ""
expect "Hydrogen is refused"             2 google/gemini-4-hydrogen
expect "Carbon is refused"               2 google/gemini-4-carbon
expect "Flash-Lite (gemini mode) passes" 0 google/gemini-3.1-flash-lite
expect "MiniMax-M3 (ci-llm/vendor) passes" 0 minimax/MiniMax-M3

echo "boot env"
if grep -qF 'echo "MOLECULE_LLM_DEFAULT_MODEL=${E2E_PLATFORM_MODEL}"' "$SCRIPT" &&
   ! grep -qF '[ -n "${E2E_PLATFORM_MODEL:-}" ] && echo "MOLECULE_LLM_DEFAULT_MODEL=' "$SCRIPT"; then
  pass "MOLECULE_LLM_DEFAULT_MODEL is always written from E2E_PLATFORM_MODEL"
else
  failc "the boot env must always write MOLECULE_LLM_DEFAULT_MODEL=\${E2E_PLATFORM_MODEL}"
fi
# The guard must run on every path that boots a CP or runs a scenario.
body="$(sed -n '/^require_boot_env() {/,/^}/p' "$SCRIPT")"
case "$body" in *require_platform_model*) pass "require_boot_env calls the guard" ;; *) failc "require_boot_env does not call require_platform_model" ;; esac
if sed -n '/^  scenario)/,/;;/p' "$SCRIPT" | grep -q 'require_platform_model'; then
  pass "the scenario path calls the guard"
else
  failc "the scenario path does not call require_platform_model"
fi

echo
if [ "$fails" -eq 0 ]; then echo "PASS: ephemeral platform-model guard"; else echo "FAILED: $fails"; exit 1; fi
