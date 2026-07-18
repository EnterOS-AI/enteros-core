"""Keep the marketplace RFC canonical in the private internal repository."""

from pathlib import Path


ROOT = Path(__file__).resolve().parents[3]
STUB = ROOT / "docs/design/rfc-marketplace-delivery.md"
CANONICAL_URL = (
    "https://git.moleculesai.app/molecule-ai/internal/src/branch/main/"
    "rfcs/marketplace-delivery.md"
)
MIGRATION_ISSUE_URL = (
    "https://git.moleculesai.app/molecule-ai/internal/issues/1075"
)


def test_marketplace_rfc_is_a_bounded_internal_ssot_stub() -> None:
    text = STUB.read_text()

    assert CANONICAL_URL in text
    assert MIGRATION_ISSUE_URL in text
    assert len(text.splitlines()) <= 60

    stale_duplicate_markers = (
        "resolveTemplateAssets",
        "MISSING_ASSETS",
        "28f97a7f",
        "CP #828",
    )
    assert not [marker for marker in stale_duplicate_markers if marker in text]
