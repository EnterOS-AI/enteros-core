#!/usr/bin/env bash
# Live staging E2E — the CP instance-state reconciler heals a killed workspace
# instance. ADAPTED 2026-07-03: the default provider is now MOLECULES-SERVER
# (local-docker) — the kill primitive is `docker rm -f <container>`, not `aws ec2
# terminate-instances`. E2E_PROVIDER=aws restores the legacy EC2 path unchanged.
#
# Real-infra complement to the deterministic unit tests for core#2261
# (workspace-server/internal/registry/cp_instance_reconciler.go). Those unit
# tests pin the reconcile logic against fakes; THIS script proves the loop
# actually runs in a real tenant's workspace-server and drives the EXISTING
# offline + auto-heal machinery against a real killed instance (docker container
# on molecules-server; EC2 on aws).
#
# Root regression (core#2247): a workspace whose box is killed out from under the
# platform (manual action, spot reclaim, CP reap; the local-docker analog: the
# container is `docker rm`'d or OOM-killed) fell through every existing liveness
# pass and kept reading status='online' forever, pointing at a dead instance. The
# reconciler closes that gap with CPProvisioner.IsRunning and feeds a clean "not
# running" into onOffline → RestartByID (existing-volume reprovision).
#
# What this test does:
#   1. Provision a fresh staging org (provider=$E2E_PROVIDER) + ONE workspace
#      (same default runtime/model as the full-saas harness, so it actually boots).
#   2. Poll the tenant API until the workspace is status=online; capture its
#      instance_id (on molecules-server the instance_id IS the container name).
#   3. KILL it — `docker rm -f <container>` (molecules-server) or `aws ec2
#      terminate-instances` (aws) on that exact instance.
#   4. Assert the reconciler heals it:
#        PRIMARY (gate)      — within ~180s the workspace status LEAVES
#                              'online' (the reconciler detected the dead
#                              instance via IsRunning and flipped it). This
#                              is the core regression guard: a dead instance
#                              must NOT keep reading 'online'.
#        SECONDARY (best-effort) — within ~10 min it auto-reprovisions:
#                              status returns to 'online' with a NEW
#                              instance_id (onOffline → RestartByID
#                              existing-volume heal). If reprovision doesn't
#                              finish in the bound we log it clearly but let
#                              the PRIMARY assertion stand as the gate (see
#                              the comment at the secondary block — a future
#                              tightening that promotes this to a hard gate is
#                              deliberately one edit away).
#   5. Teardown ALWAYS (EXIT trap): delete the tenant + leak-sweep so no EC2
#      is orphaned, even on a mid-test failure.
#
# Auth model + provisioning conventions are copied verbatim from
# test_staging_full_saas.sh (single MOLECULE_ADMIN_TOKEN → CP admin; per-
# tenant admin token + X-Molecule-Org-Id header for tenant API). On the aws path
# the kill primitive + leak sweep reuse lib/aws_leak_check.sh; on molecules-server
# they are inline docker (rm -f / ps -a) — see the provider abstraction block.
#
# Required env:
#   MOLECULE_CP_URL        default: https://staging-api.moleculesai.app
#   MOLECULE_ADMIN_TOKEN   staging CP admin bearer from Infisical /shared/controlplane-admin
#   E2E_PROVIDER           molecules-server (DEFAULT; docker rm -f the container)
#                          | aws (legacy EC2 terminate-instances). molecules-server
#                          needs a reachable docker daemon (DOCKER_HOST → the
#                          molecules-server host; a local stack shares it); aws
#                          needs AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY.
#
# Optional env (mirrors the full-saas harness where they overlap):
#   E2E_RUNTIME                        claude-code (default)
#   E2E_PROVISION_TIMEOUT_SECS         default 900 (cold-provision budget)
#   E2E_WORKSPACE_ONLINE_TIMEOUT_SECS  default 900 (15min). A workspace that
#                     cannot reach online in 15min is a staging/boot problem,
#                     not slow cold-boot — fail fast so the trap tears down the
#                     EC2 instead of hanging ~1h and leaking a running instance
#                     (observed: run 216031 hung 32min with a live e2e-rec EC2).
#   E2E_RECONCILE_OFFLINE_TIMEOUT_SECS default 180 (PRIMARY: leave 'online'.
#                                      Reconciler cadence is 60s — 3 cycles +
#                                      AWS terminate-visibility slack.)
#   E2E_REPROVISION_TIMEOUT_SECS       default 600 (SECONDARY: back to online
#                                      with a NEW instance_id)
#   E2E_MINIMAX_API_KEY / E2E_ANTHROPIC_API_KEY / E2E_OPENAI_API_KEY
#                                      LLM key (same priority chain as
#                                      full-saas; needed so the FIRST boot
#                                      reaches online). Empty → '{}' (the
#                                      workspace still boots online; the LLM
#                                      key only matters for a completion,
#                                      which this test never makes).
#   E2E_KEEP_ORG                       1 → skip teardown (debugging only)
#   E2E_RUN_ID                         Slug suffix; CI: ${GITHUB_RUN_ID}
#   E2E_AWS_LEAK_CHECK                 auto (default) | required | off
#   E2E_AWS_TERMINATE_LEAKS            1 → terminate slug-tagged leaked EC2 at
#                                      teardown
#
# Exit codes:
#   0  happy path (PRIMARY assertion held; SECONDARY logged either way)
#   1  generic failure (incl. PRIMARY assertion failed = regression)
#   2  missing required env
#   3  provisioning timed out
#   4  teardown left orphan resources

set -euo pipefail

CP_URL="${MOLECULE_CP_URL:-https://staging-api.moleculesai.app}"
ADMIN_TOKEN="${MOLECULE_ADMIN_TOKEN:?MOLECULE_ADMIN_TOKEN required — load staging CP_ADMIN_API_TOKEN from Infisical /shared/controlplane-admin}"
RUNTIME="${E2E_RUNTIME:-claude-code}"
# Provider knob. molecules-server (local-docker; persisted organizations.provider
# = "local") is the DEFAULT — this test now kills a docker container instead of an
# EC2. E2E_PROVIDER=aws restores the legacy AWS path (terminate-instances +
# aws_leak_check.sh) for when a cloud account returns. The canonical create-org
# request id is "molecules-server" (NOT "platform" — that is an LLM arm — and NOT
# the bare "local" alias); IsValidRequest accepts it and it persists as "local".
PROVIDER="${E2E_PROVIDER:-molecules-server}"
PROVISION_TIMEOUT_SECS="${E2E_PROVISION_TIMEOUT_SECS:-900}"
WORKSPACE_ONLINE_TIMEOUT_SECS="${E2E_WORKSPACE_ONLINE_TIMEOUT_SECS:-900}"
# PRIMARY bound: the reconciler ticks every 60s; it needs one cycle to see
# the dead instance after AWS makes the terminate visible to DescribeInstances
# (typically seconds, but can lag). 180s = ~3 cycles + slack.
RECONCILE_OFFLINE_TIMEOUT_SECS="${E2E_RECONCILE_OFFLINE_TIMEOUT_SECS:-180}"
# SECONDARY bound: full existing-volume reprovision (new EC2 boot + agent
# bootstrap) is a multi-minute cold path.
REPROVISION_TIMEOUT_SECS="${E2E_REPROVISION_TIMEOUT_SECS:-600}"
# RUN_ID_SUFFIX removed (core#2782 follow-up shellcheck): the slug now comes
# from make_collision_proof_slug below; the old suffix var is dead.

# log/fail/ok MUST be defined BEFORE the assert_collision_proof_slug call
# below (which uses `|| fail "..."`). Defining them after the call would
# error on a bad slug with `fail: command not found` instead of the
# intended diagnostic — silent misbehaviour that the lint can't catch.
# Mirrors the order in test_staging_full_saas.sh.
log()  { echo "[$(date +%H:%M:%S)] $*"; }
fail() { echo "[$(date +%H:%M:%S)] ❌ $*" >&2; exit 1; }
ok()   { echo "[$(date +%H:%M:%S)] ✅ $*"; }

# Slug MUST start with e2e- so sweep-stale-e2e-orgs.yml reaps any orphan this
# run leaks (lint_cleanup_traps.sh enforces the e2e-/rt-e2e- prefix for any
# staging tenant E2E; we honour it here too even though our filename isn't
# *staging*).
# Collision-proof slug (core#2782). The prior `head -c 32` truncation
# dropped the run_attempt suffix and let two parallel/retry runs
# collide (POST /cp/admin/orgs 409). The helper appends a random
# 8-char uuid so every run gets a unique slug regardless of how
# the workflow composes E2E_RUN_ID.
# shellcheck source=lib/collision-proof-slug.sh
# shellcheck disable=SC1091
source "$(dirname "$0")/lib/collision-proof-slug.sh"
SLUG="e2e-rec-$(make_collision_proof_slug_suffix "${E2E_RUN_ID:-}" 8)"
assert_collision_proof_slug "$SLUG" || fail "Bug in make_collision_proof_slug: produced non-collision-proof slug '$SLUG'"

# Per-runtime model slug dispatch — shared with the full-saas harness.
# shellcheck disable=SC1091
# shellcheck source=lib/model_slug.sh
source "$(dirname "$0")/lib/model_slug.sh"
# shellcheck disable=SC1091
# shellcheck source=lib/reconciler_container.sh
source "$(dirname "$0")/lib/reconciler_container.sh"
# Ephemeral-CP tenant topology (Host/X-Molecule-Org-Slug slug-routing + CORS
# Origin). SHARED helper; default (all MOLECULE_TENANT_* unset) reproduces exact
# staging behaviour byte-for-byte.
# shellcheck disable=SC1091
# shellcheck source=lib/tenant_topology.sh
source "$(dirname "$0")/lib/tenant_topology.sh"
# Kill primitive + leak sweep — provider-abstracted. The AWS lib is sourced ONLY
# on the legacy AWS path (its e2e_* helpers are undefined on the molecules-server
# path, and every call site is provider-branched below).
if [ "$PROVIDER" = "aws" ]; then
  # AWS kill primitive + leak sweep (e2e_aws_region / e2e_ec2_instances_for_slug /
  # e2e_terminate_instances / e2e_verify_no_ec2_leaks_for_slug).
  # shellcheck disable=SC1091
  # shellcheck source=lib/aws_leak_check.sh
  source "$(dirname "$0")/lib/aws_leak_check.sh"
fi

# ─── provider abstraction ───────────────────────────────────────────────
# The reconciler-heal ASSERTIONS (leaves 'online', reprovisions on a new
# instance_id) are provider-agnostic. Only the three infra primitives differ
# between an EC2 box and a local-docker container, so they are isolated here:
#   require_kill_capability          — the runner can kill the workspace instance
#   kill_workspace_instance <id> <slug>  — the "out-of-band terminate" primitive
#   verify_no_workspace_leaks <slug> — teardown left no orphan box/container
#
# molecules-server (local-docker): the workspace's instance_id IS its docker
# container name (mol-ws-<slug≤20>-<short12>, wsname SSOT; CP local_docker_
# workspace.go returns WorkspaceInstance{InstanceID: name}). So `docker rm -f
# <instance_id>` is the exact analog of `aws ec2 terminate-instances
# --instance-ids <instance_id>` — the container is gone, its named volumes
# survive, and the reconciler's CPProvisioner.IsRunning (Healthcheck =
# `docker inspect -f {{.State.Running}}`) then reads "not running" → onOffline →
# existing-volume reprovision, exactly like the EC2 path.
require_kill_capability() {
  if [ "$PROVIDER" = "aws" ]; then
    e2e_aws_creds_available || fail "AWS CLI/creds unavailable — cannot terminate the EC2 to exercise the reconciler. Set AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY (the CI workflow wires these)."
    return
  fi
  # molecules-server: need a reachable docker daemon (the one CP's local-docker
  # backend provisioned the container on — a local run shares it; a remote CP
  # needs DOCKER_HOST to point at the molecules-server host).
  docker info >/dev/null 2>&1 || fail "docker daemon unreachable — the molecules-server reconciler test kills the workspace CONTAINER. Point DOCKER_HOST at the molecules-server host (a local stack shares the daemon)."
}

# kill_workspace_instance <instance_id> <slug> — echoes the id(s) it killed.
kill_workspace_instance() {
  local instance_id="$1" slug="$2"
  if [ "$PROVIDER" = "aws" ]; then
    local region; region=$(e2e_aws_region)
    if [ -n "$instance_id" ]; then
      log "    Terminating $instance_id in $region (aws ec2 terminate-instances)..." >&2
      aws ec2 terminate-instances --region "$region" --instance-ids "$instance_id" >/dev/null \
        || fail "aws ec2 terminate-instances failed for $instance_id"
      echo "$instance_id"
      return
    fi
    # Fallback: find by slug tag and terminate.
    log "    instance_id was empty — falling back to slug-tag describe ($slug)..." >&2
    local rows killed
    rows=$(e2e_ec2_instances_for_slug "$slug" 2>/dev/null || echo "")
    killed=$(echo "$rows" | awk 'NF {print $1}' | sort -u | tr '\n' ' ')
    [ -n "$killed" ] || fail "No slug-tagged EC2 found for $slug — nothing to terminate"
    log "    Terminating $killed in $region..." >&2
    e2e_terminate_instances "$killed" || fail "terminate-instances failed for $killed"
    echo "$killed"
    return
  fi
  # molecules-server: instance_id is the container name — docker rm -f removes the
  # container (volumes persist for the existing-volume reprovision heal).
  [ -n "$instance_id" ] || fail "molecules-server: workspace reported no instance_id (container name) — nothing to docker-kill"
  log "    docker rm -f $instance_id (out-of-band container kill)..." >&2
  docker rm -f "$instance_id" >/dev/null 2>&1 \
    || fail "docker rm -f failed for container $instance_id (already gone? the reconciler needs a real kill to exercise)"
  echo "$instance_id"
}

# verify_no_workspace_leaks <slug> — asserts teardown left no orphan box/container.
# Exit codes mirror e2e_verify_no_ec2_leaks_for_slug: 0 clean, 2 tooling-missing,
# non-zero (else) leak.
verify_no_workspace_leaks() {
  local slug="$1"
  if [ "$PROVIDER" = "aws" ]; then
    e2e_verify_no_ec2_leaks_for_slug "$slug"
    return $?
  fi
  # molecules-server: no mol-ws-<slug…> container may survive teardown. Derive the
  # prefix from the killed container name when we have it (robust vs slug
  # sanitization/truncation); otherwise match mol-ws-* whose name embeds the slug.
  docker info >/dev/null 2>&1 || return 2
  local prefix survivors
  if [ -n "${ORIGINAL_INSTANCE_ID:-}" ]; then
    prefix="${ORIGINAL_INSTANCE_ID%-*}"   # mol-ws-<slug≤20>  (drop -<short12>)
  else
    prefix="mol-ws-"
  fi
  survivors=$(docker ps -a --format '{{.Names}}' 2>/dev/null | grep -F "$prefix" || true)
  if [ -n "$survivors" ]; then
    echo "⚠️  LEAK: molecules-server container(s) survived teardown (prefix=$prefix): $survivors" >&2
    return 4
  fi
  return 0
}

CURL_COMMON=(-sS --fail-with-body --max-time 30)

# ─── cleanup trap ───────────────────────────────────────────────────────
# Identical teardown contract to test_staging_full_saas.sh: delete the
# tenant (synchronous GDPR cascade), poll for the org row to disappear, then
# assert no slug-tagged EC2 survives. A leaked resource at teardown is a CI
# failure (exit 4). The trap is installed UP-FRONT so a mid-test failure
# (including a failed PRIMARY assertion) still cleans up.
CLEANUP_DONE=0
cleanup_org() {
  # Capture upstream exit code IMMEDIATELY — must be the first statement in
  # the trap, before any command (including the CLEANUP_DONE check) clobbers $?.
  local entry_rc=$?

  if [ "$CLEANUP_DONE" = "1" ]; then return 0; fi
  CLEANUP_DONE=1

  if [ "${E2E_KEEP_ORG:-0}" = "1" ]; then
    log "E2E_KEEP_ORG=1 — skipping teardown. Manually delete $SLUG when done."
    return 0
  fi

  log "🧹 Tearing down org $SLUG..."

  # 120s curl budget for the synchronous DELETE cascade (EC2 terminate alone
  # is 30-60s), then poll up to 60s for organizations.status='purged'/gone.
  if curl "${CURL_COMMON[@]}" --max-time 120 -X DELETE "$CP_URL/cp/admin/tenants/$SLUG" \
    -H "Authorization: Bearer $ADMIN_TOKEN" \
    -H "Content-Type: application/json" \
    -d "{\"confirm\":\"$SLUG\"}" >/dev/null 2>&1; then
    ok "Teardown request accepted"
  else
    log "Teardown returned non-2xx (may already be gone)"
  fi

  local leak_count=1
  local elapsed=0
  while [ "$elapsed" -lt 60 ]; do
    leak_count=$(curl "${CURL_COMMON[@]}" "$CP_URL/cp/admin/orgs" \
      -H "Authorization: Bearer $ADMIN_TOKEN" 2>/dev/null \
      | python3 -c "import json,sys; d=json.load(sys.stdin); print(sum(1 for o in d.get('orgs', []) if o.get('slug')=='$SLUG' and o.get('status') != 'purged'))" \
      2>/dev/null || echo 1)
    if [ "$leak_count" = "0" ]; then
      break
    fi
    sleep 5
    elapsed=$((elapsed + 5))
  done

  if [ "$leak_count" != "0" ]; then
    echo "⚠️  LEAK: org $SLUG still present post-teardown after ${elapsed}s (count=$leak_count)" >&2
    exit 4
  fi
  local leak_rc=0
  verify_no_workspace_leaks "$SLUG" || leak_rc=$?
  if [ "$leak_rc" != "0" ]; then
    case "$leak_rc" in
      2) exit 2 ;;
      *) exit 4 ;;
    esac
  fi
  ok "Teardown clean — no orphan org or workspace box/container for $SLUG (${elapsed}s)"

  # Normalize unexpected upstream exit codes to 1 — `set -e` propagates the
  # raw exit code of the failing command (e.g. curl exits 22 under
  # --fail-with-body), but this script's contract only emits {0,1,2,3,4}.
  case "$entry_rc" in
    0|1|2|3|4) ;;
    *) exit 1 ;;
  esac
}
trap cleanup_org EXIT INT TERM

# ─── 0. Preflight ───────────────────────────────────────────────────────
log "═══════════════════════════════════════════════════════════════════"
log " Staging reconciler-heals-terminated-instance E2E (core#2261)"
log "   CP:                 $CP_URL"
log "   Slug:               $SLUG"
log "   Runtime:            $RUNTIME"
log "   Online timeout:     ${WORKSPACE_ONLINE_TIMEOUT_SECS}s"
log "   PRIMARY (offline):  ${RECONCILE_OFFLINE_TIMEOUT_SECS}s"
log "   SECONDARY (reprov): ${REPROVISION_TIMEOUT_SECS}s"
log "═══════════════════════════════════════════════════════════════════"

log "0/6 Preflight: CP reachable?"
curl "${CURL_COMMON[@]}" "$CP_URL/health" >/dev/null || fail "CP health check failed"
ok "CP reachable"

admin_call() {
  local method="$1"; shift
  local path="$1"; shift
  curl "${CURL_COMMON[@]}" -X "$method" "$CP_URL$path" \
    -H "Authorization: Bearer $ADMIN_TOKEN" \
    -H "Content-Type: application/json" \
    "$@"
}

# ─── 1. Create org ──────────────────────────────────────────────────────
# create_org_with_retry (SSOT lib) tolerates a TRANSIENT 502/503/504 from the
# tenant edge during a staging-tenant-cd roll (the create-502 false-ready
# class) while still failing closed on any 4xx. The shared CP /health preflight
# is 200 on the surviving instance mid-roll, so it is not the real "ready to
# create" signal — the create itself must ride out the transport blip.
# shellcheck source=lib/create_org_with_retry.sh
source "$(dirname "$0")/lib/create_org_with_retry.sh"
log "1/6 Creating org $SLUG via /cp/admin/orgs (provider=$PROVIDER)..."
create_org_with_retry "$CP_URL" "$ADMIN_TOKEN" \
  "{\"slug\":\"$SLUG\",\"name\":\"E2E $SLUG\",\"owner_user_id\":\"e2e-runner:$SLUG\",\"provider\":\"$PROVIDER\"}" \
  || fail "Org create failed (non-transient / retries exhausted): $CREATE_ORG_RESP"
CREATE_RESP="$CREATE_ORG_RESP"
echo "$CREATE_RESP" | python3 -m json.tool >/dev/null || fail "Org create returned non-JSON: $CREATE_RESP"
ORG_ID=$(echo "$CREATE_RESP" | python3 -c "import json,sys; print(json.load(sys.stdin).get('id',''))")
[ -z "$ORG_ID" ] && fail "Org create response missing 'id': $CREATE_RESP"
ok "Org created (id=$ORG_ID)"

# ─── 2. Wait for tenant provisioning ────────────────────────────────────
log "2/6 Waiting for tenant provisioning (up to ${PROVISION_TIMEOUT_SECS}s)..."
DEADLINE=$(( $(date +%s) + PROVISION_TIMEOUT_SECS ))
LAST_STATUS=""
while true; do
  if [ "$(date +%s)" -gt "$DEADLINE" ]; then
    fail "Tenant provisioning timed out after ${PROVISION_TIMEOUT_SECS}s (last: $LAST_STATUS)"
  fi
  LIST_JSON=$(admin_call GET /cp/admin/orgs 2>/dev/null || echo '{"orgs":[]}')
  # /cp/admin/orgs exposes 'instance_status' (org_instances.status), NOT 'status'.
  STATUS=$(echo "$LIST_JSON" | python3 -c "
import json, sys
d = json.load(sys.stdin)
for o in d.get('orgs', []):
    if o.get('slug') == '$SLUG':
        print(o.get('instance_status', ''))
        sys.exit(0)
print('')
" 2>/dev/null || echo "")
  if [ "$STATUS" != "$LAST_STATUS" ]; then
    log "    status → $STATUS"
    LAST_STATUS="$STATUS"
  fi
  case "$STATUS" in
    running)  break ;;
    failed)
      log "── DIAGNOSTIC BURST (step 2 — tenant provisioning failed) ──"
      echo "$LIST_JSON" | python3 -c "
import json, sys
d = json.load(sys.stdin)
for o in d.get('orgs', []):
    if o.get('slug') == '$SLUG':
        print(json.dumps(o, indent=2))
        sys.exit(0)
print('(no org row found for slug=$SLUG — DB drift?)')
" 2>&1 | sed 's/^/  /'
      log "── END DIAGNOSTIC ──"
      # Tenant provisioning failures are a CP-side fault, not a reconciler
      # regression — exit 3 (provisioning) to keep the signal honest.
      echo "[$(date +%H:%M:%S)] ❌ Tenant provisioning failed for $SLUG (see diagnostic above)" >&2
      exit 3
      ;;
    *)        sleep 15 ;;
  esac
done
ok "Tenant provisioning complete"

# Derive the tenant-facing routing/CORS topology via the shared helper. Default
# (all MOLECULE_TENANT_* unset) keeps the exact staging slug.<domain> subdomain +
# no route headers; the ephemeral runner points MOLECULE_TENANT_URL at the CP base
# and sets MOLECULE_TENANT_ROUTE_DOMAIN / _ORIGIN_TEMPLATE for slug-routing. Sets
# TENANT_URL / TENANT_ROUTE_HOST / TENANT_ROUTE_HDRS[] / TENANT_ORIGIN in this scope.
derive_tenant_topology "$SLUG" "$CP_URL" \
  || fail "Could not derive tenant topology for $SLUG (ephemeral slug-routing needs MOLECULE_TENANT_ORIGIN_TEMPLATE — see lib/tenant_topology.sh)"
log "    TENANT_URL=$TENANT_URL"
if [ ${#TENANT_ROUTE_HDRS[@]} -gt 0 ]; then
  log "    tenant routing via Host=$TENANT_ROUTE_HOST + X-Molecule-Org-Slug=$SLUG (ephemeral-CP slug routing); CORS origin=$TENANT_ORIGIN"
fi

# ─── 3. Retrieve per-tenant admin token ────────────────────────────────
log "3/6 Fetching per-tenant admin token..."
TENANT_TOKEN_RESP=$(admin_call GET "/cp/admin/orgs/$SLUG/admin-token")
TENANT_TOKEN=$(echo "$TENANT_TOKEN_RESP" | python3 -c "import json,sys; print(json.load(sys.stdin).get('admin_token',''))" 2>/dev/null || echo "")
[ -z "$TENANT_TOKEN" ] && fail "Could not retrieve per-tenant admin token for $SLUG"
ok "Tenant admin token retrieved (len=${#TENANT_TOKEN})"

# Wait for tenant readiness before any tenant API call. Under ephemeral slug-
# routing the CP answers /health for ANY Host (vacuous readiness), so probe
# /org/identity — a tenant-owned handler the CP proxies — WITH the route headers +
# X-Molecule-Org-Id so a not-yet-routable tenant is actually caught. Staging (empty
# TENANT_ROUTE_HDRS) keeps the global /health probe ⇒ exact staging behaviour.
# Mirrors test_staging_concierge_e2e.sh / test_staging_full_saas.sh step 4.
log "    Waiting for tenant TLS / DNS / routing propagation..."
TLS_DEADLINE=$(( $(date +%s) + 15 * 60 ))
while true; do
  if [ ${#TENANT_ROUTE_HDRS[@]} -gt 0 ]; then
    curl -sSfk --max-time 5 "${TENANT_ROUTE_HDRS[@]}" -H "X-Molecule-Org-Id: $ORG_ID" "$TENANT_URL/org/identity" >/dev/null 2>&1 && break
  else
    curl -sSfk --max-time 5 "$TENANT_URL/health" >/dev/null 2>&1 && break
  fi
  if [ "$(date +%s)" -gt "$TLS_DEADLINE" ]; then
    fail "Tenant never became routable within 15m (probe: $( [ ${#TENANT_ROUTE_HDRS[@]} -gt 0 ] && echo '/org/identity via route headers' || echo '/health' ))"
  fi
  sleep 5
done
ok "Tenant reachable at $TENANT_URL"

tenant_call() {
  local method="$1"; shift
  local path="$1"; shift
  # X-Molecule-Org-Id is REQUIRED — the tenant guard 404s anything without it
  # (it does NOT 403, to hide tenant existence from org scanners).
  curl "${CURL_COMMON[@]}" -X "$method" "$TENANT_URL$path" \
    "${TENANT_ROUTE_HDRS[@]}" \
    -H "Authorization: Bearer $TENANT_TOKEN" \
    -H "X-Molecule-Org-Id: $ORG_ID" \
    -H "Origin: $TENANT_ORIGIN" \
    "$@"
}

# Helper: read a single field off GET /workspaces/<id>. Echoes '' on any
# error so callers can poll without `set -e` aborting on a transient blip.
ws_field() {
  local wid="$1"; local field="$2"
  tenant_call GET "/workspaces/$wid" 2>/dev/null \
    | python3 -c "import json,sys; print(json.load(sys.stdin).get('$field') or '')" 2>/dev/null \
    || echo ""
}

# ─── 4. Provision ONE workspace ─────────────────────────────────────────
# Same secrets-injection priority chain as the full-saas harness so the
# FIRST boot reaches online. We never make a completion in this test (the
# whole exercise is instance-state, not the LLM), so an absent key is
# tolerable — but wiring the same keys keeps boot behaviour identical to the
# sibling and avoids a config path that only this test would exercise.
SECRETS_JSON='{}'
# Platform-managed path (E2E_LLM_PATH=platform, the DEFAULT for this test):
# the workspace boots on the CP LLM proxy with NO tenant key, model
# moonshot/kimi-k2.6 — the exact create combo test_staging_full_saas.sh uses
# successfully. This test only needs the workspace to reach status=online so
# it can kill the EC2 and assert the reconciler heals it; it does NOT exercise
# a real LLM completion, so the platform path is both sufficient and the one
# proven to create cleanly. (The BYOK key paths below 400'd at create — see
# the create-failure capture added below — which is why platform is default.)
if [ "${E2E_LLM_PATH:-platform}" = "platform" ]; then
  log "    LLM path: PLATFORM-MANAGED (no tenant key; SSOT default model via proxy)"
  SECRETS_JSON='{}'
elif [ -n "${E2E_MINIMAX_API_KEY:-}" ]; then
  SECRETS_JSON=$(python3 -c "import json,os; print(json.dumps({'MINIMAX_API_KEY': os.environ['E2E_MINIMAX_API_KEY']}))")
elif [ -n "${E2E_ANTHROPIC_API_KEY:-}" ]; then
  SECRETS_JSON=$(python3 -c "import json,os; print(json.dumps({'ANTHROPIC_API_KEY': os.environ['E2E_ANTHROPIC_API_KEY']}))")
elif [ -n "${E2E_OPENAI_API_KEY:-}" ]; then
  SECRETS_JSON=$(python3 -c "
import json, os
k = os.environ['E2E_OPENAI_API_KEY']
print(json.dumps({
    'OPENAI_API_KEY': k,
    'OPENAI_BASE_URL': 'https://api.openai.com/v1',
    'MODEL_PROVIDER': 'openai:gpt-4o',
    'HERMES_INFERENCE_PROVIDER': 'custom',
    'HERMES_CUSTOM_BASE_URL': 'https://api.openai.com/v1',
    'HERMES_CUSTOM_API_KEY': k,
    'HERMES_CUSTOM_API_MODE': 'chat_completions',
}))
")
fi

E2E_LLM_PATH="${E2E_LLM_PATH:-platform}" MODEL_SLUG=$(E2E_LLM_PATH="${E2E_LLM_PATH:-platform}" pick_model_slug "$RUNTIME")
log "    MODEL_SLUG=$MODEL_SLUG"

log "4/6 Provisioning workspace (runtime=$RUNTIME)..."
# --fail-with-body makes curl exit non-zero on a 4xx/5xx but STILL writes the
# response body to stdout; the `|| { ... }` catches that so the body is printed
# instead of `set -e` aborting the command-substitution silently (the old bug
# that hid the real HTTP-400 reason). $WS_RESP holds the body either way.
WS_RESP=$(tenant_call POST /workspaces \
  -H "Content-Type: application/json" \
  -d "{\"name\":\"E2E Reconciler\",\"runtime\":\"$RUNTIME\",\"tier\":2,\"model\":\"$MODEL_SLUG\",\"secrets\":$SECRETS_JSON}") || {
  rc=$?
  fail "Workspace create failed (curl rc=$rc, model=$MODEL_SLUG). Response body: $WS_RESP"
}
WS_ID=$(echo "$WS_RESP" | python3 -c "import json,sys; print(json.load(sys.stdin).get('id',''))" 2>/dev/null)
[ -z "$WS_ID" ] && fail "Workspace create response missing 'id' (model=$MODEL_SLUG): $WS_RESP"
log "    WS_ID=$WS_ID"

# Wait for the workspace to reach status=online and capture its instance_id.
log "    Waiting for workspace to reach status=online (up to $((WORKSPACE_ONLINE_TIMEOUT_SECS/60)) min)..."
ONLINE_DEADLINE=$(( $(date +%s) + WORKSPACE_ONLINE_TIMEOUT_SECS ))
ORIGINAL_INSTANCE_ID=""
ONLINE_SINCE=""
# Grace before falling back to the AWS workspace tag when the tenant API
# does not surface instance_id (observed on staging).
INSTANCE_ID_GRACE_SECS="${E2E_INSTANCE_ID_GRACE_SECS:-45}"
WS_LAST_STATUS=""
while true; do
  if [ "$(date +%s)" -gt "$ONLINE_DEADLINE" ]; then
    # Boot-failure diagnostic burst (#2310-class): last_sample_error is often
    # EMPTY for a config-resolution failure (the agent never sampled — it
    # failed before its first heartbeat), so a bare "err=" tells us nothing
    # (run 223233). Surface the FULL workspace record + every plausible error
    # field so the actual reason (e.g. unservable provider, missing key, wrong
    # model arm) is visible without re-running.
    WS_LAST_ERR=$(ws_field "$WS_ID" "last_sample_error")
    log "── DIAGNOSTIC BURST (step 4 — workspace never reached online) ──"
    log "    model=$MODEL_SLUG  llm_path=${E2E_LLM_PATH:-platform}  secrets=$([ "$SECRETS_JSON" = '{}' ] && echo '(none)' || echo '(set)')"
    for f in status last_sample_error last_error error provisioning_error instance_id instance_status; do
      log "    ${f}=$(ws_field "$WS_ID" "$f")"
    done
    log "    full record:"
    tenant_call GET "/workspaces/$WS_ID" 2>/dev/null \
      | python3 -m json.tool 2>/dev/null | sed 's/^/      /' \
      || log "      (could not fetch /workspaces/$WS_ID)"
    log "── END DIAGNOSTIC ──"
    if [ "$WS_LAST_STATUS" = "online" ]; then
      fail "Workspace $WS_ID reached status=online but no kill target could be resolved within ${WORKSPACE_ONLINE_TIMEOUT_SECS}s (API omits instance_id; provider=$PROVIDER fallback found no matching instance/container — see diagnostic burst above)"
    fi
    fail "Workspace $WS_ID never reached status=online within ${WORKSPACE_ONLINE_TIMEOUT_SECS}s (last status=$WS_LAST_STATUS, err=$WS_LAST_ERR; see diagnostic burst above)"
  fi
  WS_STATUS=$(ws_field "$WS_ID" "status")
  if [ "$WS_STATUS" != "$WS_LAST_STATUS" ]; then
    log "    $WS_ID → $WS_STATUS"
    WS_LAST_STATUS="$WS_STATUS"
  fi
  if [ "$WS_STATUS" = "online" ]; then
    [ -z "$ONLINE_SINCE" ] && ONLINE_SINCE=$(date +%s)
    ORIGINAL_INSTANCE_ID=$(ws_field "$WS_ID" "instance_id")
    if [ -n "$ORIGINAL_INSTANCE_ID" ]; then
      break
    fi
    # The workspace is online but the tenant API does not surface instance_id
    # (verified live on staging: GET /workspaces/:id has NO instance_id field —
    # the DB has it, the response omits it, on BOTH providers). After a short
    # grace we resolve the kill target from the provider's own control surface,
    # not the optional API field. The reconciler reads instance_id from the DB
    # and acts on the real box regardless of what the API surfaces, so a
    # provider-resolved instance is the correct kill target. Without a fallback
    # the loop spins to the online deadline and fails with a misleading "never
    # reached online" (last status=online) — the actual regression this fixes.
    if [ "$PROVIDER" = "aws" ] && [ $(( $(date +%s) - ONLINE_SINCE )) -ge "$INSTANCE_ID_GRACE_SECS" ]; then
      # ws-tenant-<slug>-<wsid...> is the workspace EC2 (vs tenant-<slug>).
      ORIGINAL_INSTANCE_ID=$(e2e_ec2_instances_for_slug "$SLUG" 2>/dev/null \
        | awk '$2 ~ /^ws-tenant-/ {print $1}' | sort -u | head -1)
      if [ -n "$ORIGINAL_INSTANCE_ID" ]; then
        log "    instance_id not surfaced by API after ${INSTANCE_ID_GRACE_SECS}s — using AWS workspace tag: $ORIGINAL_INSTANCE_ID"
        break
      fi
    fi
    # molecules-server (local-docker): the SAME grace-fallback the AWS path has,
    # resolved from the docker daemon the kill step already uses (require_kill_
    # capability guarantees it is reachable). The workspace container is named
    # `mol-ws-<slug≤20>-<short12>` where <short12> is the workspace UUID with
    # dashes stripped, first 12 hex chars (wsname SSOT; verified live: WS_ID
    # d32826e3-45e6-… → mol-ws-<slug>-d32826e345e6). An org runs MULTIPLE mol-ws
    # containers (concierge + this workspace), so we disambiguate on that exact
    # per-workspace suffix — NEVER a bare slug match that could kill the wrong
    # box. This is the real ready signal (status=online AND a resolvable kill
    # target), not the optional API field.
    if [ "$PROVIDER" != "aws" ] && [ $(( $(date +%s) - ONLINE_SINCE )) -ge "$INSTANCE_ID_GRACE_SECS" ] && docker info >/dev/null 2>&1; then
      WS_FRAG=${WS_ID//-/}
      WS_FRAG=${WS_FRAG:0:12}
      ORIGINAL_INSTANCE_ID=$(resolve_molecules_server_container "$WS_ID")
      if [ -n "$ORIGINAL_INSTANCE_ID" ]; then
        log "    instance_id not surfaced by API after ${INSTANCE_ID_GRACE_SECS}s — resolved container from docker: $ORIGINAL_INSTANCE_ID"
        break
      fi
      log "    $WS_ID online but no mol-ws-*-$WS_FRAG container yet (docker) — waiting"
    fi
    log "    $WS_ID online but instance_id not populated yet — waiting"
  fi
  # 'failed' is transient on cold boot (bootstrap-watcher deadline vs heartbeat
  # recovery, cp#245). Keep polling; only the deadline hard-fails.
  sleep 10
done
ok "Workspace online (instance_id=$ORIGINAL_INSTANCE_ID)"

# ─── 5. Kill the workspace instance (EC2 terminate → docker rm -f) ───────
# Kill the EXACT instance the workspace reported. On AWS this is `aws ec2
# terminate-instances`; on molecules-server it is `docker rm -f <container>` —
# both remove the box out-of-band (volumes survive) so the reconciler's IsRunning
# reads "not running" and drives the SAME onOffline → existing-volume heal. The
# provider-branched primitive lives in kill_workspace_instance (top of file).
log "5/6 KILLING the workspace instance ($PROVIDER) to simulate an out-of-band termination..."
require_kill_capability
# local-docker only: capture the DOCKER CONTAINER ID (not the name) of the box we
# are about to kill. On molecules-server the reprovision heal recreates the
# container under the SAME name (mol-ws-<slug>-<short12>) — so the container NAME
# and the API's instance_id (when it even surfaces it) are IDENTICAL before and
# after the heal. The only signal that a genuine NEW instance came up is a NEW
# container ID. Without this, the 6b SECONDARY assertion below can NEVER be
# satisfied on this topology and dead-soaks the full REPROVISION_TIMEOUT_SECS
# window every run (verified: run 550823/attempt 2 sat in 6b from reprovision-
# online at 01:13:31 until the run was cancelled, ~6 min of pure soak — this
# widens the wall-clock window in which an external cancel / job timeout lands
# on the required lane, one of the two flakes in core#4548). Captured here,
# before the kill, so the post-heal compare below has a real baseline.
ORIGINAL_CONTAINER_UID=""
if [ "$PROVIDER" != "aws" ] && docker info >/dev/null 2>&1; then
  ORIGINAL_CONTAINER_UID=$(docker inspect -f '{{.Id}}' "$ORIGINAL_INSTANCE_ID" 2>/dev/null || echo "")
fi
KILLED_IDS=$(kill_workspace_instance "$ORIGINAL_INSTANCE_ID" "$SLUG")
ok "Killed workspace instance: $KILLED_IDS — reconciler should now detect the dead instance"

# ─── 6a. PRIMARY assertion — workspace leaves 'online' ─────────────────
# This is THE regression gate for core#2261/#2247. The reconciler runs every
# 60s in the tenant's workspace-server; when CPProvisioner.IsRunning returns a
# clean "not running" for the terminated EC2, onOffline flips the row off
# 'online'. A dead instance that keeps reading 'online' is exactly the bug.
log "6a/6 PRIMARY: asserting workspace leaves 'online' within ${RECONCILE_OFFLINE_TIMEOUT_SECS}s (reconciler heal-detection)..."
OFFLINE_DEADLINE=$(( $(date +%s) + RECONCILE_OFFLINE_TIMEOUT_SECS ))
LEFT_ONLINE=0
REC_LAST_STATUS=""
while true; do
  if [ "$(date +%s)" -gt "$OFFLINE_DEADLINE" ]; then
    break
  fi
  REC_STATUS=$(ws_field "$WS_ID" "status")
  if [ "$REC_STATUS" != "$REC_LAST_STATUS" ]; then
    log "    $WS_ID status → ${REC_STATUS:-<empty>}"
    REC_LAST_STATUS="$REC_STATUS"
  fi
  # Any non-online status (offline/provisioning/awaiting_agent/restarting/…)
  # proves the reconciler acted. We deliberately don't pin the exact target
  # status: onOffline flips offline AND kicks RestartByID, so the row may race
  # straight into a provisioning/restarting state — all of which are "no longer
  # falsely online".
  if [ -n "$REC_STATUS" ] && [ "$REC_STATUS" != "online" ]; then
    LEFT_ONLINE=1
    ok "PRIMARY held — workspace left 'online' (now '$REC_STATUS') after EC2 termination"
    break
  fi
  sleep 10
done

if [ "$LEFT_ONLINE" != "1" ]; then
  fail "PRIMARY FAILED (core#2261 regression): workspace $WS_ID still reads status=online ${RECONCILE_OFFLINE_TIMEOUT_SECS}s after its EC2 ($KILLED_IDS) was terminated. The reconciler did NOT detect the dead instance — a terminated EC2 is masquerading as a healthy workspace."
fi

# ─── 6b. SECONDARY assertion — auto-reprovision (best-effort) ──────────
# The onOffline → RestartByID existing-volume heal should bring the workspace
# back to 'online' on a NEW instance_id. This is best-effort: a full EC2 cold
# reprovision is a multi-minute path that shares the same boot-flake surface
# as the initial provision. If it doesn't finish within the bound we LOG it
# clearly but DO NOT fail — the PRIMARY assertion above is the gate.
#
# FUTURE TIGHTENING (deliberately one edit away): once this reprovision path
# is proven reliable on staging, promote the `log "SECONDARY ..."` soft-miss
# below to a `fail ...` so a stuck reprovision becomes a hard gate.
log "6b/6 SECONDARY (best-effort): asserting auto-reprovision to online with a NEW instance within ${REPROVISION_TIMEOUT_SECS}s..."
REPROV_DEADLINE=$(( $(date +%s) + REPROVISION_TIMEOUT_SECS ))
REPROV_OK=0
REPROV_LAST_STATUS=""
NEW_INSTANCE_ID=""
while true; do
  if [ "$(date +%s)" -gt "$REPROV_DEADLINE" ]; then
    break
  fi
  RP_STATUS=$(ws_field "$WS_ID" "status")
  if [ "$RP_STATUS" != "$REPROV_LAST_STATUS" ]; then
    log "    $WS_ID status → ${RP_STATUS:-<empty>}"
    REPROV_LAST_STATUS="$RP_STATUS"
  fi
  if [ "$RP_STATUS" = "online" ]; then
    if [ "$PROVIDER" != "aws" ]; then
      # local-docker: the tenant API does NOT surface instance_id here, and the
      # heal recreates the container under the SAME name — so "NEW instance_id"
      # via ws_field can never be satisfied (it dead-soaks to the deadline). The
      # genuine new-instance signal on this topology is a NEW docker container
      # ID under the same name. Resolve it from the daemon (same surface the
      # boot path + kill step use) and compare against the pre-kill container ID.
      NEW_CONTAINER_NAME=$(resolve_molecules_server_container "$WS_ID")
      if [ -n "$NEW_CONTAINER_NAME" ]; then
        NEW_CONTAINER_UID=$(docker inspect -f '{{.Id}}' "$NEW_CONTAINER_NAME" 2>/dev/null || echo "")
        if [ -n "$NEW_CONTAINER_UID" ] && [ "$NEW_CONTAINER_UID" != "$ORIGINAL_CONTAINER_UID" ]; then
          NEW_INSTANCE_ID="$NEW_CONTAINER_NAME"
          REPROV_OK=1
          break
        fi
      fi
      # online again but the replacement container is not up yet (or, if
      # ORIGINAL_CONTAINER_UID could not be captured, we can't prove the swap) —
      # keep polling until a distinct container ID materializes.
    else
      NEW_INSTANCE_ID=$(ws_field "$WS_ID" "instance_id")
      if [ -n "$NEW_INSTANCE_ID" ] && [ "$NEW_INSTANCE_ID" != "$ORIGINAL_INSTANCE_ID" ]; then
        REPROV_OK=1
        break
      fi
      # online again but instance_id either not surfaced yet or still the old
      # (terminated) id — keep polling until the reprovision swaps it.
    fi
  fi
  sleep 15
done

if [ "$REPROV_OK" = "1" ]; then
  if [ "$PROVIDER" != "aws" ]; then
    # Short IDs for readability only — use printf, not ${VAR:0:N}, so the
    # KI-013 container-name truncation guard (SEV-2499) does not flag a log line.
    _orig_short=$(printf '%.12s' "$ORIGINAL_CONTAINER_UID")
    _new_short=$(printf '%.12s' "$NEW_CONTAINER_UID")
    ok "SECONDARY held — auto-reprovisioned to online on a NEW container (id ${_orig_short}… → ${_new_short}…, same name $NEW_INSTANCE_ID)"
  else
    ok "SECONDARY held — auto-reprovisioned to online on NEW instance_id=$NEW_INSTANCE_ID (was $ORIGINAL_INSTANCE_ID)"
  fi
else
  # Soft-miss — see FUTURE TIGHTENING note above. PRIMARY is the gate.
  log "⚠️  SECONDARY not satisfied within ${REPROVISION_TIMEOUT_SECS}s (status=${REPROV_LAST_STATUS:-<empty>}, instance_id=${NEW_INSTANCE_ID:-<none>}, original=$ORIGINAL_INSTANCE_ID). NOT failing — the PRIMARY heal-detection assertion is the gate; reprovision is a slower, flakier cold path. Promote this to a hard fail once it's proven reliable."
fi

ok "Reconciler live E2E PASSED — PRIMARY heal-detection held (SECONDARY: $([ "$REPROV_OK" = "1" ] && echo "held" || echo "soft-miss, logged"))"
# Teardown runs via the EXIT trap.
