#!/bin/sh
# Startup/lifecycle helpers. Sourcing this file has no side effects.

dev_default_platform_mode() {
    case "$(uname -s)" in
        MINGW*|MSYS*|CYGWIN*) echo docker ;;
        *) echo host ;;
    esac
}

dev_service_alive() {
    case "$1" in
        host) kill -0 "$2" 2>/dev/null ;;
        docker)
            # Catch crash loops even when inspect happens between restarts.
            [ "$(docker inspect -f '{{.State.Status}} {{.RestartCount}} {{.State.OOMKilled}}' "$2" 2>/dev/null)" = "running 0 false" ]
            ;;
        *) return 1 ;;
    esac
}

dev_service_logs() {
    # Preserve container logs before the exit trap removes the container.
    if [ "$1" = docker ]; then
        docker logs --tail 80 "$2" >> "$3" 2>&1 || true
    fi
    echo "    Last startup output ($3):" >&2
    tail -n 80 "$3" >&2 || true
}

dev_wait_http() (
    # name, URL, host|docker, PID|container ID, log path, timeout seconds
    name=$1 url=$2 mode=$3 identity=$4 logfile=$5 timeout=$6
    case "$timeout" in
        ''|*[!0-9]*|0) echo "Invalid startup timeout: $timeout" >&2; return 1 ;;
    esac
    start=$(date +%s)
    deadline=$((start + timeout))
    next_progress=$((start + 30))
    echo "    Waiting for $name ($timeout seconds)..."
    while :; do
        if ! dev_service_alive "$mode" "$identity"; then
            echo "    $name exited or restarted before becoming ready." >&2
            dev_service_logs "$mode" "$identity" "$logfile"
            return 1
        fi
        now=$(date +%s)
        remaining=$((deadline - now))
        [ "$remaining" -gt 0 ] || break
        request_timeout=2
        [ "$remaining" -ge 2 ] || request_timeout=$remaining
        code=$(curl --noproxy '*' --connect-timeout 1 --max-time "$request_timeout" \
            -s -o /dev/null -w '%{http_code}' "$url" 2>/dev/null) || code=000
        now=$(date +%s)
        # Check liveness again: another listener must not mask our child dying.
        if [ "$code" = 200 ] && [ "$now" -le "$deadline" ] && dev_service_alive "$mode" "$identity"; then
            echo "    $name ready (t+$((now - start))s)"
            return 0
        fi
        [ "$now" -lt "$deadline" ] || break
        if [ "$now" -ge "$next_progress" ]; then
            echo "    still waiting for $name — $((now - start))s elapsed"
            next_progress=$((now + 30))
        fi
        sleep 1
    done
    echo "    $name did not become ready within ${timeout}s." >&2
    dev_service_logs "$mode" "$identity" "$logfile"
    return 1
)

dev_stop_process() (
    [ -n "$1" ] || return 0
    kill -0 "$1" 2>/dev/null || return 0
    case "$(uname -s)" in
        MINGW*|MSYS*|CYGWIN*)
            # MSYS PIDs differ from Windows PIDs. Stop only this owned tree,
            # including Next's worker, without killing other node processes.
            winpid=$(ps -p "$1" | awk 'NR == 2 { print $4 }')
            if [ -n "$winpid" ]; then
                MSYS2_ARG_CONV_EXCL='*' taskkill.exe /PID "$winpid" /T /F >/dev/null 2>&1 || true
            fi
            ;;
        *)
            pkill -TERM -P "$1" 2>/dev/null || true
            ;;
    esac
    kill "$1" 2>/dev/null || true
)

dev_compose_platform() (
    # Host URLs belong to host processes. Translate only local-stack URLs;
    # deliberate external database, Redis, and object-store overrides survive.
    case "${DATABASE_URL:-}" in
        ""|*localhost:*|*127.0.0.1:*|*\[::1\]:*|*::1:*)
            DATABASE_URL="postgres://${POSTGRES_USER:-dev}:${POSTGRES_PASSWORD:-dev}@postgres:5432/${POSTGRES_DB:-molecule}?sslmode=disable" ;;
    esac
    case "${REDIS_URL:-}" in
        ""|*localhost:*|*127.0.0.1:*|*\[::1\]:*|*::1:*) REDIS_URL=redis://redis:6379 ;;
    esac
    case "${MOLECULE_WORKSPACE_DATA_ENDPOINT:-}" in
        ""|http://localhost:*|http://127.0.0.1:*|http://\[::1\]:*|http://::1:*)
            MOLECULE_WORKSPACE_DATA_ENDPOINT=http://minio:9000 ;;
    esac
    export DATABASE_URL REDIS_URL MOLECULE_WORKSPACE_DATA_ENDPOINT
    docker compose -f "$ROOT/docker-compose.yml" -f "$ROOT/docker-compose.start.yml" "$@"
)
