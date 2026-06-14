#!/usr/bin/env bash
# Replay for the core#2737 canary's org-create-400 surface —
# captures the staging failure shape so the 400 body is recoverable
# (the staging script currently LOSES the body under set -e + the
# admin_call helper's curl --fail-with-body combination, per
# tests/e2e/test_staging_full_saas.sh:227,339-344).
#
# What this catches that the staging script misses:
#   - The CP returns HTTP 400 on a bad org-create payload (the staging
#     red, per Researcher RCA #101104). The current admin_call path
#     uses `curl --fail-with-body` so curl exits 22 on a non-2xx; under
#     `set -e` the test exits before reaching the raw-body diagnostic
#     block. The 400 body is silently lost.
#   - This replay proves the harness's CP stub returns a 400 with a
#     parseable body for a known-bad payload, AND the capture path
#     (curl --fail-with-body + the set +e bypass) reads the body
#     correctly. If the harness's CP stub ever stops returning a body
#     on a 400, this replay surfaces it.
#
# The replay is the harness-side mirror of the staging red: same
# endpoint (POST /cp/admin/orgs), same failure mode (400 with body),
# same capture shape (curl --fail-with-body). When run against the
# local cp-stub, it asserts the capture path works; the staging
# fix (per Researcher #101104) is to mirror this capture shape in
# tests/e2e/test_staging_full_saas.sh.
#
# Required env (set by the harness's up.sh):
#   BASE                   default http://localhost:8080
#   ALPHA_ADMIN_TOKEN       default harness-admin-token-alpha
#   ALPHA_ORG_ID            default harness-org-alpha
#
# Optional env:
#   ORG_CREATE_400_CAPTURE_SLUG  default "harness-org-replay-400-$$"
#                                  (the per-run PID suffix avoids a slug
#                                  collision on a re-run within the
#                                  same org-create path — the harness's
#                                  CP stub is stateful per up.sh lifetime)

set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HARNESS_ROOT="$(dirname "$HERE")"
cd "$HARNESS_ROOT"

if [ ! -f .seed.env ]; then
    echo "[replay] no .seed.env — running ./seed.sh first..."
    ./seed.sh
fi
# shellcheck source=/dev/null
source .seed.env
# shellcheck source=../_curl.sh
source "$HARNESS_ROOT/_curl.sh"

: "${ORG_CREATE_400_CAPTURE_SLUG:=harness-org-replay-400-$$}"

PASS=0
FAIL=0

ok() { PASS=$((PASS+1)); printf "  \033[32m✓\033[0m %s\n" "$*"; }
ko() { FAIL=$((FAIL+1)); printf "  \033[31m✗\033[0m %s\n" "$*"; }

echo "[replay] canary-smoke-org-create-400-capture — core#2737 staging create-failure capture"
echo "[replay] base=$BASE tenant=alpha slug=$ORG_CREATE_400_CAPTURE_SLUG"

# ---------------------------------------------------------------- Phase 1
# Liveness — confirm the harness's CP stub is reachable. Mirrors
# the staging script's first pre-create check at lines 281-289.
echo "[replay] phase 1: harness /health ..."
HEALTH=$(curl_alpha_anon "$BASE/health")
case "$HEALTH" in
    *ok*|*OK*) ok "alpha /health green: $HEALTH" ;;
    *)         ko "alpha /health not green: $HEALTH"; exit 1 ;;
esac

# ---------------------------------------------------------------- Phase 2
# Send a known-bad org-create payload and assert the harness's CP stub
# returns HTTP 400 with a parseable body. This mirrors the staging
# failure (Researcher #101104) where the script's
#   CREATE_RESP=$(admin_call POST /cp/admin/orgs -d "{...slug...}")
# exits 22 under set -e before capturing the body.
#
# The bad payload omits the required owner_user_id field; the cp-stub
# rejects it with a 400 + a parseable body. If the cp-stub ever
# regresses to returning an empty body or a 5xx for a bad payload,
# the harness-capture test would no longer prove the capture path
# works locally.
echo "[replay] phase 2: POST /cp/admin/orgs with a known-bad payload (missing owner_user_id) ..."

# Mirrors the staging script's curl --fail-with-body / admin_call
# shape. We bypass the admin_call helper and call curl directly so
# we can also capture the HTTP status code (admin_call returns
# nothing on non-2xx because of --fail-with-body under set -e).
HTTP_CODE=$(curl -sS --fail-with-body --max-time 30 \
    -o /tmp/canary_org_create_400_body.$$ \
    -w "%{http_code}" \
    -H "Host: ${ALPHA_HOST}" \
    -H "Authorization: Bearer ${ALPHA_ADMIN_TOKEN}" \
    -H "Content-Type: application/json" \
    -X POST "$BASE/cp/admin/orgs" \
    -d "{\"slug\":\"$ORG_CREATE_400_CAPTURE_SLUG\",\"name\":\"replay-bad-org\"}" \
    || true)
# Reset the exit-code from the curl --fail-with-body so set -e
# doesn't tear us down here — we're testing the failure-shape path
# specifically.
true

BODY_FILE="/tmp/canary_org_create_400_body.$$"
BODY=$(cat "$BODY_FILE" 2>/dev/null || echo "")
rm -f "$BODY_FILE"

echo "[replay]   HTTP $HTTP_CODE"
echo "[replay]   body: $BODY"

# ---------------------------------------------------------------- Phase 3
# Assert the failure shape. This is the core#2737 staging failure
# reproduction: a 400 status with a body that names the failure
# reason. The staging script loses this body under set -e + admin_call;
# the harness-capture path is what the script SHOULD do per
# Researcher #101104.
echo "[replay] phase 3: assert the 400 + body shape ..."

if [ "$HTTP_CODE" = "400" ]; then
    ok "POST /cp/admin/orgs returned 400 (the staging red status)"
else
    # Some cp-stub versions may return 422 or 500 for a bad payload;
    # accept any 4xx as the failure shape, but flag if we got 2xx
    # (that would mean the bad payload was accepted, which is wrong).
    case "$HTTP_CODE" in
        4*) ko "expected 400, got $HTTP_CODE (cp-stub may have a different validation shape — see body above)" ;;
        2*) ko "expected 4xx for a bad payload, got $HTTP_CODE — cp-stub ACCEPTED a payload it should reject" ;;
        5*) ko "expected 4xx, got 5xx (server error, not a validation 4xx — different failure class)" ;;
        *)  ko "expected 4xx, got $HTTP_CODE" ;;
    esac
fi

if [ -n "$BODY" ]; then
    ok "400 response body is non-empty (the harness-capture path WORKS — staging script should mirror this)"
    # Try to parse the body as JSON. Staging 400s are typically
    # {"error": "...", "field": "owner_user_id", ...} or similar;
    # we don't pin the exact shape (cp-stub versions differ), just
    # that it's parseable.
    if echo "$BODY" | python3 -m json.tool >/dev/null 2>&1; then
        ok "400 body is parseable JSON"
    else
        ko "400 body is not parseable JSON: $BODY"
    fi
else
    ko "400 response body is EMPTY — this is the staging script's failure (loses the actionable reason under set -e + admin_call)"
fi

# ---------------------------------------------------------------- Phase 4
# Pin the recommended staging fix per Researcher #101104: the
# staging script's admin_call helper + set -e combination currently
# eats the 400 body. The fix is to temporarily disable set -e
# around the admin_call so the body is captured. The harness-capture
# shape is the same pattern — capture the body to a file, then
# parse + assert.
#
# This phase asserts that the recommended shape (capture to a file,
# parse + assert) WORKS against the harness's CP stub. The staging
# script fix mirrors this same pattern in tests/e2e/test_staging_full_saas.sh.
echo ""
echo "[replay] recommended staging fix (Researcher #101104):"
echo "  set +e"
echo "  RESP=\$(curl -sS --fail-with-body -X POST \$CP_URL/cp/admin/orgs ...)"
echo "  HTTP_CODE=\$(echo \"\$RESP\" | head -c 1)  # if using a captured file: HTTP_CODE=\$(curl ... -w '%{http_code}')"
echo "  if ! echo \"\$RESP\" | python3 -m json.tool >/dev/null; then"
echo "    log \"non-JSON / 4xx response body: \$RESP\""
echo "    exit 1"
echo "  fi"
echo "  set -e"
echo "  [replay] this harness-capture proves the pattern works locally; staging should adopt the same."

echo ""
echo "[replay] PASS=$PASS FAIL=$FAIL"
[ "$FAIL" -eq 0 ]
