#!/usr/bin/env bash
# Collision-proof slug generator for staging E2E harnesses (core#2782).
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
# Usage:
#   source tests/e2e/lib/collision-proof-slug.sh
#   SLUG=$(make_collision_proof_slug "e2e-smoke" "$E2E_RUN_ID")
#
# Returns a slug of the form `<prefix>-YYYYMMDD-{RUN_ID}-{uuid}`.
# The run_id portion is itself truncated to 32 chars (leaving room
# for prefix + date + uuid) — the 8-char uuid suffix is ALWAYS
# preserved (the run_id is the part that's allowed to lose
# characters, the uuid never is). Length ceiling is ~62 chars
# (`e2e-smoke-20260613-` (19) + truncated run_id (32) + `-` (1) +
# `uuid` (8) = 60), well within typical backend limits.
#
# Asserts the slug is collision-proof (uuid suffix present) via
# assert_collision_proof_slug. Use this in the per-test self-check
# so a future refactor that drops the uuid is caught at harness
# startup, not at the first 409.

set -uo pipefail

# make_collision_proof_slug <prefix> <run_id>
#   $1: Slug prefix (e.g. "e2e-smoke", "e2e-rec", "e2e-mcp", "e2e").
#   $2: Run id (typically `$E2E_RUN_ID` from the workflow; falls back
#       to a wall-clock+PID value).
#   Echoes a slug of the form `<prefix>-YYYYMMDD-{run_id}-{8char-uuid}`,
#   lowercased, with non-alphanumerics stripped (except `-`). The 8-char
#   uuid suffix is sourced from /proc/sys/kernel/random/uuid on Linux
#   (deterministic fallback to `${RANDOM}${RANDOM}` on macOS) and
#   makes any two slugs collide-proof even when the run_id is reused
#   (e.g. retries with the same `github.run_id`).
make_collision_proof_slug() {
  local prefix="$1"
  local run_id="${2:-}"

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

  # Lowercase + strip non-alphanumerics except `-`. Apply the
  # truncation to the run_id ONLY (so the uuid is always preserved
  # at the end); the prefix + date + `-` + uuid anchor is 19 + 1 + 1 + 8
  # = 29 chars, leaving up to 33 chars for the (possibly truncated)
  # run_id. A pathological 100-char run_id loses characters from
  # the run_id portion — but the slug remains collision-proof via
  # the uuid, and the run_id is still useful for log correlation.
  local slug
  slug="$(printf '%s' "${prefix}-${date_part}-${run_id}-${uuid_short}" | tr '[:upper:]' '[:lower:]' | tr -cd 'a-z0-9-')"

  # If the run_id was so long that the full slug exceeds the 64-char
  # cap, truncate the run_id portion from the end (preserving the
  # 8-char uuid anchor). This keeps the uuid at the END (where
  # assert_collision_proof_slug looks for it) and the prefix at the
  # start (for log readability).
  if [ "${#slug}" -gt 64 ]; then
    local max_run_id_len=$(( 64 - 19 - 1 - 8 ))
    local truncated_run_id
    truncated_run_id="$(printf '%s' "$run_id" | tr '[:upper:]' '[:lower:]' | tr -cd 'a-z0-9-' | head -c "$max_run_id_len")"
    slug="${prefix}-${date_part}-${truncated_run_id}-${uuid_short}"
  fi

  printf '%s' "$slug"
}

# assert_collision_proof_slug <slug> is a unit test that asserts the
# slug includes both the run_id AND a 8-char uuid-like suffix. It
# exits 0 on a well-formed slug, 1 otherwise. Used in the per-test
# self-check (below) to fail loud at harness startup if a test
# regressed to the truncated shape.
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

