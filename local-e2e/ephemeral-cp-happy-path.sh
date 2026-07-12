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
# ── MODULAR PHASES (pinpoint a failing step without the full rebuild+boot) ────
#   make e2e-ephemeral-happy-path            # all: build → boot → scenario → down
#   bash local-e2e/ephemeral-cp-happy-path.sh boot       # build + boot, leave UP
#   bash local-e2e/ephemeral-cp-happy-path.sh scenario   # re-run full-saas (~2m), NO rebuild
#   bash local-e2e/ephemeral-cp-happy-path.sh down       # tear the standing CP down
# boot/all build images (skip a build if the image already exists unless
# FORCE_BUILD=1); scenario/down NEVER build — they attach to the standing CP.
#
# Overridable env:
#   CP_IMAGE / TENANT_IMAGE   pre-built images (skip the corresponding build)
#   CP_EPHEMERAL_SCRIPT       path to pr-ephemeral-cp.sh (default: sibling checkout)
#   MINIMAX_API_KEY           LLM key (default: read via `mol_secret`)
#   E2E_RUNTIME               default hermes
#   FORCE_BUILD=1             rebuild images even if they already exist

set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
cd "$ROOT"
CMD="${1:-all}"

command -v docker >/dev/null || { echo "FATAL: docker is required (the gate spins up a throwaway CP + tenant)." >&2; exit 1; }
docker info >/dev/null 2>&1 || { echo "FATAL: docker daemon not reachable." >&2; exit 1; }

SHA="$(git rev-parse --short=8 HEAD 2>/dev/null || echo local0000)"

# --- the shared spin-up plumbing (sibling controlplane clone) -----------------
# Needed by boot/all (spin-up) and down (teardown); scenario reads the runner's
# state file and needs nothing here, but resolving it is cheap + harmless.
CP_EPHEMERAL_SCRIPT="${CP_EPHEMERAL_SCRIPT:-$ROOT/../molecule-controlplane/scripts/deploy/pr-ephemeral-cp.sh}"
if [ "$CMD" != "scenario" ]; then
  [ -x "$CP_EPHEMERAL_SCRIPT" ] || {
    echo "FATAL: pr-ephemeral-cp.sh not found/executable at: $CP_EPHEMERAL_SCRIPT" >&2
    echo "       Point CP_EPHEMERAL_SCRIPT at a controlplane checkout, e.g." >&2
    echo "       CP_EPHEMERAL_SCRIPT=/path/to/molecule-controlplane/scripts/deploy/pr-ephemeral-cp.sh $0 $CMD" >&2
    exit 1
  }
fi

# image_exists <tag> — true if a local docker image with this tag is present.
image_exists() { docker image inspect "$1" >/dev/null 2>&1; }

build_images() {
  # baseline CP (the OTHER side of the substitution). MUST use Dockerfile.dockerprov
  # (multi-stage, ships the `docker` CLI): the CP's local-docker provisioner shells
  # out to `docker` to launch tenants, which the distroless/static ROOT Dockerfile
  # lacks — a CP built from it cannot provision.
  if [ -z "${CP_IMAGE:-}" ]; then
    CP_REPO="$(cd "$(dirname "$CP_EPHEMERAL_SCRIPT")/../.." && pwd)"
    CP_IMAGE="controlplane:local-baseline"
    if [ -n "${FORCE_BUILD:-}" ] || ! image_exists "$CP_IMAGE"; then
      echo "[local] building baseline ${CP_IMAGE} from ${CP_REPO} (Dockerfile.dockerprov) ..."
      docker build -f "$CP_REPO/Dockerfile.dockerprov" -t "$CP_IMAGE" "$CP_REPO"
    else
      echo "[local] reusing existing ${CP_IMAGE} (set FORCE_BUILD=1 to rebuild)"
    fi
  fi

  # tenant image UNDER TEST — your working tree. Rebuild by default (it is the code
  # under test); skip only if it already exists AND you didn't ask to force.
  if [ -z "${TENANT_IMAGE:-}" ]; then
    TENANT_IMAGE="molecule-tenant:local-${SHA}"
    if [ -n "${FORCE_BUILD:-}" ] || ! image_exists "$TENANT_IMAGE"; then
      echo "[local] building working-tree tenant image ${TENANT_IMAGE} ..."
      docker build -f workspace-server/Dockerfile.tenant --build-arg GIT_SHA="$SHA" -t "$TENANT_IMAGE" .
    else
      echo "[local] reusing existing ${TENANT_IMAGE} (set FORCE_BUILD=1 to rebuild)"
    fi
  fi
}

resolve_minimax_key() {
  if [ -z "${MINIMAX_API_KEY:-}" ]; then
    # shellcheck disable=SC1090
    [ -f "$HOME/.molecule-ai/ops.sh" ] && source "$HOME/.molecule-ai/ops.sh" 2>/dev/null || true
    if command -v mol_secret >/dev/null 2>&1; then
      MINIMAX_API_KEY="$(mol_secret /shared/controlplane MINIMAX_API_KEY 2>/dev/null || true)"
    fi
  fi
  [ -n "${MINIMAX_API_KEY:-}" ] || echo "[local] WARN: no MINIMAX_API_KEY (LLM legs will fail) — continuing so provision/online still runs."
}

# scenario/down attach to the standing CP — no build, no key needed (scenario
# reads MINIMAX from the runner's state file; down just tears down).
case "$CMD" in
  scenario|down)
    echo "[local] ${CMD}: attaching to the standing CP (no rebuild) ..."
    CP_EPHEMERAL_SCRIPT="$CP_EPHEMERAL_SCRIPT" \
    PR_NUMBER="${PR_NUMBER:-0}" \
    HEAD_SHA="$SHA" \
    E2E_RUNTIME="${E2E_RUNTIME:-hermes}" \
      bash "$ROOT/tests/e2e/ephemeral_cp_happy_path.sh" "$CMD"
    ;;
  boot|all)
    build_images
    resolve_minimax_key
    echo "[local] ${CMD}: running the gate against a throwaway CP (same runner as CI) ..."
    CP_IMAGE="$CP_IMAGE" \
    TENANT_IMAGE="$TENANT_IMAGE" \
    CP_EPHEMERAL_SCRIPT="$CP_EPHEMERAL_SCRIPT" \
    MINIMAX_API_KEY="${MINIMAX_API_KEY:-}" \
    PR_NUMBER="${PR_NUMBER:-0}" \
    HEAD_SHA="$SHA" \
    E2E_RUNTIME="${E2E_RUNTIME:-hermes}" \
      bash "$ROOT/tests/e2e/ephemeral_cp_happy_path.sh" "$CMD"
    ;;
  *)
    echo "usage: $0 [all|boot|scenario|down]" >&2
    exit 2
    ;;
esac
