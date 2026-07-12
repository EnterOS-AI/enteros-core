#!/usr/bin/env bash
# ephemeral_cp_happy_path.sh — RFC "one pre-merge ephemeral gate" §04 PROOF.
#
# Runs the CORE happy-path scenario (test_staging_full_saas.sh) against a
# THROWAWAY control-plane it spins up itself — NOT shared staging. This is the
# one-scenario proof of the SDK-owned happy-path model: prove full-saas runs
# against an ephemeral CP with ZERO staging creds, image-substituted so the
# TENANT is THIS PR's build and the CP is the released baseline.
#
# IMAGE SUBSTITUTION (core PR): CP = released baseline (CP_IMAGE); tenant = THIS
# PR's workspace-server build (TENANT_IMAGE), fed to the CP's local-docker
# provisioner via LOCAL_TENANT_IMAGE. (A controlplane PR does the inverse.)
#
# Reuses the CP's ephemeral spin-up plumbing (pr-ephemeral-cp.sh) — the shared
# harness the RFC generalizes. No shared staging, no post-merge dependency.
#
# RUNS LOCALLY == RUNS IN CI (RFC design tenet): this is the SINGLE entry point.
# CI (.gitea/workflows/e2e-ephemeral-happy-path.yml) is a thin wrapper that
# supplies images + creds; devs run the SAME gate on their machine with
# `make e2e-ephemeral-happy-path` — validate the full happy path before pushing,
# no CI wait.
#
# Required env:
#   CP_IMAGE             released controlplane image (baseline), present locally
#   TENANT_IMAGE         THIS PR's workspace-server/tenant image, present locally
#   CP_EPHEMERAL_SCRIPT  path to pr-ephemeral-cp.sh (cloned from the CP repo)
#   MINIMAX_API_KEY      e2e LLM key (the SAME real key the post-merge gate uses)
# Optional:
#   E2E_RUNTIME          default hermes
#   PR_NUMBER, HEAD_SHA  name the throwaway namespace (default 0 / local)

set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

: "${CP_IMAGE:?required — the baseline controlplane image}"
: "${TENANT_IMAGE:?required — this PRs workspace-server/tenant image}"
: "${CP_EPHEMERAL_SCRIPT:?required — path to pr-ephemeral-cp.sh}"
[ -x "$CP_EPHEMERAL_SCRIPT" ] || { echo "FATAL: CP_EPHEMERAL_SCRIPT not executable: $CP_EPHEMERAL_SCRIPT" >&2; exit 2; }
RUNTIME="${E2E_RUNTIME:-hermes}"

# Throwaway per-run namespace (must start with pr- — pr-ephemeral-cp.sh refuses
# to touch a non-ephemeral namespace).
NS="pr-${PR_NUMBER:-0}-$(printf '%s' "${HEAD_SHA:-local0000}" | cut -c1-8)-hp"
case "$NS" in pr-*) : ;; *) echo "FATAL: namespace must start with pr- (got '$NS')" >&2; exit 2 ;; esac

rand_hex() { python3 -c 'import secrets;print(secrets.token_hex(32))'; }
CP_ADMIN_API_TOKEN="$(rand_hex)"   # reused for the CP boot AND full-saas's admin calls

# --- assemble the CP boot-env (mirror of the CP gate orchestrator) ------------
BOOT_ENV_FILE="$(mktemp)"; chmod 600 "$BOOT_ENV_FILE"
{
  echo "MOLECULE_ENV=e2e"
  # EXPLICIT molecules-server provider (local-docker backend); AWS off.
  echo "PROVISIONER_BACKEND=docker"
  echo "MOLECULE_DEFAULT_PROVIDER=molecules-server"
  echo "MOLECULE_AWS_ENABLED=false"
  echo "MOLECULE_DEFAULT_RUNTIME=${RUNTIME}"
  # `up` creates the network as mol-net-${NS}; the CP provisions tenants onto it.
  echo "LOCAL_TENANT_SHARED_NETWORK=mol-net-${NS}"
  echo "LOCAL_TENANT_CP_URL=http://controlplane:8080"
  # THROWAWAY crown jewels (RFC finding #1-A): the CP + DB are disposable, so
  # these only need to be self-consistent for the life of the run.
  echo "CP_ADMIN_API_TOKEN=${CP_ADMIN_API_TOKEN}"
  echo "SECRETS_ENCRYPTION_KEY=$(rand_hex)"
  echo "PROVISION_SHARED_SECRET=$(rand_hex)"
  # IMAGE SUBSTITUTION: the CP provisions tenants with THIS PR's tenant image.
  echo "LOCAL_TENANT_IMAGE=${TENANT_IMAGE}"
  # e2e LLM key — the SAME real key the post-merge gate uses (RFC finding D
  # moves it to a dedicated low-value e2e Infisical path later).
  [ -n "${MINIMAX_API_KEY:-}" ] && echo "MINIMAX_API_KEY=${MINIMAX_API_KEY}"
} >> "$BOOT_ENV_FILE"

cleanup() {
  local rc=$?
  echo "[proof] tearing down ephemeral CP namespace ${NS}..." >&2
  "$CP_EPHEMERAL_SCRIPT" down --ns "$NS" >/dev/null 2>&1 || echo "[proof] (down non-zero — the standing reaper will collect it)" >&2
  rm -f "$BOOT_ENV_FILE" 2>/dev/null || true
  exit "$rc"
}
trap cleanup EXIT INT TERM

echo "[proof] spinning up throwaway CP (baseline ${CP_IMAGE}) provisioning tenant ${TENANT_IMAGE} in ${NS}..." >&2
# `up` prints CP_BASE_URL= / CP_BASE_URL_HOST= / NS= on stdout (log() → stderr),
# so eval'ing its stdout sets those vars in this shell.
CP_BASE_URL=""
# Capture first, then eval — avoids nested double-quotes inside "$(...)" which
# misparse on older bash (bash 3.2). up() prints CP_BASE_URL=/NS= on stdout.
up_output=$("$CP_EPHEMERAL_SCRIPT" up --ns "$NS" --image "$CP_IMAGE" --boot-env-file "$BOOT_ENV_FILE")
eval "$up_output"
[ -n "${CP_BASE_URL:-}" ] || { echo "FATAL: ephemeral CP up did not emit CP_BASE_URL (see its FATAL above)" >&2; exit 1; }
echo "[proof] ephemeral CP serving at ${CP_BASE_URL}" >&2

# Run the core happy-path against the ephemeral CP. tenant_call already sends
# X-Molecule-Org-Id, so MOLECULE_TENANT_URL=CP_BASE_URL routes to the tenant via
# the CP (same as the CP-side gate). Zero staging creds: the admin token is the
# throwaway one baked into the ephemeral CP.
echo "[proof] running core happy-path (full-saas, runtime=${RUNTIME}) against the ephemeral CP — zero staging creds..." >&2
MOLECULE_CP_URL="${CP_BASE_URL}" \
MOLECULE_TENANT_URL="${CP_BASE_URL}" \
MOLECULE_ADMIN_TOKEN="${CP_ADMIN_API_TOKEN}" \
E2E_REQUIRE_LIVE=1 \
E2E_RUNTIME="${RUNTIME}" \
E2E_MINIMAX_API_KEY="${MINIMAX_API_KEY:-}" \
  bash "$HERE/test_staging_full_saas.sh"
rc=$?

if [ "$rc" -eq 0 ]; then
  echo "[proof] ✅ core happy-path PASSED against an ephemeral CP — the SDK-owned-gate model holds with zero shared staging." >&2
else
  echo "[proof] ❌ core happy-path FAILED (rc=$rc) against the ephemeral CP — read the full-saas output above for the failing step." >&2
fi
exit "$rc"
