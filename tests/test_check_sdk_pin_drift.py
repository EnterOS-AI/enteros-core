"""Tests for `.gitea/scripts/check_sdk_pin_drift.py`.

THE POINT OF THIS FILE: a gate that has never been observed to fail is not a
gate. molecule-core has shipped several checks that reported success while
covering nothing (`pass:0 fail:0`, selectors matching an empty set, assertions on
fields that do not exist), so the SDK-pin drift gate ships with its BLOCKING path
exercised explicitly — not merely its happy path.

The disposition matrix under test (see the script docstring):

    pin == SDK main head                          -> OK        exit 0
    lagging, within window                        -> ADVISORY  exit 0
    lagging beyond window, bump PR in flight      -> ADVISORY  exit 0
    lagging beyond window, NO bump PR  (STUCK)    -> BLOCKING  exit 1
    undeterminable (no token / API error)         -> ADVISORY  exit 0

Only the STUCK row is red. That asymmetry is load-bearing: molecule-core's branch
protection is `contexts: ["*"]`, so every context this gate posts blocks EVERY PR
in the repo. A gate that went red on any lag would wedge the repo on a routine
SDK merge.

Run:
    python3 -m pytest tests/test_check_sdk_pin_drift.py -v

No network. All Gitea calls are monkeypatched.
"""
from __future__ import annotations

import sys
from datetime import datetime, timedelta, timezone
from pathlib import Path

import pytest

SCRIPT_DIR = Path(__file__).resolve().parent.parent / ".gitea" / "scripts"
sys.path.insert(0, str(SCRIPT_DIR))

import check_sdk_pin_drift as mod  # noqa: E402

NOW = datetime(2026, 8, 5, 12, 0, 0, tzinfo=timezone.utc)
HEAD = "fb755048e5a6b311e846c85a1b7394a6d0c5fcd2"
PINNED12 = "1426a986bd9f"


def _state(*, lag_hours: float, behind: int = 3, at_head: bool = False) -> mod.PinState:
    if at_head:
        return mod.PinState(HEAD[:12], "v0.0.0-20260805015741-" + HEAD[:12], HEAD, None, None, 0, 0.0)
    return mod.PinState(
        PINNED12,
        "v0.0.0-20260804202522-" + PINNED12,
        HEAD,
        "b68c13f0aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
        (NOW - timedelta(hours=lag_hours)).isoformat(),
        behind,
        lag_hours,
    )


# --------------------------------------------------------------------------
# go.mod parsing
# --------------------------------------------------------------------------

GO_MOD = """module git.moleculesai.app/molecule-ai/molecule-core/workspace-server

go 1.25.0

require (
\tgithub.com/google/uuid v1.6.0
\tgo.moleculesai.app/sdk/gen/go v0.0.0-20260804202522-1426a986bd9f
\tgopkg.in/yaml.v3 v3.0.1
)
"""


def test_parses_pseudo_version_and_sha():
    version, sha12 = mod.parse_go_mod_pin(GO_MOD)
    assert version == "v0.0.0-20260804202522-1426a986bd9f"
    assert sha12 == PINNED12


def test_missing_sdk_require_line_is_a_hard_error():
    """A go.mod with no SDK pin is a defect, not an unreadable signal.

    This repo's registry SSOT *is* the SDK module. Silently passing here would
    reproduce the exact vacuous-gate class this file exists to prevent.
    """
    with pytest.raises(SystemExit) as exc:
        mod.parse_go_mod_pin("module foo\n\ngo 1.25.0\n")
    assert exc.value.code == 1


def test_does_not_match_a_similarly_named_module():
    """Negative control on the regex: a different module must not satisfy it."""
    with pytest.raises(SystemExit):
        mod.parse_go_mod_pin(
            "require go.moleculesai.app/sdk/gen/goxx v0.0.0-20260804202522-1426a986bd9f\n"
        )


# --------------------------------------------------------------------------
# Disposition matrix
# --------------------------------------------------------------------------


def test_pin_at_head_is_ok(monkeypatch):
    monkeypatch.setattr(mod, "open_bump_pr_target", lambda **kw: pytest.fail(
        "must not query PRs when the pin is already at head"))
    blocking, msg = mod.classify(
        _state(lag_hours=0, at_head=True), max_lag_hours=72, gitea_url="x", token="t"
    )
    assert blocking is False
    assert msg.startswith("OK —")


def test_lag_within_window_is_advisory(monkeypatch):
    monkeypatch.setattr(mod, "open_bump_pr_target", lambda **kw: pytest.fail(
        "must not query PRs while inside the window"))
    blocking, msg = mod.classify(
        _state(lag_hours=10), max_lag_hours=72, gitea_url="x", token="t"
    )
    assert blocking is False
    assert "ADVISORY (within window)" in msg


def test_lag_beyond_window_with_bump_pr_in_flight_is_advisory(monkeypatch):
    monkeypatch.setattr(mod, "open_bump_pr_target", lambda **kw: "bump/sdk-204-restore")
    blocking, msg = mod.classify(
        _state(lag_hours=200), max_lag_hours=72, gitea_url="x", token="t"
    )
    assert blocking is False
    assert "propagation in flight" in msg
    assert "bump/sdk-204-restore" in msg


def test_lag_beyond_window_with_no_bump_pr_BLOCKS(monkeypatch):
    """THE GATE FIRING. This is the assertion that makes the gate real."""
    monkeypatch.setattr(mod, "open_bump_pr_target", lambda **kw: None)
    blocking, msg = mod.classify(
        _state(lag_hours=200, behind=7), max_lag_hours=72, gitea_url="x", token="t"
    )
    assert blocking is True, "a stuck pin beyond the window MUST block"
    assert "STUCK" in msg
    # The failure must be actionable — an operator should not have to read this
    # script to learn what to do.
    assert "sdk-pin-bump" in msg
    assert "go generate" in msg
    assert "canonicalRegistrySHA256" in msg
    assert "7 commit(s)" in msg


def test_undeterminable_in_flight_status_fails_soft(monkeypatch):
    def boom(**kw):
        raise mod.StatusUnavailable("open-PR query failed: 502")

    monkeypatch.setattr(mod, "open_bump_pr_target", boom)
    blocking, msg = mod.classify(
        _state(lag_hours=200), max_lag_hours=72, gitea_url="x", token="t"
    )
    assert blocking is False, "an unread signal must never wedge every PR in core"
    assert "fail-soft" in msg


def test_window_boundary_is_inclusive(monkeypatch):
    """Exactly at the window is still advisory; one second past it consults PRs."""
    monkeypatch.setattr(mod, "open_bump_pr_target", lambda **kw: None)
    blocking, _ = mod.classify(
        _state(lag_hours=72.0), max_lag_hours=72, gitea_url="x", token="t"
    )
    assert blocking is False
    blocking, _ = mod.classify(
        _state(lag_hours=72.001), max_lag_hours=72, gitea_url="x", token="t"
    )
    assert blocking is True


# --------------------------------------------------------------------------
# End-to-end main(), still offline
# --------------------------------------------------------------------------


def test_main_returns_1_on_stuck_pin(monkeypatch, tmp_path):
    """Exit code, not just the classifier — CI keys off the process rc."""
    gm = tmp_path / "go.mod"
    gm.write_text(GO_MOD, encoding="utf-8")
    monkeypatch.setattr(
        mod, "resolve_pin_state", lambda *a, **kw: _state(lag_hours=500, behind=12)
    )
    monkeypatch.setattr(mod, "open_bump_pr_target", lambda **kw: None)
    monkeypatch.setenv("SDK_PIN_DRIFT_TOKEN", "tok")
    assert mod.main(["--go-mod", str(gm), "--max-lag-hours", "72"]) == 1


def test_main_returns_0_when_current(monkeypatch, tmp_path):
    gm = tmp_path / "go.mod"
    gm.write_text(GO_MOD, encoding="utf-8")
    monkeypatch.setattr(
        mod, "resolve_pin_state", lambda *a, **kw: _state(lag_hours=0, at_head=True)
    )
    monkeypatch.setenv("SDK_PIN_DRIFT_TOKEN", "tok")
    assert mod.main(["--go-mod", str(gm)]) == 0


def test_main_fails_soft_when_ssot_unreachable(monkeypatch, tmp_path):
    """An unreachable SDK must not turn every core PR red."""
    gm = tmp_path / "go.mod"
    gm.write_text(GO_MOD, encoding="utf-8")

    def boom(*a, **kw):
        raise mod.StatusUnavailable("network unreachable")

    monkeypatch.setattr(mod, "resolve_pin_state", boom)
    assert mod.main(["--go-mod", str(gm)]) == 0


def test_non_json_response_is_not_a_clean_pass(monkeypatch):
    """A Cloudflare HTML error page must raise, never parse into a verdict."""

    class FakeResp:
        def read(self):
            return b"<html>error 1010</html>"

        def __enter__(self):
            return self

        def __exit__(self, *a):
            return False

    monkeypatch.setattr(mod.urllib.request, "urlopen", lambda *a, **kw: FakeResp())
    with pytest.raises(mod.StatusUnavailable):
        mod._get_json("https://example.invalid/x")
