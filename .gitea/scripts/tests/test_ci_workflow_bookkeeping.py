from pathlib import Path

import yaml


ROOT = Path(__file__).resolve().parents[2]


def load_workflow(name: str) -> dict:
    with (ROOT / "workflows" / name).open() as f:
        return yaml.safe_load(f)


def test_all_required_uses_dedicated_meta_runner_lane():
    workflow = load_workflow("ci.yml")
    all_required = workflow["jobs"]["all-required"]

    assert all_required["runs-on"] == "ci-meta"
    assert "needs" not in all_required


def test_all_required_reuses_path_filter_before_polling():
    workflow = load_workflow("ci.yml")
    all_required = workflow["jobs"]["all-required"]
    rendered = str(all_required)

    assert "--profile ci" in rendered
    assert ".gitea/scripts/detect-changes.py" in rendered
    assert "REQUIRE_PLATFORM" in rendered
    assert "REQUIRE_CANVAS" in rendered
    assert "REQUIRE_SCRIPTS" in rendered
