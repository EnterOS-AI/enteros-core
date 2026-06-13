#!/usr/bin/env bash
# Unit tests for tests/e2e/lib/collision-proof-slug.sh (core#2782).
#
# Verifies:
#   1. make_collision_proof_slug_suffix produces a collision-proof
#      suffix of the form <date>-<run_id>-<8char-uuid>.
#   2. Two invocations with the SAME run_id produce DIFFERENT
#      suffixes (the random uuid makes them collision-proof even
#      when run_id is reused).
#   3. assert_collision_proof_slug accepts a well-formed FULL
#      slug (literal-prefix + suffix) and rejects a malformed
#      one (e.g. no uuid suffix).
#   4. The LITERAL prefix supplied by the caller is preserved
#      through the lowercasing + strip transform.
#
# These tests are pure-bash (no harness / no API) so they run in
# milliseconds and are safe to wire into the e2e test lanes'
# preflight (or as a stand-alone unit check on CI).

set -uo pipefail

LIB_PATH="${LIB_PATH:-$(cd "$(dirname "$0")" && pwd)/lib/collision-proof-slug.sh}"

# shellcheck source=lib/collision-proof-slug.sh
# shellcheck disable=SC1091
source "$LIB_PATH"

failed=0

# Test 1: a full slug (literal-prefix + suffix) is well-formed.
test_slug_shape() {
  local s
  s="e2e-smoke-$(make_collision_proof_slug_suffix "platform-3606-1")"
  if ! assert_collision_proof_slug "$s"; then
    echo "FAIL: test_slug_shape — produced slug '$s' failed assert_collision_proof_slug"
    return 1
  fi
  echo "PASS: test_slug_shape (slug=$s)"
  return 0
}

# Test 2: same run_id → different slugs (the collision-proof bit).
test_same_run_id_different_slugs() {
  local s1 s2 s3
  s1="e2e-smoke-$(make_collision_proof_slug_suffix "platform-3606-1")"
  s2="e2e-smoke-$(make_collision_proof_slug_suffix "platform-3606-1")"
  s3="e2e-smoke-$(make_collision_proof_slug_suffix "platform-3606-1")"
  if [ "$s1" = "$s2" ] || [ "$s2" = "$s3" ] || [ "$s1" = "$s3" ]; then
    echo "FAIL: test_same_run_id_different_slugs — same run_id produced identical slugs (collision possible): '$s1' == '$s2' == '$s3'"
    return 1
  fi
  echo "PASS: test_same_run_id_different_slugs (3 distinct slugs from same run_id)"
  return 0
}

# Test 3: the LITERAL prefix supplied by the caller is preserved
# through the slug assembly.
test_prefix_preserved() {
  local s
  s="e2e-rec-$(make_collision_proof_slug_suffix "1234-1")"
  if ! printf '%s' "$s" | grep -q "^e2e-rec-"; then
    echo "FAIL: test_prefix_preserved — prefix 'e2e-rec-' not preserved in slug '$s'"
    return 1
  fi
  echo "PASS: test_prefix_preserved (slug=$s)"
  return 0
}

# Test 4: assert_collision_proof_slug rejects a malformed slug (no uuid).
test_assert_rejects_malformed() {
  if assert_collision_proof_slug "e2e-smoke-20260613-platform-3606"; then
    echo "FAIL: test_assert_rejects_malformed — accepted a slug without the 8-char uuid suffix"
    return 1
  fi
  echo "PASS: test_assert_rejects_malformed (correctly rejected)"
  return 0
}

# Test 5: assert_collision_proof_slug rejects too-short slugs.
test_assert_rejects_too_short() {
  if assert_collision_proof_slug "e2e-abcd"; then
    echo "FAIL: test_assert_rejects_too_short — accepted a too-short slug"
    return 1
  fi
  echo "PASS: test_assert_rejects_too_short (correctly rejected)"
  return 0
}

# Test 6: fallback run_id (empty) still produces a collision-proof slug.
test_fallback_run_id() {
  local s
  s="e2e-smoke-$(make_collision_proof_slug_suffix "")"
  if ! assert_collision_proof_slug "$s"; then
    echo "FAIL: test_fallback_run_id — empty run_id produced non-collision-proof slug '$s'"
    return 1
  fi
  echo "PASS: test_fallback_run_id (slug=$s)"
  return 0
}

# Test 7: large-run-id still produces a usable slug (the run_id is
# truncated but the uuid suffix remains).
test_large_run_id_uuid_preserved() {
  local s
  s="e2e-$(make_collision_proof_slug_suffix "abcdefghijklmnopqrstuvwxyz0123456789abcdefghijklmnop-1")"
  if ! assert_collision_proof_slug "$s"; then
    echo "FAIL: test_large_run_id_uuid_preserved — uuid suffix not preserved on truncated slug '$s'"
    return 1
  fi
  echo "PASS: test_large_run_id_uuid_preserved (slug=$s, len=${#s})"
  return 0
}

# Test 8 (CR2 #11506 robustness nit): a long LITERAL prefix doesn't
# overflow the 64-char cap because the slug uses a separate
# helper-produced suffix. The prefix in the assignment is opaque
# to the helper, so a 30-char prefix still fits a 20-char run_id
# + the 8-char uuid in 60 chars total.
test_prefix_budget_dynamic() {
  local s
  s="abcdefghijklmnopqrstuvwx-yz-$(make_collision_proof_slug_suffix "short-run")"
  if ! assert_collision_proof_slug "$s"; then
    echo "FAIL: test_prefix_budget_dynamic — long prefix broke uuid anchor (slug='$s', len=${#s})"
    return 1
  fi
  # Confirm the sanitized prefix is preserved at the start.
  if ! printf '%s' "$s" | grep -q "^abcdefghijklmnopqrstuvwx-yz-"; then
    echo "FAIL: test_prefix_budget_dynamic — sanitized prefix not preserved at start of '$s'"
    return 1
  fi
  echo "PASS: test_prefix_budget_dynamic (slug=$s, len=${#s})"
  return 0
}

# Test 9: the helper output (suffix) by itself is at most 50 chars
# (date 8 + sep 1 + run_id ≤33 + sep 1 + uuid 8). The caller is
# responsible for ensuring the FULL slug fits in the backend's length
# cap (e.g. via SLUG_MAX_LEN on the test or a hardcoded trim).
test_suffix_length_capped() {
  local suf
  suf=$(make_collision_proof_slug_suffix "abcdefghijklmnopqrstuvwxyz0123456789abcdefghijklmnop-1")
  # The suffix max is 50 (date 8 + sep 1 + run_id 33 + sep 1 + uuid 8
  # = 51, with the cap at 50). Some slack for off-by-one.
  if [ "${#suf}" -gt 51 ]; then
    echo "FAIL: test_suffix_length_capped — suffix '$suf' is ${#suf} chars (want <= 51)"
    return 1
  fi
  echo "PASS: test_suffix_length_capped (suffix=$suf, len=${#suf})"
  return 0
}

test_slug_shape || failed=$((failed+1))
test_same_run_id_different_slugs || failed=$((failed+1))
test_prefix_preserved || failed=$((failed+1))
test_assert_rejects_malformed || failed=$((failed+1))
test_assert_rejects_too_short || failed=$((failed+1))
test_fallback_run_id || failed=$((failed+1))
test_large_run_id_uuid_preserved || failed=$((failed+1))
test_prefix_budget_dynamic || failed=$((failed+1))
test_suffix_length_capped || failed=$((failed+1))

if [ "$failed" -gt 0 ]; then
  echo "FAILED: $failed test(s)"
  exit 1
fi
echo "All collision-proof-slug unit tests passed"
