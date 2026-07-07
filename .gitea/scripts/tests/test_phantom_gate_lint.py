"""Guard F — phantom-gate lint unit tests (regression-prevention-guards.md).

Fail-before / pass-after proof for the phantom-gate detector added to
ci-required-drift.py: an ENFORCED required-contexts.txt entry whose producer
workflow's Gitea Actions state is not `active` must be flagged RED; the same
entry with an active producer must pass.
"""
import importlib.util
import sys
from pathlib import Path
from unittest.mock import patch

SCRIPT = Path(__file__).resolve().parents[1] / "ci-required-drift.py"
spec = importlib.util.spec_from_file_location("ci_required_drift", SCRIPT)
drift = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = drift
spec.loader.exec_module(drift)

drift.OWNER = "molecule-ai"
drift.NAME = "molecule-core"


# ---------------------------------------------------------------------------
# load_enforced_file_contexts — same parse rule as the merge queue
# ---------------------------------------------------------------------------
_SAMPLE = """\
# header comment
# ── ENFORCED (merge-blocking) ──
CI / all-required
Secret scan / Scan diff for credential-shaped strings   # inline comment
E2E Staging SaaS (full lifecycle) / E2E Staging Concierge Creates Workspace (pull_request)

# RE-ENABLED comment block — NOT a pending marker, so entries below stay enforced
Concierge Creates Workspace Hermetic / Concierge Creates Workspace Hermetic

# pending-#2409 (not yet enforced) ──
Local Provision Lifecycle E2E / Local Provision Lifecycle E2E (stub)

# pending-#3159 (not yet enforced) ──
E2E Staging SaaS (full lifecycle) / E2E Staging Platform Boot
"""


def test_load_enforced_stops_at_first_pending_marker(tmp_path):
    f = tmp_path / "required-contexts.txt"
    f.write_text(_SAMPLE, encoding="utf-8")
    enforced = drift.load_enforced_file_contexts(str(f))
    # Above the first `# pending-#NNNN` marker, event-suffix stripped, inline
    # trailing comments removed. The "RE-ENABLED" plain-comment block does NOT
    # begin the pending tail, so the two entries around it stay enforced.
    assert enforced == [
        "CI / all-required",
        "Secret scan / Scan diff for credential-shaped strings",
        "E2E Staging SaaS (full lifecycle) / E2E Staging Concierge Creates Workspace",
        "Concierge Creates Workspace Hermetic / Concierge Creates Workspace Hermetic",
    ]
    # Entries at/below the first pending marker are documentation, never enforced.
    assert "Local Provision Lifecycle E2E / Local Provision Lifecycle E2E (stub)" not in enforced
    assert "E2E Staging SaaS (full lifecycle) / E2E Staging Platform Boot" not in enforced


def test_load_enforced_missing_file_fails_closed(tmp_path):
    missing = tmp_path / "nope.txt"
    try:
        drift.load_enforced_file_contexts(str(missing))
    except SystemExit as exc:
        assert exc.code == 3
    else:
        raise AssertionError("expected SystemExit(3) on missing SSOT (fail-closed)")


# ---------------------------------------------------------------------------
# producer_workflow_name — context → workflow-name resolution
# ---------------------------------------------------------------------------
def test_producer_workflow_name_resolves_by_prefix():
    names = {"CI", "E2E Staging SaaS (full lifecycle)", "qa-review"}
    assert drift.producer_workflow_name("CI / all-required", names) == "CI"
    assert (
        drift.producer_workflow_name(
            "E2E Staging SaaS (full lifecycle) / E2E Staging Concierge Creates Workspace",
            names,
        )
        == "E2E Staging SaaS (full lifecycle)"
    )
    assert drift.producer_workflow_name("qa-review / approved", names) == "qa-review"


def test_producer_workflow_name_longest_prefix_wins():
    # A workflow named "CI" must not shadow one named "CI Extended" for a
    # "CI Extended / ..." context.
    names = {"CI", "CI Extended"}
    assert drift.producer_workflow_name("CI Extended / job", names) == "CI Extended"


def test_producer_workflow_name_none_when_unmatched():
    assert drift.producer_workflow_name("Ghost Workflow / job", {"CI"}) is None


# ---------------------------------------------------------------------------
# detect_phantom_gates — the FAIL-BEFORE / PASS-AFTER core
# ---------------------------------------------------------------------------
def _states(*pairs):
    return {name: {"state": state, "file": file} for name, state, file in pairs}


def test_phantom_flagged_when_producer_disabled():
    """FAIL-BEFORE: an enforced context whose producer is disabled_manually is a
    PHANTOM and must be flagged (P1)."""
    enforced = [
        "CI / all-required",
        "E2E Staging SaaS (full lifecycle) / E2E Staging Concierge Creates Workspace",
    ]
    states = _states(
        ("CI", "active", "ci.yml"),
        ("E2E Staging SaaS (full lifecycle)", "disabled_manually", "e2e-staging-saas.yml"),
    )
    findings, debug = drift.detect_phantom_gates(enforced, states)
    assert findings, "expected a phantom finding for the disabled producer"
    assert any("P1" in f for f in findings)
    assert any("e2e-staging-saas.yml" in f for f in findings)
    assert debug["phantom_disabled"] == [
        {
            "context": "E2E Staging SaaS (full lifecycle) / E2E Staging Concierge Creates Workspace",
            "producer_file": "e2e-staging-saas.yml",
            "producer_name": "E2E Staging SaaS (full lifecycle)",
            "state": "disabled_manually",
        }
    ]


def test_no_phantom_when_producer_active():
    """PASS-AFTER: re-enabling the producer clears the finding (GREEN)."""
    enforced = [
        "CI / all-required",
        "E2E Staging SaaS (full lifecycle) / E2E Staging Concierge Creates Workspace",
    ]
    states = _states(
        ("CI", "active", "ci.yml"),
        ("E2E Staging SaaS (full lifecycle)", "active", "e2e-staging-saas.yml"),
    )
    findings, debug = drift.detect_phantom_gates(enforced, states)
    assert findings == []
    assert debug["phantom_disabled"] == []
    assert len(debug["ok"]) == 2


def test_phantom_flagged_when_auto_disabled():
    """Gitea auto-disables a workflow (state='disabled') after repeated failures
    — that is a phantom too, not just an operator toggle."""
    enforced = ["Some Gate / job"]
    states = _states(("Some Gate", "disabled", "some-gate.yml"))
    findings, _ = drift.detect_phantom_gates(enforced, states)
    assert any("P1" in f for f in findings)


def test_phantom_flagged_when_no_producer_workflow():
    """P2: an enforced context that NO workflow name prefixes can never be
    posted — flag it even though it is not 'disabled' per se."""
    enforced = ["Deleted Workflow / job"]
    states = _states(("CI", "active", "ci.yml"))
    findings, debug = drift.detect_phantom_gates(enforced, states)
    assert any("P2" in f for f in findings)
    assert debug["phantom_no_producer"] == ["Deleted Workflow / job"]


def test_governance_and_all_active_pass():
    """The real ENFORCED set shape: governance contexts resolve to their
    workflow names; all active → no findings."""
    enforced = [
        "CI / all-required",
        "qa-review / approved",
        "security-review / approved",
        "sop-checklist / all-items-acked",
        "reserved-path-review / reserved-path-review",
    ]
    states = _states(
        ("CI", "active", "ci.yml"),
        ("qa-review", "active", "qa-review.yml"),
        ("security-review", "active", "security-review.yml"),
        ("sop-checklist", "active", "sop-checklist.yml"),
        ("reserved-path-review", "active", "reserved-path-review.yml"),
    )
    findings, _ = drift.detect_phantom_gates(enforced, states)
    assert findings == []


def test_duplicate_name_disabled_not_masked_by_active():
    """Defensive: if two workflow files share a `name:`, the disabled one must
    win so a phantom is never hidden behind an active sibling."""
    body = {
        "workflows": [
            {"id": "a.yml", "name": "Dup", "state": "active"},
            {"id": "b.yml", "name": "Dup", "state": "disabled_manually"},
        ]
    }
    with patch.object(drift, "api", return_value=(200, body)):
        states = drift.fetch_workflow_states()
    assert states["Dup"]["state"] == "disabled_manually"


# ---------------------------------------------------------------------------
# fetch_workflow_states — API parse
# ---------------------------------------------------------------------------
def test_fetch_workflow_states_parses_name_state_file():
    body = {
        "workflows": [
            {"id": "ci.yml", "name": "CI", "state": "active", "path": ".gitea/workflows/ci.yml"},
            {
                "id": "e2e-staging-saas.yml",
                "name": "E2E Staging SaaS (full lifecycle)",
                "state": "disabled_manually",
                "path": ".gitea/workflows/e2e-staging-saas.yml",
            },
        ]
    }
    with patch.object(drift, "api", return_value=(200, body)):
        states = drift.fetch_workflow_states()
    assert states["CI"] == {"state": "active", "file": "ci.yml"}
    assert states["E2E Staging SaaS (full lifecycle)"] == {
        "state": "disabled_manually",
        "file": "e2e-staging-saas.yml",
    }


# ---------------------------------------------------------------------------
# Guard F TRIGGER promotion (#3467): the phantom-gate lint must ALSO run on
# pull_request so a phantom RED-blocks the PR pre-merge (under the `[*]`
# all-green branch protection) instead of only being detected post-merge on
# push:[main]. These tests are the fail-before/pass-after proof for the TRIGGER
# change: drop the pull_request trigger (revert the fix) and
# test_guard_f_runs_on_pull_request fails; drop push:[main] or workflow_dispatch
# and test_guard_f_keeps_push_main_and_dispatch fails.
# ---------------------------------------------------------------------------
import yaml  # noqa: E402  (grouped with the trigger-regression tests)

_WORKFLOW = (
    Path(__file__).resolve().parents[2] / "workflows" / "lint-phantom-gate.yml"
)


def _load_on_block():
    doc = yaml.safe_load(_WORKFLOW.read_text(encoding="utf-8"))
    # PyYAML (YAML 1.1) resolves the bare `on:` key to the boolean True, so a
    # workflow's trigger block can live under either the string "on" or True.
    if isinstance(doc, dict) and "on" in doc:
        return doc["on"]
    if isinstance(doc, dict) and True in doc:
        return doc[True]
    raise AssertionError(f"no on: block found in {_WORKFLOW}")


def test_guard_f_runs_on_pull_request():
    """The promotion: Guard F must be triggered on pull_request so a phantom
    RED-blocks the PR pre-merge, not only post-merge on push:[main]."""
    on = _load_on_block()
    assert isinstance(on, dict), f"unexpected on: block shape: {on!r}"
    assert "pull_request" in on, (
        "Guard F (lint-phantom-gate) must run on pull_request so a phantom "
        "required-context RED-blocks the PR BEFORE it merges (not only "
        "post-merge on push:[main]) — regression-prevention-guards.md §Guard F "
        "/ #3467."
    )


def test_guard_f_keeps_push_main_and_dispatch():
    """Promotion is ADDITIVE: the push:[main] regression detector and the
    on-demand workflow_dispatch trigger must remain."""
    on = _load_on_block()
    assert "workflow_dispatch" in on, "on-demand dispatch trigger must remain"
    push = on.get("push")
    assert isinstance(push, dict), f"push: trigger must remain a mapping, got {push!r}"
    assert "main" in (push.get("branches") or []), (
        "push:[main] regression detector must remain — it guards MAIN, where a "
        "workflow toggle-off actually lands."
    )
