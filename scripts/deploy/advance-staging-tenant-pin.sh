#!/usr/bin/env bash
# advance-staging-tenant-pin.sh — promote the STAGING tenant-app image pin.
#
# The local-docker control plane consumes the tenant-app desired state from the
# runtime_image_pins row template_name='molecule-tenant'. Promoting that row via
# the CP admin API makes fresh provisions dynamic: no CP restart is needed for a
# newly-created tenant to use the candidate image.
#
# Usage:
#   advance-staging-tenant-pin.sh --tag staging-<sha>
#   advance-staging-tenant-pin.sh --image registry.../molecule-tenant:staging-<sha>
#   advance-staging-tenant-pin.sh --image registry.../molecule-tenant@sha256:<digest> --git-sha <sha>
#   advance-staging-tenant-pin.sh --tag staging-<sha> --dry-run
#
# Required auth:
#   CP_ADMIN_API_TOKEN, or INFISICAL_CLIENT_ID / INFISICAL_CLIENT_SECRET /
#   INFISICAL_PROJECT_ID so the script can fetch CP_ADMIN_API_TOKEN from
#   staging /shared/controlplane-admin.
#
# GITHUB_OUTPUT:
#   old_image old_digest old_git_sha new_image new_digest new_git_sha
set -euo pipefail
export MSYS_NO_PATHCONV=1 MSYS2_ARG_CONV_EXCL='*'

CP_BASE_URL="${CP_BASE_URL:-https://staging-api.moleculesai.app}"
INFISICAL_BASE="${INFISICAL_BASE:-https://key.moleculesai.app}"
INFISICAL_ENV="${INFISICAL_ENV:-staging}"
INFISICAL_PATH="${INFISICAL_PATH:-/shared/controlplane-admin}"
TENANT_IMAGE_NAME="${TENANT_IMAGE_NAME:-registry.moleculesai.app/molecule-ai/molecule-tenant}"
TEMPLATE_NAME="molecule-tenant"
IMAGE=""
TAG=""
GIT_SHA="${TENANT_PIN_GIT_SHA:-}"
DRY_RUN=0
CURL=(curl -fsS -A curl/8.4.0 --doh-url https://cloudflare-dns.com/dns-query)

usage() { sed -n '2,25p' "$0" | sed 's/^# \{0,1\}//'; exit "${1:-0}"; }
while [ "$#" -gt 0 ]; do
  case "$1" in
    --image) IMAGE="$2"; shift 2;;
    --tag) TAG="$2"; shift 2;;
    --git-sha) GIT_SHA="$2"; shift 2;;
    --dry-run) DRY_RUN=1; shift;;
    -h|--help) usage 0;;
    *) echo "unknown arg: $1" >&2; usage 2;;
  esac
done

log() { printf '>> [tenant-pin] %s\n' "$*" >&2; }

[ -n "$IMAGE" ] || { [ -n "$TAG" ] && IMAGE="${TENANT_IMAGE_NAME}:${TAG}"; }
[ -n "$IMAGE" ] || { echo "FATAL: one of --image / --tag is required" >&2; usage 2; }

if [ -z "$GIT_SHA" ] && [ -n "${GITHUB_SHA:-}" ] && [ -n "$TAG" ]; then
  short="${GITHUB_SHA:0:7}"
  if [ "$TAG" = "staging-$short" ] || [ "$TAG" = "main-$short" ]; then
    GIT_SHA="$GITHUB_SHA"
  fi
fi
if [ -z "$GIT_SHA" ] && [ -n "$TAG" ]; then
  case "$TAG" in
    staging-[0-9a-fA-F]*|main-[0-9a-fA-F]*) GIT_SHA="${TAG#staging-}"; GIT_SHA="${GIT_SHA#main-}" ;;
  esac
fi

resolve_digest() {
  case "$IMAGE" in
    *@sha256:*) printf '%s\n' "${IMAGE##*@}"; return 0;;
  esac
  docker image inspect "$IMAGE" --format '{{.Id}}' 2>/dev/null | head -1 || true
}

DIGEST="$(resolve_digest | tr '[:upper:]' '[:lower:]' | xargs)"
if ! printf '%s' "$DIGEST" | grep -Eq '^sha256:[a-f0-9]{64}$'; then
  echo "FATAL: cannot resolve a sha256 image id/digest for $IMAGE (got '${DIGEST:-<empty>}')" >&2
  exit 1
fi

fetch_cp_token() {
  if [ -n "${CP_ADMIN_API_TOKEN:-}" ]; then
    printf '%s' "$CP_ADMIN_API_TOKEN"
    return 0
  fi
  for v in INFISICAL_CLIENT_ID INFISICAL_CLIENT_SECRET INFISICAL_PROJECT_ID; do
    [ -n "${!v:-}" ] || { echo "FATAL: $v is required when CP_ADMIN_API_TOKEN is unset" >&2; exit 2; }
  done
  enc_path="$(printf '%s' "$INFISICAL_PATH" | sed 's#/#%2F#g')"
  tok="$("${CURL[@]}" -X POST "$INFISICAL_BASE/api/v1/auth/universal-auth/login" \
    -H 'Content-Type: application/json' \
    -d "{\"clientId\":\"$INFISICAL_CLIENT_ID\",\"clientSecret\":\"$INFISICAL_CLIENT_SECRET\"}" \
    | python3 -c 'import sys,json; d=json.load(sys.stdin); sys.stdout.write(d.get("accessToken") or "")')"
  [ -n "$tok" ] || { echo "FATAL: Infisical auth returned empty accessToken" >&2; exit 1; }
  cp_token="$("${CURL[@]}" "$INFISICAL_BASE/api/v3/secrets/raw/CP_ADMIN_API_TOKEN?workspaceId=$INFISICAL_PROJECT_ID&environment=$INFISICAL_ENV&secretPath=$enc_path" \
    -H "Authorization: Bearer $tok" \
    | python3 -c 'import sys,json; d=json.load(sys.stdin); sys.stdout.write(((d.get("secret") or {}).get("secretValue")) or "")')"
  [ -n "$cp_token" ] || { echo "FATAL: Infisical returned empty CP_ADMIN_API_TOKEN" >&2; exit 1; }
  printf '%s' "$cp_token"
}

CP_TOKEN="$(fetch_cp_token)"
if [ -n "${GITHUB_ACTIONS:-}${GITEA_ACTIONS:-}" ]; then
  echo "::add-mask::$CP_TOKEN"
fi

cp_json() {
  local method="$1" path="$2" body="${3:-}"
  if [ -n "$body" ]; then
    "${CURL[@]}" -X "$method" "${CP_BASE_URL%/}$path" \
      -H "Authorization: Bearer $CP_TOKEN" \
      -H "Content-Type: application/json" \
      -d "$body"
  else
    "${CURL[@]}" -X "$method" "${CP_BASE_URL%/}$path" \
      -H "Authorization: Bearer $CP_TOKEN"
  fi
}

pins="$(cp_json GET /cp/admin/runtime-image)"
read -r OLD_DIGEST OLD_GIT_SHA < <(printf '%s' "$pins" | python3 -c '
import json, sys
d = json.load(sys.stdin)
for p in d.get("pins", []):
    if p.get("template_name") == "molecule-tenant" and p.get("region", "global") == "global":
        print((p.get("image_digest") or "") + "\t" + (p.get("git_sha") or ""))
        break
else:
    print("\t")
')

old_ref_for() {
  local digest="$1" git_sha="$2"
  if [ -n "$digest" ] && [ -n "$git_sha" ]; then
    printf '%s:staging-%s\n' "$TENANT_IMAGE_NAME" "${git_sha:0:7}"
  elif [ -n "$digest" ]; then
    printf '%s@%s\n' "$TENANT_IMAGE_NAME" "$digest"
  fi
}

OLD_IMAGE="$(old_ref_for "$OLD_DIGEST" "$OLD_GIT_SHA")"
log "current pin digest=${OLD_DIGEST:-<unset>} git_sha=${OLD_GIT_SHA:-<unset>} ref=${OLD_IMAGE:-<unset>}"
log "target  pin digest=$DIGEST git_sha=${GIT_SHA:-<unset>} ref=$IMAGE"

if [ -n "${GITHUB_OUTPUT:-}" ]; then
  {
    echo "old_image=${OLD_IMAGE}"
    echo "old_digest=${OLD_DIGEST}"
    echo "old_git_sha=${OLD_GIT_SHA}"
    echo "new_image=${IMAGE}"
    echo "new_digest=${DIGEST}"
    echo "new_git_sha=${GIT_SHA}"
  } >> "$GITHUB_OUTPUT"
fi

if [ "$OLD_DIGEST" = "$DIGEST" ] && [ "${OLD_GIT_SHA:-}" = "${GIT_SHA:-}" ]; then
  log "pin already at target — no write needed"
  echo "OLD_IMAGE=${OLD_IMAGE}"; echo "NEW_IMAGE=${IMAGE}"
  exit 0
fi

if [ "$DRY_RUN" = "1" ]; then
  log "DRY-RUN: would promote $TEMPLATE_NAME ${OLD_DIGEST:-<unset>} -> $DIGEST"
  echo "OLD_IMAGE=${OLD_IMAGE}"; echo "NEW_IMAGE=${IMAGE}"
  exit 0
fi

body="$(python3 -c 'import json,sys; print(json.dumps({"template_name":"molecule-tenant","image_digest":sys.argv[1],"git_sha":sys.argv[2],"notes":sys.argv[3]}))' \
  "$DIGEST" "$GIT_SHA" "staging tenant image ${IMAGE}")"
cp_json POST /cp/admin/runtime-image/promote "$body" >/dev/null

after="$(cp_json GET /cp/admin/runtime-image)"
now_digest="$(printf '%s' "$after" | python3 -c '
import json, sys
d=json.load(sys.stdin)
for p in d.get("pins", []):
    if p.get("template_name") == "molecule-tenant" and p.get("region", "global") == "global":
        print(p.get("image_digest") or "")
        break
')"
if [ "$now_digest" != "$DIGEST" ]; then
  echo "FATAL: CP pin write did not take — $TEMPLATE_NAME digest is '${now_digest:-<empty>}', expected '$DIGEST'" >&2
  exit 1
fi
log "pin promoted via CP admin API: $TEMPLATE_NAME ${OLD_DIGEST:-<unset>} -> $DIGEST"
echo "OLD_IMAGE=${OLD_IMAGE}"; echo "NEW_IMAGE=${IMAGE}"
