from __future__ import annotations

import sys
from pathlib import Path

import pytest


SCRIPTS = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(SCRIPTS))

from _review_policy import (  # noqa: E402
    DEFAULT_REQUIRED_APPROVALS,
    DEFAULT_REVIEWER_SET,
    recognized_reviewers,
    required_approvals,
)


def test_default_policy_is_the_current_single_approval_roster() -> None:
    assert required_approvals() == DEFAULT_REQUIRED_APPROVALS == 1
    assert recognized_reviewers() == set(DEFAULT_REVIEWER_SET)
    assert {"core-lead", "core-security", "release-manager"} <= DEFAULT_REVIEWER_SET
    assert "outside-reviewer" not in DEFAULT_REVIEWER_SET


def test_explicit_reviewer_override_is_trimmed_and_deduplicated() -> None:
    assert recognized_reviewers("core-lead, core-security,core-lead") == {
        "core-lead",
        "core-security",
    }


@pytest.mark.parametrize("raw", ["", "core-lead,", ",core-lead", "core-lead,,pm", "bad login"])
def test_invalid_reviewer_override_fails_closed(raw: str) -> None:
    with pytest.raises(ValueError, match="REVIEWER_SET"):
        recognized_reviewers(raw)


@pytest.mark.parametrize("raw,want", [("1", 1), ("2", 2), ("01", 1)])
def test_positive_required_approval_override(raw: str, want: int) -> None:
    assert required_approvals(raw) == want


@pytest.mark.parametrize("raw", ["", "0", "-1", "+1", "1.0", "one"])
def test_invalid_required_approval_override_fails_closed(raw: str) -> None:
    with pytest.raises(ValueError, match="REQUIRED_APPROVALS"):
        required_approvals(raw)
