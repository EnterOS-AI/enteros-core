#!/usr/bin/env bash
# Give Chromium its own network namespace. Host Docker/netlink changes must not
# flush its socket pools (core#5106). The test client exposes its loopback servers
# over Playwright's connection, preserving localhost origins and dynamic ports.
# Usage: start-playwright-server.sh NAME [CANVAS_DIR]
# Prints the WebSocket endpoint; the caller owns successful-container cleanup.
set -euo pipefail

[[ $# -ge 1 && $# -le 2 && "$1" =~ ^[a-zA-Z0-9][a-zA-Z0-9_.-]*$ ]] || {
  echo "usage: $0 NAME [CANVAS_DIR]" >&2
  exit 2
}
name="$1"
canvas="${2:-$(cd "$(dirname "$0")/../../canvas" && pwd)}"
package="$canvas/node_modules/playwright-core"
version="$(node -p 'require(process.argv[1]).version' "$package/package.json")"
[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || {
  echo "::error::Unsupported Playwright version: $version" >&2
  exit 1
}

# The installed lockfile version selects both protocol code and browser image.
# Copy the installed package instead of downloading a second npm dependency tree.
# No workspace, Docker socket, host filesystem, or host network is mounted.
id="$(docker create --init --name "$name" --network bridge --shm-size 1g \
  --label "molecule.ci.run=${GITHUB_RUN_ID:-local}" \
  --user pwuser --workdir /home/pwuser -p 127.0.0.1::3000 \
  "mcr.microsoft.com/playwright:v${version}-noble" \
  node /opt/playwright-core/cli.js run-server --host 0.0.0.0 --port 3000)"
cleanup_failed_start() {
  docker logs "$id" >&2 || true
  timeout 30 docker rm -fv "$id" >/dev/null 2>&1 || true
}
trap cleanup_failed_start EXIT
docker cp "$package" "$id:/opt/playwright-core"
docker start "$id" >/dev/null
[[ "$(docker inspect -f '{{.HostConfig.NetworkMode}}' "$id")" == bridge ]] || {
  echo "::error::Playwright must run in an isolated bridge network namespace" >&2
  exit 1
}
port="$(docker port "$id" 3000/tcp | sed -n 's/^127\.0\.0\.1://p')"
[[ "$port" =~ ^[0-9]+$ ]] || {
  echo "::error::Cannot resolve Playwright's loopback port" >&2
  exit 1
}
for _ in $(seq 1 30); do
  if curl --noproxy '*' -fsS --max-time 1 "http://127.0.0.1:$port/" >/dev/null 2>&1; then
    trap - EXIT
    printf 'ws://127.0.0.1:%s/\n' "$port"
    exit 0
  fi
  [[ "$(docker inspect -f '{{.State.Running}}' "$id")" == true ]] || break
  sleep 1
done
echo "::error::Isolated Playwright server did not become ready" >&2
exit 1
