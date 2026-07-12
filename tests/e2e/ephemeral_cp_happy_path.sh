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
# `make e2e-ephemeral-happy-path` — validate the full happy path before pushing.
#
# ── MODULAR PHASES — iterate a failing step WITHOUT the full rebuild+boot ─────
# The boot (~minutes: build CP+tenant, boot CP, create DB, migrate) and the
# scenario (~2 min) are independently runnable so you can pinpoint a failing
# step fast instead of paying the whole cycle each time:
#
#   all        (default) boot → scenario → teardown. What CI runs. Unchanged.
#   boot       start PG + boot the CP, LEAVE IT UP, write a reattach state file,
#              and print the exact command to run the scenario against it.
#   scenario   run full-saas against the standing CP from the reattach file.
#              Repeatable in ~2 min — the fast pinpoint loop.
#   down       tear down the standing CP + PG + reattach file.
#
# Fast local loop (iterate on a failing scenario step, no rebuild/reboot):
#   ./ephemeral_cp_happy_path.sh boot         # once  (~minutes)
#   ./ephemeral_cp_happy_path.sh scenario     # many  (~2 min) while you fix
#   ./ephemeral_cp_happy_path.sh down         # when done
# (KEEP_UP=1 ./ephemeral_cp_happy_path.sh all  runs once but leaves the CP up so
#  you can attach a scenario / poke the CP after a failure.)
#
# Required env (boot / all): CP_IMAGE, TENANT_IMAGE, CP_EPHEMERAL_SCRIPT, MINIMAX_API_KEY
# Optional: E2E_RUNTIME (default hermes); PR_NUMBER, HEAD_SHA (name the namespace)

set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CMD="${1:-all}"

RUNTIME="${E2E_RUNTIME:-hermes}"
ROUTE_DOMAIN="staging.moleculesai.app"   # MUST match MOLECULE_TOPO_CP_APP_DOMAIN below

# Throwaway per-run namespace (must start with pr- — pr-ephemeral-cp.sh refuses
# to touch a non-ephemeral namespace). Deterministic in PR_NUMBER/HEAD_SHA so the
# boot / scenario / down phases all address the SAME namespace + state file.
NS="pr-${PR_NUMBER:-0}-$(printf '%s' "${HEAD_SHA:-local0000}" | cut -c1-8)-hp"
case "$NS" in pr-*) : ;; *) echo "FATAL: namespace must start with pr- (got '$NS')" >&2; exit 2 ;; esac
STATE_FILE="${EPHEMERAL_STATE_FILE:-${TMPDIR:-/tmp}/ephemeral-cp-${NS}.env}"

rand_hex() { python3 -c 'import secrets;print(secrets.token_hex(32))'; }

require_boot_env() {
  : "${CP_IMAGE:?required — the baseline controlplane image}"
  : "${TENANT_IMAGE:?required — this PRs workspace-server/tenant image}"
  : "${CP_EPHEMERAL_SCRIPT:?required — path to pr-ephemeral-cp.sh}"
  [ -x "$CP_EPHEMERAL_SCRIPT" ] || { echo "FATAL: CP_EPHEMERAL_SCRIPT not executable: $CP_EPHEMERAL_SCRIPT" >&2; exit 2; }
}

# ── start_pg: throwaway postgres:16 on an ephemeral host port ────────────────
# The CP's `up` requires an EXTERNAL PG — it creates a fresh per-run database on
# it and does NOT stand up its own. `up` reaches it via --pg-container (docker
# exec psql — the runner image has no host psql client) and via
# host.docker.internal:<port> from the CP container. Sets PG_CTR / PG_PORT.
PG_CTR=""; PG_PORT=""; PG_SUPERPASS="ephemeral-pr-pg"
start_pg() {
  PG_CTR="pg-${NS}"
  docker rm -f "$PG_CTR" >/dev/null 2>&1 || true
  docker run -d --name "$PG_CTR" \
    -e POSTGRES_USER=postgres -e POSTGRES_PASSWORD="$PG_SUPERPASS" \
    -e POSTGRES_DB=postgres \
    -p 127.0.0.1:0:5432 postgres:16 >/dev/null \
    || { echo "FATAL: could not start ephemeral Postgres container ${PG_CTR}" >&2; exit 1; }
  PG_PORT="$(docker port "$PG_CTR" 5432/tcp | awk -F: '/127\.0\.0\.1:/ {print $2; exit}')"
  [ -n "$PG_PORT" ] || PG_PORT="$(docker port "$PG_CTR" 5432/tcp | head -1 | awk -F: '{print $NF}')"
  [ -n "$PG_PORT" ] || { echo "FATAL: no host port for ${PG_CTR}" >&2; docker logs "$PG_CTR" 2>&1 | tail -20 >&2 || true; exit 1; }
  local ready=""
  for _ in $(seq 1 30); do
    if docker exec "$PG_CTR" pg_isready -U postgres >/dev/null 2>&1; then ready=1; break; fi
    sleep 1
  done
  [ -n "$ready" ] || { echo "FATAL: ephemeral Postgres ${PG_CTR} never became ready" >&2; docker logs "$PG_CTR" 2>&1 | tail -20 >&2 || true; exit 1; }
  echo "[proof] ephemeral PG ${PG_CTR} ready on 127.0.0.1:${PG_PORT}" >&2
}

# ── boot_cp: assemble the boot-env, boot the CP via `up` (sets CP_BASE_URL) ──
CP_BASE_URL=""; CP_ADMIN_API_TOKEN=""
boot_cp() {
  CP_ADMIN_API_TOKEN="$(rand_hex)"   # reused for the CP boot AND full-saas's admin calls
  local boot_env; boot_env="$(mktemp)"; chmod 600 "$boot_env"
  {
    echo "MOLECULE_ENV=e2e"
    # EXPLICIT molecules-server provider (local-docker backend); AWS off.
    echo "PROVISIONER_BACKEND=docker"
    echo "MOLECULE_DEFAULT_PROVIDER=molecules-server"
    echo "MOLECULE_AWS_ENABLED=false"
    echo "MOLECULE_DEFAULT_RUNTIME=${RUNTIME}"
    # MOLECULE_ENV=e2e is a REAL provisioning env: the CP does NOT boot-fetch from
    # Infisical and FAIL-CLOSES at boot unless the full MOLECULE_TOPO_* staging-mirror
    # set is injected (controlplane cmd/server/bootsecrets.go requireE2ETopologyInjected
    # over internal/envs RequiredTopologyKeys). These are the NON-SECRET staging-mirror
    # topology labels the CP ships as envs.E2EStagingMirrorTopology(); MOLECULE_AWS_ENABLED
    # =false above means none are ever dialed — they only satisfy the boot assertion and
    # set CP appDomain=${ROUTE_DOMAIN} (used for slug routing in the scenario).
    echo "MOLECULE_TOPO_AWS_ACCOUNT_ID=004947743811"
    echo "MOLECULE_TOPO_AWS_REGION=us-east-2"
    echo "MOLECULE_TOPO_AWS_VPC_ID=vpc-0f35ce782265b34dd"
    echo "MOLECULE_TOPO_AWS_SUBNET_ID=subnet-0bf1813c16efe69c6"
    echo "MOLECULE_TOPO_AWS_SECURITY_GROUP_ID=sg-0996f755348630e6d"
    echo "MOLECULE_TOPO_AWS_TENANT_INSTANCE_PROFILE=MoleculeTenantEICRole-staging"
    echo "MOLECULE_TOPO_AWS_WORKSPACE_INSTANCE_PROFILE=MoleculeTenantEICRole-staging"
    echo "MOLECULE_TOPO_AWS_TENANT_AMI=ami-09cdbb1de48dd8f3c"
    echo "MOLECULE_TOPO_AWS_TENANT_IMAGE=004947743811.dkr.ecr.us-east-2.amazonaws.com/molecule-ai/platform-tenant:latest"
    echo "MOLECULE_TOPO_AWS_ECR_REGISTRY=004947743811.dkr.ecr.us-east-2.amazonaws.com"
    echo "MOLECULE_TOPO_CF_ZONE=moleculesai.app"
    echo "MOLECULE_TOPO_CF_ZONE_ID=a034108eda16d131ef7f766b923ef464"
    echo "MOLECULE_TOPO_CF_TENANT_SUBDOMAIN_SUFFIX=staging.moleculesai.app"
    echo "MOLECULE_TOPO_CP_APP_DOMAIN=${ROUTE_DOMAIN}"
    echo "MOLECULE_TOPO_CP_BASE_URL=https://staging-api.moleculesai.app"
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
  } >> "$boot_env"

  echo "[proof] spinning up throwaway CP (baseline ${CP_IMAGE}) provisioning tenant ${TENANT_IMAGE} in ${NS}..." >&2
  # `up` prints CP_BASE_URL= / CP_BASE_URL_HOST= / NS= on stdout (log() → stderr).
  # Capture first, then eval — avoids nested double-quotes inside "$(...)".
  local up_output
  up_output=$("$CP_EPHEMERAL_SCRIPT" up --ns "$NS" --image "$CP_IMAGE" \
    --pg-host 127.0.0.1 --pg-port "$PG_PORT" --pg-container "$PG_CTR" \
    --pg-superuser postgres --pg-superpass "$PG_SUPERPASS" \
    --boot-env-file "$boot_env")
  local up_rc=$?
  rm -f "$boot_env" 2>/dev/null || true
  [ "$up_rc" -eq 0 ] || { echo "FATAL: ephemeral CP up exited $up_rc" >&2; exit 1; }
  eval "$up_output"
  [ -n "${CP_BASE_URL:-}" ] || { echo "FATAL: ephemeral CP up did not emit CP_BASE_URL (see its FATAL above)" >&2; exit 1; }
  echo "[proof] ephemeral CP serving at ${CP_BASE_URL}" >&2
}

write_state() {
  umask 077
  {
    echo "NS=${NS}"
    echo "PG_CTR=${PG_CTR}"
    echo "PG_PORT=${PG_PORT}"
    echo "CP_BASE_URL=${CP_BASE_URL}"
    echo "CP_ADMIN_API_TOKEN=${CP_ADMIN_API_TOKEN}"
    echo "RUNTIME=${RUNTIME}"
    echo "ROUTE_DOMAIN=${ROUTE_DOMAIN}"
    echo "MINIMAX_API_KEY=${MINIMAX_API_KEY:-}"
  } > "$STATE_FILE"
  chmod 600 "$STATE_FILE"
}

load_state() {
  [ -f "$STATE_FILE" ] || { echo "FATAL: no reattach state at ${STATE_FILE} — run '$0 boot' first (same PR_NUMBER/HEAD_SHA)." >&2; exit 2; }
  # shellcheck disable=SC1090
  . "$STATE_FILE"
}

# ── run_scenario: full-saas against the standing CP (uses globals set above or
# loaded from state). The CP wildcard proxy routes tenants by SLUG (Host /
# X-Molecule-Org-Slug), NOT by X-Molecule-Org-Id (the CP injects that toward the
# tenant). MOLECULE_TENANT_URL=CP_BASE_URL sends tenant traffic at the CP;
# MOLECULE_TENANT_ROUTE_DOMAIN makes full-saas attach Host=<slug>.<domain> +
# X-Molecule-Org-Slug so the CP routes it to the provisioned tenant. Zero staging
# creds: the admin token is the throwaway one baked into the ephemeral CP.
run_scenario() {
  echo "[proof] running core happy-path (full-saas, runtime=${RUNTIME}) against the ephemeral CP — zero staging creds..." >&2
  MOLECULE_CP_URL="${CP_BASE_URL}" \
  MOLECULE_TENANT_URL="${CP_BASE_URL}" \
  MOLECULE_TENANT_ROUTE_DOMAIN="${ROUTE_DOMAIN}" \
  MOLECULE_ADMIN_TOKEN="${CP_ADMIN_API_TOKEN}" \
  E2E_REQUIRE_LIVE=1 \
  E2E_RUNTIME="${RUNTIME}" \
  E2E_MINIMAX_API_KEY="${MINIMAX_API_KEY:-}" \
  E2E_PROVISION_TIMEOUT_SECS="${E2E_PROVISION_TIMEOUT_SECS:-300}" \
    bash "$HERE/test_staging_full_saas.sh"
}

# ── dump_diagnostics: on a scenario failure, surface the CP + tenant container
# logs so the failing step is diagnosable WITHOUT a local repro. The runner
# otherwise tears the CP down (all mode) and leaves nothing to inspect. The CP
# is molecule-cp-${NS}; the local-docker provisioner launches tenants onto
# mol-net-${NS}, so a network filter catches them (or reveals none were launched).
dump_diagnostics() {
  echo "── DIAGNOSTIC BURST (ephemeral CP ${NS}) ─────────────────────────────" >&2
  echo "[diag] containers on mol-net-${NS}:" >&2
  docker ps -a --filter "network=mol-net-${NS}" \
    --format '  {{.Names}}  {{.Image}}  {{.Status}}' >&2 2>/dev/null || true
  echo "[diag] CP logs (molecule-cp-${NS}, tail 200):" >&2
  docker logs --tail 200 "molecule-cp-${NS}" 2>&1 | sed 's/^/  cp| /' >&2 \
    || echo "  (no CP container molecule-cp-${NS})" >&2
  local c
  for c in $(docker ps -a --filter "network=mol-net-${NS}" --format '{{.Names}}' 2>/dev/null); do
    case "$c" in "molecule-cp-${NS}"|"pg-${NS}") continue ;; esac
    echo "[diag] tenant container ${c} logs (tail 200):" >&2
    docker logs --tail 200 "$c" 2>&1 | sed "s/^/  ${c}| /" >&2 || true
  done
  echo "── END DIAGNOSTIC BURST ──────────────────────────────────────────────" >&2
}

teardown() {
  echo "[proof] tearing down ephemeral CP namespace ${NS}..." >&2
  if [ -n "${CP_EPHEMERAL_SCRIPT:-}" ] && [ -x "${CP_EPHEMERAL_SCRIPT:-}" ]; then
    "$CP_EPHEMERAL_SCRIPT" down --ns "$NS" >/dev/null 2>&1 || echo "[proof] (down non-zero — the standing reaper will collect it)" >&2
  fi
  if [ -n "${PG_CTR:-}" ]; then docker rm -f "$PG_CTR" >/dev/null 2>&1 || true; fi
  rm -f "$STATE_FILE" 2>/dev/null || true
}

print_reattach() {
  cat >&2 <<EOF
[proof] ✅ ephemeral CP is UP and left running (namespace ${NS}).
[proof]    CP_BASE_URL = ${CP_BASE_URL}
[proof]    reattach state: ${STATE_FILE}
[proof] Run the scenario against it (repeatable, ~2 min):
[proof]    PR_NUMBER=${PR_NUMBER:-0} HEAD_SHA=${HEAD_SHA:-local0000} $0 scenario
[proof] Tear it down when done:
[proof]    PR_NUMBER=${PR_NUMBER:-0} HEAD_SHA=${HEAD_SHA:-local0000} $0 down
EOF
}

case "$CMD" in
  all)
    require_boot_env
    trap 'rc=$?; if [ -n "${KEEP_UP:-}" ]; then print_reattach; else teardown; fi; exit "$rc"' EXIT INT TERM
    start_pg
    boot_cp
    write_state    # so `KEEP_UP=1 … all` (or a mid-run peek) can attach a scenario
    run_scenario; rc=$?
    if [ "$rc" -eq 0 ]; then
      echo "[proof] ✅ core happy-path PASSED against an ephemeral CP — the SDK-owned-gate model holds with zero shared staging." >&2
    else
      echo "[proof] ❌ core happy-path FAILED (rc=$rc) against the ephemeral CP — read the full-saas output above for the failing step." >&2
      dump_diagnostics   # CP + tenant logs BEFORE the trap tears the CP down
    fi
    exit "$rc"   # trap tears down (or, with KEEP_UP=1, leaves it up + prints reattach)
    ;;
  boot)
    require_boot_env
    start_pg
    boot_cp
    write_state
    print_reattach
    ;;
  scenario)
    load_state
    run_scenario; rc=$?
    if [ "$rc" -eq 0 ]; then
      echo "[proof] ✅ scenario PASSED against standing CP ${CP_BASE_URL}." >&2
    else
      echo "[proof] ❌ scenario FAILED (rc=$rc) — the CP is still UP (${CP_BASE_URL}); fix and re-run '$0 scenario'." >&2
      dump_diagnostics
    fi
    exit "$rc"
    ;;
  down)
    [ -f "$STATE_FILE" ] && load_state || true
    teardown
    ;;
  *)
    echo "usage: $0 [all|boot|scenario|down]" >&2
    exit 2
    ;;
esac
