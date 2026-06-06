#!/usr/bin/env bash
# sop-tier-check — verify a Gitea PR satisfies the §SOP-6 approval gate.
#
# Reads the PR's tier label, walks approving reviewers, and checks team
# membership against the tier's approval expression. Passes only when
# ALL clauses in the expression are satisfied by the set of approving
# reviewers (AND-composition; internal#189).
#
# Expression syntax:
#   "team-a"          — OR-set: any ONE of the comma-separated teams
#   "team-a AND team-b" — AND: BOTH must each have ≥1 approver
#   "(a,b,c)"         — OR-set wrapped in parens; same as "a,b,c"
#
# Example: "qa AND security AND (managers,ceo)" means:
#   ≥1 approver in team "qa"  AND
#   ≥1 approver in team "security"  AND
#   ≥1 approver in team "managers" OR "ceo"
#
# Per the spec (internal#189), the hard gate here pairs with the
# advisory gate of sop-conformance LLM-judge (internal#188): each
# required-team click must reflect real verification (visible in review
# body or A2A messages), not rubber-stamp APPROVE. Both gates together
# close the "teammate clicks APPROVE without verifying" gap.
#
# Invoked from `.gitea/workflows/sop-tier-check.yml`. The workflow sets
# the env vars below; this script does no IO outside of stdout/stderr +
# the Gitea API.
#
# Required env:
#   GITEA_TOKEN   — bot PAT with read:organization,read:user,
#                   read:issue,read:repository scopes
#   GITEA_HOST    — e.g. git.moleculesai.app
#   REPO          — owner/name (from github.repository)
#   PR_NUMBER     — int (from github.event.pull_request.number)
#   PR_AUTHOR     — login (from github.event.pull_request.user.login)
#
# Optional:
#   SOP_DEBUG=1        — print per-API-call diagnostic lines. Default: off.
#   SOP_LEGACY_CHECK=1 — revert to OR-gate (≥1 approver from any eligible
#                         team). Grace window for PRs in-flight when the
#                         new AND-composition was deployed. Expires 2026-05-17
#                         (7-day burn-in window; internal#189 Phase 1).
#                         Set by workflow for PRs merged before the deploy.

set -euo pipefail

# Ensure jq is available. Runners may not have it pre-installed, and the
# workflow-level jq install can fail on runners with network restrictions
# (GitHub releases not reachable from some runner networks — infra#241
# follow-up). This fallback is idempotent — no-op when jq is already on PATH.
# SOP_FAIL_OPEN=1 makes this always exit 0 so CI never blocks on jq absence.
if ! command -v jq >/dev/null 2>&1; then
  echo "::notice::jq not found on PATH — attempting install..."
  _jq_installed="no"
  # apt-get first (primary) — Ubuntu package mirrors are reliably reachable.
  if apt-get update -qq && apt-get install -y -qq jq 2>/dev/null; then
    echo "::notice::jq installed via apt-get: $(jq --version)"
    _jq_installed="yes"
  # GitHub binary as secondary fallback — may fail on restricted networks.
  elif timeout 120 curl -sSL \
    "https://github.com/jqlang/jq/releases/download/jq-1.7.1/jq-linux-amd64" \
    -o /usr/local/bin/jq \
    && chmod +x /usr/local/bin/jq; then
    echo "::notice::jq binary downloaded: $(/usr/local/bin/jq --version)"
    _jq_installed="yes"
  fi
  if ! command -v jq >/dev/null 2>&1; then
    echo "::error::jq installation failed — apt-get and GitHub binary both failed."
    echo "::error::sop-tier-check requires jq for all JSON API parsing."
    # SOP_FAIL_OPEN=1 is set in the workflow step's env — makes script always
    # exit 0 so CI never blocks. The SOP-6 tier review gate remains enforced.
    if [ "${SOP_FAIL_OPEN:-}" = "1" ]; then
      echo "::warning::SOP_FAIL_OPEN=1 — exiting 0 so CI does not block."
      exit 0
    fi
    exit 1
  fi
fi

debug() {
  if [ "${SOP_DEBUG:-}" = "1" ]; then
    echo "  [debug] $*" >&2
  fi
}

# Validate env
: "${GITEA_TOKEN:?GITEA_TOKEN required}"
: "${GITEA_HOST:?GITEA_HOST required}"
: "${REPO:?REPO required (owner/name)}"
: "${PR_NUMBER:?PR_NUMBER required}"
: "${PR_AUTHOR:?PR_AUTHOR required}"

OWNER="${REPO%%/*}"
NAME="${REPO##*/}"
API="https://${GITEA_HOST}/api/v1"
AUTH="Authorization: token ${GITEA_TOKEN}"
echo "::notice::tier-check start: repo=$OWNER/$NAME pr=$PR_NUMBER author=$PR_AUTHOR"

# Sanity: token resolves to a user.
# Use || true on the jq pipeline so that set -euo pipefail (line 45) does not
# cause the script to exit prematurely when the token is empty/invalid — the
# if check below handles that case gracefully. Without || true, a 401 from an
# empty/invalid token causes jq to exit 1, triggering set -e and exiting the
# entire script before SOP_FAIL_OPEN can be evaluated (the check is in the jq-
# install block; if jq is already on PATH, that block is skipped entirely).
WHOAMI=$(curl -sS -H "$AUTH" "${API}/user" | jq -r '.login // ""') || true
if [ -z "$WHOAMI" ]; then
  echo "::error::GITEA_TOKEN cannot resolve a user via /api/v1/user — check the token scope and that the secret is wired correctly."
  if [ "${SOP_FAIL_OPEN:-}" = "1" ]; then
    echo "::warning::SOP_FAIL_OPEN=1 — exiting 0 so CI does not block."
    exit 0
  fi
  exit 1
fi
echo "::notice::token resolves to user: $WHOAMI"

# 0.5 Read PR head SHA so we can reject stale approvals after head moves
# (internal#816). Reviews carry the commit_id they were submitted against.
HEAD_SHA=$(curl -sS -H "$AUTH" "${API}/repos/${OWNER}/${NAME}/pulls/${PR_NUMBER}" | jq -r '.head.sha // ""') || true
if [ -z "$HEAD_SHA" ]; then
  echo "::error::Failed to fetch PR head SHA — token may be invalid."
  if [ "${SOP_FAIL_OPEN:-}" = "1" ]; then
    echo "::warning::SOP_FAIL_OPEN=1 — exiting 0 so CI does not block."
    exit 0
  fi
  exit 1
fi
debug "pr-head-sha=$HEAD_SHA"

# 1. Read tier label. || true ensures set -euo pipefail does not abort the
# script if curl or jq fails (e.g. 401 from empty token).
LABELS=$(curl -sS -H "$AUTH" "${API}/repos/${OWNER}/${NAME}/issues/${PR_NUMBER}/labels" | jq -r '.[].name') || true
TIER=""
for L in $LABELS; do
  case "$L" in
    tier:low|tier:medium|tier:high)
      if [ -n "$TIER" ]; then
        echo "::error::Multiple tier labels: $TIER + $L. Apply exactly one."
        exit 1
      fi
      TIER="$L"
    ;;
  esac
done
if [ -z "$TIER" ]; then
  echo "::error::PR has no tier:low|tier:medium|tier:high label. Apply one before merge."
  exit 1
fi
debug "tier=$TIER"

# 2. Tier → required team expression (AND-composition; internal#189)
#
# Expression syntax:
#   clause-a AND clause-b AND ...   — ALL clauses must pass
#   team-a,team-b,team-c            — OR-set: ≥1 approver in ANY of these teams
#   (team-a,team-b)                 — same as team-a,team-b (parens optional)
#
# This map is the single source of truth. Update it when the team structure
# or policy changes. Teams referenced here but absent in Gitea are treated
# as unachievable (would always fail) — operators notice the clear error
# and create the missing team.
#
# Current Gitea teams: ceo, engineers, managers, qa, security
declare -A TIER_EXPR=(
  # tier:low — same as previous OR gate: any engineer, manager, or ceo.
  ["tier:low"]="engineers,managers,ceo"

  # tier:medium — AND of (managers) AND (engineers) AND (qa,security)
  # ≥1 approver from managers AND ≥1 from engineers AND ≥1 from qa OR security.
  ["tier:medium"]="managers AND engineers AND qa,security"

  # tier:high — ceo only. The AND-composition adds no value for a
  # single-team gate, but the framework is wired for consistency.
  ["tier:high"]="ceo"
)

EXPR="${TIER_EXPR[$TIER]-}"
if [ -z "$EXPR" ]; then
  echo "::error::No expression defined for tier $TIER in TIER_EXPR map."
  exit 1
fi
debug "expression=$EXPR"

# 3. Legacy OR-gate override (7-day burn-in grace window; internal#189 Phase 1)
if [ "${SOP_LEGACY_CHECK:-}" = "1" ]; then
  LEGACY_ELIGIBLE=""
  case "$TIER" in
    tier:low)    LEGACY_ELIGIBLE="engineers managers ceo" ;;
    tier:medium) LEGACY_ELIGIBLE="managers ceo" ;;
    tier:high)   LEGACY_ELIGIBLE="ceo" ;;
  esac
  echo "::notice::SOP_LEGACY_CHECK=1 — using OR-gate ({$LEGACY_ELIGIBLE}) for this PR."
  ELIGIBLE="$LEGACY_ELIGIBLE"
fi

# 4. Resolve all team names → IDs
# /orgs/{org}/teams/{slug}/... endpoints don't exist on Gitea 1.22;
# we use /teams/{id}.
# set +e prevents set -e from aborting the script if curl fails (e.g. empty token).
ORG_TEAMS_FILE=$(mktemp)
trap 'rm -f "$ORG_TEAMS_FILE"' EXIT
set +e
HTTP_CODE=$(curl -sS -o "$ORG_TEAMS_FILE" -w '%{http_code}' -H "$AUTH" \
  "${API}/orgs/${OWNER}/teams")
_HTTP_EXIT=$?
set -e
debug "teams-list HTTP=$HTTP_CODE (curl exit=$_HTTP_EXIT) size=$(wc -c <"$ORG_TEAMS_FILE")"
if [ "${SOP_DEBUG:-}" = "1" ]; then
  echo "  [debug] teams-list body (first 300 chars):" >&2
  head -c 300 "$ORG_TEAMS_FILE" >&2; echo >&2
fi
if [ "$_HTTP_EXIT" -ne 0 ] || [ "$HTTP_CODE" != "200" ]; then
  echo "::error::GET /orgs/${OWNER}/teams failed (curl exit=$_HTTP_EXIT HTTP=$HTTP_CODE) — token may lack read:org scope or be invalid."
  if [ "${SOP_FAIL_OPEN:-}" = "1" ]; then
    echo "::warning::SOP_FAIL_OPEN=1 — exiting 0 so CI does not block."
    exit 0
  fi
  exit 1
fi

# Collect every team name that appears in the expression.
# Bash word-splitting on $EXPR splits on spaces, so "AND" appears as a
# token. We skip it explicitly.
declare -A TEAM_ID
_all_teams=""
for _raw_clause in $EXPR; do
  # Strip parens and split on comma.
  _clause=${_raw_clause//[()]/}
  for _t in $(echo "$_clause" | tr ',' '\n'); do
    _t=$(echo "$_t" | tr -d '[:space:]')
    [ -z "$_t" ] && continue
    # Skip AND / OR operator tokens (bash word-split produced them from
    # spaces in the expression string).
    [ "$_t" = "AND" ] || [ "$_t" = "OR" ] && continue
    # Skip if already in set.
    case " $_all_teams " in
      *" $_t "*) ;;  # already present
      *) _all_teams="${_all_teams} $_t " ;;
    esac
  done
done

for _t in $_all_teams; do
  _t=$(echo "$_t" | tr -d ' ')
  [ -z "$_t" ] && continue
  _id=$(jq -r --arg t "$_t" '.[] | select(.name==$t) | .id' <"$ORG_TEAMS_FILE" | head -1)
  if [ -z "$_id" ] || [ "$_id" = "null" ]; then
    # "??" suffix marks teams that don't exist yet (tier:medium qa/security).
    # Treat as permanently failing clause; clear error message guides ops.
    if [[ "$_t" == *"???" ]]; then
      debug "team \"$_t\" not found (expected — pending team creation per internal#189)"
      continue
    fi
    _visible=$(jq -r '.[]?.name? // empty' <"$ORG_TEAMS_FILE" 2>/dev/null | tr '\n' ' ')
    echo "::error::Team \"$_t\" referenced in tier $TIER expression but not found in org $OWNER. Teams visible: $_visible"
    exit 1
  fi
  TEAM_ID[$_t]="$_id"
  debug "team-id: $_t → $_id"
done

# 5. Read approving reviewers. set +e disables set -e temporarily so that curl
# failures (e.g. empty/invalid token → HTTP 401) do not abort the script before
# SOP_FAIL_OPEN is evaluated. set -e is restored immediately after.
set +e
REVIEWS=$(curl -sS -H "$AUTH" "${API}/repos/${OWNER}/${NAME}/pulls/${PR_NUMBER}/reviews")
_REVIEWS_EXIT=$?
set -e
if [ $_REVIEWS_EXIT -ne 0 ] || [ -z "$REVIEWS" ]; then
  echo "::error::Failed to fetch reviews (curl exit=$_REVIEWS_EXIT) — token may be invalid or unreachable."
  if [ "${SOP_FAIL_OPEN:-}" = "1" ]; then
    echo "::warning::SOP_FAIL_OPEN=1 — exiting 0 so CI does not block."
    exit 0
  fi
  exit 1
fi
APPROVERS=$(echo "$REVIEWS" | jq -r --arg head_sha "$HEAD_SHA" '[.[] | select(.state=="APPROVED" and .commit_id == $head_sha) | .user.login] | unique | .[]') || true
if [ -z "$APPROVERS" ]; then
  echo "::error::No approving reviews on this PR. Set SOP_DEBUG=1 and re-run for diagnostics."
  exit 1
fi
debug "approvers: $(echo "$APPROVERS" | tr '\n' ' ')"

# 6. For each approver: skip self-review; probe team membership by id.
# Build $APPROVER_TEAMS[<user>]=space-surrounded team names (e.g. " managers ").
# Pre/post spaces ensure case patterns *${_t}* match even when the name
# is the first or last entry (bash case *word* needs delimiters on both sides).
#
# FAIL-CLOSED AUTHORIZATION (security: SOP tier gate is an AUTHORIZATION gate).
#
# This used to fall back to /orgs/{org}/members/{user} whenever every team
# probe failed and credit any org member as a member of EVERY queried team.
# That was a privilege-escalation: org membership is NOT team membership, so
# a 403/visibility/token-scope gap on the team probes silently promoted a
# plain org member to satisfy tier:high (ceo). An inability-to-verify became
# an authorization GRANT. The fallback is REMOVED — org membership must never
# satisfy a team-gated tier.
#
# A team-membership probe has exactly three meaningful outcomes:
#   200 / 204  → the user IS a member of that team       (credit it)
#   404        → the user is definitively NOT a member    (no credit, verified)
#   anything else (403 / 401 / 5xx / curl failure / non-numeric)
#              → membership CANNOT be read                 (cannot-verify)
#
# Per the dev-sop fail-closed rule (inability-to-verify = failure, never a
# pass — and here, never an authorization grant), a cannot-verify outcome on
# ANY probe is a HARD infra failure: we publish a loud cannot-verify error and
# exit non-zero. We do NOT proceed to evaluate the tier expression on a partial
# / unverifiable membership picture, because doing so could let an unverifiable
# approver's clause silently fail-or-pass on incomplete data. Fix the token
# scope (read:organization) or the runner network — not the gate.
declare -A APPROVER_TEAMS
_verify_failed=""   # accumulates "<user>:<team>(HTTP <code>)" for probes we could not read
for U in $APPROVERS; do
  [ "$U" = "$PR_AUTHOR" ] && debug "skip self-review by $U" && continue
  for T in "${!TEAM_ID[@]}"; do
    ID="${TEAM_ID[$T]}"
    set +e
    CODE=$(curl -sS -o /dev/null -w '%{http_code}' -H "$AUTH" \
      "${API}/teams/${ID}/members/${U}")
    _curl_exit=$?
    set -e
    debug "probe: $U in team $T (id=$ID) → HTTP $CODE (curl exit=$_curl_exit)"
    if [ "$_curl_exit" -ne 0 ]; then
      # curl itself failed (DNS, connection refused, timeout) — unreachable.
      _verify_failed="${_verify_failed}${_verify_failed:+, }${U}:${T}(curl exit ${_curl_exit})"
      continue
    fi
    case "$CODE" in
      200|204)
        APPROVER_TEAMS[$U]="${APPROVER_TEAMS[$U]:- } ${APPROVER_TEAMS[$U]:+ }$T "
        debug "$U qualifies for team $T"
        ;;
      404)
        # Definitively not a member of this team — a verified negative.
        debug "$U is NOT a member of team $T (verified 404)"
        ;;
      *)
        # 403/401/5xx/etc — membership is unreadable. Do NOT treat as "not a
        # member" and do NOT fall back to org membership. This is cannot-verify.
        _verify_failed="${_verify_failed}${_verify_failed:+, }${U}:${T}(HTTP ${CODE})"
        ;;
    esac
  done
done

# Fail-closed: if ANY membership probe could not be read, we cannot make an
# authorization decision. Publish a loud cannot-verify / infra-failed status
# and exit non-zero. Never grant the tier on unverifiable membership.
if [ -n "$_verify_failed" ]; then
  echo "::error::sop-tier-check CANNOT VERIFY team membership — gate FAILS CLOSED."
  echo "::error::Unreadable membership probe(s): ${_verify_failed}"
  echo "::error::A team-membership probe returned 403/401/5xx (or curl failed). The SOP tier gate is an authorization gate; an inability to verify team membership is treated as a FAILURE, never a pass. Org membership is NOT team membership and is never credited as a fallback."
  echo "::error::Fix: ensure GITEA_TOKEN (SOP_TIER_CHECK_TOKEN) has read:organization scope and the Gitea API is reachable from the runner, then re-run. Do NOT relax this gate."
  exit 1
fi

# 7. Evaluate the tier expression.
#
# legacy OR-gate: use the simplified loop from before internal#189.
if [ -n "${LEGACY_ELIGIBLE:-}" ]; then
  OK=""
  for _u in "${!APPROVER_TEAMS[@]}"; do
    for _t2 in $LEGACY_ELIGIBLE; do
      case "${APPROVER_TEAMS[$_u]}" in
        *${_t2}*)
          echo "::notice::approver $_u is in team $_t2 (eligible for $TIER)"
          OK="yes"
          break
        ;;
      esac
    done
    [ -n "$OK" ] && break
  done
  if [ -z "$OK" ]; then
    echo "::error::Tier $TIER requires approval from a non-author member of {$LEGACY_ELIGIBLE}. Set SOP_DEBUG=1 to see per-probe HTTP codes."
    exit 1
  fi
  echo "::notice::sop-tier-check passed: $TIER (legacy OR-gate)"
  exit 0
fi

# AND-gate: evaluate the expression clause by clause.
# _passed_clauses and _failed_clauses accumulate for the status description.
_passed_clauses=""
_failed_clauses=""

for _raw_clause in $EXPR; do
  # Normalise: strip parens, replace commas with spaces so bash word-split
  # can iterate the OR-set members. The previous form
  #   _clause=$(echo ... | tr ',' '\n' | tr -d '[:space:]' | grep -v '^$')
  # collapsed every member into one concatenated token because
  # `tr -d '[:space:]'` strips the very newlines that just separated them
  # ("engineers,managers,ceo" -> "engineersmanagersceo"), so the OR-clause
  # only ever evaluated as a single nonsense team name and never matched
  # APPROVER_TEAMS. Fixed in #229: leave the comma-separated members as
  # space-separated tokens for `for _t in $_clause`.
  _no_parens=${_raw_clause//[()]/}
  _clause=${_no_parens//,/ }
  _clause_passed="no"
  _clause_names=""
  for _t in $_clause; do
    # Append (don't overwrite) team name to the human-readable accumulator.
    # The previous form `_clause_names="${_clause_names:+, }${_t}"`
    # rewrote the variable on every iteration, so the FAIL message only
    # ever showed the LAST team. Fixed: prepend prior value before the
    # comma-separator, then append the new team name.
    _clause_names="${_clause_names}${_clause_names:+, }${_t}"
    # Skip teams not yet in Gitea (qa??? / security??? placeholders).
    [[ "$_t" == *"???" ]] && debug "clause \"$_t\": skipped (team pending creation)" && continue
    [ -z "${TEAM_ID[$_t]:-}" ] && debug "clause \"$_t\": no ID resolved, skipping" && continue
    for _u in "${!APPROVER_TEAMS[@]}"; do
      # Note: APPROVER_TEAMS values are space-surrounded (e.g. " managers ").
      # Pattern *${_t}* matches team name anywhere in the space-padded string.
      case "${APPROVER_TEAMS[$_u]}" in
        *${_t}*)
          _clause_passed="yes"
          debug "clause \"$_t\": satisfied by $_u"
          break
        ;;
      esac
    done
  done

  # Label for display: strip "???" from pending teams.
  _label=$(echo "$_raw_clause" | tr -d '()' | tr ',' '/' | tr -d '[:space:]' | sed 's/???//g')

  if [ "$_clause_passed" = "yes" ]; then
    # Append (don't overwrite) — same accumulator bug as _clause_names above.
    _passed_clauses="${_passed_clauses}${_passed_clauses:+, }$_label"
    echo "::notice::clause [$_label]: PASS — satisfied by approving reviewer(s)"
  else
    _failed_clauses="${_failed_clauses}${_failed_clauses:+, }$_label"
    echo "::error::clause [$_label]: FAIL — no approving reviewer belongs to any of these teams (${_clause_names}). Set SOP_DEBUG=1 to see per-team probe results."
  fi
done

if [ -n "$_failed_clauses" ]; then
  echo ""
  echo "::error::sop-tier-check FAILED for $TIER."
  echo "  Passed :${_passed_clauses}"
  echo "  Missing:${_failed_clauses}"
  echo "  All clauses must be satisfied. Each missing team needs an APPROVED review from one of its members."
  exit 1
fi

echo "::notice::sop-tier-check PASSED: $TIER — all required clauses satisfied [${_passed_clauses}]"
