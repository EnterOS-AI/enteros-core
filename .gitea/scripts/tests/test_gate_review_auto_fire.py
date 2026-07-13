"""Regression test #765 — gate auto-fire on real qa/security APPROVED review.

Validates the structural configuration of qa-review.yml and security-review.yml
so that a real team-member APPROVED review fires the workflow and POSTs the
exact branch-protection-required context name. This is the test #2020's
stale-context failure would have caught.
"""

from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parents[2]


def load_workflow(name: str) -> dict:
    with (ROOT / "workflows" / name).open() as f:
        return yaml.safe_load(f)


def _job_guard_string(workflow: dict) -> str:
    """Return the raw job-level `if:` string for the single job."""
    jobs = workflow["jobs"]
    # Both qa-review and security-review have exactly one job named "approved".
    job = jobs["approved"]
    return str(job.get("if", ""))


def _post_step(workflow: dict) -> dict:
    """Return the explicit POST /statuses step from the job steps list."""
    jobs = workflow["jobs"]
    steps = jobs["approved"]["steps"]
    for step in steps:
        name = step.get("name", "")
        if "Post required status context" in name:
            return step
    raise AssertionError("No explicit POST status step found")


PR_SHAPED_TRIGGERS = ("pull_request", "pull_request_target", "pull_request_review")


def assert_review_trigger_contract(on: dict, name: str) -> None:
    """The qa/security approved-gates have TWO coherent states. Assert whichever
    one the file is actually in — and fail on an incoherent mix.

    OFF (current — CTO directive 2026-07-11/12, core#4067): DISPATCH-ONLY, with
    no PR-shaped trigger at all. This is not laziness, it is the only durable
    off-switch: Gitea fires a `pull_request_target` workflow even when it has
    been `disabled_manually` via the Actions API, so a lingering PR trigger
    re-posts a FAILING `<gate> / approved` context on every PR and wedges merge
    under branch protection ['*'] — forcing a controlled BP relax on each one.
    Dropping the triggers posts no context, so there is nothing to wedge.

    ON: if a PR trigger comes back, it must be `pull_request_review` carrying
    `submitted` — otherwise an APPROVED review never fires the gate that branch
    protection is waiting on, which is the #2020 stale-context wedge.

    This test used to assert ON unconditionally. It had been RED on main since
    core#4067 and nobody saw it, because test-ops-scripts.yml is path-filtered to
    scripts/** and .gitea/scripts/** and nothing had touched those since. Encode
    the real contract instead of the dead one.
    """
    present = [t for t in PR_SHAPED_TRIGGERS if t in on]
    if not present:
        return  # OFF, and coherently so.
    assert "pull_request_review" in on, (
        f"{name} has PR-shaped trigger(s) {present} but NOT pull_request_review: "
        "an APPROVED review cannot fire the gate, while pull_request_target still "
        "posts a failing context on every PR. That is the #2020 wedge. Either add "
        "pull_request_review (ON) or drop all PR triggers (OFF, dispatch-only)."
    )
    types = on["pull_request_review"].get("types", [])
    assert "submitted" in types, (
        f"{name}: pull_request_review must include 'submitted' or an APPROVED "
        "review does not fire the gate"
    )


class TestQaReviewDirectTrigger:
    def test_trigger_is_pull_request_review_submitted(self):
        wf = load_workflow("qa-review.yml")
        # PyYAML parses bare 'on' as boolean True.
        assert_review_trigger_contract(wf[True], "qa-review.yml")

    def test_job_guard_has_no_review_state_check(self):
        wf = load_workflow("qa-review.yml")
        guard = _job_guard_string(wf)
        assert "github.event.review.state" not in guard, (
            "job guard must NOT check review.state (#2159: Gitea 1.22.6 payload unreliable); "
            "evaluator (review-check.sh) verifies actual APPROVE via API"
        )
        assert "github.event_name == 'pull_request_target'" in guard
        assert "github.event_name == 'pull_request_review'" in guard

    def test_post_step_uses_status_post_token(self):
        wf = load_workflow("qa-review.yml")
        post = _post_step(wf)
        env = post.get("env", {})
        # PR #3422 (SSOT ci-status lib): the write-scoped token is resolved by
        # the "Resolve CI_STATUS_TOKEN" step (direct Gitea org secret first,
        # Infisical prod /shared/ci-status fallback), exported to $GITHUB_ENV
        # as CI_STATUS_TOKEN, and consumed by emit_review_status under the
        # CI_STATUS_TOKEN env name (was GITEA_TOKEN pre-lib). Same contract:
        # the POST step MUST source the write-scoped token from $GITHUB_ENV,
        # never a broad-scope secret directly.
        assert env.get("CI_STATUS_TOKEN") == "${{ env.CI_STATUS_TOKEN }}", (
            "POST step must consume the $GITHUB_ENV-resolved write-scoped "
            "CI_STATUS_TOKEN (env.CI_STATUS_TOKEN) for the status POST"
        )

    def test_post_step_context_name_exact(self):
        """The context POSTed must byte-match the branch-protection requirement."""
        wf = load_workflow("qa-review.yml")
        post = _post_step(wf)
        # PR #3422: the exact BP context now travels via env.STATUS_CONTEXT into
        # the SSOT lib's emit_review_status (unit-tested in test_ci_status.sh);
        # pin BOTH the byte-exact name and the lib call so neither can drift.
        env = post.get("env", {})
        assert env.get("STATUS_CONTEXT") == "qa-review / approved (pull_request_target)", (
            "POST step must emit exact BP-required context name (env.STATUS_CONTEXT)"
        )
        assert "emit_review_status" in post.get("run", ""), (
            "POST step must call the SSOT lib emit_review_status"
        )


class TestSecurityReviewDirectTrigger:
    def test_trigger_is_pull_request_review_submitted(self):
        wf = load_workflow("security-review.yml")
        # PyYAML parses bare 'on' as boolean True.
        assert_review_trigger_contract(wf[True], "security-review.yml")

    def test_job_guard_has_no_review_state_check(self):
        wf = load_workflow("security-review.yml")
        guard = _job_guard_string(wf)
        assert "github.event.review.state" not in guard, (
            "job guard must NOT check review.state (#2159: Gitea 1.22.6 payload unreliable); "
            "evaluator (review-check.sh) verifies actual APPROVE via API"
        )
        assert "github.event_name == 'pull_request_target'" in guard
        assert "github.event_name == 'pull_request_review'" in guard

    def test_post_step_uses_status_post_token(self):
        wf = load_workflow("security-review.yml")
        post = _post_step(wf)
        env = post.get("env", {})
        # PR #3422 (SSOT ci-status lib): the write-scoped token is resolved by
        # the "Resolve CI_STATUS_TOKEN" step (direct Gitea org secret first,
        # Infisical prod /shared/ci-status fallback), exported to $GITHUB_ENV
        # as CI_STATUS_TOKEN, and consumed by emit_review_status under the
        # CI_STATUS_TOKEN env name (was GITEA_TOKEN pre-lib). Same contract:
        # the POST step MUST source the write-scoped token from $GITHUB_ENV,
        # never a broad-scope secret directly.
        assert env.get("CI_STATUS_TOKEN") == "${{ env.CI_STATUS_TOKEN }}", (
            "POST step must consume the $GITHUB_ENV-resolved write-scoped "
            "CI_STATUS_TOKEN (env.CI_STATUS_TOKEN) for the status POST"
        )

    def test_post_step_context_name_exact(self):
        """The context POSTed must byte-match the branch-protection requirement."""
        wf = load_workflow("security-review.yml")
        post = _post_step(wf)
        # PR #3422: the exact BP context now travels via env.STATUS_CONTEXT into
        # the SSOT lib's emit_review_status (unit-tested in test_ci_status.sh);
        # pin BOTH the byte-exact name and the lib call so neither can drift.
        env = post.get("env", {})
        assert env.get("STATUS_CONTEXT") == "security-review / approved (pull_request_target)", (
            "POST step must emit exact BP-required context name (env.STATUS_CONTEXT)"
        )
        assert "emit_review_status" in post.get("run", ""), (
            "POST step must call the SSOT lib emit_review_status"
        )


class TestRefireScriptContextName:
    """review-refire-status.sh must emit the BP-required (pull_request_target) context."""

    def test_refire_script_context_is_pull_request_target(self):
        script = ROOT / "scripts" / "review-refire-status.sh"
        content = script.read_text()
        assert 'CONTEXT="${TEAM}-review / approved (pull_request_target)"' in content, (
            "refire script CONTEXT must be the exact BP-required (pull_request_target) variant"
        )
        assert 'approved (pull_request)"' not in content, (
            "refire script must NOT post bare (pull_request) context"
        )


class TestSopChecklistReviewRefireLifecycleStatus:
    """The review-refire job must not publish a skipped lifecycle status."""

    def test_review_refire_has_no_job_level_if(self):
        wf = load_workflow("sop-checklist.yml")
        job = wf["jobs"]["review-refire"]

        assert "if" not in job, (
            "review-refire must not use a job-level if: wildcard branch "
            "protection treats the skipped pull_request_target status as "
            "merge-blocking. Keep slash-command guards on the steps instead."
        )

    def test_review_refire_work_stays_step_guarded(self):
        wf = load_workflow("sop-checklist.yml")
        rendered = str(wf["jobs"]["review-refire"])

        assert "steps.classify.outputs.run_qa == 'true'" in rendered
        assert "steps.classify.outputs.run_security == 'true'" in rendered
        assert "Fetch CI_STATUS_TOKEN from Infisical SSOT" in rendered


class TestRefireTokenSeparation:
    """The /qa-recheck + /security-recheck backstop must also use STATUS_POST_TOKEN."""

    def _refire_step(self, workflow_name: str, step_name_keyword: str) -> dict:
        wf = load_workflow(workflow_name)
        jobs = wf["jobs"]
        steps = jobs["review-refire"]["steps"]
        for step in steps:
            name = step.get("name", "")
            if step_name_keyword in name:
                return step
        raise AssertionError(f"No refire step matching {step_name_keyword!r}")

    def test_qa_refire_uses_status_post_token(self):
        step = self._refire_step("sop-checklist.yml", "Refire qa-review")
        env = step.get("env", {})
        # KMS consolidation (#971/#3274): the STATUS_POST_TOKEN env var the
        # review-refire-status.sh script reads is now sourced from the
        # Infisical SSOT (prod /shared/ci-status) via $GITHUB_ENV
        # (CI_STATUS_TOKEN), replacing the old secrets.STATUS_POST_TOKEN.
        assert env.get("STATUS_POST_TOKEN") == "${{ env.CI_STATUS_TOKEN }}", (
            "qa refire must receive the Infisical-sourced CI_STATUS_TOKEN "
            "under the STATUS_POST_TOKEN env var"
        )
        # Evaluator stays on read token (no GITHUB_TOKEN fallback; it lacks
        # read:org scope for team-membership checks).
        assert env.get("GITEA_TOKEN") == "${{ secrets.SOP_CHECKLIST_GATE_TOKEN }}", (
            "qa refire evaluator must use read-scoped SOP_CHECKLIST_GATE_TOKEN"
        )

    def test_security_refire_uses_status_post_token(self):
        step = self._refire_step("sop-checklist.yml", "Refire security-review")
        env = step.get("env", {})
        # KMS consolidation (#971/#3274): the STATUS_POST_TOKEN env var the
        # review-refire-status.sh script reads is now sourced from the
        # Infisical SSOT (prod /shared/ci-status) via $GITHUB_ENV
        # (CI_STATUS_TOKEN), replacing the old secrets.STATUS_POST_TOKEN.
        assert env.get("STATUS_POST_TOKEN") == "${{ env.CI_STATUS_TOKEN }}", (
            "security refire must receive the Infisical-sourced CI_STATUS_TOKEN "
            "under the STATUS_POST_TOKEN env var"
        )
        assert env.get("GITEA_TOKEN") == "${{ secrets.SOP_CHECKLIST_GATE_TOKEN }}", (
            "security refire evaluator must use read-scoped SOP_CHECKLIST_GATE_TOKEN"
        )
