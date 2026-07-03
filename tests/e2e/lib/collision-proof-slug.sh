#!/usr/bin/env bash
# Collision-proof slug SUFFIX generator for staging E2E harnesses (core#2782).
#
# ROOT CAUSE (Researcher RCA #100639): staging Platform Boot fails at
# POST /cp/admin/orgs HTTP 409 because the harness creates platform
# orgs with COLLIDING slugs against stale tenant state. The prior
# head -c 32 truncation in test_staging_full_saas.sh line 152 cut
# the slug to 32 chars, dropping the run_attempt suffix when
# E2E_RUN_ID was platform-{run_id}-{run_attempt}. Two runs
# (e.g. run_id 3606 attempt 1 + 3606 attempt 2, OR two parallel
# jobs on the same day) produced the same truncated slug, hence 409.
#
# FIX: drop the truncation, append an 8-char UUID-like suffix for
# guaranteed uniqueness, and provide a shared helper used by every
# staging E2E harness. The infra purge of existing stale slugs is
# a separate owner/ops action (out of scope here per the ticket).
#
# Usage: the literal prefix MUST be in the caller so
# lint_cleanup_traps.sh can verify the SLUG= assignment starts with
# a covered e2e-* or rt-e2e-* prefix (see #11510).
#
#   source tests/e2e/lib/collision-proof-slug.sh
#   SLUG="e2e-smoke-$(make_collision_proof_slug_suffix "$E2E_RUN_ID" 11)"
#   assert_collision_proof_slug "$SLUG" || fail "..."
#
# The returned suffix is <date>-<sanitized_run_id>-<uuid>. The 8-char
# uuid is sourced from /proc/sys/kernel/random/uuid on Linux, fallback
# to two RANDOM draws on macOS. 32 bits of entropy is enough to
# defeat the original collision class.
#
# Asserts the full slug is collision-proof via assert_collision_proof_slug.
# Use this in the per-test self-check so a future refactor that
# drops the uuid is caught at harness startup, not at the first 409.
#
# core#60: the FULL slug must also fit in CP_ORG_SLUG_MAX_LEN (31 chars,
# the CP's org-slug cap; the org-create endpoint rejects longer slugs
# with HTTP 400, which under CURL_COMMON's --fail-with-body + set -e
# aborts the harness before the body-logging line can run). The
# helper truncates the run_id segment (NOT the uuid anchor) so the
# collision-proof guarantee is preserved.

set -uo pipefail

# CP_ORG_SLUG_MAX_LEN is the CP's org-slug character cap (regex
# ^[a-z][a-z0-9-]{2,31}$: a leading char plus 2-31 additional =
# 32-char absolute max). The org-create endpoint rejects
# longer slugs with HTTP 400 in practice per the staging 400s
# in run 363934, core#60. The #65 e2e-peer-visibility lane
# hit a 33-char slug like `e2e-pv-20260614-364043-2-e560b630`
# that needed this exact cap to keep the prefix+uuid+date
# layout below the regex's 32-char ceiling.
: "${CP_ORG_SLUG_MAX_LEN:=32}"

# make_collision_proof_slug_suffix <run_id> [prefix_len]
#   1: Run id (typically E2E_RUN_ID from the workflow; falls back
#      to a wall-clock+PID value when empty).
#   2: Optional length of the caller's literal prefix in the
#      SLUG=... assignment. When supplied, the context budget is
#      computed precisely (CP_ORG_SLUG_MAX_LEN - prefix_len - 9,
#      where 9 = 8 uuid + 1 anchor separator). When omitted, the
#      helper uses a conservative default of 11 (the "e2e-smoke-"
#      prefix length).
#
#   Echoes a collision-proof SUFFIX of the form
#   <date>-<sanitized_run_id>-<uuid>, lowercased, with non-
#   alphanumerics stripped (except -). The 8-char uuid is ALWAYS
#   emitted at the END of the suffix; the leading <date>-<run_id>
#   context is truncated — or dropped WHOLESALE — to fit
#   CP_ORG_SLUG_MAX_LEN. When even the date does not fit after a
#   long caller prefix (e.g. `cp455-claude-code-`, 18 chars), the
#   suffix degrades to the bare 8-char uuid rather than aborting
#   with an empty suffix — so the slug is ALWAYS collision-proof.
make_collision_proof_slug_suffix() {
  local run_id="${1:-}"
  local prefix_len="${2:-11}"

  if [ -z "$run_id" ]; then
    run_id="$(date +%H%M%S)-$$"
  fi

  local date_part
  date_part="$(date +%Y%m%d)"

  local uuid_short
  if [ -r /proc/sys/kernel/random/uuid ]; then
    uuid_short="$(tr -d '-' < /proc/sys/kernel/random/uuid | head -c 8)"
  else
    uuid_short="$(printf '%04x%04x' "$RANDOM" "$RANDOM")"
  fi
  # Defensive: the 8-char uuid is the SOLE collision-proof anchor and is
  # asserted downstream via `-[0-9a-f]{8}$`. If the entropy source ever
  # misbehaves (short/empty read, SIGPIPE truncation, non-hex), fall back
  # to $RANDOM so we NEVER emit a short/empty anchor — that was the
  # class of bug that produced the empty suffix + `...-` slug at
  # test_minimal_boot_cell.sh (core boot-to-registration aborted before
  # provisioning).
  if ! printf '%s' "$uuid_short" | grep -qE '^[0-9a-f]{8}$'; then
    uuid_short="$(printf '%04x%04x' "$RANDOM" "$RANDOM")"
  fi

  # The 8-char uuid anchor is NON-NEGOTIABLE and is ALWAYS the tail of the
  # suffix — dropping it reintroduces the core#2782 collision class (and,
  # via the early `return 1` this replaces, produced an EMPTY suffix that
  # yielded the collision-UNsafe slug `<prefix>-`). The optional
  # <date>-<run_id> context in FRONT of the uuid is truncated — or dropped
  # entirely — so the FULL slug (caller prefix + suffix) never exceeds
  # CP_ORG_SLUG_MAX_LEN. The context budget is the cap minus the caller's
  # prefix, minus the uuid anchor (8) and its leading separator (1).
  #
  #   full = prefix_len + [ len(context) + 1 ] + 8   (context present)
  #   full = prefix_len + 8                           (context dropped; the
  #                                                    caller prefix's own
  #                                                    trailing '-' supplies
  #                                                    the anchor separator)
  local context_budget=$(( CP_ORG_SLUG_MAX_LEN - prefix_len - 8 - 1 ))

  local context=""
  if [ "$context_budget" -ge "${#date_part}" ]; then
    # Room for the whole date; keep it, then append as much sanitized
    # run_id as still fits (its own '-' separator included). We prefer
    # whole segments — a partial-date fragment adds no value since the
    # uuid alone is the collision guarantee.
    context="$date_part"
    local run_id_budget=$(( context_budget - ${#date_part} - 1 ))
    if [ "$run_id_budget" -ge 1 ]; then
      local sanitized_run_id
      sanitized_run_id="$(printf '%s' "$run_id" | tr '[:upper:]' '[:lower:]' | tr -cd 'a-z0-9-' | head -c "$run_id_budget")"
      # Strip any trailing '-' left by truncation so we never emit '--'.
      sanitized_run_id="$(printf '%s' "$sanitized_run_id" | sed 's/-*$//')"
      if [ -n "$sanitized_run_id" ]; then
        context="${context}-${sanitized_run_id}"
      fi
    fi
  fi

  if [ -n "$context" ]; then
    printf '%s-%s' "$context" "$uuid_short"
  else
    # Prefix too long for any <date>-<run_id> context — emit the bare uuid
    # anchor. The caller's literal prefix already ends in '-', so the full
    # slug still matches the assert's `-<8hex>$` anchor and stays <= cap.
    printf '%s' "$uuid_short"
  fi
}

# assert_collision_proof_slug <slug> asserts the FULL slug ends in
# an 8-char uuid suffix AND fits in CP_ORG_SLUG_MAX_LEN. Use this
# in the per-test self-check so a future refactor that drops the
# uuid OR exceeds the CP cap is caught at harness startup, not at
# the first 400/409.
assert_collision_proof_slug() {
  local slug="$1"
  if ! printf '%s' "$slug" | grep -qE -- '-[0-9a-f]{8}$'; then
    echo "FAIL: slug '$slug' is not collision-proof, missing 8-char hex uuid suffix at end" >&2
    return 1
  fi
  if [ "${#slug}" -lt 24 ]; then
    echo "FAIL: slug '$slug' is too short to be collision-proof, len=${#slug} want >=24" >&2
    return 1
  fi
  if [ "${#slug}" -gt "${CP_ORG_SLUG_MAX_LEN}" ]; then
    echo "FAIL: slug '$slug' is too long, len=${#slug} max=${CP_ORG_SLUG_MAX_LEN}, CP /cp/admin/orgs rejects with HTTP 400" >&2
    return 1
  fi
  return 0
}
