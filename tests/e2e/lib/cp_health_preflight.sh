#!/usr/bin/env bash
# cp_health_preflight — poll a Control-Plane /health endpoint until it is
# STABLY ready, tolerating cold-start / mid-recreate transients.
#
# PURPOSE
# =======
# The completion-gated e2e lanes each ran a SINGLE-SHOT preflight:
#   code=$(curl -sS -o /dev/null -w "%{http_code}" --max-time 10 "$CP/health")
#   [ "$code" != "200" ] && exit 1
# Two brittleness bugs (mc#3244 acknowledged a pending durable retry fix):
#   1. Under Actions' default `bash -eo pipefail`, a transport timeout makes
#      the command substitution fail and `-e` aborts the step BEFORE the
#      status check even runs (no `|| echo 000`).
#   2. A CP that is mid-recreate / cold-starting (a TRANSIENT, not a bug)
#      returns 000/5xx/non-200 for a few seconds and the single shot
#      hard-fails the canary via `exit 1`.
# The sibling local-cp gate already tolerates 000/5xx and waits for a stable
# Nx200 streak — this makes every lane's preflight the same robust variant.
#
# CONTRACT
# ========
#   cp_health_preflight <base_url> [context_label]
#     <base_url>       e.g. "$MOLECULE_CP_URL" / "$CP_BASE_URL"; /health is
#                      appended. Falls back to MOLECULE_CP_URL / CP_BASE_URL.
#     [context_label]  used only in the failure message ("... not a <label>
#                      bug"); default "workspace".
#   Returns 0 as soon as it observes CP_HEALTH_PREFLIGHT_STREAK consecutive
#   200s (proving steady readiness, not a one-off blip). Returns 1 only after
#   CP_HEALTH_PREFLIGHT_DEADLINE seconds of persistent non-ready. A transient
#   (000 / 5xx / any non-200) is a RETRY, never a hard fail.
#
# Tunables (env, with defaults):
#   CP_HEALTH_PREFLIGHT_DEADLINE  total wall-clock seconds to wait (default 180)
#   CP_HEALTH_PREFLIGHT_POLL      seconds between polls (default 5)
#   CP_HEALTH_PREFLIGHT_STREAK    consecutive 200s required (default 3)
#   CP_HEALTH_PREFLIGHT_TIMEOUT   per-attempt curl --max-time seconds (default 10)
#   CP_HEALTH_PREFLIGHT_CURL      override curl binary (tests)
cp_health_preflight() {
  local base="${1:-${MOLECULE_CP_URL:-${CP_BASE_URL:-}}}"
  local label="${2:-workspace}"
  local deadline_s="${CP_HEALTH_PREFLIGHT_DEADLINE:-180}"
  local poll_s="${CP_HEALTH_PREFLIGHT_POLL:-5}"
  local need_streak="${CP_HEALTH_PREFLIGHT_STREAK:-3}"
  local per_timeout="${CP_HEALTH_PREFLIGHT_TIMEOUT:-10}"
  local curl_bin="${CP_HEALTH_PREFLIGHT_CURL:-curl}"

  if [ -z "$base" ]; then
    echo "::error::cp_health_preflight: no CP base URL (arg1 / MOLECULE_CP_URL / CP_BASE_URL all empty)"
    return 2
  fi

  local url="${base%/}/health"
  local start now elapsed code last="000" streak=0
  start=$(date +%s)

  while true; do
    # `|| code=000`: a transport timeout / connection refused must NOT abort
    # the step under `bash -e` — it is a retryable cold-start signal.
    code=$("$curl_bin" -sS -o /dev/null -w "%{http_code}" --max-time "$per_timeout" "$url" 2>/dev/null) || code="000"
    last="$code"

    if [ "$code" = "200" ]; then
      streak=$((streak + 1))
      if [ "$streak" -ge "$need_streak" ]; then
        echo "Staging CP healthy ✓ (stable ${need_streak}x200 at ${url})"
        return 0
      fi
    else
      # Cold-start / mid-recreate / transient 5xx / 000 → reset the streak and
      # keep polling. Only a persistent non-ready run past the deadline fails.
      if [ "$streak" -gt 0 ]; then
        echo "::notice::CP health streak reset (HTTP ${code} at ${url})"
      fi
      streak=0
    fi

    now=$(date +%s)
    elapsed=$((now - start))
    if [ "$elapsed" -ge "$deadline_s" ]; then
      echo "::error::Staging CP not healthy after ${deadline_s}s (last HTTP ${last} at ${url}) — infra / cold-start, not a ${label} bug."
      return 1
    fi
    sleep "$poll_s"
  done
}
