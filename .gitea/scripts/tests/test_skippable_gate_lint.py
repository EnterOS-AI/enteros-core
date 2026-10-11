"""Guard S — skippable-enforced-gate lint unit tests.

Fail-before / pass-after proof for `detect_skippable_enforced_gates` in
ci-required-drift.py.

THE HOLE THIS CLOSES
--------------------
Branch protection on molecule-core/main is `status_check_contexts: ["*"]`.
Gitea's wildcard gate requires every POSTED status to be success and treats a
`skipped` status as passing — so an ENFORCED context that goes `skipped`
enforces NOTHING, and a merge taken through any path other than
gitea-merge-queue.py (which fail-closes on `skipped`) proceeds green.

A job goes `skipped` for two statically-detectable reasons:

  S1 — the ENFORCED context names a JOB that does not exist in its producer
       workflow (renamed / deleted job). Nothing ever posts the context.
       Guard F only resolves the producer WORKFLOW, so a job rename slips past
       it with the workflow still `active`.

  S2 — the producer job, or ANY job in its transitive `needs:` closure, carries
       a job-level `if:`. When that `if:` is false the job is `skipped`, and
       Gitea does not start a dependent whose `needs:` was skipped — the skip
       propagates all the way to the ENFORCED context.

       This is the mechanism observed live on molecule-core PR #4911 / #5016:
       `CI / Platform (Go)` did not succeed, so Gitea never started
       `CI / all-required` and posted `skipped` for it (run 593358, job 879124
       `all-required completed skipped`). The merge queue fail-closed and held
       both PRs; branch protection would not have.
"""
import importlib.util
import sys
import textwrap
from pathlib import Path

SCRIPT = Path(__file__).resolve().parents[1] / "ci-required-drift.py"
spec = importlib.util.spec_from_file_location("ci_required_drift", SCRIPT)
drift = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = drift
spec.loader.exec_module(drift)


def _write(tmp_path, name, body):
    d = tmp_path / "workflows"
    d.mkdir(exist_ok=True)
    (d / name).write_text(textwrap.dedent(body), encoding="utf-8")
    return str(d)


CLEAN_CI = """\
    name: CI
    on:
      pull_request:
        branches: [main]
    jobs:
      changes:
        name: Detect changes
        runs-on: ubuntu-latest
        steps: [{run: 'true'}]
      platform-build:
        name: Platform (Go)
        needs: changes
        runs-on: ubuntu-latest
        steps: [{run: 'true'}]
      all-required:
        needs: [changes, platform-build]
        runs-on: ubuntu-latest
        steps: [{run: 'true'}]
"""

ENFORCED = ["CI / all-required"]


# ---------------------------------------------------------------------------
# PASS-AFTER: the shape main actually has today must be clean.
# ---------------------------------------------------------------------------
def test_clean_workflow_has_no_findings(tmp_path):
    wf = _write(tmp_path, "ci.yml", CLEAN_CI)
    findings, debug = drift.detect_skippable_enforced_gates(ENFORCED, wf)
    assert findings == [], findings
    assert debug["ok"] == [
        {
            "context": "CI / all-required",
            "job": "all-required",
            "closure": ["all-required", "changes", "platform-build"],
        }
    ]


# ---------------------------------------------------------------------------
# FAIL-BEFORE (S2): a job-level `if:` ON THE ENFORCED JOB ITSELF.
# ---------------------------------------------------------------------------
def test_if_on_the_enforced_job_itself_is_flagged(tmp_path):
    body = CLEAN_CI.replace(
        "      all-required:\n        needs: [changes, platform-build]\n",
        "      all-required:\n        if: github.event_name == 'push'\n"
        "        needs: [changes, platform-build]\n",
    )
    wf = _write(tmp_path, "ci.yml", body)
    findings, _ = drift.detect_skippable_enforced_gates(ENFORCED, wf)
    assert findings, "a job-level `if:` on the enforced job must be flagged"
    assert "S2" in findings[0]
    assert "all-required" in findings[0]


# ---------------------------------------------------------------------------
# FAIL-BEFORE (S2): a job-level `if:` on a TRANSITIVE need. This is the exact
# live shape — `all-required` never carries the `if:`, its upstream does.
# ---------------------------------------------------------------------------
def test_if_on_a_transitive_need_is_flagged(tmp_path):
    body = CLEAN_CI.replace(
        "      changes:\n        name: Detect changes\n",
        "      changes:\n        name: Detect changes\n"
        "        if: github.event_name == 'push'\n",
    )
    wf = _write(tmp_path, "ci.yml", body)
    findings, _ = drift.detect_skippable_enforced_gates(ENFORCED, wf)
    assert findings, "a job-level `if:` on a transitive need must be flagged"
    assert "S2" in findings[0]
    # The report must name the SKIPPABLE job, not just the enforced context —
    # otherwise the fix hint points at the wrong place.
    assert "changes" in findings[0]


def test_if_two_hops_up_is_flagged(tmp_path):
    """all-required -> platform-build -> changes. The `if:` is two edges away;
    the skip still propagates, so the closure walk must be transitive, not
    one-level."""
    body = CLEAN_CI.replace(
        "      all-required:\n        needs: [changes, platform-build]\n",
        "      all-required:\n        needs: [platform-build]\n",
    ).replace(
        "      changes:\n        name: Detect changes\n",
        "      changes:\n        name: Detect changes\n"
        "        if: github.ref == 'refs/heads/main'\n",
    )
    wf = _write(tmp_path, "ci.yml", body)
    findings, _ = drift.detect_skippable_enforced_gates(ENFORCED, wf)
    assert findings, "a transitive (2-hop) `if:` must be flagged"
    assert "changes" in findings[0]


# ---------------------------------------------------------------------------
# FAIL-BEFORE (S1): the enforced context names a job that no longer exists.
# ---------------------------------------------------------------------------
def test_missing_producer_job_is_flagged(tmp_path):
    wf = _write(tmp_path, "ci.yml", CLEAN_CI)
    findings, _ = drift.detect_skippable_enforced_gates(["CI / all-requiredz"], wf)
    assert findings, "an enforced context naming a nonexistent job must be flagged"
    assert "S1" in findings[0]


def test_job_matched_by_name_not_only_key(tmp_path):
    """Gitea builds the context from `job.name` when present, the job KEY when
    not. Both must resolve, or the lint reports phantom S1s on every named job."""
    wf = _write(tmp_path, "ci.yml", CLEAN_CI)
    findings, debug = drift.detect_skippable_enforced_gates(["CI / Platform (Go)"], wf)
    assert findings == [], findings
    assert debug["ok"][0]["job"] == "platform-build"


# ---------------------------------------------------------------------------
# FAIL-CLOSED: unparseable / absent inputs must never read as a pass.
# ---------------------------------------------------------------------------
def test_unparseable_workflow_is_a_finding_not_a_silent_pass(tmp_path):
    wf = _write(tmp_path, "ci.yml", "name: CI\njobs:\n  - this is not a mapping\n")
    findings, _ = drift.detect_skippable_enforced_gates(ENFORCED, wf)
    assert findings, "a workflow whose jobs: is not a mapping must FAIL, not pass"


def test_empty_enforced_set_is_fail_closed(tmp_path):
    wf = _write(tmp_path, "ci.yml", CLEAN_CI)
    findings, _ = drift.detect_skippable_enforced_gates([], wf)
    assert findings, "an EMPTY enforced set is the vacuous-pass signature; FAIL"


# ---------------------------------------------------------------------------
# The REAL repo must be clean — this is the no-wedge precondition.
# ---------------------------------------------------------------------------
def test_live_repo_is_clean():
    root = Path(__file__).resolve().parents[2]
    enforced = drift.load_enforced_file_contexts(str(root / "required-contexts.txt"))
    findings, debug = drift.detect_skippable_enforced_gates(
        enforced, str(root / "workflows")
    )
    assert findings == [], findings
    assert debug["enforced_count"] == len(enforced) > 0
