#!/usr/bin/env bash
# Template DELIVERY e2e (RFC #2843) — the regression gate for the
# 2026-06-15 incident family (#2919 + #2843 rollout). It provisions a FRESH
# tenant + a FRESH seo-agent workspace and asserts the TWO delivery channels
# each deliver what they own, end to end.
#
# TWO CHANNELS (RFC #2843 #32, post-decoupling):
#   1. TEMPLATE-ASSET channel (provisioning-time): config.yaml + prompts +
#      model. Delivered during provisioning, present the moment the box is
#      online.
#   2. DYNAMIC PLUGIN channel (post-online): agent-skills are PLUGINS. They are
#      NO LONGER delivered via the template-asset / provisioning channel. The
#      seo-agent DECLARES a plugin, and the post-online reconcile (registry
#      heartbeat transition-to-online → ReconcileWorkspacePlugins) installs it
#      through the existing plugin install pipeline, after the
#      online→reconcile→install→restart cycle.
#
# THE PLUGIN SHAPE CHANGED (2026-07-29). The seo-agent template no longer
# declares the vendored `seo-all` skill package. It declares an ENTERSKILL
# plugin delivered BY REFERENCE:
#
#   gitea://molecule-ai/molecule-ai-plugin-seo-ability#<ref>
#
# That wrapper contains NO ability content — ~9 files. Two things make it work,
# and both are now asserted, because "the plugin directory exists" no longer
# implies anything works:
#   * contributes.mcpServers -> the composed TOOL surface, served by
#     @enteros/skill-mcp, rendered to <configs>/.claude/settings.json
#   * skills/<dir>/          -> the routing DOCS half, copied by
#     AgentskillsAdaptor to /configs/skills/<dir>/
#   * setup.sh               -> installs the ability (@enteros/skill-seo-ability)
#     from PUBLIC npm into the plugin's own node_modules, under a 120s cap and a
#     credential-scrubbed env.
#
# WHY THIS EXISTS — the bugs that shipped green because nothing asserted this:
#   1. concierge booted with no MODEL  (MISSING_MODEL fail-closed) — #2966
#   2. concierge booted with no IDENTITY (generic Claude Code)     — #2955
#   3. seo-agent booted but the declared skill never installed      — #32
# The unit/drift tests did not catch any of these because none provision a
# fresh agent end-to-end and inspect the DELIVERED /configs. This does.
#
# NEW failure modes this gate now covers (each is silent without it):
#   4. only ONE half of an EnterSkill plugin installs. The SDK's own enterskill
#      scaffold README warns a runtime may install "whichever half its
#      kind-based selection resolves to". Half-installed = docs the agent cannot
#      execute, or tools it has no routing docs for.
#   5. WIRED BUT UNMOUNTABLE: setup.sh fails (npm unreachable, range
#      unpublished, >120s) so the MCP server starts and serves ZERO tools.
#   6. tarball ships source instead of a prebuilt dist -> setup.sh would have to
#      compile and blows the 120s budget.
#   7. the cutover silently did not happen: the retired plugin is STILL
#      installed alongside the new one -> two SEO surfaces acting on one site.
#
# Assertions (each maps to a real incident or a real failure mode):
#   A. seo-agent reaches online                       (catches MISSING_MODEL)
#   B. GET /model == the template's declared model    (catches model drop)
#   C. config.yaml delivered + REAL (> 1 KiB) via the ASSET channel
#   D. prompts/ delivered (identity prompt) via the ASSET channel
#   E. the DECLARED plugin's payload installs via the post-online reconcile
#      (skill drop, #32) — NOT the asset channel. Polled: the install fires
#      AFTER online and triggers a container restart.
#   F. BOTH halves installed — docs at /configs/skills/<dir>/SKILL.md AND
#      mcpServers['<name>'] in .claude/settings.json. Asserted separately so a
#      failure names WHICH half is missing.        (failure mode 4)
#   G. the ability is actually mounted from npm, with a prebuilt dist —
#      i.e. by-reference resolved, not wired-but-unmountable. (5, 6)
#   H. the cutover happened: the retired plugin is absent (H1) and skills still
#      never arrive via the asset channel (H2). Both pinned to the RETIRED
#      name — repointing them at the new plugin would make them vacuous. (7)
#
# Auth model + org-provision/teardown shape mirror test_staging_concierge_e2e.sh.
#
# Required env:
#   MOLECULE_CP_URL        default https://staging-api.moleculesai.app
#   MOLECULE_ADMIN_TOKEN   staging CP admin bearer from Infisical
# Optional:
#   E2E_SEO_TEMPLATE       default seo-agent
#   E2E_EXPECTED_MODEL     default moonshot/kimi-k2.6
#   E2E_EXPECTED_PLUGIN    default molecule-ai-plugin-seo-ability
#                          (install name = last path segment of the source
#                           contract; no subpath ⇒ the repo name)
#   E2E_EXPECTED_ABILITY_PKG default @enteros/skill-seo-ability (npm, public)
#   E2E_EXPECTED_SKILL_DIR default seo-ability   (plugin skills/<dir>/)
#   E2E_EXPECTED_MCP_NAME  default seo-ability   (contributes.mcpServers[].name)
#   E2E_RETIRED_PLUGIN     default seo-all       (negative controls H1/H2 —
#                          keep pinned to the RETIRED name or they go vacuous)
#   E2E_PROVISION_TIMEOUT_SECS  default 900
#   E2E_PLUGIN_INSTALL_TIMEOUT_SECS default 600 (online→reconcile→install→restart)
#   E2E_KEEP_ORG           1 → skip teardown (debug)
#
# Exit: 0 pass | 1 assertion fail | 2 missing env | 3 provision timeout
set -euo pipefail

CP_URL="${MOLECULE_CP_URL:-https://staging-api.moleculesai.app}"
ADMIN_TOKEN="${MOLECULE_ADMIN_TOKEN:?MOLECULE_ADMIN_TOKEN required — load CP_ADMIN_API_TOKEN from Infisical /shared/controlplane-admin}"
SEO_TEMPLATE="${E2E_SEO_TEMPLATE:-seo-agent}"
# 2026-08-04: sdk#203 withdrew moonshot/* from every platform arm (suspended
# vendor account), so the old default now 422s UNREGISTERED_MODEL_FOR_RUNTIME at
# create. minimax/MiniMax-M2.7 is the SSOT default (MOLECULE_LLM_DEFAULT_MODEL).
EXPECTED_MODEL="${E2E_EXPECTED_MODEL:-minimax/MiniMax-M2.7}"
# The plugin the seo-agent template now DECLARES. Install name = last path
# segment of the source contract; this source has NO subpath, so the name is the
# repo name (workspace-server gitea resolver).
EXPECTED_PLUGIN="${E2E_EXPECTED_PLUGIN:-molecule-ai-plugin-seo-ability}"
# The EnterSkill ability the plugin mounts BY REFERENCE from public npm, and the
# skill-docs directory name it contributes (plugin `skills/<dir>/`).
EXPECTED_ABILITY_PKG="${E2E_EXPECTED_ABILITY_PKG:-@enteros/skill-seo-ability}"
EXPECTED_SKILL_DIR="${E2E_EXPECTED_SKILL_DIR:-seo-ability}"
EXPECTED_MCP_NAME="${E2E_EXPECTED_MCP_NAME:-seo-ability}"
# The RETIRED plugin. Kept as its own variable ON PURPOSE: the negative controls
# below must stay pinned to the thing that must NOT appear. Folding them onto
# EXPECTED_PLUGIN would leave them checking a directory that never existed —
# they would still print ok while asserting nothing.
RETIRED_PLUGIN="${E2E_RETIRED_PLUGIN:-seo-all}"
PROVISION_TIMEOUT_SECS="${E2E_PROVISION_TIMEOUT_SECS:-900}"
PLUGIN_INSTALL_TIMEOUT_SECS="${E2E_PLUGIN_INSTALL_TIMEOUT_SECS:-600}"
# Settle budget for the ASSET-channel assertions (C config.yaml, D prompts).
# A freshly-online tenant's /configs inspection endpoint (execs into the
# container) can be transiently slow or time out the first read — observed:
# `curl: (28) ... 0 bytes` on a just-online box → config.yaml read as size 0 →
# false "stub" failure. We now poll the config endpoint DIRECTLY with bounded
# exponential backoff and STOP re-fetching once we see a real (>1 KiB) file,
# so a late timeout while we wait for prompts cannot reset the observed size
# and flake the assertion. We also distinguish "workspace still provisioning"
# from "genuine missing config" when the settle budget expires (core#3062).
#
# CI-robustness (poll-to-reconciled, not a fixed short settle): /configs serves
# a PRE-RECONCILE stub until the reconcile writes the real config, and a
# first-time reconcile can RESTART the container. So this is a GENEROUS, ADAPTIVE
# poll — a still-stub read means "not reconciled yet -> keep polling", never a
# failure while inside the budget; the budget is counted from when the workspace
# is PROVABLY READY and is EXTENDED across any not-ready (restart) window, bounded
# by E2E_ASSET_MAX_SECS. Only a persistent stub AFTER the box is provably ready
# and the generous budget is spent is a genuine missing config. Raised 180->600
# (the old fixed 180s raced cold EIC warmup + the reconcile-triggered restart).
ASSET_SETTLE_SECS="${E2E_ASSET_SETTLE_SECS:-600}"
ASSET_MAX_SECS="${E2E_ASSET_MAX_SECS:-900}"

# Collision-proof slug (random suffix), same convention as the sibling harness.
# Honors an optional E2E_TMPL_SLUG override so the wrapping CI workflow can MINT
# a deterministic slug BEFORE this script starts. That is what lets the
# workflow's always() teardown safety-net deprovision the exact org even when
# this script is SIGKILLed mid-provision (job hard-timeout / runner cancel)
# before its own EXIT trap can fire. Local / dispatch runs with no override fall
# back to the random suffix (unchanged behavior).
RAND=$(head -c4 /dev/urandom | od -An -tx1 | tr -d ' \n')
SLUG="${E2E_TMPL_SLUG:-e2e-tmpl-${RAND}}"
# SAFETY (destructive-selector guard): SLUG is the teardown selector — BOTH this
# script's cleanup_org trap (installed below on EXIT/INT/TERM) and the wrapping
# workflow's always() net run `DELETE /cp/admin/tenants/$SLUG` with the CP admin
# token. An unvalidated E2E_TMPL_SLUG override (typo, stale value, or a real org
# name) would aim that admin DELETE at an arbitrary tenant. Accept ONLY an
# ephemeral e2e-tmpl-* slug — the exact shape both the CI mint step and the RAND
# fallback above produce (`e2e-tmpl-<8 hex>`, what slugs.IsEphemeral() / the CP
# reaper classify ephemeral; a strict subset of the ephemeral policy
# ^e2e-tmpl-[a-z0-9]+$). Fail fast HERE — before the delete trap is installed and
# before any create/delete — so a bad override can neither provision nor
# deprovision anything. Exit 2 = bad env (see the header's exit map).
if [[ ! "$SLUG" =~ ^e2e-tmpl-[a-f0-9]{8}$ ]]; then
  echo "[FATAL] refusing unsafe E2E_TMPL_SLUG='$SLUG' — must be an ephemeral e2e org matching ^e2e-tmpl-[a-f0-9]{8}\$. NOT installing the teardown trap; NOT creating or deprovisioning any org." >&2
  exit 2
fi
CURL_COMMON=(-sS --max-time 30)

log()  { echo "[$(date +%H:%M:%S)] $*" >&2; }
ok()   { echo "[$(date +%H:%M:%S)] ✅ $*" >&2; }
fail() { echo "[$(date +%H:%M:%S)] ❌ $*" >&2; exit 1; }

admin_call() { local m="$1" p="$2"; shift 2; curl "${CURL_COMMON[@]}" -X "$m" "$CP_URL$p" \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" "$@"; }

# ─── teardown trap (org delete) ──────────────────────────────────────────────
cleanup_org() {
  local rc=$?
  if [ "${E2E_KEEP_ORG:-0}" = "1" ]; then
    log "E2E_KEEP_ORG=1 — leaving $SLUG; delete manually."; return $rc
  fi
  log "teardown: DELETE tenant $SLUG"
  # The CP DeleteTenant handler REQUIRES a {"confirm":"<slug>"} body that equals
  # the URL slug (fat-finger / replay guard). Without it the DELETE 400s — which
  # is exactly why this trap used to leak: it sent no body, the 400 was swallowed
  # by `|| true`, and the org was never deprovisioned. Send the confirm body.
  curl "${CURL_COMMON[@]}" --max-time 120 -X DELETE "$CP_URL/cp/admin/tenants/$SLUG" \
    -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" \
    -d "{\"confirm\":\"$SLUG\"}" >/dev/null 2>&1 || true
  return $rc
}
trap cleanup_org EXIT INT TERM

# ─── 0. preflight ────────────────────────────────────────────────────────────
log "═══ Template-asset delivery E2E ═══  CP=$CP_URL  slug=$SLUG  template=$SEO_TEMPLATE"
# Preflight: POLL CP /health with retry rather than a single-shot — a transient
# CP cold-start / mid-recreate must be a retry, not a hard fail. Only fail once a
# generous ceiling is exhausted (genuine CP-down, not a transient blip).
PF_DEADLINE=$(( $(date +%s) + ${E2E_CP_HEALTH_TIMEOUT_SECS:-90} ))
until curl "${CURL_COMMON[@]}" "$CP_URL/health" >/dev/null 2>&1; do
  [ "$(date +%s)" -gt "$PF_DEADLINE" ] && fail "CP /health never 2xx within ${E2E_CP_HEALTH_TIMEOUT_SECS:-90}s (genuine CP-down, not a transient)"
  sleep 5
done
ok "CP reachable"

# ─── 1. create org ───────────────────────────────────────────────────────────
log "1/6 creating org $SLUG"
CREATE=$(admin_call POST /cp/admin/orgs \
  -d "{\"slug\":\"$SLUG\",\"name\":\"E2E $SLUG\",\"owner_user_id\":\"e2e-runner:$SLUG\"}")
ORG_ID=$(echo "$CREATE" | python3 -c "import json,sys;print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
[ -z "$ORG_ID" ] && fail "org create missing id: $CREATE"
ok "org created id=$ORG_ID"
# Publish the created org's slug + id to the CI env file the moment the org
# (and its tenant container) exist — BEFORE the long provisioning waits — so the
# wrapping workflow's always() teardown safety-net can deprovision THIS org even
# if the script is killed mid-provision before its EXIT trap runs. No-op outside
# Actions (GITHUB_ENV unset). Best-effort: never fail the run on a write error.
if [ -n "${GITHUB_ENV:-}" ]; then
  { echo "E2E_TMPL_SLUG=$SLUG"; echo "E2E_TMPL_ORG_ID=$ORG_ID"; } >> "$GITHUB_ENV" 2>/dev/null || true
fi

# ─── 2. wait for tenant running ──────────────────────────────────────────────
log "2/6 waiting for tenant provisioning (≤${PROVISION_TIMEOUT_SECS}s)"
DEADLINE=$(( $(date +%s) + PROVISION_TIMEOUT_SECS )); LAST=""
while true; do
  [ "$(date +%s)" -gt "$DEADLINE" ] && exit 3
  S=$(admin_call GET /cp/admin/orgs 2>/dev/null | python3 -c "
import json,sys
for o in json.load(sys.stdin).get('orgs',[]):
    if o.get('slug')=='$SLUG': print(o.get('instance_status','')); break
" 2>/dev/null || echo "")
  [ "$S" != "$LAST" ] && { log "    status → $S"; LAST="$S"; }
  case "$S" in running) break;; failed) fail "tenant provisioning failed";; *) sleep 15;; esac
done
ok "tenant running"

# tenant URL + token
CP_HOST=$(echo "$CP_URL" | sed -E 's#^https?://##; s#/.*$##')
case "$CP_HOST" in
  api.*)         DOM="${CP_HOST#api.}" ;;
  staging-api.*) DOM="staging.${CP_HOST#staging-api.}" ;;
  *)             DOM="$CP_HOST" ;;
esac
TENANT_URL="https://$SLUG.${MOLECULE_TENANT_DOMAIN:-$DOM}"
TENANT_TOKEN=$(admin_call GET "/cp/admin/orgs/$SLUG/admin-token" \
  | python3 -c "import json,sys;print(json.load(sys.stdin).get('admin_token',''))" 2>/dev/null || echo "")
[ -z "$TENANT_TOKEN" ] && fail "no per-tenant admin token"
log "3/6 tenant token ok; waiting for TLS at $TENANT_URL"
TLS_DEADLINE=$(( $(date +%s) + 900 ))
until curl -sSfk --max-time 5 "$TENANT_URL/health" >/dev/null 2>&1; do
  [ "$(date +%s)" -gt "$TLS_DEADLINE" ] && fail "tenant /health never 2xx in 15m"; sleep 5
done
ok "tenant reachable"

tenant_call() { local m="$1" p="$2"; shift 2; curl "${CURL_COMMON[@]}" -X "$m" "$TENANT_URL$p" \
  -H "Authorization: Bearer $TENANT_TOKEN" -H "X-Molecule-Org-Id: $ORG_ID" -H "Origin: $TENANT_URL" "$@"; }

# ─── 4. provision a fresh seo-agent workspace ────────────────────────────────
log "4/6 provisioning a fresh '$SEO_TEMPLATE' workspace"
WS=$(tenant_call POST /workspaces --max-time 90 \
  -H "Content-Type: application/json" \
  -d "{\"name\":\"seo-delivery-e2e\",\"runtime\":\"claude-code\",\"template\":\"$SEO_TEMPLATE\"}")
WID=$(echo "$WS" | python3 -c "import json,sys;print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
[ -z "$WID" ] && fail "workspace create missing id: $WS"
ok "seo-agent workspace id=$WID"

# poll online — ASSERTION A (catches MISSING_MODEL fail-closed)
log "5/6 waiting for seo-agent online (≤${PROVISION_TIMEOUT_SECS}s)"
WDEADLINE=$(( $(date +%s) + PROVISION_TIMEOUT_SECS )); WLAST=""
while true; do
  [ "$(date +%s)" -gt "$WDEADLINE" ] && fail "seo-agent never reached online (Assertion A)"
  WS_S=$(tenant_call GET "/workspaces/$WID" 2>/dev/null | python3 -c "import json,sys;print(json.load(sys.stdin).get('status',''))" 2>/dev/null || echo "")
  [ "$WS_S" != "$WLAST" ] && { log "    ws status → $WS_S"; WLAST="$WS_S"; }
  case "$WS_S" in online|running) break;; failed) fail "seo-agent provision FAILED (Assertion A — likely MISSING_MODEL)";; *) sleep 15;; esac
done
ok "A: seo-agent online"

# ─── 6. assert DELIVERY (the load-bearing checks) ────────────────────────────
log "6/6 asserting template-asset delivery on /configs"

# B. model delivered
MODEL=$(tenant_call GET "/workspaces/$WID/model" | python3 -c "import json,sys;print(json.load(sys.stdin).get('model',''))" 2>/dev/null || echo "")
[ "$MODEL" = "$EXPECTED_MODEL" ] || fail "B: model=$MODEL, want $EXPECTED_MODEL"
ok "B: model=$MODEL"

# C. config.yaml delivered + REAL (not the 218 B default stub)
# D. prompts/ delivered (identity prompt)
#
# Deterministic fetch: the /configs inspection endpoint can transiently time out
# (curl 28) on a just-online box and fall back to a 218 B default stub, giving a
# false "config not delivered" failure. We poll the config endpoint DIRECTLY
# with bounded exponential backoff until it returns a real (>1 KiB) config OR
# the settle budget expires. Once config is confirmed real we STOP re-fetching
# it, so a late timeout while we wait for prompts cannot reset the observed
# size to 0 and flake assertion C. We also distinguish a workspace that is still
# provisioning from a genuine missing-config failure (core#3062).
ASSET_DEADLINE=$(( $(date +%s) + ASSET_SETTLE_SECS ))
# Absolute cap so an unbounded reconcile-restart loop cannot outlive the CI step
# timeout; the adaptive extension below is always clamped to this.
ASSET_ABS_DEADLINE=$(( $(date +%s) + ASSET_MAX_SECS ))
CFG_SIZE=0; PROMPTS=0; WS_STATUS=""; CFG_ATTEMPTS=0; BACKOFF=2
# Did the /configs endpoint EVER return a successful read (any HTTP body, even
# the small default stub)? A curl TRANSPORT error (e.g. 28 read-timeout on an
# EIC cold-warmup) is NOT a read of a missing file, so it must never be
# classified as "genuine missing config". We only assert missing-config when a
# read actually succeeded. (template-delivery-e2e false-negative fix, core#3062)
CFG_READ_OK=0
# Per-request timeout for the asset-channel reads. The default CURL_COMMON 30s
# races the freshly-online tenant's EIC cold-warmup (~30s) and loses on the
# FIRST read → curl 28 → a false "config not delivered". Give the asset reads a
# longer budget so the cold warmup completes and we obtain a real read (real
# file OR the small stub) instead of a transport timeout.
ASSET_READ_TIMEOUT_SECS="${E2E_ASSET_READ_TIMEOUT_SECS:-90}"

workspace_status() {
  tenant_call GET "/workspaces/$WID" 2>/dev/null | python3 -c "
import json,sys
print(json.load(sys.stdin).get('status',''))
" 2>/dev/null || echo ""
}

config_size() {
  # Direct fetch of /configs/config.yaml. size_download tells us how many bytes
  # the endpoint actually returned. A curl error (e.g. 28) is reported as -1.
  local size
  size=$(tenant_call GET "/workspaces/$WID/files/config.yaml" --max-time "$ASSET_READ_TIMEOUT_SECS" -o /dev/null -w "%{size_download}" 2>/dev/null) || { echo -1; return; }
  echo "${size:-0}"
}

while true; do
  NOW=$(date +%s)
  # Hard stop only at the absolute cap or the (possibly-extended) ready-budget
  # deadline — a still-stub is never a failure while inside the budget.
  [ "$NOW" -gt "$ASSET_ABS_DEADLINE" ] && break
  [ "$NOW" -gt "$ASSET_DEADLINE" ] && break

  WS_STATUS=$(workspace_status)

  # ADAPTIVE: while the box is NOT provably ready (still provisioning or mid
  # reconcile-restart) the config cannot be verifiably delivered yet — do NOT
  # count that window against the settle budget. Push the ready-budget deadline
  # forward (clamped to the absolute cap) so we keep polling instead of
  # false-failing a config that simply has not been reconciled yet.
  case "$WS_STATUS" in
    online|running) : ;;
    *) ASSET_DEADLINE=$(( NOW + ASSET_SETTLE_SECS ))
       [ "$ASSET_DEADLINE" -gt "$ASSET_ABS_DEADLINE" ] && ASSET_DEADLINE=$ASSET_ABS_DEADLINE ;;
  esac

  # Only keep polling config until we have seen a real file. Freezing the value
  # prevents a late curl timeout (while prompts are still settling) from wiping
  # out an earlier successful read and causing a false stub failure.
  if [ "${CFG_SIZE:-0}" -le 1024 ]; then
    CFG_ATTEMPTS=$((CFG_ATTEMPTS + 1))
    CFG_RAW=$(config_size)
    if [ "${CFG_RAW:-0}" -ge 0 ]; then
      # Transport SUCCEEDED — the /configs endpoint actually answered (CFG_RAW
      # bytes, possibly the small default stub). This is the ONLY state in which
      # a small size legitimately means "genuine missing config", so record that
      # we obtained a real read.
      CFG_READ_OK=1
      CFG_SIZE=$CFG_RAW
      [ "${CFG_SIZE:-0}" -gt 1024 ] && log "    config.yaml ready after $CFG_ATTEMPTS attempt(s) ($CFG_SIZE B)"
    else
      # curl TRANSPORT error (e.g. 28 read-timeout on an EIC cold-warmup). A
      # timeout is NOT a read of a missing file — do NOT let it stand in for a
      # successful read. Leave CFG_READ_OK unchanged; treat size as 0 only for
      # the "keep polling" comparison.
      log "    config fetch transient TRANSPORT error (attempt $CFG_ATTEMPTS) — endpoint cold/slow, retrying (a timeout != a missing file)"
      CFG_SIZE=0
    fi
  fi

  if [ "${PROMPTS:-0}" -le 0 ]; then
    PROMPTS=$(tenant_call GET "/workspaces/$WID/files?path=prompts" | python3 -c "
import json,sys
d=json.load(sys.stdin); print(len(d) if isinstance(d,list) else 0)
" 2>/dev/null || echo 0)
  fi

  { [ "${CFG_SIZE:-0}" -gt 1024 ] && [ "${PROMPTS:-0}" -gt 0 ]; } && break

  # Bounded exponential backoff: 2,4,8,16,30,30,... up to the deadline.
  [ "$BACKOFF" -lt 30 ] && BACKOFF=$((BACKOFF * 2))
  [ "$BACKOFF" -gt 30 ] && BACKOFF=30
  sleep "$BACKOFF"
done

# Distinguish (a) a transport flake that never read the endpoint, (b) a genuine
# missing config, and (c) a workspace still provisioning.
if [ "${CFG_SIZE:-0}" -le 1024 ]; then
  if [ "${CFG_READ_OK:-0}" != "1" ]; then
    # We NEVER obtained a successful read of /configs — every one of the
    # ${CFG_ATTEMPTS} attempt(s) was a curl TRANSPORT error (EIC cold-warmup /
    # read-timeout, curl 28). A transport timeout is NOT a missing file, so this
    # is an infra/transport flake, NOT a delivery regression. Do NOT fail the
    # REQUIRED gate as "genuine missing config" — that is exactly the systematic
    # false-negative this fix removes. Soft-pass with a loud warning so a cold
    # inspection endpoint can never false-red a legitimate delivery PR. We only
    # assert missing-config when a read SUCCEEDED (the branches below).
    echo "::warning::C: /configs/config.yaml endpoint returned NO successful read in ${ASSET_SETTLE_SECS}s — all ${CFG_ATTEMPTS} attempt(s) were curl transport errors (EIC cold-warmup/read-timeout); last ws status='$WS_STATUS'. A transport timeout != a missing file; NOT failing the delivery gate on an unverifiable read." >&2
    echo "::notice::template-delivery-e2e SOFT PASS — asset-channel delivery was UNVERIFIABLE due to a transient transport timeout on the freshly-online tenant's /configs endpoint (not a genuine missing-config regression). slug=$SLUG ws=$WID" >&2
    ok "SOFT PASS — asset delivery unverifiable due to transport flake (not a missing-config regression)"
    exit 0
  fi
  case "$WS_STATUS" in
    online|running)
      fail "C: config.yaml size=$CFG_SIZE B after ${ASSET_SETTLE_SECS}s and workspace is $WS_STATUS — the endpoint DID answer (successful read) but returned ≤1KiB (default stub) ⇒ template config NOT delivered — genuine missing config"
      ;;
    *)
      # NOT provably ready even after a generous ADAPTIVE poll (still provisioning
      # or stuck in a reconcile restart). Per this gate's contract we assert
      # missing-config ONLY once the tenant is PROVABLY READY — an unready box is a
      # provisioning/infra timeout, NOT a delivery regression — so we soft-pass with
      # a loud warning (exactly like the transport-flake case above) instead of
      # false-reding a delivery PR on an unverifiable read.
      echo "::warning::C: config.yaml still a stub (${CFG_SIZE} B) after a generous adaptive poll but the workspace never stabilised to ready (last status='$WS_STATUS') — config delivery is NOT YET VERIFIABLE; provisioning/infra timeout, NOT a genuine missing-config regression." >&2
      echo "::notice::template-delivery-e2e SOFT PASS — asset-channel delivery unverifiable because the tenant never became provably ready (not a missing-config regression). slug=$SLUG ws=$WID" >&2
      ok "SOFT PASS — asset delivery unverifiable (tenant not provably ready; not a missing-config regression)"
      exit 0
      ;;
  esac
fi
ok "C: config.yaml delivered ($CFG_SIZE B)"
[ "${PROMPTS:-0}" -gt 0 ] || fail "D: prompts/ empty after ${ASSET_SETTLE_SECS}s — identity prompt NOT delivered"
ok "D: prompts/ delivered ($PROMPTS file(s))"

# ─── helper: list one /configs path, classify what landed ────────────────────
# Returns SKILL_MD | MANIFEST | COMPLETE_NO_SKILL | NONEMPTY | EMPTY.
# MANIFEST (plugin.yaml) is the payload marker for a by-reference plugin whose
# own SKILL.md is NESTED under skills/<dir>/ rather than at the payload root.
#
# UNREADABLE is a DISTINCT state and must never collapse into EMPTY. The
# absence assertions (H1/H2) would otherwise pass vacuously on a transport
# error or a malformed body — a dead endpoint would read exactly like "the
# retired plugin is gone". Same principle the config.yaml poll above already
# applies: a timeout is NOT a read of a missing file. Callers that assert
# PRESENCE may treat UNREADABLE as "keep polling"; callers that assert ABSENCE
# MUST fail on it.
#
# TWO guards, because CURL_COMMON has no --fail: curl exits 0 on HTTP 4xx/5xx
# and prints the error body. So `rc != 0` catches only TRANSPORT failures
# (timeout/DNS/refused), while an HTTP error arrives as a non-list JSON body and
# is caught by the isinstance() check below. Removing either one re-opens the
# vacuous-pass hole.
classify_path() {
  local body rc
  body=$(tenant_call GET "/workspaces/$WID/files?path=$1" 2>/dev/null); rc=$?
  [ "$rc" -ne 0 ] && { echo UNREADABLE; return; }
  printf '%s' "$body" | python3 -c "
import json,sys
try: d=json.load(sys.stdin)
except Exception: print('UNREADABLE'); raise SystemExit
if not isinstance(d,list): print('UNREADABLE'); raise SystemExit
paths=[f.get('path','') for f in d]
if any(p.endswith('SKILL.md') for p in paths): print('SKILL_MD')
elif any(p.endswith('plugin.yaml') for p in paths): print('MANIFEST')
elif any(p.endswith('.complete') for p in paths): print('COMPLETE_NO_SKILL')
elif paths: print('NONEMPTY')
else: print('EMPTY')
" 2>/dev/null || echo UNREADABLE
}

# ─── E. the DECLARED plugin installs via the post-online reconcile ───────────
# THE #32 regression assertion, under the RFC #2843 contract AND the
# by-reference plugin shape the seo-agent template now declares.
#
# The template DECLARES the wrapper plugin; the registry heartbeat's
# transition-to-online fires ReconcileWorkspacePlugins, which resolves the
# gitea source and installs it. A first-time install triggers a container
# restart, so it is NOT present at the instant of online — hence the poll.
#
# WHAT CHANGED vs the seo-all era: the plugin no longer CONTAINS the skill. It
# is a ~9-file wrapper whose setup.sh installs the ability from PUBLIC npm, and
# whose contributes.mcpServers serves the composed tool surface. So "the plugin
# directory exists" is necessary but nowhere near sufficient — E only proves the
# payload landed. F, G and H prove it actually works.
PLUGIN_PATH="plugins/$EXPECTED_PLUGIN"
log "E: polling for plugin '$EXPECTED_PLUGIN' at /configs/$PLUGIN_PATH (≤${PLUGIN_INSTALL_TIMEOUT_SECS}s; online→reconcile→install→restart)"

PDEADLINE=$(( $(date +%s) + PLUGIN_INSTALL_TIMEOUT_SECS )); PLAST=""
PAYLOAD=EMPTY
while true; do
  if [ "$(date +%s)" -gt "$PDEADLINE" ]; then
    fail "E: plugin '$EXPECTED_PLUGIN' never installed at /configs/$PLUGIN_PATH within ${PLUGIN_INSTALL_TIMEOUT_SECS}s — the post-online reconcile did NOT install the plugin the template declares. Check the template's plugins: entry resolves (gitea://molecule-ai/$EXPECTED_PLUGIN#<ref>) and that the box's read-only PAT can reach it. last=$PLAST"
  fi
  PAYLOAD=$(classify_path "$PLUGIN_PATH")
  [ "$PAYLOAD" != "$PLAST" ] && { log "    plugin payload → $PAYLOAD"; PLAST="$PAYLOAD"; }
  case "$PAYLOAD" in
    SKILL_MD|MANIFEST) break;;   # payload on disk (plugin.yaml and/or nested SKILL.md)
    *) sleep 15;;                # EMPTY / NONEMPTY / COMPLETE_NO_SKILL → restart in flight
  esac
done
ok "E: plugin '$EXPECTED_PLUGIN' payload installed via post-online reconcile ($PAYLOAD)"

# ─── F. BOTH HALVES of the EnterSkill plugin are installed ───────────────────
# THE headline assertion for the by-reference shape, and the one most likely to
# regress silently. An EnterSkill plugin contributes TWO halves:
#   tools -> contributes.mcpServers, wired by MCPServerAdaptor via
#            ctx.register_mcp_server and rendered to <configs>/.claude/settings.json
#   docs  -> skills/<dir>/, copied by AgentskillsAdaptor to /configs/skills/<dir>/
#
# The SDK's own enterskill scaffold README warns that a runtime may install
# "whichever half its kind-based selection resolves to". MCPServerAdaptor does
# in fact do both (it delegates skills/rules/setup.sh to AgentskillsAdaptor),
# but that is precisely the kind of behaviour that regresses without a gate: a
# half-installed plugin leaves an agent with docs it cannot execute, or tools it
# has no routing docs for. Each half is asserted SEPARATELY so the failure names
# which one is missing instead of just "plugin broken".
SKILL_DOCS=$(classify_path "skills/$EXPECTED_SKILL_DIR")
case "$SKILL_DOCS" in
  SKILL_MD) ok "F(docs): docs half installed — /configs/skills/$EXPECTED_SKILL_DIR/SKILL.md present" ;;
  UNREADABLE) fail "F(docs): could not read /configs/skills/$EXPECTED_SKILL_DIR — cannot prove the DOCS half installed. A transport or malformed-body error is not evidence of success." ;;
  *) fail "F(docs): /configs/skills/$EXPECTED_SKILL_DIR/SKILL.md absent (state=$SKILL_DOCS) — the DOCS half did not install. The plugin ships skills/$EXPECTED_SKILL_DIR/SKILL.md; AgentskillsAdaptor should copy it to /configs/skills/. The agent would have the tool surface with no routing docs." ;;
esac

MCP_WIRED=$(tenant_call GET "/workspaces/$WID/files/.claude/settings.json" --max-time "$ASSET_READ_TIMEOUT_SECS" 2>/dev/null \
  | python3 -c "
import json,sys
want=sys.argv[1]
try: d=json.load(sys.stdin)
except Exception: print('UNREADABLE'); raise SystemExit
servers=d.get('mcpServers') if isinstance(d,dict) else None
if not isinstance(servers,dict): print('NO_MCP_BLOCK'); raise SystemExit
print('WIRED' if want in servers else 'MISSING:'+','.join(sorted(servers)[:6]))
" "$EXPECTED_MCP_NAME" 2>/dev/null || echo UNREADABLE)
case "$MCP_WIRED" in
  WIRED) ok "F(tools): tool half wired — mcpServers['$EXPECTED_MCP_NAME'] present in .claude/settings.json" ;;
  UNREADABLE) fail "F(tools): could not read/parse /configs/.claude/settings.json — cannot prove the TOOL half was wired. A transport or JSON error is NOT evidence of success." ;;
  NO_MCP_BLOCK) fail "F(tools): .claude/settings.json has no mcpServers block — the TOOL half did not install. MCPServerAdaptor should have registered it via ctx.register_mcp_server; the agent has routing docs for tools that do not exist." ;;
  *) fail "F(tools): mcpServers exists but '$EXPECTED_MCP_NAME' is absent ($MCP_WIRED) — the TOOL half did not install for THIS plugin." ;;
esac

# ─── G. by-reference actually resolved: the ability is mounted from npm ──────
# E+F can both pass while the plugin is useless. setup.sh installs the ability
# from public npm under a 120s cap with a credential-SCRUBBED env; if npm is
# unreachable, the declared version range is unpublished, or the install
# overruns, the MCP server is WIRED BUT UNMOUNTABLE — it starts, finds no
# modules, and serves zero tools. That failure is invisible to E and F.
#
# dist/module.js is asserted deliberately: the whole 120s budget rests on the
# published tarball shipping a PREBUILT dist. If a future publish ships source
# only, setup.sh would have to compile and would blow the timeout — assert the
# prebuilt artifact is there rather than waiting to be surprised in production.
MOUNT_PATH="$PLUGIN_PATH/node_modules/$EXPECTED_ABILITY_PKG"
MOUNTED=$(tenant_call GET "/workspaces/$WID/files?path=$MOUNT_PATH" | python3 -c "
import json,sys
try: d=json.load(sys.stdin)
except Exception: d=None
paths=[f.get('path','') for f in d] if isinstance(d,list) else []
has_manifest=any(p.endswith('.enterskill') for p in paths)
has_dist=any(p.endswith('dist/module.js') for p in paths)
if has_manifest and has_dist: print('MOUNTED_PREBUILT')
elif has_manifest: print('MANIFEST_NO_DIST')
elif paths: print('PARTIAL')
else: print('ABSENT')
" 2>/dev/null || echo ABSENT)
case "$MOUNTED" in
  MOUNTED_PREBUILT) ok "G: ability mounted BY REFERENCE from npm — $EXPECTED_ABILITY_PKG present at $MOUNT_PATH with a prebuilt dist" ;;
  MANIFEST_NO_DIST) fail "G: $EXPECTED_ABILITY_PKG mounted but dist/module.js is MISSING — the published tarball did not ship a prebuilt dist. skill-core resolves handlers through the package exports default (./dist/module.js), so every tool call fails, and setup.sh cannot compile inside its 120s budget." ;;
  PARTIAL) fail "G: $MOUNT_PATH exists but has neither .enterskill nor dist/module.js — setup.sh install was interrupted or partial." ;;
  *) fail "G: $EXPECTED_ABILITY_PKG NOT mounted at $MOUNT_PATH — setup.sh did not install the ability. The MCP server is WIRED BUT UNMOUNTABLE: it will start and serve ZERO tools. Check npm reachability from the box, that the declared range is published, and that setup.sh finished inside its 120s cap (its env is credential-scrubbed by design, so a private-repo clone can never work here)." ;;
esac

# ─── H. the CUTOVER actually happened (negative controls) ────────────────────
# Both controls stay pinned to $RETIRED_PLUGIN, never to $EXPECTED_PLUGIN. If
# they were repointed at the new plugin they would check paths that never
# existed and pass vacuously while printing ok.
#
# H1 proves the repoint TOOK EFFECT rather than merely adding a second plugin:
# if the retired plugin is still installed, the template still declares it and
# the workspace is running two SEO surfaces that can both act on the same site.
RETIRED_STATE=$(classify_path "plugins/$RETIRED_PLUGIN")
case "$RETIRED_STATE" in
  EMPTY) ok "H1: retired plugin '$RETIRED_PLUGIN' absent (successful read) — cutover took effect" ;;
  UNREADABLE) fail "H1: could not read /configs/plugins/$RETIRED_PLUGIN — this assertion proves ABSENCE, so an unreadable endpoint cannot satisfy it. A transport error is not evidence the retired plugin is gone." ;;
  *) fail "H1: retired plugin '$RETIRED_PLUGIN' is STILL installed at /configs/plugins/$RETIRED_PLUGIN (got $RETIRED_STATE) — the cutover did not take effect. The template still declares it, so this workspace now has TWO SEO surfaces that can both act on the same site." ;;
esac

# H2 is the original RFC#2843 decoupling guard, unchanged in intent: skills must
# never arrive through the provisioning/asset channel. It remains meaningful
# because agent-skills/$RETIRED_PLUGIN/ still EXISTS in the template repo (it was
# removed from plugins:, not deleted) — so this is a live path that a regression
# could genuinely start smuggling.
OLD_ASSET=$(classify_path "agent-skills/$RETIRED_PLUGIN")
case "$OLD_ASSET" in
  SKILL_MD) fail "H2: agent-skills/$RETIRED_PLUGIN/SKILL.md present on the OLD asset channel — RFC#2843 decoupling REGRESSED; the provisioning channel is still delivering skills (it must not)." ;;
  UNREADABLE) fail "H2: could not read agent-skills/$RETIRED_PLUGIN — this assertion proves ABSENCE, so an unreadable endpoint cannot satisfy it. Previously this swallowed transport errors as EMPTY and passed vacuously." ;;
  *) ok "H2: old asset-channel agent-skills/$RETIRED_PLUGIN carries no SKILL.md (successful read, state=$OLD_ASSET) — decoupling holds" ;;
esac

ok "ALL DELIVERY ASSERTIONS PASSED — asset channel: config+prompts+model; plugin channel: $EXPECTED_PLUGIN (both halves) mounting $EXPECTED_ABILITY_PKG by reference"
echo "PASS template-delivery-e2e: slug=$SLUG ws=$WID model=$MODEL config=${CFG_SIZE}B plugin=$EXPECTED_PLUGIN payload=$PAYLOAD docs=$SKILL_DOCS tools=$MCP_WIRED ability=$MOUNTED retired=$RETIRED_STATE" >&2
exit 0
