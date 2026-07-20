#!/usr/bin/env bash
# ═══════════════════════════════════════════════════════════════════════════════
# FULL-JOURNEY staging E2E — the SSOT real-LLM scenario. Its workflow runs a
# credential-free syntax self-check on pull requests and the real live journey
# only on push-to-main / dispatch. It is an independent staging signal, not an
# ordering dependency for staging-CD or production redeploy. The controlplane
# pipeline has its own matching staging scenario. Every step here is a REAL
# assertion (a deterministic side effect / a real completion), never a PONG.
# LLM = MiniMax (cheap): the org is platform-managed (the CP LLM proxy
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
#                                     present + online, then HARD-GATE its
#                                     loaded_mcp_tools inventory (poll-then-fail):
#                                     it MUST report provision_workspace or the
#                                     gate fails. runtime#181 heartbeat producer
#                                     landed 2026-06-25. Defense-in-depth ON TOP OF
#                                     the created-agent auth check (both must hold).
#   STEP 4  CREATE A TEAM             drive a real A2A message (MiniMax) asking the
#                                     concierge to create a team member; assert the
#                                     DETERMINISTIC side effect — the workspace
#                                     appears in GET /workspaces (the management
#                                     MCP provision_workspace verb was really run)
#                                     AND (STEP 4.6) the created member received its
#                                     platform LLM auth — MOLECULE_LLM_USAGE_TOKEN
#                                     is present (presence-only via the workspace
#                                     API; never the value, never docker-exec).
#   STEP 5  ASSIGN TEAM WORK          the CREATED-AGENT AUTH + REAL-FIRST-TURN HARD
#                                     GATE: assign the new member a real task over
#                                     A2A; assert it is accepted AND the round-trip
#                                     completes with a real known-answer MiniMax
#                                     completion (a2a_assert_real_completion) — NOT
#                                     "Not logged in"/error/empty. This reds on the
#                                     created-agent auth bug (no LLM token) and is
#                                     INDEPENDENT of runtime#181.
#   STEP 6  TEARDOWN                  the throwaway org is deleted through the
#                                     synchronous CP purge; its exact receipt,
#                                     audit row, and org absence are verified.
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
# removes that probe and relies on the real side-effect PLUS a HARD-GATE
# `loaded_mcp_tools` inventory guard (core#3082) — the runtime#181 producer landed
# 2026-06-25, so §4.5 now polls-then-fails on an absent/incomplete inventory.
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
#   MOLECULE_ADMIN_TOKEN   staging CP admin bearer from Infisical /shared/controlplane-admin
#
# Optional env:
#   E2E_PROVISION_TIMEOUT_SECS    default 900 (15 min cold tenant budget)
#   E2E_CONCIERGE_ONLINE_SECS     default 900 (concierge boot-to-online budget)
#   E2E_MCP_TOOLS_SECS            default 240 (STEP 4.5 poll budget for the
#                                 loaded_mcp_tools heartbeat to publish
#                                 provision_workspace before the HARD FAIL)
#   E2E_AGENT_ACT_SECS            default 420 (LLM think+tool-call budget after we
#                                 send the message — generous for nondeterminism)
#   E2E_KEEP_ORG                  1 → skip teardown (debugging only)
#   E2E_RUN_ID                    slug suffix; CI: ${GITHUB_RUN_ID}-${RUN_ATTEMPT}
#   E2E_INFRA_BACKEND             required: local-docker (only active backend)
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
#   4  teardown receipt/audit/org-absence proof failed
#   5  E2E_REQUIRE_LIVE=1 but the concierge could not be exercised (no
#      platform-agent image / never came online) — false-green guard
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# shellcheck disable=SC1091
# shellcheck source=_lib.sh
source "$SCRIPT_DIR/_lib.sh"
# Exact synchronous CP purge receipt + exact-org absence verifier.
# shellcheck disable=SC1091
# shellcheck source=lib/cp_purge_receipt.sh
source "$SCRIPT_DIR/lib/cp_purge_receipt.sh"

# shellcheck source=lib/tenant_api_ready.sh
source "$SCRIPT_DIR/lib/tenant_api_ready.sh"
# Shared tenant-facing routing/CORS topology deriver (the #4406 contract extracted
# into ONE helper). Default (all MOLECULE_TENANT_* unset) reproduces exact staging
# behaviour; the ephemeral runner sets MOLECULE_TENANT_URL/_ROUTE_DOMAIN/_ORIGIN_
# TEMPLATE to route by slug over the loopback CP.
# shellcheck disable=SC1091
# shellcheck source=lib/tenant_topology.sh
source "$SCRIPT_DIR/lib/tenant_topology.sh"
e2e_cp_require_local_backend || exit 2
# Real-completion error-as-text scanner — used to detect the concierge
# surfacing its tool/LLM error AS a reply ("Agent error …") so a broken agent
# can't read as "asked but politely declined".
# shellcheck disable=SC1091
# shellcheck source=lib/completion_assert.sh
source "$SCRIPT_DIR/lib/completion_assert.sh"
# Shared PRESENCE-ONLY probe for the platform CP-proxy token — the SAME field
# contract full-SaaS 8c uses (one place to update when core a06a52eb lands, #4042).
# shellcheck disable=SC1091
# shellcheck source=lib/workspace_env_presence.sh
source "$SCRIPT_DIR/lib/workspace_env_presence.sh"
# SSOT for the platform MCP management tool verb. Sourcing this gives us
# PLATFORM_MCP_REQUIRED_TOOL and PLATFORM_MCP_REQUIRED_TOOL_ID so the test
# cannot drift from the real tool name again.
# shellcheck disable=SC1091
# shellcheck source=lib/provision_tool_ssot.sh
source "$SCRIPT_DIR/lib/provision_tool_ssot.sh"
# Structured observability → Loki/Grafana: every step below emits a tagged
# event (e2e_run_id + env + git_sha + step + status + duration_secs + error)
# so a failed run renders as a TIMELINE in the e2e-runs Grafana dashboard
# instead of needing a dig through raw runner/CP logs. FAIL-SOFT: a down obs
# stack never fails or slows this gate. See tests/e2e/lib/obs.sh.
# shellcheck disable=SC1091
# shellcheck source=lib/obs.sh
source "$SCRIPT_DIR/lib/obs.sh"

CP_URL="${MOLECULE_CP_URL:-https://staging-api.moleculesai.app}"
e2e_cp_require_staging_origin "$CP_URL" || exit 2
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
# fail()/ok() also feed the obs timeline: a failure is attributed to the step
# currently in flight (obs_fail_current), guarded so the pre-init PR-mode path
# emits nothing. obs is fail-soft so this never changes fail()'s exit behaviour.
fail() { echo "[$(date +%H:%M:%S)] ❌ $*" >&2; [ -n "${OBS_RUN_ID:-}" ] && obs_fail_current fail "$*"; exit 1; }
ok()   { echo "[$(date +%H:%M:%S)] ✅ $*"; }

# ─── PR-mode early-exit (core#3081 / CR2 #12653) ──────────────────────────────
# The workflow emits this job's status on pull_request, but the context is not
# presence-required by the merge queue while parked in required-contexts.txt.
# A posted red still blocks through main's branch-protection wildcard. The
# workflow sets E2E_REQUIRE_LIVE=0 on pull_request because PRs do not have
# staging creds wired; the real staging test would exit 2 at the ADMIN_TOKEN
# check below. The PR-mode arm is a self-check:
#   - bash -n on the script's own syntax (catches PR-merge regressions
#     that break the script BEFORE it runs).
# On push / dispatch, E2E_REQUIRE_LIVE=1, the real staging test
# runs against live staging, and skip_loud on missing infra exits 5
# (HARD FAIL — the false-green guard).
if [ "${REQUIRE_LIVE}" = "0" ] && [ -z "${ADMIN_TOKEN}" ]; then
  log "PR-mode: E2E_REQUIRE_LIVE=0 and no MOLECULE_ADMIN_TOKEN — skipping live staging test."
  log "(the real staging test runs on push-to-main / dispatch with E2E_REQUIRE_LIVE=1)"
  # Self-check: bash -n on the script's own syntax. The script IS the
  # live validation on push; on PR, the arm is 'script exists and is bash-clean'.
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
  fail "MOLECULE_ADMIN_TOKEN required (staging CP_ADMIN_API_TOKEN from Infisical /shared/controlplane-admin) — E2E_REQUIRE_LIVE=1 needs staging creds"
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
    [ -n "${OBS_RUN_ID:-}" ] && obs_fail_current fail "skip-as-fail (E2E_REQUIRE_LIVE=1): $*"
    exit 5
  fi
  [ -n "${OBS_RUN_ID:-}" ] && obs_fail_current skip "$*"
  exit 0
}

safe_body_preview() {
  local limit="${2:-400}"
  { printf '%s' "$1" | redact_secrets | head -c "$limit"; } || true
}

extract_a2a_text() {
  python3 "$SCRIPT_DIR/lib/a2a_text_extract.py"
}

extract_chat_history_agent_text() {
  python3 "$SCRIPT_DIR/lib/chat_history_agent_text.py"
}

a2a_text_has_expected() {
  local text="$1"
  local expected="$2"
  printf '%s' "$text" | tr '[:lower:]' '[:upper:]' | grep -qF -- "$(printf '%s' "$expected" | tr '[:lower:]' '[:upper:]')"
}

a2a_queue_id_from_response() {
  python3 -c "
import json, sys
try:
    d = json.load(sys.stdin)
except Exception:
    sys.exit(0)
if not isinstance(d, dict):
    sys.exit(0)
queued = d.get('queued') is True or str(d.get('status') or '').lower() == 'queued'
qid = d.get('queue_id') or ''
if queued and isinstance(qid, str):
    print(qid)
" 2>/dev/null
}

a2a_queue_response_body() {
  python3 -c "
import json, sys
try:
    body = json.load(sys.stdin).get('response_body')
except Exception:
    sys.exit(0)
if body is not None:
    print(json.dumps(body))
" 2>/dev/null
}

is_push_async_queued_response() {
  python3 -c "
import json, sys
try:
    d = json.load(sys.stdin)
except Exception:
    sys.exit(1)
if not isinstance(d, dict):
    sys.exit(1)
if d.get('delivery_mode') == 'push-async' and str(d.get('status') or '').lower() == 'queued':
    sys.exit(0)
sys.exit(1)
" 2>/dev/null
}

poll_a2a_queue_result() {
  local ws_id="$1"
  local queue_id="$2"
  local out_file="$3"
  local label="$4"
  local poll_tmp qstatus resp code rc poll_attempt
  poll_tmp="$TMPDIR_E2E/${label//[^a-zA-Z0-9]/_}_queue"

  for poll_attempt in $(seq 1 30); do
    : >"$poll_tmp"
    set +e
    code=$(tenant_call GET "/workspaces/$ws_id/a2a/queue/$queue_id" \
      --max-time 30 \
      -H "X-Workspace-ID: $ws_id" \
      -o "$poll_tmp" \
      -w '%{http_code}' \
      2>/dev/null)
    rc=$?
    set -e
    code=${code:-000}
    resp=$(cat "$poll_tmp" 2>/dev/null || echo "")

    if [ "$rc" != "0" ] || [ "$code" = "000" ] || [ "$code" = "404" ]; then
      log "    $label queue poll attempt $poll_attempt/30: curl_rc=$rc http=$code — retrying"
      sleep 2
      continue
    fi
    if [ "$code" -lt 200 ] || [ "$code" -ge 300 ]; then
      fail "$label queue poll failed (http=$code): $(safe_body_preview "$resp" 400)"
    fi

    qstatus=$(printf '%s' "$resp" | python3 -c "
import json, sys
try:
    print(json.load(sys.stdin).get('status', ''))
except Exception:
    print('')
" 2>/dev/null || echo "")
    case "$qstatus" in
      completed)
        local response_body
        response_body=$(printf '%s' "$resp" | a2a_queue_response_body || echo "")
        if [ -n "$response_body" ]; then
          printf '%s' "$response_body" >"$out_file"
          return 0
        fi
        ;;
      failed|dropped)
        fail "$label queue item $queue_id terminal status=$qstatus: $(safe_body_preview "$resp" 400)"
        ;;
      queued|dispatched|in_progress|"")
        log "    $label queue poll attempt $poll_attempt/30 status=${qstatus:-<empty>} — retrying"
        sleep 2
        ;;
      *)
        fail "$label queue poll unexpected status=$qstatus: $(safe_body_preview "$resp" 400)"
        ;;
    esac
  done

  fail "$label queue poll timed out waiting for $queue_id to complete"
}

poll_push_async_chat_history() {
  local ws_id="$1"
  local out_file="$2"
  local label="$3"
  local timeout_secs="${4:-$AGENT_ACT_SECS}"
  local history_tmp="$TMPDIR_E2E/${label//[^a-zA-Z0-9]/_}_chat_history"
  local deadline=$(( $(date +%s) + timeout_secs ))
  local poll_count=0 code rc text

  : >"$out_file"
  while true; do
    : >"$history_tmp"
    set +e
    code=$(tenant_call GET "/workspaces/$ws_id/chat-history?limit=20" \
      --max-time 30 \
      -o "$history_tmp" \
      -w '%{http_code}' \
      2>/dev/null)
    rc=$?
    set -e
    code=${code:-000}

    if [ "$rc" = "0" ] && [ "$code" -ge 200 ] && [ "$code" -lt 300 ]; then
      text=$(cat "$history_tmp" 2>/dev/null | extract_chat_history_agent_text 2>/dev/null || echo "")
      if [ -n "$text" ]; then
        printf '%s' "$text" >"$out_file"
        return 0
      fi
    fi

    if [ "$(date +%s)" -gt "$deadline" ]; then
      log "    $label push-async chat-history poll timed out (last http=$code rc=$rc)"
      return 1
    fi
    poll_count=$((poll_count + 1))
    if [ $((poll_count % 6)) -eq 0 ]; then
      log "    $label push-async still waiting for agent reply in chat-history (last http=$code)"
    fi
    sleep 5
  done
}

CURL_COMMON=(-sS --max-time 30)
TMPDIR_E2E=$(mktemp -d -t cncrg-mk-XXXXXX)

# ─── teardown trap (worker delete + exact CP purge proof) ────────────────────
CLEANUP_DONE=0
WORKER_ID=""        # set once the concierge creates it (for targeted delete)
TENANT_URL=""       # set after provisioning
TENANT_TOKEN=""
ORG_ID=""
ORG_CREATION_VERIFIED=0
cleanup() {
  local entry_rc=$?
  [ "$CLEANUP_DONE" = "1" ] && return 0
  CLEANUP_DONE=1
  rm -rf "$TMPDIR_E2E" 2>/dev/null || true

  if [ "$ORG_CREATION_VERIFIED" != "1" ]; then
    log "No verified creation-returned org identity for $SLUG — skipping destructive org teardown."
    [ -n "${OBS_RUN_ID:-}" ] && obs_run_end "$( [ "$entry_rc" = "0" ] && echo skip || echo fail )"
    case "$entry_rc" in 0|1|2|3|4|5) return 0 ;; *) exit 1 ;; esac
  fi

  # Best-effort targeted delete of the worker the concierge created, so the org
  # delete below isn't the only thing reaping it (defensive — org delete cascades
  # anyway). Only attempted if we resolved its id and have tenant creds.
  if [ -n "$WORKER_ID" ] && [ -n "$TENANT_URL" ] && [ -n "$TENANT_TOKEN" ]; then
    curl "${CURL_COMMON[@]}" -X DELETE "$TENANT_URL/workspaces/$WORKER_ID?confirm=true" \
      "${TENANT_ROUTE_HDRS[@]}" \
      -H "Authorization: Bearer $TENANT_TOKEN" \
      -H "X-Molecule-Org-Id: $ORG_ID" \
      -H "Origin: $TENANT_ORIGIN" \
      -H "X-Confirm-Name: $WORKER_NAME" >/dev/null 2>&1 || true
  fi

  if [ "${E2E_KEEP_ORG:-0}" = "1" ]; then
    log "E2E_KEEP_ORG=1 — skipping teardown. Manually delete $SLUG when done."
    [ -n "${OBS_RUN_ID:-}" ] && obs_run_end "$( [ "$entry_rc" = "0" ] && echo kept || echo fail )"
    return 0
  fi
  [ -n "${OBS_RUN_ID:-}" ] && obs_step_start teardown
  log "🧹 Tearing down org $SLUG..."
  local purge_verify_rc=0
  e2e_cp_delete_and_verify_purge \
    "$CP_URL" "$ADMIN_TOKEN" "$SLUG" "$ORG_ID" || purge_verify_rc=$?
  if [ "$purge_verify_rc" != "0" ]; then
    if [ -n "${OBS_RUN_ID:-}" ]; then
      obs_step_end teardown fail "CP purge receipt/audit or exact-tenant HTTP 404 verification failed" "purge_verify_rc=$purge_verify_rc"
      obs_step_end zero_leftover_verify skip "direct provider enumeration was not performed" "purge_verify_rc=$purge_verify_rc"
      obs_run_end fail
    fi
    case "$purge_verify_rc" in 2) exit 2 ;; *) exit 4 ;; esac
  fi
  local teardown_message teardown_proof
  case "$E2E_CP_PURGE_RESULT" in
    purged)
      teardown_message="CP purge completed and exact org absent"
      teardown_proof="cp_purge_receipt_audit_exact_tenant_404"
      ;;
    already_absent)
      teardown_message="Exact org already absent; no DELETE or purge audit required"
      teardown_proof="exact_tenant_404"
      ;;
    *)
      [ -n "${OBS_RUN_ID:-}" ] && obs_step_end teardown fail "CP purge helper returned an unknown success result" "result=${E2E_CP_PURGE_RESULT:-unset}"
      exit 4
      ;;
  esac
  if [ -n "${OBS_RUN_ID:-}" ]; then
    obs_step_end teardown pass "$teardown_message" "proof=$teardown_proof"
    obs_step_end zero_leftover_verify skip "direct provider enumeration is not available on this generic runner" "proof=$teardown_proof"
    obs_run_end "$( [ "$entry_rc" = "0" ] && echo pass || echo fail )"
  fi
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
    "${TENANT_ROUTE_HDRS[@]}" \
    -H "Authorization: Bearer $TENANT_TOKEN" \
    -H "X-Molecule-Org-Id: $ORG_ID" \
    -H "Origin: $TENANT_ORIGIN" "$@"
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
# Initialise the structured-obs run identity. OBS_SLUG is the org slug == the
# universal run link the leak reaper + run_footprint use; OBS_ENV is guessed
# from CP_URL. From here every step emits a tagged event to Loki/Grafana.
OBS_TEST="staging_concierge_full_journey"
# Exported so lib/obs.sh (sourced) reads them as the run's slug/org cross-link;
# export also documents to shellcheck that they are consumed out-of-file.
export OBS_SLUG="$SLUG"
export OBS_CP_URL="$CP_URL"
obs_init "$OBS_TEST"
obs_step_start preflight
curl "${CURL_COMMON[@]}" "$CP_URL/health" >/dev/null || fail "CP health check failed"
ok "CP reachable"
obs_step_end preflight pass "" "cp_url=$CP_URL"

# ─── 1. Create org (CP installs + provisions the concierge as platform root) ──
obs_step_start org_create
log "1/6 CREATE A NEW ORG — creating org $SLUG..."
CREATE_BODYFILE="$TMPDIR_E2E/create-org-response.json"
set +e
CREATE_HTTP_CODE=$(admin_call POST /cp/admin/orgs \
  -o "$CREATE_BODYFILE" -w '%{http_code}' \
  -d "{\"slug\":\"$SLUG\",\"name\":\"E2E $SLUG\",\"owner_user_id\":\"e2e-runner:$SLUG\"}")
CREATE_CURL_RC=$?
set -e
CREATE_RESP=$(cat "$CREATE_BODYFILE" 2>/dev/null || true)
if [ "$CREATE_CURL_RC" != "0" ]; then
  fail "Org create request failed (curl_rc=$CREATE_CURL_RC http=${CREATE_HTTP_CODE:-000}); raw body: $CREATE_RESP"
fi
case "$CREATE_HTTP_CODE" in
  2[0-9][0-9]) ;;
  *) fail "Org create returned non-2xx (http=${CREATE_HTTP_CODE:-000}); raw body: $CREATE_RESP" ;;
esac
echo "$CREATE_RESP" | python3 -m json.tool >/dev/null || fail "Org create non-JSON: $CREATE_RESP"
ORG_ID=$(echo "$CREATE_RESP" | python3 -c "import json,sys; print(json.load(sys.stdin).get('id',''))")
e2e_cp_validate_org_id "$ORG_ID" \
  || fail "Org create response missing a valid UUID 'id': $CREATE_RESP"
ORG_CREATION_VERIFIED=1
e2e_cp_publish_creation_identity "$SLUG" "$ORG_ID" \
  || fail "Could not publish the verified org creation identity for teardown"
export OBS_ORG_ID="$ORG_ID"
ok "Org created (id=$ORG_ID)"
obs_step_end org_create pass "" "org_id=$ORG_ID"

# ─── 2. Wait for tenant provisioning ─────────────────────────────────────────
obs_step_start provision
log "1/6 CREATE A NEW ORG — waiting for tenant provisioning (up to ${PROVISION_TIMEOUT_SECS}s)..."
DEADLINE=$(( $(date +%s) + PROVISION_TIMEOUT_SECS ))
LAST_STATUS=""
while true; do
  [ "$(date +%s)" -gt "$DEADLINE" ] && { obs_fail_current timeout "tenant provisioning exceeded ${PROVISION_TIMEOUT_SECS}s (last instance_status='$LAST_STATUS')" "last_status=$LAST_STATUS"; exit 3; }
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
obs_step_end provision pass "" "instance_status=running"

# Derive the tenant-facing routing/CORS topology via the SHARED helper
# (lib/tenant_topology.sh). Sets in THIS scope: TENANT_URL / TENANT_ROUTE_HOST /
# TENANT_ROUTE_HDRS[] (Host + X-Molecule-Org-Slug, empty on staging) / TENANT_ORIGIN.
# Default (all MOLECULE_TENANT_* unset) reproduces the exact staging
# slug.<domain> subdomain + Origin=$TENANT_URL behaviour byte-for-byte; the
# ephemeral runner passes MOLECULE_TENANT_URL=CP base + MOLECULE_TENANT_ROUTE_DOMAIN
# + MOLECULE_TENANT_ORIGIN_TEMPLATE so the tenant is reached by slug over the CP.
derive_tenant_topology "$SLUG" "$CP_URL" \
  || fail "Could not derive tenant topology for $SLUG (ephemeral slug-routing needs MOLECULE_TENANT_ORIGIN_TEMPLATE for a valid CORS Origin)"
log "    TENANT_URL=$TENANT_URL"
if [ ${#TENANT_ROUTE_HDRS[@]} -gt 0 ]; then
  log "    tenant routing via Host=$TENANT_ROUTE_HOST + X-Molecule-Org-Slug=$SLUG; CORS Origin=$TENANT_ORIGIN (ephemeral-CP slug routing)"
fi

# ─── 3. Per-tenant admin token + TLS readiness ───────────────────────────────
obs_step_start tenant_health
log "1/6 CREATE A NEW ORG — fetching per-tenant admin token..."
TENANT_TOKEN=$(admin_call GET "/cp/admin/orgs/$SLUG/admin-token" \
  | python3 -c "import json,sys; print(json.load(sys.stdin).get('admin_token',''))" 2>/dev/null || echo "")
[ -z "$TENANT_TOKEN" ] && fail "Could not retrieve per-tenant admin token for $SLUG"
ok "Tenant admin token retrieved (len=${#TENANT_TOKEN})"

log "    Waiting for tenant TLS / DNS propagation..."
TLS_DEADLINE=$(( $(date +%s) + 15 * 60 ))
while true; do
  # Under ephemeral slug-routing the CP answers /health for ANY Host (its own
  # global handler), so a route-header /health probe is vacuous — it 2xx's before
  # the tenant route is up. Probe /org/identity (a tenant-owned proxied handler)
  # WITH the route headers + X-Molecule-Org-Id so a not-yet-routable tenant is
  # actually caught. Staging (empty TENANT_ROUTE_HDRS) keeps the global /health
  # check byte-for-byte. Mirrors the #4406 reference (test_staging_full_saas.sh step 4).
  if [ ${#TENANT_ROUTE_HDRS[@]} -gt 0 ]; then
    curl -sSfk --max-time 5 "${TENANT_ROUTE_HDRS[@]}" -H "X-Molecule-Org-Id: $ORG_ID" "$TENANT_URL/org/identity" >/dev/null 2>&1 && break
  else
    curl -sSfk --max-time 5 "$TENANT_URL/health" >/dev/null 2>&1 && break
  fi
  [ "$(date +%s)" -gt "$TLS_DEADLINE" ] && fail "Tenant never became routable within 15m (probe: $( [ ${#TENANT_ROUTE_HDRS[@]} -gt 0 ] && echo '/org/identity via route headers' || echo '/health' ))"
  sleep 5
done
ok "Tenant reachable at $TENANT_URL"

# /health is allowlisted past the tenant guard, so it goes green while the app's
# real proxied routes are still coming up (controlplane#1012). The CP publishes
# instance_status=running on a /health-only canary, so 'running' can precede
# 'app serving' by a few seconds under load. Gate on the ACTUAL API contract the
# canvas assertions below depend on, to a stable streak, before asserting — a
# genuinely half-wired tenant never stabilises and this fails loudly.
# wait_tenant_api_ready (lib/tenant_api_ready.sh) is NOT slug-routing aware —
# it sends only Authorization/X-Molecule-Org-Id/Origin=$turl, with NO Host/X-Molecule-
# Org-Slug. On staging that is correct (the subdomain routes). Under ephemeral
# slug-routing it would hit the CP base URL with no route headers → the wrong/no
# tenant → a vacuous or false readiness signal. So under ephemeral run a route-
# aware readiness streak via tenant_call (which threads TENANT_ROUTE_HDRS +
# TENANT_ORIGIN); staging keeps the proven shared helper byte-for-byte.
if [ ${#TENANT_ROUTE_HDRS[@]} -gt 0 ]; then
  READY_DEADLINE=$(( $(date +%s) + 180 )); READY_STREAK=0
  while true; do
    READY_BODY="$TMPDIR_E2E/ready_body"; : >"$READY_BODY"
    set +e
    READY_CODE=$(tenant_call GET /workspaces --max-time 10 -o "$READY_BODY" -w '%{http_code}' 2>/dev/null); READY_RC=$?
    set -e
    READY_FIRST=$(head -c 256 "$READY_BODY" 2>/dev/null | tr -d '[:space:]' | head -c 1)
    if [ "$READY_RC" = "0" ] && [ "$READY_CODE" = "200" ] && [ "$READY_FIRST" = "[" ]; then
      READY_STREAK=$((READY_STREAK + 1)); [ "$READY_STREAK" -ge 2 ] && break
    else
      READY_STREAK=0
    fi
    [ "$(date +%s)" -gt "$READY_DEADLINE" ] && fail "Tenant proxied API (GET /workspaces) never became ready under slug-routing — half-wired tenant (controlplane#1012) (last http=$READY_CODE first='$READY_FIRST')"
    sleep 3
  done
else
  wait_tenant_api_ready "$TENANT_URL" /workspaces "$TENANT_TOKEN" "$ORG_ID" "concierge-creates" \
    || fail "Tenant proxied API (GET /workspaces) never became ready — half-wired tenant (controlplane#1012)"
fi
obs_step_end tenant_health pass "" "tenant_url=$TENANT_URL"

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
obs_step_start canvas
log "2/6 CANVAS WORKS — asserting the tenant's proxied API resolves (not 404/502/SPA-HTML)..."
CANVAS_BODY_TMP="$TMPDIR_E2E/canvas_body"
# canvas_probe <label> <method> <path> <auth:tenant|open> <shape:array|json|nothtml>
# Asserts the probe in place; calls fail() (which EXITS the script — so this
# MUST NOT be invoked inside a $() command substitution, where exit would only
# leave the subshell) on any non-200 / wrong-shape / HTML-fallback.
canvas_probe() {
  local label="$1" method="$2" path="$3" auth="$4" shape="$5"
  local code rc body first
  local -a args=(-sS --max-time 25 -X "$method" -o "$CANVAS_BODY_TMP" -w '%{http_code}')
  # Thread the ephemeral slug-routing headers (Host + X-Molecule-Org-Slug) on
  # EVERY canvas probe, incl. the open routes (/org/identity, /canvas/viewport),
  # so the CP wildcard proxy resolves the tenant; empty on staging ⇒ the subdomain
  # routes ⇒ byte-identical. Tenant-auth probes present the tenant's real
  # CORS_ORIGINS ($TENANT_ORIGIN == $TENANT_URL on staging).
  args+=("${TENANT_ROUTE_HDRS[@]}")
  if [ "$auth" = "tenant" ]; then
    args+=(-H "Authorization: Bearer $TENANT_TOKEN" -H "X-Molecule-Org-Id: $ORG_ID" -H "Origin: $TENANT_ORIGIN")
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
}

canvas_probe "GET /workspaces" GET /workspaces tenant array
ok "CANVAS WORKS: GET /workspaces → 200 + real JSON list"
canvas_probe "GET /org/identity" GET /org/identity open json
ok "CANVAS WORKS: GET /org/identity → 200 + JSON"
canvas_probe "GET /canvas/viewport" GET /canvas/viewport open nothtml
ok "CANVAS WORKS: GET /canvas/viewport → 200 (not 404/502/SPA-HTML)"
canvas_probe "GET /requests/pending" GET /requests/pending tenant nothtml
ok "CANVAS WORKS: GET /requests/pending → 200 (not 404/502/SPA-HTML)"
ok "═ STEP 2 PASS: tenant canvas/app + proxied API resolve (the controlplane#1012 break is gated)"
obs_step_end canvas pass "" "routes=workspaces,org_identity,canvas_viewport,requests_pending"

# ─── 2.5 COLD-OPEN (browser-mechanism) — the in-browser canvas slug-header path ─
# STEP 2 above proves the proxied API resolves under the ADMIN header shape
# (X-Molecule-Org-Id). It does NOT exercise what the REAL browser sends on a cold
# open: the canvas derives the org slug from window.location.hostname and attaches
# it as X-Molecule-Org-Slug on every authed fetch (canvas/src/lib/api.ts
# platformAuthHeaders + canvas/src/lib/tenant.ts getTenantSlug). On staging's
# 2-level host <slug>.staging.moleculesai.app the OLD suffix-strip derivation
# produced "<slug>.staging" (it stripped the default .moleculesai.app suffix).
# The PRE-fix CP resolveOrgSlug honored that bogus header verbatim OVER the
# trusted Host → LookupOrgBySlug 404 → the operator's in-browser /workspaces 404
# that the admin-bearer STEP 2 never reproduces. TWO fixes closed it: the canvas
# now derives the first DNS label (#3408), AND the CP made the trusted Host
# AUTHORITATIVE over the client X-Molecule-Org-Slug header (controlplane
# a0ca9507 — unit-pinned in internal/router/resolve_test.go "stale mismatched
# header, valid host resolves via host"). This cold-open step replays the
# BROWSER's exact header on a cold open of the fresh org and asserts:
#   (a) the FIXED first-label slug ("<slug>") → GET /workspaces 200 + JSON array
#       (the list a cold open must render) AND /workspaces/<id>/chat-history 200;
#   (b) the OLD suffix-strip slug ("<slug>.staging", when the host is multi-label)
#       → STILL 200 — the trusted Host resolves the org and the stale/bogus header
#       is IGNORED (a valid tenant is never 404'd by a bad client header). This is
#       a REGRESSION LOCK on the CP host-first repair: if the CP ever regressed to
#       honoring the header over the Host, this bogus slug would 404 again and the
#       operator's cold-open /workspaces 404 would return.
# The tenant bearer + org-id are kept for cross-proxy-mode robustness (the CP
# resolves the org from the trusted Host BEFORE the tenant is reached, so the
# slug header is the load-bearing variable under test — it must NOT flip the
# outcome); Origin mirrors a same-origin canvas XHR. Ref: canvas/src/lib/tenant.ts
# first-label derivation (core#2509 class) + controlplane router resolveOrg
# host-first precedence (a0ca9507).
obs_step_start cold_open
log "2.5/6 COLD-OPEN — replaying the browser's X-Molecule-Org-Slug on a cold open of the fresh org..."
# Cold-open browser-slug derivation. On staging the slug lives in the tenant
# subdomain host ($SLUG.staging.moleculesai.app), so derive both the FIXED
# first-label slug and the OLD suffix-strip form from it. Under ephemeral slug-
# routing the CP base URL carries NO slug — the tenant is addressed by the ROUTE
# host ($SLUG.$ROUTE_DOMAIN via TENANT_ROUTE_HOST) — so the browser slug IS the
# routing slug and we send the route Host header so the wildcard proxy resolves
# the tenant. There is only ONE routing slug (no distinct 2-level-subdomain
# suffix-strip form), so CO_SLUG_OLD==CO_SLUG_FIRSTLABEL and the regression-lock
# arm (b) elides: the 2-level-subdomain host-first precedence is a staging-DNS
# property covered by the controlplane resolveOrg host-first unit
# (internal/router/resolve_test.go) + the canvas first-label unit (#3408), not by
# this loopback single-container CP.
if [ ${#TENANT_ROUTE_HDRS[@]} -gt 0 ]; then
  CO_HOST="$TENANT_ROUTE_HOST"
  CO_ROUTE_HOST_HDR=(-H "Host: $TENANT_ROUTE_HOST")
  CO_SLUG_FIRSTLABEL="$SLUG"
  CO_SLUG_OLD="$SLUG"
else
  CO_HOST="${TENANT_URL#*://}"; CO_HOST="${CO_HOST%%/*}"
  CO_ROUTE_HOST_HDR=()
  CO_SLUG_FIRSTLABEL="${CO_HOST%%.*}"        # canvas getTenantSlug() FIXED: leftmost DNS label
  CO_SLUG_OLD="${CO_HOST%.moleculesai.app}"  # OLD suffix-strip of the default .moleculesai.app
fi
CO_BODY_TMP="$TMPDIR_E2E/cold_open_body"
# cold_open_call <slug-header> <path> -> echoes the HTTP code; writes body to $CO_BODY_TMP.
cold_open_call() {
  local slughdr="$1" path="$2" code
  # CO_ROUTE_HOST_HDR carries the ephemeral route Host (=$SLUG.$ROUTE_DOMAIN) so the
  # CP wildcard proxy resolves the tenant; empty on staging (the subdomain routes).
  # X-Molecule-Org-Slug is the VARIABLE UNDER TEST (the browser-derived slug), kept
  # LAST so it is what the CP sees as the client header. Origin = the tenant's real
  # CORS_ORIGINS ($TENANT_ORIGIN == $TENANT_URL on staging).
  set +e
  code=$(curl -sS --max-time 25 -o "$CO_BODY_TMP" -w '%{http_code}' \
    "${CO_ROUTE_HOST_HDR[@]}" \
    -H "Authorization: Bearer $TENANT_TOKEN" -H "X-Molecule-Org-Id: $ORG_ID" \
    -H "X-Molecule-Org-Slug: $slughdr" -H "Origin: $TENANT_ORIGIN" \
    "$TENANT_URL$path" 2>/dev/null)
  set -e
  printf '%s' "${code:-000}"
}
# (a) FIXED first-label slug: the cold-open /workspaces list MUST resolve to this
#     org and return a real, NON-EMPTY JSON array (the concierge platform root the
#     canvas renders on open). Poll until the concierge lands (installed at
#     provision; STEP 3 gives it a longer settle) so step (c) has a workspace id.
co_deadline=$(( $(date +%s) + 120 )); CO_WS_JSON=""; co_code=000; co_trim=""
while :; do
  co_code=$(cold_open_call "$CO_SLUG_FIRSTLABEL" /workspaces)
  CO_WS_JSON=$(cat "$CO_BODY_TMP" 2>/dev/null || echo "")
  co_trim=$(printf '%s' "$CO_WS_JSON" | tr -d '[:space:]')
  { [ "$co_code" = "200" ] && [ "${co_trim:0:1}" = "[" ] && [ "$co_trim" != "[]" ]; } && break
  [ "$(date +%s)" -gt "$co_deadline" ] && fail "COLD-OPEN: GET /workspaces with the canvas first-label slug '$CO_SLUG_FIRSTLABEL' never returned 200 + a NON-EMPTY JSON array within 120s (last http=$co_code body: $(printf '%s' "$CO_WS_JSON" | head -c 160)) — the fresh org's cold-open workspace list does not render"
  sleep 6
done
ok "COLD-OPEN: GET /workspaces (browser slug '$CO_SLUG_FIRSTLABEL') → 200 + non-empty JSON array (list renders)"
# (b) REGRESSION LOCK on the CP host-first repair (controlplane a0ca9507): on a
#     multi-label host the OLD suffix-strip derivation ("<slug>.staging") differs
#     from the first label. Sending it as X-Molecule-Org-Slug MUST still resolve
#     200 — the trusted Host is authoritative and the stale/bogus header is
#     ignored, so a valid tenant is NEVER 404'd by a bad client header. A 404 here
#     means the CP regressed to header-over-Host and the operator's cold-open
#     /workspaces 404 has returned. Skipped on a single-label host (no distinct
#     suffix-strip derivation to exercise).
if [ "$CO_SLUG_OLD" != "$CO_SLUG_FIRSTLABEL" ]; then
  co_bad=$(cold_open_call "$CO_SLUG_OLD" /workspaces)
  [ "$co_bad" = "200" ] || fail "COLD-OPEN regression-lock: GET /workspaces with the OLD suffix-strip slug '$CO_SLUG_OLD' returned HTTP $co_bad, expected 200. The CP must resolve the org from the trusted Host and IGNORE a stale/bogus X-Molecule-Org-Slug header (controlplane a0ca9507 host-first precedence); a non-200 here means the CP regressed to header-over-Host and the 2-level-subdomain in-browser /workspaces 404 has returned."
  ok "COLD-OPEN regression-lock: OLD suffix-strip slug '$CO_SLUG_OLD' → 200 (trusted Host authoritative; bogus header ignored; 2-level-subdomain 404 stays dead)"
else
  log "    (single-label host — no distinct suffix-strip derivation to regression-lock)"
fi
# (c) chat-history for the first listed workspace (the cold-open chat panel) → 200.
CO_WID=$(printf '%s' "$CO_WS_JSON" | python3 -c "import json,sys
try: a=json.load(sys.stdin)
except Exception: a=[]
print(a[0].get('id','') if isinstance(a,list) and a and isinstance(a[0],dict) else '')")
[ -n "$CO_WID" ] || fail "COLD-OPEN: /workspaces returned an EMPTY list on cold open — no workspace to render (expected the concierge platform root at provision)"
co_ch=$(cold_open_call "$CO_SLUG_FIRSTLABEL" "/workspaces/$CO_WID/chat-history")
[ "$co_ch" = "200" ] || fail "COLD-OPEN: GET /workspaces/$CO_WID/chat-history with browser slug '$CO_SLUG_FIRSTLABEL' returned HTTP $co_ch, expected 200 — the cold-open chat panel would not load"
ok "COLD-OPEN: GET /workspaces/$CO_WID/chat-history (browser slug) → 200"
ok "═ STEP 2.5 PASS: cold-open browser-slug path resolves (the in-browser /workspaces 404 is gated; 2-level-subdomain regression locked)"
obs_step_end cold_open pass "" "first_label_slug=$CO_SLUG_FIRSTLABEL old_slug=$CO_SLUG_OLD"

# ─── 3. PLATFORM AGENT APPEARS — discover the concierge (kind='platform' root) ─
obs_step_start concierge_online
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
obs_step_end concierge_online pass "" "concierge_id=$CONCIERGE_ID"

# ─── 4.5. loaded_mcp_tools inventory check (core#3082) — HARD GATE (never-skip) ─
obs_step_start mcp_tools
# The concierge runtime reports `loaded_mcp_tools` on its heartbeat with the
# platform management `${PLATFORM_MCP_REQUIRED_TOOL}` tool in it. That field is
# produced by the runtime#181 heartbeat producer, which LANDED 2026-06-25
# ("fix(core#3082): wire loaded_mcp_tools producer at init"). A healthy staging
# concierge reports ~46 loaded_mcp_tools INCLUDING $(required_provision_tool_id)
# (RCA 2026-07-11). The tools load at init INDEPENDENTLY of the separate #64
# model-billing bug (they are present regardless of the LLM model), so this check
# is safe to enforce now and does not depend on the #64 fix.
#
# NEVER-SKIP (owner directive: "loaded_mcp_tools should never be skipped" on a
# prod-deploy hard gate). We POLL for the field to populate (a cold concierge can
# publish its heartbeat a few seconds after it flips online, so we do not
# single-shot read + false-fail), then HARD FAIL if loaded_mcp_tools is still
# absent/empty OR does not contain the required provision tool. A concierge that
# boots with no management-MCP tools can never pass this gate.
#
# This is defense-in-depth ON TOP OF — not a replacement for — the created-agent
# AUTH + real-first-turn gates below (STEP 4→5), which exercise the LLM USAGE
# TOKEN, a DIFFERENT concern. Both must hold. (Historical note: this check briefly
# skip_loud/continued while runtime#181 was pending; that decoupling was to avoid
# an EXIT before STEP 4→5 ran. Now that the producer has landed we POLL-then-FAIL
# in place, so both the MCP-inventory gate AND the auth gate run and both hold.)
MCP_TOOLS_SECS="${E2E_MCP_TOOLS_SECS:-240}"
log "3.5/6 PLATFORM AGENT APPEARS — loaded_mcp_tools inventory HARD GATE (poll ≤${MCP_TOOLS_SECS}s for $(required_provision_tool_id); runtime#181 producer landed 2026-06-25)..."

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

# Poll until the required tool is present, or the budget is exhausted. We keep the
# last non-empty inventory read so a HARD FAIL can report what WAS loaded.
mcp_deadline=$(( $(date +%s) + MCP_TOOLS_SECS ))
LOADED_TOOLS=""
mcp_ok=0
while :; do
  CUR_TOOLS=$(concierge_loaded_mcp_tools_json)
  [ -n "$CUR_TOOLS" ] && [ "$CUR_TOOLS" != "[]" ] && LOADED_TOOLS="$CUR_TOOLS"
  if [ -n "$CUR_TOOLS" ] && [ "$CUR_TOOLS" != "[]" ] \
     && [ "$(loaded_mcp_tools_has_required "$CUR_TOOLS")" = "yes" ]; then
    mcp_ok=1
    break
  fi
  [ "$(date +%s)" -ge "$mcp_deadline" ] && break
  sleep 8
done

if [ "$mcp_ok" = "1" ]; then
  ok "loaded_mcp_tools inventory confirms $(required_provision_tool_id)"
elif [ -n "$LOADED_TOOLS" ] && [ "$LOADED_TOOLS" != "[]" ]; then
  # Field populated but the required tool never appeared within budget: the
  # concierge booted with a management-MCP that is missing provision_workspace.
  fail "loaded_mcp_tools present but $(required_provision_tool_id) not reported within ${MCP_TOOLS_SECS}s — the concierge's management MCP did not expose the provision tool. HARD GATE (never-skip): runtime#181 producer landed 2026-06-25; a healthy concierge reports ~46 tools incl. this one. Tools: $(echo "$LOADED_TOOLS" | head -c 400)"
else
  # Field absent/empty for the whole budget: the mgmt-MCP tools failed to load at
  # init (broken concierge boot). runtime#181 producer landed 2026-06-25, so an
  # empty inventory is a real failure, not a not-yet-landed producer.
  fail "loaded_mcp_tools absent/empty after ${MCP_TOOLS_SECS}s — the concierge reported NO management-MCP tools. HARD GATE (never-skip): runtime#181 producer landed 2026-06-25 and a healthy concierge reports ~46 tools incl. $(required_provision_tool_id); an empty inventory means the mgmt-MCP failed to load at init."
fi

# Pre-state: the worker MUST NOT exist yet (so its later appearance is causally
# the concierge's doing, not a pre-existing row).
PRE_EXISTING=$(find_worker_by_name)
[ -n "$PRE_EXISTING" ] && fail "worker '$WORKER_NAME' already exists pre-test ($PRE_EXISTING) — name collision, cannot prove causality"
ok "Pre-state confirmed: '$WORKER_NAME' does not exist yet"
obs_step_end mcp_tools pass "" "required_tool=$(required_provision_tool_id)"

# ─── 5. Drive the AGENT: A2A message/send → it must create the workspace ──────
obs_step_start create_team_a2a
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
    A2A_QID=$(printf '%s' "$A2A_RESP" | a2a_queue_id_from_response || echo "")
    if [ -n "$A2A_QID" ]; then
      log "    create-team A2A queued (queue_id=$A2A_QID); polling durable result"
      poll_a2a_queue_result "$CONCIERGE_ID" "$A2A_QID" "$A2A_TMP" "create-team A2A"
      A2A_RESP=$(cat "$A2A_TMP" 2>/dev/null || echo "")
    fi
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
AGENT_TEXT=$(printf '%s' "$A2A_RESP" | extract_a2a_text 2>/dev/null || echo "")
log "    concierge replied (first 300 chars): $(echo "$AGENT_TEXT" | head -c 300)"
if [ -z "$AGENT_TEXT" ]; then
  log "    concierge A2A text extraction empty; response body: $(safe_body_preview "$A2A_RESP" 300)"
fi
obs_step_end create_team_a2a pass "" "http_code=$A2A_CODE" "attempts=$A2A_ATTEMPT"

# ─── 6. ASSERT the deterministic side effect: the worker now EXISTS ───────────
obs_step_start create_team_verify
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
obs_step_end create_team_verify pass "" "worker_id=$WORKER_ID" "worker_kind=${WORKER_KIND:-workspace}"

# ─── 4.7. CONCIERGE MANAGES A CHILD'S SCHEDULES (org key, CROSS-workspace) ─────
# The capability the management-mode schedule tools deliver (mcp-server#111): the
# concierge / org API key can CREATE, LIST and DELETE schedules on ANOTHER
# workspace. Those verbs POST/GET/DELETE /workspaces/<TARGET>/schedules under the
# ORG key — exactly what tenant_call does here: TENANT_TOKEN is the org-scoped
# bearer, which core's WorkspaceAuth org-token branch admits for EVERY workspace in
# the org. We target the CHILD ($WORKER_ID), NOT the caller — so this proves the
# cross-workspace grant end-to-end against the real tenant core, deterministically.
# (The LLM tool-invocation + tool-registration surface is unit-gated in mcp-server's
# management.test.ts; this gate covers the load-bearing core route + org-key auth.)
obs_step_start concierge_manages_schedule
log "4.7/6 CONCIERGE MANAGES SCHEDULES — org key creates a schedule on CHILD $WORKER_ID (cross-workspace)..."
_SCHED_NAME="e2e-cncrg-sched-$(printf '%s' "$WORKER_ID" | tr -cd 'a-f0-9' | cut -c1-8)"
_SCHED_TMP="$TMPDIR_E2E/cncrg_sched_out"
_SCHED_BODY_JSON=$(SCHED_NAME="$_SCHED_NAME" python3 -c 'import json,os; print(json.dumps({"name":os.environ["SCHED_NAME"],"cron_expr":"0 9 * * *","prompt":"e2e cross-workspace schedule (concierge/org key)","enabled":True}))')
# CREATE — bounded retry only on a 5xx daemon-arm race (the auth/route itself is
# deterministic; a 401/403 is a HARD, non-retryable capability regression).
_SCHED_CODE=000
for _CS_ATTEMPT in $(seq 1 6); do
  : >"$_SCHED_TMP"
  _SCHED_CODE=$(tenant_call POST "/workspaces/$WORKER_ID/schedules" \
    -H "Content-Type: application/json" -d "$_SCHED_BODY_JSON" \
    -o "$_SCHED_TMP" -w '%{http_code}' 2>/dev/null || echo 000)
  _SCHED_RESP=$(cat "$_SCHED_TMP" 2>/dev/null || echo "")
  echo "$_SCHED_CODE" | grep -Eq '^2..$' && break
  if echo "$_SCHED_CODE" | grep -Eq '^(401|403)$'; then
    fail "CONCIERGE CROSS-WORKSPACE SCHEDULE DENIED: org-key POST /workspaces/$WORKER_ID/schedules → http=$_SCHED_CODE. \
The org/tenant key is NOT admitted for another workspace's schedule route — the load-bearing dependency of the management-mode schedule tools (mcp-server#111) is broken (WorkspaceAuth org-token branch). Body: $(echo "$_SCHED_RESP" | head -c 300)"
  fi
  log "    create attempt $_CS_ATTEMPT/6 → http=$_SCHED_CODE (daemon-arm/forward race); retrying"; sleep 10
done
if ! echo "$_SCHED_CODE" | grep -Eq '^2..$'; then
  fail "CONCIERGE SCHEDULE CREATE FAILED: org-key POST /workspaces/$WORKER_ID/schedules → http=$_SCHED_CODE (want 2xx) after retries. Body: $(echo "$_SCHED_RESP" | head -c 300)"
fi
_SCHED_ID=$(printf '%s' "$_SCHED_RESP" | python3 -c "import sys,json
try: d=json.load(sys.stdin)
except Exception: d={}
print((d.get('id') if isinstance(d,dict) else '') or ((d.get('schedule') or {}).get('id') if isinstance(d,dict) else '') or '')" 2>/dev/null || echo "")
ok "org key CREATED a schedule on CHILD $WORKER_ID (http=$_SCHED_CODE, id=${_SCHED_ID:-?})"
# LIST — the created schedule must be visible to the org key on the child.
_LIST_RESP=$(tenant_call GET "/workspaces/$WORKER_ID/schedules" 2>/dev/null || echo "")
if ! printf '%s' "$_LIST_RESP" | grep -q "$_SCHED_NAME"; then
  fail "CONCIERGE SCHEDULE LIST MISSING: org-key GET /workspaces/$WORKER_ID/schedules did not contain '$_SCHED_NAME'. Body: $(echo "$_LIST_RESP" | head -c 300)"
fi
ok "org key LISTED child $WORKER_ID schedules and saw '$_SCHED_NAME' (cross-workspace read OK)"
# DELETE — prove the delete verb + clean up (non-fatal on cleanup miss).
if [ -n "$_SCHED_ID" ]; then
  _DEL_CODE=$(tenant_call DELETE "/workspaces/$WORKER_ID/schedules/$_SCHED_ID" -o /dev/null -w '%{http_code}' 2>/dev/null || echo 000)
  echo "$_DEL_CODE" | grep -Eq '^2..$' && ok "org key DELETED the schedule on child (http=$_DEL_CODE)" || log "    delete returned $_DEL_CODE (non-fatal cleanup)"
fi
ok "═ STEP 4.7 PASS: concierge/org-key CROSS-workspace schedule management works end-to-end"
obs_step_end concierge_manages_schedule pass "" "worker_id=$WORKER_ID" "sched_id=${_SCHED_ID:-none}"

# ─── 4.6. CREATED-AGENT LLM AUTH PRESENCE (corroborating, observability-tolerant)
# The concierge-created member must receive the platform LLM auth — the CP proxy
# usage token MOLECULE_LLM_USAGE_TOKEN — or it cannot complete a turn and replies
# "Agent error: Not logged in · Please run /login" (the exact bug this gate now
# guards). The token is injected into the member's container env at PROVISION
# time (it is NOT a tenant secret), so it surfaces only through an env-key
# PRESENCE signal on the workspace object (core env-presence observability,
# a06a52eb). This check is PRESENCE-ONLY — never the value, never docker-exec —
# and is INDEPENDENT of the runtime#181 MCP-tools producer:
#   present      → ok (auth propagated)
#   absent       → HARD fail (THIS is the bug; once a06a52eb exposes the field it
#                  reads false today and true once a57b73e9 propagates the token)
#   unobservable → ADVISORY (a06a52eb not landed) — defer to the REAL FIRST TURN
#                  gate in STEP 5, which is authoritative and needs no new field.
obs_step_start created_agent_auth_presence
log "4.6/6 CREATED-AGENT LLM AUTH — presence-check MOLECULE_LLM_USAGE_TOKEN on member $WORKER_ID (key presence only, value NEVER read)..."

# The primary env-presence probe (present|absent|unobservable) now lives in the
# shared lib/workspace_env_presence.sh as workspace_platform_llm_token_presence —
# ONE field contract used by both this test and full-SaaS 8c (hardened per
# code-review #4032; dormant until core a06a52eb, #4042). Called at the case below.

# Secondary surface: the secrets KEY list (has_value flag, never the value) — in
# case the fix lands the token as a workspace_secret rather than a pure env var.
# Absence here alone is NOT proof (the token is normally env-injected, not a
# secret), so a "no" maps back to unobservable; only a "yes" is a positive.
created_agent_token_key_in_secrets() {  # echoes yes|no|unobservable
  tenant_call GET "/workspaces/$WORKER_ID/secrets" 2>/dev/null | python3 -c '
import sys, json
KEY = "MOLECULE_LLM_USAGE_TOKEN"
try: rows = json.load(sys.stdin)
except Exception: print("unobservable"); sys.exit(0)
if not isinstance(rows, list): print("unobservable"); sys.exit(0)
print("yes" if any(isinstance(r, dict) and r.get("key") == KEY for r in rows) else "no")'
}

AUTH_PRESENCE=$(workspace_platform_llm_token_presence "$WORKER_ID")
if [ "$AUTH_PRESENCE" = "unobservable" ] && [ "$(created_agent_token_key_in_secrets)" = "yes" ]; then
  AUTH_PRESENCE=present
fi
case "$AUTH_PRESENCE" in
  present)
    ok "CREATED-AGENT LLM AUTH PRESENT: member $WORKER_ID carries MOLECULE_LLM_USAGE_TOKEN (presence-only — value never read)"
    obs_step_end created_agent_auth_presence pass "" "worker_id=$WORKER_ID" "presence=present" ;;
  absent)
    # fail() attributes the obs failure to this in-flight step (obs_fail_current).
    fail "CREATED-AGENT LLM AUTH MISSING: the concierge-created member $WORKER_ID has NO MOLECULE_LLM_USAGE_TOKEN — a created agent without the platform LLM usage token cannot authenticate and surfaces 'Not logged in' on its first turn. HARD gate (the LLM token, INDEPENDENT of the runtime#181 MCP-tools producer)." ;;
  *)
    log "    ⏭️  ADVISORY: MOLECULE_LLM_USAGE_TOKEN presence is not yet observable via the workspace API (core a06a52eb env-presence observability not landed). The REAL FIRST TURN gate in STEP 5 is authoritative and catches the same bug deterministically."
    obs_step_end created_agent_auth_presence pass "" "worker_id=$WORKER_ID" "presence=unobservable" ;;
esac

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

# ─── 5. ASSIGN TEAM WORK — the CREATED-AGENT AUTH + REAL-FIRST-TURN HARD GATE ──
# The workspace the concierge just created via the management MCP
# ${PLATFORM_MCP_REQUIRED_TOOL} verb IS the team member. "Assigning work" = sending it a
# real task over the A2A user→agent path and proving the member ACCEPTS it (2xx)
# AND runs a real LLM turn that reports back. NOT a PONG: a deterministic
# known-answer arithmetic task (17×23=391), so a broken member that echoes its
# error-as-text FAILs via a2a_assert_real_completion. MiniMax keeps it cheap
# (one short turn, minimal tokens).
#
# THIS IS THE REGRESSION GATE for the created-agent auth escape: a concierge-
# created workspace that never received the platform LLM usage token
# (MOLECULE_LLM_USAGE_TOKEN) cannot authenticate and answers its FIRST TURN with
# "Agent error: Not logged in · Please run /login". a2a_assert_real_completion
# classifies that as error-as-text (the "Not logged in" / "Please run /login"
# markers) AND it lacks the known answer → HARD fail. This is DEEP (a completed
# model turn / real output, NOT status==online or field-presence — the exact
# 'presence/no-op-text not callability' flaw that let the bug through) and is
# INDEPENDENT of the runtime#181 MCP-tools producer (it exercises the LLM token,
# not loaded_mcp_tools). It REDS on today's broken behavior and GREENS once the
# LLM-auth fix (token propagation to created workspaces) lands.
obs_step_start assign_work
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
WORK_CHAT_TEXT_TMP="$TMPDIR_E2E/work_chat_text"
WORK_ACCEPTED=0; WORK_OK=0; WORK_CODE=000; WORK_RC=1; WORK_TEXT=""; WORK_RESP=""
for WORK_ATTEMPT in $(seq 1 8); do
  : >"$WORK_TMP"
  set +e
  WORK_CODE=$(tenant_call POST "/workspaces/$WORKER_ID/a2a" \
    --max-time "$AGENT_ACT_SECS" -H "Content-Type: application/json" \
    -d "$WORK_PAYLOAD" -o "$WORK_TMP" -w '%{http_code}' 2>/dev/null)
  WORK_RC=$?
  set -e
  WORK_CODE=${WORK_CODE:-000}
  WORK_RESP=$(cat "$WORK_TMP" 2>/dev/null || echo "")
  if [ "$WORK_RC" = "0" ] && [ "$WORK_CODE" -ge 200 ] && [ "$WORK_CODE" -lt 300 ]; then
    WORK_ACCEPTED=1
    WORK_QID=$(printf '%s' "$WORK_RESP" | a2a_queue_id_from_response || echo "")
    if [ -n "$WORK_QID" ]; then
      log "    assign-work A2A queued (queue_id=$WORK_QID); polling durable result"
      poll_a2a_queue_result "$WORKER_ID" "$WORK_QID" "$WORK_TMP" "assign-work A2A"
      WORK_RESP=$(cat "$WORK_TMP" 2>/dev/null || echo "")
    fi

    WORK_PUSH_ASYNC=0
    if printf '%s' "$WORK_RESP" | is_push_async_queued_response; then
      WORK_PUSH_ASYNC=1
      log "    assign-work A2A returned push-async queued; polling chat-history for the agent reply"
      if poll_push_async_chat_history "$WORKER_ID" "$WORK_CHAT_TEXT_TMP" "assign-work" "$AGENT_ACT_SECS"; then
        WORK_TEXT=$(cat "$WORK_CHAT_TEXT_TMP" 2>/dev/null || echo "")
      else
        WORK_TEXT=""
      fi
    else
      WORK_TEXT=$(printf '%s' "$WORK_RESP" | extract_a2a_text 2>/dev/null || echo "")
    fi
    log "    team member replied (first 200 chars): $(echo "$WORK_TEXT" | head -c 200)"
    if hit=$(a2a_completion_error_marker "$WORK_TEXT"); then
      fail "assign-work — real-completion gate: agent returned an ERROR-AS-TEXT payload (matched '$hit'). Raw: ${WORK_TEXT:0:200}"
    fi
    if a2a_text_has_expected "$WORK_TEXT" "$WORK_EXPECT"; then
      WORK_OK=1
      break
    fi
    if [ "$WORK_ATTEMPT" -lt 8 ]; then
      if [ "$WORK_PUSH_ASYNC" = "1" ]; then
        break
      fi
      if [ -z "$WORK_TEXT" ]; then
        log "    assign-work attempt $WORK_ATTEMPT/8 returned no extracted text — retrying"
      else
        log "    assign-work attempt $WORK_ATTEMPT/8 missing expected token '$WORK_EXPECT' — retrying"
      fi
      sleep 15
      continue
    fi
    break
  fi
  if echo "$WORK_CODE" | grep -Eq '^(502|503|504)$'; then
    log "    assign-work A2A cold-start attempt $WORK_ATTEMPT/8 returned $WORK_CODE — retrying"
    [ "$WORK_ATTEMPT" -lt 8 ] && { sleep 15; continue; }
  fi
  break
done
[ "$WORK_ACCEPTED" = "1" ] || fail "ASSIGN TEAM WORK: A2A POST to team member $WORKER_ID failed (curl_rc=$WORK_RC, http=$WORK_CODE) after $WORK_ATTEMPT attempt(s): $(safe_body_preview "$WORK_RESP" 300)"
ok "Task ACCEPTED by the team member (A2A 2xx)"
if [ "$WORK_OK" != "1" ] && [ -z "$WORK_TEXT" ]; then
  log "    assign-work A2A text extraction empty; response body: $(safe_body_preview "$WORK_RESP" 300)"
fi
# Hard gate: a real (non-error-as-text) round-trip containing the known answer.
a2a_assert_real_completion "$WORK_TEXT" "$WORK_EXPECT" "assign-work"
ok "═ STEP 5 PASS: work assigned to the team member round-tripped a real MiniMax completion (got '$WORK_EXPECT')"
obs_step_end assign_work pass "" "http_code=$WORK_CODE" "expected=$WORK_EXPECT"

ok "═══ FULL-JOURNEY STAGING E2E PASSED (org → canvas → agent → team → work) ═══"
log "Proven end-to-end: (1) org created; (2) tenant canvas/app + proxied API resolve (/workspaces 200 real list, /org/identity, /canvas/viewport, /requests/pending — not 404/502/SPA-HTML); \
(3) platform agent auto-appeared online with ${PLATFORM_MCP_REQUIRED_TOOL} in loaded_mcp_tools; \
(4) a natural-language A2A request → the concierge's LLM invoked ${PLATFORM_MCP_REQUIRED_TOOL} via the platform MCP → real org mutation (team member '$WORKER_NAME' id=$WORKER_ID); \
(5) the team member accepted assigned work and round-tripped a real MiniMax completion. Teardown runs via EXIT trap."
