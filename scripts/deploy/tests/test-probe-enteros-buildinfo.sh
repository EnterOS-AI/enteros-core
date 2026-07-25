#!/usr/bin/env bash
# test-probe-enteros-buildinfo.sh — behavior tests for the ADVISORY enteros.ai
# edge probe (Enter OS rebrand Phase 2, internal#1089).
#
# Drives the REAL script against a fake docker + fake curl (same harness style
# as test-managed-flags.sh) and asserts on what curl was actually ASKED to
# fetch plus the exit code — so every property below has a reachable fail arm:
#
#   1. DUAL-PREFIX fqdn derivation: a mol-tenant-* and an enteros-tenant-*
#      container BOTH get probed at <slug>.<domain>/buildinfo. A single-prefix
#      matcher (either direction) goes red here.
#   2. FAIL CLOSED on probe failure, with the loud internal#1089 ::warning::
#      that names what to check — proving the fail arm the flip-to-blocking
#      depends on is real and reachable.
#   3. A parked 200 (HTML, no git_sha) must NOT pass — an answering host is
#      not an answering TENANT.
#   4. NEVER FABRICATE "NOTHING EXISTS": a failed `docker ps` is a probe error
#      (exit 1, no curl ever runs), NOT an empty fleet.
#   5. A genuinely EMPTY enumeration is a clean no-op (exit 0, no curl).
#   6. --fqdn canary mode needs no docker at all.
#   7. An unrecognizably-named labelled tenant fails closed (cannot derive its
#      fqdn => cannot claim coverage).
#   8. Retry budget: PROBE_ATTEMPTS attempts actually happen.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
script="$here/../probe-enteros-buildinfo.sh"

fail() { echo "FAIL: $*" >&2; exit 1; }

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin" "$tmp/bin-nodocker"

# ── Fake docker: `ps` output driven by FAKE_PS_MODE ──────────────────────────
cat > "$tmp/bin/docker" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
if [ "${1:-}" = "ps" ]; then
  case "${FAKE_PS_MODE:-dual}" in
    dual)
      # LITERAL dual-generation fixture (mirrors test-managed-flags.sh test 7):
      # never derived from the script's prefix list, so a matcher blind to
      # either brand generation is caught by the curl-log assertion.
      printf 'mol-tenant-alpha\n'
      printf 'enteros-tenant-beta\n'
      ;;
    empty) ;;
    badname) printf 'weird-name-tenant\n' ;;
    fail) exit 1 ;;
  esac
  exit 0
fi
exit 0
FAKE

# ── Fake curl: records every argv line; body driven by FAKE_CURL_MODE ────────
cat > "$tmp/bin/curl" <<'FAKECURL'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "${FAKE_CURL_LOG:?}"
case "${FAKE_CURL_MODE:-ok}" in
  ok)   echo '{"git_sha":"deadbee1234567890"}' ;;
  html) echo '<html><body>parked</body></html>' ;;
  fail) exit 6 ;;
esac
FAKECURL
chmod +x "$tmp/bin/docker" "$tmp/bin/curl"

# For test 6 (--fqdn mode is docker-free): a tripwire docker that records any
# invocation and fails loudly. If canary mode ever grows a docker dependency,
# the tripwire log turns non-empty and the assertion goes red.
cp "$tmp/bin/curl" "$tmp/bin-nodocker/curl"
cat > "$tmp/bin-nodocker/docker" <<'TRIP'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "${FAKE_DOCKER_TRIPWIRE:?}"
exit 1
TRIP
chmod +x "$tmp/bin-nodocker/docker"

# run_probe <ps-mode> <curl-mode> <args...> — runs the real script, captures
# combined output in $out and exit code in $rc; curl argv lines in $curl_log.
out=""; rc=0; curl_log="$tmp/curl.log"
run_probe() {
  local ps_mode="$1" curl_mode="$2"; shift 2
  : > "$curl_log"
  rc=0
  out="$(PATH="$tmp/bin:$PATH" \
         FAKE_PS_MODE="$ps_mode" FAKE_CURL_MODE="$curl_mode" \
         FAKE_CURL_LOG="$curl_log" \
         PROBE_ATTEMPTS="${PROBE_ATTEMPTS:-1}" PROBE_SLEEP_SECS=0 \
         bash "$script" "$@" 2>&1)" || rc=$?
}

# 1. DUAL-PREFIX derivation: both brand generations are probed on the target
#    domain. Asserted on curl's ARGV — a slug/prefix regression cannot hide.
run_probe dual ok --cp-env production --domain enteros.example
[ "$rc" = 0 ] || fail "happy path exited $rc: $out"
grep -q 'https://alpha\.enteros\.example/buildinfo' "$curl_log" \
  || fail "legacy mol-tenant-* slug was not probed (curl log: $(cat "$curl_log"))"
grep -q 'https://beta\.enteros\.example/buildinfo' "$curl_log" \
  || fail "enteros-tenant-* slug was not probed — matcher is blind to the new brand generation"
[ "$(wc -l < "$curl_log" | tr -d " ")" = 2 ] || fail "expected exactly 2 probes, got: $(cat "$curl_log")"

# 2. FAIL CLOSED + the loud advisory warning. This is the fail arm the
#    flip-to-blocking relies on: prove it is reachable and names the checks.
run_probe dual fail --cp-env production --domain enteros.example
[ "$rc" = 1 ] || fail "curl failure did not fail the probe (rc=$rc) — the advisory arm can never fire"
grep -q '::warning::' <<<"$out" || fail "no ::warning:: on probe failure"
grep -q 'internal#1089' <<<"$out" || fail "warning does not reference internal#1089 Phase 2"
grep -q 'DNS' <<<"$out" || fail "warning does not say what to check (DNS/ingress/TLS/body)"
grep -q 'continue-on-error' <<<"$out" || fail "warning does not state the flip-to-blocking condition"

# 3. A parked 200 page (no git_sha) is NOT a served tenant.
run_probe dual html --cp-env production --domain enteros.example
[ "$rc" = 1 ] || fail "an HTML body with no git_sha passed the probe — a parked page would read as routed"
grep -q 'no git_sha' <<<"$out" || fail "wrong-service failure did not name the missing git_sha"

# 4. NEVER fabricate "nothing exists": docker ps failure is a probe ERROR.
run_probe fail ok --cp-env production --domain enteros.example
[ "$rc" = 1 ] || fail "a FAILED docker ps was treated as success (rc=$rc) — a probe error fabricated 'no tenants'"
grep -q 'ENUMERATE' <<<"$out" || fail "enumeration failure not reported as such: $out"
grep -q 'no running' <<<"$out" && fail "enumeration failure was reported as an empty fleet"
[ -s "$curl_log" ] && fail "probes ran despite a failed enumeration"

# 5. A genuinely empty fleet is a clean no-op.
run_probe empty ok --cp-env production --domain enteros.example
[ "$rc" = 0 ] || fail "empty fleet exited $rc: $out"
grep -q 'nothing to probe' <<<"$out" || fail "empty fleet not reported: $out"
[ -s "$curl_log" ] && fail "probes ran against an empty fleet"

# 6. --fqdn canary mode: never touches docker (publish workflow shape). The
#    tripwire docker records-and-fails on ANY invocation, so a docker
#    dependency shows up as rc!=0 AND a non-empty tripwire log.
: > "$curl_log"
tripwire="$tmp/docker.tripwire"; : > "$tripwire"
rc=0
out="$(PATH="$tmp/bin-nodocker:$PATH" \
       FAKE_CURL_MODE=ok FAKE_CURL_LOG="$curl_log" \
       FAKE_DOCKER_TRIPWIRE="$tripwire" \
       PROBE_ATTEMPTS=1 PROBE_SLEEP_SECS=0 \
       bash "$script" --fqdn canary.enteros.example 2>&1)" || rc=$?
[ "$rc" = 0 ] || fail "--fqdn mode exited $rc (does it depend on docker?): $out"
[ -s "$tripwire" ] && fail "--fqdn mode invoked docker: $(cat "$tripwire")"
grep -q 'https://canary\.enteros\.example/buildinfo' "$curl_log" \
  || fail "--fqdn mode did not probe the given host: $(cat "$curl_log")"
[ "$(wc -l < "$curl_log" | tr -d " ")" = 1 ] || fail "--fqdn mode probed more than the one host"

# 7. A labelled tenant with an unrecognizable name fails closed.
run_probe badname ok --cp-env production --domain enteros.example
[ "$rc" = 1 ] || fail "an underivable tenant name passed silently (rc=$rc) — coverage would be hollow"
grep -q 'no known brand prefix' <<<"$out" || fail "underivable name not explained: $out"

# 8. Retry budget: PROBE_ATTEMPTS attempts actually happen before the verdict.
PROBE_ATTEMPTS=3 run_probe dual fail --cp-env production --domain enteros.example
[ "$rc" = 1 ] || fail "retry run unexpectedly passed"
[ "$(wc -l < "$curl_log" | tr -d " ")" = 6 ] \
  || fail "expected 3 attempts x 2 tenants = 6 curl calls, got $(wc -l < "$curl_log" | tr -d " ")"

echo "ok: enteros.ai edge probe derives both brand generations, fails closed with the internal#1089 warning, never fabricates an empty fleet, and canary mode is docker-free"
