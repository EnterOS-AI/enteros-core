#!/usr/bin/env bash
# Unit test for ephemeral_cp_happy_path.sh's real-runtime-matrix opt-in.
#
# Pins the DEFAULT-OFF invariance + arming semantics of the multi-runtime matrix
# (E2E_EPHEMERAL_REAL_RUNTIME_MATRIX):
#   * OFF (unset/""/0/false) — matrix_runtimes() == exactly E2E_RUNTIME (hermes),
#     pv_provision_mode() == external. Byte-identical to the pre-matrix gate: the
#     seed loop seeds one image, peer_visibility runs the external-row slice.
#   * ON (1/true/yes/on) — matrix_runtimes() == the full maintained matrix
#     (overridable via E2E_EPHEMERAL_MATRIX_RUNTIMES), pv_provision_mode() ==
#     managed (real boot to online).
#   * resolve_template_ref() DIGEST-PINS hermes/openclaw/claude-code (the hermes
#     pin is the EXACT happy-path input and must not drift on this task), honours
#     the per-runtime WORKSPACE_TEMPLATE_<RT>_REF override, and falls back to the
#     moving :latest tag for an unpinned runtime.
#
# Sources the script (its dispatch is guarded so sourcing does NOT boot a CP) and
# touches no docker — every function under test is pure.
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# E2E_RUNTIME must be its default (hermes) at source time — matrix_runtimes()
# reads the RUNTIME global the script sets from it (line "RUNTIME=${E2E_RUNTIME:-hermes}").
unset E2E_RUNTIME E2E_EPHEMERAL_REAL_RUNTIME_MATRIX E2E_EPHEMERAL_MATRIX_RUNTIMES 2>/dev/null || true

# shellcheck source=ephemeral_cp_happy_path.sh disable=SC1091
source "$HERE/ephemeral_cp_happy_path.sh"

fails=0
pass() { echo "  PASS: $1"; }
failc() { echo "  FAIL: $1"; fails=$((fails + 1)); }

# eq <desc> <expected> <actual>
eq() {
  if [ "$2" = "$3" ]; then pass "$1"; else failc "$1 — expected [$2] got [$3]"; fi
}

# The exact hermes digest the happy path provisions with. If this test ever needs
# editing to match, that is the signal a matrix change touched the happy-path input.
HERMES_PIN="registry.moleculesai.app/molecule-ai/workspace-template-hermes@sha256:8aab6b48f2af15c3dd1056a5d1f5e7a61fb9a8a66aa7aca5b51cf2d2702215f9"
OPENCLAW_PIN="registry.moleculesai.app/molecule-ai/workspace-template-openclaw@sha256:24085f9895af728b7166f9f7908cf4a6b825a4c98abdeb0c1462daa8a5838715"
CLAUDE_PIN="registry.moleculesai.app/molecule-ai/workspace-template-claude-code@sha256:50b8068739f35fd3c7404afe7b8336f5057285b12e73b69e62ccd5c6dcc6ddab"

echo "Test: real-runtime-matrix opt-in — DEFAULT-OFF invariance"

# ── DEFAULT OFF (unset) ──────────────────────────────────────────────────────
unset E2E_EPHEMERAL_REAL_RUNTIME_MATRIX E2E_EPHEMERAL_MATRIX_RUNTIMES
if real_runtime_matrix_enabled; then failc "OFF: enabled must be false when unset"; else pass "OFF: matrix disabled when unset"; fi
eq "OFF: matrix_runtimes == just hermes (today's single seed)" "hermes" "$(matrix_runtimes)"
eq "OFF: pv_provision_mode == external (today's slice)"        "external" "$(pv_provision_mode)"

# Negative control: a wrong wiring that leaked the matrix into the OFF path would
# make this the 3-runtime list — assert it is NOT.
if [ "$(matrix_runtimes)" = "hermes openclaw claude-code" ]; then
  failc "OFF neg-control: matrix_runtimes MUST NOT be the full matrix when flag off"
else
  pass "OFF neg-control: matrix_runtimes is not the full matrix when flag off"
fi

# Falsey spellings are all OFF.
for v in "" "0" "false" "no" "off" "garbage"; do
  E2E_EPHEMERAL_REAL_RUNTIME_MATRIX="$v"
  if real_runtime_matrix_enabled; then failc "OFF: '$v' must be disabled"; else pass "OFF: '$v' disabled"; fi
  eq "OFF('$v'): matrix_runtimes == hermes" "hermes" "$(matrix_runtimes)"
  eq "OFF('$v'): pv mode == external"       "external" "$(pv_provision_mode)"
done
unset E2E_EPHEMERAL_REAL_RUNTIME_MATRIX

echo "Test: real-runtime-matrix opt-in — ARMED semantics"

# ── ARMED ────────────────────────────────────────────────────────────────────
for v in "1" "true" "yes" "on"; do
  E2E_EPHEMERAL_REAL_RUNTIME_MATRIX="$v"
  if real_runtime_matrix_enabled; then pass "ON: '$v' enabled"; else failc "ON: '$v' must be enabled"; fi
  eq "ON('$v'): matrix_runtimes == full matrix" "hermes openclaw claude-code" "$(matrix_runtimes)"
  eq "ON('$v'): pv mode == managed"             "managed" "$(pv_provision_mode)"
done

# Subset override — a maintainer soaks hermes+openclaw before adding claude-code.
# (Consumed by matrix_runtimes()/real_runtime_matrix_enabled() in the sourced
# script; shellcheck cannot trace the read across `source`.)
# shellcheck disable=SC2034
E2E_EPHEMERAL_REAL_RUNTIME_MATRIX=1
# shellcheck disable=SC2034
E2E_EPHEMERAL_MATRIX_RUNTIMES="hermes openclaw"
eq "ON+override: matrix_runtimes honours E2E_EPHEMERAL_MATRIX_RUNTIMES" "hermes openclaw" "$(matrix_runtimes)"
unset E2E_EPHEMERAL_MATRIX_RUNTIMES E2E_EPHEMERAL_REAL_RUNTIME_MATRIX

echo "Test: resolve_template_ref — digest pins + overrides + fallback"

eq "hermes pin (== happy-path input, must not drift)" "$HERMES_PIN"   "$(resolve_template_ref hermes)"
eq "openclaw pin"                                      "$OPENCLAW_PIN" "$(resolve_template_ref openclaw)"
eq "claude-code pin"                                   "$CLAUDE_PIN"   "$(resolve_template_ref claude-code)"

# Per-runtime override env.
eq "openclaw override honoured" "myreg/openclaw@sha256:dead" \
   "$(WORKSPACE_TEMPLATE_OPENCLAW_REF='myreg/openclaw@sha256:dead' resolve_template_ref openclaw)"
eq "claude-code override honoured" "myreg/cc@sha256:beef" \
   "$(WORKSPACE_TEMPLATE_CLAUDE_CODE_REF='myreg/cc@sha256:beef' resolve_template_ref claude-code)"

# Unpinned runtime → moving-tag fallback on the default registry.
eq "codex (unpinned) → :latest fallback" \
   "registry.moleculesai.app/molecule-ai/workspace-template-codex:latest" \
   "$(resolve_template_ref codex)"

# MOLECULE_IMAGE_REGISTRY override flows into the fallback.
eq "registry override flows into fallback" \
   "myreg.example/workspace-template-codex:latest" \
   "$(MOLECULE_IMAGE_REGISTRY='myreg.example' resolve_template_ref codex)"

echo
if [ "$fails" -eq 0 ]; then
  echo "ALL PASS (real-runtime-matrix unit)"
  exit 0
else
  echo "$fails FAILED (real-runtime-matrix unit)"
  exit 1
fi
