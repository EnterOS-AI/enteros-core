from pathlib import Path

import yaml


ROOT = Path(__file__).resolve().parents[2]


def load_workflow(name: str) -> dict:
    with (ROOT / "workflows" / name).open() as f:
        return yaml.safe_load(f)


def _all_required(workflow: dict) -> dict:
    return workflow["jobs"]["all-required"]


def test_all_required_does_no_docker_work_off_docker_host_pool():
    workflow = load_workflow("ci.yml")
    all_required = _all_required(workflow)

    # The sentinel does NO docker work, so it must NOT occupy the general
    # `docker-host` pool. #3431 moved it OFF the dedicated `ci-meta` lane and
    # co-located it on `ubuntu-latest` (where its `needs:` already run), which
    # frees the meta executor slot immediately. Pin the post-#3431 shape: the
    # invariant is "not on docker-host", realised as `ubuntu-latest`.
    assert all_required["runs-on"] == "ubuntu-latest"
    assert all_required["runs-on"] != "docker-host"


def test_all_required_is_needs_aggregator_not_a_polling_gate():
    """fix/ci-scheduler-fanout (2026-06-01): the sentinel was converted
    from a status-polling loop (which squatted a ci-meta executor slot for
    up to 40 min per PR) into a plain `needs:` aggregator that frees the
    slot immediately. Pin the new shape so a regression to the poller is
    caught.
    """
    workflow = load_workflow("ci.yml")
    all_required = _all_required(workflow)
    rendered = str(all_required)

    # The job MUST aggregate via `needs:` (the slot-freeing design).
    assert "needs" in all_required, "all-required must be a needs: aggregator"

    # It MUST NOT reintroduce the polling loop / per-SHA status fetch that
    # was the throughput sink.
    assert "detect-changes.py" not in rendered, (
        "all-required must not run the detect-changes poller path"
    )
    assert "commits/" not in rendered and "statuses" not in rendered, (
        "all-required must not poll commit statuses (the slot-squat path)"
    )


def test_all_required_does_not_use_if_always():
    """Preserve the legacy 1.22.6 / act_runner v0.6.1 compatibility guard:
    `needs:` + `if: always()` let a non-success need pass the gate
    (feedback_gitea_needs_works_only_ifalways_broken). The sentinel must use
    plain `needs:` WITHOUT a job-level `if: always()`.
    """
    workflow = load_workflow("ci.yml")
    all_required = _all_required(workflow)

    job_if = all_required.get("if")
    assert not (isinstance(job_if, str) and "always()" in job_if), (
        "all-required must not combine needs: with if: always()"
    )


def test_all_required_needs_matches_ci_required_drift_f1_set():
    """The sentinel `needs:` list MUST equal ci-required-drift.py's
    `ci_job_names()` set: every job MINUS the sentinel itself MINUS jobs
    whose `if:` gates on github.event_name/github.ref (event-gated jobs
    skip on PRs and a `needs:` on a skipped job would never let the
    sentinel run). If they diverge, ci-required-drift F1 fires.
    """
    workflow = load_workflow("ci.yml")
    jobs = workflow["jobs"]
    sentinel = "all-required"

    expected = set()
    for key, body in jobs.items():
        if key == sentinel:
            continue
        gate = body.get("if") if isinstance(body, dict) else None
        if isinstance(gate, str) and (
            "github.event_name" in gate or "github.ref" in gate
        ):
            # event-gated → legitimately skips on some triggers; excluded
            # from both `needs:` and the F1 set.
            continue
        expected.add(key)

    needs = jobs[sentinel].get("needs", [])
    if isinstance(needs, str):
        needs = [needs]
    actual = set(needs)

    assert actual == expected, (
        f"all-required needs: {sorted(actual)} != ci_job_names() "
        f"{sorted(expected)} — ci-required-drift F1 would fire"
    )


def test_all_required_needs_reference_real_jobs():
    """F1b guard: every entry in `needs:` must name an existing job."""
    workflow = load_workflow("ci.yml")
    jobs = workflow["jobs"]
    needs = jobs["all-required"].get("needs", [])
    if isinstance(needs, str):
        needs = [needs]
    job_keys = set(jobs)
    for dep in needs:
        assert dep in job_keys, f"all-required needs unknown job {dep!r}"


def test_all_required_step_uses_extracted_script():
    """Anti-inline regression: the all-required step must invoke the
    extracted .gitea/scripts/all-required-check.sh script, not contain the
    inline check() logic. This guarantees the fail-closed contract is tested
    outside CI (test_all_required_check.sh) and cannot be silently re-inlined.
    """
    workflow = load_workflow("ci.yml")
    all_required = _all_required(workflow)
    steps = all_required.get("steps", [])
    assert steps, "all-required job has no steps"

    run_block = ""
    for step in steps:
        if step.get("name") == "Verify all aggregated CI jobs succeeded":
            run_block = step.get("run", "")
            break

    assert run_block, "all-required verify step not found"
    assert "all-required-check.sh" in run_block, (
        "all-required step must invoke .gitea/scripts/all-required-check.sh"
    )
    assert "check()" not in run_block, (
        "all-required step still contains inline check() function — re-inline regression"
    )


def test_ops_scripts_runs_audit_force_merge_shell_contract():
    """The shell integration test is not collected by pytest, so the ops
    workflow must invoke it explicitly."""
    workflow = load_workflow("test-ops-scripts.yml")
    steps = workflow["jobs"]["test"].get("steps", [])
    run_blocks = [step.get("run", "") for step in steps if isinstance(step, dict)]

    assert any(
        "bash .gitea/scripts/tests/test_audit_force_merge.sh" in run
        for run in run_blocks
    ), "test-ops-scripts.yml must execute the audit-force-merge shell contract"
