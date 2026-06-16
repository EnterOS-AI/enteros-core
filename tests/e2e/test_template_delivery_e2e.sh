#!/usr/bin/env bash
# Template-asset DELIVERY e2e (RFC #2843) — the regression gate for the
# 2026-06-15 incident family (#2919 + #2843 rollout). It provisions a FRESH
# tenant + a FRESH seo-agent workspace and asserts the template-asset channel
# actually delivers config + prompts + AGENT-SKILLS + model end to end.
#
# WHY THIS EXISTS — the bugs that shipped green because nothing asserted this:
#   1. concierge booted with no MODEL  (MISSING_MODEL fail-closed) — #2966
#   2. concierge booted with no IDENTITY (generic Claude Code)     — #2955
#   3. seo-agent booted with config+prompts but NO agent-skills/   — #32
# The unit/drift tests did not catch any of these because none provision a
# fresh agent end-to-end and inspect the DELIVERED /configs. This does.
#
# Assertions (each maps to a real incident):
#   A. seo-agent reaches online                       (catches MISSING_MODEL)
#   B. GET /model == the template's declared model    (catches model drop)
#   C. config.yaml delivered + REAL (> 1 KiB, not the 218 B default stub)
#   D. prompts/ delivered (identity prompt present)   (catches identity drop)
#   E. agent-skills/seo-all/SKILL.md delivered        (catches skill drop, #32)
#
# Auth model + org-provision/teardown shape mirror test_staging_concierge_e2e.sh.
#
# Required env:
#   MOLECULE_CP_URL        default https://staging-api.moleculesai.app
#   MOLECULE_ADMIN_TOKEN   CP admin bearer (Railway CP_ADMIN_API_TOKEN)
# Optional:
#   E2E_SEO_TEMPLATE       default seo-agent
#   E2E_EXPECTED_MODEL     default moonshot/kimi-k2.6
#   E2E_PROVISION_TIMEOUT_SECS  default 900
#   E2E_KEEP_ORG           1 → skip teardown (debug)
#
# Exit: 0 pass | 1 assertion fail | 2 missing env | 3 provision timeout
set -euo pipefail

CP_URL="${MOLECULE_CP_URL:-https://staging-api.moleculesai.app}"
ADMIN_TOKEN="${MOLECULE_ADMIN_TOKEN:?MOLECULE_ADMIN_TOKEN required — Railway CP_ADMIN_API_TOKEN}"
SEO_TEMPLATE="${E2E_SEO_TEMPLATE:-seo-agent}"
EXPECTED_MODEL="${E2E_EXPECTED_MODEL:-moonshot/kimi-k2.6}"
PROVISION_TIMEOUT_SECS="${E2E_PROVISION_TIMEOUT_SECS:-900}"

# Collision-proof slug (random suffix), same convention as the sibling harness.
RAND=$(head -c4 /dev/urandom | od -An -tx1 | tr -d ' \n')
SLUG="e2e-tmpl-${RAND}"
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
  curl "${CURL_COMMON[@]}" --max-time 120 -X DELETE "$CP_URL/cp/admin/tenants/$SLUG" \
    -H "Authorization: Bearer $ADMIN_TOKEN" >/dev/null 2>&1 || true
  return $rc
}
trap cleanup_org EXIT INT TERM

# ─── 0. preflight ────────────────────────────────────────────────────────────
log "═══ Template-asset delivery E2E ═══  CP=$CP_URL  slug=$SLUG  template=$SEO_TEMPLATE"
curl "${CURL_COMMON[@]}" "$CP_URL/health" >/dev/null || fail "CP health check failed"
ok "CP reachable"

# ─── 1. create org ───────────────────────────────────────────────────────────
log "1/6 creating org $SLUG"
CREATE=$(admin_call POST /cp/admin/orgs \
  -d "{\"slug\":\"$SLUG\",\"name\":\"E2E $SLUG\",\"owner_user_id\":\"e2e-runner:$SLUG\"}")
ORG_ID=$(echo "$CREATE" | python3 -c "import json,sys;print(json.load(sys.stdin).get('id',''))" 2>/dev/null || echo "")
[ -z "$ORG_ID" ] && fail "org create missing id: $CREATE"
ok "org created id=$ORG_ID"

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
CFG_SIZE=$(tenant_call GET "/workspaces/$WID/files" | python3 -c "
import json,sys
for f in json.load(sys.stdin):
    if f.get('path')=='config.yaml': print(f.get('size',0)); break
else: print(0)
" 2>/dev/null || echo 0)
[ "${CFG_SIZE:-0}" -gt 1024 ] || fail "C: config.yaml size=$CFG_SIZE B (≤1KiB ⇒ default stub, template config NOT delivered)"
ok "C: config.yaml delivered ($CFG_SIZE B)"

# D. prompts/ delivered (identity prompt)
PROMPTS=$(tenant_call GET "/workspaces/$WID/files?path=prompts" | python3 -c "
import json,sys
d=json.load(sys.stdin); print(len(d) if isinstance(d,list) else 0)
" 2>/dev/null || echo 0)
[ "${PROMPTS:-0}" -gt 0 ] || fail "D: prompts/ empty — identity prompt NOT delivered"
ok "D: prompts/ delivered ($PROMPTS file(s))"

# E. agent-skills/seo-all delivered — THE #32 regression assertion
SKILLS=$(tenant_call GET "/workspaces/$WID/files?path=agent-skills/seo-all" | python3 -c "
import json,sys
d=json.load(sys.stdin)
paths=[f.get('path','') for f in d] if isinstance(d,list) else []
print('SKILL_MD' if any(p.endswith('SKILL.md') for p in paths) else ('NONEMPTY' if paths else 'EMPTY'))
" 2>/dev/null || echo EMPTY)
case "$SKILLS" in
  SKILL_MD|NONEMPTY) ok "E: agent-skills/seo-all delivered ($SKILLS)";;
  *) fail "E: agent-skills/seo-all EMPTY — the seo-all skill pack was NOT delivered (#32 regression). The whole point of the SEO agent is missing.";;
esac

ok "ALL DELIVERY ASSERTIONS PASSED — config + prompts + skills + model reached /configs"
echo "PASS template-delivery-e2e: slug=$SLUG ws=$WID model=$MODEL config=${CFG_SIZE}B skills=$SKILLS" >&2
exit 0
