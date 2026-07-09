#!/usr/bin/env bash
# reload-staging-controlplane-railway.sh — legacy Railway provider adapter.
#
# This script is intentionally isolated under scripts/deploy/providers/. The
# required tenant-image CI path is provider-agnostic and must not call this
# adapter directly.
#
# For a Railway-hosted staging control plane, project a STAGING tenant image into
# Railway and recreate the staging control plane so fresh provisions read it.
#
# Durable pin movement belongs to advance-staging-tenant-pin.sh (Infisical
# /shared/controlplane::LOCAL_TENANT_IMAGE). This helper projects that chosen
# image into the running Railway staging environment and redeploys
# molecule-cp-staging so its in-memory TenantImage matches.
#
# Usage:
#   reload-staging-controlplane-railway.sh --image registry.../molecule-tenant:staging-<sha>
#   reload-staging-controlplane-railway.sh --tag staging-<sha>
#   reload-staging-controlplane-railway.sh --image ... --dry-run
#
# Required env:
#   INFISICAL_CLIENT_ID  INFISICAL_CLIENT_SECRET  INFISICAL_PROJECT_ID
# Optional:
#   INFISICAL_BASE       (default https://key.moleculesai.app)
#   TENANT_IMAGE_NAME    (bare image name for --tag)
#   STAGING_CP_BASE_URL  (default https://staging-api.moleculesai.app)
set -euo pipefail
export MSYS_NO_PATHCONV=1 MSYS2_ARG_CONV_EXCL='*'

INFISICAL_BASE="${INFISICAL_BASE:-https://key.moleculesai.app}"
TENANT_IMAGE_NAME="${TENANT_IMAGE_NAME:-registry.moleculesai.app/molecule-ai/molecule-tenant}"
STAGING_CP_BASE_URL="${STAGING_CP_BASE_URL:-https://staging-api.moleculesai.app}"
DEPLOY_SECRET_ENV="${DEPLOY_SECRET_ENV:-prod}"
DEPLOY_SECRET_PATH="${DEPLOY_SECRET_PATH:-/shared/railway-deploy}"
KEY="LOCAL_TENANT_IMAGE"
IMAGE="" ; TAG="" ; DRY_RUN=0

usage() { sed -n '2,25p' "$0" | sed 's/^# \{0,1\}//'; exit "${1:-0}"; }
while [ "$#" -gt 0 ]; do
  case "$1" in
    --image)   IMAGE="$2"; shift 2;;
    --tag)     TAG="$2"; shift 2;;
    --dry-run) DRY_RUN=1; shift;;
    -h|--help) usage 0;;
    *) echo "unknown arg: $1" >&2; usage 2;;
  esac
done
log() { printf '>> [cp-reload] %s\n' "$*" >&2; }

[ -n "$IMAGE" ] || { [ -n "$TAG" ] && IMAGE="${TENANT_IMAGE_NAME}:${TAG}"; }
[ -n "$IMAGE" ] || { echo "FATAL: one of --image / --tag is required" >&2; usage 2; }

if [ "$DRY_RUN" = "1" ]; then
  log "DRY-RUN: would set Railway staging $KEY='$IMAGE' and redeploy staging CP"
  echo "TARGET_IMAGE=${IMAGE}"
  exit 0
fi

for v in INFISICAL_CLIENT_ID INFISICAL_CLIENT_SECRET INFISICAL_PROJECT_ID; do
  [ -n "${!v:-}" ] || { echo "FATAL: $v is required (Infisical machine identity)" >&2; exit 2; }
done
command -v railway >/dev/null 2>&1 || { echo "FATAL: railway CLI not found" >&2; exit 2; }
command -v jq >/dev/null 2>&1 || { echo "FATAL: jq not found" >&2; exit 2; }

CURL=(curl -fsS --doh-url https://cloudflare-dns.com/dns-query)
enc_path="$(printf '%s' "$DEPLOY_SECRET_PATH" | sed 's#/#%2F#g')"

log "authenticating to Infisical ($INFISICAL_BASE) as machine identity"
TOKEN="$("${CURL[@]}" -X POST "$INFISICAL_BASE/api/v1/auth/universal-auth/login" \
  -H 'Content-Type: application/json' \
  -d "{\"clientId\":\"$INFISICAL_CLIENT_ID\",\"clientSecret\":\"$INFISICAL_CLIENT_SECRET\"}" \
  | jq -r '.accessToken // empty')"
[ -n "$TOKEN" ] || { echo "FATAL: Infisical auth returned empty accessToken" >&2; exit 1; }

secrets_json="$("${CURL[@]}" \
  "$INFISICAL_BASE/api/v3/secrets/raw?workspaceId=$INFISICAL_PROJECT_ID&environment=$DEPLOY_SECRET_ENV&secretPath=$enc_path" \
  -H "Authorization: Bearer $TOKEN")"
get_secret() {
  jq -r --arg k "$1" '.secrets[]? | select(.secretKey==$k) | .secretValue // empty' <<<"$secrets_json"
}

railway_token="$(get_secret RAILWAY_TOKEN_STAGING)"
project_id="$(get_secret RAILWAY_PROJECT_ID)"
env_id="$(get_secret RAILWAY_ENV_ID_STAGING)"
service_id="$(get_secret RAILWAY_SERVICE_ID_CONTROLPLANE)"
if [ "${GITHUB_ACTIONS:-}" = "true" ] || [ "${GITEA_ACTIONS:-}" = "true" ]; then
  echo "::add-mask::$railway_token"
fi
for pair in \
  "RAILWAY_TOKEN_STAGING:$railway_token" \
  "RAILWAY_PROJECT_ID:$project_id" \
  "RAILWAY_ENV_ID_STAGING:$env_id" \
  "RAILWAY_SERVICE_ID_CONTROLPLANE:$service_id"; do
  name="${pair%%:*}"
  value="${pair#*:}"
  [ -n "$value" ] || { echo "FATAL: $name missing from Infisical $DEPLOY_SECRET_ENV:$DEPLOY_SECRET_PATH" >&2; exit 1; }
done

tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT
mkdir -p "$tmpdir/.railway"
jq -n \
  --arg projectId "$project_id" \
  --arg environmentId "$env_id" \
  --arg serviceId "$service_id" \
  '{projectId:$projectId,environmentId:$environmentId,serviceId:$serviceId}' \
  > "$tmpdir/.railway/config.json"

log "projecting $KEY='$IMAGE' into Railway staging (skip-deploys)"
(
  cd "$tmpdir"
  export RAILWAY_TOKEN="$railway_token"
  export RAILWAY_PROJECT_TOKEN="$railway_token"
  railway variables \
    --service "$service_id" \
    --environment staging \
    --set "$KEY=$IMAGE" \
    --skip-deploys >/dev/null
  log "redeploying staging control plane service $service_id"
  railway redeploy --service "$service_id" --yes >/dev/null
)

log "waiting for staging CP health and router surface"
deadline=$(( $(date +%s) + 600 ))
while [ "$(date +%s)" -lt "$deadline" ]; do
  health=$(curl -sS -o /dev/null -w "%{http_code}" --max-time 10 \
    "${STAGING_CP_BASE_URL%/}/health" || echo "000")
  route=$(curl -sS -o /dev/null -w "%{http_code}" --max-time 10 \
    -H "Content-Type: application/json" \
    -X POST "${STAGING_CP_BASE_URL%/}/api/v1/internal/llm/anthropic/v1/messages" \
    -d '{}' || echo "000")
  if [ "$health" = "200" ] && [ "$route" = "401" ]; then
    log "staging CP ready with $KEY='$IMAGE'"
    echo "TARGET_IMAGE=${IMAGE}"
    exit 0
  fi
  log "staging /health: HTTP $health; anthropic route: HTTP $route (retrying)"
  sleep 15
done

echo "FATAL: staging CP did not become ready after redeploy" >&2
exit 1
