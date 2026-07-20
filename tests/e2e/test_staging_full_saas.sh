#!/usr/bin/env bash
# Full-lifecycle SaaS E2E against staging.
#
# Creates a fresh org per run (unique slug), waits for tenant provisioning,
# exercises every major workspace-level API
# (register, heartbeat, A2A, delegation, HMA memory, activity, peers),
# then tears the whole org down through the synchronous control-plane purge,
# validates its exact audit receipt, and proves the exact org is absent. The
# generic runner does not directly enumerate provider resources; current staging
# uses the molecules-server/local-Docker backend.
#
# Auth model:
#   Single MOLECULE_ADMIN_TOKEN (= staging CP_ADMIN_API_TOKEN from Infisical)
#   drives everything:
#     - POST /cp/admin/orgs to provision (no WorkOS session scraping)
#     - GET  /cp/admin/orgs/:slug/admin-token to retrieve the per-tenant
#       ADMIN_TOKEN once provisioning completes
#     - DELETE /cp/admin/tenants/:slug for teardown
#   The per-tenant admin token drives all tenant API calls (workspaces,
#   memories, a2a).
#
# Required env:
#   MOLECULE_CP_URL        default: https://staging-api.moleculesai.app
#   MOLECULE_ADMIN_TOKEN   staging CP admin bearer from Infisical
#
# Optional env:
#   E2E_RUNTIME                  hermes (default) | claude-code | codex | openclaw
#                                | seo-agent
#                                  - seo-agent: a claude-code-adapter template
#                                    VARIANT (not a distinct registry runtime).
#                                    Selected via the `template` field (config.yaml
#                                    resolves runtime=claude-code); reuses the
#                                    same MiniMax/claude-code key path. See the
#                                    TEMPLATE derivation + SECRETS_JSON block.
#   E2E_PROVISION_TIMEOUT_SECS   default 900 (15 min cold-provision budget)
#   E2E_WORKSPACE_ONLINE_TIMEOUT_SECS  default 3600 (60 min — hermes
#                                cold-boot worst-case + slack). Raised from
#                                1800 (#1646) because flaky tenant-provisioning
#                                latency (not a code regression) causes
#                                alternating pass/fail on identical SHAs.
#   E2E_KEEP_ORG                 1 → skip teardown (debugging only)
#   E2E_RUN_ID                   Slug suffix; CI: ${GITHUB_RUN_ID}
#   E2E_MODE                     full (default) | smoke
#                                (legacy alias `canary` still accepted —
#                                 mapped to `smoke` for back-compat with
#                                 any in-flight runner picking up an older
#                                 workflow checkout)
#   E2E_INFRA_BACKEND            required: local-docker (only active staging
#                                backend). Any other or unset value fails before
#                                an admin bearer is sent.
#   E2E_INTENTIONAL_FAILURE      1 → poison tenant token mid-run so the
#                                script fails; the EXIT trap MUST still
#                                tear down cleanly (and exit 4 on leak).
#                                Used by a dedicated sanity workflow
#                                that verifies the safety net.
#   E2E_LIFECYCLE                auto (default) | off
#                                When auto + MODE=full, exercises the
#                                pause→resume→online and hibernate→resume(wake)
#                                state transitions on the provisioned parent
#                                (step 10b). These are REAL transitions on the
#                                live tenant (Pause stops the container + sets
#                                status=paused; Resume re-provisions →
#                                provisioning → online; Hibernate stops +
#                                status=hibernated; the next A2A auto-wakes it).
#                                Set `off` for a fast smoke that skips the
#                                ~2x-reprovision cost. In smoke MODE it is
#                                skipped regardless (no parent stability budget).
#   E2E_REQUIRE_LIVE             1 → fail-closed-on-skip guard (CI sets this).
#                                When set, the run MUST actually complete
#                                ≥1 full provision→online→A2A cycle. A run
#                                that reaches the end without having proven
#                                a real round-trip (e.g. a future refactor
#                                short-circuits a stage, or a skip path
#                                swallows the lifecycle) exits 5 rather than
#                                reporting a false green. Mirrors CP
#                                serving-e2e's SERVING_E2E_REQUIRE_LIVE.
#   E2E_IDLE_DIGEST_TIMEOUT_SECS default 360. When the ephemeral lane enables
#                                the idle-digest assertion, poll durable fire
#                                evidence for this long. This must cover both
#                                the idle threshold and the runtime's real A2A/
#                                LLM completion latency; it is not a cadence.
#
# Exit codes:
#   0  happy path
#   1  generic failure
#   2  missing required env
#   3  provisioning timed out
#   4  teardown receipt/audit/org-absence proof failed
#   5  E2E_REQUIRE_LIVE set but the run validated no real lifecycle (no
#      false-green-on-skip)
#
# ─────────────────────────────────────────────────────────────────────────
# CURRENT EXECUTION / ENFORCEMENT:
#   • e2e-staging-saas.yml runs this harness live on protected main pushes and
#     workflow dispatches; pull requests run a credential-free syntax arm.
#   • The shared-staging contexts are parked below required-contexts.txt's
#     pending marker, so they are not presence-required by the merge queue. An
#     emitted red can still block through main's branch-protection wildcard.
#   • Staging-CD and production redeploy have independent triggers and their
#     own gate chains; this harness is post-merge/on-demand evidence, not an
#     ordering dependency for either deployment path.
#   • The current backend is molecules-server/local Docker. Teardown evidence is
#     the exact synchronous CP purge receipt plus exact-org absence; direct
#     provider enumeration is explicitly outside this generic runner.
#
# Fail-closed properties retained here: bounded readiness polls, asserted peers
# and activity, child provenance, and E2E_REQUIRE_LIVE=1 requiring a real
# provision→online→A2A lifecycle before a live run can report green.
# ─────────────────────────────────────────────────────────────────────────

set -euo pipefail

CP_URL="${MOLECULE_CP_URL:-https://staging-api.moleculesai.app}"
# #48: tolerate an absent admin token here — the PR-mode early-exit below
# (E2E_REQUIRE_LIVE=0 + no token) handles the pull_request lane cleanly. On a
# real run (push/dispatch, E2E_REQUIRE_LIVE=1) the missing-token case is
# caught as a HARD FAIL just past the PR-mode block, with a clear message.
ADMIN_TOKEN="${MOLECULE_ADMIN_TOKEN:-}"
RUNTIME="${E2E_RUNTIME:-hermes}"
PROVISION_TIMEOUT_SECS="${E2E_PROVISION_TIMEOUT_SECS:-900}"
WORKSPACE_ONLINE_TIMEOUT_SECS="${E2E_WORKSPACE_ONLINE_TIMEOUT_SECS:-3600}"
# RUN_ID_SUFFIX removed (core#2782 follow-up shellcheck): the slug now comes
# from make_collision_proof_slug below; the old suffix var is dead.
MODE="${E2E_MODE:-full}"
# `canary` is a legacy alias for `smoke` retained for back-compat with
# any in-flight runner picking up an older workflow checkout during the
# 2026-05-11 canary→staging rename rollout. Both map to the same slug
# prefix below. Remove the `canary` alias after one week of no-old-mode
# observations.
if [ "$MODE" = "canary" ]; then
  MODE="smoke"
fi
case "$MODE" in
  full|smoke) ;;
  *) echo "E2E_MODE must be 'full' or 'smoke' (got: $MODE)" >&2; exit 2 ;;
esac

# Collision-proof slug (core#2782). The prior `head -c 32` truncation
# dropped the run_attempt suffix and let two parallel/retry runs
# collide (POST /cp/admin/orgs 409). The helper appends a random
# 8-char uuid so every run gets a unique slug regardless of how
# the workflow composes E2E_RUN_ID. Asserted via the unit test
# tests/e2e/test_collision_proof_slug_unit.sh.
# Note: `source` + `assert_collision_proof_slug` happens AFTER
# log/fail/ok are defined below (the assert calls `fail` on
# mismatch). Avoid referencing `fail` before its definition.
# shellcheck source=lib/collision-proof-slug.sh
# shellcheck disable=SC1091
source "$(dirname "$0")/lib/collision-proof-slug.sh"
# shellcheck source=lib/idle_digest_wait.sh
# shellcheck disable=SC1091
source "$(dirname "$0")/lib/idle_digest_wait.sh"
# shellcheck source=lib/self_schedule_create_retry.sh
# shellcheck disable=SC1091
source "$(dirname "$0")/lib/self_schedule_create_retry.sh"

log()  { echo "[$(date +%H:%M:%S)] $*"; }
fail() { echo "[$(date +%H:%M:%S)] ❌ $*" >&2; exit 1; }
ok()   { echo "[$(date +%H:%M:%S)] ✅ $*"; }

# Collision-proof slug construction (core#2782) — runs AFTER log/fail/ok
# are defined so the assert below can call `fail` on mismatch.
# Self-check: fail loud at harness startup if a future refactor
# drops the uuid suffix (defense in depth — the unit test
# already covers this, but a redundant check in the harness
# itself is cheap).
if [ "$MODE" = "smoke" ]; then
  # core#60: pass the prefix length (11 for "e2e-smoke-") so the
  # helper's run_id budget is computed precisely against the CP's
  # 31-char org-slug cap. Without this, the helper uses a
  # conservative default and a future prefix change would silently
  # produce over-cap slugs.
  SLUG="e2e-smoke-$(make_collision_proof_slug_suffix "${E2E_RUN_ID:-}" 11)"
else
  # core#60: pass the prefix length (4 for "e2e-"). The non-smoke
  # path has the same 31-char CP cap, so the budget math is
  # identical — just the prefix literal is shorter.
  SLUG="e2e-$(make_collision_proof_slug_suffix "${E2E_RUN_ID:-}" 4)"
fi
assert_collision_proof_slug "$SLUG" || fail "Bug in make_collision_proof_slug: produced non-collision-proof slug '$SLUG' (assert_collision_proof_slug failed)"

# ─── fail-closed-on-skip live-lifecycle guard ───────────────────────────
# E2E_REQUIRE_LIVE=1 (set by CI) asserts this run ACTUALLY exercised a full
# provision→online→A2A cycle. Each load-bearing lifecycle stage stamps a
# milestone via live_milestone(); at the very end, require_live_or_die()
# checks every required milestone fired. Mechanism: without this, a future
# refactor that short-circuits a stage — or a skip/early-return path that
# swallows the lifecycle — would let the script reach its final `ok` and
# report GREEN having validated nothing. Mirrors CP serving-e2e's
# SERVING_E2E_REQUIRE_LIVE (skip-if-absent must be LOUD, never silent green).
REQUIRE_LIVE="${E2E_REQUIRE_LIVE:-0}"
LIVE_MILESTONES=""
live_milestone() {
  # Idempotent set-membership append. Space-delimited; names are tokens.
  case " $LIVE_MILESTONES " in
    *" $1 "*) ;;
    *) LIVE_MILESTONES="$LIVE_MILESTONES $1" ;;
  esac
}
require_live_or_die() {
  # No-op unless CI demanded a live run.
  [ "$REQUIRE_LIVE" = "1" ] || return 0

  # ── Canonical DECLARED happy-path milestone set (the SSOT) ──────────────────
  # This MUST equal molcontracts.HappyPathMilestones (9 ids, RFC #4428 Phase 2).
  # The core-side derive-gate (workspace-server/internal/e2emilestones,
  # TestHappyPathMilestonesMatchRunner) greps THIS line with the regex
  # requiredMilestonesRE and set-compares it (both directions) to the vendored
  # binding. Keep it a single-line quoted assignment, and when the SDK milestone
  # set changes, change the id list here — the gate will red otherwise.
  local required="provisioned tenant_online workspace_online a2a_roundtrip memory_online delegation_provenance cascade_guard lifecycle_pause_resume lifecycle_hibernate_wake"

  # ── Mode-aware ENFORCEMENT partition ────────────────────────────────────────
  # The DECLARED set is always all 9 (SSOT). But a milestone is only *required to
  # have fired* when the stage that proves it was supposed to run THIS mode.
  # Blindly enforcing all 9 would false-red smoke mode / E2E_LIFECYCLE=off, where
  # those stages legitimately never execute (see the mode gates at the memory,
  # delegation, and 10b lifecycle stages). This is the runner-side complement of
  # the derive-gate: the gate locks WHICH ids are declared; this locks WHICH of
  # them this mode must actually reach.
  #   always              : provisioned tenant_online workspace_online a2a_roundtrip
  #   MODE=full           : + memory_online (step 9)  + delegation_provenance (step 10)
  #   full & lifecycle!=off: + cascade_guard + lifecycle_pause_resume + lifecycle_hibernate_wake (step 10b)
  local enforced="provisioned tenant_online workspace_online a2a_roundtrip"
  if [ "$MODE" = "full" ]; then
    enforced="$enforced memory_online delegation_provenance"
    if [ "${E2E_LIFECYCLE:-auto}" != "off" ]; then
      enforced="$enforced cascade_guard lifecycle_pause_resume lifecycle_hibernate_wake"
    fi
  fi

  # Drift self-check: every enforced id MUST be a member of the declared set, so
  # enforcement can never check an id the SSOT doesn't declare (a typo would
  # otherwise silently never be verified). Fail loud on divergence.
  local e r found
  for e in $enforced; do
    found=0
    for r in $required; do [ "$e" = "$r" ] && { found=1; break; }; done
    [ "$found" = "1" ] || fail "require_live_or_die bug: enforced milestone '$e' is not in the declared required set — enforcement/SSOT drift. declared='$required' enforced='$enforced'"
  done

  local m missing=""
  for m in $enforced; do
    case " $LIVE_MILESTONES " in
      *" $m "*) ;;
      *) missing="$missing $m" ;;
    esac
  done
  if [ -n "$missing" ]; then
    echo "[$(date +%H:%M:%S)] ❌ E2E_REQUIRE_LIVE=1 but the run did NOT prove the milestones required for MODE=$MODE E2E_LIFECYCLE=${E2E_LIFECYCLE:-auto} — missing milestone(s):${missing}. Reached:${LIVE_MILESTONES:-<none>}. This is a false-green-on-skip guard: a run that skipped a stage it SHOULD have run MUST NOT report green." >&2
    exit 5
  fi
}

# ─── PR-mode early-exit (#48 — mirrors test_staging_concierge_creates_workspace_e2e.sh) ──
# This harness is invoked by two jobs in e2e-staging-saas.yml. Both emit a
# credential-free pull-request status and run live on push/dispatch with
# E2E_REQUIRE_LIVE=1. Their contexts are not presence-required while parked;
# an emitted red still blocks through main's branch-protection wildcard.
# E2E_REQUIRE_LIVE=0 on pull_request runs because PRs do not have staging creds
# wired; without this block the script would hard-fail at the first admin-auth
# call and red-X every PR (a false-red, not a real regression). The PR-mode gate
# is a self-check: bash -n on the script's own syntax (catches PR-merge
# regressions that would break the real run on push-to-main). On push / dispatch,
# E2E_REQUIRE_LIVE=1 and the real staging boot runs and HARD FAILs
# (exit 5 via require_live_or_die) on a run that validated no live lifecycle.
if [ "${REQUIRE_LIVE}" = "0" ] && [ -z "${ADMIN_TOKEN}" ]; then
  log "PR-mode: E2E_REQUIRE_LIVE=0 and no MOLECULE_ADMIN_TOKEN — skipping live staging boot."
  log "(the real staging boot runs on push-to-main / dispatch with E2E_REQUIRE_LIVE=1)"
  if ! bash -n "$0"; then
    fail "PR-mode self-check FAILED: bash -n on $0 returned non-zero — script has a syntax error"
  fi
  ok "PR-mode self-check PASSED: $(basename "$0") is bash-clean (real staging boot runs on push-to-main with E2E_REQUIRE_LIVE=1)"
  exit 0
fi
# Beyond here we are running for real: REQUIRE_LIVE=1 OR ADMIN_TOKEN is set.
# A real run with no admin token is a HARD FAIL (was the `:?` default before #48).
if [ -z "${ADMIN_TOKEN}" ]; then
  fail "MOLECULE_ADMIN_TOKEN required (staging CP_ADMIN_API_TOKEN from Infisical /shared/controlplane-admin) — a non-PR run (E2E_REQUIRE_LIVE=${REQUIRE_LIVE}) needs staging creds"
fi

# Per-runtime model slug dispatch — see lib/model_slug.sh for the rationale.
# Extracted so unit tests (tests/e2e/test_model_slug.sh) can pin every branch
# without booting the full 11-step lifecycle.
# shellcheck disable=SC1091
# shellcheck source=lib/model_slug.sh
source "$(dirname "$0")/lib/model_slug.sh"
# shellcheck disable=SC1091
# shellcheck source=lib/workspace_env_presence.sh
source "$(dirname "$0")/lib/workspace_env_presence.sh"
# shellcheck disable=SC1091
# shellcheck source=lib/cp_purge_receipt.sh
source "$(dirname "$0")/lib/cp_purge_receipt.sh"
# shellcheck source=lib/workspace_create_retry.sh
source "$(dirname "$0")/lib/workspace_create_retry.sh"
e2e_cp_require_local_backend || exit 2
e2e_cp_require_staging_origin "$CP_URL" || exit 2
# shellcheck disable=SC1091
# shellcheck source=lib/completion_assert.sh
# molecule-core#1995 (#1994 follow-on): real-completion + per-provider
# liveness + byok-routing assertion helpers. Adds gates that FAIL on an
# error-as-text payload (the trap the shape-only A2A checks missed).
source "$(dirname "$0")/lib/completion_assert.sh"
# shellcheck disable=SC1091
# shellcheck source=lib/http_status_capture.sh
source "$(dirname "$0")/lib/http_status_capture.sh"

CURL_COMMON=(-sS --fail-with-body --max-time 30)
E2E_TMP_FILES=()

# Infra-skip helper (core#2917). Emits a machine-readable scan_status line
# and exits 0 so the advisory staging gate goes green-with-skip rather than
# false-red on a known transient A2A-layer degradation. The trap still tears
# down the org.
#
# Fail-closed on repeated skips: a broadly broken agent that triggers skips on
# every A2A call would otherwise paint the advisory lane green while masking a
# real regression. We allow one distinct skip reason per run; a second distinct
# reason (or any repeated skip after the cap) converts to a hard failure.
INFRA_SKIP_REASONS=""
infra_skip() {
  local reason="$1"
  local detail="${2:-}"
  case " $INFRA_SKIP_REASONS " in
    *" $reason "*) ;;
    *) INFRA_SKIP_REASONS="$INFRA_SKIP_REASONS $reason" ;;
  esac
  local distinct_count
  distinct_count=$(echo "$INFRA_SKIP_REASONS" | wc -w | tr -d ' ')
  if [ "$distinct_count" -ge 2 ]; then
    fail "infra-skip cap exceeded ($distinct_count distinct reasons:${INFRA_SKIP_REASONS:-none}) — refusing false-green on repeated A2A-layer degradation"
  fi
  echo "[$(date +%H:%M:%S)] ⚠️  scan_status: infra-skip:${reason}${detail:+ $detail}"
  exit 0
}

e2e_tmp() {
  local f
  f=$(mktemp "$1")
  E2E_TMP_FILES+=("$f")
  printf '%s' "$f"
}

# ─── cleanup trap ───────────────────────────────────────────────────────
CLEANUP_DONE=0
ORG_ID=""
ORG_CREATION_VERIFIED=0
CREATE_BODYFILE=""
cleanup_org() {
  # Capture upstream exit code IMMEDIATELY — must be the first statement
  # in the trap, before any command (including the CLEANUP_DONE check)
  # that would clobber $?.
  local entry_rc=$?

  if [ "$CLEANUP_DONE" = "1" ]; then return 0; fi
  CLEANUP_DONE=1

  # ${arr[@]:-} — bash 3.2 (macOS) errors on EMPTY-array expansion under
  # `set -u` ("E2E_TMP_FILES[@]: unbound variable"), turning a fully-PASSED
  # run into rc=1 inside this trap (`|| true` does NOT save it: the shell
  # aborts on the expansion itself, before rm runs). bash 4.4+ (CI) is fine —
  # this is a local==CI portability guard.
  rm -f "${E2E_TMP_FILES[@]:-}" 2>/dev/null || true

  if [ "${E2E_KEEP_ORG:-0}" = "1" ]; then
    log "E2E_KEEP_ORG=1 — skipping teardown. Manually delete $SLUG when done."
    return 0
  fi

  if [ "$ORG_CREATION_VERIFIED" != "1" ]; then
    log "No verified creation-returned org identity for $SLUG — skipping destructive org teardown."
    case "$entry_rc" in 0|1|2|3|4|5) return 0 ;; *) exit 1 ;; esac
  fi

  log "🧹 Tearing down org $SLUG..."

  # DELETE returns only after executeOrgPurge has completed the selected
  # provider cleanup and DB-row step. Verify the exact receipt/audit identity,
  # then require the exact-tenant boot-events endpoint to return 404. This is
  # honest CP purge evidence, not a claim that this generic runner scanned
  # Docker itself.
  local purge_verify_rc=0
  e2e_cp_delete_and_verify_purge \
    "$CP_URL" "$ADMIN_TOKEN" "$SLUG" "$ORG_ID" || purge_verify_rc=$?
  case "$purge_verify_rc" in
    0) ;;
    2) exit 2 ;;
    *) exit 4 ;;
  esac

  # Normalize unexpected upstream exit codes to 1 (generic failure). The
  # script's documented contract (header "Exit codes" section) only emits
  # {0, 1, 2, 3, 4}, but `set -e` propagates the raw exit code of the
  # failing command — e.g. curl exits 22 on HTTP error under
  # --fail-with-body. Without this normalization, the
  # E2E_INTENTIONAL_FAILURE sanity workflow (e2e-staging-sanity.yml)
  # gets rc=22 from the poisoned-token curl, falls through its
  # case statement, and opens a false-positive priority-high
  # "safety net broken" issue (#2159, 2026-04-27).
  case "$entry_rc" in
    0|1|2|3|4|5) ;;        # contracted codes — let bash use entry_rc
    *) exit 1 ;;            # anything else is a generic failure
  esac
}

# Wrapper for the EXIT/INT/TERM trap: capture the original exit code,
# remove the org-create bodyfile (created later), run teardown, and
# propagate the original code. Defined as a function so the trap string
# is simple and cannot pick up an unbalanced quote from inline command
# substitution (core#2917).
cleanup_org_and_bodyfile() {
  local entry_rc=$?
  if [ -n "$CREATE_BODYFILE" ]; then
    rm -f "$CREATE_BODYFILE" 2>/dev/null || true
  fi
  cleanup_org
  exit "$entry_rc"
}
trap cleanup_org_and_bodyfile EXIT INT TERM

# ─── 0. Preflight ───────────────────────────────────────────────────────
log "═══════════════════════════════════════════════════════════════════"
log " Staging full-SaaS E2E"
log "   CP:      $CP_URL"
log "   Slug:    $SLUG"
log "   Runtime: $RUNTIME"
log "   Mode:    $MODE"
log "   Timeout: ${PROVISION_TIMEOUT_SECS}s"
[ "${E2E_INTENTIONAL_FAILURE:-0}" = "1" ] && log "   ⚠️  INTENTIONAL_FAILURE=1 — this run MUST fail mid-way; teardown MUST still clean up"
log "═══════════════════════════════════════════════════════════════════"

log "0/11 Preflight: CP reachable?"
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

# ─── 1. Create org via admin endpoint ───────────────────────────────────
log "1/11 Creating org $SLUG via /cp/admin/orgs..."
# core#60: capture status + body explicitly with curl -w '%{http_code}'
# -o bodyfile inside a set +e block (mirror the pattern at lines
# 875-889 for the workspace-create call), so a 400/409 body is
# ALWAYS logged for diagnosis instead of being swallowed by
# CURL_COMMON's --fail-with-body + set -e aborting the script
# before the body-logging line runs. The pre-fix code path
# (admin_call POST ... bare in a $(...)) would propagate curl's
# nonzero exit through the command substitution under
# set -euo pipefail, aborting the whole harness with no body
# in the CI logs.
CREATE_BODYFILE="$(mktemp -t create-org-resp.XXXXXX)"
# cleanup_org_and_bodyfile (EXIT/INT/TERM trap) removes this bodyfile and runs
# teardown only after a successful create returned a valid org identity. A
# non-2xx/invalid create response is logged but cannot authorize a slug-only
# delete that might target a pre-existing tenant.
set +e
CREATE_HTTP_CODE=$(curl "${CURL_COMMON[@]}" -X POST "$CP_URL/cp/admin/orgs" \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d "{\"slug\":\"$SLUG\",\"name\":\"E2E $SLUG\",\"owner_user_id\":\"e2e-runner:$SLUG\"}" \
  -o "$CREATE_BODYFILE" \
  -w '%{http_code}')
CURL_RC=$?
set -e
CREATE_RESP="$(cat "$CREATE_BODYFILE")"
# core#2782: log the full 409 response body on a collision so the
# stale-slug-vs-fresh-slug diagnostic is queryable from CI logs.
# Pre-#60 the JSON was piped to /dev/null (`python3 -m json.tool
# >/dev/null`) which silently swallowed the body — triage on the
# 2026-06-12 staging Platform Boot red had to guess whether the
# 409 was a slug collision or a different state-conflict. With
# the explicit -o bodyfile + -w '%{http_code}' above, the body
# is always on disk for logging regardless of HTTP status.
if [ "$CURL_RC" -ne 0 ] || [ "$CREATE_HTTP_CODE" -lt 200 ] || [ "$CREATE_HTTP_CODE" -ge 300 ]; then
  log "❌ Org create failed (curl_rc=$CURL_RC http=$CREATE_HTTP_CODE slug_len=${#SLUG}); raw response body:"
  log "--- BEGIN CREATE RESPONSE ---"
  log "$CREATE_RESP"
  log "--- END CREATE RESPONSE ---"
  if [ "${#SLUG}" -gt 32 ]; then
    fail "Org create returned non-2xx AND slug is ${#SLUG} chars (over the CP's 32-char cap). The slug helper's assertion should have caught this; check collision-proof-slug.sh's run_id_budget math."
  fi
  fail "Org create returned non-2xx (http=$CREATE_HTTP_CODE) — see body above. Common causes: 409=slug collision (a prior run left a stale org; the slug helper should prevent this — check E2E_RUN_ID propagation), 400=slug too long (should be caught by the 32-char cap assertion), 401=ADMIN_TOKEN not set or expired, 422=schema mismatch (check the -d payload matches the CP's expected shape)."
fi
if [ -z "$CREATE_RESP" ] || ! echo "$CREATE_RESP" | python3 -m json.tool >/dev/null 2>&1; then
  log "❌ Org create returned non-JSON; raw body: $CREATE_RESP"
  fail "Org create returned non-JSON (see body above)"
fi
# Capture org_id for tenant-guard header on every subsequent tenant call.
# Without X-Molecule-Org-Id matching MOLECULE_ORG_ID on the tenant, the
# tenant-guard middleware returns 404 to avoid leaking tenant existence.
ORG_ID=$(echo "$CREATE_RESP" | python3 -c "import json,sys; print(json.load(sys.stdin).get('id',''))")
if ! e2e_cp_validate_org_id "$ORG_ID"; then
  log "❌ Org create response missing 'id'; raw body: $CREATE_RESP"
  fail "Org create response missing a valid UUID 'id' (see body above)"
fi
ORG_CREATION_VERIFIED=1
e2e_cp_publish_creation_identity "$SLUG" "$ORG_ID" \
  || fail "Could not publish the verified org creation identity for teardown"
ok "Org created (id=$ORG_ID http=$CREATE_HTTP_CODE slug_len=${#SLUG})"

# ─── 2. Wait for tenant provisioning ────────────────────────────────────
log "2/11 Waiting for tenant provisioning (up to ${PROVISION_TIMEOUT_SECS}s)..."
DEADLINE=$(( $(date +%s) + PROVISION_TIMEOUT_SECS ))
LAST_STATUS=""
while true; do
  if [ "$(date +%s)" -gt "$DEADLINE" ]; then
    fail "Tenant provisioning timed out after ${PROVISION_TIMEOUT_SECS}s (last: $LAST_STATUS)"
  fi
  LIST_JSON=$(admin_call GET /cp/admin/orgs 2>/dev/null || echo '{"orgs":[]}')
  # NOTE: /cp/admin/orgs exposes 'instance_status' (from org_instances.status),
  # NOT 'status'. Field was bug-fixed 2026-04-21 after harness timed out on a
  # fully-provisioned tenant because the polled field was always ''. The
  # admin handler struct intentionally has no top-level `status` — the org
  # row's status is derivable via instance_status for ops.
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
      # Diagnostic burst: dump the org row so the operator sees
      # `last_error` (CP migration 022 / handler #289 — issue #285).
      # Pre-fix the harness only logged "Tenant provisioning failed",
      # forcing whoever debugs canary to scrape CP server logs to
      # learn WHY. Same shape as the TLS-readiness burst at step 4
      # (PR #2107). Redacts nothing because /cp/admin/orgs already
      # returns a narrow, ops-safe shape (id/slug/name/plan/
      # member_count/instance_status/last_error/timestamps —
      # no tokens, no encrypted fields).
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
      fail "Tenant provisioning failed for $SLUG (see diagnostic above for last_error)"
      ;;
    *)        sleep 15 ;;
  esac
done
ok "Tenant provisioning complete"
live_milestone provisioned

# Derive tenant domain from CP hostname so the same harness works in
# both prod (api.moleculesai.app → moleculesai.app) and staging
# (staging-api.moleculesai.app → staging.moleculesai.app). Override
# via MOLECULE_TENANT_DOMAIN for local/self-hosted.
CP_HOST=$(echo "$CP_URL" | sed -E 's#^https?://##; s#/.*$##')
case "$CP_HOST" in
  api.*)         DERIVED_DOMAIN="${CP_HOST#api.}" ;;
  staging-api.*) DERIVED_DOMAIN="staging.${CP_HOST#staging-api.}" ;;
  *)             DERIVED_DOMAIN="$CP_HOST" ;;
esac
TENANT_DOMAIN="${MOLECULE_TENANT_DOMAIN:-$DERIVED_DOMAIN}"
# MOLECULE_TENANT_URL override — the EPHEMERAL-CP path (RFC "one pre-merge gate"
# §04). Staging front-doors each tenant at its own subdomain (slug.<domain>) so the
# Host alone routes. An ephemeral CP has no per-tenant subdomain — it is one
# throwaway container whose wildcard proxy resolves the tenant by SLUG. So the
# ephemeral runner points this at the throwaway CP base URL; default (unset) keeps
# the exact staging subdomain behavior.
TENANT_URL="${MOLECULE_TENANT_URL:-https://$SLUG.$TENANT_DOMAIN}"
log "    TENANT_URL=$TENANT_URL"

# ── ephemeral-CP tenant ROUTING headers ──────────────────────────────────
# The CP wildcard proxy resolves the tenant by SLUG — resolveOrg() reads the
# Host-derived slug OR the X-Molecule-Org-Slug fallback (controlplane
# internal/router/router.go). X-Molecule-Org-Id is NOT a routing input (the CP
# INJECTS it toward the tenant). So when MOLECULE_TENANT_URL points at the CP base
# URL we carry the routing slug the SAME way the CP-side gate does
# (local-cp-staging-e2e-gate.sh: Host: <slug>.<suffix> + X-Molecule-Org-Slug: <slug>).
# The runner knows the app domain but not our per-run SLUG (minted above), so build
# the Host here from SLUG + MOLECULE_TENANT_ROUTE_DOMAIN. Default unset ⇒ no extra
# headers ⇒ exact staging behavior.
TENANT_ROUTE_HOST="${MOLECULE_TENANT_ROUTE_HOST:-}"
if [ -z "$TENANT_ROUTE_HOST" ] && [ -n "${MOLECULE_TENANT_ROUTE_DOMAIN:-}" ]; then
  TENANT_ROUTE_HOST="$SLUG.$MOLECULE_TENANT_ROUTE_DOMAIN"
fi
TENANT_ROUTE_HDRS=()
if [ -n "$TENANT_ROUTE_HOST" ]; then
  TENANT_ROUTE_HDRS=(-H "Host: $TENANT_ROUTE_HOST" -H "X-Molecule-Org-Slug: $SLUG")
  log "    tenant routing via Host=$TENANT_ROUTE_HOST + X-Molecule-Org-Slug=$SLUG (ephemeral-CP slug routing)"
fi

# ─── 3. Retrieve per-tenant admin token ────────────────────────────────
log "3/11 Fetching per-tenant admin token..."
TENANT_TOKEN_RESP=$(admin_call GET "/cp/admin/orgs/$SLUG/admin-token")
TENANT_TOKEN=$(echo "$TENANT_TOKEN_RESP" | python3 -c "import json,sys; print(json.load(sys.stdin).get('admin_token',''))" 2>/dev/null || echo "")
[ -z "$TENANT_TOKEN" ] && fail "Could not retrieve per-tenant admin token for $SLUG"
ok "Tenant admin token retrieved (len=${#TENANT_TOKEN})"

# ─── 4. Wait for tenant TLS / DNS propagation ──────────────────────────
# Kept below the 20-min provision envelope so a genuinely-stuck tenant
# still fails loud at the earlier provision step rather than masquerading
# as a TLS issue. CF DNS propagation + tunnel hostname registration +
# ACME cert + edge cache run 5-7 min on a healthy day; +5 min headroom
# over the previous 10-min cap covers the slower path observed in #2090.
#
# On timeout, dump DNS + curl -v + headers so the next failure identifies
# the broken layer (DNS / TLS / HTTP). Authorization is redacted
# defensively in case a future caller adds an auth header to this probe.
log "4/11 Waiting for tenant TLS / DNS propagation..."
TLS_TIMEOUT_SEC=$((15 * 60))
TLS_DEADLINE=$(( $(date +%s) + TLS_TIMEOUT_SEC ))
TENANT_HOST="${TENANT_URL#http*://}"
TENANT_HOST="${TENANT_HOST%%/*}"
TENANT_HOST="${TENANT_HOST%%:*}"
while true; do
  # When routing by slug-header (ephemeral CP), /health is answered by the CP's
  # OWN global handler for every Host, so it can NEVER prove the tenant is up —
  # that would stamp tenant_online without reaching the tenant (false-green).
  # Probe /org/identity (an open, tenant-owned handler the CP proxies through)
  # WITH the routing headers, mirroring local-cp-staging-e2e-gate.sh's tenant
  # readiness probe. Staging (empty TENANT_ROUTE_HDRS) keeps the /health check.
  if [ ${#TENANT_ROUTE_HDRS[@]} -gt 0 ]; then
    if curl -sSfk --max-time 5 "${TENANT_ROUTE_HDRS[@]}" -H "X-Molecule-Org-Id: $ORG_ID" "$TENANT_URL/org/identity" >/dev/null 2>&1; then
      break
    fi
  elif curl -sSfk --max-time 5 "$TENANT_URL/health" >/dev/null 2>&1; then
    break
  fi
  if [ "$(date +%s)" -gt "$TLS_DEADLINE" ]; then
    log "── DIAGNOSTIC BURST (TLS-readiness timeout) ──"
    log "DNS lookup ($TENANT_HOST):"
    getent hosts "$TENANT_HOST" 2>&1 || log "  (no DNS resolution)"
    log "curl -v $TENANT_URL/health (last 40 lines):"
    curl -kv --max-time 10 "$TENANT_URL/health" 2>&1 \
      | sed -E 's/(Authorization|Cookie):.*/\1: [redacted]/i' \
      | tail -n 40 | sed 's/^/  /' || true
    log "── END DIAGNOSTIC ──"
    fail "Tenant URL never responded 2xx on /health within ${TLS_TIMEOUT_SEC}s"
  fi
  sleep 5
done
ok "Tenant reachable at $TENANT_URL"
live_milestone tenant_online

# Sanity-test path: once the tenant is provisioned, poisoning the
# tenant token proves the EXIT trap + leak assertion still fire.
# Gate AFTER provisioning so the provision path itself stays valid.
EFFECTIVE_TENANT_TOKEN="$TENANT_TOKEN"
if [ "${E2E_INTENTIONAL_FAILURE:-0}" = "1" ]; then
  log "⚠️  INTENTIONAL_FAILURE: poisoning tenant token for the workspace-provision step"
  EFFECTIVE_TENANT_TOKEN="poisoned-$$"
fi

tenant_call() {
  local method="$1"; shift
  local path="$1"; shift
  # X-Molecule-Org-Id is REQUIRED — tenant guard 404s anything without
  # it (it does NOT 403, to hide tenant existence from org scanners).
  # TENANT_ROUTE_HDRS is empty in staging (Host-subdomain routes); on an
  # ephemeral CP it carries Host=<slug>.<domain> + X-Molecule-Org-Slug so the
  # CP wildcard proxy routes this to the tenant (X-Molecule-Org-Id is NOT a
  # routing input — it's the tenant-guard identity header).
  curl "${CURL_COMMON[@]}" -X "$method" "$TENANT_URL$path" \
    -H "Authorization: Bearer $EFFECTIVE_TENANT_TOKEN" \
    -H "X-Molecule-Org-Id: $ORG_ID" \
    "${TENANT_ROUTE_HDRS[@]}" \
    "$@"
}

sanitize_http_body() {
  python3 -c '
import re, sys
s = sys.stdin.read()
s = re.sub(r"(?i)(Authorization:\s*Bearer\s+)[A-Za-z0-9._~+/=-]+", r"\1[redacted]", s)
s = re.sub(r"(?i)(\"(?:auth_token|access_token|refresh_token|token|api_key|secret|password)\"\s*:\s*\")[^\"]+\"", r"\1[redacted]\"", s)
s = re.sub(r"(?i)((?:auth_token|access_token|refresh_token|api_key|secret|password)=)[^&\s]+", r"\1[redacted]", s)
print(s[:4000])
'
}

# create_workspace_post — `POST /workspaces` with a bounded retry over the
# cold-origin write window (RCA'd via core#4307's header capture: Cloudflare
# returns an empty-body 503 + Retry-After for ~1-2s after health passes but
# before the tenant origin accepts writes). Honors the origin's Retry-After.
# Retry policy lives in lib/workspace_create_retry.sh (create_should_retry_cold):
# retries ONLY an empty-body 503 or a connection reset (curl 000 / no status
# line) — the two "never reached a handler" signatures. A JSON body (real
# 422/400/… create error) and 502/504 (maybe-processed → non-idempotent) are
# surfaced on the FIRST try. A persistent transient exhausts the budget and
# returns the last body so the caller's id-check fails RED with the full header
# diagnostic. Every attempt is logged, so a transient that self-heals stays
# visible and is never silently masked.
#
# The attempt budget stays bounded but must cover the ACTUAL cold window: the
# core#4307 RCA estimated ~1-2s, but under concurrent-provision load on the
# shared staging docker host the tenant origin warms slower — run 546336
# (Platform Boot) saw a persistent empty-body 503 from Cloudflare across the old
# 4-attempt / ~8s budget. 8 attempts at Retry-After ~2s (~16s) covers the
# observed longer window. This does NOT slow the deterministic-failure path: a
# non-cold status (JSON app error, 502/504) is surfaced on attempt 1 by
# create_should_retry_cold, so ONLY a genuinely-persistent cold-origin transient
# consumes the budget — and a never-warming origin still fails RED in ~16s, well
# under a CI job timeout. Env-overridable for a still-longer window if needed.
#
# Args: $1 = human label, $2 = headers dump file (-D target). Remaining args are
# passed verbatim to `tenant_call POST /workspaces` (payload etc.). Echoes the
# final response body on stdout; leaves the final response headers in $2.
CREATE_MAX_ATTEMPTS="${CREATE_MAX_ATTEMPTS:-8}"
create_workspace_post() {
  local label="$1" hdrs="$2"; shift 2
  local attempt resp id status ra who
  for (( attempt = 1; attempt <= CREATE_MAX_ATTEMPTS; attempt++ )); do
    : > "$hdrs"
    # --fail-with-body + set -e would abort here on a non-2xx; the existing
    # create sites disarm set -e so curl still writes the body. Same guard.
    # A connection reset leaves $resp empty and writes no status line to $hdrs.
    set +e
    resp=$(tenant_call POST /workspaces -D "$hdrs" "$@")
    local curl_rc=$?   # curl's transport exit: 0=got a response; 28=timeout
                       # (maybe-processed), 7/56/…=connection reset/refused.
    set -e
    id=$(printf '%s' "$resp" | python3 -c "import json,sys; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
    if [ -n "$id" ]; then
      printf '%s' "$resp"
      return 0
    fi
    status=$(create_parse_status "$hdrs")
    # Pass curl_rc so a client TIMEOUT (28) is NOT retried (the origin may have
    # already processed this non-idempotent POST → re-POST would double-create),
    # while a genuine connection reset/refused still is.
    if ! create_should_retry_cold "$status" "$resp" "$curl_rc"; then
      # Real app error (non-empty body), a 502/504, or any non-cold status —
      # surface immediately so the caller's id-check names it. No retry, no
      # masking, and no re-POST of a possibly-already-processed create.
      printf '%s' "$resp"
      return 0
    fi
    ra=$(create_parse_retry_after "$hdrs")
    who=$(create_parse_server "$hdrs")
    if [ "$attempt" -lt "$CREATE_MAX_ATTEMPTS" ]; then
      echo "[$(date +%H:%M:%S)] ⏳ $label create attempt $attempt/$CREATE_MAX_ATTEMPTS: empty-body ${status:-conn-reset} from '${who:-?}' (cold-origin write window) — honoring Retry-After ${ra}s" >&2
      sleep "$ra"
    fi
  done
  # Budget exhausted on a persistent cold-origin transient — hand the last body
  # back so the caller fails RED with the header diagnostic. Not masked.
  printf '%s' "$resp"
  return 0
}

wait_workspaces_online_routable() {
  local label="$1"; shift
  local deadline=$(( $(date +%s) + WORKSPACE_ONLINE_TIMEOUT_SECS ))
  local wid ws_last_status ws_last_url ws_url_missing_logged ws_failed_logged
  local ws_json ws_status ws_url ws_last_err

  log "$label"
  for wid in "$@"; do
    ws_last_status=""
    ws_last_url=""
    ws_url_missing_logged=0
    ws_failed_logged=0
    while true; do
      if [ "$(date +%s)" -gt "$deadline" ]; then
        ws_last_err=$(tenant_call GET "/workspaces/$wid" 2>/dev/null | \
          python3 -c "import json,sys; print(json.load(sys.stdin).get('last_sample_error',''))" 2>/dev/null || echo "")
        fail "Workspace $wid never reached online with a routable URL within ${WORKSPACE_ONLINE_TIMEOUT_SECS}s (~$((WORKSPACE_ONLINE_TIMEOUT_SECS/60)) min) (last status=$ws_last_status, url=$ws_last_url, err=$ws_last_err)"
      fi
      ws_json=$(tenant_call GET "/workspaces/$wid" 2>/dev/null || echo '{}')
      ws_status=$(echo "$ws_json" | python3 -c "import json,sys; print(json.load(sys.stdin).get('status') or '')" 2>/dev/null)
      ws_url=$(echo "$ws_json" | python3 -c "import json,sys; print(json.load(sys.stdin).get('url') or '')" 2>/dev/null)
      if [ "$ws_status" != "$ws_last_status" ]; then
        log "    $wid → $ws_status"
        ws_last_status="$ws_status"
      fi
      if [ -n "$ws_url" ] && [ "$ws_url" != "$ws_last_url" ]; then
        log "    $wid url ready: $ws_url"
        ws_last_url="$ws_url"
      fi
      case "$ws_status" in
        online)
          if [ -n "$ws_url" ]; then
            break
          fi
          if [ "$ws_url_missing_logged" = "0" ]; then
            log "    $wid online but URL is not assigned yet — waiting for workspace routing readiness"
            ws_url_missing_logged=1
          fi
          sleep 10
          ;;
        failed)
          # Not a hard fail — bootstrap-watcher frequently marks failed at
          # 5 min on hermes, then heartbeat recovers to online around 10-13
          # min when install.sh finishes. Log once per workspace so the CI
          # output isn't spammy.
          if [ "$ws_failed_logged" = "0" ]; then
            log "    $wid transiently failed — waiting for heartbeat recovery (bootstrap-watcher deadline, see cp#245)"
            ws_failed_logged=1
          fi
          sleep 10
          ;;
        *)      sleep 10 ;;
      esac
    done
    ok "    $wid online and routable"
  done
}

# ─── 5. Provision parent workspace ─────────────────────────────────────
# Inject the LLM provider key so the runtime can authenticate at boot.
# Branch by which secret is set so the script supports multiple paths
# without forcing every dispatch to ship them all. Priority order
# matters — first non-empty wins:
#
#   E2E_MINIMAX_API_KEY → claude-code MiniMax path. Cheapest; selected as the
#     protected push/dispatch default in 2026-05 and retained today. Routes via
#     the claude-code
#     template's `minimax` provider (workspace-configs-templates/
#     claude-code-default/config.yaml:64-69) which sets
#     ANTHROPIC_BASE_URL=https://api.minimax.io/anthropic at boot.
#     MINIMAX_API_KEY is the vendor-specific env name the adapter
#     reads (PR #244 — per-vendor envs prevent ANTHROPIC_AUTH_TOKEN
#     collisions when a user runs MiniMax + Z.ai workspaces side-by-
#     side).
#
#   E2E_ANTHROPIC_API_KEY → claude-code direct-Anthropic path (added
#     2026-05-04 after #2578 left the operator with an awkward choice
#     between paying OpenAI's billing top-up and registering a new
#     MiniMax account). Lower friction than MiniMax for operators
#     who already have an Anthropic API key for their own Claude
#     Code session. Pricier per-token than MiniMax but billing is
#     still independent of MOLECULE_STAGING_OPENAI_API_KEY. Pinned to the
#     claude-code runtime — hermes/codex/openclaw use OpenAI-shaped envs.
#
#   E2E_OPENAI_API_KEY → hermes/codex/openclaw paths. Kept as fallback
#     for operator dispatches that explicitly want to exercise the
#     OpenAI path. The HERMES_* fields pin hermes-agent's bridge to
#     api.openai.com (template-hermes' derive-provider.sh otherwise
#     resolves openai/* → openrouter.ai and 401s). MODEL_PROVIDER
#     follows workspace/config.py:258's 'provider:model' format.
#
# All empty → '{}' (workspace will fail at first turn with an
# expected, actionable auth error rather than masking the test).
SECRETS_JSON='{}'
# Platform-managed path (E2E_LLM_PATH=platform) — the moonshot/kimi
# NOT_CONFIGURED regression (RFC#340 Fix A #2187). Molecule owns billing via the
# CP LLM proxy, so the workspace needs NO tenant key: provision with empty
# secrets and let the workspace boot purely on (a) the proxy env the control
# plane injects + (b) the manifest-derived `provider: platform` Fix A stamps into
# the generated config.yaml. This is the path that booted NOT_CONFIGURED in prod
# precisely because the BYOK branches below never exercise it. We deliberately
# skip the key-injection branches so a stray E2E_*_API_KEY in the runner env
# cannot silently convert this into a BYOK run and mask the regression.
if [ "${E2E_LLM_PATH:-}" = "platform" ]; then
  log "    LLM path: PLATFORM-MANAGED (no tenant key; proxy + Fix A provider stamp)"
  SECRETS_JSON='{}'
elif [ -n "${E2E_MINIMAX_API_KEY:-}" ]; then
  SECRETS_JSON=$(python3 -c "
import json, os
k = os.environ['E2E_MINIMAX_API_KEY']
print(json.dumps({
    'MINIMAX_API_KEY': k,
}))
")
elif [ -n "${E2E_ANTHROPIC_API_KEY:-}" ]; then
  # Direct Anthropic path — claude-code adapter reads ANTHROPIC_API_KEY
  # natively when ANTHROPIC_BASE_URL is unset. Useful for operators
  # who already have an Anthropic API key (e.g. for their own Claude
  # Code session) and want to avoid setting up a separate MiniMax
  # account just for E2E. Pricier per-token than MiniMax but billing
  # is still independent of MOLECULE_STAGING_OPENAI_API_KEY, so an OpenAI
  # quota collapse doesn't wedge this path. Pinned to the claude-code
  # runtime: hermes/codex/openclaw use OpenAI-shaped envs and won't honour
  # ANTHROPIC_API_KEY without further wiring. pick_model_slug maps this
  # branch to claude-sonnet-4-6 so the claude-code provider registry
  # selects anthropic-api instead of the OAuth-only sonnet alias.
  SECRETS_JSON=$(python3 -c "
import json, os
k = os.environ['E2E_ANTHROPIC_API_KEY']
print(json.dumps({
    'ANTHROPIC_API_KEY': k,
}))
")
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

# Idle-digest sub-step (task #219): with E2E_IDLE_DIGEST_CHECK=on (the
# ephemeral gate), inject the shrunken fire interval so the contract-driven
# idle digest can arm + fire within the run. Additive NON-LLM env only — it can
# never flip the platform-managed path to BYOK (the guard above is about keys).
# MOLECULE_MAILBOX_KERNEL=1 is explicit until the native-default runtime
# (mailbox kernel default ON) propagates into the template image pins; drop the
# line then so this gate proves the DEFAULT, not the flag.
if [ "${E2E_IDLE_DIGEST_CHECK:-}" = "on" ]; then
  SECRETS_JSON=$(SECRETS_JSON_IN="$SECRETS_JSON" python3 -c "
import json, os
s = json.loads(os.environ['SECRETS_JSON_IN'])
s['MOLECULE_MAILBOX_KERNEL'] = '1'
s['MOLECULE_IDLE_FIRE_SECONDS'] = os.environ.get('E2E_IDLE_FIRE_SECONDS', '30')
# Deterministic digest content: the goal-state provider seeds goal.yaml from
# this env at boot (runtime bootstrap_from_env, source=env-bootstrap). No LLM
# tool-call round-trip — the platform-managed model acked GOALSET twice in CI
# without invoking the tool, so an A2A seed is an unacceptable flake source.
s['MOLECULE_IDLE_GOAL'] = 'Keep the e2e pipeline green; pull the next backlog item when idle.'
print(json.dumps(s))
")
  log "    idle-digest check ON: MOLECULE_IDLE_FIRE_SECONDS=${E2E_IDLE_FIRE_SECONDS:-30} injected into workspace secrets"
fi

# Scheduler autonomous-fire sub-step (P5c): with E2E_SCHEDULER_CHECK=on, declare
# the molecule-scheduler kind:trigger plugin so the workspace boot-installs it
# (per-workspace delivery — the runtime arms the daemon when the plugin is
# present), and shrink the poll so a `* * * * *` schedule fires within the run.
# Additive NON-LLM env only — same platform-managed guard rationale as the idle
# block above. The plugin source is the public repo, fetched anonymously at boot.
if [ "${E2E_SCHEDULER_CHECK:-}" = "on" ]; then
  SECRETS_JSON=$(SECRETS_JSON_IN="$SECRETS_JSON" python3 -c "
import json, os
s = json.loads(os.environ['SECRETS_JSON_IN'])
src = os.environ.get('E2E_SCHEDULER_PLUGIN_SOURCE', 'gitea://molecule-ai/molecule-ai-plugin-scheduler#v0.1.0')
existing = s.get('MOLECULE_DECLARED_PLUGINS', '').strip()
s['MOLECULE_DECLARED_PLUGINS'] = (existing + ',' + src).strip(',') if existing else src
s['MOLECULE_TRIGGER_POLL_SECONDS'] = os.environ.get('E2E_TRIGGER_POLL_SECONDS', '10')
print(json.dumps(s))
")
  log "    scheduler check ON: declared molecule-scheduler + MOLECULE_TRIGGER_POLL_SECONDS=${E2E_TRIGGER_POLL_SECONDS:-10}"
fi

# Self-schedule tool sub-step (10f): with E2E_SELF_SCHEDULE_CHECK=on, additively
# declare the molecule-ai-plugin-schedule-self plugin (audience:self on its
# mcpServers contribution) onto the workspace roster so the runtime's audience
# injector renders the self-mode create_schedule tool surface. FIRING is done by
# the scheduler trigger daemon (10d), so 10f is CO-GATED on E2E_SCHEDULER_CHECK=on
# (enforced in the 10f VERIFY block). Additive NON-LLM env only — same
# platform-managed guard rationale as the scheduler/digest blocks above; the plugin
# source is the public repo, fetched anonymously at boot. Comma-appended so it
# coexists with the scheduler + digest-mail plugins on one roster.
if [ "${E2E_SELF_SCHEDULE_CHECK:-}" = "on" ]; then
  SECRETS_JSON=$(SECRETS_JSON_IN="$SECRETS_JSON" python3 -c "
import json, os
s = json.loads(os.environ['SECRETS_JSON_IN'])
src = os.environ.get('E2E_SELF_SCHEDULE_PLUGIN_SOURCE', 'gitea://molecule-ai/molecule-ai-plugin-schedule-self#v0.1.2')
existing = s.get('MOLECULE_DECLARED_PLUGINS', '').strip()
s['MOLECULE_DECLARED_PLUGINS'] = (existing + ',' + src).strip(',') if existing else src
print(json.dumps(s))
")
  log "    self-schedule check ON: declared ${E2E_SELF_SCHEDULE_PLUGIN_SOURCE:-molecule-ai-plugin-schedule-self#v0.1.2} additively onto MOLECULE_DECLARED_PLUGINS (co-gated on E2E_SCHEDULER_CHECK for the fire path)"
fi

# Native digest-provider plugin sub-step (10e, RFC molecule-core#4413): with
# E2E_DIGEST_PLUGIN_CHECK=on, declare a native digest-provider plugin and turn on
# the runtime's in-process digest-provider loader, so the workspace boot-installs
# the plugin and the idle-digest assembler loads its DigestProvider IN-PROCESS.
# We deliberately do NOT set MOLECULE_NATIVE_PLUGIN_NAMES: the load-time trust
# gate must admit this plugin from the VENDORED native-plugins registry (the
# runtime#310 SSOT source) — the whole point of this step. MOLECULE_MAILBOX_KERNEL
# + the shrunken fire interval are (idempotently) set so the digest actually arms
# and the loader runs within the window. Additive NON-LLM env only.
if [ "${E2E_DIGEST_PLUGIN_CHECK:-}" = "on" ]; then
  SECRETS_JSON=$(SECRETS_JSON_IN="$SECRETS_JSON" python3 -c "
import json, os
s = json.loads(os.environ['SECRETS_JSON_IN'])
src = os.environ.get('E2E_DIGEST_PLUGIN_SOURCE', 'gitea://molecule-ai/molecule-ai-plugin-digest-mail#v0.1.0')
existing = s.get('MOLECULE_DECLARED_PLUGINS', '').strip()
s['MOLECULE_DECLARED_PLUGINS'] = (existing + ',' + src).strip(',') if existing else src
s['MOLECULE_DIGEST_PROVIDER_PLUGINS'] = '1'          # D1 loader flag
s.setdefault('MOLECULE_MAILBOX_KERNEL', '1')          # ensure the idle digest arms
s.setdefault('MOLECULE_IDLE_FIRE_SECONDS', os.environ.get('E2E_IDLE_FIRE_SECONDS', '30'))
print(json.dumps(s))
")
  log "    digest-plugin check ON: declared ${E2E_DIGEST_PLUGIN_SOURCE:-molecule-ai-plugin-digest-mail} + MOLECULE_DIGEST_PROVIDER_PLUGINS=1 (trust source = vendored registry, no NATIVE_PLUGIN_NAMES)"
fi

MODEL_SLUG=$(pick_model_slug "$RUNTIME")
log "    MODEL_SLUG=$MODEL_SLUG"

# ─── BYOK vendor keys ship in the CREATE payload (no defer, no opt-in) ──
# Every vendor-key arm above (MiniMax / Anthropic / OpenAI-hermes) builds
# SECRETS_JSON holding ONLY that arm's own vendor key(s); the PLATFORM
# path (E2E_LLM_PATH=platform) builds SECRETS_JSON='{}'. We ship SECRETS_JSON
# verbatim in the POST /workspaces create payload for EVERY arm — no create-vs-
# defer split, no post-create opt-in write. Why this is correct AND robust:
#
#   - Controlplane validates BYOK creds at CREATE time (POST /workspaces →
#     MISSING_BYOK_CREDENTIAL when a byok model slug's vendor key is absent from
#     the payload). A key present at create is the create-time byok signal for
#     EVERY arm, not just MiniMax. The old strip-list DEFERRED some keys to a
#     post-create write, which risked that create reject: the MiniMax arm was
#     patched around it (7c657011) but the Anthropic arm still deferred
#     ANTHROPIC_API_KEY and hit the SAME failure. Shipping every arm's key at
#     create closes that whole class.
#   - workspace-server's secret-write gate
#     (rejectPlatformManagedDirectLLMBypassForWorkspace, secrets.go) blocks a
#     byok key only while the workspace's DERIVED provider is the closed
#     `platform` arm. platform-vs-BYOK is no longer a stored, opt-in-able mode:
#     the per-workspace `llm_billing_mode` override + its PUT/GET
#     /admin/workspaces/:id/llm-billing-mode endpoint were DELETED 2026-06-30
#     (881b3f6f1, internal#718). The gate now keys off providers.DeriveProvider →
#     IsPlatform, evaluated at create from the model slug in the payload: a byok
#     model derives byok, so its key is accepted at create. The PLATFORM path
#     ships no key, so the gate has nothing to block and the workspace stays
#     platform_managed (the moonshot/kimi NOT_CONFIGURED regression guard —
#     deliberately untouched).
#
# The deleted opt-in flow (create platform_managed → PUT billing-mode=byok →
# write the deferred key) is what the strip-list + a post-create write existed to
# serve. With the mode derived, that machinery is vestigial and removed. The
# #1994 byok-routing guard (8c) independently proves the key actually routes byok
# (MOLECULE_LLM_USAGE_TOKEN absent on the parent), so nothing is masked.

# ─── runtime → provision-selector resolution ────────────────────────────
# Most runtimes are selected directly by the `runtime` field. seo-agent is
# the exception: it is NOT a registry runtime (absent from manifest.json +
# runtime_registry.go knownRuntimes) — it is a claude-code-adapter template
# VARIANT selected by the `template` field. The ws-server Create handler reads
# the template's config.yaml, which declares `runtime: claude-code`, and
# resolves the concrete runtime from there (workspace.go:290-336). So for
# seo-agent we send template="seo-agent" and OMIT runtime, letting the
# template resolve it — sending an explicit runtime="seo-agent" would
# RUNTIME_UNSUPPORTED-422 at workspace.go:374-384 because it is not in
# knownRuntimes. PROVISION_TEMPLATE is "" for every real registry runtime.
PROVISION_TEMPLATE=""
case "$RUNTIME" in
  seo-agent) PROVISION_TEMPLATE="seo-agent" ;;
esac

# Build the create payload in Python so the optional `template`/`runtime`
# fields are emitted conditionally and the secrets blob is embedded without
# shell-escaping hazards. Args: name, [parent_id|""].
build_create_payload() {
  local name="$1" parent_id="${2:-}"
  E2E_WS_NAME="$name" \
  E2E_WS_PARENT_ID="$parent_id" \
  E2E_WS_RUNTIME="$RUNTIME" \
  E2E_WS_TEMPLATE="$PROVISION_TEMPLATE" \
  E2E_WS_MODEL="$MODEL_SLUG" \
  E2E_WS_SECRETS="$SECRETS_JSON" \
  python3 -c "
import json, os
secrets = json.loads(os.environ['E2E_WS_SECRETS'] or '{}')
payload = {
    'name': os.environ['E2E_WS_NAME'],
    'tier': 2,
    'model': os.environ['E2E_WS_MODEL'],
    'secrets': secrets,
}
tmpl = os.environ.get('E2E_WS_TEMPLATE', '')
if tmpl:
    # Template-selected variant (seo-agent): the template's config.yaml
    # resolves runtime=claude-code server-side. Do NOT also send an explicit
    # runtime — seo-agent is not a registry runtime and would 422.
    payload['template'] = tmpl
else:
    payload['runtime'] = os.environ['E2E_WS_RUNTIME']
pid = os.environ.get('E2E_WS_PARENT_ID', '')
if pid:
    payload['parent_id'] = pid
print(json.dumps(payload))
"
}

if [ -n "$PROVISION_TEMPLATE" ]; then
  log "5/11 Provisioning parent workspace (runtime=$RUNTIME via template=$PROVISION_TEMPLATE → claude-code adapter)..."
else
  log "5/11 Provisioning parent workspace (runtime=$RUNTIME)..."
fi
# tenant_call inherits CURL_COMMON's --fail-with-body, so a non-2xx create
# (e.g. the 422 RUNTIME_UNSUPPORTED below) makes curl exit 22. Capturing it
# bare as $(tenant_call ...) propagates that 22 through the command
# substitution and, under `set -euo pipefail`, ABORTS the whole script right
# here — before the `fail "... Response: ..."` handler below can print the
# body. The result was an opaque `curl: (22) ... error: 422` + teardown with
# no body (run 220702, main f78fef4c, step "5/11 Provisioning parent
# workspace"). set +e / `|| true` keeps the 22 from tripping `set -e`; curl
# still WROTE the body to stdout (that's what --fail-with-body does), so
# PARENT_RESP holds the 422 JSON and the id-check below surfaces WHY.
#
# Capture the RESPONSE HEADERS too (-D), because the body alone has repeatedly
# been useless here: this create intermittently returns a 503 with a COMPLETELY
# EMPTY body, ~1s after step 4 declared the tenant reachable (run 496830, main
# 6ff307d6d, "E2E Staging Platform Boot"; and 3 fresh tenants in run 494525).
# An empty-body 503 is the tell that it did NOT come from the Go handler — that
# would emit JSON — so it is being synthesised by something in front of the app
# (CF edge / tenant ingress / the app's own listener not yet accepting). The
# headers are the only thing that can distinguish those: `server:`, `cf-ray:`,
# and the HTTP status line name the responder. Without them this red is
# permanently un-RCA-able, which is how it survived this long.
PARENT_HDRS=$(mktemp)
E2E_TMP_FILES+=("$PARENT_HDRS")
# Bounded retry over the transient empty-body cold-origin 5xx (see
# create_workspace_post). A real create error (JSON body) still surfaces on the
# first try; a persistent 5xx exhausts the budget and the id-check below fails
# RED with the header diagnostic.
PARENT_RESP=$(create_workspace_post 'E2E Parent' "$PARENT_HDRS" \
  -H "Content-Type: application/json" \
  -d "$(build_create_payload 'E2E Parent')")
# Surface the workspace-create error CLEARLY instead of dying on a Python
# KeyError when the response has no 'id'. The load-bearing cases this names:
#   - seo-agent: an "invalid template" 400 if the seo-agent template isn't
#     present in the tenant's configs/cache dir (template-cache refresh gap).
PARENT_ID=$(echo "$PARENT_RESP" | python3 -c "import json,sys; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
if [ -z "$PARENT_ID" ]; then
  fail "Parent workspace create returned no 'id' (runtime=$RUNTIME, template=${PROVISION_TEMPLATE:-<none>}).
Response body: $(printf '%s' "$PARENT_RESP" | sanitize_http_body)
Response headers (who answered? an empty body + 503 here means it was NOT the Go handler — check 'server:' / 'cf-ray:'):
$(sanitize_http_body < "$PARENT_HDRS")"
fi
log "    PARENT_ID=$PARENT_ID"
# BYOK vendor key(s) shipped in the create payload above — nothing to write
# post-create (see the "BYOK vendor keys ship in the CREATE payload" block).

# ─── 6. Provision child (full mode only) ────────────────────────────────
CHILD_ID=""
if [ "$MODE" = "full" ]; then
  log "6/11 Provisioning child workspace..."
  # Same --fail-with-body / set -e abort guard as the parent create above:
  # let a non-2xx return the body so the id-check below surfaces it instead
  # of the script dying opaquely on curl exit 22.
  CHILD_HDRS=$(mktemp)
  E2E_TMP_FILES+=("$CHILD_HDRS")
  # Same bounded cold-origin 5xx retry + header capture as the parent create.
  CHILD_RESP=$(create_workspace_post 'E2E Child' "$CHILD_HDRS" \
    -H "Content-Type: application/json" \
    -d "$(build_create_payload 'E2E Child' "$PARENT_ID")")
  CHILD_ID=$(echo "$CHILD_RESP" | python3 -c "import json,sys; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
  if [ -z "$CHILD_ID" ]; then
    fail "Child workspace create returned no 'id' (runtime=$RUNTIME, template=${PROVISION_TEMPLATE:-<none>}).
Response body: $(printf '%s' "$CHILD_RESP" | sanitize_http_body)
Response headers (who answered? an empty body + 503 here means it was NOT the Go handler — check 'server:' / 'cf-ray:'):
$(sanitize_http_body < "$CHILD_HDRS")"
  fi
  log "    CHILD_ID=$CHILD_ID"
  # The child's create payload carried the same vendor key(s) — nothing to
  # write post-create (see the parent's create above).
else
  log "6/11 Canary mode — skipping child workspace"
fi

# ─── 7. Wait for workspace(s) online ───────────────────────────────────
# Hermes cold-boot takes 10-13 min on slow apt days (apt + uv + hermes
# install + npm browser-tools). The controlplane bootstrap-watcher
# deadline fires at 5 min and sets status=failed prematurely; heartbeat
# then transitions failed → online after install.sh finishes. So:
#
#   - ${WORKSPACE_ONLINE_TIMEOUT_SECS}s (~$((WORKSPACE_ONLINE_TIMEOUT_SECS/60)) min)
#     deadline (hermes worst-case + slack). Configurable via
#     E2E_WORKSPACE_ONLINE_TIMEOUT_SECS (#1646).
#   - 'failed' is a TRANSIENT state we must tolerate — log and keep
#     polling, only hard-fail at the deadline. Pre-bootstrap-watcher-fix
#     (controlplane#245) this was a flake generator: workspace went
#     failed→online inside our window but we bailed at the failed read.
WS_TO_CHECK=("$PARENT_ID")
[ -n "$CHILD_ID" ] && WS_TO_CHECK+=("$CHILD_ID")
wait_workspaces_online_routable "7/11 Waiting for workspace(s) to reach status=online (up to $((WORKSPACE_ONLINE_TIMEOUT_SECS/60)) min — hermes cold boot)..." "${WS_TO_CHECK[@]}"
live_milestone workspace_online

# ─── 7a. Real chat image upload/download round-trip ───────────────────
# This deliberately uses the production workflow: tenant admin/session auth
# uploads an image through the same /chat/uploads path the canvas uses. The
# byte-for-byte download check proves the platform delivered image bytes, not
# just metadata/name plumbing.
log "7a/11 Real image upload/download round-trip..."
PNG_FIXTURE=$(e2e_tmp /tmp/molecule-e2e-image.XXXXXX.png)
printf '%s' 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAFgwJ/lCqT+wAAAABJRU5ErkJggg==' | base64 -d > "$PNG_FIXTURE"
PNG_SHA=$(sha256sum "$PNG_FIXTURE" | awk '{print $1}')
for wid in "${WS_TO_CHECK[@]}"; do
  UP_TMP=$(e2e_tmp /tmp/e2e_upload.XXXXXX)
  UP_CODE_FILE=$(mktemp -t e2e_upload_code.XXXXXX)
  E2E_TMP_FILES+=("$UP_CODE_FILE")
  capture_http_status "$UP_CODE_FILE" \
    curl "${CURL_COMMON[@]}" -X POST "$TENANT_URL/workspaces/$wid/chat/uploads" \
    -H "Authorization: Bearer $EFFECTIVE_TENANT_TOKEN" \
    -H "X-Molecule-Org-Id: $ORG_ID" \
    "${TENANT_ROUTE_HDRS[@]}" \
    -F "files=@$PNG_FIXTURE;filename=e2e-smoke.png;type=image/png" \
    -o "$UP_TMP" \
    -w '%{http_code}'
  UP_CODE="$HTTP_CAPTURE_CODE"
  UP_CURL_RC="$HTTP_CAPTURE_RC"
  if ! http_code_is_exact_success "$UP_CODE" "$UP_CURL_RC" 200 201; then
    fail "Workspace $wid image upload returned $UP_CODE (curl_rc=$UP_CURL_RC): $(head -c 500 "$UP_TMP" | sanitize_http_body)"
  fi
  UP_URI=$(python3 -c "
import json, sys
d=json.load(open(sys.argv[1]))
def walk(x):
    if isinstance(x, dict):
        if x.get('uri'):
            print(x['uri']); raise SystemExit
        for v in x.values(): walk(v)
    elif isinstance(x, list):
        for v in x: walk(v)
walk(d)
" "$UP_TMP" 2>/dev/null || echo "")
  UP_MIME=$(python3 -c "
import json, sys
d=json.load(open(sys.argv[1]))
def walk(x):
    if isinstance(x, dict) and x.get('uri'):
        print(x.get('mimeType') or x.get('mime') or ''); raise SystemExit
    if isinstance(x, dict):
        for v in x.values(): walk(v)
    elif isinstance(x, list):
        for v in x: walk(v)
walk(d)
" "$UP_TMP" 2>/dev/null || echo "")
  rm -f "$UP_TMP" "$UP_CODE_FILE"
  [ -n "$UP_URI" ] || fail "Workspace $wid upload response had no workspace URI"
  [ "$UP_MIME" = "image/png" ] || fail "Workspace $wid upload returned mime=$UP_MIME, want image/png"

  DOWNLOAD_PATH="$UP_URI"
  case "$DOWNLOAD_PATH" in workspace:*) DOWNLOAD_PATH="${DOWNLOAD_PATH#workspace:}" ;; esac
  DL_TMP=$(e2e_tmp /tmp/e2e_download.XXXXXX.png)
  DL_CODE_FILE=$(mktemp -t e2e_download_code.XXXXXX)
  E2E_TMP_FILES+=("$DL_CODE_FILE")
  capture_http_status "$DL_CODE_FILE" \
    curl "${CURL_COMMON[@]}" "$TENANT_URL/workspaces/$wid/chat/download?path=$(python3 -c 'import urllib.parse,sys; print(urllib.parse.quote(sys.argv[1], safe=""))' "$DOWNLOAD_PATH")" \
    -H "Authorization: Bearer $EFFECTIVE_TENANT_TOKEN" \
    -H "X-Molecule-Org-Id: $ORG_ID" \
    "${TENANT_ROUTE_HDRS[@]}" \
    -o "$DL_TMP" \
    -w '%{http_code}'
  DL_CODE="$HTTP_CAPTURE_CODE"
  DL_CURL_RC="$HTTP_CAPTURE_RC"
  if ! http_code_is_exact_success "$DL_CODE" "$DL_CURL_RC" 200; then
    fail "Workspace $wid image download returned $DL_CODE (curl_rc=$DL_CURL_RC): $(head -c 300 "$DL_TMP" | sanitize_http_body)"
  fi
  DL_SHA=$(sha256sum "$DL_TMP" | awk '{print $1}')
  rm -f "$DL_TMP" "$DL_CODE_FILE"
  [ "$DL_SHA" = "$PNG_SHA" ] || fail "Workspace $wid image download SHA mismatch: upload=$PNG_SHA download=$DL_SHA"
  ok "    $wid image upload/download OK ($UP_MIME, sha256=$DL_SHA)"
done
rm -f "$PNG_FIXTURE"

# ─── 7b. Canvas-terminal diagnose ──────────────────────────────────────
# The active staging backend is local Docker. The endpoint returns
# {"ok": bool, "remote": bool, "first_failure": "name", "steps": [...]};
# use its own shape signal to keep the assertions honest:
#       * remote:false ⇒ genuine diagnoseLocal container-running check —
#         hard-assert ok=true (apart from the known socket-less check below).
#       * remote:true  ⇒ a response shape with no valid local-Docker assertion;
#         skip explicitly rather than false-pass it. Container execution is
#         covered by wait_workspaces_online_routable (step 7) + A2A (step 8).
log "7b/11 Canvas-terminal diagnose probe..."
for wid in "${WS_TO_CHECK[@]}"; do
  DIAG_JSON=$(tenant_call GET "/workspaces/$wid/terminal/diagnose" 2>/dev/null || echo '{}')
  DIAG_OK=$(echo "$DIAG_JSON" | python3 -c "import json,sys; d=json.load(sys.stdin); print('true' if d.get('ok') else 'false')" 2>/dev/null || echo "false")
  DIAG_REMOTE=$(echo "$DIAG_JSON" | python3 -c "import json,sys; d=json.load(sys.stdin); print('true' if d.get('remote') else 'false')" 2>/dev/null || echo "false")
  if [ "$DIAG_OK" = "true" ]; then
    ok "    $wid terminal-reachable (canvas terminal will work)"
  elif [ "$DIAG_REMOTE" = "true" ]; then
    # The active backend is local Docker, so a remote-shape response has no
    # assertion we can honestly make here.
    log "    ⏭️  $wid terminal-diagnose remote-shape hard-check SKIPPED (active backend=local-docker): this response has no valid local assertion; container execution is covered by online+routable (step 7) and A2A (step 8)."
  else
    # diagnoseLocal (remote:false) reported ok=false. Apart from the known
    # docker-client absence below, this is a real local-Docker failure.
    DIAG_FAIL=$(echo "$DIAG_JSON" | python3 -c "import json,sys; d=json.load(sys.stdin); print(d.get('first_failure','unknown'))" 2>/dev/null || echo "unknown")
    DIAG_DETAIL=$(echo "$DIAG_JSON" | python3 -c "import json,sys; d=json.load(sys.stdin); s=[x for x in d.get('steps',[]) if not x.get('ok')]; step=s[0] if s else {}; print(' — '.join(x for x in [step.get('error',''), step.get('detail','')] if x))" 2>/dev/null || echo "")
    # #767: always emit the full diagnose JSON so operators see every step's
    # Detail field even when the Python extraction above fails or the shape
    # drifts. The burst is bracketed like steps 2 and 4 for grep-friendly CI.
    log "── DIAGNOSTIC BURST (step 7b — terminal diagnose for $wid) ──"
    echo "$DIAG_JSON" | python3 -m json.tool 2>/dev/null || echo "$DIAG_JSON"
    log "── END DIAGNOSTIC ──"
    if [ "$DIAG_FAIL" = "docker-available" ]; then
      # A real staging TENANT's workspace-server runs INSIDE the tenant container
      # with NO docker socket (by security design — a tenant must never reach the
      # host daemon; that IS the finding-#1 escape surface). So diagnoseLocal's
      # `h.docker == nil` docker-available check legitimately reports not-ok, and
      # that is NOT a terminal regression: the tenant terminal does not
      # docker-exec-into-itself via the workspace-server's own client. Container
      # execution is already asserted by online+routable (step 7) + A2A (step 8),
      # so SKIP this ONE sub-step (post-#3969 the routing is correct — remote:false
      # — but diagnoseLocal's first check is docker-availability). A diagnose
      # failure at ANY OTHER step (container-running / exec) still hard-fails below.
      # FOLLOW-UP: terminal_diagnose.diagnoseLocal should model the real tenant
      # terminal path instead of hard-erroring on a nil docker client.
      log "    ⏭️  $wid terminal-diagnose docker-available SKIPPED (local-Docker tenant workspace-server has no docker socket by design; container execution is covered by steps 7+8): $DIAG_DETAIL"
    else
      fail "Workspace $wid diagnoseLocal (remote:false) reported ok=false at step '$DIAG_FAIL': $DIAG_DETAIL — the local-Docker container-running check failed (docker.Ping / container exec)"
    fi
  fi
done

# ─── 7c. Workspace files API config.yaml PUT ───────────────────────────
# Exercise the Canvas Config save path with a run-specific marker. The current
# local-Docker tenant process has no Docker socket; until WriteFile models that
# topology, only its exact `docker not available` 5xx is an explicit skip. All
# other transport, auth, route, validation, and server failures remain hard
# failures. This is a PUT check, not a read-back round trip.
log "7c/11 Files API config.yaml PUT..."
CONFIG_MARKER="# molecule-synth-e2e: ${E2E_RUN_ID:-unknown} ${RUNTIME} $(date -u +%Y-%m-%dT%H:%M:%SZ)"
CONFIG_PAYLOAD="${CONFIG_MARKER}
name: synth-canary
runtime: ${RUNTIME}
"
# (Idle-digest goal seeding happens via an A2A goal-set turn after step 8 —
# an idle_prompt line here proved ineffective on the ephemeral topology: the
# files-API PUT does not reboot the workspace, so the boot-time
# migrate_from_config never re-runs. Caught by this gate's own first run.)
for wid in "${WS_TO_CHECK[@]}"; do
  PUT_BODY=$(python3 -c "import json,sys; print(json.dumps({'content': sys.stdin.read()}))" <<< "$CONFIG_PAYLOAD")
  # Capture body to a tempfile so curl's -w '%{http_code}' is the only
  # thing on stdout. The first version used `-w '\n%{http_code}\n'` and
  # parsed via `tail -n 2 | head -n 1`, which broke because bash $(...)
  # strips the trailing newline → only 2 lines remain in the captured
  # value → head -n 1 returned the body, not the status code. Caught
  # post-merge by E2E Staging SaaS at 22:06 UTC: a 200-with-body got
  # misreported as "PUT returned <body>".
  PUT_TMP=$(mktemp -t synth_put.XXXXXX)
  PUT_CODE_FILE=$(mktemp -t synth_put_code.XXXXXX)
  E2E_TMP_FILES+=("$PUT_TMP" "$PUT_CODE_FILE")
  capture_http_status "$PUT_CODE_FILE" \
    tenant_call PUT "/workspaces/$wid/files/config.yaml" \
    -H "Content-Type: application/json" \
    -d "$PUT_BODY" \
    -o "$PUT_TMP" \
    -w '%{http_code}'
  PUT_CODE="$HTTP_CAPTURE_CODE"
  PUT_CURL_RC="$HTTP_CAPTURE_RC"
  PUT_BODY_OUT=$(cat "$PUT_TMP" 2>/dev/null || echo "")
  rm -f "$PUT_TMP" "$PUT_CODE_FILE"
  if ! http_code_is_exact_success "$PUT_CODE" "$PUT_CURL_RC" 200 204; then
    # The active local-Docker WriteFile path shape-routes to docker
    # CopyToContainer. The narrow skip below is gated on the
    # SPECIFIC docker-less-tenant signal, mirroring 7b's `docker-available`
    # narrowing — NOT on "any non-2xx". A real staging tenant's
    # workspace-server runs INSIDE the tenant container with NO docker socket
    # (#206, by security design). WriteFile's container dispatch therefore
    # falls through findContainer (h.docker==nil ⇒ "") to writeViaEphemeral,
    # whose first act is `if h.docker == nil { return "docker not available" }`
    # — surfaced by the handler as HTTP 500 {"error":"failed to write file:
    # docker not available"} (workspace-server container_files.go:130-131,
    # templates.go:851-853). That is the ONLY non-2xx we skip.
    #
    # Everything else stays a HARD FAIL because it points at a
    # real outage this step exists to catch, not a backend-shape mismatch:
    #   - 000 → CP/tenant unreachable (curl rc!=0 / connection refused).
    #   - 5xx WITHOUT the docker-not-available marker → a genuine config-write
    #     regression (e.g. writeViaEphemeral failed AFTER the docker check).
    #   - 4xx (400/401/403/404/…) → auth/route/validation regression.
    if [ "$PUT_CODE" != "000" ] && \
         { [ "$PUT_CODE" -ge 500 ] 2>/dev/null; } && \
         printf '%s' "$PUT_BODY_OUT" | grep -qi 'docker not available'; then
      # Local-Docker tenant, 5xx, body carries the docker-less signal — the
      # WriteFile docker-exec dispatch has no local analog on a socket-less
      # tenant. Config-write coverage for this backend is provided by the
      # container reading /configs/config.yaml directly via bind-mount at
      # boot (asserted by online+routable step 7). SKIP this ONE sub-step.
      log "    ⏭️  $wid Files API PUT config.yaml container-write hard-check SKIPPED (local-Docker socket-less tenant; docker-not-available): PUT returned $PUT_CODE (curl_rc=$PUT_CURL_RC). The docker-exec fallback needs a socket the tenant lacks by design (#206). Body: $PUT_BODY_OUT"
      continue
    else
      # 000 (unreachable) or 5xx-without-marker or any 4xx → a real outage /
      # regression, NOT a backend-shape mismatch. Hard-fail so this gate
      # keeps catching what it exists to catch on local-Docker.
      fail "Workspace $wid Files API PUT config.yaml returned $PUT_CODE (curl_rc=$PUT_CURL_RC; local-Docker backend, NOT the docker-not-available socket-less signal): $PUT_BODY_OUT — a 000 means CP/tenant unreachable; a 5xx without 'docker not available' is a genuine config-write regression; a 4xx is an auth/route/validation regression. This is a real failure, not a backend-shape mismatch."
    fi
  fi
  # This remains PUT-only until local-Docker read/write paths share the same
  # supported storage contract; do not describe it as round-trip coverage.
  ok "    $wid config.yaml PUT OK (HTTP $PUT_CODE)"
done

# Saving config.yaml follows the same path as Canvas Config Save & Restart.
# The controlplane can briefly put the workspace back into provisioning and
# clear its route while the runtime restarts, so A2A must wait on the same
# externally routable readiness boundary again.
wait_workspaces_online_routable "7d/11 Waiting for workspace(s) to recover routing after config.yaml PUT..." "${WS_TO_CHECK[@]}"

# ─── A2A send with 202-queued poll helper (core#2437 part B) ───────────
# Sends POST /workspaces/:id/a2a. If the agent is busy/starting and returns
# a 2xx with queued:true + queue_id, poll GetA2AQueueStatus until the durable
# result is available. Handles curl rc 28 / http 000 / 404 retryable while the
# queue row is still materializing, and transient 502/503/504 cold-start.
# Prints the final A2A JSON-RPC response body to stdout; logs to stderr.
#
# Detect the runtime's synchronous interrupt-acknowledgement. When an A2A
# message/send lands on an agent that is mid-turn (a freshly-provisioned
# parent is often still working its boot/child-notification task —
# "iteration 1/90"), the native_session SDK (claude-agent-sdk / hermes) does
# NOT reject the request as busy: it INTERRUPTS its current turn and returns a
# clean 200 with an ack — "⚡ Interrupting current task (iteration N/M). I'll
# respond to your message shortly." — in place of the requested answer, which
# it produces on its NEXT turn. Because the ack is a clean 200 (not an
# upstream timeout), it bypasses the busy→enqueue durable path in
# a2a_proxy_helpers.go and passes straight through to the caller. The ack is
# benign (message accepted), NOT an error payload, so the caller must re-send
# to collect the real reply rather than assert content against the ack.
a2a_is_interrupt_ack() {
  local body="$1" txt
  txt=$(printf '%s' "$body" | python3 -c "
import json,sys
try:
    d=json.load(sys.stdin)
    r=d.get('result') or {}
    parts=r.get('parts') or []
    # task-shaped responses carry text under result.status.message.parts
    if not parts:
        parts=((r.get('status') or {}).get('message') or {}).get('parts') or []
    print(' '.join(p.get('text','') for p in parts if isinstance(p,dict)))
except Exception:
    print('')" 2>/dev/null || echo "")
  # Fall back to the raw body when the shape doesn't parse — the signature
  # phrase is distinctive enough that a raw match won't false-positive on a
  # real answer.
  [ -z "$txt" ] && txt="$body"
  printf '%s' "$txt" | grep -qiE "Interrupting current task|respond to your message shortly"
}

a2a_send_or_poll_queue() {
  local ws_id="$1"; shift
  local payload="$1"; shift
  local label="$1"
  local tmp qid resp code rc attempt poll_attempt poll_tmp
  local a2a_gateway_error_seen=0 last_qstatus="" queue_poll_count=0
  local interrupt_ack_count=0 max_interrupt_ack=6 interrupt_ack_backoff=12
  tmp=$(mktemp -t a2a_poll.XXXXXX)
  qid=""

  for attempt in $(seq 1 12); do
    if [ -n "$qid" ]; then
      # We have a queue_id — poll GetA2AQueueStatus for the durable result.
      poll_tmp=$(mktemp -t a2a_qpoll.XXXXXX)
      for poll_attempt in $(seq 1 30); do
        : >"$poll_tmp"
        set +e
        code=$(tenant_call GET "/workspaces/$ws_id/a2a/queue/$qid" \
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
          echo "    $label queue poll attempt $poll_attempt/30: curl_rc=$rc http=$code — retryable, backing off 2s" >&2
          sleep 2
          continue
        fi
        if [ "$code" -lt 200 ] || [ "$code" -ge 300 ]; then
          rm -f "$poll_tmp" "$tmp"
          fail "$label queue poll failed (http=$code): $(printf '%s' "$resp" | sanitize_http_body)"
        fi

        local qstatus
        qstatus=$(printf '%s' "$resp" | python3 -c "
import json,sys
try:
    print(json.load(sys.stdin).get('status',''))
except Exception:
    print('')" 2>/dev/null || echo "")
        case "$qstatus" in
          completed)
            resp=$(printf '%s' "$resp" | python3 -c "
import json,sys
try:
    rb=json.load(sys.stdin).get('response_body')
    print(json.dumps(rb) if rb is not None else '')
except Exception:
    print('')" 2>/dev/null || echo "")
            if [ -n "$resp" ]; then
              # The interrupt-ack can arrive THROUGH THE QUEUE too: with the
              # settling/busy→enqueue path (core#4069) the send is queued, the
              # drain dispatches it to a mid-task agent, and the agent's
              # synchronous interrupt-ack becomes the queue item's durable
              # response_body — bypassing the synchronous elif below (found by
              # the ephemeral gate: PONG failed at +28s with the ack via a
              # COMPLETED queue item). Same treatment: re-send bounded for the
              # real reply; a wedged agent still exhausts the budget → RED.
              if a2a_is_interrupt_ack "$resp"; then
                interrupt_ack_count=$((interrupt_ack_count + 1))
                if [ "$interrupt_ack_count" -le "$max_interrupt_ack" ]; then
                  echo "    $label interrupt-ack via queue item $qid (agent mid-task) — re-sending for the real reply (${interrupt_ack_count}/${max_interrupt_ack}), backing off ${interrupt_ack_backoff}s" >&2
                  qid=""
                  rm -f "$poll_tmp"
                  sleep "$interrupt_ack_backoff"
                  continue 2
                fi
                rm -f "$poll_tmp" "$tmp"
                fail "$label agent never produced a real reply after ${interrupt_ack_count} interrupt-acknowledgement(s) (~$((max_interrupt_ack * interrupt_ack_backoff))s, last via queue): it accepted the message and interrupted its current task but never yielded a follow-up turn (likely wedged mid-task). Last ack: $(printf '%s' "$resp" | sanitize_http_body | head -c 300)"
              fi
              code=200
              break 2
            fi
            ;;
          failed|dropped)
            rm -f "$poll_tmp" "$tmp"
            fail "$label queue item $qid terminal status=$qstatus: $(printf '%s' "$resp" | sanitize_http_body)"
            ;;
          queued|dispatched|in_progress|"")
            last_qstatus="$qstatus"
            queue_poll_count=$((queue_poll_count + 1))
            echo "    $label queue poll attempt $poll_attempt/30 status=$qstatus — backing off 2s" >&2
            sleep 2
            ;;
          *)
            rm -f "$poll_tmp" "$tmp"
            fail "$label queue poll unexpected status=$qstatus: $(printf '%s' "$resp" | sanitize_http_body)"
            ;;
        esac
      done
      rm -f "$poll_tmp"
      # Ran out of queue poll attempts.
      # core#2917: if a gateway-edge error preceded a queued task that never
      # drained, treat it as a transient A2A-layer infra-skip rather than a
      # workspace-code failure. The flag is only set for edge signals (Bad
      # Gateway/Gateway Timeout/error-code 502/504/no healthy upstream), never
      # for agent-origin signals that could mask a real regression.
      # Verified signature: 502/503/504 on an initial POST, queue_id assigned,
      # then 30/30 polls stuck in queued/dispatched/in_progress/empty.
      case "$last_qstatus" in
        queued|dispatched|in_progress|"")
          if [ "$a2a_gateway_error_seen" = "1" ] && [ -n "$qid" ]; then
            rm -f "$tmp"
            infra_skip "a2a-queue-timeout" "queue_id=$qid poll_count=${queue_poll_count}/30 last_status=${last_qstatus:-<empty>}"
          fi
          ;;
      esac
      fail "$label queue poll timed out waiting for $qid to complete"
    fi

    # Initial POST (or retry before we have a queue_id).
    : >"$tmp"
    set +e
    code=$(tenant_call POST "/workspaces/$ws_id/a2a" \
      --max-time 90 \
      -H "Content-Type: application/json" \
      -H "X-Workspace-ID: $ws_id" \
      -d "$payload" \
      -o "$tmp" \
      -w '%{http_code}' \
      2>/dev/null)
    rc=$?
    set -e
    code=${code:-000}
    resp=$(cat "$tmp" 2>/dev/null || echo "")

    if [ "$rc" = "0" ] && [ "$code" -ge 200 ] && [ "$code" -lt 300 ]; then
      local is_queued
      is_queued=$(printf '%s' "$resp" | python3 -c "
import json,sys
try:
    d=json.load(sys.stdin)
    print('true' if d.get('queued') is True or (d.get('status') or '').lower() == 'queued' else 'false')
except Exception:
    print('false')" 2>/dev/null || echo "false")
      if [ "$is_queued" = "true" ]; then
        qid=$(printf '%s' "$resp" | python3 -c "
import json,sys
try:
    print(json.load(sys.stdin).get('queue_id',''))
except Exception:
    print('')" 2>/dev/null || echo "")
        if [ -n "$qid" ]; then
          echo "    $label A2A queued (queue_id=$qid); switching to poll" >&2
          continue
        fi
      elif a2a_is_interrupt_ack "$resp"; then
        # Synchronous interrupt-ack (agent was mid-turn). The message WAS
        # accepted and the current task interrupted; the real answer lands on
        # the agent's next turn, which this request did not carry. Re-send to
        # collect it: on re-send the agent is idle (→ synchronous real answer)
        # or still draining the interrupt (→ upstream-busy → the durable
        # enqueue+poll path above). Bounded and spaced so a genuinely wedged
        # agent (interrupt accepted, never yields a follow-up turn) still
        # fails RED instead of being masked as a pass on the ack text.
        interrupt_ack_count=$((interrupt_ack_count + 1))
        if [ "$interrupt_ack_count" -le "$max_interrupt_ack" ]; then
          echo "    $label interrupt-ack (agent mid-task) — re-sending for the real reply (${interrupt_ack_count}/${max_interrupt_ack}), backing off ${interrupt_ack_backoff}s" >&2
          sleep "$interrupt_ack_backoff"
          continue
        fi
        rm -f "$tmp"
        fail "$label agent never produced a real reply after ${interrupt_ack_count} interrupt-acknowledgement(s) (~$((max_interrupt_ack * interrupt_ack_backoff))s): it accepted the message and interrupted its current task but never yielded a follow-up turn (likely wedged mid-task). Last ack: $(printf '%s' "$resp" | sanitize_http_body | head -c 300)"
      else
        break
      fi
    fi

    local safe_body
    safe_body=$(printf '%s' "$resp" | sanitize_http_body)
    if echo "$code" | grep -Eq '^(502|503|504)$'; then
      # core#2917: split gateway-edge signals (unambiguous transient infra,
      # eligible for the queue-timeout infra-skip) from agent-origin signals
      # that can hide a real workspace-agent regression. Only edge signals set
      # a2a_gateway_error_seen; agent-origin retries are still allowed but will
      # never skip-to-green if the queue never drains.
      if echo "$safe_body" | grep -Eqi 'Service Unavailable|Bad Gateway|Gateway Timeout|error code: 502|error code: 504|no healthy upstream'; then
        a2a_gateway_error_seen=1
        echo "    $label A2A transient gateway $code attempt $attempt/12: $safe_body" >&2
        if [ "$attempt" -lt 12 ]; then
          sleep 10
          continue
        fi
      elif echo "$safe_body" | grep -Eqi 'workspace agent unreachable|connection refused|workspace agent busy|native_session|restarting|restart triggered|workspace has no URL|has no URL|"status" *: *"provisioning"|provisioning|not publicly routable'; then
        echo "    $label A2A agent-origin $code attempt $attempt/12: $safe_body" >&2
        if [ "$attempt" -lt 12 ]; then
          # Agent restart/cold-start can take tens of seconds; keep polling,
          # but do NOT treat this as an edge-gateway transient eligible for skip.
          #
          # The `workspace has no URL` / `"status":"provisioning"` 503 is the
          # config.yaml-PUT restart flap: step 7c PUTs config.yaml, which
          # triggers a workspace restart; step 7d's routing-recovery poll can
          # observe the PRE-restart online state and pass ~1s later, then the
          # restart flips the workspace back to provisioning (no URL) just as
          # this A2A send fires (RCA of run 480639, 2026-07-12: 7d online at
          # 11:21:57 → 503 provisioning at 11:22:12). Treat it as the same
          # "workspace not ready, come back" class as `restarting`: keep polling
          # (30s) until the restart settles and the URL returns. A workspace
          # genuinely stuck in provisioning still exhausts the budget → RED.
          #
          # `workspace URL is not publicly routable` (502) is the sibling race
          # on the URL's FORM rather than its presence: ~1s after online, the
          # workspace row can still carry a URL shape the tenant's SSRF guard
          # (a2a_proxy.go isSafeURL) rejects, until the workspace's own
          # registration/heartbeat lands the routable form (observed on the
          # ephemeral gate: identical send 1s post-online PONGed in one run and
          # 502'd in the next). Same settling class, same bounded retry.
          sleep 30
          continue
        fi
      fi
    fi
    break
  done

  rm -f "$tmp"
  if [ "$rc" != "0" ] || [ "$code" -lt 200 ] || [ "$code" -ge 300 ]; then
    # core#2917: outright A2A connect timeout (curl_rc=28, http=000) is the
    # second verified transient-infra signature, not a workspace bug.
    if [ "$rc" = "28" ] && [ "$code" = "000" ]; then
      infra_skip "a2a-connect-timeout" "curl_rc=$rc http=$code attempt=$attempt label=$label"
    fi
    fail "$label failed after $attempt attempt(s) (curl_rc=$rc, http=$code): $(printf '%s' "$resp" | sanitize_http_body)"
  fi
  printf '%s' "$resp"
}

# When a2a_send_or_poll_queue hits a verified transient-infra signature it
# calls infra_skip(), but because the function is invoked via command
# substitution, bash exit 0 only terminates the subshell and the captured
# marker is returned as stdout. Detect that marker in the parent shell and
# re-invoke infra_skip so the whole advisory lane actually skips instead of
# falling through to the real-completion gate and failing.
a2a_handle_infra_skip() {
  local output="$1" label="${2:-$1}"
  case "$output" in
    *"scan_status: infra-skip:"*)
      local reason detail
      reason=$(printf '%s' "$output" | sed -n 's/.*scan_status: infra-skip:\([^[:space:]]*\).*/\1/p')
      detail=$(printf '%s' "$output" | sed -n 's/.*scan_status: infra-skip:[^[:space:]]*[[:space:]][[:space:]]*\(.*\)/\1/p')
      infra_skip "${reason:-a2a-layer}" "${detail:-$label}"
      ;;
  esac
}

# ─── 8. A2A round-trip on parent ───────────────────────────────────────
log "8/11 Sending A2A message to parent — expecting agent response..."
# Smoke prompt phrasing — DO NOT trim back to the bare "Reply with exactly: PONG"
# version that ran here pre-2026-05-02. After the Platform Capabilities preamble
# (#2332, 2026-04-30) landed in the system prompt, GPT-4o began intermittently
# refusing the bare echo prompt with messages like:
#   - "I'm unable to do that."
#   - "I'm unable to fulfill that request. Can I assist you with anything else?"
#   - "I'm unable to reply with responses that don't allow me to fulfill tasks…"
# 3 fails / 10 runs ≈ 30% flake. Root cause: the preamble primes the model
# ("Use them proactively") to expect tool use, then a zero-tool echo request
# reads as out-of-role. Real user prompts (which is what hits prod) don't
# trigger this — only this contrived smoke prompt does, so the right fix is
# in the prompt phrasing, not in the platform's system prompt. Keep the
# explicit "no tools needed" framing so the model has permission to comply.
A2A_PAYLOAD=$(python3 -c "
import json, uuid
print(json.dumps({
    'jsonrpc': '2.0',
    'method': 'message/send',
    'id': 'e2e-msg-1',
    'params': {
        'message': {
            'role': 'user',
            'messageId': f'e2e-{uuid.uuid4().hex[:8]}',
            'parts': [{'kind': 'text', 'text': 'This is the platform smoke test verifying agent wiring. No tools or memory are needed — please respond with exactly the single token: PONG'}]
        }
    }
}))
")
# Override CURL_COMMON's --max-time 30 for THIS call only. Each canary
# creates a fresh org → workspace, so the A2A POST hits a cold model:
# claude-code adapter starts its event loop, opens TLS to the LLM
# endpoint, ships the first prompt, waits for first token. With MiniMax
# (which is the canary default since #2710) cold-call latency
# routinely exceeds 30s on the first request after workspace boot.
# 90s gives ~3x headroom over observed cold-call P95 (~25-30s).
# Subsequent A2A turns hit the same workspace and are sub-second, so
# this only widens the window for step 8/11 of the canary's first turn.
# core#2437 part B: send A2A and, if the agent is busy/starting, poll the
# queue status endpoint until the durable result is available.
A2A_RESP=$(a2a_send_or_poll_queue "$PARENT_ID" "$A2A_PAYLOAD" "A2A parent")
a2a_handle_infra_skip "$A2A_RESP" "A2A parent"
AGENT_TEXT=$(echo "$A2A_RESP" | python3 -c "
import json, sys
d = json.load(sys.stdin)
parts = d.get('result', {}).get('parts', [])
print(parts[0].get('text', '') if parts else '')
" 2>/dev/null || echo "")
if [ -z "$AGENT_TEXT" ]; then
  fail "A2A returned no text. Raw: $A2A_RESP"
fi

# Specific error-class checks — each pattern caught a real P0 bug on
# 2026-04-23 that a generic "error|exception" check missed or misreported:
#
#   "[hermes-agent error 401]"       → gateway API_SERVER_KEY not propagated (hermes #12)
#   "Invalid API key"                → tenant auth chain (CP #238 race)
#   "model_not_found"                → hermes custom provider slug passthrough (#13)
#   "Encrypted content is not supported" → hermes codex_responses API misroute (#14)
#   "Unknown provider"               → bridge misconfigured PROVIDER= (regression of #13 fix)
#   "hermes-agent unreachable"       → gateway process died
#   "exceeded your current quota"    → MOLECULE_STAGING_OPENAI_API_KEY billing (NOT a platform regression — #2578)
#
# Fail LOUD with the specific pattern so CI log + alert channel makes the
# regression unambiguous.
if echo "$AGENT_TEXT" | grep -qF "[hermes-agent error 401]"; then
  fail "A2A — REGRESSION: hermes gateway auth broken (API_SERVER_KEY not in runtime env). See template-hermes#12. Raw: $AGENT_TEXT"
fi
if echo "$AGENT_TEXT" | grep -qF "hermes-agent unreachable"; then
  fail "A2A — REGRESSION: hermes gateway process down. Check /var/log/hermes-gateway.log in the workspace runtime. Raw: $AGENT_TEXT"
fi
if echo "$AGENT_TEXT" | grep -qF "model_not_found"; then
  fail "A2A — REGRESSION: model slug passed through with provider prefix. See template-hermes#13. Raw: $AGENT_TEXT"
fi
if echo "$AGENT_TEXT" | grep -qF "Encrypted content is not supported"; then
  fail "A2A — REGRESSION: hermes custom provider hit /v1/responses instead of chat_completions. Config.yaml should declare api_mode: chat_completions. See template-hermes#14. Raw: $AGENT_TEXT"
fi
if echo "$AGENT_TEXT" | grep -qF "Unknown provider"; then
  fail "A2A — REGRESSION: install.sh set PROVIDER to a value not in hermes's registry. Run 'hermes doctor' on the workspace to see valid values. Raw: $AGENT_TEXT"
fi
# "Invalid API key" — the comment block lists this as a CP #238 race
# (tenant auth chain) signal but the grep was missing. Caller-side
# 401's containing this exact phrase don't match the generic
# "error|exception" catch-all below, so they'd slip through.
if echo "$AGENT_TEXT" | grep -qF "Invalid API key"; then
  fail "A2A — REGRESSION: tenant auth chain returned 'Invalid API key'. Likely CP boot-event 401 race (CP #238) or stale OPENAI_API_KEY in the runtime env. Raw: $AGENT_TEXT"
fi
# Provider quota exhausted — distinguish from a platform regression so
# the canary alert names the operator action directly instead of falling
# through to the generic "error-shaped response" message. Steps 0-7 having
# passed means the platform itself is healthy (CP up, tenant provisioned,
# workspace online, A2A delivery end-to-end). When the agent comes back
# with a provider-side 429, that is a billing event on the configured
# OpenAI key, not a platform regression. Tracked in #2578.
if echo "$AGENT_TEXT" | grep -qiE "exceeded your current quota|insufficient_quota"; then
  fail "A2A — PROVIDER QUOTA EXHAUSTED (NOT a platform regression). Operator action: top up MOLECULE_STAGING_OPENAI_API_KEY billing or rotate to a higher-quota org at Settings → Secrets and Variables → Actions. Tracked in #2578. Raw: $AGENT_TEXT"
fi
# Empty-completion class — the agent runtime reached the LLM and got a
# 2xx back, but the assistant turn carried NO text part (empty content,
# or tool_calls/reasoning-only with no surfaced text), so the runtime
# returns the literal "Error: message contained no text content." as its
# reply text. Steps 0-7 passing means the platform is healthy (CP up,
# tenant provisioned, workspace online + routable, A2A delivery e2e); the
# break is the configured completion BACKEND returning an empty turn — a
# model/provider-side regression, NOT a workspace-server or harness bug,
# and NOT NOT_CONFIGURED (that fails earlier, at boot). Name it explicitly
# so the canary alert points at the model, not the platform: a generic
# "error-shaped response" misdirects triage to workspace-server. Observed
# 2026-06-03/04 across every staging canary on MODEL_SLUG=MiniMax-M2 (the
# canary default since #2710) — 100% on the parent's first cold turn,
# identical across the then-scheduled main synthetic and PR runs (so it was an
# environmental backend regression, not PR-introduced).
if echo "$AGENT_TEXT" | grep -qiF "message contained no text content"; then
  fail "A2A — EMPTY COMPLETION (backend regression, NOT a platform/workspace-server bug). The configured model (MODEL_SLUG=${MODEL_SLUG:-?}) returned a 2xx completion with no text part; the runtime surfaced 'message contained no text content.'. Operator action: check the staging LLM backend / proxy for the canary model (the claude-code MiniMax-BYOK default is the BARE registered id MiniMax-M2.7 — the colon minimax:MiniMax-M2.7 is UNREGISTERED on claude-code, internal#718) — empty assistant turns, not an auth/quota/boot fault. Raw: $AGENT_TEXT"
fi
# Generic catch-all — falls through if none of the known regressions hit.
# _ResultError is the claude-code runtime surfacing an LLM/backend/runtime
# failure AS text. Diagnose it explicitly (#2712) so the next canary run
# prints the upstream error instead of forcing operators to scrape workspace
# logs. The suite still fails; this is diagnostics-only.
if echo "$AGENT_TEXT" | grep -qiF "_ResultError"; then
  diagnose_staging_result_error "$PARENT_ID" "$A2A_RESP" "A2A parent _ResultError"
  _redacted_agent_text=$(printf '%s' "$AGENT_TEXT" | redact_secrets)
  fail "A2A — STAGING LLM/BACKEND/RUNTIME FAILURE (_ResultError). The canary agent surfaced its LLM/backend/runtime error as a text payload. See the diagnostic burst above for the full A2A response, workspace state, and recent activity logs (including any upstream HTTP status/body the runtime reported). Raw (redacted): ${_redacted_agent_text:0:500}"
fi
if echo "$AGENT_TEXT" | grep -qiE "error|exception"; then
  fail "A2A returned an error-shaped response: $AGENT_TEXT"
fi

# Content assertion — the prompt asks the model to reply with exactly "PONG".
# Real models produce "PONG" (possibly with minor wrapping); a broken pipeline
# that echoes the prompt back or returns truncated context won't. Normalize
# to uppercase before matching to tolerate "pong" / "Pong".
if ! echo "$AGENT_TEXT" | tr '[:lower:]' '[:upper:]' | grep -qF "PONG"; then
  fail "A2A reply didn't contain expected PONG token. Real: $AGENT_TEXT"
fi

ok "A2A parent round-trip succeeded: \"${AGENT_TEXT:0:80}\""

# ─── 8b. Real-completion known-answer round-trip (CORE GATE, #1994) ────
# The existing PONG check + generic error grep above already do a lot, but
# this stanza is the canonical real-completion gate the #1994 follow-on
# adds: a DETERMINISTIC known-answer prompt asserted via
# a2a_assert_real_completion, which FAILS on an error-as-text payload
# ({"kind":"text","text":"Agent error (Exception) ..."}). That payload
# matches the historical shape-only check `"kind":"text"` and so passed CI
# on a fully broken agent (drained-key / byok-misroute, 2026-05-2x). This
# gate makes that case RED. Reuses the same cold-start retry-on-transient
# (502/503/504) loop the PONG probe uses — retry-once-on-network, never on
# agent-error. Single round-trip → the one place we spend a non-trivial
# token budget (default backend MiniMax — cheap token plan).
KA_PAYLOAD=$(python3 -c "
import json, uuid
print(json.dumps({
    'jsonrpc': '2.0',
    'method': 'message/send',
    'id': 'e2e-known-answer-1',
    'params': {
        'message': {
            'role': 'user',
            'messageId': f'e2e-{uuid.uuid4().hex[:8]}',
            'parts': [{'kind': 'text', 'text': 'Reply with exactly the word PINEAPPLE and nothing else.'}]
        }
    }
}))
")
# core#2437 part B: send A2A and poll queue status if the agent queues it.
KA_RESP=$(a2a_send_or_poll_queue "$PARENT_ID" "$KA_PAYLOAD" "A2A known-answer")
a2a_handle_infra_skip "$KA_RESP" "A2A known-answer"
KA_TEXT=$(echo "$KA_RESP" | python3 -c "
import json, sys
def extract_text(d):
    out = []
    # Standard A2A JSON-RPC result.parts
    for p in d.get('result', {}).get('parts', []) or []:
        if p.get('kind') == 'text' or p.get('type') == 'text':
            t = p.get('text') or ''
            if t:
                out.append(t)
    # A2A Task status message parts
    for p in d.get('result', {}).get('status', {}).get('message', {}).get('parts', []) or []:
        if p.get('kind') == 'text' or p.get('type') == 'text':
            t = p.get('text') or ''
            if t:
                out.append(t)
    # Alternative message.parts placement
    for p in d.get('result', {}).get('message', {}).get('parts', []) or []:
        if p.get('kind') == 'text' or p.get('type') == 'text':
            t = p.get('text') or ''
            if t:
                out.append(t)
    # Artifacts
    for a in d.get('result', {}).get('artifacts', []) or []:
        for p in a.get('parts', []) or []:
            if p.get('kind') == 'text' or p.get('type') == 'text':
                t = p.get('text') or ''
                if t:
                    out.append(t)
    return '\n'.join(out)
try:
    d = json.load(sys.stdin)
    print(extract_text(d))
except Exception:
    print('')
" 2>/dev/null || echo "")
# Debuggability: if extraction is empty, surface what the proxy returned so a
# queued/artifact/non-text shape does not silently fail the gate.
if [ -z "$KA_TEXT" ]; then
  KA_SAFE_BODY=$(printf '%s' "$KA_RESP" | sanitize_http_body)
  log "    known-answer A2A extraction empty; response body: $KA_SAFE_BODY"
fi
# CORE GATE: contains PINEAPPLE (real round-trip) AND no error-as-text.
a2a_assert_real_completion "$KA_TEXT" "PINEAPPLE" "A2A known-answer (parent, $RUNTIME/$MODEL_SLUG)"

# ─── 8c retired: the goal is now seeded DETERMINISTICALLY at provision time
# via the MOLECULE_IDLE_GOAL workspace secret (runtime goal-state
# bootstrap_from_env). The prior A2A "call goal_set" seed acked GOALSET twice
# in CI without the tool ever persisting a goal — LLM tool-follow is not a
# gate-grade mechanism. The fire assertion in the wrapper is unchanged.
# Real, deterministic LLM round-trip proven — the load-bearing milestone for
# the fail-closed-on-skip guard. Stamped AFTER a2a_assert_real_completion (not
# after the looser PONG check) so the milestone means a verified completion,
# not just a 2xx-with-text.
live_milestone a2a_roundtrip

# ─── 8c. byok-routing regression guard (#1994) ─────────────────────────
# The parent was provisioned with the customer's OWN vendor key (MINIMAX_API_KEY
# / ANTHROPIC_API_KEY in SECRETS_JSON) → it must route BYOK, never through the
# platform proxy. #1994 was the inverse: a byok workspace baked platform_managed
# on (re-)provision → routed through the platform proxy → drained the platform
# LLM key.
#
# HOW THIS IS GUARDED NOW (rewritten 2026-07-11): the per-workspace
# `llm_billing_mode` override + its GET/PUT /admin/workspaces/:id/llm-billing-mode
# endpoint were DELETED 2026-06-30 (881b3f6f1, internal#718). platform-vs-BYOK is
# no longer a stored, readable mode — it is DERIVED deterministically from the
# model's provider (providers.DeriveProvider → IsPlatform), so the old
# resolved_mode read (GET …/llm-billing-mode) is gone (probing it returned an HTML
# 404 — the red this replaces).
#
# The DISCRIMINATING signal is the PLATFORM CP-proxy usage token,
# MOLECULE_LLM_USAGE_TOKEN: workspace_provision.go injects it into a workspace's
# container env ONLY on the platform route, so a correctly-routed BYOK workspace
# (using its OWN vendor key) MUST NOT carry it; a #1994-regressed one baked
# platform_managed WOULD. We read its env-key PRESENCE (never the value) off
# GET /workspaces/:id via the shared workspace_platform_llm_token_presence probe —
# the same signal test_staging_concierge_creates_workspace uses for the inverse.
# Not tautological with 8b: a misrouted workspace returns a real completion too.
#
# DORMANT UNTIL a06a52eb (core #4042): that env-presence field is NOT yet exposed
# by the workspace API, so the probe returns `unobservable` on the current build
# and this gate is ADVISORY — #1994 has NO live workspace-API gate until a06a52eb
# lands. The structure is correct, so it flips to a HARD gate automatically once
# the field appears. present->fail / absent->ok / unobservable->advisory. Every
# byok arm (MiniMax / Anthropic / OpenAI-hermes) is checked; only the genuine
# platform/no-key path (E2E_LLM_PATH=platform) is legitimately platform_managed.
if [ "${E2E_LLM_PATH:-}" != "platform" ] && { [ -n "${E2E_MINIMAX_API_KEY:-}" ] || [ -n "${E2E_ANTHROPIC_API_KEY:-}" ] || [ -n "${E2E_OPENAI_API_KEY:-}" ]; }; then
  # Mirror the SECRETS_JSON precedence: E2E_LLM_PATH=platform is provisioned
  # platform-managed (SECRETS_JSON='{}') EVEN IF a stray E2E_*_API_KEY leaks into
  # the runner env, so it must NOT enter the byok branch (finding 2). Which byok
  # vendor key did this arm ship? (matches the SECRETS_JSON branch.)
  if [ -n "${E2E_MINIMAX_API_KEY:-}" ]; then _byok_key="MINIMAX_API_KEY"
  elif [ -n "${E2E_ANTHROPIC_API_KEY:-}" ]; then _byok_key="ANTHROPIC_API_KEY"
  else _byok_key="OPENAI_API_KEY"; fi
  _plat_tok=$(workspace_platform_llm_token_presence "$PARENT_ID")
  case "$_plat_tok" in
    absent)
      ok "8c.  byok-routing guard (#1994): byok parent ($RUNTIME/$MODEL_SLUG) carries NO platform CP-proxy token (MOLECULE_LLM_USAGE_TOKEN absent) → routing BYOK on its own '$_byok_key', not the platform proxy. #1994 stays fixed." ;;
    present)
      fail "byok-routing guard TRIPPED (#1994 regression): byok parent $PARENT_ID has the PLATFORM CP-proxy token MOLECULE_LLM_USAGE_TOKEN injected → it is routing through the platform LLM proxy and draining the platform key instead of its own '$_byok_key'. A byok workspace must NOT carry the platform usage token."
      ;;
    *)
      # a06a52eb env-presence not exposed on this build → the probe is DORMANT.
      # Do NOT claim #1994 coverage: 8b's completion does NOT distinguish routing
      # (a proxy-misroute completes too). Advisory until a06a52eb lands (core #4042).
      log "8c.  byok-routing guard DORMANT (advisory) — MOLECULE_LLM_USAGE_TOKEN presence not observable via GET /workspaces/$PARENT_ID (core a06a52eb env-presence not landed; probe='$_plat_tok'). #1994 has NO live gate until a06a52eb (core #4042); NOT a pass."
      ;;
  esac
else
  log "8c.  byok-routing guard skipped — parent carries no own-vendor key (E2E_LLM_PATH=platform / no-key path is legitimately platform_managed)."
fi

# ─── 8d. Per-offered-provider liveness matrix (SSOT-driven, #1994 class) ─
# For each platform-servable model the providers.yaml SSOT
# (runtimes.<runtime>.providers[platform].models) declares for this
# runtime, send a minimal max_tokens-bounded "say ok" probe and assert a
# NON-ERROR completion. Purpose: exercise each offered provider's AUTH +
# ROUTING path so a drained key / wrong base-URL / byok-misroute fails the
# gate (the #1994 class). Providers/models come from the SSOT — not a
# hardcoded list — so the matrix tracks providers.yaml automatically.
#
# This lane provisions ONE parent workspace with ONE configured key, so we
# can only truly drive the providers that key authenticates. Probing a
# model whose provider key is absent in this lane is reported SKIP (rc=75),
# not FAIL — keeping the gate deterministic + low-flake. The matrix still
# proves the configured provider's full auth+routing path end-to-end, and
# logs the offered set so over/under-offer drift is visible in the CI log.
provider_liveness_probe() {
  local model_id="$1"
  # Map the SSOT platform model id (e.g. minimax/MiniMax-M2.7) to the
  # vendor namespace token to decide whether THIS lane has its key.
  local vendor="${model_id%%/*}"
  case "$vendor" in
    minimax)   [ -n "${E2E_MINIMAX_API_KEY:-}" ]   || return 75 ;;
    anthropic) [ -n "${E2E_ANTHROPIC_API_KEY:-}" ] || return 75 ;;
    openai)    [ -n "${E2E_OPENAI_API_KEY:-}" ]    || return 75 ;;
    *)         return 75 ;;  # kimi/moonshot etc. — no key wired in this lane
  esac
  local probe_payload
  probe_payload=$(python3 -c "
import json, uuid
print(json.dumps({
    'jsonrpc': '2.0',
    'method': 'message/send',
    'id': 'e2e-liveness-' + uuid.uuid4().hex[:6],
    'params': {
        'message': {
            'role': 'user',
            'messageId': f'e2e-{uuid.uuid4().hex[:8]}',
            'parts': [{'kind': 'text', 'text': 'Reply with exactly: ok'}],
        },
        'configuration': {'max_tokens': 32}
    }
}))
")
  local tmp code rc resp
  tmp=$(mktemp -t liveness_a2a.XXXXXX)
  set +e
  code=$(tenant_call POST "/workspaces/$PARENT_ID/a2a" \
    --max-time 60 \
    -H "Content-Type: application/json" \
    -d "$probe_payload" \
    -o "$tmp" -w '%{http_code}' 2>/dev/null)
  rc=$?
  set -e
  resp=$(cat "$tmp" 2>/dev/null || echo "")
  rm -f "$tmp"
  if [ "$rc" != "0" ] || [ "${code:-000}" -lt 200 ] || [ "${code:-000}" -ge 300 ]; then
    log "      probe $model_id: HTTP ${code:-000} rc=$rc"
    return 1
  fi
  local text
  text=$(echo "$resp" | python3 -c "
import json,sys
try:
    d=json.load(sys.stdin); p=d.get('result',{}).get('parts',[])
    print(p[0].get('text','') if p else '')
except Exception: print('')" 2>/dev/null || echo "")
  if [ -z "$text" ] || a2a_completion_error_marker "$text" >/dev/null; then
    log "      probe $model_id: error-as-text or empty: ${text:0:120}"
    return 1
  fi
  return 0
}
if ! provider_liveness_matrix "$RUNTIME" provider_liveness_probe; then
  fail "Per-provider liveness matrix: at least one offered provider failed its auth+routing probe (see matrix above). This is the #1994 class — a drained key / wrong base-URL / byok-misroute."
fi
ok "Per-provider liveness matrix passed (all probed offered providers completed without error)"

# ─── 9. HMA + peers + activity (full mode) ─────────────────────────────
if [ "$MODE" = "full" ]; then
  log "9/11 Writing + reading HMA memory on parent..."
  MEM_PAYLOAD=$(python3 -c "
import json
print(json.dumps({
    'content': 'E2E memory seed — run $SLUG',
    'scope': 'LOCAL'
}))
")
  # SURFACE THE BODY (mirrors the step-9b / A2A pattern): the previous
  # `>/dev/null || fail "memory POST failed"` discarded the response body
  # that --fail-with-body deliberately preserves on a non-2xx, so a 500 from
  # the workspace-server HMA path (e.g. "failed to store memory" /
  # "failed to resolve writable namespaces", or a 503 "memory plugin is not
  # configured") was reported as a bare "memory POST failed" — opaque, the
  # same #2310-class blind spot. Route http_code into -w and body into -o,
  # then fail with the sanitized status+body so the mechanism is visible.
  MEM_POST_TMP=$(e2e_tmp /tmp/e2e_mem_post.XXXXXX)
  set +e
  MEM_POST_CODE=$(tenant_call POST "/workspaces/$PARENT_ID/memories" \
    -H "Content-Type: application/json" \
    -d "$MEM_PAYLOAD" \
    -o "$MEM_POST_TMP" -w "%{http_code}" 2>/dev/null)
  MEM_POST_RC=$?
  set -e
  MEM_POST_CODE=${MEM_POST_CODE:-000}
  if [ "$MEM_POST_RC" != "0" ] || [ "$MEM_POST_CODE" -lt 200 ] || [ "$MEM_POST_CODE" -ge 300 ]; then
    MEM_POST_BODY=$(head -c 400 "$MEM_POST_TMP" 2>/dev/null | sanitize_http_body)
    fail "memory POST /workspaces/$PARENT_ID/memories failed (curl_rc=$MEM_POST_RC, http=$MEM_POST_CODE): ${MEM_POST_BODY:-<empty body>}"
  fi

  # Same fail-closed surfacing for the read-back: a 5xx / network error here
  # previously slipped through the bare `$(tenant_call ...)` capture and only
  # showed up as "not readable" with an empty list.
  MEM_LIST_TMP=$(e2e_tmp /tmp/e2e_mem_list.XXXXXX)
  set +e
  MEM_LIST_CODE=$(tenant_call GET "/workspaces/$PARENT_ID/memories?scope=LOCAL" \
    -o "$MEM_LIST_TMP" -w "%{http_code}" 2>/dev/null)
  MEM_LIST_RC=$?
  set -e
  MEM_LIST_CODE=${MEM_LIST_CODE:-000}
  MEM_LIST=$(cat "$MEM_LIST_TMP" 2>/dev/null || echo "")
  if [ "$MEM_LIST_RC" != "0" ] || [ "$MEM_LIST_CODE" -lt 200 ] || [ "$MEM_LIST_CODE" -ge 300 ]; then
    fail "memory GET /workspaces/$PARENT_ID/memories failed (curl_rc=$MEM_LIST_RC, http=$MEM_LIST_CODE): $(printf '%s' "$MEM_LIST" | sanitize_http_body | head -c 400)"
  fi
  if ! echo "$MEM_LIST" | grep -q "run $SLUG"; then
    fail "HMA memory not readable after write (http=$MEM_LIST_CODE). List: $(printf '%s' "$MEM_LIST" | sanitize_http_body | head -c 200)"
  fi
  ok "HMA memory write+read roundtripped"
  # Milestone: HMA memory write+read roundtripped against the live workspace
  # (RFC #4428 Phase 2). Stamped ONLY here, after the hard read-back assertion.
  live_milestone memory_online

  log "9b.  Peer discovery + activity log smoke..."
  # FAIL-CLOSED: assert a real 2xx, not merely "not 404". The previous
  # `[ "$PEERS_CODE" = "404" ] && fail` only caught the route-missing case —
  # a 5xx, 000 (connection failure), or empty capture ALL fell through to
  # "reachable" (false-green: a broken-but-present route read as healthy).
  # Mechanism: route the http_code into its own tempfile (no stderr capture,
  # which the old `2>&1 | head -1` could pollute with a curl error line) and
  # require 2xx explicitly.
  PEERS_TMP=$(e2e_tmp /tmp/e2e_peers.XXXXXX)
  set +e
  PEERS_CODE=$(tenant_call GET "/registry/$PARENT_ID/peers" \
    -o "$PEERS_TMP" -w "%{http_code}" 2>/dev/null)
  PEERS_RC=$?
  set -e
  PEERS_CODE=${PEERS_CODE:-000}
  if [ "$PEERS_CODE" = "404" ]; then
    fail "Peers endpoint missing (404) — route regression. /registry/$PARENT_ID/peers"
  fi
  if [ "$PEERS_RC" != "0" ] || [ "$PEERS_CODE" -lt 200 ] || [ "$PEERS_CODE" -ge 300 ]; then
    fail "Peers endpoint unhealthy (curl_rc=$PEERS_RC, http=$PEERS_CODE) — not a clean 2xx, so 'reachable' would be a false-green. Body: $(head -c 200 "$PEERS_TMP" 2>/dev/null | sanitize_http_body)"
  fi
  ok "Peers endpoint reachable (HTTP $PEERS_CODE)"

  # FAIL-CLOSED: the activity-log read was `|| echo '[]'` then the count was
  # only LOGGED, never asserted — a 5xx / network failure silently became an
  # empty list and the step exited 0 having validated nothing (false-green:
  # "validated nothing" class). Assert the endpoint returns a 2xx and a
  # parseable activity shape. We do NOT assert count>0 (the parent may
  # legitimately have 0 events this early — that's a real, valid state), but
  # we DO require the call to have actually succeeded and returned valid JSON.
  # The workspace id is a PATH param, not a query param: the route is
  # /workspaces/:id/activity (router.go:269 groups wsAuth under /workspaces/:id;
  # ActivityHandler.List reads c.Param("id")). This used to call
  # `/activity?workspace_id=...`, which is not a route AT ALL — it 404s to the
  # canvas SPA and the assertion reported HTML from a JSON endpoint. It would
  # have failed on staging too; nobody ever saw it because step 9 (memory) 503'd
  # and aborted the run BEFORE 9b on every staging pass. The ephemeral gate is
  # the first environment to get past memory (core#4166 bundles the sidecar),
  # so it is the first to actually reach this call.
  ACTIVITY_TMP=$(e2e_tmp /tmp/e2e_activity.XXXXXX)
  set +e
  ACTIVITY_CODE=$(tenant_call GET "/workspaces/$PARENT_ID/activity?limit=5" \
    -o "$ACTIVITY_TMP" -w "%{http_code}" 2>/dev/null)
  ACTIVITY_RC=$?
  set -e
  ACTIVITY_CODE=${ACTIVITY_CODE:-000}
  if [ "$ACTIVITY_RC" != "0" ] || [ "$ACTIVITY_CODE" -lt 200 ] || [ "$ACTIVITY_CODE" -ge 300 ]; then
    fail "Activity-log endpoint unhealthy (curl_rc=$ACTIVITY_RC, http=$ACTIVITY_CODE) — was previously swallowed by '|| echo []' and reported as 0 events (false-green). Body: $(head -c 200 "$ACTIVITY_TMP" 2>/dev/null | sanitize_http_body)"
  fi
  ACTIVITY_COUNT=$(python3 -c "import json,sys
d=json.load(open(sys.argv[1]))
print(len(d if isinstance(d, list) else d.get('events', [])))" "$ACTIVITY_TMP" 2>/dev/null) \
    || fail "Activity-log returned HTTP $ACTIVITY_CODE but body was not parseable JSON (events array / {events:[...]}). Body: $(head -c 200 "$ACTIVITY_TMP" 2>/dev/null | sanitize_http_body)"
  log "    Activity events observed: $ACTIVITY_COUNT (endpoint 2xx + parseable ✓)"

  # ─── 9c. Workspace KV memory Edit round-trip ─────────────────────────
  # Pins the Edit affordance added to the canvas Memory tab. The UI calls
  # POST /workspaces/:id/memory with if_match_version, so the contract is:
  #   1. initial POST creates row at version 1
  #   2. GET returns version 1 + value
  #   3. POST with if_match_version=1 updates → version 2
  #   4. POST with if_match_version=1 again → 409 (optimistic-lock enforcement)
  # Without (3) there is no Edit; without (4) two concurrent writers can
  # silently overwrite each other and the agent loses delegation-ledger state.
  log "9c.  Memory KV Edit round-trip (Edit affordance + 409 gate)"
  EDIT_KEY="e2e_edit_gate_$SLUG"

  # 1. seed
  tenant_call POST "/workspaces/$PARENT_ID/memory" \
    -H "Content-Type: application/json" \
    -d "{\"key\":\"$EDIT_KEY\",\"value\":{\"step\":1}}" >/dev/null \
    || fail "memory KV seed POST failed"

  # 2. read back, capture version
  EDIT_GET=$(tenant_call GET "/workspaces/$PARENT_ID/memory/$EDIT_KEY")
  EDIT_VER=$(echo "$EDIT_GET" | python3 -c "import json,sys; print(json.load(sys.stdin)['version'])" 2>/dev/null || echo "")
  [ -z "$EDIT_VER" ] && fail "memory KV GET missing version field. Body: ${EDIT_GET:0:200}"

  # 3. conditional update with matching version
  tenant_call POST "/workspaces/$PARENT_ID/memory" \
    -H "Content-Type: application/json" \
    -d "{\"key\":\"$EDIT_KEY\",\"value\":{\"step\":2},\"if_match_version\":$EDIT_VER}" >/dev/null \
    || fail "memory KV conditional Edit failed (if_match_version=$EDIT_VER)"

  # 4. value flipped + version incremented?
  EDIT_GET2=$(tenant_call GET "/workspaces/$PARENT_ID/memory/$EDIT_KEY")
  EDIT_VAL2=$(echo "$EDIT_GET2" | python3 -c "import json,sys; print(json.load(sys.stdin)['value'].get('step'))" 2>/dev/null || echo "")
  [ "$EDIT_VAL2" = "2" ] || fail "memory KV Edit did not persist new value. Body: ${EDIT_GET2:0:200}"

  # 5. stale-version POST must 409 — pin the optimistic-lock contract.
  #
  # tenant_call uses CURL_COMMON which carries --fail-with-body, so an
  # expected-409 makes curl exit 22. The previous shape
  #   $(tenant_call ... -w "%{http_code}" || echo "000")
  # concatenated the captured "409" with the fallback "000" giving a
  # bogus "409000" value (caught on PR #2792's first E2E run, which is
  # also why staging-saas E2E has been silent-failing this gate since
  # PR #2787 merged). Fix: route the status code into its own tempfile
  # so curl's exit code can't pollute the captured stdout. set +e/-e
  # keeps the 22 from tripping the outer `set -e` pipeline.
  set +e
  tenant_call POST "/workspaces/$PARENT_ID/memory" \
    -H "Content-Type: application/json" \
    -d "{\"key\":\"$EDIT_KEY\",\"value\":{\"step\":3},\"if_match_version\":$EDIT_VER}" \
    -o /tmp/memory_stale_resp.txt -w "%{http_code}" >/tmp/memory_stale_code.txt 2>/dev/null
  set -e
  EDIT_STALE_CODE=$(cat /tmp/memory_stale_code.txt 2>/dev/null || echo "000")
  [ "$EDIT_STALE_CODE" = "409" ] || fail "memory KV stale Edit must 409 (optimistic-lock). Got '$EDIT_STALE_CODE': $(cat /tmp/memory_stale_resp.txt 2>/dev/null | head -c 200)"

  # cleanup
  tenant_call DELETE "/workspaces/$PARENT_ID/memory/$EDIT_KEY" >/dev/null 2>&1 || true
  ok "Memory KV Edit round-trip + 409 gate passed"

  # ─── 9d. shared_context removal gate ─────────────────────────────────
  # Pin the deletion of GET /workspaces/:id/shared-context. The route + handler
  # were removed; team-shared knowledge now flows through memory v2's
  # team:<id> namespace. If anyone re-introduces a shared-context endpoint
  # without going through RFC #2789, this gate fires.
  SC_BODY=$(mktemp -t shared_context_body.XXXXXX)
  SC_CODE_FILE=$(mktemp -t shared_context_code.XXXXXX)
  E2E_TMP_FILES+=("$SC_BODY" "$SC_CODE_FILE")
  capture_http_status "$SC_CODE_FILE" \
    tenant_call GET "/workspaces/$PARENT_ID/shared-context" \
    -o "$SC_BODY" -w "%{http_code}"
  SC_CODE="$HTTP_CAPTURE_CODE"
  SC_CURL_RC="$HTTP_CAPTURE_RC"
  if ! http_code_is_exact_removed_route "$SC_CODE" "$SC_CURL_RC"; then
    fail "shared-context route removal requires exact HTTP 404 + curl rc 22, got HTTP $SC_CODE + curl rc $SC_CURL_RC: $(head -c 300 "$SC_BODY" | sanitize_http_body)"
  fi
  ok "shared-context route confirmed removed (exact HTTP 404 + curl rc 22)"
  rm -f "$SC_BODY" "$SC_CODE_FILE"
else
  log "9/11 Canary mode — skipping HMA / peers / activity / memory-edit / shared-context-gone"
fi

# ─── 10. Delegation mechanics (full mode + child) ──────────────────────
if [ "$MODE" = "full" ] && [ -n "$CHILD_ID" ]; then
  log "10/11 Delegation mechanics: parent → child via proxy"
  DELEG_PAYLOAD=$(python3 -c "
import json, uuid
print(json.dumps({
    'jsonrpc': '2.0',
    'method': 'message/send',
    'id': 'e2e-deleg-1',
    'params': {
        'message': {
            'role': 'user',
            'messageId': f'e2e-deleg-{uuid.uuid4().hex[:8]}',
            'parts': [{'kind': 'text', 'text': 'This is the platform smoke test verifying child workspace wiring. No tools or memory are needed — please respond with exactly the single token: CHILD_PONG'}]
        }
    }
}))
")
  # Caller identity. The proxy reads exactly ONE header for this
  # (a2a_proxy.go: `callerID := c.GetHeader("X-Workspace-ID")`), and callerID is
  # what becomes activity_logs.source_id (callerIDToSourceID). This step used to
  # send `X-Source-Workspace-Id`, which NO Go code reads — grep the tree for it
  # and you get zero hits. So callerID was "", source_id was NULL, and "child
  # records parent as source" could never be true. It never failed because the
  # assertion below was soft-logged; fail-closing it is what finally surfaced it.
  #
  # Mint the PARENT's OWN workspace token instead of reusing the tenant admin
  # token. With an admin/org bearer the proxy classifies the call as a canvas
  # user (isGenuineCanvasUser) and BYPASSES both CanCommunicate and the
  # can_delegate gate — the step would go green without ever exercising the
  # peer-auth path it claims to test. With the parent's own token the real
  # agent→agent contract runs end to end: the token must bind to callerID
  # (validateCallerToken), CanCommunicate(parent→child) must allow it, same-org
  # must hold, and can_delegate must be true.
  PT_TMP=$(e2e_tmp /tmp/e2e_parent_tok.XXXXXX)
  set +e
  PT_CODE=$(tenant_call POST "/admin/workspaces/$PARENT_ID/tokens" \
    -o "$PT_TMP" -w '%{http_code}' 2>/dev/null)
  set -e
  PT_CODE=${PT_CODE:-000}
  PARENT_WS_TOKEN=$(python3 -c "
import json, sys
try:
    print(json.load(open(sys.argv[1])).get('auth_token', ''))
except Exception:
    print('')
" "$PT_TMP" 2>/dev/null || echo "")
  rm -f "$PT_TMP"
  if [ -z "$PARENT_WS_TOKEN" ]; then
    fail "Could not mint the parent workspace's auth token (POST /admin/workspaces/\$PARENT_ID/tokens http=$PT_CODE). Without it the delegation call can only run as an admin/canvas caller, which bypasses CanCommunicate + can_delegate and would fake-pass this step."
  fi

  DELEG_TMP=$(mktemp -t deleg_a2a.XXXXXX)
  for DELEG_ATTEMPT in $(seq 1 12); do
    : >"$DELEG_TMP"
    set +e
    # Raw curl (not tenant_call) because this call authenticates as the PARENT
    # workspace, not as the tenant admin. Must still send X-Molecule-Org-Id
    # or TenantGuard 404s — previously missing, caused section 10 to
    # fail rc=22 despite everything upstream being correct (2026-04-21).
    DELEG_CODE=$(curl "${CURL_COMMON[@]}" -X POST "$TENANT_URL/workspaces/$CHILD_ID/a2a" \
      -H "Authorization: Bearer $PARENT_WS_TOKEN" \
      -H "X-Molecule-Org-Id: $ORG_ID" \
      "${TENANT_ROUTE_HDRS[@]}" \
      -H "X-Workspace-ID: $PARENT_ID" \
      -H "Content-Type: application/json" \
      -d "$DELEG_PAYLOAD" \
      -o "$DELEG_TMP" \
      -w '%{http_code}' \
      2>/dev/null)
    DELEG_RC=$?
    set -e
    DELEG_CODE=${DELEG_CODE:-000}
    DELEG_RESP=$(cat "$DELEG_TMP" 2>/dev/null || echo "")
    if [ "$DELEG_RC" = "0" ] && [ "$DELEG_CODE" -ge 200 ] && [ "$DELEG_CODE" -lt 300 ]; then
      break
    fi

    DELEG_SAFE_BODY=$(printf '%s' "$DELEG_RESP" | sanitize_http_body)
    if echo "$DELEG_CODE" | grep -Eq '^(502|503|504)$' && echo "$DELEG_SAFE_BODY" | grep -Eqi 'Service Unavailable|Bad Gateway|Gateway Timeout|error code: 502|error code: 504|workspace agent unreachable|connection refused|no healthy upstream|workspace agent busy|native_session'; then
      log "    Delegation A2A cold-start attempt $DELEG_ATTEMPT/12 returned $DELEG_CODE: $DELEG_SAFE_BODY"
      if [ "$DELEG_ATTEMPT" -lt 12 ]; then
        DELEG_SLEEP=10
        if echo "$DELEG_SAFE_BODY" | grep -Eqi 'workspace agent busy|native_session'; then
          DELEG_SLEEP=30
        fi
        sleep "$DELEG_SLEEP"
        continue
      fi
    fi
    break
  done
  rm -f "$DELEG_TMP"
  if [ "$DELEG_RC" != "0" ] || [ "$DELEG_CODE" -lt 200 ] || [ "$DELEG_CODE" -ge 300 ]; then
    DELEG_SAFE_BODY=$(printf '%s' "$DELEG_RESP" | sanitize_http_body)
    fail "Delegation A2A POST failed after $DELEG_ATTEMPT attempt(s) (curl_rc=$DELEG_RC, http=$DELEG_CODE): $DELEG_SAFE_BODY"
  fi
  DELEG_TEXT=$(echo "$DELEG_RESP" | python3 -c "
import json, sys
try:
    d = json.load(sys.stdin)
    parts = d.get('result', {}).get('parts', [])
    print(parts[0].get('text', '') if parts else '')
except Exception:
    print('')
" 2>/dev/null || echo "")
  [ -z "$DELEG_TEXT" ] && fail "Delegation returned no text. Raw: ${DELEG_RESP:0:200}"
  ok "Delegation proxy works (child responded: \"${DELEG_TEXT:0:60}\")"

  # FAIL-CLOSED via bounded readiness-POLL (was soft-logged false-green).
  # The activity pipeline is async, so an immediate single read can miss the
  # parent reference — but "did not reference parent" was previously just
  # LOGGED and the step passed regardless, so a genuinely broken provenance
  # pipeline (parent never recorded as source) read as success. Mechanism:
  # poll the child activity log for the parent id for a bounded window
  # (E2E_CHILD_ACTIVITY_TIMEOUT_SECS, default 60s) — this is the real
  # readiness signal (provenance row materialised), not a fixed sleep — and
  # hard-fail with a named mechanism if it never appears.
  CHILD_ACT_DEADLINE=$(( $(date +%s) + ${E2E_CHILD_ACTIVITY_TIMEOUT_SECS:-60} ))
  CHILD_ACT_SEEN=0
  CHILD_ACT_LASTCODE="000"
  while true; do
    CHILD_ACT_TMP=$(e2e_tmp /tmp/e2e_child_act.XXXXXX)
    set +e
    # Same bug as 9b: the workspace id is a PATH param. `/activity?workspace_id=`
    # is not a registered route — it 404s to the canvas SPA, whose HTML never
    # contains $PARENT_ID, so this loop polled a 404 for the full 60s and then
    # hard-failed with "delegation-provenance pipeline regression". The pipeline
    # was fine; the URL was not.
    CHILD_ACT_CODE=$(tenant_call GET "/workspaces/$CHILD_ID/activity?limit=20" \
      -o "$CHILD_ACT_TMP" -w "%{http_code}" 2>/dev/null)
    set -e
    CHILD_ACT_LASTCODE=${CHILD_ACT_CODE:-000}
    if grep -q "$PARENT_ID" "$CHILD_ACT_TMP" 2>/dev/null; then
      CHILD_ACT_SEEN=1
      break
    fi
    [ "$(date +%s)" -ge "$CHILD_ACT_DEADLINE" ] && break
    sleep 5
  done
  if [ "$CHILD_ACT_SEEN" = "1" ]; then
    ok "Child activity log records parent as source"
    # Milestone: delegation provenance proven — the child's activity log records
    # the parent as source_id after a real agent→agent A2A (RFC #4428 Phase 2).
    # Stamped ONLY on the hard-asserted success branch (the else branch fails).
    live_milestone delegation_provenance
  else
    fail "Child activity log never referenced parent $PARENT_ID within ${E2E_CHILD_ACTIVITY_TIMEOUT_SECS:-60}s (last http=$CHILD_ACT_LASTCODE) — delegation-provenance pipeline regression (parent not recorded as source). Previously soft-logged → false-green."
  fi
fi

# ─── 10b. Pause/Resume + Hibernate/Resume lifecycle transitions ─────────
# Exercise the REAL workspace lifecycle state machine on the provisioned
# parent — the transitions that previously had only handler unit tests
# (handlers_additional_test.go / hibernation_test.go) and NO real-infra
# coverage. Each transition is asserted against the live DB-backed status the
# GET /workspaces/:id endpoint returns, so a regression in the Pause/Resume/
# Hibernate handlers (workspace_restart.go) or their CP stop/re-provision
# wiring fails the gate instead of leaving runtime resources or wedging a tenant.
#
# Contract (workspace_restart.go):
#   POST /pause     online → 'paused'  (container stopped, url cleared)  {"status":"paused"}
#   POST /resume    paused → 'provisioning' → … → 'online' (re-provision) {"status":"provisioning"}
#   POST /hibernate online → 'hibernating' → 'hibernated' (container stopped) {"status":"hibernated"}
#   auto-wake       next A2A message/send on a hibernated ws → online
#
# Gated to full MODE (smoke has no parent-stability budget) + E2E_LIFECYCLE.
# Runs LAST (after all read-only A2A/memory/peer checks) so the pause/stop
# cycles don't disturb the earlier assertions. Skips are LOUD (logged), and
# any broken transition hard-fails — never a silent pass.
if [ "$MODE" = "full" ] && [ "${E2E_LIFECYCLE:-auto}" != "off" ]; then
  # Run the lifecycle on a LEAF workspace, not the parent.
  #
  # Pause and Resume refuse to act on a workspace with live descendants unless
  # ?cascade=true (workspace_restart.go: 409 + the descendant list) — a deliberate
  # guard, so pausing a parent cannot silently orphan its running children. In
  # full mode the parent ALWAYS has a child (step 6 creates one), so "pause the
  # parent" could never have succeeded: this step was dead on arrival. Nobody saw
  # it for the same reason nobody saw the dead /activity route — staging aborted
  # at step 9 (memory) before ever reaching 10b, on every run.
  #
  # The child is a leaf, so it exercises the SAME state machine (pause → resume →
  # hibernate → auto-wake) with no cascade semantics in the way. The cascade guard
  # itself is pinned by its own negative assertion below — strictly more coverage
  # than this step has ever had.
  # CHILD_ID is guaranteed non-empty here: 10b is full-mode-only, and full mode
  # hard-fails at step 6 if the child create returned no id. No fallback — a
  # `${CHILD_ID:-$LIFECYCLE_WS}` self-reference would abort under `set -u`, and a
  # `${CHILD_ID:-$PARENT_ID}` fallback would silently pause the PARENT, which is
  # the exact thing this step must never do.
  LIFECYCLE_WS="$CHILD_ID"
  log "10b/11 Lifecycle transitions: pause→resume→online, hibernate→resume(wake) on leaf $LIFECYCLE_WS..."

  # ── negative gate: the cascade guard must REFUSE to pause a parent with a live
  # child. If it ever regressed, a parent pause would strand its running children
  # and nothing else in this suite would notice.
  if [ -n "$CHILD_ID" ]; then
    CASC_TMP=$(e2e_tmp /tmp/e2e_cascade.XXXXXX)
    set +e
    CASC_CODE=$(tenant_call POST "/workspaces/$PARENT_ID/pause" \
      -o "$CASC_TMP" -w '%{http_code}' 2>/dev/null)
    set -e
    CASC_CODE=${CASC_CODE:-000}
    CASC_BODY=$(cat "$CASC_TMP" 2>/dev/null || echo "")
    rm -f "$CASC_TMP"
    if [ "$CASC_CODE" != "409" ] || ! printf '%s' "$CASC_BODY" | grep -q "descendants"; then
      fail "Cascade guard: POST /workspaces/\$PARENT_ID/pause (no ?cascade) must be REFUSED with 409 + the descendant list while child $CHILD_ID is live — got http=$CASC_CODE. A regression here lets a parent pause strand its running children. Body: $(printf '%s' "$CASC_BODY" | sanitize_http_body | head -c 200)"
    fi
    ok "    cascade guard: parent pause refused while a child is live (HTTP 409)"
    # Milestone: cascade guard proven — a parent pause with a live child is
    # REFUSED 409 + descendant list (RFC #4428 Phase 2). Stamped inside the
    # CHILD_ID branch, after the hard 409+"descendants" assertion.
    live_milestone cascade_guard
  fi

  lifecycle_status() {  # echoes the live workspace status
    tenant_call GET "/workspaces/$LIFECYCLE_WS" 2>/dev/null \
      | python3 -c "import json,sys; print(json.load(sys.stdin).get('status') or '')" 2>/dev/null || echo ""
  }
  # Bounded readiness-poll for a target status — same fail-closed shape as
  # wait_workspaces_online_routable, but for an arbitrary terminal status.
  wait_status() {  # $1=target $2=timeout_secs $3=label
    local target="$1" timeout="$2" label="$3"
    local deadline cur last=""
    deadline=$(( $(date +%s) + timeout ))
    while true; do
      cur=$(lifecycle_status)
      if [ "$cur" != "$last" ]; then log "    leaf status → ${cur:-<empty>}"; last="$cur"; fi
      [ "$cur" = "$target" ] && return 0
      if [ "$(date +%s)" -gt "$deadline" ]; then
        log "    [lifecycle] $label never reached '$target' within ${timeout}s (last='$cur')"
        return 1
      fi
      sleep 10
    done
  }

  # ── pause → paused ──
  PAUSE_RESP=$(tenant_call POST "/workspaces/$LIFECYCLE_WS/pause" 2>/dev/null || echo '{}')
  PAUSE_STATUS=$(echo "$PAUSE_RESP" | python3 -c "import json,sys; print(json.load(sys.stdin).get('status',''))" 2>/dev/null || echo "")
  [ "$PAUSE_STATUS" = "paused" ] || fail "Pause: POST /pause returned status='$PAUSE_STATUS' (expected 'paused'). Body: ${PAUSE_RESP:0:200}"
  # Poll the DB-backed status — the response body could lie; the GET proves the row.
  wait_status "paused" 120 "pause" || fail "Pause: workspace $LIFECYCLE_WS never settled at status=paused (DB row) — Pause handler / CP stop regression (workspace_restart.go Pause)."
  ok "    pause → paused (DB-verified)"

  # ── resume → provisioning → online ──
  RESUME_RESP=$(tenant_call POST "/workspaces/$LIFECYCLE_WS/resume" 2>/dev/null || echo '{}')
  RESUME_STATUS=$(echo "$RESUME_RESP" | python3 -c "import json,sys; print(json.load(sys.stdin).get('status',''))" 2>/dev/null || echo "")
  [ "$RESUME_STATUS" = "provisioning" ] || fail "Resume: POST /resume returned status='$RESUME_STATUS' (expected 'provisioning'). Body: ${RESUME_RESP:0:200}"
  # Resume re-provisions from the preserved config volume; reuse the same
  # online+routable readiness boundary the initial boot used; CP re-provision +
  # heartbeat recovery can still take minutes.
  wait_workspaces_online_routable "    Waiting for the leaf workspace to return online after resume (up to $((WORKSPACE_ONLINE_TIMEOUT_SECS/60)) min)..." "$LIFECYCLE_WS"
  ok "    resume → provisioning → online (DB-verified)"
  # Milestone: pause→resume lifecycle proven — leaf DB-verified paused, then
  # resumed back to online/routable (RFC #4428 Phase 2). Stamped after both the
  # pause and resume DB-verified assertions above complete.
  live_milestone lifecycle_pause_resume

  # ── hibernate (force coverage: core#4292/#4293) ──
  # force-hibernate only MEANS anything on a BUSY workspace. On an idle one, the
  # atomic claim's force-path (which DROPS the `active_tasks = 0` predicate) is
  # indistinguishable from the non-force path, so an idle hibernate cannot tell a
  # working `force` from a broken one. core#4292 slipped this very gate for exactly
  # that reason: 10b hibernated an IDLE leaf, `?force=true` answered 200 while the
  # atomic claim (still carrying `AND active_tasks = 0`) matched no row and stopped
  # nothing — the workspace stayed online and billing. #4293 fixed the claim; this
  # step now PROVES the fix by driving a genuinely busy workspace first.
  #
  # active_tasks is heartbeat-fed (the agent self-reports its in-flight turn count
  # every ~30s). Two ways to make the workspace busy, selected by E2E_BUSY_INJECT:
  #
  #   * E2E_BUSY_INJECT=1 (the ephemeral gate — task #92): the tenant image is
  #     built with `-tags e2e_busy_inject`, which exposes POST
  #     /workspaces/:id/test-busy. It pins active_tasks to a floor synchronously
  #     (and holds it across heartbeats), so busy is DETERMINISTIC without a real
  #     LLM turn. This route exists ONLY in the tag-built throwaway tenant image;
  #     it is absent (404) from every shipped/staging binary. A non-busy 10b under
  #     this mode is a hard red (see E2E_HIBERNATE_FORCE_BUSY_REQUIRED below).
  #
  #   * default (staging lanes, untagged images): drive a real long turn and poll
  #     until the DB reflects it — best-effort by nature. CAPTURE-FIRST /
  #     SOAK-TO-ENFORCE: when provably busy we run the strict force-coverage
  #     assertions (a)+(b); otherwise we LOG LOUD and fall back to the idle
  #     force-hibernate (a valid transition, not force-coverage) unless
  #     E2E_HIBERNATE_FORCE_BUSY_REQUIRED=1 makes a failed drive fatal.

  # Deterministic busy via the test-only inject route (E2E_BUSY_INJECT=1). Pins
  # active_tasks=1 in the same DB column the hibernate 409 guard + atomic claim
  # read, then confirms GET reflects it. Returns 0 iff active_tasks>0 was
  # established. A 404 here means the tenant image was NOT built with the
  # e2e_busy_inject tag — a wiring error the operator must fix, not a soft skip.
  _hib_busy_inject() {
    local bi_tmp bi_code cur deadline
    bi_tmp=$(mktemp -t hib_inject.XXXXXX)
    set +e
    bi_code=$(tenant_call POST "/workspaces/$LIFECYCLE_WS/test-busy" --max-time 20 \
      -H "Content-Type: application/json" -d '{"active_tasks":1,"is_busy":true}' \
      -o "$bi_tmp" -w '%{http_code}' 2>/dev/null)
    set -e
    bi_code=${bi_code:-000}
    if [ "$bi_code" != "200" ]; then
      log "    busy-inject: POST /test-busy → http=$bi_code (expected 200). If 404, the tenant image lacks -tags e2e_busy_inject. Body: $(cat "$bi_tmp" 2>/dev/null | sanitize_http_body | head -c 200)"
      rm -f "$bi_tmp"
      return 1
    fi
    rm -f "$bi_tmp"
    # Confirm the DB reflects it. The sticky floor keeps active_tasks>=1 across
    # heartbeats, so a single confirming read is sufficient and stable.
    deadline=$(( $(date +%s) + 30 ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
      cur=$(tenant_call GET "/workspaces/$LIFECYCLE_WS" 2>/dev/null \
        | python3 -c "import json,sys; print(int(json.load(sys.stdin).get('active_tasks') or 0))" 2>/dev/null || echo 0)
      log "    busy-inject poll: active_tasks=${cur:-?} (want >0)"
      if [ "${cur:-0}" -gt 0 ] 2>/dev/null; then
        log "    active_tasks=$cur — workspace is busy via test-inject (deterministic)"
        return 0
      fi
      sleep 3
    done
    return 1
  }

  # Release a previously-injected hold so post-wake heartbeats stop reporting busy
  # (the sticky floor would otherwise re-raise active_tasks after the workspace
  # wakes). No-op / harmless when the route is absent.
  _hib_busy_release() {
    tenant_call POST "/workspaces/$LIFECYCLE_WS/test-busy" --max-time 20 \
      -H "Content-Type: application/json" -d '{"active_tasks":0}' \
      -o /dev/null -w '' 2>/dev/null || true
    log "    busy-inject: released active_tasks hold"
  }

  # Drive a long, model-independent in-flight turn WITHOUT draining the queue, so
  # the turn stays live while we fire the two hibernate calls. Returns 0 once
  # GET /workspaces/:id reports active_tasks>0 (the same column the handler's 409
  # guard and atomic claim read — so the witness and the decision agree).
  _hib_busy_drive() {
    # Deterministic path first when the gate armed it.
    if [ "${E2E_BUSY_INJECT:-0}" = "1" ]; then
      _hib_busy_inject && return 0
      return 1
    fi
    local sleep_secs="${E2E_HIBERNATE_BUSY_SLEEP_SECS:-180}"
    local wait_secs="${E2E_HIBERNATE_BUSY_WAIT_SECS:-150}"
    local busy_payload deadline cur bd_tmp bd_code bd_qid bd_attempt
    busy_payload=$(python3 -c "
import json, uuid
secs = ${sleep_secs}
print(json.dumps({
    'jsonrpc': '2.0', 'method': 'message/send', 'id': 'e2e-busy-1',
    'params': {'message': {'role': 'user',
        'messageId': f'e2e-busy-{uuid.uuid4().hex[:8]}',
        'parts': [{'kind': 'text', 'text':
            'Platform lifecycle test. Using your shell/bash tool, run exactly this one '
            f'command and wait for it to finish: sleep {secs}. Then reply with the single '
            'token BUSYDONE. Do nothing else.'}]}}}))
")
    # Send the long turn, retrying the transient post-resume 5xx ("agent
    # restarting" / "waking") the same way the wake path does — the leaf has just
    # come back from resume, so its agent may still be booting. Do NOT drain the
    # queue: we want the turn left in flight. Log the send outcome (queue_id) so a
    # soak run shows whether the turn was even accepted.
    bd_tmp=$(mktemp -t hib_busy.XXXXXX)
    for bd_attempt in $(seq 1 8); do
      : >"$bd_tmp"
      # CURL_COMMON carries --fail-with-body, so curl EXITS NON-ZERO on 4xx/5xx
      # while still writing the code via -w. A trailing `|| echo 000` would
      # CONCATENATE onto that code ("503" -> "503000"), breaking the numeric and
      # regex checks below. This function is called in `if` context so errexit is
      # ignored throughout its body — capture the code directly, no `||`.
      bd_code=$(tenant_call POST "/workspaces/$LIFECYCLE_WS/a2a" --max-time 30 \
        -H "Content-Type: application/json" \
        -d "$busy_payload" -o "$bd_tmp" -w '%{http_code}' 2>/dev/null)
      bd_code=${bd_code:-000}
      if [ "$bd_code" -ge 200 ] 2>/dev/null && [ "$bd_code" -lt 300 ] 2>/dev/null; then
        bd_qid=$(python3 -c "import json,sys; d=json.load(open('$bd_tmp')); print(d.get('queue_id') or d.get('id') or '')" 2>/dev/null || echo "")
        log "    busy-drive A2A accepted (http=$bd_code queue_id=${bd_qid:-<none>}) — long turn in flight"
        break
      fi
      if echo "$bd_code" | grep -Eq '^(502|503|504)$' && [ "$bd_attempt" -lt 8 ]; then
        log "    busy-drive A2A attempt $bd_attempt/8: http=$bd_code (agent restarting) — backing off 15s"
        sleep 15
        continue
      fi
      log "    busy-drive A2A send got http=$bd_code (non-retryable): $(cat "$bd_tmp" 2>/dev/null | sanitize_http_body | head -c 160)"
      break
    done
    rm -f "$bd_tmp"
    # Poll active_tasks over several heartbeats (~30s cadence). Log EACH reading so
    # a soak run shows whether the count ever leaves 0 (distinguishes "agent never
    # ran the turn" from "turn ran but active_tasks not reported / too brief").
    deadline=$(( $(date +%s) + wait_secs ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
      cur=$(tenant_call GET "/workspaces/$LIFECYCLE_WS" 2>/dev/null \
        | python3 -c "import json,sys; print(int(json.load(sys.stdin).get('active_tasks') or 0))" 2>/dev/null || echo 0)
      log "    busy-drive poll: active_tasks=${cur:-?} (want >0)"
      if [ "${cur:-0}" -gt 0 ] 2>/dev/null; then
        log "    active_tasks=$cur — workspace is genuinely busy"
        return 0
      fi
      sleep 10
    done
    return 1
  }

  if _hib_busy_drive; then
    # (a) non-force hibernate on a BUSY workspace → 409, echoing the live count.
    #     CURL_COMMON carries --fail-with-body → curl EXITS NON-ZERO on the
    #     expected 409, still writing the code via -w. `|| echo 000` would
    #     concatenate ("409" -> "409000") and defeat the 409 check, so capture
    #     under a local `set +e` (this runs in the then-branch, errexit is live).
    _NF_TMP=$(mktemp -t hib_nf.XXXXXX)
    set +e
    _NF_CODE=$(tenant_call POST "/workspaces/$LIFECYCLE_WS/hibernate" --max-time 30 \
      -o "$_NF_TMP" -w '%{http_code}' 2>/dev/null)
    set -e
    _NF_CODE=${_NF_CODE:-000}
    _NF_AT=$(python3 -c "import json,sys; print(int(json.load(open('$_NF_TMP')).get('active_tasks') or 0))" 2>/dev/null || echo 0)
    [ "$_NF_CODE" = "409" ] || fail "Hibernate(busy): non-force POST /hibernate returned http=$_NF_CODE (expected 409 active-tasks conflict). A busy workspace must refuse a non-force hibernate. Body: $(cat "$_NF_TMP" 2>/dev/null | sanitize_http_body | head -c 200)"
    rm -f "$_NF_TMP"
    # The 409 CODE is the hard assertion (busyness is already proven by the
    # active_tasks>0 poll that gated entry to this branch). The count in the
    # conflict body is corroborating, not contractual — log it, don't couple
    # a red to its exact shape.
    if [ "${_NF_AT:-0}" -gt 0 ] 2>/dev/null; then
      ok "    non-force hibernate on BUSY ws → 409 (active_tasks=$_NF_AT)"
    else
      ok "    non-force hibernate on BUSY ws → 409 (body active-task count absent/0; code is authoritative)"
    fi

    # (b) force hibernate on the SAME busy workspace → must terminate the in-flight
    #     task AND land 'hibernated' in the DB. This is the exact #4293 assertion:
    #     an idle-only test can never reach it.
    HIB_RESP=$(tenant_call POST "/workspaces/$LIFECYCLE_WS/hibernate?force=true" 2>/dev/null || echo '{}')
    HIB_STATUS=$(echo "$HIB_RESP" | python3 -c "import json,sys; print(json.load(sys.stdin).get('status',''))" 2>/dev/null || echo "")
    [ "$HIB_STATUS" = "hibernated" ] || fail "Hibernate(busy,force): POST /hibernate?force=true returned status='$HIB_STATUS' (expected 'hibernated'). Body: ${HIB_RESP:0:200}"
    wait_status "hibernated" 120 "hibernate-force-busy" || fail "Hibernate(busy,force): workspace $LIFECYCLE_WS never settled at status=hibernated (DB row) after force on a BUSY workspace — core#4293 regression: force must drop the active_tasks=0 claim predicate, terminate the in-flight task, and stop the container, not no-op to a lying 200."
    ok "    force hibernate on BUSY ws → hibernated (DB-verified; core#4293 covered)"
    # Release any injected busy hold now that the force-on-busy assertions passed,
    # so the workspace can wake IDLE below (the sticky floor would otherwise
    # re-raise active_tasks on the post-wake heartbeats). No-op under the
    # real-turn drive (the route is absent / the hold was never set).
    if [ "${E2E_BUSY_INJECT:-0}" = "1" ]; then _hib_busy_release; fi
  else
    # Busy-drive did not establish active_tasks>0 within the bound. Under the soak
    # default this is a LOUD non-fatal fallback (the run still exercises the idle
    # transition); flip E2E_HIBERNATE_FORCE_BUSY_REQUIRED=1 once the drive is proven
    # reliable to make this a hard red instead.
    if [ "${E2E_HIBERNATE_FORCE_BUSY_REQUIRED:-0}" = "1" ]; then
      fail "Hibernate(force coverage): could not drive active_tasks>0 within ${E2E_HIBERNATE_BUSY_WAIT_SECS:-120}s (runtime never self-reported an in-flight turn). With E2E_HIBERNATE_FORCE_BUSY_REQUIRED=1 this is fatal — the gate must genuinely exercise force on a busy workspace (task #92)."
    fi
    log "    WARN[task#92 soak]: could not establish active_tasks>0 within the bound (runtime busy-report timing, or the agent did not run the sleep turn). Falling back to IDLE force-hibernate — this run does NOT cover the force-on-busy class (core#4293)."
    HIB_RESP=$(tenant_call POST "/workspaces/$LIFECYCLE_WS/hibernate?force=true" 2>/dev/null || echo '{}')
    HIB_STATUS=$(echo "$HIB_RESP" | python3 -c "import json,sys; print(json.load(sys.stdin).get('status',''))" 2>/dev/null || echo "")
    [ "$HIB_STATUS" = "hibernated" ] || fail "Hibernate(idle fallback): POST /hibernate?force=true returned status='$HIB_STATUS' (expected 'hibernated'). Body: ${HIB_RESP:0:200}"
    # Even the idle path polls the DB row — the 200 alone was the #4292 lie.
    wait_status "hibernated" 120 "hibernate" || fail "Hibernate(idle fallback): workspace $LIFECYCLE_WS never settled at status=hibernated (DB row) even though POST /hibernate?force=true answered '$HIB_STATUS' — suspect a no-op hibernate (atomic claim matched no row) before suspecting the container stop."
    ok "    hibernate → hibernated (DB-verified; idle fallback, force-on-busy NOT covered this run)"
  fi

  # ── resume-from-hibernate via auto-wake on next A2A ──
  # A hibernated workspace auto-wakes on the next incoming A2A message/send
  # (no explicit /resume — Resume only handles status=paused). Send a wake
  # A2A and assert the workspace returns to online. We accept transient cold
  # 5xx during wake (same edge class the PONG probe tolerates) and poll the
  # status to the online boundary rather than asserting on the single A2A code.
  log "    Hibernate auto-wake: sending A2A to wake the hibernated leaf..."
  WAKE_PAYLOAD=$(python3 -c "
import json, uuid
print(json.dumps({
    'jsonrpc': '2.0',
    'method': 'message/send',
    'id': 'e2e-wake-1',
    'params': {
        'message': {
            'role': 'user',
            'messageId': f'e2e-wake-{uuid.uuid4().hex[:8]}',
            'parts': [{'kind': 'text', 'text': 'This is the platform lifecycle smoke test waking a hibernated workspace. No tools or memory are needed — please respond with exactly the single token: WOKE'}]
        }
    }
}))
")
  WAKE_TMP=$(mktemp -t wake_a2a.XXXXXX)
  for WAKE_ATTEMPT in $(seq 1 12); do
    : >"$WAKE_TMP"
    set +e
    WAKE_CODE=$(tenant_call POST "/workspaces/$LIFECYCLE_WS/a2a" \
      --max-time 90 \
      -H "Content-Type: application/json" \
      -d "$WAKE_PAYLOAD" \
      -o "$WAKE_TMP" -w '%{http_code}' 2>/dev/null)
    WAKE_RC=$?
    set -e
    WAKE_CODE=${WAKE_CODE:-000}
    if [ "$WAKE_RC" = "0" ] && [ "$WAKE_CODE" -ge 200 ] && [ "$WAKE_CODE" -lt 300 ]; then
      break
    fi
    WAKE_SAFE_BODY=$(cat "$WAKE_TMP" 2>/dev/null | sanitize_http_body)
    # Wake legitimately returns transient 5xx while the container restarts —
    # retry that class only (bounded), never a 4xx.
    if echo "$WAKE_CODE" | grep -Eq '^(502|503|504)$' && [ "$WAKE_ATTEMPT" -lt 12 ]; then
      log "    wake A2A cold/restart attempt $WAKE_ATTEMPT/12 returned $WAKE_CODE: ${WAKE_SAFE_BODY:0:120}"
      sleep 15
      continue
    fi
    break
  done
  rm -f "$WAKE_TMP"
  # The auto-wake contract is the STATUS transition (hibernated → online), not
  # the A2A body content — assert the live DB row, the real readiness signal.
  wait_status "online" "$WORKSPACE_ONLINE_TIMEOUT_SECS" "hibernate-wake" \
    || fail "Hibernate auto-wake: workspace $LIFECYCLE_WS never returned to status=online after a wake A2A (last A2A http=$WAKE_CODE) — auto-wake-on-message regression (a hibernated ws must re-provision on the next A2A)."
  ok "    hibernate → online via auto-wake A2A (DB-verified)"
  # Milestone: idle force-hibernate → auto-wake → online proven, DB-verified
  # (RFC #4428 Phase 2). Stamped at the SHARED auto-wake success — reached by
  # both the busy and idle-fallback hibernate branches — so it is NOT contingent
  # on the best-effort busy-drive (the force-on-busy #4293 coverage is task #92,
  # capture-first, and deliberately NOT gated to this milestone).
  live_milestone lifecycle_hibernate_wake
  ok "Lifecycle transitions passed: pause→resume→online + hibernate→wake→online"
else
  log "10b/11 Lifecycle transitions skipped (MODE=$MODE, E2E_LIFECYCLE=${E2E_LIFECYCLE:-auto}) — pause/resume/hibernate only run in full mode with E2E_LIFECYCLE!=off."
fi

# ─── 10c. Idle-digest poll (task #219, E2E_IDLE_DIGEST_CHECK=on only) ──
# Poll while the workspaces are alive until the digest has actually completed.
# The runtime posts the digest through a real synchronous A2A/LLM call; that
# completion can legitimately outlive a fixed `fire_seconds * N` sleep. Durable
# goal-state (`last_included_at`) is the authoritative witness across container
# replacement, with the current container log retained as a second signal.
if [ "${E2E_IDLE_DIGEST_CHECK:-}" = "on" ]; then
  _IDLE_TIMEOUT_SECS="${E2E_IDLE_DIGEST_TIMEOUT_SECS:-360}"
  _IDLE_POLL_SECS="${E2E_IDLE_DIGEST_POLL_SECS:-10}"

  # The enabled assertion requires the ephemeral topology's shared Docker
  # daemon. A missing CLI/socket is a broken gate, not permission to skip it.
  if idle_digest_require_docker; then
    _ID_FIRED=""

    idle_digest_probe() {
      local _idc
      _ID_FIRED=""
      while IFS= read -r _idc; do
        [ -n "$_idc" ] || continue
        # Container logs are useful when the same container survives. Durable
        # goal state survives CP-driven replacement under the same name and is
        # written only after a successful digest post.
        if docker logs "$_idc" 2>&1 | grep -F "Idle digest: fired" >/dev/null; then
          _ID_FIRED="$_idc (runtime log)"
          return 0
        fi
        if docker exec "$_idc" sh -c \
          'grep -Eq "^last_included_at:[[:space:]]*[^[:space:]]" /workspace/.molecule/idle-prompt/providers/goal-state/goal.yaml' \
          >/dev/null 2>&1; then
          _ID_FIRED="$_idc (durable last_included_at)"
          return 0
        fi
      done < <(docker ps --format '{{.Names}}' 2>/dev/null | grep -E '^mol-ws-' || true)
      return 1
    }

    idle_digest_diagnostics() {
      local _idc _idenv _idgoal
      while IFS= read -r _idc; do
        [ -n "$_idc" ] || continue
        _idenv=$(docker exec "$_idc" sh -c 'env | grep -E "^MOLECULE_(IDLE|MAILBOX)" | sort | tr "\n" " "' 2>/dev/null || echo "exec-failed")
        _idgoal=$(docker exec "$_idc" sh -c 'cat /workspace/.molecule/idle-prompt/providers/goal-state/goal.yaml 2>/dev/null | tr "\n" " "' 2>/dev/null | head -c 500)
        log "    [idle-digest diag] $_idc env: ${_idenv:-none}"
        log "    [idle-digest diag] $_idc goal.yaml: ${_idgoal:-ABSENT}"
      done < <(docker ps --format '{{.Names}}' 2>/dev/null | grep -E '^mol-ws-' || true)
    }

    log "10c/11 Idle-digest: polling durable completion evidence for up to ${_IDLE_TIMEOUT_SECS}s (fire threshold=${E2E_IDLE_FIRE_SECONDS:-30}s; poll=${_IDLE_POLL_SECS}s)."
    if idle_digest_wait "$_IDLE_TIMEOUT_SECS" "$_IDLE_POLL_SECS" idle_digest_probe; then
      ok "    idle-digest FIRED (evidence: $_ID_FIRED)"
    else
      idle_digest_diagnostics
      fail "Idle digest never completed within ${_IDLE_TIMEOUT_SECS}s — env + durable goal diagnostics above name the broken leg (task #219 sub-step)."
    fi

    unset -f idle_digest_probe idle_digest_diagnostics
  else
    fail "E2E_IDLE_DIGEST_CHECK=on requires a usable Docker CLI and daemon to inspect completion evidence."
  fi
fi

# ─── 10d. Scheduler autonomous-fire (task #37, E2E_SCHEDULER_CHECK=on only) ──
# End-to-end proof of scheduler-as-trigger-plugin (RFC): the workspace declared
# the molecule-scheduler kind:trigger plugin at provision (secret-injection block
# above), so it boot-installed the plugin and armed the trigger daemon. This step
# creates a once-a-minute schedule through the SAME tenant API a user would, then
# waits for the daemon to autonomously FIRE it — exercising the full chain:
#   declare → boot-install → arm daemon → advertise `scheduler` capability →
#   create routes to the runtime volume grid → daemon polls grid → self-schedules
#   a turn (writes schedule-history.json + logs "fired schedule").
# The whole step is behind E2E_SCHEDULER_CHECK so it stays dark until the
# ephemeral runtime pin carries plugin boot-install + the trigger scaffold; once
# on, its fail arms are REACHABLE (create-4xx and never-fired are distinct hard
# failures, each with capability/grid/health diagnostics).
if [ "${E2E_SCHEDULER_CHECK:-}" = "on" ]; then
  _SCHED_TIMEOUT_SECS="${E2E_SCHEDULER_TIMEOUT_SECS:-360}"
  _SCHED_POLL_SECS="${E2E_SCHEDULER_POLL_SECS:-10}"
  # Bounded pre/post-create waits that CLOSE the #4448 arm race instead of hiding
  # it behind a longer fire timeout. cap-ready gates the create on the trigger
  # daemon having armed (necessary precondition); create-verify then re-issues the
  # create through the capability-advertisement lag until the schedule is CONFIRMED
  # on the runtime volume grid — so a create that silently mis-routed to the
  # retired DB (native-scheduler not advertised in core yet) is retried into place
  # rather than firing nowhere for ${_SCHED_TIMEOUT_SECS}s (armed:0).
  _SCHED_CAP_TIMEOUT_SECS="${E2E_SCHEDULER_CAP_TIMEOUT_SECS:-120}"
  _SCHED_CREATE_TIMEOUT_SECS="${E2E_SCHEDULER_CREATE_TIMEOUT_SECS:-120}"
  # A run-unique name so the durable history/grid probe can't match a stale entry
  # from another run sharing a recycled container, and so the fired-log grep is
  # unambiguous. Bounded + cron-safe (no spaces/quotes).
  _SCHED_NAME="e2e-fire-${E2E_RUN_ID:-local}"

  if idle_digest_require_docker; then
    # Shared diagnostics — used by ALL fail arms below (cap-ready, create-verify,
    # fire poll), so define it once up front. Dumps the on-volume grid + daemon
    # heartbeat + run log for every mol-ws container so a failure names the leg.
    scheduler_diagnostics() {
      local _sc _grid _health _hist _caps
      while IFS= read -r _sc; do
        [ -n "$_sc" ] || continue
        _grid=$(docker exec "$_sc" sh -c 'cat /configs/schedules/schedules.yaml 2>/dev/null | tr "\n" " "' 2>/dev/null | head -c 400)
        _health=$(docker exec "$_sc" sh -c 'cat /configs/schedules/schedule-health.json 2>/dev/null | tr "\n" " "' 2>/dev/null | head -c 300)
        _hist=$(docker exec "$_sc" sh -c 'cat /configs/schedules/schedule-history.json 2>/dev/null | tr "\n" " "' 2>/dev/null | head -c 400)
        _caps=$(docker exec "$_sc" sh -c 'env | grep -E "^MOLECULE_(DECLARED_PLUGINS|TRIGGER)" | sort | tr "\n" " "' 2>/dev/null || echo "exec-failed")
        log "    [scheduler diag] $_sc env: ${_caps:-none}"
        log "    [scheduler diag] $_sc grid: ${_grid:-ABSENT (create never reached the volume — capability/routing gap)}"
        log "    [scheduler diag] $_sc health: ${_health:-ABSENT (daemon never wrote a heartbeat — not armed)}"
        log "    [scheduler diag] $_sc history: ${_hist:-ABSENT (no schedule ever fired)}"
      done < <(docker ps --format '{{.Names}}' 2>/dev/null | grep -E '^mol-ws-' || true)
    }

    # ── 10d.1 Daemon-armed precondition (the "capability-ready" wait) ──
    # BEFORE creating, wait until the trigger daemon is actually up and ticking.
    # Observable proxy: the daemon writes /configs/schedules/schedule-health.json
    # on every poll tick once it has armed; a NON-NULL `last_tick` proves it
    # booted, armed its grid, and completed ≥1 poll cycle (`armed` may still be 0
    # here — no schedule yet — that's expected). This cleanly separates the
    # "plugin never boot-installed / daemon never armed" failure leg (health
    # ABSENT here) from the capability-routing leg the create-verify loop handles
    # next: the daemon being locally armed does NOT guarantee core has yet
    # PROCESSED the heartbeat advertising `scheduler` (that lag is #4448 root cause
    # #1), so the create step below re-issues the create until core routes it to
    # the volume — this gate only guarantees the daemon exists to route to.
    _SCHED_CAP_EVIDENCE=""
    scheduler_capability_ready() {
      local _sc _health
      _SCHED_CAP_EVIDENCE=""
      while IFS= read -r _sc; do
        [ -n "$_sc" ] || continue
        _health=$(docker exec "$_sc" sh -c 'cat /configs/schedules/schedule-health.json 2>/dev/null' 2>/dev/null)
        # Non-null quoted last_tick = daemon armed & ticking. A literal `null`
        # (pre-first-tick fallback {"last_tick":null,...}) does NOT match, so the
        # gate correctly keeps waiting until the daemon has actually ticked.
        if printf '%s' "$_health" | grep -Eq '"last_tick"[[:space:]]*:[[:space:]]*"[^"]+"'; then
          _SCHED_CAP_EVIDENCE="$_sc (schedule-health.json last_tick live)"
          return 0
        fi
      done < <(docker ps --format '{{.Names}}' 2>/dev/null | grep -E '^mol-ws-' || true)
      return 1
    }

    log "10d/11 Scheduler: waiting up to ${_SCHED_CAP_TIMEOUT_SECS}s for the trigger daemon to arm (schedule-health.json ticking) BEFORE creating the schedule — the daemon-ready precondition for the create-verify loop (#4448; poll=${_SCHED_POLL_SECS}s)."
    if idle_digest_wait "$_SCHED_CAP_TIMEOUT_SECS" "$_SCHED_POLL_SECS" scheduler_capability_ready; then
      ok "    trigger daemon armed & ticking — ready to accept a volume-routed create (evidence: $_SCHED_CAP_EVIDENCE)"
    else
      scheduler_diagnostics
      fail "Scheduler: the workspace never advertised native-scheduler within ${_SCHED_CAP_TIMEOUT_SECS}s (no mol-ws container wrote a daemon heartbeat with a live last_tick). The molecule-scheduler trigger plugin never boot-installed / the daemon never armed, so a schedule created now would mis-route to the retired DB path (post-#4399 fires nowhere → armed:0). The diagnostics above name the leg: ABSENT health = plugin didn't boot-install (check MOLECULE_DECLARED_PLUGINS reached the container and the trigger scaffold is present in the runtime pin)."
    fi

    # ── 10d.2 Create + verify it reached the volume grid (closes race #1 & #2) ──
    # Create via the tenant API (the exact path Canvas uses); a `* * * * *` cron
    # fires at the top of every minute. Then VERIFY the schedule actually landed
    # on the daemon's volume grid (/configs/schedules/schedules.yaml — read from
    # the container directly: ground truth, NOT through core's routing).
    #
    # The #4448 race: right after online, core may not yet have processed the
    # workspace's heartbeat advertising the `scheduler` capability, so
    # scheduleBackendIsVolume is false and the create SILENTLY routes to the
    # retired workspace_schedules DB — a 2xx, but the schedule fires nowhere
    # post-#4399 (armed:0). The create-response `id` is the deterministic routing
    # tell: the volume path returns id==name, the DB path returns a generated UUID.
    #
    # So we RE-ISSUE the create through that capability lag (bounded), but only
    # when it is SAFE: a re-create runs only while the schedule is NOT yet on the
    # volume grid AND the id shows a DB-misroute (a UUID) — there is no
    # unique(workspace_id,name) constraint (migration 015), so a repeated DB-routed
    # insert is a harmless stray, and re-posting can't collide with a volume entry
    # (no 409). Once core has the capability the next create routes to the volume
    # and lands. A volume-routed create writes the grid synchronously, so once
    # id==name we only poll for file visibility (never re-create → never 409).
    _SCHED_GRID_EVIDENCE=""
    scheduler_grid_has_entry() {
      local _sc _grid
      _SCHED_GRID_EVIDENCE=""
      while IFS= read -r _sc; do
        [ -n "$_sc" ] || continue
        _grid=$(docker exec "$_sc" sh -c 'cat /configs/schedules/schedules.yaml 2>/dev/null' 2>/dev/null)
        if printf '%s' "$_grid" | grep -Fq "$_SCHED_NAME"; then
          _SCHED_GRID_EVIDENCE="$_sc (schedules.yaml)"
          return 0
        fi
      done < <(docker ps --format '{{.Names}}' 2>/dev/null | grep -E '^mol-ws-' || true)
      return 1
    }

    scheduler_create_once() {
      # POST the schedule; sets _SCHED_CODE/_SCHED_BODY/_SCHED_ID. Returns 0 on a
      # 2xx create, 1 otherwise. A non-2xx is a hard, non-retryable fail arm
      # (routing/plugin/auth — e.g. a 502 volume-forward failure); the silent
      # DB-misroute this step defends against returns 2xx, so callers treat a
      # non-2xx as fatal.
      local _tmp
      _tmp=$(mktemp)
      _SCHED_CODE=$(tenant_call POST "/workspaces/$PARENT_ID/schedules" \
        -H "Content-Type: application/json" \
        -d "{\"name\":\"$_SCHED_NAME\",\"cron_expr\":\"* * * * *\",\"timezone\":\"UTC\",\"prompt\":\"E2E scheduler self-fire probe — reply OK.\"}" \
        -o "$_tmp" -w '%{http_code}' 2>/dev/null) || true
      _SCHED_BODY=$(cat "$_tmp" 2>/dev/null | sanitize_http_body); rm -f "$_tmp"
      _SCHED_ID=$(printf '%s' "$_SCHED_BODY" | python3 -c "import json,sys; print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
      echo "$_SCHED_CODE" | grep -Eq '^2[0-9][0-9]$'
    }

    _SCHED_CODE=""; _SCHED_BODY=""; _SCHED_ID=""
    _sched_do_create=1     # create on the first iteration
    _sched_elapsed=0
    _sched_why=""; _sched_next=""
    log "    Scheduler: creating '* * * * *' schedule '$_SCHED_NAME' on $PARENT_ID and verifying it reached the volume grid (retrying the capability-advertisement race for up to ${_SCHED_CREATE_TIMEOUT_SECS}s; poll=${_SCHED_POLL_SECS}s) — #4448."
    while true; do
      if [ "$_sched_do_create" = "1" ]; then
        if ! scheduler_create_once; then
          scheduler_diagnostics
          fail "Scheduler: create schedule returned http=$_SCHED_CODE (expected 2xx): ${_SCHED_BODY:0:300}. A 5xx/failed-forward here means the create routed to the volume but the runtime forward failed, or an auth/plugin fault (the silent DB-misroute returns 2xx, not 5xx). Check the workspace runtime is reachable, MOLECULE_DECLARED_PLUGINS reached the container, and the trigger scaffold is present in the runtime pin."
        fi
        _sched_do_create=0
      fi

      if scheduler_grid_has_entry; then
        break   # CONFIRMED on the volume grid — the only success arm
      fi

      # Grid-absent — classify from the create-response id (deterministic routing tell).
      if printf '%s' "$_SCHED_ID" | grep -Eiq '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'; then
        _sched_why="create mis-routed to the retired DB (id='$_SCHED_ID' is a UUID) — native-scheduler capability not advertised in core yet"
        _sched_do_create=1; _sched_next="re-creating"   # safe: not on the volume, so no 409; DB strays fire nowhere
      elif [ "$_SCHED_ID" = "$_SCHED_NAME" ]; then
        _sched_why="create routed to the volume (id='$_SCHED_ID') but the grid write is not visible yet"
        _sched_do_create=0; _sched_next="re-polling"    # do NOT re-create a volume entry (would 409); just re-poll
      else
        _sched_why="routing unclear (id='${_SCHED_ID:-empty}')"
        _sched_do_create=0; _sched_next="re-polling"    # conservative: re-poll rather than risk a volume 409
      fi

      if [ "$_sched_elapsed" -ge "$_SCHED_CREATE_TIMEOUT_SECS" ]; then
        scheduler_diagnostics
        fail "Scheduler: schedule '$_SCHED_NAME' never reached the volume grid within ${_SCHED_CREATE_TIMEOUT_SECS}s (last: $_sched_why; create http=$_SCHED_CODE). This is the #4448 mis-route/arm race: the create kept landing in the retired core DB (native-scheduler capability never advertised — molecule-scheduler plugin didn't boot-install / daemon didn't arm) OR the runtime grid write never landed — either way the daemon will never fire it. The grid/health diagnostics above name the leg: ABSENT grid = create never reached the volume."
      fi

      log "    Scheduler: not yet on the volume grid — $_sched_why; $_sched_next (${_sched_elapsed}/${_SCHED_CREATE_TIMEOUT_SECS}s)."
      sleep "$_SCHED_POLL_SECS"
      _sched_elapsed=$((_sched_elapsed + _SCHED_POLL_SECS))
    done
    ok "    schedule '$_SCHED_NAME' created (http=$_SCHED_CODE, id='$_SCHED_ID') and CONFIRMED on the volume grid (evidence: $_SCHED_GRID_EVIDENCE) — routed to the runtime volume, not the retired DB"

    # ── 10d.4 Fire poll (unchanged final step) ──
    _SCHED_FIRED=""
    scheduler_fire_probe() {
      local _sc
      _SCHED_FIRED=""
      while IFS= read -r _sc; do
        [ -n "$_sc" ] || continue
        # Container log: the daemon logs `fired schedule '<name>'` (scheduler.py).
        if docker logs "$_sc" 2>&1 | grep -F "fired schedule '$_SCHED_NAME'" >/dev/null; then
          _SCHED_FIRED="$_sc (runtime log)"
          return 0
        fi
        # Durable: schedule-history.json on the persisted config volume carries a
        # {"name":"<name>","status":"fired"} entry, written only after a real
        # self-scheduled turn — survives CP-driven container replacement.
        if docker exec "$_sc" sh -c \
          "grep -Fq '\"name\": \"$_SCHED_NAME\"' /configs/schedules/schedule-history.json 2>/dev/null && grep -Fq '\"status\": \"fired\"' /configs/schedules/schedule-history.json 2>/dev/null" \
          >/dev/null 2>&1; then
          _SCHED_FIRED="$_sc (durable schedule-history.json)"
          return 0
        fi
      done < <(docker ps --format '{{.Names}}' 2>/dev/null | grep -E '^mol-ws-' || true)
      return 1
    }

    log "    Scheduler: polling fire evidence for up to ${_SCHED_TIMEOUT_SECS}s (poll=${_SCHED_POLL_SECS}s)."
    if idle_digest_wait "$_SCHED_TIMEOUT_SECS" "$_SCHED_POLL_SECS" scheduler_fire_probe; then
      ok "    scheduler autonomously FIRED '$_SCHED_NAME' (evidence: $_SCHED_FIRED)"
    else
      scheduler_diagnostics
      fail "Scheduler never fired '$_SCHED_NAME' within ${_SCHED_TIMEOUT_SECS}s — the grid/health/history diagnostics above name the broken leg: ABSENT grid = create didn't reach the volume (capability not advertised); ABSENT health = daemon not armed (plugin didn't boot-install); grid+health present but no history = daemon armed but the trigger lane never delivered (self-scheduler turn path)."
    fi

    unset -f scheduler_capability_ready scheduler_create_once scheduler_grid_has_entry scheduler_fire_probe scheduler_diagnostics
  else
    fail "E2E_SCHEDULER_CHECK=on requires a usable Docker CLI and daemon to inspect fire evidence."
  fi
fi

# ─── 10e. Native digest-provider plugin load (RFC #4413, E2E_DIGEST_PLUGIN_CHECK=on only) ──
# End-to-end proof that a NATIVE digest-provider plugin is loaded IN-PROCESS by
# the runtime's idle-digest assembler, admitted by the LOAD-TIME trust gate from
# the VENDORED native-plugins registry (runtime#310) — NOT from an env allow-list
# (we never set MOLECULE_NATIVE_PLUGIN_NAMES). The workspace declared the plugin
# at provision (secret-injection block above) with MOLECULE_DIGEST_PROVIDER_PLUGINS=1,
# so it boot-installed the plugin under its repo dir and, when the digest armed,
# build_default_providers() discovered contributes.digestProviders, imported the
# provider, and passed the trust gate because the plugin's install name is in the
# registry-derived native set. The runtime logs
#   digest-provider: loaded '<provider_id>' from plugin <install-name> (native=True)
# ONLY when all of that succeeded — the single unambiguous evidence line. The step
# is behind E2E_DIGEST_PLUGIN_CHECK so it stays dark until the ephemeral runtime
# pin carries the loader + the registry trust source; its fail arm is REACHABLE
# (a refused/absent plugin never emits native=True and the diagnostics name why).
if [ "${E2E_DIGEST_PLUGIN_CHECK:-}" = "on" ]; then
  _DP_TIMEOUT_SECS="${E2E_DIGEST_PLUGIN_TIMEOUT_SECS:-360}"
  _DP_POLL_SECS="${E2E_DIGEST_PLUGIN_POLL_SECS:-10}"
  # The install-dir basename the trust gate matches = the source's repo segment
  # (NOT the registry `name` field) — the exact identity the runtime keys on.
  _DP_NAME=$(E2E_DIGEST_PLUGIN_SOURCE="${E2E_DIGEST_PLUGIN_SOURCE:-gitea://molecule-ai/molecule-ai-plugin-digest-mail#v0.1.0}" python3 -c "
import os
s = os.environ['E2E_DIGEST_PLUGIN_SOURCE'].split('#', 1)[0].rstrip('/')
print(s.rsplit('/', 1)[-1])
")

  if idle_digest_require_docker; then
    _DP_LOADED=""
    digest_plugin_probe() {
      local _sc
      _DP_LOADED=""
      while IFS= read -r _sc; do
        [ -n "$_sc" ] || continue
        # The loader's success line (plugin_loader.py): native=True proves the
        # registry trust gate admitted an official/reserved provider from this
        # native plugin. Grepping the install-name + native=True is unambiguous.
        # The line is not run-scoped, but the ephemeral gate provisions a fresh
        # mol-ws- container per run (no reuse), so a stale prior-run match cannot
        # occur here — same assumption 10c's generic grep already relies on.
        if docker logs "$_sc" 2>&1 | grep -F "from plugin ${_DP_NAME} (native=True)" >/dev/null; then
          _DP_LOADED="$_sc (runtime log)"
          return 0
        fi
      done < <(docker ps --format '{{.Names}}' 2>/dev/null | grep -E '^mol-ws-' || true)
      return 1
    }

    digest_plugin_diagnostics() {
      local _sc _env _installed _warn
      while IFS= read -r _sc; do
        [ -n "$_sc" ] || continue
        _env=$(docker exec "$_sc" sh -c 'env | grep -E "^MOLECULE_(DIGEST_PROVIDER_PLUGINS|DECLARED_PLUGINS|NATIVE_PLUGIN_NAMES)" | sort | tr "\n" " "' 2>/dev/null || echo "exec-failed")
        _installed=$(docker exec "$_sc" sh -c 'ls -1 /configs/plugins /plugins 2>/dev/null | tr "\n" " "' 2>/dev/null | head -c 300)
        # `|| true`: a zero-match grep exits 1, which under `set -euo pipefail`
        # (line 119) would abort this diag fn BEFORE the log lines print — and
        # zero matches IS the loader-off leg this diagnostic exists to explain.
        _warn=$(docker logs "$_sc" 2>&1 | grep -F "digest-provider:" | tail -n 5 | tr '\n' '|' | head -c 500 || true)
        log "    [digest-plugin diag] $_sc env: ${_env:-none}"
        log "    [digest-plugin diag] $_sc installed plugins: ${_installed:-ABSENT (boot-install never fetched the plugin — check MOLECULE_DECLARED_PLUGINS reached the container)}"
        log "    [digest-plugin diag] $_sc loader lines: ${_warn:-NONE (loader never ran — flag off, or the digest never armed; refused/native=False here means the trust source rejected the plugin)}"
      done < <(docker ps --format '{{.Names}}' 2>/dev/null | grep -E '^mol-ws-' || true)
    }

    log "10e/11 Digest plugin: polling for in-process native load of '${_DP_NAME}' for up to ${_DP_TIMEOUT_SECS}s (poll=${_DP_POLL_SECS}s)."
    if idle_digest_wait "$_DP_TIMEOUT_SECS" "$_DP_POLL_SECS" digest_plugin_probe; then
      ok "    native digest-provider plugin '${_DP_NAME}' loaded IN-PROCESS via the registry trust gate (evidence: $_DP_LOADED)"
    else
      digest_plugin_diagnostics
      fail "Native digest-provider plugin '${_DP_NAME}' never loaded (native=True) within ${_DP_TIMEOUT_SECS}s. The diagnostics above name the broken leg: ABSENT installed = boot-install never fetched the plugin (MOLECULE_DECLARED_PLUGINS gap); loader lines showing native=False / 'refused' = the plugin installed but the trust gate rejected it (registry trust source not carried by the runtime pin, or install name != registry source segment); NO loader lines = MOLECULE_DIGEST_PROVIDER_PLUGINS off or the digest never armed."
    fi

    unset -f digest_plugin_probe digest_plugin_diagnostics
  else
    fail "E2E_DIGEST_PLUGIN_CHECK=on requires a usable Docker CLI and daemon to inspect load evidence."
  fi
fi

# ─── 10f. Self-schedule tool (RFC, E2E_SELF_SCHEDULE_CHECK=on only) ──────────
# End-to-end proof that the audience:self create_schedule TOOL — delivered by the
# molecule-ai-plugin-schedule-self plugin (declared additively at provision, above)
# and rendered onto the workspace by the runtime's audience injector — creates a
# schedule that lands on the workspace's OWN volume grid and FIRES, using ONLY the
# per-workspace token (no org/admin key). Firing is done by the scheduler trigger
# daemon (10d), so this step is CO-GATED on E2E_SCHEDULER_CHECK=on.
#
# Deterministic (no LLM): from the gate host we `docker exec -i` into the OWN mol-ws
# container and pipe a two-line JSON-RPC stream (initialize, then tools/call
# create_schedule) into `npx --prefer-offline @molecule-ai/mcp-server@1.9.3 -e
# MOLECULE_MCP_MODE=self`, capturing stdout — mirroring test_mcp_stdio_staging.sh.
# create_schedule lives ONLY on the Node self-mode stdio surface (NOT core's Go MCP
# HTTP bridge), so this is the sole path that exercises the real tool with no agent
# turn (an LLM/A2A path would be non-deterministic — the flake this step avoids).
#
# Two legs:
#   LEG A (injector proof): parse the rendered self mcpServers entry env from the
#     runtime's native adapter config → assert MOLECULE_MCP_MODE=self + token-file
#     present; NEG CONTROL: org/admin keys ABSENT from that RENDERED entry env
#     (box-independent evidence — NOT /proc/<pid>/environ, which inherits ambient
#     process env on a concierge box even though authHeaders() never reads it).
#   LEG B (tool proof): create_schedule TWICE — (i) explicit workspace_id and (ii)
#     workspace_id OMITTED (must SELF-RESOLVE via the runtime WORKSPACE_ID injection
#     + the self-mode self-default a recent fix restored) — assert each lands on the
#     OWN grid + the explicit one FIRES (reuses 10d's log/history verification).
#     NEG CONTROL: a FOREIGN workspace_id is REJECTED (tool error / core 401) AND
#     never arrives on any grid; precondition-assert the self container carries no
#     admin/org token so the 401 leg cannot false-pass.
#
# Dark until preconditions confirmed (default OFF): needs a workspace-template pin
# carrying runtime#328's self-audience injector AND a prebaked mcp-server 1.9.3 (so
# `npx --prefer-offline` resolves offline in the no-network gate). Its fail arms are
# REACHABLE (each leg has a distinct hard failure with diagnostics). It registers NO
# live_milestone (LIVE_MILESTONES is a closed SSOT — adding one trips the drift gate).
if [ "${E2E_SELF_SCHEDULE_CHECK:-}" = "on" ]; then
  # Co-gate: the scheduler trigger daemon (10d) supplies the FIRE path 10f depends
  # on. Requiring E2E_SCHEDULER_CHECK=on keeps this a hard, reachable contract.
  if [ "${E2E_SCHEDULER_CHECK:-}" != "on" ]; then
    fail "E2E_SELF_SCHEDULE_CHECK=on requires E2E_SCHEDULER_CHECK=on — the scheduler trigger daemon (10d) fires the self-created schedule. Enable both together."
  fi
  _SS_TIMEOUT_SECS="${E2E_SELF_SCHEDULE_TIMEOUT_SECS:-360}"
  _SS_POLL_SECS="${E2E_SELF_SCHEDULE_POLL_SECS:-10}"
  _SS_CAP_TIMEOUT_SECS="${E2E_SELF_SCHEDULE_CAP_TIMEOUT_SECS:-120}"
  _SS_CREATE_TIMEOUT_SECS="${E2E_SELF_SCHEDULE_CREATE_TIMEOUT_SECS:-120}"
  _SS_SETTLE_SECS="${E2E_SELF_SCHEDULE_SETTLE_SECS:-30}"
  _SS_MCP_SPEC="${E2E_SELF_SCHEDULE_MCP_SPEC:-@molecule-ai/mcp-server@1.9.3}"
  # Run-unique names (distinct from 10d's e2e-fire-* to avoid a ScheduleStore
  # name-collision 409 + grid-provenance ambiguity). Bounded, cron-safe.
  _SS_SN="e2e-self-fire-${E2E_RUN_ID:-local}"
  _SS_SN_OMIT="e2e-self-omit-${E2E_RUN_ID:-local}"
  _SS_FN="e2e-foreign-${E2E_RUN_ID:-local}"

  if idle_digest_require_docker; then
    # Globals set by the probe fns below; pre-init so `set -u` never trips when a
    # diagnostic reads one before its producer ran.
    _SS_SELF_CID=""; _SS_SELF_WSID=""; _SS_CAP_EVIDENCE=""; _SS_FIRED=""
    _SS_CLASS=""; _SS_ID=""; _SS_OUT_FILE=""; _SS_LEGA_DETAIL=""
    _SS_CREATE_DETAIL=""

    # LEG A + the rendered-launch extractor parse the DEFAULT-runtime hermes
    # config.yaml with the runner's python; make PyYAML importable (fail-loud
    # best-effort install) so a missing lib cannot masquerade as a missing self
    # entry (PARSE_FAIL yaml-lib-missing). No-op when already present or for the
    # claude-code JSON path.
    if ! python3 -c 'import yaml' 2>/dev/null; then
      pip install --quiet PyYAML 2>/dev/null || python3 -m pip install --quiet PyYAML 2>/dev/null || true
    fi

    # Per-runtime native MCP-config path/format the audience injector renders the
    # self entry into (mirrors the mcp_render / plugins_reconcile SSOT, ADR-005).
    self_schedule_adapter_config_path() {
      local _rt
      _rt=$(printf '%s' "${1:-claude-code}" | tr 'A-Z' 'a-z' | tr '-' '_')
      case "$_rt" in
        # hermes: the runtime writes native MCP servers to $HERMES_HOME/config.yaml,
        # and the container runs the runtime under HERMES_HOME=/tmp/.hermes + HOME=/tmp
        # (adapter._hermes_config_path), NOT the build-time installer default
        # /home/agent/.hermes/config.yaml (which nothing rewrites at runtime). Inspect
        # the path the injector actually renders to + hermes-agent reads. (codex/openclaw
        # /home/agent paths are unverified for the same HOME=/tmp effect — follow-up.)
        hermes)   printf '%s' "/tmp/.hermes/config.yaml" ;;
        openclaw) printf '%s' "/home/agent/.openclaw/openclaw.json" ;;
        codex)    printf '%s' "/home/agent/.codex/config.toml" ;;
        *)        printf '%s' "/configs/.claude/settings.json" ;;
      esac
    }
    self_schedule_adapter_config_format() {
      local _rt
      _rt=$(printf '%s' "${1:-claude-code}" | tr 'A-Z' 'a-z' | tr '-' '_')
      case "$_rt" in
        hermes) printf '%s' "yaml" ;;
        codex)  printf '%s' "toml" ;;
        *)      printf '%s' "json" ;;
      esac
    }

    # Resolve the OWN mol-ws container: the one whose WORKSPACE_ID env == PARENT_ID
    # (provisioner.go sets WORKSPACE_ID=<id> in every workspace container). "$WS_ID
    # from container env" — deterministic, no name-mangling on the short-12 suffix.
    self_schedule_resolve_own_container() {
      local _sc _wsid
      _SS_SELF_CID=""; _SS_SELF_WSID=""
      while IFS= read -r _sc; do
        [ -n "$_sc" ] || continue
        _wsid=$(docker exec "$_sc" sh -c 'printf "%s" "${WORKSPACE_ID:-}"' 2>/dev/null || true)
        if [ -n "$_wsid" ] && [ "$_wsid" = "$PARENT_ID" ]; then
          _SS_SELF_CID="$_sc"
          _SS_SELF_WSID="$_wsid"
          return 0
        fi
      done < <(docker ps --format '{{.Names}}' 2>/dev/null | grep -E '^mol-ws-' || true)
      return 1
    }

    # $1=name present on the OWN (self) container's volume grid?
    self_schedule_own_grid_has() {
      local _grid
      _grid=$(docker exec "$_SS_SELF_CID" sh -c 'cat /configs/schedules/schedules.yaml 2>/dev/null' 2>/dev/null || true)
      printf '%s' "$_grid" | grep -Fq "$1"
    }

    # $1=name present on ANY mol-ws container's volume grid? (foreign non-arrival).
    self_schedule_any_grid_has() {
      local _sc _grid
      while IFS= read -r _sc; do
        [ -n "$_sc" ] || continue
        _grid=$(docker exec "$_sc" sh -c 'cat /configs/schedules/schedules.yaml 2>/dev/null' 2>/dev/null || true)
        if printf '%s' "$_grid" | grep -Fq "$1"; then
          return 0
        fi
      done < <(docker ps --format '{{.Names}}' 2>/dev/null | grep -E '^mol-ws-' || true)
      return 1
    }

    # $1=name fired? Reuses 10d's verification verbatim: the daemon logs
    # `fired schedule '<name>'` OR writes a durable {"name":..,"status":"fired"}.
    self_schedule_fire_probe() {
      local _name="$1" _sc
      _SS_FIRED=""
      while IFS= read -r _sc; do
        [ -n "$_sc" ] || continue
        if docker logs "$_sc" 2>&1 | grep -F "fired schedule '$_name'" >/dev/null; then
          _SS_FIRED="$_sc (runtime log)"
          return 0
        fi
        if docker exec "$_sc" sh -c \
          "grep -Fq '\"name\": \"$_name\"' /configs/schedules/schedule-history.json 2>/dev/null && grep -Fq '\"status\": \"fired\"' /configs/schedules/schedule-history.json 2>/dev/null" \
          >/dev/null 2>&1; then
          _SS_FIRED="$_sc (durable schedule-history.json)"
          return 0
        fi
      done < <(docker ps --format '{{.Names}}' 2>/dev/null | grep -E '^mol-ws-' || true)
      return 1
    }

    # NC precondition: the self (own) workspace is ORDINARY — its container must NOT
    # carry an admin/org token. If it did, core WorkspaceAuth would accept a FOREIGN
    # workspace_id for ANY id and the 401 leg of the foreign-id neg control would
    # false-pass. (LEG A's org-key control reads the RENDERED spec-env, not the
    # container env, precisely because a concierge box can carry an org key ambiently;
    # the self workspace here is an ordinary child, so its container env is clean.)
    self_schedule_assert_ordinary_box() {
      local _leak
      _leak=$(docker exec "$_SS_SELF_CID" sh -c 'env | grep -E "^(MOLECULE_ADMIN_TOKEN|MOLECULE_ORG_API_KEY|ORG_API_KEY)=" | sed "s/=.*/=<present>/" | tr "\n" " "' 2>/dev/null || true)
      [ -z "$_leak" ]
    }

    # LEG A: read the rendered self mcpServers entry env from the runtime's native
    # adapter config and assert MODE=self + token-file present, org/admin keys absent
    # — SCOPED to the self entry env (box-independent). Sets _SS_LEGA_DETAIL.
    self_schedule_injector_proof() {
      local _cfg _fmt _content _res
      _cfg=$(self_schedule_adapter_config_path "$RUNTIME")
      _fmt=$(self_schedule_adapter_config_format "$RUNTIME")
      _content=$(docker exec "$_SS_SELF_CID" sh -c "cat '$_cfg' 2>/dev/null" 2>/dev/null || true)
      if [ -z "$_content" ]; then
        _SS_LEGA_DETAIL="adapter config '$_cfg' absent/empty on $_SS_SELF_CID (runtime=$RUNTIME) — the audience injector never rendered the self entry"
        return 3
      fi
      _res=$(SS_CFG_RAW="$_content" SS_CFG_FMT="$_fmt" python3 -c '
import json, os, sys
raw = os.environ.get("SS_CFG_RAW", "")
fmt = os.environ.get("SS_CFG_FMT", "json")
data = None
perr = ""
try:
    data = json.loads(raw)
except Exception as e:
    perr = str(e)
if data is None:
    if fmt in ("yaml", "yml"):
        try:
            import yaml
            data = yaml.safe_load(raw)
        except ImportError:
            print("PARSE_FAIL yaml-lib-missing"); sys.exit(0)
        except Exception as e:
            print("PARSE_FAIL yaml:" + str(e)); sys.exit(0)
    elif fmt == "toml":
        try:
            import tomllib
            data = tomllib.loads(raw)
        except Exception as e:
            print("PARSE_FAIL toml:" + str(e)); sys.exit(0)
    else:
        print("PARSE_FAIL json:" + perr); sys.exit(0)
found = []
def walk(o):
    if isinstance(o, dict):
        env = o.get("env")
        if isinstance(env, dict) and str(env.get("MOLECULE_MCP_MODE", "")) == "self":
            found.append(env)
        for v in o.values():
            walk(v)
    elif isinstance(o, list):
        for v in o:
            walk(v)
walk(data)
if not found:
    print("NO_SELF_ENTRY"); sys.exit(0)
env = found[0]
if str(env.get("MOLECULE_MCP_MODE", "")) != "self":
    print("FAIL mode-not-self"); sys.exit(0)
if not env.get("MOLECULE_WORKSPACE_TOKEN_FILE"):
    print("FAIL token-file-absent"); sys.exit(0)
leaked = [k for k in ("MOLECULE_API_KEY", "MOLECULE_ORG_API_KEY", "ORG_API_KEY", "MOLECULE_ADMIN_TOKEN") if k in env]
if leaked:
    print("FAIL org-key-leaked:" + ",".join(leaked)); sys.exit(0)
print("OK token_file=" + str(env.get("MOLECULE_WORKSPACE_TOKEN_FILE")))
' 2>/dev/null || echo "PARSE_FAIL exec")
      _SS_LEGA_DETAIL="$_res (config=$_cfg fmt=$_fmt)"
      case "$_res" in
        OK*)           return 0 ;;
        NO_SELF_ENTRY) return 2 ;;
        *)             return 1 ;;
      esac
    }

    # Drive ONE create_schedule call via the self-mode mcp-server on the OWN
    # container. $1 = the create_schedule arguments JSON. Sets _SS_OUT_FILE (captured
    # stdout) and _SS_CLASS ∈ {ok, tool-error, error, no-result, no-response,
    # parse-failed}. Swallows the nonzero RC (server exits nonzero on stdin EOF —
    # expected, mirrors test_mcp_stdio_staging.sh:128-132).
    self_schedule_invoke_create() {
      local _args="$1" _init _call _cfg _fmt _cfg_raw _nodebin _cpath
      local _result_fields=()
      _SS_ID=""
      _SS_OUT_FILE=$(e2e_tmp /tmp/e2e_self_schedule.XXXXXX)
      _init='{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"e2e-10f","version":"1.0.0"}}}'
      _call=$(printf '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"create_schedule","arguments":%s}}' "$_args")
      # Drive the PLUGIN'S RENDERED launch, NOT a hand-rolled npx: read the
      # command/args/env the audience injector actually wrote into the adapter
      # config (the offline `sh -c` shim + npm_config registry/cache + injected
      # MOLECULE_MCP_MODE=self / token-file / WORKSPACE_ID), so a regression in
      # that rendered entry is CAUGHT here instead of masked by a substitute
      # invocation. hermes keeps node/npx OFF the container PATH (adapter_base's
      # mcp_launch_env adds it only for children the runtime itself spawns), so we
      # resolve the node bin and prepend it — replicating the runtime's spawn env —
      # else the shim's npm/npx would not resolve.
      _cfg=$(self_schedule_adapter_config_path "$RUNTIME")
      _fmt=$(self_schedule_adapter_config_format "$RUNTIME")
      _cfg_raw=$(docker exec "$_SS_SELF_CID" sh -c "cat '$_cfg' 2>/dev/null" 2>/dev/null || true)
      _nodebin=$(docker exec "$_SS_SELF_CID" sh -c 'for d in "$HERMES_HOME/node/bin" "$HOME/.hermes/node/bin" /tmp/.hermes/node/bin /home/agent/.hermes/node/bin; do [ -x "$d/npx" ] && { printf "%s" "$d"; exit 0; }; done; if command -v npx >/dev/null 2>&1; then dirname "$(command -v npx)"; fi' 2>/dev/null || true)
      _cpath=$(docker exec "$_SS_SELF_CID" sh -c 'printf "%s" "${PATH:-/usr/local/bin:/usr/bin:/bin}"' 2>/dev/null || printf '%s' "/usr/local/bin:/usr/bin:/bin")
      local _dexec_args=()
      mapfile -d '' _dexec_args < <(SS_CFG_RAW="$_cfg_raw" SS_CFG_FMT="$_fmt" SS_CID="$_SS_SELF_CID" SS_NODEBIN="$_nodebin" SS_CPATH="$_cpath" python3 -c '
import json, os, sys
raw = os.environ.get("SS_CFG_RAW", ""); fmt = os.environ.get("SS_CFG_FMT", "json")
data = None
try:
    data = json.loads(raw)
except Exception:
    pass
if data is None and fmt in ("yaml", "yml"):
    try:
        import yaml; data = yaml.safe_load(raw)
    except Exception:
        data = None
if data is None and fmt == "toml":
    try:
        import tomllib; data = tomllib.loads(raw)
    except Exception:
        data = None
entry = [None]
def walk(o):
    if entry[0] is not None:
        return
    if isinstance(o, dict):
        env = o.get("env")
        if isinstance(env, dict) and str(env.get("MOLECULE_MCP_MODE", "")) == "self" and ("command" in o or "args" in o):
            entry[0] = o; return
        for v in o.values():
            walk(v)
    elif isinstance(o, list):
        for v in o:
            walk(v)
walk(data if data is not None else {})
e = entry[0]
if e is None:
    sys.exit(0)  # empty argv -> caller treats as no-render
out = []
for k, v in (e.get("env") or {}).items():
    out.append("-e"); out.append("%s=%s" % (k, v))
nb = os.environ.get("SS_NODEBIN", ""); cp = os.environ.get("SS_CPATH", "")
out.append("-e"); out.append("PATH=%s" % ((nb + ":" + cp) if nb else cp))
out.append(os.environ["SS_CID"])
cmd = e.get("command")
if cmd is not None:
    out.append(str(cmd))
for a in (e.get("args") or []):
    out.append(str(a))
sys.stdout.write("\0".join(out))
' 2>/dev/null)
      if [ "${#_dexec_args[@]}" -eq 0 ]; then
        _SS_CLASS="no-render"
        log "    10f: could not extract the rendered self-launch (MODE=self command entry) from $_cfg — LEG A injector-proof should have caught a missing render first"
        return 0
      fi
      {
        printf '%s\n' "$_init"
        printf '%s\n' "$_call"
      } | docker exec -i "${_dexec_args[@]}" > "$_SS_OUT_FILE" 2>&1 || {
        local _rc=$?
        log "    10f: self-mode mcp-server exited rc=$_rc (nonzero on stdin EOF is expected — mirrors test_mcp_stdio_staging.sh)"
      }
      mapfile -t _result_fields < <(python3 -c '
import json, sys
cls = "no-response"
rid = ""
try:
    fh = open(sys.argv[1], "r", errors="replace")
except Exception:
    print("no-response"); print(""); sys.exit(0)
for line in fh:
    line = line.strip()
    if not line or line[:1] != "{":
        continue
    try:
        m = json.loads(line)
    except Exception:
        continue
    if m.get("id") == 2:
        if "error" in m:
            cls = "error"
        else:
            r = m.get("result", None)
            if r is None:
                cls = "no-result"
            elif isinstance(r, dict) and r.get("isError") is True:
                cls = "tool-error"
            elif isinstance(r, dict):
                # self-mode returns an HTTP/auth rejection (e.g. core 401 on a
                # FOREIGN workspace_id) as a NORMAL result whose content carries
                # {"error": ...} WITHOUT isError (see self-mode.test.ts). Treat an
                # embedded content error as a tool-error so the foreign-id neg
                # control sees the rejection; a successful create has no error key.
                emb = False
                for c in (r.get("content") or []):
                    t = c.get("text", "") if isinstance(c, dict) else ""
                    ts = t.strip()
                    if ts[:1] == "{":
                        try:
                            o = json.loads(ts)
                        except Exception:
                            o = None
                        if isinstance(o, dict):
                            if o.get("id") is not None:
                                rid = str(o.get("id"))
                            if o.get("error"):
                                emb = True; break
                cls = "tool-error" if emb else "ok"
            else:
                cls = "ok"
        break
print(cls)
print(rid)
' "$_SS_OUT_FILE" 2>/dev/null || printf 'parse-failed\n\n')
      _SS_CLASS="${_result_fields[0]:-parse-failed}"
      _SS_ID="${_result_fields[1]:-}"
    }

    # Shared diagnostics — called by EVERY fail arm below. Dumps per-container
    # declared-plugins/mode env, installed-plugin dirs, recent self/schedule log
    # lines, and the last captured JSON-RPC payload. Every grep is `|| true` because
    # a zero-match grep exits 1 under `set -euo pipefail` and would abort this fn
    # before the log lines print — and a zero match IS the leg it explains.
    self_schedule_diagnostics() {
      local _sc _decl _plug _slog _grid _health _state _history
      # Always snapshot the resolved target directly. A sibling's healthy daemon is
      # not evidence for this grid, and the target may have stopped and disappeared
      # from `docker ps` by the time the failure arm runs.
      if [ -n "${_SS_SELF_CID:-}" ]; then
        _grid=$(docker exec "$_SS_SELF_CID" sh -c 'cat /configs/schedules/schedules.yaml 2>/dev/null' 2>/dev/null | tr '\n' ' ' | head -c 1200 || true)
        _health=$(docker exec "$_SS_SELF_CID" sh -c 'cat /configs/schedules/schedule-health.json 2>/dev/null' 2>/dev/null | tr '\n' ' ' | head -c 800 || true)
        _state=$(docker exec "$_SS_SELF_CID" sh -c 'cat /configs/schedules/schedule-state.json 2>/dev/null' 2>/dev/null | tr '\n' ' ' | head -c 1200 || true)
        _history=$(docker exec "$_SS_SELF_CID" sh -c 'cat /configs/schedules/schedule-history.json 2>/dev/null' 2>/dev/null | tr '\n' ' ' | tail -c 1600 || true)
        log "    [self-schedule diag] TARGET $_SS_SELF_CID schedules.yaml: ${_grid:-ABSENT}"
        log "    [self-schedule diag] TARGET $_SS_SELF_CID schedule-health.json: ${_health:-ABSENT}"
        log "    [self-schedule diag] TARGET $_SS_SELF_CID schedule-state.json: ${_state:-ABSENT}"
        log "    [self-schedule diag] TARGET $_SS_SELF_CID schedule-history.json tail: ${_history:-ABSENT}"
      fi
      while IFS= read -r _sc; do
        [ -n "$_sc" ] || continue
        _decl=$(docker exec "$_sc" sh -c 'env | grep -E "^MOLECULE_(DECLARED_PLUGINS|MCP_MODE|TRIGGER)" | sort | tr "\n" " "' 2>/dev/null || true)
        _plug=$(docker exec "$_sc" sh -c 'ls -1 /configs/plugins /plugins 2>/dev/null | tr "\n" " "' 2>/dev/null | head -c 300 || true)
        _slog=$(docker logs "$_sc" 2>&1 | grep -iE "self|schedule|mcp-server|audience" | tail -n 5 | tr '\n' '|' | head -c 500 || true)
        log "    [self-schedule diag] $_sc declared/mode: ${_decl:-none}"
        log "    [self-schedule diag] $_sc installed plugins: ${_plug:-ABSENT (boot-install never fetched schedule-self — check MOLECULE_DECLARED_PLUGINS reached the container)}"
        log "    [self-schedule diag] $_sc recent self/schedule log: ${_slog:-NONE}"
      done < <(docker ps --format '{{.Names}}' 2>/dev/null | grep -E '^mol-ws-' || true)
      if [ -n "${_SS_OUT_FILE:-}" ] && [ -f "${_SS_OUT_FILE:-}" ]; then
        log "    [self-schedule diag] last create JSON-RPC payload (class=${_SS_CLASS:-?}, id=${_SS_ID:-empty}): $(sanitize_http_body < "$_SS_OUT_FILE" | tr '\n' '|' | head -c 600 || true)"
      fi
    }

    # ── 10f.0 Resolve the own container ──
    log "10f/11 Self-schedule: resolving the OWN workspace container (WORKSPACE_ID=$PARENT_ID) for the deterministic self-mode invocation (up to ${_SS_CAP_TIMEOUT_SECS}s; poll=${_SS_POLL_SECS}s)."
    if idle_digest_wait "$_SS_CAP_TIMEOUT_SECS" "$_SS_POLL_SECS" self_schedule_resolve_own_container; then
      ok "    resolved own container: $_SS_SELF_CID (WORKSPACE_ID=$_SS_SELF_WSID)"
    else
      self_schedule_diagnostics
      fail "Self-schedule: no running mol-ws container reported WORKSPACE_ID=$PARENT_ID within ${_SS_CAP_TIMEOUT_SECS}s — cannot drive the self-mode create without the own container."
    fi

    # ── 10f.A Injector proof (rendered self spec-env, org-key-free) ──
    log "    LEG A: asserting the rendered self mcpServers entry env on $_SS_SELF_CID (MOLECULE_MCP_MODE=self + token-file present; org/admin keys ABSENT)."
    if self_schedule_injector_proof; then
      ok "    LEG A: self-mode spec env correct AND org-key-free — $_SS_LEGA_DETAIL"
    else
      self_schedule_diagnostics
      fail "LEG A self-schedule injector proof failed: $_SS_LEGA_DETAIL. Expected the rendered self entry env to carry MOLECULE_MCP_MODE=self + MOLECULE_WORKSPACE_TOKEN_FILE and to OMIT MOLECULE_API_KEY/MOLECULE_ORG_API_KEY/ORG_API_KEY/MOLECULE_ADMIN_TOKEN. NO_SELF_ENTRY = the injector never rendered an audience:self entry (template missing runtime#328 / plugin not declared); org-key-leaked = the injector leaked a privileged key onto the ordinary-box self surface (a real regression); PARSE_FAIL yaml-lib-missing = the runner python lacks PyYAML for the hermes config.yaml adapter path (verify before flipping 10f on)."
    fi

    # ── 10f.B precondition: ordinary box (no admin/org token) ──
    if self_schedule_assert_ordinary_box; then
      ok "    precondition: own container carries no admin/org token — the foreign-id 401 neg control is meaningful"
    else
      self_schedule_diagnostics
      fail "Self-schedule precondition failed: the OWN workspace container carries an admin/org token (MOLECULE_ADMIN_TOKEN/MOLECULE_ORG_API_KEY/ORG_API_KEY present). WorkspaceAuth would then accept a FOREIGN workspace_id for any id, so the foreign-id negative control below would false-pass. Investigate token injection — the ordinary-box token model is violated."
    fi

    # ── 10f.B.1 Daemon-armed wait (mirror 10d; closes #4448 before create) ──
    log "    LEG B: waiting up to ${_SS_CAP_TIMEOUT_SECS}s for the trigger daemon to arm (schedule-health.json ticking) before creating — #4448 arm-race guard."
    if idle_digest_wait "$_SS_CAP_TIMEOUT_SECS" "$_SS_POLL_SECS" self_schedule_target_capability_ready; then
      ok "    trigger daemon armed & ticking (evidence: $_SS_CAP_EVIDENCE)"
    else
      self_schedule_diagnostics
      fail "Self-schedule: the trigger daemon never armed within ${_SS_CAP_TIMEOUT_SECS}s (no live last_tick). A schedule created now would not fire. 10f is co-gated on E2E_SCHEDULER_CHECK=on (10d) — check the scheduler plugin boot-installed and the daemon armed."
    fi

    # ── 10f.B.2 create with EXPLICIT workspace_id → lands on OWN grid ──
    _SS_ARGS_EXPLICIT=$(SS_WS="$PARENT_ID" SS_NM="$_SS_SN" python3 -c 'import json,os; print(json.dumps({"workspace_id":os.environ["SS_WS"],"name":os.environ["SS_NM"],"cron_expr":"* * * * *","prompt":"e2e self-schedule explicit-id probe","enabled":True}))')
    log "    LEG B(i): create_schedule with EXPLICIT workspace_id=$PARENT_ID name='$_SS_SN' via self-mode mcp-server ($_SS_MCP_SPEC) on $_SS_SELF_CID, retrying only a proven UUID DB-misroute for up to ${_SS_CREATE_TIMEOUT_SECS}s."
    if self_schedule_create_until_own_grid "$_SS_ARGS_EXPLICIT" "$_SS_SN"; then
      ok "    LEG B(i): explicit-id schedule '$_SS_SN' landed on the OWN volume grid ($_SS_SELF_CID:/configs/schedules/schedules.yaml)"
    else
      _ss_create_rc=$?
      self_schedule_diagnostics
      if [ "$_ss_create_rc" = "2" ]; then
        fail "LEG B(i): explicit-id create_schedule failed before OWN-grid confirmation ($_SS_CREATE_DETAIL). The captured JSON-RPC payload is in the diagnostics above. A tool-error/error is not a capability-cache race and remains a hard failure."
      fi
      fail "LEG B(i): explicit-id schedule '$_SS_SN' never appeared on the OWN grid within ${_SS_CREATE_TIMEOUT_SECS}s ($_SS_CREATE_DETAIL). A UUID+missing-grid result was retried through the bounded capability-cache window; volume/unclear ids were poll-only to avoid duplicate creates."
    fi

    # ── 10f.B.3 create with OMITTED workspace_id → SELF-RESOLVES + lands ──
    _SS_ARGS_OMIT=$(SS_NM="$_SS_SN_OMIT" python3 -c 'import json,os; print(json.dumps({"name":os.environ["SS_NM"],"cron_expr":"* * * * *","prompt":"e2e self-schedule omit-id probe","enabled":True}))')
    log "    LEG B(ii): create_schedule with workspace_id OMITTED name='$_SS_SN_OMIT' — must SELF-RESOLVE to the own workspace and land, retrying only a proven UUID DB-misroute for up to ${_SS_CREATE_TIMEOUT_SECS}s."
    if self_schedule_create_until_own_grid "$_SS_ARGS_OMIT" "$_SS_SN_OMIT"; then
      ok "    LEG B(ii): omit-id schedule '$_SS_SN_OMIT' SELF-RESOLVED to the own workspace and landed on the OWN grid"
    else
      _ss_create_rc=$?
      self_schedule_diagnostics
      if [ "$_ss_create_rc" = "2" ]; then
        fail "LEG B(ii): omit-id create_schedule failed before OWN-grid confirmation ($_SS_CREATE_DETAIL). The self-mode server must default workspace_id to the container's own WORKSPACE_ID; a tool-error/error is a hard self-resolution/auth failure, not a retry signal."
      fi
      fail "LEG B(ii): omit-id schedule '$_SS_SN_OMIT' never appeared on the OWN grid within ${_SS_CREATE_TIMEOUT_SECS}s ($_SS_CREATE_DETAIL). A UUID+missing-grid result was retried through the bounded capability-cache window; volume/unclear ids were poll-only to avoid duplicate creates."
    fi

    # ── 10f.B.4 the explicit-id self-created schedule FIRES (reuse 10d) ──
    log "    LEG B: polling fire evidence for '$_SS_SN' for up to ${_SS_TIMEOUT_SECS}s (reuses 10d's log/history verification; poll=${_SS_POLL_SECS}s)."
    if idle_digest_wait "$_SS_TIMEOUT_SECS" "$_SS_POLL_SECS" self_schedule_fire_probe "$_SS_SN"; then
      ok "    LEG B: self-created schedule '$_SS_SN' autonomously FIRED (evidence: $_SS_FIRED)"
    else
      self_schedule_diagnostics
      fail "LEG B: self-created schedule '$_SS_SN' never fired within ${_SS_TIMEOUT_SECS}s — it landed on the grid but the trigger daemon never delivered a self-scheduled turn (scheduler co-gate / trigger-lane leg)."
    fi

    # ── 10f.B.5 NEG CONTROL: FOREIGN workspace_id rejected + never arrives ──
    _SS_FWS="${CHILD_ID:-00000000-0000-4000-8000-0000000ff0f0}"
    _SS_ARGS_FOREIGN=$(SS_WS="$_SS_FWS" SS_NM="$_SS_FN" python3 -c 'import json,os; print(json.dumps({"workspace_id":os.environ["SS_WS"],"name":os.environ["SS_NM"],"cron_expr":"* * * * *","prompt":"e2e self-schedule foreign-id neg control","enabled":True}))')
    log "    LEG B NEG CONTROL: create_schedule with FOREIGN workspace_id=$_SS_FWS name='$_SS_FN' — must be REJECTED (tool error / core 401) and never arrive on any grid."
    self_schedule_invoke_create "$_SS_ARGS_FOREIGN"
    case "$_SS_CLASS" in
      tool-error|error)
        ok "    LEG B NC: foreign-id create was REJECTED at the tool/auth boundary (class=$_SS_CLASS) — the self box carries only the per-workspace token, so core WorkspaceAuth 401s a foreign id"
        ;;
      *)
        self_schedule_diagnostics
        fail "LEG B NC: foreign-id create_schedule was NOT rejected (class=$_SS_CLASS) — a create against a FOREIGN workspace_id must fail (tool error / core 401 from WorkspaceAuth). A non-error here means the self box could mutate another workspace's grid — a cross-tenant auth regression."
        ;;
    esac
    log "    LEG B NC: settling ${_SS_SETTLE_SECS}s, then asserting '$_SS_FN' arrived on NO mol-ws grid (container-inspectable non-arrival)."
    sleep "$_SS_SETTLE_SECS"
    if self_schedule_any_grid_has "$_SS_FN"; then
      self_schedule_diagnostics
      fail "LEG B NC: the foreign-named schedule '$_SS_FN' APPEARED on a mol-ws volume grid despite the rejected create — the foreign create mutated a grid it must never reach. This is a hard cross-tenant isolation failure."
    else
      ok "    LEG B NC: '$_SS_FN' is absent from every mol-ws volume grid — the rejected foreign create never mutated any grid (isolation holds)"
    fi

    ok "    10f self-schedule tool: LEG A injector proof + LEG B (explicit-id, omit-id self-resolve, fire) + foreign-id/org-key neg controls all PASSED"

    unset -f self_schedule_adapter_config_path self_schedule_adapter_config_format \
      self_schedule_resolve_own_container \
      self_schedule_own_grid_has self_schedule_any_grid_has self_schedule_fire_probe \
      self_schedule_assert_ordinary_box self_schedule_injector_proof \
      self_schedule_invoke_create self_schedule_diagnostics
  else
    fail "E2E_SELF_SCHEDULE_CHECK=on requires a usable Docker CLI and daemon to drive the self-mode create_schedule tool and inspect grid/fire evidence."
  fi
fi

# ─── 11. Teardown runs via trap ────────────────────────────────────────
# Fail-closed-on-skip: before declaring PASS, assert (when CI demanded a live
# run) that every load-bearing lifecycle milestone actually fired. A run that
# reaches here without provision→online→A2A having truly happened exits 5
# instead of reporting green. Teardown still runs (EXIT trap) on that exit.
require_live_or_die
log "11/11 All checks passed. Teardown runs via EXIT trap."
ok "═══ STAGING $MODE-SAAS E2E PASSED ═══"
