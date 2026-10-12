"""Production consumes completed staging evidence, never an arbitrary green run."""

from copy import deepcopy
import json
import os
from pathlib import Path
import subprocess
import sys

import pytest
import yaml

ROOT = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(ROOT / ".gitea/scripts"))
import staging_release as release

SHA = "a" * 40
DIGEST = "sha256:" + "b" * 64
REPO = "molecule-ai/molecule-core"


@pytest.fixture
def evidence():
    pin = {"template_name": "molecule-tenant", "region": "global", "git_sha": SHA,
           "image_digest": DIGEST, "notes":
           f"[ci-pin-provenance v1 repo={REPO} wf=staging-tenant-cd run=123 job=advance-pin sha={SHA}]"}
    run = {"id": 123, "head_sha": SHA, "head_branch": "main", "event": "push",
           "path": "staging-tenant-cd.yml@refs/heads/main", "status": "completed", "conclusion": "success"}
    jobs = [{"name": f"{name} (description)", "status": "completed", "conclusion": "success",
             "started_at": "2026-10-11T12:00:00Z"} for name in sorted(release.REQUIRED_JOBS)]
    return pin, run, jobs


def test_exact_staging_candidate_is_qualified(evidence):
    assert release.qualify(*evidence, REPO) == {
        "sha": SHA, "sha7": SHA[:7], "tag": "staging-" + SHA[:7],
        "digest": DIGEST, "staging_run": "123",
    }


@pytest.mark.parametrize("field,value", [
    ("conclusion", "failure"), ("conclusion", "skipped"), ("conclusion", "cancelled"),
    ("status", "in_progress"), ("id", 124), ("head_branch", "feature"),
    ("path", "publish-workspace-server-image.yml@refs/heads/main"),
    ("path", "staging-tenant-cd.yml@refs/heads/feature"), ("head_sha", "c" * 40),
    ("event", "pull_request"),
])
def test_unqualified_run_is_rejected(evidence, field, value):
    pin, run, jobs = evidence
    run[field] = value
    with pytest.raises((release.ReleaseError, release.provenance.StampError)):
        release.qualify(pin, run, jobs, REPO)


@pytest.mark.parametrize("field,value", [
    ("git_sha", "a" * 7), ("git_sha", "c" * 40), ("image_digest", "latest"),
    ("notes", "manual promote"),
    ("notes", f"[ci-pin-provenance v1 repo=another/repo run=123 sha={SHA}]"),
])
def test_unbound_pin_is_rejected(evidence, field, value):
    pin, run, jobs = evidence
    pin[field] = value
    with pytest.raises((release.ReleaseError, release.provenance.StampError)):
        release.qualify(pin, run, jobs, REPO)


@pytest.mark.parametrize("name", sorted(release.REQUIRED_JOBS))
@pytest.mark.parametrize("state", ["failure", "skipped", "pending", "missing"])
def test_every_validation_job_must_succeed(evidence, name, state):
    pin, run, jobs = evidence
    for job in list(jobs):
        if job["name"].startswith(name + " ("):
            if state == "missing":
                jobs.remove(job)
            else:
                job["conclusion"] = state
    with pytest.raises(release.ReleaseError, match=name):
        release.qualify(pin, run, jobs, REPO)


def test_phantom_or_duplicate_jobs_do_not_qualify(evidence):
    pin, run, jobs = evidence
    duplicate = jobs + [deepcopy(jobs[0])]
    with pytest.raises(release.ReleaseError, match="ambiguous"):
        release.qualify(pin, run, duplicate, REPO)
    jobs[0]["started_at"] = "1970-01-01T00:00:00Z"
    with pytest.raises(release.ReleaseError, match="execution evidence"):
        release.qualify(pin, run, jobs, REPO)


def test_pin_must_be_unique_and_present(evidence):
    pin = evidence[0]
    assert release.tenant_pin({"pins": [pin]}) == pin
    for document in ({"pins": []}, {"pins": [pin, pin]}, {}):
        with pytest.raises(release.ReleaseError):
            release.tenant_pin(document)


def test_job_pagination_reads_to_an_empty_page(monkeypatch):
    calls = []
    def fake_fetch(path, *_):
        calls.append(path)
        return {"jobs": [{"name": "one"}]} if "page=1" in path else {"jobs": []}
    monkeypatch.setattr(release, "fetch", fake_fetch)
    assert release.read_jobs("123", REPO, "https://example.invalid", "test") == [{"name": "one"}]
    assert len(calls) == 2


@pytest.mark.parametrize("response", [{}, {"jobs": None}, {"jobs": [None]}, []])
def test_malformed_job_response_fails_closed(monkeypatch, response):
    monkeypatch.setattr(release, "fetch", lambda *_: response)
    with pytest.raises(release.ReleaseError):
        release.read_jobs("123", REPO, "https://example.invalid", "test")


def workflow(name):
    return yaml.safe_load((ROOT / ".gitea/workflows" / name).read_text(encoding="utf-8"))


def test_qualification_covers_all_shipped_staging_jobs():
    assert release.REQUIRED_JOBS == set(workflow("staging-tenant-cd.yml")["jobs"])


@pytest.mark.parametrize("frozen,armed,expected", [
    ("true", "true", "0"), ("YES", "true", "0"), ("disabled", "true", "0"),
    ("false", "", "0"), ("false", "false", "0"), ("false", "true", "1"),
])
def test_production_mode_decision_executes_the_real_guard(tmp_path, frozen, armed, expected):
    steps = workflow("promote-prod-tenant-pin.yml")["jobs"]["promote"]["steps"]
    body = next(s["run"] for s in steps if s.get("id") == "mode")
    output = tmp_path / "output"
    result = subprocess.run(["bash", "-e", "-o", "pipefail", "-c", body],
                            env={**os.environ, "PROD_AUTO_DEPLOY_DISABLED": frozen,
                                 "PROMOTE_ARMED": armed, "GITHUB_OUTPUT": str(output)},
                            capture_output=True, text=True)
    assert result.returncode == 0, result.stderr
    assert output.read_text().strip() == f"apply={expected}"


def test_production_writes_are_manual_and_bound_to_the_staging_digest():
    doc = workflow("promote-prod-tenant-pin.yml")
    assert set(doc.get("on", doc.get(True))) == {"workflow_dispatch"}
    job = doc["jobs"]["promote"]
    assert job["if"] == "github.ref == 'refs/heads/main'"
    steps = job["steps"]
    target = next(i for i, s in enumerate(steps) if s.get("id") == "target")
    for sid in ("apply", "roll"):
        idx = next(i for i, s in enumerate(steps) if s.get("id") == sid)
        assert target < idx
        assert steps[idx]["if"] == "${{ steps.mode.outputs.apply == '1' }}"
        assert steps[idx]["env"]["TARGET_DIGEST"] == "${{ steps.target.outputs.digest }}"
    roll = next(s for s in steps if s.get("id") == "roll")
    assert roll["env"]["EXPECTED_BUILD_SHA"] == "${{ steps.target.outputs.sha }}"
    assert "rollback-production-release.sh" in next(s["run"] for s in steps if s.get("name", "").startswith("Restore previous"))


def test_production_inventory_and_live_pin_audits_are_independent_of_code_ci():
    publisher = workflow("publish-workspace-server-image.yml")
    for job in publisher["jobs"].values():
        for step in job["steps"]:
            # An indirect dispatch from the publisher would restore the
            # push-to-production coupling despite a dispatch-only trigger.
            assert "/dispatches" not in step.get("run", "")
    inventory = workflow("redeploy-tenants-on-main.yml")
    assert set(inventory.get("on", inventory.get(True))) == {"workflow_dispatch"}
    body = "\n".join(s.get("run", "") for s in inventory["jobs"]["redeploy-k8s"]["steps"])
    assert "fleet-drift-audit.sh --strict" in body
    assert "advance-staging-tenant-pin.sh" not in body
    steps = workflow("pin-provenance-guard.yml")["jobs"]["guard"]["steps"]
    for step in steps:
        if step.get("name", "").startswith(("Read the production", "Audit —")):
            assert step["if"] == "${{ github.event_name == 'workflow_dispatch' && github.ref == 'refs/heads/main' }}"


@pytest.fixture
def production_sandbox(tmp_path):
    for name in ("prod_cp_endpoints.py", "prod_pin_plan.py"):
        dest = tmp_path / ".gitea/scripts" / name
        dest.parent.mkdir(parents=True, exist_ok=True)
        dest.write_text((ROOT / ".gitea/scripts" / name).read_text(encoding="utf-8"))
    for name, body in {
        "advance-staging-tenant-pin.sh": 'echo "pin|$CP_BASE_URL|$INFISICAL_ENV|$*" >> "$CALLS"\nexit "${PIN_EXIT:-0}"\n',
        "redeploy-tenant-fleet-k8s.sh": 'echo "roll|$EXPECTED_BUILD_SHA|$*" >> "$CALLS"\nexit "${ROLL_EXIT:-0}"\n',
    }.items():
        dest = tmp_path / "scripts/deploy" / name
        dest.parent.mkdir(parents=True, exist_ok=True)
        dest.write_text("#!/usr/bin/env bash\n" + body)
        dest.chmod(0o755)
    (tmp_path / "pins-prod.json").write_text(json.dumps({"pins": [{
        "template_name": "molecule-tenant", "region": "global", "image_digest": DIGEST, "git_sha": SHA,
    }]}))
    env = {**os.environ, "RUNNER_TEMP": str(tmp_path), "CALLS": str(tmp_path / "calls"),
           "DEPLOY_PYTHON3": sys.executable, "TENANT_IMAGE_NAME": "registry.example/tenant",
           "TENANT_IMAGE": "registry.example/tenant", "TARGET_DIGEST": "sha256:" + "c" * 64,
           "TENANT_PIN_GIT_SHA": "d" * 40,
           "PROD_CP_BASE_URLS": "prod=https://prod.example=prod,stage=https://stage.example=staging"}
    return tmp_path, env


def test_apply_reconciles_current_pin_and_never_mutates_staging(production_sandbox):
    path, env = production_sandbox
    (path / "needed.txt").write_text("")  # DB already current; boot secret can still drift.
    step = next(s for s in workflow("promote-prod-tenant-pin.yml")["jobs"]["promote"]["steps"] if s.get("id") == "apply")
    result = subprocess.run(["bash", "-e", "-o", "pipefail", "-c", step["run"]],
                            cwd=path, env=env, capture_output=True, text=True)
    assert result.returncode == 0, result.stderr
    assert (path / "calls").read_text().splitlines() == [
        f"pin|https://prod.example|prod|--image registry.example/tenant@{env['TARGET_DIGEST']} --git-sha {env['TENANT_PIN_GIT_SHA']}"
    ]


@pytest.mark.parametrize("mode", ["validate", "restore", "pin_failure", "roll_failure", "both_fail", "invalid_snapshot"])
def test_failed_release_recovery_uses_previous_production_identity(production_sandbox, mode):
    path, env = production_sandbox
    args = ["bash", str(ROOT / "scripts/deploy/rollback-production-release.sh"), str(path)]
    if mode == "validate":
        args.append("--validate-only")
    if mode in {"pin_failure", "both_fail"}:
        env["PIN_EXIT"] = "1"
    if mode in {"roll_failure", "both_fail"}:
        env["ROLL_EXIT"] = "1"
    if mode == "invalid_snapshot":
        (path / "pins-prod.json").write_text('{"pins": []}')
    result = subprocess.run(args, cwd=path, env=env, capture_output=True, text=True)
    assert (result.returncode == 0) == (mode in {"validate", "restore"}), result.stderr
    calls = (path / "calls").read_text().splitlines() if (path / "calls").exists() else []
    if mode in {"validate", "invalid_snapshot"}:
        assert not calls
    else:
        assert calls[0] == f"pin|https://prod.example|prod|--image registry.example/tenant@{DIGEST} --git-sha {SHA}"
        assert calls[1] == f"roll|{SHA}|--digest {DIGEST} --allow-empty"
        assert len(calls) == 2
