"""Keep implemented design archives discoverable without stale active paths."""

from pathlib import Path


ROOT = Path(__file__).resolve().parents[3]


def test_hermes_archive_points_to_the_live_repo_qualified_implementation() -> None:
    for relative in (
        "docs/adapters/archive/hermes-adapter-design.md",
        "docs/adapters/archive/hermes-adapter-plan.md",
    ):
        text = (ROOT / relative).read_text(encoding="utf-8")
        banner = text.splitlines()[0]
        assert "molecule-ai-workspace-template-hermes" in banner
        assert "`adapter.py` + `executor.py`" in banner
        assert "workspace-configs-templates/hermes" not in banner


def test_active_adr_link_follows_the_archived_adr() -> None:
    active = (
        ROOT / "docs/adr/ADR-004-unconditional-concierge-and-one-ensure-flow.md"
    ).read_text(encoding="utf-8")

    assert "archive/ADR-002-local-build-mode-via-registry-presence.md" in active
    assert not (
        ROOT / "docs/adr/ADR-002-local-build-mode-via-registry-presence.md"
    ).exists()
    assert (
        ROOT
        / "docs/adr/archive/ADR-002-local-build-mode-via-registry-presence.md"
    ).is_file()
    archived = (
        ROOT
        / "docs/adr/archive/ADR-002-local-build-mode-via-registry-presence.md"
    ).read_text(encoding="utf-8")
    assert "[quick-start guide](../../quickstart.md)" in archived
