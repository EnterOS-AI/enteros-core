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
# shellcheck disable=SC2016
setup_line=$(line_of '^"\$ROOT/infra/scripts/setup\.sh"$')
# shellcheck disable=SC2016
token_file_line=$(line_of '\$HOME/\.molecule-ai/gitea-token')
# shellcheck disable=SC2016
template_token_line=$(line_of 'export MOLECULE_TEMPLATE_REPO_TOKEN="\$MOLECULE_GITEA_TOKEN"')
wait_ch_line=$(line_of '^wait_for_langfuse_clickhouse_native$')
docker_run_line=$(line_of '^(if )?docker run -d --name molecule-core-langfuse-1')
wait_http_line=$(line_of '^wait_for_langfuse_http$')
banner_line=$(line_of 'Molecule AI dev environment ready')

[ "$token_file_line" -lt "$setup_line" ] || fail "local Gitea token bootstrap must happen before setup.sh clones private repos"
[ "$template_token_line" -lt "$setup_line" ] || fail "MOLECULE_TEMPLATE_REPO_TOKEN export must happen before platform/infra startup"
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

echo "PASS: dev-start.sh gates the ready banner on Langfuse readiness"
echo "PASS: dev-start.sh wires local private-repo tokens without persisting them"

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
