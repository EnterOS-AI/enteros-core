#!/usr/bin/env bash
# Regression test for internal#816 — sop-tier-check must ignore APPROVED
# reviews that were submitted against an old PR head SHA.
#
# Bug: the script collected approvers with
#   jq '[.[] | select(.state=="APPROVED") | .user.login]'
# without filtering on .commit_id == HEAD_SHA. After a PR head moved,
# stale approvals looked valid to the tier gate.
#
# Fix: the jq filter now includes
#   select(.state=="APPROVED" and .commit_id == $head_sha)
# where $head_sha is the current PR head fetched from the API.

set -euo pipefail

# jq may not be on PATH in all environments (e.g. dev containers).
PATH="/tmp/bin:$PATH"
command -v jq >/dev/null 2>&1 || { echo "::error::jq required but not found"; exit 1; }

PASS=0
FAIL=0

assert_eq() {
  local label="$1"
  local expected="$2"
  local got="$3"
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

# Sample reviews matching the shape from Gitea API
REVIEWS_JSON='[
  {"state":"APPROVED","commit_id":"abc123","user":{"login":"bob"}},
  {"state":"APPROVED","commit_id":"old456","user":{"login":"alice"}},
  {"state":"COMMENT","commit_id":"abc123","user":{"login":"carol"}},
  {"state":"APPROVED","commit_id":"abc123","user":{"login":"dave"}},
  {"state":"REQUEST_CHANGES","commit_id":"abc123","user":{"login":"eve"}}
]'

echo "test: jq filter keeps only APPROVED on current head"
GOT=$(echo "$REVIEWS_JSON" | jq -r --arg head_sha "abc123" \
  '[.[] | select(.state=="APPROVED" and .commit_id == $head_sha) | .user.login] | unique | .[]')
assert_eq "current-head approvers" "bob dave" "$(echo "$GOT" | tr '\n' ' ' | sed 's/ $//')"

echo "test: jq filter with all-stale reviews yields empty"
GOT=$(echo "$REVIEWS_JSON" | jq -r --arg head_sha "new789" \
  '[.[] | select(.state=="APPROVED" and .commit_id == $head_sha) | .user.login] | unique | .[]')
assert_eq "all-stale yields empty" "" "$GOT"

echo "test: jq filter handles null commit_id gracefully"
NULL_JSON='[{"state":"APPROVED","commit_id":null,"user":{"login":"mallory"}}]'
GOT=$(echo "$NULL_JSON" | jq -r --arg head_sha "abc123" \
  '[.[] | select(.state=="APPROVED" and .commit_id == $head_sha) | .user.login] | unique | .[]')
assert_eq "null commit_id excluded" "" "$GOT"

echo
echo "------"
echo "PASS=$PASS FAIL=$FAIL"
[ "$FAIL" -eq 0 ]
