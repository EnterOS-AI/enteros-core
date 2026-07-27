#!/usr/bin/env bash
# Staging E2E for MCP stdio transport (runtime#61 regression).
#
# Verifies that the MCP server in the claude-code workspace image
# handles stdout redirected to a regular file — the exact failure
# mode openclaw hits when capturing MCP output.
#
# Required env:
#   MOLECULE_CP_URL        default: https://staging-api.moleculesai.app
#   MOLECULE_ADMIN_TOKEN   staging CP admin bearer from Infisical
#   E2E_INFRA_BACKEND      required: local-docker (only active backend; the SSOT
#                          teardown helper refuses to run against anything else)
#
# Optional env:
#   E2E_KEEP_ORG           1 → skip teardown (debugging only)
#   E2E_RUN_ID             Slug suffix; CI: ${GITHUB_RUN_ID}

set -euo pipefail

CP_URL="${MOLECULE_CP_URL:-https://staging-api.moleculesai.app}"
ADMIN_TOKEN="${MOLECULE_ADMIN_TOKEN:?MOLECULE_ADMIN_TOKEN required — load staging CP_ADMIN_API_TOKEN from Infisical /shared/controlplane-admin}"
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

# Collision-proof slug (core#2782). The prior `head -c 32` truncation
# dropped the run_attempt suffix and let two parallel/retry runs
# collide (POST /cp/admin/orgs 409). The helper appends a random
# 8-char uuid so every run gets a unique slug regardless of how
# the workflow composes E2E_RUN_ID.
# shellcheck source=lib/collision-proof-slug.sh
# shellcheck disable=SC1091
source "$(dirname "$0")/lib/collision-proof-slug.sh"
# SSOT teardown contract (e2e_cp_delete_and_verify_purge): exact purge receipt +
# audit row + exact-org absence, INCLUDING the bounded retry for the CP's
# transient 409 "organization has an active lifecycle operation".
# shellcheck source=tests/e2e/lib/cp_purge_receipt.sh
# shellcheck disable=SC1091
source "$(dirname "$0")/lib/cp_purge_receipt.sh"
SLUG="e2e-mcp-$(make_collision_proof_slug_suffix "${E2E_RUN_ID:-}" 8)"
assert_collision_proof_slug "$SLUG" || fail "Bug in make_collision_proof_slug: produced non-collision-proof slug '$SLUG'"

CURL_COMMON=(-sS --fail-with-body --max-time 30)

ORG_ID=""
ORG_CREATION_VERIFIED=0

# ─── cleanup trap ───────────────────────────────────────────────────────
# The DELETE goes through the SSOT helper e2e_cp_delete_and_verify_purge
# (lib/cp_purge_receipt.sh) — the SAME teardown contract full_saas, the
# concierge lanes and the peer-visibility gate use.
#
# This script used to hand-roll it, and in the weakest possible shape: ONE
# fire-and-forget DELETE whose status code was discarded entirely
# (`>/dev/null 2>&1`), with NO verification that the org ever went away. Against
# the control plane's org-lifecycle fence that is a silent leak generator:
# claimOrgPurge answers an immediate HTTP 409 "organization has an active
# lifecycle operation" while any org-scoped mutation lease is open — including
# the CP's own asynchronous workspace provision for the org this run just
# created — and a 409 writes NO purge audit row. The tenant survived and the
# script reported "Teardown request accepted" either way, because
# `--fail-with-body`'s non-zero exit was being routed into the `|| log ...`
# branch and then thrown away. Fixed once in the shared lib by 3dd3675e2 and
# never propagated here.
#
# The helper fails CLOSED — a persistent 409, any other non-200, a malformed or
# mismatched receipt, or an org that survives its budget all exit 4 — and the
# DELETE's status is now REPORTED rather than discarded.
CLEANUP_DONE=0
cleanup_org() {
  local entry_rc=$?
  if [ "$CLEANUP_DONE" = "1" ]; then return 0; fi
  CLEANUP_DONE=1

  if [ "${E2E_KEEP_ORG:-0}" = "1" ]; then
    log "E2E_KEEP_ORG=1 → leaving $SLUG behind for inspection"
    return 0
  fi

  if [ "$ORG_CREATION_VERIFIED" != "1" ]; then
    log "no verified creation-returned org identity for $SLUG — nothing this run created to delete"
    return 0
  fi

  log "Cleanup: deleting tenant $SLUG..."
  local purge_rc=0
  e2e_cp_delete_and_verify_purge "$CP_URL" "$ADMIN_TOKEN" "$SLUG" "$ORG_ID" || purge_rc=$?
  case "$purge_rc" in
    0) ok "Teardown verified: $SLUG provably purged" ;;
    2) exit 2 ;;
    *)
      echo "::warning::[teardown] $SLUG was not provably purged — investigate now; sweep-stale-e2e-orgs retries on a later main push after its 90m age floor" >&2
      [ "$entry_rc" -eq 0 ] && exit 4
      ;;
  esac
}
# INT/TERM as well as EXIT: a graceful CI cancellation delivers SIGTERM/SIGINT;
# without trapping those, bash dies before the EXIT trap runs and the tenant
# leaks (the staging-provisioning-outage leak class). A SIGKILL still bypasses
# this — the sweep-stale-e2e-orgs janitor is the backstop for that case.
trap cleanup_org EXIT INT TERM

# ─── provision tenant ───────────────────────────────────────────────────
log "Provisioning tenant $SLUG..."
TENANT=$(curl "${CURL_COMMON[@]}" -X POST "$CP_URL/cp/admin/orgs" \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d "{\"slug\":\"$SLUG\",\"name\":\"MCP Stdio E2E $SLUG\"}")
# The creation-returned id is the teardown's collision-safe authority: the helper
# refuses to DELETE a slug whose current org id is not this one. Capturing it is
# what ARMS the teardown — before this the create body was discarded outright, so
# the trap had no identity to verify against and could only fire blind.
ORG_ID=$(echo "$TENANT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
e2e_cp_validate_org_id "$ORG_ID" \
  || fail "org creation returned no usable UUID id (got '$ORG_ID') — refusing to arm a destructive teardown against it"
ORG_CREATION_VERIFIED=1
ok "Tenant provisioned (org_id=$ORG_ID)"

# ─── get tenant admin token ─────────────────────────────────────────────
log "Fetching tenant admin token..."
for _ in $(seq 1 30); do
  TOKEN_RESP=$(curl -sS --max-time 10 "$CP_URL/cp/admin/orgs/$SLUG/admin-token" \
    -H "Authorization: Bearer $ADMIN_TOKEN" 2>/dev/null || echo '{}')
  TOKEN=$(echo "$TOKEN_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('admin_token',''))" 2>/dev/null || echo "")
  [ -n "$TOKEN" ] && break
  sleep 2
done
[ -n "$TOKEN" ] || fail "Could not retrieve tenant admin token"
ok "Tenant admin token obtained"

# ─── create claude-code workspace ───────────────────────────────────────
log "Creating claude-code workspace..."
WS=$(curl "${CURL_COMMON[@]}" -X POST "$CP_URL/workspaces" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"MCP Stdio Test","role":"Test","runtime":"claude-code","tier":1}')
WS_ID=$(echo "$WS" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
ok "Workspace created: $WS_ID"

# ─── wait for online ────────────────────────────────────────────────────
log "Waiting for workspace to come online (up to 120s)..."
for _ in $(seq 1 24); do
  STATUS=$(curl -sS --max-time 10 "$CP_URL/workspaces/$WS_ID" \
    -H "Authorization: Bearer $TOKEN" 2>/dev/null \
    | python3 -c "import sys,json; print(json.load(sys.stdin).get('status',''))" 2>/dev/null || echo "")
  [ "$STATUS" = "online" ] && break
  sleep 5
done
[ "$STATUS" = "online" ] || fail "Workspace did not come online (status=$STATUS)"
ok "Workspace online"

# ─── get workspace container info ───────────────────────────────────────
log "Fetching workspace runtime info..."
RUNTIME_INFO=$(curl -sS --max-time 10 "$CP_URL/workspaces/$WS_ID" \
  -H "Authorization: Bearer $TOKEN" 2>/dev/null)
CONTAINER_ID=$(echo "$RUNTIME_INFO" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('container_id',''))" 2>/dev/null || echo "")
[ -n "$CONTAINER_ID" ] || fail "No container_id in workspace response"
ok "Container ID: $CONTAINER_ID"

# ─── MCP stdio transport test ───────────────────────────────────────────
log "Testing MCP stdio transport with regular-file stdout..."

OUTPUT=$(mktemp)
trap 'rm -f "$OUTPUT"; cleanup_org' EXIT INT TERM

# Send initialize + tools/list via stdin, capture stdout to regular file
{
  echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}'
  echo '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
} | docker exec -i -e WORKSPACE_ID="$WS_ID" "$CONTAINER_ID" \
  python -m molecule_runtime.a2a_mcp_server > "$OUTPUT" 2>&1 || {
  RC=$?
  log "MCP server exited with code $RC (expected for stdin EOF)"
}

if grep -q '"result"' "$OUTPUT"; then
  ok "MCP server handles regular-file stdout"
else
  fail "MCP server did not produce JSON-RPC result. Output:\n$(head -20 "$OUTPUT")"
fi

if grep -q '"tools"' "$OUTPUT"; then
  ok "MCP tools/list returns tools"
else
  fail "MCP tools/list did not return tools. Output:\n$(head -20 "$OUTPUT")"
fi

# ─── summary ────────────────────────────────────────────────────────────
log "All tests passed ✅"
