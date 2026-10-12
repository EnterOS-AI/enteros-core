#!/usr/bin/env bash
# Offline unit test for lib/workspace_create_retry.sh — the classifier + header
# parsers behind the staging full-SaaS `POST /workspaces` cold-origin retry.
# No network, no curl. Proves the retry decision distinguishes the cold-origin
# "never reached a handler" signatures (empty-body 503 / connection reset) from
# a real app error and from the maybe-processed 502/504 (never retried, to keep
# a non-idempotent create from duplicating). Negative control: SENTINEL_BROKEN=1
# fault-injects BOTH the classifier AND the header parsers, so EVERY assertion
# below has a demonstrated fail arm (a test you have not seen fail proves nothing).
# The H_* header fixtures below are consumed only inside the single-quoted
# assertion strings passed to ok()/eval, which shellcheck cannot follow — so it
# reports every fixture as unused. They ARE used. (File-level directive: must
# precede the first command to apply file-wide.)
# shellcheck disable=SC2034
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib/workspace_create_retry.sh
source "$HERE/lib/workspace_create_retry.sh"

# Fault injection so we can PROVE these assertions can fail.
if [ "${SENTINEL_BROKEN:-0}" = "1" ]; then
  # Broken classifier: retry EVERYTHING (incl. real JSON errors + 502/504) and
  # never on the cold signatures — the exact over-corrections the guards catch.
  # Also INVERTS the transport-exit rule (retries a client timeout — the removed
  # double-create — and refuses a genuine connection reset) so BOTH new
  # curl-exit assertions below have a demonstrated fail arm.
  create_should_retry_cold() {
    local status="$1" body="$2" curl_exit="${3:-}"
    printf '%s' "$body" | grep -q '[^[:space:]]' && return 0  # WRONG: retries JSON errors
    if [ -n "$curl_exit" ] && [ "$curl_exit" != "0" ]; then
      [ "$curl_exit" = "28" ] && return 0 || return 1          # WRONG: retries timeout, refuses reset
    fi
    return 1                                                    # WRONG: never retries empty cold
  }
  # Broken parsers: head instead of tail (interim 1xx wins), no default/date
  # guard, no server extraction — so the parser assertions have a fail arm too.
  create_parse_status()      { head -1 "$1" 2>/dev/null | awk '{print $2}' | tr -dc '0-9'; }
  create_parse_retry_after() { grep -i '^retry-after:' "$1" 2>/dev/null | head -1 | tr -dc '0-9'; }
  create_parse_server()      { printf 'BROKEN'; }
fi

# All fixtures live in ONE scratch dir cleaned by an EXIT trap, so a failed
# assertion's `exit 1` never leaks scratch (RFC #2873 / lint_cleanup_traps.sh).
# A trapped dir (not a per-file array) is required because hdr() runs inside a
# `$(...)` subshell, where an array append would not survive to the parent.
E2E_TMPDIR=$(mktemp -d -t wscreate-XXXXXX)
trap 'rm -rf "$E2E_TMPDIR"' EXIT INT TERM

pass=0 fail=0
ok()  { if eval "$2"; then echo "  PASS: $1"; pass=$((pass+1)); else echo "  FAIL: $1"; fail=$((fail+1)); fi; }
hdr() { local f; f=$(mktemp "$E2E_TMPDIR/h.XXXXXX"); printf '%b' "$1" > "$f"; echo "$f"; }

echo "── classifier: create_should_retry_cold ──"
# Cold-origin "never reached a handler" → RETRY (rc 0)
ok "empty-body 503 → retry"          'create_should_retry_cold 503 ""'
ok "whitespace-only 503 → retry"     'create_should_retry_cold 503 "   "'
ok "empty status (conn reset) → retry" 'create_should_retry_cold "" ""'
ok "curl 000 → retry"                'create_should_retry_cold 000 ""'
# Maybe-processed gateway errors → NEVER retry (non-idempotent create)
ok "empty-body 502 → no retry"       '! create_should_retry_cold 502 ""'
ok "empty-body 504 → no retry"       '! create_should_retry_cold 504 ""'
# Transport failures (empty status line): the curl exit code decides. A client
# TIMEOUT (28) is maybe-processed → NEVER retry (would double-create the
# non-idempotent POST); a connection reset/refused (7/56/…) never reached a
# handler → retry. Mirrors the Go coldCreateTransportRetryable + TS classifier.
ok "curl timeout 28 → no retry"      '! create_should_retry_cold "" "" 28'
ok "curl reset 56 → retry"           'create_should_retry_cold "" "" 56'
ok "curl refused 7 → retry"          'create_should_retry_cold "" "" 7'
ok "curl ssl-connect 35 → retry"     'create_should_retry_cold "" "" 35'
# A 503 response still makes curl --fail-with-body exit 22, but a status line
# WAS received → classify on the status (retry), not as a transport failure.
ok "503 with curl-exit 22 → retry (response received)" 'create_should_retry_cold 503 "" 22'
# Real app errors: NON-empty body → NEVER retry, even on a 503 status.
ok "JSON 422 body → no retry"        '! create_should_retry_cold 422 "{\"error\":\"RUNTIME_UNSUPPORTED\"}"'
ok "JSON 400 body → no retry"        '! create_should_retry_cold 400 "{\"error\":\"invalid template\"}"'
ok "503 WITH json body → no retry"   '! create_should_retry_cold 503 "{\"error\":\"boom\"}"'
# Other non-cold empties → no retry.
ok "empty 404 → no retry"            '! create_should_retry_cold 404 ""'

echo "── header parsers ──"
# The FINAL status line wins over an interim 1xx preface (Expect: 100-continue).
H_100=$(hdr 'HTTP/1.1 100 Continue\r\n\r\nHTTP/1.1 503 Service Unavailable\r\nretry-after: 2\r\nserver: cloudflare\r\n')
H_103=$(hdr 'HTTP/2 103 \r\nlink: </x>; rel=preload\r\nHTTP/2 503 \r\nretry-after: 2\r\nserver: cloudflare\r\n')
H_CF=$(hdr 'HTTP/2 503 \r\ndate: Thu, 16 Jul 2026 19:01:25 GMT\r\ncontent-length: 0\r\nretry-after: 2\r\nserver: cloudflare\r\ncf-ray: a1c3415e0ed2368b-FRA\r\n')
H_11=$(hdr 'HTTP/1.1 503 Service Unavailable\r\nRetry-After: 5\r\nServer: nginx\r\n')
H_NORA=$(hdr 'HTTP/2 503 \r\nserver: cloudflare\r\n')
H_HOSTILE=$(hdr 'HTTP/2 503 \r\nretry-after: 900\r\nserver: cloudflare\r\n')
# A 20+digit all-numeric Retry-After passes ^[0-9]+$ but overflows signed-64-bit
# shell arithmetic — the pre-fix `[ "$raw" -gt 10 ]` errored ("integer
# expression expected"), skipped the cap, and slept the giant value until CI
# timed out. Must clamp to the 10s cap by LENGTH before any numeric compare.
H_OVERFLOW=$(hdr 'HTTP/2 503 \r\nretry-after: 99999999999999999999\r\nserver: cloudflare\r\n')
H_ZEROPAD=$(hdr 'HTTP/2 503 \r\nretry-after: 0000000005\r\nserver: cloudflare\r\n')
H_DATE=$(hdr 'HTTP/2 503 \r\nretry-after: Wed, 21 Oct 2026 07:28:00 GMT\r\nserver: cloudflare\r\n')
ok "status = FINAL line past 100-Continue" '[ "$(create_parse_status "$H_100")" = "503" ]'
ok "status = FINAL line past 103 Early-Hints" '[ "$(create_parse_status "$H_103")" = "503" ]'
ok "status from HTTP/1.1 line"       '[ "$(create_parse_status "$H_11")" = "503" ]'
ok "retry-after parsed (2)"          '[ "$(create_parse_retry_after "$H_CF")" = "2" ]'
ok "retry-after case-insensitive (5)" '[ "$(create_parse_retry_after "$H_11")" = "5" ]'
ok "retry-after default when absent"  '[ "$(create_parse_retry_after "$H_NORA")" = "2" ]'
ok "hostile retry-after capped at 10" '[ "$(create_parse_retry_after "$H_HOSTILE")" = "10" ]'
ok "over-int64 (20-digit) retry-after capped at 10 (no overflow)" '[ "$(create_parse_retry_after "$H_OVERFLOW")" = "10" ]'
ok "zero-padded 0000000005 → 5 (length-strip, not clamp)" '[ "$(create_parse_retry_after "$H_ZEROPAD")" = "5" ]'
ok "HTTP-date retry-after → default 2" '[ "$(create_parse_retry_after "$H_DATE")" = "2" ]'
ok "server=cloudflare parsed"         '[ "$(create_parse_server "$H_CF")" = "cloudflare" ]'
ok "server=nginx parsed"              '[ "$(create_parse_server "$H_11")" = "nginx" ]'

echo "──"
echo "totals: pass=$pass fail=$fail"
if [ "$fail" -ne 0 ]; then echo "❌ workspace-create retry unit FAILED"; exit 1; fi
echo "✅ workspace-create retry unit PASSED"
