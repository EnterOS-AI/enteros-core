#!/usr/bin/env bash
# Offline regression test for lib/a2a_text_extract.py.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
EXTRACT="$HERE/lib/a2a_text_extract.py"
PASS=0
FAIL=0

expect_text() {
  local desc="$1"
  local json="$2"
  local want="$3"
  local got
  got=$(printf '%s' "$json" | python3 "$EXTRACT")
  if [ "$got" = "$want" ]; then
    echo "PASS: $desc"
    PASS=$((PASS + 1))
  else
    echo "FAIL: $desc - got '$got', want '$want'"
    FAIL=$((FAIL + 1))
  fi
}

expect_empty() {
  local desc="$1"
  local json="$2"
  local got
  got=$(printf '%s' "$json" | python3 "$EXTRACT")
  if [ -z "$got" ]; then
    echo "PASS: $desc"
    PASS=$((PASS + 1))
  else
    echo "FAIL: $desc - got unexpected '$got'"
    FAIL=$((FAIL + 1))
  fi
}

expect_text "result.parts text" \
  '{"result":{"parts":[{"kind":"text","text":"direct"}]}}' \
  "direct"

expect_text "result.status.message.parts text" \
  '{"result":{"status":{"message":{"parts":[{"type":"text","text":"status message"}]}}}}' \
  "status message"

expect_text "result.message.parts text" \
  '{"result":{"message":{"parts":[{"kind":"text","text":"message text"}]}}}' \
  "message text"

expect_text "result.artifacts parts text" \
  '{"result":{"artifacts":[{"parts":[{"kind":"text","text":"artifact text"}]}]}}' \
  "artifact text"

expect_text "queue status response_body text" \
  '{"status":"completed","response_body":{"result":{"artifacts":[{"parts":[{"type":"text","text":"queued artifact"}]}]}}}' \
  "queued artifact"

expect_empty "invalid json is empty" \
  '{"result":'

expect_empty "non-text part is ignored" \
  '{"result":{"parts":[{"kind":"image","text":"hidden"}]}}'

echo "=== a2a_text_extract.py unit: $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]
