#!/usr/bin/env bash
# Fail-direction proof for the SSOT-teardown gate in lint_cleanup_traps.sh.
#
# WHAT THIS PINS
# --------------
# The lint is the deliverable that turns "we fixed eight scripts" into "a ninth
# cannot regress". A lint nobody has ever seen go RED is indistinguishable from a
# lint that matches nothing — the exact trap that let this class survive twelve
# days after 3dd3675e2 fixed it in lib/cp_purge_receipt.sh and nowhere else.
#
# So this harness MUTATES a throwaway copy of the tree: it reinstates a
# hand-rolled `DELETE /cp/admin/tenants` of the shape every one of the eight
# migrated scripts used to carry, runs the REAL lint against that copy, and
# asserts the lint exits non-zero naming the offending FILE and LINE.
#
# CASES
#   1. GREEN CONTROL — the unmutated copy passes. Without this, every RED below
#      could just be a broken harness (a copy that lost a file, a python that
#      cannot run) rather than the lint biting.
#   2. RED on a reinstated curl `-X DELETE` — names file and line.
#   3. RED on a reinstated `admin_call DELETE "/cp/admin/tenants/$SLUG"` — the
#      helper-function shape, which is what the eighth instance (the concierge
#      provisioning-retry purge) actually looked like and what a line-level
#      `-X DELETE` grep would have missed entirely.
#   4. RED on a hand-rolled DELETE in a WORKFLOW yaml — the class is not
#      shell-only; a `run:` block can leak a tenant just as well.
#   5. RED when the DELETE is split across shell line-continuations — the shape
#      real curl invocations actually take. A naive per-line matcher scores this
#      GREEN, which is a lint that cannot see its own subject.
#   6. NOT red on prose/log narration that merely names the request
#      ("log \"teardown: DELETE /cp/admin/tenants/$SLUG\"") or on the
#      redeploy-fleet POST fan-out. A lint that cries wolf gets disabled.
#   7. RED when the allowlist goes stale — an entry whose file no longer holds a
#      hand-rolled DELETE must be removed, or it silently re-authorizes a future
#      one.
#   8. RED when the SSOT helper itself is renamed/removed out from under the
#      allowlist.
#
# Offline: no docker, no network, no credentials.

set -uo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO=$(cd "$HERE/../.." && pwd)
LINT_REL="tests/e2e/lint_cleanup_traps.sh"
[ -f "$REPO/$LINT_REL" ] || { echo "FAIL: $REPO/$LINT_REL not found" >&2; exit 1; }

PASS=0
FAIL=0
pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
bad()  { echo "  FAIL: $1"; FAIL=$((FAIL + 1)); }

WORK=$(mktemp -d -t cp-purge-lint-unit-XXXXXX)
trap 'rm -rf "$WORK"' EXIT INT TERM

# A pristine copy of exactly the subtrees the lint scans. Copying (rather than
# pointing the lint at the live tree with an env knob) keeps the PRODUCTION lint
# knob-free: there is no "scan somewhere else" switch a future caller could
# misuse to aim the gate at an empty directory and score a free green.
PRISTINE="$WORK/pristine"
mkdir -p "$PRISTINE"
for sub in tests scripts .gitea; do
  [ -d "$REPO/$sub" ] || continue
  cp -R "$REPO/$sub" "$PRISTINE/$sub"
done

fresh_copy() {  # <name> -> echoes the new root
  local root="$WORK/$1"
  rm -rf "$root"
  cp -R "$PRISTINE" "$root"
  printf '%s' "$root"
}

# NOTE: run_lint is invoked as `rc=$(run_lint ...)`, i.e. inside a
# command-substitution SUBSHELL — anything it assigns dies with that subshell.
# LINT_OUT is therefore derived from the root by the caller, not exported from
# here, so the output file is readable afterwards.
LINT_OUT=""
lint_out_for() { printf '%s' "$1/.lint-output.txt"; }
run_lint() {  # <root> -> echoes rc; output lands in $(lint_out_for <root>)
  local root="$1" out
  out=$(lint_out_for "$root")
  ( cd "$root" && PYTHONUTF8=1 bash "$root/$LINT_REL" ) >"$out" 2>&1
  echo $?
}

# ── 1. GREEN CONTROL ──────────────────────────────────────────────────────────
echo "── 1. green control: the unmutated tree passes ──"
ROOT=$(fresh_copy control)
rc=$(run_lint "$ROOT"); LINT_OUT=$(lint_out_for "$ROOT")
if [ "$rc" = "0" ]; then
  pass "unmutated tree is GREEN (rc=0) — later REDs are the lint biting, not a broken harness"
else
  bad "unmutated tree is already RED (rc=$rc) — every case below is meaningless until this is green"
  echo "----- lint output -----"; cat "$LINT_OUT"; echo "-----------------------"
  echo ""
  echo "── RESULT: $PASS passed, $FAIL failed ──"
  exit 1
fi
if grep -q "routes through e2e_cp_delete_and_verify_purge" "$LINT_OUT"; then
  pass "the SSOT-teardown gate actually ran (it did not silently match nothing)"
else
  bad "the SSOT-teardown gate produced no verdict line — it may not have run at all"
fi

# ── fixture construction ──────────────────────────────────────────────────────
# The regression fixtures are ASSEMBLED at runtime from these fragments and are
# never written as literals in this file. That is not cosmetic: the gate under
# test scans tests/**, and a harness that spelled out a hand-rolled teardown
# would be flagged by the very lint it is verifying — so the green control could
# only be restored by exempting this file, and an exemption here is a hole big
# enough to hide the next regression in. Assembling the fixtures keeps this
# harness subject to the same gate as everything else.
DEL="DE""LETE"
TPATH="/cp/admin""/tenants"

# ── mutation helper ───────────────────────────────────────────────────────────
# Reinstates a hand-rolled teardown into a victim file and asserts the lint reds
# NAMING that file and the exact injected line. The line is located by a UNIQUE
# marker token embedded in the generated statement itself, so the assertion can
# never drift from the fixture.
assert_red_naming() {  # <case> <root> <relpath> <marker-token>
  local case_name="$1" root="$2" rel="$3" marker="$4"
  local lineno rc out
  lineno=$(grep -n -F -- "$marker" "$root/$rel" | head -1 | cut -d: -f1)
  if [ -z "$lineno" ]; then
    bad "$case_name — could not locate the injected line in $rel (bad fixture)"
    return
  fi
  rc=$(run_lint "$root"); out=$(lint_out_for "$root")
  if [ "$rc" = "0" ]; then
    bad "$case_name — lint stayed GREEN on a reinstated hand-rolled teardown in $rel"
    return
  fi
  if ! grep -q "file=$rel," "$out"; then
    bad "$case_name — lint went RED but never named the file $rel"
    echo "----- lint output -----"; head -c 2000 "$out"; echo; echo "-----------------------"
    return
  fi
  if ! grep -q "file=$rel,line=$lineno::" "$out"; then
    bad "$case_name — lint named $rel but not line $lineno (got: $(grep -o "file=$rel,line=[0-9]*" "$out" | head -3 | tr '\n' ' '))"
    return
  fi
  if ! grep -q "e2e_cp_delete_and_verify_purge" "$out"; then
    bad "$case_name — the error does not tell the reader which helper to use"
    return
  fi
  pass "$case_name — RED naming $rel line $lineno"
}

# ── 2. reinstated raw curl teardown ───────────────────────────────────────────
echo ""
echo "-- 2. reinstated 'curl -X <verb>' (the shape all seven scripts carried) --"
ROOT=$(fresh_copy curl_delete)
VICTIM="tests/e2e/test_2307_peer_visibility_staging.sh"
{
  echo 'regressed_teardown_curl() {'
  printf '  curl -sS --max-time 120 -X %s "$CP_URL%s/$MUT2SLUG" -H "Authorization: Bearer $T" >/dev/null 2>&1\n' "$DEL" "$TPATH"
  echo '}'
} >> "$ROOT/$VICTIM"
assert_red_naming "raw curl teardown" "$ROOT" "$VICTIM" 'MUT2SLUG'

# ── 3. reinstated admin_call teardown ─────────────────────────────────────────
echo ""
echo "-- 3. reinstated 'admin_call <verb>' (the shape the EIGHTH instance had) --"
ROOT=$(fresh_copy admin_call_delete)
VICTIM="tests/e2e/test_mcp_stdio_staging.sh"
{
  echo 'regressed_teardown_admincall() {'
  printf '  admin_call %s "%s/$MUT3SLUG" -d "{}" >/dev/null 2>&1 || true\n' "$DEL" "$TPATH"
  echo '}'
} >> "$ROOT/$VICTIM"
assert_red_naming "admin_call teardown" "$ROOT" "$VICTIM" 'MUT3SLUG'

# ── 4. reinstated teardown in a workflow ──────────────────────────────────────
echo ""
echo "-- 4. reinstated hand-rolled teardown inside a workflow run: block --"
ROOT=$(fresh_copy workflow_delete)
VICTIM=".gitea/workflows/e2e-ephemeral-happy-path.yml"
{
  echo '# regressed teardown net (fixture)'
  printf '      curl -sS -X %s "$CP%s/$MUT4SLUG" -d "{}"\n' "$DEL" "$TPATH"
} >> "$ROOT/$VICTIM"
assert_red_naming "workflow teardown" "$ROOT" "$VICTIM" 'MUT4SLUG'

# ── 5. teardown split across line continuations ───────────────────────────────
echo ""
echo "-- 5. teardown split across shell line-continuations (what real curl looks like) --"
ROOT=$(fresh_copy continuation_delete)
VICTIM="tests/e2e/test_template_delivery_e2e.sh"
{
  echo 'regressed_teardown_multiline() {'
  printf '  curl -sS \\\n'
  printf '    --max-time 120 \\\n'
  printf '    -X %s \\\n' "$DEL"
  printf '    -H "Authorization: Bearer $T" \\\n'
  printf '    "$CP_URL%s/$MUT5SLUG" >/dev/null 2>&1\n' "$TPATH"
  echo '}'
} >> "$ROOT/$VICTIM"
# The violation must be reported at the START of the joined logical statement —
# the `curl -sS \` line — NOT at the line where the URL happens to appear. A
# per-line matcher scores this whole case GREEN, which is a lint that cannot see
# its own subject.
CONT_START=$(grep -n -F -- 'regressed_teardown_multiline() {' "$ROOT/$VICTIM" | head -1 | cut -d: -f1)
CONT_START=$((CONT_START + 1))
rc=$(run_lint "$ROOT"); OUT=$(lint_out_for "$ROOT")
if [ "$rc" != "0" ] && grep -q "file=$VICTIM,line=$CONT_START::" "$OUT"; then
  pass "multi-line curl teardown — RED naming $VICTIM line $CONT_START (statement start)"
elif [ "$rc" = "0" ]; then
  bad "multi-line curl teardown — lint stayed GREEN; a per-line matcher cannot see its own subject"
else
  bad "multi-line curl teardown — RED but not at statement start line $CONT_START (got: $(grep -o "file=$VICTIM,line=[0-9]*" "$OUT" | head -3 | tr '\n' ' '))"
fi

# ── 6. no false positives ─────────────────────────────────────────────────────
echo ""
echo "-- 6. narration and the redeploy-fleet POST must NOT trip the gate --"
ROOT=$(fresh_copy no_false_positive)
VICTIM="tests/e2e/test_minimal_boot_cell.sh"
{
  echo 'narration_only() {'
  printf '  log "teardown: %s %s/$SLUG (executeOrgPurge cascade)"\n' "$DEL" "$TPATH"
  printf '  echo "::warning::%s %s/$SLUG did not complete"\n' "$DEL" "$TPATH"
  printf '  # %s %s/:slug is documented here in prose only.\n' "$DEL" "$TPATH"
  printf '  curl -sS -X POST "$CP_URL%s/redeploy-fleet" -d "{}"\n' "$TPATH"
  echo '}'
} >> "$ROOT/$VICTIM"
rc=$(run_lint "$ROOT"); OUT=$(lint_out_for "$ROOT")
if [ "$rc" = "0" ]; then
  pass "log/echo narration, prose comments and the redeploy-fleet POST are not flagged"
else
  bad "false positive: the gate flagged narration or the redeploy-fleet POST"
  echo "----- lint output -----"; head -c 1500 "$OUT"; echo; echo "-----------------------"
fi

# ── 7. stale allowlist entry ──────────────────────────────────────────────────
echo ""
echo "-- 7. a stale allowlist entry must RED (it silently re-authorizes a regression) --"
ROOT=$(fresh_copy stale_allowlist)
VICTIM=".gitea/workflows/sweep-stale-e2e-orgs.yml"
# Neutralize the janitor's own teardown so its allowlist entry is now unearned.
# Plain sed, not a python heredoc: this harness runs the lint ~9 times and each
# extra interpreter is another process to fork. Fail loudly if the fixture
# premise no longer holds.
grep -qF -- "-X $DEL" "$ROOT/$VICTIM" \
  || bad "fixture error: the janitor no longer contains a hand-rolled teardown to neutralize"
sed -i.bak "s|-X $DEL|-X GET|g" "$ROOT/$VICTIM" && rm -f "$ROOT/$VICTIM.bak"
rc=$(run_lint "$ROOT"); OUT=$(lint_out_for "$ROOT")
if [ "$rc" != "0" ] && grep -q "stale SSOT-teardown allowlist entry" "$OUT"; then
  pass "an allowlist entry that no longer earns its exemption goes RED"
else
  bad "a stale allowlist entry stayed GREEN (rc=$rc) — the allowlist can rot into a hole"
fi

# ── 8. SSOT helper renamed ────────────────────────────────────────────────────
echo ""
echo "-- 8. renaming/removing the SSOT helper must RED (allowlists defer to it) --"
ROOT=$(fresh_copy ssot_renamed)
SSOT_COPY="$ROOT/tests/e2e/lib/cp_purge_receipt.sh"
grep -qF -- "e2e_cp_delete_and_verify_purge()" "$SSOT_COPY" \
  || bad "fixture error: SSOT helper definition not found in the copy"
sed -i.bak "s|e2e_cp_delete_and_verify_purge()|e2e_cp_delete_and_verify_purge_RENAMED()|g" "$SSOT_COPY" && rm -f "$SSOT_COPY.bak"
rc=$(run_lint "$ROOT"); OUT=$(lint_out_for "$ROOT")
if [ "$rc" != "0" ] && grep -q "does not define e2e_cp_delete_and_verify_purge" "$OUT"; then
  pass "a renamed/removed SSOT helper goes RED instead of leaving every caller dangling"
else
  bad "the SSOT helper was renamed and the gate stayed GREEN (rc=$rc)"
fi

echo ""
echo "-- RESULT: $PASS passed, $FAIL failed --"
[ "$FAIL" -eq 0 ] || exit 1
exit 0

