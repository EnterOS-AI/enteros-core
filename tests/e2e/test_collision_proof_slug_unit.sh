#!/usr/bin/env bash
# Unit tests for tests/e2e/lib/collision-proof-slug.sh (core#2782).
#
# Verifies:
#   1. make_collision_proof_slug produces a slug with a 8-char hex
#      uuid suffix at the end (the collision-proof bit).
#   2. Two invocations of make_collision_proof_slug with the SAME
#      E2E_RUN_ID produce DIFFERENT slugs (the random suffix makes
#      them collision-proof even when run_id is reused).
#   3. assert_collision_proof_slug accepts a well-formed slug and
#      rejects a malformed one (e.g. no uuid suffix).
#   4. The prefix is preserved through the lowercasing + strip
#      transform (a "e2e-smoke" prefix still shows up as "e2e-smoke").
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

# Test 1: well-formed slug has a uuid suffix.
test_slug_shape() {
  local s
  s=$(make_collision_proof_slug "e2e-smoke" "platform-3606-1")
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
  s1=$(make_collision_proof_slug "e2e-smoke" "platform-3606-1")
  s2=$(make_collision_proof_slug "e2e-smoke" "platform-3606-1")
  s3=$(make_collision_proof_slug "e2e-smoke" "platform-3606-1")
  if [ "$s1" = "$s2" ] || [ "$s2" = "$s3" ] || [ "$s1" = "$s3" ]; then
    echo "FAIL: test_same_run_id_different_slugs — same run_id produced identical slugs (collision possible): '$s1' == '$s2' == '$s3'"
    return 1
  fi
  echo "PASS: test_same_run_id_different_slugs (3 distinct slugs from same run_id)"
  return 0
}

# Test 3: prefix is preserved through transform.
test_prefix_preserved() {
  local s
  s=$(make_collision_proof_slug "e2e-rec" "1234-1")
  if ! printf '%s' "$s" | grep -q "^e2e-rec-"; then
    echo "FAIL: test_prefix_preserved — prefix 'e2e-rec-' not preserved in slug '$s'"
    return 1
  fi
  echo "PASS: test_prefix_preserved (slug=$s)"
  return 0
}

# Test 4: assert_collision_proof_slug rejects a malformed slug (no uuid).
test_assert_rejects_malformed() {
  # "e2e-smoke-20260613-platform-3606" — the OLD shape (no uuid
  # suffix, just 32-char truncated). assert must REJECT (return
  # non-zero) — the test passes if the assert correctly rejects.
  if assert_collision_proof_slug "e2e-smoke-20260613-platform-3606"; then
    echo "FAIL: test_assert_rejects_malformed — accepted a 32-char slug without the 8-char uuid suffix"
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
  s=$(make_collision_proof_slug "e2e-smoke" "")
  if ! assert_collision_proof_slug "$s"; then
    echo "FAIL: test_fallback_run_id — empty run_id produced non-collision-proof slug '$s'"
    return 1
  fi
  echo "PASS: test_fallback_run_id (slug=$s)"
  return 0
}

# Test 7: large-run-id still produces a usable slug (the 64-char
# cap may truncate, but the uuid suffix must remain).
test_large_run_id_uuid_preserved() {
  # 50-char run_id + prefix + date + uuid = ~80 chars before cap.
  local s
  s=$(make_collision_proof_slug "e2e" "abcdefghijklmnopqrstuvwxyz0123456789abcdefghijklmnop-1")
  if ! assert_collision_proof_slug "$s"; then
    echo "FAIL: test_large_run_id_uuid_preserved — uuid suffix not preserved on truncated slug '$s'"
    return 1
  fi
  echo "PASS: test_large_run_id_uuid_preserved (slug=$s, len=${#s})"
  return 0
}

test_slug_shape || failed=$((failed+1))
test_same_run_id_different_slugs || failed=$((failed+1))
test_prefix_preserved || failed=$((failed+1))
test_assert_rejects_malformed || failed=$((failed+1))
test_assert_rejects_too_short || failed=$((failed+1))
test_fallback_run_id || failed=$((failed+1))
test_large_run_id_uuid_preserved || failed=$((failed+1))

if [ "$failed" -gt 0 ]; then
  echo "FAILED: $failed test(s)"
  exit 1
fi
echo "All collision-proof-slug unit tests passed"
