from pathlib import Path


ROOT = Path(__file__).resolve().parents[3]


def test_staging_delivery_uses_current_minimax_default() -> None:
    """The deployed template journey must exercise the current platform default."""
    workflow = ROOT / ".gitea/workflows/template-delivery-e2e-staging.yml"
    text = workflow.read_text()

    assert "E2E_EXPECTED_MODEL: minimax/MiniMax-M3" in text
