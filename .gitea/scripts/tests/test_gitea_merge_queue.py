import importlib
import sys
from pathlib import Path

import pytest

SCRIPT = Path(__file__).resolve().parents[1] / "gitea-merge-queue.py"
spec = importlib.util.spec_from_file_location("gitea_merge_queue", SCRIPT)
mq = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = mq
spec.loader.exec_module(mq)

# Capture the REAL loader before the autouse stub below replaces it, so the
# dedicated loader tests can exercise the genuine parser on a tmp file.
_REAL_LOAD_ENFORCED = mq.load_enforced_file_contexts


@pytest.fixture(autouse=True)
def _no_enforced_file_contexts_by_default(monkeypatch):
    """internal#3181: process_once / enumerate_readiness now read
    `.gitea/required-contexts.txt` via load_enforced_file_contexts(). The
    integration tests below predate that feature and only set up
    branch-protection + governance statuses; left unpatched they would pick
    up the REAL repo file (whose entries aren't in their fixtures) and
    correctly go `wait`. Default the loader to [] so the legacy BP-path tests
    keep asserting the BP path; the dedicated SSOT-enforced tests pass
    `enforced_file_contexts=` explicitly to evaluate_merge_readiness (and
    call load_enforced_file_contexts on a tmp file), so they are unaffected."""
    monkeypatch.setattr(mq, "load_enforced_file_contexts", lambda path: [])


def test_latest_statuses_dedupes_by_context_newest_first():
    statuses = [
        {"context": "CI / all-required (pull_request)", "status": "failure"},
        {"context": "sop-checklist / all-items-acked (pull_request_target)", "state": "success"},
        {"context": "CI / all-required (pull_request)", "status": "success"},
    ]

    latest = mq.latest_statuses_by_context(statuses)

    assert latest["CI / all-required (pull_request)"]["status"] == "failure"
    assert latest["sop-checklist / all-items-acked (pull_request_target)"]["state"] == "success"


def test_required_contexts_green_rejects_missing_and_pending():
    latest = mq.latest_statuses_by_context([
        {"context": "CI / all-required (pull_request)", "status": "success"},
        {"context": "sop-checklist / all-items-acked (pull_request_target)", "status": "pending"},
    ])

    ok, missing_or_bad = mq.required_contexts_green(
        latest,
        [
            "CI / all-required (pull_request)",
            "sop-checklist / all-items-acked (pull_request_target)",
            "qa-review / approved (pull_request_target)",
        ],
    )

    assert ok is False
    assert missing_or_bad == [
        "sop-checklist / all-items-acked (pull_request_target)=pending",
        "qa-review / approved (pull_request_target)=missing",
    ]


def test_required_contexts_green_rejects_pending_with_descriptive_prefix():
    """A required context left `pending` is a partial view, not a soft-fail.

    A gate can post `pending` with a human-readable description prefix (e.g. a
    `[volume-skipped]` note when it capped its own work). The merge queue must
    NOT treat any such pending required context as an acceptable soft-fail —
    the gate did not finish evaluating. Proven here against a real required
    context so the guard tracks the live required set.
    """
    latest = mq.latest_statuses_by_context([
        {"context": "CI / all-required (pull_request)", "status": "success"},
        {
            "context": "Secret scan / Scan diff for credential-shaped strings (pull_request)",
            "status": "pending",
            "description": "[volume-skipped] cap hit; please file ...",
        },
    ])

    ok, missing_or_bad = mq.required_contexts_green(
        latest,
        [
            "CI / all-required (pull_request)",
            "Secret scan / Scan diff for credential-shaped strings (pull_request)",
        ],
    )

    assert ok is False
    assert (
        "Secret scan / Scan diff for credential-shaped strings (pull_request)=pending"
        in missing_or_bad
    )


# ── core#4363: BP wildcard "*" handling ─────────────────────────────────────
# Production branch protection on main is status_check_contexts=['*']. Until this
# fix, required_contexts_green treated "*" as a LITERAL context name, so it
# returned (False, ["*=missing"]) on EVERY PR — even a fully green one — and the
# merge queue decided `wait` on all of them. The bot merged nothing; humans did
# every merge. These tests exercise the ['*'] path the existing suite never did
# (its _ready_kwargs uses a literal required_contexts list).


def test_wildcard_all_posted_green_is_green():
    latest = mq.latest_statuses_by_context([
        {"context": "CI / all-required (pull_request)", "status": "success"},
        {"context": "E2E Chat / E2E Chat (pull_request)", "status": "success"},
    ])
    ok, bad = mq.required_contexts_green(latest, ["*"])
    assert ok is True, bad
    assert bad == []


def test_wildcard_waits_on_any_posted_red():
    # Negative control: with "*", ANY posted non-success blocks — there is no
    # advisory tier (matches BP ['*'] and task #113).
    latest = mq.latest_statuses_by_context([
        {"context": "CI / all-required (pull_request)", "status": "success"},
        {"context": "E2E Chat / E2E Chat (pull_request)", "status": "failure"},
    ])
    ok, bad = mq.required_contexts_green(latest, ["*"])
    assert ok is False
    assert bad == ["E2E Chat / E2E Chat (pull_request)=failure"]


def test_wildcard_waits_on_any_posted_pending():
    latest = mq.latest_statuses_by_context([
        {"context": "CI / all-required (pull_request)", "status": "success"},
        {"context": "E2E Chat / E2E Chat (pull_request)", "status": "pending"},
    ])
    ok, bad = mq.required_contexts_green(latest, ["*"])
    assert ok is False
    assert bad == ["E2E Chat / E2E Chat (pull_request)=pending"]


def test_wildcard_with_zero_posted_is_vacuously_green_at_this_layer():
    # With NO posted statuses, "*" has nothing to reject -> green at THIS layer.
    # That is intentional and safe ONLY because the PRESENCE of specific contexts
    # is enforced separately by step 4b (enforced_file_contexts from
    # .gitea/required-contexts.txt). See test_wildcard_green_but_enforced_file_
    # context_missing_waits below for the composition.
    ok, bad = mq.required_contexts_green({}, ["*"])
    assert ok is True
    assert bad == []


def test_wildcard_plus_literal_dedupes_a_double_flag():
    latest = mq.latest_statuses_by_context([
        {"context": "CI / all-required (pull_request)", "status": "failure"},
    ])
    ok, bad = mq.required_contexts_green(
        latest, ["*", "CI / all-required (pull_request)"]
    )
    assert ok is False
    # flagged by both the wildcard sweep and the literal check -> listed ONCE
    assert bad == ["CI / all-required (pull_request)=failure"]


def test_non_required_red_present_is_false_under_wildcard():
    # Defense in depth for the force_merge path: under "*", nothing is
    # "non-required", so force must never be justified by a "non-required red".
    # A literal reading here would call this red non-required -> force=True ->
    # force_merge bypasses Gitea BP. Fail closed.
    latest = mq.latest_statuses_by_context([
        {"context": "CI / all-required (pull_request)", "status": "success"},
        {"context": "some advisory / lint (pull_request)", "status": "failure"},
    ])
    assert mq._non_required_red_present(latest, ["*"]) is False


def test_wildcard_ready_pr_merges_end_to_end():
    # THE POINT: with the production ['*'] required set and an all-green head,
    # the queue now reaches decision.ready — i.e. the bot would actually merge.
    # Before the fix this returned action="wait" forever.
    decision = mq.evaluate_merge_readiness(**_ready_kwargs(required_contexts=["*"]))
    assert decision.action == "merge", decision.reason
    assert decision.ready is True
    assert decision.force is False


def test_wildcard_ready_pr_with_a_red_waits_end_to_end():
    # Negative control on the whole decision: same ['*'] set, but a red in the PR
    # head statuses -> the bot must WAIT, never merge.
    kwargs = _ready_kwargs(required_contexts=["*"])
    kwargs["pr_status"] = {
        "state": "failure",
        "statuses": [
            {"context": "CI / all-required (pull_request)", "status": "success"},
            {"context": "CI / Platform (Go) (pull_request)", "status": "success"},
            {"context": "E2E Chat / E2E Chat (pull_request)", "status": "failure"},
        ],
    }
    decision = mq.evaluate_merge_readiness(**kwargs)
    assert decision.ready is False
    assert decision.action == "wait"
    # Assert the REASON names the actual red, not just that it waited. The old
    # buggy code ALSO returned wait here (via `*=missing`), so an action-only
    # assertion passes against the bug too — it must fail closed for the RIGHT
    # reason (the E2E Chat failure), which only the fixed '*' sweep produces.
    assert "E2E Chat / E2E Chat (pull_request)=failure" in decision.reason
    assert "*=missing" not in decision.reason


def test_wildcard_green_but_enforced_file_context_missing_waits():
    # THE CROWN JEWEL: step 4b (.gitea/required-contexts.txt presence check) is
    # now REACHABLE. All BP-posted contexts are green ('*' passes), but a
    # required-contexts.txt entry never posted at all -> the queue must WAIT.
    # Before the fix, step 4 returned wait first and this check was dead code.
    decision = mq.evaluate_merge_readiness(
        **_ready_kwargs(
            required_contexts=["*"],
            enforced_file_contexts=["E2E Ephemeral CP Happy Path / E2E Ephemeral CP Happy Path"],
        )
    )
    assert decision.ready is False
    assert decision.action == "wait"
    assert "enforced" in decision.reason.lower() or "ephemeral" in decision.reason.lower()


def test_choose_next_pr_sorts_by_queue_label_timestamp_then_number():
    issues = [
        {
            "number": 12,
            "pull_request": {},
            "labels": [{"name": "merge-queue"}],
            "created_at": "2026-05-13T05:00:00Z",
            "updated_at": "2026-05-13T06:00:00Z",
        },
        {
            "number": 9,
            "pull_request": {},
            "labels": [{"name": "merge-queue"}],
            "created_at": "2026-05-13T04:00:00Z",
            "updated_at": "2026-05-13T07:00:00Z",
        },
        {
            "number": 7,
            "labels": [{"name": "merge-queue"}],
            "created_at": "2026-05-13T03:00:00Z",
        },
    ]

    selected = mq.choose_next_queued_issue(issues, queue_label="merge-queue")

    assert selected["number"] == 9


def test_pr_needs_update_when_base_sha_absent_from_commits():
    commits = [
        {"sha": "head"},
        {"sha": "parent"},
    ]

    assert mq.pr_contains_base_sha(commits, "mainsha") is False
    assert mq.pr_contains_base_sha(commits, "parent") is True


def _ready_kwargs(**overrides):
    """Default kwargs for a fully-ready merge; override per test.

    The uniform SOP governance gate (qa-review/security-review/sop-checklist)
    was removed 2026-07-14, so the ready baseline is just branch-protection
    required contexts (GOVERNANCE_REQUIRED_CONTEXTS is now empty).
    """
    base = dict(
        main_status={
            "state": "success",
            "statuses": [{"context": "CI / all-required (push)", "status": "success"}],
        },
        pr_status={
            "state": "success",
            "statuses": [
                {"context": "CI / all-required (pull_request)", "status": "success"},
                # CRITICAL fail-closed contexts (RCA core#1676) — a genuinely
                # ready PR has these green; the step-0 guard requires them.
                {"context": "CI / Platform (Go) (pull_request)", "status": "success"},
            ],
        },
        required_contexts=[
            "CI / all-required (pull_request)",
        ],
        required_approvals=2,
        approvers={"agent-reviewer-cr2", "agent-researcher"},
        request_changes=[],
        pr_has_current_base=True,
        mergeable=True,
    )
    base.update(overrides)
    return base


def test_merge_decision_requires_main_green_pr_green_and_current_base():
    decision = mq.evaluate_merge_readiness(**_ready_kwargs())

    assert decision.ready is True
    assert decision.action == "merge"
    assert decision.force is False  # no non-required reds present


def test_behind_main_but_mergeable_pr_merges_directly():
    """§SOP-22 (#2358): a behind-main but CONFLICT-FREE PR (mergeable is True)
    merges DIRECTLY — no update step. Branch protection does not require strict
    up-to-date, and calling /update would dismiss the genuine approvals
    (dismiss_stale_approvals), forcing re-review every tick (the throughput
    bottleneck). This replaces the old update-before-merge behavior."""
    decision = mq.evaluate_merge_readiness(
        **_ready_kwargs(pr_has_current_base=False, mergeable=True)
    )

    assert decision.ready is True
    assert decision.action == "merge"


def test_behind_main_and_not_mergeable_pr_updates():
    """The /update path is reached ONLY when the PR is NOT mergeable AND its head
    lacks current main — refreshing the branch may resolve a behind-main
    non-conflict; a real conflict 409s and is held (#2352)."""
    decision = mq.evaluate_merge_readiness(
        **_ready_kwargs(pr_has_current_base=False, mergeable=False)
    )

    assert decision.ready is False
    assert decision.action == "update"


def test_current_base_but_not_mergeable_pr_waits():
    """Up-to-date with main yet Gitea reports not-mergeable → genuine conflict
    against current main (or still computing). The queue cannot act: WAIT,
    never update (update would not help) and never merge (fail-closed)."""
    decision = mq.evaluate_merge_readiness(
        **_ready_kwargs(pr_has_current_base=True, mergeable=False)
    )

    assert decision.ready is False
    assert decision.action == "wait"
    assert "not mergeable" in decision.reason


def test_behind_main_and_mergeable_none_waits_not_update():
    """§SOP-22 (CR2 #2374) — the churn-residual fix. A BEHIND-MAIN PR whose
    mergeability Gitea is STILL COMPUTING (mergeable is None) must WAIT, NOT take
    the /update path. The old code collapsed None→False, so a behind-main +
    None PR returned action="update" → /pulls/{n}/update → dismiss_stale_approvals
    → the exact rebase-churn this change eliminates, fired during the compute
    window. None and False are now DISTINCT: None waits, False updates."""
    decision = mq.evaluate_merge_readiness(
        **_ready_kwargs(pr_has_current_base=False, mergeable=None)
    )

    assert decision.ready is False
    assert decision.action == "wait"  # NOT "update" — no churn during compute
    assert "computed" in decision.reason


def test_current_base_and_mergeable_none_waits():
    """Up-to-date with main + mergeable None (still computing) → WAIT (unchanged
    fail-closed; just confirming None is never merged regardless of base)."""
    decision = mq.evaluate_merge_readiness(
        **_ready_kwargs(pr_has_current_base=True, mergeable=None)
    )

    assert decision.ready is False
    assert decision.action == "wait"


def test_MergePermissionError_inherits_from_ApiError():
    assert issubclass(mq.MergePermissionError, mq.ApiError)


def test_MergePermissionError_message_preserved():
    exc = mq.MergePermissionError("POST /merge -> HTTP 405: User not allowed")
    assert "405" in str(exc)
    assert "User not allowed" in str(exc)


# --------------------------------------------------------------------------
# Fix 1: merge criterion — genuine approvals on current head + required-only
# --------------------------------------------------------------------------

REVIEWERS = {"agent-reviewer", "agent-researcher", "agent-reviewer-cr2"}


def test_genuine_approvals_counts_two_distinct_on_current_head():
    reviews = [
        {"state": "APPROVED", "user": {"login": "agent-researcher"},
         "official": True, "stale": False, "dismissed": False, "commit_id": "HEAD"},
        {"state": "APPROVED", "user": {"login": "agent-reviewer-cr2"},
         "official": True, "stale": False, "dismissed": False, "commit_id": "HEAD"},
    ]
    approvers, rc = mq.genuine_approvals(reviews, headsha="HEAD", reviewer_set=REVIEWERS)
    assert approvers == {"agent-researcher", "agent-reviewer-cr2"}
    assert rc == []


def test_genuine_approvals_ignores_stale_dismissed_and_wrong_head():
    reviews = [
        # stale → not counted
        {"state": "APPROVED", "user": {"login": "agent-researcher"},
         "official": True, "stale": True, "dismissed": False, "commit_id": "OLD"},
        # dismissed → not counted
        {"state": "APPROVED", "user": {"login": "agent-reviewer-cr2"},
         "official": True, "stale": False, "dismissed": True, "commit_id": "HEAD"},
        # commit_id mismatch (prior head) → not counted
        {"state": "APPROVED", "user": {"login": "agent-reviewer"},
         "official": True, "stale": False, "dismissed": False, "commit_id": "OLD"},
    ]
    approvers, rc = mq.genuine_approvals(reviews, headsha="HEAD", reviewer_set=REVIEWERS)
    assert approvers == set()
    assert rc == []


def test_genuine_approvals_ignores_unofficial_and_outsiders():
    reviews = [
        # unofficial → not counted
        {"state": "APPROVED", "user": {"login": "agent-researcher"},
         "official": False, "stale": False, "dismissed": False, "commit_id": "HEAD"},
        # outside reviewer set (e.g. CTO-agent / random) → not counted
        {"state": "APPROVED", "user": {"login": "hongming-codex-laptop"},
         "official": True, "stale": False, "dismissed": False, "commit_id": "HEAD"},
    ]
    approvers, rc = mq.genuine_approvals(reviews, headsha="HEAD", reviewer_set=REVIEWERS)
    assert approvers == set()


def test_genuine_approvals_latest_review_supersedes_earlier():
    # agent-reviewer-cr2 approved then later requested changes on same head.
    reviews = [
        {"state": "APPROVED", "user": {"login": "agent-reviewer-cr2"},
         "official": True, "stale": False, "dismissed": False, "commit_id": "HEAD"},
        {"state": "REQUEST_CHANGES", "user": {"login": "agent-reviewer-cr2"},
         "official": True, "stale": False, "dismissed": False, "commit_id": "HEAD"},
    ]
    approvers, rc = mq.genuine_approvals(reviews, headsha="HEAD", reviewer_set=REVIEWERS)
    assert approvers == set()
    assert rc == ["agent-reviewer-cr2"]


def test_genuine_approvals_out_of_roster_request_changes_still_blocks():
    """FIX-2 (internal#3210): an official current-head REQUEST_CHANGES from a
    login OUTSIDE REVIEWER_SET (e.g. the CTO/founder) must be surfaced in the
    request_changes list so the merge is blocked — while roster approvals are
    still tallied. The earlier reviewer_set filter dropped it silently."""
    reviews = [
        {"state": "APPROVED", "user": {"login": "agent-researcher"},
         "official": True, "stale": False, "dismissed": False, "commit_id": "HEAD"},
        {"state": "APPROVED", "user": {"login": "agent-reviewer-cr2"},
         "official": True, "stale": False, "dismissed": False, "commit_id": "HEAD"},
        # CTO/founder — NOT in REVIEWER_SET — requests changes on current head.
        {"state": "REQUEST_CHANGES", "user": {"login": "hongming-cto"},
         "official": True, "stale": False, "dismissed": False, "commit_id": "HEAD"},
    ]
    approvers, rc = mq.genuine_approvals(reviews, headsha="HEAD", reviewer_set=REVIEWERS)
    # Roster approvals are still counted (roster gate unchanged for approvers).
    assert approvers == {"agent-researcher", "agent-reviewer-cr2"}
    # The out-of-roster block is honored.
    assert "hongming-cto" in rc

    # End-to-end: that block flows into evaluate_merge_readiness and WAITS,
    # even though the 2-genuine approval floor is otherwise satisfied.
    decision = mq.evaluate_merge_readiness(
        **_ready_kwargs(approvers=approvers, request_changes=rc, required_approvals=2)
    )
    assert decision.ready is False
    assert decision.action == "wait"
    assert "REQUEST_CHANGES" in decision.reason
    assert "hongming-cto" in decision.reason


def test_merge_blocked_when_open_request_changes_on_current_head():
    decision = mq.evaluate_merge_readiness(
        **_ready_kwargs(request_changes=["agent-reviewer-cr2"])
    )
    assert decision.ready is False
    assert decision.action == "wait"
    assert "REQUEST_CHANGES" in decision.reason


def test_merge_blocked_when_insufficient_genuine_approvals():
    decision = mq.evaluate_merge_readiness(
        **_ready_kwargs(approvers={"agent-researcher"}, required_approvals=2)
    )
    assert decision.ready is False
    assert decision.action == "wait"
    assert "insufficient genuine approvals" in decision.reason


def test_removed_sop_contexts_do_not_block_merge():
    # Regression for the 2026-07-14 SOP-gate removal: qa-review /
    # security-review / sop-checklist are no longer required, so a PR that is
    # otherwise ready (BP-required CI green, genuine approvals, mergeable) must
    # merge even if those now-orphan contexts are red — they are non-required
    # noise and must never block. (Before removal these were a uniform gate.)
    pr_status = {
        "state": "success",
        "statuses": [
            {"context": "CI / all-required (pull_request)", "status": "success"},
            {"context": "CI / Platform (Go) (pull_request)", "status": "success"},
            {"context": "qa-review / approved (pull_request_target)", "status": "failure"},
            {"context": "security-review / approved (pull_request_target)", "status": "pending"},
            {"context": "sop-checklist / all-items-acked (pull_request_target)", "status": "failure"},
        ],
    }
    decision = mq.evaluate_merge_readiness(**_ready_kwargs(pr_status=pr_status))
    assert decision.ready is True, decision.reason


def test_non_required_advisory_red_does_not_block_merge():
    # Governance checks are green; only advisory non-required reds (Staging SaaS)
    # are present → PR is still mergeable with force_merge bypassing the advisory.
    pr_status = {
        "state": "failure",  # combined polluted by advisory non-required reds
        "statuses": [
            {"context": "CI / all-required (pull_request)", "status": "success"},
            {"context": "CI / Platform (Go) (pull_request)", "status": "success"},
            {"context": "qa-review / approved (pull_request_target)", "status": "success"},
            {"context": "security-review / approved (pull_request_target)", "status": "success"},
            {"context": "sop-checklist / all-items-acked (pull_request_target)", "status": "success"},
            {"context": "Staging SaaS / e2e (pull_request)", "status": "failure"},
        ],
    }
    decision = mq.evaluate_merge_readiness(**_ready_kwargs(pr_status=pr_status))
    assert decision.ready is True
    assert decision.action == "merge"
    assert decision.force is True


def test_failing_required_context_blocks_even_with_approvals():
    pr_status = {
        "state": "failure",
        "statuses": [
            {"context": "CI / all-required (pull_request)", "status": "failure"},
        ],
    }
    decision = mq.evaluate_merge_readiness(**_ready_kwargs(pr_status=pr_status))
    assert decision.ready is False
    assert decision.action == "wait"
    assert decision.force is False
    # all-required IS a CRITICAL fail-closed context (RCA core#1676); a failing
    # all-required is now caught by the step-0 critical guard (an even stronger
    # block — force_merge cannot bypass it).
    assert "required context" in decision.reason.lower()


# --------------------------------------------------------------------------
# CRITICAL fail-closed contexts (RCA core#1676 — merged with
# CI/Platform(Go)=failure AND CI/all-required=skipped onto a red main; the
# force_merge path swept these up as "non-required reds" and bypassed them).
# --------------------------------------------------------------------------

def _rca_1676_statuses():
    """The exact critical-context shape that let core PR #1676 merge red."""
    return {
        "state": "failure",
        "statuses": [
            {"context": "CI / Platform (Go) (pull_request)", "status": "failure"},
            {"context": "CI / all-required (pull_request)", "status": "skipped"},
            {"context": "qa-review / approved (pull_request_target)", "status": "success"},
            {"context": "security-review / approved (pull_request_target)", "status": "success"},
            {"context": "sop-checklist / all-items-acked (pull_request_target)", "status": "success"},
        ],
    }


def test_critical_contexts_block_helper_flags_1676():
    latest = mq.latest_statuses_by_context(_rca_1676_statuses()["statuses"])
    reasons = mq.critical_contexts_block(latest)
    joined = " ".join(reasons)
    assert "CI / Platform (Go)" in joined
    assert "CI / all-required" in joined


def test_critical_guard_blocks_1676_even_force_merge_path():
    # mergeable=True + genuine approvals is the EXACT force_merge precondition
    # that let #1676 through. The step-0 critical guard must block it anyway,
    # and the decision must NOT be a (forced) merge.
    decision = mq.evaluate_merge_readiness(
        **_ready_kwargs(
            pr_status=_rca_1676_statuses(),
            # The #1676 gap: BP-required set did NOT enumerate the critical
            # contexts, so step-4 alone would have let them slip to force.
            required_contexts=["sop-checklist / all-items-acked (pull_request_target)"],
            mergeable=True,
        )
    )
    assert decision.ready is False
    assert decision.action == "wait"
    assert decision.force is False
    assert "CRITICAL" in decision.reason


def test_critical_guard_blocks_skipped_all_required():
    statuses = {
        "state": "success",  # combined can mask a skipped sentinel as non-failing
        "statuses": [
            {"context": "CI / Platform (Go) (pull_request)", "status": "success"},
            {"context": "CI / all-required (pull_request)", "status": "skipped"},
        ],
    }
    decision = mq.evaluate_merge_readiness(
        **_ready_kwargs(pr_status=statuses, required_contexts=[], mergeable=True)
    )
    assert decision.ready is False
    assert "CI / all-required" in decision.reason


def test_critical_guard_blocks_missing_platform_go():
    statuses = {
        "state": "success",
        "statuses": [
            {"context": "CI / all-required (pull_request)", "status": "success"},
            # CI / Platform (Go) entirely absent → cannot prove green → BLOCK.
        ],
    }
    decision = mq.evaluate_merge_readiness(
        **_ready_kwargs(pr_status=statuses, required_contexts=[], mergeable=True)
    )
    assert decision.ready is False
    assert "CI / Platform (Go)=missing" in decision.reason


def test_critical_guard_allows_when_both_green():
    statuses = {
        "state": "success",
        "statuses": [
            {"context": "CI / Platform (Go) (pull_request)", "status": "success"},
            {"context": "CI / all-required (pull_request)", "status": "success"},
        ],
    }
    decision = mq.evaluate_merge_readiness(
        **_ready_kwargs(
            pr_status=statuses,
            required_contexts=[
                "CI / Platform (Go) (pull_request)",
                "CI / all-required (pull_request)",
            ],
            mergeable=True,
        )
    )
    assert decision.ready is True
    assert decision.action == "merge"


def test_unmergeable_pr_blocks():
    decision = mq.evaluate_merge_readiness(**_ready_kwargs(mergeable=False))
    assert decision.ready is False
    assert decision.action == "wait"
    assert "not mergeable" in decision.reason


# --------------------------------------------------------------------------
# Fix 1 (cont.): required contexts come from branch protection (fail-closed)
# --------------------------------------------------------------------------

def test_parse_branch_protection_uses_status_check_contexts():
    bp = mq.parse_branch_protection({
        "enable_status_check": True,
        "status_check_contexts": [
            "CI / all-required (pull_request)",
            "E2E API Smoke Test / E2E API Smoke Test (pull_request)",
        ],
        "required_approvals": 2,
        "block_on_rejected_reviews": True,
    })
    assert bp.required_contexts == [
        "CI / all-required (pull_request)",
        "E2E API Smoke Test / E2E API Smoke Test (pull_request)",
    ]
    assert bp.required_approvals == 2
    assert bp.block_on_rejected_reviews is True


def test_parse_branch_protection_fail_closed_when_contexts_missing():
    # enable_status_check true but no enumerable contexts → must raise so the
    # queue HOLDS rather than merging against an unverified required set.
    import pytest
    with pytest.raises(mq.BranchProtectionUnavailable):
        mq.parse_branch_protection({
            "enable_status_check": True,
            "status_check_contexts": None,
            "required_approvals": 2,
        })
    with pytest.raises(mq.BranchProtectionUnavailable):
        mq.parse_branch_protection({
            "enable_status_check": True,
            "status_check_contexts": [],
            "required_approvals": 2,
        })


def test_parse_branch_protection_fail_closed_on_non_object():
    import pytest
    with pytest.raises(mq.BranchProtectionUnavailable):
        mq.parse_branch_protection(None)


# --------------------------------------------------------------------------
# FIX-1 (internal#3210, CRITICAL fail-open): required_approvals FLOOR.
#
# A degraded `required_approvals` from branch protection — 0 (admin lowered
# it / migration reset / blanked-restored BP), a negative (passes a naive
# `< N`), or a bool True (isinstance(True, int) is True → coerces to 1,
# HALVING a 2-genuine bar) — must NEVER weaken or skip the genuine-approval
# gate. parse_branch_protection clamps UP to REQUIRED_APPROVALS_DEFAULT;
# evaluate_merge_readiness applies an independent floor of 1 on the
# non-exempt path. These tests pin both layers.
# --------------------------------------------------------------------------

def _bp_body(required_approvals):
    return {
        "enable_status_check": True,
        "status_check_contexts": ["CI / all-required (pull_request)"],
        "required_approvals": required_approvals,
        "block_on_rejected_reviews": True,
    }


def test_parse_branch_protection_clamps_zero_up_to_default():
    """required_approvals: 0 from BP must clamp UP to the default floor (2),
    never zero/skip the genuine-approval gate."""
    bp = mq.parse_branch_protection(_bp_body(0))
    assert bp.required_approvals == mq.REQUIRED_APPROVALS_DEFAULT
    assert bp.required_approvals >= 1


def test_parse_branch_protection_clamps_negative_up_to_default():
    """A negative required_approvals (would pass a naive `len < N`) must
    clamp UP to the default floor, never below it."""
    bp = mq.parse_branch_protection(_bp_body(-1))
    assert bp.required_approvals == mq.REQUIRED_APPROVALS_DEFAULT


def test_parse_branch_protection_rejects_bool_true_uses_default():
    """bool True is NOT a valid approval count. isinstance(True, int) is True
    in Python, so the naive int() path would coerce True->1 and HALVE a
    2-genuine bar. It must be rejected and fall back to the default floor."""
    bp = mq.parse_branch_protection(_bp_body(True))
    assert bp.required_approvals == mq.REQUIRED_APPROVALS_DEFAULT
    # Defensive: a False would also be rejected (treated as invalid).
    bp_false = mq.parse_branch_protection(_bp_body(False))
    assert bp_false.required_approvals == mq.REQUIRED_APPROVALS_DEFAULT


def test_parse_branch_protection_honors_stricter_value():
    """A BP value ABOVE the default floor is honored as-is (stricter wins)."""
    bp = mq.parse_branch_protection(_bp_body(3))
    assert bp.required_approvals == 3


def test_parse_branch_protection_missing_approvals_uses_default():
    """No required_approvals key at all → the default floor, not zero."""
    body = _bp_body(0)
    del body["required_approvals"]
    bp = mq.parse_branch_protection(body)
    assert bp.required_approvals == mq.REQUIRED_APPROVALS_DEFAULT


def test_evaluate_merge_readiness_floors_zero_required_approvals_nonexempt():
    """Defence-in-depth: even if a degraded required_approvals=0 reaches
    evaluate_merge_readiness on the NON-exempt path, the genuine-approval
    gate is NOT skipped — a PR with zero approvers must still WAIT."""
    decision = mq.evaluate_merge_readiness(
        **_ready_kwargs(
            approvers=set(),
            required_approvals=0,
            # runtime_bump_exempt omitted → defaults to False (non-exempt)
        )
    )
    assert decision.ready is False
    assert decision.action == "wait"
    assert "insufficient genuine approvals" in decision.reason
    # The floor is reported as need >= 1, never 0.
    assert "need 1" in decision.reason


def test_evaluate_merge_readiness_floors_negative_required_approvals_nonexempt():
    """A negative required_approvals must not pass a naive `len(approvers) < N`
    check — the floor forces the gate to require at least 1."""
    decision = mq.evaluate_merge_readiness(
        **_ready_kwargs(
            approvers=set(),
            required_approvals=-5,
        )
    )
    assert decision.ready is False
    assert decision.action == "wait"
    assert "insufficient genuine approvals" in decision.reason


def test_evaluate_merge_readiness_floor_does_not_break_runtime_bump_exemption():
    """The non-exempt floor must NOT touch the runtime-bump exemption path,
    which legitimately zeroes the HUMAN approval bar (a bot cannot
    self-approve). With exempt=True + 0 approvers the PR still merges."""
    decision = mq.evaluate_merge_readiness(
        **_ready_kwargs(
            approvers=set(),
            required_approvals=0,
            runtime_bump_exempt=True,
        )
    )
    assert decision.ready is True
    assert decision.action == "merge"


# --------------------------------------------------------------------------
# Fix 2: HOL — a permanent merge error HOLDS the PR (applies HOLD_LABEL)
# --------------------------------------------------------------------------

def test_process_once_holds_pr_on_permanent_merge_error(monkeypatch):
    """A 405 on merge must apply HOLD_LABEL so the queue advances, not loop."""
    calls = {"hold_label": None, "merge_attempts": 0}

    monkeypatch.setattr(mq, "OWNER", "molecule-ai")
    monkeypatch.setattr(mq, "NAME", "molecule-core")
    monkeypatch.setattr(mq, "WATCH_BRANCH", "main")
    monkeypatch.setattr(mq, "QUEUE_LABEL", "merge-queue")
    monkeypatch.setattr(mq, "HOLD_LABEL", "merge-queue-hold")
    monkeypatch.setattr(mq, "AUTO_DISCOVER", True)
    monkeypatch.setattr(mq, "OPT_OUT_LABELS", {"merge-queue-hold", "do-not-auto-merge", "wip"})
    monkeypatch.setattr(mq, "REVIEWER_SET", REVIEWERS)

    monkeypatch.setattr(mq, "get_branch_protection", lambda branch: mq.BranchProtection(
        required_contexts=["CI / all-required (pull_request)"],
        required_approvals=2,
        block_on_rejected_reviews=True,
    ))
    main_sha = "b" * 40
    head_sha = "a" * 40
    monkeypatch.setattr(mq, "get_branch_head", lambda branch: main_sha)

    def fake_combined(sha, *, prefer_live=False):
        if sha == main_sha:
            return {"state": "success", "statuses": [{"context": "CI / all-required (push)", "status": "success"}]}
        return {"state": "success", "statuses": [
            {"context": "CI / all-required (pull_request)", "status": "success"},
            {"context": "CI / Platform (Go) (pull_request)", "status": "success"},
            {"context": "qa-review / approved (pull_request_target)", "status": "success"},
            {"context": "security-review / approved (pull_request_target)", "status": "success"},
            {"context": "sop-checklist / all-items-acked (pull_request_target)", "status": "success"},
        ]}
    monkeypatch.setattr(mq, "get_combined_status", fake_combined)

    monkeypatch.setattr(mq, "list_candidate_issues", lambda *, auto_discover: [
        {"number": 100, "pull_request": {}, "labels": [{"name": "merge-queue"}],
         "created_at": "2026-06-01T00:00:00Z"},
    ])
    monkeypatch.setattr(mq, "get_pull", lambda n: {
        "state": "open", "number": n, "mergeable": True,
        "base": {"ref": "main", "repo_id": 1},
        "head": {"sha": head_sha, "repo_id": 1},
        "labels": [{"name": "merge-queue"}],
    })
    monkeypatch.setattr(mq, "get_pull_commits", lambda n: [{"sha": main_sha}, {"sha": head_sha}])
    monkeypatch.setattr(mq, "get_pull_reviews", lambda n: [
        {"state": "APPROVED", "user": {"login": "agent-researcher"},
         "official": True, "stale": False, "dismissed": False, "commit_id": head_sha},
        {"state": "APPROVED", "user": {"login": "agent-reviewer-cr2"},
         "official": True, "stale": False, "dismissed": False, "commit_id": head_sha},
    ])

    def fake_merge(pr_number, *, dry_run, force=False):
        calls["merge_attempts"] += 1
        raise mq.MergePermissionError("POST /merge -> HTTP 405: User not allowed to merge PR")
    monkeypatch.setattr(mq, "merge_pull", fake_merge)

    def fake_add_label(pr_number, label_name, *, dry_run):
        calls["hold_label"] = (pr_number, label_name)
    monkeypatch.setattr(mq, "add_label_by_name", fake_add_label)
    monkeypatch.setattr(mq, "post_comment", lambda *a, **k: None)

    rc = mq.process_once(dry_run=False)

    assert rc == 0
    assert calls["merge_attempts"] == 1
    # The HOL fix: PR was held, not silently left re-selectable.
    assert calls["hold_label"] == (100, "merge-queue-hold")


# --------------------------------------------------------------------------
# Fix 2 (cont.): mergeable=None is fail-CLOSED — Gitea is still computing the
# conflict check, so the queue must WAIT (re-check next tick), NOT merge. Only
# an explicit mergeable==True proceeds to an autonomous merge.
# --------------------------------------------------------------------------

def _fully_ready_process_once_monkeypatch(monkeypatch, mergeable, calls):
    """Wire process_once so every gate is green EXCEPT the `mergeable` field,
    which is set to the value under test. Records merge attempts in `calls`."""
    monkeypatch.setattr(mq, "OWNER", "molecule-ai")
    monkeypatch.setattr(mq, "NAME", "molecule-core")
    monkeypatch.setattr(mq, "WATCH_BRANCH", "main")
    monkeypatch.setattr(mq, "QUEUE_LABEL", "merge-queue")
    monkeypatch.setattr(mq, "HOLD_LABEL", "merge-queue-hold")
    monkeypatch.setattr(mq, "AUTO_DISCOVER", True)
    monkeypatch.setattr(mq, "OPT_OUT_LABELS", {"merge-queue-hold", "do-not-auto-merge", "wip"})
    monkeypatch.setattr(mq, "REVIEWER_SET", REVIEWERS)
    monkeypatch.setattr(mq, "get_branch_protection", lambda branch: mq.BranchProtection(
        required_contexts=["CI / all-required (pull_request)"],
        required_approvals=2,
        block_on_rejected_reviews=True,
    ))
    main_sha = "b" * 40
    head_sha = "a" * 40
    monkeypatch.setattr(mq, "get_branch_head", lambda branch: main_sha)

    def fake_combined(sha, *, prefer_live=False):
        if sha == main_sha:
            return {"state": "success", "statuses": [{"context": "CI / all-required (push)", "status": "success"}]}
        return {"state": "success", "statuses": [
            {"context": "CI / all-required (pull_request)", "status": "success"},
            {"context": "CI / Platform (Go) (pull_request)", "status": "success"},
            {"context": "qa-review / approved (pull_request_target)", "status": "success"},
            {"context": "security-review / approved (pull_request_target)", "status": "success"},
            {"context": "sop-checklist / all-items-acked (pull_request_target)", "status": "success"},
        ]}
    monkeypatch.setattr(mq, "get_combined_status", fake_combined)

    monkeypatch.setattr(mq, "list_candidate_issues", lambda *, auto_discover: [
        {"number": 102, "pull_request": {}, "labels": [{"name": "merge-queue"}],
         "created_at": "2026-06-01T00:00:00Z"},
    ])
    # mergeable is the value under test; everything else is fully ready.
    monkeypatch.setattr(mq, "get_pull", lambda n: {
        "state": "open", "number": n, "mergeable": mergeable,
        "base": {"ref": "main", "repo_id": 1},
        "head": {"sha": head_sha, "repo_id": 1},
        "labels": [{"name": "merge-queue"}],
    })
    monkeypatch.setattr(mq, "get_pull_commits", lambda n: [{"sha": main_sha}, {"sha": head_sha}])
    monkeypatch.setattr(mq, "get_pull_reviews", lambda n: [
        {"state": "APPROVED", "user": {"login": "agent-researcher"},
         "official": True, "stale": False, "dismissed": False, "commit_id": head_sha},
        {"state": "APPROVED", "user": {"login": "agent-reviewer-cr2"},
         "official": True, "stale": False, "dismissed": False, "commit_id": head_sha},
    ])

    def fake_merge(pr_number, *, dry_run, force=False):
        calls["merge_attempts"] += 1
    monkeypatch.setattr(mq, "merge_pull", fake_merge)

    def fake_add_label(pr_number, label_name, *, dry_run):
        calls["hold_label"] = (pr_number, label_name)
    monkeypatch.setattr(mq, "add_label_by_name", fake_add_label)
    monkeypatch.setattr(mq, "update_pull", lambda *a, **k: calls.__setitem__("updated", True))
    monkeypatch.setattr(mq, "post_comment", lambda *a, **k: None)


def test_process_once_waits_when_mergeable_is_none(monkeypatch):
    """FAIL-CLOSED: mergeable=None means Gitea is still computing the conflict
    check. The queue must NOT merge this tick; it waits and re-checks next tick.
    Critically: this is transient, so the PR is NOT hold-labelled (it stays
    queued and re-selectable)."""
    calls = {"merge_attempts": 0, "hold_label": None, "updated": False}
    _fully_ready_process_once_monkeypatch(monkeypatch, mergeable=None, calls=calls)

    rc = mq.process_once(dry_run=False)

    assert rc == 0
    # The bug regression: a None mergeable must NEVER trigger an autonomous merge.
    assert calls["merge_attempts"] == 0
    # Transient, not permanent — must NOT be held/dequeued; retried next tick.
    assert calls["hold_label"] is None
    assert calls["updated"] is False


def test_process_once_waits_when_mergeable_field_absent(monkeypatch):
    """Some Gitea versions omit the `mergeable` field entirely. Absent must be
    treated the same as None — fail-closed, wait, no merge."""
    calls = {"merge_attempts": 0, "hold_label": None, "updated": False}
    # Reuse the ready wiring but drop the mergeable key from the pull payload.
    _fully_ready_process_once_monkeypatch(monkeypatch, mergeable=None, calls=calls)
    head_sha = "a" * 40
    monkeypatch.setattr(mq, "get_pull", lambda n: {
        "state": "open", "number": n,  # no "mergeable" key at all
        "base": {"ref": "main", "repo_id": 1},
        "head": {"sha": head_sha, "repo_id": 1},
        "labels": [{"name": "merge-queue"}],
    })

    rc = mq.process_once(dry_run=False)

    assert rc == 0
    assert calls["merge_attempts"] == 0
    assert calls["hold_label"] is None


def test_process_once_merges_when_mergeable_is_true(monkeypatch):
    """The decisive case: an explicit mergeable==True (with every other gate
    green) DOES proceed to the autonomous merge."""
    calls = {"merge_attempts": 0, "hold_label": None, "updated": False}
    _fully_ready_process_once_monkeypatch(monkeypatch, mergeable=True, calls=calls)

    rc = mq.process_once(dry_run=False)

    assert rc == 0
    assert calls["merge_attempts"] == 1
    assert calls["hold_label"] is None


def test_process_once_behind_main_mergeable_none_waits_no_update(monkeypatch):
    """§SOP-22 (CR2 #2374) — end-to-end churn-residual regression. A BEHIND-MAIN
    PR (commits do NOT contain main_sha) whose mergeability Gitea is STILL
    COMPUTING (mergeable=None) must WAIT: process_once returns 0 and NEVER calls
    update_pull (which dismisses genuine approvals via dismiss_stale_approvals)
    NOR merge_pull NOR hold. The old None→False collapse routed this exact case
    into the /update path → approval-dismissing rebase churn during the compute
    window. This proves the durable churn elimination: no update, approvals
    preserved, re-checked next tick."""
    calls = {"merge_attempts": 0, "hold_label": None, "updated": False}
    _fully_ready_process_once_monkeypatch(monkeypatch, mergeable=None, calls=calls)
    # Make the head BEHIND main: commits do NOT contain main_sha. This is the
    # case the bug missed (the prior None test had current base, masking it).
    behind_head = "a" * 40
    monkeypatch.setattr(mq, "get_pull_commits", lambda n: [{"sha": behind_head}])

    rc = mq.process_once(dry_run=False)

    assert rc == 0
    assert calls["updated"] is False  # NO /update → approvals NOT dismissed
    assert calls["merge_attempts"] == 0  # never merge on an unknown
    assert calls["hold_label"] is None  # transient → not held, retried next tick


# --------------------------------------------------------------------------
# §SOP-22: DIRECT-MERGE throughput fix (#2358). A conflict-free 2-genuine PR
# merges WITHOUT a pre-merge /update call, so its approvals are NOT dismissed by
# dismiss_stale_approvals. The merge bar (2-genuine-on-current-head +
# BP-required green + mergeable + no RC + opt-out) is UNCHANGED; only the
# unnecessary update-before-merge churn is removed. The /update path survives
# for the genuine case it is needed (not-mergeable + behind-main), where a real
# conflict 409s and is held per #2352. mergeable=None stays fail-closed.
# --------------------------------------------------------------------------


def test_process_once_merges_conflict_free_pr_without_update(monkeypatch):
    """§SOP-22(a) — the core throughput fix. A conflict-free, fully-approved PR
    merges WITHOUT update_pull ever being called. The old behavior called
    /update first whenever the head lacked current main, which dismissed the 2
    genuine approvals (dismiss_stale_approvals) and forced re-review every tick.
    Assert update_pull is NOT invoked and merge_pull IS invoked."""
    calls = {"merge_attempts": 0, "hold_label": None, "updated": False}
    _fully_ready_process_once_monkeypatch(monkeypatch, mergeable=True, calls=calls)
    # Make the head BEHIND main: commits do NOT contain main_sha. Under the old
    # logic this alone forced an update_pull; under the fix it merges directly.
    head_sha = "a" * 40
    monkeypatch.setattr(mq, "get_pull_commits", lambda n: [{"sha": head_sha}])

    rc = mq.process_once(dry_run=False)

    assert rc == 0
    assert calls["merge_attempts"] == 1  # merged directly
    assert calls["updated"] is False  # NO update_pull → approvals NOT dismissed
    assert calls["hold_label"] is None


def test_process_once_behind_main_conflict_free_merges_directly(monkeypatch):
    """§SOP-22(b) — explicit behind-main + conflict-free case: it still merges
    directly (branch protection does not require strict up-to-date)."""
    calls = {"merge_attempts": 0, "hold_label": None, "updated": False}
    _fully_ready_process_once_monkeypatch(monkeypatch, mergeable=True, calls=calls)
    behind_head = "a" * 40
    monkeypatch.setattr(mq, "get_pull_commits", lambda n: [{"sha": behind_head}])

    rc = mq.process_once(dry_run=False)

    assert rc == 0
    assert calls["merge_attempts"] == 1
    assert calls["updated"] is False


def test_process_once_pauses_when_main_not_green_no_direct_merge(monkeypatch):
    """§SOP-22 backstop — the serialized safety that makes direct-merge safe:
    when main's required push contexts are NOT green (e.g. a prior direct merge
    introduced a semantic main-break caught by post-merge main CI), the queue
    PAUSES — it does NOT merge the next PR onto an unverified/red main."""
    calls = {"merge_attempts": 0, "hold_label": None, "updated": False}
    _fully_ready_process_once_monkeypatch(monkeypatch, mergeable=True, calls=calls)
    main_sha = "b" * 40

    def red_main_combined(sha, *, prefer_live=False):
        if sha == main_sha:
            return {"state": "failure",
                    "statuses": [{"context": "CI / all-required (push)", "status": "failure"}]}
        return {"state": "success",
                "statuses": [{"context": "CI / all-required (pull_request)", "status": "success"},
                             {"context": "CI / Platform (Go) (pull_request)", "status": "success"}]}
    monkeypatch.setattr(mq, "get_combined_status", red_main_combined)

    rc = mq.process_once(dry_run=False)

    assert rc == 0
    assert calls["merge_attempts"] == 0  # paused — no merge onto red main
    assert calls["updated"] is False


def test_direct_merge_bar_unchanged_behind_main(monkeypatch):
    """§SOP-22(d) — the merge bar is UNCHANGED on the new direct-merge path. A
    behind-main + conflict-free PR is still rejected (no merge) when ANY gate
    fails: insufficient genuine approvals, red required context, open
    REQUEST_CHANGES, or opt-out label. Direct-merge removes the update churn, it
    does NOT weaken the bar — fail-closed on every gate."""
    head_sha = "a" * 40
    behind_main = dict(pr_has_current_base=False, mergeable=True)

    # <2 genuine approvals → wait, not merge.
    d = mq.evaluate_merge_readiness(
        **_ready_kwargs(approvers={"agent-researcher"}, **behind_main)
    )
    assert d.action == "wait" and d.ready is False

    # Red required context → wait, not merge.
    red_required = {"state": "failure", "statuses": [
        {"context": "CI / all-required (pull_request)", "status": "failure"}]}
    d = mq.evaluate_merge_readiness(
        **_ready_kwargs(pr_status=red_required, **behind_main)
    )
    assert d.action == "wait" and d.ready is False

    # Open REQUEST_CHANGES on current head → wait, not merge.
    d = mq.evaluate_merge_readiness(
        **_ready_kwargs(request_changes=["agent-reviewer-cr2"], **behind_main)
    )
    assert d.action == "wait" and d.ready is False


# --------------------------------------------------------------------------
# Fix 3: status fetch is fail-closed (failed fetch != green)
# --------------------------------------------------------------------------

def test_status_fetch_failure_is_fail_closed(monkeypatch):
    """If the PR head status fetch raises, the PR is skipped — never merged."""
    merged = {"called": False}

    monkeypatch.setattr(mq, "OWNER", "molecule-ai")
    monkeypatch.setattr(mq, "NAME", "molecule-core")
    monkeypatch.setattr(mq, "WATCH_BRANCH", "main")
    monkeypatch.setattr(mq, "QUEUE_LABEL", "merge-queue")
    monkeypatch.setattr(mq, "HOLD_LABEL", "merge-queue-hold")
    monkeypatch.setattr(mq, "AUTO_DISCOVER", True)
    monkeypatch.setattr(mq, "OPT_OUT_LABELS", {"merge-queue-hold", "do-not-auto-merge", "wip"})
    monkeypatch.setattr(mq, "REVIEWER_SET", REVIEWERS)
    monkeypatch.setattr(mq, "get_branch_protection", lambda branch: mq.BranchProtection(
        required_contexts=["CI / all-required (pull_request)"],
        required_approvals=2, block_on_rejected_reviews=True,
    ))
    main_sha = "b" * 40
    head_sha = "a" * 40
    monkeypatch.setattr(mq, "get_branch_head", lambda branch: main_sha)

    def fake_combined(sha, *, prefer_live=False):
        if sha == main_sha:
            return {"state": "success",
                    "statuses": [{"context": "CI / all-required (push)", "status": "success"}]}
        # PR head status fetch fails — fail-closed must propagate.
        raise mq.ApiError("GET /commits/HEAD/status -> HTTP 502: bad gateway")
    monkeypatch.setattr(mq, "get_combined_status", fake_combined)

    monkeypatch.setattr(mq, "list_candidate_issues", lambda *, auto_discover: [
        {"number": 101, "pull_request": {}, "labels": [{"name": "merge-queue"}],
         "created_at": "2026-06-01T00:00:00Z"},
    ])
    monkeypatch.setattr(mq, "get_pull", lambda n: {
        "state": "open", "number": n, "mergeable": True,
        "base": {"ref": "main", "repo_id": 1},
        "head": {"sha": head_sha, "repo_id": 1},
        "labels": [{"name": "merge-queue"}],
    })
    monkeypatch.setattr(mq, "get_pull_commits", lambda n: [{"sha": main_sha}, {"sha": head_sha}])
    monkeypatch.setattr(mq, "get_pull_reviews", lambda n: [])

    def fake_merge(*a, **k):
        merged["called"] = True
    monkeypatch.setattr(mq, "merge_pull", fake_merge)

    # process_once lets the ApiError propagate; main() swallows it as a tick
    # no-op. Either way, no merge happens.
    import pytest
    with pytest.raises(mq.ApiError):
        mq.process_once(dry_run=False)
    assert merged["called"] is False


# --------------------------------------------------------------------------
# Pagination: api_paginated loops pages and is fail-closed on page errors
# --------------------------------------------------------------------------

def test_api_paginated_loops_pages_until_partial(monkeypatch):
    """api_paginated fetches all pages and stops when a page is < page_size."""
    calls = []

    def fake_api(method, path, *, query=None, **kw):
        page = int((query or {}).get("page", "1"))
        limit = int((query or {}).get("limit", "50"))
        calls.append((page, limit))
        if page == 1:
            return 200, [{"number": 1}, {"number": 2}]
        if page == 2:
            return 200, [{"number": 3}]
        return 200, []

    monkeypatch.setattr(mq, "api", fake_api)
    results = mq.api_paginated("GET", "/repos/o/r/issues", page_size=2)
    assert len(results) == 3
    assert results[0]["number"] == 1
    assert results[1]["number"] == 2
    assert results[2]["number"] == 3
    assert calls == [(1, 2), (2, 2)]


def test_api_paginated_raises_on_non_list(monkeypatch):
    """A page that returns a dict instead of list is an error."""
    def fake_api(method, path, *, query=None, **kw):
        return 200, {"message": "not found"}

    monkeypatch.setattr(mq, "api", fake_api)
    with pytest.raises(mq.ApiError):
        mq.api_paginated("GET", "/repos/o/r/issues")


def test_get_combined_status_propagates_paginated_statuses_error(monkeypatch):
    """If the paginated /statuses enrichment raises, the error propagates
    (fail-closed — we do NOT silently fall back to an incomplete status set)."""
    monkeypatch.setattr(mq, "OWNER", "o")
    monkeypatch.setattr(mq, "NAME", "r")

    def fake_api(method, path, *, query=None, **kw):
        if path.endswith("/status"):
            return 200, {"state": "success", "statuses": [{"context": "c1", "status": "success", "id": 1}]}
        if path.endswith("/statuses"):
            raise mq.ApiError("GET /statuses -> HTTP 502")
        raise mq.ApiError(f"unexpected {path}")

    monkeypatch.setattr(mq, "api", fake_api)
    with pytest.raises(mq.ApiError, match="GET /statuses"):
        mq.get_combined_status("a" * 40)


def test_process_once_holds_tick_when_branch_protection_unavailable(monkeypatch):
    """BP enumeration failure → HOLD the whole tick (no merge, rc 0)."""
    merged = {"called": False}
    monkeypatch.setattr(mq, "WATCH_BRANCH", "main")

    def fake_bp(branch):
        raise mq.BranchProtectionUnavailable("403 forbidden")
    monkeypatch.setattr(mq, "get_branch_protection", fake_bp)
    monkeypatch.setattr(mq, "merge_pull", lambda *a, **k: merged.__setitem__("called", True))

    rc = mq.process_once(dry_run=False)
    assert rc == 0
    assert merged["called"] is False


# --------------------------------------------------------------------------
# Fix 4 (issue #2352): a persistent 409-conflict-on-update HOLDS the PR and
# the queue ADVANCES — it does not retry the conflicted PR forever (HOL).
# --------------------------------------------------------------------------

def test_BranchUpdateConflictError_inherits_from_ApiError():
    assert issubclass(mq.BranchUpdateConflictError, mq.ApiError)


def test_update_pull_raises_conflict_error_on_409(monkeypatch):
    """A 409 from the /update endpoint becomes BranchUpdateConflictError so
    process_once can HOLD-and-advance rather than letting it propagate as a
    plain ApiError (which would leave the PR queued and re-selectable)."""
    monkeypatch.setattr(mq, "OWNER", "molecule-ai")
    monkeypatch.setattr(mq, "NAME", "molecule-core")

    def fake_api(method, path, **kwargs):
        raise mq.ApiError("POST /pulls/1409/update -> HTTP 409: merge conflict")
    monkeypatch.setattr(mq, "api", fake_api)

    import pytest
    with pytest.raises(mq.BranchUpdateConflictError) as exc_info:
        mq.update_pull(1409, dry_run=False)
    assert "409" in str(exc_info.value)


def test_update_pull_reraises_non_409_errors(monkeypatch):
    """Non-409 update failures (e.g. 500) are NOT conflicts; they must NOT be
    swallowed as a hold — they re-raise as the original ApiError so the tick is
    a transient no-op and the PR is retried next tick."""
    monkeypatch.setattr(mq, "OWNER", "molecule-ai")
    monkeypatch.setattr(mq, "NAME", "molecule-core")

    def fake_api(method, path, **kwargs):
        raise mq.ApiError("POST /pulls/1409/update -> HTTP 500: server error")
    monkeypatch.setattr(mq, "api", fake_api)

    import pytest
    with pytest.raises(mq.ApiError) as exc_info:
        mq.update_pull(1409, dry_run=False)
    # Re-raised as plain ApiError, NOT the conflict subclass.
    assert not isinstance(exc_info.value, mq.BranchUpdateConflictError)
    assert "500" in str(exc_info.value)


def _stale_pr_update_409_monkeypatch(monkeypatch, queued_issues, calls):
    """Wire process_once so the selected PR needs an update (head does NOT
    contain main) and the /update call returns a 409 conflict. Everything else
    is green. Records merge attempts and the applied hold label in `calls`."""
    monkeypatch.setattr(mq, "OWNER", "molecule-ai")
    monkeypatch.setattr(mq, "NAME", "molecule-core")
    monkeypatch.setattr(mq, "WATCH_BRANCH", "main")
    monkeypatch.setattr(mq, "QUEUE_LABEL", "merge-queue")
    monkeypatch.setattr(mq, "HOLD_LABEL", "merge-queue-hold")
    monkeypatch.setattr(mq, "AUTO_DISCOVER", True)
    monkeypatch.setattr(mq, "OPT_OUT_LABELS", {"merge-queue-hold", "do-not-auto-merge", "wip"})
    monkeypatch.setattr(mq, "REVIEWER_SET", REVIEWERS)
    monkeypatch.setattr(mq, "get_branch_protection", lambda branch: mq.BranchProtection(
        required_contexts=["CI / all-required (pull_request)"],
        required_approvals=2,
        block_on_rejected_reviews=True,
    ))
    main_sha = "b" * 40
    head_sha = "a" * 40
    monkeypatch.setattr(mq, "get_branch_head", lambda branch: main_sha)

    def fake_combined(sha, *, prefer_live=False):
        if sha == main_sha:
            return {"state": "success", "statuses": [{"context": "CI / all-required (push)", "status": "success"}]}
        return {"state": "success", "statuses": [
            {"context": "CI / all-required (pull_request)", "status": "success"},
            {"context": "CI / Platform (Go) (pull_request)", "status": "success"},
            {"context": "qa-review / approved (pull_request_target)", "status": "success"},
            {"context": "security-review / approved (pull_request_target)", "status": "success"},
            {"context": "sop-checklist / all-items-acked (pull_request_target)", "status": "success"},
        ]}
    monkeypatch.setattr(mq, "get_combined_status", fake_combined)

    # Scan-loop process_once enumerates candidates via list_candidate_issues.
    monkeypatch.setattr(mq, "list_candidate_issues", lambda *, auto_discover: queued_issues)
    monkeypatch.setattr(mq, "get_pull", lambda n: {
        "state": "open", "number": n, "mergeable": False,
        "base": {"ref": "main", "repo_id": 1},
        "head": {"sha": head_sha, "repo_id": 1},
        "labels": [{"name": "merge-queue"}],
    })
    # NOTE: mergeable is False (real conflict) AND commits do NOT contain
    # main_sha → pr_has_current_base is False → decision.action == "update".
    # Under the #2358 direct-merge fix the update path is reached ONLY when the
    # PR is NOT mergeable; a mergeable=True behind-main PR would merge directly,
    # so this fixture sets mergeable=False to exercise the #2352 409-on-update
    # hold path.
    monkeypatch.setattr(mq, "get_pull_commits", lambda n: [{"sha": head_sha}])
    monkeypatch.setattr(mq, "get_pull_reviews", lambda n: [
        {"state": "APPROVED", "user": {"login": "agent-researcher"},
         "official": True, "stale": False, "dismissed": False, "commit_id": head_sha},
        {"state": "APPROVED", "user": {"login": "agent-reviewer-cr2"},
         "official": True, "stale": False, "dismissed": False, "commit_id": head_sha},
    ])

    def fake_update(pr_number, *, dry_run):
        calls["update_attempts"] += 1
        raise mq.BranchUpdateConflictError(
            "POST /pulls/%d/update -> HTTP 409: merge conflict" % pr_number
        )
    monkeypatch.setattr(mq, "update_pull", fake_update)

    def fake_merge(pr_number, *, dry_run, force=False):
        calls["merge_attempts"] += 1
    monkeypatch.setattr(mq, "merge_pull", fake_merge)

    def fake_add_label(pr_number, label_name, *, dry_run):
        calls["hold_label"] = (pr_number, label_name)
        calls.setdefault("holds", []).append((pr_number, label_name))
    monkeypatch.setattr(mq, "add_label_by_name", fake_add_label)
    monkeypatch.setattr(mq, "post_comment", lambda *a, **k: None)


def test_process_once_holds_pr_on_409_conflict_on_update(monkeypatch):
    """The #2352 regression: a queued PR whose /update returns 409 must get the
    HOLD_LABEL (so the queue advances) and must NOT be merged. Without the fix
    the 409 propagated, the PR stayed queued, and the next tick re-selected the
    SAME conflicted PR forever (head-of-line block)."""
    calls = {"update_attempts": 0, "merge_attempts": 0, "hold_label": None}
    _stale_pr_update_409_monkeypatch(
        monkeypatch,
        queued_issues=[
            {"number": 1409, "pull_request": {}, "labels": [{"name": "merge-queue"}],
             "created_at": "2026-06-01T00:00:00Z"},
        ],
        calls=calls,
    )

    rc = mq.process_once(dry_run=False)

    assert rc == 0
    assert calls["update_attempts"] == 1
    # Held, not merged — fail-closed.
    assert calls["hold_label"] == (1409, "merge-queue-hold")
    assert calls["merge_attempts"] == 0


def test_queue_advances_past_held_conflicted_pr(monkeypatch):
    """End-to-end HOL proof for #2352 under the scan-loop architecture: PR #1409
    (oldest) hits a 409-on-update and is HELD (HOLD_LABEL applied); once held it
    carries an opt-out label so it is excluded from candidate selection and can
    never re-block the queue. The 409-conflict hold (#2354) and the
    scan-through-skip (#2356) coexist: a held conflicted PR is both held AND no
    longer a candidate, so newer ready PRs behind it are unblocked."""
    calls = {"update_attempts": 0, "merge_attempts": 0, "hold_label": None}
    conflicted = {"number": 1409, "pull_request": {},
                  "labels": [{"name": "merge-queue"}],
                  "created_at": "2026-06-01T00:00:00Z"}
    next_ready = {"number": 1500, "pull_request": {},
                  "labels": [{"name": "merge-queue"}],
                  "created_at": "2026-06-02T00:00:00Z"}
    _stale_pr_update_409_monkeypatch(
        monkeypatch,
        queued_issues=[conflicted, next_ready],
        calls=calls,
    )

    # Tick 1: oldest (#1409) is selected, 409-on-update → held, then the scan
    # CONTINUES to #1500 (which also 409s in this fixture and is likewise held).
    # The key #2352 property: the conflicted oldest PR is held and does NOT stop
    # the scan from advancing past it.
    rc = mq.process_once(dry_run=False)
    assert rc == 0
    assert (1409, "merge-queue-hold") in calls["holds"]
    assert calls["merge_attempts"] == 0  # held, not merged — fail-closed

    # Simulate the label now present on #1409 (as the real hold would persist).
    conflicted["labels"] = [{"name": "merge-queue"}, {"name": "merge-queue-hold"}]

    # Next selection: the scan-loop candidate selector must SKIP the now-held
    # #1409 (HOLD_LABEL is in OPT_OUT_LABELS) and surface the next ready
    # candidate #1500 — the held PR no longer head-of-line blocks. The legacy
    # opt-IN selector (choose_next_queued_issue) honours the same hold.
    opt_out = {"merge-queue-hold", "do-not-auto-merge", "wip"}
    remaining = mq.choose_candidate_issues(
        [conflicted, next_ready],
        queue_label="merge-queue",
        opt_out_labels=opt_out,
        auto_discover=True,
    )
    assert [i["number"] for i in remaining] == [1500]
    selected = mq.choose_next_queued_issue(
        [conflicted, next_ready],
        queue_label="merge-queue",
        hold_label="merge-queue-hold",
    )
    assert selected is not None
    assert selected["number"] == 1500


# --------------------------------------------------------------------------
# §SOP-22: AUTO-DISCOVERY (opt-OUT, label-optional). The queue must be
# self-sustaining — a ready PR is considered/merged with NO `merge-queue`
# label, while opt-out labels (merge-queue-hold / do-not-auto-merge / wip) and
# drafts are skipped. The merge bar (approvals/required-green/mergeable) is
# unchanged; only candidate selection changes.
# --------------------------------------------------------------------------

OPT_OUT = {"merge-queue-hold", "do-not-auto-merge", "wip"}


def _issue(number, labels, *, created="2026-06-01T00:00:00Z", draft=False, is_pr=True):
    pr = {"draft": draft} if is_pr else None
    out = {
        "number": number,
        "labels": [{"name": n} for n in labels],
        "created_at": created,
    }
    if pr is not None:
        out["pull_request"] = pr
    return out


def test_auto_discover_selects_unlabeled_ready_pr():
    """A ready PR with NO merge-queue label is auto-considered (the autonomy fix:
    agents cannot self-label because their token lacks write:issue)."""
    issues = [_issue(50, labels=[])]  # no merge-queue label at all
    selected = mq.choose_next_candidate_issue(
        issues, queue_label="merge-queue", opt_out_labels=OPT_OUT, auto_discover=True
    )
    assert selected is not None
    assert selected["number"] == 50


def test_auto_discover_skips_opt_out_labels():
    """Each opt-out label keeps a PR OUT of autonomous merging (the human escape
    hatch). A PR carrying any of them is never selected even though it is open."""
    for optout in OPT_OUT:
        issues = [_issue(60, labels=[optout])]
        selected = mq.choose_next_candidate_issue(
            issues, queue_label="merge-queue", opt_out_labels=OPT_OUT, auto_discover=True
        )
        assert selected is None, f"{optout!r} should opt the PR out"


def test_auto_discover_skips_opt_out_even_when_queue_labeled():
    """An opt-out label beats the merge-queue label: a held/wip PR that also
    carries merge-queue is still skipped."""
    issues = [_issue(61, labels=["merge-queue", "wip"])]
    selected = mq.choose_next_candidate_issue(
        issues, queue_label="merge-queue", opt_out_labels=OPT_OUT, auto_discover=True
    )
    assert selected is None


def test_auto_discover_skips_drafts():
    issues = [_issue(62, labels=[], draft=True)]
    selected = mq.choose_next_candidate_issue(
        issues, queue_label="merge-queue", opt_out_labels=OPT_OUT, auto_discover=True
    )
    assert selected is None


def test_auto_discover_skips_non_pull_issues():
    """A plain issue (no pull_request key) is never a merge candidate."""
    issues = [_issue(63, labels=[], is_pr=False)]
    selected = mq.choose_next_candidate_issue(
        issues, queue_label="merge-queue", opt_out_labels=OPT_OUT, auto_discover=True
    )
    assert selected is None


def test_auto_discover_oldest_first_skipping_opt_out():
    """Selection is FIFO (oldest created_at first), and the opt-out PR is passed
    over for the next-oldest eligible PR."""
    issues = [
        _issue(70, labels=["do-not-auto-merge"], created="2026-06-01T01:00:00Z"),
        _issue(71, labels=[], created="2026-06-01T02:00:00Z"),
        _issue(72, labels=["merge-queue"], created="2026-06-01T03:00:00Z"),
    ]
    selected = mq.choose_next_candidate_issue(
        issues, queue_label="merge-queue", opt_out_labels=OPT_OUT, auto_discover=True
    )
    assert selected["number"] == 71  # 70 opted out, 71 is next-oldest eligible


def test_opt_in_mode_requires_queue_label():
    """AUTO_DISCOVER off restores legacy opt-IN: only merge-queue-labeled PRs are
    candidates; an unlabeled ready PR is NOT selected."""
    issues = [
        _issue(80, labels=[], created="2026-06-01T01:00:00Z"),
        _issue(81, labels=["merge-queue"], created="2026-06-01T02:00:00Z"),
    ]
    selected = mq.choose_next_candidate_issue(
        issues, queue_label="merge-queue", opt_out_labels=OPT_OUT, auto_discover=False
    )
    assert selected["number"] == 81


def test_opt_in_mode_still_honours_opt_out():
    """Even in opt-IN mode, an opt-out label on a queue-labeled PR skips it."""
    issues = [_issue(82, labels=["merge-queue", "merge-queue-hold"])]
    selected = mq.choose_next_candidate_issue(
        issues, queue_label="merge-queue", opt_out_labels=OPT_OUT, auto_discover=False
    )
    assert selected is None


def test_list_candidate_issues_omits_label_filter_when_auto_discover(monkeypatch):
    """The auto-discovery listing must NOT pass a `labels` filter (so unlabeled
    PRs are enumerated); the opt-IN listing must keep filtering by QUEUE_LABEL."""
    captured = {}

    def fake_api(method, path, *, query=None, **kw):
        captured["query"] = dict(query or {})
        return 200, []

    monkeypatch.setattr(mq, "api", fake_api)
    monkeypatch.setattr(mq, "QUEUE_LABEL", "merge-queue")

    mq.list_candidate_issues(auto_discover=True)
    assert "labels" not in captured["query"]
    assert captured["query"].get("type") == "pulls"

    mq.list_candidate_issues(auto_discover=False)
    assert captured["query"].get("label") == "merge-queue"


def _wire_ready_process_once(monkeypatch, *, issues, pr_payload, calls):
    """Wire process_once fully green EXCEPT candidate selection / pull payload,
    which the caller supplies to exercise auto-discovery end-to-end."""
    monkeypatch.setattr(mq, "OWNER", "molecule-ai")
    monkeypatch.setattr(mq, "NAME", "molecule-core")
    monkeypatch.setattr(mq, "WATCH_BRANCH", "main")
    monkeypatch.setattr(mq, "QUEUE_LABEL", "merge-queue")
    monkeypatch.setattr(mq, "HOLD_LABEL", "merge-queue-hold")
    monkeypatch.setattr(mq, "AUTO_DISCOVER", True)
    monkeypatch.setattr(mq, "OPT_OUT_LABELS", OPT_OUT)
    monkeypatch.setattr(mq, "REVIEWER_SET", REVIEWERS)
    monkeypatch.setattr(mq, "get_branch_protection", lambda branch: mq.BranchProtection(
        required_contexts=["CI / all-required (pull_request)"],
        required_approvals=2, block_on_rejected_reviews=True,
    ))
    main_sha = "b" * 40
    head_sha = "a" * 40
    monkeypatch.setattr(mq, "get_branch_head", lambda branch: main_sha)

    def fake_combined(sha, *, prefer_live=False):
        if sha == main_sha:
            return {"state": "success", "statuses": [
                {"context": "CI / all-required (push)", "status": "success"},
            ]}
        return {"state": "success", "statuses": [
            {"context": "CI / all-required (pull_request)", "status": "success"},
            {"context": "CI / Platform (Go) (pull_request)", "status": "success"},
            {"context": "qa-review / approved (pull_request_target)", "status": "success"},
            {"context": "security-review / approved (pull_request_target)", "status": "success"},
            {"context": "sop-checklist / all-items-acked (pull_request_target)", "status": "success"},
        ]}
    monkeypatch.setattr(mq, "get_combined_status", fake_combined)
    monkeypatch.setattr(mq, "list_candidate_issues", lambda *, auto_discover: issues)
    monkeypatch.setattr(mq, "get_pull", lambda n: dict(pr_payload, number=n))
    monkeypatch.setattr(mq, "get_pull_commits", lambda n: [{"sha": main_sha}, {"sha": head_sha}])
    monkeypatch.setattr(mq, "get_pull_reviews", lambda n: [
        {"state": "APPROVED", "user": {"login": "agent-researcher"},
         "official": True, "stale": False, "dismissed": False, "commit_id": head_sha},
        {"state": "APPROVED", "user": {"login": "agent-reviewer-cr2"},
         "official": True, "stale": False, "dismissed": False, "commit_id": head_sha},
    ])

    def fake_merge(pr_number, *, dry_run, force=False):
        calls["merged"] = pr_number
    monkeypatch.setattr(mq, "merge_pull", fake_merge)
    monkeypatch.setattr(mq, "update_pull", lambda *a, **k: calls.__setitem__("updated", True))
    monkeypatch.setattr(mq, "post_comment", lambda *a, **k: None)
    monkeypatch.setattr(mq, "add_label_by_name", lambda *a, **k: None)
    return main_sha, head_sha


def test_process_once_auto_merges_unlabeled_ready_pr(monkeypatch):
    """End-to-end: a fully-ready PR with NO merge-queue label is auto-merged.
    This is the core autonomy fix — no human/agent labeling required."""
    calls = {"merged": None, "updated": False}
    head_sha = "a" * 40
    _wire_ready_process_once(
        monkeypatch,
        issues=[_issue(90, labels=[])],  # NO merge-queue label
        pr_payload={
            "state": "open", "mergeable": True, "draft": False,
            "base": {"ref": "main", "repo_id": 1},
            "head": {"sha": head_sha, "repo_id": 1},
            "labels": [],
        },
        calls=calls,
    )

    rc = mq.process_once(dry_run=False)

    assert rc == 0
    assert calls["merged"] == 90  # merged despite no merge-queue label


def test_process_once_skips_opt_out_labeled_pr(monkeypatch):
    """A fully-ready PR carrying an opt-out label is NOT merged (skipped)."""
    for optout in OPT_OUT:
        calls = {"merged": None, "updated": False}
        head_sha = "a" * 40
        _wire_ready_process_once(
            monkeypatch,
            issues=[_issue(91, labels=[optout])],
            pr_payload={
                "state": "open", "mergeable": True, "draft": False,
                "base": {"ref": "main", "repo_id": 1},
                "head": {"sha": head_sha, "repo_id": 1},
                "labels": [{"name": optout}],
            },
            calls=calls,
        )
        rc = mq.process_once(dry_run=False)
        assert rc == 0
        assert calls["merged"] is None, f"{optout!r} PR must not be merged"


def test_process_once_does_not_merge_unapproved_pr(monkeypatch):
    """A not-ready PR (only one genuine approval) is auto-considered but NOT
    merged — auto-discovery does not lower the merge bar."""
    calls = {"merged": None, "updated": False}
    head_sha = "a" * 40
    main_sha, _ = _wire_ready_process_once(
        monkeypatch,
        issues=[_issue(92, labels=[])],
        pr_payload={
            "state": "open", "mergeable": True, "draft": False,
            "base": {"ref": "main", "repo_id": 1},
            "head": {"sha": head_sha, "repo_id": 1},
            "labels": [],
        },
        calls=calls,
    )
    # Only ONE genuine approval → below the required 2.
    monkeypatch.setattr(mq, "get_pull_reviews", lambda n: [
        {"state": "APPROVED", "user": {"login": "agent-researcher"},
         "official": True, "stale": False, "dismissed": False, "commit_id": head_sha},
    ])

    rc = mq.process_once(dry_run=False)

    assert rc == 0
    assert calls["merged"] is None


def test_process_once_does_not_merge_red_required_pr(monkeypatch):
    """A not-ready PR (required context red) is auto-considered but NOT merged."""
    calls = {"merged": None, "updated": False}
    head_sha = "a" * 40
    main_sha = "b" * 40
    _wire_ready_process_once(
        monkeypatch,
        issues=[_issue(93, labels=[])],
        pr_payload={
            "state": "open", "mergeable": True, "draft": False,
            "base": {"ref": "main", "repo_id": 1},
            "head": {"sha": head_sha, "repo_id": 1},
            "labels": [],
        },
        calls=calls,
    )

    # Required PR context is FAILURE; main stays green.
    def fake_combined(sha, *, prefer_live=False):
        if sha == main_sha:
            return {"state": "success",
                    "statuses": [{"context": "CI / all-required (push)", "status": "success"}]}
        return {"state": "failure",
                "statuses": [{"context": "CI / all-required (pull_request)", "status": "failure"}]}
    monkeypatch.setattr(mq, "get_combined_status", fake_combined)

    rc = mq.process_once(dry_run=False)

    assert rc == 0
    assert calls["merged"] is None


def test_process_once_does_not_merge_unmergeable_pr(monkeypatch):
    """A not-ready PR (mergeable False = conflicts) is auto-considered but NOT
    merged."""
    calls = {"merged": None, "updated": False}
    head_sha = "a" * 40
    _wire_ready_process_once(
        monkeypatch,
        issues=[_issue(94, labels=[])],
        pr_payload={
            "state": "open", "mergeable": False, "draft": False,
            "base": {"ref": "main", "repo_id": 1},
            "head": {"sha": head_sha, "repo_id": 1},
            "labels": [],
        },
        calls=calls,
    )

    rc = mq.process_once(dry_run=False)

    assert rc == 0
    assert calls["merged"] is None


# --------------------------------------------------------------------------
# §SOP-22 (cont.): HEAD-OF-LINE (HOL) — a non-ready auto-discovered candidate
# must NOT block the newer ready PRs behind it. The queue SCANS THROUGH the
# FIFO candidate list, skipping `wait` candidates (REQUEST_CHANGES, mergeable
# != True, insufficient genuine approvals, or red required CI), and merges the
# first ready PR in the SAME tick. (Regression for the #1519-style false
# candidate the reviewer caught: open + unlabeled + mergeable=false + current-
# head official REQUEST_CHANGES + <2 genuine approvals.)
# --------------------------------------------------------------------------

MAIN_SHA = "b" * 40


def _wire_multi_candidate_process_once(monkeypatch, *, issues, pulls, reviews, calls):
    """Wire process_once for MULTIPLE candidates, dispatching get_pull /
    get_pull_reviews / head-status BY PR NUMBER so each candidate can have a
    different readiness. `pulls` maps number -> pull payload; `reviews` maps
    number -> reviews list. Main is green; each PR head status is green."""
    monkeypatch.setattr(mq, "OWNER", "molecule-ai")
    monkeypatch.setattr(mq, "NAME", "molecule-core")
    monkeypatch.setattr(mq, "WATCH_BRANCH", "main")
    monkeypatch.setattr(mq, "QUEUE_LABEL", "merge-queue")
    monkeypatch.setattr(mq, "HOLD_LABEL", "merge-queue-hold")
    monkeypatch.setattr(mq, "AUTO_DISCOVER", True)
    monkeypatch.setattr(mq, "OPT_OUT_LABELS", OPT_OUT)
    monkeypatch.setattr(mq, "REVIEWER_SET", REVIEWERS)
    monkeypatch.setattr(mq, "get_branch_protection", lambda branch: mq.BranchProtection(
        required_contexts=["CI / all-required (pull_request)"],
        required_approvals=2, block_on_rejected_reviews=True,
    ))
    monkeypatch.setattr(mq, "get_branch_head", lambda branch: MAIN_SHA)

    def fake_combined(sha, *, prefer_live=False):
        if sha == MAIN_SHA:
            return {"state": "success", "statuses": [{"context": "CI / all-required (push)", "status": "success"}]}
        return {"state": "success", "statuses": [
            {"context": "CI / all-required (pull_request)", "status": "success"},
            {"context": "CI / Platform (Go) (pull_request)", "status": "success"},
            {"context": "qa-review / approved (pull_request_target)", "status": "success"},
            {"context": "security-review / approved (pull_request_target)", "status": "success"},
            {"context": "sop-checklist / all-items-acked (pull_request_target)", "status": "success"},
        ]}
    monkeypatch.setattr(mq, "get_combined_status", fake_combined)

    monkeypatch.setattr(mq, "list_candidate_issues", lambda *, auto_discover: issues)
    monkeypatch.setattr(mq, "get_pull", lambda n: dict(pulls[n], number=n))
    # Each PR head contains current main (so no candidate needs an update; the
    # only differentiator is readiness). head sha is the pull's own head.
    monkeypatch.setattr(
        mq, "get_pull_commits",
        lambda n: [{"sha": MAIN_SHA}, {"sha": pulls[n]["head"]["sha"]}],
    )
    monkeypatch.setattr(mq, "get_pull_reviews", lambda n: reviews[n])

    def fake_merge(pr_number, *, dry_run, force=False):
        calls.setdefault("merged", [])
        calls["merged"].append(pr_number)
    monkeypatch.setattr(mq, "merge_pull", fake_merge)
    monkeypatch.setattr(mq, "update_pull", lambda *a, **k: calls.__setitem__("updated", True))
    monkeypatch.setattr(mq, "post_comment", lambda *a, **k: None)
    monkeypatch.setattr(mq, "add_label_by_name", lambda *a, **k: None)


def _two_approvals(head_sha):
    return [
        {"state": "APPROVED", "user": {"login": "agent-researcher"},
         "official": True, "stale": False, "dismissed": False, "commit_id": head_sha},
        {"state": "APPROVED", "user": {"login": "agent-reviewer-cr2"},
         "official": True, "stale": False, "dismissed": False, "commit_id": head_sha},
    ]


def test_hol_unready_oldest_does_not_block_newer_ready_pr(monkeypatch):
    """The OLDEST auto-discovered candidate is NOT ready (mergeable=false). The
    queue must SKIP it and merge the NEWER ready PR in the SAME tick — no HOL."""
    calls = {"updated": False}
    old_head, new_head = "a" * 40, "c" * 40
    _wire_multi_candidate_process_once(
        monkeypatch,
        issues=[
            _issue(500, labels=[], created="2026-06-01T01:00:00Z"),  # oldest, NOT ready
            _issue(501, labels=[], created="2026-06-01T02:00:00Z"),  # newer, READY
        ],
        pulls={
            500: {"state": "open", "mergeable": False, "draft": False,  # conflict
                  "base": {"ref": "main", "repo_id": 1},
                  "head": {"sha": old_head, "repo_id": 1}, "labels": []},
            501: {"state": "open", "mergeable": True, "draft": False,
                  "base": {"ref": "main", "repo_id": 1},
                  "head": {"sha": new_head, "repo_id": 1}, "labels": []},
        },
        reviews={500: _two_approvals(old_head), 501: _two_approvals(new_head)},
        calls=calls,
    )

    rc = mq.process_once(dry_run=False)

    assert rc == 0
    # The newer ready PR merged; the non-ready oldest did not block it.
    assert calls.get("merged") == [501]


def test_hol_1519_style_false_candidate_never_merged_and_never_blocks(monkeypatch):
    """Live #1519 repro: oldest, open, UNLABELED, but mergeable=false + a
    current-head official REQUEST_CHANGES + only ONE genuine approval. It must
    NEVER be merged and must NEVER block the newer ready PR behind it."""
    calls = {"updated": False}
    false_head, ready_head = "a" * 40, "c" * 40
    _wire_multi_candidate_process_once(
        monkeypatch,
        issues=[
            _issue(1519, labels=[], created="2026-05-20T00:00:00Z"),  # oldest false candidate
            _issue(2000, labels=[], created="2026-06-01T00:00:00Z"),  # newer, READY
        ],
        pulls={
            1519: {"state": "open", "mergeable": False, "draft": False,
                   "base": {"ref": "main", "repo_id": 1},
                   "head": {"sha": false_head, "repo_id": 1}, "labels": []},
            2000: {"state": "open", "mergeable": True, "draft": False,
                   "base": {"ref": "main", "repo_id": 1},
                   "head": {"sha": ready_head, "repo_id": 1}, "labels": []},
        },
        reviews={
            1519: [
                # one genuine approval (below 2) ...
                {"state": "APPROVED", "user": {"login": "agent-researcher"},
                 "official": True, "stale": False, "dismissed": False, "commit_id": false_head},
                # ... plus a current-head official REQUEST_CHANGES (human action needed)
                {"state": "REQUEST_CHANGES", "user": {"login": "agent-reviewer"},
                 "official": True, "stale": False, "dismissed": False, "commit_id": false_head},
            ],
            2000: _two_approvals(ready_head),
        },
        calls=calls,
    )

    rc = mq.process_once(dry_run=False)

    assert rc == 0
    # #1519 is never merged; the ready PR behind it merges this same tick.
    assert calls.get("merged") == [2000]
    assert 1519 not in calls.get("merged", [])


def test_hol_unready_red_required_ci_is_skipped_for_ready_pr(monkeypatch):
    """A candidate whose required CI is RED is skipped (not waited-on) so the
    newer ready PR merges in the same tick."""
    calls = {"updated": False}
    red_head, ready_head = "a" * 40, "c" * 40
    _wire_multi_candidate_process_once(
        monkeypatch,
        issues=[
            _issue(600, labels=[], created="2026-06-01T01:00:00Z"),  # required CI red
            _issue(601, labels=[], created="2026-06-01T02:00:00Z"),  # ready
        ],
        pulls={
            600: {"state": "open", "mergeable": True, "draft": False,
                  "base": {"ref": "main", "repo_id": 1},
                  "head": {"sha": red_head, "repo_id": 1}, "labels": []},
            601: {"state": "open", "mergeable": True, "draft": False,
                  "base": {"ref": "main", "repo_id": 1},
                  "head": {"sha": ready_head, "repo_id": 1}, "labels": []},
        },
        reviews={600: _two_approvals(red_head), 601: _two_approvals(ready_head)},
        calls=calls,
    )
    # PR 600's required PR context is FAILURE; 601 (and main) stay green.
    def fake_combined(sha, *, prefer_live=False):
        if sha == MAIN_SHA:
            return {"state": "success",
                    "statuses": [{"context": "CI / all-required (push)", "status": "success"}]}
        state = "failure" if sha == red_head else "success"
        return {"state": state,
                "statuses": [
                    {"context": "CI / all-required (pull_request)", "status": state},
                    {"context": "CI / Platform (Go) (pull_request)", "status": state},
                    {"context": "qa-review / approved (pull_request_target)", "status": "success"},
                    {"context": "security-review / approved (pull_request_target)", "status": "success"},
                    {"context": "sop-checklist / all-items-acked (pull_request_target)", "status": "success"},
                ]}
    monkeypatch.setattr(mq, "get_combined_status", fake_combined)

    rc = mq.process_once(dry_run=False)

    assert rc == 0
    assert calls.get("merged") == [601]


def test_hol_all_candidates_unready_merges_nothing(monkeypatch):
    """If EVERY candidate is non-ready, the queue merges nothing (fail-closed)
    and does not loop — it simply finds no actionable PR this tick."""
    calls = {"updated": False}
    h1, h2 = "a" * 40, "c" * 40
    _wire_multi_candidate_process_once(
        monkeypatch,
        issues=[
            _issue(700, labels=[], created="2026-06-01T01:00:00Z"),  # RC
            _issue(701, labels=[], created="2026-06-01T02:00:00Z"),  # unmergeable
        ],
        pulls={
            700: {"state": "open", "mergeable": True, "draft": False,
                  "base": {"ref": "main", "repo_id": 1},
                  "head": {"sha": h1, "repo_id": 1}, "labels": []},
            701: {"state": "open", "mergeable": False, "draft": False,
                  "base": {"ref": "main", "repo_id": 1},
                  "head": {"sha": h2, "repo_id": 1}, "labels": []},
        },
        reviews={
            700: _two_approvals(h1) + [
                {"state": "REQUEST_CHANGES", "user": {"login": "agent-reviewer"},
                 "official": True, "stale": False, "dismissed": False, "commit_id": h1},
            ],
            701: _two_approvals(h2),
        },
        calls=calls,
    )

    rc = mq.process_once(dry_run=False)

    assert rc == 0
    assert calls.get("merged") is None  # nothing merged; no HOL loop


def test_opt_out_draft_label_excludes_candidate():
    """The literal `draft` label is now an opt-out label (added to the default
    OPT_OUT_LABELS), independent of Gitea draft STATE — a human can opt a PR out
    by labeling it `draft` without converting it to a draft PR."""
    # `draft` must be in the shipped default opt-out set.
    assert "draft" in mq.OPT_OUT_LABELS
    opt_out = OPT_OUT | {"draft"}
    issues = [_issue(800, labels=["draft"], draft=False)]  # label only, not draft STATE
    selected = mq.choose_next_candidate_issue(
        issues, queue_label="merge-queue", opt_out_labels=opt_out, auto_discover=True
    )
    assert selected is None


def test_choose_candidate_issues_returns_full_fifo_list_skipping_opt_out():
    """choose_candidate_issues returns ALL eligible candidates oldest-first (so
    process_once can scan past non-ready ones), skipping opt-out/draft/non-PR."""
    issues = [
        _issue(72, labels=["merge-queue"], created="2026-06-01T03:00:00Z"),
        _issue(70, labels=["do-not-auto-merge"], created="2026-06-01T01:00:00Z"),  # opt-out
        _issue(71, labels=[], created="2026-06-01T02:00:00Z"),
        _issue(73, labels=[], draft=True, created="2026-06-01T00:30:00Z"),         # draft
        _issue(74, labels=[], is_pr=False, created="2026-06-01T00:00:00Z"),        # not a PR
    ]
    ordered = mq.choose_candidate_issues(
        issues, queue_label="merge-queue", opt_out_labels=OPT_OUT, auto_discover=True
    )
    assert [i["number"] for i in ordered] == [71, 72]  # FIFO, opt-out/draft/non-PR dropped


def test_process_once_defensive_skip_when_pull_payload_opted_out(monkeypatch):
    """If the listing missed an opt-out label but the authoritative pull payload
    carries it (stale listing race), process_once must still skip the merge."""
    calls = {"merged": None, "updated": False}
    head_sha = "a" * 40
    _wire_ready_process_once(
        monkeypatch,
        issues=[_issue(95, labels=[])],  # listing shows no opt-out
        pr_payload={
            "state": "open", "mergeable": True, "draft": False,
            "base": {"ref": "main", "repo_id": 1},
            "head": {"sha": head_sha, "repo_id": 1},
            "labels": [{"name": "do-not-auto-merge"}],  # live pull is opted out
        },
        calls=calls,
    )

    rc = mq.process_once(dry_run=False)

    assert rc == 0
    assert calls["merged"] is None


# ---------------------------------------------------------------------------
# readiness-enumeration + post-batch summary
# ---------------------------------------------------------------------------

def test_enumerate_readiness_evaluates_all_candidates(monkeypatch):
    """enumerate_readiness returns every candidate's state, not stopping at
    the first actionable one."""
    old_head, new_head = "a" * 40, "c" * 40
    _wire_multi_candidate_process_once(
        monkeypatch,
        issues=[
            _issue(500, labels=[], created="2026-06-01T01:00:00Z"),
            _issue(501, labels=[], created="2026-06-01T02:00:00Z"),
        ],
        pulls={
            500: {"state": "open", "mergeable": False, "draft": False,
                  "base": {"ref": "main", "repo_id": 1},
                  "head": {"sha": old_head, "repo_id": 1}, "labels": []},
            501: {"state": "open", "mergeable": True, "draft": False,
                  "base": {"ref": "main", "repo_id": 1},
                  "head": {"sha": new_head, "repo_id": 1}, "labels": []},
        },
        reviews={500: _two_approvals(old_head), 501: _two_approvals(new_head)},
        calls={},
    )

    entries = mq.enumerate_readiness(dry_run=False)

    assert len(entries) == 2
    by_num = {e.pr_number: e for e in entries}
    assert by_num[500].decision is not None
    assert by_num[500].decision.ready is False
    assert by_num[501].decision is not None
    assert by_num[501].decision.ready is True


def test_enumerate_readiness_includes_ineligible_pr(monkeypatch):
    """enumerate_readiness marks fork / wrong-base PRs as ineligible
    (decision=None) while still evaluating the rest."""
    head = "a" * 40
    _wire_multi_candidate_process_once(
        monkeypatch,
        issues=[
            _issue(600, labels=[], created="2026-06-01T01:00:00Z"),
            _issue(601, labels=[], created="2026-06-01T02:00:00Z"),
        ],
        pulls={
            600: {"state": "open", "mergeable": True, "draft": False,
                  "base": {"ref": "main", "repo_id": 1},
                  "head": {"sha": head, "repo_id": 2}, "labels": []},  # fork
            601: {"state": "open", "mergeable": True, "draft": False,
                  "base": {"ref": "main", "repo_id": 1},
                  "head": {"sha": head, "repo_id": 1}, "labels": []},
        },
        reviews={600: _two_approvals(head), 601: _two_approvals(head)},
        calls={},
    )

    entries = mq.enumerate_readiness(dry_run=False)

    by_num = {e.pr_number: e for e in entries}
    assert by_num[600].decision is None
    assert "not merge-eligible" in by_num[600].reason
    assert by_num[601].decision is not None
    assert by_num[601].decision.ready is True


def test_enumerate_readiness_fail_closed_on_api_error(monkeypatch):
    """If get_pull raises for one candidate, that candidate is recorded as
    unverifiable; other candidates are still evaluated."""
    head = "a" * 40
    _wire_multi_candidate_process_once(
        monkeypatch,
        issues=[
            _issue(700, labels=[], created="2026-06-01T01:00:00Z"),
            _issue(701, labels=[], created="2026-06-01T02:00:00Z"),
        ],
        pulls={
            700: {"state": "open", "mergeable": True, "draft": False,
                  "base": {"ref": "main", "repo_id": 1},
                  "head": {"sha": head, "repo_id": 1}, "labels": []},
            701: {"state": "open", "mergeable": True, "draft": False,
                  "base": {"ref": "main", "repo_id": 1},
                  "head": {"sha": head, "repo_id": 1}, "labels": []},
        },
        reviews={700: _two_approvals(head), 701: _two_approvals(head)},
        calls={},
    )

    original_get_pull = mq.get_pull
    def failing_get_pull(n):
        if n == 700:
            raise mq.ApiError("simulated API failure")
        return original_get_pull(n)
    monkeypatch.setattr(mq, "get_pull", failing_get_pull)

    entries = mq.enumerate_readiness(dry_run=False)

    by_num = {e.pr_number: e for e in entries}
    assert by_num[700].decision is None
    assert "unverifiable" in by_num[700].reason
    assert by_num[701].decision is not None
    assert by_num[701].decision.ready is True


def test_print_post_batch_summary_counts_correctly(capsys):
    entries = [
        mq.ReadinessEntry(pr_number=1, decision=mq.MergeDecision(True, "merge", "ready"), reason="ready"),
        mq.ReadinessEntry(pr_number=2, decision=mq.MergeDecision(False, "wait", "CI red"), reason="CI red"),
        mq.ReadinessEntry(pr_number=3, decision=None, reason="draft"),
    ]
    mq.print_post_batch_summary(entries)
    captured = capsys.readouterr()
    out = captured.out
    assert "total_candidates=3" in out
    assert "ready=1" in out
    assert "waiting=1" in out
    assert "ineligible/unverifiable=1" in out
    assert "PR #1: state=ready" in out
    assert "PR #2: state=waiting" in out
    assert "PR #3: state=ineligible" in out


# ---------------------------------------------------------------------------
# Conductor snapshot consumption (operator-config#158 / molecule-core#2502)
# ---------------------------------------------------------------------------

import json
import os
import tempfile


def _fresh_ts():
    # Conductor snapshots are only honored within a 10-minute freshness window
    # (load_conductor_snapshot in gitea-merge-queue.py). A frozen literal ts
    # goes stale the moment wall-clock passes that window, silently dropping the
    # snapshot and self-fetching -> empty OWNER/NAME -> "/repos///" crash. Default
    # to NOW so the snapshot is fresh whenever the suite runs. Tests that want a
    # STALE snapshot pass ts= explicitly (test_load_conductor_snapshot_ignores_stale_snapshot).
    from datetime import datetime, timezone
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def _make_snapshot(prs, ts=None):
    return {"ts": ts if ts is not None else _fresh_ts(),
            "repo": "molecule-ai/molecule-core", "prs": prs}


def test_list_candidate_issues_uses_snapshot_when_present(monkeypatch):
    """When CONDUCTOR_SNAPSHOT_FILE is present and fresh, list_candidate_issues
    returns the snapshot PRs instead of hitting the API."""
    snapshot = _make_snapshot([
        {"number": 10, "title": "PR 10", "head_sha": "a" * 40,
         "labels": ["merge-queue"],
         "combined_state": "success", "statuses": []},
        {"number": 20, "title": "PR 20", "head_sha": "b" * 40,
         "labels": [],
         "combined_state": "success", "statuses": []},
    ])
    with tempfile.NamedTemporaryFile(mode="w", suffix=".json", delete=False) as f:
        json.dump(snapshot, f)
        path = f.name
    try:
        monkeypatch.setenv("CONDUCTOR_SNAPSHOT_FILE", path)
        # reload so load_conductor_snapshot sees the env var
        candidates = mq.list_candidate_issues(auto_discover=True)
        assert len(candidates) == 2
        assert [c["number"] for c in candidates] == [10, 20]
    finally:
        os.unlink(path)


def test_list_queued_issues_uses_snapshot_label_filter(monkeypatch):
    """list_queued_issues (opt-IN mode) filters the snapshot by QUEUE_LABEL."""
    snapshot = _make_snapshot([
        {"number": 11, "title": "Labeled", "head_sha": "a" * 40,
         "labels": ["merge-queue"], "combined_state": "success", "statuses": []},
        {"number": 22, "title": "Unlabeled", "head_sha": "b" * 40,
         "labels": [], "combined_state": "success", "statuses": []},
    ])
    with tempfile.NamedTemporaryFile(mode="w", suffix=".json", delete=False) as f:
        json.dump(snapshot, f)
        path = f.name
    try:
        monkeypatch.setenv("CONDUCTOR_SNAPSHOT_FILE", path)
        monkeypatch.setattr(mq, "QUEUE_LABEL", "merge-queue")
        queued = mq.list_queued_issues()
        assert len(queued) == 1
        assert queued[0]["number"] == 11
    finally:
        os.unlink(path)


def test_get_combined_status_uses_snapshot_when_sha_matches(monkeypatch):
    """get_combined_status returns snapshot data when the SHA is an open PR head."""
    head_sha = "c" * 40
    snapshot = _make_snapshot([
        {"number": 30, "title": "PR 30", "head_sha": head_sha,
         "labels": [],
         "combined_state": "failure",
         "statuses": [
             {"context": "CI / all-required (pull_request)", "status": "failure"},
         ]},
    ])
    with tempfile.NamedTemporaryFile(mode="w", suffix=".json", delete=False) as f:
        json.dump(snapshot, f)
        path = f.name
    try:
        monkeypatch.setenv("CONDUCTOR_SNAPSHOT_FILE", path)
        combined = mq.get_combined_status(head_sha)
        assert combined["state"] == "failure"
        assert len(combined["statuses"]) == 1
        assert combined["statuses"][0]["context"] == "CI / all-required (pull_request)"
        assert combined["statuses"][0]["status"] == "failure"
    finally:
        os.unlink(path)


def test_get_combined_status_self_fetches_when_sha_not_in_snapshot(monkeypatch):
    """If the SHA is not in the snapshot, get_combined_status falls back to API."""
    snapshot = _make_snapshot([
        {"number": 40, "head_sha": "d" * 40, "labels": [],
         "combined_state": "success", "statuses": []},
    ])
    with tempfile.NamedTemporaryFile(mode="w", suffix=".json", delete=False) as f:
        json.dump(snapshot, f)
        path = f.name
    try:
        monkeypatch.setenv("CONDUCTOR_SNAPSHOT_FILE", path)
        monkeypatch.setattr(mq, "OWNER", "o")
        monkeypatch.setattr(mq, "NAME", "r")

        def fake_api(method, path, **kw):
            if path.endswith("/status"):
                return 200, {"state": "success", "statuses": [{"context": "c1", "status": "success"}]}
            if path.endswith("/statuses"):
                return 200, []
            raise mq.ApiError("unexpected")

        monkeypatch.setattr(mq, "api", fake_api)
        combined = mq.get_combined_status("e" * 40)
        assert combined["state"] == "success"
    finally:
        os.unlink(path)


def test_load_conductor_snapshot_ignores_stale_snapshot(monkeypatch):
    """A snapshot older than 10 minutes is treated as absent (self-fetch)."""
    from datetime import datetime, timezone, timedelta
    old_ts = (datetime.now(timezone.utc) - timedelta(minutes=15)).strftime("%Y-%m-%dT%H:%M:%SZ")
    snapshot = _make_snapshot([{"number": 50, "head_sha": "f" * 40, "labels": []}], ts=old_ts)
    with tempfile.NamedTemporaryFile(mode="w", suffix=".json", delete=False) as f:
        json.dump(snapshot, f)
        path = f.name
    try:
        monkeypatch.setenv("CONDUCTOR_SNAPSHOT_FILE", path)
        assert mq.load_conductor_snapshot() is None
    finally:
        os.unlink(path)


def test_load_conductor_snapshot_discards_snapshot_without_ts(monkeypatch):
    """internal#3210 MEDIUM (fail-closed): a snapshot with NO `ts` field has an
    UNVERIFIABLE age — it could be arbitrarily old (a wedged conductor). The old
    `if ts_str:` guard skipped the age check entirely on an absent/empty ts and
    trusted the snapshot as fresh. It must instead be discarded (return None →
    the caller self-fetches live state), never trusted as current."""
    snapshot = {  # no "ts" key at all
        "repo": "molecule-ai/molecule-core",
        "prs": [{"number": 60, "head_sha": "a" * 40, "labels": []}],
    }
    with tempfile.NamedTemporaryFile(mode="w", suffix=".json", delete=False) as f:
        json.dump(snapshot, f)
        path = f.name
    try:
        monkeypatch.setenv("CONDUCTOR_SNAPSHOT_FILE", path)
        assert mq.load_conductor_snapshot() is None
    finally:
        os.unlink(path)

    # An explicitly EMPTY ts is the same case (freshness unverifiable).
    snapshot_empty = _make_snapshot(
        [{"number": 61, "head_sha": "b" * 40, "labels": []}], ts=""
    )
    with tempfile.NamedTemporaryFile(mode="w", suffix=".json", delete=False) as f:
        json.dump(snapshot_empty, f)
        path2 = f.name
    try:
        monkeypatch.setenv("CONDUCTOR_SNAPSHOT_FILE", path2)
        assert mq.load_conductor_snapshot() is None
    finally:
        os.unlink(path2)


def test_load_conductor_snapshot_discards_snapshot_with_malformed_ts(monkeypatch):
    """internal#3210 MEDIUM (fail-closed): a snapshot whose `ts` does not parse
    has an UNVERIFIABLE age. The old `except ValueError: pass` swallowed the
    strptime failure and fell through to `return snapshot` ('treat as fresh
    (conservative)') — which was ANTI-conservative (an old snapshot from a
    wedged conductor would be trusted as current). It must be discarded (return
    None → self-fetch)."""
    snapshot = _make_snapshot(
        [{"number": 70, "head_sha": "c" * 40, "labels": []}],
        ts="not-a-timestamp",
    )
    with tempfile.NamedTemporaryFile(mode="w", suffix=".json", delete=False) as f:
        json.dump(snapshot, f)
        path = f.name
    try:
        monkeypatch.setenv("CONDUCTOR_SNAPSHOT_FILE", path)
        assert mq.load_conductor_snapshot() is None
    finally:
        os.unlink(path)


def test_load_conductor_snapshot_honors_fresh_dated_snapshot(monkeypatch):
    """The fail-closed ts guard does NOT regress the happy path: a present,
    parseable, in-window ts is still trusted and returned (so the existing
    snapshot-consumption fast path is preserved)."""
    snapshot = _make_snapshot(
        [{"number": 80, "head_sha": "d" * 40, "labels": []}]
    )  # _make_snapshot defaults ts to now → fresh + parseable
    with tempfile.NamedTemporaryFile(mode="w", suffix=".json", delete=False) as f:
        json.dump(snapshot, f)
        path = f.name
    try:
        monkeypatch.setenv("CONDUCTOR_SNAPSHOT_FILE", path)
        loaded = mq.load_conductor_snapshot()
        assert loaded is not None
        assert loaded["prs"][0]["number"] == 80
    finally:
        os.unlink(path)


# --------------------------------------------------------------------------
# Fix 3: non-main base PRs are skipped loudly/observably, not silently.
# core#2548 stopped the per-tick PR-comment flood; the skip must still be
# observable in workflow logs and must not affect legitimate main-targeted PRs.
# --------------------------------------------------------------------------

def _make_pr(**overrides) -> dict:
    base = {
        "state": "open",
        "draft": False,
        "labels": [],
        "base": {"ref": "main", "repo_id": 1},
        "head": {"repo_id": 1, "sha": "a" * 40},
    }
    base.update(overrides)
    return base


def test_early_skip_reason_skips_non_main_base_observably():
    """A stacked PR whose base is not main is skipped with an observable reason
    and NO PR comment (the observable path is the workflow notice)."""
    pr = _make_pr(base={"ref": "feature/stacked", "repo_id": 1})
    reason, comment = mq._early_skip_reason(pr, watch_branch="main")
    assert reason == "base is not `main`"
    assert comment is None


def test_early_skip_reason_does_not_skip_main_targeted_pr():
    """A legitimate main-targeted PR is not skipped for base reasons."""
    pr = _make_pr()
    reason, comment = mq._early_skip_reason(pr, watch_branch="main")
    assert reason is None
    assert comment is None


def test_early_skip_reason_skips_fork_pr_with_comment():
    """Fork PRs are skipped AND receive a PR comment (not silent)."""
    pr = _make_pr(head={"repo_id": 2, "sha": "a" * 40})
    reason, comment = mq._early_skip_reason(pr, watch_branch="main")
    assert reason == "fork PR"
    assert comment is not None
    assert "fork PRs are not supported" in comment


def test_early_skip_reason_skips_closed_draft_and_opt_out():
    """Other early-skip classes produce observable reasons and no comment."""
    assert mq._early_skip_reason(_make_pr(state="closed"), watch_branch="main") == ("not open", None)
    assert mq._early_skip_reason(_make_pr(draft=True), watch_branch="main") == ("draft", None)
    assert mq._early_skip_reason(
        _make_pr(labels=[{"name": "do-not-auto-merge"}]), watch_branch="main"
    ) == ("opt-out label", None)


def test_evaluate_candidate_non_main_base_skips_without_comment(capsys, monkeypatch):
    """End-to-end: _evaluate_candidate skips a non-main base PR, emits a workflow
    notice, and does NOT post a PR comment."""
    pr = _make_pr(number=42, base={"ref": "feature/stacked", "repo_id": 1})
    monkeypatch.setattr(mq, "get_pull", lambda _n: pr)

    comments_posted = []
    monkeypatch.setattr(mq, "post_comment", lambda n, b, *, dry_run: comments_posted.append((n, b, dry_run)))

    decision, ctx = mq._evaluate_candidate(
        {"number": 42},
        main_sha="m" * 40,
        main_status={"state": "success", "statuses": []},
        required_contexts=[],
        required_approvals=2,
        dry_run=False,
    )

    assert decision is None
    assert ctx["pr_number"] == 42
    assert comments_posted == []
    captured = capsys.readouterr()
    assert "base is not `main`" in captured.out
    assert "skipping" in captured.out


def test_evaluate_candidate_main_base_proceeds_to_merge_bar(monkeypatch):
    """A main-targeted PR is not early-skipped; it reaches the merge-readiness
    evaluation (which, with no statuses/approvals in this minimal mock, waits)."""
    pr = _make_pr(number=99, base={"ref": "main", "repo_id": 1})
    monkeypatch.setattr(mq, "get_pull", lambda _n: pr)
    monkeypatch.setattr(mq, "get_pull_commits", lambda _n: [{"sha": "m" * 40}])
    monkeypatch.setattr(mq, "get_combined_status", lambda _sha: {"state": "success", "statuses": []})
    monkeypatch.setattr(mq, "get_pull_reviews", lambda _n: [])

    decision, ctx = mq._evaluate_candidate(
        {"number": 99},
        main_sha="m" * 40,
        main_status={"state": "success", "statuses": []},
        required_contexts=[],
        required_approvals=2,
        dry_run=False,
    )

    assert decision is not None
    assert ctx["pr_number"] == 99
    assert decision.ready is False


# =============================================================================
# Runtime-bump exemption (RFC internal#131 PR-A; spec 9c2e9c88)
# =============================================================================
# These tests pin the is_runtime_bump_exempt() predicate. Each guard and
# condition has a positive case (the rule fires) and a negative case (the
# rule does not fire because the input is missing or wrong). A happy-path
# test pins the all-conditions-pass → exempt outcome. dup-close has its
# own test for the "newer wins" semantics.
# =============================================================================


def _bump_pr(
    *,
    author: str = "bump-bot",
    head_ref: str = "runtime-bump/claude-code/v1.2.3",
    labels: list[dict] | None = None,
) -> dict:
    """Build a minimal PR dict shaped like a bump-bot runtime-bump PR."""
    return {
        "number": 1234,
        "state": "open",
        "user": {"login": author},
        "head": {"ref": head_ref, "sha": "a" * 40},
        "base": {"ref": "main"},
        "labels": labels or [],
    }


def _runtime_version_patch(
    added: str = "claude-code@v1.2.3",
    removed: str = "claude-code@v1.2.2",
) -> str:
    """Build a minimal unified-diff patch string for .runtime-version."""
    return (
        f"--- a/.runtime-version\n"
        f"+++ b/.runtime-version\n"
        f"@@ -1,1 +1,1 @@\n"
        f"-{removed}\n"
        f"+{added}\n"
    )


def _runtime_version_file(
    *,
    added: str = "claude-code@v1.2.3",
    removed: str = "claude-code@v1.2.2",
) -> dict:
    """Build a single-file .runtime-version diff entry."""
    return {
        "filename": ".runtime-version",
        "status": "modified",
        "additions": 1,
        "deletions": 1,
        "changes": 2,
        "patch": _runtime_version_patch(added=added, removed=removed),
    }


def _set_allowlist(monkeypatch, *runtimes: str) -> None:
    """Set RUNTIME_BUMP_EXEMPT_TEMPLATES for the test."""
    monkeypatch.setattr(mq, "RUNTIME_BUMP_EXEMPT_TEMPLATES", set(runtimes))


# ---- GUARD 1: author must be bump-bot ----

def test_runtime_bump_exempt_rejects_non_bump_bot_author():
    pr = _bump_pr(author="alice")
    exempt, reason = mq.is_runtime_bump_exempt(
        pr=pr,
        pr_files=[_runtime_version_file()],
        required_contexts=["runtime-provision-smoke"],
        latest_runtime_v_tag="v1.2.3",
        rc_active=False,
    )
    assert exempt is False
    assert "author=" in reason
    assert "bump-bot" in reason


def test_runtime_bump_exempt_rejects_missing_user_field():
    pr = _bump_pr()
    pr.pop("user", None)
    exempt, reason = mq.is_runtime_bump_exempt(
        pr=pr,
        pr_files=[_runtime_version_file()],
        required_contexts=["runtime-provision-smoke"],
        latest_runtime_v_tag="v1.2.3",
        rc_active=False,
    )
    assert exempt is False
    assert "author=" in reason


# ---- GUARD 2: diff must be exactly .runtime-version with 1 added + 1 removed ----

def test_runtime_bump_exempt_rejects_no_runtime_version_file():
    pr = _bump_pr()
    pr_files = [{"filename": "README.md", "patch": "--- a/README\n+++ b/README\n"}]
    exempt, reason = mq.is_runtime_bump_exempt(
        pr=pr,
        pr_files=pr_files,
        required_contexts=["runtime-provision-smoke"],
        latest_runtime_v_tag="v1.2.3",
        rc_active=False,
    )
    assert exempt is False
    assert ".runtime-version" in reason
    assert "does not touch" in reason


def test_runtime_bump_exempt_rejects_multi_file_diff():
    pr = _bump_pr()
    pr_files = [
        _runtime_version_file(),
        {"filename": "go.mod", "patch": "--- a/go.mod\n+++ b/go.mod\n"},
    ]
    exempt, reason = mq.is_runtime_bump_exempt(
        pr=pr,
        pr_files=pr_files,
        required_contexts=["runtime-provision-smoke"],
        latest_runtime_v_tag="v1.2.3",
        rc_active=False,
    )
    assert exempt is False
    assert "1 other file" in reason


def test_runtime_bump_exempt_rejects_multi_line_runtime_version_diff():
    pr = _bump_pr()
    # Patch with 2 added lines (one of which is the version, one is blank).
    pr_files = [{
        "filename": ".runtime-version",
        "patch": (
            "--- a/.runtime-version\n"
            "+++ b/.runtime-version\n"
            "@@ -1,2 +1,2 @@\n"
            "-claude-code@v1.2.2\n"
            "-# old comment\n"
            "+claude-code@v1.2.3\n"
            "+# new comment\n"
        ),
    }]
    exempt, reason = mq.is_runtime_bump_exempt(
        pr=pr,
        pr_files=pr_files,
        required_contexts=["runtime-provision-smoke"],
        latest_runtime_v_tag="v1.2.3",
        rc_active=False,
    )
    assert exempt is False
    assert "2 added" in reason or "added" in reason


def test_runtime_bump_exempt_rejects_empty_patch_text():
    pr = _bump_pr()
    pr_files = [{"filename": ".runtime-version", "patch": ""}]
    exempt, reason = mq.is_runtime_bump_exempt(
        pr=pr,
        pr_files=pr_files,
        required_contexts=["runtime-provision-smoke"],
        latest_runtime_v_tag="v1.2.3",
        rc_active=False,
    )
    assert exempt is False
    assert "patch" in reason


# ---- GUARD 3: no active release-candidate ----

def test_runtime_bump_exempt_rejects_active_release_candidate():
    pr = _bump_pr()
    exempt, reason = mq.is_runtime_bump_exempt(
        pr=pr,
        pr_files=[_runtime_version_file()],
        required_contexts=["runtime-provision-smoke"],
        latest_runtime_v_tag="v1.2.3",
        rc_active=True,
    )
    assert exempt is False
    assert "release-candidate" in reason or "rc" in reason.lower()


# ---- CONDITION 1: ==SSOT ----

def test_runtime_bump_exempt_rejects_unverifiable_latest_tag():
    pr = _bump_pr()
    exempt, reason = mq.is_runtime_bump_exempt(
        pr=pr,
        pr_files=[_runtime_version_file()],
        required_contexts=["runtime-provision-smoke"],
        latest_runtime_v_tag=None,
        rc_active=False,
    )
    assert exempt is False
    assert "==SSOT" in reason or "unverifiable" in reason


def test_runtime_bump_exempt_rejects_ssot_mismatch():
    pr = _bump_pr()
    exempt, reason = mq.is_runtime_bump_exempt(
        pr=pr,
        pr_files=[_runtime_version_file(added="claude-code@v1.2.3", removed="claude-code@v1.2.2")],
        required_contexts=["runtime-provision-smoke"],
        latest_runtime_v_tag="v1.2.4",  # bump value != latest tag
        rc_active=False,
    )
    assert exempt is False
    assert "==SSOT" in reason
    # ==SSOT compares the VERSION part of the .runtime-version line
    # to the latest tag (not the full `<runtime>@<version>` string).
    assert "v1.2.3" in reason
    assert "v1.2.4" in reason


# ---- CONDITION 2a: GATE-ADEQUACY (CI side) ----

def test_runtime_bump_exempt_rejects_no_runtime_smoke_context(monkeypatch):
    # The smoke check fires AFTER the allowlist check in my implementation,
    # so to land on the smoke check we need the runtime on the allowlist.
    _set_allowlist(monkeypatch, "claude-code")
    pr = _bump_pr()
    exempt, reason = mq.is_runtime_bump_exempt(
        pr=pr,
        pr_files=[_runtime_version_file()],
        required_contexts=["CI / all-required (pull_request)"],  # no runtime smoke
        latest_runtime_v_tag="v1.2.3",
        rc_active=False,
    )
    assert exempt is False
    assert "runtime-provision" in reason or "smoke" in reason


def test_runtime_bump_exempt_rejects_empty_required_contexts():
    pr = _bump_pr()
    exempt, reason = mq.is_runtime_bump_exempt(
        pr=pr,
        pr_files=[_runtime_version_file()],
        required_contexts=[],
        latest_runtime_v_tag="v1.2.3",
        rc_active=False,
    )
    assert exempt is False
    assert "empty" in reason


def test_runtime_bump_exempt_accepts_case_insensitive_smoke_substring(monkeypatch):
    """The smoke-context substring match is case-INsensitive: a required
    context named `CI / RUNTIME-PROVISION-SMOKE (pull_request)` (uppercase
    "RUNTIME-PROVISION-SMOKE") should still satisfy the gate when
    RUNTIME_PROVISION_SMOKE_CONTEXTS contains the lowercase form. The
    runtime must also be on the allowlist for the exemption to actually
    fire — this test sets the allowlist so the smoke check is the one
    under test."""
    _set_allowlist(monkeypatch, "claude-code")
    pr = _bump_pr()
    exempt, reason = mq.is_runtime_bump_exempt(
        pr=pr,
        pr_files=[_runtime_version_file(added="claude-code@v0.5.1", removed="claude-code@v0.5.0")],
        required_contexts=["CI / RUNTIME-PROVISION-SMOKE (pull_request)"],
        latest_runtime_v_tag="v0.5.1",
        rc_active=False,
    )
    # All guards + conditions satisfied: exempt=True. The smoke match
    # being case-insensitive is what lets the uppercase required context
    # satisfy the lowercase substring list.
    assert exempt is True, f"expected exempt (smoke match should be case-insensitive), got: {reason}"
    assert "v0.5.0" in reason
    assert "v0.5.1" in reason


# ---- CONDITION 2b: GATE-ADEQUACY (template allowlist) ----

def test_runtime_bump_exempt_rejects_runtime_not_on_allowlist(monkeypatch):
    _set_allowlist(monkeypatch, "hermes")  # claude-code not on allowlist
    pr = _bump_pr()
    exempt, reason = mq.is_runtime_bump_exempt(
        pr=pr,
        pr_files=[_runtime_version_file()],
        required_contexts=["runtime-provision-smoke"],
        latest_runtime_v_tag="v1.2.3",
        rc_active=False,
    )
    assert exempt is False
    assert "claude-code" in reason
    assert "allowlist" in reason


def test_runtime_bump_exempt_rejects_unparseable_runtime_name():
    """If the .runtime-version value is not in '<runtime>@<version>'
    format, the function cannot determine the runtime name → fail-closed."""
    pr = _bump_pr()
    exempt, reason = mq.is_runtime_bump_exempt(
        pr=pr,
        pr_files=[_runtime_version_file(added="v1.2.3", removed="v1.2.2")],
        required_contexts=["runtime-provision-smoke"],
        latest_runtime_v_tag="v1.2.3",
        rc_active=False,
    )
    assert exempt is False
    assert "format" in reason or "<runtime>@<version>" in reason


# ---- CONDITION 3: EXCLUDE-BREAKING ----

def test_runtime_bump_exempt_rejects_semver_major(monkeypatch):
    _set_allowlist(monkeypatch, "claude-code")
    pr = _bump_pr()
    exempt, reason = mq.is_runtime_bump_exempt(
        pr=pr,
        pr_files=[_runtime_version_file(added="claude-code@v2.0.0", removed="claude-code@v1.9.9")],
        required_contexts=["runtime-provision-smoke"],
        latest_runtime_v_tag="v2.0.0",
        rc_active=False,
    )
    assert exempt is False
    assert "MAJOR" in reason


def test_runtime_bump_exempt_rejects_breaking_label(monkeypatch):
    _set_allowlist(monkeypatch, "claude-code")
    # Use a 0.x version so the semver-MAJOR check doesn't fire first —
    # we want to specifically test the breaking-label rejection.
    pr = _bump_pr(labels=[{"name": "breaking"}])
    exempt, reason = mq.is_runtime_bump_exempt(
        pr=pr,
        pr_files=[_runtime_version_file(
            added="claude-code@v0.5.2", removed="claude-code@v0.5.1"
        )],
        required_contexts=["runtime-provision-smoke"],
        latest_runtime_v_tag="v0.5.2",
        rc_active=False,
    )
    assert exempt is False
    assert "breaking" in reason.lower()


def test_runtime_bump_exempt_rejects_unparseable_semver(monkeypatch):
    _set_allowlist(monkeypatch, "claude-code")
    pr = _bump_pr()
    exempt, reason = mq.is_runtime_bump_exempt(
        pr=pr,
        pr_files=[_runtime_version_file(added="claude-code@not-a-version", removed="claude-code@v1.2.2")],
        required_contexts=["runtime-provision-smoke"],
        latest_runtime_v_tag="not-a-version",
        rc_active=False,
    )
    assert exempt is False
    assert "semver" in reason or "parse" in reason.lower()


# ---- Happy path: all guards + conditions pass → exempt ----

def test_runtime_bump_exempt_happy_path(monkeypatch):
    _set_allowlist(monkeypatch, "claude-code")
    # Use 0.x to avoid the semver-MAJOR exclusion.
    pr = _bump_pr()
    exempt, reason = mq.is_runtime_bump_exempt(
        pr=pr,
        pr_files=[_runtime_version_file(
            added="claude-code@v0.5.3", removed="claude-code@v0.5.2"
        )],
        required_contexts=["CI / all-required (pull_request)", "runtime-provision-smoke"],
        latest_runtime_v_tag="v0.5.3",
        rc_active=False,
    )
    assert exempt is True, f"expected exempt, got: {reason}"
    assert "claude-code" in reason
    assert "v0.5.2" in reason
    assert "v0.5.3" in reason


def test_runtime_bump_exempt_happy_path_with_v_prefix(monkeypatch):
    """Versions with a leading 'v' (e.g. 'v1.2.3') parse correctly."""
    _set_allowlist(monkeypatch, "claude-code")
    pr = _bump_pr()
    exempt, reason = mq.is_runtime_bump_exempt(
        pr=pr,
        pr_files=[_runtime_version_file(added="claude-code@v0.5.1", removed="claude-code@v0.5.0")],
        required_contexts=["runtime-provision-smoke"],
        latest_runtime_v_tag="v0.5.1",
        rc_active=False,
    )
    assert exempt is True, f"expected exempt, got: {reason}"


# ---- Fail-closed contracts ----

def test_runtime_bump_exempt_treats_non_dict_file_entry_as_unverifiable():
    """If the pr_files list contains a non-dict entry (e.g. None or a
    string), the structural invariant is broken → fail-closed to not-exempt."""
    pr = _bump_pr()
    exempt, reason = mq.is_runtime_bump_exempt(
        pr=pr,
        pr_files=[_runtime_version_file(), "garbage"],
        required_contexts=["runtime-provision-smoke"],
        latest_runtime_v_tag="v1.2.3",
        rc_active=False,
    )
    assert exempt is False
    assert "non-dict" in reason or "unverifiable" in reason


def test_runtime_bump_exempt_treats_empty_files_as_no_diff():
    """An empty pr_files list (e.g. when the API call failed) means we
    have no .runtime-version → fail-closed to not-exempt."""
    pr = _bump_pr()
    exempt, reason = mq.is_runtime_bump_exempt(
        pr=pr,
        pr_files=[],
        required_contexts=["runtime-provision-smoke"],
        latest_runtime_v_tag="v1.2.3",
        rc_active=False,
    )
    assert exempt is False
    assert ".runtime-version" in reason


# ---- evaluate_merge_readiness honours runtime_bump_exempt ----

def test_evaluate_merge_readiness_skips_approvals_check_when_exempt():
    """When runtime_bump_exempt=True, required_approvals is effectively 0
    → even with 0 approvers the PR is NOT blocked on the approvals check."""
    decision = mq.evaluate_merge_readiness(
        **_ready_kwargs(
            approvers=set(),  # zero approvers
            required_approvals=2,
            runtime_bump_exempt=True,
        )
    )
    # Approvals gate is bypassed; PR is mergeable (main + CI + mergeable=True).
    assert decision.ready is True
    assert decision.action == "merge"


def test_evaluate_merge_readiness_still_enforces_approvals_when_not_exempt():
    """When runtime_bump_exempt=False (default), zero approvers blocks."""
    decision = mq.evaluate_merge_readiness(
        **_ready_kwargs(
            approvers=set(),
            required_approvals=2,
            # runtime_bump_exempt omitted → defaults to False
        )
    )
    assert decision.ready is False
    assert decision.action == "wait"
    assert "insufficient genuine approvals" in decision.reason


# ---- dup-close: keep newest, close older ----

def test_dup_close_superseded_bump_prs_keeps_newest_closes_older():
    """Two PRs with the same title (bump-bot republish): keep the one with
    the latest updated_at, close the older one."""
    new_pr = {
        "number": 200,
        "title": "runtime: claude-code bump to v1.2.3",
        "user": {"login": "bump-bot"},
        "state": "open",
        "updated_at": "2026-06-14T18:00:00Z",
    }
    old_pr = {
        "number": 199,
        "title": "runtime: claude-code bump to v1.2.3",
        "user": {"login": "bump-bot"},
        "state": "open",
        "updated_at": "2026-06-14T12:00:00Z",
    }
    closed: list[int] = []
    comments: list[tuple[int, str]] = []

    def fake_close(pr_number, *, dry_run):
        closed.append(pr_number)

    def fake_comment(pr_number, body, *, dry_run):
        comments.append((pr_number, body))

    result = mq.dup_close_superseded_bump_prs(
        [new_pr, old_pr],
        close_fn=fake_close,
        comment_fn=fake_comment,
        dry_run=False,
    )
    assert result == [199]
    assert 199 in closed
    assert 200 not in closed
    # The old PR gets a comment explaining the close.
    assert any(c[0] == 199 for c in comments)
    assert any("#200" in c[1] for c in comments)


def test_dup_close_superseded_bump_prs_no_op_on_single_pr():
    """A single bump PR is not a duplicate → no close, no comment."""
    pr = {
        "number": 300,
        "title": "runtime: claude-code bump to v1.2.4",
        "user": {"login": "bump-bot"},
        "state": "open",
        "updated_at": "2026-06-14T18:00:00Z",
    }
    closed: list[int] = []
    comments: list[tuple[int, str]] = []

    def fake_close(pr_number, *, dry_run):
        closed.append(pr_number)

    def fake_comment(pr_number, body, *, dry_run):
        comments.append((pr_number, body))

    result = mq.dup_close_superseded_bump_prs(
        [pr],
        close_fn=fake_close,
        comment_fn=fake_comment,
        dry_run=False,
    )
    assert result == []
    assert closed == []
    assert comments == []


def test_dup_close_superseded_bump_prs_ignores_non_bump_bot_authors():
    """A non-bump-bot PR is not a candidate for dup-close (it may be a
    hand-edit and should not be auto-closed)."""
    bump_pr = {
        "number": 400,
        "title": "runtime: claude-code bump to v1.2.5",
        "user": {"login": "bump-bot"},
        "state": "open",
        "updated_at": "2026-06-14T18:00:00Z",
    }
    hand_pr = {
        "number": 401,
        "title": "runtime: claude-code bump to v1.2.5",
        "user": {"login": "agent-dev-a"},
        "state": "open",
        "updated_at": "2026-06-14T12:00:00Z",
    }
    closed: list[int] = []

    def fake_close(pr_number, *, dry_run):
        closed.append(pr_number)

    def fake_comment(pr_number, body, *, dry_run):
        pass

    result = mq.dup_close_superseded_bump_prs(
        [bump_pr, hand_pr],
        close_fn=fake_close,
        comment_fn=fake_comment,
        dry_run=False,
    )
    assert result == []  # the hand PR is ignored, no dup to close


# ---- Helpers: _parse_bump_pr_title ----

def test_parse_bump_pr_title_format_1_runtime_dash_version():
    """Format 1: 'runtime:<name>-v<ver>' — the dash form used by some
    bump-bot configs (no space, no 'bump to' verb)."""
    assert mq._parse_bump_pr_title("runtime:claude-code-v1.2.3") == ("claude-code", "v1.2.3")
    assert mq._parse_bump_pr_title("runtime:kimi-cli-v0.5.0") == ("kimi-cli", "v0.5.0")


def test_parse_bump_pr_title_format_2_bump_to():
    """Format 2: '<name> bump to <ver>' — the verb form."""
    assert mq._parse_bump_pr_title("claude-code bump to v1.2.3") == ("claude-code", "v1.2.3")
    # Version without leading 'v' should be normalized to leading 'v'.
    assert mq._parse_bump_pr_title("claude-code bump to 1.2.3") == ("claude-code", "v1.2.3")


def test_parse_bump_pr_title_hybrid_runtime_prefix_and_bump_to():
    """Hybrid: 'runtime: <name> bump to <ver>' — the prefix is decorative;
    the body uses the verb form. The runtime name is the segment between
    the prefix and the verb, the version is what comes after 'bump to'."""
    assert mq._parse_bump_pr_title("runtime: claude-code bump to v1.2.3") == ("claude-code", "v1.2.3")


def test_parse_bump_pr_title_normalizes_version_across_formats():
    """Same (runtime, version) from different title formats MUST produce
    the same bucket key — otherwise dedup would fragment across formats
    and the auto-merge exemption would still race stale vs fresh CI."""
    a = mq._parse_bump_pr_title("runtime:claude-code-v1.2.3")
    b = mq._parse_bump_pr_title("claude-code bump to v1.2.3")
    c = mq._parse_bump_pr_title("claude-code bump to 1.2.3")
    d = mq._parse_bump_pr_title("runtime: claude-code bump to v1.2.3")
    assert a == b == c == d, (
        f"dedup key must be stable across title formats; got: "
        f"a={a} b={b} c={c} d={d}"
    )


def test_parse_bump_pr_title_unrecognized_returns_none():
    """Titles that don't match any bump-bot format return None — the
    caller treats these as singleton buckets (no spurious dedup)."""
    assert mq._parse_bump_pr_title("fix typo in docs") is None
    assert mq._parse_bump_pr_title("") is None
    assert mq._parse_bump_pr_title("runtime:") is None  # prefix only
    assert mq._parse_bump_pr_title("runtime: claude-code") is None  # no version
    assert mq._parse_bump_pr_title("claude-code bumped v1.2.3") is None  # wrong verb
    # Non-semver version is not a bump-bot format
    assert mq._parse_bump_pr_title("claude-code bump to latest") is None


def test_dup_close_superseded_bump_prs_separate_runtime_versions():
    """Two bump-bot PRs with DIFFERENT (runtime, version) tuples are not
    dupes — they land in separate buckets and are not closed. Pins
    that the parser actually distinguishes versions."""
    pr_v123 = {
        "number": 500,
        "title": "runtime: claude-code bump to v1.2.3",
        "user": {"login": "bump-bot"},
        "state": "open",
        "updated_at": "2026-06-14T18:00:00Z",
    }
    pr_v124 = {
        "number": 501,
        "title": "runtime: claude-code bump to v1.2.4",
        "user": {"login": "bump-bot"},
        "state": "open",
        "updated_at": "2026-06-14T18:00:00Z",
    }
    closed: list[int] = []
    def fake_close(pr_number, *, dry_run): closed.append(pr_number)
    def fake_comment(pr_number, body, *, dry_run): pass
    result = mq.dup_close_superseded_bump_prs(
        [pr_v123, pr_v124],
        close_fn=fake_close,
        comment_fn=fake_comment,
        dry_run=False,
    )
    assert result == []
    assert closed == []


# ---- Helpers: _is_active_release_candidate ----

def test_is_active_release_candidate_detects_rc_label():
    prs = [
        {
            "number": 1,
            "state": "open",
            "labels": [{"name": "release-candidate"}],
        },
    ]
    assert mq._is_active_release_candidate(prs, pr_number=999) is True


def test_is_active_release_candidate_ignores_closed_prs():
    prs = [
        {
            "number": 1,
            "state": "closed",
            "labels": [{"name": "release-candidate"}],
        },
    ]
    assert mq._is_active_release_candidate(prs, pr_number=999) is False


def test_is_active_release_candidate_ignores_self():
    prs = [
        {
            "number": 999,
            "state": "open",
            "labels": [{"name": "release-candidate"}],
        },
    ]
    # The PR being evaluated is excluded from the RC check.
    assert mq._is_active_release_candidate(prs, pr_number=999) is False


def test_is_active_release_candidate_returns_false_when_no_rc_labels():
    prs = [
        {
            "number": 1,
            "state": "open",
            "labels": [{"name": "bug"}, {"name": "enhancement"}],
        },
    ]
    assert mq._is_active_release_candidate(prs, pr_number=999) is False


# --------------------------------------------------------------------------
# SSOT-as-ENFORCED: required-contexts.txt is the enforced merge gate
# (internal#3181 — close the PR#3181 force-merge-over-red regression).
# --------------------------------------------------------------------------
def _write_ctx_file(tmp_path, text):
    p = tmp_path / "required-contexts.txt"
    p.write_text(text, encoding="utf-8")
    return str(p)


def test_strip_event_removes_known_suffixes():
    assert mq._strip_event("A / B (pull_request)") == "A / B"
    assert mq._strip_event("A / B (push)") == "A / B"
    assert mq._strip_event("A / B (pull_request_target)") == "A / B"
    # No suffix → unchanged (bare file form).
    assert mq._strip_event("A / B") == "A / B"


def test_load_enforced_file_contexts_parses_and_strips(tmp_path):
    path = _write_ctx_file(
        tmp_path,
        "# header comment\n"
        "CI / all-required\n"
        "E2E API Smoke Test / E2E API Smoke Test  # inline note\n"
        "\n"
        "Secret scan / Scan diff for credential-shaped strings\n",
    )
    out = _REAL_LOAD_ENFORCED(path)
    assert out == [
        "CI / all-required",
        "E2E API Smoke Test / E2E API Smoke Test",
        "Secret scan / Scan diff for credential-shaped strings",
    ]


def test_load_enforced_file_contexts_excludes_pending_tail(tmp_path):
    # Everything at or below the first `# pending-#NNNN` marker is excluded.
    path = _write_ctx_file(
        tmp_path,
        "CI / all-required\n"
        "qa-review / approved\n"
        "# pending-#3159 (not yet enforced)\n"
        "E2E Staging SaaS (full lifecycle) / E2E Staging Concierge Creates Workspace\n"
        "E2E Staging SaaS (full lifecycle) / E2E Staging Platform Boot\n",
    )
    out = _REAL_LOAD_ENFORCED(path)
    assert out == ["CI / all-required", "qa-review / approved"]
    assert not any("E2E Staging" in c for c in out)


def test_EnforcedContextsUnavailable_inherits_from_ApiError():
    # So main()'s `except ApiError` handler catches it → rc 1 (no merge + page).
    assert issubclass(mq.EnforcedContextsUnavailable, mq.ApiError)


def test_load_enforced_file_contexts_missing_file_fails_closed(tmp_path):
    # RC 13618: a MISSING SSOT file must NOT silently disable enforcement
    # (the old fail-OPEN returned []). It now raises so the gate HOLDS.
    with pytest.raises(mq.EnforcedContextsUnavailable):
        _REAL_LOAD_ENFORCED(str(tmp_path / "nope.txt"))


def test_load_enforced_file_contexts_unreadable_file_fails_closed(tmp_path):
    # An UNREADABLE path (here: a directory → IsADirectoryError, an OSError)
    # is a read failure → fail-closed raise, not an empty allowlist.
    with pytest.raises(mq.EnforcedContextsUnavailable):
        _REAL_LOAD_ENFORCED(str(tmp_path))


def test_load_enforced_file_contexts_corrupt_nonutf8_fails_closed(tmp_path):
    # A corrupt/binary SSOT raises UnicodeDecodeError (a ValueError, NOT an
    # OSError). It must STILL fail closed CLEANLY as EnforcedContextsUnavailable
    # — not slip through as an unhandled traceback. (Audit RC 13618 follow-up.)
    p = tmp_path / "required-contexts.txt"
    p.write_bytes(b"CI / all-required\n\xff\xfe not utf-8 \x80\x81\n")
    with pytest.raises(mq.EnforcedContextsUnavailable):
        _REAL_LOAD_ENFORCED(str(p))


def test_load_enforced_file_contexts_empty_file_is_valid_empty(tmp_path):
    # DISTINCT from a read failure: a file that READS fine but is
    # legitimately empty (comments only) returns [] WITHOUT raising — a valid
    # "enforce BP + governance only" state, not the RC 13618 error.
    path = _write_ctx_file(tmp_path, "# only a comment\n\n   \n")
    assert _REAL_LOAD_ENFORCED(path) == []


def test_load_enforced_file_contexts_all_pending_is_valid_empty(tmp_path):
    # Every entry parked below the pending marker → readable, empty enforced
    # set → [] (no raise). The sequencing escape hatch must not fail closed.
    path = _write_ctx_file(
        tmp_path,
        "# pending-#3159 (not yet enforced)\n"
        "E2E Staging SaaS (full lifecycle) / E2E Staging Platform Boot\n",
    )
    assert _REAL_LOAD_ENFORCED(path) == []


def test_process_once_fails_closed_when_enforced_contexts_unreadable(monkeypatch):
    """RC 13618 integration: if the SSOT file can't be read at merge time,
    process_once must NOT merge. The loader raises EnforcedContextsUnavailable
    (before the candidate loop); process_once lets it propagate so main()
    surfaces rc 1 (no merge + operators paged) — never a silent fall-back to
    BP + governance only."""
    merged = {"called": False}
    monkeypatch.setattr(mq, "WATCH_BRANCH", "main")
    monkeypatch.setattr(mq, "get_branch_protection", lambda branch: mq.BranchProtection(
        required_contexts=["CI / all-required (pull_request)"],
        required_approvals=2,
        block_on_rejected_reviews=True,
    ))

    def boom(path):
        raise mq.EnforcedContextsUnavailable(f"{path} unreadable (simulated)")
    # Override the autouse []-stub: this test exercises a read FAILURE.
    monkeypatch.setattr(mq, "load_enforced_file_contexts", boom)
    monkeypatch.setattr(mq, "merge_pull", lambda *a, **k: merged.__setitem__("called", True))

    with pytest.raises(mq.EnforcedContextsUnavailable):
        mq.process_once(dry_run=False)
    assert merged["called"] is False


# ---------------------------------------------------------------------------
# internal#3210 HIGH: an empty PUSH_REQUIRED_CONTEXTS must NOT vacuously pass
# the main-green backstop. push_required_contexts() raises
# PushRequiredContextsUnavailable (an ApiError) so the gate HOLDS rather than
# letting required_contexts_green(main_latest, []) == (True, []) wave through
# ANY main state (including all-red).
# ---------------------------------------------------------------------------


def test_PushRequiredContextsUnavailable_inherits_from_ApiError():
    # So main()'s `except ApiError` handler catches it → rc 1 (no merge + page).
    assert issubclass(mq.PushRequiredContextsUnavailable, mq.ApiError)


@pytest.mark.parametrize("raw", ["", "   ", ",", " , , "])
def test_push_required_contexts_raises_when_parse_empty(monkeypatch, raw):
    """Empty / whitespace-only / all-comma PUSH_REQUIRED_CONTEXTS parses to []
    and must fail closed — NOT return [] (which would vacuously green main)."""
    monkeypatch.setattr(mq, "PUSH_REQUIRED_CONTEXTS_RAW", raw)
    with pytest.raises(mq.PushRequiredContextsUnavailable):
        mq.push_required_contexts()


def test_push_required_contexts_returns_configured_set():
    """Regression guard: a normal configured value still parses to the set
    (the fail-closed raise must not break the happy path)."""
    # The module default is non-empty; assert it parses to a non-empty list.
    assert mq.push_required_contexts() == ["CI / all-required (push)"]


def test_process_once_holds_tick_when_push_required_contexts_empty(monkeypatch):
    """internal#3210 HIGH integration: with PUSH_REQUIRED_CONTEXTS empty, the
    main-green check in process_once would VACUOUSLY pass (the backstop that
    pauses the queue on a red main disappears). push_required_contexts() raises
    PushRequiredContextsUnavailable BEFORE the candidate loop; process_once lets
    it propagate so main() surfaces rc 1 — and crucially NO PR is merged.

    Main is wired ALL-RED here: the bug let the queue keep merging onto a red
    main; the fix must HOLD instead."""
    merged = {"called": False}
    monkeypatch.setattr(mq, "WATCH_BRANCH", "main")
    monkeypatch.setattr(mq, "PUSH_REQUIRED_CONTEXTS_RAW", "")  # parses to []
    monkeypatch.setattr(mq, "get_branch_protection", lambda branch: mq.BranchProtection(
        required_contexts=["CI / all-required (pull_request)"],
        required_approvals=2,
        block_on_rejected_reviews=True,
    ))
    main_sha = "b" * 40
    monkeypatch.setattr(mq, "get_branch_head", lambda branch: main_sha)
    # Main is RED — the exact state the old vacuous pass would have ignored.
    monkeypatch.setattr(mq, "get_combined_status", lambda sha: {
        "state": "failure",
        "statuses": [{"context": "CI / all-required (push)", "status": "failure"}],
    })
    monkeypatch.setattr(mq, "merge_pull", lambda *a, **k: merged.__setitem__("called", True))

    with pytest.raises(mq.PushRequiredContextsUnavailable):
        mq.process_once(dry_run=False)
    assert merged["called"] is False


def test_enforced_file_contexts_green_event_insensitive_match():
    latest = mq.latest_statuses_by_context([
        {"context": "E2E Staging SaaS (full lifecycle) / X (pull_request)", "status": "success"},
    ])
    ok, bad = mq.enforced_file_contexts_green(
        latest, ["E2E Staging SaaS (full lifecycle) / X"]
    )
    assert ok is True
    assert bad == []


def test_enforced_file_contexts_green_flags_red_and_missing():
    latest = mq.latest_statuses_by_context([
        {"context": "Foo / Bar (pull_request)", "status": "failure"},
    ])
    ok, bad = mq.enforced_file_contexts_green(
        latest, ["Foo / Bar", "Absent / Job"]
    )
    assert ok is False
    assert bad == ["Foo / Bar=failure", "Absent / Job=missing"]


def test_enforced_file_red_blocks_merge_not_forced_over():
    """The PR#3181 regression: a context in required-contexts.txt but NOT in
    BP was force-merged over while red. With enforcement it must `wait`."""
    pr_status = {
        "state": "failure",
        "statuses": [
            {"context": "CI / all-required (pull_request)", "status": "success"},
            {"context": "CI / Platform (Go) (pull_request)", "status": "success"},
            {"context": "qa-review / approved (pull_request_target)", "status": "success"},
            {"context": "security-review / approved (pull_request_target)", "status": "success"},
            {"context": "sop-checklist / all-items-acked (pull_request_target)", "status": "success"},
            # In required-contexts.txt, NOT in BP, RED — the #3181 case.
            {
                "context": "E2E Staging SaaS (full lifecycle) / E2E Staging Concierge Creates Workspace (pull_request)",
                "status": "failure",
            },
        ],
    }
    decision = mq.evaluate_merge_readiness(
        **_ready_kwargs(pr_status=pr_status),
        enforced_file_contexts=[
            "E2E Staging SaaS (full lifecycle) / E2E Staging Concierge Creates Workspace"
        ],
    )
    assert decision.ready is False
    assert decision.action == "wait"
    assert decision.force is False
    assert "enforced required-contexts.txt" in decision.reason


def test_enforced_file_green_allows_merge():
    pr_status = {
        "state": "success",
        "statuses": [
            {"context": "CI / all-required (pull_request)", "status": "success"},
            {"context": "CI / Platform (Go) (pull_request)", "status": "success"},
            {"context": "qa-review / approved (pull_request_target)", "status": "success"},
            {"context": "security-review / approved (pull_request_target)", "status": "success"},
            {"context": "sop-checklist / all-items-acked (pull_request_target)", "status": "success"},
            {
                "context": "E2E Staging SaaS (full lifecycle) / E2E Staging Concierge Creates Workspace (pull_request)",
                "status": "success",
            },
        ],
    }
    decision = mq.evaluate_merge_readiness(
        **_ready_kwargs(pr_status=pr_status),
        enforced_file_contexts=[
            "E2E Staging SaaS (full lifecycle) / E2E Staging Concierge Creates Workspace"
        ],
    )
    assert decision.ready is True
    assert decision.action == "merge"


def test_parked_pending_context_does_not_block(tmp_path):
    """A red context PARKED below the pending marker is NOT loaded as
    enforced, so it does not block — the #3159 sequencing escape hatch."""
    path = _write_ctx_file(
        tmp_path,
        "CI / all-required\n"
        "# pending-#3159 (not yet enforced)\n"
        "E2E Staging SaaS (full lifecycle) / E2E Staging Platform Boot\n",
    )
    enforced = _REAL_LOAD_ENFORCED(path)
    pr_status = {
        "state": "failure",
        "statuses": [
            {"context": "CI / all-required (pull_request)", "status": "success"},
            {"context": "CI / Platform (Go) (pull_request)", "status": "success"},
            {"context": "qa-review / approved (pull_request_target)", "status": "success"},
            {"context": "security-review / approved (pull_request_target)", "status": "success"},
            {"context": "sop-checklist / all-items-acked (pull_request_target)", "status": "success"},
            # Parked context is RED but must NOT block (it is below the marker).
            {
                "context": "E2E Staging SaaS (full lifecycle) / E2E Staging Platform Boot (pull_request)",
                "status": "failure",
            },
        ],
    }
    decision = mq.evaluate_merge_readiness(
        **_ready_kwargs(pr_status=pr_status),
        enforced_file_contexts=enforced,
    )
    # Only "CI / all-required" is enforced from the file; it is green, and the
    # parked red is treated as a non-required advisory red (force bypass).
    assert decision.ready is True
    assert decision.action == "merge"
    assert decision.force is True  # advisory red present → force bypass


# ==========================================================================
# internal#3210 (final tail) — LIVE pre-merge re-check vs snapshot staleness.
#
# get_combined_status prefers the conductor snapshot while it is within its
# freshness window. A required/enforced/critical context can flip to RED
# *inside* that window, AFTER the snapshot was captured but before the queue
# acts, and still read GREEN from the snapshot — so the merge DECISION sees
# green. process_once only re-checks that MAIN has not moved before the merge
# POST; it did NOT re-verify the candidate head's own statuses live. These
# tests cover the fix: an extra LIVE (snapshot-bypassed) status read of the
# single candidate about to merge, re-running the same status gates, that
# SKIPS the PR on any regression.
# ==========================================================================
def _live_recheck_process_once_monkeypatch(
    monkeypatch, *, live_pr_statuses, calls, decision_pr_statuses=None
):
    """Wire process_once fully-ready EXCEPT the candidate head's LIVE statuses
    are configurable independently of the DECISION-pass statuses.

    The decision pass (get_combined_status with prefer_live=False — the
    snapshot/scan read) sees `decision_pr_statuses` (a fully-GREEN default), so
    the decision is `merge`. The FINAL live pre-merge re-check (prefer_live=
    True) returns `live_pr_statuses` — set it RED to model a within-window flip.

    Records every merge attempt in calls["merge_attempts"] and every live
    re-fetch in calls["live_refetch_shas"] (spy)."""
    monkeypatch.setattr(mq, "OWNER", "molecule-ai")
    monkeypatch.setattr(mq, "NAME", "molecule-core")
    monkeypatch.setattr(mq, "WATCH_BRANCH", "main")
    monkeypatch.setattr(mq, "QUEUE_LABEL", "merge-queue")
    monkeypatch.setattr(mq, "HOLD_LABEL", "merge-queue-hold")
    monkeypatch.setattr(mq, "AUTO_DISCOVER", True)
    monkeypatch.setattr(mq, "OPT_OUT_LABELS", {"merge-queue-hold", "do-not-auto-merge", "wip"})
    monkeypatch.setattr(mq, "REVIEWER_SET", REVIEWERS)
    monkeypatch.setattr(mq, "get_branch_protection", lambda branch: mq.BranchProtection(
        required_contexts=["CI / all-required (pull_request)"],
        required_approvals=2,
        block_on_rejected_reviews=True,
    ))
    main_sha = "b" * 40
    head_sha = "a" * 40
    monkeypatch.setattr(mq, "get_branch_head", lambda branch: main_sha)

    green_pr_statuses = decision_pr_statuses if decision_pr_statuses is not None else [
        {"context": "CI / all-required (pull_request)", "status": "success"},
        {"context": "CI / Platform (Go) (pull_request)", "status": "success"},
        {"context": "qa-review / approved (pull_request_target)", "status": "success"},
        {"context": "security-review / approved (pull_request_target)", "status": "success"},
        {"context": "sop-checklist / all-items-acked (pull_request_target)", "status": "success"},
    ]

    def fake_combined(sha, *, prefer_live=False):
        if sha == main_sha:
            return {"state": "success", "statuses": [
                {"context": "CI / all-required (push)", "status": "success"}]}
        if prefer_live:
            # The FINAL pre-merge re-check — return the configurable LIVE set.
            calls["live_refetch_shas"].append(sha)
            return {"state": "unknown", "statuses": list(live_pr_statuses)}
        # The DECISION pass (snapshot/scan) — fully green by default.
        return {"state": "success", "statuses": list(green_pr_statuses)}
    monkeypatch.setattr(mq, "get_combined_status", fake_combined)

    monkeypatch.setattr(mq, "list_candidate_issues", lambda *, auto_discover: [
        {"number": 333, "pull_request": {}, "labels": [{"name": "merge-queue"}],
         "created_at": "2026-06-01T00:00:00Z"},
    ])
    monkeypatch.setattr(mq, "get_pull", lambda n: {
        "state": "open", "number": n, "mergeable": True,
        "base": {"ref": "main", "repo_id": 1},
        "head": {"sha": head_sha, "repo_id": 1},
        "labels": [{"name": "merge-queue"}],
    })
    monkeypatch.setattr(mq, "get_pull_commits", lambda n: [{"sha": main_sha}, {"sha": head_sha}])
    monkeypatch.setattr(mq, "get_pull_reviews", lambda n: [
        {"state": "APPROVED", "user": {"login": "agent-researcher"},
         "official": True, "stale": False, "dismissed": False, "commit_id": head_sha},
        {"state": "APPROVED", "user": {"login": "agent-reviewer-cr2"},
         "official": True, "stale": False, "dismissed": False, "commit_id": head_sha},
    ])

    def fake_merge(pr_number, *, dry_run, force=False):
        calls["merge_attempts"] += 1
    monkeypatch.setattr(mq, "merge_pull", fake_merge)
    monkeypatch.setattr(mq, "add_label_by_name", lambda *a, **k: None)
    monkeypatch.setattr(mq, "update_pull", lambda *a, **k: calls.__setitem__("updated", True))
    monkeypatch.setattr(mq, "post_comment", lambda *a, **k: None)
    return head_sha


def test_process_once_live_recheck_red_required_aborts_merge(monkeypatch, capsys):
    """(a) GREEN in the snapshot/decision pass but RED on the LIVE pre-merge
    re-fetch → the PR is NOT merged. A required context that flipped to red
    within the snapshot's freshness window (after capture) must abort the
    merge for this candidate (treat as skip), closing the within-window
    staleness fail-open."""
    calls = {"merge_attempts": 0, "live_refetch_shas": [], "updated": False}
    head_sha = _live_recheck_process_once_monkeypatch(
        monkeypatch,
        live_pr_statuses=[
            # A REQUIRED context now RED on the live re-fetch.
            {"context": "CI / all-required (pull_request)", "status": "failure"},
            {"context": "CI / Platform (Go) (pull_request)", "status": "success"},
            {"context": "qa-review / approved (pull_request_target)", "status": "success"},
            {"context": "security-review / approved (pull_request_target)", "status": "success"},
            {"context": "sop-checklist / all-items-acked (pull_request_target)", "status": "success"},
        ],
        calls=calls,
    )

    rc = mq.process_once(dry_run=False)

    assert rc == 0
    # The fail-open this closes: a snapshot-green-but-live-red head must NOT merge.
    assert calls["merge_attempts"] == 0
    # The live re-check must have been invoked for the candidate head (spy).
    assert calls["live_refetch_shas"] == [head_sha]
    out = capsys.readouterr().out
    assert "PR #333 SKIPPED: live pre-merge re-check" in out
    assert "CI / all-required" in out


def test_process_once_live_recheck_red_critical_aborts_merge(monkeypatch, capsys):
    """(a, variant) A CRITICAL context (CI / Platform (Go)) flipping red on the
    live re-fetch also aborts — the critical guard is re-run live and force
    cannot bypass it."""
    calls = {"merge_attempts": 0, "live_refetch_shas": [], "updated": False}
    head_sha = _live_recheck_process_once_monkeypatch(
        monkeypatch,
        live_pr_statuses=[
            {"context": "CI / all-required (pull_request)", "status": "success"},
            # CRITICAL context now RED on the live re-fetch.
            {"context": "CI / Platform (Go) (pull_request)", "status": "failure"},
            {"context": "qa-review / approved (pull_request_target)", "status": "success"},
            {"context": "security-review / approved (pull_request_target)", "status": "success"},
            {"context": "sop-checklist / all-items-acked (pull_request_target)", "status": "success"},
        ],
        calls=calls,
    )

    rc = mq.process_once(dry_run=False)

    assert rc == 0
    assert calls["merge_attempts"] == 0
    assert calls["live_refetch_shas"] == [head_sha]
    out = capsys.readouterr().out
    assert "PR #333 SKIPPED: live pre-merge re-check" in out
    assert "CI / Platform (Go)" in out


def test_process_once_live_recheck_green_merges_normally(monkeypatch, capsys):
    """(b) GREEN in the snapshot AND GREEN on the live pre-merge re-fetch →
    merges normally. Regression guard: the live re-check must not block a
    genuinely-ready PR, and it IS invoked before the merge (spy)."""
    calls = {"merge_attempts": 0, "live_refetch_shas": [], "updated": False}
    head_sha = _live_recheck_process_once_monkeypatch(
        monkeypatch,
        live_pr_statuses=[
            {"context": "CI / all-required (pull_request)", "status": "success"},
            {"context": "CI / Platform (Go) (pull_request)", "status": "success"},
            {"context": "qa-review / approved (pull_request_target)", "status": "success"},
            {"context": "security-review / approved (pull_request_target)", "status": "success"},
            {"context": "sop-checklist / all-items-acked (pull_request_target)", "status": "success"},
        ],
        calls=calls,
    )

    rc = mq.process_once(dry_run=False)

    assert rc == 0
    # Still green live → merges.
    assert calls["merge_attempts"] == 1
    # (c) The live re-fetch IS invoked before the merge.
    assert calls["live_refetch_shas"] == [head_sha]
    out = capsys.readouterr().out
    assert "SKIPPED: live pre-merge re-check" not in out


def test_process_once_live_recheck_red_enforced_file_context_aborts_merge(monkeypatch, capsys):
    """(a, enforced-SSOT variant) An ENFORCED `.gitea/required-contexts.txt`
    entry green-in-snapshot but RED on the live re-fetch aborts the merge — the
    enforced-file gate is re-run live too (only when the enforced set is
    non-empty, matching evaluate_merge_readiness step 4b)."""
    calls = {"merge_attempts": 0, "live_refetch_shas": [], "updated": False}
    enforced_green = {
        "context": "E2E Staging SaaS (full lifecycle) / E2E Staging Concierge "
                   "Creates Workspace (pull_request)",
        "status": "success",
    }
    head_sha = _live_recheck_process_once_monkeypatch(
        monkeypatch,
        # DECISION pass: the enforced context is GREEN, so the decision reaches
        # `merge` (this models the snapshot the decision trusted).
        decision_pr_statuses=[
            {"context": "CI / all-required (pull_request)", "status": "success"},
            {"context": "CI / Platform (Go) (pull_request)", "status": "success"},
            {"context": "qa-review / approved (pull_request_target)", "status": "success"},
            {"context": "security-review / approved (pull_request_target)", "status": "success"},
            {"context": "sop-checklist / all-items-acked (pull_request_target)", "status": "success"},
            enforced_green,
        ],
        # LIVE re-fetch: the SAME enforced context has flipped RED.
        live_pr_statuses=[
            {"context": "CI / all-required (pull_request)", "status": "success"},
            {"context": "CI / Platform (Go) (pull_request)", "status": "success"},
            {"context": "qa-review / approved (pull_request_target)", "status": "success"},
            {"context": "security-review / approved (pull_request_target)", "status": "success"},
            {"context": "sop-checklist / all-items-acked (pull_request_target)", "status": "success"},
            {"context": "E2E Staging SaaS (full lifecycle) / E2E Staging Concierge "
                        "Creates Workspace (pull_request)", "status": "failure"},
        ],
        calls=calls,
    )
    # The SAME enforced set the decision used must be re-checked live. Override
    # the autouse [] stub for THIS test so process_once sees a non-empty
    # enforced set (event-stripped form, as the loader produces).
    monkeypatch.setattr(
        mq, "load_enforced_file_contexts",
        lambda path: [
            "E2E Staging SaaS (full lifecycle) / E2E Staging Concierge Creates Workspace"
        ],
    )

    rc = mq.process_once(dry_run=False)

    assert rc == 0
    assert calls["merge_attempts"] == 0
    assert calls["live_refetch_shas"] == [head_sha]
    out = capsys.readouterr().out
    assert "PR #333 SKIPPED: live pre-merge re-check" in out
    assert "E2E Staging Concierge Creates Workspace" in out


def test_get_combined_status_prefer_live_bypasses_snapshot(monkeypatch):
    """The live re-fetch must GENUINELY bypass the snapshot. With a fresh
    snapshot present for the SHA, get_combined_status(sha) returns the snapshot,
    but get_combined_status(sha, prefer_live=True) hits the live API instead."""
    import json as _json
    import os as _os
    import tempfile as _tempfile

    head_sha = "c" * 40
    snapshot = _make_snapshot([
        {"number": 90, "title": "PR 90", "head_sha": head_sha, "labels": [],
         "combined_state": "success",
         "statuses": [{"context": "CI / all-required (pull_request)", "status": "success"}]},
    ])
    with _tempfile.NamedTemporaryFile(mode="w", suffix=".json", delete=False) as f:
        _json.dump(snapshot, f)
        path = f.name
    try:
        monkeypatch.setenv("CONDUCTOR_SNAPSHOT_FILE", path)
        monkeypatch.setattr(mq, "OWNER", "o")
        monkeypatch.setattr(mq, "NAME", "r")

        live_calls = {"n": 0}

        def fake_api(method, path, **kw):
            if path.endswith("/status"):
                live_calls["n"] += 1
                # LIVE state differs from the snapshot (now failure).
                return 200, {"state": "failure", "statuses": [
                    {"context": "CI / all-required (pull_request)", "status": "failure"}]}
            if path.endswith("/statuses"):
                return 200, []
            raise mq.ApiError("unexpected")
        monkeypatch.setattr(mq, "api", fake_api)

        # Default (snapshot) read: returns the snapshot's GREEN, no live call.
        snap = mq.get_combined_status(head_sha)
        assert snap["state"] == "success"
        assert live_calls["n"] == 0

        # prefer_live: bypasses the snapshot, hits the live API → FAILURE.
        live = mq.get_combined_status(head_sha, prefer_live=True)
        assert live["state"] == "failure"
        assert live_calls["n"] == 1
    finally:
        _os.unlink(path)


def test_live_premerge_status_regressions_helper(monkeypatch):
    """Unit: live_premerge_status_regressions returns [] when live is green and
    a non-empty reason list when a required/critical/enforced context is red,
    and it always reads with prefer_live=True (snapshot bypassed)."""
    seen = {"prefer_live": None}

    def fake_combined(sha, *, prefer_live=False):
        seen["prefer_live"] = prefer_live
        return {"state": "success", "statuses": [
            {"context": "CI / all-required (pull_request)", "status": "success"},
            {"context": "CI / Platform (Go) (pull_request)", "status": "success"},
        ]}
    monkeypatch.setattr(mq, "get_combined_status", fake_combined)

    regressions = mq.live_premerge_status_regressions(
        "a" * 40,
        required_contexts=["CI / all-required (pull_request)"],
        enforced_file_contexts=[],
    )
    assert regressions == []
    assert seen["prefer_live"] is True  # snapshot genuinely bypassed

    def fake_combined_red(sha, *, prefer_live=False):
        return {"state": "failure", "statuses": [
            {"context": "CI / all-required (pull_request)", "status": "failure"},
            {"context": "CI / Platform (Go) (pull_request)", "status": "success"},
        ]}
    monkeypatch.setattr(mq, "get_combined_status", fake_combined_red)

    regressions = mq.live_premerge_status_regressions(
        "a" * 40,
        required_contexts=["CI / all-required (pull_request)"],
        enforced_file_contexts=[],
    )
    assert regressions  # non-empty → abort
    assert any("CI / all-required" in r for r in regressions)
