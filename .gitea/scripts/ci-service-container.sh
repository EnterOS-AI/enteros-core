#!/usr/bin/env bash
# ci-service-container.sh — start a CI job's throwaway Postgres / Redis on the
# runner's SHARED host docker daemon without leaking a volume per run.
#
#   ci-service-container.sh postgres NAME IMAGE [docker-run-args...]
#   ci-service-container.sh redis    NAME IMAGE [docker-run-args...]
#
# Removes any same-named leftover (`rm -fv`), then `docker run -d`s IMAGE as NAME
# with a size-capped tmpfs at the image's data dir and a molecule.ci.run label.
# Ports, network, env and health checks come from the caller's extra args, which
# go before IMAGE. Prints the container id, like `docker run -d`.
#
# WHY: postgres:16, pgvector/pgvector:pg15 and redis:7 declare a VOLUME for their
# data dir, so a plain `docker run` creates an ANONYMOUS volume per start. The
# teardowns removed the containers with `docker rm -f` — no -v — which leaves that
# volume dangling (two per e2e run; ~17k on one CI host in six weeks), and a
# cancelled job never runs its teardown at all. With a tmpfs mounted at the data
# dir Docker creates no volume in the first place, so there is nothing for a
# teardown, or the lack of one, to leak. The data is throwaway by construction.
# The size cap matters: tmpfs pages are RAM on a host shared with tenant pods.
#
# The label marks the container as one CI run's (molecule.ci.run=<run id>) so a
# host janitor can find a killed job's leftovers without guessing by name.
set -euo pipefail

usage() { echo "usage: $0 postgres|redis NAME IMAGE [docker-run-args...]" >&2; exit 2; }
[ "$#" -ge 3 ] || usage
kind="$1" name="$2" image="$3"
shift 3

case "$kind" in
  postgres) data_dir=/var/lib/postgresql/data size=1g ;;
  redis)    data_dir=/data size=256m ;;
  *)        usage ;;
esac

docker rm -fv "$name" >/dev/null 2>&1 || true
exec docker run -d --name "$name" \
  --label "molecule.ci.run=${GITHUB_RUN_ID:-local}" \
  --tmpfs "${data_dir}:size=${size}" \
  "$@" "$image"
