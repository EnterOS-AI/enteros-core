#!/usr/bin/env bash
# Security regression test for the SOP tier-gate AUTHORIZATION bypass.
#
# Bug (fixed in fix/sop-tier-authz-no-org-fallback):
#   sop-tier-check.sh probed team membership at /teams/{id}/members/{user}.
#   If EVERY team probe failed (e.g. 403 — token lacks read:organization, or
#   any visibility/flakiness gap), it FELL BACK to /orgs/{org}/members/{user}
#   and credited that org member as a member of EVERY queried team. The
#   evaluator then treated those synthetic memberships as real, so a plain
#   NON-CEO org member satisfied tier:high (ceo). A visibility/auth gap became
#   a real highest-tier authorization PASS — privilege escalation.
#
# Fix (fail-closed authorization):
#   - The org-member ⇒ "member of all teams" fallback is REMOVED. Org
#     membership is never credited as team membership.
#   - A team probe that returns anything other than 200/204 (member) or 404
#     (verified non-member) is a CANNOT-VERIFY condition: the gate fails loud
#     (exit 1) with a cannot-verify status and never grants the tier.
#
# Method: this is a true end-to-end test. It prepends a fake `curl` to PATH
# that serves canned Gitea API responses keyed by URL, then runs the REAL
# sop-tier-check.sh. The fake exercises the genuine probe→credit→evaluate
# path — no logic is re-implemented in the test.

set -euo pipefail

THIS_DIR="$(cd "$(dirname "$0")" && pwd)"
SCRIPT_DIR="$(cd "$THIS_DIR/.." && pwd)"
SCRIPT="$SCRIPT_DIR/sop-tier-check.sh"

command -v jq >/dev/null 2>&1 || { echo "::error::jq required but not found"; exit 1; }
[ -f "$SCRIPT" ] || { echo "::error::sop-tier-check.sh not found at $SCRIPT — test must fail loudly if the script is absent"; exit 1; }

# sop-tier-check.sh uses `declare -A` (associative arrays), which require
# bash >= 4. CI runners (Ubuntu) ship bash 5; macOS ships 3.2. Resolve a
# bash >= 4 to run the script under.
pick_bash() {
  local c
  for c in bash /opt/homebrew/bin/bash /usr/local/bin/bash /bin/bash; do
    local p; p="$(command -v "$c" 2>/dev/null || true)"
    [ -n "$p" ] || continue
    local maj; maj="$("$p" -c 'echo "${BASH_VERSINFO[0]}"' 2>/dev/null || echo 0)"
    if [ "${maj:-0}" -ge 4 ]; then echo "$p"; return 0; fi
  done
  return 1
}
BASH4="$(pick_bash)" || { echo "::error::need bash >= 4 to run sop-tier-check.sh (associative arrays); none found"; exit 1; }
echo "using bash: $BASH4 ($("$BASH4" -c 'echo $BASH_VERSION'))"

PASS=0
FAIL=0

assert_eq() {
  local label="$1" expected="$2" got="$3"
  if [ "$expected" = "$got" ]; then
    echo "  PASS  $label"
    PASS=$((PASS + 1))
  else
    echo "  FAIL  $label"
    echo "        expected: <$expected>"
    echo "        got:      <$got>"
    FAIL=$((FAIL + 1))
  fi
}

assert_contains() {
  local label="$1" haystack="$2" needle="$3"
  if printf '%s' "$haystack" | grep -qF -- "$needle"; then
    echo "  PASS  $label"
    PASS=$((PASS + 1))
  else
    echo "  FAIL  $label (missing substring: <$needle>)"
    FAIL=$((FAIL + 1))
  fi
}

assert_not_contains() {
  local label="$1" haystack="$2" needle="$3"
  if printf '%s' "$haystack" | grep -qF -- "$needle"; then
    echo "  FAIL  $label (unexpected substring present: <$needle>)"
    FAIL=$((FAIL + 1))
  else
    echo "  PASS  $label"
    PASS=$((PASS + 1))
  fi
}

# ---------------------------------------------------------------------------
# Fake-curl harness.
#
# The real script calls curl in two shapes:
#   (a) body capture:   curl -sS -H AUTH URL                 -> prints JSON body
#   (b) http-code:      curl -sS -o FILE -w '%{http_code}' -H AUTH URL
#   (c) http-code only: curl -sS -o /dev/null -w '%{http_code}' -H AUTH URL
#
# Our fake reads the URL (last non-flag arg), looks up a response in fixture
# files under $FIXDIR, and emits body and/or http-code accordingly.
# ---------------------------------------------------------------------------

make_harness() {
  # $1 = scenario dir to populate with fixtures
  local FIXDIR="$1"
  local BIN="$FIXDIR/bin"
  mkdir -p "$BIN"
  cat > "$BIN/curl" <<'FAKE'
#!/usr/bin/env bash
# Fake curl for sop-tier-check authz tests. Looks up canned responses by URL.
set -u
FIXDIR="${SOP_TEST_FIXDIR:?SOP_TEST_FIXDIR unset}"

url=""
out=""
want_code="no"
prev=""
for a in "$@"; do
  case "$prev" in
    -o) out="$a" ;;
  esac
  case "$a" in
    http*://*) url="$a" ;;
    '%{http_code}') want_code="yes" ;;
  esac
  # -w '%{http_code}' arrives as the value of the -w flag
  if [ "$prev" = "-w" ] && [ "$a" = '%{http_code}' ]; then want_code="yes"; fi
  prev="$a"
done

# Map URL -> fixture key (a filename-safe slug).
# We only need the path after /api/v1.
path="${url#*/api/v1}"
slug="$(printf '%s' "$path" | tr '/?=&' '____')"

body_file="$FIXDIR/body${slug}"
code_file="$FIXDIR/code${slug}"

# Emit body to -o target (or capture for stdout) when a body fixture exists.
body=""
if [ -f "$body_file" ]; then body="$(cat "$body_file")"; fi
if [ -n "$out" ]; then
  printf '%s' "$body" > "$out"
else
  printf '%s' "$body"
fi

# Emit http code when requested.
if [ "$want_code" = "yes" ]; then
  if [ -f "$code_file" ]; then
    printf '%s' "$(cat "$code_file")"
  else
    printf '200'
  fi
fi
exit 0
FAKE
  chmod +x "$BIN/curl"
  echo "$BIN"
}

# Common fixtures shared by scenarios. $1 = FIXDIR, $2 = approver login,
# $3 = tier label name (e.g. tier:high), $4 = teams JSON.
seed_common() {
  local FIXDIR="$1" approver="$2" tier="$3" teams_json="$4"
  mkdir -p "$FIXDIR"
  # /user -> whoami
  printf '%s' '{"login":"sop-bot"}' > "$FIXDIR/body_user"
  # PR head sha
  printf '%s' '{"head":{"sha":"headsha1"}}' \
    > "$FIXDIR/body_repos_molecule-ai_molecule-core_pulls_42"
  # labels
  printf '%s' "[{\"name\":\"$tier\"}]" \
    > "$FIXDIR/body_repos_molecule-ai_molecule-core_issues_42_labels"
  # org teams list
  printf '%s' "$teams_json" > "$FIXDIR/body_orgs_molecule-ai_teams"
  printf '%s' '200' > "$FIXDIR/code_orgs_molecule-ai_teams"
  # reviews: one APPROVED on current head by $approver
  printf '%s' "[{\"state\":\"APPROVED\",\"commit_id\":\"headsha1\",\"user\":{\"login\":\"$approver\"}}]" \
    > "$FIXDIR/body_repos_molecule-ai_molecule-core_pulls_42_reviews"
}

run_script() {
  # $1 = FIXDIR (must contain bin/curl). Returns combined stdout+stderr; sets RC.
  local FIXDIR="$1"
  local BIN="$FIXDIR/bin"
  set +e
  OUT=$(
    SOP_TEST_FIXDIR="$FIXDIR" \
    PATH="$BIN:$PATH" \
    GITEA_TOKEN="faketoken" \
    GITEA_HOST="git.moleculesai.app" \
    REPO="molecule-ai/molecule-core" \
    PR_NUMBER="42" \
    PR_AUTHOR="pr-author" \
    SOP_DEBUG="0" \
    SOP_LEGACY_CHECK="0" \
    "$BASH4" "$SCRIPT" 2>&1
  )
  RC=$?
  set -e
  printf '%s' "$OUT"
  return $RC
}

TEAMS_JSON='[{"name":"ceo","id":10},{"name":"engineers","id":11},{"name":"managers","id":12}]'

echo "=============================================================="
echo "Scenario 1: tier:high, team probe 403 (cannot read), approver"
echo "            is a plain org member but NOT in ceo team."
echo "            EXPECT: tier NOT granted (fail-closed cannot-verify)."
echo "=============================================================="
S1="$(mktemp -d)"
make_harness "$S1" >/dev/null
seed_common "$S1" "org-only-bob" "tier:high" "$TEAMS_JSON"
# Team membership probe for ceo (id=10) returns 403 — cannot read.
printf '%s' '403' > "$S1/code_teams_10_members_org-only-bob"
# The OLD bug path: org membership probe would 204 and synthetic-credit.
printf '%s' '204' > "$S1/code_orgs_molecule-ai_members_org-only-bob"
set +e
OUT1="$(run_script "$S1")"; RC1=$?
set -e
echo "$OUT1" | sed 's/^/    /'
echo "    (exit=$RC1)"
assert_eq "S1 exit non-zero (tier NOT granted)" "1" "$([ "$RC1" -ne 0 ] && echo 1 || echo 0)"
assert_not_contains "S1 did NOT print PASSED" "$OUT1" "sop-tier-check PASSED"
assert_contains "S1 cannot-verify error surfaced" "$OUT1" "CANNOT VERIFY"
assert_contains "S1 names the unreadable probe (403)" "$OUT1" "HTTP 403"
rm -rf "$S1"

echo
echo "=============================================================="
echo "Scenario 2: tier:high, genuine ceo team member (probe 204)."
echo "            EXPECT: tier GRANTED."
echo "=============================================================="
S2="$(mktemp -d)"
make_harness "$S2" >/dev/null
seed_common "$S2" "real-ceo" "tier:high" "$TEAMS_JSON"
printf '%s' '204' > "$S2/code_teams_10_members_real-ceo"   # ceo team: member
set +e
OUT2="$(run_script "$S2")"; RC2=$?
set -e
echo "$OUT2" | sed 's/^/    /'
echo "    (exit=$RC2)"
assert_eq "S2 exit zero (granted)" "0" "$RC2"
assert_contains "S2 printed PASSED" "$OUT2" "sop-tier-check PASSED"
rm -rf "$S2"

echo
echo "=============================================================="
echo "Scenario 3: tier:high, approver is an org member but a VERIFIED"
echo "            non-member of ceo (team probe 404). Org probe would"
echo "            204 — must NEVER be synthetic-credited."
echo "            EXPECT: tier NOT granted (clause FAIL), no fallback."
echo "=============================================================="
S3="$(mktemp -d)"
make_harness "$S3" >/dev/null
seed_common "$S3" "org-member-carol" "tier:high" "$TEAMS_JSON"
printf '%s' '404' > "$S3/code_teams_10_members_org-member-carol"  # verified NOT in ceo
printf '%s' '204' > "$S3/code_orgs_molecule-ai_members_org-member-carol" # org member (must be ignored)
set +e
OUT3="$(run_script "$S3")"; RC3=$?
set -e
echo "$OUT3" | sed 's/^/    /'
echo "    (exit=$RC3)"
assert_eq "S3 exit non-zero (tier NOT granted)" "1" "$([ "$RC3" -ne 0 ] && echo 1 || echo 0)"
assert_not_contains "S3 did NOT print PASSED" "$OUT3" "sop-tier-check PASSED"
assert_contains "S3 reported a real clause FAIL (not cannot-verify)" "$OUT3" "FAILED for tier:high"
assert_not_contains "S3 did NOT cannot-verify (404 is a verified negative)" "$OUT3" "CANNOT VERIFY"
rm -rf "$S3"

echo
echo "------"
echo "PASS=$PASS FAIL=$FAIL"
[ "$FAIL" -eq 0 ]
