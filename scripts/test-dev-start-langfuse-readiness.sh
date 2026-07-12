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
RESET_LIB="$ROOT/scripts/lib/docker-reset.sh"
NUKE="$ROOT/scripts/nuke-and-rebuild.sh"

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
# Unanchored: #3946 moved this echo inside the `if [ "$FRESH" ]; …; else` branch
# (the non-fresh path), so it is now indented. The ordering invariant it stands
# for — stale containers are cleaned before dynamic ports are chosen — is
# unchanged; only its column-0 position moved.
cleanup_call_line=$(line_of 'Cleaning up previous local dev containers')
host_cleanup_fn_line=$(line_of '^cleanup_repo_host_processes\(\) \{')
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
[ "$host_cleanup_fn_line" -lt "$cleanup_fn_line" ] || fail "repo host-process cleanup must be defined before cleanup_dev_stack"
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
printf '%s\n' "$cleanup_body" | grep -Fq 'cleanup_repo_host_processes' \
  || fail "cleanup must stop stale repo-local platform/canvas host processes"
printf '%s\n' "$cleanup_body" | grep -Fq 'docker-compose.yml" down --remove-orphans' \
  || fail "cleanup must tear down the full compose stack, including platform fallback containers"
printf '%s\n' "$cleanup_body" | grep -Fq 'docker-compose.infra.yml" down --remove-orphans' \
  || fail "cleanup must tear down the infra-only compose stack"
printf '%s\n' "$cleanup_body" | grep -Fq 'docker rm -f molecule-core-langfuse-1' \
  || fail "cleanup must remove the manually-run Langfuse container"
if printf '%s\n' "$cleanup_body" | grep -Eq -- '--volumes|-v( |$)'; then
  fail "cleanup must not delete named volumes by default"
fi

host_cleanup_body=$(function_body cleanup_repo_host_processes)
repo_match_body=$(function_body repo_dev_pid_matches)
printf '%s\n' "$host_cleanup_body" | grep -Fq 'lsof -nP -iTCP -sTCP:LISTEN' \
  || fail "host cleanup must include stale listener PIDs, not just go/next parent wrappers"
printf '%s\n' "$host_cleanup_body" | grep -Fq 'pgrep -f' \
  || fail "host cleanup must include go run / next dev parent wrappers"
printf '%s\n' "$host_cleanup_body" | grep -Fq 'repo_dev_pid_matches' \
  || fail "host cleanup must filter candidate PIDs through repo cwd scoping"
printf '%s\n' "$repo_match_body" | grep -Fq 'ROOT/workspace-server' \
  || fail "host cleanup must be cwd-scoped to the repo workspace-server"
printf '%s\n' "$repo_match_body" | grep -Fq 'ROOT/canvas' \
  || fail "host cleanup must be cwd-scoped to the repo canvas"
printf '%s\n' "$host_cleanup_body" | grep -Fq 'pkill -TERM -P' \
  || fail "host cleanup must terminate child listener processes before parents"
printf '%s\n' "$host_cleanup_body" | grep -Fq 'kill -KILL' \
  || fail "host cleanup must force-kill stubborn stale repo-local processes"

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

# --fresh full-reset (opt-in, DESTRUCTIVE): must wipe named volumes AND purge the
# stale-image trap (locally-built template images + the localbuild clone cache),
# but ONLY on explicit opt-in. Each assertion below pins the SPECIFIC command, not
# a substring that a comment could satisfy, so it fails on the exact regression.
fresh_body=$(function_body fresh_reset)
[ -n "$fresh_body" ] || fail "--fresh must be implemented by a fresh_reset function"

# BOTH compose stacks must be down -v'd: the MinIO object-store volume (miniodata)
# is declared only in docker-compose.infra.yml, so wiping only the main stack
# silently preserves object-store state --fresh promises to wipe.
printf '%s\n' "$fresh_body" | grep -Eq 'docker-compose\.yml" down -v' \
  || fail "fresh_reset must 'down -v' the main compose stack (DB/Redis volumes)"
printf '%s\n' "$fresh_body" | grep -Eq 'docker-compose\.infra\.yml" down -v' \
  || fail "fresh_reset must 'down -v' the infra compose stack (miniodata/Langfuse volumes)"

# Template-image purge: match the actual mol_rm_by_filter command (label + rmi),
# not the bare 'molecule-local/' substring, which also appears in comments.
printf '%s\n' "$fresh_body" | grep -Eq 'mol_rm_by_filter "template images".*docker rmi -f' \
  || fail "fresh_reset must purge molecule-local template images via mol_rm_by_filter ... docker rmi -f"

# Build-clone cache: clear the default path AND the MOLECULE_LOCAL_BUILD_CACHE
# override localbuild.go honors first (else the stale-image trap survives --fresh).
printf '%s\n' "$fresh_body" | grep -Fq 'workspace-template-build' \
  || fail "fresh_reset must clear the default localbuild clone cache"
printf '%s\n' "$fresh_body" | grep -Fq 'MOLECULE_LOCAL_BUILD_CACHE' \
  || fail "fresh_reset must also clear the MOLECULE_LOCAL_BUILD_CACHE override dir"

# ws-* purge is delegated to the shared, UUID-scoped mol_purge_ws_objects helper.
printf '%s\n' "$fresh_body" | grep -Fq 'mol_purge_ws_objects' \
  || fail "fresh_reset must purge ws-* via the shared mol_purge_ws_objects helper"

# Shared lib: the ws-* filter MUST be UUID-scoped, never a bare '^ws-' prefix
# (which would delete an unrelated project's ws-* object on the same host).
[ -f "$RESET_LIB" ] || fail "scripts/lib/docker-reset.sh (shared teardown helpers) must exist"
grep -Eq "MOL_WS_UUID_RE='\^ws-\[0-9a-f\]\{8\}" "$RESET_LIB" \
  || fail "docker-reset.sh MOL_WS_UUID_RE must be scoped to ^ws-<8hex>-…, not a bare ^ws-"
grep -Fq 'mol_purge_ws_objects()' "$RESET_LIB" \
  || fail "docker-reset.sh must define mol_purge_ws_objects"
# The purge helper must FILTER WITH $MOL_WS_UUID_RE — not an inline pattern. Pins
# the scope to the (audited) variable, so a broadened literal like "^ws-.*" that
# slips past the literal check below still fails this assertion.
# shellcheck disable=SC2016  # grepping for the LITERAL text "$MOL_WS_UUID_RE"
grep -Fq '"$MOL_WS_UUID_RE"' "$RESET_LIB" \
  || fail "mol_purge_ws_objects must pass \$MOL_WS_UUID_RE as the filter (not an inline ^ws- pattern)"
# Neither reset script/lib may reintroduce a bare '^ws-' literal (a quoted
# '^ws-'/"^ws-" — the buggy prefix) or GNU-only 'xargs -r'. Anchored with ^[^#]*
# so a quoted '^ws-' or 'xargs -r' in COMMENT prose can't false-fire; only real
# code (before any #) is checked.
for _f in "$DEV_START" "$NUKE" "$RESET_LIB"; do
  if grep -Eq "^[^#]*[\"']\^ws-[\"']" "$_f"; then
    fail "$(basename "$_f") uses a bare '^ws-' literal in code (must be the scoped MOL_WS_UUID_RE)"
  fi
  # Match piped USAGE (`| xargs -r`) in code, not backtick prose in comments.
  if grep -Eq "^[^#]*\| *xargs +-r" "$_f"; then
    fail "$(basename "$_f") pipes into 'xargs -r' (BSD/macOS xargs rejects it)"
  fi
done

# nuke-and-rebuild.sh must reuse the shared helper, not its old buggy inline purge.
[ -f "$NUKE" ] || fail "scripts/nuke-and-rebuild.sh must exist"
grep -Fq 'docker-reset.sh' "$NUKE" \
  || fail "nuke-and-rebuild.sh must source the shared scripts/lib/docker-reset.sh"
grep -Fq 'mol_purge_ws_objects' "$NUKE" \
  || fail "nuke-and-rebuild.sh must purge ws-* via mol_purge_ws_objects (UUID-scoped)"

# The destructive wipe must NEVER fire on Ctrl-C: the exit-trap handler cleanup()
# must invoke cleanup_dev_stack (volume-preserving), never fresh_reset.
cleanup_trap_body=$(function_body cleanup)
if printf '%s\n' "$cleanup_trap_body" | grep -Fq 'fresh_reset'; then
  fail "exit-trap cleanup() must NOT call fresh_reset — --fresh wipe must never fire on Ctrl-C"
fi

# fresh_reset must run before dynamic port selection on the --fresh path (same
# ordering invariant the non-fresh cleanup echo already guards above).
fresh_call_line=$(line_of '^[[:space:]]+fresh_reset$')
[ "$fresh_call_line" -lt "$pick_port_line" ] \
  || fail "fresh_reset must run before dynamic host-port selection on the --fresh path"

# Both flag spellings arm the reset (matched independently, not as one literal so
# a harmless reorder doesn't break the test); unknown flags must still fail loud.
for _spell in '--fresh' '--remove-volumes'; do
  awk '/^for arg in "\$@"; do$/{f=1} f{print} f&&/^done$/{exit}' "$DEV_START" \
    | grep -Fq -- "$_spell" || fail "arg parser must accept $_spell"
done
awk '/^for arg in "\$@"; do$/{f=1} f{print} f&&/^done$/{exit}' "$DEV_START" \
  | grep -Fq 'FRESH=1' || fail "the fresh flags must set FRESH=1"
grep -Fq "unknown option '" "$DEV_START" \
  || fail "arg parser must fail loud on an unknown flag, not silently swallow it"

echo "PASS: dev-start.sh --fresh (aka --remove-volumes) is a full opt-in reset; exit-trap keeps data"
