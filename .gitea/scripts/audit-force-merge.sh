#!/usr/bin/env bash
# audit-force-merge — detect a §SOP-6 force-merge after PR close, emit
# `incident.force_merge` to stdout as structured JSON.
#
# Vector's docker_logs source picks up runner stdout; the JSON gets
# shipped to Loki on molecule-canonical-obs, indexable by event_type.
# Query example:
#
#   {host="operator"} |= "event_type" |= "incident.force_merge" | json
#
# A force-merge is detected when a PR closed-with-merged=true had at
# least one of the repo's required-status-check contexts in a state
# other than "success" at the merge commit's SHA. That's exactly what
# the Gitea force_merge:true API call lets through, so it's a faithful
# detector of the override path.
#
# Triggers on `pull_request_target: closed` (loaded from base branch
# per §SOP-6 security model). No-op when merged=false.
#
# Required env (set by the workflow):
#   GITEA_TOKEN, GITEA_HOST, REPO, PR_NUMBER
#   plus one of REQUIRED_CHECKS_JSON (preferred) or REQUIRED_CHECKS (legacy)
#
# REQUIRED_CHECKS_JSON is a JSON object keyed by branch name. Each value
# is an array of status-check context names that branch protection
# requires for that branch. The script looks up the PR's base branch and
# evaluates only the checks declared for that branch.
#
#   {"main": ["CI / all-required (pull_request)", ...],
#    "staging": ["CI / all-required (pull_request)", ...]}
#
# REQUIRED_CHECKS (legacy) is a newline-separated list used when the
# JSON variable is not set. Declared in the workflow YAML rather than
# fetched from /branch_protections (which needs admin scope — 
# has read-only). Trade dynamism for simplicity: when the required-check
# set changes, update both branch protection AND this env. Keeping them
# in sync is less complexity than granting the audit bot admin perms on
# every repo.

set -euo pipefail

: "${GITEA_TOKEN:?required}"
: "${GITEA_HOST:?required}"
: "${REPO:?required}"
: "${PR_NUMBER:?required}"
if [ -z "${REQUIRED_CHECKS_JSON:-}" ] && [ -z "${REQUIRED_CHECKS:-}" ]; then
  echo "::error::Either REQUIRED_CHECKS_JSON or REQUIRED_CHECKS must be set"
  exit 1
fi

OWNER="${REPO%%/*}"
NAME="${REPO##*/}"
API="https://${GITEA_HOST}/api/v1"
AUTH="Authorization: token ${GITEA_TOKEN}"

# 1. Fetch the PR. If not merged, no-op.
# Fail-closed: verify HTTP 200 before parsing. A 401/403/404 means the token
# is invalid or the PR is inaccessible — we must NOT silently treat that as
# "not merged" and skip the audit.
PR_TMP=$(mktemp)
PR_HTTP=$(curl -sS -o "$PR_TMP" -w '%{http_code}' -H "$AUTH" \
  "${API}/repos/${OWNER}/${NAME}/pulls/${PR_NUMBER}")
PR=$(cat "$PR_TMP")
rm -f "$PR_TMP"
if [ "$PR_HTTP" != "200" ]; then
  echo "::error::GET /pulls/${PR_NUMBER} returned HTTP ${PR_HTTP} — cannot evaluate merge state."
  exit 1
fi
# FAIL-CLOSED: a 200 response with a missing/malformed `merged` field must
# NOT be treated as "not merged" (that would silently skip the audit).
# We verify both presence AND correct type for every field we consume.
PR_SCHEMA_OK=$(echo "$PR" | jq -r '
  (.merged | type == "boolean") and
  (.merge_commit_sha | type == "string") and
  (.merged_by | type == "object") and (.merged_by.login | type == "string") and
  (.base | type == "object") and (.base.ref | type == "string") and
  (.head | type == "object") and (.head.sha | type == "string")
')
if [ "$PR_SCHEMA_OK" != "true" ]; then
  echo "::error::GET /pulls/${PR_NUMBER} returned HTTP 200 but one or more required fields are missing, null, or of wrong type — cannot evaluate force-merge."
  exit 1
fi
MERGED=$(echo "$PR" | jq -r '.merged')
if [ "$MERGED" != "true" ]; then
  echo "::notice::PR #${PR_NUMBER} closed without merge — no audit emission."
  exit 0
fi

MERGE_SHA=$(echo "$PR" | jq -r '.merge_commit_sha')
MERGED_BY=$(echo "$PR" | jq -r '.merged_by.login')
TITLE=$(echo "$PR" | jq -r '.title // ""')
BASE_BRANCH=$(echo "$PR" | jq -r '.base.ref')
HEAD_SHA=$(echo "$PR" | jq -r '.head.sha')

# 2. Required status checks — branch-aware JSON dict takes precedence.
if [ -n "${REQUIRED_CHECKS_JSON:-}" ]; then
  # FAIL-CLOSED: if REQUIRED_CHECKS_JSON is set, the branch entry must exist
  # and be an array. A missing branch or non-array value means the config is
  # malformed or drifted — we must NOT silently treat it as "no checks".
  _RC_JSON_OK=$(echo "$REQUIRED_CHECKS_JSON" | jq -r --arg branch "$BASE_BRANCH" '
    has($branch) and (.[$branch] | type == "array")
  ')
  if [ "$_RC_JSON_OK" != "true" ]; then
    echo "::error::REQUIRED_CHECKS_JSON missing or non-array entry for branch '$BASE_BRANCH' — cannot evaluate required checks."
    exit 1
  fi
  REQUIRED=$(echo "$REQUIRED_CHECKS_JSON" | jq -r --arg branch "$BASE_BRANCH" '.[$branch] | .[]')
else
  REQUIRED="$REQUIRED_CHECKS"
fi
if [ -z "${REQUIRED//[[:space:]]/}" ]; then
  echo "::notice::REQUIRED_CHECKS empty for branch '$BASE_BRANCH' — force-merge not applicable."
  exit 0
fi

# 3. Status-check state at the PR HEAD (where checks ran). The merge
#    commit doesn't get its own checks; we evaluate the PR's last
#    commit, which is what branch protection compared against.
# Fail-closed: verify HTTP 200. A 401/403/404 means the status is
# unreadable — we must NOT treat that as "no statuses" and skip checks.
STATUS_TMP=$(mktemp)
STATUS_HTTP=$(curl -sS -o "$STATUS_TMP" -w '%{http_code}' -H "$AUTH" \
  "${API}/repos/${OWNER}/${NAME}/commits/${HEAD_SHA}/status")
STATUS=$(cat "$STATUS_TMP")
rm -f "$STATUS_TMP"
if [ "$STATUS_HTTP" != "200" ]; then
  echo "::error::GET /commits/${HEAD_SHA}/status returned HTTP ${STATUS_HTTP} — cannot evaluate required checks."
  exit 1
fi
# FAIL-CLOSED: a 200 status response missing the 'statuses' array, or with
# 'statuses' set to a non-array type (null/string/object), must NOT be treated
# as "no checks" — that would silently declare all checks green.
if ! echo "$STATUS" | jq -e '(.statuses | type) == "array"' >/dev/null; then
  echo "::error::GET /commits/${HEAD_SHA}/status returned HTTP 200 but 'statuses' is missing or not an array — cannot evaluate required checks."
  exit 1
fi
declare -A CHECK_STATE
while IFS=$'\t' read -r ctx state; do
  [ -n "$ctx" ] && CHECK_STATE[$ctx]="$state"
done < <(echo "$STATUS" | jq -r '.statuses | .[] | "\(.context)\t\(.status)"')

# 4. For each required check, was it green at merge? YAML block scalars
#    (`|`) leave a trailing newline; skip blank/whitespace-only lines.
FAILED_CHECKS=()
while IFS= read -r req; do
  trimmed="${req#"${req%%[![:space:]]*}"}"   # ltrim
  trimmed="${trimmed%"${trimmed##*[![:space:]]}"}"  # rtrim
  [ -z "$trimmed" ] && continue
  state="${CHECK_STATE[$trimmed]:-missing}"
  if [ "$state" != "success" ]; then
    FAILED_CHECKS+=("${trimmed}=${state}")
  fi
done <<< "$REQUIRED"

if [ "${#FAILED_CHECKS[@]}" -eq 0 ]; then
  echo "::notice::PR #${PR_NUMBER} merged with all required checks green — not a force-merge."
  exit 0
fi

# 5. Emit structured audit event.
NOW=$(date -u +%Y-%m-%dT%H:%M:%SZ)
# jq -R (raw input) converts each line to a JSON string; jq -s wraps into array.
# If FAILED_CHECKS is unexpectedly empty (shouldn't happen — we exit above),
# this produces []. No || true needed.
FAILED_JSON=$(printf '%s\n' "${FAILED_CHECKS[@]}" | jq -R . | jq -s .)

# Print as a single-line JSON so Vector's parse_json transform can pick
# it up cleanly from docker_logs.
jq -nc \
  --arg event_type "incident.force_merge" \
  --arg ts "$NOW" \
  --arg repo "$REPO" \
  --argjson pr "$PR_NUMBER" \
  --arg title "$TITLE" \
  --arg base "$BASE_BRANCH" \
  --arg merged_by "$MERGED_BY" \
  --arg merge_sha "$MERGE_SHA" \
  --argjson failed_checks "$FAILED_JSON" \
  '{event_type: $event_type, ts: $ts, repo: $repo, pr: $pr, title: $title,
    base_branch: $base, merged_by: $merged_by, merge_sha: $merge_sha,
    failed_checks: $failed_checks}'

echo "::warning::FORCE-MERGE detected on PR #${PR_NUMBER} by ${MERGED_BY}: ${#FAILED_CHECKS[@]} required check(s) not green at merge time."
