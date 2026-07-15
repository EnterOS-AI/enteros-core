"""Guard G — disabled-workflow PR-emitter lint unit tests.

The wedge Guard G exists to prevent (task #97, live on 2026-07-13):

  Disabling a workflow in the Gitea Actions config does NOT stop
  `pull_request_target` from firing it — it only blocks manual rerun/dispatch.
  So a disabled workflow keeps posting commit statuses, and when one of its runs
  ends `failure` (or `cancelled`, which posts `failure` with zero logs) the
  status is UNCLEARABLE:

      POST /actions/runs/<id>/rerun -> 400 {"message":"workflow ... is disabled"}

  Under main's `['*']` wildcard branch protection every reported context must be
  success, so the PR is stuck with no API path out. gate-check-v3 and
  sop-checklist wedged core#4126/#4173/#4263/#4305 exactly this way.

Each test below states the mutation it kills. A guard whose test passes when the
behaviour it guards is removed is not a guard.
"""
import importlib.util
import sys
from pathlib import Path

SCRIPT = Path(__file__).resolve().parents[1] / "ci-required-drift.py"
spec = importlib.util.spec_from_file_location("ci_required_drift", SCRIPT)
drift = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = drift
spec.loader.exec_module(drift)


def _write(tmp_path, name, text):
    d = tmp_path / "workflows"
    d.mkdir(exist_ok=True)
    (d / name).write_text(text, encoding="utf-8")
    return str(d)


# ---------------------------------------------------------------------------
# workflow_pr_triggers — the YAML 1.1 `on:` booleanization trap
# ---------------------------------------------------------------------------
def test_pr_triggers_found_under_the_boolean_true_key():
    """PyYAML parses the bare key `on:` as the BOOLEAN True, not the string
    "on". EVERY workflow in this repo writes `on:`, so a guard that only looked
    up doc["on"] would see nothing anywhere and pass vacuously on all of them.

    MUTATION: drop the `doc.get(True)` fallback -> this test FAILS.
    """
    doc = {True: {"pull_request_target": {"types": ["opened"]}}}
    assert drift.workflow_pr_triggers(doc) == ["pull_request_target"]


def test_pr_triggers_string_and_list_forms():
    assert drift.workflow_pr_triggers({"on": "pull_request"}) == ["pull_request"]
    assert drift.workflow_pr_triggers({"on": ["push", "pull_request"]}) == ["pull_request"]


def test_pr_triggers_ignores_non_pr_events():
    doc = {True: {"push": {"branches": ["main"]}, "workflow_dispatch": None}}
    assert drift.workflow_pr_triggers(doc) == []


# ---------------------------------------------------------------------------
# detect_disabled_pr_emitters — the guard itself
# ---------------------------------------------------------------------------
def test_disabled_workflow_still_firing_on_pr_is_RED(tmp_path):
    """The exact core#4126 wedge: disabled_manually + pull_request_target.

    MUTATION: make detect_disabled_pr_emitters return no findings -> FAILS.
    """
    wf_dir = _write(
        tmp_path,
        "gate-check-v3.yml",
        "name: gate-check-v3\non:\n  pull_request_target:\n    types: [opened]\njobs:\n  g:\n    runs-on: x\n",
    )
    states = {"gate-check-v3": {"state": "disabled_manually", "file": "gate-check-v3.yml"}}
    findings, debug = drift.detect_disabled_pr_emitters(states, wf_dir)
    assert findings, "a disabled workflow still firing pull_request_target must be RED"
    assert "gate-check-v3.yml" in findings[0]
    assert debug["offenders"][0]["triggers"] == ["pull_request_target"]


def test_disabled_workflow_with_no_pr_trigger_is_GREEN(tmp_path):
    """The fixed state: disabled AND no PR trigger -> emits nothing, cannot wedge.

    This is the PASS arm; without it the guard could be trivially satisfied by
    always failing.
    """
    wf_dir = _write(
        tmp_path,
        "gate-check-v3.yml",
        "name: gate-check-v3\non:\n  workflow_dispatch:\njobs:\n  g:\n    runs-on: x\n",
    )
    states = {"gate-check-v3": {"state": "disabled_manually", "file": "gate-check-v3.yml"}}
    findings, _ = drift.detect_disabled_pr_emitters(states, wf_dir)
    assert findings == []


def test_ACTIVE_workflow_firing_on_pr_is_GREEN(tmp_path):
    """An ACTIVE workflow with a PR trigger is the normal, correct case — it can
    be rerun, so it cannot wedge. Guard G must not flag it.

    MUTATION: drop the `state != active` filter (flag every workflow) -> FAILS.
    That mutation would red-flag essentially every gate in the repo.
    """
    wf_dir = _write(
        tmp_path,
        "ci.yml",
        "name: CI\non:\n  pull_request:\njobs:\n  g:\n    runs-on: x\n",
    )
    states = {"CI": {"state": "active", "file": "ci.yml"}}
    findings, debug = drift.detect_disabled_pr_emitters(states, wf_dir)
    assert findings == []
    assert debug["checked"] == []  # active workflows are not Guard G's business


def test_auto_disabled_state_also_counts(tmp_path):
    """Gitea also auto-disables workflows (state `disabled`) after repeated
    failures/inactivity. That wedges identically — rerun still 400s.

    MUTATION: match only the literal "disabled_manually" -> FAILS.
    """
    wf_dir = _write(
        tmp_path,
        "sop-checklist.yml",
        "name: sop-checklist\non:\n  pull_request_target:\n    types: [opened]\n  issue_comment:\n    types: [created]\njobs:\n  g:\n    runs-on: x\n",
    )
    states = {"sop-checklist": {"state": "disabled", "file": "sop-checklist.yml"}}
    findings, _ = drift.detect_disabled_pr_emitters(states, wf_dir)
    assert findings, "an AUTO-disabled workflow firing on PRs wedges just the same"


def test_unparseable_disabled_workflow_fails_CLOSED(tmp_path):
    """We cannot prove a workflow we cannot parse is safe. Fail closed.

    MUTATION: `continue` silently on YAMLError instead of recording a finding
    -> FAILS. Silently skipping is how a guard goes green on the case it exists
    to catch.
    """
    wf_dir = _write(tmp_path, "broken.yml", "name: broken\non: [\n  unclosed\n")
    states = {"broken": {"state": "disabled_manually", "file": "broken.yml"}}
    findings, _ = drift.detect_disabled_pr_emitters(states, wf_dir)
    assert findings and "failing closed" in findings[0]


def test_the_repo_itself_is_clean(tmp_path):
    """End-to-end over the REAL .gitea/workflows tree, with the two workflows
    that actually wedged us pinned as disabled. This is the assertion that would
    have gone red before this change and goes green after it — i.e. it tests the
    fix, not just the detector.
    """
    repo_wf = Path(__file__).resolve().parents[2] / "workflows"
    states = {
        "gate-check-v3": {"state": "disabled_manually", "file": "gate-check-v3.yml"},
        "sop-checklist": {"state": "disabled_manually", "file": "sop-checklist.yml"},
        "qa-review": {"state": "disabled_manually", "file": "qa-review.yml"},
        "security-review": {"state": "disabled_manually", "file": "security-review.yml"},
    }
    findings, debug = drift.detect_disabled_pr_emitters(states, str(repo_wf))
    assert findings == [], (
        "a disabled workflow in .gitea/workflows still declares a PR trigger and "
        f"will wedge PRs: {debug['offenders']}"
    )


# ---------------------------------------------------------------------------
# The four fail-OPEN gaps found reviewing Guard G after it landed (core#4309).
# Each was a door the guard did not look at, so each is a way the wedge it exists
# to prevent could still happen while the guard reported GREEN.
# ---------------------------------------------------------------------------
def test_issue_comment_on_a_disabled_workflow_is_RED(tmp_path):
    """THE LIVE ONE. core#4309 dropped `pull_request_target` from sop-checklist but
    KEPT `issue_comment`, reasoning that "a comment event does not post a PR-blocking
    status on its own". That is false:

      - a PR IS an issue in the Gitea API, so a PR comment fires the workflow;
      - sop-checklist's `all-items-acked` runs on /sop-ack and POSTs a commit status;
      - its `review-refire` job has NO job-level `if:` — it runs on EVERY comment and
        reports its own status context regardless;
      - the workflow is disabled, so a red/cancelled run of it can never be rerun.

    So any comment on any PR could re-wedge it. The workflow disabled to STOP the
    wedge could still cause it.

    MUTATION: drop "issue_comment" from PR_STATUS_EVENTS -> this test FAILS.
    """
    wf_dir = _write(
        tmp_path,
        "sop-checklist.yml",
        "name: sop-checklist\non:\n  issue_comment:\n    types: [created]\njobs:\n  x:\n    runs-on: ubuntu-latest\n",
    )
    states = {"sop-checklist": {"file": "sop-checklist.yml", "state": "disabled_manually"}}
    findings, debug = drift.detect_disabled_pr_emitters(states, workflows_dir=wf_dir)
    assert findings, "a disabled workflow firing on issue_comment can still post an unrerunnable status on a PR head"
    assert debug["offenders"][0]["triggers"] == ["issue_comment"]


def test_pull_request_review_events_are_checked(tmp_path):
    """`pull_request_review` / `pull_request_review_comment` fire in PR context and
    post on the PR head just like pull_request does — reserved-path-review is built
    on exactly that. A disabled workflow carrying them wedges identically.

    MUTATION: drop them from PR_STATUS_EVENTS -> this test FAILS.
    """
    for event in ("pull_request_review", "pull_request_review_comment"):
        wf_dir = _write(
            tmp_path,
            "w.yml",
            f"name: w\non:\n  {event}:\n    types: [submitted]\njobs:\n  x:\n    runs-on: ubuntu-latest\n",
        )
        states = {"w": {"file": "w.yml", "state": "disabled_manually"}}
        findings, _ = drift.detect_disabled_pr_emitters(states, workflows_dir=wf_dir)
        assert findings, f"a disabled workflow firing on {event} was not caught"


def test_dot_yaml_workflows_are_not_invisible(tmp_path):
    """Gitea reads BOTH *.yml and *.yaml. The guard globbed only *.yml, so a `.yaml`
    workflow was the one file an author could add that Guard G would never look at —
    a hole nothing in the repo exercised, which is exactly why it would have survived
    until it mattered.

    MUTATION: glob only "*.yml" in workflow_files() -> this test FAILS.
    """
    wf_dir = _write(
        tmp_path,
        "sneaky.yaml",
        "name: sneaky\non:\n  pull_request_target:\n    types: [opened]\njobs:\n  x:\n    runs-on: ubuntu-latest\n",
    )
    states = {"sneaky": {"file": "sneaky.yaml", "state": "disabled_manually"}}
    findings, _ = drift.detect_disabled_pr_emitters(states, workflows_dir=wf_dir)
    assert findings, "a disabled .yaml workflow firing on PR events was invisible to the guard"


def test_unjoinable_api_response_fails_CLOSED(tmp_path):
    """The whole guard keys off workflow_states. If that dict is empty — or stops
    carrying the `file` field the guard joins on (an API shape change, a token that
    lost Actions scope) — the loop finds nothing not-active, reports zero findings,
    and PASSES.

    A guard whose PASS arm is what a broken input produces is not a guard: it would
    report GREEN while inspecting ZERO workflows, exactly when it is blind.

    MUTATION: delete the G2 reachability check -> this test FAILS.
    """
    wf_dir = _write(
        tmp_path,
        "real.yml",
        "name: real\non:\n  pull_request_target:\n    types: [opened]\njobs:\n  x:\n    runs-on: ubuntu-latest\n",
    )
    # Shape drift: the API answers, but with no `file` key to join on.
    states = {"real": {"name": "real", "state": "disabled_manually"}}
    findings, debug = drift.detect_disabled_pr_emitters(states, workflows_dir=wf_dir)
    assert findings and findings[0].startswith("G2"), (
        "an unjoinable Actions-API response must fail CLOSED, not silently inspect "
        f"nothing and pass (findings={findings})"
    )
    assert debug["joinable"] == 0

    # ...and the empty-response case.
    findings, _ = drift.detect_disabled_pr_emitters({}, workflows_dir=wf_dir)
    assert findings and findings[0].startswith("G2")


def test_a_clean_repo_still_passes_and_is_not_vacuous(tmp_path):
    """The G2 guard must not fire when the join genuinely works — otherwise it is
    just a permanent red. A joinable response with an ACTIVE workflow is GREEN, and
    the debug proves the guard actually looked at something."""
    wf_dir = _write(
        tmp_path,
        "ok.yml",
        "name: ok\non:\n  pull_request:\n    types: [opened]\njobs:\n  x:\n    runs-on: ubuntu-latest\n",
    )
    states = {"ok": {"file": "ok.yml", "state": "active"}}
    findings, debug = drift.detect_disabled_pr_emitters(states, workflows_dir=wf_dir)
    assert findings == []
    assert debug["joinable"] == 1, "the guard must prove it joined, not just find nothing"
