"""Keep the marketplace RFC canonical in the private internal repository."""

from pathlib import Path


ROOT = Path(__file__).resolve().parents[3]
STUB = ROOT / "docs/design/rfc-marketplace-delivery.md"
CANONICAL_PATH = "Molecule-AI/internal/rfcs/marketplace-delivery.md"
MIGRATION_PR = "internal#1081"
FORBIDDEN_INTERNAL_URL = "https://git.moleculesai.app/molecule-ai/internal"


def test_marketplace_rfc_is_a_bounded_internal_ssot_stub() -> None:
    text = STUB.read_text()

    assert CANONICAL_PATH in text
    assert MIGRATION_PR in text
    # DOCUMENTATION_POLICY.md requires public -> internal references to be
    # path-only; a full private URL leaks internal filenames to crawlers.
    assert FORBIDDEN_INTERNAL_URL not in text
    assert len(text.splitlines()) <= 60

    stale_duplicate_markers = (
        "resolveTemplateAssets",
        "MISSING_ASSETS",
        "28f97a7f",
        "CP #828",
    )
    assert not [marker for marker in stale_duplicate_markers if marker in text]
