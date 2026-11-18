#!/usr/bin/env bash
# Behavioral startup regression tests; no Docker daemon or Go build required.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
. "$ROOT/scripts/lib/dev-startup.sh"
test_dir=$(mktemp -d "${TMPDIR:-/tmp}/molecule-startup-test.XXXXXX")
http_pid=
cleanup_test() {
    dev_stop_process "$http_pid"
    rm -f "$test_dir"/*
    rmdir "$test_dir"
}
trap cleanup_test EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

# Platform selection is determined by the execution environment, not by
# whether a compiler happens to be installed on a restricted Windows host.
for os in MINGW64_NT-10.0 MSYS_NT-10.0 CYGWIN_NT-10.0 Linux Darwin; do
    uname() { echo "$os"; }
    expected=host
    case "$os" in MINGW*|MSYS*|CYGWIN*) expected=docker ;; esac
    [ "$(dev_default_platform_mode)" = "$expected" ] || fail "mode on $os"
done
unset -f uname

# Verify config translation and external overrides without touching a daemon.
(
    docker() {
        printf '%s\n' "$DATABASE_URL" "$REDIS_URL" "$MOLECULE_WORKSPACE_DATA_ENDPOINT" "$@"
    }
    DATABASE_URL=postgres://dev:dev@127.0.0.1:15432/molecule
    REDIS_URL=redis://localhost:16379
    MOLECULE_WORKSPACE_DATA_ENDPOINT=http://127.0.0.1:19000
    dev_compose_platform config > "$test_dir/config"
    grep -Fxq 'postgres://dev:dev@postgres:5432/molecule?sslmode=disable' "$test_dir/config"
    grep -Fxq 'redis://redis:6379' "$test_dir/config"
    grep -Fxq 'http://minio:9000' "$test_dir/config"
    [ "$MOLECULE_WORKSPACE_DATA_ENDPOINT" = http://127.0.0.1:19000 ]
    DATABASE_URL=postgres://db.example/custom
    REDIS_URL=rediss://redis.example:6380
    MOLECULE_WORKSPACE_DATA_ENDPOINT=https://objects.example
    dev_compose_platform config > "$test_dir/config"
    grep -Fxq "$DATABASE_URL" "$test_dir/config"
    grep -Fxq "$REDIS_URL" "$test_dir/config"
    grep -Fxq "$MOLECULE_WORKSPACE_DATA_ENDPOINT" "$test_dir/config"
) || fail "host/container configuration translation"

cat > "$test_dir/http.cjs" <<'JS'
const http = require('node:http');
const fs = require('node:fs');
const started = Date.now();
const server = http.createServer((req, res) => {
  if (req.url === '/hang') return;
  if (req.url === '/redirect') {
    res.writeHead(302, { Location: '/ready' }); return res.end();
  }
  if (req.url === '/slow' && Date.now() - started < 1800) {
    res.writeHead(503); return res.end();
  }
  res.writeHead(req.url === '/error' ? 500 : 200); res.end('ready');
});
server.listen(0, '127.0.0.1', () => fs.writeFileSync(process.argv[2], String(server.address().port)));
JS
node "$test_dir/http.cjs" "$test_dir/port" > "$test_dir/server.log" 2>&1 &
http_pid=$!
for _ in {1..50}; do
    [ ! -s "$test_dir/port" ] || break
    sleep .1
done
[ -s "$test_dir/port" ] || fail "HTTP fixture did not start"
url="http://127.0.0.1:$(cat "$test_dir/port")"
dev_wait_http delayed "$url/slow" host "$http_pid" "$test_dir/server.log" 8

# A hung connection must consume the wall-clock budget, not N unbounded curls.
started=$(date +%s)
if dev_wait_http hung "$url/hang" host "$http_pid" "$test_dir/server.log" 3 > "$test_dir/result" 2>&1; then
    fail "hung server was ready"
fi
elapsed=$(($(date +%s) - started))
[ "$elapsed" -le 5 ] || fail "3s deadline took ${elapsed}s"
grep -q 'within 3s' "$test_dir/result" || fail "missing timeout diagnostic"
for route in error redirect; do
    if dev_wait_http "$route" "$url/$route" host "$http_pid" "$test_dir/server.log" 1 > "$test_dir/result" 2>&1; then
        fail "$route was accepted as ready"
    fi
done

# An unrelated healthy listener cannot hide an already-exited child.
(exit 1) &
dead_pid=$!
wait "$dead_pid" || true
if dev_wait_http dead "$url/ready" host "$dead_pid" "$test_dir/server.log" 10 > "$test_dir/result" 2>&1; then
    fail "dead process was accepted as ready"
fi
grep -q 'exited or restarted' "$test_dir/result" || fail "missing exit diagnostic"

(
    docker() {
        case "$1" in
            inspect) echo "$state" ;;
            logs) echo 'fatal: migration failed' ;;
        esac
    }
    state='running 0 false'
    dev_wait_http container "$url/ready" docker test-container "$test_dir/docker.log" 3
    for state in 'exited 0 false' 'restarting 1 false' 'running 2 false' 'running 0 true'; do
        if dev_wait_http container "$url/ready" docker test-container "$test_dir/docker.log" 3 > "$test_dir/result" 2>&1; then
            fail "container $state was accepted as ready"
        fi
        grep -q 'fatal: migration failed' "$test_dir/docker.log" || fail "lost container logs"
    done
)

# Exercise the real CLI parser without invoking startup or its cleanup trap.
bash "$ROOT/scripts/dev-start.sh" --docker-platform --help >/dev/null
bash "$ROOT/scripts/dev-start.sh" --host-platform --help >/dev/null
if bash "$ROOT/scripts/dev-start.sh" --host-platform --docker-platform > "$test_dir/result" 2>&1; then
    fail "conflicting runtime flags accepted"
fi
echo 'PASS: platform selection, endpoint parity, delayed readiness, wall-clock deadlines, failed/restarting processes, diagnostics, CLI validation'
