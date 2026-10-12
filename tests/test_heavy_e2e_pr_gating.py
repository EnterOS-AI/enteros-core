from pathlib import Path

import yaml


ROOT = Path(__file__).resolve().parents[1]


def workflow_on(path: Path):
    doc = yaml.safe_load(path.read_text())
    return doc.get("on") or doc.get(True)


def test_local_chat_e2e_is_a_real_fail_closed_pr_lane():
    path = ROOT / ".gitea/workflows/e2e-chat.yml"
    doc = yaml.safe_load(path.read_text())
    events = workflow_on(path)

    assert set(events) == {"push", "pull_request", "workflow_dispatch"}
    assert set(events["pull_request"]["branches"]) == {"main", "staging"}
    assert "detect-changes" not in doc["jobs"]
    assert all(
        "No-op pass" not in step.get("name", "")
        for job in doc["jobs"].values()
        for step in job.get("steps", [])
    )


def test_staging_canvas_e2e_keeps_its_explicit_heavy_lane_gate():
    path = ROOT / ".gitea/workflows/e2e-staging-canvas.yml"
    text = path.read_text()
    events = workflow_on(path)

    assert set(events) == {"push", "pull_request", "workflow_dispatch"}
    assert "merge-queue" in text
    assert "/issues/${{ github.event.pull_request.number }}/labels" in text
    assert "PR is not in merge-queue" in text
