"""Unit tests for .gitea/scripts/lint_continue_on_error_tracking.py.

Pins the pure-logic surface (find_coe_truthies, find_tracker_in_window,
validate_tracker) without making real HTTP calls. The end-to-end git
ls-tree + Gitea API path is exercised by running the workflow
against real PRs.

Run locally::

    python3 -m unittest .gitea/scripts/tests/test_lint_continue_on_error_tracking.py -v

Mirrors the pattern in test_lint_pre_flip_continue_on_error.py +
scripts/ops/test_check_migration_collisions.py.
"""
from __future__ import annotations

import importlib.util
import sys
import unittest
from pathlib import Path
from unittest import mock

# Load the script as a module without invoking main(). Tests must NOT
# depend on the full runtime env contract (GITEA_TOKEN etc.), so we
# import individual functions and stub the network surface explicitly.
SCRIPT_PATH = Path(__file__).resolve().parent.parent / "lint_continue_on_error_tracking.py"
spec = importlib.util.spec_from_file_location("lcoet", SCRIPT_PATH)
lcoet = importlib.util.module_from_spec(spec)
spec.loader.exec_module(lcoet)


# --------------------------------------------------------------------------
# Fixtures: minimal valid workflow YAML with continue-on-error + tracker
# --------------------------------------------------------------------------
COE_YML_WITH_TRACKER = """\
name: e2e-staging-saas
on:
  pull_request:
    types: [opened, synchronize, reopened]
jobs:
  prune-stale-e2e-dns:
    name: Prune stale e2e DNS records
    runs-on: ubuntu-latest
    if: always()
    # mc#3173: governance tracker — best-effort cleanup; transient
    # CF API failures must not block merge.
    continue-on-error: true
    timeout-minutes: 10
    steps:
      - run: echo prune
"""

COE_YML_NO_TRACKER = """\
name: e2e-staging-saas
on:
  pull_request:
    types: [opened, synchronize, reopened]
jobs:
  prune-stale-e2e-dns:
    name: Prune stale e2e DNS records
    runs-on: ubuntu-latest
    if: always()
    # best-effort cleanup; no tracker comment
    continue-on-error: true
    timeout-minutes: 10
    steps:
      - run: echo prune
"""

COE_YML_INLINE_TRACKER = """\
name: e2e-staging-saas
on:
  pull_request:
    types: [opened, synchronize, reopened]
jobs:
  prune-stale-e2e-dns:
    runs-on: ubuntu-latest
    if: always()
    continue-on-error: true  # mc#3173 inline tracker
    timeout-minutes: 10
    steps:
      - run: echo prune
"""

NO_COE_YML = """\
name: e2e-staging-saas
on:
  pull_request:
    types: [opened, synchronize, reopened]
jobs:
  prune-stale-e2e-dns:
    runs-on: ubuntu-latest
    if: always()
    timeout-minutes: 10
    steps:
      - run: echo prune
"""


# --------------------------------------------------------------------------
# find_coe_truthies + find_tracker_in_window (pure parsing)
# --------------------------------------------------------------------------
class TestFindCoeTruthies(unittest.TestCase):
    """Pin the AST-driven detection of continue-on-error: true directives."""

    def test_detects_truthy_continue_on_error(self):
        """A workflow with continue-on-error: true must surface the (job_key, line) tuple."""
        raw_lines = COE_YML_WITH_TRACKER.splitlines()
        doc = lcoet.yaml.load(COE_YML_WITH_TRACKER, Loader=lcoet._LineLoader)
        locs = lcoet.find_coe_truthies(doc, raw_lines)
        self.assertEqual(len(locs), 1, f"expected 1 truthy coe, got {locs}")
        jkey, line = locs[0]
        self.assertEqual(jkey, "prune-stale-e2e-dns")
        # Line is 1-based; the fixture's coe is on line 12
        # (the `continue-on-error: true` key, not the comment lines 10-11).
        self.assertEqual(line, 12)

    def test_no_truthy_continue_on_error(self):
        """A workflow with no continue-on-error: true must surface empty locs."""
        raw_lines = NO_COE_YML.splitlines()
        doc = lcoet.yaml.load(NO_COE_YML, Loader=lcoet._LineLoader)
        locs = lcoet.find_coe_truthies(doc, raw_lines)
        self.assertEqual(locs, [])


class TestFindTrackerInWindow(unittest.TestCase):
    """Pin the comment-scan window: ±2 lines of the directive's line."""

    def test_tracker_on_line_above(self):
        """Tracker comment one line above the directive (within window)."""
        raw_lines = COE_YML_WITH_TRACKER.splitlines()
        # The fixture's coe is line 11; tracker comment is line 10.
        tracker = lcoet.find_tracker_in_window(raw_lines, 11)
        self.assertIsNotNone(tracker, "tracker on line above should match within window")
        slug, num = tracker
        self.assertEqual(slug, "mc")
        self.assertEqual(num, 3173)

    def test_no_tracker_comment(self):
        """No tracker comment anywhere within window -> None."""
        raw_lines = COE_YML_NO_TRACKER.splitlines()
        # Find the line of the coe directive
        doc = lcoet.yaml.load(COE_YML_NO_TRACKER, Loader=lcoet._LineLoader)
        locs = lcoet.find_coe_truthies(doc, raw_lines)
        self.assertEqual(len(locs), 1)
        _, line = locs[0]
        tracker = lcoet.find_tracker_in_window(raw_lines, line)
        self.assertIsNone(tracker, f"expected None, got {tracker}")

    def test_inline_tracker_on_directive_line(self):
        """Inline trailing comment on the directive's own line is matched (line 0 of the window)."""
        raw_lines = COE_YML_INLINE_TRACKER.splitlines()
        doc = lcoet.yaml.load(COE_YML_INLINE_TRACKER, Loader=lcoet._LineLoader)
        locs = lcoet.find_coe_truthies(doc, raw_lines)
        self.assertEqual(len(locs), 1)
        _, line = locs[0]
        tracker = lcoet.find_tracker_in_window(raw_lines, line)
        self.assertIsNotNone(tracker, "inline tracker comment should match")
        slug, num = tracker
        self.assertEqual(slug, "mc")
        self.assertEqual(num, 3173)

    def test_tracker_outside_window_is_not_matched(self):
        """Tracker comment far above (>2 lines) the directive must NOT be matched."""
        raw_lines = [
            "# mc#3173 way too far above",
            "name: x",
            "on:",
            "  pull_request:",
            "    types: [opened]",
            "jobs:",
            "  prune-stale-e2e-dns:",
            "    runs-on: ubuntu-latest",
            "    continue-on-error: true",
        ]
        # coe is line 9; tracker is line 1. Distance = 8 > WINDOW (2).
        tracker = lcoet.find_tracker_in_window(raw_lines, 9)
        self.assertIsNone(tracker, "out-of-window tracker should not match")


# --------------------------------------------------------------------------
# validate_tracker (HTTP stubbed)
# --------------------------------------------------------------------------
class TestValidateTracker(unittest.TestCase):
    """Pin the age/state validation of a tracker reference.

    Stubs `fetch_issue` so the test does not depend on Gitea. Pins
    the real-world failure modes the lint guards against:
    - closed state (defect fixed but mask not flipped)
    - too-old (>14d) — forces renewal cadence
    - 404 / not_found
    - 403 / forbidden — graceful degrade, not a lint failure
    """

    def test_open_fresh_tracker_passes(self):
        """An open issue 5 days old passes."""
        from datetime import datetime, timedelta, timezone
        fresh = (datetime.now(timezone.utc) - timedelta(days=5)).isoformat()
        with mock.patch.object(
            lcoet,
            "fetch_issue",
            return_value=("ok", {"state": "open", "created_at": fresh}),
        ):
            ok, reason = lcoet.validate_tracker("mc", 3173, max_age_days=14)
        self.assertTrue(ok, f"expected ok, got reason: {reason}")
        self.assertIn("open", reason)
        self.assertIn("5d old", reason)

    def test_closed_tracker_fails(self):
        """A closed issue fails (defect fixed, mask should be flipped)."""
        with mock.patch.object(
            lcoet, "fetch_issue", return_value=("ok", {"state": "closed"})
        ):
            ok, reason = lcoet.validate_tracker("mc", 3173, max_age_days=14)
        self.assertFalse(ok, "closed tracker must fail validation")
        self.assertIn("closed", reason)

    def test_too_old_tracker_fails(self):
        """An open issue >14 days old fails (renewal cadence violation)."""
        from datetime import datetime, timedelta, timezone
        old = (datetime.now(timezone.utc) - timedelta(days=20)).isoformat()
        with mock.patch.object(
            lcoet,
            "fetch_issue",
            return_value=("ok", {"state": "open", "created_at": old}),
        ):
            ok, reason = lcoet.validate_tracker("mc", 3173, max_age_days=14)
        self.assertFalse(ok, "20-day-old tracker must fail the 14d cap")
        self.assertIn("20 days old", reason)
        self.assertIn(">14d cap", reason)

    def test_not_found_tracker_fails(self):
        """A 404 fails (tracker issue was deleted)."""
        with mock.patch.object(
            lcoet, "fetch_issue", return_value=("not_found", None)
        ):
            ok, reason = lcoet.validate_tracker("mc", 3173, max_age_days=14)
        self.assertFalse(ok, "404 tracker must fail")
        self.assertIn("does not exist", reason)

    def test_forbidden_graceful_degrade(self):
        """A 403 is graceful-degraded to ok=True so token-scope issues
        don't red-X every PR. The Tier 2a contract: surface via stderr
        but don't fail the lint."""
        with mock.patch.object(
            lcoet, "fetch_issue", return_value=("forbidden", None)
        ):
            ok, reason = lcoet.validate_tracker("mc", 3173, max_age_days=14)
        self.assertTrue(ok, "403 must graceful-degrade to ok=True")
        self.assertIn("forbidden", reason)

    def test_error_fails_closed(self):
        """A non-403/404 HTTP error fails CLOSED (defensive — don't
        skip on outage, force the operator to fix the token)."""
        with mock.patch.object(
            lcoet, "fetch_issue", return_value=("error", None)
        ):
            ok, reason = lcoet.validate_tracker("mc", 3173, max_age_days=14)
        self.assertFalse(ok, "non-403/404 error must fail closed")


# --------------------------------------------------------------------------
class TestUnmaskedWorkflowsStayUnmasked(unittest.TestCase):
    """Pin the mc#4602 / mc#4603 close-out.

    These two jobs previously asserted the OPPOSITE — that `drift` and
    `lifecycle-real` MUST carry `continue-on-error: true` — because the
    governance decision at the time was to keep the masks and renew their
    trackers. Both masks were removed instead of renewed a fourth time:

      * `drift` was masking a gate that could not run at all
        (APP_SSOT_READ_TOKEN was never provisioned, so the script always
        took its skip branch). It now reads DRIFT_BOT_TOKEN from the
        Infisical SSOT and compares 15 shared tokens x {light, dark}.
      * `lifecycle-real` was promoted on 9 days of LOG evidence
        (279 runs, 278 x "18 passed, 0 failed", 0 skip-path exits, and one
        run cancelled on a superseded head), which is what mc#2408 asked
        for.

    So the pin is inverted: a silent RE-MASK of either job is now the
    regression to catch.
    """

    REPO_ROOT = Path(__file__).resolve().parents[3]
    UNMASKED = {
        "design-token-drift-gate.yml": "drift",
        "local-provision-e2e.yml": "lifecycle-real",
    }

    def _coe_jobs(self, filename):
        path = self.REPO_ROOT / ".gitea" / "workflows" / filename
        self.assertTrue(path.exists(), f"workflow not found at {path}")
        raw = path.read_text(encoding="utf-8")
        doc = lcoet.yaml.load(raw, Loader=lcoet._LineLoader)
        return {jkey for jkey, _ in lcoet.find_coe_truthies(doc, raw.splitlines())}

    def test_unmasked_jobs_have_no_truthy_coe(self):
        for filename, job in self.UNMASKED.items():
            with self.subTest(workflow=filename, job=job):
                coe_jobs = self._coe_jobs(filename)
                self.assertNotIn(
                    job,
                    coe_jobs,
                    f"{job} was UNMASKED deliberately (mc#4602 / mc#4603 "
                    f"close-out) and must not regain continue-on-error: true "
                    f"silently. If the mask is genuinely needed again, that is "
                    f"a governance decision: re-add it WITH a fresh open "
                    f"tracker inside the {lcoet.WINDOW}-line window and update "
                    f"this test. found coe jobs: {coe_jobs}",
                )


# --------------------------------------------------------------------------
# Repo-wide invariant over the LIVE workflow tree
# --------------------------------------------------------------------------
class TestLiveWorkflowTrackerInvariant(unittest.TestCase):
    """Every `continue-on-error: true` still present in the repo must carry a
    tracker reference within the scan window.

    This replaces the two per-job pins with the rule they were instances of,
    so it keeps protecting the masks that remain (and any added later) rather
    than only the two that happened to exist when it was written.
    """

    REPO_ROOT = Path(__file__).resolve().parents[3]
    WORKFLOWS_DIR = REPO_ROOT / ".gitea" / "workflows"

    def test_every_live_coe_has_a_tracker_in_window(self):
        checked = 0
        untracked = []
        # Enumerate via the LINT'S OWN file-discovery helper rather than a
        # local glob. A local `*.yml` glob was narrower than the rule it
        # guards — the lint also reads `*.yaml`, so a mask added in a
        # `.yaml` workflow would be enforced by the lint and invisible here.
        # No `.yaml` workflow exists today, so this closes a latent gap
        # rather than a live one, and it cannot re-open by drift.
        for path in lcoet._iter_workflow_files(self.WORKFLOWS_DIR):
            raw = path.read_text(encoding="utf-8")
            try:
                doc = lcoet.yaml.load(raw, Loader=lcoet._LineLoader)
            except lcoet.yaml.YAMLError:
                continue  # lint-workflow-yaml covers parse errors separately
            raw_lines = raw.splitlines()
            for jkey, line in lcoet.find_coe_truthies(doc, raw_lines):
                checked += 1
                tracker = lcoet.find_tracker_in_window(raw_lines, line)
                if tracker is None:
                    untracked.append(f"{path.name}:{line} job={jkey}")
                else:
                    slug, num = tracker
                    self.assertIn(slug, ("mc", "internal"))
                    self.assertGreater(num, 0)
        self.assertEqual(untracked, [], f"untracked masks: {untracked}")
        # A rule that inspected NOTHING reports success identically to one
        # that inspected everything. If the repo ever reaches zero masks this
        # assertion is the thing that has to be consciously retired.
        self.assertGreater(
            checked,
            0,
            "no `continue-on-error: true` found in .gitea/workflows — this "
            "test would be a vacuous pass; confirm that is real and retire it "
            "deliberately rather than leaving it green over an empty set.",
        )


if __name__ == "__main__":
    unittest.main()
