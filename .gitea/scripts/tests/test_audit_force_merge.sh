#!/usr/bin/env bash
# test_audit_force_merge.sh — regression lock for audit-force-merge fail-closed
# behavior. Covers jq schema filters plus the real script against a fake Gitea
# API so pagination, validation, and incident decisions are exercised together.
#
# Usage: bash test_audit_force_merge.sh

set -euo pipefail

fail() { echo "FAIL: $*" >&2; exit 1; }
pass() { echo "PASS: $*"; }

[ -x "$(command -v jq)" ] || { echo "SKIP: jq not on PATH"; exit 0; }

HEAD_SHA="deadbeef00000000000000000000000000000000"
THIS_DIR="$(cd "$(dirname "$0")" && pwd)"
AUDIT_SCRIPT="$(cd "$THIS_DIR/.." && pwd)/audit-force-merge.sh"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

# The schema validation jq expression from audit-force-merge.sh.
validate_pr_schema() {
  jq -r '
    (.merged | type == "boolean") and
    (.merge_commit_sha | type == "string") and
    (.merged_by | type == "object") and (.merged_by.login | type == "string") and
    (.base | type == "object") and (.base.ref | type == "string") and
    (.head | type == "object") and (.head.sha | type == "string")
  '
}

validate_statuses_type() {
  jq -r '(.statuses | type) == "array"'
}

# T1 — valid PR payload → true
T1=$(echo '{"merged":true,"merge_commit_sha":"abc","merged_by":{"login":"u"},"base":{"ref":"main"},"head":{"sha":"def"}}' | validate_pr_schema)
[ "$T1" = "true" ] || fail "T1: valid payload should pass schema"
pass "T1: valid payload passes schema"

# T2 — merged=false (valid types) → true (schema is about types, not values)
T2=$(echo '{"merged":false,"merge_commit_sha":"abc","merged_by":{"login":"u"},"base":{"ref":"main"},"head":{"sha":"def"}}' | validate_pr_schema)
[ "$T2" = "true" ] || fail "T2: merged=false with valid types should pass schema"
pass "T2: merged=false with valid types passes schema"

# T3 — missing merged field → false
T3=$(echo '{"merge_commit_sha":"abc","merged_by":{"login":"u"},"base":{"ref":"main"},"head":{"sha":"def"}}' | validate_pr_schema)
[ "$T3" = "false" ] || fail "T3: missing merged should fail schema"
pass "T3: missing merged fails schema"

# T4 — merged is string "true" instead of boolean → false
T4=$(echo '{"merged":"true","merge_commit_sha":"abc","merged_by":{"login":"u"},"base":{"ref":"main"},"head":{"sha":"def"}}' | validate_pr_schema)
[ "$T4" = "false" ] || fail "T4: merged as string should fail schema"
pass "T4: merged as string fails schema"

# T5 — merge_commit_sha is null → false
T5=$(echo '{"merged":true,"merge_commit_sha":null,"merged_by":{"login":"u"},"base":{"ref":"main"},"head":{"sha":"def"}}' | validate_pr_schema)
[ "$T5" = "false" ] || fail "T5: null merge_commit_sha should fail schema"
pass "T5: null merge_commit_sha fails schema"

# T6 — merged_by is null → false
T6=$(echo '{"merged":true,"merge_commit_sha":"abc","merged_by":null,"base":{"ref":"main"},"head":{"sha":"def"}}' | validate_pr_schema)
[ "$T6" = "false" ] || fail "T6: null merged_by should fail schema"
pass "T6: null merged_by fails schema"

# T7 — base.ref is number → false
T7=$(echo '{"merged":true,"merge_commit_sha":"abc","merged_by":{"login":"u"},"base":{"ref":123},"head":{"sha":"def"}}' | validate_pr_schema)
[ "$T7" = "false" ] || fail "T7: numeric base.ref should fail schema"
pass "T7: numeric base.ref fails schema"

# T8 — head is missing → false
T8=$(echo '{"merged":true,"merge_commit_sha":"abc","merged_by":{"login":"u"},"base":{"ref":"main"}}' | validate_pr_schema)
[ "$T8" = "false" ] || fail "T8: missing head should fail schema"
pass "T8: missing head fails schema"

# T9 — statuses missing → false
T9=$(echo '{}' | validate_statuses_type)
[ "$T9" = "false" ] || fail "T9: missing statuses should fail type check"
pass "T9: missing statuses fails type check"

# T10 — statuses is string → false
T10=$(echo '{"statuses":"unexpected"}' | validate_statuses_type)
[ "$T10" = "false" ] || fail "T10: string statuses should fail type check"
pass "T10: string statuses fails type check"

# T11 — statuses is null → false
T11=$(echo '{"statuses":null}' | validate_statuses_type)
[ "$T11" = "false" ] || fail "T11: null statuses should fail type check"
pass "T11: null statuses fails type check"

# T12 — statuses is array → true
T12=$(echo '{"statuses":[{"context":"c1","status":"success"}]}' | validate_statuses_type)
[ "$T12" = "true" ] || fail "T12: array statuses should pass type check"
pass "T12: array statuses passes type check"

# T13 — empty array statuses → true
T13=$(echo '{"statuses":[]}' | validate_statuses_type)
[ "$T13" = "true" ] || fail "T13: empty array statuses should pass type check"
pass "T13: empty array statuses passes type check"

# T14-T16: REQUIRED_CHECKS_JSON branch entry validation
validate_required_checks_json() {
  local branch="$1"
  local json="$2"
  echo "$json" | jq -r --arg branch "$branch" 'has($branch) and (.[$branch] | type == "array")'
}

# T14 — branch exists and is array → true
T14=$(validate_required_checks_json "main" '{"main":["CI / all-required"]}')
[ "$T14" = "true" ] || fail "T14: existing array branch should pass"
pass "T14: existing array branch passes"

# T15 — branch missing → false
T15=$(validate_required_checks_json "staging" '{"main":["CI / all-required"]}')
[ "$T15" = "false" ] || fail "T15: missing branch should fail"
pass "T15: missing branch fails"

# T16 — branch entry is string instead of array → false
T16=$(validate_required_checks_json "main" '{"main":"CI / all-required"}')
[ "$T16" = "false" ] || fail "T16: string branch entry should fail"
pass "T16: string branch entry fails"

# ---------------------------------------------------------------------------
# T17+ — /statuses pagination (status-pagination RCA, #2440-family).
# The reader now pages /commits/{sha}/statuses to exhaustion instead of reading
# the capped combined /status view. These lock the page-accumulation,
# newest-wins collapse, short-page stop, and fail-closed contracts.
# ---------------------------------------------------------------------------

# Page-body type validator used per page (bare array, not an object).
validate_page_is_array() { jq -e 'type == "array"' >/dev/null 2>&1 && echo true || echo false; }

# newest-wins collapse: mirror the script's max-by-id jq (order-independent).
collapse_newest_per_context() {
  declare -A CS
  while IFS=$'\t' read -r ctx state; do
    [ -n "$ctx" ] && CS[$ctx]="$state"
  done < <(jq -r 'group_by(.context) | map(max_by(.id)) | .[] | "\(.context)\t\(.status)"')
  state="${CS[CI / all-required (push)]:-missing}"
  echo "$state"
}

# T17 — a bare JSON array page passes the per-page array check.
T17=$(echo '[{"context":"c1","status":"success"}]' | validate_page_is_array)
[ "$T17" = "true" ] || fail "T17: bare array page should pass array check"
pass "T17: bare array page passes array check"

# T18 — a non-array page (object) fails the per-page array check → fail-closed.
T18=$(echo '{"statuses":[]}' | validate_page_is_array)
[ "$T18" = "false" ] || fail "T18: object page should fail array check (fail-closed)"
pass "T18: object page fails array check (fail-closed)"

# T19 — required SUCCESS on PAGE 2 is FOUND after accumulation (not missing).
#   page1: 100 noise rows (older ids); page2: the required-context success.
PAGE1=$(jq -nc '[range(0;100) | {id:., context:("noise-\(.) (push)"), status:"pending"}]')
PAGE2='[{"id":200,"context":"CI / all-required (push)","status":"success"}]'
# Accumulation matching the script: two-arg `jq -s '.[0] + .[1]'` over the
# running accumulator and the new page.
ACCUM=$(jq -s '.[0] + .[1]' <(echo "$PAGE1") <(echo "$PAGE2"))
LEN=$(echo "$ACCUM" | jq 'length')
[ "$LEN" = "101" ] || fail "T19: accumulated length should be 101, got $LEN"
RESULT=$(echo "$ACCUM" | collapse_newest_per_context)
[ "$RESULT" = "success" ] || fail "T19: required success on page2 must be FOUND, got '$RESULT'"
pass "T19: required success on page2 is found after pagination"

# T20 — genuinely-absent required context across all pages stays 'missing'
#       → fail-closed (counted as not-green, flags the force-merge).
ABSENT=$(jq -nc '[range(0;100) | {id:., context:("noise-\(.) (push)"), status:"success"}]')
RESULT2=$(echo "$ABSENT" | collapse_newest_per_context)
[ "$RESULT2" = "missing" ] || fail "T20: absent required context must stay 'missing', got '$RESULT2'"
pass "T20: genuinely-absent required context stays missing (fail-closed)"

# T21 — non-monotonic order: newest id (157, neither first nor last in list)
#       a NEWER success row (oldest-first append → last overwrite wins).
DUP='[{"id":155,"context":"CI / all-required (push)","status":"pending"},
      {"id":157,"context":"CI / all-required (push)","status":"success"},
      {"id":125,"context":"CI / all-required (push)","status":"failure"}]'
RESULT3=$(echo "$DUP" | collapse_newest_per_context)
[ "$RESULT3" = "success" ] || fail "T21: newest (success) must win over older (failure), got '$RESULT3'"
pass "T21: newest row per context wins after pagination collapse"

# T22 — a non-empty short page MUST continue. Gitea silently caps `limit=100`
#       to 50 on this installation, so `count < requested limit` is not proof
#       that the collection is exhausted.
PAGE_COUNT=$(echo "$PAGE2" | jq 'length')   # 1 row
if [ "$PAGE_COUNT" -eq 0 ]; then SHORT="stop"; else SHORT="continue"; fi
[ "$SHORT" = "continue" ] || fail "T22: non-empty short page should continue pagination"
pass "T22: non-empty short page continues pagination"

# T23 — only an explicit empty page terminates pagination.
EMPTY='[]'
EMPTY_COUNT=$(echo "$EMPTY" | jq 'length')
if [ "$EMPTY_COUNT" -eq 0 ]; then EMPTY_RESULT="stop"; else EMPTY_RESULT="continue"; fi
[ "$EMPTY_RESULT" = "stop" ] || fail "T23: empty page should stop pagination"
pass "T23: explicit empty page stops pagination"

# ---------------------------------------------------------------------------
# T24+ — execute the real audit script against a fake Gitea API. These are the
# behavioral regressions; the smaller jq checks above merely document helpers.
# The fake server deliberately returns fewer rows than the requested limit to
# model Gitea's configured api.max_response_items=50 clamp.
# ---------------------------------------------------------------------------

curl() {
  local url="" out="" want_code=0 arg page body author merger
  while [ "$#" -gt 0 ]; do
    arg="$1"
    shift
    case "$arg" in
      -o)
        out="${1:-}"
        shift
        ;;
      -w)
        want_code=1
        shift
        ;;
      -H|-A|-X)
        shift
        ;;
      http://*|https://*)
        url="$arg"
        ;;
    esac
  done

  [ -n "${MOCK_CALL_LOG:-}" ] && printf '%s\n' "$url" >> "$MOCK_CALL_LOG"
  body='{}'
  case "$url" in
    *"/pulls/42/files?"*)
      page="${url#*page=}"
      page="${page%%&*}"
      case "${MOCK_SCENARIO:-}" in
        reserved_server_cap)
          case "$page" in
            1) body=$(jq -nc '[range(1;31) | {filename:("src/noise-" + (.|tostring) + ".txt")} ]') ;;
            2) body='[{"filename":"docs/design/new-rfc.md"}]' ;;
            *) body='[]' ;;
          esac
          ;;
        reserved_malformed) body='{"unexpected":"object"}' ;;
        reserved_control_filename) body='[{"filename":"docs/design/evil\nsecond"}]' ;;
        *) body='[]' ;;
      esac
      ;;
    *"/pulls/42")
      author="pr-author"
      merger="release-manager"
      if [ "${MOCK_SCENARIO:-}" = "reserved_server_cap" ] || \
         [ "${MOCK_SCENARIO:-}" = "reserved_malformed" ] || \
         [ "${MOCK_SCENARIO:-}" = "reserved_control_filename" ]; then
        author="release-manager"
      fi
      body=$(jq -nc \
        --arg author "$author" \
        --arg merger "$merger" \
        --arg head "$HEAD_SHA" \
        '{merged:true, merge_commit_sha:"merge-sha", merged_by:{login:$merger},
          user:{login:$author}, title:"pagination regression", base:{ref:"main"},
          head:{sha:$head}}')
      ;;
    *"/commits/${HEAD_SHA}/statuses?"*)
      page="${url#*page=}"
      page="${page%%&*}"
      case "${MOCK_SCENARIO:-}" in
        server_cap)
          case "$page" in
            1) body=$(jq -nc '[range(1;51) | {id:., context:("noise-" + (.|tostring)), status:"success"}]') ;;
            2) body='[{"id":51,"context":"CI / all-required (push)","status":"success"}]' ;;
            *) body='[]' ;;
          esac
          ;;
        explicit_empty)
          case "$page" in
            1) body='[{"id":1,"context":"CI / all-required (push)","status":"success"}]' ;;
            *) body='[]' ;;
          esac
          ;;
        duplicate_id)
          case "$page" in
            1) body=$(jq -nc '[{id:1, context:"CI / all-required (push)", status:"success"}] + [range(2;51) | {id:., context:("noise-" + (.|tostring)), status:"success"}]') ;;
            2) body='[{"id":50,"context":"late-duplicate","status":"success"}]' ;;
            *) body='[]' ;;
          esac
          ;;
        malformed_row)
          body='[{"id":"not-an-integer","context":"CI / all-required (push)","status":"success"}]'
          ;;
        non_monotonic)
          case "$page" in
            1) body='[{"id":1,"context":"CI / all-required (push)","status":"success"},{"id":3,"context":"noise","status":"success"}]' ;;
            2) body='[{"id":2,"context":"late-old-row","status":"success"}]' ;;
            *) body='[]' ;;
          esac
          ;;
        page_bound)
          if [ "$page" -le 201 ]; then
            if [ "$page" -eq 1 ]; then
              body='[{"id":1,"context":"CI / all-required (push)","status":"success"}]'
            else
              body=$(jq -nc --argjson id "$page" '[{id:$id, context:("noise-" + ($id|tostring)), status:"success"}]')
            fi
          else
            body='[]'
          fi
          ;;
        reserved_server_cap|reserved_malformed|reserved_control_filename)
          case "$page" in
            1) body='[{"id":1,"context":"CI / all-required (push)","status":"success"}]' ;;
            *) body='[]' ;;
          esac
          ;;
        *) body='[]' ;;
      esac
      ;;
  esac

  [ -n "$out" ] && printf '%s' "$body" > "$out"
  [ "$want_code" -eq 1 ] && printf '200'
}
export -f curl
export HEAD_SHA

run_audit() {
  local scenario="$1"
  AUDIT_OUT_FILE="$WORK/${scenario}.out"
  AUDIT_CALL_LOG="$WORK/${scenario}.calls"
  : > "$AUDIT_CALL_LOG"
  set +e
  MOCK_SCENARIO="$scenario" \
  MOCK_CALL_LOG="$AUDIT_CALL_LOG" \
  GITEA_TOKEN="test-token" \
  GITEA_HOST="git.moleculesai.app" \
  REPO="molecule-ai/molecule-core" \
  PR_NUMBER="42" \
  REQUIRED_CHECKS_JSON='{"main":["CI / all-required (push)"]}' \
  RESERVED_PATHS_FILE="$(cd "$THIS_DIR/../../.." && pwd)/.gitea/reserved-paths.txt" \
    bash "$AUDIT_SCRIPT" > "$AUDIT_OUT_FILE" 2>&1
  AUDIT_RC=$?
  set -e
  AUDIT_OUT=$(cat "$AUDIT_OUT_FILE")
}

# T24 — live defect: request limit=100, server returns 50 rows, and the needed
# status is on page 2. The audit must read page 2 AND the empty page 3, then
# report all checks green (never incident.force_merge).
run_audit server_cap
[ "$AUDIT_RC" = "0" ] || fail "T24: capped-page audit should succeed, rc=$AUDIT_RC output=$AUDIT_OUT"
[[ "$AUDIT_OUT" != *'incident.force_merge'* ]] || fail "T24: capped page produced false incident.force_merge"
[[ "$AUDIT_OUT" == *'all required checks green'* ]] || fail "T24: page-2 required success was not found"
grep -qF "statuses?page=3&limit=100" "$AUDIT_CALL_LOG" || fail "T24: audit did not prove exhaustion with empty page 3"
pass "T24: server-capped page walks through page-2 success to empty page 3"

# T25 — even a one-row first page requires a second, empty request. A short
# non-empty page is never an exhaustion proof.
run_audit explicit_empty
[ "$AUDIT_RC" = "0" ] || fail "T25: explicit-empty audit should succeed, rc=$AUDIT_RC output=$AUDIT_OUT"
grep -qF "statuses?page=2&limit=100" "$AUDIT_CALL_LOG" || fail "T25: one-row page incorrectly terminated pagination"
pass "T25: only an explicit empty page terminates status pagination"

# T26 — a duplicate id across page boundaries means the snapshot is ambiguous
# (usually an unstable ordering/boundary). Fail closed instead of evaluating a
# partial or duplicated history.
run_audit duplicate_id
[ "$AUDIT_RC" = "1" ] || fail "T26: duplicate status id should fail closed, rc=$AUDIT_RC output=$AUDIT_OUT"
[[ "$AUDIT_OUT" == *'duplicate status id'* ]] || fail "T26: duplicate-id failure should be explicit"
pass "T26: duplicate status id across pages fails closed"

# T27 — malformed rows cannot safely participate in max-by-id selection.
run_audit malformed_row
[ "$AUDIT_RC" = "1" ] || fail "T27: malformed status row should fail closed, rc=$AUDIT_RC output=$AUDIT_OUT"
[[ "$AUDIT_OUT" == *'malformed status row'* ]] || fail "T27: malformed-row failure should be explicit"
pass "T27: malformed status row fails closed"

# T28 — explicit stable id ordering is part of the pagination request; default
# ordering is not a safe cross-page contract.
run_audit explicit_empty
grep -qF 'sort=highestindex' "$AUDIT_CALL_LOG" || fail "T28: status pagination did not request stable id order"
pass "T28: status pagination requests stable highest-index ordering"

# T29 — an out-of-order id despite the explicit sort means the page snapshot
# is not trustworthy and must fail closed.
run_audit non_monotonic
[ "$AUDIT_RC" = "1" ] || fail "T29: non-monotonic status ids should fail closed, rc=$AUDIT_RC output=$AUDIT_OUT"
[[ "$AUDIT_OUT" == *'not strictly increasing by id'* ]] || fail "T29: non-monotonic-id failure should be explicit"
pass "T29: non-monotonic status ids fail closed"

# T30 — the same short-page bug existed in the reserved-files pagination loop.
# A reserved path on page 2 must still trigger the detective self-merge event,
# and page 3 must be read to prove file-list exhaustion.
run_audit reserved_server_cap
[ "$AUDIT_RC" = "0" ] || fail "T30: reserved-path audit should succeed, rc=$AUDIT_RC output=$AUDIT_OUT"
[[ "$AUDIT_OUT" == *'incident.reserved_self_merge'* ]] || fail "T30: reserved path on capped page 2 was missed"
grep -qF 'files?limit=50&page=3' "$AUDIT_CALL_LOG" || fail "T30: files pagination did not terminate on explicit empty page"
pass "T30: reserved-files pagination survives server cap and terminates on empty page"

# T31 — reserved-path detection is intentionally best-effort, but malformed
# file pages must be explicit and must not abort the independent force-merge
# status audit.
run_audit reserved_malformed
[ "$AUDIT_RC" = "0" ] || fail "T31: malformed reserved-files page must not abort force-merge audit, rc=$AUDIT_RC output=$AUDIT_OUT"
[[ "$AUDIT_OUT" == *'returned a malformed page'* ]] || fail "T31: malformed reserved-files page should emit a warning"
[[ "$AUDIT_OUT" == *'all required checks green'* ]] || fail "T31: status audit did not continue after malformed reserved-files page"
pass "T31: malformed reserved-files page warns while status audit continues"

# T32 — an endpoint that never reaches empty within the safety bound must fail
# closed instead of occupying the audit runner indefinitely. The fake becomes
# empty only after page 201, just beyond the production bound.
run_audit page_bound
[ "$AUDIT_RC" = "1" ] || fail "T32: excessive non-empty status pages should fail closed, rc=$AUDIT_RC output=$AUDIT_OUT"
[[ "$AUDIT_OUT" == *'exceeded 200 non-empty pages'* ]] || fail "T32: page-bound failure should be explicit"
pass "T32: excessive non-empty status pages fail closed"

# T33 — filenames are consumed through a newline-delimited jq stream and the
# matcher emits tab-delimited evidence. Reject control characters at the API
# boundary so one filename cannot be split into synthetic paths/evidence.
run_audit reserved_control_filename
[ "$AUDIT_RC" = "0" ] || fail "T33: malformed reserved filename must not abort force-merge audit, rc=$AUDIT_RC output=$AUDIT_OUT"
[[ "$AUDIT_OUT" == *'returned a malformed page'* ]] || fail "T33: control-character filename should mark the page malformed"
[[ "$AUDIT_OUT" != *'incident.reserved_self_merge'* ]] || fail "T33: control-character filename must not create synthetic reserved-path evidence"
[[ "$AUDIT_OUT" == *'all required checks green'* ]] || fail "T33: status audit did not continue after malformed filename"
pass "T33: control-character reserved filename is rejected without aborting status audit"

echo
echo "ALL AUDIT-FORCE-MERGE CHECKS PASSED"
