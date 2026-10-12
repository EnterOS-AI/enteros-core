#!/usr/bin/env bash
# Shared fail direction for a queued A2A response that cannot be polled.
# The caller supplies infra_skip_advisory and fail so lane policy and result
# counters remain owned by the lifecycle driver.

a2a_queue_id_from_response() {
  python3 -c '
import json
import re
import sys

try:
    document = json.load(sys.stdin)
except (json.JSONDecodeError, UnicodeDecodeError):
    raise SystemExit(1)

if not isinstance(document, dict):
    raise SystemExit(1)
queue_id = document.get("queue_id")
if not isinstance(queue_id, str):
    raise SystemExit(1)
queue_id = queue_id.strip()
if not re.fullmatch(r"[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}", queue_id):
    raise SystemExit(1)
print(queue_id.lower())
'
}

require_a2a_queue_id() {
  local queue_id="${1:-}"
  if [[ "$queue_id" =~ ^[[:xdigit:]]{8}-[[:xdigit:]]{4}-[[:xdigit:]]{4}-[[:xdigit:]]{4}-[[:xdigit:]]{12}$ ]]; then
    return 0
  fi

  infra_skip_advisory \
    "a2a-queued-no-queue-id" \
    "initial POST was queued but returned no pollable queue_id"
  fail "A2A queued response omitted a valid queue_id — refusing to poll an invalid queue-status path"
  return 1
}
