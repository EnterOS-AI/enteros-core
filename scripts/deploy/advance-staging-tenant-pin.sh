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
#   The Infisical universal-auth creds (CLIENT_ID/SECRET/PROJECT_ID) are ALSO
#   required to write the LOCAL_TENANT_IMAGE boot SSOT below. To advance only the
#   DB pin with a pre-supplied CP_ADMIN_API_TOKEN and no Infisical creds, pass
#   SKIP_SSOT_WRITE=1 (a conscious skip; the CP boot default may then drift).
#
# GITHUB_OUTPUT:
#   old_image old_digest old_git_sha new_image new_digest new_git_sha
set -euo pipefail
export MSYS_NO_PATHCONV=1 MSYS2_ARG_CONV_EXCL='*'

CP_BASE_URL="${CP_BASE_URL:-https://staging-api.moleculesai.app}"
INFISICAL_BASE="${INFISICAL_BASE:-https://key.moleculesai.app}"
INFISICAL_ENV="${INFISICAL_ENV:-staging}"
INFISICAL_PATH="${INFISICAL_PATH:-/shared/controlplane-admin}"   # CP admin token
# The tenant-app image the control plane reads at BOOT (LOCAL_TENANT_IMAGE). This
# is a SECOND, independent SSOT from the runtime_image_pins DB row: the DB pin
# makes FRESH provisions dynamic (no CP restart), but LOCAL_TENANT_IMAGE is the
# default a rebooted / freshly-provisioned CP falls back to. Rolling the pin but
# leaving this stale is EXACTLY how prod broke every fresh org (the pin was fixed
# on running containers, never written back to the boot SSOT — see
# molecule-controlplane/scripts/deploy/local-cp-prod-pin-promote.sh). So this
# script now writes BOTH, on every path, and verifies the write landed.
CP_SSOT_PATH="${CP_SSOT_PATH:-/shared/controlplane}"
SSOT_SECRET_NAME="${SSOT_SECRET_NAME:-LOCAL_TENANT_IMAGE}"
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
  # Jobs carrying this script may run on a different Gitea runner from the
  # upstream image-availability gate. The registry tag is the shared handoff;
  # a previous runner's Docker cache is not. Pull on this runner before
  # inspecting so digest resolution is independent and cannot reuse a stale
  # local tag.
  log "pulling $IMAGE on this runner before digest resolution"
  if ! docker pull "$IMAGE" >/dev/null; then
    echo "FATAL: cannot pull $IMAGE on this runner for digest resolution" >&2
    return 1
  fi
  docker image inspect "$IMAGE" --format '{{.Id}}' 2>/dev/null | head -1
}

DIGEST="$(resolve_digest | tr '[:upper:]' '[:lower:]' | xargs)"
if ! printf '%s' "$DIGEST" | grep -Eq '^sha256:[a-f0-9]{64}$'; then
  echo "FATAL: cannot resolve a sha256 image id/digest for $IMAGE (got '${DIGEST:-<empty>}')" >&2
  exit 1
fi

# One shared Infisical universal-auth access token, used for BOTH the CP-admin
# token read AND the LOCAL_TENANT_IMAGE write below. Fails LOUD on any failure —
# it never returns an empty token as if it succeeded, because a silent auth no-op
# is precisely how the prod pin write went stale without anyone noticing.
#
# It is deliberately ECHO-FREE (emits no ::add-mask:: or any other stdout): the
# caller runs it in the PARENT shell and masks the token itself. Emitting a
# workflow command from here would be captured — and corrupt the value — whenever
# a caller runs it inside a command substitution (e.g. CP_TOKEN="$(fetch_cp_token)").
INFISICAL_ACCESS_TOKEN=""
infisical_login() {
  [ -n "$INFISICAL_ACCESS_TOKEN" ] && return 0
  for v in INFISICAL_CLIENT_ID INFISICAL_CLIENT_SECRET INFISICAL_PROJECT_ID; do
    [ -n "${!v:-}" ] || { echo "FATAL: $v is required for Infisical access (set the universal-auth creds, or pass CP_ADMIN_API_TOKEN with SKIP_SSOT_WRITE=1 to run without Infisical)" >&2; exit 2; }
  done
  INFISICAL_ACCESS_TOKEN="$("${CURL[@]}" -X POST "$INFISICAL_BASE/api/v1/auth/universal-auth/login" \
    -H 'Content-Type: application/json' \
    -d "{\"clientId\":\"$INFISICAL_CLIENT_ID\",\"clientSecret\":\"$INFISICAL_CLIENT_SECRET\"}" \
    | python3 -c 'import sys,json; d=json.load(sys.stdin); sys.stdout.write(d.get("accessToken") or "")')"
  [ -n "$INFISICAL_ACCESS_TOKEN" ] || { echo "FATAL: Infisical auth returned empty accessToken" >&2; exit 1; }
}

# Mask a secret in CI logs. Safe to call in the parent shell (stdout is the job
# log the runner scans for workflow commands) — never inside a substitution.
mask() { [ -n "${GITHUB_ACTIONS:-}${GITEA_ACTIONS:-}" ] && [ -n "${1:-}" ] && echo "::add-mask::$1"; return 0; }

fetch_cp_token() {
  if [ -n "${CP_ADMIN_API_TOKEN:-}" ]; then
    printf '%s' "$CP_ADMIN_API_TOKEN"
    return 0
  fi
  # INFISICAL_ACCESS_TOKEN was set by the up-front login in the parent shell, so
  # this is a cached no-op: it never logs in twice and — being echo-free — never
  # pollutes this command substitution's stdout.
  infisical_login
  enc_path="$(printf '%s' "$INFISICAL_PATH" | sed 's#/#%2F#g')"
  cp_token="$("${CURL[@]}" "$INFISICAL_BASE/api/v3/secrets/raw/CP_ADMIN_API_TOKEN?workspaceId=$INFISICAL_PROJECT_ID&environment=$INFISICAL_ENV&secretPath=$enc_path" \
    -H "Authorization: Bearer $INFISICAL_ACCESS_TOKEN" \
    | python3 -c 'import sys,json; d=json.load(sys.stdin); sys.stdout.write(((d.get("secret") or {}).get("secretValue")) or "")')"
  [ -n "$cp_token" ] || { echo "FATAL: Infisical returned empty CP_ADMIN_API_TOKEN" >&2; exit 1; }
  printf '%s' "$cp_token"
}

# --- Up-front auth: assert + log in BEFORE any mutation, so we never half-apply --
# Infisical creds are needed if EITHER the CP admin token must be fetched (no
# CP_ADMIN_API_TOKEN) OR the LOCAL_TENANT_IMAGE write will run (not skipped). Doing
# the login here, in the parent shell, means: (a) missing creds fail loud BEFORE
# the DB pin is touched (a CP_ADMIN_API_TOKEN-only caller that forgot Infisical
# creds no longer aborts mid-promote — it is told up front to add creds or set
# SKIP_SSOT_WRITE=1); (b) INFISICAL_ACCESS_TOKEN is set ONCE in the parent and
# reused by both the CP-token read and the SSOT write (no double login); and
# (c) it is masked in a context whose stdout is the job log, not a capture.
need_infisical=0
[ -z "${CP_ADMIN_API_TOKEN:-}" ] && need_infisical=1
[ "${SKIP_SSOT_WRITE:-0}" != "1" ] && need_infisical=1
if [ "$need_infisical" = "1" ]; then
  infisical_login
  mask "$INFISICAL_ACCESS_TOKEN"
fi

CP_TOKEN="$(fetch_cp_token)"
mask "$CP_TOKEN"

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

# ---- LOCAL_TENANT_IMAGE (the CP boot-default SSOT) read/write ----
inf_get_raw() {  # <secretName> -> its value, or '' if absent/404 (never errors)
  local name="$1" enc
  enc="$(printf '%s' "$CP_SSOT_PATH" | sed 's#/#%2F#g')"
  { "${CURL[@]}" "$INFISICAL_BASE/api/v3/secrets/raw/$name?workspaceId=$INFISICAL_PROJECT_ID&environment=$INFISICAL_ENV&secretPath=$enc" \
      -H "Authorization: Bearer $INFISICAL_ACCESS_TOKEN" 2>/dev/null || true; } \
    | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
    sys.stdout.write(((d.get("secret") or {}).get("secretValue")) or "")
except Exception:
    pass'
}

write_ssot_pin() {  # reconcile Infisical LOCAL_TENANT_IMAGE to the rolled ref
  local ref="$1" before after body verb
  if [ "${SKIP_SSOT_WRITE:-0}" = "1" ]; then
    log "SKIP_SSOT_WRITE=1 — leaving $SSOT_SECRET_NAME untouched (conscious skip; the CP boot default may drift from the live fleet)"
    return 0
  fi
  infisical_login   # cached no-op (token was set up front); echo-free
  before="$(inf_get_raw "$SSOT_SECRET_NAME")"
  if [ "$before" = "$ref" ]; then
    log "SSOT ok: $SSOT_SECRET_NAME already = $ref ($INFISICAL_ENV $CP_SSOT_PATH)"
    return 0
  fi
  if [ "$DRY_RUN" = "1" ]; then
    log "DRY-RUN: would set $SSOT_SECRET_NAME = $ref (was ${before:-<unset>}; $INFISICAL_ENV $CP_SSOT_PATH)"
    return 0
  fi
  body="$(python3 -c 'import json,sys; print(json.dumps({"workspaceId":sys.argv[1],"environment":sys.argv[2],"secretPath":sys.argv[3],"secretValue":sys.argv[4]}))' \
    "$INFISICAL_PROJECT_ID" "$INFISICAL_ENV" "$CP_SSOT_PATH" "$ref")"
  # Pick the verb from whether the secret already exists (we just read it): an
  # existing key is a PATCH (update), an absent one a POST (create). We deliberately
  # do NOT fall PATCH->POST — a transient PATCH failure on an existing key would then
  # POST and get a false "already exists" 4xx that masks the real error. An HTTP 2xx
  # is not trusted either: the value is read back below and asserted (verify, never
  # assume; a write that REPORTS success without landing is the worst failure mode).
  [ -n "$before" ] && verb=PATCH || verb=POST
  "${CURL[@]}" -X "$verb" "$INFISICAL_BASE/api/v3/secrets/raw/$SSOT_SECRET_NAME" \
    -H "Authorization: Bearer $INFISICAL_ACCESS_TOKEN" -H 'Content-Type: application/json' \
    -d "$body" >/dev/null \
    || { echo "FATAL: $verb of $SSOT_SECRET_NAME to Infisical $INFISICAL_ENV $CP_SSOT_PATH failed" >&2; exit 1; }
  after="$(inf_get_raw "$SSOT_SECRET_NAME")"
  if [ "$after" != "$ref" ]; then
    echo "FATAL: $SSOT_SECRET_NAME write did not take — is '${after:-<empty>}', expected '$ref' ($INFISICAL_ENV $CP_SSOT_PATH)" >&2
    exit 1
  fi
  log "SSOT written + verified: $SSOT_SECRET_NAME ${before:-<unset>} -> $ref ($INFISICAL_ENV $CP_SSOT_PATH)"
}

# The ref recorded as LOCAL_TENANT_IMAGE is exactly the image the fleet rolled to.
# IMAGE is always a fully-qualified, pullable reference — `<name>:<tag>` (from
# --tag or --image name:tag) or `<name>@sha256:<digest>` (from --image name@digest)
# — so it is recorded verbatim. We deliberately do NOT reconstruct a `:staging-<sha>`
# tag from a digest+git-sha: that would guess a prefix the real tag may not use
# (e.g. a `main-<sha>` build), writing an unpullable ref and re-breaking fresh-org
# boot — the very drift this script exists to prevent.
SSOT_REF="$IMAGE"

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
  # DB pin is already at target, but the boot-default SSOT drifts INDEPENDENTLY
  # (it is exactly the state staging is in now: pin current, LOCAL_TENANT_IMAGE
  # stale). Reconcile it here too — write_ssot_pin no-ops if already correct.
  log "DB pin already at target — reconciling the CP boot-default SSOT"
  write_ssot_pin "$SSOT_REF"
  echo "OLD_IMAGE=${OLD_IMAGE}"; echo "NEW_IMAGE=${IMAGE}"
  exit 0
fi

if [ "$DRY_RUN" = "1" ]; then
  log "DRY-RUN: would promote $TEMPLATE_NAME ${OLD_DIGEST:-<unset>} -> $DIGEST"
  log "DRY-RUN: would set $SSOT_SECRET_NAME = $SSOT_REF ($INFISICAL_ENV $CP_SSOT_PATH)"
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
# Write the SECOND SSOT (the CP boot default) to match. Both must move together;
# a DB-pin roll without this is the exact drift that broke prod fresh-org signup.
write_ssot_pin "$SSOT_REF"
echo "OLD_IMAGE=${OLD_IMAGE}"; echo "NEW_IMAGE=${IMAGE}"
