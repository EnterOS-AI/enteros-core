#!/usr/bin/env bash
# Pre-E2E readiness for promoted workspace runtime images: the registry must
# serve every pinnable runtime's promoted immutable image_ref, with manifest
# bytes that hash to the promoted digest, every child manifest likewise, and
# every config and layer blob they reference. Staging workspaces are k8s pods whose
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

reply_body="$tmp_dir/reply.body"
reply_headers="$tmp_dir/reply.headers"
curl_err="$tmp_dir/curl.err"
reply_status=""

# registry_request <GET|HEAD> <v2-path> <label>: sets reply_status to a
# definitive answer (200/401/403/404). Transport failures and every other
# status are retried within the attempt bound and the global budget; a curl
# failure is never read as a status, because a stalled or truncated body still
# prints the 200 it started with. The credential travels in curl's stdin
# config, never in argv, and redirects are not followed so it cannot be
# replayed to another origin.
registry_request() {
  local method="$1" path="$2" label="$3" attempt=1 rc remaining
  while :; do
    remaining=$((deadline - $(date +%s)))
    [[ "$remaining" -gt 0 ]] || fail "global ${READINESS_TIMEOUT}s budget exhausted before $label was verified"
    rm -f "$reply_body" "$reply_headers"
    rc=0
    reply_status="$({
      printf 'silent\nshow-error\nmax-time = %s\n' "$REQUEST_TIMEOUT"
      # A HEAD reply carries the blob's own Content-Length, which the manifest
      # size bound must not apply to.
      if [[ "$method" == HEAD ]]; then
        printf 'head\n'
      else
        printf 'max-filesize = %s\n' "$MAX_MANIFEST_BYTES"
      fi
      printf 'header = "User-Agent: curl/8.4.0"\n'
      printf 'header = "Accept: %s"\n' "$MANIFEST_ACCEPT"
      printf 'user = "%s:%s"\n' "$reg_user" "$reg_token"
      printf 'output = "%s"\n' "$reply_body"
      printf 'dump-header = "%s"\n' "$reply_headers"
      printf 'write-out = "%%{http_code}"\n'
    } | curl --disable --config - "https://$REGISTRY_HOST/v2/$path" 2>"$curl_err")" || rc=$?
    if [[ "$rc" -eq 0 ]]; then
      case "$reply_status" in 200|401|403|404) return 0 ;; esac
    elif [[ "$rc" -eq 63 ]] || grep -q 'Maximum file size exceeded' "$curl_err" 2>/dev/null; then
      # curl reports the bound as exit 63 or, in some builds, 56.
      fail "registry response for $label exceeds the ${MAX_MANIFEST_BYTES}-byte manifest bound"
    fi
    [[ "$attempt" -lt "$REQUEST_ATTEMPTS" ]] \
      || fail "registry did not answer for $label after $REQUEST_ATTEMPTS bounded attempt(s) (last HTTP ${reply_status:-000}, curl exit $rc: $(head -c 300 "$curl_err" 2>/dev/null))"
    echo "::warning::runtime image readiness: registry answered HTTP ${reply_status:-000} (curl exit $rc) for $label; retrying within the global budget" >&2
    remaining=$((deadline - $(date +%s)))
    [[ "$remaining" -gt "$RETRY_DELAY" ]] \
      || fail "retry delay would exhaust the global ${READINESS_TIMEOUT}s budget before $label"
    sleep "$RETRY_DELAY"
    attempt=$((attempt + 1))
  done
}

require_found() {
  local label="$1" what="$2"
  case "$reply_status" in
    200) ;;
    404) fail "registry does not serve $what for $label" ;;
    *) fail "registry refused the configured credential (HTTP $reply_status) for $label" ;;
  esac
}

sha256_file() {
  node -e '
    "use strict";
    const fs = require("fs");
    const hash = require("crypto").createHash("sha256");
    process.stdout.write(hash.update(fs.readFileSync(process.argv[1])).digest("hex"));
  ' "$1"
}

# Prints "manifest <digest>" for each child of an index / manifest list and
# "blob <digest>" for the config and every layer of an image manifest. Exits
# non-zero on any other shape, so an unrecognised document cannot pass with
# nothing checked beneath it.
manifest_references() {
  node -e '
    "use strict";
    const DIGEST = /^sha256:[a-f0-9]{64}$/;
    const out = [];
    const need = (d) => { if (typeof d !== "string" || !DIGEST.test(d)) process.exit(1); return d; };
    let doc;
    try { doc = JSON.parse(require("fs").readFileSync(process.argv[1], "utf8")); } catch (_) { process.exit(1); }
    if (Array.isArray(doc?.manifests)) {
      if (doc.manifests.length === 0) process.exit(1);
      for (const m of doc.manifests) out.push("manifest " + need(m?.digest));
    } else if (doc?.config && Array.isArray(doc?.layers)) {
      out.push("blob " + need(doc.config.digest));
      for (const l of doc.layers) out.push("blob " + need(l?.digest));
    } else {
      process.exit(1);
    }
    process.stdout.write(out.join("\n") + "\n");
  ' "$1"
}

# verify_manifest <repository> <digest> <label> <refs-out>: the manifest must
# be served, hash to <digest>, and be advertised as <digest>. Its references
# are appended to <refs-out>.
verify_manifest() {
  local repository="$1" digest="$2" label="$3" refs_out="$4" body_digest advertised
  registry_request GET "$repository/manifests/$digest" "$label"
  require_found "$label" "manifest $digest"
  [[ -s "$reply_body" ]] || fail "registry returned an empty manifest $digest for $label"
  body_digest="sha256:$(sha256_file "$reply_body")"
  [[ "$body_digest" == "$digest" ]] \
    || fail "registry manifest $digest for $label hashes to $body_digest"
  advertised="$(grep -i '^docker-content-digest:' "$reply_headers" | tail -n 1 | tr -d '\r' | sed 's/^[^:]*:[[:space:]]*//' | tr 'A-F' 'a-f')" || advertised=""
  [[ "$advertised" == "$digest" ]] \
    || fail "registry advertised Docker-Content-Digest '${advertised:-<none>}' for manifest $digest of $label"
  manifest_references "$reply_body" >> "$refs_out" \
    || fail "manifest $digest for $label is not an image manifest or index with valid digests"
}

while IFS=$'\t' read -r runtime image_ref; do
  path="${image_ref#*/}"
  repository="${path%@*}"
  digest="${path##*@}"
  label="runtime $runtime"
  top_refs="$tmp_dir/refs.top"
  image_refs="$tmp_dir/refs.image"
  : > "$top_refs"
  : > "$image_refs"
  verify_manifest "$repository" "$digest" "$label" "$top_refs"
  manifest_count=1
  if grep -q '^manifest ' "$top_refs"; then
    grep -q '^blob ' "$top_refs" && fail "manifest $digest for $label mixes child manifests and blobs"
    while read -r _ child; do
      verify_manifest "$repository" "$child" "$label" "$image_refs"
      manifest_count=$((manifest_count + 1))
    done < "$top_refs"
    grep -q '^manifest ' "$image_refs" && fail "a child manifest of $digest for $label is itself an index"
  else
    cp "$top_refs" "$image_refs"
  fi
  blob_count=0
  while read -r _ blob; do
    registry_request HEAD "$repository/blobs/$blob" "$label"
    require_found "$label" "blob $blob"
    blob_count=$((blob_count + 1))
  done < <(sort -u "$image_refs")
  echo ">> [runtime-image-readiness] verified runtime=$runtime manifests=$manifest_count blobs=$blob_count ref=$image_ref"
done < "$validated_tsv"

elapsed=$(( $(date +%s) - started ))
echo "runtime image readiness PASS: $image_count exact digest(s) served by $REGISTRY_HOST, every manifest and blob present, in ${elapsed}s"
