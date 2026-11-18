#!/usr/bin/env bash
# require-deploy-toolchain.sh — resolve the tools a deploy job needs into
# ABSOLUTE, PROVEN-RUNNABLE paths, or fail with a diagnostic that NAMES the
# missing tool.
#
# ── WHY THIS EXISTS ─────────────────────────────────────────────────────────
#
# `redeploy-tenants-on-main.yml`'s k8s arm shipped calling `python3` directly.
# The `local-deploy` runner does not have python3, so its first self-triggered
# run (644986, main@fcfb9842f) died with:
#
#     /data/workdir/.../act/workflow/3.sh: line 30: python3: command not found
#     exit status 127
#
# Measured on that runner (pod ci/deploy-runner-*, image
# registry.moleculesai.app/molecule-ai/deploy-runner:mtls, Alpine 3.23,
# act_runner HOST executor) on 2026-08-11 — in the RUNNER's own environment,
# not an interactive shell on the node:
#
#     PATH (pid 1 == the runner process, and the steps inherit it)
#                     = /usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
#     python3|python|python3.1x|pypy3 -> ABSENT (and no python binary
#                     anywhere under /usr /bin /sbin /opt)
#     kubectl         -> ABSENT
#     jq              -> ABSENT
#     curl, wget, openssl, docker, git, bash, apk -> present
#
# So it is a genuinely missing interpreter, NOT a service-vs-login PATH split.
#
# ── WHY A SHARED SCRIPT AND NOT A `run:` BLOCK ──────────────────────────────
#
# Two reasons, both of which the platform has already paid for once:
#
#   1. `command not found` is the WORST possible diagnostic here, because it
#      names the SYMPTOM at a line number and says nothing about the runner,
#      the dependency, or the remedy. Worse, it lands MID-STEP: a bare
#      `V="$(python3 ...)"` under the runner's inherited `-e` dies ON THE
#      ASSIGNMENT, above whatever guard the author wrote for the empty case.
#      Resolving tools UP FRONT means the failure is a named preflight, not a
#      corpse halfway through a credential step.
#
#   2. Logic that lives in YAML cannot be unit-tested, so its fail arms rot
#      unexercised. `.gitea/scripts/lib/jq-install.sh` was extracted from an
#      inline block for exactly this reason (core#2460); this is that pattern
#      applied to the deploy lane. See tests/test-require-deploy-toolchain.sh —
#      it drives BOTH directions (tool present => resolves; tool absent and
#      unprovisionable => named ::error:: + non-zero).
#
# ── WHY PROVISIONING, AND WHY IT IS NOT "JUST INSTALL IT ON THE BOX" ────────
#
# The `local-deploy` runner is not a box anyone logs into. It is a single-pod
# Deployment (`ci/deploy-runner`, nodeSelector robot-1) built from
# operator-config `ops/runners/k8s/deploy-runner.Dockerfile`, imported straight
# into robot-1's containerd with `imagePullPolicy: Never`, and its workdir is an
# emptyDir. So:
#
#   * a package installed by hand into the running pod EVAPORATES on the next
#     restart, and nothing re-applies it — the "dependency nobody declared"
#     failure mode, reintroduced;
#   * the DURABLE home for the dependency is that Dockerfile, whose own header
#     already says "Host executor therefore needs the tooling IN this image".
#
# This script therefore does BOTH, in priority order:
#   * prefer whatever the image already ships (so the day the Dockerfile grows
#     python3 + kubectl, this becomes a pure no-op resolver and NOTHING here
#     has to change);
#   * otherwise provision from the runner's own pinned Alpine repositories, so
#     the lane is not dead while that image change waits on an operator.
#
# The dependency is DECLARED either way — in the workflow that calls this, by
# name, with a fail-closed arm. That is the property whose absence caused 644986.
#
# ── WHY NOT THE ALTERNATIVES ────────────────────────────────────────────────
#
#   actions/setup-python — its CPython builds are glibc/manylinux. This runner
#       is Alpine/musl (3.23.4, measured). They do not execute here.
#   a job `container:` — the runner is registered `local-deploy:host` (HOST
#       executor, no docker-mode label), so a job container is not selectable;
#       and its DOCKER_HOST points at the PRODUCTION docker daemon, so a job
#       container would run CI workload on a client-serving host.
#   the pod's in-cluster ServiceAccount instead of kubectl+kubeconfig — the SA
#       is `cp-deployer`, whose RoleBindings exist ONLY in controlplane-prod and
#       controlplane-staging and whose Role grants no `pods/exec` and no
#       cluster-wide deployment patch. It cannot census or roll. (Measured.)
#
# ── USAGE ───────────────────────────────────────────────────────────────────
#
#   require-deploy-toolchain.sh 'VAR:command:apk-package[:probe args]' ...
#
# Prints `VAR=/absolute/path` lines on stdout, one per spec, ONLY if EVERY spec
# resolved. A caller appends them to $GITHUB_ENV — after validating the capture,
# never by redirecting this script straight into it.
#
# Env knobs (tests + future images):
#   DEPLOY_TOOLCHAIN_APK       apk binary (default: apk); set to the empty
#                              string to refuse provisioning entirely.
#   DEPLOY_TOOLCHAIN_NO_INSTALL=1  resolve-only; never provision.

set -euo pipefail

toolchain_err() {
  echo "::error::deploy toolchain: $*" >&2
}

runner_id() {
  printf '%s' "${RUNNER_NAME:-<unknown runner>}"
}

# resolve_one <command> -> prints absolute path on stdout, or nothing + rc 1.
resolve_one() {
  local cmd="$1" path
  path="$(command -v -- "$cmd" 2>/dev/null || true)"
  [ -n "$path" ] || return 1
  case "$path" in
    /*) ;;
    # A shell builtin/function/alias is not a thing later steps can exec by
    # absolute path, and silently accepting one would put us right back on
    # ambient resolution.
    *) return 1 ;;
  esac
  [ -x "$path" ] || return 1
  printf '%s' "$path"
}

# apk_run <apk-bin> <args...> — bounded. A package manager that hangs on a
# stalled mirror would otherwise burn the whole 45-minute job budget and then
# report a timeout, which says nothing about what was actually wrong.
# DEPLOY_TOOLCHAIN_TIMEOUT seconds, only when a `timeout` binary exists (both
# coreutils and busybox provide one; do not make the bound a hard dependency).
apk_run() {
  local bin="$1"
  shift
  if command -v timeout >/dev/null 2>&1; then
    timeout "${DEPLOY_TOOLCHAIN_TIMEOUT:-180}" "$bin" "$@" >/dev/null 2>&1
  else
    "$bin" "$@" >/dev/null 2>&1
  fi
}

# probe_one <path> <probe args...> — the binary must RUN, not merely exist.
#
# `command -v kubectl` passing while the tool cannot actually do anything is a
# measured failure on this platform (both control planes ship the kubectl BINARY
# and no credentials, so every `command -v kubectl` preflight passed vacuously).
# Existence is one rung below capability; probe it.
probe_one() {
  local path="$1"
  shift
  "$path" "$@" >/dev/null 2>&1
}

main() {
  [ "$#" -gt 0 ] || {
    toolchain_err "no tool specs given; refusing to resolve an EMPTY toolchain and report success"
    return 2
  }

  local apk_bin="${DEPLOY_TOOLCHAIN_APK-apk}"
  # NOT `[ ... ] && apk_bin=""` — a false test makes that AND-list the failing
  # command, and under this script's own `set -e` the FALSE branch would abort
  # the run. The bug class this whole file exists to remove, one rung smaller.
  if [ "${DEPLOY_TOOLCHAIN_NO_INSTALL:-0}" = "1" ]; then
    apk_bin=""
  fi
  # An apk that is named but absent is not "provisioning available".
  if [ -n "$apk_bin" ] && ! command -v -- "$apk_bin" >/dev/null 2>&1; then
    apk_bin=""
  fi

  # Buffer every resolution and emit only once ALL of them succeeded. A partial
  # stdout would be a partial $GITHUB_ENV: some steps pinned, others silently
  # back on PATH lookup — a half-armed preflight that reads as armed.
  local out="" spec var cmd pkg probe path installed=0

  for spec in "$@"; do
    var="${spec%%:*}"
    spec="${spec#*:}"
    cmd="${spec%%:*}"
    spec="${spec#*:}"
    case "$spec" in
      *:*) pkg="${spec%%:*}"; probe="${spec#*:}" ;;
      *)   pkg="$spec";       probe="--version" ;;
    esac

    if [ -z "$var" ] || [ -z "$cmd" ]; then
      toolchain_err "malformed spec (need 'VAR:command:apk-package[:probe args]')"
      return 2
    fi

    if ! path="$(resolve_one "$cmd")"; then
      if [ -z "$apk_bin" ]; then
        toolchain_err "'$cmd' is NOT installed on runner '$(runner_id)' and provisioning is disabled (DEPLOY_TOOLCHAIN_NO_INSTALL/DEPLOY_TOOLCHAIN_APK). Add '$pkg' to the deploy-runner image (operator-config ops/runners/k8s/deploy-runner.Dockerfile) — that is the durable home for a host-executor dependency."
        return 1
      fi
      echo "::notice::deploy toolchain: '$cmd' absent on runner '$(runner_id)'; provisioning '$pkg' from the runner's pinned Alpine repositories." >&2
      if [ "$installed" -eq 0 ]; then
        if ! apk_run "$apk_bin" update; then
          toolchain_err "'$cmd' is missing and the package index could not be refreshed on runner '$(runner_id)' ($apk_bin update failed). This job needs '$cmd'; it will NOT continue without it."
          return 1
        fi
        installed=1
      fi
      if ! apk_run "$apk_bin" add --no-cache "$pkg"; then
        toolchain_err "'$cmd' is missing and installing package '$pkg' FAILED on runner '$(runner_id)'. Bake '$pkg' into the deploy-runner image (operator-config ops/runners/k8s/deploy-runner.Dockerfile) — a host-executor runner has no other durable place to carry it."
        return 1
      fi
      if ! path="$(resolve_one "$cmd")"; then
        toolchain_err "package '$pkg' installed on runner '$(runner_id)' but '$cmd' is still not an executable on PATH. The package does not provide the command this job calls."
        return 1
      fi
    fi

    # shellcheck disable=SC2086 # probe args are a deliberate word-split spec field
    if ! probe_one "$path" $probe; then
      toolchain_err "'$cmd' resolves to '$path' on runner '$(runner_id)' but '$path $probe' FAILED. A present-but-unrunnable tool is not a satisfied dependency, and treating it as one is how a preflight passes while the capability is absent."
      return 1
    fi

    echo "::notice::deploy toolchain: $var=$path" >&2
    out="${out}${var}=${path}"$'\n'
  done

  printf '%s' "$out"
}

main "$@"
