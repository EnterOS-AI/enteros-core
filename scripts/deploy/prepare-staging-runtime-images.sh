#!/usr/bin/env bash
# Pre-E2E readiness for promoted workspace runtime images: the registry must
# serve every pinnable runtime's promoted immutable image_ref, with manifest
# bytes that hash to the promoted digest. Staging workspaces are k8s pods whose
# kubelets pull from that registry, so an unserved digest is a provision that
# fails part-way through the E2E instead of here. The control plane owns both
# input projections: /cp/runtimes identifies the pinnable set and the admin pin
# list supplies each registry-resolved immutable image_ref. This consumer never
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
READINESS_TIMEOUT="${RUNTIME_IMAGE_READINESS_TIMEOUT_SECONDS:-600}"
REQUEST_TIMEOUT="${RUNTIME_IMAGE_REQUEST_TIMEOUT_SECONDS:-30}"
REQUEST_ATTEMPTS="${RUNTIME_IMAGE_REQUEST_ATTEMPTS:-3}"
RETRY_DELAY="${RUNTIME_IMAGE_REQUEST_RETRY_DELAY_SECONDS:-5}"
# The registry credential is sent only to this host. A CP-projected image_ref
# naming any other host fails the plan before the first registry request.
REGISTRY_HOST="${RUNTIME_IMAGE_REGISTRY_HOST:-registry.moleculesai.app}"
MANIFEST_ACCEPT="application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json"
MAX_MANIFEST_BYTES=5242880

for name in CP_BASE_URL INFISICAL_BASE INFISICAL_CLIENT_ID INFISICAL_CLIENT_SECRET INFISICAL_PROJECT_ID REG_USER REG_TOKEN; do
  [[ -n "${!name:-}" ]] || fail "$name is required"
done
[[ "$INFISICAL_ENV" == "staging" ]] || fail "INFISICAL_ENV must be exactly staging"
infisical_client_id="$INFISICAL_CLIENT_ID"
infisical_client_secret="$INFISICAL_CLIENT_SECRET"
infisical_project_id="$INFISICAL_PROJECT_ID"
reg_user="$REG_USER"
reg_token="$REG_TOKEN"
unset INFISICAL_CLIENT_ID INFISICAL_CLIENT_SECRET INFISICAL_PROJECT_ID REG_USER REG_TOKEN
[[ "$infisical_project_id" =~ ^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$ ]] \
  || fail "INFISICAL_PROJECT_ID must be a lowercase UUID"
case "$infisical_client_id$infisical_client_secret" in
  *$'\n'*|*$'\r'*|*$'\t'*|*'"'*|*$'\\'*) fail "Infisical client credentials contain invalid JSON control characters" ;;
esac
case "$reg_user$reg_token" in
  *$'\n'*|*$'\r'*|*$'\t'*|*'"'*|*$'\\'*) fail "registry credentials contain invalid control/config characters" ;;
esac
case "$reg_user" in *:*) fail "REG_USER must not contain ':'" ;; esac
[[ "$REGISTRY_HOST" =~ ^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$ ]] || fail "RUNTIME_IMAGE_REGISTRY_HOST must be a bare host name"

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
require_positive_int RUNTIME_IMAGE_REQUEST_TIMEOUT_SECONDS "$REQUEST_TIMEOUT"
require_positive_int RUNTIME_IMAGE_REQUEST_ATTEMPTS "$REQUEST_ATTEMPTS"
case "$RETRY_DELAY" in
  ''|*[!0-9]*) fail "RUNTIME_IMAGE_REQUEST_RETRY_DELAY_SECONDS must be a non-negative integer" ;;
esac
for command_name in curl node; do
  command -v "$command_name" >/dev/null 2>&1 || fail "required command is unavailable: $command_name"
done

# Parse one required, non-empty JSON string from stdin without echoing the
# credential-bearing document on failure. Node is part of the reviewed
# local-deploy runner contract because actions/checkout requires it; jq is not
# installed on that host executor.
json_required_string() {
  local field_path="$1"
  node -e '
    "use strict";
    try {
      let value = JSON.parse(require("fs").readFileSync(0, "utf8"));
      for (const key of process.argv[1].split(".")) value = value?.[key];
      if (typeof value !== "string" || value.length === 0) process.exit(1);
      process.stdout.write(value);
    } catch (_) {
      process.exit(1);
    }
  ' "$field_path"
}

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
if ! login_response="$(printf '%s' "$login_body" | curl --disable --silent --show-error --fail-with-body \
  --max-time 30 --user-agent curl/8.4.0 --request POST \
  --header 'Content-Type: application/json' --data-binary @- \
  "$INFISICAL_BASE/api/v1/auth/universal-auth/login")"; then
  fail "Infisical universal-auth login failed"
fi
unset login_body
if ! infisical_token="$(printf '%s' "$login_response" | json_required_string accessToken)"; then
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
} | curl --disable --config - "$secret_url")"; then
  fail "could not read CP_ADMIN_API_TOKEN from Infisical staging /shared/controlplane-admin"
fi
unset infisical_token
if ! cp_token="$(printf '%s' "$secret_response" | json_required_string secret.secretValue)"; then
  fail "Infisical returned an empty CP_ADMIN_API_TOKEN"
fi
unset secret_response
case "$cp_token" in
  *$'\n'*|*$'\r'*|*'"'*|*$'\\'*) fail "CP admin token contains invalid control/config characters" ;;
esac

{
  printf 'silent\nshow-error\nfail-with-body\nmax-time = 30\n'
  printf 'header = "User-Agent: curl/8.4.0"\n'
} | curl --disable --config - "$CP_BASE_URL/cp/runtimes" > "$catalog_json" \
  || fail "could not read the staging runtime catalog"
{
  printf 'silent\nshow-error\nfail-with-body\nmax-time = 30\n'
  printf 'header = "User-Agent: curl/8.4.0"\n'
  printf 'header = "Authorization: Bearer %s"\n' "$cp_token"
} | curl --disable --config - "$CP_BASE_URL/cp/admin/runtime-image" > "$pins_json" \
  || fail "could not read promoted runtime-image pins from staging"
unset cp_token

# Build and validate the complete CP-owned plan before the first registry request.
# Every pinnable runtime must have exactly one global pin and a resolved ref.
# shellcheck disable=SC2016 # Node template literals must not expand in Bash.
if ! node -e '
  "use strict";
  const fs = require("fs");
  const fail = (message) => { console.error(message); process.exit(1); };
  let catalog, pinDoc;
  try {
    catalog = JSON.parse(fs.readFileSync(process.argv[1], "utf8"));
    pinDoc = JSON.parse(fs.readFileSync(process.argv[2], "utf8"));
  } catch (_) {
    fail("runtime catalog or pin document is not valid JSON");
  }
  if (!Array.isArray(catalog?.runtimes) || !Array.isArray(pinDoc?.pins)) {
    fail("runtime catalog or pin document has an invalid shape");
  }
  const required = catalog.runtimes.filter((runtime) => runtime?.pinnable === true);
  if (required.length === 0) fail("runtime catalog has no pinnable runtimes");
  const seen = new Set();
  const rows = [];
  for (const runtime of required) {
    if (typeof runtime?.name !== "string" || seen.has(runtime.name)) {
      fail("runtime catalog has an invalid or duplicate pinnable runtime name");
    }
    seen.add(runtime.name);
    const matches = pinDoc.pins.filter((pin) =>
      pin?.template_name === runtime.name && pin?.region === "global"
    );
    if (matches.length !== 1) {
      fail(`missing one exact global image pin for runtime ${runtime.name} (found ${matches.length})`);
    }
    const pin = matches[0];
    rows.push([runtime.name, runtime.image, pin.image_digest, pin.image_ref ?? ""]);
  }
  for (const row of rows) {
    if (row.some((value) => typeof value !== "string" || /[\t\r\n]/.test(value))) {
      fail("runtime catalog/pin plan contains a non-string or control character");
    }
  }
  process.stdout.write(rows.map((row) => row.join("\t")).join("\n") + "\n");
' "$catalog_json" "$pins_json" > "$plan_tsv"; then
  fail "runtime catalog/pin plan is incomplete or invalid; refusing a partial readiness check"
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
  [[ "${image_ref%%/*}" == "$REGISTRY_HOST" ]] \
    || fail "image_ref for runtime $runtime is not on $REGISTRY_HOST; refusing to send the registry credential to another host"
done < "$validated_tsv"

manifest_body="$tmp_dir/manifest.body"
manifest_headers="$tmp_dir/manifest.headers"
curl_err="$tmp_dir/curl.err"

# fetch_manifest <image_ref>: prints the HTTP status ("000" on a transport
# failure). The credential travels in curl's stdin config, never in argv, and
# redirects are not followed so it cannot be replayed to another origin.
fetch_manifest() {
  local path="${1#*/}"
  local repository="${path%@*}" digest="${path##*@}"
  {
    printf 'silent\nshow-error\nmax-time = %s\nmax-filesize = %s\n' "$REQUEST_TIMEOUT" "$MAX_MANIFEST_BYTES"
    printf 'header = "User-Agent: curl/8.4.0"\n'
    printf 'header = "Accept: %s"\n' "$MANIFEST_ACCEPT"
    printf 'user = "%s:%s"\n' "$reg_user" "$reg_token"
    printf 'output = "%s"\n' "$manifest_body"
    printf 'dump-header = "%s"\n' "$manifest_headers"
    printf 'write-out = "%%{http_code}"\n'
  } | curl --disable --config - "https://$REGISTRY_HOST/v2/$repository/manifests/$digest" 2>"$curl_err"
}

sha256_file() {
  node -e '
    "use strict";
    const fs = require("fs");
    const hash = require("crypto").createHash("sha256");
    process.stdout.write(hash.update(fs.readFileSync(process.argv[1])).digest("hex"));
  ' "$1"
}

while IFS=$'\t' read -r runtime image_ref; do
  digest="${image_ref##*@}"
  attempt=1
  status=""
  while :; do
    remaining=$((deadline - $(date +%s)))
    [[ "$remaining" -gt 0 ]] || fail "global ${READINESS_TIMEOUT}s budget exhausted before runtime $runtime was verified"
    rm -f "$manifest_body" "$manifest_headers"
    status="$(fetch_manifest "$image_ref")" || true
    case "$status" in
      200|404|401|403) break ;;
    esac
    [[ "$attempt" -lt "$REQUEST_ATTEMPTS" ]] \
      || fail "registry did not answer for runtime $runtime after $REQUEST_ATTEMPTS bounded attempt(s) (last HTTP ${status:-000}: $(head -c 300 "$curl_err" 2>/dev/null)): $image_ref"
    echo "::warning::runtime image readiness: registry answered HTTP ${status:-000} for runtime=$runtime; retrying within the global budget" >&2
    remaining=$((deadline - $(date +%s)))
    [[ "$remaining" -gt "$RETRY_DELAY" ]] \
      || fail "retry delay would exhaust the global ${READINESS_TIMEOUT}s budget before runtime $runtime"
    sleep "$RETRY_DELAY"
    attempt=$((attempt + 1))
  done
  case "$status" in
    404) fail "registry does not serve the promoted digest for runtime $runtime: $image_ref" ;;
    401|403) fail "registry refused the configured credential (HTTP $status) for runtime $runtime" ;;
  esac
  [[ -s "$manifest_body" ]] || fail "registry returned an empty manifest for runtime $runtime"
  body_digest="sha256:$(sha256_file "$manifest_body")"
  [[ "$body_digest" == "$digest" ]] \
    || fail "registry manifest for runtime $runtime hashes to $body_digest, not the promoted $digest"
  advertised="$(grep -i '^docker-content-digest:' "$manifest_headers" | tail -n 1 | tr -d '\r' | sed 's/^[^:]*:[[:space:]]*//' | tr 'A-F' 'a-f')" || advertised=""
  [[ "$advertised" == "$digest" ]] \
    || fail "registry advertised Docker-Content-Digest '${advertised:-<none>}' for runtime $runtime, expected $digest"
  echo ">> [runtime-image-readiness] verified runtime=$runtime served_digest=$image_ref"
done < "$validated_tsv"

elapsed=$(( $(date +%s) - started ))
echo "runtime image readiness PASS: $image_count exact digest(s) served by $REGISTRY_HOST in ${elapsed}s"
