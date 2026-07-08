#!/usr/bin/env bash
# Offline regression test for lib/chat_history_agent_text.py.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
EXTRACT="$HERE/lib/chat_history_agent_text.py"
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

expect_text "latest agent content wins" \
  '{"messages":[{"role":"user","content":"17*23"},{"role":"agent","content":"old"},{"role":"agent","content":"391"}]}' \
  "391"

expect_text "assistant role accepted" \
  '{"messages":[{"role":"assistant","content":"391"}]}' \
  "391"

expect_text "nested content text accepted" \
  '{"messages":[{"role":"agent","content":{"result":"391"}}]}' \
  "391"

expect_empty "user-only history is empty" \
  '{"messages":[{"role":"user","content":"391"}]}'

expect_empty "invalid json is empty" \
  '{"messages":'

echo "=== chat_history_agent_text.py unit: $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]
