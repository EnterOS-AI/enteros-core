#!/usr/bin/env bash
# Read-only fingerprint for jobs that must reach the local-Docker production
# daemon through the dedicated `local-deploy` runner. The endpoint stays in
# runner configuration; this repository proves exact equality, mTLS, daemon
# reachability, and the staging CP's molecule-net attachment without recording
# the private address. That container fingerprint proves this is the daemon the
# live staging local-Docker provisioner actually mutates.
set -euo pipefail

local_deploy_guard_fail() {
  echo "::error::local-deploy daemon guard: $*" >&2
  return 1
}

[[ -z "${DOCKER_CONTEXT:-}" ]] || local_deploy_guard_fail "DOCKER_CONTEXT must be empty"

current_host="${DOCKER_HOST:-}"
expected_host="${MOLECULE_PROD_DOCKER_HOST:-}"
[[ -n "$expected_host" ]] || local_deploy_guard_fail "MOLECULE_PROD_DOCKER_HOST must be supplied by runner configuration"
[[ "$current_host" == "$expected_host" ]] || local_deploy_guard_fail "DOCKER_HOST must exactly match runner-configured MOLECULE_PROD_DOCKER_HOST"
case "$current_host" in
  tcp://*) ;;
  *) local_deploy_guard_fail "the local-deploy endpoint must be a runner-configured mTLS tcp://host:port" ;;
esac

authority="${current_host#tcp://}"
if [[ -z "$authority" || "$authority" == *[[:space:]/@]* || "$authority" != *:* ]]; then
  local_deploy_guard_fail "the local-deploy endpoint must be an exact tcp://host:port authority"
fi
remote_name="${authority%:*}"
remote_port="${authority##*:}"
if [[ -z "$remote_name" || "$remote_name" == *:* || ! "$remote_port" =~ ^[1-9][0-9]{0,4}$ ]]; then
  local_deploy_guard_fail "the local-deploy endpoint must be an exact tcp://host:port authority"
fi
(( 10#$remote_port <= 65535 )) || local_deploy_guard_fail "the local-deploy endpoint port is outside 1..65535"

[[ "${DOCKER_TLS_VERIFY:-}" == "1" ]] || local_deploy_guard_fail "DOCKER_TLS_VERIFY must be 1"
cert_path="${DOCKER_CERT_PATH:-}"
[[ -n "$cert_path" ]] || local_deploy_guard_fail "DOCKER_CERT_PATH must be supplied by runner configuration"
for pem in ca.pem cert.pem key.pem; do
  [[ -s "$cert_path/$pem" ]] || local_deploy_guard_fail "missing mTLS material $cert_path/$pem"
done

# Exposed to callers that source this guard. Every subsequent Docker operation
# can carry the same endpoint/certificate pins instead of trusting ambient CLI
# resolution after the one-time fingerprint.
readonly -a DOCKER_PIN_ARGS=(
  --host "$current_host"
  --tlsverify
  --tlscacert "$cert_path/ca.pem"
  --tlscert "$cert_path/cert.pem"
  --tlskey "$cert_path/key.pem"
)
readonly STAGING_CP_CONTAINER="molecule-cp-staging"
readonly LOCAL_NETWORK="molecule-net"
docker_pinned() {
  command docker "${DOCKER_PIN_ARGS[@]}" "$@"
}

docker_pinned info >/dev/null 2>&1 || local_deploy_guard_fail "Docker daemon is unreachable at the runner-configured endpoint"
docker_pinned network inspect "$LOCAL_NETWORK" >/dev/null 2>&1 \
  || local_deploy_guard_fail "required Docker network $LOCAL_NETWORK is missing"
docker_pinned container inspect "$STAGING_CP_CONTAINER" >/dev/null 2>&1 \
  || local_deploy_guard_fail "$STAGING_CP_CONTAINER is missing from the selected daemon"
if ! staging_network_id="$(docker_pinned container inspect \
  --format "{{with index .NetworkSettings.Networks \"$LOCAL_NETWORK\"}}{{.NetworkID}}{{end}}" \
  "$STAGING_CP_CONTAINER" 2>/dev/null)"; then
  local_deploy_guard_fail "could not inspect $STAGING_CP_CONTAINER network attachment"
fi
[[ -n "${staging_network_id//[[:space:]]/}" ]] \
  || local_deploy_guard_fail "$STAGING_CP_CONTAINER is not attached to $LOCAL_NETWORK"

echo "local-deploy Docker daemon verified: runner-configured mTLS endpoint + $STAGING_CP_CONTAINER on $LOCAL_NETWORK"
