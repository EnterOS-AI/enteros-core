#!/usr/bin/env bash
# Regression tests for .gitea/scripts/lib/ci-status.sh — the SSOT library for
# the governance status-emission logic shared by reserved-path-review /
# secret-scan. Closes the test-coverage gap flagged in the molecule-code-reviewer
# REQUEST_CHANGES on PR #3422:
#
#   (1) internal-host derivation (GITHUB_SERVER_URL -> internal Gitea, public
#       fallback, trailing-slash trim)                       — derive_gitea_base
#   (2) CI_STATUS_TOKEN resolution: direct Gitea org secret FIRST, Infisical
#       fallback, fail-closed vs best-effort                 — resolve_ci_status_token
#   (3) review-status emission + host-derivation path        — emit_review_status
#   (4) secret-scan re-assert idempotency / success-gating / best-effort /
#       event->sha mapping                                   — reassert_commit_status
#   (5) the workflows are actually WIRED to the lib (regression guard so a future
#       edit can't silently un-wire the fix from CI).
#
# Design (mirrors test_review_check.sh / test_jq_install.sh):
#   - custom assert_eq / assert_contains framework, no bats dependency.
#   - curl is mocked via a pure-bash shim injected through the lib's CI_STATUS_CURL
#     tunable — NO network, NO python HTTP fixture server (so the suite is green on
#     both the ubuntu CI runner and a dev box). python3 + jq are the real binaries.
#   - functions are driven IN the current shell (stdout redirected to a file, not
#     captured via $(...)), so the CI_STATUS_TOKEN global the function sets is
#     observable for assertions.

set -uo pipefail

THIS_DIR="$(cd "$(dirname "$0")" && pwd)"
LIB="$(cd "$THIS_DIR/../lib" && pwd)/ci-status.sh"
SCRIPTS_DIR="$(cd "$THIS_DIR/.." && pwd)"
WF_DIR="$(cd "$THIS_DIR/../../workflows" && pwd)"

PASS=0
FAIL=0
FAILED_TESTS=""

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
    FAILED_TESTS="${FAILED_TESTS} ${label}"
  fi
}

assert_contains() {
  local label="$1" needle="$2" haystack="$3"
  if printf '%s' "$haystack" | grep -qF "$needle"; then
    echo "  PASS  $label"
    PASS=$((PASS + 1))
  else
    echo "  FAIL  $label"
    echo "        needle:   <$needle>"
    echo "        haystack: <$(printf '%s' "$haystack" | head -c 300)>"
    FAIL=$((FAIL + 1))
    FAILED_TESTS="${FAILED_TESTS} ${label}"
  fi
}

assert_not_contains() {
  local label="$1" needle="$2" haystack="$3"
  if printf '%s' "$haystack" | grep -qF "$needle"; then
    echo "  FAIL  $label (unexpected: <$needle>)"
    FAIL=$((FAIL + 1))
    FAILED_TESTS="${FAILED_TESTS} ${label}"
  else
    echo "  PASS  $label"
    PASS=$((PASS + 1))
  fi
}

assert_file_contains() {
  local label="$1" path="$2" needle="$3"
  if [ -f "$path" ] && grep -qF "$needle" "$path"; then
    echo "  PASS  $label"
    PASS=$((PASS + 1))
  else
    echo "  FAIL  $label (needle not found in $path: <$needle>)"
    FAIL=$((FAIL + 1))
    FAILED_TESTS="${FAILED_TESTS} ${label}"
  fi
}

# --- existence + syntax -----------------------------------------------------
echo
echo "== existence =="
if [ -f "$LIB" ]; then
  echo "  PASS  lib exists: $LIB"
  PASS=$((PASS + 1))
else
  echo "  FAIL  lib not found: $LIB"
  echo "Cannot proceed."
  exit 1
fi

echo
echo "== bash syntax =="
if bash -n "$LIB" 2>&1; then
  echo "  PASS  bash -n passes"
  PASS=$((PASS + 1))
else
  echo "  FAIL  bash -n failed"
  FAIL=$((FAIL + 1))
  FAILED_TESTS="${FAILED_TESTS} bash_n"
fi

# --- mock curl --------------------------------------------------------------
# Behavior driven by MOCK_* env vars. Records every call (URL) to MOCK_CALL_LOG.
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
MOCK_CURL="$WORK/curl"
cat > "$MOCK_CURL" <<'MOCK'
#!/usr/bin/env bash
# Mock curl for test_ci_status.sh. Dispatches on the request URL.
url=""; out=""; data=""; want_code=0
args=("$@")
i=0
while [ "$i" -lt "${#args[@]}" ]; do
  a="${args[$i]}"
  case "$a" in
    -o) i=$((i+1)); out="${args[$i]:-}" ;;
    -d) i=$((i+1)); data="${args[$i]:-}" ;;
    -w) i=$((i+1)); want_code=1 ;;
    -A|-H|-K|-X) i=$((i+1)) ;;   # skip the value arg for these flags
    http://*|https://*) url="$a" ;;
  esac
  i=$((i+1))
done
[ -n "${MOCK_CALL_LOG:-}" ] && printf '%s\n' "$url" >> "$MOCK_CALL_LOG"
emit_code() { [ "$want_code" = "1" ] && printf '%s' "$1"; }
case "$url" in
  *"/auth/universal-auth/login"*)
    [ "${MOCK_INFISICAL_FAIL:-0}" = "1" ] && exit 22
    printf '{"accessToken":"%s"}' "${MOCK_INFISICAL_ACCESSTOKEN:-}"
    ;;
  *"/secrets/raw/CI_STATUS_TOKEN"*)
    [ "${MOCK_INFISICAL_FAIL:-0}" = "1" ] && exit 22
    printf '{"secret":{"secretValue":"%s"}}' "${MOCK_INFISICAL_SECRETVALUE:-}"
    ;;
  *"/statuses/"*)
    [ -n "${MOCK_POST_BODY_OUT:-}" ] && printf '%s' "$data" > "$MOCK_POST_BODY_OUT"
    [ -n "${MOCK_POST_URL_OUT:-}" ]  && printf '%s' "$url"  > "$MOCK_POST_URL_OUT"
    emit_code "${MOCK_POST_HTTP:-201}"
    ;;
  *"/pulls/"*)
    [ -n "$out" ] && printf '{"head":{"sha":"%s"}}' "${MOCK_PR_HEADSHA:-abc123head}" > "$out"
    emit_code "${MOCK_PR_HTTP:-200}"
    ;;
  *)
    emit_code "000"
    ;;
esac
exit 0
MOCK
chmod +x "$MOCK_CURL"

# shellcheck source=/dev/null
source "$LIB"

# Reset every env var the lib reads so cases don't leak into each other. The
# MOCK_* control vars are read by the mock curl SUBPROCESS, so they must be
# exported; we (re-)declare them exported-empty here, and a case's plain
# `MOCK_x=...` assignment keeps the export attribute. Function INPUT vars
# (REPO, PR_NUMBER, CI_STATUS_TOKEN, ...) are read by the in-shell function, so
# they need no export.
reset_env() {
  unset CI_STATUS_TOKEN CI_STATUS_TOKEN_DIRECT REQUIRE \
        INFISICAL_CI_CLIENT_ID INFISICAL_CI_CLIENT_SECRET INFISICAL_PROJECT_ID INFISICAL_BASE_URL \
        GITHUB_ENV GITHUB_SERVER_URL GITEA_HOST \
        REPO PR_NUMBER EVAL_OUTCOME STATUS_CONTEXT \
        EVENT_NAME PR_HEAD_SHA PUSH_SHA CONTEXT_BASE DESCRIPTION \
        MOCK_CALL_LOG MOCK_INFISICAL_FAIL MOCK_INFISICAL_ACCESSTOKEN MOCK_INFISICAL_SECRETVALUE \
        MOCK_PR_HTTP MOCK_PR_HEADSHA MOCK_POST_HTTP MOCK_POST_BODY_OUT MOCK_POST_URL_OUT 2>/dev/null || true
  export CI_STATUS_CURL="$MOCK_CURL"
  export MOCK_CALL_LOG="" MOCK_INFISICAL_FAIL="" MOCK_INFISICAL_ACCESSTOKEN="" \
         MOCK_INFISICAL_SECRETVALUE="" MOCK_PR_HTTP="" MOCK_PR_HEADSHA="" \
         MOCK_POST_HTTP="" MOCK_POST_BODY_OUT="" MOCK_POST_URL_OUT=""
}

# Run a lib function in-shell (stdout+stderr -> $OUT_FILE, so the CI_STATUS_TOKEN
# global the function sets remains observable). Does NOT reset CI_STATUS_TOKEN —
# it is an INPUT for emit/reassert and an OUTPUT for resolve (reset_env already
# unset it before the case set its inputs). Sets RC and OUT.
OUT_FILE="$WORK/out"
run_fn() {
  "$@" > "$OUT_FILE" 2>&1
  RC=$?
  OUT=$(cat "$OUT_FILE")
}

# ===========================================================================
# (1) derive_gitea_base — internal-host derivation
# ===========================================================================
echo
echo "== derive_gitea_base (host derivation) =="

reset_env; GITHUB_SERVER_URL="http://molecule-gitea-local:3000"
assert_eq "D1 internal GITHUB_SERVER_URL preferred" "http://molecule-gitea-local:3000" "$(derive_gitea_base)"

reset_env; GITEA_HOST="git.moleculesai.app"
assert_eq "D2 fallback to https://GITEA_HOST when GITHUB_SERVER_URL empty" "https://git.moleculesai.app" "$(derive_gitea_base)"

reset_env
assert_eq "D3 literal default when both unset" "https://git.moleculesai.app" "$(derive_gitea_base)"

reset_env; GITHUB_SERVER_URL="http://molecule-gitea-local:3000/"
assert_eq "D4 trailing slash trimmed (no // before /api/v1)" "http://molecule-gitea-local:3000" "$(derive_gitea_base)"

# ===========================================================================
# (2) resolve_ci_status_token — direct-first, Infisical fallback
# ===========================================================================
echo
echo "== resolve_ci_status_token (token resolution) =="

# R1 — direct Gitea org secret present → used verbatim, NO external round-trip.
reset_env
export MOCK_CALL_LOG="$WORK/r1_calls"; : > "$MOCK_CALL_LOG"
CI_STATUS_TOKEN_DIRECT="direct-tok-123"
run_fn resolve_ci_status_token
assert_eq  "R1 rc 0" "0" "$RC"
assert_eq  "R1 token == direct secret" "direct-tok-123" "$CI_STATUS_TOKEN"
assert_contains "R1 log says Gitea org secret" "resolved from Gitea org secret" "$OUT"
assert_eq  "R1 NO curl calls (no Infisical round-trip)" "0" "$(wc -l < "$MOCK_CALL_LOG" | tr -d ' ')"
assert_contains "R1 token masked" "::add-mask::direct-tok-123" "$OUT"

# R2 — direct empty → Infisical login + secret read fallback succeeds.
reset_env
export MOCK_CALL_LOG="$WORK/r2_calls"; : > "$MOCK_CALL_LOG"
INFISICAL_CI_CLIENT_ID="cid"; INFISICAL_CI_CLIENT_SECRET="csec"; INFISICAL_PROJECT_ID="pid"
MOCK_INFISICAL_ACCESSTOKEN="itok-xyz"; MOCK_INFISICAL_SECRETVALUE="infisical-tok-456"
run_fn resolve_ci_status_token
assert_eq  "R2 rc 0" "0" "$RC"
assert_eq  "R2 token == Infisical secretValue" "infisical-tok-456" "$CI_STATUS_TOKEN"
assert_contains "R2 log says Infisical fallback" "resolved from Infisical fallback" "$OUT"
assert_contains "R2 login endpoint called" "/auth/universal-auth/login" "$(cat "$WORK/r2_calls")"
assert_contains "R2 secret-read endpoint called" "/secrets/raw/CI_STATUS_TOKEN" "$(cat "$WORK/r2_calls")"

# R3 — direct empty + Infisical login returns empty accessToken + REQUIRE=1 → fail closed.
reset_env
REQUIRE="1"
INFISICAL_CI_CLIENT_ID="cid"; INFISICAL_CI_CLIENT_SECRET="csec"; INFISICAL_PROJECT_ID="pid"
MOCK_INFISICAL_ACCESSTOKEN=""    # login yields no token
run_fn resolve_ci_status_token
assert_eq  "R3 rc 1 (fail closed)" "1" "$RC"
assert_eq  "R3 token empty" "" "$CI_STATUS_TOKEN"
assert_contains "R3 ::error:: failing closed" "::error::CI_STATUS_TOKEN absent" "$OUT"

# R4 — direct empty + no Infisical creds + REQUIRE=0 → best-effort skip (rc 0).
reset_env
REQUIRE="0"
run_fn resolve_ci_status_token
assert_eq  "R4 rc 0 (best-effort)" "0" "$RC"
assert_eq  "R4 token empty" "" "$CI_STATUS_TOKEN"
assert_contains "R4 ::warning:: unavailable" "::warning::CI_STATUS_TOKEN unavailable" "$OUT"

# R5 — GITHUB_ENV export on success (cross-step consumption).
reset_env
GITHUB_ENV="$WORK/gh_env"; : > "$GITHUB_ENV"
CI_STATUS_TOKEN_DIRECT="env-tok-789"
run_fn resolve_ci_status_token
assert_file_contains "R5 CI_STATUS_TOKEN appended to GITHUB_ENV" "$GITHUB_ENV" "CI_STATUS_TOKEN=env-tok-789"

# R6 — Infisical fallback network failure + REQUIRE=1 → fail closed (curl -fsS exits nonzero).
reset_env
REQUIRE="1"
INFISICAL_CI_CLIENT_ID="cid"; INFISICAL_CI_CLIENT_SECRET="csec"; INFISICAL_PROJECT_ID="pid"
MOCK_INFISICAL_FAIL="1"
run_fn resolve_ci_status_token
assert_eq  "R6 rc 1 (Infisical unreachable, fail closed)" "1" "$RC"
assert_contains "R6 ::error:: failing closed" "::error::CI_STATUS_TOKEN absent" "$OUT"

# ===========================================================================
# (3) emit_review_status — review-status POST + host-derivation path
# ===========================================================================
echo
echo "== emit_review_status (review-status emission) =="

# E1 — success outcome → posts state=success with the exact BP context.
reset_env
GITHUB_SERVER_URL="http://molecule-gitea-local:3000"
REPO="molecule-ai/molecule-core"; PR_NUMBER="3422"; EVAL_OUTCOME="success"
STATUS_CONTEXT="reserved-path-review / reserved-path-review (pull_request_target)"
CI_STATUS_TOKEN="tok"
MOCK_PR_HEADSHA="deadbeefsha"
export MOCK_POST_BODY_OUT="$WORK/e1_body"; export MOCK_POST_URL_OUT="$WORK/e1_url"
run_fn emit_review_status
assert_eq  "E1 rc 0" "0" "$RC"
assert_contains "E1 POST body state=success" '"state":"success"' "$(cat "$WORK/e1_body")"
assert_contains "E1 POST body carries STATUS_CONTEXT" "reserved-path-review / reserved-path-review (pull_request_target)" "$(cat "$WORK/e1_body")"
assert_contains "E1 POST hit internal-derived host" "http://molecule-gitea-local:3000/api/v1/repos/molecule-ai/molecule-core/statuses/deadbeefsha" "$(cat "$WORK/e1_url")"
assert_contains "E1 notice posted success" "::notice::posted success" "$OUT"

# E2 — non-success eval outcome → posts state=failure.
reset_env
REPO="molecule-ai/molecule-core"; PR_NUMBER="3422"; EVAL_OUTCOME="failure"
STATUS_CONTEXT="reserved-path-review / reserved-path-review (pull_request_target)"
CI_STATUS_TOKEN="tok"
export MOCK_POST_BODY_OUT="$WORK/e2_body"
run_fn emit_review_status
assert_eq  "E2 rc 0" "0" "$RC"
assert_contains "E2 POST body state=failure" '"state":"failure"' "$(cat "$WORK/e2_body")"
assert_contains "E2 reserved-path context posted" "reserved-path-review / reserved-path-review (pull_request_target)" "$(cat "$WORK/e2_body")"

# E3 — GET /pulls non-200 → loud failure (rc 1).
reset_env
REPO="molecule-ai/molecule-core"; PR_NUMBER="3422"; EVAL_OUTCOME="success"
STATUS_CONTEXT="reserved-path-review / reserved-path-review (pull_request_target)"; CI_STATUS_TOKEN="tok"
MOCK_PR_HTTP="500"
run_fn emit_review_status
assert_eq  "E3 rc 1 (GET failed)" "1" "$RC"
assert_contains "E3 ::error:: GET" "::error::GET /pulls/3422 returned HTTP 500" "$OUT"

# E4 — POST /statuses non-2xx → loud failure (rc 1); emission is fail-closed.
reset_env
REPO="molecule-ai/molecule-core"; PR_NUMBER="3422"; EVAL_OUTCOME="success"
STATUS_CONTEXT="reserved-path-review / reserved-path-review (pull_request_target)"; CI_STATUS_TOKEN="tok"
MOCK_POST_HTTP="500"
run_fn emit_review_status
assert_eq  "E4 rc 1 (POST failed)" "1" "$RC"
assert_contains "E4 ::error:: POST" "::error::POST /statuses/" "$OUT"

# ===========================================================================
# (4) reassert_commit_status — secret-scan re-assert
# ===========================================================================
echo
echo "== reassert_commit_status (secret-scan re-assert) =="

# A1 — token present, pull_request event, POST 201 → re-asserts, rc 0.
reset_env
REPO="molecule-ai/molecule-core"; EVENT_NAME="pull_request"; PR_HEAD_SHA="prheadsha"
CONTEXT_BASE="Secret scan / Scan diff for credential-shaped strings"
CI_STATUS_TOKEN="tok"
export MOCK_POST_BODY_OUT="$WORK/a1_body"; export MOCK_POST_URL_OUT="$WORK/a1_url"
run_fn reassert_commit_status
assert_eq  "A1 rc 0" "0" "$RC"
assert_contains "A1 notice re-asserted with (pull_request) suffix" 'context="Secret scan / Scan diff for credential-shaped strings (pull_request)"' "$OUT"
assert_contains "A1 POST body state=success" '"state":"success"' "$(cat "$WORK/a1_body")"
assert_contains "A1 POST targets PR head sha" "/statuses/prheadsha" "$(cat "$WORK/a1_url")"

# A2 — no token (fork PR) → warn + skip, NO POST, rc 0.
reset_env
REPO="molecule-ai/molecule-core"; EVENT_NAME="pull_request"; PR_HEAD_SHA="prheadsha"
CONTEXT_BASE="Secret scan / Scan diff for credential-shaped strings"
export MOCK_CALL_LOG="$WORK/a2_calls"; : > "$MOCK_CALL_LOG"
run_fn reassert_commit_status
assert_eq  "A2 rc 0 (best-effort)" "0" "$RC"
assert_contains "A2 warning no token" "::warning::re-assert: no CI_STATUS_TOKEN" "$OUT"
assert_eq  "A2 NO POST attempted" "0" "$(wc -l < "$MOCK_CALL_LOG" | tr -d ' ')"

# A3 — push event → uses PUSH_SHA + (push) suffix.
reset_env
REPO="molecule-ai/molecule-core"; EVENT_NAME="push"; PUSH_SHA="pushsha999"
CONTEXT_BASE="Secret scan / Scan diff for credential-shaped strings"
CI_STATUS_TOKEN="tok"
export MOCK_POST_URL_OUT="$WORK/a3_url"
run_fn reassert_commit_status
assert_eq  "A3 rc 0" "0" "$RC"
assert_contains "A3 notice has (push) suffix" "(push)\" on sha=pushsha999" "$OUT"
assert_contains "A3 POST targets push sha" "/statuses/pushsha999" "$(cat "$WORK/a3_url")"

# A4 — POST non-2xx → best-effort warning, still rc 0 (NEVER fails the gate).
reset_env
REPO="molecule-ai/molecule-core"; EVENT_NAME="pull_request"; PR_HEAD_SHA="prheadsha"
CONTEXT_BASE="Secret scan / Scan diff for credential-shaped strings"
CI_STATUS_TOKEN="tok"; MOCK_POST_HTTP="500"
run_fn reassert_commit_status
assert_eq  "A4 rc 0 (best-effort even on POST 500)" "0" "$RC"
assert_contains "A4 warning HTTP 500" "::warning::re-assert POST returned HTTP 500" "$OUT"

# A5 — event resolves to empty SHA → warn + skip, rc 0.
reset_env
REPO="molecule-ai/molecule-core"; EVENT_NAME="pull_request"; PR_HEAD_SHA=""
CONTEXT_BASE="Secret scan / Scan diff for credential-shaped strings"
CI_STATUS_TOKEN="tok"
export MOCK_CALL_LOG="$WORK/a5_calls"; : > "$MOCK_CALL_LOG"
run_fn reassert_commit_status
assert_eq  "A5 rc 0" "0" "$RC"
assert_contains "A5 warning could not resolve SHA" "could not resolve target SHA" "$OUT"
assert_eq  "A5 NO POST attempted" "0" "$(wc -l < "$MOCK_CALL_LOG" | tr -d ' ')"

# ===========================================================================
# (5) workflow wiring — the fix must actually be wired into CI
# ===========================================================================
echo
echo "== workflow wiring (regression guard) =="

# W1/W2 (qa-review.yml, security-review.yml) removed 2026-07-14 — the SOP
# review gate was fully removed and those workflows deleted. secret-scan +
# reserved-path-review are the remaining emit_review_status consumers.
SS="$WF_DIR/secret-scan.yml"
RPR="$WF_DIR/reserved-path-review.yml"

assert_file_contains "W3 secret-scan sources ci-status lib"       "$SS"  ".gitea/scripts/lib/ci-status.sh"
assert_file_contains "W3 secret-scan calls reassert_commit_status" "$SS"  "reassert_commit_status"
assert_file_contains "W3 secret-scan passes CONTEXT_BASE"          "$SS"  "Secret scan / Scan diff for credential-shaped strings"

assert_file_contains "W6 reserved-path-review sources ci-status lib"    "$RPR" ".gitea/scripts/lib/ci-status.sh"
assert_file_contains "W6 reserved-path-review calls resolve_ci_status_token" "$RPR" "resolve_ci_status_token"
assert_file_contains "W6 reserved-path-review calls emit_review_status" "$RPR" "emit_review_status"
assert_file_contains "W6 reserved-path-review passes reserved STATUS_CONTEXT" "$RPR" "reserved-path-review / reserved-path-review (pull_request_target)"

# W5 (review-check.sh) removed 2026-07-14 — the qa/security review evaluator
# was deleted with the SOP gate. The derive_gitea_base SSOT is still covered by
# the D1–D4 cases above.

# W4 — the re-assert step MUST stay success()-gated (only reinforces GREEN).
echo
echo "== W4 secret-scan re-assert is success()-gated =="
W4=$(awk '/Re-assert Secret scan status/{found=1} found && /if:[[:space:]]*success\(\)/{print "OK"; exit}' "$SS")
assert_eq "W4 re-assert step has if: success()" "OK" "$W4"

# W7 — no dangling legacy inline-emission fragments may survive in the
# remaining review workflow(s): after the emit_review_status lib call the old
# inline variables ($post_code / $head_sha / $status_state) are UNSET, so a
# leftover `if [ "$post_code" ... ]` block would kill the step with `set -u`
# unbound-var at runtime (originally found live on qa-review.yml, PR #3422
# first revision; that workflow has since been deleted with the SOP gate).
echo
echo "== W7 no dangling legacy emission fragments =="
for wf in "$RPR"; do
  if grep -qE '\$\{?post_code' "$wf"; then
    echo "  FAIL  W7 $(basename "$wf") still references \$post_code (dangling legacy fragment)"
    FAIL=$((FAIL + 1)); FAILED_TESTS="${FAILED_TESTS} W7_$(basename "$wf")"
  else
    echo "  PASS  W7 $(basename "$wf") has no dangling \$post_code fragment"
    PASS=$((PASS + 1))
  fi
done

echo
echo "------"
echo "PASS=$PASS FAIL=$FAIL"
if [ "$FAIL" -gt 0 ]; then
  echo "Failed:$FAILED_TESTS"
fi
[ "$FAIL" -eq 0 ]
