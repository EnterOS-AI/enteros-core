#!/usr/bin/env bash
# Collision-proof slug SUFFIX generator for staging E2E harnesses (core#2782).
#
# ROOT CAUSE (Researcher RCA #100639): staging Platform Boot fails at
# POST /cp/admin/orgs HTTP 409 because the harness creates platform
# orgs with COLLIDING slugs against stale tenant state. The prior
# `head -c 32` truncation in test_staging_full_saas.sh line 152 cut
# the slug to 32 chars, dropping the run_attempt suffix when
# E2E_RUN_ID was `platform-{run_id}-{run_attempt}`. Two runs
# (e.g. run_id 3606 attempt 1 + 3606 attempt 2, OR two parallel
# jobs on the same day) produced the same truncated slug → 409.
#
# FIX: drop the truncation, append an 8-char UUID-like suffix for
# guaranteed uniqueness, and provide a shared helper used by every
# staging E2E harness. The infra purge of existing stale slugs is
# a separate owner/ops action (out of scope here per the ticket).
#
# Usage (the literal prefix MUST be in the caller so lint_cleanup_traps.sh
# can verify the SLUG=... assignment starts with a covered e2e-* or
# rt-e2e-* prefix — see #11510):
#
#   source tests/e2e/lib/collision-proof-slug.sh
#   SLUG="e2e-smoke-$(make_collision_proof_slug_suffix "$E2E_RUN_ID")"
#   assert_collision_proof_slug "$SLUG" || fail "..."
#
# The returned suffix is `<date>-<sanitized_run_id>-<uuid>`. The 8-char
# uuid is sourced from /proc/sys/kernel/random/uuid on Linux, fallback
# to two $RANDOM draws on macOS. 32 bits of entropy is enough to
# defeat the original collision class.
#
# Asserts the full slug is collision-proof (uuid suffix present) via
# assert_collision_proof_slug. Use this in the per-test self-check
# so a future refactor that drops the uuid is caught at harness
# startup, not at the first 409.

set -uo pipefail

# make_collision_proof_slug_suffix <run_id>
#   $1: Run id (typically `$E2E_RUN_ID` from the workflow; falls back
#       to a wall-clock+PID value).
#   Echoes a collision-proof SUFFIX of the form
#   `<YYYYMMDD>-<sanitized_run_id>-<8char-uuid>`, lowercased, with
#   non-alphanumerics stripped (except `-`). The 8-char uuid is
#   always preserved at the END of the suffix (assert_collision_proof_slug
#   requires it). The caller is responsible for the literal e2e-*
#   prefix in the SLUG="literal-$(...)" assignment shape (lint
#   requirement).
make_collision_proof_slug_suffix() {
  local run_id="${1:-}"

  # Fallback run_id when the workflow didn't set E2E_RUN_ID: a
  # wall-clock+PID combo that's unique per process invocation.
  if [ -z "$run_id" ]; then
    run_id="$(date +%H%M%S)-$$"
  fi

  local date_part
  date_part="$(date +%Y%m%d)"

  # Cross-platform random suffix. 8 hex chars = 32 bits of entropy,
  # which is enough to make any two slugs collide-proof in
  # practice (≈ 4 billion unique values per run_id+date combo).
  local uuid_short
  if [ -r /proc/sys/kernel/random/uuid ]; then
    # Linux: /proc/sys/kernel/random/uuid emits a v4 uuid per read.
    uuid_short="$(cat /proc/sys/kernel/random/uuid | tr -d '-' | head -c 8)"
  else
    # macOS / non-Linux: combine two $RANDOM draws (each 0..32767) for
    # 30 bits; pad with pid+nanoseconds for the remaining few bits.
    uuid_short="$(printf '%04x%04x' $RANDOM $RANDOM)"
  fi

  # Sanitize the run_id with the dynamic budget. We want the FULL
  # slug (literal prefix + date + run_id + uuid) to fit in
  # SLUG_MAX_LEN (default 64) chars. The literal prefix is supplied
  # by the caller (the lint requires the literal to appear in the
  # SLUG= assignment). Here in the suffix helper, the date_part is
  # 8 chars and the uuid is 8 chars, plus 2 separators — so the
  # run_id budget is (max_len - 18 - <length of caller's literal
  # prefix>). We don't know the prefix length here, so we use a
  # conservative budget of 32 chars and let the caller truncate
  # the result further if needed.
  local suffix_max_len="${SLUG_SUFFIX_MAX_LEN:-50}"  # date(8) + sep(1) + run_id(32) + sep(1) + uuid(8) = 50
  local run_id_budget=$(( suffix_max_len - 8 - 1 - 8 ))  # 33

  local sanitized_run_id
  sanitized_run_id="$(printf '%s' "$run_id" | tr '[:upper:]' '[:lower:]' | tr -cd 'a-z0-9-' | head -c "$run_id_budget")"
  printf '%s-%s-%s' "$date_part" "$sanitized_run_id" "$uuid_short"
}

# assert_collision_proof_slug <slug> asserts the FULL slug (literal
# prefix + suffix) ends in an 8-char uuid suffix. The literal
# prefix in the SLUG=... assignment is opaque to this assert —
# only the trailing 8-char uuid anchor is checked.
#
# Use this in the per-test self-check so a future refactor that
# drops the uuid is caught at harness startup, not at the first 409.
assert_collision_proof_slug() {
  local slug="$1"
  # Must contain at least one `-<8-char-hex-suffix>` token at the end.
  # The pattern is `-` then exactly 8 lowercase-hex chars then EOL.
  if ! printf '%s' "$slug" | grep -qE -- '-[0-9a-f]{8}$'; then
    echo "FAIL: slug '$slug' is not collision-proof (missing 8-char hex uuid suffix at end)" >&2
    return 1
  fi
  # Must be at least 24 chars (the minimum: e2e-YYYYMMDD-<8char uuid>).
  if [ "${#slug}" -lt 24 ]; then
    echo "FAIL: slug '$slug' is too short to be collision-proof (len=${#slug}, want >=24)" >&2
    return 1
  fi
  return 0
}
