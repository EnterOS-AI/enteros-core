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
CERTDIR="${GITHUB_WORKSPACE:-$PWD}/.dind-certs-${NS}"
api_port=""   # set by up() once the dind has published 2376; read by nested_docker

# Two EXPLICIT daemon targets. Which daemon a bare `docker` hits depends on
# inherited env (GITHUB_ENV persists DOCKER_HOST into every later step of the
# job), so "bare docker" is ambiguous inside this script and has already caused
# one silent misroute: `up` used to `export DOCKER_HOST` before its failure
# diagnostics, which aimed `docker logs "$DIND"` at the NESTED daemon — the one
# it had just declared unreachable — so it died on the same missing ca.pem and
# the dind logs naming the real cause were never captured. Never call bare
# `docker` here; say which daemon you mean.
#
#   host_docker   — the runner's daemon, where the $DIND container itself lives.
#                   ALL lifecycle + diagnostic calls.
#   nested_docker — the dind's own daemon, reachable only over TLS. ONLY the
#                   reachability probe.
host_docker() {  # runner's daemon: strip any inherited dind wiring
  env -u DOCKER_HOST -u DOCKER_TLS_VERIFY -u DOCKER_CERT_PATH docker "$@"
}
nested_docker() {  # the dind's daemon, addressed explicitly (never exported)
  DOCKER_HOST="tcp://127.0.0.1:${api_port}" DOCKER_TLS_VERIFY=1 \
    DOCKER_CERT_PATH="$CERTDIR" docker "$@"
}

ephemeral_port() {  # $1=container-port/tcp → host loopback port
  host_docker port "$DIND" "$1" 2>/dev/null | awk -F: '/127\.0\.0\.1:/ {print $2; exit}' \
    || host_docker port "$DIND" "$1" 2>/dev/null | head -1 | awk -F: '{print $NF}'
}

up() {
  host_docker info >/dev/null 2>&1 || { echo "::error::docker daemon not reachable"; exit 2; }
  host_docker rm -fv "$DIND" >/dev/null 2>&1 || true
  # --privileged is REQUIRED for a nested dockerd. If denied, the runner forbids
  # privileged → this approach needs the rootless-dind fallback (fail loud). We
  # also publish the harness's cf-proxy(8080) + cp-stub(9090) ports off the dind
  # to host loopback NOW, so once the nested compose binds them (0.0.0.0 inside
  # the dind) the job reaches them via 127.0.0.1:<ephemeral> — the same host-
  # loopback idiom the pg/redis steps already use, no cross-network wiring.
  #
  # /certs is deliberately NOT a mount. It used to be a NAMED volume
  # ("${DIND}-certs"), which is the one object in this design that `docker rm
  # -fv "$DIND"` cannot reap — a named volume dies ONLY via an explicit
  # `docker volume rm`, i.e. only if the teardown step actually runs. Cancels,
  # job timeouts and SIGKILLs skip it, run IDs are never revisited, and a plain
  # `docker volume prune` skips NAMED volumes (they need `--all`), so nothing
  # ever swept them: 85 orphaned dind-harness-*-certs accumulated on the fleet.
  # Leaving /certs in the container's own writable layer restores the header's
  # invariant — ONE object, destroyed atomically by ONE `docker rm -fv`, with no
  # second fallible command. The dind entrypoint still generates the full client
  # cert set there; we only ever read it back out with `docker cp`.
  if ! host_docker run -d --name "$DIND" --privileged \
      -e DOCKER_TLS_CERTDIR=/certs \
      -p 127.0.0.1::2376 -p 127.0.0.1::8080 -p 127.0.0.1::9090 \
      "$DIND_IMAGE" >/dev/null; then
    echo "::error::'docker run --privileged' denied on this runner — the per-job dind isolation needs the rootless-dind fallback (task #78)."
    exit 1
  fi
  api_port="$(ephemeral_port 2376/tcp)"
  local http_port; http_port="$(ephemeral_port 8080/tcp)"
  local cp_port; cp_port="$(ephemeral_port 9090/tcp)"
  [ -n "$api_port" ] && [ -n "$http_port" ] && [ -n "$cp_port" ] || {
    echo "::error::dind did not publish all expected ports (2376=$api_port 8080=$http_port 9090=$cp_port)"; host_docker logs "$DIND" 2>&1 | tail -40; exit 1; }
  rm -rf "$CERTDIR"; mkdir -p "$CERTDIR"
  # The TLS client needs ALL THREE of ca.pem/cert.pem/key.pem. `docker cp` of
  # /certs/client/. is NOT atomic, so this loop used to gate on cert.pem ALONE
  # and could break the instant cert.pem landed — before ca.pem existed. Every
  # subsequent `docker info` then failed INSTANTLY client-side with
  #   unable to resolve docker endpoint: open .../ca.pem: no such file or directory
  # and the reachability loop below blamed a nested dockerd that was very
  # possibly healthy the whole time ("never became reachable over DOCKER_HOST"
  # after a 63.5s `up`: ~3s of certs then 30 instant client-side failures).
  # Gate on the whole set — the loop must not break until the client can
  # actually connect.
  local got=""
  for _ in $(seq 1 30); do
    if host_docker cp "$DIND:/certs/client/." "$CERTDIR/" >/dev/null 2>&1 \
       && [ -f "$CERTDIR/ca.pem" ] && [ -f "$CERTDIR/cert.pem" ] && [ -f "$CERTDIR/key.pem" ]; then
      got=1; break
    fi
    sleep 2
  done
  [ -n "$got" ] || { echo "::error::dind never produced a COMPLETE client TLS cert set (need ca.pem+cert.pem+key.pem in $CERTDIR)"; host_docker logs "$DIND" 2>&1 | tail -40; exit 1; }
  local ok=""
  for _ in $(seq 1 30); do nested_docker info >/dev/null 2>&1 && { ok=1; break; }; sleep 2; done
  # NOTE: host_docker, not bare docker. This diagnostic must read the logs of
  # the dind CONTAINER (host daemon); aiming it at the nested daemon we just
  # declared unreachable is how the real cause stayed invisible.
  [ -n "$ok" ] || { echo "::error::nested dockerd never became reachable over DOCKER_HOST"; host_docker logs "$DIND" 2>&1 | tail -60; exit 1; }
  {
    echo "DIND=$DIND"
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
  # nested one — host_docker strips the wiring `up` wrote into $GITHUB_ENV, which
  # every later step of the job inherits. ONE rm now destroys every nested
  # container/volume/network/image the harness made, atomically: with /certs no
  # longer a named volume there is no second object needing a second, separately
  # fallible command. `-v` also reaps the image's anonymous /var/lib/docker
  # volume, so a job killed before this step leaves only DANGLING-ANONYMOUS
  # state, which an ordinary `docker volume prune` sweeps — unlike the named
  # certs volume, which it skips.
  #
  # Report what was ACTUALLY reaped. `down` derives $DIND from DIND_NS /
  # GITHUB_RUN_ID / GITHUB_JOB; if a teardown step's env ever drifts from its
  # `up` step's, this reaps a name that never existed — and every command here
  # is `|| true`, so the old unconditional "its whole topology is gone" printed
  # a clean success over a total no-op. Say which it was.
  local existed=""
  host_docker inspect "$DIND" >/dev/null 2>&1 && existed=1
  host_docker rm -fv "$DIND" >/dev/null 2>&1 || true
  rm -rf "$CERTDIR" 2>/dev/null || true
  if [ -n "$existed" ]; then
    echo "[dind] $DIND down — its whole topology is gone" >&2
  else
    # NOT an error: a job whose `up` never ran (earlier step failed, or the
    # no-op path) legitimately has nothing to reap. But never call it a teardown.
    echo "::warning::[dind] no container named $DIND existed — nothing was torn down. If an 'up' ran in this job, its DIND_NS/GITHUB_RUN_ID/GITHUB_JOB differed from this step's and the dind is now ORPHANED." >&2
  fi
}

case "$CMD" in
  up)   up ;;
  down) down ;;
  *)    echo "usage: $0 up|down" >&2; exit 2 ;;
esac
