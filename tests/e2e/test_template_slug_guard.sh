#!/usr/bin/env bash
# Regression test for the E2E_TMPL_SLUG destructive-selector guard added to
# tests/e2e/test_template_delivery_e2e.sh (core#3375 review).
#
# WHY: that script trusts the E2E_TMPL_SLUG override — the slug BOTH its own
# EXIT/INT/TERM cleanup trap AND the wrapping workflow's always() net delete via
# `DELETE /cp/admin/tenants/$SLUG` with the CP admin token. Before the guard, a
# typo'd / arbitrary override turned a CI coordination variable into a
# tenant-deletion selector. The guard rejects any non-ephemeral slug and
# fail-fasts BEFORE the delete trap is installed or any org is created/deleted.
#
# This test proves BOTH directions of that guard, fully OFFLINE:
#   - REJECT: a non-`e2e-tmpl-<8hex>` override exits 2 with the refusal message,
#     BEFORE any CP call (the guard runs before preflight), so nothing is
#     provisioned or deprovisioned.
#   - ACCEPT: a legitimately-minted ephemeral slug passes the guard (does NOT
#     exit 2 / print the refusal); it then fails later at preflight because CP is
#     pointed at a closed port — that non-2 exit is the proof the guard let it
#     through, with no real infra touched.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
E2E="$SCRIPT_DIR/test_template_delivery_e2e.sh"

STDERR_TMP="$(mktemp -t tmpl-slug-guard-XXXXXX)"
trap 'rm -f "$STDERR_TMP"' EXIT INT TERM

PASS=0
FAIL=0

# Run the e2e with a dummy admin token and the given slug. CP is pointed at a
# closed port so an ACCEPTED slug fails fast at preflight instead of touching
# real infrastructure. Prints the exit code; stderr is captured to $STDERR_TMP.
run_guard() {  # $1=slug
  MOLECULE_ADMIN_TOKEN="dummy-not-a-real-token" \
  MOLECULE_CP_URL="http://127.0.0.1:9" \
  E2E_TMPL_SLUG="$1" \
    bash "$E2E" 2>"$STDERR_TMP" >/dev/null
  echo $?
}

reject_case() {  # $1=label  $2=slug
  local label="$1" slug="$2" rc
  rc=$(run_guard "$slug")
  if [ "$rc" = "2" ] && grep -q "refusing unsafe E2E_TMPL_SLUG" "$STDERR_TMP"; then
    echo "  ✓ REJECT $label"
    PASS=$((PASS + 1))
  else
    echo "  ✗ REJECT $label — expected exit 2 + refusal, got rc=$rc" >&2
    sed 's/^/        /' "$STDERR_TMP" >&2
    FAIL=$((FAIL + 1))
  fi
}

accept_case() {  # $1=label  $2=slug
  local label="$1" slug="$2" rc
  rc=$(run_guard "$slug")
  # A valid ephemeral slug clears the guard: it must NOT exit 2 and must NOT emit
  # the refusal. It proceeds to preflight and fails there (CP unreachable) — that
  # non-2 exit is exactly the proof the guard accepted it.
  if [ "$rc" != "2" ] && ! grep -q "refusing unsafe E2E_TMPL_SLUG" "$STDERR_TMP"; then
    echo "  ✓ ACCEPT $label (cleared guard, rc=$rc)"
    PASS=$((PASS + 1))
  else
    echo "  ✗ ACCEPT $label — expected the guard to accept it (rc≠2, no refusal), got rc=$rc" >&2
    sed 's/^/        /' "$STDERR_TMP" >&2
    FAIL=$((FAIL + 1))
  fi
}

echo "Test: E2E_TMPL_SLUG destructive-selector guard (core#3375)"
echo

# ── REJECT: anything that is not an ephemeral e2e-tmpl-<8 hex> slug ──
reject_case "arbitrary prod-looking org name"     "acme-prod"
reject_case "bare org slug, no e2e-tmpl- prefix"  "hongmingwang"
reject_case "wrong prefix (rt-e2e-)"              "rt-e2e-deadbeef"
reject_case "missing e2e- (tmpl- only)"           "tmpl-deadbeef"
reject_case "prefix ok, non-hex suffix"           "e2e-tmpl-notavalidhex"
reject_case "prefix ok, suffix too short"         "e2e-tmpl-dead"
reject_case "prefix ok, suffix too long"          "e2e-tmpl-deadbeef0"
reject_case "uppercase suffix (od emits lower)"   "e2e-tmpl-DEADBEEF"
reject_case "trailing whitespace"                 "e2e-tmpl-deadbeef "
reject_case "path-traversal selector"             "e2e-tmpl-deadbeef/../victim"
reject_case "command-substitution attempt"        'e2e-tmpl-$(id)'

# ── ACCEPT: legitimately-minted ephemeral slugs clear the guard ──
accept_case "canonical minted slug"               "e2e-tmpl-deadbeef"
accept_case "all-digit hex suffix"                "e2e-tmpl-01234567"
accept_case "all-alpha hex suffix"                "e2e-tmpl-abcdefab"

echo
echo "Guard test: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
