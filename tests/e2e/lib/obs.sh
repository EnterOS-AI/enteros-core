#!/usr/bin/env bash
# obs.sh — shared STRUCTURED OBSERVABILITY lib for E2E scripts.
#
# WHY THIS EXISTS (the gap it closes): until now a failed e2e was debuggable
# ONLY by digging through raw runner stdout or CP container logs — there was no
# place to see, per run, WHICH step failed, how long each step took, and the
# error/context, as a queryable timeline. Vector on the local obs stack ships
# only its own internal logs to Loki, so e2e runs left no trace in Grafana.
#
# This lib makes every e2e emit a structured event PER STEP to Loki (queryable
# in Grafana), tagged so one run renders as a filterable timeline:
#   - e2e_run_id (== E2E_RUN_ID / the e2e- slug — the same universal link the
#     leak reaper + run_footprint.sh use), env, git_sha, test name, host;
#   - per step: name, start/end epoch + DURATION_SECS, status
#     (running|pass|fail|timeout|skip|leak), and on failure the error + context
#     (HTTP code, body snippet, leaked resource, owner teardown op).
# The companion dashboard (operator-config/obs/grafana/dashboards/e2e-runs.json)
# turns these into a per-run step table + duration bars + failing-step error,
# so debugging a failed e2e = open the dashboard, not grep the runner log.
#
# DESIGN CONTRACT (load-bearing — keep it true):
#   * FAIL-SOFT. Obs is best-effort telemetry: a down/slow Loki must NEVER fail
#     or slow an e2e. Every function returns 0; the push has a short timeout and
#     swallows all errors (warns once to stderr).
#   * `set -euo pipefail` SAFE. Source-able with no side effects; every helper is
#     guarded so it can't trip the caller's `set -e`.
#   * NO HARD DEPENDENCIES beyond curl (already used by every e2e). JSON is built
#     in pure bash — no python3/jq needed (so it runs identically in CI Linux and
#     on a dev box that lacks python3).
#   * SECRET-SAFE. _obs_redact strips Bearer tokens / JWTs / long hex before any
#     value is shipped. NEVER pass a raw admin/tenant token as context; the
#     redactor is a backstop, not a license. org_instances.admin_token is never
#     read here.
#
# CONFIG (all optional; sensible defaults):
#   OBS_ENABLED        1 (default) | 0 to disable all emission.
#   OBS_LOKI_URL       Loki base URL. Default http://localhost:3102 (local obs
#                      stack; molecule-obs-loki). CI/staging: set to the obs Loki.
#   OBS_LOKI_TENANT    optional X-Scope-OrgID (multi-tenant Loki). Local Loki runs
#                      auth_enabled:false so this is unset locally.
#   OBS_LOKI_BEARER    optional Authorization bearer for an authed Loki. Fetch via
#                      Infisical in the caller; never hardcode.
#   OBS_PUSH_TIMEOUT   curl --max-time for the push (default 5s).
#   E2E_RUN_ID         the run id (also the slug seed). Used as e2e_run_id.
#   OBS_ENV            env label; auto-guessed from MOLECULE_CP_URL when unset.
#   OBS_GIT_SHA        git sha; auto from GITHUB_SHA or `git rev-parse` when unset.
#   OBS_SLUG/OBS_ORG_ID  cross-link to the tenant the run created (set by caller).
#
# USAGE:
#   source "$(dirname "$0")/lib/obs.sh"
#   obs_init "my_test_name"
#   obs_step_start org_create
#   ... do the step ...
#   obs_step_end   org_create pass "" "http_code=$code"      # or:
#   obs_step_end   org_create fail "org create 500" "http_code=$code" "body=$snip"
#   # in fail()/skip handlers, attribute to the in-flight step:
#   obs_fail_current fail "$msg"
#   # leak verify (run_footprint integration — item 7):
#   obs_leak container mol-ws-abc "executeOrgPurge:purgeInfra"
#   # at the end (EXIT trap):
#   obs_run_end "$overall_status"

# ---------------------------------------------------------------------------
# Internal helpers
# ---------------------------------------------------------------------------

# _obs_now_ns: current time in nanoseconds (Loki push timestamp).
_obs_now_ns() {
  local ns
  ns=$(date +%s%N 2>/dev/null)
  case "$ns" in
    ''|*[!0-9]*) ns="$(date +%s)000000000" ;;
  esac
  printf '%s' "$ns"
}

# _obs_iso: UTC ISO-8601 timestamp for the human-readable line field.
_obs_iso() { date -u +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || printf 'unknown'; }

# _obs_lbl VALUE: sanitize a Loki LABEL value to a bounded charset.
_obs_lbl() { printf '%s' "$1" | LC_ALL=C tr -c 'a-zA-Z0-9_.:/-' '_'; }

# _obs_trunc VALUE [N]: truncate to N chars (default 600) — bounds line size.
_obs_trunc() { printf '%.*s' "${2:-600}" "$1"; }

# _obs_redact VALUE: scrub secrets (Bearer tokens, JWTs, long hex) before ship.
_obs_redact() {
  printf '%s' "$1" | LC_ALL=C sed -E \
    -e 's/([Bb]earer )[A-Za-z0-9._~+/=-]+/\1[REDACTED]/g' \
    -e 's/eyJ[A-Za-z0-9._-]{8,}/[REDACTED_JWT]/g' \
    -e 's/[A-Fa-f0-9]{32,}/[REDACTED_HEX]/g' 2>/dev/null || printf '%s' "$1"
}

# _obs_json_escape STRING: escape a string for embedding as a JSON string value.
_obs_json_escape() {
  local s="$1"
  s=${s//\\/\\\\}
  s=${s//\"/\\\"}
  s=${s//$'\t'/\\t}
  s=${s//$'\r'/\\r}
  s=${s//$'\n'/\\n}
  printf '%s' "$s"
}

# _obs_field_str KEY VALUE -> "KEY":"<escaped redacted value>"
_obs_field_str() {
  printf '"%s":"%s"' "$1" "$(_obs_json_escape "$(_obs_trunc "$(_obs_redact "$2")" 600)")"
}

# _obs_field_num KEY VALUE -> "KEY":<int>  (falls back to quoted if not an int)
_obs_field_num() {
  case "$2" in
    ''|*[!0-9]*) printf '"%s":"%s"' "$1" "$(_obs_json_escape "$2")" ;;
    *)           printf '"%s":%s' "$1" "$2" ;;
  esac
}

# _obs_guess_env: derive the env label from the CP URL when OBS_ENV is unset.
_obs_guess_env() {
  local u="${MOLECULE_CP_URL:-${OBS_CP_URL:-${CP_URL:-}}}"
  case "$u" in
    *staging-api*|*staging.*)              printf 'staging' ;;
    *localhost*|*127.0.0.1*|*lvh.me*)      printf 'local' ;;
    *api.moleculesai*)                     printf 'prod' ;;
    *)                                     printf '%s' "${E2E_ENV:-unknown}" ;;
  esac
}

# _obs_git_sha: short git sha — GITHUB_SHA (CI) or `git rev-parse`, else unknown.
_obs_git_sha() {
  if [ -n "${GITHUB_SHA:-}" ]; then printf '%.12s' "$GITHUB_SHA"; return 0; fi
  local s
  s=$(git rev-parse --short=12 HEAD 2>/dev/null || printf '')
  printf '%s' "${s:-unknown}"
}

# _obs_push LABELS_FRAGMENT LINE_JSON — ship one event to Loki (fail-soft).
# Circuit breaker: once a push fails (Loki down/slow), _OBS_DOWN trips and every
# later push short-circuits — so a down Loki costs AT MOST one timeout for the
# whole run, never one-per-step (the stdout breadcrumb in obs_event still fires).
_obs_push() {
  [ "${OBS_ENABLED:-1}" = "1" ] || return 0
  [ "${_OBS_DOWN:-0}" = "1" ] && return 0
  local ns line_esc body url
  ns=$(_obs_now_ns)
  line_esc=$(_obs_json_escape "$2")
  body="{\"streams\":[{\"stream\":{$1},\"values\":[[\"$ns\",\"$line_esc\"]]}]}"
  url="${OBS_LOKI_URL:-http://localhost:3102}"
  url="${url%/}/loki/api/v1/push"
  local hdr=(-H "Content-Type: application/json")
  [ -n "${OBS_LOKI_TENANT:-}" ] && hdr+=(-H "X-Scope-OrgID: ${OBS_LOKI_TENANT}")
  [ -n "${OBS_LOKI_BEARER:-}" ] && hdr+=(-H "Authorization: Bearer ${OBS_LOKI_BEARER}")
  if ! curl -sS --max-time "${OBS_PUSH_TIMEOUT:-5}" -o /dev/null "${hdr[@]}" \
        -X POST "$url" --data-binary "$body" >/dev/null 2>&1; then
    _OBS_DOWN=1
    if [ "${_OBS_WARNED:-0}" != "1" ]; then
      printf '[obs] WARN: could not ship to Loki at %s — disabling further pushes (e2e never fails on obs).\n' "$url" >&2
      _OBS_WARNED=1
    fi
  fi
  return 0
}

# _obs_labels STEP STATUS -> the Loki stream label fragment (bounded labels).
_obs_labels() {
  printf '"job":"e2e","env":"%s","test":"%s","run_id":"%s","step":"%s","status":"%s"' \
    "$(_obs_lbl "${OBS_ENV:-unknown}")" \
    "$(_obs_lbl "${OBS_TEST:-e2e}")" \
    "$(_obs_lbl "${OBS_RUN_ID:-unknown}")" \
    "$(_obs_lbl "$1")" \
    "$(_obs_lbl "$2")"
}

# ---------------------------------------------------------------------------
# Public API
# ---------------------------------------------------------------------------

# obs_init [TEST_NAME] — initialise run identity. Idempotent-ish; call once near
# the top of the script. Emits a run=running marker.
obs_init() {
  OBS_TEST="${1:-${OBS_TEST:-e2e}}"
  OBS_ENABLED="${OBS_ENABLED:-1}"
  OBS_LOKI_URL="${OBS_LOKI_URL:-http://localhost:3102}"
  OBS_RUN_ID="${OBS_RUN_ID:-${E2E_RUN_ID:-local-$(date +%s)-$$}}"
  OBS_RUN_ID="$(_obs_lbl "$OBS_RUN_ID")"
  OBS_ENV="${OBS_ENV:-$(_obs_guess_env)}"
  OBS_GIT_SHA="${OBS_GIT_SHA:-$(_obs_git_sha)}"
  OBS_HOST="${OBS_HOST:-$(hostname 2>/dev/null || printf 'unknown')}"
  declare -gA _OBS_T 2>/dev/null || true
  _OBS_RUN_START=$(date +%s)
  _OBS_PASS=0
  _OBS_FAIL=0
  obs_event run running 0 "" "git_sha=${OBS_GIT_SHA}" "host=${OBS_HOST}" "loki=${OBS_LOKI_URL}"
  return 0
}

# obs_event STEP STATUS DURATION_SECS ERROR [k=v ...] — low-level emit.
obs_event() {
  local step="$1" status="$2" dur="$3" err="${4:-}"
  shift 4 2>/dev/null || shift "$#"
  local fields kv key val
  fields="$(_obs_field_str ts "$(_obs_iso)")"
  fields="$fields,$(_obs_field_str run_id "${OBS_RUN_ID:-unknown}")"
  fields="$fields,$(_obs_field_str test "${OBS_TEST:-e2e}")"
  fields="$fields,$(_obs_field_str env "${OBS_ENV:-unknown}")"
  fields="$fields,$(_obs_field_str git_sha "${OBS_GIT_SHA:-unknown}")"
  fields="$fields,$(_obs_field_str step "$step")"
  fields="$fields,$(_obs_field_str status "$status")"
  fields="$fields,$(_obs_field_num duration_secs "$dur")"
  [ -n "${OBS_SLUG:-}" ]   && fields="$fields,$(_obs_field_str slug "$OBS_SLUG")"
  [ -n "${OBS_ORG_ID:-}" ] && fields="$fields,$(_obs_field_str org_id "$OBS_ORG_ID")"
  if [ -n "$err" ]; then
    fields="$fields,$(_obs_field_str error "$err")"
  fi
  for kv in "$@"; do
    key="${kv%%=*}"
    val="${kv#*=}"
    [ -z "$key" ] && continue
    fields="$fields,$(_obs_field_str "$key" "$val")"
  done
  _obs_push "$(_obs_labels "$step" "$status")" "{$fields}"
  # Cross-reference breadcrumb in the runner log (so stdout <-> obs link is easy).
  printf '[obs] run=%s step=%s status=%s dur=%ss%s\n' \
    "${OBS_RUN_ID:-?}" "$step" "$status" "$dur" \
    "$( [ -n "$err" ] && printf ' err=%s' "$(_obs_trunc "$err" 140)" )" >&2
  return 0
}

# obs_step_start STEP — stamp the step start; emits a running marker.
obs_step_start() {
  local step="$1"
  _OBS_CUR_STEP="$step"
  _OBS_CUR_START=$(date +%s)
  _OBS_T["$step"]="$_OBS_CUR_START" 2>/dev/null || true
  obs_event "$step" running 0 ""
  return 0
}

# obs_step_end STEP STATUS [ERROR] [k=v ...] — compute duration + emit.
obs_step_end() {
  local step="$1" status="$2"
  shift 2 2>/dev/null || shift "$#"
  local err="${1:-}"
  [ "$#" -gt 0 ] && shift
  local start now dur
  start="${_OBS_T[$step]:-${_OBS_CUR_START:-$(date +%s)}}"
  now=$(date +%s)
  dur=$(( now - start ))
  [ "$dur" -lt 0 ] && dur=0
  case "$status" in
    pass)               _OBS_PASS=$(( ${_OBS_PASS:-0} + 1 )) ;;
    fail|timeout|error) _OBS_FAIL=$(( ${_OBS_FAIL:-0} + 1 )) ;;
  esac
  obs_event "$step" "$status" "$dur" "$err" "$@"
  return 0
}

# obs_fail_current STATUS ERROR [k=v ...] — attribute a failure/skip to the
# step currently in flight (for use inside fail()/skip_loud()/timeout exits).
obs_fail_current() {
  local status="$1" err="${2:-}"
  shift 2 2>/dev/null || shift "$#"
  obs_step_end "${_OBS_CUR_STEP:-unknown}" "$status" "$err" "$@"
  return 0
}

# obs_leak RESOURCE_TYPE NAME OWNER_OP — emit a teardown-leak event (item 7:
# a run_footprint VERIFY-FAIL / reaper-fire renders in the same obs view with
# the leaked resource + the teardown op that should have removed it).
obs_leak() {
  obs_event leak_verify leak 0 "teardown left residual resource" \
    "leak_resource_type=${1:-}" "leak_resource_name=${2:-}" "leak_owner_op=${3:-}"
  return 0
}

# obs_run_end STATUS — emit the run-summary event (step=run) used by the
# "recent runs" dashboard table. Call from the EXIT trap.
obs_run_end() {
  [ "${_OBS_RUN_ENDED:-0}" = "1" ] && return 0
  _OBS_RUN_ENDED=1
  local status="$1" now start dur
  now=$(date +%s)
  start="${_OBS_RUN_START:-$now}"
  dur=$(( now - start ))
  [ "$dur" -lt 0 ] && dur=0
  obs_event run "$status" "$dur" "" \
    "steps_pass=${_OBS_PASS:-0}" "steps_fail=${_OBS_FAIL:-0}" \
    "git_sha=${OBS_GIT_SHA:-unknown}" "slug=${OBS_SLUG:-}" "org_id=${OBS_ORG_ID:-}"
  return 0
}
