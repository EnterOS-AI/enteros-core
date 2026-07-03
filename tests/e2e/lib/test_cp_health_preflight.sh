#!/usr/bin/env bash
# test_cp_health_preflight.sh — offline unit test for cp_health_preflight.sh.
#
# Proves the readiness-poll contract the completion-gated e2e lanes depend on:
#   (a) a cold-start burst (transport-fail 000 → 5xx → 200s) RETRIES, then
#       succeeds once it sees a stable Nx200 streak
#   (b) a flap in the middle RESETS the streak (requires N *consecutive* 200s)
#   (c) persistent non-ready returns 1 (never hangs, never a false green)
#   (d) a missing URL is a config error (return 2)
#
# curl is mocked via CP_HEALTH_PREFLIGHT_CURL; each call pops the next line of a
# PLAN file ("<curl_exit> <http_code>") and prints the code (or nothing + a
# non-zero exit to simulate a transport failure, exactly what makes the helper's
# `|| code=000` fire). A COUNTER records calls so we can assert EXACT attempt
# counts. Zero real sleeps (CP_HEALTH_PREFLIGHT_POLL=0) → deterministic + fast.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

# shellcheck source=tests/e2e/lib/cp_health_preflight.sh
. "$ROOT/cp_health_preflight.sh"

export CP_HEALTH_PREFLIGHT_POLL=0
export CP_HEALTH_PREFLIGHT_STREAK=3
export CP_HEALTH_PREFLIGHT_DEADLINE=60

fail() { echo "FAIL: $*" >&2; exit 1; }
count() { if [[ -s "$1" ]]; then wc -l <"$1" | tr -d ' '; else echo 0; fi; }

# make_mock_curl PATH COUNTER PLAN — PLAN lines: "<curl_exit> <http_code>";
# the last line repeats once exhausted (so "always 503" needs one line).
make_mock_curl() {
  local path="$1" counter="$2" plan="$3"
  cat >"$path" <<SH
#!/usr/bin/env bash
echo "call" >> "$counter"
n=\$(wc -l < "$counter" | tr -d ' ')
line=\$(sed -n "\${n}p" "$plan"); [[ -z "\$line" ]] && line=\$(tail -n1 "$plan")
cexit=\$(printf '%s' "\$line" | awk '{print \$1}')
ccode=\$(printf '%s' "\$line" | awk '{print \$2}')
[[ "\$cexit" == "0" ]] && printf '%s' "\$ccode"
exit "\$cexit"
SH
  chmod +x "$path"
  export CP_HEALTH_PREFLIGHT_CURL="$path"
}

# (a) cold-start burst then stable 3x200 → success after exactly 5 attempts.
COUNTER="$TMPDIR/a.count"; : >"$COUNTER"
PLAN="$TMPDIR/a.plan"
printf '%s\n' '28 000' '0 503' '0 200' '0 200' '0 200' > "$PLAN"
make_mock_curl "$TMPDIR/a.sh" "$COUNTER" "$PLAN"
cp_health_preflight "https://cp.example" "workspace" >/dev/null || fail "(a) expected ready after cold-start burst"
got=$(count "$COUNTER"); [[ "$got" == "5" ]] || fail "(a) expected 5 attempts (000+503+200+200+200), got $got"
echo "PASS (a) cold-start burst retried, then stable 3x200 (5 attempts)"

# (b) a flap in the middle resets the streak → needs 3 CONSECUTIVE 200s.
COUNTER="$TMPDIR/b.count"; : >"$COUNTER"
PLAN="$TMPDIR/b.plan"
printf '%s\n' '0 200' '0 503' '0 200' '0 200' '0 200' > "$PLAN"
make_mock_curl "$TMPDIR/b.sh" "$COUNTER" "$PLAN"
cp_health_preflight "https://cp.example" >/dev/null || fail "(b) expected ready after flap+recovery"
got=$(count "$COUNTER"); [[ "$got" == "5" ]] || fail "(b) flap must reset streak; expected 5 attempts, got $got"
echo "PASS (b) mid-flap reset the streak (needs 3 consecutive 200s, 5 attempts)"

# (c) persistent non-ready → return 1 (deadline reached, no hang, no false green).
COUNTER="$TMPDIR/c.count"; : >"$COUNTER"
PLAN="$TMPDIR/c.plan"
printf '%s\n' '0 503' > "$PLAN"
make_mock_curl "$TMPDIR/c.sh" "$COUNTER" "$PLAN"
if CP_HEALTH_PREFLIGHT_DEADLINE=0 cp_health_preflight "https://cp.example" "concierge" >/dev/null; then
  fail "(c) expected non-zero on persistent non-ready"
fi
echo "PASS (c) persistent non-ready returned 1 (no hang, no false green)"

# (c2) persistent transport failure (000) also returns 1.
COUNTER="$TMPDIR/c2.count"; : >"$COUNTER"
PLAN="$TMPDIR/c2.plan"
printf '%s\n' '28 000' > "$PLAN"
make_mock_curl "$TMPDIR/c2.sh" "$COUNTER" "$PLAN"
if CP_HEALTH_PREFLIGHT_DEADLINE=0 cp_health_preflight "https://cp.example" >/dev/null; then
  fail "(c2) expected non-zero on persistent transport failure"
fi
echo "PASS (c2) persistent transport failure (000) returned 1"

# (d) missing URL is a config error (return 2), not a hang.
unset CP_HEALTH_PREFLIGHT_CURL
rc=0; ( MOLECULE_CP_URL="" CP_BASE_URL="" cp_health_preflight "" >/dev/null 2>&1 ) || rc=$?
[[ "$rc" == "2" ]] || fail "(d) expected return 2 for missing URL, got $rc"
echo "PASS (d) missing URL is a config error (return 2)"

echo "cp_health_preflight test passed"
