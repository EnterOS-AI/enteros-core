"""Shared review-governance policy for preventive and detective merge gates.

The merge queue and post-merge audit must agree on both the recognised reviewer
roster and the minimum approval floor. Keep the checked-in defaults here; each
consumer may accept an explicit environment override, but no workflow carries a
second copy of the defaults.
"""

from __future__ import annotations

import re


DEFAULT_REVIEWER_SET = frozenset(
    {
        "molecule-code-reviewer",
        "core-qa",
        "core-security",
        "core-lead",
        "cp-lead",
        "dev-lead",
        "app-lead",
        "sdk-lead",
        "infra-lead",
        "release-manager",
        "pm",
        "agent-pm",
        "hongming",
        "cui",
        "hongming-personal",
        "hongming-pc2",
        "claude-ceo-assistant",
        "hongming-ceo-delegated",
    }
)
DEFAULT_REQUIRED_APPROVALS = 1

_LOGIN_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_.-]*$")
_POSITIVE_INT_RE = re.compile(r"^[0-9]+$")


def recognized_reviewers(raw: str | None = None) -> set[str]:
    """Return the configured reviewer roster, rejecting ambiguous overrides."""

    if raw is None:
        return set(DEFAULT_REVIEWER_SET)
    parts = raw.split(",")
    if not parts or any(not part.strip() for part in parts):
        raise ValueError("REVIEWER_SET must be a non-empty comma-separated login list")
    reviewers = {part.strip() for part in parts}
    invalid = sorted(login for login in reviewers if not _LOGIN_RE.fullmatch(login))
    if invalid:
        raise ValueError(f"REVIEWER_SET contains invalid login(s): {','.join(invalid)}")
    return reviewers


def required_approvals(raw: str | None = None) -> int:
    """Return the positive approval floor, using the shared default when unset."""

    if raw is None:
        return DEFAULT_REQUIRED_APPROVALS
    if not _POSITIVE_INT_RE.fullmatch(raw):
        raise ValueError("REQUIRED_APPROVALS must be a positive integer")
    value = int(raw)
    if value < 1:
        raise ValueError("REQUIRED_APPROVALS must be a positive integer")
    return value
