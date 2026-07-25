from __future__ import annotations

import json
import os
from pathlib import Path
import subprocess

import yaml


ROOT = Path(__file__).resolve().parents[3]
FILTER = ROOT / "tests/e2e/lib/filter_go_e2e_run_orgs.py"
TEARDOWN = ROOT / "tests/e2e/lib/go_e2e_run_teardown.sh"
HELPER = "bash tests/e2e/lib/go_e2e_run_teardown.sh"
STEP_NAME = "Teardown safety net (runs on timeout/cancel/failure)"

# The e2e-staging-saas / e2e-workspace-lifecycle-staging TEST lanes were retired
# (their journeys now run per-PR on the isolated ephemeral-CP gate, which tears
# down its own dind topology and never leaks real staging orgs — so it needs no
# go-e2e-run teardown net). Only the surviving staging workflows remain here.
EXPECTED_STEPS = {
    ("runtime-default-flip-gate.yml", "flip-gate"): "mcp",
    ("staging-tenant-cd.yml", "e2e-smoke"): "life mcp",
}


def _run_filter(roster: dict, run_id: str, tags: str) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        [
            "python3",
            str(FILTER),
            "--run-id",
            run_id,
            "--tags",
            tags,
        ],
        input=json.dumps(roster),
        text=True,
        capture_output=True,
        check=False,
    )


def test_filter_matches_only_exact_run_and_tag_scoped_slugs() -> None:
    roster = {
        "orgs": [
            {"slug": "e2e-req-5679-00a1fe", "id": "org-exact", "instance_status": "running"},
            {"slug": "e2e-mcp-5679-00a2fe", "id": "org-other-tag", "instance_status": "running"},
            {"slug": "e2e-req-2605679-00a3fe", "id": "org-run-substring", "instance_status": "running"},
            {"slug": "prefix-e2e-req-5679-00a4fe", "id": "org-prefix", "instance_status": "running"},
            {"slug": "e2e-req-5679-00a5fe-suffix", "id": "org-suffix", "instance_status": "running"},
            {"slug": "e2e-req-5679-00a6fe", "id": "org-purged", "instance_status": "purged"},
        ]
    }

    result = _run_filter(roster, "5679", "req")

    assert result.returncode == 0, result.stderr
    assert result.stdout == "e2e-req-5679-00a1fe\torg-exact\n"


def test_filter_accepts_the_jobs_explicit_multi_tag_scope() -> None:
    roster = {
        "orgs": [
            {"slug": "e2e-life-42-12ab45", "id": "life-id", "instance_status": "running"},
            {"slug": "e2e-mcp-42-54cd21", "id": "mcp-id", "instance_status": "failed"},
            {"slug": "e2e-cncrg-42-11ef11", "id": "other-id", "instance_status": "running"},
        ]
    }

    result = _run_filter(roster, "42", "life mcp")

    assert result.returncode == 0, result.stderr
    assert result.stdout.splitlines() == [
        "e2e-life-42-12ab45\tlife-id",
        "e2e-mcp-42-54cd21\tmcp-id",
    ]


def test_filter_fails_closed_on_invalid_scope_or_conflicting_identity() -> None:
    roster = {
        "orgs": [
            {"slug": "e2e-req-7-00aa01", "id": "org-a", "instance_status": "running"},
            {"slug": "e2e-req-7-00aa01", "id": "org-b", "instance_status": "running"},
        ]
    }

    for run_id, tags in (("7;rm", "req"), ("7", "req *"), ("7", "")):
        result = _run_filter(roster, run_id, tags)
        assert result.returncode != 0
        assert result.stdout == ""

    conflict = _run_filter(roster, "7", "req")
    assert conflict.returncode != 0
    assert conflict.stdout == ""


def _run_teardown(**overrides: str) -> subprocess.CompletedProcess[str]:
    env = os.environ.copy()
    env.update(
        {
            "CP_BASE_URL": "https://staging-api.moleculesai.app",
            "E2E_INFRA_BACKEND": "local-docker",
            "GITHUB_RUN_ID": "5679",
            "TEARDOWN_TAGS": "req",
            "ADMIN_TOKEN": "unit-test-token-must-not-be-used",
        }
    )
    env.update(overrides)
    return subprocess.run(
        ["bash", str(TEARDOWN)],
        text=True,
        capture_output=True,
        env=env,
        check=False,
    )


def test_teardown_rejects_non_staging_origin_before_bearer_use() -> None:
    result = _run_teardown(CP_BASE_URL="https://api.moleculesai.app")
    assert result.returncode == 0
    assert "rejected its staging target/backend before bearer use" in result.stdout
    assert "Safety-net teardown:" not in result.stdout


def test_teardown_rejects_invalid_run_scope_before_roster_discovery() -> None:
    result = _run_teardown(GITHUB_RUN_ID="5679-other")
    assert result.returncode == 0
    assert "GITHUB_RUN_ID must contain digits only" in result.stdout
    assert "Safety-net teardown:" not in result.stdout


def test_all_go_teardown_workflows_call_one_shared_helper() -> None:
    found: dict[tuple[str, str], str] = {}
    for workflow_name in {name for name, _ in EXPECTED_STEPS}:
        path = ROOT / ".gitea/workflows" / workflow_name
        workflow = yaml.safe_load(path.read_text())
        for job_name, job in workflow["jobs"].items():
            for step in job.get("steps", []):
                if step.get("name") != STEP_NAME:
                    continue
                assert step.get("run", "").strip() == HELPER
                assert "always()" in step.get("if", "")
                assert "pull_request" in step.get("if", "")
                assert step.get("env", {}).get("ADMIN_TOKEN") == "${{ env.CP_ADMIN_API_TOKEN }}"
                found[(workflow_name, job_name)] = step.get("env", {}).get(
                    "TEARDOWN_TAGS", ""
                )

    assert found == EXPECTED_STEPS


def test_staging_runbook_names_the_go_cleanup_ssot_and_delayed_backstop() -> None:
    runbook = (ROOT / "tests/e2e/STAGING_SAAS_E2E.md").read_text()
    assert "tests/e2e/lib/go_e2e_run_teardown.sh" in runbook
    assert "e2e-<tag>-<run-id>-<six-hex>" in runbook
    assert "90-minute age floor" in runbook
