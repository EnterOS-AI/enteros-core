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
#   CANVAS_TAG         canvas tag for the shared canvas app (default: matches --tag/staging-latest)
#   STAGING_CANVAS_APP_CONTAINER  OPT-IN shared-canvas container to roll (default: EMPTY = skip).
#                      Do NOT point this at the central staging-app container
#                      (molecule-staging-app) — that serves the staging console
#                      (molecule-app), not the per-tenant canvas. See the
#                      shared-canvas block below.
#   EXPECTED_BUILD_SHA optional expected /buildinfo.git_sha. Defaults to the
#                      suffix of --tag staging-<sha>.
#   HEALTH_GATE_ATTEMPTS / HEALTH_GATE_SLEEP_SECS tune /buildinfo polling
#                      (defaults: 20 attempts, 3s sleep).
#   TENANT_FLAGS       space-separated KEY=VALUE rollout flags to apply to every
#                      tenant container, e.g. "DELEGATION_LEDGER_WRITE=1".
#                      DECLARATIVE AND REVERSIBLE — see MANAGED_FLAG_KEYS below.
#                      Default empty = every managed flag OFF.
#
# SAFETY: only recreates STATELESS cp-env=<env> platform containers; never
# removes a named volume; each swap is health-gated + self-rolls-back; --dry-run
# performs zero mutations. STAGING scoped by default (cp-env=staging).
set -euo pipefail

# ---------------------------------------------------------------------------
# MANAGED ROLLOUT FLAGS — declarative, and REVERSIBLE.
#
# Tenant env is inherited BY COPY: swap_tenant reads the RUNNING container's
# Config.Env and re-applies it to the new image. There is no declarative source,
# which means anything set by hand on a container is STICKY FOREVER — copied into
# every future redeploy, with no way to unset it. A burn-in flag you cannot
# un-flip is not a burn-in flag; a rollback that cannot roll the flag back is not
# a rollback.
#
# So the keys below are OWNED by this script. On every swap they are STRIPPED from
# the inherited env and re-applied from $TENANT_FLAGS. Consequences, and they are
# the point:
#   * TENANT_FLAGS is the SINGLE SOURCE OF TRUTH for these keys.
#   * Removing a flag from TENANT_FLAGS genuinely turns it OFF on the next roll —
#     it does not linger in the copied env.
#   * A hand-set value on a live container is transient and will be reverted,
#     which is what stops staging from drifting away from what the repo says.
#
# Add a key here the moment a flag needs to be rollable, NOT when someone first
# wants it on.
MANAGED_FLAG_KEYS=(
  DELEGATION_LEDGER_WRITE
  DELEGATION_RESULT_INBOX_PUSH
)

# apply_managed_flags <envfile>: strip every managed key from the inherited env,
# then append exactly what TENANT_FLAGS asks for.
apply_managed_flags() {
  local envfile="$1"
  local key rc
  for key in "${MANAGED_FLAG_KEYS[@]}"; do
    # FAIL CLOSED. `grep -v` exits 1 when it prints NO lines — which happens for a
    # real envfile that contains nothing but this one key. Written as
    # `grep -v ... > tmp && mv`, that skips the mv, leaves the stale flag in place,
    # and STILL returns 0: the strip silently no-ops in exactly the case it matters,
    # and the container rolls with the flag it was supposed to have lost. The same
    # swallow hid any genuine error (a failed redirect: ENOSPC, read-only tmpdir).
    #
    # So: 0 = matched, 1 = matched nothing (both fine, and the .tmp is authoritative
    # either way), >=2 = a real grep error, which must abort the roll.
    set +e
    grep -vE "^${key}=" "$envfile" > "${envfile}.tmp"
    rc=$?
    set -e
    if [ "$rc" -ge 2 ]; then
      rm -f "${envfile}.tmp"
      echo "::error::apply_managed_flags: grep failed (rc=$rc) stripping '$key' from $envfile" >&2
      return 1
    fi
    if ! mv "${envfile}.tmp" "$envfile"; then
      rm -f "${envfile}.tmp"
      echo "::error::apply_managed_flags: could not rewrite $envfile while stripping '$key'" >&2
      return 1
    fi
  done
  local kv
  for kv in $TENANT_FLAGS; do
    case "$kv" in
      *=*) ;;
      *) echo "::error::TENANT_FLAGS entry '$kv' is not KEY=VALUE" >&2; return 1 ;;
    esac
    key="${kv%%=*}"
    # Refuse to set anything not declared managed: an unmanaged key would be
    # sticky (never stripped), i.e. exactly the irreversibility this exists to
    # prevent. Fail loudly rather than quietly create a one-way door.
    local ok=0 m
    for m in "${MANAGED_FLAG_KEYS[@]}"; do [ "$m" = "$key" ] && ok=1; done
    if [ "$ok" -ne 1 ]; then
      echo "::error::TENANT_FLAGS key '$key' is not in MANAGED_FLAG_KEYS — it would be" >&2
      echo "::error::copied into every future redeploy with no way to unset it. Declare it first." >&2
      return 1
    fi
    echo "$kv" >> "$envfile"
  done

  # SAY WHAT WAS ROLLED. Without this line the staging CD log gives no way to tell
  # which flag state the fleet actually came up with — and "the burn-in silently
  # ended two merges ago" is precisely the failure this mechanism must not have.
  if [ -n "$TENANT_FLAGS" ]; then
    echo "  managed flags: [$TENANT_FLAGS] (all other managed keys stripped)"
  else
    echo "  managed flags: none set — every managed key stripped (${MANAGED_FLAG_KEYS[*]})"
  fi
}
export MSYS_NO_PATHCONV=1 MSYS2_ARG_CONV_EXCL='*'

TENANT_IMAGE="${TENANT_IMAGE:-registry.moleculesai.app/molecule-ai/molecule-tenant}"
CANVAS_APP_IMAGE="${CANVAS_APP_IMAGE:-registry.moleculesai.app/molecule-ai/canvas}"
# Shared canvas app is OPT-IN (default empty → skipped), mirroring the prod
# workflow's PROD_CANVAS_APP_CONTAINER. Left empty so the fleet roll never
# clobbers the central staging-app container.
STAGING_CANVAS_APP_CONTAINER="${STAGING_CANVAS_APP_CONTAINER:-}"
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

# FAIL CLOSED IF THE CALLER DID NOT DECLARE ITS FLAG STATE.
#
# This script STRIPS every managed key from the inherited tenant env and re-applies
# only what TENANT_FLAGS names. So a call site that simply forgets to pass it does
# NOT get "the old behaviour" — it silently turns those flags OFF across the entire
# fleet. Shipped as `TENANT_FLAGS="${TENANT_FLAGS:-}"`, this mechanism was a flag
# ERASER, not a flag switch: any unrelated merge to main triggers staging CD, and
# that roll would have ended an in-flight burn-in with no log line.
#
# A grep-the-workflows lint cannot close this — one file can invoke the script
# twice and mention TENANT_FLAGS once. So the requirement is structural and lives
# HERE, at the one place every caller passes through.
#
# Empty is a perfectly good value: it means "all managed flags dark", the default
# and the safe state. What is NOT acceptable is UNSET — a caller that never thought
# about it. Say TENANT_FLAGS="" and mean it.
#
# Placed BELOW arg parsing so --help still prints help, and skipped for --dry-run,
# which performs zero mutations by contract.
if [ "$DRY_RUN" != "1" ] && [ -z "${TENANT_FLAGS+x}" ]; then
  echo "::error::TENANT_FLAGS is not set. This script strips every managed rollout" >&2
  echo "::error::flag (${MANAGED_FLAG_KEYS[*]}) from the inherited tenant env and" >&2
  echo "::error::re-applies only what TENANT_FLAGS declares — so running without it" >&2
  echo "::error::would silently turn those flags OFF on every tenant." >&2
  echo "::error::" >&2
  echo "::error::  flags dark (the default):  TENANT_FLAGS=\"\" bash $0 ..." >&2
  echo "::error::  turn one on:               TENANT_FLAGS=\"DELEGATION_LEDGER_WRITE=1\" bash $0 ..." >&2
  echo "::error::" >&2
  echo "::error::In CI: set STAGING_TENANT_FLAGS in env: and pass" >&2
  echo "::error::TENANT_FLAGS=\"\${STAGING_TENANT_FLAGS-}\" on the command line." >&2
  exit 1
fi
TENANT_FLAGS="${TENANT_FLAGS:-}"   # --dry-run may legitimately reach here unset
log() { printf '>> [fleet] %s\n' "$*" >&2; }

# Resolve the target image ref (either an explicit --image, or TENANT_IMAGE:tag).
if [ -z "$IMAGE" ]; then
  [ -n "$TAG" ] || TAG="staging-latest"
  IMAGE="${TENANT_IMAGE}:${TAG}"
fi
EXPECTED_BUILD_SHA="${EXPECTED_BUILD_SHA:-}"
if [ -z "$EXPECTED_BUILD_SHA" ] && printf '%s' "${TAG:-}" | grep -Eq '^staging-[0-9a-fA-F]{7,40}$'; then
  EXPECTED_BUILD_SHA="${TAG#staging-}"
fi
HEALTH_GATE_ATTEMPTS="${HEALTH_GATE_ATTEMPTS:-20}"
HEALTH_GATE_SLEEP_SECS="${HEALTH_GATE_SLEEP_SECS:-3}"
CANVAS_TAG="${CANVAS_TAG:-${TAG:-staging-latest}}"
CIMG="${CANVAS_APP_IMAGE}:${CANVAS_TAG}"
log "target tenant image = ${IMAGE}  (cp-env=${CP_ENV}, dry_run=${DRY_RUN})"
[ -z "$EXPECTED_BUILD_SHA" ] || log "expected tenant /buildinfo git_sha prefix = ${EXPECTED_BUILD_SHA}"

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

build_sha_matches() {
  local got expected
  got="$(printf '%s' "${1:-}" | tr '[:upper:]' '[:lower:]' | xargs)"
  expected="$(printf '%s' "${2:-}" | tr '[:upper:]' '[:lower:]' | xargs)"
  [ -n "$got" ] && [ -n "$expected" ] || return 1
  case "$got" in
    "$expected"*) return 0;;
  esac
  case "$expected" in
    "$got"*) return 0;;
  esac
  return 1
}

json_git_sha() {
  sed -n 's/.*"git_sha"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p'
}

# health_gate <container>: probe the tenant's published :8080 /buildinfo through
# the host loopback. Returns 0 when it answers and, for staging-<sha> rolls,
# reports the candidate git_sha.
health_gate() {
  local name="$1" port body got last=""
  port="$(docker port "$name" 8080/tcp 2>/dev/null | head -1 | sed 's/.*://')"
  [ -n "$port" ] || return 1
  for _ in $(seq 1 "$HEALTH_GATE_ATTEMPTS"); do
    body="$(curl -fsS --max-time 5 "http://127.0.0.1:${port}/buildinfo" 2>/dev/null || true)"
    if [ -n "$body" ]; then
      got="$(printf '%s' "$body" | json_git_sha | head -1)"
      last="$got"
      if [ -z "$EXPECTED_BUILD_SHA" ]; then
        return 0
      fi
      if build_sha_matches "$got" "$EXPECTED_BUILD_SHA"; then
        log "  ${name} /buildinfo git_sha=${got} matches ${EXPECTED_BUILD_SHA}"
        return 0
      fi
    fi
    sleep "$HEALTH_GATE_SLEEP_SECS"
  done
  if [ -n "$EXPECTED_BUILD_SHA" ]; then
    echo "::error::$name /buildinfo git_sha=${last:-<empty>} did not match expected ${EXPECTED_BUILD_SHA}" >&2
  fi
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
  # Managed rollout flags are re-derived from TENANT_FLAGS, never inherited.
  apply_managed_flags "$envfile" || return 1
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

# Shared staging canvas app — OPT-IN (default OFF), mirroring the production
# workflow's PROD_CANVAS_APP_CONTAINER (empty → skip).
#
# WHY OPT-IN NOW (regression fix): this step used to UNCONDITIONALLY re-install
# the canvas image onto the hard-coded container `molecule-staging-app`. But
# `molecule-staging-app` is the CENTRAL staging console — the CF tunnel routes
# staging-app.moleculesai.app → :3101 to it, and staging-app is the staging
# analogue of app.moleculesai.app, which serves the customer console
# (molecule-app), NOT the per-tenant Org Concierge canvas. So every staging
# fleet roll clobbered the console with the canvas, and staging-app.moleculesai.app
# rendered the tenant "Organization" view — the recurring "staging-app
# recognized as a tenant" regression that kept coming back after each redeploy.
# The canvas is already baked INTO each mol-tenant-* image and served per-tenant
# at <slug>.…; a standalone shared-canvas container is not part of the (prod)
# architecture. Left unset by default so the central staging-app container is
# never touched. To roll a shared canvas somewhere NON-central, set
# STAGING_CANVAS_APP_CONTAINER to that container's name.
if [ -n "${STAGING_CANVAS_APP_CONTAINER}" ] \
   && docker ps -a --format '{{.Names}}' | grep -qx "${STAGING_CANVAS_APP_CONTAINER}"; then
  cvc="${STAGING_CANVAS_APP_CONTAINER}"
  log "== shared canvas app: ${cvc} -> ${CIMG} =="
  docker pull "$CIMG" >/dev/null 2>&1 || log "WARN: pull $CIMG failed; using local image if present"
  if docker image inspect "$CIMG" >/dev/null 2>&1; then
    net="$(docker inspect "$cvc" --format '{{range $k,$v := .NetworkSettings.Networks}}{{$k}}{{end}}')"
    pbind="$(docker inspect "$cvc" --format '{{range $p,$b := .HostConfig.PortBindings}}{{range $b}}{{.HostPort}}{{end}}{{end}}')"
    cenv="$(mktemp)"
    docker inspect "$cvc" --format '{{range .Config.Env}}{{println .}}{{end}}' \
      | grep -vE '^(PATH|NODE_VERSION|YARN_VERSION|NODE_ENV|HOSTNAME|PORT|NEXT_TELEMETRY_DISABLED)=' > "$cenv"
    docker rename "$cvc" "${cvc}-redeploy-bak"
    docker stop "${cvc}-redeploy-bak" >/dev/null
    if docker run -d --name "$cvc" --network "${net:-molecule-net}" \
         --restart unless-stopped --env-file "$cenv" \
         -p "${pbind:-3101}:3000" "$CIMG" >/dev/null 2>/tmp/cvrun.err; then
      sleep 5
      if curl -fsS --max-time 8 "http://127.0.0.1:${pbind:-3101}/" >/dev/null 2>&1; then
        docker rm "${cvc}-redeploy-bak" >/dev/null 2>&1 || true
        log "  ✓ ${cvc} rolled to ${CIMG}"
      else
        echo "::error::${cvc} unhealthy after roll — rolling back" >&2
        docker rm -f "$cvc" 2>/dev/null || true
        docker rename "${cvc}-redeploy-bak" "$cvc"
        docker start "$cvc" >/dev/null
        FAILED=1
      fi
    else
      echo "::error::docker run failed for ${cvc}:" >&2; cat /tmp/cvrun.err >&2
      docker rm -f "$cvc" 2>/dev/null || true
      docker rename "${cvc}-redeploy-bak" "$cvc"
      docker start "$cvc" >/dev/null
      FAILED=1
    fi
    rm -f "$cenv"
  else
    log "WARN: canvas image $CIMG unavailable — skipping ${cvc} roll"
  fi
else
  log "shared canvas app roll skipped (STAGING_CANVAS_APP_CONTAINER unset — default; the central staging-app container is left intact)"
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
