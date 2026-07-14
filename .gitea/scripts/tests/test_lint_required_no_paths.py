"""Tests for lint-required-no-paths — BOTH arms of the
"a required check must not be able to go green without running" invariant.

  ARM A (blocking)     — declarative `on: paths:` on a required workflow.
  ARM B (report-only)  — the SAME filter hand-rolled in shell inside a
                         detect-changes job, whose boolean output gates the
                         required-context job's real work.

Every guard here is NEGATIVE-CONTROLLED: for each detector we assert it
FIRES on the broken input AND stays silent on the correct input. A guard
that has only ever been seen to pass is not a guard
(feedback_negative_control_every_test).

The final class is a LIVE mutation proof against the real repo: the four
lanes that carry the Arm-B shape today MUST be detected, and the lanes that
do not carry it MUST NOT be. If someone converts a lane to always-run, the
corresponding assertion here is what tells them to promote the lint.
"""
import importlib.util
import sys
import textwrap
from pathlib import Path

import pytest
import yaml

REPO_ROOT = Path(__file__).resolve().parents[3]
SCRIPT = Path(__file__).resolve().parents[1] / "lint-required-no-paths.py"
spec = importlib.util.spec_from_file_location("lint_required_no_paths", SCRIPT)
lint = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = lint
spec.loader.exec_module(lint)


def _doc(src: str) -> dict:
    return yaml.safe_load(textwrap.dedent(src))


# ---------------------------------------------------------------------------
# The canonical broken shape, as it exists in the repo today: a detect-changes
# job computing a boolean from a diff, gating a required-context job.
# ---------------------------------------------------------------------------
BROKEN = """
    name: E2E Thing
    on:
      pull_request:
    jobs:
      detect-changes:
        outputs:
          api: ${{ steps.decide.outputs.api }}
        steps:
          - id: decide
            run: python3 .gitea/scripts/detect-changes.py --profile api
      e2e:
        name: E2E Thing
        needs: detect-changes
        steps:
          - name: No-op pass (paths filter excluded this commit)
            if: needs.detect-changes.outputs.api != 'true'
            run: |
              echo "No changes — gate satisfied without running tests."
              exit 0
          - if: needs.detect-changes.outputs.api == 'true'
            uses: actions/checkout@v6
          - name: Run the actual E2E
            if: needs.detect-changes.outputs.api == 'true'
            run: go test ./tests/e2e/...
"""

# The same lane, FIXED: the check always runs.
FIXED = """
    name: E2E Thing
    on:
      pull_request:
    jobs:
      e2e:
        name: E2E Thing
        steps:
          - uses: actions/checkout@v6
          - name: Run the actual E2E
            run: go test ./tests/e2e/...
"""


# ===========================================================================
# ARM B — detect_noop_gate
# ===========================================================================
class TestArmBFiresOnBrokenInput:
    def test_fires_on_the_canonical_broken_lane(self):
        findings = lint.detect_noop_gate(_doc(BROKEN), "e2e")
        assert findings, "detector MISSED the green-by-no-op shape"

    def test_reports_both_b1_and_b2_on_the_canonical_lane(self):
        blob = " ".join(lint.detect_noop_gate(_doc(BROKEN), "e2e"))
        assert "[B1 no-op arm]" in blob
        assert "[B2 all-steps-gated]" in blob

    def test_b1_alone_fires_when_an_ungated_preflight_step_exists(self):
        """The e2e-peer-visibility.yml shape.

        This lane has ONE ungated substantive step (a `bash -n` script-syntax
        preflight), so the all-steps-gated signature (B2) does NOT hold — yet
        the required context still greens without running the E2E. The first
        draft of this detector used B2 alone and MISSED this lane. B1 (the
        explicit no-op arm) is what catches it.
        """
        doc = _doc(BROKEN)
        doc["jobs"]["e2e"]["steps"].insert(
            0, {"name": "Validate driving scripts", "run": "bash -n tests/e2e/lib/assert.sh"}
        )
        blob = " ".join(lint.detect_noop_gate(doc, "e2e"))
        assert "[B1 no-op arm]" in blob, "B1 must catch what B2 cannot"
        assert "[B2 all-steps-gated]" not in blob

    def test_b2_alone_fires_when_the_noop_echo_step_is_deleted(self):
        """Deleting the tell-tale echo step must NOT satisfy the lint.

        The defect is the all-steps-skippable shape, not the echo step. A lane
        that removes the "No-op pass" step but keeps the gate still greens
        without running.
        """
        doc = _doc(BROKEN)
        doc["jobs"]["e2e"]["steps"] = [
            s for s in doc["jobs"]["e2e"]["steps"] if "No-op pass" not in str(s.get("name"))
        ]
        blob = " ".join(lint.detect_noop_gate(doc, "e2e"))
        assert "[B2 all-steps-gated]" in blob, "B2 must survive deletion of the echo step"
        assert "[B1 no-op arm]" not in blob

    def test_fires_on_git_diff_and_paths_filter_predicates_too(self):
        """The predicate source is behavioural, not a hardcoded script name."""
        for producer in (
            {"id": "decide", "run": "git diff --name-only origin/main | grep -q api"},
            {"id": "decide", "uses": "dorny/paths-filter@v3"},
        ):
            doc = _doc(BROKEN)
            doc["jobs"]["detect-changes"]["steps"] = [producer]
            assert lint.detect_noop_gate(doc, "e2e"), f"missed producer {producer}"


class TestArmBSilentOnCorrectInput:
    """NEGATIVE CONTROLS. A detector that cannot be seen to stay quiet on a
    correct workflow will red-block the repo the moment a lane is fixed."""

    def test_silent_on_the_fixed_always_run_lane(self):
        assert lint.detect_noop_gate(_doc(FIXED), "e2e") == []

    def test_silent_when_the_gate_is_not_diff_derived(self):
        """`if: needs.build.outputs.image != ''` is not a paths filter.

        Only a predicate computed from a repo DIFF degrades the gate the way
        `on: paths:` does. Gating on a build artifact is ordinary sequencing.
        """
        doc = _doc(BROKEN)
        doc["jobs"]["detect-changes"]["steps"] = [
            {"id": "decide", "run": "echo api=true >> $GITHUB_OUTPUT"}
        ]
        assert lint.detect_noop_gate(doc, "e2e") == []

    def test_silent_when_the_producer_exports_no_outputs(self):
        doc = _doc(BROKEN)
        del doc["jobs"]["detect-changes"]["outputs"]
        assert lint.detect_noop_gate(doc, "e2e") == []

    def test_silent_when_the_job_does_not_need_the_producer(self):
        doc = _doc(BROKEN)
        del doc["jobs"]["e2e"]["needs"]
        assert lint.detect_noop_gate(doc, "e2e") == []

    def test_a_real_step_behind_the_noop_arm_is_not_inert(self):
        """If the `!= 'true'` arm actually RUNS the check, it is not a no-op."""
        doc = _doc(BROKEN)
        doc["jobs"]["e2e"]["steps"][0]["run"] = "go test ./tests/e2e/..."
        blob = " ".join(lint.detect_noop_gate(doc, "e2e"))
        assert "[B1 no-op arm]" not in blob


class TestInertStepClassification:
    @pytest.mark.parametrize(
        "body",
        [
            'echo "nothing to do"',
            'echo "a"\necho "b"\nexit 0',
            "set -euo pipefail\necho ok\n:",
            "# just a comment\ntrue",
        ],
    )
    def test_inert_bodies(self, body):
        assert lint._is_inert_step({"run": body}) is True

    @pytest.mark.parametrize(
        "body",
        [
            "go test ./...",
            'echo "starting"\nbash tests/e2e/run.sh',
            "docker compose up -d",
            'echo hi\ncurl -fsS https://example.test/health',
        ],
    )
    def test_substantive_bodies(self, body):
        assert lint._is_inert_step({"run": body}) is False

    def test_an_action_step_is_substantive(self):
        assert lint._is_inert_step({"uses": "actions/setup-go@v5"}) is False

    def test_setup_actions_are_not_the_substantive_work(self):
        assert lint._is_setup_step({"uses": "actions/checkout@v6"}) is True
        assert lint._is_setup_step({"uses": "docker/login-action@v3"}) is True
        assert lint._is_setup_step({"uses": "dorny/paths-filter@v3"}) is False


# ===========================================================================
# ARM A — detect_paths_filters (the original, blocking arm)
# ===========================================================================
class TestArmA:
    def _write(self, tmp_path: Path, src: str) -> Path:
        p = tmp_path / "wf.yml"
        p.write_text(textwrap.dedent(src))
        return p

    def test_fires_on_paths(self, tmp_path):
        p = self._write(tmp_path, """
            name: X
            on:
              pull_request:
                paths: ['**.go']
            jobs: {}
        """)
        assert lint.detect_paths_filters(p)

    def test_fires_on_paths_ignore(self, tmp_path):
        p = self._write(tmp_path, """
            name: X
            on:
              push:
                paths-ignore: ['docs/**']
            jobs: {}
        """)
        assert lint.detect_paths_filters(p)

    def test_silent_without_a_filter(self, tmp_path):
        p = self._write(tmp_path, """
            name: X
            on:
              pull_request:
                types: [opened]
            jobs: {}
        """)
        assert lint.detect_paths_filters(p) == []


# ===========================================================================
# ENFORCED-context enumeration — the bug that made this lint a no-op.
# ===========================================================================
class TestEnforcedContextEnumeration:
    def test_stops_at_the_first_pending_marker(self, tmp_path):
        f = tmp_path / "required-contexts.txt"
        f.write_text(textwrap.dedent("""
            # a comment
            CI / all-required
            E2E API Smoke Test / E2E API Smoke Test (pull_request)

            # pending-#2409 (not yet enforced) ---
            Local Provision Lifecycle E2E / Local Provision Lifecycle E2E (stub)
            # pending-#3159 (not yet enforced) ---
            E2E Staging SaaS (full lifecycle) / E2E Staging Platform Boot
        """))
        got = lint.load_enforced_contexts(str(f))
        assert got == ["CI / all-required", "E2E API Smoke Test / E2E API Smoke Test"]

    def test_event_suffix_is_stripped(self):
        assert lint.strip_event("CI / all-required (pull_request)") == "CI / all-required"
        assert lint.strip_event("CI / all-required") == "CI / all-required"

    def test_a_job_name_ending_in_parens_is_not_mistaken_for_an_event(self):
        ctx = "Local Provision Lifecycle E2E / Local Provision Lifecycle E2E (stub)"
        assert lint.strip_event(ctx) == ctx

    def test_wildcard_is_recognised_as_the_meta_gate(self):
        """`["*"]` is what live BP actually holds. It must NOT be treated as an
        enumerable context — doing so is what made this lint resolve ZERO
        workflows and green out."""
        assert "*" in lint.WILDCARD_CONTEXTS
        assert lint.parse_context("*") is None

    def test_missing_ssot_file_fails_closed(self, tmp_path):
        with pytest.raises(SystemExit) as e:
            lint.load_enforced_contexts(str(tmp_path / "nope.txt"))
        assert e.value.code == 3


# ===========================================================================
# LIVE MUTATION PROOF against the real repo.
# ===========================================================================
class TestAgainstTheRealRepo:
    """These are the assertions that make the lint real.

    If a lane below is converted to always-run, its assertion here fails —
    that is the signal to strike it from the list and, once the list is
    empty, to set NOOP_GATE_ENFORCE=1 and make Arm B blocking.
    """

    # Every one of these is an ENFORCED (merge-blocking) context whose job can
    # post SUCCESS without running. Verified live 2026-07-14.
    STILL_BROKEN = {
        "e2e-api.yml": "e2e-api",
        "e2e-peer-visibility.yml": "peer-visibility",
        "handlers-postgres-integration.yml": "integration",
        "template-delivery-e2e.yml": "delivery",
    }
    # ENFORCED contexts that correctly always run. These are the negative
    # controls that prove the detector is not just shouting at everything.
    STILL_CLEAN = {
        "ci.yml": "all-required",
        "secret-scan.yml": "scan",
        "concierge-creates-workspace-hermetic.yml": "hermetic",
    }

    def _load(self, name: str) -> dict:
        p = REPO_ROOT / ".gitea" / "workflows" / name
        if not p.is_file():
            pytest.skip(f"{name} not present in this checkout")
        return yaml.safe_load(p.read_text())

    @pytest.mark.parametrize("wf,job", sorted(STILL_BROKEN.items()))
    def test_enforced_lanes_with_a_noop_arm_are_detected(self, wf, job):
        findings = lint.detect_noop_gate(self._load(wf), job)
        assert findings, (
            f"{wf}::{job} is an ENFORCED context that can green without "
            f"running, but the detector did not flag it."
        )

    @pytest.mark.parametrize("wf,job", sorted(STILL_CLEAN.items()))
    def test_enforced_lanes_that_always_run_are_not_flagged(self, wf, job):
        findings = lint.detect_noop_gate(self._load(wf), job)
        assert findings == [], f"false positive on {wf}::{job}: {findings}"

    def test_arm_a_is_clean_across_the_enforced_set(self):
        """Arm A is BLOCKING. If this ever fails, the repo is wedged — so it is
        also the check that proves making Arm A live here does not wedge it."""
        ssot = REPO_ROOT / ".gitea" / "required-contexts.txt"
        if not ssot.is_file():
            pytest.skip("required-contexts.txt not present")
        offenders = []
        for ctx in lint.load_enforced_contexts(str(ssot)):
            wf_name = ctx.split(" / ", 1)[0]
            for p in (REPO_ROOT / ".gitea" / "workflows").glob("*.y*ml"):
                doc = yaml.safe_load(p.read_text())
                if isinstance(doc, dict) and doc.get("name") == wf_name:
                    if lint.detect_paths_filters(p):
                        offenders.append(ctx)
        assert offenders == [], f"Arm A would BLOCK on: {offenders}"
