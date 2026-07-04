#!/usr/bin/env bash
# redeploy-staging-fleet.sh — roll the local-docker STAGING tenant fleet onto a
# freshly published tenant image, session-preservingly.
#
# SSOT for the tenant-fleet swap. Both the gated CD pipeline
# (.gitea/workflows/staging-tenant-cd.yml) and the manual rollback path
# (.gitea/workflows/redeploy-tenants-on-staging.yml, workflow_dispatch) call
# THIS script so the swap algorithm lives in exactly one committed, hand-runnable
# place (mirrors the CP's scripts/deploy/ SSOT convention).
#
# WHAT IT ROLLS (and why it is session-safe)
# ------------------------------------------
# A local-docker tenant is TWO containers:
#   * mol-tenant-<slug>  — the Go platform + baked canvas (the `molecule-tenant`
#     image). STATELESS (no volume mounts). Labelled molecule.cp-env=<env>.
#   * mol-ws-<slug>      — the agent runtime (openclaw/claude-code template
#     image, a DIFFERENT image). Owns the durable session in the NAMED volume
#     mol-ws-rtstate-<id> (~/.openclaw). NOT labelled molecule.cp-env.
# This script filters on `molecule.local-tenant=1` AND `molecule.cp-env=<env>`,
# so it ONLY recreates the STATELESS mol-tenant-* platform containers. It never
# touches the session-bearing mol-ws-* container or its rtstate volume, so the
# concierge session survives a fleet roll BY CONSTRUCTION. (The CP-driven
# restart path is separately session-safe via controlplane #1118 —
# DeprovisionWorkspaceKeepState preserves mol-ws-rtstate-* on non-prune
# teardown.) A verify step below asserts the rtstate volumes are untouched.
#
# SWAP ALGORITHM (canary-first, health-gated, self-rolling-back)
#   1. Discover the running <env> platform containers by label.
#   2. Roll the FIRST as a canary: recreate onto the new image preserving
#      env / network / labels / extra-hosts / restart policy, publish a fresh
#      ephemeral 127.0.0.1 port (the CP resolves the tenant port dynamically via
#      `docker port`, so a new port is safe), then health-gate /buildinfo. On
#      failure, roll the container back to the prior image and ABORT the fleet.
#   3. Fan out to the rest, same health-gate + rollback per container.
#   4. Recreate the shared staging canvas app molecule-staging-app (if present).
#
# Usage:
#   redeploy-staging-fleet.sh --image <ref>            # roll onto <ref>
#   redeploy-staging-fleet.sh --tag  staging-<sha>     # roll onto TENANT_IMAGE:tag
#   redeploy-staging-fleet.sh --dry-run                # discover + plan only
#   redeploy-staging-fleet.sh --cp-env staging         # (default) env label filter
#
# Env overrides (no-hardcoding):
#   TENANT_IMAGE       registry.moleculesai.app/molecule-ai/molecule-tenant
#   CANVAS_APP_IMAGE   registry.moleculesai.app/molecule-ai/canvas
#   CANVAS_TAG         canvas tag for molecule-staging-app (default: matches --tag/staging-latest)
#
# SAFETY: only recreates STATELESS cp-env=<env> platform containers; never
# removes a named volume; each swap is health-gated + self-rolls-back; --dry-run
# performs zero mutations. STAGING scoped by default (cp-env=staging).
set -euo pipefail
export MSYS_NO_PATHCONV=1 MSYS2_ARG_CONV_EXCL='*'

TENANT_IMAGE="${TENANT_IMAGE:-registry.moleculesai.app/molecule-ai/molecule-tenant}"
CANVAS_APP_IMAGE="${CANVAS_APP_IMAGE:-registry.moleculesai.app/molecule-ai/canvas}"
CP_ENV="staging"
IMAGE="" ; TAG="" ; DRY_RUN=0

usage() { sed -n '2,45p' "$0" | sed 's/^# \{0,1\}//'; exit "${1:-0}"; }
while [ "$#" -gt 0 ]; do
  case "$1" in
    --image)   IMAGE="$2"; shift 2;;
    --tag)     TAG="$2";   shift 2;;
    --cp-env)  CP_ENV="$2"; shift 2;;
    --dry-run) DRY_RUN=1; shift;;
    -h|--help) usage 0;;
    *) echo "unknown arg: $1" >&2; usage 2;;
  esac
done
log() { printf '>> [fleet] %s\n' "$*" >&2; }

# Resolve the target image ref (either an explicit --image, or TENANT_IMAGE:tag).
if [ -z "$IMAGE" ]; then
  [ -n "$TAG" ] || TAG="staging-latest"
  IMAGE="${TENANT_IMAGE}:${TAG}"
fi
CANVAS_TAG="${CANVAS_TAG:-${TAG:-staging-latest}}"
CIMG="${CANVAS_APP_IMAGE}:${CANVAS_TAG}"
log "target tenant image = ${IMAGE}  (cp-env=${CP_ENV}, dry_run=${DRY_RUN})"

docker info >/dev/null 2>&1 || { echo "FATAL: docker daemon not reachable (need /var/run/docker.sock)" >&2; exit 1; }

# Pull the target image (best-effort: on the single build host it may already be
# present from the publish --load). Skip the pull for a bare local tag ref.
if [ "$DRY_RUN" = "0" ]; then
  if printf '%s' "$IMAGE" | grep -q '/'; then
    docker pull "$IMAGE" >/dev/null 2>&1 || log "WARN: docker pull $IMAGE failed; using locally-present image if any"
  fi
fi
if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
  if [ "$DRY_RUN" = "1" ]; then
    log "WARN(dry-run): target image $IMAGE not present locally — a real run would pull it"
  else
    echo "::error::tenant image $IMAGE not available locally or in registry" >&2
    exit 1
  fi
fi

# Discover running <env> platform containers (the STATELESS mol-tenant-* boxes).
mapfile -t TENANTS < <(docker ps \
  --filter 'label=molecule.local-tenant=1' \
  --filter "label=molecule.cp-env=${CP_ENV}" \
  --format '{{.Names}}' | sort)

if [ "${#TENANTS[@]}" -eq 0 ]; then
  log "no running ${CP_ENV} tenant platform containers found — nothing to roll"
else
  log "tenants to roll (${#TENANTS[@]}): ${TENANTS[*]}"
fi

# Snapshot the session-bearing rtstate volumes BEFORE the roll so we can assert
# the fleet swap left every one intact (session-preservation regression guard).
mapfile -t RTSTATE_BEFORE < <(docker volume ls --format '{{.Name}}' | grep -E '^mol-ws-rtstate-' | sort || true)
log "session (rtstate) volumes present before roll: ${#RTSTATE_BEFORE[@]}"

if [ "$DRY_RUN" = "1" ]; then
  log "DRY-RUN: would roll ${#TENANTS[@]} tenant(s) onto ${IMAGE} (canary-first, health-gated)"
  for t in "${TENANTS[@]}"; do
    cur="$(docker inspect "$t" --format '{{.Config.Image}}' 2>/dev/null || echo '?')"
    printf '   - %-46s %s -> %s\n' "$t" "$cur" "$IMAGE" >&2
  done
  log "DRY-RUN: session volumes that would be PRESERVED (untouched): ${#RTSTATE_BEFORE[@]}"
  log "DRY-RUN complete — zero mutations performed"
  exit 0
fi

# health_gate <container>: probe the tenant's published :8080 /buildinfo through
# the host loopback. Returns 0 when it answers.
health_gate() {
  local name="$1" port
  port="$(docker port "$name" 8080/tcp 2>/dev/null | head -1 | sed 's/.*://')"
  [ -n "$port" ] || return 1
  for _ in $(seq 1 20); do
    if curl -fsS --max-time 5 "http://127.0.0.1:${port}/buildinfo" >/dev/null 2>&1; then
      return 0
    fi
    sleep 3
  done
  return 1
}

# swap_tenant <container> <image>: recreate onto <image> preserving env /
# network / labels / extra-hosts / restart policy; health-gate + roll back.
swap_tenant() {
  local name="$1" image="$2"
  local bak="${name}-redeploy-bak"
  local net restart envfile
  net="$(docker inspect "$name" --format '{{range $k,$v := .NetworkSettings.Networks}}{{$k}}{{end}}')"
  restart="$(docker inspect "$name" --format '{{.HostConfig.RestartPolicy.Name}}')"
  envfile="$(mktemp)"
  docker inspect "$name" --format '{{range .Config.Env}}{{println .}}{{end}}' \
    | grep -vE '^(PATH|NODE_VERSION|YARN_VERSION)=' > "$envfile"
  local labelargs=() hostargs=()
  while IFS= read -r l; do [ -n "$l" ] && labelargs+=( --label "$l" ); done \
    < <(docker inspect "$name" --format '{{range $k,$v := .Config.Labels}}{{$k}}={{$v}}{{println}}{{end}}' \
          | grep -vE '^(org.opencontainers|com.docker)')
  while IFS= read -r h; do [ -n "$h" ] && hostargs+=( --add-host "$h" ); done \
    < <(docker inspect "$name" --format '{{range .HostConfig.ExtraHosts}}{{println .}}{{end}}')

  docker rename "$name" "$bak"
  docker stop "$bak" >/dev/null

  if ! docker run -d \
        --name "$name" \
        --network "$net" \
        --restart "${restart:-always}" \
        --env-file "$envfile" \
        "${labelargs[@]}" "${hostargs[@]}" \
        -p 127.0.0.1::8080 \
        "$image" >/dev/null 2>/tmp/run.err; then
    echo "::error::docker run failed for $name:" >&2; cat /tmp/run.err >&2
    rm -f "$envfile"
    docker rm -f "$name" 2>/dev/null || true
    docker rename "$bak" "$name"; docker start "$name" >/dev/null
    return 1
  fi
  rm -f "$envfile"

  if ! health_gate "$name"; then
    echo "::error::$name failed health gate on $image — rolling back" >&2
    docker rm -f "$name" 2>/dev/null || true
    docker rename "$bak" "$name"; docker start "$name" >/dev/null
    return 1
  fi
  docker rm "$bak" >/dev/null 2>&1 || true
  log "  ✓ $name rolled to $image"
  return 0
}

FAILED=0
FIRST=1
for t in "${TENANTS[@]}"; do
  if [ "$FIRST" = 1 ]; then
    log "== canary: $t =="
    swap_tenant "$t" "$IMAGE" || { echo "::error::canary $t failed — aborting fleet roll" >&2; FAILED=1; break; }
    FIRST=0
  else
    log "== $t =="
    swap_tenant "$t" "$IMAGE" || { FAILED=1; break; }
  fi
done

# Shared staging canvas app (molecule-staging-app): recreate from the canvas
# image. Separate container (not a tenant). Skipped cleanly when absent.
if docker ps -a --format '{{.Names}}' | grep -qx 'molecule-staging-app'; then
  log "== shared canvas app: molecule-staging-app -> ${CIMG} =="
  docker pull "$CIMG" >/dev/null 2>&1 || log "WARN: pull $CIMG failed; using local image if present"
  if docker image inspect "$CIMG" >/dev/null 2>&1; then
    net="$(docker inspect molecule-staging-app --format '{{range $k,$v := .NetworkSettings.Networks}}{{$k}}{{end}}')"
    pbind="$(docker inspect molecule-staging-app --format '{{range $p,$b := .HostConfig.PortBindings}}{{range $b}}{{.HostPort}}{{end}}{{end}}')"
    cenv="$(mktemp)"
    docker inspect molecule-staging-app --format '{{range .Config.Env}}{{println .}}{{end}}' \
      | grep -vE '^(PATH|NODE_VERSION|YARN_VERSION|NODE_ENV|HOSTNAME|PORT|NEXT_TELEMETRY_DISABLED)=' > "$cenv"
    docker rename molecule-staging-app molecule-staging-app-redeploy-bak
    docker stop molecule-staging-app-redeploy-bak >/dev/null
    if docker run -d --name molecule-staging-app --network "${net:-molecule-net}" \
         --restart unless-stopped --env-file "$cenv" \
         -p "${pbind:-3101}:3000" "$CIMG" >/dev/null 2>/tmp/cvrun.err; then
      sleep 5
      if curl -fsS --max-time 8 "http://127.0.0.1:${pbind:-3101}/" >/dev/null 2>&1; then
        docker rm molecule-staging-app-redeploy-bak >/dev/null 2>&1 || true
        log "  ✓ molecule-staging-app rolled to ${CIMG}"
      else
        echo "::error::molecule-staging-app unhealthy after roll — rolling back" >&2
        docker rm -f molecule-staging-app 2>/dev/null || true
        docker rename molecule-staging-app-redeploy-bak molecule-staging-app
        docker start molecule-staging-app >/dev/null
        FAILED=1
      fi
    else
      echo "::error::docker run failed for molecule-staging-app:" >&2; cat /tmp/cvrun.err >&2
      docker rm -f molecule-staging-app 2>/dev/null || true
      docker rename molecule-staging-app-redeploy-bak molecule-staging-app
      docker start molecule-staging-app >/dev/null
      FAILED=1
    fi
    rm -f "$cenv"
  else
    log "WARN: canvas image $CIMG unavailable — skipping molecule-staging-app roll"
  fi
else
  log "molecule-staging-app container not present — skipping shared-app roll"
fi

# Session-preservation assertion: every rtstate volume that existed before the
# roll must still exist after it (the fleet swap must never destroy a session).
mapfile -t RTSTATE_AFTER < <(docker volume ls --format '{{.Name}}' | grep -E '^mol-ws-rtstate-' | sort || true)
LOST=0
for v in "${RTSTATE_BEFORE[@]}"; do
  printf '%s\n' "${RTSTATE_AFTER[@]}" | grep -qxF "$v" || { echo "::error::session volume $v was REMOVED by the fleet roll (session-preservation VIOLATED)" >&2; LOST=1; }
done
[ "$LOST" = 0 ] && log "session-preservation OK: all ${#RTSTATE_BEFORE[@]} rtstate volume(s) intact after roll"
[ "$LOST" = 0 ] || FAILED=1

if [ "$FAILED" != 0 ]; then
  echo "::error::staging fleet redeploy had at least one failure (see log above)" >&2
  exit 1
fi
log "staging fleet + shared canvas app redeploy complete (image=${IMAGE}, tenants=${#TENANTS[@]})"
