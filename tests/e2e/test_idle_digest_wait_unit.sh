#!/usr/bin/env bash
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=lib/idle_digest_wait.sh
# shellcheck disable=SC1091
source "$HERE/lib/idle_digest_wait.sh"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

assert_eq() {
  local want="$1" got="$2" label="$3"
  [ "$got" = "$want" ] || fail "$label: got '$got', want '$want'"
}

PROBES=0
SLEEPS=""
SUCCEED_ON=1

probe() {
  PROBES=$((PROBES + 1))
  [ "$PROBES" -ge "$SUCCEED_ON" ]
}

sleep() {
  SLEEPS="${SLEEPS}${SLEEPS:+,}$1"
}

# Immediate evidence must not add a fixed soak delay.
idle_digest_wait 360 10 probe
assert_eq 1 "$PROBES" "immediate probe count"
assert_eq "" "$SLEEPS" "immediate sleep schedule"

# Evidence appearing on the third probe is returned as soon as it exists.
PROBES=0
SLEEPS=""
SUCCEED_ON=3
idle_digest_wait 360 10 probe
assert_eq 3 "$PROBES" "eventual probe count"
assert_eq "10,10" "$SLEEPS" "eventual sleep schedule"

# Timeout is bounded and includes a final probe exactly at the deadline.
PROBES=0
SLEEPS=""
SUCCEED_ON=99
if idle_digest_wait 25 10 probe; then
  fail "timeout case unexpectedly succeeded"
fi
assert_eq 4 "$PROBES" "timeout probe count"
assert_eq "10,10,5" "$SLEEPS" "timeout sleep schedule"

# Invalid budgets fail before invoking the probe or sleep.
PROBES=0
SLEEPS=""
if idle_digest_wait nope 10 probe 2>/dev/null; then
  fail "non-numeric timeout unexpectedly succeeded"
fi
assert_eq 0 "$PROBES" "invalid timeout probe count"
assert_eq "" "$SLEEPS" "invalid timeout sleep schedule"

if idle_digest_wait 10 0 probe 2>/dev/null; then
  fail "zero poll interval unexpectedly succeeded"
fi

# Enabling the live idle-digest assertion is a hard contract: the harness must
# refuse to run when Docker evidence cannot be inspected instead of silently
# skipping the assertion and reporting success.
if (
  # Intentionally hide any host Docker binary to exercise the missing-CLI arm.
  # shellcheck disable=SC2123
  PATH=/nonexistent
  idle_digest_require_docker
) 2>/dev/null; then
  fail "missing docker unexpectedly passed the idle-digest preflight"
fi

docker() {
  [ "${1:-}" = "ps" ] || return 0
  return 1
}
if idle_digest_require_docker 2>/dev/null; then
  fail "unavailable docker daemon unexpectedly passed the idle-digest preflight"
fi

docker() {
  [ "${1:-}" = "ps" ]
}
idle_digest_require_docker \
  || fail "usable docker unexpectedly failed the idle-digest preflight"
unset -f docker

# Pin the production harness to evidence polling. A future edit must not restore
# the fixed-soak shape that failed while the synchronous digest completion was
# still in flight.
SAAS_SCRIPT="$HERE/test_staging_full_saas.sh"
grep -Fq 'idle_digest_wait "$_IDLE_TIMEOUT_SECS" "$_IDLE_POLL_SECS" idle_digest_probe' "$SAAS_SCRIPT" \
  || fail "full-SaaS harness no longer uses bounded idle-digest evidence polling"
grep -Fq 'idle_digest_require_docker' "$SAAS_SCRIPT" \
  || fail "full-SaaS harness no longer fails closed when Docker is unavailable"
if grep -Fq '_IDLE_SOAK_SECS' "$SAAS_SCRIPT"; then
  fail "full-SaaS harness regressed to a fixed idle-digest soak"
fi

# This unit is load-bearing only if the normal PR CI lane actually executes it.
CI_WORKFLOW="$HERE/../../.gitea/workflows/ci.yml"
grep -Fq 'bash tests/e2e/test_idle_digest_wait_unit.sh' "$CI_WORKFLOW" \
  || fail "idle-digest unit is not wired into CI"

echo "PASS: idle digest bounded polling"
