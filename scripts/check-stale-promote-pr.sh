#!/usr/bin/env bash
# scripts/check-stale-promote-pr.sh
#
# Scan open auto-promote PRs (base=main head=staging) for the
# silent-block failure mode that motivated issue #2975:
#   - PR sat for hours with mergeStateStatus=BLOCKED
#   - reviewDecision=REVIEW_REQUIRED (auto-merge armed but waiting
#     on a human approval that never comes)
#
# When found, emit:
#   - GitHub Actions notice/warning lines (workflow summary surface)
#   - Optionally post a comment on the PR (--comment)
#
# Exit code is the count of stale PRs found, capped at 125 so callers
# can detect "alarm fired" via `if ! check-stale-promote-pr.sh; then …`.
# Exit 0 = clean, exit ≥1 = at least N stale PRs need attention.
#
# Used by .github/workflows/auto-promote-stale-alarm.yml. Logic lives
# here (not inline in the workflow YAML) so we can:
#   - Unit-test it with a stubbed `gh` (see test-check-stale-promote-pr.sh)
#   - Run it ad-hoc by an operator: `scripts/check-stale-promote-pr.sh`
#   - Reuse the same surface in any sibling workflow that needs the same
#     check (SSOT — one detector, many callers).
#
# Requires: `gh` CLI, `jq`. `GH_TOKEN` env in the workflow context.

set -euo pipefail

# -----------------------------------------------------------------------------
# Inputs
# -----------------------------------------------------------------------------

# Threshold beyond which a BLOCKED+REVIEW_REQUIRED promote PR is "stale"
# enough to alarm. 4 hours is the floor: most legitimate gates clear
# inside an hour, so 4× headroom is plenty for slow CI without false-
# alarming. Override via env for tests + edge ops.
STALE_HOURS="${STALE_HOURS:-4}"

# Repo defaults to the current `gh` context. Tests pass --repo explicitly.
REPO="${GITHUB_REPOSITORY:-}"

# Whether to post a comment to the PR. Off by default to avoid noise on
# manual ad-hoc runs; the cron workflow turns it on.
POST_COMMENT="${POST_COMMENT:-false}"

# Where to read the open-PR JSON from. Empty = call `gh` live. Tests
# point this at a fixture file.
PR_FIXTURE="${PR_FIXTURE:-}"

# Where to read "now" from. Empty = real clock. Tests freeze time so
# the staleness math is deterministic.
NOW_OVERRIDE="${NOW_OVERRIDE:-}"

while [ $# -gt 0 ]; do
  case "$1" in
    --repo) REPO="$2"; shift 2 ;;
    --comment) POST_COMMENT="true"; shift ;;
    --no-comment) POST_COMMENT="false"; shift ;;
    --fixture) PR_FIXTURE="$2"; shift 2 ;;
    --stale-hours) STALE_HOURS="$2"; shift 2 ;;
    -h|--help)
      sed -n '1,/^set /p' "$0" | grep '^# ' | sed 's/^# //'
      exit 0
      ;;
    *) echo "unknown arg: $1" >&2; exit 64 ;;
  esac
done

if [ -z "$REPO" ] && [ -z "$PR_FIXTURE" ]; then
  echo "::error::REPO env (or GITHUB_REPOSITORY) required when no fixture given" >&2
  exit 2
fi

# -----------------------------------------------------------------------------
# Clock helpers — split out so tests can freeze time
# -----------------------------------------------------------------------------

now_epoch() {
  if [ -n "$NOW_OVERRIDE" ]; then
    printf '%s\n' "$NOW_OVERRIDE"
  else
    date -u +%s
  fi
}

# Parse RFC3339 timestamps the way GitHub emits them (e.g.
# "2026-05-05T23:15:00Z"). gnu-date uses -d, bsd-date uses -j -f. Cover
# both because the workflow runs on ubuntu-latest (gnu) but operators
# may run this script on macOS (bsd).
to_epoch() {
  local ts="$1"
  # gnu-date path first.
  if date -u -d "$ts" +%s 2>/dev/null; then
    return 0
  fi
  # bsd-date fallback — strip optional fractional seconds before %S.
  local ts_clean="${ts%%.*}"
  ts_clean="${ts_clean%Z}Z"
  date -u -j -f "%Y-%m-%dT%H:%M:%SZ" "$ts_clean" +%s 2>/dev/null || {
    echo "::error::cannot parse timestamp: $ts" >&2
    return 1
  }
}

# -----------------------------------------------------------------------------
# Fetch open auto-promote PRs
# -----------------------------------------------------------------------------

fetch_prs() {
  if [ -n "$PR_FIXTURE" ]; then
    cat "$PR_FIXTURE"
    return 0
  fi
  gh pr list --repo "$REPO" \
    --base main --head staging --state open \
    --json number,title,createdAt,mergeStateStatus,reviewDecision,url
}

# -----------------------------------------------------------------------------
# Stale detection
# -----------------------------------------------------------------------------

# Read PR list from stdin, emit one TSV line per stale PR:
#   <num>\t<age_hours>\t<url>\t<title>
# Caller decides what to do (warn, comment, escalate).
detect_stale() {
  local now_ts
  now_ts="$(now_epoch)"
  local stale_seconds=$((STALE_HOURS * 3600))

  jq -r '.[] | [.number, .createdAt, .mergeStateStatus, .reviewDecision, .url, .title] | @tsv' \
    | while IFS=$'\t' read -r num created_at merge_state review_decision url title; do
        # Only alarm on the specific failure mode: BLOCKED + REVIEW_REQUIRED.
        # Other BLOCKED reasons (DIRTY, BEHIND, failed checks) are the
        # author's signal-to-fix; this script targets the silent
        # "no human reviewed yet" wedge specifically.
        [ "$merge_state" = "BLOCKED" ] || continue
        [ "$review_decision" = "REVIEW_REQUIRED" ] || continue

        local created_ts
        created_ts="$(to_epoch "$created_at")" || continue
        local age=$((now_ts - created_ts))
        if [ "$age" -ge "$stale_seconds" ]; then
          local age_h=$((age / 3600))
          printf '%s\t%d\t%s\t%s\n' "$num" "$age_h" "$url" "$title"
        fi
      done
}

# -----------------------------------------------------------------------------
# Reporting
# -----------------------------------------------------------------------------

# Comment body — kept short; the issue body has the full design.
comment_body() {
  local age_h="$1"
  cat <<EOF
⚠️ This auto-promote PR has been BLOCKED on \`REVIEW_REQUIRED\` for **${age_h}h**.

Auto-merge is armed, but main's branch protection requires 1 review and no human has approved. Until someone reviews, the staging→main promote chain is wedged and downstream consumers (canvas builds, tenant redeploys) won't see new code.

**Action**: a human reviewer on \`@Molecule-AI/maintainers\` should approve this PR (or mark it as not ready and close).

Detected by \`scripts/check-stale-promote-pr.sh\` per issue #2975.
EOF
}

post_comment() {
  local pr_num="$1"
  local age_h="$2"
  if [ "$POST_COMMENT" != "true" ]; then
    return 0
  fi
  # Idempotency: only one alarm comment per PR. Look for the marker
  # string in existing comments before posting a new one.
  local existing
  existing="$(gh pr view "$pr_num" --repo "$REPO" --json comments \
    --jq '.comments[] | select(.body | test("scripts/check-stale-promote-pr.sh per issue #2975")) | .databaseId' \
    | head -n1)"
  if [ -n "$existing" ]; then
    echo "::notice::PR #$pr_num already has a stale-alarm comment ($existing) — not re-posting"
    return 0
  fi
  comment_body "$age_h" | gh pr comment "$pr_num" --repo "$REPO" --body-file -
  echo "::notice::Posted stale-alarm comment on PR #$pr_num (age=${age_h}h)"
}

# -----------------------------------------------------------------------------
# Main
# -----------------------------------------------------------------------------

stale_count=0
while IFS=$'\t' read -r num age_h url title; do
  [ -n "$num" ] || continue
  stale_count=$((stale_count + 1))
  echo "::warning title=Stale auto-promote PR::PR #$num — BLOCKED on REVIEW_REQUIRED for ${age_h}h. $url"
  {
    echo "## ⚠️ Stale auto-promote PR detected"
    echo
    echo "- PR: #$num — \`$title\`"
    echo "- Age: ${age_h}h"
    echo "- State: BLOCKED on REVIEW_REQUIRED"
    echo "- URL: $url"
    echo
    echo "Auto-merge is armed but waiting on a human review. See issue #2975."
  } >> "${GITHUB_STEP_SUMMARY:-/dev/null}"
  post_comment "$num" "$age_h"
done < <(fetch_prs | detect_stale)

if [ "$stale_count" -eq 0 ]; then
  echo "::notice::No stale auto-promote PRs detected (threshold: ${STALE_HOURS}h)"
fi

# Cap exit code so we don't accidentally break shells that interpret
# >125 as signal-style. 1..N maps to "1..N stale PRs".
exit $(( stale_count > 125 ? 125 : stale_count ))
