#!/usr/bin/env bash
# Offline unit test for lib/workspace_create_retry.sh — the classifier + header
# parsers behind the staging full-SaaS `POST /workspaces` cold-origin 5xx retry.
# No network, no curl. Proves the retry decision distinguishes a transient
# empty-body 5xx (retry) from a real app error (never retry) and from a
# persistent outage (exhausts → RED). Negative-control: flip SENTINEL_BROKEN=1
# to invert the classifier and watch the guard assertions FAIL.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib/workspace_create_retry.sh
source "$HERE/lib/workspace_create_retry.sh"

# Optional fault injection so we can PROVE these assertions can fail (a test you
# have not seen fail proves nothing).
if [ "${SENTINEL_BROKEN:-0}" = "1" ]; then
  # Broken variant: retry EVERYTHING (incl. real JSON errors) and never on 5xx —
  # the exact over-corrections the guards below must catch.
  create_is_transient_cold_5xx() {
    local body="$2"
    printf '%s' "$body" | grep -q '[^[:space:]]' && return 0  # WRONG: retries JSON errors
    return 1                                                    # WRONG: never retries empty 5xx
  }
fi

pass=0 fail=0
ok()  { if eval "$2"; then echo "  PASS: $1"; pass=$((pass+1)); else echo "  FAIL: $1"; fail=$((fail+1)); fi; }

hdr() { # write a fixture header dump to a temp file, echo its path
  local f; f=$(mktemp); printf '%b' "$1" > "$f"; echo "$f"; }

echo "── classifier: create_is_transient_cold_5xx ──"
# Transient cold-origin: empty body + 5xx → RETRY (rc 0)
ok "empty-body 503 → retry"         'create_is_transient_cold_5xx 503 ""'
ok "empty-body 502 → retry"         'create_is_transient_cold_5xx 502 ""'
ok "empty-body 504 → retry"         'create_is_transient_cold_5xx 504 ""'
ok "whitespace-only 503 → retry"    'create_is_transient_cold_5xx 503 "   "'
# Real app errors: NON-empty body → NEVER retry (rc 1), even on a 5xx status.
ok "JSON 422 body → no retry"        '! create_is_transient_cold_5xx 422 "{\"error\":\"RUNTIME_UNSUPPORTED\"}"'
ok "JSON 400 body → no retry"        '! create_is_transient_cold_5xx 400 "{\"error\":\"invalid template\"}"'
ok "5xx WITH json body → no retry"   '! create_is_transient_cold_5xx 503 "{\"error\":\"boom\"}"'
# Non-5xx empty (e.g. odd 000/404) → not our transient → no retry.
ok "empty 404 → no retry"            '! create_is_transient_cold_5xx 404 ""'
ok "empty 000 → no retry"            '! create_is_transient_cold_5xx 000 ""'

echo "── header parsers ──"
H_CF=$(hdr 'HTTP/2 503 \r\ndate: Thu, 16 Jul 2026 19:01:25 GMT\r\ncontent-length: 0\r\nretry-after: 2\r\nserver: cloudflare\r\ncf-ray: a1c3415e0ed2368b-FRA\r\n')
H_11=$(hdr 'HTTP/1.1 503 Service Unavailable\r\nRetry-After: 5\r\nServer: nginx\r\n')
H_NORA=$(hdr 'HTTP/2 503 \r\nserver: cloudflare\r\n')
H_HOSTILE=$(hdr 'HTTP/2 503 \r\nretry-after: 900\r\nserver: cloudflare\r\n')
ok "status from HTTP/2 line"         '[ "$(create_parse_status "$H_CF")" = "503" ]'
ok "status from HTTP/1.1 line"       '[ "$(create_parse_status "$H_11")" = "503" ]'
ok "retry-after parsed (2)"          '[ "$(create_parse_retry_after "$H_CF")" = "2" ]'
ok "retry-after case-insensitive (5)" '[ "$(create_parse_retry_after "$H_11")" = "5" ]'
ok "retry-after default when absent"  '[ "$(create_parse_retry_after "$H_NORA")" = "2" ]'
ok "hostile retry-after capped at 10" '[ "$(create_parse_retry_after "$H_HOSTILE")" = "10" ]'
ok "server=cloudflare parsed"         '[ "$(create_parse_server "$H_CF")" = "cloudflare" ]'
ok "server=nginx parsed"              '[ "$(create_parse_server "$H_11")" = "nginx" ]'

rm -f "$H_CF" "$H_11" "$H_NORA" "$H_HOSTILE"
echo "──"
echo "totals: pass=$pass fail=$fail"
if [ "$fail" -ne 0 ]; then echo "❌ workspace-create retry unit FAILED"; exit 1; fi
echo "✅ workspace-create retry unit PASSED"
