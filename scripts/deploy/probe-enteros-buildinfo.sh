#!/usr/bin/env bash
# probe-enteros-buildinfo.sh — ADVISORY Enter OS (enteros.ai) edge probe of
# tenant /buildinfo. Enter OS rebrand Phase 2, internal#1089.
#
# WHAT THIS IS (and is not)
# -------------------------
# The BLOCKING deploy gate is unchanged and lives in
# scripts/deploy/redeploy-staging-fleet.sh: it health-gates every rolled
# tenant's /buildinfo DAEMON-SIDE (a curl sidecar in the tenant's own netns),
# i.e. it proves the service behind <slug>.moleculesai.app. THIS script is the
# ADDITIVE second arm: it probes the SAME tenants through the NEW brand edge —
# https://<slug>.<enteros domain>/buildinfo — from the runner, through
# DNS + Cloudflare + tunnel ingress. It therefore measures enteros.ai ROUTING
# readiness, which the daemon-side gate cannot see.
#
# The CALLING WORKFLOW marks the step `continue-on-error: true`, so a red here
# never fails or rolls back a deploy. This script itself FAILS CLOSED (exit 1
# on any probe failure) so that, when ops has seeded the enteros.ai tenant
# routing and the arm has soaked green, flipping to blocking is ONLY deleting
# the `continue-on-error` line — the fail arm is already real and reachable.
#
# NEVER FABRICATE "NOTHING EXISTS": in discovery mode a FAILED `docker ps` is
# a probe error (exit 1), NOT an empty fleet. Only a SUCCESSFUL, genuinely
# empty enumeration is treated as "nothing to probe" (exit 0) — the same fleet
# the blocking gate just saw.
#
# Usage:
#   probe-enteros-buildinfo.sh --cp-env <env> --domain <apex>
#       Discover the running tenant platform containers on the current Docker
#       daemon (same label filter as the fleet swap script:
#       molecule.local-tenant=1 + molecule.cp-env=<env>), derive each slug from
#       the container name (<brand>-tenant-<slug>, both brand generations),
#       and probe https://<slug>.<apex>/buildinfo for each.
#   probe-enteros-buildinfo.sh --fqdn <host>
#       Probe exactly https://<host>/buildinfo (no Docker needed) — the
#       publish workflow's ops-seeded canary mode.
#
# Env overrides (no-hardcoding):
#   BRAND_PREFIXES      container-name brand prefixes, SAME list the fleet
#                       swap script uses (default "mol enteros"). Mirrors the
#                       SDK ResourcePrefix/LegacyResourcePrefixes shape.
#   EXPECTED_BUILD_SHA  optional expected /buildinfo git_sha (prefix match).
#                       Unset => any non-empty git_sha passes (routing check
#                       only); the served sha is always logged. Wire this from
#                       the image's OCI revision label when the arm flips to
#                       blocking, so edge identity is enforced too.
#   PROBE_ATTEMPTS / PROBE_SLEEP_SECS   retry tuning (default 3 / 5).
#   CURL_MAX_TIME       per-attempt curl --max-time (default 15).
set -euo pipefail

log() { printf '>> [enteros-probe] %s\n' "$*" >&2; }

# Same brand-prefix SSOT shape as redeploy-staging-fleet.sh: resource NAMES may
# carry EITHER brand generation's prefix; every name matcher derives from this
# ONE list so the SDK-side prefix flip cannot blind the probe to one generation.
BRAND_PREFIXES="${BRAND_PREFIXES:-mol enteros}"
# shellcheck disable=SC2086  # word-splitting of BRAND_PREFIXES is intended
_BRAND_ALT="$(printf '%s\n' $BRAND_PREFIXES | paste -sd'|' -)"
TENANT_NAME_RE="^(${_BRAND_ALT})-tenant-"

EXPECTED_BUILD_SHA="${EXPECTED_BUILD_SHA:-}"
PROBE_ATTEMPTS="${PROBE_ATTEMPTS:-3}"
PROBE_SLEEP_SECS="${PROBE_SLEEP_SECS:-5}"
CURL_MAX_TIME="${CURL_MAX_TIME:-15}"

CP_ENV=""
DOMAIN=""
FQDN=""

usage() { sed -n '2,50p' "$0" | sed 's/^# \{0,1\}//'; exit "${1:-0}"; }
while [ "$#" -gt 0 ]; do
  case "$1" in
    --cp-env) CP_ENV="$2"; shift 2;;
    --domain) DOMAIN="$2"; shift 2;;
    --fqdn)   FQDN="$2";   shift 2;;
    -h|--help) usage 0;;
    *) echo "unknown arg: $1" >&2; usage 2;;
  esac
done

if [ -n "$FQDN" ] && { [ -n "$CP_ENV" ] || [ -n "$DOMAIN" ]; }; then
  echo "::error::--fqdn is mutually exclusive with --cp-env/--domain" >&2
  exit 2
fi
if [ -z "$FQDN" ] && { [ -z "$CP_ENV" ] || [ -z "$DOMAIN" ]; }; then
  echo "::error::need either --fqdn <host>, or both --cp-env <env> and --domain <apex>" >&2
  exit 2
fi

# Mirrors the fleet swap script's json_git_sha (kept in lockstep — tiny by
# design so the two cannot drift apart meaningfully).
json_git_sha() {
  sed -n 's/.*"git_sha"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p'
}

# Mirrors the fleet swap script's build_sha_matches (prefix match, either way).
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

# warn_probe_failed <fqdn> <reason>: the LOUD advisory-arm warning. An advisory
# failure is still a real failure — this names exactly what to check.
warn_probe_failed() {
  local fqdn="$1" reason="$2"
  echo "::warning::[internal#1089 Phase 2 — ADVISORY enteros.ai arm] https://${fqdn}/buildinfo FAILED: ${reason}. The moleculesai.app deploy gate is separate and already passed daemon-side, so this red means the ENTEROS EDGE PATH did not serve the tenant. Check, in order: (1) DNS — does ${fqdn} resolve via a proxied record in the enteros.ai Cloudflare zone (ops-seeded)? (2) Ingress — does the CF tunnel/edge config route Host ${fqdn} to the SAME backend as the tenant's moleculesai.app fqdn? (3) TLS/proxy — cert + proxy status for *.${fqdn#*.}. (4) Body — an HTTP 200 without a git_sha means the host answered with the WRONG SERVICE (parked page / wrong ingress target). Until ops seeds enteros.ai tenant routing under internal#1089 Phase 2 this warning is EXPECTED and non-blocking; once seeded and soak-green, flip this arm to blocking (delete continue-on-error in the calling workflow, and wire EXPECTED_BUILD_SHA)."
}

# probe_fqdn <fqdn>: edge-probe https://<fqdn>/buildinfo with retries.
# Success requires curl-exit 0 AND a non-empty git_sha (a parked 200 page must
# never pass) AND, when EXPECTED_BUILD_SHA is set, a prefix match on it.
probe_fqdn() {
  local fqdn="$1" url body sha rc last="" attempt
  url="https://${fqdn}/buildinfo"
  for attempt in $(seq 1 "$PROBE_ATTEMPTS"); do
    [ "$attempt" -gt 1 ] && sleep "$PROBE_SLEEP_SECS"
    rc=0
    body="$(curl -fsS --connect-timeout 5 --max-time "$CURL_MAX_TIME" "$url" 2>/dev/null)" || rc=$?
    if [ "$rc" -ne 0 ]; then
      last="curl exit ${rc} (DNS/TLS/connect/HTTP error)"
      continue
    fi
    sha="$(printf '%s' "$body" | json_git_sha | head -1)"
    if [ -z "$sha" ]; then
      last="HTTP OK but no git_sha in body (wrong service behind the host?)"
      continue
    fi
    if [ -n "$EXPECTED_BUILD_SHA" ] && ! build_sha_matches "$sha" "$EXPECTED_BUILD_SHA"; then
      last="served git_sha=${sha} does not match expected ${EXPECTED_BUILD_SHA}"
      continue
    fi
    log "OK ${url} git_sha=${sha}${EXPECTED_BUILD_SHA:+ (matches expected)}"
    return 0
  done
  warn_probe_failed "$fqdn" "${last:-no attempt ran} after ${PROBE_ATTEMPTS} attempt(s)"
  return 1
}

# ── Single-fqdn (canary) mode — no Docker involved ───────────────────────────
if [ -n "$FQDN" ]; then
  log "canary mode: probing https://${FQDN}/buildinfo (attempts=${PROBE_ATTEMPTS})"
  if probe_fqdn "$FQDN"; then
    exit 0
  fi
  exit 1
fi

# ── Discovery mode: enumerate the SAME fleet the blocking gate just rolled ───
# Label-driven (prefix-agnostic), identical filters to the fleet swap script.
# A FAILED enumeration is a probe error, never an empty fleet — fail closed
# WITHOUT fabricating "nothing exists".
if ! names="$(docker ps \
      --filter 'label=molecule.local-tenant=1' \
      --filter "label=molecule.cp-env=${CP_ENV}" \
      --format '{{.Names}}' 2>/dev/null)"; then
  echo "::warning::[internal#1089 Phase 2 — ADVISORY enteros.ai arm] could not ENUMERATE the ${CP_ENV} tenant containers (docker ps failed). Refusing to conclude 'no tenants' from a failed probe — treating as a probe error. Check the runner's DOCKER_HOST / docker-socket-proxy reachability." >&2
  exit 1
fi
mapfile -t TENANTS < <(printf '%s\n' "$names" | sed '/^$/d' | sort)

if [ "${#TENANTS[@]}" -eq 0 ]; then
  log "no running ${CP_ENV} tenant platform containers found — nothing to probe (matches the blocking gate's view of the fleet)"
  exit 0
fi
log "probing ${#TENANTS[@]} tenant(s) via https://<slug>.${DOMAIN}/buildinfo"

FAILED=0
for name in "${TENANTS[@]}"; do
  if ! printf '%s' "$name" | grep -Eq "$TENANT_NAME_RE"; then
    # A labelled tenant container whose name matches NO brand generation: we
    # cannot derive its fqdn, so we cannot verify it. Fail closed (loudly)
    # instead of silently skipping a tenant the arm claims to cover.
    echo "::warning::[internal#1089 Phase 2 — ADVISORY enteros.ai arm] tenant container '${name}' matches no known brand prefix (${BRAND_PREFIXES}); cannot derive its <slug>.${DOMAIN} fqdn to probe. If a new brand prefix was introduced, add it to BRAND_PREFIXES here AND in redeploy-staging-fleet.sh." >&2
    FAILED=1
    continue
  fi
  slug="$(printf '%s' "$name" | sed -E "s/${TENANT_NAME_RE}//")"
  probe_fqdn "${slug}.${DOMAIN}" || FAILED=1
done

if [ "$FAILED" != 0 ]; then
  log "one or more enteros.ai edge probes failed (see ::warning:: lines above)"
  exit 1
fi
log "all ${#TENANTS[@]} tenant(s) served /buildinfo through the enteros.ai edge"
