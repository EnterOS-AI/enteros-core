#!/usr/bin/env bash
# Unit test for wait_tenant_api_ready (tenant_api_ready.sh). Drives the helper
# with a fake curl (TENANT_API_READY_CURL) that returns scripted HTTP codes +
# bodies from a scenario file, so we can assert the readiness/auth/HTML predicates
# without a live tenant. Wired in .gitea/workflows/ci.yml alongside the other
# tests/e2e/lib/*_unit.sh checks.
set -uo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
LIB="$SCRIPT_DIR/tenant_api_ready.sh"
# shellcheck source=tenant_api_ready.sh
source "$LIB" || { echo "FAIL: cannot source $LIB" >&2; exit 1; }

TMP=$(mktemp -d -t tenant-ready-unit-XXXXXX)
trap 'rm -rf "$TMP"' EXIT INT TERM

# Fake curl: emits, per invocation, the next "CODE<TAB>BODY" line from $SEQFILE
# (last line repeats once exhausted). Honors -o <file> (body) and -w (code→stdout).
cat > "$TMP/curl" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
out=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift 2;;
    -w) shift 2;;
    -H|-A|--max-time) shift 2;;
    -sS|-s) shift;;
    http://*|https://*) shift;;
    *) shift;;
  esac
done
idx_file="${SEQFILE:?}.idx"
n=$(cat "$idx_file" 2>/dev/null || echo 0)
line=$(sed -n "$((n+1))p" "${SEQFILE:?}")
[ -z "$line" ] && line=$(tail -n 1 "${SEQFILE:?}")   # repeat last
echo $((n+1)) > "$idx_file"
code=${line%%$'\t'*}
body=${line#*$'\t'}
[ -n "$out" ] && printf '%s' "$body" > "$out"
printf '%s' "$code"
FAKE
chmod +x "$TMP/curl"

PASS=0; FAIL=0
run() {  # run <scenario-lines-file> <deadline> <poll> <streak>
  local seq="$1"
  printf '%s' "" > "$seq.idx"
  TENANT_API_READY_CURL="$TMP/curl" SEQFILE="$seq" \
    TENANT_API_READY_DEADLINE="$2" TENANT_API_READY_POLL="$3" \
    TENANT_API_READY_STREAK="$4" TENANT_API_READY_TIMEOUT=5 \
    wait_tenant_api_ready "https://t.example" /workspaces "tok" "org" "unit" 2>"$TMP/err"
}
seqfile() { local f="$TMP/$1"; shift; : > "$f"; for l in "$@"; do printf '%s\n' "$l" >> "$f"; done; echo "$f"; }
delcount() { grep -c . "$1.idx" >/dev/null 2>&1; cat "$1.idx" 2>/dev/null || echo 0; }
check() { local label="$1" want="$2" got="$3"; if [ "$got" = "$want" ]; then echo "PASS: $label (rc=$got)"; PASS=$((PASS+1)); else echo "FAIL: $label want=$want got=$got" >&2; sed 's/^/  /' "$TMP/err" >&2; FAIL=$((FAIL+1)); fi; }

TAB=$'\t'

# 1. two JSON 200s → ready (rc 0)
s=$(seqfile s1 "200${TAB}[]" "200${TAB}[]"); run "$s" 30 0 2; check "stable 2x200 JSON is ready" 0 $?

# 2. NEG-CONTROL: 401 → FAIL FAST (rc 1) on the FIRST call, not after the deadline
s=$(seqfile s2 "401${TAB}unauthorized"); run "$s" 30 0 2; rc=$?
check "401 auth failure fails fast" 1 "$rc"
[ "$(cat "$s.idx")" = "1" ] && { echo "PASS: 401 made exactly ONE call (no deadline burn)"; PASS=$((PASS+1)); } || { echo "FAIL: 401 should be 1 call, was $(cat "$s.idx")" >&2; FAIL=$((FAIL+1)); }
grep -q "AUTH failure" "$TMP/err" && { echo "PASS: 401 reports AUTH not half-wired"; PASS=$((PASS+1)); } || { echo "FAIL: 401 msg" >&2; FAIL=$((FAIL+1)); }

# 3. 403 → also fail fast
s=$(seqfile s3 "403${TAB}forbidden"); run "$s" 30 0 2; check "403 auth failure fails fast" 1 $?

# 4. 200-but-HTML (SPA fallback) never satisfies the streak → times out (rc 1)
s=$(seqfile s4 "200${TAB}<html><body>spa</body></html>"); run "$s" 2 1 2; rc=$?
check "200-but-SPA-HTML is NOT ready (times out)" 1 "$rc"
grep -q "controlplane#1012" "$TMP/err" && { echo "PASS: persistent not-ready reports half-wired"; PASS=$((PASS+1)); } || { echo "FAIL: 1012 msg" >&2; FAIL=$((FAIL+1)); }

# 5. transient 503 then JSON 200s → recovers to ready (rc 0)
s=$(seqfile s5 "503${TAB}" "200${TAB}[]" "200${TAB}[]"); run "$s" 30 0 2; check "503 then 200 JSON recovers" 0 $?

# 6. HTML-200 then real JSON-200s → recovers (streak reset on HTML, then completes)
s=$(seqfile s6 "200${TAB}<html>" "200${TAB}[]" "200${TAB}[]"); run "$s" 30 0 2; check "HTML-200 then JSON-200 recovers" 0 $?

echo "=== tenant_api_ready unit: passed=$PASS failed=$FAIL ==="
[ "$FAIL" = "0" ]
