"""Pin the JSON-RPC wire-payload role string format.

The a2a-sdk 1.x migration sweep (PR #2184) over-corrected: it changed
every `"role": "user"` literal in JSON-RPC payload construction to
`"role": "ROLE_USER"` to match the protobuf enum names used by the
1.x native types (a2a.types.Role.ROLE_AGENT / ROLE_USER). That was
correct for in-process Message construction but WRONG for outbound
JSON-RPC wire payloads — the workspace's own a2a-sdk runs requests
through the v0.3 compat adapter (because main.py sets
enable_v0_3_compat=True), and that adapter validates against the
v0.3 Pydantic Role enum (`agent`|`user` lowercase). Sending
"ROLE_USER" makes the receiver reject the request with JSON-RPC
-32600 (Invalid Request), which manifests on the canvas as
"Failed to deliver to <peer>: Invalid Request (code=-32600)".

This test does the cheapest possible drift detection: walk every
workspace/*.py file that constructs a JSON-RPC payload (those grep
positive for `"role":` as a dict key) and assert no
`"ROLE_USER"` / `"ROLE_AGENT"` string literals slip in. The native
Python `Role.ROLE_*` form (with the dot) is fine — the SDK handles
serialization for those.
"""

from __future__ import annotations

import re
from pathlib import Path

WORKSPACE_ROOT = Path(__file__).resolve().parents[1]

# Files under workspace/ that emit JSON-RPC wire payloads (grep-positive
# for the `"role":` dict key). Keep narrow so the test stays fast.
WIRE_PAYLOAD_FILES = [
    "a2a_client.py",
    "a2a_cli.py",
    "heartbeat.py",
    "main.py",
    "builtin_tools/a2a_tools.py",
    "builtin_tools/delegation.py",
]

# String-literal patterns that signal the protobuf-enum-name leak.
# Match either "ROLE_USER" or 'ROLE_USER' but NOT Role.ROLE_USER (the
# legitimate Python type-level reference, no quotes around the enum
# name part).
FORBIDDEN_LITERAL = re.compile(r"""['"]ROLE_(USER|AGENT)['"]""")


def test_no_protobuf_enum_strings_in_jsonrpc_wire_payloads():
    offenders: list[str] = []
    for rel in WIRE_PAYLOAD_FILES:
        path = WORKSPACE_ROOT / rel
        if not path.exists():
            continue
        for lineno, line in enumerate(path.read_text().splitlines(), 1):
            if FORBIDDEN_LITERAL.search(line):
                offenders.append(f"{rel}:{lineno}: {line.strip()}")

    assert not offenders, (
        "JSON-RPC wire payloads must use the v0.3 compat-layer-accepted "
        "lowercase role strings ('user' / 'agent'), not the protobuf "
        "enum names ('ROLE_USER' / 'ROLE_AGENT'). The v0.3 compat "
        "adapter validates against the Pydantic Role enum and rejects "
        "the protobuf names with JSON-RPC -32600 (Invalid Request). "
        "Offending lines:\n  " + "\n  ".join(offenders)
    )
