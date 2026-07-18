"""Keep the marketplace RFC canonical in the private internal repository."""

from pathlib import Path

import yaml


ROOT = Path(__file__).resolve().parents[3]
STUB = ROOT / "docs/design/rfc-marketplace-delivery.md"
OPS_WORKFLOW = ROOT / ".gitea/workflows/test-ops-scripts.yml"
CANONICAL_PATH = "Molecule-AI/internal/rfcs/marketplace-delivery.md"
MIGRATION_PR = "internal#1081"
FORBIDDEN_INTERNAL_URL = "https://git.moleculesai.app/molecule-ai/internal"


def test_marketplace_rfc_is_a_bounded_internal_ssot_stub() -> None:
    text = STUB.read_text()
    normalized = " ".join(text.split())

    assert CANONICAL_PATH in text
    assert MIGRATION_PR in text
    # DOCUMENTATION_POLICY.md requires public -> internal references to be
    # path-only; a full private URL leaks internal filenames to crawlers.
    assert FORBIDDEN_INTERNAL_URL not in text
    assert len(text.splitlines()) <= 60
    assert (
        "required pre-merge `template-delivery-e2e` proves config/prompts "
        "asset delivery"
    ) in normalized
    assert (
        "manual, branch-protection-exempt `template-delivery-e2e-staging.yml` "
        "is the `seo-all` post-deploy proof lane"
        in normalized
    )
    assert "not a required pre-merge proof" in normalized
    assert "does not claim a current staging run" in normalized

    stale_duplicate_markers = (
        "resolveTemplateAssets",
        "MISSING_ASSETS",
        "28f97a7f",
        "CP #828",
    )
    assert not [marker for marker in stale_duplicate_markers if marker in text]


def test_marketplace_stub_changes_trigger_the_required_guard() -> None:
    workflow = yaml.load(OPS_WORKFLOW.read_text(), Loader=yaml.BaseLoader)
    guarded_path = str(STUB.relative_to(ROOT))

    for event in ("push", "pull_request"):
        assert guarded_path in workflow["on"][event]["paths"]
