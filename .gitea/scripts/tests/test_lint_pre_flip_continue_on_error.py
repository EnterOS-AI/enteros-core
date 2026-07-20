"""Unit tests for .gitea/scripts/lint_pre_flip_continue_on_error.py.

These tests pin the pure-logic surface (flip detection + per-flip
verdict aggregation) without making real HTTP calls. The end-to-end
git ls-tree + Gitea API path is exercised by running the workflow
against real PRs.

Run locally::

    python3 -m unittest .gitea/scripts/tests/test_lint_pre_flip_continue_on_error.py -v

Mirrors the pattern in scripts/ops/test_check_migration_collisions.py
+ scripts/test_build_runtime_package.py.
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
SCRIPT_PATH = Path(__file__).resolve().parent.parent / "lint_pre_flip_continue_on_error.py"
spec = importlib.util.spec_from_file_location("lpfc", SCRIPT_PATH)
lpfc = importlib.util.module_from_spec(spec)
spec.loader.exec_module(lpfc)


# --------------------------------------------------------------------------
# Fixtures: minimal valid workflow YAML on each side of a "diff"
# --------------------------------------------------------------------------
CI_YML_BASE = """\
name: CI
on:
  push:
    branches: [main]
jobs:
  platform-build:
    name: Platform (Go)
    runs-on: ubuntu-latest
    continue-on-error: true
    steps:
      - run: echo platform
  canvas-build:
    name: Canvas (Next.js)
    runs-on: ubuntu-latest
    continue-on-error: true
    steps:
      - run: echo canvas
  all-required:
    runs-on: ubuntu-latest
    continue-on-error: true
    needs: [platform-build, canvas-build]
    steps:
      - run: echo ok
"""

CI_YML_HEAD_FLIPPED = """\
name: CI
on:
  push:
    branches: [main]
jobs:
  platform-build:
    name: Platform (Go)
    runs-on: ubuntu-latest
    continue-on-error: false
    steps:
      - run: echo platform
  canvas-build:
    name: Canvas (Next.js)
    runs-on: ubuntu-latest
    continue-on-error: false
    steps:
      - run: echo canvas
  all-required:
    runs-on: ubuntu-latest
    continue-on-error: true
    needs: [platform-build, canvas-build]
    steps:
      - run: echo ok
"""

CI_YML_HEAD_NO_DIFF = CI_YML_BASE  # identical to base, no flip


# --------------------------------------------------------------------------
# 1. CoE coercion (truthy/falsy/quoted/absent)
# --------------------------------------------------------------------------
class TestCoerceCoE(unittest.TestCase):
    def test_python_bool_true(self):
        self.assertTrue(lpfc._coerce_coe(True))

    def test_python_bool_false(self):
        self.assertFalse(lpfc._coerce_coe(False))

    def test_none_is_false(self):
        # GitHub Actions default: absent == false.
        self.assertFalse(lpfc._coerce_coe(None))

    def test_string_true_lowercase(self):
        # Quoted "true" in YAML — Gitea Actions normalizes to True.
        self.assertTrue(lpfc._coerce_coe("true"))

    def test_string_True_titlecase(self):
        self.assertTrue(lpfc._coerce_coe("True"))

    def test_string_yes(self):
        # YAML 1.1 truthy form.
        self.assertTrue(lpfc._coerce_coe("yes"))

    def test_string_false(self):
        self.assertFalse(lpfc._coerce_coe("false"))

    def test_string_random_falsy(self):
        # An unrecognized string is treated as falsy — safer than
        # silently coercing "maybe" to True and false-positiving a
        # flip.
        self.assertFalse(lpfc._coerce_coe("maybe"))


# --------------------------------------------------------------------------
# 2. Diff detection — flips, not arbitrary changes
# --------------------------------------------------------------------------
class TestDetectFlips(unittest.TestCase):
    def test_no_flip_in_diff_passes(self):
        # Acceptance test #1: PR doesn't flip continue-on-error → 0 flips.
        flips = lpfc.detect_flips(
            {".gitea/workflows/ci.yml": CI_YML_BASE},
            {".gitea/workflows/ci.yml": CI_YML_HEAD_NO_DIFF},
        )
        self.assertEqual(flips, [])

    def test_flip_detected_in_one_file(self):
        flips = lpfc.detect_flips(
            {".gitea/workflows/ci.yml": CI_YML_BASE},
            {".gitea/workflows/ci.yml": CI_YML_HEAD_FLIPPED},
        )
        # Two jobs flipped: platform-build, canvas-build. all-required
        # is still true on both sides.
        self.assertEqual(len(flips), 2)
        keys = sorted(f["job_key"] for f in flips)
        self.assertEqual(keys, ["canvas-build", "platform-build"])

    def test_context_name_render(self):
        flips = lpfc.detect_flips(
            {".gitea/workflows/ci.yml": CI_YML_BASE},
            {".gitea/workflows/ci.yml": CI_YML_HEAD_FLIPPED},
        )
        platform = next(f for f in flips if f["job_key"] == "platform-build")
        self.assertEqual(platform["context"], "CI / Platform (Go) (push)")
        self.assertEqual(platform["workflow_name"], "CI")

    def test_context_falls_back_to_job_key_when_no_name(self):
        base = "name: WF\njobs:\n  foo:\n    continue-on-error: true\n    runs-on: x\n    steps: []\n"
        head = "name: WF\njobs:\n  foo:\n    continue-on-error: false\n    runs-on: x\n    steps: []\n"
        flips = lpfc.detect_flips({"a.yml": base}, {"a.yml": head})
        self.assertEqual(len(flips), 1)
        self.assertEqual(flips[0]["context"], "WF / foo (push)")

    def test_no_flip_when_only_one_side_has_file(self):
        # Newly added workflow file — head has CoE:false, base has no
        # file. Adding a new workflow with CoE:false is fine; there's
        # nothing to mask.
        flips = lpfc.detect_flips(
            {},  # base has no workflow files
            {".gitea/workflows/new.yml": CI_YML_HEAD_FLIPPED},
        )
        self.assertEqual(flips, [])

    def test_no_flip_when_job_removed(self):
        # Job exists on base, not on head — a removal, not a flip.
        head = """\
name: CI
jobs:
  canvas-build:
    name: Canvas (Next.js)
    continue-on-error: true
    runs-on: ubuntu-latest
    steps: []
"""
        flips = lpfc.detect_flips(
            {".gitea/workflows/ci.yml": CI_YML_BASE},
            {".gitea/workflows/ci.yml": head},
        )
        self.assertEqual(flips, [])

    def test_no_flip_when_job_added_with_false(self):
        # New job on head with CoE:false — no base side; not a flip.
        head_with_new = CI_YML_BASE.replace(
            "  all-required:",
            "  newjob:\n    name: New Job\n    continue-on-error: false\n"
            "    runs-on: x\n    steps: []\n"
            "  all-required:",
        )
        flips = lpfc.detect_flips(
            {".gitea/workflows/ci.yml": CI_YML_BASE},
            {".gitea/workflows/ci.yml": head_with_new},
        )
        self.assertEqual(flips, [])

    def test_yaml_parse_error_warns_not_raises(self):
        # Malformed YAML on head — should warn (stderr) and skip,
        # not raise.
        bad_head = "name: CI\njobs:\n  :::\n"
        # Capture stderr so the test isn't noisy.
        with mock.patch.object(sys, "stderr"):
            flips = lpfc.detect_flips(
                {".gitea/workflows/ci.yml": CI_YML_BASE},
                {".gitea/workflows/ci.yml": bad_head},
            )
        self.assertEqual(flips, [])


# --------------------------------------------------------------------------
# 3. grep_fail_markers — the regex / substring matcher
# --------------------------------------------------------------------------
class TestGrepFailMarkers(unittest.TestCase):
    def test_clean_log_returns_empty(self):
        log = "===== test run starting =====\nPASS\nok  example.com/foo  1.234s\n"
        self.assertEqual(lpfc.grep_fail_markers(log), [])

    def test_go_minus_minus_minus_fail_caught(self):
        log = "ok  example.com/foo  1.234s\n--- FAIL: TestBar (0.01s)\n    bar_test.go:42:\n"
        matches = lpfc.grep_fail_markers(log)
        self.assertEqual(len(matches), 1)
        self.assertIn("FAIL: TestBar", matches[0])

    def test_go_package_fail_caught(self):
        log = "FAIL\texample.com/baz\t1.234s\n"
        matches = lpfc.grep_fail_markers(log)
        self.assertEqual(len(matches), 1)
        self.assertIn("FAIL", matches[0])

    def test_bash_error_directive_caught(self):
        # `lint-curl-status-capture` pattern: a python heredoc inside a
        # bash step that prints `::error::` then sys.exit(1). With
        # continue-on-error:true the job rolls up as success despite
        # this line. THAT's the masking we're trying to catch.
        log = "Running scan...\n::error::Found 3 curl-status-capture pollution site(s):\n"
        matches = lpfc.grep_fail_markers(log)
        self.assertEqual(len(matches), 1)
        self.assertIn("::error::", matches[0])

    def test_caps_matches_at_max_5(self):
        log = "\n".join(["--- FAIL: T%d" % i for i in range(20)])
        matches = lpfc.grep_fail_markers(log)
        self.assertEqual(len(matches), 5)


# --------------------------------------------------------------------------
# 4. verify_flip — single-flip verdict assembly (network surface stubbed)
# --------------------------------------------------------------------------
def _stub_status(context: str, state: str, target_url: str = "/owner/repo/actions/runs/1/jobs/0") -> dict:
    """Build a single-context combined-status response."""
    return {
        "state": state,
        "statuses": [
            {"context": context, "status": state, "target_url": target_url, "description": ""}
        ],
    }


FLIP_FIXTURE = {
    "workflow_path": ".gitea/workflows/ci.yml",
    "workflow_name": "CI",
    "job_key": "platform-build",
    "job_name": "Platform (Go)",
    "context": "CI / Platform (Go) (push)",
}


class TestVerifyFlip(unittest.TestCase):
    def test_flip_with_clean_history_passes(self):
        # Acceptance test #2: flip detected, last 5 runs clean → exit 0.
        with mock.patch.object(lpfc, "recent_commits_on_branch", return_value=["sha1", "sha2", "sha3"]):
            with mock.patch.object(
                lpfc, "combined_status",
                side_effect=[_stub_status(FLIP_FIXTURE["context"], "success") for _ in range(3)],
            ):
                with mock.patch.object(lpfc, "fetch_log", return_value="ok  example.com/foo  1s\nPASS\n"):
                    verdict = lpfc.verify_flip(FLIP_FIXTURE, "main", 5)
        self.assertEqual(verdict["fail_runs"], [])
        self.assertEqual(verdict["masked_runs"], [])
        self.assertEqual(verdict["checked_commits"], 3)
        self.assertEqual(verdict["warnings"], [])

    def test_flip_with_recent_fail_blocks(self):
        # Acceptance test #3: flip detected, recent run has --- FAIL → exit 1.
        # Setup: 3 commits, the most recent run's log shows --- FAIL
        # but the STATUS is success (Quirk #10 mask). That's the
        # masked_runs case.
        log_with_fail = "ok  example.com/foo  1s\n--- FAIL: TestSqlmock (0.01s)\n    sqlmock_test.go:42:\n"
        with mock.patch.object(lpfc, "recent_commits_on_branch", return_value=["sha1", "sha2", "sha3"]):
            with mock.patch.object(
                lpfc, "combined_status",
                side_effect=[_stub_status(FLIP_FIXTURE["context"], "success") for _ in range(3)],
            ):
                with mock.patch.object(lpfc, "fetch_log", side_effect=[log_with_fail, "PASS\n", "PASS\n"]):
                    verdict = lpfc.verify_flip(FLIP_FIXTURE, "main", 5)
        self.assertEqual(len(verdict["masked_runs"]), 1)
        self.assertEqual(verdict["masked_runs"][0]["sha"], "sha1")
        self.assertTrue(any("TestSqlmock" in s for s in verdict["masked_runs"][0]["samples"]))
        self.assertEqual(verdict["fail_runs"], [])

    def test_red_status_alone_blocks(self):
        # Status itself is `failure` — block without needing log
        # markers. (Belt-and-braces: even with a clean log, a `failure`
        # status means the job's exit code was non-zero.)
        with mock.patch.object(lpfc, "recent_commits_on_branch", return_value=["sha1"]):
            with mock.patch.object(
                lpfc, "combined_status",
                return_value=_stub_status(FLIP_FIXTURE["context"], "failure"),
            ):
                with mock.patch.object(lpfc, "fetch_log", return_value="some unrelated text\n"):
                    verdict = lpfc.verify_flip(FLIP_FIXTURE, "main", 5)
        self.assertEqual(len(verdict["fail_runs"]), 1)
        self.assertEqual(verdict["fail_runs"][0]["status"], "failure")

    def test_unreadable_log_on_success_blocks(self):
        # Fail-closed: log fetch 404 (None) on a success status is a
        # potential Quirk #10 mask — we cannot verify it's genuine, so
        # we block the flip rather than allowing it.
        with mock.patch.object(lpfc, "recent_commits_on_branch", return_value=["sha1"]):
            with mock.patch.object(
                lpfc, "combined_status",
                return_value=_stub_status(FLIP_FIXTURE["context"], "success"),
            ):
                with mock.patch.object(lpfc, "fetch_log", return_value=None):
                    verdict = lpfc.verify_flip(FLIP_FIXTURE, "main", 5)
        self.assertEqual(verdict["fail_runs"], [])
        self.assertEqual(len(verdict["masked_runs"]), 1)
        self.assertIn("log unavailable", verdict["masked_runs"][0]["samples"][0])
        self.assertTrue(any("log unavailable" in w for w in verdict["warnings"]))

    def test_unreadable_log_with_failure_status_still_blocks(self):
        # Edge case: log fetch fails BUT the status itself is `failure`.
        # We can still block — the status alone is sufficient signal,
        # we don't need the log to confirm.
        with mock.patch.object(lpfc, "recent_commits_on_branch", return_value=["sha1"]):
            with mock.patch.object(
                lpfc, "combined_status",
                return_value=_stub_status(FLIP_FIXTURE["context"], "failure"),
            ):
                with mock.patch.object(lpfc, "fetch_log", return_value=None):
                    verdict = lpfc.verify_flip(FLIP_FIXTURE, "main", 5)
        self.assertEqual(len(verdict["fail_runs"]), 1)
        self.assertIn("log unavailable", verdict["fail_runs"][0]["samples"][0])

    def test_zero_runs_history_allows_with_warning(self):
        # #107: commits exist but NONE carry a matching-context run (a dormant
        # push-only lane, a workflow_dispatch-only cron, or a paths-filtered job
        # whose paths were not touched). A zero-run job cannot be MASKING a
        # failure — there is nothing to mask — so the flip is ALLOWED with a
        # ::warning::, per the documented "no run history → warn + allow" intent.
        # (Negative control: before the #107 fix this appended to masked_runs and
        # blocked — see git history of verify_flip's checked_commits==0 branch.)
        with mock.patch.object(lpfc, "recent_commits_on_branch", return_value=["sha1", "sha2"]):
            with mock.patch.object(
                lpfc, "combined_status",
                return_value={"state": "success", "statuses": []},  # no matching context
            ):
                verdict = lpfc.verify_flip(FLIP_FIXTURE, "main", 5)
        self.assertEqual(verdict["checked_commits"], 0)
        self.assertEqual(verdict["fail_runs"], [])
        self.assertEqual(verdict["masked_runs"], [])          # NOT blocked
        self.assertEqual(len(verdict["warnings"]), 1)         # allowed, but surfaced
        self.assertIn("allowing flip", verdict["warnings"][0])

    def test_zero_commits_blocks(self):
        # Empty branch (newly created repo, e.g.). Fail-closed: block.
        with mock.patch.object(lpfc, "recent_commits_on_branch", return_value=[]):
            verdict = lpfc.verify_flip(FLIP_FIXTURE, "main", 5)
        self.assertEqual(verdict["checked_commits"], 0)
        self.assertEqual(verdict["fail_runs"], [])
        self.assertEqual(len(verdict["masked_runs"]), 1)
        self.assertIn("cannot verify flip", verdict["masked_runs"][0]["samples"][0])

    def test_combined_status_api_error_blocks(self):
        # Fail-closed: combined_status ApiError means the check history is
        # unreadable — we cannot verify the flip, so block as masked.
        with mock.patch.object(lpfc, "recent_commits_on_branch", return_value=["sha1"]):
            with mock.patch.object(
                lpfc, "combined_status",
                side_effect=lpfc.ApiError("GET /statuses/sha → HTTP 500"),
            ):
                verdict = lpfc.verify_flip(FLIP_FIXTURE, "main", 5)
        self.assertEqual(verdict["checked_commits"], 0)
        self.assertEqual(verdict["fail_runs"], [])
        # The ApiError itself is a hard fail-closed masked_run (an UNREADABLE
        # status is different from a verified-zero-run job — we cannot say it is
        # clean, so it must still block). The trailing checked_commits==0 goes to
        # warnings (#107), so masked_runs holds exactly the ApiError entry.
        self.assertEqual(len(verdict["masked_runs"]), 1)
        self.assertIn("API error", verdict["masked_runs"][0]["samples"][0])


# --------------------------------------------------------------------------
# 5. Multiple-flip aggregation in main()
# --------------------------------------------------------------------------
class TestMainAggregation(unittest.TestCase):
    """Tests that `main()` aggregates multiple flips and exits 1 when
    ANY one of them has a masked or red recent run. Acceptance test #4.

    We stub at the verify_flip + workflows_at_sha + _require_runtime_env
    boundary so we don't need real git or HTTP.
    """

    def setUp(self):
        # The actual env values are irrelevant — _require_runtime_env
        # is stubbed out — but the module reads OWNER/NAME at import
        # time. Patch the runtime env contract to a no-op for the
        # duration of each test.
        self._patches = [
            mock.patch.object(lpfc, "_require_runtime_env", return_value=None),
            mock.patch.object(lpfc, "BASE_REF", "main"),
            mock.patch.object(lpfc, "BASE_SHA", "deadbeefcafe"),
            mock.patch.object(lpfc, "HEAD_SHA", "feedfaceabad"),
            mock.patch.object(lpfc, "RECENT_COMMITS_N", 5),
        ]
        for p in self._patches:
            p.start()
        self.addCleanup(lambda: [p.stop() for p in self._patches])

    def test_multiple_flips_aggregated_one_bad_blocks(self):
        # PR flips 3 jobs; 1 has a recent fail → exit 1, naming that job.
        flips = [
            {"workflow_path": ".gitea/workflows/ci.yml", "workflow_name": "CI",
             "job_key": "platform-build", "job_name": "Platform (Go)",
             "context": "CI / Platform (Go) (push)"},
            {"workflow_path": ".gitea/workflows/ci.yml", "workflow_name": "CI",
             "job_key": "canvas-build", "job_name": "Canvas (Next.js)",
             "context": "CI / Canvas (Next.js) (push)"},
            {"workflow_path": ".gitea/workflows/ci.yml", "workflow_name": "CI",
             "job_key": "python-lint", "job_name": "Python Lint & Test",
             "context": "CI / Python Lint & Test (push)"},
        ]
        clean = {"flip": flips[0], "checked_commits": 5, "masked_runs": [],
                 "fail_runs": [], "warnings": []}
        bad = {"flip": flips[1], "checked_commits": 5,
               "masked_runs": [{"sha": "abc1234567", "status": "success",
                                "target_url": "/x/y/actions/runs/1/jobs/0",
                                "samples": ["--- FAIL: TestSqlmock"]}],
               "fail_runs": [], "warnings": []}
        also_clean = {"flip": flips[2], "checked_commits": 5, "masked_runs": [],
                      "fail_runs": [], "warnings": []}

        with mock.patch.object(lpfc, "workflows_at_sha", return_value={}):
            with mock.patch.object(lpfc, "detect_flips", return_value=flips):
                with mock.patch.object(lpfc, "verify_flip",
                                       side_effect=[clean, bad, also_clean]):
                    # Capture stdout to assert on naming.
                    captured = []
                    with mock.patch("builtins.print", side_effect=lambda *a, **k: captured.append(" ".join(str(x) for x in a))):
                        rc = lpfc.main([])
        self.assertEqual(rc, 1)
        # The blocking error message must name the failing job.
        joined = "\n".join(captured)
        self.assertIn("canvas-build", joined)
        # And it must mention the empirical class so a reviewer can
        # cross-link the right RFC.
        self.assertTrue("mc#664" in joined or "PR#656" in joined)

    def test_no_flips_in_diff_exits_zero(self):
        # Acceptance test #1 at main() level: empty flips → exit 0.
        with mock.patch.object(lpfc, "workflows_at_sha", return_value={}):
            with mock.patch.object(lpfc, "detect_flips", return_value=[]):
                rc = lpfc.main([])
        self.assertEqual(rc, 0)

    def test_all_flips_clean_exits_zero(self):
        flips = [{"workflow_path": ".gitea/workflows/ci.yml", "workflow_name": "CI",
                  "job_key": "platform-build", "job_name": "Platform (Go)",
                  "context": "CI / Platform (Go) (push)"}]
        clean = {"flip": flips[0], "checked_commits": 5, "masked_runs": [],
                 "fail_runs": [], "warnings": []}
        with mock.patch.object(lpfc, "workflows_at_sha", return_value={}):
            with mock.patch.object(lpfc, "detect_flips", return_value=flips):
                with mock.patch.object(lpfc, "verify_flip", return_value=clean):
                    rc = lpfc.main([])
        self.assertEqual(rc, 0)

    def test_dry_run_forces_exit_zero_even_with_bad_flip(self):
        # --dry-run never fails, even when verification finds masked runs.
        flips = [{"workflow_path": ".gitea/workflows/ci.yml", "workflow_name": "CI",
                  "job_key": "platform-build", "job_name": "Platform (Go)",
                  "context": "CI / Platform (Go) (push)"}]
        bad = {"flip": flips[0], "checked_commits": 5,
               "masked_runs": [{"sha": "abc1234567", "status": "success",
                                "target_url": "/x/y/actions/runs/1/jobs/0",
                                "samples": ["--- FAIL: TestSqlmock"]}],
               "fail_runs": [], "warnings": []}
        with mock.patch.object(lpfc, "workflows_at_sha", return_value={}):
            with mock.patch.object(lpfc, "detect_flips", return_value=flips):
                with mock.patch.object(lpfc, "verify_flip", return_value=bad):
                    rc = lpfc.main(["--dry-run"])
        self.assertEqual(rc, 0)


# --------------------------------------------------------------------------
# 6. Context-name rendering (the format Gitea Actions actually emits)
# --------------------------------------------------------------------------
class TestContextName(unittest.TestCase):
    def test_push_event(self):
        self.assertEqual(
            lpfc.context_name("CI", "Platform (Go)", "push"),
            "CI / Platform (Go) (push)",
        )

    def test_pull_request_event(self):
        self.assertEqual(
            lpfc.context_name("CI", "Platform (Go)", "pull_request"),
            "CI / Platform (Go) (pull_request)",
        )

    def test_workflow_name_falls_back_to_filename(self):
        # No top-level `name:` → falls back to filename minus extension.
        doc = {"jobs": {"foo": {"continue-on-error": True}}}
        self.assertEqual(
            lpfc.workflow_name(doc, fallback="my-workflow"),
            "my-workflow",
        )


# --------------------------------------------------------------------------
# 7. #106 — PAGINATED per-commit statuses (combined /status caps at 30)
# --------------------------------------------------------------------------
class TestPaginatedCommitStatuses(unittest.TestCase):
    """all_commit_statuses() must walk the paginated /statuses endpoint and
    surface EVERY context, including ones that sort past the 30-entry cap of
    the combined /status endpoint (#106). Negative-controlled: the old
    un-paginated fetch (a single page / the 30-capped combined endpoint) would
    NOT contain a context at position 40, so this test reds against it.
    """

    #: 60 contexts: ctx-00 .. ctx-59, each a success entry.
    NUM_CONTEXTS = 60

    def _paged_api(self):
        """Return a fake `api()` that serves the 60 contexts across pages of
        STATUS_PAGE_LIMIT, recording which pages were requested."""
        all_entries = [
            {"context": f"ctx-{i:02d}", "status": "success",
             "target_url": f"/o/r/actions/runs/{i}/jobs/0",
             "updated_at": f"2026-07-20T00:{i:02d}:00Z", "id": i}
            for i in range(self.NUM_CONTEXTS)
        ]
        pages_seen = []

        def fake_api(method, path, *, body=None, query=None):
            self.assertEqual(method, "GET")
            self.assertIn("/statuses", path)
            page = int((query or {}).get("page", "1"))
            limit = int((query or {}).get("limit", str(lpfc.STATUS_PAGE_LIMIT)))
            pages_seen.append(page)
            start = (page - 1) * limit
            return 200, all_entries[start:start + limit]

        return fake_api, pages_seen, all_entries

    def test_reads_context_beyond_position_30(self):
        fake_api, pages_seen, all_entries = self._paged_api()
        with mock.patch.object(lpfc, "api", side_effect=fake_api):
            out = lpfc.all_commit_statuses("deadbeef")
        contexts = {s["context"] for s in out}
        # The whole point of #106: a context at position 40 (past the 30 cap)
        # must be present.
        self.assertIn("ctx-40", contexts)
        # And one that only exists on page 2 (position 55 with limit 50) — this
        # proves the pagination LOOP ran, not just a single wide page.
        self.assertIn("ctx-55", contexts)
        self.assertEqual(len(contexts), self.NUM_CONTEXTS)
        # More than one page was fetched.
        self.assertIn(2, pages_seen)

        # NEGATIVE CONTROL: the old un-paginated fetch returned only the first
        # 30 (combined /status cap). Prove ctx-40 is genuinely unreachable that
        # way, so the assertion above is load-bearing and would red against the
        # pre-fix code.
        old_capped = {s["context"] for s in all_entries[:30]}
        self.assertNotIn("ctx-40", old_capped)

    def test_stops_on_short_first_page(self):
        # Fewer than a full page → exactly one request, no needless page 2.
        entries = [{"context": f"c{i}", "status": "success",
                    "target_url": "", "updated_at": f"t{i}", "id": i}
                   for i in range(5)]
        calls = []

        def fake_api(method, path, *, body=None, query=None):
            calls.append((query or {}).get("page"))
            return 200, entries

        with mock.patch.object(lpfc, "api", side_effect=fake_api):
            out = lpfc.all_commit_statuses("sha")
        self.assertEqual(len(out), 5)
        self.assertEqual(calls, ["1"])  # only page 1

    def test_latest_per_context_dedup(self):
        # The /statuses list endpoint returns ALL historical statuses, NOT
        # newest-first. Reduce to latest-per-context by updated_at so a stale
        # `pending` never shadows a later `success`.
        entries = [
            {"context": "CI / X (push)", "status": "pending",
             "target_url": "/a", "updated_at": "2026-07-20T00:00:00Z", "id": 1},
            {"context": "CI / X (push)", "status": "success",
             "target_url": "/b", "updated_at": "2026-07-20T00:05:00Z", "id": 2},
        ]

        def fake_api(method, path, *, body=None, query=None):
            return 200, entries

        with mock.patch.object(lpfc, "api", side_effect=fake_api):
            out = lpfc.all_commit_statuses("sha")
        self.assertEqual(len(out), 1)
        self.assertEqual(out[0]["status"], "success")
        self.assertEqual(out[0]["target_url"], "/b")

    def test_non_list_body_raises(self):
        def fake_api(method, path, *, body=None, query=None):
            return 200, {"not": "a list"}

        with mock.patch.object(lpfc, "api", side_effect=fake_api):
            with self.assertRaises(lpfc.ApiError):
                lpfc.all_commit_statuses("sha")


# --------------------------------------------------------------------------
# 8. #107 — paths-filtered workflows accept pull_request evidence
# --------------------------------------------------------------------------
class TestWorkflowIsPathsFiltered(unittest.TestCase):
    def test_push_paths_is_filtered(self):
        # Bare `on:` — PyYAML parses the key as the boolean True; the helper
        # must still find the trigger config.
        doc = lpfc.yaml.safe_load(
            "name: WF\non:\n  push:\n    branches: [main]\n    paths: ['x/**']\n"
            "jobs:\n  j:\n    continue-on-error: true\n"
        )
        self.assertTrue(lpfc.workflow_is_paths_filtered(doc))

    def test_pull_request_paths_ignore_is_filtered(self):
        doc = lpfc.yaml.safe_load(
            "name: WF\non:\n  pull_request:\n    paths-ignore: ['docs/**']\n"
            "jobs:\n  j:\n    continue-on-error: true\n"
        )
        self.assertTrue(lpfc.workflow_is_paths_filtered(doc))

    def test_no_paths_is_not_filtered(self):
        doc = lpfc.yaml.safe_load(
            "name: WF\non:\n  push:\n    branches: [main]\n"
            "jobs:\n  j:\n    continue-on-error: true\n"
        )
        self.assertFalse(lpfc.workflow_is_paths_filtered(doc))

    def test_on_as_list_is_not_filtered(self):
        doc = lpfc.yaml.safe_load(
            "name: WF\non: [push, pull_request]\n"
            "jobs:\n  j:\n    continue-on-error: true\n"
        )
        self.assertFalse(lpfc.workflow_is_paths_filtered(doc))

    def test_quoted_on_key_still_works(self):
        # If someone quotes "on": the key is the string "on" not True.
        doc = lpfc.yaml.safe_load(
            'name: WF\n"on":\n  push:\n    paths: [\'x/**\']\n'
            "jobs:\n  j:\n    continue-on-error: true\n"
        )
        self.assertTrue(lpfc.workflow_is_paths_filtered(doc))


PATHS_FILTERED_BASE = """\
name: Secret Pattern Drift
on:
  push:
    branches: [main]
    paths: ['.gitea/workflows/**']
  pull_request:
    paths: ['.gitea/workflows/**']
jobs:
  scan:
    name: scan
    runs-on: ubuntu-latest
    continue-on-error: true
    steps:
      - run: echo scan
"""

PATHS_FILTERED_HEAD = PATHS_FILTERED_BASE.replace(
    "continue-on-error: true", "continue-on-error: false"
)


class TestDetectFlipsPathsFiltered(unittest.TestCase):
    def test_paths_filtered_flip_accepts_both_contexts(self):
        flips = lpfc.detect_flips(
            {".gitea/workflows/secret-pattern-drift.yml": PATHS_FILTERED_BASE},
            {".gitea/workflows/secret-pattern-drift.yml": PATHS_FILTERED_HEAD},
        )
        self.assertEqual(len(flips), 1)
        f = flips[0]
        self.assertTrue(f["paths_filtered"])
        self.assertEqual(
            sorted(f["accept_contexts"]),
            ["Secret Pattern Drift / scan (pull_request)",
             "Secret Pattern Drift / scan (push)"],
        )
        # Back-compat: the primary context is still the push one.
        self.assertEqual(f["context"], "Secret Pattern Drift / scan (push)")

    def test_non_paths_filtered_flip_accepts_only_push(self):
        flips = lpfc.detect_flips(
            {".gitea/workflows/ci.yml": CI_YML_BASE},
            {".gitea/workflows/ci.yml": CI_YML_HEAD_FLIPPED},
        )
        for f in flips:
            self.assertFalse(f["paths_filtered"])
            self.assertEqual(f["accept_contexts"], [f["context"]])


PATHS_FLIP_FIXTURE = {
    "workflow_path": ".gitea/workflows/secret-pattern-drift.yml",
    "workflow_name": "Secret Pattern Drift",
    "job_key": "scan",
    "job_name": "scan",
    "context": "Secret Pattern Drift / scan (push)",
    "accept_contexts": [
        "Secret Pattern Drift / scan (push)",
        "Secret Pattern Drift / scan (pull_request)",
    ],
    "paths_filtered": True,
}


class TestVerifyFlipPathsFiltered(unittest.TestCase):
    def test_pr_only_green_run_blesses_flip(self):
        # #107 catch-22 resolution: a paths-filtered workflow never fires on a
        # main PUSH, so only a pull_request context exists on recent commits.
        # A green PR run with a clean log MUST now bless the flip.
        pr_ctx = "Secret Pattern Drift / scan (pull_request)"
        with mock.patch.object(lpfc, "recent_commits_on_branch", return_value=["sha1"]):
            with mock.patch.object(
                lpfc, "combined_status",
                return_value=_stub_status(pr_ctx, "success"),
            ):
                with mock.patch.object(lpfc, "fetch_log", return_value="PASS\nok\n"):
                    verdict = lpfc.verify_flip(PATHS_FLIP_FIXTURE, "main", 5)
        self.assertEqual(verdict["checked_commits"], 1)
        self.assertEqual(verdict["fail_runs"], [])
        self.assertEqual(verdict["masked_runs"], [])

    def test_pr_run_with_masked_log_still_blocks(self):
        # Safety preserved: a PR-event run whose log shows --- FAIL despite a
        # success status is still a Quirk #10 mask → block.
        pr_ctx = "Secret Pattern Drift / scan (pull_request)"
        with mock.patch.object(lpfc, "recent_commits_on_branch", return_value=["sha1"]):
            with mock.patch.object(
                lpfc, "combined_status",
                return_value=_stub_status(pr_ctx, "success"),
            ):
                with mock.patch.object(
                    lpfc, "fetch_log",
                    return_value="--- FAIL: TestSecretScan (0.01s)\n",
                ):
                    verdict = lpfc.verify_flip(PATHS_FLIP_FIXTURE, "main", 5)
        self.assertEqual(len(verdict["masked_runs"]), 1)
        self.assertTrue(any("TestSecretScan" in s for s in verdict["masked_runs"][0]["samples"]))

    def test_both_contexts_on_one_commit_masked_pr_blocks(self):
        # A commit carries BOTH a clean push context AND a masked pull_request
        # context. We must NOT stop at the first (clean push) match — the
        # masked PR run has to block. Proves the removed `break`.
        push_ctx = "Secret Pattern Drift / scan (push)"
        pr_ctx = "Secret Pattern Drift / scan (pull_request)"
        both = {
            "state": "success",
            "statuses": [
                {"context": push_ctx, "status": "success",
                 "target_url": "/o/r/actions/runs/1/jobs/0"},
                {"context": pr_ctx, "status": "success",
                 "target_url": "/o/r/actions/runs/2/jobs/0"},
            ],
        }
        # push log clean, PR log masked.
        logs = {"/o/r/actions/runs/1/jobs/0": "PASS\n",
                "/o/r/actions/runs/2/jobs/0": "--- FAIL: TestX\n"}
        with mock.patch.object(lpfc, "recent_commits_on_branch", return_value=["sha1"]):
            with mock.patch.object(lpfc, "combined_status", return_value=both):
                with mock.patch.object(
                    lpfc, "fetch_log",
                    side_effect=lambda url: logs[url],
                ):
                    verdict = lpfc.verify_flip(PATHS_FLIP_FIXTURE, "main", 5)
        self.assertEqual(verdict["checked_commits"], 2)  # both contexts evaluated
        self.assertEqual(len(verdict["masked_runs"]), 1)
        self.assertTrue(any("TestX" in s for s in verdict["masked_runs"][0]["samples"]))

    def test_zero_runs_message_names_all_accept_contexts(self):
        # #107: a paths-filtered flip with no matching runs is ALLOWED (warning),
        # and the warning must name every accepted context so the reader can see
        # both the push and pull_request contexts were checked.
        with mock.patch.object(lpfc, "recent_commits_on_branch", return_value=["sha1"]):
            with mock.patch.object(
                lpfc, "combined_status",
                return_value={"state": "success", "statuses": []},
            ):
                verdict = lpfc.verify_flip(PATHS_FLIP_FIXTURE, "main", 5)
        self.assertEqual(verdict["masked_runs"], [])          # allowed, not blocked
        msg = verdict["warnings"][0]
        self.assertIn("pull_request", msg)
        self.assertIn("allowing flip", msg)


# --------------------------------------------------------------------------
# 9. Zero-run outcome must be SAFE, not blanket-allow — the #4524 fail-open fix
#    (findings [2]/[6]/[7]): allow ONLY when the workflow cannot gate a PR.
# --------------------------------------------------------------------------
class TestWorkflowGatesPr(unittest.TestCase):
    """workflow_gates_pr() decides the zero-run branch: a workflow that
    triggers on pull_request/pull_request_target CAN post a merge-blocking
    status, so zero matches there is could-not-verify (fail-closed); one that
    can't gate a PR is safe to allow on zero runs."""

    def test_pull_request_mapping_gates(self):
        doc = lpfc.yaml.safe_load(
            "name: WF\non:\n  pull_request:\n    branches: [main]\n"
            "jobs:\n  j:\n    continue-on-error: true\n"
        )
        self.assertTrue(lpfc.workflow_gates_pr(doc))

    def test_pull_request_target_mapping_gates(self):
        doc = lpfc.yaml.safe_load(
            "name: WF\non:\n  pull_request_target:\n    branches: [main]\n"
            "jobs:\n  j:\n    continue-on-error: true\n"
        )
        self.assertTrue(lpfc.workflow_gates_pr(doc))

    def test_list_form_with_pull_request_gates(self):
        doc = lpfc.yaml.safe_load(
            "name: WF\non: [push, pull_request]\n"
            "jobs:\n  j:\n    continue-on-error: true\n"
        )
        self.assertTrue(lpfc.workflow_gates_pr(doc))

    def test_push_only_does_not_gate(self):
        doc = lpfc.yaml.safe_load(
            "name: WF\non:\n  push:\n    branches: [main]\n"
            "jobs:\n  j:\n    continue-on-error: true\n"
        )
        self.assertFalse(lpfc.workflow_gates_pr(doc))

    def test_dispatch_and_schedule_only_does_not_gate(self):
        # A workflow_dispatch-only cron cannot post a PR-blocking status.
        doc = lpfc.yaml.safe_load(
            "name: Nightly\non:\n  workflow_dispatch: {}\n  schedule:\n"
            "    - cron: '0 3 * * *'\n"
            "jobs:\n  j:\n    continue-on-error: true\n"
        )
        self.assertFalse(lpfc.workflow_gates_pr(doc))

    def test_scalar_pull_request_gates(self):
        doc = lpfc.yaml.safe_load(
            "name: WF\non: pull_request\n"
            "jobs:\n  j:\n    continue-on-error: true\n"
        )
        self.assertTrue(lpfc.workflow_gates_pr(doc))


# A PR-gating (on: pull_request) workflow whose flip carries gates_pr=True.
PR_GATING_FLIP_FIXTURE = {
    "workflow_path": ".gitea/workflows/ci.yml",
    "workflow_name": "CI",
    "job_key": "platform-build",
    "job_name": "Platform (Go)",
    "context": "CI / Platform (Go) (push)",
    "accept_contexts": ["CI / Platform (Go) (push)"],
    "paths_filtered": False,
    "gates_pr": True,
}

# A workflow_dispatch/push-only cron whose flip carries gates_pr=False.
DISPATCH_ONLY_FLIP_FIXTURE = {
    "workflow_path": ".gitea/workflows/nightly.yml",
    "workflow_name": "Nightly",
    "job_key": "sweep",
    "job_name": "Sweep",
    "context": "Nightly / Sweep (push)",
    "accept_contexts": ["Nightly / Sweep (push)"],
    "paths_filtered": False,
    "gates_pr": False,
}


class TestZeroRunFailOpenFix(unittest.TestCase):
    def test_a_pr_gating_zero_match_namemismatch_blocks(self):
        # Finding [2]: the flipped job IS running and RED, but its real Gitea
        # context ("CI / platform-build (ubuntu-latest) (push)" — a matrix
        # display name) does NOT byte-match the accepted context
        # ("CI / Platform (Go) (push)"). That produces ZERO matching statuses.
        # Because the workflow gates PRs, zero matches is could-not-verify, not
        # no-coverage — the flip MUST fail-closed (block), or the still-red job
        # gets un-masked onto main.
        namemismatch_red = {
            "state": "failure",
            "statuses": [
                {"context": "CI / platform-build (ubuntu-latest) (push)",
                 "status": "failure",
                 "target_url": "/o/r/actions/runs/9/jobs/0"},
            ],
        }
        with mock.patch.object(lpfc, "recent_commits_on_branch", return_value=["sha1", "sha2"]):
            with mock.patch.object(lpfc, "combined_status", return_value=namemismatch_red):
                verdict = lpfc.verify_flip(PR_GATING_FLIP_FIXTURE, "main", 5)
        # No context matched → nothing verified.
        self.assertEqual(verdict["checked_commits"], 0)
        # ...and because the workflow gates PRs, that BLOCKS.
        self.assertEqual(verdict["fail_runs"], [])
        self.assertEqual(len(verdict["masked_runs"]), 1)
        self.assertIn("COULD-NOT-VERIFY", verdict["masked_runs"][0]["samples"][0])

        # NEGATIVE CONTROL: the #4524 code had no gates_pr distinction and
        # blanket-ALLOWED any zero-match flip. Reproduce that pre-fix path by
        # dropping gates_pr from the record and prove the SAME red, name-
        # mismatched job is (wrongly) allowed — i.e. this test genuinely reds
        # against the pre-fix behavior.
        legacy = dict(PR_GATING_FLIP_FIXTURE)
        legacy.pop("gates_pr")
        with mock.patch.object(lpfc, "recent_commits_on_branch", return_value=["sha1", "sha2"]):
            with mock.patch.object(lpfc, "combined_status", return_value=namemismatch_red):
                legacy_verdict = lpfc.verify_flip(legacy, "main", 5)
        self.assertEqual(legacy_verdict["masked_runs"], [])   # pre-fix: allowed
        self.assertTrue(any("allowing flip" in w for w in legacy_verdict["warnings"]))

    def test_b_dispatch_only_cron_zero_runs_allows(self):
        # A workflow_dispatch-only cron cannot post a merge-blocking status on a
        # PR, so a flip on it with zero run evidence is genuinely safe → ALLOW
        # (warning, not block). This is the SAFE half of the fix.
        with mock.patch.object(lpfc, "recent_commits_on_branch", return_value=["sha1", "sha2"]):
            with mock.patch.object(
                lpfc, "combined_status",
                return_value={"state": "success", "statuses": []},  # zero runs
            ):
                verdict = lpfc.verify_flip(DISPATCH_ONLY_FLIP_FIXTURE, "main", 5)
        self.assertEqual(verdict["checked_commits"], 0)
        self.assertEqual(verdict["fail_runs"], [])
        self.assertEqual(verdict["masked_runs"], [])          # NOT blocked
        self.assertEqual(len(verdict["warnings"]), 1)
        self.assertIn("does not gate PRs", verdict["warnings"][0])
        self.assertIn("allowing flip", verdict["warnings"][0])

    def test_detect_flips_sets_gates_pr(self):
        # End-to-end: detect_flips must derive gates_pr from the head workflow's
        # triggers so verify_flip's zero-run branch is fed the right value.
        # CI_YML_* is on: push (no PR trigger) → gates_pr False.
        push_flips = lpfc.detect_flips(
            {".gitea/workflows/ci.yml": CI_YML_BASE},
            {".gitea/workflows/ci.yml": CI_YML_HEAD_FLIPPED},
        )
        self.assertTrue(all(f["gates_pr"] is False for f in push_flips))
        # PATHS_FILTERED_* is on: push + pull_request → gates_pr True.
        pr_flips = lpfc.detect_flips(
            {".gitea/workflows/secret-pattern-drift.yml": PATHS_FILTERED_BASE},
            {".gitea/workflows/secret-pattern-drift.yml": PATHS_FILTERED_HEAD},
        )
        self.assertEqual(len(pr_flips), 1)
        self.assertTrue(pr_flips[0]["gates_pr"])


PATHS_PR_HEAD_FLIP_FIXTURE = {
    "workflow_path": ".gitea/workflows/secret-pattern-drift.yml",
    "workflow_name": "Secret Pattern Drift",
    "job_key": "scan",
    "job_name": "scan",
    "context": "Secret Pattern Drift / scan (push)",
    "accept_contexts": [
        "Secret Pattern Drift / scan (push)",
        "Secret Pattern Drift / scan (pull_request)",
    ],
    "paths_filtered": True,
    "gates_pr": True,
}


class TestVerifyFlipPrHeadSampling(unittest.TestCase):
    def test_c_paths_filtered_verified_via_pr_head(self):
        # Finding [7]: a paths-filtered workflow never fires on a plain push to
        # main, so recent main commits carry NO run for it — the pull_request
        # run lives only on the PR-HEAD sha. verify_flip must sample the PR head
        # and, seeing a GREEN pull_request run there with a clean log, bless the
        # flip (checked_commits==1). Without PR-head sampling this would fall
        # into the zero-run branch and — since the workflow gates PRs — BLOCK,
        # which is the catch-22 finding [7] describes.
        PR_HEAD = "prhead0123456789"
        pr_ctx = "Secret Pattern Drift / scan (pull_request)"

        def fake_combined(sha):
            if sha == PR_HEAD:
                return _stub_status(pr_ctx, "success",
                                    target_url="/o/r/actions/runs/7/jobs/0")
            return {"state": "success", "statuses": []}  # main commits: no runs

        with mock.patch.object(lpfc, "recent_commits_on_branch", return_value=["m1", "m2"]):
            with mock.patch.object(lpfc, "combined_status", side_effect=fake_combined):
                with mock.patch.object(lpfc, "fetch_log", return_value="PASS\nok\n"):
                    verdict = lpfc.verify_flip(
                        PATHS_PR_HEAD_FLIP_FIXTURE, "main", 5, pr_head_sha=PR_HEAD,
                    )
        # The PR-head run was seen and verified.
        self.assertEqual(verdict["checked_commits"], 1)
        self.assertEqual(verdict["fail_runs"], [])
        self.assertEqual(verdict["masked_runs"], [])          # blessed, not blocked

    def test_c_paths_filtered_masked_pr_head_blocks(self):
        # Companion to the positive case: PR-head sampling is a REAL verification,
        # not a rubber-stamp. A PR-head pull_request run whose log shows --- FAIL
        # despite a success status (Quirk #10 mask) must still BLOCK.
        PR_HEAD = "prhead0123456789"
        pr_ctx = "Secret Pattern Drift / scan (pull_request)"

        def fake_combined(sha):
            if sha == PR_HEAD:
                return _stub_status(pr_ctx, "success",
                                    target_url="/o/r/actions/runs/7/jobs/0")
            return {"state": "success", "statuses": []}

        with mock.patch.object(lpfc, "recent_commits_on_branch", return_value=["m1", "m2"]):
            with mock.patch.object(lpfc, "combined_status", side_effect=fake_combined):
                with mock.patch.object(
                    lpfc, "fetch_log",
                    return_value="--- FAIL: TestSecretScan (0.02s)\n",
                ):
                    verdict = lpfc.verify_flip(
                        PATHS_PR_HEAD_FLIP_FIXTURE, "main", 5, pr_head_sha=PR_HEAD,
                    )
        self.assertEqual(verdict["checked_commits"], 1)
        self.assertEqual(len(verdict["masked_runs"]), 1)
        self.assertTrue(
            any("TestSecretScan" in s for s in verdict["masked_runs"][0]["samples"])
        )

    def test_c_pr_head_not_sampled_for_non_paths_filtered(self):
        # PR-head sampling is scoped to paths-filtered flips (their evidence
        # structurally lives on the PR head). A normal push-lane flip must NOT
        # pull the PR head — its evidence is the main-push run. Prove the PR
        # head is never fetched for a non-paths-filtered flip.
        PR_HEAD = "prhead0123456789"
        seen = []

        def fake_combined(sha):
            seen.append(sha)
            return _stub_status(PR_GATING_FLIP_FIXTURE["context"], "success")

        with mock.patch.object(lpfc, "recent_commits_on_branch", return_value=["m1"]):
            with mock.patch.object(lpfc, "combined_status", side_effect=fake_combined):
                with mock.patch.object(lpfc, "fetch_log", return_value="PASS\n"):
                    lpfc.verify_flip(
                        PR_GATING_FLIP_FIXTURE, "main", 5, pr_head_sha=PR_HEAD,
                    )
        self.assertNotIn(PR_HEAD, seen)


if __name__ == "__main__":
    unittest.main()
