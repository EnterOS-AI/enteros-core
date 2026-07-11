#!/usr/bin/env python3
"""PR diff-size / destructive-diff guard.

Implements core#2875: block stale branches whose head has drifted into a
massive destructive diff against current main (e.g., PR #1100: 481 files
changed, ~55k deletions). The guard runs on every PR and fails loudly when
any of the configured thresholds are exceeded.

The check compares the PR head against the merge base of the target branch,
so rebasing a stale branch to a clean, narrow diff will clear the guard.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
from collections.abc import Callable
from pathlib import Path
from urllib.parse import quote
from urllib.request import Request, urlopen


PROTECTED_PATHS = (
    ".gitea/workflows/",
    ".gitea/scripts/",
    "tests/e2e/",
    "workspace-server/internal/handlers/",
    "workspace-server/internal/provisioner/",
    "workspace-server/internal/middleware/",
    "canvas/src/",
)

DEFAULT_MAX_CHANGED_FILES = int(os.environ.get("DIFFGUARD_MAX_CHANGED_FILES", "100"))
DEFAULT_MAX_DELETIONS = int(os.environ.get("DIFFGUARD_MAX_DELETIONS", "5000"))
DEFAULT_MAX_INSERTIONS = int(os.environ.get("DIFFGUARD_MAX_INSERTIONS", "10000"))
PROTECTED_DELETION_OVERRIDE_LABEL = "diff-guard:pm-approved"
PM_APPROVERS_PATH = Path(__file__).parents[1] / "diff-guard-pm-approvers.txt"


def load_pm_approvers(path: str | os.PathLike[str]) -> set[str]:
    """Load the base-branch allowlist of human Gitea logins."""
    with open(path, encoding="utf-8") as approvers_file:
        return {
            line.casefold()
            for raw_line in approvers_file
            if (line := raw_line.strip()) and not line.startswith("#")
        }


def fetch_pr_timeline(event: dict[str, object]) -> list[dict[str, object]]:
    """Fetch the trusted issue timeline used to identify the label actor."""
    server_url = os.environ.get("GITEA_SERVER_URL", "").rstrip("/")
    token = os.environ.get("GITEA_TOKEN", "")
    repository = event.get("repository", {})
    pull_request = event.get("pull_request", {})
    if not isinstance(repository, dict) or not isinstance(pull_request, dict):
        raise ValueError("event is missing repository or pull_request metadata")

    full_name = repository.get("full_name") or os.environ.get("GITHUB_REPOSITORY", "")
    pr_number = pull_request.get("number") or event.get("number")
    if not server_url or not token or not isinstance(full_name, str) or not pr_number:
        raise ValueError("timeline API configuration is incomplete")

    try:
        owner, repo = full_name.split("/", 1)
        pr_number = int(pr_number)
    except (TypeError, ValueError) as exc:
        raise ValueError("invalid repository or pull request metadata") from exc

    timeline: list[dict[str, object]] = []
    for page in range(1, 21):
        url = (
            f"{server_url}/api/v1/repos/{quote(owner, safe='')}/"
            f"{quote(repo, safe='')}/issues/{pr_number}/timeline?limit=100&page={page}"
        )
        request = Request(
            url,
            headers={
                "Authorization": f"token {token}",
                "Accept": "application/json",
                "User-Agent": "curl/8.4.0",
            },
        )
        with urlopen(request, timeout=10) as response:  # noqa: S310 - fixed Gitea host
            page_items = json.load(response)
        if not isinstance(page_items, list):
            raise ValueError("timeline API returned a non-list response")
        if not all(isinstance(item, dict) for item in page_items):
            raise ValueError("timeline API returned a malformed event")
        timeline.extend(page_items)
        if len(page_items) < 100:
            return timeline

    raise ValueError("timeline API exceeded the 2,000-event safety bound")


def protected_deletion_override_active(
    event_path: str | os.PathLike[str] | None = None,
    *,
    timeline_fetcher: Callable[
        [dict[str, object]], list[dict[str, object]]
    ] = fetch_pr_timeline,
    approvers_path: str | os.PathLike[str] = PM_APPROVERS_PATH,
) -> bool:
    """Return whether an authorized PM most recently applied the override."""
    raw_path = (
        os.fspath(event_path)
        if event_path is not None
        else os.environ.get("GITHUB_EVENT_PATH", "")
    )
    if not raw_path:
        return False

    try:
        with open(raw_path, encoding="utf-8") as event_file:
            event = json.load(event_file)
    except (OSError, json.JSONDecodeError, TypeError) as exc:
        print(
            "::warning::could not read diff-guard event labels; "
            f"failing closed: {exc}"
        )
        return False

    if not isinstance(event, dict):
        print("::warning::diff-guard event is not an object; failing closed")
        return False

    pull_request = event.get("pull_request", {})
    if not isinstance(pull_request, dict):
        print("::warning::diff-guard event has no pull request; failing closed")
        return False
    labels = pull_request.get("labels", [])
    label_present = isinstance(labels, list) and any(
        isinstance(label, dict)
        and label.get("name") == PROTECTED_DELETION_OVERRIDE_LABEL
        for label in labels
    )
    if not label_present:
        return False

    try:
        authorized_actors = load_pm_approvers(approvers_path)
    except OSError as exc:
        print(f"::warning::could not read PM approver policy; failing closed: {exc}")
        return False
    if not authorized_actors:
        print("::warning::PM approver policy is empty; protected-path override fails closed")
        return False

    try:
        timeline = timeline_fetcher(event)
    except (OSError, ValueError, TypeError, json.JSONDecodeError) as exc:
        print(f"::warning::could not verify diff-guard label actor; failing closed: {exc}")
        return False
    if not isinstance(timeline, list):
        print("::warning::diff-guard timeline is malformed; failing closed")
        return False

    label_events = [
        item
        for item in timeline
        if isinstance(item, dict)
        and isinstance(item.get("label"), dict)
        and item["label"].get("name") == PROTECTED_DELETION_OVERRIDE_LABEL
        and item.get("type") in {"label", "unlabel"}
    ]
    if not label_events or not all(isinstance(item.get("id"), int) for item in label_events):
        print("::warning::no valid diff-guard label event found; failing closed")
        return False

    latest = max(label_events, key=lambda item: item["id"])
    if latest.get("type") != "label":
        print("::warning::latest diff-guard label event is removal; failing closed")
        return False

    actor = latest.get("user", {})
    login = actor.get("login", "") if isinstance(actor, dict) else ""
    if not isinstance(login, str) or login.casefold() not in authorized_actors:
        print(
            "::warning::diff-guard label was not applied by an authorized PM; "
            "failing closed"
        )
        return False

    print(f"::notice::verified {PROTECTED_DELETION_OVERRIDE_LABEL} actor: {login}")
    return True


def git(*args: str) -> str:
    result = subprocess.run(
        ["git", *args],
        capture_output=True,
        text=True,
        check=True,
    )
    return result.stdout


def main() -> int:
    base_ref = os.environ.get("PR_BASE_REF", os.environ.get("GITHUB_BASE_REF", "main"))
    head_sha = os.environ.get("PR_HEAD_SHA", os.environ.get("GITHUB_SHA", ""))

    if not head_sha:
        # In a pull_request workflow, GITHUB_SHA is the merge commit. Use the
        # PR head ref instead when available.
        head_sha = os.environ.get("GITHUB_EVENT_PULL_REQUEST_HEAD_SHA", "HEAD")
    if not head_sha:
        head_sha = "HEAD"

    # Ensure base ref is available.
    try:
        git("rev-parse", f"origin/{base_ref}")
    except subprocess.CalledProcessError:
        git("fetch", "origin", base_ref)

    # Find merge base so the diff reflects only what the PR added, not main
    # drift since the branch was created. If no merge base exists (e.g.,
    # unrelated-history branch), fall back to the base ref itself — the guard
    # should still catch a massive destructive diff.
    try:
        merge_base = git("merge-base", f"origin/{base_ref}", head_sha).strip()
    except subprocess.CalledProcessError:
        print(
            f"::warning::no merge base with origin/{base_ref}; "
            "falling back to direct diff"
        )
        merge_base = f"origin/{base_ref}"

    # Diff stat.
    numstat = git("diff", "--numstat", f"{merge_base}..{head_sha}").strip()
    changed_files = 0
    insertions = 0
    deletions = 0
    for line in numstat.splitlines():
        parts = line.split()
        if len(parts) < 3:
            continue
        add, rem = parts[0], parts[1]
        if add == "-" or rem == "-":
            continue  # binary
        insertions += int(add)
        deletions += int(rem)
        changed_files += 1

    # Deleted files and protected-path deletions.
    name_status = git("diff", "--name-status", f"{merge_base}..{head_sha}").strip()
    deleted_files: list[str] = []
    protected_deletions: list[str] = []
    for line in name_status.splitlines():
        if not line:
            continue
        status, path = line.split("\t", 1)
        if status.startswith("D"):
            deleted_files.append(path)
            if any(path.startswith(p) for p in PROTECTED_PATHS):
                protected_deletions.append(path)

    # Evaluate thresholds.
    failures: list[str] = []
    if changed_files > DEFAULT_MAX_CHANGED_FILES:
        failures.append(
            f"changed files ({changed_files}) exceeds threshold ({DEFAULT_MAX_CHANGED_FILES})"
        )
    if insertions > DEFAULT_MAX_INSERTIONS:
        failures.append(
            f"insertions (+{insertions}) exceeds threshold ({DEFAULT_MAX_INSERTIONS})"
        )
    if deletions > DEFAULT_MAX_DELETIONS:
        failures.append(
            f"deletions (-{deletions}) exceeds threshold ({DEFAULT_MAX_DELETIONS})"
        )
    protected_deletion_override = protected_deletion_override_active()
    if protected_deletions and not protected_deletion_override:
        failures.append(
            f"deleted {len(protected_deletions)} protected path(s): "
            + ", ".join(protected_deletions[:10])
        )

    # Report.
    print(f"Diff guard: {changed_files} files changed, +{insertions}/-{deletions} lines")
    print(f"Deleted files: {len(deleted_files)}")
    if protected_deletions:
        print(f"Protected-path deletions: {len(protected_deletions)}")
    if protected_deletions and protected_deletion_override:
        print(
            f"::notice::protected-path deletion check overridden by "
            f"{PROTECTED_DELETION_OVERRIDE_LABEL}; size thresholds remain enforced"
        )

    if failures:
        print("::error::PR diff guard failed:")
        for f in failures:
            print(f"  - {f}")
        print(
            "If this diff is intentional, split the PR or ask a PM to apply "
            f"the {PROTECTED_DELETION_OVERRIDE_LABEL} label."
        )
        return 1

    print("Diff guard passed.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
