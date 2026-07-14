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
