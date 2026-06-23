#!/bin/sh
# clone-manifest.sh — clone all repos listed in manifest.json into their
# target directories. Replaces hardcoded git-clone lines in Dockerfiles.
#
# Usage:
#   ./scripts/clone-manifest.sh <manifest.json> <ws-templates-dir> <org-templates-dir> <plugins-dir>
#
# Requires: git, jq (lighter than python3 — ~2MB vs ~50MB in Alpine)
#
# Auth (optional) — two modes, keyed on MOLECULE_GITEA_TOKEN:
#   STRICT (token set; CI / operator refresh): the token grants access to
#     the private platform templates, so ANY clone failure is a genuine
#     error and aborts (exit 1). This is the build-correctness path.
#   BEST-EFFORT (no token; ecosystem contributor via setup.sh/dev-start.sh):
#     a contributor shouldn't need creds to spin up a local dev env. Clone
#     what's public; SKIP (with a warning) ONLY repos the manifest marks
#     `"private": true` — those need a token. A failure of any UNMARKED
#     (public) repo still ABORTS (exit 1), so a bad ref / deleted repo /
#     network outage is never swallowed as a missing-creds skip. Exit 0 when
#     the only failures were private skips. The palette is then sparse but
#     the platform runs.
#   Set MOLECULE_GITEA_TOKEN to the SSOT-managed template read token to
#   populate the full set.
#
#   The token (when set) never enters the Docker image: this script runs
#   in the trusted CI context BEFORE `docker buildx build`, populates
#   .tenant-bundle-deps/, then `Dockerfile.tenant` COPYs from there with
#   the .git directories already stripped (see line ~67 below).

set -euo pipefail

MANIFEST="${1:?Usage: clone-manifest.sh <manifest.json> <ws-dir> <org-dir> <plugins-dir>}"
WS_DIR="${2:?Missing workspace-templates dir}"
ORG_DIR="${3:?Missing org-templates dir}"
PLUGINS_DIR="${4:?Missing plugins dir}"

# Strip JSON5-style // comments from manifest.json before parsing.
# The automated Integration Tester appends a trailing comment
# (// Triggered by ... ) which is valid JSON5 but not standard JSON.
# jq's default parser rejects it. This sed removes only full-line comments
# (lines starting with optional whitespace followed by //) before jq reads the file.
_strip_comments() {
    # Remove full-line // comments (whitespace-safe); pass-through for non-comment lines
    sed 's/^[[:space:]]*\/\/.*//' "$MANIFEST"
}
MANIFEST_JSON="$(_strip_comments)"

EXPECTED=0
CLONED=0
SKIPPED=0

# Strict vs best-effort mode.
#
# STRICT=1 when MOLECULE_GITEA_TOKEN is set (CI / operator refresh): the
# token grants access to the private platform templates, so ANY clone
# failure is a genuine error and must fail the build.
#
# STRICT=0 when no token is set (ecosystem contributor running
# infra/scripts/setup.sh → dev-start.sh): the private platform templates
# (seo-agent, platform-agent, google-adk — internal IP) are simply not
# fetchable. Hard-failing here blocked local bootstrap on creds a
# contributor doesn't have (and shouldn't need). In this mode we clone what
# is public, SKIP what we can't access with a warning, and exit 0 — the
# Canvas template palette is then sparse but the platform runs. We still
# fail loudly if even the PUBLIC repos can't be cloned (real network /
# manifest breakage, not just missing creds).
if [ -n "${MOLECULE_GITEA_TOKEN:-}" ]; then STRICT=1; else STRICT=0; fi

# clone_one_with_retry — clone a single repo, retrying on transient failure.
#
# Why: the publish-workspace-server-image (and harness-replays) CI jobs
# clone the full manifest (~36 repos) serially on a memory-constrained
# Gitea Actions runner. Under host memory pressure the OOM killer
# occasionally SIGKILLs git-remote-https mid-clone:
#
#   error: git-remote-https died of signal 9
#   fatal: the remote end hung up unexpectedly
#
# (observed in publish-workspace-server-image run 4622 on 2026-05-10 — the
# job died on the 14th of 36 clones, which wedged staging→main). One
# transient SIGKILL / network blip would otherwise fail the whole tenant
# image rebuild. Retrying after a short backoff lets the pressure subside.
# The durable fix is more runner RAM/swap (tracked with Infra-SRE); this
# just stops a single flake from being release-blocking.
#
# Args: <target_dir> <name> <clone_url> <display_url> <ref> [max_attempts]
# max_attempts defaults to 3 (CI: retry transient SIGKILL/network flakes).
# Best-effort callers pass 1 — a tokenless private-repo clone fails on auth,
# not a transient flake, so retrying just wastes the backoff window.
clone_one_with_retry() {
    local tdir="$1" name="$2" url="$3" display="$4" ref="$5"
    local attempt=1 max_attempts="${6:-3}" backoff

    while : ; do
        # A killed attempt can leave a partial directory behind; git clone
        # refuses a non-empty target, so wipe it before each try.
        rm -rf "$tdir/$name"

        if [ "$ref" = "main" ]; then
            if git clone --depth=1 -q "$url" "$tdir/$name"; then return 0; fi
        elif echo "$ref" | grep -qE '^[0-9a-f]{40}$'; then
            # Pinned SHA (RFC #2927 manifest ref-pinning): `--branch <sha>` fails
            # with "Remote branch <sha> not found" because git's --branch only
            # resolves named refs. Clone the full repo (no --depth so the SHA
            # is reachable in history) then check out the pinned SHA.
            if git clone -q "$url" "$tdir/$name" \
                && (cd "$tdir/$name" && git checkout -q "$ref"); then
                # Drop .git after checkout — we only need the tree (matches
                # the post-clone .git strip below in clone_category).
                rm -rf "$tdir/$name/.git"
                return 0
            fi
        else
            if git clone --depth=1 -q --branch "$ref" "$url" "$tdir/$name"; then return 0; fi
        fi

        if [ "$attempt" -ge "$max_attempts" ]; then
            # Single-attempt best-effort callers handle their own (friendlier)
            # messaging; only the retrying CI path emits the ::error:: annotation.
            if [ "$max_attempts" -gt 1 ]; then
                echo "::error::clone failed after ${max_attempts} attempts: ${display}" >&2
            fi
            return 1
        fi
        backoff=$((attempt * 3))   # 3s, then 6s
        echo "  ⚠ clone attempt ${attempt}/${max_attempts} failed for ${display} — retrying in ${backoff}s" >&2
        sleep "$backoff"
        attempt=$((attempt + 1))
    done
}

clone_category() {
    local category="$1"
    local target_dir="$2"

    mkdir -p "$target_dir"

    local count
    count=$(echo "$MANIFEST_JSON" | jq -r ".${category} | length")
    EXPECTED=$((EXPECTED + count))

    local i=0
    while [ "$i" -lt "$count" ]; do
        local name repo ref private
        name=$(echo "$MANIFEST_JSON" | jq -r ".${category}[$i].name")
        repo=$(echo "$MANIFEST_JSON" | jq -r ".${category}[$i].repo")
        ref=$(echo "$MANIFEST_JSON" | jq -r ".${category}[$i].ref // \"main\"")
        # `private: true` marks repos that REQUIRE a token to clone. Only
        # these may be skipped in best-effort (tokenless) mode; an unmarked
        # (public) repo that fails is a genuine error and must fail the run
        # even without a token. (manifest.json _comment.)
        private=$(echo "$MANIFEST_JSON" | jq -r ".${category}[$i].private // false")

        # Idempotent: skip if the target already looks populated. Lets the
        # README quickstart rerun setup.sh safely without having to delete
        # already-cloned repos. A directory with any entries counts as
        # populated; empty dirs reclone (may exist from a prior failed run).
        if [ -d "$target_dir/$name" ] && [ -n "$(ls -A "$target_dir/$name" 2>/dev/null || true)" ]; then
            echo "  skipping $target_dir/$name (already populated)"
            CLONED=$((CLONED + 1))
            i=$((i + 1))
            continue
        fi

        # Build the clone URL. When MOLECULE_GITEA_TOKEN is set (CI path)
        # embed it as basic-auth so private repos succeed. The username
        # part ("oauth2") is conventional and ignored by Gitea — only the
        # token-as-password is verified.
        #
        # manifest.json was migrated to lowercase org slugs on
        # 2026-05-07 (post-suspension reconciliation), so we use $repo
        # verbatim — no on-the-fly tolower transform needed.
        if [ -n "${MOLECULE_GITEA_TOKEN:-}" ]; then
            clone_url="https://oauth2:${MOLECULE_GITEA_TOKEN}@git.moleculesai.app/${repo}.git"
            display_url="https://oauth2:***@git.moleculesai.app/${repo}.git"
        else
            clone_url="https://git.moleculesai.app/${repo}.git"
            display_url="$clone_url"
        fi

        echo "  cloning $display_url -> $target_dir/$name (ref=$ref)"
        if [ "$STRICT" -eq 1 ]; then
            # Token present → genuine clone. Retry transient flakes; a final
            # failure is a real error and must abort the build.
            if clone_one_with_retry "$target_dir" "$name" "$clone_url" "$display_url" "$ref" 3; then
                CLONED=$((CLONED + 1))
            else
                echo "::error::clone failed for '$name' ($display_url) with MOLECULE_GITEA_TOKEN set — genuine failure, not a missing-creds skip" >&2
                exit 1
            fi
        else
            # No token → best effort. A failure is only TOLERATED for a repo
            # explicitly marked `private: true` (needs creds we don't have).
            # A failure of any UNMARKED (public) repo — bad ref, deleted repo,
            # DNS/network outage, git regression, Gitea non-auth error — is a
            # GENUINE error and must still abort, so a real outage can never be
            # silently swallowed as a missing-creds skip.
            if clone_one_with_retry "$target_dir" "$name" "$clone_url" "$display_url" "$ref" 1; then
                CLONED=$((CLONED + 1))
            elif [ "$private" = "true" ]; then
                echo "  ⚠ skipping '$name' — marked private and MOLECULE_GITEA_TOKEN is unset (set the token to include it). Bootstrap continues with a reduced template palette." >&2
                SKIPPED=$((SKIPPED + 1))
                rm -rf "$target_dir/$name"   # drop any partial dir so a later token-backed run re-clones cleanly
            else
                echo "::error::clone failed for PUBLIC repo '$name' ($display_url) — genuine failure (not a missing-creds skip). Check the manifest ref / network / repo existence. (If this repo is actually private, mark it \"private\": true in manifest.json.)" >&2
                exit 1
            fi
        fi
        i=$((i + 1))
    done

    # Strip .git dirs to save space
    find "$target_dir" -name '.git' -type d -exec rm -rf {} + 2>/dev/null || true
}

echo "==> Cloning workspace templates..."
clone_category "workspace_templates" "$WS_DIR"

echo "==> Cloning org templates..."
clone_category "org_templates" "$ORG_DIR"

echo "==> Cloning plugins..."
clone_category "plugins" "$PLUGINS_DIR"

# Verify the outcome.
if [ "$STRICT" -eq 1 ]; then
    # Token present: every expected repo must have cloned.
    if [ "$CLONED" -ne "$EXPECTED" ]; then
        echo "::error::Expected $EXPECTED repos but only cloned $CLONED — some clones failed"
        exit 1
    fi
    echo "==> Done. $CLONED/$EXPECTED repos cloned successfully."
else
    # No token, and we got here — so every failure was a tolerated `private`
    # skip (any PUBLIC-repo failure would already have exited 1 above). A real
    # outage can't reach this point: it fails the public clones first. setup.sh
    # tolerates an empty palette (the platform falls through to a bare
    # default), so we exit 0. CLONED==0 here just means every manifest entry
    # was marked private — warn loudly, but still don't block bootstrap.
    if [ "$CLONED" -eq 0 ] && [ "$EXPECTED" -gt 0 ]; then
        echo "  ⚠ WARNING: 0/$EXPECTED template/plugin repos cloned ($SKIPPED private, skipped) — every manifest entry needs MOLECULE_GITEA_TOKEN. The platform will start with an EMPTY template palette. Set the token to populate it." >&2
    else
        echo "==> Done (best-effort, no MOLECULE_GITEA_TOKEN). $CLONED/$EXPECTED cloned, $SKIPPED skipped (marked private; set the token to include them)."
    fi
fi
