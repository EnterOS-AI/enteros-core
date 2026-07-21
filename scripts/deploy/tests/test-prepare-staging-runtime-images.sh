#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
READINESS="$ROOT/scripts/deploy/prepare-staging-runtime-images.sh"
GUARD="$ROOT/scripts/deploy/require-local-deploy-daemon.sh"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

[[ -f "$READINESS" ]] || fail "missing runtime-image readiness consumer: $READINESS"
[[ -f "$GUARD" ]] || fail "missing local-deploy daemon guard: $GUARD"

mkdir -p "$TMP_DIR/bin" "$TMP_DIR/certs"
printf 'ca\n' > "$TMP_DIR/certs/ca.pem"
printf 'cert\n' > "$TMP_DIR/certs/cert.pem"
printf 'key\n' > "$TMP_DIR/certs/key.pem"

cat > "$TMP_DIR/bin/dirname" <<'MOCK_DIRNAME'
#!/usr/bin/env bash
set -euo pipefail
for name in INFISICAL_CLIENT_ID INFISICAL_CLIENT_SECRET INFISICAL_PROJECT_ID; do
  [[ ! -v "$name" ]] || { echo "Infisical credential environment reached a child process" >&2; exit 90; }
done
/usr/bin/dirname "$@"
MOCK_DIRNAME

cat > "$TMP_DIR/bin/curl" <<'MOCK_CURL'
#!/usr/bin/env bash
set -euo pipefail
for name in INFISICAL_CLIENT_ID INFISICAL_CLIENT_SECRET INFISICAL_PROJECT_ID; do
  [[ ! -v "$name" ]] || { echo "Infisical credential environment reached curl" >&2; exit 90; }
done
args="$*"
for secret in "${EXPECTED_INFISICAL_SECRET:?}" "${EXPECTED_ACCESS_TOKEN:?}" "${EXPECTED_CP_TOKEN:?}"; do
  [[ "$args" != *"$secret"* ]] || { echo "secret leaked into curl argv" >&2; exit 91; }
done
stdin="$(cat)"
printf '%s\n' "$args" >> "${CURL_LOG:?}"
url="${!#}"
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
  *) echo "unexpected curl URL: $url" >&2; exit 96 ;;
esac
MOCK_CURL

cat > "$TMP_DIR/bin/docker" <<'MOCK_DOCKER'
#!/usr/bin/env bash
set -euo pipefail

if [[ "${1:-}" == "--host" && "${2:-}" == "${MOCK_REMOTE_HOST:?}" \
   && "${3:-}" == "--tlsverify" && "${4:-}" == "--tlscacert" \
   && "${6:-}" == "--tlscert" && "${8:-}" == "--tlskey" ]]; then
  shift 9
fi
printf '%s\n' "$*" >> "${DOCKER_LOG:?}"
case "${1:-} ${2:-}" in
  "info ") [[ "${MOCK_DOCKER_MODE:-success}" != "unreachable" ]] ;;
  "network inspect")
    [[ "${3:-}" == "molecule-net" && "${MOCK_DOCKER_MODE:-success}" != "missing-network" ]]
    ;;
  "container inspect")
    if [[ "${3:-}" == "--format" ]]; then
      if [[ "${MOCK_DOCKER_MODE:-success}" != "staging-wrong-network" ]]; then
        printf 'fixture-network-id\n'
      fi
    else
      [[ "${3:-}" == "molecule-cp-staging" && "${MOCK_DOCKER_MODE:-success}" != "missing-staging-cp" ]]
    fi
    ;;
  "pull "*)
    case "${MOCK_PULL_MODE:-success}" in
      success) exit 0 ;;
      always-fail) exit 1 ;;
      fail-once)
        if [[ ! -e "${PULL_STATE:?}" ]]; then : > "$PULL_STATE"; exit 1; fi
        exit 0
        ;;
    esac
    ;;
  "image inspect")
    ref="${!#}"
    if [[ "${MOCK_INSPECT_MISMATCH:-0}" == "1" ]]; then
      printf 'registry.example.test/molecule-ai/workspace-template-wrong@sha256:%064d\n' 0
    else
      printf '%s\n' "$ref"
    fi
    ;;
  *) echo "unexpected docker invocation: $*" >&2; exit 97 ;;
esac
MOCK_DOCKER

cat > "$TMP_DIR/bin/timeout" <<'MOCK_TIMEOUT'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "${TIMEOUT_LOG:?}"
while [[ "${1:-}" == --* ]]; do shift; done
[[ "${1:-}" =~ ^[1-9][0-9]*s$ ]] || exit 98
shift
"$@"
MOCK_TIMEOUT
chmod +x "$TMP_DIR/bin/dirname" "$TMP_DIR/bin/curl" "$TMP_DIR/bin/docker" "$TMP_DIR/bin/timeout"

CATALOG="$TMP_DIR/catalog.json"
PINS="$TMP_DIR/pins.json"
CURL_LOG="$TMP_DIR/curl.log"
DOCKER_LOG="$TMP_DIR/docker.log"
TIMEOUT_LOG="$TMP_DIR/timeout.log"
PULL_STATE="$TMP_DIR/pull-state"
INFISICAL_ID='fixture-client-id'
INFISICAL_SECRET='fixture-client-secret-must-not-leak'
ACCESS_TOKEN='fixture-access-token-must-not-leak'
CP_TOKEN='fixture-cp-token-must-not-leak'
REMOTE_HOST='tcp://runtime-daemon.example.test:2376'
DIGEST_A="sha256:$(printf 'a%.0s' {1..64})"
DIGEST_B="sha256:$(printf 'b%.0s' {1..64})"
REF_A="registry.example.test/molecule-ai/workspace-template-claude-code@$DIGEST_A"
REF_B="registry.example.test/molecule-ai/workspace-template-hermes@$DIGEST_B"

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
  : > "$DOCKER_LOG"
  : > "$TIMEOUT_LOG"
  rm -f "$PULL_STATE"
  EXPECTED_INFISICAL_ID="$INFISICAL_ID" \
    EXPECTED_INFISICAL_SECRET="$INFISICAL_SECRET" \
    EXPECTED_ACCESS_TOKEN="$ACCESS_TOKEN" \
    EXPECTED_CP_TOKEN="$CP_TOKEN" \
    CATALOG_FIXTURE="$CATALOG" PINS_FIXTURE="$PINS" \
    CURL_LOG="$CURL_LOG" DOCKER_LOG="$DOCKER_LOG" TIMEOUT_LOG="$TIMEOUT_LOG" \
    PULL_STATE="$PULL_STATE" MOCK_REMOTE_HOST="$REMOTE_HOST" \
    DOCKER_HOST="${TEST_DOCKER_HOST:-$REMOTE_HOST}" \
    MOLECULE_PROD_DOCKER_HOST="${TEST_EXPECTED_DOCKER_HOST:-$REMOTE_HOST}" \
    DOCKER_CONTEXT="${TEST_DOCKER_CONTEXT:-}" DOCKER_TLS_VERIFY="${TEST_TLS_VERIFY:-1}" \
    DOCKER_CERT_PATH="${TEST_CERT_PATH:-$TMP_DIR/certs}" \
    CP_BASE_URL="https://staging.example.test" \
    INFISICAL_BASE="https://key.example.test" INFISICAL_ENV=staging \
    INFISICAL_CLIENT_ID="$INFISICAL_ID" INFISICAL_CLIENT_SECRET="$INFISICAL_SECRET" \
    INFISICAL_PROJECT_ID="293c3669-423b-4610-96f0-d3f7a611b340" \
    RUNTIME_IMAGE_READINESS_TIMEOUT_SECONDS=1200 \
    RUNTIME_IMAGE_PULL_TIMEOUT_SECONDS=600 RUNTIME_IMAGE_PULL_ATTEMPTS=2 \
    RUNTIME_IMAGE_PULL_RETRY_DELAY_SECONDS=0 PATH="$TMP_DIR/bin:$PATH" \
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
  for secret in "$INFISICAL_SECRET" "$ACCESS_TOKEN" "$CP_TOKEN"; do
    [[ "$output" != *"$secret"* ]] || fail "$label leaked a secret"
  done
}

run_endpoint_drift() { TEST_DOCKER_HOST='tcp://other.example.test:2376' run_gate; }
run_missing_certs() { TEST_CERT_PATH="$TMP_DIR/missing-certs" run_gate; }
run_missing_network() { MOCK_DOCKER_MODE=missing-network run_gate; }
run_missing_staging_cp() { MOCK_DOCKER_MODE=missing-staging-cp run_gate; }
run_wrong_staging_network() { MOCK_DOCKER_MODE=staging-wrong-network run_gate; }

write_catalog
write_good_pins
output="$(run_gate)" || fail "good readiness failed: $output"
[[ "$output" == *"runtime image readiness PASS: 2 exact digest(s)"* ]] || fail "missing pass summary: $output"
for secret in "$INFISICAL_SECRET" "$ACCESS_TOKEN" "$CP_TOKEN"; do
  [[ "$output" != *"$secret"* ]] || fail "good readiness leaked a secret"
done
grep -Fx "pull $REF_A" "$DOCKER_LOG" >/dev/null || fail "claude-code digest not pulled"
grep -Fx "pull $REF_B" "$DOCKER_LOG" >/dev/null || fail "hermes digest not pulled"
[[ "$(grep -cE ' docker .* pull ' "$TIMEOUT_LOG")" == "2" ]] || fail "pulls are not independently timeout-bounded"

# Runner label alone is not the daemon boundary: endpoint drift, missing mTLS,
# missing molecule-net, or an absent/misattached staging CP must fail before
# auth/API calls or pull.
expect_failure "endpoint drift" "must exactly match" run_endpoint_drift
[[ ! -s "$CURL_LOG" ]] || fail "endpoint drift reached credential/API calls"
expect_failure "missing mTLS key" "missing mTLS material" run_missing_certs
[[ ! -s "$CURL_LOG" ]] || fail "missing mTLS key reached credential/API calls"
expect_failure "missing network" "molecule-net is missing" run_missing_network
[[ ! -s "$CURL_LOG" ]] || fail "missing network reached credential/API calls"
expect_failure "missing staging CP" "molecule-cp-staging is missing" run_missing_staging_cp
[[ ! -s "$CURL_LOG" ]] || fail "missing staging CP reached credential/API calls"
expect_failure "wrong staging CP network" "molecule-cp-staging is not attached" run_wrong_staging_network
[[ ! -s "$CURL_LOG" ]] || fail "wrong staging CP network reached credential/API calls"

cat > "$PINS" <<JSON
{"pins":[{"template_name":"claude-code","region":"global","image_digest":"$DIGEST_A","image_ref":"$REF_A"}]}
JSON
expect_failure "missing pin" "missing one exact global image pin" run_gate
if grep -q '^pull ' "$DOCKER_LOG"; then fail "incomplete plan performed a partial pull"; fi

write_good_pins
python3 - "$PINS" <<'PY'
import json, sys
p = json.load(open(sys.argv[1]))
p["pins"][0]["image_ref"] = "registry.example.test/molecule-ai/workspace-template-claude-code:latest"
json.dump(p, open(sys.argv[1], "w"))
PY
expect_failure "mutable ref" "invalid immutable image_ref" run_gate
if grep -q '^pull ' "$DOCKER_LOG"; then fail "mutable ref reached docker pull"; fi

write_good_pins
set +e
pull_output="$(MOCK_PULL_MODE=always-fail run_gate)"; pull_rc=$?
set -e
[[ $pull_rc -ne 0 && "$pull_output" == *"docker pull failed"* ]] || fail "pull failure did not fail closed"

retry_output="$(MOCK_PULL_MODE=fail-once run_gate)" || fail "bounded retry did not recover: $retry_output"
[[ "$(grep -c '^pull ' "$DOCKER_LOG")" == "3" ]] || fail "retry count is not bounded to one extra attempt"

set +e
inspect_output="$(MOCK_INSPECT_MISMATCH=1 run_gate)"; inspect_rc=$?
set -e
[[ $inspect_rc -ne 0 && "$inspect_output" == *"exact RepoDigest verification failed"* ]] || fail "digest mismatch did not fail closed"

echo "PASS: Core staging CD pre-pulls the CP-projected exact runtime digests on the guarded local-deploy daemon"
