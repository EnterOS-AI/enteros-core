#!/usr/bin/env bash
# Offline fail-direction proof for full-SaaS HTTP status capture.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck disable=SC1091
# shellcheck source=lib/http_status_capture.sh
source "$SCRIPT_DIR/lib/http_status_capture.sh"

TMP_CODE=$(mktemp)
trap 'rm -f "$TMP_CODE"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }
pass() { echo "PASS: $*"; }

fake_request() {
  local raw_status="$1" rc="$2"
  printf '%s' "$raw_status"
  return "$rc"
}

assert_capture() {
  local name="$1" raw="$2" source_rc="$3" expected_code="$4"
  local code="unset" request_rc="unset"
  capture_http_status "$TMP_CODE" fake_request "$raw" "$source_rc"
  code="$HTTP_CAPTURE_CODE"
  request_rc="$HTTP_CAPTURE_RC"
  [ "$code" = "$expected_code" ] || fail "$name normalized code=$code, expected $expected_code"
  [ "$request_rc" = "$source_rc" ] || fail "$name rc=$request_rc, expected $source_rc"
  pass "$name -> code=$code rc=$request_rc"
}

assert_capture "expected HTTP 404 with curl --fail rc" 404 22 404
assert_capture "HTTP 401 remains authoritative" 401 22 401
assert_capture "HTTP 500 remains authoritative" 500 22 500
assert_capture "transport failure normalizes once" "" 7 000
assert_capture "polluted multi-code output is rejected" 404000 22 000

http_code_is_exact_removed_route 404 || fail "exact 404 must prove a removed GET route"
for rejected in 200 401 405 500 000 404000; do
  if http_code_is_exact_removed_route "$rejected"; then
    fail "removed-route gate accepted HTTP $rejected"
  fi
done
pass "removed-route gate accepts only exact HTTP 404"

python3 - "$SCRIPT_DIR/test_staging_full_saas.sh" <<'PY'
from pathlib import Path
import re
import sys

script = Path(sys.argv[1]).read_text(encoding="utf-8")
for retired in (
    r"^\s*UP_CODE=\$\(curl",
    r"^\s*DL_CODE=\$\(curl",
    r"^\s*PUT_CODE=\$\(tenant_call",
    r"^\s*SC_CODE=\$\(tenant_call",
):
    if re.search(retired, script, re.MULTILINE):
        raise SystemExit(f"FAIL: unsafe command-substitution status capture remains: {retired}")
if script.count('capture_http_status "') != 4:
    raise SystemExit("FAIL: all four full-SaaS HTTP status sites must use capture_http_status")
if 'if ! http_code_is_exact_removed_route "$SC_CODE"' not in script:
    raise SystemExit("FAIL: shared-context removal is not gated on exact HTTP 404")
if 'head -c 300 "$SC_BODY" | sanitize_http_body' not in script:
    raise SystemExit("FAIL: shared-context failure does not include a redacted body preview")
print("PASS: full-SaaS harness uses safe capture at all four evidenced sites")
PY

echo "All HTTP status capture unit tests passed"
