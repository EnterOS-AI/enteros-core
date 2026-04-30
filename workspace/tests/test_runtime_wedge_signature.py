"""runtime_wedge public-API signature snapshot — drift gate.

``BaseAdapter`` docstring explicitly tells adapter authors to call
``runtime_wedge.mark_wedged(reason)`` / ``clear_wedge()`` when their
SDK hits a non-recoverable error class — the heartbeat thread reads
``is_wedged()`` / ``wedge_reason()`` to flip the workspace to
``degraded`` and surface the cause to the canvas.

That's a public adapter-facing API. Renaming any of the four
functions silently breaks every adapter that calls them: the import
still resolves the module, the missing attribute raises
``AttributeError`` only when the adapter actually hits its first
SDK error — long after the rename merges.

Same drift class as the BaseAdapter signature snapshot (#2378, #2380)
and skill_loader gate (#2381), applied to the module-level
function surface.
"""

import sys
from pathlib import Path

import pytest

WORKSPACE_DIR = Path(__file__).parent.parent
if str(WORKSPACE_DIR) not in sys.path:
    sys.path.insert(0, str(WORKSPACE_DIR))

from tests._signature_snapshot import (  # noqa: E402
    build_module_functions_record,
    compare_against_snapshot,
)

SNAPSHOT_PATH = Path(__file__).parent / "snapshots" / "runtime_wedge_signature.json"


def _build_full_snapshot() -> dict:
    """Pin only the four contract functions adapters call. Other module-
    level helpers (``reset_for_test``, internal state) intentionally
    aren't part of the snapshot — adapters MUST NOT depend on them.
    """
    import runtime_wedge

    return build_module_functions_record(
        runtime_wedge,
        function_names=[
            "is_wedged",
            "wedge_reason",
            "mark_wedged",
            "clear_wedge",
        ],
    )


def test_runtime_wedge_signature_matches_snapshot():
    compare_against_snapshot(_build_full_snapshot(), SNAPSHOT_PATH)


def test_snapshot_has_required_functions():
    """Defense-in-depth: even if both source and snapshot are updated
    together, removing any of the four adapter-facing functions
    requires explicit edit here. The required set is the documented
    public contract — see ``BaseAdapter`` docstring.
    """
    if not SNAPSHOT_PATH.exists():
        pytest.skip(f"{SNAPSHOT_PATH.name} not generated yet")

    import json
    snapshot = json.loads(SNAPSHOT_PATH.read_text())
    fn_names = {f["name"] for f in snapshot["functions"]}

    required = {
        "is_wedged",  # platform-side heartbeat reads this
        "wedge_reason",  # surfaces the why on the canvas
        "mark_wedged",  # adapters call this on non-recoverable errors
        "clear_wedge",  # adapters call this on auto-recovery
    }
    missing = required - fn_names
    if missing:
        pytest.fail(
            f"runtime_wedge snapshot is missing required functions: {sorted(missing)}.\n"
            "Either restore them on runtime_wedge.py, OR coordinate adapter "
            "updates AND remove the entry from `required` in this test "
            "with a justification."
        )

    for fn in snapshot["functions"]:
        if fn.get("missing"):
            pytest.fail(
                f"runtime_wedge.{fn['name']} resolved as a non-function — "
                "either it was replaced by a different kind of attribute "
                "(class? module-level alias?) which adapters' direct call "
                "would break, OR it was removed entirely."
            )
