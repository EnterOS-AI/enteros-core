#!/usr/bin/env bash
# dind.sh — per-job Docker-in-Docker isolation for the harness (task #78).
#
# The harness runs a docker-compose stack (cp-stub, tenant-alpha/beta, postgres,
# redis, cf-proxy) on the CI runner's SHARED long-lived docker daemon. A
# cancelled/SIGKILLed run orphans the fixed-name harness-* containers + volumes
# that no teardown can reliably reap, and two concurrent runs collide on the
# fixed compose project + ports — the chronic non-hermetic "Harness Replays"
# reds. The robust fix (principal's call; a scheduled sweeper was rejected as a
# band-aid) is to run the whole stack inside a DISPOSABLE per-job docker:dind
# daemon: everything lives inside it and dies with one atomic `docker rm -f`,
# even on cancel — leaks become structurally impossible. Feasibility proven by
# .gitea/workflows/dind-smoke.yml.
#
#   dind.sh up    start a disposable dind, wait healthy, and export to
#                 $GITHUB_ENV: DOCKER_HOST/DOCKER_TLS_VERIFY/DOCKER_CERT_PATH so
#                 every `docker compose` targets the ISOLATED daemon; BASE +
#                 CP_STUB_BASE so the replays' curls reach the harness ports the
#                 nested compose publishes (forwarded off the dind to host
#                 loopback); HARNESS_BIND_ADDR=0.0.0.0 so cf-proxy binds all dind
#                 interfaces (reachable through the forward).
#   dind.sh down  docker rm -fv the ONE dind — destroys the whole nested topology.
#
# The harness *.sh (up/down/seed/run-all-replays) need NO changes: they call bare
# `docker compose`, which follows DOCKER_HOST, and read BASE/CP_STUB_BASE from env.

set -uo pipefail
CMD="${1:-}"
# Mirrored into OUR registry (digest-identical to Hub's docker:27-dind) so a
# cold runner never spends Docker Hub's ~100/6h ANONYMOUS pull budget just to
# start the nested daemon. Hub's cap reds every open PR at once when it trips.
# Override with DIND_IMAGE=docker:27-dind to go back to Hub.
DIND_IMAGE="${DIND_IMAGE:-registry.moleculesai.app/molecule-ai/docker:27-dind}"
# Run-scoped, deterministic name so `down` finds it without carrying state.
NS="${DIND_NS:-${GITHUB_RUN_ID:-local}-${GITHUB_JOB:-harness}}"
DIND="dind-harness-${NS}"
CERTS_VOL="${DIND}-certs"
CERTDIR="${GITHUB_WORKSPACE:-$PWD}/.dind-certs-${NS}"

ephemeral_port() {  # $1=container-port/tcp → host loopback port
  docker port "$DIND" "$1" 2>/dev/null | awk -F: '/127\.0\.0\.1:/ {print $2; exit}' \
    || docker port "$DIND" "$1" 2>/dev/null | head -1 | awk -F: '{print $NF}'
}

up() {
  docker info >/dev/null 2>&1 || { echo "::error::docker daemon not reachable"; exit 2; }
  docker rm -fv "$DIND" >/dev/null 2>&1 || true
  docker volume rm "$CERTS_VOL" >/dev/null 2>&1 || true
  docker volume create "$CERTS_VOL" >/dev/null
  # --privileged is REQUIRED for a nested dockerd. If denied, the runner forbids
  # privileged → this approach needs the rootless-dind fallback (fail loud). We
  # also publish the harness's cf-proxy(8080) + cp-stub(9090) ports off the dind
  # to host loopback NOW, so once the nested compose binds them (0.0.0.0 inside
  # the dind) the job reaches them via 127.0.0.1:<ephemeral> — the same host-
  # loopback idiom the pg/redis steps already use, no cross-network wiring.
  if ! docker run -d --name "$DIND" --privileged \
      -e DOCKER_TLS_CERTDIR=/certs -v "${CERTS_VOL}:/certs" \
      -p 127.0.0.1::2376 -p 127.0.0.1::8080 -p 127.0.0.1::9090 \
      "$DIND_IMAGE" >/dev/null; then
    echo "::error::'docker run --privileged' denied on this runner — the per-job dind isolation needs the rootless-dind fallback (task #78)."
    exit 1
  fi
  local api_port; api_port="$(ephemeral_port 2376/tcp)"
  local http_port; http_port="$(ephemeral_port 8080/tcp)"
  local cp_port; cp_port="$(ephemeral_port 9090/tcp)"
  [ -n "$api_port" ] && [ -n "$http_port" ] && [ -n "$cp_port" ] || {
    echo "::error::dind did not publish all expected ports (2376=$api_port 8080=$http_port 9090=$cp_port)"; docker logs "$DIND" 2>&1 | tail -40; exit 1; }
  rm -rf "$CERTDIR"; mkdir -p "$CERTDIR"
  local got=""
  for _ in $(seq 1 30); do
    if docker cp "$DIND:/certs/client/." "$CERTDIR/" >/dev/null 2>&1 && [ -f "$CERTDIR/cert.pem" ]; then got=1; break; fi
    sleep 2
  done
  [ -n "$got" ] || { echo "::error::dind never produced client TLS certs"; docker logs "$DIND" 2>&1 | tail -40; exit 1; }
  export DOCKER_HOST="tcp://127.0.0.1:${api_port}" DOCKER_TLS_VERIFY=1 DOCKER_CERT_PATH="$CERTDIR"
  local ok=""
  for _ in $(seq 1 30); do docker info >/dev/null 2>&1 && { ok=1; break; }; sleep 2; done
  [ -n "$ok" ] || { echo "::error::nested dockerd never became reachable over DOCKER_HOST"; docker logs "$DIND" 2>&1 | tail -60; exit 1; }
  {
    echo "DIND=$DIND"
    echo "DIND_CERTS_VOL=$CERTS_VOL"
    echo "DIND_CERTDIR=$CERTDIR"
    echo "DOCKER_HOST=tcp://127.0.0.1:${api_port}"
    echo "DOCKER_TLS_VERIFY=1"
    echo "DOCKER_CERT_PATH=${CERTDIR}"
    echo "HARNESS_BIND_ADDR=0.0.0.0"
    echo "BASE=http://127.0.0.1:${http_port}"
    echo "CP_STUB_BASE=http://127.0.0.1:${cp_port}"
  } >> "${GITHUB_ENV:-/dev/stdout}"
  echo "[dind] $DIND up — DOCKER_HOST=tcp://127.0.0.1:${api_port}; harness http=http://127.0.0.1:${http_port} cp-stub=http://127.0.0.1:${cp_port}" >&2
}

down() {
  # Operate on the HOST daemon (where the dind container itself lives), NOT the
  # nested one — unset the wiring `up` exported. One rm then destroys every
  # nested container/volume/network/image the harness made, atomically.
  unset DOCKER_HOST DOCKER_TLS_VERIFY DOCKER_CERT_PATH
  docker rm -fv "$DIND" >/dev/null 2>&1 || true
  docker volume rm "$CERTS_VOL" >/dev/null 2>&1 || true
  rm -rf "$CERTDIR" 2>/dev/null || true
  echo "[dind] $DIND down — its whole topology is gone" >&2
}

case "$CMD" in
  up)   up ;;
  down) down ;;
  *)    echo "usage: $0 up|down" >&2; exit 2 ;;
esac
