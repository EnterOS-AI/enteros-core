#!/usr/bin/env bash
# advance-staging-tenant-pin.sh — advance the STAGING tenant-image pin so new
# staging provisions boot the freshly published image.
#
# The control plane reads LOCAL_TENANT_IMAGE from Infisical (/shared/controlplane,
# environment=staging) at boot and uses it as the default image for NEW
# local-docker staging provisions (controlplane cmd/server/main.go ->
# LocalDocker.TenantImage). Nothing auto-advances it, so a fresh tenant image
# never becomes the default until this pin is moved. This script moves it.
#
# It writes ONLY the durable Infisical value (idempotent). It does NOT restart
# molecule-cp-staging — the running CP keeps its in-memory pin until its OWN
# deploy pipeline recreates it (avoiding a cross-workflow race with the CP's
# `staging-deploy-e2e` concurrency group). Already-running tenants are advanced
# separately + immediately by redeploy-staging-fleet.sh; this pin governs the
# image FUTURE fresh provisions adopt on the next CP restart.
#
# The OLD value is printed (and emitted to $GITHUB_OUTPUT as old_image) so a
# caller can revert on a downstream gate failure.
#
# Usage:
#   advance-staging-tenant-pin.sh --image molecule-tenant:staging-<sha>
#   advance-staging-tenant-pin.sh --tag staging-<sha>          # -> TENANT_IMAGE_NAME:tag
#   advance-staging-tenant-pin.sh --image ... --dry-run        # read + plan only
#
# Required env (Infisical machine identity, staging project):
#   INFISICAL_CLIENT_ID  INFISICAL_CLIENT_SECRET  INFISICAL_PROJECT_ID
# Optional:
#   INFISICAL_BASE       (default https://key.moleculesai.app)
#   INFISICAL_ENV        (default staging)
#   INFISICAL_PATH       (default /shared/controlplane)
#   TENANT_IMAGE_NAME    (bare image name for --tag; default molecule-tenant)
#
# SAFETY: writes exactly one Infisical secret; prints no credential; --dry-run
# performs zero writes. STAGING scoped (environment=staging).
set -euo pipefail
export MSYS_NO_PATHCONV=1 MSYS2_ARG_CONV_EXCL='*'

INFISICAL_BASE="${INFISICAL_BASE:-https://key.moleculesai.app}"
INFISICAL_ENV="${INFISICAL_ENV:-staging}"
INFISICAL_PATH="${INFISICAL_PATH:-/shared/controlplane}"
TENANT_IMAGE_NAME="${TENANT_IMAGE_NAME:-molecule-tenant}"
KEY="LOCAL_TENANT_IMAGE"
IMAGE="" ; TAG="" ; DRY_RUN=0

usage() { sed -n '2,33p' "$0" | sed 's/^# \{0,1\}//'; exit "${1:-0}"; }
while [ "$#" -gt 0 ]; do
  case "$1" in
    --image)   IMAGE="$2"; shift 2;;
    --tag)     TAG="$2";   shift 2;;
    --dry-run) DRY_RUN=1; shift;;
    -h|--help) usage 0;;
    *) echo "unknown arg: $1" >&2; usage 2;;
  esac
done
log() { printf '>> [pin] %s\n' "$*" >&2; }

[ -n "$IMAGE" ] || { [ -n "$TAG" ] && IMAGE="${TENANT_IMAGE_NAME}:${TAG}"; }
[ -n "$IMAGE" ] || { echo "FATAL: one of --image / --tag is required" >&2; usage 2; }

for v in INFISICAL_CLIENT_ID INFISICAL_CLIENT_SECRET INFISICAL_PROJECT_ID; do
  [ -n "${!v:-}" ] || { echo "FATAL: $v is required (Infisical machine identity)" >&2; exit 2; }
done

# curl through DoH so *.moleculesai.app resolves even when the local resolver
# hijacks :53 (see memory infisical-ssot-audit). Harmless where DNS is clean.
CURL=(curl -fsS --doh-url https://cloudflare-dns.com/dns-query)

# encode the secret path for the query string (/ -> %2F).
enc_path="$(printf '%s' "$INFISICAL_PATH" | sed 's#/#%2F#g')"

log "authenticating to Infisical ($INFISICAL_BASE) as machine identity"
TOKEN="$("${CURL[@]}" -X POST "$INFISICAL_BASE/api/v1/auth/universal-auth/login" \
  -H 'Content-Type: application/json' \
  -d "{\"clientId\":\"$INFISICAL_CLIENT_ID\",\"clientSecret\":\"$INFISICAL_CLIENT_SECRET\"}" \
  | python3 -c 'import sys,json; v=(json.load(sys.stdin) or {}).get("accessToken"); sys.stdout.write(v if isinstance(v,str) else "")')"
[ -n "$TOKEN" ] || { echo "FATAL: Infisical auth returned empty accessToken" >&2; exit 1; }

read_secret() {
  "${CURL[@]}" "$INFISICAL_BASE/api/v3/secrets/raw/$KEY?workspaceId=$INFISICAL_PROJECT_ID&environment=$INFISICAL_ENV&secretPath=$enc_path" \
    -H "Authorization: Bearer $TOKEN" 2>/dev/null \
    | python3 -c 'import sys,json;
d=json.load(sys.stdin);
s=d.get("secret") if isinstance(d,dict) else None;
sys.stdout.write((s or {}).get("secretValue","") if s else "")'
}

OLD="$(read_secret || true)"
log "current pin ($INFISICAL_ENV $INFISICAL_PATH::$KEY) = '${OLD:-<unset>}'"
log "target  pin = '$IMAGE'"

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  { echo "old_image=${OLD}"; echo "new_image=${IMAGE}"; } >> "$GITHUB_OUTPUT"
fi

if [ "$OLD" = "$IMAGE" ]; then
  log "pin already at target — no write needed"
  echo "OLD_IMAGE=${OLD}"; echo "NEW_IMAGE=${IMAGE}"
  exit 0
fi

if [ "$DRY_RUN" = "1" ]; then
  log "DRY-RUN: would advance $KEY '${OLD:-<unset>}' -> '$IMAGE' (no write performed)"
  echo "OLD_IMAGE=${OLD}"; echo "NEW_IMAGE=${IMAGE}"
  exit 0
fi

# Update if present, else create. Infisical raw-secret PATCH/POST take a JSON body.
body="$(python3 -c 'import json,sys; print(json.dumps({"environment":sys.argv[1],"secretPath":sys.argv[2],"secretValue":sys.argv[3],"workspaceId":sys.argv[4]}))' \
  "$INFISICAL_ENV" "$INFISICAL_PATH" "$IMAGE" "$INFISICAL_PROJECT_ID")"

if [ -n "$OLD" ]; then
  method=PATCH
else
  method=POST
fi
log "writing pin via $method"
resp="$("${CURL[@]}" -X "$method" "$INFISICAL_BASE/api/v3/secrets/raw/$KEY" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d "$body" 2>&1)" || { echo "FATAL: Infisical write failed: $resp" >&2; exit 1; }

# Verify end-state (not exit-code trust): re-read and confirm.
NOW="$(read_secret || true)"
if [ "$NOW" != "$IMAGE" ]; then
  echo "FATAL: pin write did not take — $KEY is '$NOW', expected '$IMAGE'" >&2
  exit 1
fi
log "pin advanced: $KEY '${OLD:-<unset>}' -> '$IMAGE' (verified)"
echo "OLD_IMAGE=${OLD}"; echo "NEW_IMAGE=${IMAGE}"
