"""Tests for ``card_helpers.enrich_card_skills`` — the defensive swap that
replaces ``AgentCard.skills`` with rich metadata from the adapter's
loaded skills, falling back to the static stubs on shape mismatch.

The whole point of the helper (vs inline in main.py) is that a future
adapter author who returns a non-standard ``loaded_skills`` shape
should NOT silently downgrade their workspace boot to not-configured —
``setup()`` succeeded, the agent works, only the card's skill metadata
enrichment is degraded.
"""
from __future__ import annotations

import sys
from pathlib import Path

WORKSPACE_DIR = Path(__file__).resolve().parents[1]
if str(WORKSPACE_DIR) not in sys.path:
    sys.path.insert(0, str(WORKSPACE_DIR))

from a2a.types import AgentCard, AgentCapabilities, AgentInterface, AgentSkill

from card_helpers import enrich_card_skills


def _make_card(static_skill_names):
    return AgentCard(
        name="test-agent",
        description="test",
        version="0.0.0",
        supported_interfaces=[
            AgentInterface(protocol_binding="https://a2a.g/v1", url="http://x:8000")
        ],
        capabilities=AgentCapabilities(streaming=True, push_notifications=False),
        skills=[
            AgentSkill(id=n, name=n, description=n, tags=[], examples=[])
            for n in static_skill_names
        ],
        default_input_modes=["text/plain"],
        default_output_modes=["text/plain"],
    )


class _SkillMetadata:
    """Mimics the adapter-side Skill.metadata shape."""
    def __init__(self, id, name, description, tags, examples):
        self.id = id
        self.name = name
        self.description = description
        self.tags = tags
        self.examples = examples


class _Skill:
    def __init__(self, **kwargs):
        self.metadata = _SkillMetadata(**kwargs)


def test_returns_false_on_none():
    """No loaded_skills → caller didn't load any → no swap, no log spam."""
    card = _make_card(["a", "b"])
    assert enrich_card_skills(card, None) is False
    # Static stubs preserved.
    assert [s.id for s in card.skills] == ["a", "b"]


def test_returns_false_on_empty_list():
    """Empty list → same treatment as None: nothing to enrich."""
    card = _make_card(["a"])
    assert enrich_card_skills(card, []) is False
    assert [s.id for s in card.skills] == ["a"]


def test_swaps_in_rich_metadata_on_canonical_shape():
    """The happy path: adapter returns Skill objects with the canonical
    .metadata shape, card gets the richer descriptions/tags/examples."""
    card = _make_card(["search"])  # static stub
    rich = [
        _Skill(
            id="search",
            name="Web Search",
            description="Search the web for the user's question",
            tags=["web", "io"],
            examples=["who won the world cup in 2022?"],
        ),
    ]
    assert enrich_card_skills(card, rich) is True
    assert len(card.skills) == 1
    assert card.skills[0].id == "search"
    assert card.skills[0].name == "Web Search"
    assert "web" in card.skills[0].tags
    assert card.skills[0].examples == ["who won the world cup in 2022?"]


def test_returns_false_and_keeps_stubs_when_metadata_attr_missing(capsys):
    """Defensive: a future adapter that returns objects without
    ``.metadata`` would otherwise raise AttributeError and propagate to
    main.py's outer except — silently degrading an OK boot to
    not-configured. Helper logs + returns False instead, static stubs
    stay in place.

    This is the reason the helper exists at all; without it the
    inline swap in main.py at PR #2756 was a coupling between adapter
    discipline and tenant-facing readiness."""
    card = _make_card(["a"])

    class NoMetadata:
        id = "x"  # has id but no .metadata.id (the canonical path)

    assert enrich_card_skills(card, [NoMetadata()]) is False
    # Static stub preserved.
    assert [s.id for s in card.skills] == ["a"]
    # Operator gets a log line.
    captured = capsys.readouterr()
    assert "skill metadata enrichment failed" in captured.out


def test_returns_false_when_metadata_is_partial(capsys):
    """Partial shape — has .metadata but the .metadata object lacks one
    of the canonical attrs (here: ``examples``). The list comprehension
    raises AttributeError on ``skill.metadata.examples`` access, which
    the helper swallows. (In production, a2a.types.AgentSkill is a
    Pydantic model that ALSO raises on missing required fields — both
    failure modes route through the same except branch.)"""
    card = _make_card(["a"])

    class PartialMeta:
        def __init__(self):
            self.id = "x"
            self.name = "x"
            self.description = "x"
            self.tags = []
            # examples missing

    class PartialSkill:
        def __init__(self):
            self.metadata = PartialMeta()

    result = enrich_card_skills(card, [PartialSkill()])
    assert result is False
    assert [s.id for s in card.skills] == ["a"]
    captured = capsys.readouterr()
    assert "skill metadata enrichment failed" in captured.out


def test_failure_is_atomic_no_partial_swap(capsys):
    """If the second skill is malformed, the FIRST skill's swap must NOT
    leak into card.skills. We use a list-comprehension which builds the
    full list before assignment; verify that property holds.

    Without this property, a misbehaving adapter could half-corrupt the
    card — operators would see "1 skill listed" when 3 were declared,
    no log line if the inline swap was partial."""
    card = _make_card(["a", "b"])

    valid = _Skill(id="x", name="x", description="x", tags=[], examples=[])

    class BadSkill:
        # No .metadata at all.
        pass

    assert enrich_card_skills(card, [valid, BadSkill()]) is False
    # Original two static stubs intact — card.skills was never reassigned.
    assert [s.id for s in card.skills] == ["a", "b"]
