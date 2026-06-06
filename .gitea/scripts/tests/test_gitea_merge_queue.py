import importlib.util
import sys
from pathlib import Path

SCRIPT = Path(__file__).resolve().parents[1] / "gitea-merge-queue.py"
spec = importlib.util.spec_from_file_location("gitea_merge_queue", SCRIPT)
mq = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = mq
spec.loader.exec_module(mq)


def test_latest_statuses_dedupes_by_context_newest_first():
    statuses = [
        {"context": "CI / all-required (pull_request)", "status": "failure"},
        {"context": "sop-checklist / all-items-acked (pull_request)", "state": "success"},
        {"context": "CI / all-required (pull_request)", "status": "success"},
    ]

    latest = mq.latest_statuses_by_context(statuses)

    assert latest["CI / all-required (pull_request)"]["status"] == "failure"
    assert latest["sop-checklist / all-items-acked (pull_request)"]["state"] == "success"


def test_required_contexts_green_rejects_missing_and_pending():
    latest = mq.latest_statuses_by_context([
        {"context": "CI / all-required (pull_request)", "status": "success"},
        {"context": "sop-checklist / all-items-acked (pull_request)", "status": "pending"},
    ])

    ok, missing_or_bad = mq.required_contexts_green(
        latest,
        [
            "CI / all-required (pull_request)",
            "sop-checklist / all-items-acked (pull_request)",
            "qa-review / approved (pull_request)",
        ],
    )

    assert ok is False
    assert missing_or_bad == [
        "sop-checklist / all-items-acked (pull_request)=pending",
        "qa-review / approved (pull_request)=missing",
    ]


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
    """Default kwargs for a fully-ready merge; override per test."""
    base = dict(
        main_status={
            "state": "success",
            "statuses": [{"context": "CI / all-required (push)", "status": "success"}],
        },
        pr_status={
            "state": "success",
            "statuses": [{"context": "CI / all-required (pull_request)", "status": "success"}],
        },
        required_contexts=["CI / all-required (pull_request)"],
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


def test_merge_decision_updates_stale_pr_before_merge():
    decision = mq.evaluate_merge_readiness(**_ready_kwargs(pr_has_current_base=False))

    assert decision.ready is False
    assert decision.action == "update"


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
    approvers, rc = mq.genuine_approvals(reviews, head_sha="HEAD", reviewer_set=REVIEWERS)
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
    approvers, rc = mq.genuine_approvals(reviews, head_sha="HEAD", reviewer_set=REVIEWERS)
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
    approvers, rc = mq.genuine_approvals(reviews, head_sha="HEAD", reviewer_set=REVIEWERS)
    assert approvers == set()


def test_genuine_approvals_latest_review_supersedes_earlier():
    # agent-reviewer-cr2 approved then later requested changes on same head.
    reviews = [
        {"state": "APPROVED", "user": {"login": "agent-reviewer-cr2"},
         "official": True, "stale": False, "dismissed": False, "commit_id": "HEAD"},
        {"state": "REQUEST_CHANGES", "user": {"login": "agent-reviewer-cr2"},
         "official": True, "stale": False, "dismissed": False, "commit_id": "HEAD"},
    ]
    approvers, rc = mq.genuine_approvals(reviews, head_sha="HEAD", reviewer_set=REVIEWERS)
    assert approvers == set()
    assert rc == ["agent-reviewer-cr2"]


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


def test_non_required_red_does_not_block_merge():
    # Required (CI) green; non-required governance reds present → still merge,
    # and force is set so force_merge bypasses ONLY those non-required reds.
    pr_status = {
        "state": "failure",  # combined polluted by non-required reds
        "statuses": [
            {"context": "CI / all-required (pull_request)", "status": "success"},
            {"context": "qa-review / approved (pull_request)", "status": "failure"},
            {"context": "security-review / approved (pull_request)", "status": "pending"},
            {"context": "sop-tier-check / tier-check (pull_request)", "status": "failure"},
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
    assert "required contexts not green" in decision.reason


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
    monkeypatch.setattr(mq, "REVIEWER_SET", REVIEWERS)

    monkeypatch.setattr(mq, "get_branch_protection", lambda branch: mq.BranchProtection(
        required_contexts=["CI / all-required (pull_request)"],
        required_approvals=2,
        block_on_rejected_reviews=True,
    ))
    main_sha = "b" * 40
    head_sha = "a" * 40
    monkeypatch.setattr(mq, "get_branch_head", lambda branch: main_sha)

    def fake_combined(sha):
        ctx = "CI / all-required (push)" if sha == main_sha else "CI / all-required (pull_request)"
        return {"state": "success", "statuses": [{"context": ctx, "status": "success"}]}
    monkeypatch.setattr(mq, "get_combined_status", fake_combined)

    monkeypatch.setattr(mq, "list_queued_issues", lambda: [
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
    monkeypatch.setattr(mq, "REVIEWER_SET", REVIEWERS)
    monkeypatch.setattr(mq, "get_branch_protection", lambda branch: mq.BranchProtection(
        required_contexts=["CI / all-required (pull_request)"],
        required_approvals=2,
        block_on_rejected_reviews=True,
    ))
    main_sha = "b" * 40
    head_sha = "a" * 40
    monkeypatch.setattr(mq, "get_branch_head", lambda branch: main_sha)

    def fake_combined(sha):
        ctx = "CI / all-required (push)" if sha == main_sha else "CI / all-required (pull_request)"
        return {"state": "success", "statuses": [{"context": ctx, "status": "success"}]}
    monkeypatch.setattr(mq, "get_combined_status", fake_combined)

    monkeypatch.setattr(mq, "list_queued_issues", lambda: [
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
    monkeypatch.setattr(mq, "REVIEWER_SET", REVIEWERS)
    monkeypatch.setattr(mq, "get_branch_protection", lambda branch: mq.BranchProtection(
        required_contexts=["CI / all-required (pull_request)"],
        required_approvals=2, block_on_rejected_reviews=True,
    ))
    main_sha = "b" * 40
    head_sha = "a" * 40
    monkeypatch.setattr(mq, "get_branch_head", lambda branch: main_sha)

    def fake_combined(sha):
        if sha == main_sha:
            return {"state": "success",
                    "statuses": [{"context": "CI / all-required (push)", "status": "success"}]}
        # PR head status fetch fails — fail-closed must propagate.
        raise mq.ApiError("GET /commits/HEAD/status -> HTTP 502: bad gateway")
    monkeypatch.setattr(mq, "get_combined_status", fake_combined)

    monkeypatch.setattr(mq, "list_queued_issues", lambda: [
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
    monkeypatch.setattr(mq, "REVIEWER_SET", REVIEWERS)
    monkeypatch.setattr(mq, "get_branch_protection", lambda branch: mq.BranchProtection(
        required_contexts=["CI / all-required (pull_request)"],
        required_approvals=2,
        block_on_rejected_reviews=True,
    ))
    main_sha = "b" * 40
    head_sha = "a" * 40
    monkeypatch.setattr(mq, "get_branch_head", lambda branch: main_sha)

    def fake_combined(sha):
        ctx = "CI / all-required (push)" if sha == main_sha else "CI / all-required (pull_request)"
        return {"state": "success", "statuses": [{"context": ctx, "status": "success"}]}
    monkeypatch.setattr(mq, "get_combined_status", fake_combined)

    monkeypatch.setattr(mq, "list_queued_issues", lambda: queued_issues)
    monkeypatch.setattr(mq, "get_pull", lambda n: {
        "state": "open", "number": n, "mergeable": True,
        "base": {"ref": "main", "repo_id": 1},
        "head": {"sha": head_sha, "repo_id": 1},
        "labels": [{"name": "merge-queue"}],
    })
    # NOTE: commits do NOT contain main_sha → pr_has_current_base is False →
    # decision.action == "update".
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
    """End-to-end HOL proof for #2352: PR #1409 (oldest) hits a 409-on-update
    and is held; on the NEXT tick choose_next_queued_issue must SKIP the held
    PR and select the next ready PR (#1500) instead of stalling on #1409."""
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

    # Tick 1: oldest (#1409) is selected, 409-on-update → held.
    rc = mq.process_once(dry_run=False)
    assert rc == 0
    assert calls["hold_label"] == (1409, "merge-queue-hold")

    # Simulate the label now present on #1409 (as the real hold would persist).
    conflicted["labels"] = [{"name": "merge-queue"}, {"name": "merge-queue-hold"}]

    # Tick 2: the queue must ADVANCE — choose_next_queued_issue skips the held
    # #1409 and selects the next ready candidate #1500, NOT re-select #1409.
    selected = mq.choose_next_queued_issue(
        [conflicted, next_ready],
        queue_label="merge-queue",
        hold_label="merge-queue-hold",
    )
    assert selected is not None
    assert selected["number"] == 1500
