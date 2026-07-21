#!/usr/bin/env bash
# ephemeral_cp_happy_path.sh — RFC "one pre-merge ephemeral gate" §04 PROOF.
#
# Runs the CORE happy-path scenario (test_staging_full_saas.sh) against a
# THROWAWAY control-plane it spins up itself — NOT shared staging. This is the
# core-owned implementation of the intended shared happy-path contract: prove
# full-saas runs against an ephemeral CP with ZERO staging creds,
# image-substituted so the TENANT is THIS PR's build and the CP is a
# caller-selected baseline.
#
# IMAGE SUBSTITUTION (core PR): CP = caller-supplied baseline (CP_IMAGE); tenant
# = THIS PR's workspace-server build (TENANT_IMAGE), fed to the CP's local-docker
# provisioner via LOCAL_TENANT_IMAGE. CI builds CP_IMAGE from the workflow's
# pinned CP_EPHEMERAL_REF; the local wrapper builds it from the selected sibling
# checkout. (A controlplane PR does the inverse.)
#
# Reuses the CP's ephemeral spin-up plumbing (pr-ephemeral-cp.sh) — the shared
# harness the RFC generalizes. No shared staging, no post-merge dependency. The
# SDK migration is still open; this core-owned script remains the implementation.
#
# RUNS LOCALLY AND IN CI (RFC design tenet): this is the SINGLE scenario entry
# point. CI (.gitea/workflows/e2e-ephemeral-happy-path.yml) supplies pinned
# images, creds, and dind; devs run the same scenario on direct Docker with
# `make e2e-ephemeral-happy-path` — validate the full happy path before pushing.
#
# ── MODULAR PHASES — iterate a failing step WITHOUT the full rebuild+boot ─────
# The boot (~minutes: build CP+tenant, boot CP, create DB, migrate) and the
# scenario (~2 min) are independently runnable so you can pinpoint a failing
# step fast instead of paying the whole cycle each time:
#
#   all        (default) boot → scenario → teardown. What CI runs. Unchanged.
#   boot       start PG + boot the CP, LEAVE IT UP, write a reattach state file,
#              and print the exact command to run the scenario against it.
#   scenario   run full-saas against the standing CP from the reattach file.
#              Repeatable in ~2 min — the fast pinpoint loop.
#   down       tear down the standing CP + PG + reattach file.
#
# Fast local loop (iterate on a failing scenario step, no rebuild/reboot):
#   ./ephemeral_cp_happy_path.sh boot         # once  (~minutes)
#   ./ephemeral_cp_happy_path.sh scenario     # many  (~2 min) while you fix
#   ./ephemeral_cp_happy_path.sh down         # when done
# (KEEP_UP=1 ./ephemeral_cp_happy_path.sh all  runs once but leaves the CP up so
#  you can attach a scenario / poke the CP after a failure.)
#
# Required env (boot / all): CP_IMAGE, TENANT_IMAGE, CP_EPHEMERAL_SCRIPT, MINIMAX_API_KEY
# Optional: E2E_RUNTIME (default hermes); PR_NUMBER, HEAD_SHA (name the namespace)

set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CMD="${1:-all}"

RUNTIME="${E2E_RUNTIME:-hermes}"
# The ephemeral env's app domain. lvh.me is public wildcard DNS → 127.0.0.1
# (industry-standard loopback wildcard), which makes the CP's provision-readiness
# canary SELF-CONTAINED: its public-route leg (3) probes
# http://<slug>.lvh.me:8080/workspaces from INSIDE the CP container → DNS
# 127.0.0.1 → the CP's own :8080 → hostSlugFromRequest strips ".lvh.me"
# (== appDomain) → resolveOrg → the CP wildcard proxy → the tenant's WorkspaceAuth
# answers 401 → publicRouteRegistered passes. That genuinely EXERCISES the same
# Host→slug→org→proxy→tenant chain staging uses (minus the CF edge) instead of
# dialing the REAL staging.moleculesai.app edge, which can never resolve a
# throwaway org (observed: canary leg 3 got 404 from live staging → CP marked
# the provision failed at ~30s). MUST match MOLECULE_TOPO_CP_APP_DOMAIN +
# LOCAL_TENANT_URL_TEMPLATE below.
ROUTE_DOMAIN="lvh.me"

# The tenant's PUBLIC front-door URL template — the SINGLE source for both the
# CP's LOCAL_TENANT_URL_TEMPLATE (which the CP turns into the tenant's
# CORS_ORIGINS + PLATFORM_URL via publicTenantURL/PublicURLForSlug) and any
# scenario that must present a same-origin Origin header (the concierge harness).
# The :8080 port is structural, not incidental: lvh.me DNS→127.0.0.1 loops back
# to the CP's own :8080, so the tenant's ONE allowed origin is exactly this
# string with {slug} substituted. Deriving both from a single definition is what
# keeps the Origin a harness sends byte-identical to the origin the CP allows —
# a gin-contrib/cors mismatch is an empty-body 403 (what broke the first port).
TENANT_PUBLIC_URL_TEMPLATE="http://{slug}.${ROUTE_DOMAIN}:8080"

# Throwaway per-run namespace (must start with pr- — pr-ephemeral-cp.sh refuses
# to touch a non-ephemeral namespace). Deterministic in PR_NUMBER/HEAD_SHA so the
# boot / scenario / down phases all address the SAME namespace + state file.
NS="pr-${PR_NUMBER:-0}-$(printf '%s' "${HEAD_SHA:-local0000}" | cut -c1-8)-hp"
case "$NS" in pr-*) : ;; *) echo "FATAL: namespace must start with pr- (got '$NS')" >&2; exit 2 ;; esac
STATE_FILE="${EPHEMERAL_STATE_FILE:-${TMPDIR:-/tmp}/ephemeral-cp-${NS}.env}"

# ── DIND mode (EPHEMERAL_DIND=1) — the CI posture (mc#4081 / task#78) ────────
# The whole topology (PG, CP, tenants, workspaces) runs inside a per-job
# DISPOSABLE docker:dind daemon (tests/harness/dind.sh, the harness-replays
# pattern): the caller sets DOCKER_HOST at the dind BEFORE invoking this runner,
# and one `docker rm -fv` destroys everything atomically — even on cancel. This
# is the structural fix for the SHARED docker-host interference that killed the
# gate's first CI runs (the host-loopback docker-proxy died mid-run with every
# container healthy) and for historical cross-run tenant leaks (an early `down`
# implementation only swept the leg network). The pinned helper now removes all
# non-self containers on the exact namespace network; inside the dind:
#   * published ports must bind 0.0.0.0 (the dind pre-forwards its FIXED :8080
#     to the job's host loopback at create time — dind.sh exports it as $BASE;
#     host.docker.internal inside the dind = the dind's own gateway, which a
#     loopback bind would refuse — the same Linux lesson as CP#1526);
#   * the CP publishes on the dind's fixed :8080 (CP_PUBLISH_ADDR, CP#1549) and
#     the caller-visible URL is the dind forward (CP_HOST_BASE_OVERRIDE=$BASE).
# Local default stays DIRECT (no dind — fast laptop iteration); run
# `bash tests/harness/dind.sh up` first + EPHEMERAL_DIND=1 for CI parity.
if [ "${EPHEMERAL_DIND:-}" = "1" ]; then
  PUBLISH_BIND="0.0.0.0"
  [ -n "${BASE:-}" ] || { echo "FATAL: EPHEMERAL_DIND=1 but \$BASE is unset — start tests/harness/dind.sh up first (it exports BASE=the dind's forwarded :8080)" >&2; exit 2; }
else
  PUBLISH_BIND="127.0.0.1"
fi

rand_hex() { python3 -c 'import secrets;print(secrets.token_hex(32))'; }
# 32 random bytes, base64-encoded (44 chars). REQUIRED for SECRETS_ENCRYPTION_KEY:
# the tenant's parser (workspace-server/internal/crypto/aes.go loadKeyFromEnv)
# accepts ONLY "32 bytes raw or base64-encoded" — a 64-char hex key IS valid
# base64 alphabet and decodes to 48 bytes, so the tenant FATALs at boot
# ("decoded to 48 bytes (expected 32)") while the CP (whose parseKey accepts
# hex OR base64) boots fine. base64(32 bytes) satisfies BOTH parsers.
rand_b64_32() { python3 -c 'import secrets,base64;print(base64.b64encode(secrets.token_bytes(32)).decode())'; }

require_boot_env() {
  : "${CP_IMAGE:?required — the baseline controlplane image}"
  : "${TENANT_IMAGE:?required — this PRs workspace-server/tenant image}"
  : "${CP_EPHEMERAL_SCRIPT:?required — path to pr-ephemeral-cp.sh}"
  [ -x "$CP_EPHEMERAL_SCRIPT" ] || { echo "FATAL: CP_EPHEMERAL_SCRIPT not executable: $CP_EPHEMERAL_SCRIPT" >&2; exit 2; }
}

# ── host_docker / pull_via_host: get an image into the CURRENT docker store
# without paying a registry pull per job.
#
# Under EPHEMERAL_DIND=1 every job gets a BRAND-NEW nested daemon with an empty
# image store, so a plain `docker pull` re-fetches the image from the registry on
# EVERY run. Two things break because of that:
#
#   1. Docker Hub's ANONYMOUS pull limit. postgres:16 came from docker.io with no
#      credentials, so a busy day exhausted the quota and the gate died with
#      "toomanyrequests: You have reached your unauthenticated pull rate limit"
#      before the happy path ran a single step. That reds every PR at once and
#      has nothing to do with any PR's diff.
#   2. Cold-pull latency. The 4.5GB workspace-template pull stalled ~29min mid
#      layer on one run — a 40min gate that is almost entirely download.
#
# The HOST daemon (the one dind.sh itself talks to in order to launch the dind)
# keeps its image cache ACROSS jobs. So: resolve the image on the host once, then
# stream it into the nested daemon over docker save|load. Steady state is zero
# registry traffic per job; a cache-cold host pays exactly one pull, amortized
# over every later job on that runner.
#
# This is not new host access — dind.sh already uses the host daemon to start the
# dind. When DOCKER_HOST is unset we are already talking to the host, so the
# helper degrades to a plain pull and the non-dind path is byte-identical.
host_docker() {
  env -u DOCKER_HOST -u DOCKER_TLS_VERIFY -u DOCKER_CERT_PATH docker "$@"
}

# A cached copy is only authoritative for a ref that CANNOT drift, i.e. one
# pinned by digest. Every other ref is a moving tag — above all
# workspace-template-<rt>:latest, which CI re-pushes on every merge to core.
# Trusting a host-cached copy of a moving tag would pin each runner to whichever
# build it happened to cache FIRST, so two runners would silently run the gate
# against different tenant images and a PR's result would depend on where it
# landed. That is precisely the cross-job shared state this dind work exists to
# eliminate — a cache that skips resolution is just the old shared daemon wearing
# a different hat. So: moving tags are ALWAYS re-resolved against the registry.
#
# This costs a manifest round-trip, not a download: the host's layer cache still
# satisfies every unchanged layer, so the steady-state cost of correctness here
# is one HEAD-sized request. And the refs we pull are on OUR registry, never
# Docker Hub, so no anonymous rate limit applies to that round-trip.
ref_is_digest_pinned() {
  case "$1" in *@sha256:*) return 0 ;; *) return 1 ;; esac
}

pull_via_host() {
  local ref="$1"
  # Not running against a nested daemon: plain pull, unchanged behaviour.
  if [ -z "${DOCKER_HOST:-}" ]; then
    if ref_is_digest_pinned "$ref" && docker image inspect "$ref" >/dev/null 2>&1; then
      echo "[proof] ${ref} is digest-pinned and already local — no registry pull" >&2
      return 0
    fi
    docker pull "$ref" >&2
    return $?
  fi
  # Nested: resolve on the HOST (its layer cache survives across jobs)...
  if ref_is_digest_pinned "$ref" && host_docker image inspect "$ref" >/dev/null 2>&1; then
    echo "[proof] host cache HIT for digest-pinned ${ref} — no registry pull" >&2
  else
    echo "[proof] resolving ${ref} on the host daemon (moving tag → always re-resolve; unchanged layers come from the host cache)..." >&2
    host_docker pull "$ref" >&2 || return 1
  fi
  # ...then stream it into the nested daemon. Bytes move host→dind, not net→dind.
  host_docker save "$ref" | docker load >&2 || return 1
}

# ── seed_workspace_image: put the runtime's template image in the LOCAL docker
# store under its BARE tag. The CP's local-docker workspace provisioner runs
# `docker run workspace-template-<runtime>:latest` (imageregistry.go
# localRuntimeTag — the self-host model expects the tag pre-seeded in the local
# store; on molecules-server the deploy pipeline seeds it). A bare tag can't be
# pulled from docker.io, so an unseeded host fails workspace provisioning with
# "docker run: Unable to find image". Pull our registry ref and retag it for
# the one runtime the gate exercises.
#
# We do NOT skip this when the bare tag already exists in the store. `:latest` is
# a moving tag, so "already present" says nothing about WHICH build is present —
# honouring a leftover tag makes the gate's verdict depend on the runner's
# history. The opt-out is explicit: set WORKSPACE_IMAGE_PRESEEDED=1 when you have
# deliberately put a locally BUILT template under the bare tag and want the gate
# to exercise that instead of the published one.
# ── Real multi-runtime matrix (opt-in, DEFAULT-OFF) ──────────────────────────
# Make the ephemeral CP boot the REAL multi-runtime matrix (hermes + openclaw +
# claude-code) to status=online per-PR, so peer_visibility exercises the SAME
# real-boot matrix staging does instead of the secret-free `external:true` row
# slice. STRICTLY OPT-IN: with E2E_EPHEMERAL_REAL_RUNTIME_MATRIX unset/"" the gate
# is BYTE-IDENTICAL to today — a single-runtime hermes seed and peer_visibility in
# external mode. Both infra legs the matrix needs are already present, verified:
#   (a) all three workspace-template images are published to
#       registry.moleculesai.app/molecule-ai and pull the same way the hermes one
#       already does in CI (digest-pinned below), and
#   (b) all three runtimes carry `minimax/MiniMax-M2.7` in their platform arm
#       (workspace-server/internal/providers/gen/registry_gen.go), and the
#       platform provider is anthropic-compat authed by the CP-injected
#       MOLECULE_LLM_USAGE_TOKEN — so the CP's single MINIMAX_API_KEY serves the
#       WHOLE matrix through /cp/internal/llm. NO anthropic/moonshot key is needed.
# It stays opt-in because the openclaw/claude-code cold-boot-to-online inside the
# nested dind has not yet SOAKED — arm it (=1) only after a maintainer proves the
# matrix green in CI. Arm a subset first via E2E_EPHEMERAL_MATRIX_RUNTIMES.
real_runtime_matrix_enabled() {
  case "${E2E_EPHEMERAL_REAL_RUNTIME_MATRIX:-}" in
    1|true|yes|on) return 0 ;;
    *) return 1 ;;
  esac
}

# The runtime set the gate stands up template images for (and, matrix-on, boots to
# online). OFF: exactly the single E2E_RUNTIME (hermes) — today's behaviour. ON:
# the full maintained matrix, overridable so a maintainer can soak a SUBSET (e.g.
# drop claude-code) with E2E_EPHEMERAL_MATRIX_RUNTIMES without a code change.
matrix_runtimes() {
  if real_runtime_matrix_enabled; then
    printf '%s' "${E2E_EPHEMERAL_MATRIX_RUNTIMES:-hermes openclaw claude-code}"
  else
    printf '%s' "$RUNTIME"
  fi
}

# peer_visibility provisioning mode: `managed` (real boot to online) when the
# matrix is armed, else `external` (awaiting_agent rows, no container/LLM) — the
# default, byte-identical to today.
pv_provision_mode() {
  if real_runtime_matrix_enabled; then printf 'managed'; else printf 'external'; fi
}

# ── resolve_template_ref: the pinned registry ref for one runtime's template
# image. DIGEST-PIN (audit Q1 #2 / no-flakes moving-tag rule):
# workspace-template-<rt>:latest is re-pushed on every template merge, so a
# failed/partial rebuild could silently change WHICH image this REQUIRED gate
# provisions with NO core diff, and red (or worse, false-pass) an unrelated PR on
# a template change nobody reviewed. Same hazard/fix as CP_EPHEMERAL_REF in
# .gitea/workflows/e2e-ephemeral-happy-path.yml. Image-layer only: this does NOT
# touch the in-image @molecule-ai/mcp-server npm version-range resolution (that
# runs at workspace runtime and is orthogonal to which layer we pull here).
#
# TO BUMP a pin: read the new digest and paste it below — in its own reviewed PR,
# so the template advance is reviewed as what gates the gate —
#   skopeo inspect --format '{{.Digest}}' \
#     docker://registry.moleculesai.app/molecule-ai/workspace-template-<rt>:latest
# (equivalently `docker buildx imagetools inspect ...:latest`, the template repo's
#  Build&push `pushed ...@sha256:...` log line, or a registry v2 manifest HEAD).
# Each pin is overridable via WORKSPACE_TEMPLATE_<RT>_REF (RT upper, '-'→'_').
#   hermes      = runtime 0.4.31 / mcp-server 1.9.3 (sha-4dad193) — UNCHANGED;
#                 the happy-path's input, do NOT retag it on this task.
#   openclaw    = workspace-template-openclaw:latest resolved 2026-07-20.
#   claude-code = workspace-template-claude-code:latest resolved 2026-07-20.
resolve_template_ref() {
  local rt="$1"
  local registry="${MOLECULE_IMAGE_REGISTRY:-registry.moleculesai.app/molecule-ai}"
  case "$rt" in
    hermes)      printf '%s' "${WORKSPACE_TEMPLATE_HERMES_REF:-${registry}/workspace-template-hermes@sha256:8aab6b48f2af15c3dd1056a5d1f5e7a61fb9a8a66aa7aca5b51cf2d2702215f9}" ;;
    openclaw)    printf '%s' "${WORKSPACE_TEMPLATE_OPENCLAW_REF:-${registry}/workspace-template-openclaw@sha256:24085f9895af728b7166f9f7908cf4a6b825a4c98abdeb0c1462daa8a5838715}" ;;
    claude-code) printf '%s' "${WORKSPACE_TEMPLATE_CLAUDE_CODE_REF:-${registry}/workspace-template-claude-code@sha256:50b8068739f35fd3c7404afe7b8336f5057285b12e73b69e62ccd5c6dcc6ddab}" ;;
    *)
      # No pinned digest for this runtime; fall back to the moving tag
      # (unpinned, best-effort — the gate does not exercise it by default).
      printf '%s' "${registry}/workspace-template-${rt}:latest" ;;
  esac
}

# Seed ONE runtime's template image into the LOCAL docker store under its BARE
# tag (see the file-level comment above; extracted so the matrix path can seed
# each runtime with byte-identical behaviour).
seed_one_runtime_image() {
  local rt="$1"
  local bare="workspace-template-${rt}:latest"
  if [ -n "${WORKSPACE_IMAGE_PRESEEDED:-}" ]; then
    docker image inspect "$bare" >/dev/null 2>&1 \
      || { echo "FATAL: WORKSPACE_IMAGE_PRESEEDED=1 but ${bare} is not in the docker store" >&2; exit 1; }
    echo "[proof] WORKSPACE_IMAGE_PRESEEDED=1 — using the ${bare} already in the store, not the published one" >&2
    return 0
  fi
  local ref; ref="$(resolve_template_ref "$rt")"
  echo "[proof] seeding ${bare} from ${ref}..." >&2
  # Resolve on the host cache (digest = content-addressed, so a HIT is exact) or pull,
  # then stream into the nested daemon CAPTURING the ID that `docker load` reports, and
  # tag THAT to the bare name. We must use the load-reported ID (not `image inspect`)
  # for TWO independent reasons, both observed on the CI dind:
  #  (1) the digest-ref NAME does not survive `docker save | docker load` — the image
  #      lands by ID only, so `docker tag <digest-ref> $bare` fails "No such image"; and
  #  (2) the HOST (containerd image store) reports the MANIFEST digest as `.Id` for a
  #      digest-ref, which is NOT the CONFIG-digest ID the dind loaded, so tagging a
  #      host-inspected ID ALSO fails "No such image". `docker load`'s reported ID is
  #      the dind's own handle — the only reliable one. (Moving-tag refs work too:
  #      save|load round-trips them and load still reports the loaded ID.)
  if ref_is_digest_pinned "$ref" && host_docker image inspect "$ref" >/dev/null 2>&1; then
    echo "[proof] host cache HIT for digest-pinned ${ref} — no registry pull" >&2
  else
    host_docker pull "$ref" >&2 || { echo "FATAL: cannot obtain ${ref} — the CP cannot provision ${rt} workspaces without it" >&2; exit 1; }
  fi
  local _load_out img_id
  _load_out="$(host_docker save "$ref" | docker load 2>&1)" || { echo "FATAL: could not stream ${ref} into the nested daemon" >&2; exit 1; }
  printf '%s\n' "$_load_out" >&2
  img_id="$(printf '%s\n' "$_load_out" | sed -n 's/^Loaded image ID: *//p; s/^Loaded image: *//p' | tail -1)"
  [ -n "$img_id" ] || { echo "FATAL: could not determine the loaded image ID for ${ref} from docker load output" >&2; exit 1; }
  docker tag "$img_id" "$bare"
  echo "[proof] seeded ${bare}" >&2
}

# Seed every runtime the gate will provision: just E2E_RUNTIME (hermes) by
# default — byte-identical to the pre-matrix single-image seed — or the full
# maintained matrix when E2E_EPHEMERAL_REAL_RUNTIME_MATRIX=1.
seed_workspace_image() {
  local rt
  for rt in $(matrix_runtimes); do
    seed_one_runtime_image "$rt"
  done
}

# ── start_pg: throwaway postgres:16 on an ephemeral host port ────────────────
# The CP's `up` requires an EXTERNAL PG — it creates a fresh per-run database on
# it and does NOT stand up its own. `up` reaches it via --pg-container (docker
# exec psql — the runner image has no host psql client) and via
# host.docker.internal:<port> from the CP container. Sets PG_CTR / PG_PORT.
PG_CTR=""; PG_PORT=""; PG_SUPERPASS="ephemeral-pr-pg"
# Mirrored into OUR registry, digest-identical to Hub's postgres:16. Hub caps
# ANONYMOUS pulls (~100/6h per IP), and a fresh dind has an EMPTY image store —
# so pulling this from Hub on every job is what exhausted the quota and killed
# the gate before step 1. Override with PG_IMAGE=postgres:16 to go back to Hub.
PG_IMAGE="${PG_IMAGE:-registry.moleculesai.app/molecule-ai/postgres:16}"
start_pg() {
  PG_CTR="pg-${NS}"
  docker rm -f "$PG_CTR" >/dev/null 2>&1 || true
  # Seed via the host cache: a bare `docker run postgres:16` inside a fresh dind
  # pulls from Docker Hub ANONYMOUSLY on every job, and that quota runs out
  # ("toomanyrequests: unauthenticated pull rate limit") — reding every open PR
  # at once, before the happy path executes a single step.
  pull_via_host "$PG_IMAGE" \
    || { echo "FATAL: could not obtain ${PG_IMAGE} for ${PG_CTR}" >&2; exit 1; }
  docker run -d --name "$PG_CTR" \
    -e POSTGRES_USER=postgres -e POSTGRES_PASSWORD="$PG_SUPERPASS" \
    -e POSTGRES_DB=postgres \
    -p "${PUBLISH_BIND}:0:5432" "$PG_IMAGE" >/dev/null \
    || { echo "FATAL: could not start ephemeral Postgres container ${PG_CTR}" >&2; exit 1; }
  PG_PORT="$(docker port "$PG_CTR" 5432/tcp | awk -F: '/127\.0\.0\.1:/ {print $2; exit}')"
  [ -n "$PG_PORT" ] || PG_PORT="$(docker port "$PG_CTR" 5432/tcp | head -1 | awk -F: '{print $NF}')"
  [ -n "$PG_PORT" ] || { echo "FATAL: no host port for ${PG_CTR}" >&2; docker logs "$PG_CTR" 2>&1 | tail -20 >&2 || true; exit 1; }
  local ready=""
  # wait_for: poll the REAL ready signal (pg_isready); the 30x 1s loop is a
  # safety deadline we break out of the instant PG is ready, never wait out.
  for _ in $(seq 1 30); do
    if docker exec "$PG_CTR" pg_isready -U postgres >/dev/null 2>&1; then ready=1; break; fi
    sleep 1  # wait_for: pg_isready backoff (real-signal poll above, not a fixed timer)
  done
  [ -n "$ready" ] || { echo "FATAL: ephemeral Postgres ${PG_CTR} never became ready" >&2; docker logs "$PG_CTR" 2>&1 | tail -20 >&2 || true; exit 1; }
  echo "[proof] ephemeral PG ${PG_CTR} ready on 127.0.0.1:${PG_PORT}" >&2
}

# ── boot_cp: assemble the boot-env, boot the CP via `up` (sets CP_BASE_URL) ──
CP_BASE_URL=""; CP_ADMIN_API_TOKEN=""
boot_cp() {
  CP_ADMIN_API_TOKEN="$(rand_hex)"   # reused for the CP boot AND full-saas's admin calls
  local boot_env; boot_env="$(mktemp)"; chmod 600 "$boot_env"
  {
    echo "MOLECULE_ENV=e2e"
    # EXPLICIT molecules-server provider (local-docker backend); AWS off.
    echo "PROVISIONER_BACKEND=docker"
    echo "MOLECULE_DEFAULT_PROVIDER=molecules-server"
    echo "MOLECULE_AWS_ENABLED=false"
    echo "MOLECULE_DEFAULT_RUNTIME=${RUNTIME}"
    # MOLECULE_ENV=e2e is a REAL provisioning env: the CP does NOT boot-fetch from
    # Infisical and FAIL-CLOSES at boot unless the full MOLECULE_TOPO_* staging-mirror
    # set is injected (controlplane cmd/server/bootsecrets.go requireE2ETopologyInjected
    # over internal/envs RequiredTopologyKeys). These are the NON-SECRET staging-mirror
    # topology labels the CP ships as envs.E2EStagingMirrorTopology(); MOLECULE_AWS_ENABLED
    # =false above means none are ever dialed — they only satisfy the boot assertion and
    # set CP appDomain=${ROUTE_DOMAIN} (used for slug routing in the scenario).
    echo "MOLECULE_TOPO_AWS_ACCOUNT_ID=004947743811"
    echo "MOLECULE_TOPO_AWS_REGION=us-east-2"
    echo "MOLECULE_TOPO_AWS_VPC_ID=vpc-0f35ce782265b34dd"
    echo "MOLECULE_TOPO_AWS_SUBNET_ID=subnet-0bf1813c16efe69c6"
    echo "MOLECULE_TOPO_AWS_SECURITY_GROUP_ID=sg-0996f755348630e6d"
    echo "MOLECULE_TOPO_AWS_TENANT_INSTANCE_PROFILE=MoleculeTenantEICRole-staging"
    echo "MOLECULE_TOPO_AWS_WORKSPACE_INSTANCE_PROFILE=MoleculeTenantEICRole-staging"
    echo "MOLECULE_TOPO_AWS_TENANT_AMI=ami-09cdbb1de48dd8f3c"
    echo "MOLECULE_TOPO_AWS_TENANT_IMAGE=004947743811.dkr.ecr.us-east-2.amazonaws.com/molecule-ai/platform-tenant:latest"
    echo "MOLECULE_TOPO_AWS_ECR_REGISTRY=004947743811.dkr.ecr.us-east-2.amazonaws.com"
    echo "MOLECULE_TOPO_CF_ZONE=moleculesai.app"
    echo "MOLECULE_TOPO_CF_ZONE_ID=a034108eda16d131ef7f766b923ef464"
    echo "MOLECULE_TOPO_CF_TENANT_SUBDOMAIN_SUFFIX=staging.moleculesai.app"
    echo "MOLECULE_TOPO_CP_APP_DOMAIN=${ROUTE_DOMAIN}"
    # SELF-REFERENTIAL CP base URL — NOT the staging mirror. Two delivery paths
    # flow from these into the tenant and would otherwise point the ephemeral
    # tenant at a LIVE environment:
    #   1. /cp/tenants/config delivers MOLECULE_CP_URL := os.Getenv("CP_BASE_URL")
    #      (controlplane tenant_config.go). With CP_BASE_URL unset it delivered
    #      "" — the tenant's boot refresh BLANKED its injected MOLECULE_CP_URL
    #      and cpurl.Base() fell through to the managed default
    #      https://api.moleculesai.app: the ephemeral tenant sent its workspace
    #      provision POST to PROD and died on a 401 (observed: "cp provisioner:
    #      provision failed (401): <unstructured body, 0 bytes>", plus the
    #      concierge's platform-default-model fetch 401).
    #   2. MOLECULE_TOPO_CP_BASE_URL → molEnv.CP.BaseURL → LLMProxyBaseURL →
    #      the MOLECULE_LLM_* proxy env injected into tenant+workspaces. On the
    #      staging value, workspace LLM traffic would egress to the REAL
    #      staging-api instead of THIS CP's /cp/internal/llm proxy.
    # http://controlplane:8080 is the CP's own alias on the shared docker
    # network — every consumer (tenant, workspaces) lives on that network.
    echo "MOLECULE_TOPO_CP_BASE_URL=http://controlplane:8080"
    echo "CP_BASE_URL=http://controlplane:8080"
    # Canary public-route leg (3) loop-back — see the ROUTE_DOMAIN comment. The
    # template WINS over the https://<slug>.<domain> default in publicTenantURL,
    # so the canary probes http://<slug>.lvh.me:8080 (DNS→127.0.0.1 = the CP's
    # own loopback, :8080 = the CP itself) and traverses the REAL wildcard-proxy
    # routing chain to the tenant instead of the unreachable staging edge.
    echo "LOCAL_TENANT_URL_TEMPLATE=${TENANT_PUBLIC_URL_TEMPLATE}"
    # `up` creates the network as mol-net-${NS}; the CP provisions tenants onto it.
    echo "LOCAL_TENANT_SHARED_NETWORK=mol-net-${NS}"
    echo "LOCAL_TENANT_CP_URL=http://controlplane:8080"
    # LINUX containerized-CP posture (CP #1526): tenant ports must publish on
    # 0.0.0.0 — a 127.0.0.1-bound host port is unreachable from inside the CP
    # container on Linux (host.docker.internal = the gateway IP, which a
    # loopback bind does not accept), so the CP's tenant health probe times
    # out every provision (observed on this gate's first two CI runs; the
    # same run is green on macOS, where Docker Desktop special-cases
    # host.docker.internal to reach loopback binds). Ignored by CPs without
    # the knob; strict {127.0.0.1,0.0.0.0} allowlist on the CP side.
    echo "LOCAL_TENANT_BIND_ADDR=0.0.0.0"
    # THROWAWAY crown jewels (RFC finding #1-A): the CP + DB are disposable, so
    # these only need to be self-consistent for the life of the run.
    echo "CP_ADMIN_API_TOKEN=${CP_ADMIN_API_TOKEN}"
    # base64(32B), NOT hex — the tenant refuses to boot on a hex key (see rand_b64_32).
    echo "SECRETS_ENCRYPTION_KEY=$(rand_b64_32)"
    echo "PROVISION_SHARED_SECRET=$(rand_hex)"
    # IMAGE SUBSTITUTION: the CP provisions tenants with THIS PR's tenant image.
    echo "LOCAL_TENANT_IMAGE=${TENANT_IMAGE}"
    # e2e LLM key — the SAME real key the post-merge gate uses (RFC finding D
    # moves it to a dedicated low-value e2e Infisical path later).
    [ -n "${MINIMAX_API_KEY:-}" ] && echo "MINIMAX_API_KEY=${MINIMAX_API_KEY}"
  } >> "$boot_env"

  echo "[proof] spinning up throwaway CP (baseline ${CP_IMAGE}) provisioning tenant ${TENANT_IMAGE} in ${NS}..." >&2
  # DIND mode: the CP must bind the dind's fixed :8080 on ALL interfaces
  # (CP_PUBLISH_ADDR, CP#1549 allowlist) and the caller-visible URL is the
  # dind's pre-forwarded host-loopback port (CP_HOST_BASE_OVERRIDE=$BASE from
  # dind.sh) — `up`'s boot verify then proves reach THROUGH the forward.
  if [ "${EPHEMERAL_DIND:-}" = "1" ]; then
    export CP_PUBLISH_ADDR="0.0.0.0:8080"
    export CP_HOST_BASE_OVERRIDE="$BASE"
  fi
  # `up` prints CP_BASE_URL= / CP_BASE_URL_HOST= / NS= on stdout (log() → stderr).
  # Capture first, then eval — avoids nested double-quotes inside "$(...)".
  local up_output
  up_output=$("$CP_EPHEMERAL_SCRIPT" up --ns "$NS" --image "$CP_IMAGE" \
    --pg-host 127.0.0.1 --pg-port "$PG_PORT" --pg-container "$PG_CTR" \
    --pg-superuser postgres --pg-superpass "$PG_SUPERPASS" \
    --boot-env-file "$boot_env")
  local up_rc=$?
  rm -f "$boot_env" 2>/dev/null || true
  [ "$up_rc" -eq 0 ] || { echo "FATAL: ephemeral CP up exited $up_rc" >&2; exit 1; }
  eval "$up_output"
  [ -n "${CP_BASE_URL:-}" ] || { echo "FATAL: ephemeral CP up did not emit CP_BASE_URL (see its FATAL above)" >&2; exit 1; }
  echo "[proof] ephemeral CP serving at ${CP_BASE_URL}" >&2
}

write_state() {
  umask 077
  {
    echo "NS=${NS}"
    echo "PG_CTR=${PG_CTR}"
    echo "PG_PORT=${PG_PORT}"
    echo "CP_BASE_URL=${CP_BASE_URL}"
    echo "CP_ADMIN_API_TOKEN=${CP_ADMIN_API_TOKEN}"
    echo "RUNTIME=${RUNTIME}"
    echo "ROUTE_DOMAIN=${ROUTE_DOMAIN}"
    echo "MINIMAX_API_KEY=${MINIMAX_API_KEY:-}"
  } > "$STATE_FILE"
  chmod 600 "$STATE_FILE"
}

load_state() {
  [ -f "$STATE_FILE" ] || { echo "FATAL: no reattach state at ${STATE_FILE} — run '$0 boot' first (same PR_NUMBER/HEAD_SHA)." >&2; exit 2; }
  # shellcheck disable=SC1090
  . "$STATE_FILE"
}

# ── run_scenario: full-saas against the standing CP (uses globals set above or
# loaded from state). The CP wildcard proxy routes tenants by SLUG (Host /
# X-Molecule-Org-Slug), NOT by X-Molecule-Org-Id (the CP injects that toward the
# tenant). MOLECULE_TENANT_URL=CP_BASE_URL sends tenant traffic at the CP;
# MOLECULE_TENANT_ROUTE_DOMAIN makes full-saas attach Host=<slug>.<domain> +
# X-Molecule-Org-Slug so the CP routes it to the provisioned tenant. Zero staging
# creds: the admin token is the throwaway one baked into the ephemeral CP.
#
# E2E_MODE defaults to FULL (2026-07-13). It used to be pinned to `smoke`, whose
# stated reason was: "memory plugin — needs MEMORY_PLUGIN_URL infra the ephemeral
# env doesn't stand up". THAT REASON IS DEAD, and leaving the pin would have
# silently capped this gate at the happy-path core forever:
#
#   * core#4166 makes the memory sidecar BUNDLED and default-ON — the tenant
#     defaults MEMORY_PLUGIN_URL to its own loopback sidecar and starts it
#     whenever DATABASE_URL is set (workspace-server/entrypoint-tenant.sh), and
#     the binary is baked into Dockerfile.tenant. No external URL, no extra infra.
#   * the CP's local-docker provisioner already injects DATABASE_URL against a
#     PGVECTOR postgres, and already runs a per-tenant REDIS sibling.
#
# So `full` needs NOTHING the ephemeral env lacks — only TIME (+1 child workspace,
# +2 LLM completions, 2 re-provisions), which is why the CI timeout went to 75m.
# full adds, on top of smoke's provision → tenant online → workspace online+
# routable → A2A completion: the child workspace, HMA memories (step 9), peers +
# activity (9b), KV memory (9c), the delegation matrix (10), and the
# pause/resume/hibernate/wake lifecycle (10b — E2E_LIFECYCLE defaults to `auto`).
#
# That is the whole of `E2E Staging SaaS`'s coverage except its BYOK LLM leg (this
# gate runs the PLATFORM leg — see below), and it is what lets the post-merge
# staging lanes be retired instead of merely duplicated. Override with
# E2E_MODE=smoke for a fast local loop.
# workspace provisioned with NO tenant key, model = the hermes default
# minimax/MiniMax-M2.7 (SLASH form = PLATFORM-managed per CTO task#83; the
# CP LLM proxy bills, required_env=[]), completion flows workspace → tenant
# proxy env → THIS ephemeral CP's /cp/internal/llm proxy → api.minimax.io with
# the CP's own MINIMAX_API_KEY (injected via the boot-env). The BYOK matrix is
# the staging BYOK job's coverage, not this gate's. NOTE: hermes' harness model
# map has no MiniMax BYOK arm (pick_model_slug hermes → openai/gpt-4o), so a
# stray E2E_MINIMAX_API_KEY here would go BYOK-openai and 422 — the platform
# arm also deliberately ignores E2E_*_API_KEY for exactly this class of drift.
run_scenario() {
  echo "[proof] running core happy-path (full-saas, runtime=${RUNTIME}) against the ephemeral CP — zero staging creds..." >&2
  # E2E_WORKSPACE_ONLINE_TIMEOUT_SECS bounds the SCRIPT's waits, not just the job.
  # test_staging_full_saas.sh defaults WORKSPACE_ONLINE_TIMEOUT_SECS to 3600 (sized for
  # shared-staging cold boot), and full mode spends that budget THREE times on independent
  # deadlines: step 7 boot, the 10b resume, and the 10b hibernate-wake. 3×3600s = 180
  # min of legally-waiting script against a 75-min job cap — so a wedged re-provision
  # would be killed by the runner with the named `fail` never printed and the diagnostic
  # burst never collected. An opaque runner kill is exactly the un-readable red this
  # gate exists to abolish. 900s is generous for a local docker re-provision (observed
  # ~10s) while keeping every arm of the scenario inside the job budget.
  #
  # NOTE: keep this comment ABOVE the command. A comment placed between the
  # backslash-continued env assignments below terminates the logical line — `#` would
  # swallow the rest of it, severing the env prefix from the `bash` call, and the
  # scenario would run with no admin token and no E2E_REQUIRE_LIVE. Shellcheck catches
  # it as SC2034 ("appears unused"), which is what those assignments become.
  #
  # E2E_SCHEDULER_CHECK and E2E_DIGEST_PLUGIN_CHECK default ON: the precondition
  # that used to keep steps 10d/10e dark — a provisioned runtime image without the
  # 0.4.9 bits — was verified CLEARED on 2026-07-16. The gate provisions the
  # DIGEST-PINNED workspace-template-hermes @sha256:8aab6b48… (sha-4dad193 = runtime
  # 0.4.31; see seed_workspace_image). That tree carries all four legs both steps
  # need: the kind:trigger scaffold, declared-plugin boot-install
  # (MOLECULE_DECLARED_PLUGINS), the digest-provider plugin loader, and the
  # vendored native-plugins registry TRUST source (runtime#310).
  #
  # PINNED-image note (no-flakes rule): the gate NO LONGER re-resolves :latest — it
  # pins the digest above, so a template drift/rollback cannot silently change this
  # required gate's input. If 10d/10e reds in the future, FIRST check whether
  # WORKSPACE_TEMPLATE_HERMES_REF is STALE vs a newer INTENTIONAL template republish
  # (bump the pinned digest in seed_workspace_image) as the likely mechanism
  # before theorizing about core-side causes.
  #
  # 10e scope note still holds: the baked digest roster renders every section
  # regardless (D1 flag default-off), so 10e gates only the NEW native
  # plugin-load path, not the digest itself.
  #
  # E2E_SELF_SCHEDULE_CHECK defaults ON (10f), like 10d/10e above — it is a native
  # plugin, gated by default. Step 10f deterministically drives the audience:self
  # create_schedule TOOL via a docker-exec self-mode mcp-server
  # (@molecule-ai/mcp-server 1.9.3) and asserts it lands on the OWN grid + fires
  # (org-key/foreign-id neg controls). Its JOINED precondition was VERIFIED CLEARED
  # 2026-07-18 and is now DIGEST-PINNED (seed_workspace_image): the gate provisions
  # workspace-template-hermes @sha256:8aab6b48… (sha-4dad193 = runtime 0.4.31), which
  # carries runtime#328's self-audience injector (_inject_self_env) + #329 hardening
  # (force MOLECULE_WORKSPACE_TOKEN_FILE, inject WORKSPACE_ID) + #330/#333's prebake of
  # mcp-server 1.9.3 (self-mode X-Molecule-Org-Id tenant routing) via
  # scripts/prebake-mgmt-mcp.sh + platform_agent_identity.py PINNED_VERSION=1.9.3 /
  # COMPATIBLE_RANGE=^1.8.0, so `npx --prefer-offline` resolves offline in the
  # no-network gate. The injector fires on audience:self alone (the plugin's
  # runtimes:[claude_code] is not consulted on the render path), so the self entry
  # renders on the gate's default hermes template too.
  #
  # PINNED-image note (no-flakes rule): the gate NO LONGER re-resolves :latest — it
  # pins the digest in seed_workspace_image, so a template drift/rollback CANNOT
  # silently change this required gate's input. If 10f reds in the future, FIRST check
  # whether WORKSPACE_TEMPLATE_HERMES_REF is STALE vs a newer INTENTIONAL template
  # republish (bump the pinned digest in seed_workspace_image, its own PR) before
  # theorizing about core-side causes. Keep this note ABOVE the env block (a `#` BETWEEN the
  # backslash-continued lines below would sever the env prefix — see the 444-448 note).
  MOLECULE_CP_URL="${CP_BASE_URL}" \
  MOLECULE_TENANT_URL="${CP_BASE_URL}" \
  MOLECULE_TENANT_ROUTE_DOMAIN="${ROUTE_DOMAIN}" \
  MOLECULE_ADMIN_TOKEN="${CP_ADMIN_API_TOKEN}" \
  E2E_REQUIRE_LIVE=1 \
  E2E_RUNTIME="${RUNTIME}" \
  E2E_BUSY_INJECT="${E2E_BUSY_INJECT:-0}" \
  E2E_HIBERNATE_FORCE_BUSY_REQUIRED="${E2E_HIBERNATE_FORCE_BUSY_REQUIRED:-0}" \
  E2E_LLM_PATH=platform \
  E2E_INFRA_BACKEND=local-docker \
  E2E_CP_ALLOW_EPHEMERAL_LOOPBACK=1 \
  E2E_MODE="${E2E_MODE:-full}" \
  E2E_PROVISION_TIMEOUT_SECS="${E2E_PROVISION_TIMEOUT_SECS:-300}" \
  E2E_WORKSPACE_ONLINE_TIMEOUT_SECS="${E2E_WORKSPACE_ONLINE_TIMEOUT_SECS:-900}" \
  E2E_IDLE_DIGEST_CHECK="${E2E_IDLE_DIGEST_CHECK:-on}" \
  E2E_IDLE_FIRE_SECONDS="${E2E_IDLE_FIRE_SECONDS:-30}" \
  E2E_SCHEDULER_CHECK="${E2E_SCHEDULER_CHECK:-on}" \
  E2E_TRIGGER_POLL_SECONDS="${E2E_TRIGGER_POLL_SECONDS:-10}" \
  E2E_DIGEST_PLUGIN_CHECK="${E2E_DIGEST_PLUGIN_CHECK:-on}" \
  E2E_DIGEST_PLUGIN_SOURCE="${E2E_DIGEST_PLUGIN_SOURCE:-gitea://molecule-ai/molecule-ai-plugin-digest-mail#v0.1.0}" \
  E2E_SELF_SCHEDULE_CHECK="${E2E_SELF_SCHEDULE_CHECK:-on}" \
  E2E_SELF_SCHEDULE_PLUGIN_SOURCE="${E2E_SELF_SCHEDULE_PLUGIN_SOURCE:-gitea://molecule-ai/molecule-ai-plugin-schedule-self#v0.1.2}" \
  E2E_SELF_SCHEDULE_MCP_SPEC="${E2E_SELF_SCHEDULE_MCP_SPEC:-@molecule-ai/mcp-server@1.9.3}" \
    bash "$HERE/test_staging_full_saas.sh"
}

# ── Extra scenarios (RFC #86 — pull the post-push staging lanes LEFT) ────────
# The happy-path above IS the full-saas journey. These are the OTHER
# `e2e-staging-saas.yml` lanes that today run LIVE only on push[main] (a `bash -n`
# self-check on pull_request), being moved one at a time onto THIS per-PR gate so
# a green PR actually exercises them. Each runs against the SAME standing
# throwaway CP, creates its OWN org (isolated from the happy-path's org and from
# sibling extras) and tears that org down itself, so ordering is independent.
# They run only AFTER the happy-path passes — a broken CP would red them for
# reasons unrelated to the scenario under test.
#
# ADVISORY SOAK: while E2E_EPHEMERAL_EXTRA_ADVISORY=1 an extra scenario's failure
# is logged LOUD but does NOT fail the (required) gate — we prove the scenario is
# deterministic per-PR before it can block merges (and before the post-push lane
# it mirrors is retired). Flip the flag to 0 in the workflow once it has soaked
# green to promote it to a real gate. This is the capture-first / enforce-later
# path: the scenario is genuinely RUN every PR (non-vacuous) the whole time.

# concierge user_tasks — the agent→user "ask" contract over BOTH surfaces (REST +
# the MCP tools/call envelope) plus cross-workspace authz scoping. Workspaces are
# `external` rows (no container/LLM), so this needs only the CP + API — the
# cheapest lane to move left. Mirrors e2e-staging-saas.yml job
# `e2e-staging-concierge-user-tasks`.
run_scenario_concierge_user_tasks() {
  echo "[proof][extra] concierge user_tasks (agent->user ask over REST+MCP, cross-ws authz) against the ephemeral CP..." >&2
  # MOLECULE_TENANT_URL + ROUTE_DOMAIN carry the ephemeral-CP slug routing the
  # same way run_scenario does (the CP is one throwaway container; the tenant is
  # reached via its base URL + Host/X-Molecule-Org-Slug, not a real subdomain).
  # MOLECULE_TENANT_ORIGIN_TEMPLATE — the ONLY scenario that sends an Origin
  # header (mirroring the Cloudflare edge). In staging Origin == the tenant's own
  # https subdomain, which IS its CORS_ORIGINS, so it passes. Here the tenant's
  # CORS_ORIGINS is TENANT_PUBLIC_URL_TEMPLATE (set as LOCAL_TENANT_URL_TEMPLATE
  # above), NOT the CP base URL — so the harness must present THAT origin or
  # gin-contrib/cors returns an empty-body 403. Pass the SAME template so the two
  # can never drift; the harness substitutes its own {slug}.
  MOLECULE_CP_URL="${CP_BASE_URL}" \
  MOLECULE_TENANT_URL="${CP_BASE_URL}" \
  MOLECULE_TENANT_ROUTE_DOMAIN="${ROUTE_DOMAIN}" \
  MOLECULE_TENANT_ORIGIN_TEMPLATE="${TENANT_PUBLIC_URL_TEMPLATE}" \
  MOLECULE_ADMIN_TOKEN="${CP_ADMIN_API_TOKEN}" \
  E2E_INFRA_BACKEND=local-docker \
  E2E_CP_ALLOW_EPHEMERAL_LOOPBACK=1 \
  E2E_PROVISION_TIMEOUT_SECS="${E2E_PROVISION_TIMEOUT_SECS:-300}" \
  E2E_RUN_ID="${PR_NUMBER:-0}-${HEAD_SHA:-local0000}-cncrgut" \
    bash "$HERE/test_staging_concierge_e2e.sh"
}

# concierge CREATES-A-WORKSPACE — the FULL real-workspace-boot journey: a fresh
# org's concierge (kind='platform' root) is driven over A2A to invoke its
# platform-MCP provision_workspace verb, the created member boots a REAL runtime,
# and it round-trips a REAL platform-managed MiniMax completion (17x23=391).
# Unlike run_scenario_concierge_user_tasks (which uses `external` rows, no
# container/LLM — the cheapest lane), this boots REAL tenant + member containers
# and runs REAL LLM turns. The ephemeral CP already boots real workspaces + real
# A2A/MiniMax completions in its own happy path (child workspace + delegation
# matrix, model = the hermes default minimax/MiniMax-M2.7 PLATFORM-managed via
# THIS CP's /cp/internal/llm proxy + MINIMAX_API_KEY), so the infra fully
# supports it. Mirrors e2e-staging-saas.yml job
# `e2e-staging-concierge-creates-workspace`.
run_scenario_concierge_creates_workspace() {
  echo "[proof][extra] concierge CREATES-a-workspace (real A2A -> platform-MCP provision_workspace + member boot + real MiniMax turn) against the ephemeral CP..." >&2
  # Topology env identical to run_scenario_concierge_user_tasks: MOLECULE_TENANT_URL
  # + ROUTE_DOMAIN carry the ephemeral-CP slug routing (Host + X-Molecule-Org-Slug),
  # and MOLECULE_TENANT_ORIGIN_TEMPLATE presents the tenant's real CORS_ORIGINS
  # (= LOCAL_TENANT_URL_TEMPLATE, set on the CP boot-env) so the Origin the script
  # sends is byte-identical to what the tenant's gin-contrib/cors allows — else an
  # empty-body 403 before any handler runs. Pass the SAME template so the two can
  # never drift; the script substitutes its own {slug}.
  #   E2E_CP_ALLOW_EPHEMERAL_LOOPBACK=1 lets e2e_cp_require_staging_origin accept
  #     the loopback CP base URL (it already honors this flag; no script edit).
  #   E2E_REQUIRE_LIVE=1: a concierge/member that never reaches online is a HARD
  #     FAIL (exit 5), never a silent skip — the false-green guard a real gate needs
  #     (and it flips the script past its no-creds PR-mode self-check into the real
  #     run since MOLECULE_ADMIN_TOKEN is also supplied).
  MOLECULE_CP_URL="${CP_BASE_URL}" \
  MOLECULE_TENANT_URL="${CP_BASE_URL}" \
  MOLECULE_TENANT_ROUTE_DOMAIN="${ROUTE_DOMAIN}" \
  MOLECULE_TENANT_ORIGIN_TEMPLATE="${TENANT_PUBLIC_URL_TEMPLATE}" \
  MOLECULE_ADMIN_TOKEN="${CP_ADMIN_API_TOKEN}" \
  E2E_INFRA_BACKEND=local-docker \
  E2E_CP_ALLOW_EPHEMERAL_LOOPBACK=1 \
  E2E_REQUIRE_LIVE=1 \
  E2E_PROVISION_TIMEOUT_SECS="${E2E_PROVISION_TIMEOUT_SECS:-600}" \
  E2E_RUN_ID="${PR_NUMBER:-0}-${HEAD_SHA:-local0000}-cncrgmk" \
    bash "$HERE/test_staging_concierge_creates_workspace_e2e.sh"
}

# external_runtime — the external-mode workspace lifecycle journey (RFC #86 port).
# Registers external-runtime workspace rows and asserts all FOUR awaiting_agent
# transitions (create→awaiting_agent DB-verified, register→online, sweep→awaiting_agent,
# re-register→online) plus the BYO meta-runtime (kimi/kimi-cli) provision→online→A2A
# poll-queue arms. Workspaces are `external` rows (no container / no LLM / no compute),
# so — like concierge_user_tasks — this needs only the CP + workspace-server API and
# runs entirely inside the throwaway dind CP over lvh.me loopback with ZERO staging creds.
# Mirrors e2e-staging-saas.yml job `e2e-staging-external-runtime`
# (required context "E2E Staging External Runtime / E2E Staging External Runtime").
run_scenario_external_runtime() {
  echo "[proof][extra] external_runtime (external-mode workspace lifecycle: 4 awaiting_agent transitions + BYO kimi/kimi-cli poll-queue A2A) against the ephemeral CP..." >&2
  # Same topology-env shape as run_scenario_concierge_user_tasks: the CP is one
  # throwaway container; the tenant is reached via its base URL + Host/X-Molecule-Org-Slug
  # (not a real subdomain). MOLECULE_TENANT_ORIGIN_TEMPLATE == the CP's
  # LOCAL_TENANT_URL_TEMPLATE, so the Origin the harness presents is byte-identical to the
  # tenant's CORS_ORIGINS (else gin-contrib/cors returns an empty-body 403). Default
  # (all MOLECULE_TENANT_* unset) reproduces exact staging behaviour byte-for-byte.
  # E2E_REQUIRE_LIVE=1 arms the script's fail-closed contract (exit 5): a clean exit that
  # did NOT actually drive all four awaiting_agent transitions can never masquerade green.
  MOLECULE_CP_URL="${CP_BASE_URL}" \
  MOLECULE_TENANT_URL="${CP_BASE_URL}" \
  MOLECULE_TENANT_ROUTE_DOMAIN="${ROUTE_DOMAIN}" \
  MOLECULE_TENANT_ORIGIN_TEMPLATE="${TENANT_PUBLIC_URL_TEMPLATE}" \
  MOLECULE_ADMIN_TOKEN="${CP_ADMIN_API_TOKEN}" \
  E2E_INFRA_BACKEND=local-docker \
  E2E_CP_ALLOW_EPHEMERAL_LOOPBACK=1 \
  E2E_REQUIRE_LIVE=1 \
  E2E_PROVISION_TIMEOUT_SECS="${E2E_PROVISION_TIMEOUT_SECS:-300}" \
  E2E_RUN_ID="${PR_NUMBER:-0}-${HEAD_SHA:-local0000}-extrt" \
    bash "$HERE/test_staging_external_runtime.sh"
}

run_scenario_reconciler_heals_terminated() {
  echo "[proof][extra] reconciler heals a killed workspace instance (provision -> docker rm -f the container -> assert reconciler flips it off 'online') against the ephemeral CP..." >&2
  # Same ephemeral-CP slug-routing contract as run_scenario_concierge_user_tasks:
  # the CP is one throwaway container reached via its base URL + Host/
  # X-Molecule-Org-Slug (NOT a real subdomain), and MOLECULE_TENANT_ORIGIN_TEMPLATE
  # carries the tenant's ONE allowed CORS origin (the CP's LOCAL_TENANT_URL_TEMPLATE
  # substituted per-slug) so the threaded Origin can never drift from CORS_ORIGINS.
  #
  # Unlike the concierge lane, this journey provisions a REAL local-docker workspace
  # (it must reach status=online so we can kill its container), so it also mirrors
  # the happy-path's real-workspace knobs (E2E_RUNTIME / E2E_LLM_PATH=platform /
  # online timeout). E2E_PROVIDER is PINNED to molecules-server so the kill primitive
  # is `docker rm -f <container>` on the current DOCKER_HOST (the dind daemon the CP
  # provisioned on) — never the legacy AWS terminate path (no AWS creds in the gate).
  # PRIMARY assertion (workspace leaves 'online') is the gate; SECONDARY (existing-
  # volume reprovision) stays best-effort/soft-miss inside the script.
  MOLECULE_CP_URL="${CP_BASE_URL}" \
  MOLECULE_TENANT_URL="${CP_BASE_URL}" \
  MOLECULE_TENANT_ROUTE_DOMAIN="${ROUTE_DOMAIN}" \
  MOLECULE_TENANT_ORIGIN_TEMPLATE="${TENANT_PUBLIC_URL_TEMPLATE}" \
  MOLECULE_ADMIN_TOKEN="${CP_ADMIN_API_TOKEN}" \
  E2E_INFRA_BACKEND=local-docker \
  E2E_CP_ALLOW_EPHEMERAL_LOOPBACK=1 \
  E2E_PROVIDER=molecules-server \
  E2E_RUNTIME="${RUNTIME}" \
  E2E_LLM_PATH=platform \
  E2E_PROVISION_TIMEOUT_SECS="${E2E_PROVISION_TIMEOUT_SECS:-300}" \
  E2E_WORKSPACE_ONLINE_TIMEOUT_SECS="${E2E_WORKSPACE_ONLINE_TIMEOUT_SECS:-900}" \
  E2E_RECONCILE_OFFLINE_TIMEOUT_SECS="${E2E_RECONCILE_OFFLINE_TIMEOUT_SECS:-180}" \
  E2E_REPROVISION_TIMEOUT_SECS="${E2E_REPROVISION_TIMEOUT_SECS:-600}" \
  E2E_RUN_ID="${PR_NUMBER:-0}-${HEAD_SHA:-local0000}-rec" \
    bash "$HERE/test_reconciler_heals_terminated_instance.sh"
}

# peer_visibility — the LITERAL mcp_molecule_list_peers PLATFORM contract (POST
# /workspaces/:id/mcp, JSON-RPC tools/call name=list_peers, through the real
# WorkspaceAuth + MCPRateLimiter chain; asserts the sibling peer set is present,
# non-empty, and NOT a native-sessions fallback — the byte-for-byte call
# mcp_molecule_list_peers makes from a canvas agent).
#
# TWO provision modes, selected by pv_provision_mode() (the real-runtime-matrix
# opt-in flag):
#
#   external (DEFAULT, byte-identical to today) — workspace rows are
#     `external:true` (awaiting_agent, no runtime container / LLM boot), so the
#     scenario exercises the platform auth + peer-set contract WITHOUT runtime
#     images or backed platform models. The same secret-free slice the docker-host
#     local leg (test_peer_visibility_mcp_local.sh) proves.
#
#   managed (E2E_EPHEMERAL_REAL_RUNTIME_MATRIX=1) — the FULL staging behaviour:
#     one real workspace per runtime in matrix_runtimes(), each booted to
#     status=online, then the literal list_peers call. This is the multi-runtime
#     real-boot coverage the staging peer-visibility job carries and the reason
#     the matrix flag exists. It works on the ephemeral CP because seed_workspace_image
#     pre-seeds every matrix runtime's template image and E2E_MODEL_SLUG pins the
#     platform model to `minimax/MiniMax-M2.7` — the ONE vendor the ephemeral CP is
#     keyed for (MINIMAX_API_KEY) and a registered platform model for all three
#     runtimes — OVERRIDING the staging default (pv_platform_model_for_runtime →
#     anthropic/moonshot), which the ephemeral CP's LLM proxy is NOT keyed to serve.
#
# UNLIKE concierge, NO peer-visibility curl sends an Origin header (the MCP call +
# provisioning are server-to-server, not a browser CORS request), so this scenario
# needs NO MOLECULE_TENANT_ORIGIN_TEMPLATE — only the Host + X-Molecule-Org-Slug
# slug routing (+ X-Molecule-Org-Id). Mirrors e2e-staging-saas.yml's peer-visibility
# job.
run_scenario_peer_visibility() {
  local mode; mode="$(pv_provision_mode)"
  if [ "$mode" = "managed" ]; then
    local rts; rts="$(matrix_runtimes)"
    echo "[proof][extra] peer_visibility (literal MCP list_peers, REAL managed-boot matrix=[${rts}], model=${E2E_MODEL_SLUG:-minimax/MiniMax-M2.7}) against the ephemeral CP..." >&2
    # Managed real-boot: E2E_MODEL_SLUG pins the model to the ephemeral CP's one
    # keyed vendor (overriding pv_platform_model_for_runtime's staging
    # anthropic/moonshot defaults). Timeout is the REAL cold-boot budget — three
    # runtime containers booting to online, not an awaiting_agent row flip.
    MOLECULE_CP_URL="${CP_BASE_URL}" \
    MOLECULE_TENANT_URL="${CP_BASE_URL}" \
    MOLECULE_TENANT_ROUTE_DOMAIN="${ROUTE_DOMAIN}" \
    MOLECULE_ADMIN_TOKEN="${CP_ADMIN_API_TOKEN}" \
    E2E_INFRA_BACKEND=local-docker \
    E2E_CP_ALLOW_EPHEMERAL_LOOPBACK=1 \
    PV_PROVISION_MODE=managed \
    PV_RUNTIMES="$rts" \
    E2E_MODEL_SLUG="${E2E_MODEL_SLUG:-minimax/MiniMax-M2.7}" \
    E2E_PROVISION_TIMEOUT_SECS="${E2E_PV_PROVISION_TIMEOUT_SECS:-900}" \
    E2E_RUN_ID="${PR_NUMBER:-0}-${HEAD_SHA:-local0000}-pv" \
      bash "$HERE/test_peer_visibility_mcp_staging.sh"
    return $?
  fi
  echo "[proof][extra] peer_visibility (literal MCP list_peers, external-mode) against the ephemeral CP..." >&2
  MOLECULE_CP_URL="${CP_BASE_URL}" \
  MOLECULE_TENANT_URL="${CP_BASE_URL}" \
  MOLECULE_TENANT_ROUTE_DOMAIN="${ROUTE_DOMAIN}" \
  MOLECULE_ADMIN_TOKEN="${CP_ADMIN_API_TOKEN}" \
  E2E_INFRA_BACKEND=local-docker \
  E2E_CP_ALLOW_EPHEMERAL_LOOPBACK=1 \
  PV_PROVISION_MODE=external \
  PV_RUNTIMES="${PV_RUNTIMES:-hermes openclaw claude-code}" \
  E2E_PROVISION_TIMEOUT_SECS="${E2E_PROVISION_TIMEOUT_SECS:-300}" \
  E2E_RUN_ID="${PR_NUMBER:-0}-${HEAD_SHA:-local0000}-pv" \
    bash "$HERE/test_peer_visibility_mcp_staging.sh"
}

# ── Go-harness staging journeys (RFC #86 — pull the go-staging-test lanes LEFT) ─
# The five scenarios above shell out to tests/e2e/test_*.sh. The next three shell
# out to the TOPOLOGY-AWARE Go staging-e2e harness (#4525:
# workspace-server/internal/staginge2e/tenant_topology.go) instead — the SAME
# `-tags staging_e2e` test binary the post-push `e2e-staging-saas.yml` jobs run,
# now aimed at THIS throwaway CP by threading the ephemeral topology through env:
#
#   CP_BASE_URL / CP_ADMIN_API_TOKEN      requireStagingEnv (the harness's admin surface)
#   STAGING_E2E=1                         arms the suite (else it t.Skips LOUD)
#   MOLECULE_TENANT_URL=$CP_BASE_URL      tenant traffic hits the CP base, not a real subdomain
#   MOLECULE_TENANT_ROUTE_DOMAIN=lvh.me   → Host=<slug>.lvh.me + X-Molecule-Org-Slug (CP wildcard proxy routing)
#   MOLECULE_TENANT_ORIGIN_TEMPLATE=…     the tenant's ONE allowed CORS origin (= the CP's
#                                         LOCAL_TENANT_URL_TEMPLATE); doTenantJSON sends an Origin,
#                                         so this must be byte-identical or gin-contrib/cors 403s
#   E2E_CP_ALLOW_EPHEMERAL_LOOPBACK=1     lets validateStagingCPBase accept the http loopback CP base
#
# All three create their OWN throwaway org (e2eSlug) with t.Cleanup admin-DELETE
# teardown, isolated from the happy-path org and each other — and the whole CP is
# a disposable dind that dies at gate end, so no run-id teardown net is needed
# here (unlike the post-push jobs). The Go toolchain is present on this runner
# (actions/setup-go, e2e-ephemeral-happy-path.yml). Run from workspace-server so
# the module + go.sum resolve.

# workspace_requests — TestWorkspaceCanRaiseTaskAndApprovalToUser (core#2606): a
# NORMAL workspace mints its own token and raises a task AND an approval into the
# org's unified pending inbox via the wsAuth POST /workspaces/:id/requests path.
# SECRET-FREE on the loopback CP: it lands request ROWS and polls the org pending
# view — it never waits for the workspace to boot online, so no runtime image or
# platform model is required. Mirrors e2e-staging-saas.yml job
# `E2E Staging SaaS / workspace-requests`.
run_scenario_workspace_requests() {
  echo "[proof][extra] workspace_requests (core#2606: a normal workspace raises task+approval to the user's unified inbox via wsAuth) against the ephemeral CP..." >&2
  # requireStagingEnv reads CP_BASE_URL + CP_ADMIN_API_TOKEN from the ENVIRONMENT,
  # so export the two standing shell vars into the subshell (they are already set
  # by boot_cp) — a `go test` exec only inherits EXPORTED vars. Do NOT re-assign
  # them in the env-prefix below: assigning a var AND expanding the same name in
  # one simple command is SC2097/SC2098 (the expansion would see the outer, not
  # the just-assigned, value). Exporting first, then expanding, is warning-clean.
  ( cd "$HERE/../../workspace-server" && \
    export CP_BASE_URL CP_ADMIN_API_TOKEN && \
    STAGING_E2E=1 \
    MOLECULE_TENANT_URL="${CP_BASE_URL}" \
    MOLECULE_TENANT_ROUTE_DOMAIN="${ROUTE_DOMAIN}" \
    MOLECULE_TENANT_ORIGIN_TEMPLATE="${TENANT_PUBLIC_URL_TEMPLATE}" \
    MOLECULE_ADMIN_TOKEN="${CP_ADMIN_API_TOKEN}" \
    E2E_INFRA_BACKEND=local-docker \
    E2E_CP_ALLOW_EPHEMERAL_LOOPBACK=1 \
      go test -tags staging_e2e ./internal/staginge2e/ -run TestWorkspaceCanRaiseTaskAndApprovalToUser -count=1 -v -timeout 30m )
}

# concierge_platform_agent — TestConciergePlatformAgent_Staging: platform-agent
# install + /org/identity, kind on the workspace API, discovery-peers admin-auth
# regression guard, BYOK billing-mode round-trip, and the concierge config-tab
# auth sweep. SECRET-FREE on the loopback CP: every assertion is DB/handler state
# — the test deliberately does NOT wait for the concierge or the ordinary
# workspace to boot online (concierge_platform_test.go), so no runtime image or
# platform model is needed. This is the DB-level HALF; the mgmt-MCP present+
# callable half (TestPlatformAgentMgmtMCP_Staging) requires a real concierge boot
# and belongs with the real-runtime matrix (E2E_EPHEMERAL_REAL_RUNTIME_MATRIX) —
# it is intentionally NOT wired here. Mirrors e2e-staging-saas.yml job
# `E2E Staging Concierge Platform Agent` (its first go-test step only).
run_scenario_concierge_platform_agent() {
  echo "[proof][extra] concierge_platform_agent (platform-agent install + identity, kind, discovery-peers authz, BYOK billing round-trip, config-tab auth — DB/handler state, no boot) against the ephemeral CP..." >&2
  # See run_scenario_workspace_requests for the export-then-expand rationale
  # (requireStagingEnv reads CP_BASE_URL + CP_ADMIN_API_TOKEN from the env; a bare
  # env-prefix reassignment of those names would trip SC2097/SC2098).
  ( cd "$HERE/../../workspace-server" && \
    export CP_BASE_URL CP_ADMIN_API_TOKEN && \
    STAGING_E2E=1 \
    MOLECULE_TENANT_URL="${CP_BASE_URL}" \
    MOLECULE_TENANT_ROUTE_DOMAIN="${ROUTE_DOMAIN}" \
    MOLECULE_TENANT_ORIGIN_TEMPLATE="${TENANT_PUBLIC_URL_TEMPLATE}" \
    MOLECULE_ADMIN_TOKEN="${CP_ADMIN_API_TOKEN}" \
    E2E_INFRA_BACKEND=local-docker \
    E2E_CP_ALLOW_EPHEMERAL_LOOPBACK=1 \
      go test -tags staging_e2e ./internal/staginge2e/ -run TestConciergePlatformAgent_Staging -count=1 -v -timeout 35m )
}

# plugin_install_lifecycle — TestPluginInstallLifecycle_Staging: registry is
# non-empty, install a registry plugin, ListInstalled reads it back (CP #3125
# EIC guard), and the agent stays online across the install-triggered restart
# (#159 mgmt-MCP self-heal). UNLIKE the two above, this REALLY BOOTS a workspace
# — Step 3 waits for online+routable, Step 4 waits for the post-install
# restart→online — so it needs one real runtime container + a keyed platform
# model. That is exactly the real-workspace path the happy-path (E2E_MODE=full)
# already stands up on this CP (seeded hermes template + the CP's MINIMAX_API_KEY
# platform proxy), so it runs here WITHOUT the multi-runtime matrix flag (it uses
# only the single default RUNTIME=hermes image, not matrix_runtimes()). Kept
# advisory: it is the heaviest of the three (a real cold boot + restart), so it
# soaks for determinism before it can gate. Mirrors e2e-staging-saas.yml job
# `E2E Staging Plugin Install Lifecycle`.
run_scenario_plugin_install_lifecycle() {
  echo "[proof][extra] plugin_install_lifecycle (registry non-empty -> install -> ListInstalled reads back -> agent stays online across the install restart; REAL workspace boot) against the ephemeral CP..." >&2
  # See run_scenario_workspace_requests for the export-then-expand rationale
  # (requireStagingEnv reads CP_BASE_URL + CP_ADMIN_API_TOKEN from the env; a bare
  # env-prefix reassignment of those names would trip SC2097/SC2098).
  ( cd "$HERE/../../workspace-server" && \
    export CP_BASE_URL CP_ADMIN_API_TOKEN && \
    STAGING_E2E=1 \
    MOLECULE_TENANT_URL="${CP_BASE_URL}" \
    MOLECULE_TENANT_ROUTE_DOMAIN="${ROUTE_DOMAIN}" \
    MOLECULE_TENANT_ORIGIN_TEMPLATE="${TENANT_PUBLIC_URL_TEMPLATE}" \
    MOLECULE_ADMIN_TOKEN="${CP_ADMIN_API_TOKEN}" \
    E2E_INFRA_BACKEND=local-docker \
    E2E_CP_ALLOW_EPHEMERAL_LOOPBACK=1 \
      go test -tags staging_e2e ./internal/staginge2e/ -run TestPluginInstallLifecycle_Staging -count=1 -v -timeout 55m )
}

# Dispatch one extra-scenario key to its runner. An UNKNOWN key is a hard error,
# never a silent skip: a typo in E2E_EPHEMERAL_EXTRA_SCENARIOS that quietly ran
# nothing would be the exact vacuous-green this gate exists to abolish.
# Exit 97 is the RESERVED unknown-key/misconfig sentinel. It must be a code the
# scenario runners themselves NEVER emit: the concierge scenario legitimately
# exits 2 (its cleanup_org EXIT trap / env guards), so keying misconfig on 2 would
# misclassify a genuine ran-and-failed scenario as a never-ran typo. 97 collides
# with nothing a scenario returns; a known scenario that somehow exits 97 is
# remapped below so it can never masquerade as a misconfig.
readonly EXTRA_MISCONFIG_RC=97
run_one_extra_scenario() {
  local rc
  case "$1" in
    concierge_user_tasks) run_scenario_concierge_user_tasks; rc=$? ;;
    concierge_creates_workspace) run_scenario_concierge_creates_workspace; rc=$? ;;
    external_runtime) run_scenario_external_runtime; rc=$? ;;
    reconciler_heals_terminated) run_scenario_reconciler_heals_terminated; rc=$? ;;
    peer_visibility) run_scenario_peer_visibility; rc=$? ;;
    workspace_requests) run_scenario_workspace_requests; rc=$? ;;
    concierge_platform_agent) run_scenario_concierge_platform_agent; rc=$? ;;
    plugin_install_lifecycle) run_scenario_plugin_install_lifecycle; rc=$? ;;
    *) echo "[proof][extra] ❌ unknown extra scenario '$1' — check E2E_EPHEMERAL_EXTRA_SCENARIOS" >&2; return "$EXTRA_MISCONFIG_RC" ;;
  esac
  # A known scenario must never masquerade as the misconfig sentinel.
  [ "$rc" -eq "$EXTRA_MISCONFIG_RC" ] && rc=1
  return "$rc"
}

# Set by run_extra_scenarios: 1 when the extra-scenario LIST itself is a misconfig
# (an unknown/typo'd key, or a non-empty value that names ZERO scenarios). A
# misconfig means the wrong thing — or nothing — ran, so it is NEVER
# advisory-suppressible: gate_extra_scenarios fails the gate on it regardless of
# E2E_EPHEMERAL_EXTRA_ADVISORY. Distinct from a scenario that genuinely RAN and
# failed (counted in run_extra_scenarios's return), which the advisory soak may
# still suppress.
EXTRA_MISCONFIG=0

# Set by run_extra_scenarios: the KEYS of the scenarios that RAN and FAILED, in
# order. The failed-COUNT (run_extra_scenarios's return) says HOW MANY reds there
# were; this says WHICH — which is what gate_extra_scenarios needs to apply the
# per-scenario gating policy (a graduated scenario gates even under the global
# advisory soak; see E2E_EPHEMERAL_EXTRA_GATING / gate_extra_scenarios).
EXTRA_FAILED_KEYS=()

# Run every scenario named in E2E_EPHEMERAL_EXTRA_SCENARIOS (comma/space list)
# against the standing CP. Returns the COUNT of scenarios that RAN and FAILED
# (0 = none) so the caller can gate or advise on it. Records the failed keys in
# EXTRA_FAILED_KEYS. Sets EXTRA_MISCONFIG=1 (out of band) for a never-ran
# misconfig. Never returns early — every listed scenario runs so one failure does
# not mask a second.
run_extra_scenarios() {
  local list="${E2E_EPHEMERAL_EXTRA_SCENARIOS:-}"
  [ -n "$list" ] || return 0
  EXTRA_MISCONFIG=0
  EXTRA_FAILED_KEYS=()
  # A non-empty value that tokenizes to NOTHING (e.g. "," or whitespace) passes the
  # -n guard above but yields zero loop iterations — which would silently return 0
  # (green, nothing ran). That is a misconfig, not "no extras": detect zero tokens
  # explicitly and fail (never-ran ≠ all-passed).
  local -a keys=(); local s
  for s in ${list//,/ }; do keys+=("$s"); done
  if [ "${#keys[@]}" -eq 0 ]; then
    echo "[proof][extra] ❌ E2E_EPHEMERAL_EXTRA_SCENARIOS is set ('${list}') but names ZERO scenarios (no tokens after splitting) — misconfig; refusing to pass vacuously." >&2
    EXTRA_MISCONFIG=1
    return 0
  fi
  local failed=0 rc
  for s in "${keys[@]}"; do
    run_one_extra_scenario "$s"; rc=$?
    case "$rc" in
      0) echo "[proof][extra] ✅ ${s} PASSED against the ephemeral CP." >&2 ;;
      "$EXTRA_MISCONFIG_RC") # UNKNOWN key (reserved sentinel) — a typo that ran
         # nothing. Flag it as a misconfig so it fails the gate UNCONDITIONALLY,
         # even under E2E_EPHEMERAL_EXTRA_ADVISORY=1: a never-ran scenario must
         # never read as an advisory-suppressible green. A scenario that RAN and
         # failed (any other non-zero, INCLUDING its own exit 2) falls through to
         # the failed-count below and stays advisory-suppressible.
         echo "[proof][extra] ❌ ${s} is an UNKNOWN scenario key (ran nothing) — misconfig; fails the gate even under E2E_EPHEMERAL_EXTRA_ADVISORY=1." >&2
         EXTRA_MISCONFIG=1 ;;
      *) failed=$((failed + 1))
         EXTRA_FAILED_KEYS+=("$s")
         echo "[proof][extra] ❌ ${s} FAILED against the ephemeral CP." >&2 ;;
    esac
  done
  return "$failed"
}

# A scenario key is GATING-OVERRIDE when it is named in E2E_EPHEMERAL_EXTRA_GATING
# (comma/space list): its failure fails the gate EVEN under the global advisory
# soak (E2E_EPHEMERAL_EXTRA_ADVISORY=1). This is how an individual scenario
# graduates from capture-first soak to merge-blocking without flipping the whole
# extra-set hard — a genuinely-still-flaky lane (external_runtime,
# reconciler_heals_terminated) can keep soaking in the SAME run while a proven one
# (peer_visibility, concierge_creates_workspace — the #4543 coverage-hole fix)
# gates. Returns 0 (is-gating) / 1 (not-listed).
_extra_scenario_is_gating_override() {
  local key="$1" g list="${E2E_EPHEMERAL_EXTRA_GATING:-}"
  for g in ${list//,/ }; do
    [ "$g" = "$key" ] && return 0
  done
  return 1
}

# FAIL-CLOSED GUARD (defense-in-depth with test_ephemeral_gate_is_enforced.py):
# every key in E2E_EPHEMERAL_EXTRA_GATING MUST also appear in
# E2E_EPHEMERAL_EXTRA_SCENARIOS. A gating key that is NOT in the scenario list
# never runs → never enters EXTRA_FAILED_KEYS → can NEVER fail the gate: the gate
# stays green and a regression to that journey merges silently (fail-open). That
# is the exact hole the gating list exists to close, so a mis-declared list must
# FAIL THE GATE — same class as the unknown-key/zero-token misconfig above, never
# advisory-suppressible. Returns 0 when the subset holds, 1 (with ::error::) when
# any gating key is missing from the scenario list.
_assert_gating_subset_of_scenarios() {
  local gate_list="${E2E_EPHEMERAL_EXTRA_GATING:-}" scen="${E2E_EPHEMERAL_EXTRA_SCENARIOS:-}"
  local g s found; local -a missing=()
  for g in ${gate_list//,/ }; do
    found=0
    for s in ${scen//,/ }; do [ "$s" = "$g" ] && { found=1; break; }; done
    [ "$found" -eq 0 ] && missing+=("$g")
  done
  if [ "${#missing[@]}" -ne 0 ]; then
    echo "::error::[proof][extra] E2E_EPHEMERAL_EXTRA_GATING names scenario(s) NOT in E2E_EPHEMERAL_EXTRA_SCENARIOS: ${missing[*]} — a gating key that never runs can never fail, so the gate would fail OPEN (a regression to that journey merges GREEN). Add the key to E2E_EPHEMERAL_EXTRA_SCENARIOS or remove it from E2E_EPHEMERAL_EXTRA_GATING. FAILING the gate (fail-closed misconfig)." >&2
    return 1
  fi
  return 0
}

# Run the extra scenarios against the standing CP and apply the gating policy.
# Shared by `all` and `scenario` so the advertised local loop exercises exactly
# what CI does. Returns 0 if the extras do NOT force a gate failure, 1 if they do:
#   * a gating list that is NOT a subset of the scenario list → gates ALWAYS
#     (fail-closed misconfig; a gating key that never runs can never fail);
#   * a scenario that RAN and FAILED → gates UNLESS the global advisory soak
#     (E2E_EPHEMERAL_EXTRA_ADVISORY=1) suppresses it — but a scenario named in
#     E2E_EPHEMERAL_EXTRA_GATING GATES REGARDLESS of the soak (it has graduated);
#   * a MISCONFIG (unknown key / zero-token list) → gates ALWAYS (never suppressible).
gate_extra_scenarios() {
  local extra_failed gate=0
  # Checked BEFORE the empty-scenario early return: a gating list set with an
  # empty scenario list is itself the fail-open misconfig, and must not slip
  # through by returning 0 early.
  _assert_gating_subset_of_scenarios || gate=1
  [ -n "${E2E_EPHEMERAL_EXTRA_SCENARIOS:-}" ] || return "$gate"
  run_extra_scenarios; extra_failed=$?
  if [ "${EXTRA_MISCONFIG:-0}" -ne 0 ]; then
    echo "[proof][extra] ❌ extra-scenario MISCONFIG (unknown key or zero-token list) — failing the gate; NOT advisory-suppressible." >&2
    gate=1
  fi
  if [ "$extra_failed" -ne 0 ]; then
    local soak="${E2E_EPHEMERAL_EXTRA_ADVISORY:-0}" s hard=0
    # Classify EACH ran-and-failed scenario. A scenario gates when the soak is
    # off OR when it is a gating-override (graduated) scenario; otherwise the
    # soak suppresses it. Iterate names (EXTRA_FAILED_KEYS), not just the count,
    # so a graduated red and a still-soaking red in the SAME run are told apart.
    for s in "${EXTRA_FAILED_KEYS[@]}"; do
      if [ "$soak" != "1" ] || _extra_scenario_is_gating_override "$s"; then
        hard=$((hard + 1))
        echo "[proof][extra] ❌ ${s} FAILED and is GATING (merge-blocking) — advisory soak does NOT suppress it." >&2
      else
        echo "[proof][extra] ⚠️ ${s} FAILED — ADVISORY soak (E2E_EPHEMERAL_EXTRA_ADVISORY=1, not in E2E_EPHEMERAL_EXTRA_GATING); NOT failing the gate. Read the [extra] output above and promote to gating (add it to E2E_EPHEMERAL_EXTRA_GATING) once green." >&2
      fi
    done
    [ "$hard" -ne 0 ] && gate=1
  fi
  return "$gate"
}

# ── idle-digest sub-step assertion (task #219) — RETIRED as the gating check:
# the CP re-provisions replacement containers under the SAME name, and the
# collector followers are name-keyed, so a replacement's log (where the fired
# line can land) is never captured (run 496595). The AUTHORITATIVE assert now
# lives in full-saas step 10c (live docker logs + durable goal.yaml
# last_included_at, while the org is still alive). Kept for ad-hoc use. ─────
# full-saas step 10c soaked the fleet idle with a shrunken
# MOLECULE_IDLE_FIRE_SECONDS; the runtime must have ARMED the contract-driven
# digest loop and FIRED a digest. Asserted from the collector's streamed log
# FILES — the workspace containers are already gone when we regain control.
# This is a SUB-STEP gate: it reds the digest wiring specifically, on a
# scenario that has otherwise already passed, with its own reachable fail arms
# (no arm line = boot wiring broken; armed-but-never-fired = fire path broken).
assert_idle_digest() {
  [ "${E2E_IDLE_DIGEST_CHECK:-on}" = "on" ] || { echo "[proof] idle-digest check OFF (E2E_IDLE_DIGEST_CHECK=${E2E_IDLE_DIGEST_CHECK})." >&2; return 0; }
  if [ -z "$TENANT_LOG_DIR" ] || [ ! -d "$TENANT_LOG_DIR" ]; then
    echo "[proof] ❌ idle-digest: no tenant log capture dir — the collector never ran, cannot prove the digest fired." >&2
    return 1
  fi
  local armed fired
  armed=$(grep -l "Idle digest: contract-driven digest loop armed" "$TENANT_LOG_DIR"/*.log 2>/dev/null | head -1)
  fired=$(grep -l "Idle digest: fired" "$TENANT_LOG_DIR"/*.log 2>/dev/null | head -1)
  if [ -z "$armed" ]; then
    echo "[proof] ❌ idle-digest: NO workspace log contains the digest-loop ARM line — the mailbox-kernel idle digest did not come up on boot (task #219)." >&2
    return 1
  fi
  if [ -z "$fired" ]; then
    echo "[proof] ❌ idle-digest: loop ARMED ($(basename "$armed")) but NEVER FIRED within the soak window — fire path broken (delta gate / skip check / poster)." >&2
    grep -h "Idle digest:" "$TENANT_LOG_DIR"/*.log 2>/dev/null | tail -10 | sed 's/^/[proof]    /' >&2
    return 1
  fi
  echo "[proof] ✅ idle-digest: contract-driven digest ARMED + FIRED ($(basename "$fired"))" >&2
  grep -h "Idle digest:" "$TENANT_LOG_DIR"/*.log 2>/dev/null | tail -5 | sed 's/^/[proof]    /' >&2
  return 0
}

# ── dump_diagnostics: on a scenario failure, surface the CP + tenant container
# logs so the failing step is diagnosable WITHOUT a local repro. The runner
# otherwise tears the CP down (all mode) and leaves nothing to inspect. The CP
# is molecule-cp-${NS}; the local-docker provisioner launches tenants onto
# mol-net-${NS}, so a network filter catches them (or reveals none were launched).

# ── tenant log collector ───────────────────────────────────────────────────
# WHY THIS EXISTS, and why dump_diagnostics alone was not enough.
#
# dump_diagnostics used to read tenant logs with `docker logs` at failure time.
# It captured ZERO tenant log lines on precisely the failures that needed them,
# because the tenant container was already GONE by then: test_staging_full_saas.sh
# tears the ORG down as soon as a step fails ("Tearing down org …" → CP
# `admin delete-tenant` → the tenant container is removed) and only THEN returns
# non-zero to us. The CP log tail in the dump literally showed `admin
# delete-tenant … completed` ABOVE the (empty) tenant section.
#
# So the gate destroyed its own evidence. A tenant that provisions but never
# becomes ready ("local-docker concierge: tenant … not ready", org stuck at
# status=provisioning to the timeout) reported THAT it failed while making it
# structurally impossible to see WHY — it forced a CP regression to be reverted
# on timeline evidence alone (2026-07-13) rather than read.
#
# Reordering the teardown would be fragile: the teardown lives in the INNER
# script, and any future caller that tears down on its own reintroduces the bug.
# Instead, FOLLOW each tenant's logs into a file from the moment the container
# appears. `docker logs -f` keeps streaming until the container dies, and the
# file outlives the container — so the log survives no matter who tears down
# first, or in what order.
TENANT_LOG_DIR=""
COLLECTOR_PID=""
COLLECTOR_RAN=""   # set once a collector is actually launched — see dump_diagnostics

start_tenant_log_collector() {
  TENANT_LOG_DIR="$(mktemp -d 2>/dev/null)" || {
    # Do NOT fail the run for this — but say so loudly, because a silent failure
    # here is indistinguishable, in the dump, from "no tenant was ever created",
    # and that ambiguity is the exact thing this collector exists to remove.
    TENANT_LOG_DIR=""
    echo "[proof] ⚠ mktemp -d failed — tenant logs will NOT be captured this run." >&2
    return 0
  }
  (
    # Poll for tenant/workspace containers belonging to THIS run and attach a
    # follower to each exactly once. Scoping is identical to the dump below:
    # `since=` our own CP (so leaked containers from other runs on this shared
    # runner can't pollute us) intersected with the provisioner name shapes,
    # plus anything on our leg network.
    while :; do
      for c in $( { docker ps -a --filter "since=molecule-cp-${NS}" --format '{{.Names}}' 2>/dev/null | grep -E '^mol-tenant-|^ws-|^mol-ws-' ;
                    docker ps -a --filter "network=mol-net-${NS}" --format '{{.Names}}' 2>/dev/null ; } | sort -u ); do
        case "$c" in "molecule-cp-${NS}"|"pg-${NS}"|"") continue ;; esac
        [ -e "${TENANT_LOG_DIR}/${c}.following" ] && continue
        # `docker ps -a` also lists containers in CREATED state — and `docker
        # logs -f` on one of those returns IMMEDIATELY with zero bytes rather
        # than waiting for it to start. Marking such a container as followed
        # would pin an empty file for the rest of the run and reproduce exactly
        # the bug this collector fixes: a confident "FULL log" header over
        # nothing. Wait for it to leave `created`; we re-poll every second.
        case "$(docker inspect -f '{{.State.Status}}' "$c" 2>/dev/null)" in
          ""|created) continue ;;
        esac
        : > "${TENANT_LOG_DIR}/${c}.following"
        # -f streams until the container dies; the file keeps the output after
        # the container is removed. This is the whole point.
        docker logs -f --timestamps "$c" >"${TENANT_LOG_DIR}/${c}.log" 2>&1 &
        # Record the follower's PID so stop_tenant_log_collector can reap it.
        # Job control is off in a non-interactive shell, so the followers are
        # NOT in their own process group — `kill -- -PID` would not reach them.
        echo "$!" >>"${TENANT_LOG_DIR}/followers.pids"
      done
      sleep 1
    done
    # stderr → a file, not /dev/null: if docker itself is unreachable from the
    # collector, that must show up in the dump instead of masquerading as "no
    # tenant was ever created".
  ) >/dev/null 2>"${TENANT_LOG_DIR}/collector.err" &
  COLLECTOR_PID=$!
  COLLECTOR_RAN=1
  # Record the dir in the reattach state so a LATER `$0 down` — a different
  # shell, which never saw this variable — can still delete it. Without this, a
  # `scenario` run that dies before its trap orphans a /tmp dir of tenant logs.
  # Rewrite rather than append: repeated `scenario` runs share one state file.
  if [ -f "$STATE_FILE" ]; then
    grep -v '^TENANT_LOG_DIR=' "$STATE_FILE" >"${STATE_FILE}.tmp" 2>/dev/null || true
    echo "TENANT_LOG_DIR=${TENANT_LOG_DIR}" >>"${STATE_FILE}.tmp"
    mv "${STATE_FILE}.tmp" "$STATE_FILE" 2>/dev/null || rm -f "${STATE_FILE}.tmp"
    chmod 600 "$STATE_FILE" 2>/dev/null || true
  fi
  return 0
}

# A follower is `docker logs -f`. PIDs in followers.pids are append-only and a
# dead follower's PID is recyclable, so NEVER kill one blind — on a long run in
# a small PID namespace that lands on an unrelated process (plausibly the very
# `pr-ephemeral-cp.sh down` teardown is about to run).
_is_live_follower() {
  local p="$1"
  [ -n "$p" ] || return 1
  kill -0 "$p" 2>/dev/null || return 1
  case "$(ps -o comm= -p "$p" 2>/dev/null)" in
    *docker*) return 0 ;;
    *) return 1 ;;
  esac
}

stop_tenant_log_collector() {
  [ -n "$COLLECTOR_PID" ] && { kill "$COLLECTOR_PID" 2>/dev/null || true; wait "$COLLECTOR_PID" 2>/dev/null || true; }
  COLLECTOR_PID=""
  # Reap the `docker logs -f` followers the collector spawned. They exit on
  # their own once their container dies, but a still-running tenant would leave
  # one attached — kill them so the files are complete and nothing lingers.
  if [ -n "$TENANT_LOG_DIR" ] && [ -f "${TENANT_LOG_DIR}/followers.pids" ]; then
    local p killed=""
    while read -r p; do
      _is_live_follower "$p" || continue
      kill "$p" 2>/dev/null || true
      killed="${killed} ${p}"
    done <"${TENANT_LOG_DIR}/followers.pids"
    # They are children of the (now dead) subshell, not of us, so `wait` can't
    # see them. Poll until they're gone — otherwise dump_diagnostics reads the
    # files microseconds after SIGTERM and can lose the last partial line.
    local spins=0 alive=1
    while [ -n "$killed" ] && [ "$alive" -eq 1 ] && [ "$spins" -lt 30 ]; do
      alive=0
      for p in $killed; do kill -0 "$p" 2>/dev/null && alive=1; done
      [ "$alive" -eq 1 ] && sleep 0.1
      spins=$((spins + 1))
    done
    rm -f "${TENANT_LOG_DIR}/followers.pids"
  fi
}

# Remove the captured logs. KEEP_UP means "I want to poke at this run" — so keep
# them, and say where they are, rather than deleting the evidence under the user.
cleanup_tenant_log_dir() {
  [ -n "$TENANT_LOG_DIR" ] || return 0
  if [ -n "${KEEP_UP:-}" ]; then
    echo "[proof]    captured tenant logs: ${TENANT_LOG_DIR}  (removed by '$0 down')" >&2
    return 0
  fi
  rm -rf "$TENANT_LOG_DIR" 2>/dev/null || true
  TENANT_LOG_DIR=""
}

dump_diagnostics() {
  # Stop following first, so every follower has flushed before we read.
  stop_tenant_log_collector
  echo "── DIAGNOSTIC BURST (ephemeral CP ${NS}) ─────────────────────────────" >&2
  echo "[diag] HOST-WIDE container listing (context only — may include LEAKED" >&2
  echo "[diag] containers from other runs on this shared runner; logs below are" >&2
  echo "[diag] scoped to THIS run):" >&2
  docker ps -a --format '  {{.Names}}  {{.Image}}  {{.Status}}  nets={{.Networks}}' >&2 2>/dev/null || true
  echo "[diag] CP logs (molecule-cp-${NS}, tail 200):" >&2
  docker logs --tail 200 "molecule-cp-${NS}" 2>&1 | sed 's/^/  cp| /' >&2 \
    || echo "  (no CP container molecule-cp-${NS})" >&2

  # Tenant/workspace logs — read from the COLLECTOR'S FILES, not from `docker
  # logs`. The containers are typically already deleted by the inner script's
  # org teardown; the files are what survive.
  local n=0 f c lines
  if [ -n "$TENANT_LOG_DIR" ] && [ -d "$TENANT_LOG_DIR" ]; then
    for f in "$TENANT_LOG_DIR"/*.log; do
      [ -e "$f" ] || continue
      c="$(basename "$f" .log)"
      n=$((n + 1))
      lines="$(wc -l <"$f" 2>/dev/null | tr -d ' ')"; lines="${lines:-0}"
      # Cap the emission. A tenant crash-looping for the full provision timeout
      # can run to tens of thousands of lines, and burying the CI log is its own
      # kind of losing the evidence. Head AND tail: boot failures show up at the
      # top, give-up failures at the bottom.
      if [ "$lines" -le 800 ]; then
        echo "[diag] tenant/workspace container ${c} — FULL log (${lines} lines), streamed from creation" >&2
        echo "[diag]   (captured live; the container itself is usually already torn down)" >&2
        sed "s/^/  ${c}| /" "$f" >&2 || true
      else
        echo "[diag] tenant/workspace container ${c} — ${lines} lines, streamed from creation" >&2
        echo "[diag]   (captured live; showing the first 200 and last 600 lines)" >&2
        head -n 200 "$f" | sed "s/^/  ${c}| /" >&2 || true
        echo "  ${c}| ……… $((lines - 800)) lines elided ………" >&2
        tail -n 600 "$f" | sed "s/^/  ${c}| /" >&2 || true
      fi
    done
    if [ -s "${TENANT_LOG_DIR}/collector.err" ]; then
      echo "[diag] the log collector itself reported errors:" >&2
      sed 's/^/  collector| /' "${TENANT_LOG_DIR}/collector.err" >&2 || true
    fi
  fi
  if [ "$n" -eq 0 ]; then
    if [ -z "$COLLECTOR_RAN" ]; then
      # Do NOT claim an RCA we cannot support. No collector ran, so we have no
      # evidence either way — saying "provisioning failed before any tenant" here
      # would be a fabricated finding, which is worse than an honest gap.
      echo "[diag] NO tenant logs were captured — THE COLLECTOR NEVER RAN (see the ⚠ above)." >&2
      echo "[diag] This says NOTHING about whether a tenant was created. Do not read it as a cause." >&2
    else
      echo "[diag] NO tenant/workspace container was ever observed for this run." >&2
      echo "[diag] The collector polled for the whole run and saw none, so this is most" >&2
      echo "[diag] likely the finding itself: provisioning failed BEFORE any tenant" >&2
      echo "[diag] container was created — look at the CP logs above, not at a tenant." >&2
      echo "[diag] (Caveat: a container created AND removed inside a single 1s poll gap" >&2
      echo "[diag]  would also be missed. Check the CP log for a create it then rolled back.)" >&2
    fi
  fi
  echo "── END DIAGNOSTIC BURST ──────────────────────────────────────────────" >&2
}

teardown() {
  echo "[proof] tearing down ephemeral CP namespace ${NS}..." >&2
  # Idempotent: dump_diagnostics already stopped it on the failure path.
  stop_tenant_log_collector
  if [ -n "${CP_EPHEMERAL_SCRIPT:-}" ] && [ -x "${CP_EPHEMERAL_SCRIPT:-}" ]; then
    "$CP_EPHEMERAL_SCRIPT" down --ns "$NS" >/dev/null 2>&1 ||
      echo "[proof] (down non-zero — cleanup may be incomplete; CI's outer dind teardown" \
        "still removes its disposable daemon, but a direct local run must inspect namespace ${NS})" >&2
  fi
  if [ -n "${PG_CTR:-}" ]; then docker rm -f "$PG_CTR" >/dev/null 2>&1 || true; fi
  # KEEP_UP is never set on the teardown path (`all` skips teardown entirely
  # under it), and `down` is the explicit "I'm finished" verb — so drop the
  # logs unconditionally here, including a dir a previous `scenario` recorded
  # in the state file and could not clean up from its own shell.
  [ -n "$TENANT_LOG_DIR" ] && rm -rf "$TENANT_LOG_DIR" 2>/dev/null || true
  TENANT_LOG_DIR=""
  rm -f "$STATE_FILE" 2>/dev/null || true
}

print_reattach() {
  cat >&2 <<EOF
[proof] ✅ ephemeral CP is UP and left running (namespace ${NS}).
[proof]    CP_BASE_URL = ${CP_BASE_URL}
[proof]    reattach state: ${STATE_FILE}
[proof] Run the scenario against it (repeatable, ~2 min):
[proof]    PR_NUMBER=${PR_NUMBER:-0} HEAD_SHA=${HEAD_SHA:-local0000} $0 scenario
[proof] Tear it down when done:
[proof]    PR_NUMBER=${PR_NUMBER:-0} HEAD_SHA=${HEAD_SHA:-local0000} $0 down
EOF
}

# Allow this file to be SOURCED by unit tests (tests/e2e/test_extra_scenarios_gating_unit.sh
# exercises run_extra_scenarios / gate_extra_scenarios with a stubbed runner) WITHOUT
# booting a CP: when sourced, BASH_SOURCE[0] != $0, so stop before dispatch. Executed
# directly, the two are equal and dispatch proceeds unchanged.
if [ "${BASH_SOURCE[0]}" != "${0}" ]; then
  return 0 2>/dev/null || true
fi

case "$CMD" in
  all)
    require_boot_env
    seed_workspace_image
    trap 'rc=$?; stop_tenant_log_collector; if [ -n "${KEEP_UP:-}" ]; then print_reattach; cleanup_tenant_log_dir; else teardown; fi; exit "$rc"' EXIT INT TERM
    start_pg
    boot_cp
    write_state    # so `KEEP_UP=1 … all` (or a mid-run peek) can attach a scenario
    # Follow tenant logs from BEFORE the first tenant exists. run_scenario may
    # delete the tenant within seconds of a failure, so there is no later point
    # at which we could still read it. NOT started in `boot` mode: that leaves
    # the CP up and exits, which would orphan the collector.
    start_tenant_log_collector
    run_scenario; rc=$?
    if [ "$rc" -eq 0 ]; then
      echo "[proof] ✅ core happy-path PASSED against an ephemeral CP — the SDK-owned-gate model holds with zero shared staging." >&2
      # RFC #86: run any extra post-push lanes being pulled left onto this gate,
      # against the SAME standing CP. Only after the happy-path passed (a broken
      # CP must red as the happy-path, not as an unrelated extra scenario).
      gate_extra_scenarios || rc=1
    fi
    if [ "$rc" -ne 0 ]; then
      echo "[proof] ❌ core happy-path FAILED (rc=$rc) against the ephemeral CP — read the full-saas output above for the failing step." >&2
      # CP logs live; tenant logs come from the collector's FILES — run_scenario
      # has already deleted the tenant containers by the time we get here.
      dump_diagnostics
    fi
    exit "$rc"   # trap tears down (or, with KEEP_UP=1, leaves it up + prints reattach)
    ;;
  boot)
    require_boot_env
    seed_workspace_image
    start_pg
    boot_cp
    write_state
    print_reattach
    ;;
  scenario)
    load_state
    # This trap is NOT optional. Bash makes an async command ignore SIGINT when
    # job control is off, so without it a Ctrl-C on the (advertised, run-it-many-
    # times) scenario loop kills this shell and leaves the collector polling
    # `docker ps` once a second FOREVER, plus its followers. `down` cannot reap
    # them — it's a different shell. The trap kills by PID, which does reach them.
    trap 'rc=$?; stop_tenant_log_collector; cleanup_tenant_log_dir; exit "$rc"' EXIT INT TERM
    # Same reason as in `all`: the tenant is deleted on failure before we regain
    # control, so following must start BEFORE run_scenario, not after it fails.
    start_tenant_log_collector
    run_scenario; rc=$?
    if [ "$rc" -eq 0 ]; then
      stop_tenant_log_collector
      echo "[proof] ✅ scenario PASSED against standing CP ${CP_BASE_URL}." >&2
      # The advertised fast local loop is boot→scenario; if it never ran the ported
      # extra scenarios, a dev would get a false "passed locally" that CI (which
      # runs them via `all`) then reds. Run them here too, against the SAME standing
      # CP, with identical advisory/misconfig gating.
      gate_extra_scenarios || rc=1
    fi
    if [ "$rc" -ne 0 ]; then
      echo "[proof] ❌ scenario FAILED (rc=$rc) — the CP is still UP (${CP_BASE_URL}); fix and re-run '$0 scenario'." >&2
      dump_diagnostics   # stops the collector and prints the captured tenant logs
    fi
    exit "$rc"
    ;;
  down)
    [ -f "$STATE_FILE" ] && load_state || true
    teardown
    ;;
  *)
    echo "usage: $0 [all|boot|scenario|down]" >&2
    exit 2
    ;;
esac
