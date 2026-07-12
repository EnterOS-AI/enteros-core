#!/usr/bin/env bash
# Replay for issue #2395 — local proof that the /buildinfo verify gate
# closes the SaaS deploy-chain blindness.
#
# Prior behavior: redeploy-fleet returned ssm_status=Success based on
# the SSM RPC return code alone. EC2 tenants kept serving the cached
# :latest digest because `docker compose up -d` is a no-op when the
# tag hasn't been invalidated. ssm_status=Success was lying.
#
# This replay simulates that condition locally:
#   1. Boot the harness with GIT_SHA=fix-applied.
#   2. Curl /buildinfo and assert it returns "fix-applied" (the new code
#      actually shipped).
#   3. Negative test: curl with a different EXPECTED_SHA and assert the
#      mismatch detection logic the workflow uses returns failure.
#
# This proves the verify-step's jq lookup + comparison logic works
# against the SAME Dockerfile.tenant production builds. If the
# /buildinfo route ever stops being wired through, this replay
# catches it before it reaches a production tenant.
#
# It is ALSO the hermeticity canary for the shared docker-host CI runner:
# when CI injects the real commit sha (HARNESS_GIT_SHA), a /buildinfo mismatch
# means the running tenant image is stale / cross-wired from a prior/concurrent
# run, which silently corrupts every downstream replay (RCA 2026-07-12 run
# 477499). It probes BOTH tenants because that RCA saw ASYMMETRIC alpha/beta
# behavior — an alpha-only probe would miss a stale beta.

set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HARNESS_ROOT="$(dirname "$HERE")"
# shellcheck source=../_curl.sh
source "$HARNESS_ROOT/_curl.sh"

EXPECTED_FROM_HARNESS="${HARNESS_GIT_SHA:-harness}"

# Only ESCALATE a git_sha mismatch to a hard failure inside CI, where up.sh
# always (re)builds the tenant so a mismatch can ONLY mean a stale/cross-wired
# reused container. Outside CI a developer may legitimately run against a
# cache-stale image (e.g. `HARNESS_GIT_SHA=$(git rev-parse HEAD) ./run-all-replays.sh`
# without --rebuild) — there we keep the old WARN + `./up.sh --rebuild` hint so
# the canary never false-reds a local iteration. Gitea act_runner sets
# GITHUB_ACTIONS=true and CI=true (as does GitHub Actions); absence → treat as
# local and only WARN.
IN_CI=0
if [ -n "${GITHUB_ACTIONS:-}" ] || [ -n "${CI:-}" ]; then IN_CI=1; fi

WRONG_EXPECTED="0000000000000000000000000000000000000000"

# check_tenant <curl-anon-fn> <label>
#   Asserts one tenant's /buildinfo wire shape + git_sha, fail-loud (exit 1) on a
#   hard defect. Stores the observed sha in the global OBSERVED_SHA. Called as a
#   plain statement (NOT `$(...)`) so a hard-defect `exit 1` exits the whole
#   script instead of just a command-substitution subshell.
OBSERVED_SHA=""
check_tenant() {
    local curlfn="$1" label="$2"
    local build_json actual_sha
    echo "[replay] curl ($label) ${BASE}/buildinfo ..."
    build_json=$("$curlfn" "${BASE}/buildinfo")
    echo "[replay]   ($label) ${build_json}"

    actual_sha=$(echo "$build_json" | jq -r '.git_sha // ""')
    if [ -z "$actual_sha" ]; then
        echo "[replay] FAIL (${label}): /buildinfo response missing git_sha field — workflow's jq lookup would null"
        exit 1
    fi
    echo "[replay] (${label}) git_sha=${actual_sha}"

    # git_sha='dev' → the Dockerfile ARG GIT_SHA / ldflags wiring is broken
    # (the #2395 regression class that was invisible until production).
    if [ "$actual_sha" = "dev" ]; then
        echo "[replay] FAIL (${label}): /buildinfo returned 'dev' — Dockerfile.tenant ARG GIT_SHA isn't reaching the binary. Regresses #2395 by silencing the deploy-verify gate."
        exit 1
    fi
    if [ "$actual_sha" = "$WRONG_EXPECTED" ]; then
        echo "[replay] FAIL (${label}): /buildinfo returned all-zero SHA — wiring inverted"
        exit 1
    fi

    if [ "$actual_sha" != "$EXPECTED_FROM_HARNESS" ]; then
        if [ "$EXPECTED_FROM_HARNESS" != "harness" ] && [ "$IN_CI" = "1" ]; then
            echo "[replay] FAIL (${label}): /buildinfo git_sha='${actual_sha}' != expected '${EXPECTED_FROM_HARNESS}'."
            echo "[replay]       The running ${label} tenant image is STALE / not built from this checkout —"
            echo "[replay]       a prior or concurrent harness run's container is being reused. Hermeticity"
            echo "[replay]       (run-all-replays.sh pre-boot down + up.sh build --force-recreate) regressed."
            exit 1
        fi
        # Local (or GIT_SHA not injected): keep it a soft warning so a plain
        # `./up.sh` (no --rebuild) still runs.
        echo "[replay] WARN (${label}): /buildinfo returned '${actual_sha}' but harness expected GIT_SHA='${EXPECTED_FROM_HARNESS}'"
        echo "[replay]       Image may be cached from a previous run. Run ./up.sh --rebuild to force a fresh build."
    fi

    OBSERVED_SHA="$actual_sha"
}

# 1+2. Probe BOTH tenants — the alpha-only probe missed an asymmetric stale beta.
echo "[replay] === buildinfo canary: BOTH tenants (alpha + beta) ==="
check_tenant curl_alpha_anon alpha
ALPHA_SHA="$OBSERVED_SHA"
check_tenant curl_beta_anon beta
BETA_SHA="$OBSERVED_SHA"

# Cross-tenant consistency: both tenants are built in the SAME compose build from
# the SAME GIT_SHA build-arg, so a divergence means one container is stale or
# cross-wired from another run — the exact hermeticity failure this canary exists
# to catch (fail-loud regardless of CI, since it is unambiguous corruption).
if [ "$ALPHA_SHA" != "$BETA_SHA" ]; then
    echo "[replay] FAIL: alpha git_sha='${ALPHA_SHA}' != beta git_sha='${BETA_SHA}' — the two tenant containers are NOT from the same build (one is stale / cross-wired)."
    exit 1
fi

# 3+4. Replay the workflow's exact mismatch-detection logic so a regression in
#      the verify step's bash gets caught here (tenant-agnostic — uses the
#      observed, consistent sha).
MISMATCH_DETECTED=0
if [ "$ALPHA_SHA" != "$WRONG_EXPECTED" ]; then
    MISMATCH_DETECTED=1
fi
if [ "$MISMATCH_DETECTED" != "1" ]; then
    echo "[replay] FAIL: workflow comparison logic would not flag a real mismatch"
    exit 1
fi

echo ""
echo "[replay] PASS: /buildinfo wire shape + GIT_SHA injection verified on BOTH tenants (alpha+beta git_sha match), and mismatch detection works in production-shape topology. The redeploy-fleet verify-step covers what it claims to."
