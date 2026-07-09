#!/usr/bin/env bash
# Regression test for dev-start.sh local-stack readiness ordering.
#
# The Langfuse container runs migrations against ClickHouse's native port
# before it starts serving HTTP. If dev-start.sh starts Langfuse as soon as
# compose reports ClickHouse "healthy", the app can exit before :9000 is ready
# while the script still prints the success banner.
#
# Run: bash scripts/test-dev-start-langfuse-readiness.sh

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEV_START="$ROOT/scripts/dev-start.sh"
COMPOSE="$ROOT/docker-compose.infra.yml"
COMPOSE_MAIN="$ROOT/docker-compose.yml"
LOCAL_E2E_COMPOSE="$ROOT/local-e2e/docker-compose.yml"
LOCAL_CANARY="$ROOT/local-e2e/scripts/run-canary.sh"
LOCAL_TENANT_SMOKE="$ROOT/scripts/local-tenant-smoke.sh"
WORKFLOWS_DIR="$ROOT/.gitea/workflows"
SETUP="$ROOT/infra/scripts/setup.sh"

fail() {
  echo "FAIL: $1" >&2
  exit 1
}

line_of() {
  local pattern="$1"
  local line
  line=$(grep -nE "$pattern" "$DEV_START" | head -1 | cut -d: -f1 || true)
  [ -n "$line" ] || fail "missing pattern: $pattern"
  printf '%s\n' "$line"
}

function_body() {
  local name="$1"
  awk -v name="$name" '
    $0 == name "() {" { in_fn=1 }
    in_fn { print }
    in_fn && $0 == "}" { exit }
  ' "$DEV_START"
}

# shellcheck disable=SC2016
compose_project_line=$(line_of '^COMPOSE_PROJECT_NAME="\$\{COMPOSE_PROJECT_NAME:-molecule-core\}"')
cleanup_fn_line=$(line_of '^cleanup_dev_stack\(\) \{')
cleanup_call_line=$(line_of '^echo "==> Cleaning up previous local dev containers"')
pick_port_line=$(line_of '^pick_port\(\) \{')
# shellcheck disable=SC2016
setup_line=$(line_of '^"\$ROOT/infra/scripts/setup\.sh"$')
minio_port_line=$(line_of '^MOLECULE_MINIO_HOST_PORT=\$\{MOLECULE_MINIO_HOST_PORT:-\$\(pick_port 9000\)\}')
minio_export_line=$(line_of 'MOLECULE_MINIO_HOST_PORT MOLECULE_MINIO_CONSOLE_HOST_PORT')
object_backend_line=$(line_of 'MOLECULE_OBJECT_STORE_BACKEND="\$\{MOLECULE_OBJECT_STORE_BACKEND:-minio\}"')
workspace_endpoint_line=$(line_of 'MOLECULE_WORKSPACE_DATA_ENDPOINT="http://127\.0\.0\.1:\$\{MOLECULE_MINIO_HOST_PORT\}"')
# shellcheck disable=SC2016
token_file_line=$(line_of '\$HOME/\.molecule-ai/gitea-token')
# shellcheck disable=SC2016
template_token_line=$(line_of 'export MOLECULE_TEMPLATE_REPO_TOKEN="\$MOLECULE_GITEA_TOKEN"')
wait_ch_line=$(line_of '^wait_for_langfuse_clickhouse_native$')
docker_run_line=$(line_of '^(if )?docker run -d --name molecule-core-langfuse-1')
wait_http_line=$(line_of '^wait_for_langfuse_http$')
banner_line=$(line_of 'Molecule AI dev environment ready')

[ "$compose_project_line" -lt "$cleanup_fn_line" ] || fail "COMPOSE_PROJECT_NAME must be stable before cleanup runs"
[ "$cleanup_call_line" -lt "$pick_port_line" ] || fail "dev-start.sh must clean stale local containers before choosing dynamic ports"
[ "$token_file_line" -lt "$setup_line" ] || fail "local Gitea token bootstrap must happen before setup.sh clones private repos"
[ "$template_token_line" -lt "$setup_line" ] || fail "MOLECULE_TEMPLATE_REPO_TOKEN export must happen before platform/infra startup"
[ "$minio_port_line" -lt "$setup_line" ] || fail "MinIO dynamic host port must be chosen before setup.sh"
[ "$minio_export_line" -lt "$setup_line" ] || fail "MinIO host ports must be exported before docker compose starts infra"
[ "$object_backend_line" -lt "$setup_line" ] || fail "local object-store backend must default to minio before platform/infra startup"
[ "$workspace_endpoint_line" -lt "$setup_line" ] || fail "workspace-data endpoint must point at the dynamic local MinIO port before setup.sh"
[ "$setup_line" -lt "$wait_ch_line" ] || fail "ClickHouse native wait must happen after setup.sh"
[ "$wait_ch_line" -lt "$docker_run_line" ] || fail "ClickHouse native wait must happen before docker run starts Langfuse"
[ "$docker_run_line" -lt "$wait_http_line" ] || fail "Langfuse HTTP wait must happen after docker run"
[ "$wait_http_line" -lt "$banner_line" ] || fail "Langfuse HTTP wait must happen before the ready banner"

# shellcheck disable=SC2016
grep -Fq 'MOLECULE_GITEA_TOKEN=$(tr -d' "$DEV_START" \
  || fail "dev-start.sh must read the local Gitea token without echoing it"
# shellcheck disable=SC2016
if grep -q 'MOLECULE_GITEA_TOKEN=.*>> "\$ENV_FILE' "$DEV_START"; then
  fail "dev-start.sh must not persist the local Gitea token to .env"
fi

cleanup_body=$(function_body cleanup_dev_stack)
printf '%s\n' "$cleanup_body" | grep -Fq 'docker-compose.yml" down --remove-orphans' \
  || fail "cleanup must tear down the full compose stack, including platform fallback containers"
printf '%s\n' "$cleanup_body" | grep -Fq 'docker-compose.infra.yml" down --remove-orphans' \
  || fail "cleanup must tear down the infra-only compose stack"
printf '%s\n' "$cleanup_body" | grep -Fq 'docker rm -f molecule-core-langfuse-1' \
  || fail "cleanup must remove the manually-run Langfuse container"
if printf '%s\n' "$cleanup_body" | grep -Eq -- '--volumes|-v( |$)'; then
  fail "cleanup must not delete named volumes by default"
fi

ch_body=$(function_body wait_for_langfuse_clickhouse_native)
printf '%s\n' "$ch_body" | grep -q 'clickhouse-client' \
  || fail "ClickHouse native wait must probe clickhouse-client, not only HTTP /ping"
printf '%s\n' "$ch_body" | grep -q 'langfuse-clickhouse' \
  || fail "ClickHouse native wait must target the Langfuse ClickHouse service"

http_body=$(function_body wait_for_langfuse_http)
printf '%s\n' "$http_body" | grep -q 'docker inspect' \
  || fail "Langfuse HTTP wait must detect a container that exits before readiness"
# shellcheck disable=SC2016
printf '%s\n' "$http_body" | grep -q '\$LANGFUSE_HOST' \
  || fail "Langfuse HTTP wait must probe the announced Langfuse host URL"
# shellcheck disable=SC2016
grep -Fq -- '-p "127.0.0.1:${MOLECULE_LANGFUSE_HOST_PORT}:3000"' "$DEV_START" \
  || fail "direct Langfuse docker run must be loopback-bound"

echo "PASS: dev-start.sh gates the ready banner on Langfuse readiness"
echo "PASS: dev-start.sh wires local private-repo tokens without persisting them"
echo "PASS: dev-start.sh wires the local SDK object-store backend to MinIO"

grep -Eq '^  minio:' "$COMPOSE" \
  || fail "docker-compose.infra.yml must declare the MinIO service"
grep -Eq '^  minio-init:' "$COMPOSE" \
  || fail "docker-compose.infra.yml must declare the MinIO bucket bootstrap service"
grep -Fq 'minio/minio@sha256:' "$COMPOSE" \
  || fail "MinIO image must be digest-pinned"
grep -Fq 'minio/mc@sha256:' "$COMPOSE" \
  || fail "MinIO client image must be digest-pinned"
# shellcheck disable=SC2016
grep -Fq '127.0.0.1:${MOLECULE_MINIO_HOST_PORT:-9000}:9000' "$COMPOSE" \
  || fail "MinIO S3 API must be loopback-bound in local dev"
# shellcheck disable=SC2016
grep -Fq '127.0.0.1:${MOLECULE_MINIO_CONSOLE_HOST_PORT:-9001}:9001' "$COMPOSE" \
  || fail "MinIO console must be loopback-bound in local dev"
# shellcheck disable=SC2016
grep -Fq '127.0.0.1:${MOLECULE_PG_HOST_PORT:-5432}:5432' "$COMPOSE" \
  || fail "Postgres must be loopback-bound in local dev"
# shellcheck disable=SC2016
grep -Fq '127.0.0.1:${MOLECULE_REDIS_HOST_PORT:-6379}:6379' "$COMPOSE" \
  || fail "Redis must be loopback-bound in local dev"
# shellcheck disable=SC2016
grep -Fq '127.0.0.1:${MOLECULE_TEMPORAL_HOST_PORT:-7233}:7233' "$COMPOSE" \
  || fail "Temporal gRPC must be loopback-bound in local dev"
# shellcheck disable=SC2016
grep -Fq '127.0.0.1:${MOLECULE_TEMPORAL_UI_HOST_PORT:-8233}:8080' "$COMPOSE" \
  || fail "Temporal UI must be loopback-bound in local dev"
grep -Fq 'MOLECULE_WORKSPACE_DATA_BUCKET' "$COMPOSE" \
  || fail "MinIO bootstrap must use the workspace-data bucket env"
grep -Eq '^  miniodata:' "$COMPOSE" \
  || fail "docker-compose.infra.yml must persist MinIO data in a named volume"
# shellcheck disable=SC2016
grep -Fq '127.0.0.1:${MOLECULE_LANGFUSE_HOST_PORT:-3001}:3000' "$COMPOSE_MAIN" \
  || fail "compose Langfuse fallback must be loopback-bound and dynamic"
# shellcheck disable=SC2016
grep -Fq '127.0.0.1:${PLATFORM_PUBLISH_PORT:-8080}:${PLATFORM_PORT:-8080}' "$COMPOSE_MAIN" \
  || fail "compose platform fallback must be loopback-bound and dynamic"
# shellcheck disable=SC2016
grep -Fq '127.0.0.1:${CANVAS_PUBLISH_PORT:-3000}:${CANVAS_PORT:-3000}' "$COMPOSE_MAIN" \
  || fail "compose canvas fallback must be loopback-bound and dynamic"
awk '
  /^networks:/ { in_networks=1 }
  in_networks && /^  molecule-core-net:/ { in_net=1 }
  in_net && /external: true/ { found=1 }
  in_networks && /^volumes:/ { in_networks=0; in_net=0 }
  END { exit found ? 0 : 1 }
' "$COMPOSE_MAIN" \
  || fail "docker-compose.yml must mark molecule-core-net external to match included infra compose"

grep -Fq '/minio/health/ready' "$SETUP" \
  || fail "setup.sh must wait for MinIO readiness"
grep -Fq 'run --rm minio-init' "$SETUP" \
  || fail "setup.sh must create the MinIO workspace-data bucket"

case_block() {
  local var_name="$1"
  awk -v var_name="$var_name" '
    $0 ~ "^case \"\\$\\{" var_name ":-\\}\" in" { in_case=1 }
    in_case { print }
    in_case && $0 == "esac" { exit }
  ' "$DEV_START"
}

database_case=$(case_block DATABASE_URL)
printf '%s\n' "$database_case" | grep -Fq '*localhost:*' \
  || fail "DATABASE_URL override must catch stale localhost dynamic ports"
printf '%s\n' "$database_case" | grep -Fq '*127.0.0.1:*' \
  || fail "DATABASE_URL override must catch stale 127.0.0.1 dynamic ports"

redis_case=$(case_block REDIS_URL)
printf '%s\n' "$redis_case" | grep -Fq '*localhost:*' \
  || fail "REDIS_URL override must catch stale localhost dynamic ports"
printf '%s\n' "$redis_case" | grep -Fq '*127.0.0.1:*' \
  || fail "REDIS_URL override must catch stale 127.0.0.1 dynamic ports"

echo "PASS: dev-start.sh regenerates stale local DATABASE_URL/REDIS_URL values"
echo "PASS: dev-start.sh cleans stale local containers before choosing dynamic ports"
echo "PASS: compose publishes local services on loopback dynamic ports"

# Same-class local harness checks: no fixed all-interface host ports in the
# one-off canary/smoke stacks.
# shellcheck disable=SC2016
grep -Fq '127.0.0.1:${RUNTIME_PORT:-18000}:8000' "$LOCAL_E2E_COMPOSE" \
  || fail "local-e2e runtime port must be loopback-bound"
# shellcheck disable=SC2016
grep -Fq 'export RUNTIME_PORT="${RUNTIME_PORT:-$(pick_port 18000)}"' "$LOCAL_CANARY" \
  || fail "local-e2e canary must pick a free runtime host port by default"
grep -Fq 'down --volumes --remove-orphans' "$LOCAL_CANARY" \
  || fail "local-e2e canary must clean stale project containers before startup"
# shellcheck disable=SC2016
grep -Fq 'TENANT_HOST_PORT="${TENANT_HOST_PORT:-$(pick_port 18080)}"' "$LOCAL_TENANT_SMOKE" \
  || fail "local tenant smoke must pick a free tenant host port by default"
# shellcheck disable=SC2016
grep -Fq 'MEMORY_PLUGIN_HOST_PORT="${MEMORY_PLUGIN_HOST_PORT:-$(pick_port 19100)}"' "$LOCAL_TENANT_SMOKE" \
  || fail "local tenant smoke must pick a free memory-plugin host port by default"
# shellcheck disable=SC2016
grep -Fq -- '-p "127.0.0.1:${TENANT_HOST_PORT}:8080" -p "127.0.0.1:${MEMORY_PLUGIN_HOST_PORT}:9100"' "$LOCAL_TENANT_SMOKE" \
  || fail "local tenant smoke must loopback-bind tenant and memory-plugin ports"
# shellcheck disable=SC2016
grep -Fq 'http://127.0.0.1:${TENANT_HOST_PORT}${HEALTH_PATH}' "$LOCAL_TENANT_SMOKE" \
  || fail "local tenant smoke must poll the selected tenant host port"
# shellcheck disable=SC2016
grep -Fq 'http://127.0.0.1:${MEMORY_PLUGIN_HOST_PORT}/v1/health' "$LOCAL_TENANT_SMOKE" \
  || fail "local tenant smoke must poll the selected memory-plugin host port"

echo "PASS: local e2e/smoke harnesses use loopback dynamic host ports"

if grep -REn -- '-p 0:(5432|6379)' "$WORKFLOWS_DIR" >/tmp/molecule-port-hygiene-grep.txt; then
  cat /tmp/molecule-port-hygiene-grep.txt >&2
  fail "CI docker Postgres/Redis publishes must be loopback-bound, not all-interface -p 0"
fi
grep -REq -- '-p 127\.0\.0\.1::5432' "$WORKFLOWS_DIR" \
  || fail "CI workflows should use loopback-bound ephemeral Postgres publishes"
grep -REq -- '-p 127\.0\.0\.1::6379' "$WORKFLOWS_DIR" \
  || fail "CI workflows should use loopback-bound ephemeral Redis publishes"

echo "PASS: CI docker Postgres/Redis publishes are loopback-bound ephemeral ports"
