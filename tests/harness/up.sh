#!/usr/bin/env bash
# Bring the production-shape harness up.
#
# Usage: ./up.sh [--rebuild]
#
# Always operates in tests/harness/ regardless of where it's invoked
# from — test scripts under tests/harness/replays/ source it via the
# absolute path, so cd-ing first prevents compose-context surprises.

set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$HERE"

REBUILD=false
for arg in "$@"; do
    case "$arg" in
        --rebuild) REBUILD=true ;;
    esac
done

# Generate a per-run encryption key. The tenant runs with
# MOLECULE_ENV=production (intentional, to replay prod-shape bugs), and
# crypto.InitStrict() refuses to boot without SECRETS_ENCRYPTION_KEY.
# Generate fresh so:
#   - No key-shaped string lives in the repo (avoids muscle-memorying a
#     hardcoded value into other places + secret-scanner false positives).
#   - Each harness lifetime gets a unique key, mimicking prod's per-tenant
#     isolation. Persistence across runs isn't required — the harness DB
#     is wiped on every ./down.sh.
# Honor a caller-supplied value if already exported (lets a debug session
# pin a key for reproducibility).
if [ -z "${SECRETS_ENCRYPTION_KEY:-}" ]; then
    SECRETS_ENCRYPTION_KEY=$(openssl rand -base64 32)
    export SECRETS_ENCRYPTION_KEY
fi

if [ "$REBUILD" = true ]; then
    # Full clean rebuild — use when a base image or a dep (not just the Go
    # source) changed. Service names are tenant-alpha / tenant-beta (there is no
    # bare `tenant` service — the old `build ... tenant` here was a latent bug
    # that never fired because CI called ./up.sh WITHOUT --rebuild).
    docker compose -f compose.yml build --no-cache tenant-alpha tenant-beta cp-stub
else
    # Default: a CACHE-ENABLED build so the tenant + cp-stub images ALWAYS
    # match the current checkout. Docker's layer cache busts automatically
    # when the workspace-server source (COPY layer) or the GIT_SHA build-arg
    # changes, so this is a fast near-no-op when nothing changed — but it
    # GUARANTEES we never boot a stale image left by a prior/concurrent run
    # on the shared docker-host CI runner. (RCA 2026-07-12, main run 477499:
    # `up -d` reused a KEEP_UP'd prior run's tenant image, git_sha 054c6167,
    # producing org-swapped TenantGuard + empty /workspaces replay failures.)
    docker compose -f compose.yml build tenant-alpha tenant-beta cp-stub
fi

echo "[harness] starting redis + cp-stub + tenant-alpha + tenant-beta + cf-proxy ..."
# --force-recreate + --remove-orphans: NEVER reuse a container left running by a
# prior/concurrent run under the fixed `harness` compose project on this shared
# runner. Combined with the pre-boot ./down.sh in run-all-replays.sh, this makes
# each run hermetic — the replays always hit THIS checkout's freshly-built tenant.
docker compose -f compose.yml up -d --force-recreate --remove-orphans --wait

# Sudo-free reachability: cf-proxy/nginx routes by Host header to the
# right tenant container (matches production CF tunnel: same URL,
# different Host = different tenant). Replays target loopback :8080
# with a per-tenant Host header. _curl.sh centralises the helper
# functions (curl_alpha_admin, curl_beta_admin, etc.).
echo ""
echo "[harness] up. Multi-tenant topology:"
echo "          tenant-alpha:  Host: harness-tenant-alpha.localhost"
echo "          tenant-beta:   Host: harness-tenant-beta.localhost"
echo "          legacy alias:  Host: harness-tenant.localhost → alpha"
echo ""
echo "          Quick check (no /etc/hosts needed):"
echo "            curl -H 'Host: harness-tenant-alpha.localhost' http://localhost:8080/health"
echo "            curl -H 'Host: harness-tenant-beta.localhost'  http://localhost:8080/health"
echo ""
echo "Next: ./seed.sh   # register parent+child workspaces in BOTH tenants"
