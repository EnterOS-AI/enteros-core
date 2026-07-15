"""Ratchets for current CI names and tracker references."""

from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]


def read(relative: str) -> str:
    return (ROOT / relative).read_text()


def test_workflow_policy_status_names_are_version_neutral() -> None:
    workflow = read(".gitea/workflows/lint-workflow-yaml.yml")
    mask_register = read(".gitea/scripts/lint_no_coe_on_required.py")

    assert "name: Lint workflow YAML (repository compatibility policy)" in workflow
    assert "name: Lint workflow YAML for repository compatibility policy" in workflow
    assert "Gitea-1.22.6-hostile shapes" not in workflow
    assert "Gitea-1.22.6-hostile shapes" not in mask_register
    assert workflow.count("- 'tests/test_current_state_ci_docs.py'") == 2
    assert (
        "python3 -m pytest tests/test_lint_workflow_yaml.py "
        "tests/test_current_state_ci_docs.py -v"
    ) in workflow


def test_canvas_promotion_mask_uses_same_repo_tracker() -> None:
    workflow = read(".gitea/workflows/publish-canvas-image.yml")
    promote_job = workflow[workflow.index("\n  promote-canvas:\n") :]

    assert "mc#4391" in promote_job
