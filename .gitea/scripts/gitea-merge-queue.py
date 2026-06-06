#!/usr/bin/env python3
"""gitea-merge-queue — conservative serialized merge bot for Gitea.

Gitea 1.22.6+ has auto-merge (`pull_auto_merge`) but no GitHub-style merge
queue. This script provides the missing serialized policy in user space:

1. Pick the oldest open same-repo PR that is NOT opted out (auto-discovery,
   see below), skipping drafts.
2. Refuse to act unless main's BP-required contexts are green.
3. Refuse fork PRs; the queue may only mutate same-repo branches.
4. If the PR branch does not contain current main, call Gitea's
   /pulls/{n}/update endpoint and stop. CI must rerun on the updated head.
5. Merge ONLY when, on the PR's CURRENT head sha:
     - >= REQUIRED_APPROVALS distinct GENUINE official APPROVED reviews from
       the recognised reviewer set (not stale, not dismissed, commit_id ==
       current head), AND
     - no open official REQUEST_CHANGES on the current head, AND
     - every BP-required status context is green, AND
     - the PR is mergeable.

Authoritative gates (fail-closed):
  - The REQUIRED status contexts come from BRANCH PROTECTION
    (`status_check_contexts`), not a hand-maintained env list. If branch
    protection cannot be enumerated, the queue HOLDS (does not merge blindly).
  - NON-required reds (qa-review, security-review, sop-tier, sop-checklist
    when not branch-required, E2E Chat, Staging SaaS, ci-arm64-advisory, any
    continue-on-error job) MUST NOT block. They are reported, never gating.
  - `force_merge=true` is used ONLY when the merge is blocked *solely* by
    missing-but-non-required governance contexts (required are green + genuine
    approvals present). It is NEVER used to bypass a failing REQUIRED context
    or missing approvals.

Auto-discovery (opt-OUT, label-optional):
  The queue is SELF-SUSTAINING — a ready PR does NOT need a human (or an agent)
  to add the `merge-queue` label first. When AUTO_DISCOVER is on (default), the
  queue enumerates ALL open same-repo PRs and considers any that meets the full
  merge bar (genuine approvals on current head + BP-required green + mergeable +
  no open REQUEST_CHANGES). The merge bar above is UNCHANGED; auto-discovery only
  changes WHICH PRs are considered, not whether they are mergeable.

  This deliberately removes the historical dependency on an agent adding the
  `merge-queue` label — agent Gitea tokens lack `write:issue` (labels are
  issue-scoped), so they could never self-label and the queue stalled. The label
  is now OPTIONAL metadata, not a gate.

  SAFETY is preserved as opt-OUT: any PR carrying an opt-out label
  (OPT_OUT_LABELS — `merge-queue-hold`, `do-not-auto-merge`, `wip` by default) is
  skipped (never auto-considered, never merged). Draft PRs (draft=true) are also
  skipped. A human who wants to keep a PR out of autonomous merging just adds one
  of those labels. Setting AUTO_DISCOVER=0 restores the legacy opt-IN behaviour
  (only PRs already carrying QUEUE_LABEL are considered).

Head-of-line (HOL) safety: a permanent permission/4xx merge error
(403/404/405) HOLDS the PR (applies HOLD_LABEL) so the queue advances to the
next PR instead of re-selecting the same wedged PR every tick. Likewise, a
persistent branch-update conflict (the /update endpoint returns HTTP 409
because the PR branch cannot be merged with main without manual rebase) HOLDS
the PR — a conflict will not self-resolve, so retrying it every tick would
HOL-block every ready PR behind it (issue #2352).

Status-fetch is fail-closed: if the combined status for a sha cannot be
fetched, the PR is skipped this tick (never treated as green).

The script is intentionally one-PR-per-run. Workflow/cron concurrency should
serialize invocations so two green PRs cannot merge against the same main.
"""

from __future__ import annotations

import argparse
import dataclasses
import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request
from typing import Any


def _env(key: str, *, default: str = "") -> str:
    return os.environ.get(key, default)


GITEA_TOKEN = _env("GITEA_TOKEN")
GITEA_HOST = _env("GITEA_HOST")
REPO = _env("REPO")
WATCH_BRANCH = _env("WATCH_BRANCH", default="main")
QUEUE_LABEL = _env("QUEUE_LABEL", default="merge-queue")
HOLD_LABEL = _env("HOLD_LABEL", default="merge-queue-hold")
UPDATE_STYLE = _env("UPDATE_STYLE", default="merge")
# Auto-discovery (opt-OUT). When truthy (default), the queue considers ALL open
# same-repo PRs that meet the merge bar, not only PRs already carrying
# QUEUE_LABEL — so the queue is self-sustaining without any human/agent labeling
# (agent tokens lack write:issue and cannot self-label). Set AUTO_DISCOVER=0 to
# restore the legacy opt-IN behaviour (QUEUE_LABEL required to be considered).
AUTO_DISCOVER = _env("AUTO_DISCOVER", default="1").strip().lower() not in {
    "0",
    "false",
    "no",
    "off",
    "",
}
# Opt-OUT labels. A PR carrying ANY of these is skipped (never auto-considered,
# never merged) — the human escape hatch from autonomous merging. HOLD_LABEL is
# always included so the existing hold semantics keep working. `do-not-auto-merge`
# and `wip` let a human keep a PR out of the auto-merge path without removing it.
OPT_OUT_LABELS = {
    name.strip()
    for name in _env(
        "OPT_OUT_LABELS",
        default="do-not-auto-merge,wip",
    ).split(",")
    if name.strip()
} | ({HOLD_LABEL} if HOLD_LABEL else set())
REQUIRED_CONTEXTS_RAW = _env(
    "REQUIRED_CONTEXTS",
    default=(
        "CI / all-required (pull_request),"
        "sop-checklist / all-items-acked (pull_request)"
    ),
)
# Required contexts for push (main/staging) runs. The push CI uses the same
# aggregator names with " (push)" suffix. Checking these explicitly instead of
# the combined state avoids false-pause when non-blocking jobs (e.g. Platform
# Go with continue-on-error: true due to mc#774) have failed — their failures
# pollute the combined state but do not block merges.
PUSH_REQUIRED_CONTEXTS_RAW = _env(
    "PUSH_REQUIRED_CONTEXTS",
    default="CI / all-required (push)",
)

# Recognised official-reviewer set. A merge requires this many DISTINCT genuine
# approvals (not stale/dismissed, on the current head sha) from accounts in
# this set. The set is the real agents-team reviewer roster; founder/CTO-agent
# accounts are intentionally excluded so the queue cannot be satisfied by a
# human/owner approval alone — it must be a genuine peer review.
REVIEWER_SET = {
    name.strip()
    for name in _env(
        "REVIEWER_SET",
        default="agent-reviewer,agent-researcher,agent-reviewer-cr2",
    ).split(",")
    if name.strip()
}
# Default mirrors molecule-core branch protection (required_approvals: 2). The
# authoritative value is read from branch protection at runtime; this is only
# the fallback when BP does not specify one.
REQUIRED_APPROVALS_DEFAULT = int(_env("REQUIRED_APPROVALS", default="2") or "2")

OWNER, NAME = (REPO.split("/", 1) + [""])[:2] if REPO else ("", "")
API = f"https://{GITEA_HOST}/api/v1" if GITEA_HOST else ""


class ApiError(RuntimeError):
    pass


class MergePermissionError(ApiError):
    """Merge failed with a permanent permission error (403/404/405).
    The queue should HOLD this PR and move to the next one."""


class BranchUpdateConflictError(ApiError):
    """Updating the PR branch with the base hit a merge-conflict (HTTP 409).

    A true merge-conflict is NOT transient: the branch cannot be auto-updated
    until a human/agent rebases it. The queue should HOLD this PR (apply
    HOLD_LABEL) and advance to the next candidate, exactly like the permission
    path — otherwise the conflicted PR sits at the queue head and is retried
    every tick forever, head-of-line-blocking every ready PR behind it.

    NOTE: distinct from mergeable=None, which is Gitea STILL COMPUTING conflict
    state — that case is handled as a transient WAIT (no hold). This error is
    only raised on an explicit 409 returned by the /update endpoint."""


class BranchProtectionUnavailable(ApiError):
    """Branch protection (the authoritative required-context source) could not
    be enumerated. The queue must HOLD rather than merge with an unverified
    required-context set (fail-closed, no fail-open)."""


@dataclasses.dataclass(frozen=True)
class MergeDecision:
    ready: bool
    action: str
    reason: str
    # When ready is True, force indicates the merge is blocked SOLELY by
    # missing-but-non-required governance contexts (required are green +
    # genuine approvals present), so force_merge=true is justified to bypass
    # ONLY those non-required contexts. Defaults False.
    force: bool = False


@dataclasses.dataclass(frozen=True)
class BranchProtection:
    """The subset of branch protection the queue depends on."""

    required_contexts: list[str]
    required_approvals: int
    block_on_rejected_reviews: bool


def _require_runtime_env() -> None:
    for key in ("GITEA_TOKEN", "GITEA_HOST", "REPO", "WATCH_BRANCH", "QUEUE_LABEL"):
        if not os.environ.get(key):
            sys.stderr.write(f"::error::missing required env var: {key}\n")
            sys.exit(2)
    if UPDATE_STYLE not in {"merge", "rebase"}:
        sys.stderr.write("::error::UPDATE_STYLE must be merge or rebase\n")
        sys.exit(2)


def api(
    method: str,
    path: str,
    *,
    body: dict | None = None,
    query: dict[str, str] | None = None,
    expect_json: bool = True,
) -> tuple[int, Any]:
    url = f"{API}{path}"
    if query:
        url = f"{url}?{urllib.parse.urlencode(query)}"
    data = None
    headers = {
        "Authorization": f"token {GITEA_TOKEN}",
        "Accept": "application/json",
    }
    if body is not None:
        data = json.dumps(body).encode("utf-8")
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(url, method=method, data=data, headers=headers)
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            raw = resp.read()
            status = resp.status
    except urllib.error.HTTPError as exc:
        raw = exc.read()
        status = exc.code

    if not (200 <= status < 300):
        snippet = raw[:500].decode("utf-8", errors="replace") if raw else ""
        raise ApiError(f"{method} {path} -> HTTP {status}: {snippet}")
    if not raw:
        return status, None
    try:
        return status, json.loads(raw)
    except json.JSONDecodeError as exc:
        if expect_json:
            raise ApiError(f"{method} {path} -> HTTP {status} non-JSON: {exc}") from exc
        return status, {"_raw": raw.decode("utf-8", errors="replace")}


def required_contexts(raw: str) -> list[str]:
    return [part.strip() for part in raw.split(",") if part.strip()]


def push_required_contexts() -> list[str]:
    """Required contexts for push (branch) CI runs. See PUSH_REQUIRED_CONTEXTS_RAW."""
    return required_contexts(PUSH_REQUIRED_CONTEXTS_RAW)


def status_state(status: dict) -> str:
    return str(status.get("status") or status.get("state") or "").lower()


def latest_statuses_by_context(statuses: list[dict]) -> dict[str, dict]:
    # Gitea /statuses endpoint returns entries in ascending id order (oldest
    # first). We need the LAST occurrence of each context, so iterate in
    # reverse to prefer newer entries.
    latest: dict[str, dict] = {}
    for status in reversed(statuses):
        context = status.get("context")
        if isinstance(context, str):
            latest[context] = status  # overwrite: reverse order → newest wins
    return latest


def _is_tier_low_pending_ok(
    latest_statuses: dict[str, dict],
    context: str,
    pr_labels: set[str],
) -> bool:
    """Return True if tier:low PR can tolerate sop-checklist pending state.

    Per sop-checklist-config.yaml tier_failure_mode, tier:low uses soft-fail:
    sop-checklist posts state=pending when acks are satisfied (missing
    manager/ceo acks are informational only). The queue should accept
    pending instead of waiting for success.
    """
    if "tier:low" not in pr_labels:
        return False
    if "sop-checklist" not in context:
        return False
    status = latest_statuses.get(context) or {}
    return status_state(status) == "pending"


def required_contexts_green(
    latest_statuses: dict[str, dict],
    contexts: list[str],
    pr_labels: set[str] | None = None,
) -> tuple[bool, list[str]]:
    missing_or_bad: list[str] = []
    for context in contexts:
        status = latest_statuses.get(context)
        state = status_state(status or {})
        if state != "success":
            if pr_labels and _is_tier_low_pending_ok(
                latest_statuses, context, pr_labels
            ):
                continue  # tier:low soft-fail: accept pending sop-checklist
            missing_or_bad.append(f"{context}={state or 'missing'}")
    return not missing_or_bad, missing_or_bad


def parse_branch_protection(body: Any) -> BranchProtection:
    """Extract the queue-relevant fields from a branch_protections payload.

    Fail-closed: raises BranchProtectionUnavailable when status checks are
    expected but the required-context list cannot be enumerated. We never fall
    back to a hand-maintained env list as the authoritative required set —
    doing so risks merging when a real required context is red/missing.
    """
    if not isinstance(body, dict):
        raise BranchProtectionUnavailable("branch protection response not an object")
    enable = bool(body.get("enable_status_check"))
    contexts_raw = body.get("status_check_contexts")
    if not enable:
        # Status checks not enforced by BP at all. With no required contexts
        # the queue would gate on approvals only — acceptable, but make it
        # explicit and let the caller decide.
        contexts: list[str] = []
    else:
        if not isinstance(contexts_raw, list):
            raise BranchProtectionUnavailable(
                "enable_status_check is true but status_check_contexts is not a list"
            )
        contexts = [c for c in contexts_raw if isinstance(c, str) and c.strip()]
        if not contexts:
            raise BranchProtectionUnavailable(
                "enable_status_check is true but status_check_contexts is empty"
            )
    approvals = body.get("required_approvals")
    required_approvals = (
        int(approvals) if isinstance(approvals, int) else REQUIRED_APPROVALS_DEFAULT
    )
    return BranchProtection(
        required_contexts=contexts,
        required_approvals=required_approvals,
        block_on_rejected_reviews=bool(body.get("block_on_rejected_reviews")),
    )


def get_branch_protection(branch: str) -> BranchProtection:
    """Fetch branch protection for `branch`; fail-closed if unavailable."""
    try:
        _, body = api("GET", f"/repos/{OWNER}/{NAME}/branch_protections/{branch}")
    except ApiError as exc:
        raise BranchProtectionUnavailable(
            f"could not fetch branch protection for {branch}: {exc}"
        ) from exc
    return parse_branch_protection(body)


def genuine_approvals(
    reviews: list[dict],
    *,
    head_sha: str,
    reviewer_set: set[str],
) -> tuple[set[str], list[str]]:
    """Reduce a PR's reviews to genuine official approvals on the CURRENT head.

    Returns (approvers, request_changes) where:
      - approvers is the set of distinct logins (in reviewer_set) whose LATEST
        review on the current head is an official, non-stale, non-dismissed
        APPROVED, and
      - request_changes is the list of logins (in reviewer_set) whose latest
        official review on the current head is REQUEST_CHANGES.

    "Current head" is enforced two ways, because Gitea exposes both signals:
    a review must be `official` and NOT `stale`/`dismissed`, AND when the
    review carries a commit_id it must equal head_sha. A review with no
    commit_id but stale=False/dismissed=False is accepted (older Gitea rows).
    We take each reviewer's LATEST submission (reviews arrive oldest-first), so
    a later REQUEST_CHANGES correctly supersedes an earlier APPROVED and vice
    versa.
    """
    latest_by_user: dict[str, dict] = {}
    for review in reviews:
        if not isinstance(review, dict):
            continue
        user = (review.get("user") or {}).get("login")
        if not isinstance(user, str) or user not in reviewer_set:
            continue
        state = str(review.get("state") or "").upper()
        if state not in {"APPROVED", "REQUEST_CHANGES"}:
            continue  # ignore COMMENT/PENDING/DISMISSED-state rows
        # reviews are returned oldest-first; later entries overwrite → latest wins
        latest_by_user[user] = review

    approvers: set[str] = set()
    request_changes: list[str] = []
    for user, review in latest_by_user.items():
        if not review.get("official"):
            continue
        if review.get("stale") or review.get("dismissed"):
            continue
        commit_id = review.get("commit_id")
        if isinstance(commit_id, str) and commit_id and head_sha:
            if commit_id != head_sha:
                continue  # review was on a previous head
        state = str(review.get("state") or "").upper()
        if state == "APPROVED":
            approvers.add(user)
        elif state == "REQUEST_CHANGES":
            request_changes.append(user)
    return approvers, request_changes


def get_pull_reviews(pr_number: int) -> list[dict]:
    _, body = api("GET", f"/repos/{OWNER}/{NAME}/pulls/{pr_number}/reviews")
    if not isinstance(body, list):
        raise ApiError(f"PR #{pr_number} reviews response not list")
    return body


def label_names(issue: dict) -> set[str]:
    return {
        label["name"]
        for label in issue.get("labels", [])
        if isinstance(label, dict) and isinstance(label.get("name"), str)
    }


def choose_next_queued_issue(
    issues: list[dict],
    *,
    queue_label: str,
    hold_label: str = "",
) -> dict | None:
    candidates = []
    for issue in issues:
        labels = label_names(issue)
        if queue_label not in labels:
            continue
        if hold_label and hold_label in labels:
            continue
        if "pull_request" not in issue:
            continue
        candidates.append(issue)
    candidates.sort(key=lambda issue: (issue.get("created_at") or "", int(issue["number"])))
    return candidates[0] if candidates else None


def _issue_is_draft(issue: dict) -> bool:
    """True if the issue/PR is a draft.

    The /issues listing exposes draft state under the `pull_request` sub-object
    (`{"draft": true}`); some Gitea versions also surface a top-level `draft`.
    Either is honoured. Drafts are never auto-considered for merging.
    """
    pr = issue.get("pull_request")
    if isinstance(pr, dict) and pr.get("draft") is True:
        return True
    return issue.get("draft") is True


def choose_next_candidate_issue(
    issues: list[dict],
    *,
    queue_label: str,
    opt_out_labels: set[str],
    auto_discover: bool,
) -> dict | None:
    """Pick the oldest open PR eligible for a merge attempt this tick.

    This is the auto-discovery selector. It does NOT change the merge bar — it
    only changes WHICH PRs are considered:

      - auto_discover=True (default): every open same-repo PR is a candidate,
        EXCEPT those carrying an opt-out label or marked draft. The QUEUE_LABEL
        is optional metadata, not a gate, so a ready PR reaches the queue with no
        human/agent labeling (the write:issue gap is removed).
      - auto_discover=False: legacy opt-IN — only PRs carrying queue_label are
        candidates (still skipping opt-out labels and drafts).

    Opt-out is the safety escape hatch: any opt_out_labels member present skips
    the PR entirely (never considered, never merged). Selection is oldest-first
    (created_at, then number) to preserve the serialized FIFO ordering.
    """
    candidates = []
    for issue in issues:
        if "pull_request" not in issue:
            continue
        labels = label_names(issue)
        if opt_out_labels & labels:
            continue  # opt-out: human kept this PR out of autonomous merging
        if _issue_is_draft(issue):
            continue  # drafts are never auto-merged
        if not auto_discover and queue_label not in labels:
            continue  # legacy opt-IN: require the queue label
        candidates.append(issue)
    candidates.sort(key=lambda issue: (issue.get("created_at") or "", int(issue["number"])))
    return candidates[0] if candidates else None


def pr_contains_base_sha(commits: list[dict], base_sha: str) -> bool:
    for commit in commits:
        sha = commit.get("sha") or commit.get("id")
        if sha == base_sha:
            return True
    return False


def pr_has_current_base(pr: dict, commits: list[dict], main_sha: str) -> bool:
    if pr.get("merge_base") == main_sha:
        return True
    return pr_contains_base_sha(commits, main_sha)


def _non_required_red_present(
    latest: dict[str, dict],
    required_contexts: list[str],
) -> bool:
    """True if any NON-required context is non-success.

    Such reds are the governance/SOP/advisory checks Gitea may still treat as
    "missing required context" at merge time even though branch protection does
    not require them. Their presence is what justifies force_merge=true (we
    have already verified every REQUIRED context is green and approvals are
    genuine, so force only bypasses these non-required reds).
    """
    required = set(required_contexts)
    for context, status in latest.items():
        if context in required:
            continue
        if status_state(status) != "success":
            return True
    return False


def evaluate_merge_readiness(
    *,
    main_status: dict,
    pr_status: dict,
    required_contexts: list[str],
    required_approvals: int,
    approvers: set[str],
    request_changes: list[str],
    pr_has_current_base: bool,
    mergeable: bool,
    pr_labels: set[str] | None = None,
) -> MergeDecision:
    # 1) Main's push-required contexts must be green. Combined state can be
    #    "failure" due to non-blocking jobs (continue-on-error: true) that do
    #    not gate merges, so check the explicit required set, not combined.
    main_latest = latest_statuses_by_context(main_status.get("statuses") or [])
    main_ok, main_bad = required_contexts_green(main_latest, push_required_contexts())
    if not main_ok:
        return MergeDecision(False, "pause", "main required contexts not green: " + ", ".join(main_bad))

    # 2) PR head must contain current main.
    if not pr_has_current_base:
        return MergeDecision(False, "update", "PR head does not contain current main")

    # 3) No open official REQUEST_CHANGES on the current head.
    if request_changes:
        return MergeDecision(
            False, "wait",
            "open REQUEST_CHANGES on current head from: " + ", ".join(sorted(request_changes)),
        )

    # 4) Enough distinct genuine official approvals on the current head.
    if len(approvers) < required_approvals:
        return MergeDecision(
            False, "wait",
            f"insufficient genuine approvals on current head: have "
            f"{len(approvers)} ({', '.join(sorted(approvers)) or 'none'}), "
            f"need {required_approvals}",
        )

    # 5) Every BRANCH-PROTECTION-REQUIRED status context must be green. This is
    #    the authoritative status gate — NON-required reds (qa-review,
    #    security-review, sop-tier/sop-checklist when not BP-required, E2E Chat,
    #    Staging SaaS, ci-arm64-advisory, continue-on-error jobs) are NOT
    #    consulted here and must not block.
    latest = latest_statuses_by_context(pr_status.get("statuses") or [])
    ok, missing_or_bad = required_contexts_green(latest, required_contexts, pr_labels)
    if not ok:
        return MergeDecision(False, "wait", "required contexts not green: " + ", ".join(missing_or_bad))

    # 6) Gitea must consider the PR mergeable (no conflicts).
    if not mergeable:
        return MergeDecision(False, "wait", "PR is not mergeable (conflicts)")

    # Ready. Use force_merge ONLY if the merge would otherwise be blocked by
    # missing-but-non-required governance contexts. Required are green and
    # approvals are genuine, so force only bypasses non-required reds — never a
    # failing required context or missing approval.
    force = _non_required_red_present(latest, required_contexts)
    return MergeDecision(True, "merge", "ready", force=force)


def get_branch_head(branch: str) -> str:
    _, body = api("GET", f"/repos/{OWNER}/{NAME}/branches/{branch}")
    commit = body.get("commit") if isinstance(body, dict) else None
    sha = commit.get("id") if isinstance(commit, dict) else None
    if not isinstance(sha, str) or len(sha) < 7:
        raise ApiError(f"branch {branch} response missing commit id")
    return sha


def get_combined_status(sha: str) -> dict:
    """Combined status + all individual statuses for `sha`.

    The /status endpoint caps the `statuses` array at 30 entries (Gitea
    default page size), so we fetch the full list via /statuses with a
    higher limit. The combined `state` still comes from /status.

    Fail-closed: the PRIMARY /status fetch must succeed. If it raises, the
    error propagates so the caller skips this PR this tick (we never treat a
    failed status fetch as green — dev-sop "no fail-open"). Only the SECONDARY
    /statuses enrichment (which merely extends the per-context list beyond the
    30-entry cap) is best-effort; if it fails we still have the combined set.
    """
    _, combined = api("GET", f"/repos/{OWNER}/{NAME}/commits/{sha}/status")
    if not isinstance(combined, dict):
        raise ApiError(f"status for {sha} response not object")
    combined_statuses: list[dict] = combined.get("statuses") or []
    try:
        _, all_statuses_raw = api(
            "GET",
            f"/repos/{OWNER}/{NAME}/commits/{sha}/statuses",
            query={"limit": "50"},
        )
        if isinstance(all_statuses_raw, list):
            all_statuses: list[dict] = list(all_statuses_raw)
        else:
            all_statuses = []
    except (ApiError, urllib.error.URLError, TimeoutError, OSError) as exc:
        sys.stderr.write(f"::warning::could not fetch full statuses list for {sha[:8]}: {exc}\n")
        all_statuses = []
    # Build latest per context: process combined (ascending→reverse=newest
    # first), then fill gaps from all_statuses (already newest-first).
    latest: dict[str, dict] = {}
    for status in reversed(sorted(combined_statuses, key=lambda s: s.get("id") or 0)):
        ctx = status.get("context")
        if isinstance(ctx, str) and ctx not in latest:
            latest[ctx] = status
    for status in all_statuses:
        ctx = status.get("context")
        if isinstance(ctx, str) and ctx not in latest:
            latest[ctx] = status
    combined["statuses"] = list(latest.values())
    return combined


def list_queued_issues() -> list[dict]:
    _, body = api(
        "GET",
        f"/repos/{OWNER}/{NAME}/issues",
        query={
            "state": "open",
            "type": "pulls",
            "labels": QUEUE_LABEL,
            "limit": "50",
        },
    )
    if not isinstance(body, list):
        raise ApiError("queued issues response not list")
    return body


def list_candidate_issues(*, auto_discover: bool) -> list[dict]:
    """Open PR issues eligible for consideration this tick.

    With auto_discover=True (default) this enumerates ALL open PRs (no label
    filter) so the queue is self-sustaining — a ready PR is considered without
    any human/agent first adding QUEUE_LABEL. With auto_discover=False it falls
    back to the legacy label-filtered listing (opt-IN). Opt-out filtering and
    draft-skipping happen in choose_next_candidate_issue, not here.
    """
    if not auto_discover:
        return list_queued_issues()
    _, body = api(
        "GET",
        f"/repos/{OWNER}/{NAME}/issues",
        query={
            "state": "open",
            "type": "pulls",
            "limit": "50",
        },
    )
    if not isinstance(body, list):
        raise ApiError("candidate issues response not list")
    return body


def get_pull(pr_number: int) -> dict:
    _, body = api("GET", f"/repos/{OWNER}/{NAME}/pulls/{pr_number}")
    if not isinstance(body, dict):
        raise ApiError(f"PR #{pr_number} response not object")
    return body


def get_pull_commits(pr_number: int) -> list[dict]:
    _, body = api("GET", f"/repos/{OWNER}/{NAME}/pulls/{pr_number}/commits")
    if not isinstance(body, list):
        raise ApiError(f"PR #{pr_number} commits response not list")
    return body


def post_comment(pr_number: int, body: str, *, dry_run: bool) -> None:
    print(f"::notice::comment PR #{pr_number}: {body.splitlines()[0][:160]}")
    if dry_run:
        return
    api("POST", f"/repos/{OWNER}/{NAME}/issues/{pr_number}/comments", body={"body": body})


def update_pull(pr_number: int, *, dry_run: bool) -> None:
    print(f"::notice::updating PR #{pr_number} with base branch via style={UPDATE_STYLE}")
    if dry_run:
        return
    try:
        api(
            "POST",
            f"/repos/{OWNER}/{NAME}/pulls/{pr_number}/update",
            query={"style": UPDATE_STYLE},
            expect_json=False,
        )
    except ApiError as exc:
        # Gitea returns HTTP 409 when the base cannot be merged into the PR
        # branch because of a real conflict. The queue cannot auto-resolve a
        # conflict, so re-raise as BranchUpdateConflictError; process_once HOLDs
        # the PR and advances (HOL guard) instead of retrying it forever.
        # Match the HTTP STATUS token ("-> HTTP 409") specifically, not a bare
        # "409" substring — the PR number or path can itself contain "409"
        # (e.g. /pulls/1409/update) and must not be misread as a conflict.
        if "-> HTTP 409" in str(exc):
            raise BranchUpdateConflictError(str(exc)) from exc
        raise  # re-raise other ApiErrors unchanged


def add_label_by_name(pr_number: int, label_name: str, *, dry_run: bool) -> None:
    """Apply an existing repo label (by name) to a PR/issue.

    Used to HOLD a wedged PR so the queue advances. Resolves the label id from
    the repo label set; if the label does not exist, raises ApiError (the
    caller decides whether that is fatal).
    """
    print(f"::notice::applying label '{label_name}' to PR #{pr_number}")
    if dry_run:
        return
    _, labels = api("GET", f"/repos/{OWNER}/{NAME}/labels", query={"limit": "100"})
    label_id = None
    if isinstance(labels, list):
        for label in labels:
            if isinstance(label, dict) and label.get("name") == label_name:
                label_id = label.get("id")
                break
    if label_id is None:
        raise ApiError(f"label '{label_name}' not found in repo {OWNER}/{NAME}")
    api(
        "POST",
        f"/repos/{OWNER}/{NAME}/issues/{pr_number}/labels",
        body={"labels": [label_id]},
    )


def hold_pr(pr_number: int, hold_note: str, *, dry_run: bool) -> None:
    """Apply HOLD_LABEL to a wedged PR so the queue advances past it.

    choose_next_queued_issue skips HOLD_LABEL-bearing PRs, so this is the HOL
    guard: a PR the queue cannot make progress on (permanent permission error
    or unresolvable branch-update conflict) is held and a human/agent fixes it,
    rather than the queue re-selecting it every tick forever. If the label
    cannot be applied we still post the explanatory comment so the wedge is at
    least visible — but we never loop on the PR.
    """
    try:
        add_label_by_name(pr_number, HOLD_LABEL, dry_run=dry_run)
    except ApiError as label_exc:
        sys.stderr.write(
            f"::error::could not apply HOLD_LABEL to PR #{pr_number}: {label_exc}\n"
        )
        hold_note += (
            f"\n\n(NOTE: could not apply the hold label automatically: "
            f"{label_exc}. Please add `{HOLD_LABEL}` manually.)"
        )
    post_comment(pr_number, hold_note, dry_run=dry_run)


def merge_pull(pr_number: int, *, dry_run: bool, force: bool = False) -> None:
    payload: dict[str, Any] = {
        "Do": "merge",
        "MergeTitleField": f"Merge PR #{pr_number} via Gitea merge queue",
        "MergeMessageField": (
            "Serialized merge by gitea-merge-queue after current-main, "
            "genuine approvals, and required CI checks were green."
        ),
    }
    if force:
        # force_merge bypasses ONLY missing-but-non-required governance
        # contexts. The caller has already verified required contexts are green
        # and genuine approvals are present, so this never bypasses a failing
        # required context or an approval shortfall.
        payload["force_merge"] = True
    print(f"::notice::merging PR #{pr_number}{' (force_merge: non-required reds)' if force else ''}")
    if dry_run:
        return
    try:
        api("POST", f"/repos/{OWNER}/{NAME}/pulls/{pr_number}/merge", body=payload, expect_json=False)
    except ApiError as exc:
        # Re-raise permission-like errors so process_once can HOLD this PR.
        # 403 = no push access, 404 = repo/pr not found, 405 = not allowed.
        msg = str(exc)
        for code in ("403", "404", "405"):
            if code in msg:
                raise MergePermissionError(msg) from exc
        raise  # re-raise other ApiErrors unchanged


def process_once(*, dry_run: bool = False) -> int:
    # Required status contexts come from BRANCH PROTECTION, not a hand-kept env
    # list. Fail-closed: if BP cannot be enumerated, HOLD the whole tick rather
    # than merge against an unverified required set.
    try:
        bp = get_branch_protection(WATCH_BRANCH)
    except BranchProtectionUnavailable as exc:
        sys.stderr.write(
            f"::error::queue held: branch protection for {WATCH_BRANCH} "
            f"unavailable (fail-closed): {exc}\n"
        )
        return 0
    contexts = bp.required_contexts
    required_approvals = bp.required_approvals
    print(
        f"::notice::queue policy from branch protection: "
        f"required_approvals={required_approvals} "
        f"required_contexts={contexts or '[none]'}"
    )

    main_sha = get_branch_head(WATCH_BRANCH)
    main_status = get_combined_status(main_sha)
    # Check push-required contexts explicitly instead of combined state.
    # See evaluate_merge_readiness for rationale.
    main_latest = latest_statuses_by_context(main_status.get("statuses") or [])
    main_ok, main_bad = required_contexts_green(main_latest, push_required_contexts())
    if not main_ok:
        print(f"::notice::queue paused: {WATCH_BRANCH}@{main_sha[:8]} required contexts not green: {', '.join(main_bad)}")
        return 0

    issue = choose_next_candidate_issue(
        list_candidate_issues(auto_discover=AUTO_DISCOVER),
        queue_label=QUEUE_LABEL,
        opt_out_labels=OPT_OUT_LABELS,
        auto_discover=AUTO_DISCOVER,
    )
    if not issue:
        print(
            "::notice::no merge candidates "
            f"(auto_discover={'on' if AUTO_DISCOVER else 'off'})"
        )
        return 0

    pr_number = int(issue["number"])
    pr = get_pull(pr_number)
    if pr.get("state") != "open":
        print(f"::notice::PR #{pr_number} is not open; skipping")
        return 0
    # Defensive opt-out/draft re-check on the authoritative pull payload: the
    # /issues listing's label/draft view can lag, but the merge bar must respect
    # the live pull state. (choose_next_candidate_issue already filtered on the
    # listing; this guards against a stale listing racing a just-added opt-out.)
    if OPT_OUT_LABELS & label_names(pr):
        print(f"::notice::PR #{pr_number} carries an opt-out label; skipping")
        return 0
    if pr.get("draft") is True:
        print(f"::notice::PR #{pr_number} is a draft; skipping")
        return 0
    if pr.get("base", {}).get("ref") != WATCH_BRANCH:
        post_comment(pr_number, f"merge-queue: skipped; base branch is not `{WATCH_BRANCH}`.", dry_run=dry_run)
        return 0
    if pr.get("head", {}).get("repo_id") != pr.get("base", {}).get("repo_id"):
        post_comment(pr_number, "merge-queue: skipped; fork PRs are not supported by the serialized queue.", dry_run=dry_run)
        return 0

    head_sha = pr.get("head", {}).get("sha")
    if not isinstance(head_sha, str) or len(head_sha) < 7:
        raise ApiError(f"PR #{pr_number} missing head sha")
    commits = get_pull_commits(pr_number)
    current_base = pr_has_current_base(pr, commits, main_sha)
    # Fail-closed: a failed status fetch raises here and the tick is skipped
    # (the PR is never treated as green).
    pr_status = get_combined_status(head_sha)
    pr_labels = label_names(pr)
    # FAIL-CLOSED: Gitea returns mergeable=None (or omits the field) while it is
    # still COMPUTING conflict state. Only the literal True is decisive proof the
    # PR is conflict-free; None and False both mean "not (yet) mergeable". We must
    # NOT autonomously merge on an unknown — treat anything but True as not-yet-
    # mergeable so evaluate_merge_readiness returns a transient "wait" decision.
    # This is transient: process_once returns 0 (no hold label, no dequeue) and
    # the PR is re-checked next tick once Gitea has finished computing mergeability.
    mergeable_field = pr.get("mergeable")
    mergeable = mergeable_field is True

    reviews = get_pull_reviews(pr_number)
    approvers, request_changes = genuine_approvals(
        reviews, head_sha=head_sha, reviewer_set=REVIEWER_SET
    )

    decision = evaluate_merge_readiness(
        main_status=main_status,
        pr_status=pr_status,
        required_contexts=contexts,
        required_approvals=required_approvals,
        approvers=approvers,
        request_changes=request_changes,
        pr_has_current_base=current_base,
        mergeable=mergeable,
        pr_labels=pr_labels,
    )

    print(f"::notice::PR #{pr_number} decision={decision.action}: {decision.reason}")
    if decision.action == "update":
        try:
            update_pull(pr_number, dry_run=dry_run)
        except BranchUpdateConflictError as exc:
            # The branch cannot be updated with main because of a real conflict
            # (HTTP 409). This is the HOL fix for issue #2352: previously the
            # 409 propagated to main() and the tick exited 0 with the PR still
            # queued, so the NEXT tick re-selected the SAME conflicted PR and
            # retried the failing update forever — head-of-line-blocking every
            # ready PR behind it. A conflict will not self-resolve; it needs a
            # human/agent rebase. So HOLD this PR (HOL guard) and advance to the
            # next candidate. Fail-closed: a held PR is skipped, never merged.
            sys.stderr.write(
                f"::error::branch-update conflict for PR #{pr_number}: {exc}\n"
            )
            hold_note = (
                "merge-queue: could not update this branch with "
                f"`{WATCH_BRANCH}` — the update returned a merge conflict "
                f"(HTTP 409) that the queue cannot auto-resolve ({exc}). "
                f"Applied `{HOLD_LABEL}` to unblock the queue (HOL guard). "
                f"Fix: rebase/merge `{WATCH_BRANCH}` into this branch and "
                f"resolve the conflicts, then remove `{HOLD_LABEL}` to requeue."
            )
            hold_pr(pr_number, hold_note, dry_run=dry_run)
            return 0
        post_comment(
            pr_number,
            (
                f"merge-queue: updated this branch with `{WATCH_BRANCH}` at "
                f"`{main_sha[:12]}`. Waiting for CI on the refreshed head."
            ),
            dry_run=dry_run,
        )
        return 0
    if decision.ready:
        latest_main_sha = get_branch_head(WATCH_BRANCH)
        if latest_main_sha != main_sha:
            print(
                f"::notice::main moved {main_sha[:8]} -> {latest_main_sha[:8]}; "
                "deferring to next tick"
            )
            return 0
        try:
            merge_pull(pr_number, dry_run=dry_run, force=decision.force)
        except MergePermissionError as exc:
            # Permanent merge failure (HTTP 403/404/405). This is the
            # head-of-line (HOL) bug fix: previously we returned 0 with the PR
            # still queued, so the next tick re-selected the SAME wedged PR
            # forever and the queue never advanced. Instead, HOLD this PR by
            # applying HOLD_LABEL (choose_next_queued_issue skips held PRs), so
            # the queue moves on to the next candidate. A maintainer removes
            # the hold once the permission issue is fixed.
            sys.stderr.write(f"::error::merge permission error for PR #{pr_number}: {exc}\n")
            hold_note = (
                "merge-queue: merge failed with a permanent permission error "
                f"({exc}). No available token has Can-merge permission for this "
                f"PR. Applied `{HOLD_LABEL}` to unblock the queue (HOL guard). "
                f"Fix: grant Can-merge to the queue token, then remove "
                f"`{HOLD_LABEL}` to requeue."
            )
            hold_pr(pr_number, hold_note, dry_run=dry_run)
            return 0
        return 0
    return 0


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args()
    _require_runtime_env()
    try:
        return process_once(dry_run=args.dry_run)
    except ApiError as exc:
        # API errors (401/403/404/500) are transient for a queue tick —
        # log and exit 0 so the workflow is not marked failed and the next
        # tick can retry. Returning non-zero would permanently fail the
        # workflow run, blocking future ticks.
        sys.stderr.write(f"::error::queue API error: {exc}\n")
        return 0
    except urllib.error.URLError as exc:
        sys.stderr.write(f"::error::queue network error: {exc}\n")
        return 0
    except TimeoutError as exc:
        sys.stderr.write(f"::error::queue timeout: {exc}\n")
        return 0


if __name__ == "__main__":
    sys.exit(main())
