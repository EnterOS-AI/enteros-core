from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]


def test_staging_e2e_workflow_uses_stable_minimax_default() -> None:
    """Keep the consolidated staging journey on the registered MiniMax model."""
    workflow = ROOT / ".gitea/workflows/e2e-staging-saas.yml"
    text = workflow.read_text()

    assert "MiniMax-M2.7-highspeed" not in text
    assert "MiniMax-M2.7" in text

    # These standalone lanes were consolidated into e2e-staging-saas; stale
    # tests must not silently recreate a dependency on deleted workflows.
    assert not (ROOT / ".gitea/workflows/staging-smoke.yml").exists()
    assert not (ROOT / ".gitea/workflows/continuous-synth-e2e.yml").exists()
