#!/usr/bin/env bash
# ci-status.sh
#
# SSOT library for the governance status-emission logic shared by the
# reserved-path-review / secret-scan workflows. Extracted from the inline
# workflow `run:` blocks so the load-bearing branches — canonical-domain
# derivation, CI_STATUS_TOKEN resolution (direct-secret-first, Infisical
# fallback), review-status emission, and the secret-scan re-assert — are
# unit-testable rather than untested inline YAML.
#
# This mirrors the jq-install.sh extraction precedent (core#2460 / mc#1982):
# inline `run:` logic that carries a load-bearing contract is pulled into a
# sourceable lib so the contract can be pinned by tests (see
# tests/test_ci_status.sh). The workflows `source` this file and call the
# functions; the tests source the same file and drive each branch with a
# mock curl, so the coverage exercises the REAL code (zero copy-drift).
#
# IDEMPOTENT: re-sourcing this file is a clean no-op.
if [[ -n "${__CI_STATUS_SH_SOURCED:-}" ]]; then
  return 0
fi
__CI_STATUS_SH_SOURCED=1

# CF-1010 defense: git.moleculesai.app's Cloudflare WAF / Bot-Fight 403-bans a
# blank/default User-Agent BEFORE the request reaches Gitea (the self-ban that
# fails these gates closed). Pin the required accepted UA on every curl that
# traverses the public edge.
CI_STATUS_UA='curl/8.4.0'

# derive_gitea_base — echo the Gitea base URL for API calls (no /api/v1 suffix;
# the caller appends it). Domain-only operations use the canonical public Gitea
# endpoint and deliberately ignore ambient runner-provided server URLs. A
# caller may still set GITEA_HOST explicitly (for isolated tests or another
# Gitea deployment). Any trailing slash is trimmed so the caller's
# "${base}/api/v1" never doubles up.
derive_gitea_base() {
  local base="https://${GITEA_HOST:-git.moleculesai.app}"
  printf '%s' "${base%/}"
}

# resolve_ci_status_token — resolve the write-scoped CI_STATUS_TOKEN used by the
# explicit governance-status POSTs.
#
# PRIMARY source is the direct `CI_STATUS_TOKEN` Gitea org Actions secret
# (env CI_STATUS_TOKEN_DIRECT) — no external round-trip, so emission does NOT
# fail closed when the Infisical control-plane is unreachable from the runner.
# FALLBACK is the Infisical SSOT (prod /shared/ci-status) over its public
# Cloudflare hostname, fetched WITH the CF-accepted UA so CF-1010 cannot
# edge-ban the request.
#
# On success: masks the token, sets the global CI_STATUS_TOKEN, appends
# `CI_STATUS_TOKEN=<tok>` to $GITHUB_ENV (when set, for cross-step consumption),
# logs the source + length, and returns 0.
# On empty: returns 1 after a `::error::` when REQUIRE=1 (default — fail-closed,
# the qa/security contract); returns 0 with an empty CI_STATUS_TOKEN after a
# `::warning::` when REQUIRE=0 (best-effort callers such as the secret-scan
# re-assert / fork PRs with no secrets).
#
# Tunables (env): REQUIRE (default 1), INFISICAL_BASE_URL
# (default https://key.moleculesai.app), CI_STATUS_CURL (curl binary; for tests).
resolve_ci_status_token() {
  local require="${REQUIRE:-1}"
  local infisical_base="${INFISICAL_BASE_URL:-https://key.moleculesai.app}"
  local curl_bin="${CI_STATUS_CURL:-curl}"
  local src=""
  CI_STATUS_TOKEN="${CI_STATUS_TOKEN_DIRECT:-}"

  if [ -n "$CI_STATUS_TOKEN" ]; then
    src="Gitea org secret"
  elif [ -n "${INFISICAL_CI_CLIENT_ID:-}" ] && [ -n "${INFISICAL_CI_CLIENT_SECRET:-}" ]; then
    local itok=""
    itok=$("$curl_bin" -fsS -A "$CI_STATUS_UA" -X POST "$infisical_base/api/v1/auth/universal-auth/login" \
      -H 'Content-Type: application/json' \
      -d "{\"clientId\":\"$INFISICAL_CI_CLIENT_ID\",\"clientSecret\":\"$INFISICAL_CI_CLIENT_SECRET\"}" \
      | python3 -c 'import sys,json; v=json.load(sys.stdin).get("accessToken"); sys.stdout.write(v if isinstance(v,str) else "")' || true)
    if [ -n "$itok" ]; then
      CI_STATUS_TOKEN=$("$curl_bin" -fsS -A "$CI_STATUS_UA" "$infisical_base/api/v3/secrets/raw/CI_STATUS_TOKEN?workspaceId=${INFISICAL_PROJECT_ID:-}&environment=prod&secretPath=%2Fshared%2Fci-status" \
        -H "Authorization: Bearer $itok" \
        | python3 -c 'import sys,json; v=json.load(sys.stdin).get("secret",{}).get("secretValue"); sys.stdout.write(v if isinstance(v,str) else "")' || true)
      [ -n "$CI_STATUS_TOKEN" ] && src="Infisical fallback"
    fi
  fi

  if [ -n "$CI_STATUS_TOKEN" ]; then
    echo "::add-mask::$CI_STATUS_TOKEN"
    if [ -n "${GITHUB_ENV:-}" ]; then
      echo "CI_STATUS_TOKEN=$CI_STATUS_TOKEN" >> "$GITHUB_ENV"
    fi
    echo "CI_STATUS_TOKEN resolved from ${src} (len=${#CI_STATUS_TOKEN})"
    return 0
  fi

  if [ "$require" = "1" ]; then
    echo "::error::CI_STATUS_TOKEN absent as a Gitea org secret AND Infisical fallback failed — failing closed."
    return 1
  fi
  echo "::warning::CI_STATUS_TOKEN unavailable (fork PR / secret unset / Infisical unreachable) — best-effort caller will skip emission."
  return 0
}

# emit_review_status — POST the branch-protection-required review-status context
# on a pull_request_review trigger. Gitea auto-publishes the (pull_request_review)
# context, but branch protection requires the (pull_request_target) context, so
# this explicitly POSTs it to flip the gate.
#
# Reads (env): CI_STATUS_TOKEN (resolved above), REPO, PR_NUMBER, EVAL_OUTCOME,
# STATUS_CONTEXT (the exact BP context, e.g. "reserved-path-review /
# reserved-path-review (pull_request_target)"). Derives the Gitea base
# (canonical domain, CF-UA-guarded), GETs the PR to resolve head.sha, maps
# EVAL_OUTCOME (success/other) to the posted state, and POSTs the status.
# Returns non-zero on a GET/POST failure so the miss is LOUD.
#
# Tunables (env): CI_STATUS_CURL (curl binary; for tests).
emit_review_status() {
  local curl_bin="${CI_STATUS_CURL:-curl}"
  local gitea_base authfile prfile code head_sha status_state description body post_code
  gitea_base="$(derive_gitea_base)"

  authfile=$(mktemp)
  chmod 600 "$authfile"
  {
    printf 'header = "Authorization: token %s"\n' "${CI_STATUS_TOKEN:-}"
    printf 'user-agent = "%s"\n' "$CI_STATUS_UA"
  } > "$authfile"

  prfile=$(mktemp)
  code=$("$curl_bin" -sS -o "$prfile" -w '%{http_code}' -K "$authfile" \
    "${gitea_base}/api/v1/repos/${REPO}/pulls/${PR_NUMBER}")
  if [ "$code" != "200" ]; then
    echo "::error::GET /pulls/${PR_NUMBER} returned HTTP ${code}"
    rm -f "$prfile" "$authfile"
    return 1
  fi
  # Read via stdin redirect (not a path arg) so the parse is agnostic to the
  # jq binary's path conventions.
  head_sha=$(jq -r '.head.sha // ""' < "$prfile")
  rm -f "$prfile"
  if [ -z "$head_sha" ]; then
    echo "::error::could not resolve head SHA for PR ${PR_NUMBER}"
    rm -f "$authfile"
    return 1
  fi

  if [ "${EVAL_OUTCOME:-}" = "success" ]; then
    status_state="success"
    description="Approved via pull_request_review trigger"
  else
    status_state="failure"
    description="Review check failed via pull_request_review trigger"
  fi

  body=$(jq -nc \
    --arg state "$status_state" \
    --arg context "$STATUS_CONTEXT" \
    --arg description "$description" \
    '{state:$state, context:$context, description:$description}')

  post_code=$("$curl_bin" -sS -o /dev/null -w '%{http_code}' -X POST \
    -K "$authfile" -H "Content-Type: application/json" \
    -d "$body" \
    "${gitea_base}/api/v1/repos/${REPO}/statuses/${head_sha}")
  rm -f "$authfile"

  if [ "$post_code" != "200" ] && [ "$post_code" != "201" ]; then
    echo "::error::POST /statuses/${head_sha} returned HTTP ${post_code}"
    return 1
  fi
  echo "::notice::posted ${status_state} for context=\"${STATUS_CONTEXT}\" on sha=${head_sha}"
  return 0
}

# reassert_commit_status — deterministic, success-gated, BEST-EFFORT re-assert of
# a passing commit-status context (emission-miss self-heal). Closes the Gitea/
# runner terminal-UpdateTask drop under 2-runner contention that dropped the
# passing required context on #3417 / #3418 / #3405.
#
# CONTRACT:
#   - The caller MUST gate the step on `if: success()` so this only re-asserts
#     GREEN when the real gate above actually passed; on a real failure the step
#     is skipped and Gitea's auto-emitted failure stands (no false-green mask).
#   - BEST-EFFORT — it NEVER fails the gate (always returns 0); the scan step is
#     the gate, this only closes the emission-miss.
#   - Idempotent — Gitea de-dups by context (newest-wins), so a healthy auto-emit
#     is simply reinforced.
#
# Reads (env): CI_STATUS_TOKEN (resolved; empty on fork PRs → warn + skip), REPO,
# EVENT_NAME, PR_HEAD_SHA / PUSH_SHA, CONTEXT_BASE (context WITHOUT the event
# suffix), DESCRIPTION. Derives the canonical Gitea base (CF-UA-guarded).
#
# Tunables (env): CI_STATUS_CURL (curl binary; for tests).
reassert_commit_status() {
  local curl_bin="${CI_STATUS_CURL:-curl}"
  local gitea_base sha suffix context authfile body codefile code
  gitea_base="$(derive_gitea_base)"

  if [ -n "${CI_STATUS_TOKEN:-}" ]; then echo "::add-mask::$CI_STATUS_TOKEN"; fi
  if [ -z "${CI_STATUS_TOKEN:-}" ]; then
    echo "::warning::re-assert: no CI_STATUS_TOKEN (fork PR / secret unset) — relying on Gitea auto-emit only."
    return 0
  fi

  case "${EVENT_NAME:-}" in
    pull_request) sha="${PR_HEAD_SHA:-}"; suffix=" (pull_request)" ;;
    push)         sha="${PUSH_SHA:-}";    suffix=" (push)" ;;
    *)            sha="${PUSH_SHA:-}";    suffix=" (${EVENT_NAME:-unknown})" ;;
  esac
  if [ -z "$sha" ]; then
    echo "::warning::re-assert: could not resolve target SHA for event=${EVENT_NAME:-}; relying on Gitea auto-emit."
    return 0
  fi

  context="${CONTEXT_BASE}${suffix}"
  authfile=$(mktemp)
  chmod 600 "$authfile"
  {
    printf 'header = "Authorization: token %s"\n' "$CI_STATUS_TOKEN"
    printf 'user-agent = "%s"\n' "$CI_STATUS_UA"
  } > "$authfile"

  body=$(jq -nc --arg ctx "$context" --arg desc "${DESCRIPTION:-deterministic re-assert}" \
    '{state:"success", context:$ctx, description:$desc}')

  # Route -w into a tempfile so curl's non-2xx exit code cannot pollute the
  # captured status (lint-curl-status-capture contract).
  codefile=$(mktemp)
  "$curl_bin" -sS -o /dev/null -w '%{http_code}' -X POST -K "$authfile" \
    -H 'Content-Type: application/json' -d "$body" \
    "${gitea_base}/api/v1/repos/${REPO}/statuses/${sha}" >"$codefile" 2>/dev/null || true
  code=$(cat "$codefile" 2>/dev/null); [ -z "$code" ] && code="000"
  rm -f "$codefile" "$authfile"

  if [ "$code" = "200" ] || [ "$code" = "201" ]; then
    echo "::notice::re-asserted success for context=\"${context}\" on sha=${sha}"
  else
    echo "::warning::re-assert POST returned HTTP ${code} — best-effort self-heal only; Gitea auto-emit remains the primary path."
  fi
  return 0
}
