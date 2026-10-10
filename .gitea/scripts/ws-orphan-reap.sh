#!/usr/bin/env bash
# ws-orphan-reap.sh — remove leaked ws-<id> workspace containers AND their
# volumes from a CI runner host's docker daemon.
#
#   ws-orphan-reap.sh instance DATABASE_URL
#       Teardown for ONE e2e run, after its platform is stopped. Removes every
#       container the run's platform provisioned — stamped
#       molecule.platform.managed=true + molecule.platform.instance=<id>, where
#       <id> = sha256(DATABASE_URL)[:16], exactly as the platform derives it
#       (provisioner.PlatformInstanceID) — and their volumes. The DSN carries the
#       run's own ephemeral Postgres port, so the label selects THIS run's
#       workspaces and nothing a concurrent run owns: positive identity, the same
#       rule as the manifest teardowns (#4346), applied to the containers the
#       platform made on its own (a bundle import, a provision that raced a
#       delete) which no manifest knows about. No age floor: the platform is
#       already gone.
#
#   ws-orphan-reap.sh sweep
#       The standing janitor (sweep-stale-ws-orphans.yml). Removes every
#       molecule.platform.managed=true ws-<id> container CREATED more than
#       WS_MAX_AGE_HOURS ago (default 2; WS_MAX_AGE_SECONDS overrides) with its
#       volumes, capped at SAFETY_CAP per run; DRY_RUN=true only lists. Age is
#       .Created, never .State.StartedAt: a crash-looping orphan restarts every
#       few seconds, so by StartedAt it is always "young" and was never swept
#       (~300k restarts, two months). RestartCount is reported alongside.
#
# Volumes: a container's mounted ws-* volumes plus the derived
# ws-<id>-{configs,workspace,claude-sessions}. Derived because Docker auto-creates
# a bind's missing named volume WITHOUT labels (the /workspace volume, until the
# platform created it labelled; the config volume when a delete raced the
# provision), and a tier-1 container never mounts the claude-sessions volume it
# was given. `rm -fv` takes the anonymous ones.
#
# BOTH modes refuse to run against the production box (100.64.0.3), where
# managed ws-* containers are REAL tenant workspaces: DOCKER_HOST pointing at it,
# this host being it, or a Docker Desktop daemon (the production box's; CI hosts
# run Linux docker-ce) all exit 3 before anything is listed.
set -uo pipefail

PROD_ADDRS="100.64.0.3 ${WS_REAP_EXTRA_PROD_ADDRS:-}"
LABEL_MANAGED="molecule.platform.managed"
LABEL_INSTANCE="molecule.platform.instance"

die() { echo "::error::ws-orphan-reap: $1" >&2; exit "${2:-1}"; }  # $1 message, $2 exit code

refuse_prod_daemon() {
  local a os addrs=""
  # Captured, not piped into `grep -q`: under pipefail an early-exiting grep can
  # SIGPIPE the producer and turn a match into a miss.
  command -v ip >/dev/null 2>&1 && addrs="$(ip -o -4 addr show 2>/dev/null)"
  for a in $PROD_ADDRS; do
    case "${DOCKER_HOST:-}" in *"$a"*) die "DOCKER_HOST=${DOCKER_HOST} is the production box ($a) — its ws-* containers are real tenant workspaces. Refusing." 3 ;; esac
    case "$addrs" in *" $a/"*) die "this host is the production box ($a). Refusing." 3 ;; esac
  done
  os="$(docker info --format '{{.OperatingSystem}}' 2>/dev/null)" || die "docker daemon not reachable" 2
  case "$os" in
    *"Docker Desktop"*) die "the daemon is Docker Desktop ('$os') — the production box runs Docker Desktop, CI hosts never do. Refusing." 3 ;;
  esac
}

# Remove one ws-<id> container (with its anonymous volumes) and its named volumes.
reap_ws_container() {
  local cid="$1" name wsid mounted v
  name="$(docker inspect -f '{{.Name}}' "$cid" 2>/dev/null)" || return 0
  name="${name#/}"
  case "$name" in
    ws-?*) wsid="${name#ws-}" ;;
    *) echo "  skip $cid: name '$name' is not ws-<id>"; return 0 ;;
  esac
  mounted="$(docker inspect -f '{{range .Mounts}}{{if eq .Type "volume"}}{{.Name}} {{end}}{{end}}' "$cid" 2>/dev/null)"
  if ! docker rm -fv "$cid" >/dev/null 2>&1; then
    echo "  WARN: could not remove $name ($cid)"
    return 1
  fi
  echo "  removed container $name"
  for v in $mounted "ws-${wsid}-configs" "ws-${wsid}-workspace" "ws-${wsid}-claude-sessions"; do
    case "$v" in ws-*) ;; *) continue ;; esac
    docker volume rm "$v" >/dev/null 2>&1 && echo "  removed volume $v"
  done
  return 0
}

instance_id() {  # sha256(DSN) hex, first 16 chars — provisioner.PlatformInstanceID
  if command -v sha256sum >/dev/null 2>&1; then
    printf '%s' "$1" | sha256sum | head -c 16
  else
    printf '%s' "$1" | shasum -a 256 | head -c 16
  fi
}

cmd_instance() {
  local dsn="${1:-}" inst cid v n=0
  [ -n "$dsn" ] || die "instance mode needs the platform's DATABASE_URL" 2
  inst="$(instance_id "$dsn")"
  [[ "$inst" =~ ^[0-9a-f]{16}$ ]] || die "could not derive the platform instance id" 2
  refuse_prod_daemon
  echo "Reaping workspaces of platform instance $inst (this run's platform)."
  for cid in $(docker ps -aq --no-trunc --filter "label=${LABEL_MANAGED}=true" --filter "label=${LABEL_INSTANCE}=${inst}"); do
    reap_ws_container "$cid" && n=$((n + 1))
  done
  # Volumes the instance stamped whose container never came to be (a Start that
  # failed, or was abandoned, after creating them).
  for v in $(docker volume ls -q --filter "label=${LABEL_INSTANCE}=${inst}"); do
    docker volume rm "$v" >/dev/null 2>&1 && echo "  removed volume $v"
  done
  echo "Instance teardown: removed $n workspace container(s)."
}

cmd_sweep() {
  local max_age cap dry now cid name created restarts state inst ts age stale="" n_stale=0 n_keep=0 removed=0
  max_age="${WS_MAX_AGE_SECONDS:-$(( ${WS_MAX_AGE_HOURS:-2} * 3600 ))}"
  cap="${SAFETY_CAP:-100}" dry="${DRY_RUN:-false}"
  if ! [[ "$max_age" =~ ^[0-9]+$ ]] || [ "$max_age" -eq 0 ]; then
    die "invalid age floor '$max_age'" 2
  fi
  refuse_prod_daemon
  now="$(date +%s)"
  echo "Sweeping ${LABEL_MANAGED}=true ws-* containers CREATED more than ${max_age}s ago. DRY_RUN=${dry}"
  for cid in $(docker ps -aq --no-trunc --filter "label=${LABEL_MANAGED}=true"); do
    read -r name created restarts state inst < <(docker inspect -f \
      "{{.Name}} {{.Created}} {{.RestartCount}} {{.State.Status}} {{index .Config.Labels \"${LABEL_INSTANCE}\"}}" "$cid" 2>/dev/null) || continue
    name="${name#/}"
    case "$name" in ws-?*) ;; *) continue ;; esac
    ts="$(date -d "$created" +%s 2>/dev/null)" || continue
    age=$(( now - ts ))
    if [ "$age" -ge "$max_age" ]; then
      echo "  STALE  $name  age=${age}s state=${state} restarts=${restarts} instance=${inst:-none}"
      stale="$stale $cid"
      n_stale=$((n_stale + 1))
    else
      echo "  keep   $name  age=${age}s (< ${max_age}s floor) state=${state} restarts=${restarts}"
      n_keep=$((n_keep + 1))
    fi
  done
  if [ "$n_stale" -eq 0 ]; then
    echo "No stale workspace containers ($n_keep younger than the floor)."
    return 0
  fi
  if [ "$n_stale" -gt "$cap" ]; then
    die "refusing to remove $n_stale containers in one sweep (cap=$cap) — investigate (clock skew? a mislabelled daemon?)" 1
  fi
  if [ "$dry" = "true" ]; then
    echo "DRY RUN — would remove $n_stale stale workspace container(s) and their volumes."
    return 0
  fi
  for cid in $stale; do
    reap_ws_container "$cid" && removed=$((removed + 1))
  done
  echo "Sweep summary: removed=$removed of $n_stale stale workspace container(s); kept=$n_keep."
  [ "$removed" -eq "$n_stale" ]
}

case "${1:-}" in
  instance) shift; cmd_instance "$@" ;;
  sweep)    cmd_sweep ;;
  *)        echo "usage: $0 instance DATABASE_URL | sweep" >&2; exit 2 ;;
esac
