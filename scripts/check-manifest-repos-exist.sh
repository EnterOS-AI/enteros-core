#!/usr/bin/env bash
# check-manifest-repos-exist.sh — fail-fast guard: verify every repo listed in
# manifest.json actually exists on Gitea before the expensive clone step runs.
#
# WHY: deleting an org-template/workspace-template repo that is still listed in
# manifest.json breaks clone-manifest.sh with a generic git 404 error. The
# failure is deep in the publish-workspace-server-image workflow and looks like
# a transient network issue, wasting debug time. This script surfaces the
# problem immediately with a per-entry ::error:: annotation naming the missing
# repo (issue #2192).
#
# Usage:
#   ./scripts/check-manifest-repos-exist.sh <manifest.json>
#
# Exit:
#   0  all repos exist
#   1  one or more repos 404 (printed to stderr)
#   2  bad usage / missing inputs

set -euo pipefail

MANIFEST="${1:-manifest.json}"
GITEA_API="${GITEA_API:-https://git.moleculesai.app/api/v1/repos}"

if [ ! -f "$MANIFEST" ]; then
    echo "::error::manifest not found: $MANIFEST" >&2
    exit 2
fi

# Strip JSON5-style // comments before parsing (same as clone-manifest.sh)
_strip_comments() {
    sed 's/^[[:space:]]*\/\/.*//' "$MANIFEST"
}

MANIFEST_JSON="$(_strip_comments)"

MISSING=0
TOTAL=0

# Categories to check — must match clone-manifest.sh categories
check_category() {
    local category="$1"
    local count
    count=$(echo "$MANIFEST_JSON" | jq -r ".${category} | length")

    local i=0
    while [ "$i" -lt "$count" ]; do
        local name repo provider api_base token
        name=$(echo "$MANIFEST_JSON" | jq -r ".${category}[$i].name")
        repo=$(echo "$MANIFEST_JSON" | jq -r ".${category}[$i].repo")
        # provider names the SCM host the repo path resolves against
        # (see manifest.json _provider_contract). Absent ⇒ moleculesai.
        provider=$(echo "$MANIFEST_JSON" | jq -r ".${category}[$i].provider // \"moleculesai\"")
        TOTAL=$((TOTAL + 1))

        # Resolve the provider to its API base + read-token env var. Mirrors
        # clone-manifest.sh's resolver (keep the cases in sync). The GITEA_API
        # env override is preserved as the moleculesai default for back-compat.
        case "$provider" in
            moleculesai) api_base="${GITEA_API:-https://git.moleculesai.app/api/v1/repos}"; token="${MOLECULE_GITEA_TOKEN:-}" ;;
            github)      api_base="${GITHUB_API:-https://api.github.com/repos}";            token="${MOLECULE_GITHUB_TOKEN:-}" ;;
            *) echo "::error::manifest.json ${category} entry '${name}': unknown provider '${provider}' (known: moleculesai, github)" >&2; MISSING=$((MISSING + 1)); i=$((i + 1)); continue ;;
        esac

        # Check repo existence via the provider API. Many manifest repos are
        # PRIVATE (e.g. the workspace templates), so an *unauthenticated* GET
        # returns 404 even when the repo exists — indistinguishable from a
        # genuinely missing repo. We therefore authenticate with the same
        # token clone-manifest.sh uses. A 404 *with* a valid token still means
        # the repo is truly missing, which is what we want to catch. If the
        # token is unset (local dev), fall back to an unauthenticated request
        # — private repos will then 404, so run the check in CI where the
        # token is present.
        if [ -n "$token" ]; then
            http_code=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 10 \
                -H "Authorization: token ${token}" \
                "${api_base}/${repo}" 2>/dev/null || true)
        else
            http_code=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 10 "${api_base}/${repo}" 2>/dev/null || true)
        fi

        if [ "$http_code" != "200" ]; then
            echo "::error::manifest.json ${category} entry '${name}' → repo '${repo}' returned HTTP ${http_code} (expected 200). Delete the manifest entry BEFORE deleting the repo." >&2
            MISSING=$((MISSING + 1))
        fi

        i=$((i + 1))
    done
}

echo "==> Checking manifest repo existence (per-entry provider; moleculesai default → ${GITEA_API}) ..."
check_category "plugins"
check_category "workspace_templates"
check_category "org_templates"

if [ "$MISSING" -gt 0 ]; then
    echo "::error::${MISSING}/${TOTAL} manifest entries are missing — fix manifest.json before publishing." >&2
    exit 1
fi

echo "✓ All ${TOTAL} manifest entries resolved (HTTP 200)."
exit 0
