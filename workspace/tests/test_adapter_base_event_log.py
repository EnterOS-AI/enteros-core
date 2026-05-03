"""BaseAdapter.event_log wiring (#119 PR-3b).

Pins the additive event_log property contract: every adapter inherits a
no-op DisabledEventLog by default, and main.py overrides via the setter
from the observability.event_log config block. Catches accidental
contract drift — e.g. removing the setter, swapping the default to a
non-Disabled backend that allocates storage at import time, or breaking
per-instance isolation by stashing on the class.
"""

import sys
from pathlib import Path

import pytest

WORKSPACE_DIR = Path(__file__).parent.parent
if str(WORKSPACE_DIR) not in sys.path:
    sys.path.insert(0, str(WORKSPACE_DIR))

from a2a.server.agent_execution import AgentExecutor  # noqa: E402

from adapter_base import AdapterConfig, BaseAdapter  # noqa: E402
from event_log import DisabledEventLog, InMemoryEventLog, create_event_log  # noqa: E402


class _StubAdapter(BaseAdapter):
    """Minimal concrete adapter — implements only the abstract surface."""

    @staticmethod
    def name() -> str:
        return "stub"

    @staticmethod
    def display_name() -> str:
        return "Stub"

    @staticmethod
    def description() -> str:
        return "test stub"

    async def setup(self, config: AdapterConfig) -> None:
        return None

    async def create_executor(self, config: AdapterConfig) -> AgentExecutor:  # pragma: no cover
        raise NotImplementedError


def test_default_event_log_is_disabled():
    adapter = _StubAdapter()
    assert isinstance(adapter.event_log, DisabledEventLog)


def test_default_event_log_append_is_noop():
    """DisabledEventLog returns a synthetic Event so callers that want
    the id don't crash, but persists nothing — query is always []."""
    adapter = _StubAdapter()
    event = adapter.event_log.append(kind="boot", payload={"phase": "init"})
    assert event.kind == "boot"
    assert event.payload == {"phase": "init"}
    assert adapter.event_log.query() == []


def test_default_event_log_is_shared_singleton():
    """The default DisabledEventLog is module-shared because the no-op
    has no per-instance state. Allocating one per adapter would be
    wasteful and obscure the intent that 'unset' == 'disabled'."""
    a, b = _StubAdapter(), _StubAdapter()
    assert a.event_log is b.event_log


def test_setter_overrides_default():
    adapter = _StubAdapter()
    backend = InMemoryEventLog(ttl_seconds=60, max_entries=100)
    adapter.event_log = backend
    assert adapter.event_log is backend


def test_setter_provides_per_adapter_isolation():
    """Setting on one adapter must not affect another — pins that the
    backend is stored as an instance attribute (not on the class)."""
    a, b = _StubAdapter(), _StubAdapter()
    a.event_log = InMemoryEventLog()
    assert isinstance(a.event_log, InMemoryEventLog)
    assert isinstance(b.event_log, DisabledEventLog)
    assert a.event_log is not b.event_log


def test_setter_round_trip_with_factory():
    """Mirrors the main.py wiring: backend comes from create_event_log
    fed by the EventLogConfig dataclass."""
    adapter = _StubAdapter()
    adapter.event_log = create_event_log(backend="memory", ttl_seconds=300, max_entries=50)
    assert isinstance(adapter.event_log, InMemoryEventLog)

    event = adapter.event_log.append(kind="tool_call", payload={"name": "Bash"})
    assert event.id > 0
    events = adapter.event_log.query()
    assert len(events) == 1
    assert events[0].kind == "tool_call"


def test_setter_can_swap_to_disabled():
    """Operator who wires memory backend at boot, then opts out at
    runtime via a future toggle, should be able to swap. Pins that the
    setter accepts any EventLogBackend, not just InMemoryEventLog."""
    adapter = _StubAdapter()
    adapter.event_log = InMemoryEventLog()
    adapter.event_log = create_event_log(backend="disabled")
    assert isinstance(adapter.event_log, DisabledEventLog)


def test_event_log_falsy_falls_back_to_default():
    """getattr-or-default pattern: if a subclass nulls _event_log, the
    property hands back the shared DisabledEventLog rather than None."""
    adapter = _StubAdapter()
    adapter._event_log = None  # pretend a subclass cleared it
    assert isinstance(adapter.event_log, DisabledEventLog)


def test_signature_snapshot_unchanged_by_property():
    """Defense-in-depth: the signature snapshot helper walks vars(cls)
    for callables only. A @property is not callable, so adding event_log
    must not bloat adapter_base_signature.json. If this test starts
    failing, the snapshot helper changed and the additive-property
    assumption no longer holds — re-evaluate the wiring strategy."""
    from tests._signature_snapshot import build_class_signature_record

    record = build_class_signature_record(BaseAdapter)
    method_names = {m["name"] for m in record["methods"]}
    assert "event_log" not in method_names, (
        "event_log appeared in the BaseAdapter signature snapshot — the "
        "snapshot helper now captures properties. Update "
        "adapter_base_signature.json to reflect the new shape."
    )
