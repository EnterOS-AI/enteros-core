#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
READINESS="$ROOT/scripts/deploy/prepare-staging-runtime-images.sh"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

[[ -f "$READINESS" ]] || fail "missing runtime-image readiness consumer: $READINESS"

mkdir -p "$TMP_DIR/bin" "$TMP_DIR/manifests"
REAL_NODE="$(command -v node)" || fail "node is required to exercise the local-deploy runner contract"

CREDENTIAL_ENV_NAMES='INFISICAL_CLIENT_ID INFISICAL_CLIENT_SECRET INFISICAL_PROJECT_ID REG_USER REG_TOKEN'

cat > "$TMP_DIR/bin/curl" <<'MOCK_CURL'
#!/usr/bin/env bash
set -euo pipefail
for name in ${CREDENTIAL_ENV_NAMES:?}; do
  [[ ! -v "$name" ]] || { echo "credential environment $name reached curl" >&2; exit 90; }
done
args="$*"
[[ "${1:-}" == "--disable" ]] || { echo "curl did not disable ambient config first" >&2; exit 89; }
for secret in "${EXPECTED_INFISICAL_SECRET:?}" "${EXPECTED_ACCESS_TOKEN:?}" "${EXPECTED_CP_TOKEN:?}" "${EXPECTED_REG_TOKEN:?}"; do
  [[ "$args" != *"$secret"* ]] || { echo "secret leaked into curl argv" >&2; exit 91; }
done
stdin="$(cat)"
printf '%s\n' "$args" >> "${CURL_LOG:?}"
url="${!#}"
config_value() {
  sed -n "s/^$1 = \"\\(.*\\)\"\$/\\1/p" <<< "$stdin"
}
case "$url" in
  */api/v1/auth/universal-auth/login)
    [[ "$stdin" == *"${EXPECTED_INFISICAL_ID:?}"* && "$stdin" == *"$EXPECTED_INFISICAL_SECRET"* ]] || exit 92
    printf '{"accessToken":"%s"}\n' "$EXPECTED_ACCESS_TOKEN"
    ;;
  */api/v3/secrets/raw/CP_ADMIN_API_TOKEN*)
    [[ "$stdin" == *"Authorization: Bearer $EXPECTED_ACCESS_TOKEN"* ]] || exit 93
    printf '{"secret":{"secretValue":"%s"}}\n' "$EXPECTED_CP_TOKEN"
    ;;
  */cp/runtimes)
    [[ "$stdin" != *"Authorization:"* ]] || exit 94
    cat "${CATALOG_FIXTURE:?}"
    ;;
  */cp/admin/runtime-image)
    [[ "$stdin" == *"Authorization: Bearer $EXPECTED_CP_TOKEN"* ]] || exit 95
    cat "${PINS_FIXTURE:?}"
    ;;
  https://"${MOCK_REGISTRY_HOST:?}"/v2/*/manifests/sha256:*)
    printf '%s\n' "$url" >> "${REGISTRY_LOG:?}"
    [[ "$(config_value user)" == "${EXPECTED_REG_USER:?}:$EXPECTED_REG_TOKEN" ]] || exit 87
    [[ "$stdin" != *"location"* ]] || { echo "registry request follows redirects" >&2; exit 86; }
    body="$(config_value output)"
    headers="$(config_value dump-header)"
    digest="${url##*/manifests/}"
    fixture="${MANIFEST_FIXTURES:?}/${digest#sha256:}"
    serve() {
      cp "$1" "$body"
      printf 'HTTP/1.1 200 OK\r\nDocker-Content-Digest: %s\r\n\r\n' "$2" > "$headers"
      printf '200'
    }
    case "${MOCK_REGISTRY_MODE:-success}" in
      success) serve "$fixture" "$digest" ;;
      missing) printf '404' ;;
      unauthorized) printf '401' ;;
      always-unavailable) printf '503' ;;
      transport-fail) echo "curl: (7) mock connect failure" >&2; printf '000'; exit 7 ;;
      unavailable-once)
        if [[ ! -e "${REGISTRY_STATE:?}" ]]; then
          : > "$REGISTRY_STATE"
          printf '503'
        else
          serve "$fixture" "$digest"
        fi
        ;;
      wrong-body) printf '{"tampered":true}' > "$TMP_WRONG_BODY"; serve "$TMP_WRONG_BODY" "$digest" ;;
      wrong-header) serve "$fixture" "sha256:$(printf 'c%.0s' {1..64})" ;;
    esac
    ;;
  *) echo "unexpected curl URL: $url" >&2; exit 96 ;;
esac
MOCK_CURL

# The host-executor contract intentionally has Node (actions/checkout requires
# it) but not jq. Keep a poison jq in PATH so this consumer cannot regress to
# the dependency that already failed on the exact local-deploy runner. Docker is
# poisoned too: no daemon is reachable from that runner any more.
for tool in jq docker; do
  cat > "$TMP_DIR/bin/$tool" <<MOCK_POISON
#!/usr/bin/env bash
echo "readiness must not invoke $tool on the local-deploy runner" >&2
exit 98
MOCK_POISON
done

cat > "$TMP_DIR/bin/node" <<'MOCK_NODE'
#!/usr/bin/env bash
set -euo pipefail
for name in ${CREDENTIAL_ENV_NAMES:?}; do
  [[ ! -v "$name" ]] || { echo "credential environment $name reached node" >&2; exit 90; }
done
exec "${REAL_NODE:?}" "$@"
MOCK_NODE

chmod +x "$TMP_DIR/bin/curl" "$TMP_DIR/bin/jq" "$TMP_DIR/bin/docker" "$TMP_DIR/bin/node"

CATALOG="$TMP_DIR/catalog.json"
PINS="$TMP_DIR/pins.json"
CURL_LOG="$TMP_DIR/curl.log"
REGISTRY_LOG="$TMP_DIR/registry.log"
REGISTRY_STATE="$TMP_DIR/registry-state"
INFISICAL_ID='fixture-client-id'
INFISICAL_SECRET='fixture-client-secret-must-not-leak'
ACCESS_TOKEN='fixture-access-token-must-not-leak'
CP_TOKEN='fixture-cp-token-must-not-leak'
REG_USER_VALUE='fixture-registry-user'
REG_TOKEN_VALUE='fixture-registry-token-must-not-leak'
REGISTRY='registry.example.test'

# Content-addressed fixtures: each digest is the sha256 of the manifest bytes
# the mock registry serves for it, exactly as a real registry would.
manifest_fixture() {
  local body="$TMP_DIR/manifests/body-$1" hex
  printf '{"schemaVersion":2,"fixture":"%s"}' "$1" > "$body"
  hex="$(python3 -c 'import hashlib,sys; print(hashlib.sha256(open(sys.argv[1],"rb").read()).hexdigest())' "$body")"
  mv "$body" "$TMP_DIR/manifests/$hex"
  printf 'sha256:%s' "$hex"
}
DIGEST_A="$(manifest_fixture claude-code)"
DIGEST_B="$(manifest_fixture hermes)"
REF_A="$REGISTRY/molecule-ai/workspace-template-claude-code@$DIGEST_A"
REF_B="$REGISTRY/molecule-ai/workspace-template-hermes@$DIGEST_B"

write_catalog() {
  cat > "$CATALOG" <<JSON
{"runtimes":[
  {"name":"claude-code","image":"workspace-template-claude-code","pinnable":true},
  {"name":"hermes","image":"workspace-template-hermes","pinnable":true},
  {"name":"experimental","image":"workspace-template-experimental","pinnable":false}
]}
JSON
}

write_good_pins() {
  cat > "$PINS" <<JSON
{"pins":[
  {"template_name":"claude-code","region":"global","image_digest":"$DIGEST_A","image_ref":"$REF_A"},
  {"template_name":"hermes","region":"global","image_digest":"$DIGEST_B","image_ref":"$REF_B"},
  {"template_name":"molecule-tenant","region":"global","image_digest":"$DIGEST_A"}
]}
JSON
}

run_gate() {
  : > "$CURL_LOG"
  : > "$REGISTRY_LOG"
  rm -f "$REGISTRY_STATE"
  CREDENTIAL_ENV_NAMES="$CREDENTIAL_ENV_NAMES" \
    EXPECTED_INFISICAL_ID="$INFISICAL_ID" \
    EXPECTED_INFISICAL_SECRET="$INFISICAL_SECRET" \
    EXPECTED_ACCESS_TOKEN="$ACCESS_TOKEN" \
    EXPECTED_CP_TOKEN="$CP_TOKEN" \
    EXPECTED_REG_USER="$REG_USER_VALUE" EXPECTED_REG_TOKEN="$REG_TOKEN_VALUE" \
    REAL_NODE="$REAL_NODE" CATALOG_FIXTURE="$CATALOG" PINS_FIXTURE="$PINS" \
    CURL_LOG="$CURL_LOG" REGISTRY_LOG="$REGISTRY_LOG" REGISTRY_STATE="$REGISTRY_STATE" \
    MANIFEST_FIXTURES="$TMP_DIR/manifests" TMP_WRONG_BODY="$TMP_DIR/wrong-body" \
    MOCK_REGISTRY_HOST="$REGISTRY" RUNTIME_IMAGE_REGISTRY_HOST="${TEST_REGISTRY_HOST:-$REGISTRY}" \
    CP_BASE_URL="https://staging.example.test" \
    INFISICAL_BASE="https://key.example.test" INFISICAL_ENV=staging \
    INFISICAL_CLIENT_ID="$INFISICAL_ID" INFISICAL_CLIENT_SECRET="$INFISICAL_SECRET" \
    INFISICAL_PROJECT_ID="293c3669-423b-4610-96f0-d3f7a611b340" \
    REG_USER="$REG_USER_VALUE" REG_TOKEN="$REG_TOKEN_VALUE" \
    RUNTIME_IMAGE_READINESS_TIMEOUT_SECONDS="${TEST_READINESS_TIMEOUT:-600}" \
    RUNTIME_IMAGE_REQUEST_ATTEMPTS=3 \
    RUNTIME_IMAGE_REQUEST_RETRY_DELAY_SECONDS="${TEST_RETRY_DELAY:-0}" PATH="$TMP_DIR/bin:$PATH" \
    bash "$READINESS" 2>&1
}

expect_failure() {
  local label="$1" expected="$2" output rc
  shift 2
  set +e
  output="$("$@")"; rc=$?
  set -e
  [[ $rc -ne 0 ]] || fail "$label unexpectedly succeeded: $output"
  [[ "$output" == *"$expected"* ]] || fail "$label did not report '$expected': $output"
  for secret in "$INFISICAL_SECRET" "$ACCESS_TOKEN" "$CP_TOKEN" "$REG_TOKEN_VALUE"; do
    [[ "$output" != *"$secret"* ]] || fail "$label leaked a secret"
  done
}

run_foreign_host() { TEST_REGISTRY_HOST=registry.other.test run_gate; }
run_missing() { MOCK_REGISTRY_MODE=missing run_gate; }
run_unauthorized() { MOCK_REGISTRY_MODE=unauthorized run_gate; }
run_wrong_body() { MOCK_REGISTRY_MODE=wrong-body run_gate; }
run_wrong_header() { MOCK_REGISTRY_MODE=wrong-header run_gate; }
run_unavailable() { MOCK_REGISTRY_MODE=always-unavailable run_gate; }
run_transport_fail() { MOCK_REGISTRY_MODE=transport-fail run_gate; }

registry_requests() { grep -c . "$REGISTRY_LOG" || true; }

write_catalog
write_good_pins
output="$(run_gate)" || fail "good readiness failed: $output"
[[ "$output" == *"runtime image readiness PASS: 2 exact digest(s) served by $REGISTRY"* ]] || fail "missing pass summary: $output"
for secret in "$INFISICAL_SECRET" "$ACCESS_TOKEN" "$CP_TOKEN" "$REG_TOKEN_VALUE"; do
  [[ "$output" != *"$secret"* ]] || fail "good readiness leaked a secret"
done
grep -Fx "https://$REGISTRY/v2/molecule-ai/workspace-template-claude-code/manifests/$DIGEST_A" "$REGISTRY_LOG" >/dev/null \
  || fail "claude-code digest not requested from the registry"
grep -Fx "https://$REGISTRY/v2/molecule-ai/workspace-template-hermes/manifests/$DIGEST_B" "$REGISTRY_LOG" >/dev/null \
  || fail "hermes digest not requested from the registry"

# Malformed JSON and duplicate CP-owned rows must fail before the first registry
# request. This exercises the complete-plan parser without jq available.
printf '{not-json\n' > "$CATALOG"
expect_failure "malformed catalog" "not valid JSON" run_gate
[[ "$(registry_requests)" == 0 ]] || fail "malformed catalog reached the registry"

write_catalog
python3 - "$CATALOG" <<'PY'
import json, sys
p = json.load(open(sys.argv[1]))
p["runtimes"].append(dict(p["runtimes"][0]))
json.dump(p, open(sys.argv[1], "w"))
PY
expect_failure "duplicate runtime" "invalid or duplicate pinnable runtime name" run_gate
[[ "$(registry_requests)" == 0 ]] || fail "duplicate runtime reached the registry"

write_catalog
write_good_pins
python3 - "$PINS" <<'PY'
import json, sys
p = json.load(open(sys.argv[1]))
p["pins"].append(dict(p["pins"][0]))
json.dump(p, open(sys.argv[1], "w"))
PY
expect_failure "duplicate pin" "missing one exact global image pin" run_gate
[[ "$(registry_requests)" == 0 ]] || fail "duplicate pin reached the registry"

cat > "$PINS" <<JSON
{"pins":[{"template_name":"claude-code","region":"global","image_digest":"$DIGEST_A","image_ref":"$REF_A"}]}
JSON
expect_failure "missing pin" "missing one exact global image pin" run_gate
[[ "$(registry_requests)" == 0 ]] || fail "incomplete plan performed a partial check"

write_good_pins
python3 - "$PINS" <<'PY'
import json, sys
p = json.load(open(sys.argv[1]))
p["pins"][0]["image_ref"] = "registry.example.test/molecule-ai/workspace-template-claude-code:latest"
json.dump(p, open(sys.argv[1], "w"))
PY
expect_failure "mutable ref" "invalid immutable image_ref" run_gate
[[ "$(registry_requests)" == 0 ]] || fail "mutable ref reached the registry"

# The registry credential belongs to one host. A CP-projected ref naming any
# other host must stop the whole plan before ANY credentialed request.
write_good_pins
expect_failure "foreign registry host" "refusing to send the registry credential to another host" run_foreign_host
[[ "$(registry_requests)" == 0 ]] || fail "a foreign-host ref still reached the registry"

write_good_pins
expect_failure "unserved digest" "registry does not serve the promoted digest for runtime claude-code" run_missing
[[ "$(registry_requests)" == 1 ]] || fail "a definitive 404 was retried"

expect_failure "refused credential" "registry refused the configured credential (HTTP 401)" run_unauthorized

expect_failure "tampered manifest" "hashes to" run_wrong_body

expect_failure "mismatched digest header" "registry advertised Docker-Content-Digest" run_wrong_header

expect_failure "registry unavailable" "after 3 bounded attempt(s)" run_unavailable
[[ "$(registry_requests)" == 3 ]] || fail "unavailable registry was not retried exactly to the attempt bound"

expect_failure "transport failure" "mock connect failure" run_transport_fail

retry_output="$(MOCK_REGISTRY_MODE=unavailable-once run_gate)" || fail "bounded retry did not recover: $retry_output"
[[ "$(registry_requests)" == 3 ]] || fail "one transient 503 should cost exactly one extra request (got $(registry_requests))"

set +e
delay_output="$(MOCK_REGISTRY_MODE=always-unavailable TEST_READINESS_TIMEOUT=10 TEST_RETRY_DELAY=999 run_gate)"; delay_rc=$?
set -e
[[ $delay_rc -ne 0 && "$delay_output" == *"retry delay would exhaust"* ]] \
  || fail "retry delay was not charged against the global deadline: $delay_output"

echo "PASS: Core staging CD verifies the CP-projected exact runtime digests are served by the registry"
