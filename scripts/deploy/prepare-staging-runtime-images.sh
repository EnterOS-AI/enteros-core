#!/usr/bin/env bash
# Pre-E2E readiness for promoted workspace runtime images on the exact daemon
# used by staging's local-Docker provisioner. The control plane owns both input
# projections: /cp/runtimes identifies the pinnable set and the admin pin list
# supplies each registry-resolved immutable image_ref. This consumer never
# rebuilds registry policy or carries a second runtime allowlist.
set -euo pipefail

# Keep inherited xtrace from exposing any credential material below.
case $- in *x*) set +x ;; esac

fail() {
  echo "::error::staging runtime image readiness: $*" >&2
  exit 1
}

require_positive_int() {
  local name="$1" value="$2"
  case "$value" in
    ''|*[!0-9]*|0) fail "$name must be a positive integer (got ${value:-<empty>})" ;;
  esac
}

CP_BASE_URL="${CP_BASE_URL:-https://staging-api.moleculesai.app}"
INFISICAL_BASE="${INFISICAL_BASE:-https://key.moleculesai.app}"
INFISICAL_ENV="${INFISICAL_ENV:-staging}"
READINESS_TIMEOUT="${RUNTIME_IMAGE_READINESS_TIMEOUT_SECONDS:-1200}"
PULL_TIMEOUT="${RUNTIME_IMAGE_PULL_TIMEOUT_SECONDS:-600}"
PULL_ATTEMPTS="${RUNTIME_IMAGE_PULL_ATTEMPTS:-2}"
RETRY_DELAY="${RUNTIME_IMAGE_PULL_RETRY_DELAY_SECONDS:-5}"

for name in CP_BASE_URL INFISICAL_BASE INFISICAL_CLIENT_ID INFISICAL_CLIENT_SECRET INFISICAL_PROJECT_ID; do
  [[ -n "${!name:-}" ]] || fail "$name is required"
done
[[ "$INFISICAL_ENV" == "staging" ]] || fail "INFISICAL_ENV must be exactly staging"
infisical_client_id="$INFISICAL_CLIENT_ID"
infisical_client_secret="$INFISICAL_CLIENT_SECRET"
infisical_project_id="$INFISICAL_PROJECT_ID"
unset INFISICAL_CLIENT_ID INFISICAL_CLIENT_SECRET INFISICAL_PROJECT_ID
# Resolve paths only after exported credentials are gone so even this first
# subprocess cannot inherit the universal-auth identity.
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
[[ "$infisical_project_id" =~ ^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$ ]] \
  || fail "INFISICAL_PROJECT_ID must be a lowercase UUID"
case "$infisical_client_id$infisical_client_secret" in
  *$'\n'*|*$'\r'*|*$'\t'*|*'"'*|*$'\\'*) fail "Infisical client credentials contain invalid JSON control characters" ;;
esac

validate_https_origin() {
  local name="$1" value="${2%/}" origin
  case "$value" in https://*) ;; *) fail "$name must be an https origin" ;; esac
  origin="${value#https://}"
  case "$origin" in
    ''|*'/'*|*'@'*|*'?'*|*'#'*|*[[:space:]]*) fail "$name must be an https origin without path, credentials, query, or fragment" ;;
  esac
  printf '%s' "$value"
}
CP_BASE_URL="$(validate_https_origin CP_BASE_URL "$CP_BASE_URL")"
INFISICAL_BASE="$(validate_https_origin INFISICAL_BASE "$INFISICAL_BASE")"

require_positive_int RUNTIME_IMAGE_READINESS_TIMEOUT_SECONDS "$READINESS_TIMEOUT"
require_positive_int RUNTIME_IMAGE_PULL_TIMEOUT_SECONDS "$PULL_TIMEOUT"
require_positive_int RUNTIME_IMAGE_PULL_ATTEMPTS "$PULL_ATTEMPTS"
case "$RETRY_DELAY" in
  ''|*[!0-9]*) fail "RUNTIME_IMAGE_PULL_RETRY_DELAY_SECONDS must be a non-negative integer" ;;
esac
for command_name in curl jq docker timeout; do
  command -v "$command_name" >/dev/null 2>&1 || fail "required command is unavailable: $command_name"
done

# Source, rather than merely execute, so every pull/inspect below reuses the
# exact endpoint and mTLS pins that the read-only fingerprint validated.
# shellcheck source=./require-local-deploy-daemon.sh
source "$here/require-local-deploy-daemon.sh"

tmp_dir="$(mktemp -d)"
cleanup() { rm -rf "$tmp_dir"; }
trap cleanup EXIT INT TERM
catalog_json="$tmp_dir/runtimes.json"
pins_json="$tmp_dir/pins.json"
plan_tsv="$tmp_dir/plan.tsv"
validated_tsv="$tmp_dir/validated.tsv"

started="$(date +%s)"
deadline=$((started + READINESS_TIMEOUT))

# Universal-auth credentials travel in a JSON request body over stdin, never in
# any child process argv or environment. Validation above makes this builtin
# formatting JSON-safe without handing the client secret to a helper process.
printf -v login_body '{"clientId":"%s","clientSecret":"%s"}' "$infisical_client_id" "$infisical_client_secret"
unset infisical_client_id infisical_client_secret
if ! login_response="$(printf '%s' "$login_body" | curl --silent --show-error --fail-with-body \
  --max-time 30 --user-agent curl/8.4.0 --request POST \
  --header 'Content-Type: application/json' --data-binary @- \
  "$INFISICAL_BASE/api/v1/auth/universal-auth/login")"; then
  fail "Infisical universal-auth login failed"
fi
unset login_body
if ! infisical_token="$(printf '%s' "$login_response" | jq -er '.accessToken | select(type == "string" and length > 0)')"; then
  fail "Infisical universal-auth returned no accessToken"
fi
unset login_response
case "$infisical_token" in
  *$'\n'*|*$'\r'*|*'"'*|*$'\\'*) fail "Infisical access token contains invalid control/config characters" ;;
esac

secret_url="$INFISICAL_BASE/api/v3/secrets/raw/CP_ADMIN_API_TOKEN?workspaceId=$infisical_project_id&environment=staging&secretPath=%2Fshared%2Fcontrolplane-admin"
if ! secret_response="$({
  printf 'silent\nshow-error\nfail-with-body\nmax-time = 30\n'
  printf 'header = "User-Agent: curl/8.4.0"\n'
  printf 'header = "Authorization: Bearer %s"\n' "$infisical_token"
} | curl --config - "$secret_url")"; then
  fail "could not read CP_ADMIN_API_TOKEN from Infisical staging /shared/controlplane-admin"
fi
unset infisical_token
if ! cp_token="$(printf '%s' "$secret_response" | jq -er '.secret.secretValue | select(type == "string" and length > 0)')"; then
  fail "Infisical returned an empty CP_ADMIN_API_TOKEN"
fi
unset secret_response
case "$cp_token" in
  *$'\n'*|*$'\r'*|*'"'*|*$'\\'*) fail "CP admin token contains invalid control/config characters" ;;
esac

{
  printf 'silent\nshow-error\nfail-with-body\nmax-time = 30\n'
  printf 'header = "User-Agent: curl/8.4.0"\n'
} | curl --config - "$CP_BASE_URL/cp/runtimes" > "$catalog_json" \
  || fail "could not read the staging runtime catalog"
{
  printf 'silent\nshow-error\nfail-with-body\nmax-time = 30\n'
  printf 'header = "User-Agent: curl/8.4.0"\n'
  printf 'header = "Authorization: Bearer %s"\n' "$cp_token"
} | curl --config - "$CP_BASE_URL/cp/admin/runtime-image" > "$pins_json" \
  || fail "could not read promoted runtime-image pins from staging"
unset cp_token

# Build and validate the complete CP-owned plan before the first Docker pull.
# Every pinnable runtime must have exactly one global pin and a resolved ref.
if ! jq -nr \
  --slurpfile catalog "$catalog_json" \
  --slurpfile pin_doc "$pins_json" '
    ($catalog[0].runtimes // []) as $all |
    [$all[] | select(.pinnable == true)] as $required |
    ($pin_doc[0].pins // []) as $pins |
    if ($required | length) == 0 then
      error("runtime catalog has no pinnable runtimes")
    else
      $required[] as $runtime |
      [$pins[] | select(.template_name == $runtime.name and .region == "global")] as $matches |
      if ($matches | length) != 1 then
        error("missing one exact global image pin for runtime " + $runtime.name +
              " (found " + (($matches | length) | tostring) + ")")
      else
        $matches[0] as $pin |
        [$runtime.name, $runtime.image, $pin.image_digest, ($pin.image_ref // "")] | @tsv
      end
    end
  ' > "$plan_tsv"; then
  fail "runtime catalog/pin plan is incomplete or invalid; refusing a partial pre-pull"
fi

: > "$validated_tsv"
image_count=0
while IFS=$'\t' read -r runtime image_name digest image_ref; do
  [[ -n "$runtime" && -n "$image_name" && -n "$digest" && -n "$image_ref" ]] || fail "runtime catalog/pin plan contains an empty field"
  [[ "$runtime" =~ ^[a-z0-9_-]+$ ]] || fail "invalid runtime name in readiness plan: $runtime"
  [[ "$image_name" =~ ^[a-z0-9][a-z0-9._-]*$ ]] || fail "invalid runtime image name in readiness plan: $image_name"
  [[ "$digest" =~ ^sha256:[a-f0-9]{64}$ ]] || fail "invalid image_digest for runtime $runtime"
  [[ "$image_ref" =~ ^[A-Za-z0-9][A-Za-z0-9._:-]*(/[A-Za-z0-9._-]+)+@sha256:[a-f0-9]{64}$ ]] \
    || fail "invalid immutable image_ref for runtime $runtime"
  [[ "$image_ref" == */"$image_name"@"$digest" ]] || fail "image_ref for runtime $runtime does not match catalog image + promoted digest"
  printf '%s\t%s\n' "$runtime" "$image_ref" >> "$validated_tsv"
  image_count=$((image_count + 1))
done < "$plan_tsv"
[[ "$image_count" -gt 0 ]] || fail "validated readiness plan is empty"

while IFS=$'\t' read -r runtime image_ref; do
  pulled=0
  attempt=1
  while [[ "$attempt" -le "$PULL_ATTEMPTS" ]]; do
    remaining=$((deadline - $(date +%s)))
    [[ "$remaining" -gt 0 ]] || fail "global ${READINESS_TIMEOUT}s budget exhausted before runtime $runtime became ready"
    attempt_budget="$PULL_TIMEOUT"
    [[ "$attempt_budget" -le "$remaining" ]] || attempt_budget="$remaining"
    echo ">> [runtime-image-readiness] pull runtime=$runtime attempt=$attempt/$PULL_ATTEMPTS budget=${attempt_budget}s ref=$image_ref"
    set +e
    timeout --foreground --signal=TERM --kill-after=10s "${attempt_budget}s" \
      docker "${DOCKER_PIN_ARGS[@]}" pull "$image_ref"
    pull_rc=$?
    set -e
    if [[ "$pull_rc" -eq 0 ]]; then pulled=1; break; fi
    if [[ "$attempt" -lt "$PULL_ATTEMPTS" ]]; then
      echo "::warning::runtime image readiness: pull failed for runtime=$runtime rc=$pull_rc; retrying within the global budget" >&2
      [[ "$RETRY_DELAY" -eq 0 ]] || sleep "$RETRY_DELAY"
    fi
    attempt=$((attempt + 1))
  done
  [[ "$pulled" -eq 1 ]] || fail "docker pull failed for runtime $runtime after $PULL_ATTEMPTS bounded attempt(s): $image_ref"

  remaining=$((deadline - $(date +%s)))
  [[ "$remaining" -gt 0 ]] || fail "global ${READINESS_TIMEOUT}s budget exhausted before RepoDigest verification for runtime $runtime"
  inspect_budget=30
  [[ "$inspect_budget" -le "$remaining" ]] || inspect_budget="$remaining"
  if ! repo_digests="$(timeout --foreground --signal=TERM --kill-after=10s "${inspect_budget}s" \
    docker "${DOCKER_PIN_ARGS[@]}" image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "$image_ref" 2>/dev/null)"; then
    fail "bounded docker image inspect failed after pulling runtime $runtime"
  fi
  grep -Fx -- "$image_ref" <<< "$repo_digests" >/dev/null \
    || fail "exact RepoDigest verification failed for runtime $runtime after pull: expected $image_ref"
  echo ">> [runtime-image-readiness] verified runtime=$runtime exact_repo_digest=$image_ref"
done < "$validated_tsv"

elapsed=$(( $(date +%s) - started ))
echo "runtime image readiness PASS: $image_count exact digest(s) pulled + RepoDigest-verified on local-deploy in ${elapsed}s"
