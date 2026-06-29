#!/usr/bin/env bash
# ═══════════════════════════════════════════════════════════════════════════════
# FULL-JOURNEY staging E2E — the SSOT real-LLM gate that runs PER-PR (as the
# required "E2E Staging Concierge Creates Workspace" context) AND is mirrored by
# the deploy-pipeline staging gate that blocks the prod promote
# (molecule-controlplane scripts/deploy/local-cp-staging-e2e-gate.sh). Every step
# is a REAL assertion (a deterministic side effect / a real completion), never a
# PONG. LLM = MiniMax (cheap): the org is platform-managed (the CP LLM proxy
# supplies the MiniMax default) so agent turns stay short + cheap.
#
#   STEP 1  CREATE A NEW ORG          via the CP admin API; wait running + tenant
#                                     TLS + per-tenant admin token.
#   STEP 2  CANVAS WORKS              the tenant canvas/app + its PROXIED API
#                                     resolve: GET /workspaces → 200 (real list),
#                                     /org/identity, /canvas/viewport,
#                                     /requests/pending all 200 + JSON (NOT
#                                     404/502 and NOT the canvas SPA HTML
#                                     fallback). This is exactly the half-wired-
#                                     tenant break controlplane#1012 fixes.
#   STEP 3  PLATFORM AGENT APPEARS    the concierge auto-installs; assert it is
#                                     present + online AND its loaded_mcp_tools
#                                     include the management verb (callable).
#   STEP 4  CREATE A TEAM             drive a real A2A message (MiniMax) asking the
#                                     concierge to create a team member; assert the
#                                     DETERMINISTIC side effect — the workspace
#                                     appears in GET /workspaces (the management
#                                     MCP provision_workspace verb was really run).
#   STEP 5  ASSIGN TEAM WORK          assign the new team member a real task over
#                                     A2A; assert it is accepted AND the round-trip
#                                     completes with a real known-answer MiniMax
#                                     completion (a2a_assert_real_completion).
#   STEP 6  TEARDOWN                  the throwaway org is deleted cleanly (trap),
#                                     leak-checked (org row + EC2).
#
# NOTE on "team": this architecture has no separate create_team verb — a team is
# realised as workspace(s) under the org, and the management MCP
# `provision_workspace` verb IS the "create a team member" action (see the
# deploy-pipeline gate's matching framing). STEP 4 asserts that verb really ran
# via its deterministic side effect; STEP 5 then assigns that member real work.
#
# FUNCTIONAL real-LLM E2E: prove the org concierge (the platform agent) can
# actually DO org-management work — send it a natural-language request and
# assert it REALLY CREATES a workspace via its platform MCP (87 org-admin tools,
# incl. provision_workspace), NOT just that a REST API returned 200.
#
# This is the RFC docs/design/rfc-platform-agent.md §11.4 "Reach" check, made
# into a gating CI test:
#
#   "chat the platform agent → it list_workspaces then provision_workspace via the
#    platform MCP and reports back via send_message_to_user."
#
# Unlike test_staging_concierge_e2e.sh (which drives the user_tasks REST+MCP
# primitive directly — a pure DB/handler contract with NO LLM), THIS test drives
# the AGENT: it sends an A2A message/send envelope (the user→concierge chat
# path) and asserts the DETERMINISTIC SIDE EFFECT — a workspace with the exact
# name we asked for now EXISTS in GET /workspaces — which can only happen if the
# concierge's LLM actually invoked the provision_workspace platform-MCP tool.
#
# The old step 4.5 asked the LLM to SELF-REPORT its tool list; that flaked because
# the LLM can omit provision_workspace even when the tool is loaded. This version
# removes that probe and relies on the real side-effect plus an optional
# `loaded_mcp_tools` inventory guard (core#3082) once the runtime producer lands.
#
# WHAT MUST BE LIVE for this to pass GREEN (else it SKIPs LOUD, never false-red):
#   • The org's concierge must be installed as the kind='platform' root AND
#     provisioned on a runtime image that installs the molecule-platform-mcp
#     PLUGIN (RFC #3045). On SaaS staging the CP installs + provisions it at
#     org-provision time. If the concierge never reaches online, or the platform
#     MCP plugin is not wired, this test SKIPs LOUD with a clear message rather
#     than failing red.
#   • A working model for the concierge. On SaaS the concierge is platform_managed
#     (the CP-exported LLM proxy supplies the model) so no BYOK key is needed for
#     the concierge itself.
#
# Env contract (same as test_staging_concierge_e2e.sh / test_staging_full_saas.sh):
#   MOLECULE_CP_URL        default: https://staging-api.moleculesai.app
#   MOLECULE_ADMIN_TOKEN   CP admin bearer — Railway staging CP_ADMIN_API_TOKEN
#
# Optional env:
#   E2E_PROVISION_TIMEOUT_SECS    default 900 (15 min cold tenant EC2 budget)
#   E2E_CONCIERGE_ONLINE_SECS     default 900 (concierge boot-to-online budget)
#   E2E_AGENT_ACT_SECS            default 420 (LLM think+tool-call budget after we
#                                 send the message — generous for nondeterminism)
#   E2E_KEEP_ORG                  1 → skip teardown (debugging only)
#   E2E_RUN_ID                    slug suffix; CI: ${GITHUB_RUN_ID}-${RUN_ATTEMPT}
#   E2E_AWS_LEAK_CHECK            auto (default) | required | off
#   E2E_AWS_TERMINATE_LEAKS      1 → terminate slug-tagged leaked EC2 on exit
#   E2E_REQUIRE_LIVE             1 → a SKIP for "no concierge on platform image"
#                                 becomes a hard FAIL (CI sets this so a silently-
#                                 missing platform-agent image can't false-green
#                                 the gate). Default 0 (local: skip-loud).
#
# Exit codes:
#   0  happy path (concierge created the workspace) OR honest skip-loud
#   1  generic / assertion failure (agent didn't act, or tool failed)
#   2  missing required env
#   3  provisioning timed out
#   4  teardown left orphan resources
#   5  E2E_REQUIRE_LIVE=1 but the concierge could not be exercised (no
#      platform-agent image / never came online) — false-green guard
set -euo pipefail

# shellcheck disable=SC1091
# shellcheck source=_lib.sh
source "$(dirname "$0")/_lib.sh"
# AWS-leak-check lib — same teardown leak assertion the full-SaaS harness uses.
# shellcheck disable=SC1091
# shellcheck source=lib/aws_leak_check.sh
source "$(dirname "$0")/lib/aws_leak_check.sh"
# Real-completion error-as-text scanner — used to detect the concierge
# surfacing its tool/LLM error AS a reply ("Agent error …") so a broken agent
# can't read as "asked but politely declined".
# shellcheck disable=SC1091
# shellcheck source=lib/completion_assert.sh
source "$(dirname "$0")/lib/completion_assert.sh"
# SSOT for the platform MCP management tool verb. Sourcing this gives us
# PLATFORM_MCP_REQUIRED_TOOL and PLATFORM_MCP_REQUIRED_TOOL_ID so the test
# cannot drift from the real tool name again.
# shellcheck disable=SC1091
# shellcheck source=lib/provision_tool_ssot.sh
source "$(dirname "$0")/lib/provision_tool_ssot.sh"

CP_URL="${MOLECULE_CP_URL:-https://staging-api.moleculesai.app}"
ADMIN_TOKEN="${MOLECULE_ADMIN_TOKEN:-}"
PROVISION_TIMEOUT_SECS="${E2E_PROVISION_TIMEOUT_SECS:-900}"
CONCIERGE_ONLINE_SECS="${E2E_CONCIERGE_ONLINE_SECS:-900}"
AGENT_ACT_SECS="${E2E_AGENT_ACT_SECS:-420}"
# STEP 5 budget: how long to wait for the concierge-created team member to reach
# online+routable before we can assign it work over A2A (cold runtime+model boot).
TEAM_ONLINE_SECS="${E2E_TEAM_ONLINE_SECS:-900}"
REQUIRE_LIVE="${E2E_REQUIRE_LIVE:-0}"

# log/fail/ok are defined HERE (before the PR-mode early-exit block) so the
# no-creds PR-mode self-check — which logs + calls fail/ok — actually works.
# (Previously these were defined further down, after the PR-mode block that
# uses them, so under `set -e` the fork-PR / local no-creds path aborted with
# `log: command not found` exit 127 before reaching the self-check exit 0.)
log()  { echo "[$(date +%H:%M:%S)] $*"; }
fail() { echo "[$(date +%H:%M:%S)] ❌ $*" >&2; exit 1; }
ok()   { echo "[$(date +%H:%M:%S)] ✅ $*"; }

# ─── PR-mode early-exit (core#3081 / CR2 #12653) ──────────────────────────────
# A required status context that never fires on pull_request degrades the
# merge gate to a silent indefinite pending (the failure mode
# lint-required-no-paths exists to prevent). The workflow sets
# E2E_REQUIRE_LIVE=0 on pull_request runs because PRs do not have staging
# creds wired; the real staging test would just exit 2 at the ADMIN_TOKEN
# check below. The PR-mode gate is a self-check:
#   - bash -n on the script's own syntax (catches PR-merge regressions
#     that break the script BEFORE it runs).
# On push / dispatch / cron, E2E_REQUIRE_LIVE=1, the real staging test
# runs against live staging, and skip_loud on missing infra exits 5
# (HARD FAIL — the false-green guard).
if [ "${REQUIRE_LIVE}" = "0" ] && [ -z "${ADMIN_TOKEN}" ]; then
  log "PR-mode: E2E_REQUIRE_LIVE=0 and no MOLECULE_ADMIN_TOKEN — skipping live staging test."
  log "(the real staging test runs on push-to-main / dispatch / cron with E2E_REQUIRE_LIVE=1)"
  # Self-check: bash -n on the script's own syntax. The script IS the
  # gate on push; on PR, the gate is 'script exists and is bash-clean'.
  if ! bash -n "$0"; then
    fail "PR-mode self-check FAILED: bash -n on $0 returned non-zero — script has a syntax error"
  fi
  ok "PR-mode self-check PASSED: $(basename "$0") is bash-clean (real staging test runs on push-to-main with E2E_REQUIRE_LIVE=1)"
  exit 0
fi
# Beyond here, we are running for real: REQUIRE_LIVE=1 OR ADMIN_TOKEN
# is set. If ADMIN_TOKEN is set but REQUIRE_LIVE=0, that's an operator-
# dispatched local run (the original PR test path) — keep the original
# strict check below.
if [ -z "${ADMIN_TOKEN}" ]; then
  fail "MOLECULE_ADMIN_TOKEN required (Railway staging CP_ADMIN_API_TOKEN) — E2E_REQUIRE_LIVE=1 needs staging creds"
fi
# Collision-proof slug (core#2782). The prior `head -c 32` truncation
# dropped the run_attempt suffix and let two parallel/retry runs
# collide (POST /cp/admin/orgs 409). The helper appends a random
# 8-char uuid so every run gets a unique slug regardless of how
# the workflow composes E2E_RUN_ID. The `source` + `assert` run
# AFTER log/fail/ok are defined below so the assert can call `fail`
# on mismatch. Slug MUST start with 'e2e-' so sweep-stale-e2e-orgs.yml
# + lint_cleanup_traps.sh reap any orphan org. (The lint requires
# a quoted SLUG=... with a literal e2e-/rt-e2e- head.)
# shellcheck source=lib/collision-proof-slug.sh
# shellcheck disable=SC1091
source "$(dirname "$0")/lib/collision-proof-slug.sh"

# The workspace name we will ask the concierge to create. The literal
# `e2e-cncrg-w-` prefix is visible to the lint (so the SLUG=
# has a covered e2e- prefix in the assignment); the uuid suffix
# makes the name unique per run so a poll for it can never collide
# with a sibling run's name.
WORKER_NAME="e2e-cncrg-w-$(make_collision_proof_slug_suffix "${E2E_RUN_ID:-}" 12)"
WORKER_NAME=$(echo "$WORKER_NAME" | tr -cd 'a-zA-Z0-9-' | head -c 48)
# Exported so the find_worker_by_name python subshell (run in a pipe) reads it
# via os.environ — a bare shell var would not survive into the subprocess env.
export WORKER_NAME

# (log/fail/ok are defined above, before the PR-mode early-exit block.)
# SLUG construction runs after log/fail/ok so the assert can call `fail`.
SLUG="e2e-cncrg-mk-$(make_collision_proof_slug_suffix "${E2E_RUN_ID:-}" 13)"
assert_collision_proof_slug "$SLUG" || fail "Bug in make_collision_proof_slug: produced non-collision-proof slug '$SLUG'"
# skip_loud <reason>: honest skip when the concierge can't be exercised. In CI
# (E2E_REQUIRE_LIVE=1) this is a HARD FAIL (exit 5) so a missing platform-agent
# image can't false-green the gate; locally it skips 0.
skip_loud() {
  echo "[$(date +%H:%M:%S)] ⏭️  SKIP: $*" >&2
  if [ "$REQUIRE_LIVE" = "1" ]; then
    echo "[$(date +%H:%M:%S)] ❌ E2E_REQUIRE_LIVE=1 — a skip is a false-green guard breach here. Failing." >&2
    exit 5
  fi
  exit 0
}

CURL_COMMON=(-sS --max-time 30)
TMPDIR_E2E=$(mktemp -d -t cncrg-mk-XXXXXX)

# ─── teardown trap (worker delete + org delete + leak check) ─────────────────
CLEANUP_DONE=0
WORKER_ID=""        # set once the concierge creates it (for targeted delete)
TENANT_URL=""       # set after provisioning
TENANT_TOKEN=""
ORG_ID=""
cleanup() {
  local entry_rc=$?
  [ "$CLEANUP_DONE" = "1" ] && return 0
  CLEANUP_DONE=1
  rm -rf "$TMPDIR_E2E" 2>/dev/null || true

  # Best-effort targeted delete of the worker the concierge created, so the org
  # delete below isn't the only thing reaping it (defensive — org delete cascades
  # anyway). Only attempted if we resolved its id and have tenant creds.
  if [ -n "$WORKER_ID" ] && [ -n "$TENANT_URL" ] && [ -n "$TENANT_TOKEN" ]; then
    curl "${CURL_COMMON[@]}" -X DELETE "$TENANT_URL/workspaces/$WORKER_ID?confirm=true" \
      -H "Authorization: Bearer $TENANT_TOKEN" \
      -H "X-Molecule-Org-Id: $ORG_ID" \
      -H "Origin: $TENANT_URL" \
      -H "X-Confirm-Name: $WORKER_NAME" >/dev/null 2>&1 || true
  fi

  if [ "${E2E_KEEP_ORG:-0}" = "1" ]; then
    log "E2E_KEEP_ORG=1 — skipping teardown. Manually delete $SLUG when done."
    return 0
  fi
  log "🧹 Tearing down org $SLUG..."
  if curl "${CURL_COMMON[@]}" --max-time 120 -X DELETE "$CP_URL/cp/admin/tenants/$SLUG" \
    -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" \
    -d "{\"confirm\":\"$SLUG\"}" >/dev/null 2>&1; then
    ok "Teardown request accepted"
  else
    log "Teardown returned non-2xx (may already be gone)"
  fi

  # Eventual-consistency wait: org row gone / purged.
  local leak_count=1 elapsed=0
  while [ "$elapsed" -lt 60 ]; do
    leak_count=$(curl "${CURL_COMMON[@]}" "$CP_URL/cp/admin/orgs" \
      -H "Authorization: Bearer $ADMIN_TOKEN" 2>/dev/null \
      | python3 -c "import json,sys; d=json.load(sys.stdin); print(sum(1 for o in d.get('orgs', []) if o.get('slug')=='$SLUG' and o.get('status') != 'purged'))" \
      2>/dev/null || echo 1)
    [ "$leak_count" = "0" ] && break
    sleep 5; elapsed=$((elapsed + 5))
  done
  if [ "$leak_count" != "0" ]; then
    echo "⚠️  LEAK: org $SLUG still present post-teardown after ${elapsed}s (count=$leak_count)" >&2
    exit 4
  fi
  local aws_leak_rc=0
  e2e_verify_no_ec2_leaks_for_slug "$SLUG" || aws_leak_rc=$?
  if [ "$aws_leak_rc" != "0" ]; then
    case "$aws_leak_rc" in 2) exit 2 ;; *) exit 4 ;; esac
  fi
  ok "Teardown clean — no orphan org or EC2 resources for $SLUG (${elapsed}s)"
  case "$entry_rc" in 0|1|2|3|4|5) ;; *) exit 1 ;; esac
}
trap cleanup EXIT INT TERM

admin_call() {  # <method> <path> [curl args…]
  local method="$1" path="$2"; shift 2
  curl "${CURL_COMMON[@]}" -X "$method" "$CP_URL$path" \
    -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" "$@"
}

# tenant_call: Authorization (tenant admin token — also authenticates the
# concierge, which holds no per-workspace token: validateDiscoveryCaller's admin
# fallback) + X-Molecule-Org-Id (TenantGuard 404s without it) + Origin (edge WAF).
tenant_call() {  # <method> <path> [curl args…]
  local method="$1" path="$2"; shift 2
  curl "${CURL_COMMON[@]}" -X "$method" "$TENANT_URL$path" \
    -H "Authorization: Bearer $TENANT_TOKEN" \
    -H "X-Molecule-Org-Id: $ORG_ID" \
    -H "Origin: $TENANT_URL" "$@"
}

# list_workspaces_json: echo the raw GET /workspaces JSON array (tenant-scoped).
list_workspaces_json() { tenant_call GET /workspaces; }

# find_platform_root: echo the id of the kind='platform' parent_id-null root, or
# "" if none. This IS the concierge — the org's front-door agent.
find_platform_root() {
  list_workspaces_json | python3 -c "
import sys, json
try: rows = json.load(sys.stdin)
except Exception: print(''); sys.exit(0)
for w in rows if isinstance(rows, list) else []:
    if w.get('kind') == 'platform' and not w.get('parent_id'):
        print(w.get('id','')); break
else:
    print('')"
}

# workspace_field <id> <field>: echo a single field off GET /workspaces/:id.
workspace_field() {  # <id> <field>
  tenant_call GET "/workspaces/$1" | python3 -c "
import sys, json
try: d = json.load(sys.stdin)
except Exception: print(''); sys.exit(0)
print(d.get('$2','') if isinstance(d, dict) else '')"
}

# find_worker_by_name: echo the id of a workspace whose name == WORKER_NAME, or
# "" if not present. THIS is the deterministic side effect we assert on.
find_worker_by_name() {
  list_workspaces_json | python3 -c "
import sys, json, os
want = os.environ['WORKER_NAME']
try: rows = json.load(sys.stdin)
except Exception: print(''); sys.exit(0)
for w in rows if isinstance(rows, list) else []:
    if w.get('name') == want:
        print(w.get('id','')); break
else:
    print('')"
}

# ─── 0. Preflight ────────────────────────────────────────────────────────────
log "═══ Staging concierge CREATES-A-WORKSPACE (real-LLM) E2E ═══  CP=$CP_URL  Slug=$SLUG"
log "    worker the concierge will be asked to create: name=$WORKER_NAME"
curl "${CURL_COMMON[@]}" "$CP_URL/health" >/dev/null || fail "CP health check failed"
ok "CP reachable"

# ─── 1. Create org (CP installs + provisions the concierge as platform root) ──
log "1/6 CREATE A NEW ORG — creating org $SLUG..."
CREATE_RESP=$(admin_call POST /cp/admin/orgs \
  -d "{\"slug\":\"$SLUG\",\"name\":\"E2E $SLUG\",\"owner_user_id\":\"e2e-runner:$SLUG\"}")
echo "$CREATE_RESP" | python3 -m json.tool >/dev/null || fail "Org create non-JSON: $CREATE_RESP"
ORG_ID=$(echo "$CREATE_RESP" | python3 -c "import json,sys; print(json.load(sys.stdin).get('id',''))")
[ -z "$ORG_ID" ] && fail "Org create response missing 'id': $CREATE_RESP"
ok "Org created (id=$ORG_ID)"

# ─── 2. Wait for tenant provisioning ─────────────────────────────────────────
log "1/6 CREATE A NEW ORG — waiting for tenant provisioning (up to ${PROVISION_TIMEOUT_SECS}s)..."
DEADLINE=$(( $(date +%s) + PROVISION_TIMEOUT_SECS ))
LAST_STATUS=""
while true; do
  [ "$(date +%s)" -gt "$DEADLINE" ] && exit 3
  LIST_JSON=$(admin_call GET /cp/admin/orgs 2>/dev/null || echo '{"orgs":[]}')
  STATUS=$(echo "$LIST_JSON" | python3 -c "
import json, sys
d = json.load(sys.stdin)
for o in d.get('orgs', []):
    if o.get('slug') == '$SLUG':
        print(o.get('instance_status', '')); sys.exit(0)
print('')" 2>/dev/null || echo "")
  if [ "$STATUS" != "$LAST_STATUS" ]; then log "    status → $STATUS"; LAST_STATUS="$STATUS"; fi
  case "$STATUS" in
    running) break ;;
    failed)  fail "Tenant provisioning failed for $SLUG" ;;
    *)       sleep 15 ;;
  esac
done
ok "Tenant provisioning complete"

# Derive tenant domain from CP hostname (prod vs staging).
CP_HOST=$(echo "$CP_URL" | sed -E 's#^https?://##; s#/.*$##')
case "$CP_HOST" in
  api.*)         DERIVED_DOMAIN="${CP_HOST#api.}" ;;
  staging-api.*) DERIVED_DOMAIN="staging.${CP_HOST#staging-api.}" ;;
  *)             DERIVED_DOMAIN="$CP_HOST" ;;
esac
TENANT_DOMAIN="${MOLECULE_TENANT_DOMAIN:-$DERIVED_DOMAIN}"
TENANT_URL="https://$SLUG.$TENANT_DOMAIN"
log "    TENANT_URL=$TENANT_URL"

# ─── 3. Per-tenant admin token + TLS readiness ───────────────────────────────
log "1/6 CREATE A NEW ORG — fetching per-tenant admin token..."
TENANT_TOKEN=$(admin_call GET "/cp/admin/orgs/$SLUG/admin-token" \
  | python3 -c "import json,sys; print(json.load(sys.stdin).get('admin_token',''))" 2>/dev/null || echo "")
[ -z "$TENANT_TOKEN" ] && fail "Could not retrieve per-tenant admin token for $SLUG"
ok "Tenant admin token retrieved (len=${#TENANT_TOKEN})"

log "    Waiting for tenant TLS / DNS propagation..."
TLS_DEADLINE=$(( $(date +%s) + 15 * 60 ))
while true; do
  curl -sSfk --max-time 5 "$TENANT_URL/health" >/dev/null 2>&1 && break
  [ "$(date +%s)" -gt "$TLS_DEADLINE" ] && fail "Tenant /health never 2xx within 15m"
  sleep 5
done
ok "Tenant reachable at $TENANT_URL"

# ─── 2. CANVAS WORKS — the tenant canvas/app + its PROXIED API actually resolve ─
# THE break this gate exists to catch (cp#576 / controlplane#1012): a tenant can
# report instance_status=running and answer /health=200 while its APP is
# half-wired (tenant↔CP network isolation + a dead CP internal port) — the canvas
# loads but every proxied API call 401/404/502s, and gin's NoRoute then proxies
# the request to the canvas SPA so the bytes come back as HTML, not JSON. /health
# is allowlisted past the tenant guard, so the shallow check stays green while the
# app is unusable. Assert each canvas-critical route returns HTTP 200 AND a JSON
# body (NOT 404/502/503, NOT the canvas SPA HTML fallback):
#   GET /workspaces       (tenant admin auth) → 200 + a real JSON list (array)
#   GET /org/identity     (open route)        → 200 + JSON object
#   GET /canvas/viewport  (open route)        → 200 + JSON (not HTML)
#   GET /requests/pending (tenant admin auth) → 200 + JSON (not HTML)
log "2/6 CANVAS WORKS — asserting the tenant's proxied API resolves (not 404/502/SPA-HTML)..."
CANVAS_BODY_TMP="$TMPDIR_E2E/canvas_body"
CANVAS_LAST_BODY=""
# canvas_probe <label> <method> <path> <auth:tenant|open> <shape:array|json|nothtml>
# Sets CANVAS_LAST_BODY on success; calls fail() (which EXITS the script — so this
# MUST NOT be invoked inside a $() command substitution, where exit would only
# leave the subshell) on any non-200 / wrong-shape / HTML-fallback.
canvas_probe() {
  local label="$1" method="$2" path="$3" auth="$4" shape="$5"
  local code rc body first
  local -a args=(-sS --max-time 25 -X "$method" -o "$CANVAS_BODY_TMP" -w '%{http_code}')
  if [ "$auth" = "tenant" ]; then
    args+=(-H "Authorization: Bearer $TENANT_TOKEN" -H "X-Molecule-Org-Id: $ORG_ID" -H "Origin: $TENANT_URL")
  fi
  set +e
  code=$(curl "${args[@]}" "$TENANT_URL$path" 2>/dev/null); rc=$?
  set -e
  body=$(cat "$CANVAS_BODY_TMP" 2>/dev/null || echo "")
  code=${code:-000}
  if [ "$rc" != "0" ]; then
    fail "CANVAS WORKS: $label ($method $path) transport error (curl_rc=$rc, http=$code) — tenant proxied API unreachable"
  fi
  if [ "$code" != "200" ]; then
    fail "CANVAS WORKS: $label ($method $path) returned HTTP $code (want 200). A 404/502/503 here is the half-wired-tenant break (controlplane#1012). Body: $(printf '%s' "$body" | head -c 200)"
  fi
  # First non-whitespace byte '<' ⇒ we got the canvas SPA HTML fallback
  # (gin NoRoute → canvas), i.e. the API route did NOT resolve. Strip ALL leading
  # whitespace (incl. newlines) from the first chunk, then take the first char.
  first=$(printf '%s' "$body" | head -c 200 | tr -d '[:space:]' | head -c 1)
  if [ "$first" = "<" ]; then
    fail "CANVAS WORKS: $label ($method $path) returned 200 but the body is HTML (canvas SPA fallback), not JSON — the API route did NOT resolve (gin NoRoute → canvas). Body: $(printf '%s' "$body" | head -c 120)"
  fi
  case "$shape" in
    array)
      [ "$first" = "[" ] || fail "CANVAS WORKS: $label ($method $path) 200 but body is not a JSON array (real list). first='$first' body: $(printf '%s' "$body" | head -c 160)" ;;
    json)
      { [ "$first" = "{" ] || [ "$first" = "[" ]; } || fail "CANVAS WORKS: $label ($method $path) 200 but body is not a JSON object/array. first='$first' body: $(printf '%s' "$body" | head -c 160)" ;;
    nothtml) : ;;  # '<' already rejected above; any other non-HTML 200 is fine
  esac
  CANVAS_LAST_BODY="$body"
}

canvas_probe "GET /workspaces" GET /workspaces tenant array
WS_LIST_BODY="$CANVAS_LAST_BODY"
ok "CANVAS WORKS: GET /workspaces → 200 + real JSON list"
canvas_probe "GET /org/identity" GET /org/identity open json
ok "CANVAS WORKS: GET /org/identity → 200 + JSON"
canvas_probe "GET /canvas/viewport" GET /canvas/viewport open nothtml
ok "CANVAS WORKS: GET /canvas/viewport → 200 (not 404/502/SPA-HTML)"
canvas_probe "GET /requests/pending" GET /requests/pending tenant nothtml
ok "CANVAS WORKS: GET /requests/pending → 200 (not 404/502/SPA-HTML)"
ok "═ STEP 2 PASS: tenant canvas/app + proxied API resolve (the controlplane#1012 break is gated)"

# ─── 3. PLATFORM AGENT APPEARS — discover the concierge (kind='platform' root) ─
log "3/6 PLATFORM AGENT APPEARS — discovering the concierge (kind='platform' root)..."
# The CP installs the platform agent at org-provision; allow a short settle for
# the row + re-parent backfill to land.
CONCIERGE_ID=""
DISC_DEADLINE=$(( $(date +%s) + 180 ))
while true; do
  CONCIERGE_ID=$(find_platform_root)
  [ -n "$CONCIERGE_ID" ] && break
  [ "$(date +%s)" -gt "$DISC_DEADLINE" ] && break
  sleep 10
done
if [ -z "$CONCIERGE_ID" ]; then
  skip_loud "no kind='platform' concierge root in this org — the platform agent was not installed at provision. \
This needs the CP platform-agent install (RFC §3) live on staging. Until then there is no agent to drive."
fi
ok "Concierge (platform root) = $CONCIERGE_ID"

# The concierge must be ONLINE + routable for its LLM to receive the A2A message
# and reach the platform MCP. Bounded poll — generous because a cold concierge
# boots its container + loads the platform MCP server before it is reachable.
log "    Waiting for the concierge to be online (up to ${CONCIERGE_ONLINE_SECS}s)..."
ONLINE_DEADLINE=$(( $(date +%s) + CONCIERGE_ONLINE_SECS ))
C_STATUS=""; C_URL=""; LAST_C_STATUS=""
while true; do
  C_STATUS=$(workspace_field "$CONCIERGE_ID" status)
  C_URL=$(workspace_field "$CONCIERGE_ID" url)
  if [ "$C_STATUS" != "$LAST_C_STATUS" ]; then log "    concierge → ${C_STATUS:-<none>}"; LAST_C_STATUS="$C_STATUS"; fi
  if [ "$C_STATUS" = "online" ] && [ -n "$C_URL" ]; then break; fi
  if [ "$(date +%s)" -gt "$ONLINE_DEADLINE" ]; then
    LAST_ERR=$(workspace_field "$CONCIERGE_ID" last_sample_error)
    skip_loud "concierge $CONCIERGE_ID never reached online+routable within ${CONCIERGE_ONLINE_SECS}s \
(last status='${C_STATUS}', url='${C_URL}', err='${LAST_ERR}'). The concierge must be provisioned on a \
runtime image that installs the molecule-platform-mcp plugin and has a working model; otherwise it cannot \
run the ${PLATFORM_MCP_REQUIRED_TOOL} tool."
  fi
  sleep 10
done
ok "Concierge online + routable (url assigned)"

# ─── 4.5. Required loaded-MCP-tools inventory check (core#3082) ──────────────
# The concierge runtime must report `loaded_mcp_tools` on its heartbeat and
# that list must include the platform management `${PLATFORM_MCP_REQUIRED_TOOL}` tool.
# This is a REQUIRED gate: if the field is absent or does not include the
# tool, the concierge cannot satisfy the core#3082 online gate and this
# test fails closed. The real side-effect assertion below is NOT sufficient
# without this deterministic inventory check.
log "3.5/6 PLATFORM AGENT APPEARS — required loaded_mcp_tools inventory check (management verb callable)..."

# concierge_loaded_mcp_tools_json: echo the concierge's `loaded_mcp_tools`
# field as a JSON array, or "" if the field is missing/unreadable.
concierge_loaded_mcp_tools_json() {
  tenant_call GET "/workspaces/$CONCIERGE_ID" 2>/dev/null | python3 -c "
import sys, json
try: d = json.load(sys.stdin)
except Exception: print(''); sys.exit(0)
if not isinstance(d, dict): print(''); sys.exit(0)
tools = d.get('loaded_mcp_tools')
if not isinstance(tools, list): print(''); sys.exit(0)
print(json.dumps(tools))"
}

LOADED_TOOLS=$(concierge_loaded_mcp_tools_json)
if [ -n "$LOADED_TOOLS" ] && [ "$LOADED_TOOLS" != "[]" ]; then
  if [ "$(loaded_mcp_tools_has_required "$LOADED_TOOLS")" = "yes" ]; then
    ok "loaded_mcp_tools inventory confirms $(required_provision_tool_id)"
  else
    skip_loud "loaded_mcp_tools inventory check FAIL: $(required_provision_tool_id) not reported. Tools: $(echo "$LOADED_TOOLS" | head -c 400)"
  fi
else
  skip_loud "loaded_mcp_tools absent or empty — the runtime producer (runtime#181) must populate this field and include $(required_provision_tool_id)"
fi

# Pre-state: the worker MUST NOT exist yet (so its later appearance is causally
# the concierge's doing, not a pre-existing row).
PRE_EXISTING=$(find_worker_by_name)
[ -n "$PRE_EXISTING" ] && fail "worker '$WORKER_NAME' already exists pre-test ($PRE_EXISTING) — name collision, cannot prove causality"
ok "Pre-state confirmed: '$WORKER_NAME' does not exist yet"

# ─── 5. Drive the AGENT: A2A message/send → it must create the workspace ──────
log "4/6 CREATE A TEAM — sending the concierge a natural-language create-team-member request..."
# Imperative + explicit to defuse LLM nondeterminism: name the tool, the exact
# workspace NAME and ROLE, and tell it not to ask a clarifying question. The
# message/send envelope is the canvas user→agent chat path (handlers/a2a_proxy.go),
# identical to the shape test_a2a_e2e.sh / test_staging_full_saas.sh use.
AGENT_PROMPT="Please create a new workspace in this org right now using your platform tools. \
Use the ${PLATFORM_MCP_REQUIRED_TOOL} tool with name exactly \"${WORKER_NAME}\" and role \"engineer\". \
Do not ask me any clarifying questions — the name and role are final. \
After the tool succeeds, reply with the new workspace id."
A2A_PAYLOAD=$(WORKER_NAME="$WORKER_NAME" AGENT_PROMPT="$AGENT_PROMPT" python3 -c "
import json, os, uuid
print(json.dumps({
    'jsonrpc': '2.0',
    'method': 'message/send',
    'id': 'e2e-cncrg-mk-1',
    'params': {
        'message': {
            'role': 'user',
            'messageId': f'e2e-{uuid.uuid4().hex[:8]}',
            'parts': [{'kind': 'text', 'text': os.environ['AGENT_PROMPT']}],
        }
    }
}))")

# Cold concierge: first turn opens TLS to the LLM, loads the platform MCP, runs
# a tool call. Give it a wide per-call window AND retry on edge cold-start 5xx.
A2A_TMP="$TMPDIR_E2E/a2a_out"
AGENT_TEXT=""
A2A_OK=0
for A2A_ATTEMPT in $(seq 1 8); do
  : >"$A2A_TMP"
  set +e
  A2A_CODE=$(tenant_call POST "/workspaces/$CONCIERGE_ID/a2a" \
    --max-time "$AGENT_ACT_SECS" \
    -H "Content-Type: application/json" \
    -d "$A2A_PAYLOAD" \
    -o "$A2A_TMP" -w '%{http_code}' 2>/dev/null)
  A2A_RC=$?
  set -e
  A2A_CODE=${A2A_CODE:-000}
  A2A_RESP=$(cat "$A2A_TMP" 2>/dev/null || echo "")
  if [ "$A2A_RC" = "0" ] && [ "$A2A_CODE" -ge 200 ] && [ "$A2A_CODE" -lt 300 ]; then
    A2A_OK=1
    break
  fi
  if echo "$A2A_CODE" | grep -Eq '^(502|503|504)$'; then
    log "    A2A cold-start attempt $A2A_ATTEMPT/8 returned $A2A_CODE — retrying"
    [ "$A2A_ATTEMPT" -lt 8 ] && { sleep 15; continue; }
  fi
  break
done
if [ "$A2A_OK" != "1" ]; then
  # A non-2xx A2A POST is an INFRA/transport failure (agent unreachable), not an
  # "agent declined" — distinct from the assertion below.
  fail "A2A POST /workspaces/$CONCIERGE_ID/a2a failed (curl_rc=$A2A_RC, http=$A2A_CODE) after $A2A_ATTEMPT attempt(s): $(echo "$A2A_RESP" | head -c 400)"
fi
AGENT_TEXT=$(echo "$A2A_RESP" | python3 -c "
import sys, json
try: d = json.load(sys.stdin)
except Exception: print(''); sys.exit(0)
parts = (d.get('result') or {}).get('parts', []) if isinstance(d, dict) else []
print(parts[0].get('text','') if parts else '')" 2>/dev/null || echo "")
log "    concierge replied (first 300 chars): $(echo "$AGENT_TEXT" | head -c 300)"

# ─── 6. ASSERT the deterministic side effect: the worker now EXISTS ───────────
log "4/6 CREATE A TEAM — polling GET /workspaces for the new team member the concierge was asked to create..."
# The create is the side effect; the LLM may take a few turns / a moment to flush
# the tool call. Poll the NAME (deterministic) — tolerant of when exactly the row
# lands, intolerant of it never landing.
ACT_DEADLINE=$(( $(date +%s) + AGENT_ACT_SECS ))
while true; do
  WORKER_ID=$(find_worker_by_name)
  [ -n "$WORKER_ID" ] && break
  if [ "$(date +%s)" -gt "$ACT_DEADLINE" ]; then
    # The agent answered but the workspace never appeared → the LLM did NOT call
    # ${PLATFORM_MCP_REQUIRED_TOOL} (or the tool failed). Distinguish the two for the operator.
    if hit=$(a2a_completion_error_marker "$AGENT_TEXT"); then
      fail "TOOL FAILED: concierge surfaced an error-as-text reply (matched '$hit') and no workspace '$WORKER_NAME' was created. \
The platform MCP ${PLATFORM_MCP_REQUIRED_TOOL} tool errored. Reply: $(echo "$AGENT_TEXT" | head -c 400)"
    fi
    fail "AGENT DID NOT ACT: concierge replied but no workspace named '$WORKER_NAME' exists in GET /workspaces after ${AGENT_ACT_SECS}s. \
The concierge's LLM did not invoke the ${PLATFORM_MCP_REQUIRED_TOOL} platform-MCP tool. \
Reply: $(echo "$AGENT_TEXT" | head -c 400)"
  fi
  sleep 8
done
ok "DETERMINISTIC SIDE EFFECT CONFIRMED: workspace '$WORKER_NAME' now EXISTS (id=$WORKER_ID)"

# Confirm it is a real workspace row (kind='workspace') parented under the org —
# i.e. a genuine create, not a no-op echo. parent_id may be the concierge (the
# concierge creates children under itself by convention) or another node; we
# assert only that it's a non-platform workspace, which is what ${PLATFORM_MCP_REQUIRED_TOOL}
# yields.
WORKER_KIND=$(workspace_field "$WORKER_ID" kind)
if [ -n "$WORKER_KIND" ] && [ "$WORKER_KIND" != "workspace" ]; then
  fail "created node '$WORKER_NAME' has kind='$WORKER_KIND' (want 'workspace') — not a real worker create"
fi
ok "Created node is a real kind='workspace' row"

# Soft confirmation: the concierge SHOULD report back. Non-fatal (the side
# effect above is the hard proof) — but a reply that is itself an error is a
# yellow flag worth logging even though the row landed.
if [ -n "$AGENT_TEXT" ]; then
  if a2a_completion_error_marker "$AGENT_TEXT" >/dev/null; then
    log "    ⚠️  concierge reply looks like an error-as-text even though the workspace was created — investigate the tool result surfacing."
  else
    ok "Concierge replied confirming the action (non-error)"
  fi
else
  log "    (concierge returned no text part — the row landing is the proof; reply is optional)"
fi

# ─── 5. ASSIGN TEAM WORK — give the new team member a real task; round-trip it ─
# The workspace the concierge just created via the management MCP
# ${PLATFORM_MCP_REQUIRED_TOOL} verb IS the team member. "Assigning work" = sending it a
# real task over the A2A user→agent path and proving the member ACCEPTS it (2xx)
# AND runs a real LLM turn that reports back. NOT a PONG: a deterministic
# known-answer arithmetic task (17×23=391), so a broken member that echoes its
# error-as-text FAILs via a2a_assert_real_completion. MiniMax keeps it cheap
# (one short turn, minimal tokens).
log "5/6 ASSIGN TEAM WORK — waiting for team member '$WORKER_NAME' ($WORKER_ID) to come online (up to ${TEAM_ONLINE_SECS}s)..."
M_STATUS=""; M_URL=""; LAST_M_STATUS=""
MEMBER_ONLINE_DEADLINE=$(( $(date +%s) + TEAM_ONLINE_SECS ))
while true; do
  M_STATUS=$(workspace_field "$WORKER_ID" status)
  M_URL=$(workspace_field "$WORKER_ID" url)
  if [ "$M_STATUS" != "$LAST_M_STATUS" ]; then log "    team member → ${M_STATUS:-<none>}"; LAST_M_STATUS="$M_STATUS"; fi
  if [ "$M_STATUS" = "online" ] && [ -n "$M_URL" ]; then break; fi
  if [ "$(date +%s)" -gt "$MEMBER_ONLINE_DEADLINE" ]; then
    M_ERR=$(workspace_field "$WORKER_ID" last_sample_error)
    skip_loud "team member $WORKER_ID never reached online+routable within ${TEAM_ONLINE_SECS}s \
(last status='${M_STATUS}', url='${M_URL}', err='${M_ERR}'). Cannot assign work / round-trip to a member \
that never booted a runtime+model. (Under E2E_REQUIRE_LIVE=1 this is a HARD FAIL — false-green guard.)"
  fi
  sleep 10
done
ok "Team member online + routable — assigning a task"

WORK_EXPECT="391"
WORK_PROMPT="You are a team member. Your assigned task: compute 17 multiplied by 23 and reply with ONLY the resulting number and nothing else."
WORK_PAYLOAD=$(WORK_PROMPT="$WORK_PROMPT" python3 -c "
import json, os, uuid
print(json.dumps({
    'jsonrpc': '2.0',
    'method': 'message/send',
    'id': 'e2e-cncrg-work-1',
    'params': {'message': {'role': 'user', 'messageId': f'e2e-{uuid.uuid4().hex[:8]}',
        'parts': [{'kind': 'text', 'text': os.environ['WORK_PROMPT']}]}}
}))")
WORK_TMP="$TMPDIR_E2E/work_out"
WORK_OK=0; WORK_CODE=000; WORK_RC=1
for WORK_ATTEMPT in $(seq 1 8); do
  : >"$WORK_TMP"
  set +e
  WORK_CODE=$(tenant_call POST "/workspaces/$WORKER_ID/a2a" \
    --max-time "$AGENT_ACT_SECS" -H "Content-Type: application/json" \
    -d "$WORK_PAYLOAD" -o "$WORK_TMP" -w '%{http_code}' 2>/dev/null)
  WORK_RC=$?
  set -e
  WORK_CODE=${WORK_CODE:-000}
  if [ "$WORK_RC" = "0" ] && [ "$WORK_CODE" -ge 200 ] && [ "$WORK_CODE" -lt 300 ]; then WORK_OK=1; break; fi
  if echo "$WORK_CODE" | grep -Eq '^(502|503|504)$'; then
    log "    assign-work A2A cold-start attempt $WORK_ATTEMPT/8 returned $WORK_CODE — retrying"
    [ "$WORK_ATTEMPT" -lt 8 ] && { sleep 15; continue; }
  fi
  break
done
WORK_RESP=$(cat "$WORK_TMP" 2>/dev/null || echo "")
[ "$WORK_OK" = "1" ] || fail "ASSIGN TEAM WORK: A2A POST to team member $WORKER_ID failed (curl_rc=$WORK_RC, http=$WORK_CODE) after $WORK_ATTEMPT attempt(s): $(echo "$WORK_RESP" | head -c 300)"
ok "Task ACCEPTED by the team member (A2A 2xx)"
WORK_TEXT=$(echo "$WORK_RESP" | python3 -c "
import sys, json
try: d = json.load(sys.stdin)
except Exception: print(''); sys.exit(0)
parts = (d.get('result') or {}).get('parts', []) if isinstance(d, dict) else []
print(parts[0].get('text','') if parts else '')" 2>/dev/null || echo "")
log "    team member replied (first 200 chars): $(echo "$WORK_TEXT" | head -c 200)"
# Hard gate: a real (non-error-as-text) round-trip containing the known answer.
a2a_assert_real_completion "$WORK_TEXT" "$WORK_EXPECT" "assign-work"
ok "═ STEP 5 PASS: work assigned to the team member round-tripped a real MiniMax completion (got '$WORK_EXPECT')"

ok "═══ FULL-JOURNEY STAGING E2E PASSED (org → canvas → agent → team → work) ═══"
log "Proven end-to-end: (1) org created; (2) tenant canvas/app + proxied API resolve (/workspaces 200 real list, /org/identity, /canvas/viewport, /requests/pending — not 404/502/SPA-HTML); \
(3) platform agent auto-appeared online with ${PLATFORM_MCP_REQUIRED_TOOL} in loaded_mcp_tools; \
(4) a natural-language A2A request → the concierge's LLM invoked ${PLATFORM_MCP_REQUIRED_TOOL} via the platform MCP → real org mutation (team member '$WORKER_NAME' id=$WORKER_ID); \
(5) the team member accepted assigned work and round-tripped a real MiniMax completion. Teardown runs via EXIT trap."
