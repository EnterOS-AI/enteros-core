import importlib.util
import sys
from pathlib import Path

import pytest

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


def test_required_contexts_green_rejects_volume_skipped_even_for_tier_low():
    """volume-skipped pending is a partial view, not a genuine soft-fail.

    Per sop-checklist.py:1179-1187, volume_skipped posts pending with a
    '[volume-skipped]' prefix. The merge queue must NOT treat this as an
    acceptable soft-fail for tier:low — the gate did not finish evaluating.
    """
    latest = mq.latest_statuses_by_context([
        {"context": "CI / all-required (pull_request)", "status": "success"},
        {
            "context": "sop-checklist / all-items-acked (pull_request)",
            "status": "pending",
            "description": "[volume-skipped] comment-cap=1000 hit; please file ...",
        },
    ])

    ok, missing_or_bad = mq.required_contexts_green(
        latest,
        [
            "CI / all-required (pull_request)",
            "sop-checklist / all-items-acked (pull_request)",
        ],
        pr_labels={"tier:low"},
    )

    assert ok is False
    assert "sop-checklist / all-items-acked (pull_request)=pending" in missing_or_bad


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

    def fake_combined(sha):
        ctx = "CI / all-required (push)" if sha == main_sha else "CI / all-required (pull_request)"
        return {"state": "success", "statuses": [{"context": ctx, "status": "success"}]}
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

    def fake_combined(sha):
        ctx = "CI / all-required (push)" if sha == main_sha else "CI / all-required (pull_request)"
        return {"state": "success", "statuses": [{"context": ctx, "status": "success"}]}
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

    def red_main_combined(sha):
        if sha == main_sha:
            return {"state": "failure",
                    "statuses": [{"context": "CI / all-required (push)", "status": "failure"}]}
        return {"state": "success",
                "statuses": [{"context": "CI / all-required (pull_request)", "status": "success"}]}
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

    def fake_combined(sha):
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

    def fake_combined(sha):
        ctx = "CI / all-required (push)" if sha == main_sha else "CI / all-required (pull_request)"
        return {"state": "success", "statuses": [{"context": ctx, "status": "success"}]}
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

    def fake_combined(sha):
        ctx = "CI / all-required (push)" if sha == main_sha else "CI / all-required (pull_request)"
        return {"state": "success", "statuses": [{"context": ctx, "status": "success"}]}
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
    def fake_combined(sha):
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

    def fake_combined(sha):
        ctx = "CI / all-required (push)" if sha == MAIN_SHA else "CI / all-required (pull_request)"
        return {"state": "success", "statuses": [{"context": ctx, "status": "success"}]}
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
    def fake_combined(sha):
        if sha == MAIN_SHA:
            return {"state": "success",
                    "statuses": [{"context": "CI / all-required (push)", "status": "success"}]}
        state = "failure" if sha == red_head else "success"
        return {"state": state,
                "statuses": [{"context": "CI / all-required (pull_request)", "status": state}]}
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
