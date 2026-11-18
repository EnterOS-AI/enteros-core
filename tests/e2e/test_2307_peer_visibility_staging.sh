#!/usr/bin/env bash
# Staging E2E for #2307 — create fresh tenant, test peer visibility, tear down.
#
# Mirrors tests/e2e/test_staging_full_saas.sh's pattern (org create via
# /cp/admin/orgs, EXIT-trap teardown via DELETE /cp/admin/tenants/:slug
# with required {"confirm":slug} body).
#
# Required: MOLECULE_ADMIN_TOKEN exported (CP admin bearer).
#           E2E_INFRA_BACKEND=local-docker (the only active backend; the SSOT
#           teardown helper refuses to run against anything else).
# Optional:
#   MOLECULE_CP_URL  default https://staging-api.moleculesai.app
#   PARENT_RUNTIME   default claude-code

set -uo pipefail

CP_URL="${MOLECULE_CP_URL:-https://staging-api.moleculesai.app}"
ADMIN_TOKEN="${MOLECULE_ADMIN_TOKEN:?MOLECULE_ADMIN_TOKEN required}"
PARENT_RUNTIME="${PARENT_RUNTIME:-claude-code}"

# log/fail/ok MUST be defined BEFORE the assert_collision_proof_slug call
# below (which uses `|| fail "..."`). Defining them after the call would
# error on a bad slug with `fail: command not found` instead of the
# intended diagnostic. Mirrors the order in test_staging_full_saas.sh.
log()  { echo "[$(date +%H:%M:%S)] $*"; }
fail() { echo "[$(date +%H:%M:%S)] ❌ $*" >&2; exit 1; }
ok()   { echo "[$(date +%H:%M:%S)] ✅ $*"; }

# Collision-proof slug (core#2782). The prior `SLUG="e2e-2307-$RUN_ID"`
# shape used a raw 8-char timestamp tail and could collide between two
# CI runs (e.g. retry of run 3606 + fresh run 3607) on POST
# /cp/admin/orgs 409. Migrating to the shared helper appends an 8-char
# uuid so every run gets a unique slug regardless of how the workflow
# composes E2E_RUN_ID.
# shellcheck source=lib/collision-proof-slug.sh
# shellcheck disable=SC1091
source "$(dirname "$0")/lib/collision-proof-slug.sh"
# SSOT teardown contract (e2e_cp_delete_and_verify_purge): exact purge receipt +
# audit row + exact-org absence, INCLUDING the bounded retry for the CP's
# transient 409 "organization has an active lifecycle operation".
# shellcheck source=tests/e2e/lib/cp_purge_receipt.sh
# shellcheck disable=SC1091
source "$(dirname "$0")/lib/cp_purge_receipt.sh"
e2e_cp_require_local_backend || exit 2
e2e_cp_require_staging_origin "$CP_URL" || exit 2
SLUG="e2e-2307-$(make_collision_proof_slug_suffix "${E2E_RUN_ID:-}" 9)"
assert_collision_proof_slug "$SLUG" || fail "Bug in make_collision_proof_slug: produced non-collision-proof slug '$SLUG'"

ORG_ID=""
ORG_CREATION_VERIFIED=0
CLEANUP_DONE=0
TENANT_URL=""
TENANT_TOKEN=""
PARENT=""
CHILD=""
CTOK=""

admin_call() {
    local method="$1" path="$2"
    shift 2
    curl -sS -X "$method" "$CP_URL$path" \
        -H "Authorization: Bearer $ADMIN_TOKEN" \
        -H "Content-Type: application/json" \
        "$@"
}

tenant_call() {
    local method="$1" path="$2"
    shift 2
    curl -sS -X "$method" "$TENANT_URL$path" \
        -H "Authorization: Bearer $TENANT_TOKEN" \
        -H "X-Molecule-Org-Id: $ORG_ID" \
        -H "Content-Type: application/json" \
        "$@"
}

# The DELETE goes through the SSOT helper e2e_cp_delete_and_verify_purge
# (lib/cp_purge_receipt.sh) — the SAME teardown contract full_saas, the
# concierge lanes and the peer-visibility gate use.
#
# This script used to hand-roll it: one fire-and-forget DELETE with its response
# thrown away (`>/dev/null 2>&1`), then a 60s poll of the org ROSTER for the slug
# to vanish. That shape is UNRECOVERABLE against the control plane's
# org-lifecycle fence: claimOrgPurge answers an immediate HTTP 409 "organization
# has an active lifecycle operation" while any org-scoped mutation lease is open
# — including the CP's own asynchronous concierge/platform-agent workspace
# provision for the org this run just created. A 409 writes NO purge audit row,
# so the poll has nothing to converge on: nothing re-issues the DELETE, the org
# LEAKS, and the poll burns its full budget before reporting a failure that is
# not the failure. Fixed once in the shared lib by 3dd3675e2 ("retry transient
# 409 'active lifecycle operation' on teardown DELETE") and never propagated to
# this hand-rolled copy.
#
# The helper still fails CLOSED — a persistent 409, any other non-200, a
# malformed or mismatched receipt, or an org that survives its budget all exit 4
# — and it upgrades the assertion from "the roster page stopped listing it" to
# "exact purge receipt + exact completed audit row + exact-org 404".
teardown() {
    local rc=$?
    set +e
    [ "$CLEANUP_DONE" = "1" ] && return 0
    CLEANUP_DONE=1
    echo ""
    if [ "$ORG_CREATION_VERIFIED" != "1" ]; then
        echo "[teardown] no verified creation-returned org identity for $SLUG — nothing this run created to delete"
        exit $rc
    fi
    echo "[teardown] DELETE /cp/admin/tenants/$SLUG ..."
    local purge_rc=0
    e2e_cp_delete_and_verify_purge "$CP_URL" "$ADMIN_TOKEN" "$SLUG" "$ORG_ID" || purge_rc=$?
    case "$purge_rc" in
        0) echo "  ✓ tenant purged" ;;
        2) exit 2 ;;
        *)
            echo "  ⚠ LEAK: $SLUG was not provably purged — manual cleanup needed" >&2
            [ $rc -eq 0 ] && rc=4
            ;;
    esac
    exit $rc
}
trap teardown EXIT INT TERM

# ─── 1. Create the org ────────────────────────────────────────────────
echo "[1/8] POST /cp/admin/orgs — slug=$SLUG"
CREATE=$(admin_call POST /cp/admin/orgs \
    -d "{\"slug\":\"$SLUG\",\"name\":\"E2E #2307 $SLUG\",\"owner_user_id\":\"e2e-runner:$SLUG\"}")
echo "  resp: $(echo "$CREATE" | head -c 300)"
ORG_ID=$(echo "$CREATE" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null)
[ -n "$ORG_ID" ] || { echo "  ✗ org creation failed"; exit 1; }
# The creation-returned id is the teardown's collision-safe authority: the helper
# refuses to DELETE a slug whose current org id is not this one.
e2e_cp_validate_org_id "$ORG_ID" \
    || fail "org creation returned a non-UUID id ($ORG_ID) — refusing to arm a destructive teardown against it"
ORG_CREATION_VERIFIED=1
echo "  ✓ ORG_ID=$ORG_ID"

# ─── 2. Wait for tenant ready ─────────────────────────────────────────
echo "[2/8] waiting for tenant to come up (cold-start ~5-10min)..."
for i in $(seq 1 180); do
    STATUS=$(admin_call GET /cp/admin/orgs 2>/dev/null | python3 -c "
import sys, json
try: d = json.load(sys.stdin)
except Exception: sys.exit(0)
orgs = d if isinstance(d, list) else d.get('orgs', [])
for o in orgs:
    if o.get('slug') == '$SLUG':
        print(o.get('instance_status') or o.get('status') or 'unknown')
        break
" 2>/dev/null)
    [ $((i % 6)) -eq 1 ] && echo "  attempt $i: status=$STATUS"
    case "$STATUS" in running|online|ready) break ;; esac
    sleep 5
done
case "$STATUS" in running|online|ready) ;;
    *) echo "  ✗ tenant never came up (last=$STATUS)"; exit 2 ;; esac
echo "  ✓ tenant status=$STATUS"

# ─── 3. Per-tenant admin token ────────────────────────────────────────
echo "[3/8] fetching per-tenant admin token..."
TT_RESP=$(admin_call GET "/cp/admin/orgs/$SLUG/admin-token")
TENANT_TOKEN=$(echo "$TT_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('admin_token',''))" 2>/dev/null)
[ -n "$TENANT_TOKEN" ] || { echo "  ✗ tenant token fetch failed: $TT_RESP"; exit 2; }
echo "  ✓ got tenant admin token (len ${#TENANT_TOKEN})"

CP_HOST=$(echo "$CP_URL" | sed -E 's#^https?://##; s#/.*$##')
case "$CP_HOST" in
    api.*)         DERIVED_DOMAIN="${CP_HOST#api.}" ;;
    staging-api.*) DERIVED_DOMAIN="staging.${CP_HOST#staging-api.}" ;;
    *)             DERIVED_DOMAIN="$CP_HOST" ;;
esac
TENANT_URL="https://${SLUG}.${DERIVED_DOMAIN}"
echo "  tenant url: $TENANT_URL"

# ─── 4. Wait for tenant TLS/DNS readiness ─────────────────────────────
echo "[4/8] waiting for tenant /health (TLS/DNS, up to 10min)..."
for i in $(seq 1 120); do
    if curl -fsS "$TENANT_URL/health" -m 5 -k >/dev/null 2>&1; then
        echo "  ✓ /health ok (attempt $i)"
        break
    fi
    sleep 5
done

# ─── 5. Provision parent CEO workspace ────────────────────────────────
echo "[5/8] creating parent CEO ($PARENT_RUNTIME)..."
P_RESP=$(tenant_call POST /workspaces \
    -d "{\"name\":\"e2e-CEO\",\"runtime\":\"$PARENT_RUNTIME\",\"tier\":3}")
echo "  parent resp: $(echo "$P_RESP" | head -c 300)"
PARENT=$(echo "$P_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null)
PTOK=$(echo "$P_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('auth_token',''))" 2>/dev/null)
[ -n "$PARENT" ] || { echo "  ✗ parent create failed"; exit 3; }
echo "  ✓ PARENT=$PARENT  (parent_token_returned=$([ -n "$PTOK" ] && echo yes || echo no))"

# ─── 6. Wait for parent online ────────────────────────────────────────
echo "[6/8] waiting for parent to come online (up to 12min)..."
for i in $(seq 1 144); do
    WS_JSON=$(tenant_call GET "/workspaces/$PARENT" 2>/dev/null)
    S=$(echo "$WS_JSON" | python3 -c "
import sys, json
try: d = json.load(sys.stdin)
except Exception: sys.exit(0)
w = d.get('workspace') if isinstance(d.get('workspace'), dict) else d
print(w.get('status') or '')
" 2>/dev/null)
    [ $((i % 6)) -eq 1 ] && echo "  attempt $i: parent status=$S"
    [ "$S" = "online" ] && break
    sleep 5
done
[ "$S" = "online" ] || { echo "  ✗ parent never online (last=$S)"; exit 3; }
echo "  ✓ parent online"

# ─── 7. Create external child + register URL ──────────────────────────
echo "[7/8] creating external child + registering..."
C_RESP=$(tenant_call POST /workspaces \
    -d "{\"name\":\"e2e-Reno-Server\",\"runtime\":\"external\",\"external\":true,\"tier\":2,\"parent_id\":\"$PARENT\"}")
echo "  child resp: $(echo "$C_RESP" | head -c 400)"
CHILD=$(echo "$C_RESP" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null)
# External-runtime token is nested under `connection.auth_token` (verified
# 2026-04-29 against staging response shape). Fall back to top-level for
# parity with older clients.
CTOK=$(echo "$C_RESP" | python3 -c "
import sys, json
d = json.load(sys.stdin)
print(d.get('connection', {}).get('auth_token') or d.get('auth_token') or '')
" 2>/dev/null)
[ -n "$CHILD" ] || { echo "  ✗ child create failed"; exit 3; }
echo "  ✓ CHILD=$CHILD  (child_token_returned=$([ -n "$CTOK" ] && echo yes || echo no))"

# Try register with child's own token (bootstrap path); fall back to tenant_call
if [ -n "$CTOK" ]; then
    REG_RESP=$(curl -sS -X POST "$TENANT_URL/registry/register" \
        -H "Authorization: Bearer $CTOK" \
        -H "X-Molecule-Org-Id: $ORG_ID" \
        -H "Content-Type: application/json" \
        -d "{\"id\":\"$CHILD\",\"url\":\"https://example.com/molecule-test\",\"agent_card\":{\"name\":\"Reno Server\",\"description\":\"Mock\",\"version\":\"0.1.0\"}}")
else
    REG_RESP=$(tenant_call POST /registry/register \
        -d "{\"id\":\"$CHILD\",\"url\":\"https://example.com/molecule-test\",\"agent_card\":{\"name\":\"Reno Server\",\"description\":\"Mock\",\"version\":\"0.1.0\"}}")
fi
echo "  register resp: $(echo "$REG_RESP" | head -c 300)"

# ─── 8. THE TEST — peer visibility ────────────────────────────────────
echo ""
echo "[8/8] === Verdict — does parent see external child? ==="
echo ""
echo "(a) DB shape via admin: GET /cp/admin/orgs/$SLUG (workspaces listing if exposed)"

# Check children listing — most direct DB-shape signal we can get from outside
LIST=$(tenant_call GET "/workspaces?parent_id=$PARENT")
echo "  /workspaces?parent_id=$PARENT response: $(echo "$LIST" | head -c 500)"
echo ""

CHILD_LISTED=$(echo "$LIST" | python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    print('parse_error'); sys.exit(0)
ws = d if isinstance(d, list) else d.get('workspaces', d.get('items', []))
print('yes' if any(w.get('id') == '$CHILD' for w in ws) else 'no')
" 2>/dev/null)
echo "  child appears in parent's children listing: $CHILD_LISTED"

# (b) /peers from PARENT side using PTOK if provided
if [ -n "$PTOK" ]; then
    PEERS=$(curl -sS "$TENANT_URL/registry/$PARENT/peers" \
        -H "Authorization: Bearer $PTOK" \
        -H "X-Molecule-Org-Id: $ORG_ID")
    echo ""
    echo "(b) GET /registry/$PARENT/peers (parent's bearer):"
    echo "    $(echo "$PEERS" | head -c 600)"
    if echo "$PEERS" | grep -q "$CHILD"; then
        echo "  ✓ child IS in parent's /peers"
        VERDICT_B=ok
    else
        echo "  ✗ child is NOT in parent's /peers — bug REPRODUCES at API layer"
        VERDICT_B=fail
    fi
else
    echo ""
    echo "(b) parent's auth_token not exposed by /workspaces create — skipping direct /peers check"
    VERDICT_B=skipped
fi

# (c) /peers from CHILD side using CTOK
if [ -n "$CTOK" ]; then
    PEERS_C=$(curl -sS "$TENANT_URL/registry/$CHILD/peers" \
        -H "Authorization: Bearer $CTOK" \
        -H "X-Molecule-Org-Id: $ORG_ID")
    echo ""
    echo "(c) GET /registry/$CHILD/peers (child's bearer):"
    echo "    $(echo "$PEERS_C" | head -c 600)"
    if echo "$PEERS_C" | grep -q "$PARENT"; then
        echo "  ✓ parent IS in child's /peers"
        VERDICT_C=ok
    else
        echo "  ✗ parent is NOT in child's /peers"
        VERDICT_C=fail
    fi
else
    VERDICT_C=skipped
fi

echo ""
echo "=== SUMMARY for #2307 staging E2E ==="
echo "  child listed under parent: $CHILD_LISTED"
echo "  /peers parent→child:       $VERDICT_B"
echo "  /peers child→parent:       $VERDICT_C"

# Exit code: 0 if everything visible, 10 if bug reproduces, 11 if inconclusive
if [ "$CHILD_LISTED" = "yes" ] && [ "$VERDICT_B" = "ok" ]; then
    echo ""
    echo "✓ STAGING: parent fully sees external child — bug is downstream (agent code, not platform API)"
    exit 0
elif [ "$VERDICT_B" = "fail" ] || [ "$CHILD_LISTED" = "no" ]; then
    echo ""
    echo "✗ STAGING: bug REPRODUCES at platform-API layer"
    exit 10
else
    echo ""
    echo "? STAGING: inconclusive (need parent token to call /peers definitively)"
    exit 11
fi
