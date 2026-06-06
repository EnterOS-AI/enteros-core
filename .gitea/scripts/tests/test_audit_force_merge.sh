#!/usr/bin/env bash
# test_audit_force_merge.sh — regression lock for audit-force-merge fail-closed
# behavior. Verifies every schema validation path via direct jq filter tests.
#
# Usage: bash test_audit_force_merge.sh

set -euo pipefail

fail() { echo "FAIL: $*" >&2; exit 1; }
pass() { echo "PASS: $*"; }

[ -x "$(command -v jq)" ] || { echo "SKIP: jq not on PATH"; exit 0; }

HEAD_SHA="deadbeef00000000000000000000000000000000"

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

echo
echo "ALL AUDIT-FORCE-MERGE CHECKS PASSED"
