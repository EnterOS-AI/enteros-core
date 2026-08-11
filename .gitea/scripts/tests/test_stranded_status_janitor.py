"""Unit tests for `.gitea/scripts/stranded_status_janitor.py`.

Drives the acceptance cases from molecule-core #4979 plus the safety
invariants the janitor documents: it must heal a genuinely stranded context by
RE-RUNNING, must never fabricate a commit status, and must leave anything
still in flight alone.

Written unittest-style so it runs under both `python -m unittest discover` and
pytest.
"""

import datetime
import importlib.util
import sys
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[3]
SCRIPT = ROOT / ".gitea" / "scripts" / "stranded_status_janitor.py"

_spec = importlib.util.spec_from_file_location("stranded_status_janitor", SCRIPT)
janitor = importlib.util.module_from_spec(_spec)
sys.modules["stranded_status_janitor"] = janitor
_spec.loader.exec_module(janitor)


NOW = datetime.datetime(2026, 8, 3, 12, 0, 0, tzinfo=datetime.timezone.utc)


def ts(minutes_ago):
    stamp = NOW - datetime.timedelta(minutes=minutes_ago)
    return stamp.strftime("%Y-%m-%dT%H:%M:%SZ")


def row(row_id, status, context, minutes_ago=60, run=606639, job=897893, description=""):
    target = ""
    if run is not None:
        target = "/molecule-ai/molecule-core/actions/runs/%d/jobs/%d" % (run, job)
    return {
        "id": row_id,
        "status": status,
        "context": context,
        "updated_at": ts(minutes_ago),
        "target_url": target,
        "description": description,
    }


def job(job_id=897893, status="completed", conclusion="success", attempt=1):
    return {
        "id": job_id,
        "status": status,
        "conclusion": conclusion,
        "run_attempt": attempt,
    }


def run_obj(run_id=606639, status="completed", conclusion="success"):
    return {"id": run_id, "status": status, "conclusion": conclusion}


class Args:
    """Stand-in for the argparse namespace `sweep_sha` consumes."""

    def __init__(self, min_age_minutes=15, max_attempts=4, verbose=False):
        self.min_age_minutes = min_age_minutes
        self.max_attempts = max_attempts
        self.verbose = verbose


class FakeClient:
    """Records every call so a test can assert nothing but GET/rerun happened."""

    def __init__(self, statuses, runs, jobs):
        self._statuses = statuses
        self._runs = runs
        self._jobs = jobs
        self.calls = []
        self.reruns = []

    def latest_statuses(self, sha, **_kwargs):
        # Mirrors the real client: already reduced to one row per context.
        self.calls.append(("GET", "combined-status", sha))
        return list(janitor.latest_status_per_context(self._statuses.get(sha, [])).values())

    def run(self, run_id):
        self.calls.append(("GET", "run", run_id))
        return self._runs.get(run_id)

    def run_jobs(self, run_id):
        self.calls.append(("GET", "jobs", run_id))
        return list(self._jobs.get(run_id, []))

    def rerun(self, run_id):
        self.calls.append(("POST", "rerun", run_id))
        self.reruns.append(run_id)
        return 200, None


class TestLatestStatusPerContext(unittest.TestCase):
    def test_reduces_to_newest_row_not_first_row(self):
        # Response order is NOT newest-first here: the stale success is listed
        # before the live pending. Taking row[0] would report a false green.
        rows = [
            row(74, "success", "nodup-lint / no-duplication", minutes_ago=90),
            row(126, "pending", "nodup-lint / no-duplication", minutes_ago=30),
        ]
        latest = janitor.latest_status_per_context(rows)
        self.assertEqual(latest["nodup-lint / no-duplication"]["id"], 126)
        self.assertEqual(latest["nodup-lint / no-duplication"]["status"], "pending")

    def test_id_breaks_whole_second_updated_at_ties(self):
        # Measured on molecule-controlplane 8ed088a6: a pending and a success
        # for one context can share an updated_at to the second. The higher id
        # is the later insertion, matching Gitea's own max(id) reducer.
        rows = [
            row(118, "success", "Secret scan / Scan", minutes_ago=60),
            row(115, "pending", "Secret scan / Scan", minutes_ago=60),
        ]
        latest = janitor.latest_status_per_context(rows)
        self.assertEqual(latest["Secret scan / Scan"]["id"], 118)
        self.assertEqual(latest["Secret scan / Scan"]["status"], "success")

    def test_multiple_contexts_are_reduced_independently(self):
        rows = [
            row(1, "pending", "a / a", minutes_ago=90),
            row(2, "success", "a / a", minutes_ago=80),
            row(3, "pending", "b / b", minutes_ago=80),
        ]
        latest = janitor.latest_status_per_context(rows)
        self.assertEqual(latest["a / a"]["status"], "success")
        self.assertEqual(latest["b / b"]["status"], "pending")


class TestParseRunAndJob(unittest.TestCase):
    def test_parses_run_and_job(self):
        self.assertEqual(
            janitor.parse_run_and_job(
                "/molecule-ai/molecule-core/actions/runs/606639/jobs/897893"
            ),
            (606639, 897893),
        )

    def test_no_target_url(self):
        self.assertEqual(janitor.parse_run_and_job(""), (None, None))
        self.assertEqual(janitor.parse_run_and_job(None), (None, None))


class TestClassifyContext(unittest.TestCase):
    def classify(self, status_row, run, jobs, min_age=15, max_attempts=4):
        return janitor.classify_context(
            status_row, run, jobs, NOW, min_age, max_attempts
        )

    def test_phantom_job_pending_over_completed_run_is_stranded(self):
        # Measured on molecule-core 6ac234e4: the pending row names run 605222
        # but job 895694, which is NOT a member of run 605222 (it belongs to
        # run 604895, on a different SHA). The janitor does not need to detect
        # the phantom-ness — the run is completed and the context is still
        # pending, which is sufficient and safe.
        verdict, _ = self.classify(
            row(182, "pending", "staging-tenant-cd / advance-pin", run=605222, job=895694),
            run_obj(605222),
            [job(896159), job(896160)],
        )
        self.assertEqual(verdict, "stranded")

    def test_single_job_run_with_no_terminal_row_is_stranded(self):
        # Same verdict regardless of how the terminal row went missing: a
        # single-job run, job completed, context still pending.
        verdict, _ = self.classify(
            row(49, "pending", "lint / lint", run=606818, job=898129,
                description="Has started running"),
            run_obj(606818),
            [job(898129)],
        )
        self.assertEqual(verdict, "stranded")

    def test_cancelled_run_is_still_stranded(self):
        verdict, _ = self.classify(
            row(143, "pending", "E2E / detect", run=602242, job=1),
            run_obj(602242, conclusion="cancelled"),
            [job(1)],
        )
        self.assertEqual(verdict, "stranded")

    def test_genuinely_running_job_is_left_alone(self):
        verdict, detail = self.classify(
            row(49, "pending", "ci / build", run=1, job=2),
            run_obj(1, status="in_progress", conclusion=None),
            [job(2, status="in_progress", conclusion=None)],
        )
        self.assertEqual(verdict, "run-active")
        self.assertIn("in_progress", detail)

    def test_run_completed_but_a_job_still_running_is_left_alone(self):
        # A run can read `completed` while a re-run of one job is in flight.
        verdict, _ = self.classify(
            row(49, "pending", "ci / build", run=1, job=2),
            run_obj(1),
            [job(2), job(3, status="in_progress", conclusion=None)],
        )
        self.assertEqual(verdict, "jobs-active")

    def test_already_terminal_context_is_a_noop(self):
        for state in ("success", "failure", "error", "warning"):
            verdict, _ = self.classify(
                row(74, state, "nodup-lint / no-duplication"),
                run_obj(),
                [job()],
            )
            self.assertEqual(verdict, "terminal", state)

    def test_fresh_pending_is_within_grace(self):
        verdict, detail = self.classify(
            row(49, "pending", "ci / build", minutes_ago=3), run_obj(), [job()]
        )
        self.assertEqual(verdict, "too-fresh")
        self.assertIn("grace", detail)

    def test_pending_without_target_url_is_never_healed(self):
        # A workflow POSTed this pending itself. There is no run to re-run and
        # we refuse to invent a verdict for it.
        verdict, _ = self.classify(
            row(71, "pending", "Secret scan / verdict", run=None), None, []
        )
        self.assertEqual(verdict, "orphan")

    def test_attempt_cap_stops_a_rerun_loop(self):
        verdict, detail = self.classify(
            row(49, "pending", "ci / build"), run_obj(), [job(attempt=4)]
        )
        self.assertEqual(verdict, "attempts-exhausted")
        self.assertIn("cap 4", detail)

        verdict, _ = self.classify(
            row(49, "pending", "ci / build"), run_obj(), [job(attempt=3)]
        )
        self.assertEqual(verdict, "stranded")

    def test_unreadable_run_is_left_alone(self):
        verdict, _ = self.classify(row(49, "pending", "ci / build"), None, [])
        self.assertEqual(verdict, "unknown-run")

    def test_run_with_no_jobs_is_left_alone(self):
        verdict, _ = self.classify(row(49, "pending", "ci / build"), run_obj(), [])
        self.assertEqual(verdict, "no-jobs")


class TestCancellationLatch(unittest.TestCase):
    """core#5156 — Gitea's own `cancelled -> failure` stamp is merge-blocking
    but reports NOTHING about the code.

    Every test here is paired with its negative control. A change that stops
    cancelled runs from blocking must NOT stop genuinely failing runs from
    blocking; a guard that suppressed both would "pass" the cancelled case and
    silently disarm the whole board.
    """

    def classify(self, status_row, run, jobs, min_age=15, max_attempts=4):
        return janitor.classify_context(
            status_row, run, jobs, NOW, min_age, max_attempts
        )

    # ---- direction 1: a cancelled run must NOT stay red ------------------
    def test_cancelled_before_start_is_stranded(self):
        # The exact measured shape of core#5156 run 644628 / job 949071: the
        # job never started (`started_at` epoch, `run_attempt` 0) yet Gitea
        # stamped `failure` on the PR head.
        verdict, detail = self.classify(
            row(149, "failure", "gitea-merge-queue / queue (pull_request_review)",
                run=644628, job=949071, description="Has been cancelled"),
            run_obj(644628, conclusion="cancelled"),
            [job(949071, conclusion="cancelled", attempt=0)],
        )
        self.assertEqual(verdict, "stranded", detail)
        self.assertIn("CANCELLED", detail)

    def test_cancelled_mid_run_is_also_stranded(self):
        # A job cancelled AFTER it started is equally uninformative: it never
        # reached a conclusion about the code.
        verdict, _ = self.classify(
            row(85, "failure", "lint-mask-pr-atomicity / lint-mask-pr-atomicity (pull_request)",
                run=644800, job=949500, description="Has been cancelled"),
            run_obj(644800, conclusion="cancelled"),
            [job(949500, conclusion="cancelled", attempt=1)],
        )
        self.assertEqual(verdict, "stranded")

    def test_error_state_cancellation_is_also_caught(self):
        verdict, _ = self.classify(
            row(90, "error", "ci / build (pull_request)", run=1, job=2,
                description="Has been cancelled"),
            run_obj(1, conclusion="cancelled"),
            [job(2, conclusion="cancelled")],
        )
        self.assertEqual(verdict, "stranded")

    # ---- direction 2: a REAL failure must STILL be red -------------------
    def test_genuine_failure_is_never_selected(self):
        # The ordinary red. No cancellation description -> not even a candidate,
        # and it costs no run lookup.
        verdict, _ = self.classify(
            row(50, "failure", "CI / all-required (pull_request)", run=1, job=2,
                description="Failing after 12s"),
            run_obj(1, conclusion="failure"),
            [job(2, conclusion="failure")],
        )
        self.assertEqual(verdict, "terminal")

    def test_failure_whose_job_really_failed_is_never_selected(self):
        # THE load-bearing negative control. The description SAYS cancelled but
        # the job's authoritative `conclusion` says `failure`. The verdict must
        # follow the API field, not the string — otherwise a red could be
        # laundered green by writing a description.
        verdict, detail = self.classify(
            row(51, "failure", "CI / all-required (pull_request)", run=1, job=2,
                description="Has been cancelled"),
            run_obj(1, conclusion="failure"),
            [job(2, conclusion="failure")],
        )
        self.assertEqual(verdict, "real-failure", detail)
        self.assertIn("genuine red", detail)

    def test_description_match_is_exact_not_substring(self):
        # A real test failure whose name quotes the phrase must not be laundered.
        verdict, _ = self.classify(
            row(52, "failure", "CI / unit (pull_request)", run=1, job=2,
                description="Has been cancelled by the user unexpectedly"),
            run_obj(1, conclusion="failure"),
            [job(2, conclusion="failure")],
        )
        self.assertEqual(verdict, "terminal")
        self.assertFalse(
            janitor.looks_like_cancellation_stamp(
                row(52, "failure", "CI / unit", description="has been cancelled!")
            )
        )

    def test_success_is_never_touched_even_with_the_description(self):
        verdict, _ = self.classify(
            row(53, "success", "CI / unit (pull_request)", run=1, job=2,
                description="Has been cancelled"),
            run_obj(1, conclusion="cancelled"),
            [job(2, conclusion="cancelled")],
        )
        self.assertEqual(verdict, "terminal")

    # ---- the same safety invariants as the pending class -----------------
    def test_fresh_cancellation_is_within_grace(self):
        # A cancellation whose superseding run is still in flight gets
        # overwritten on its own; only an AGED one is latched.
        verdict, detail = self.classify(
            row(149, "failure", "q / q", run=1, job=2, minutes_ago=2,
                description="Has been cancelled"),
            run_obj(1, conclusion="cancelled"),
            [job(2, conclusion="cancelled")],
        )
        self.assertEqual(verdict, "too-fresh")
        self.assertIn("grace", detail)

    def test_cancellation_without_target_url_is_never_healed(self):
        verdict, _ = self.classify(
            row(149, "failure", "q / q", run=None, description="Has been cancelled"),
            None,
            [],
        )
        self.assertEqual(verdict, "terminal")

    def test_phantom_job_id_refuses_to_act(self):
        # Cannot prove cancellation for a job that is not in this run.
        verdict, _ = self.classify(
            row(149, "failure", "q / q", run=605222, job=895694,
                description="Has been cancelled"),
            run_obj(605222, conclusion="cancelled"),
            [job(896159), job(896160)],
        )
        self.assertEqual(verdict, "phantom-job")

    def test_still_running_run_is_left_alone(self):
        verdict, _ = self.classify(
            row(149, "failure", "q / q", run=1, job=2,
                description="Has been cancelled"),
            run_obj(1, status="in_progress", conclusion=None),
            [job(2, status="in_progress", conclusion=None)],
        )
        self.assertEqual(verdict, "run-active")

    def test_attempt_cap_applies_to_cancellations_too(self):
        verdict, detail = self.classify(
            row(149, "failure", "q / q", run=1, job=2,
                description="Has been cancelled"),
            run_obj(1, conclusion="cancelled"),
            [job(2, conclusion="cancelled", attempt=4)],
        )
        self.assertEqual(verdict, "attempts-exhausted")
        self.assertIn("cap 4", detail)


class TestPlanReruns(unittest.TestCase):
    def test_one_rerun_per_run_even_when_five_contexts_strand_together(self):
        # Measured: run 605222 stranded 5 contexts at once. One re-run fixes all.
        findings = [
            {"run_id": 605222, "context": "staging-tenant-cd / %s" % name}
            for name in ("advance-pin", "redeploy-fleet", "e2e-smoke",
                         "rollback-pin", "runtime-image-readiness")
        ]
        plan = janitor.plan_reruns(findings)
        self.assertEqual([f["run_id"] for f in plan], [605222])

    def test_distinct_runs_are_all_planned_in_order(self):
        findings = [{"run_id": 3}, {"run_id": 1}, {"run_id": 3}, {"run_id": 2}]
        self.assertEqual([f["run_id"] for f in janitor.plan_reruns(findings)], [3, 1, 2])

    def test_empty(self):
        self.assertEqual(janitor.plan_reruns([]), [])


class TestSweep(unittest.TestCase):
    def test_sweep_reports_the_stranded_context_only(self):
        sha = "6ac234e4a6fcd8751b03dd3ee3ce6294f6c54ad7"
        client = FakeClient(
            statuses={
                sha: [
                    # healthy: pending superseded by success
                    row(171, "success", "staging-tenant-cd / rollback-pin",
                        minutes_ago=61, run=605222, job=896164),
                    row(170, "pending", "staging-tenant-cd / rollback-pin",
                        minutes_ago=62, run=605222, job=896164),
                    # stranded: pending is the newest row
                    row(181, "pending", "staging-tenant-cd / advance-pin",
                        minutes_ago=60, run=605222, job=895692),
                    row(175, "success", "staging-tenant-cd / advance-pin",
                        minutes_ago=61, run=605222, job=896160),
                ]
            },
            runs={605222: run_obj(605222)},
            jobs={605222: [job(896159), job(896164)]},
        )
        findings = janitor.sweep_sha(client, sha, NOW, Args(), label="branch main")
        self.assertEqual(len(findings), 1)
        self.assertEqual(findings[0]["context"], "staging-tenant-cd / advance-pin")
        self.assertEqual(findings[0]["run_id"], 605222)

    def test_sweep_selects_the_cancellation_latch_and_not_the_real_red(self):
        # End-to-end through the sweep's two-phase pre-filter, on one SHA
        # carrying BOTH shapes. The cancelled context must be reported; the
        # genuinely failing one must not. This is the both-directions assertion
        # at the level that actually runs in production.
        sha = "626d2049da9f8ef81b1c301ccf75af2409f3a050"
        client = FakeClient(
            statuses={
                sha: [
                    row(149, "failure",
                        "gitea-merge-queue / queue (pull_request_review)",
                        minutes_ago=60, run=644628, job=949071,
                        description="Has been cancelled"),
                    row(150, "failure", "CI / all-required (pull_request)",
                        minutes_ago=60, run=644630, job=949080,
                        description="Failing after 3m12s"),
                    row(151, "success", "Secret scan / Scan (pull_request)",
                        minutes_ago=60, run=644631, job=949081),
                ]
            },
            runs={
                644628: run_obj(644628, conclusion="cancelled"),
                644630: run_obj(644630, conclusion="failure"),
            },
            jobs={
                644628: [job(949071, conclusion="cancelled", attempt=0)],
                644630: [job(949080, conclusion="failure")],
            },
        )
        findings = janitor.sweep_sha(client, sha, NOW, Args(), label="PR #5156")
        self.assertEqual(
            [f["context"] for f in findings],
            ["gitea-merge-queue / queue (pull_request_review)"],
        )
        self.assertEqual(findings[0]["run_id"], 644628)
        # The genuine red cost no run lookup at all — the description
        # pre-filter kept the sweep's cost contract intact.
        self.assertEqual(
            sorted({c[2] for c in client.calls if c[1] in ("run", "jobs")}),
            [644628],
        )

    def test_sweep_leaves_a_cancellation_shaped_row_whose_job_really_failed(self):
        # Mutation control at sweep level: flip ONLY the job conclusion and the
        # same row must stop being selected.
        sha = "626d2049da9f8ef81b1c301ccf75af2409f3a050"
        client = FakeClient(
            statuses={
                sha: [
                    row(149, "failure",
                        "gitea-merge-queue / queue (pull_request_review)",
                        minutes_ago=60, run=644628, job=949071,
                        description="Has been cancelled"),
                ]
            },
            runs={644628: run_obj(644628, conclusion="failure")},
            jobs={644628: [job(949071, conclusion="failure")]},
        )
        self.assertEqual(janitor.sweep_sha(client, sha, NOW, Args()), [])

    def test_sweep_is_clean_when_everything_reached_a_verdict(self):
        sha = "8ed088a682debfa91284bedcf01c3138fa42833e"
        client = FakeClient(
            statuses={
                sha: [
                    row(36, "pending", "ci / lint-deploy-gate-chain", minutes_ago=70),
                    row(76, "success", "ci / lint-deploy-gate-chain", minutes_ago=69),
                ]
            },
            runs={606639: run_obj()},
            jobs={606639: [job()]},
        )
        self.assertEqual(janitor.sweep_sha(client, sha, NOW, Args()), [])

    def test_terminal_and_fresh_contexts_cost_no_run_lookups(self):
        # A commit carries 40+ contexts and nearly all are already terminal.
        # Looking up the run for each would be thousands of calls per pass.
        sha = "beadfeed"
        client = FakeClient(
            statuses={
                sha: [
                    row(1, "success", "a / a", minutes_ago=60),
                    row(2, "failure", "b / b", minutes_ago=60),
                    row(3, "pending", "c / c", minutes_ago=2),  # inside grace
                    row(4, "pending", "d / d", minutes_ago=60, run=None),  # orphan
                ]
            },
            runs={},
            jobs={},
        )
        self.assertEqual(janitor.sweep_sha(client, sha, NOW, Args()), [])
        self.assertEqual([c for c in client.calls if c[1] in ("run", "jobs")], [])


class TestNeverFabricatesAStatus(unittest.TestCase):
    def test_client_exposes_no_status_creating_method(self):
        # Invariant 1, structurally: there is no way to POST a commit status.
        for name in ("create_status", "post_status", "set_status", "create_commit_status"):
            self.assertFalse(hasattr(janitor.Gitea, name), name)

    def test_source_never_posts_to_the_statuses_endpoint(self):
        source = SCRIPT.read_text(encoding="utf-8")
        # /statuses/ may only ever be read.
        self.assertIn('self._request("GET"', source)
        self.assertNotIn('_request("POST", "/repos/%s/statuses', source)
        # The single mutating endpoint is the re-run.
        self.assertIn("/actions/runs/%s/rerun", source)

    def test_sweep_and_plan_issue_no_writes(self):
        sha = "deadbeef"
        client = FakeClient(
            statuses={sha: [row(49, "pending", "ci / build")]},
            runs={606639: run_obj()},
            jobs={606639: [job()]},
        )
        findings = janitor.sweep_sha(client, sha, NOW, Args())
        self.assertEqual(len(findings), 1)
        self.assertEqual([c for c in client.calls if c[0] != "GET"], [])

        # Healing performs exactly one write, and it is a re-run.
        for finding in janitor.plan_reruns(findings):
            client.rerun(finding["run_id"])
        writes = [c for c in client.calls if c[0] != "GET"]
        self.assertEqual(writes, [("POST", "rerun", 606639)])


class TestIdempotence(unittest.TestCase):
    def test_second_pass_after_the_rerun_lands_finds_nothing(self):
        sha = "cafebabe"
        stranded = [row(49, "pending", "ci / build", minutes_ago=60)]
        client = FakeClient(
            statuses={sha: stranded},
            runs={606639: run_obj()},
            jobs={606639: [job()]},
        )
        self.assertEqual(len(janitor.sweep_sha(client, sha, NOW, Args())), 1)

        # The re-run posts a real terminal row; the next pass must be a no-op.
        stranded.append(row(50, "success", "ci / build", minutes_ago=1))
        self.assertEqual(janitor.sweep_sha(client, sha, NOW, Args()), [])

    def test_repeated_sweeps_without_healing_are_stable(self):
        sha = "f00d"
        client = FakeClient(
            statuses={sha: [row(49, "pending", "ci / build", minutes_ago=60)]},
            runs={606639: run_obj()},
            jobs={606639: [job()]},
        )
        first = janitor.sweep_sha(client, sha, NOW, Args())
        second = janitor.sweep_sha(client, sha, NOW, Args())
        self.assertEqual(first, second)


if __name__ == "__main__":
    unittest.main()
