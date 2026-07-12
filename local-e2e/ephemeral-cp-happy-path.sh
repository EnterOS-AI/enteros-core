#!/usr/bin/env bash
# ephemeral-cp-happy-path.sh — LOCAL wrapper for the RFC "one pre-merge ephemeral
# gate" (§04). Runs the EXACT same happy-path gate the CI workflow runs
# (tests/e2e/ephemeral_cp_happy_path.sh) — on your own machine, against a
# THROWAWAY CP it spins up, with your working-tree tenant image. No shared
# staging, no CI wait: validate the full happy path before you push.
#
# Design tenet: ONE entry point (tests/e2e/ephemeral_cp_happy_path.sh). CI is a
# thin wrapper that supplies images + creds; THIS is the thin local wrapper. Same
# runner → local result == CI result.
#
#   make e2e-ephemeral-happy-path
#     or:  bash local-e2e/ephemeral-cp-happy-path.sh
#
# Overridable env (sensible local defaults below):
#   CP_IMAGE             baseline controlplane image (default: build the sibling repo)
#   CP_EPHEMERAL_SCRIPT  path to pr-ephemeral-cp.sh (default: ../molecule-controlplane/…)
#   MINIMAX_API_KEY      LLM key (default: read via `mol_secret`)
#   E2E_RUNTIME          default hermes

set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
cd "$ROOT"

command -v docker >/dev/null || { echo "FATAL: docker is required (the gate spins up a throwaway CP + tenant)." >&2; exit 1; }
docker info >/dev/null 2>&1 || { echo "FATAL: docker daemon not reachable." >&2; exit 1; }

SHA="$(git rev-parse --short=8 HEAD 2>/dev/null || echo local0000)"

# --- 1. the shared spin-up plumbing (sibling controlplane clone) --------------
CP_EPHEMERAL_SCRIPT="${CP_EPHEMERAL_SCRIPT:-$ROOT/../molecule-controlplane/scripts/deploy/pr-ephemeral-cp.sh}"
[ -x "$CP_EPHEMERAL_SCRIPT" ] || {
  echo "FATAL: pr-ephemeral-cp.sh not found/executable at: $CP_EPHEMERAL_SCRIPT" >&2
  echo "       Point CP_EPHEMERAL_SCRIPT at a controlplane checkout, e.g." >&2
  echo "       CP_EPHEMERAL_SCRIPT=/path/to/molecule-controlplane/scripts/deploy/pr-ephemeral-cp.sh make e2e-ephemeral-happy-path" >&2
  exit 1
}

# --- 2. baseline CP image (the OTHER side of the substitution) ----------------
# Default: build the sibling controlplane so the dev needs no registry pull.
if [ -z "${CP_IMAGE:-}" ]; then
  CP_REPO="$(cd "$(dirname "$CP_EPHEMERAL_SCRIPT")/../.." && pwd)"
  CP_IMAGE="controlplane:local-baseline"
  echo "[local] CP_IMAGE unset — building baseline ${CP_IMAGE} from ${CP_REPO} ..."
  docker build -t "$CP_IMAGE" "$CP_REPO"
fi

# --- 3. tenant image UNDER TEST — your working tree ---------------------------
TENANT_IMAGE="molecule-tenant:local-${SHA}"
echo "[local] building working-tree tenant image ${TENANT_IMAGE} ..."
docker build -f workspace-server/Dockerfile.tenant --build-arg GIT_SHA="$SHA" -t "$TENANT_IMAGE" .

# --- 4. LLM key --------------------------------------------------------------
if [ -z "${MINIMAX_API_KEY:-}" ]; then
  # shellcheck disable=SC1090
  [ -f "$HOME/.molecule-ai/ops.sh" ] && source "$HOME/.molecule-ai/ops.sh" 2>/dev/null || true
  if command -v mol_secret >/dev/null 2>&1; then
    MINIMAX_API_KEY="$(mol_secret /shared/controlplane MINIMAX_API_KEY 2>/dev/null || true)"
  fi
fi
[ -n "${MINIMAX_API_KEY:-}" ] || echo "[local] WARN: no MINIMAX_API_KEY (set it, or the LLM legs will fail) — continuing so the provision/online path still runs."

# --- 5. run the SAME gate CI runs --------------------------------------------
echo "[local] running the happy-path gate against a throwaway CP (same runner as CI) ..."
CP_IMAGE="$CP_IMAGE" \
TENANT_IMAGE="$TENANT_IMAGE" \
CP_EPHEMERAL_SCRIPT="$CP_EPHEMERAL_SCRIPT" \
MINIMAX_API_KEY="${MINIMAX_API_KEY:-}" \
PR_NUMBER="${PR_NUMBER:-0}" \
HEAD_SHA="$SHA" \
E2E_RUNTIME="${E2E_RUNTIME:-hermes}" \
  bash "$ROOT/tests/e2e/ephemeral_cp_happy_path.sh"
