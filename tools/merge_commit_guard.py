#!/usr/bin/env python3
"""Post-merge guard for core#2641.

The Gitea merge queue has been observed landing multi-commit PRs at their
first commit only, silently dropping later commits. This script runs on every
push to main, inspects the merge commits in that push, and verifies that the
PR's HEAD commit is an ancestor of the new main tip. If it is not, the merge
queue dropped at least one commit and we fail loudly.

Environment:
    GITEA_TOKEN          token with repo read scope
    GITHUB_REPOSITORY    owner/repo (e.g. molecule-ai/molecule-core)
    GITHUB_SERVER_URL    Gitea base URL (default https://git.moleculesai.app)
    GITHUB_EVENT_BEFORE  pre-push SHA (github.event.before)
    GITHUB_EVENT_AFTER   post-push SHA (github.event.after)
"""

import json
import os
import re
import subprocess
import sys
import urllib.error
import urllib.request

DEFAULT_SERVER = "https://git.moleculesai.app"

# Cloudflare fronts git.moleculesai.app and its bot-protection returns 403 for
# the default "Python-urllib/x.y" User-Agent that urllib.request sends. That 403
# has nothing to do with the token's scope: it fired on every PR-head fetch and
# tripped the fail-closed api_failures path, so this guard reported a false
# "could not verify PR #N: 403" on main even when no commit was dropped (the PR
# head WAS an ancestor). Sending an explicit, non-blocked User-Agent -- the same
# way the curl-based gate scripts already reach the API -- lets the request
# through. (core#2641 follow-up.)
USER_AGENT = "molecule-merge-commit-guard/1.0"


def pr_number_from_message(message: str) -> int | None:
    """Extract the PR number from a Gitea merge commit message.

    Handles:
      - Gitea merge queue: "Merge PR #3050 via Gitea merge queue"
      - Regular merge:    "Merge pull request '#3050' (#3050) from ..."
      - Fallback:         "... (#3050)" at end of subject line
    """
    patterns = [
        r"Merge PR #(\d+)",
        r"Merge pull request ['\"]#?(\d+)['\"]",
        r"\(#(\d+)\)\s*$",
    ]
    for pattern in patterns:
        match = re.search(pattern, message, re.MULTILINE)
        if match:
            return int(match.group(1))
    return None


def git(*args: str, check: bool = True) -> subprocess.CompletedProcess[str]:
    result = subprocess.run(
        ["git", *args],
        capture_output=True,
        text=True,
        check=False,
    )
    if check and result.returncode != 0:
        sys.stderr.write(result.stderr)
        result.check_returncode()
    return result


def pushed_commits(before: str, after: str) -> list[str]:
    if before == "0000000000000000000000000000000000000000":
        # Branch creation; guard only the new tip.
        return [after]
    result = git("rev-list", f"{before}..{after}")
    return result.stdout.strip().splitlines()


def is_merge_commit(sha: str) -> bool:
    parents = git("rev-list", "--parents", "-n", "1", sha).stdout.strip().split()
    return len(parents) == 3  # sha + two parents


def fetch_pr_head(pr_number: int) -> None:
    """Fetch the PR head ref so git can resolve it for ancestry checks."""
    ref = f"refs/pull/{pr_number}/head"
    # Fetch may fail if the PR branch was deleted; in that case the head SHA
    # might still be reachable through the merge commit if the merge was
    # complete. A missing ref is not itself a failure.
    subprocess.run(
        ["git", "fetch", "--depth=1", "origin", f"{ref}:refs/remotes/origin/pr/{pr_number}/head"],
        capture_output=True,
    )


def pr_head_sha(pr_number: int, repo: str, token: str, server: str) -> str:
    url = f"{server}/api/v1/repos/{repo}/pulls/{pr_number}"
    req = urllib.request.Request(
        url,
        headers={
            "Authorization": f"token {token}",
            "Accept": "application/json",
            # Avoid the default "Python-urllib/x.y" UA, which Cloudflare 403s
            # (see USER_AGENT note above).
            "User-Agent": USER_AGENT,
        },
    )
    with urllib.request.urlopen(req, timeout=30) as response:
        data = json.load(response)
    return str(data["head"]["sha"])


def is_ancestor(ancestor: str, descendant: str) -> bool:
    return (
        subprocess.run(
            ["git", "merge-base", "--is-ancestor", ancestor, descendant],
            capture_output=True,
        ).returncode
        == 0
    )


def main() -> int:
    token = os.environ["GITEA_TOKEN"]
    repo = os.environ["GITHUB_REPOSITORY"]
    server = os.environ.get("GITHUB_SERVER_URL", DEFAULT_SERVER)
    before = os.environ.get("GITHUB_EVENT_BEFORE", "")
    after = os.environ.get("GITHUB_EVENT_AFTER", "")

    if not before or not after:
        print("merge-commit-guard: missing before/after SHAs; nothing to check.")
        return 0

    failures: list[tuple[str, int, str]] = []
    api_failures: list[tuple[str, int, str]] = []
    skipped = 0

    for sha in pushed_commits(before, after):
        if not is_merge_commit(sha):
            continue

        message = git("log", "-1", "--format=%B", sha).stdout
        pr_number = pr_number_from_message(message)
        if pr_number is None:
            skipped += 1
            continue

        try:
            head_sha = pr_head_sha(pr_number, repo, token, server)
        except urllib.error.URLError as exc:
            # CR2 #14649: fail-closed. If we cannot fetch the PR head, we
            # cannot prove the merge queue did not drop commits. Skipping the
            # check would make the guard fail-open.
            code = getattr(exc, "code", "network error")
            api_failures.append((sha, pr_number, str(code)))
            continue

        fetch_pr_head(pr_number)

        if not is_ancestor(head_sha, after):
            failures.append((sha, pr_number, head_sha))

    if api_failures:
        print(
            "ERROR: merge-commit-guard could not verify these merged PRs:",
            file=sys.stderr,
        )
        for sha, pr_number, reason in api_failures:
            print(
                f"  - commit {sha} PR #{pr_number}: {reason}",
                file=sys.stderr,
            )
        print(
            "Refusing to pass while PR heads cannot be fetched. "
            "See core#2641.",
            file=sys.stderr,
        )
        return 1

    if failures:
        print("ERROR: merge queue appears to have dropped commits from these PRs:")
        for sha, pr_number, head_sha in failures:
            print(
                f"  - commit {sha} PR #{pr_number}: "
                f"PR head {head_sha} is not an ancestor of {after}"
            )
        print(
            "See core#2641. The merge queue landed the PR at an earlier commit; "
            "later commits were silently dropped."
        )
        return 1

    print(
        f"merge-commit-guard: OK. "
        f"Checked {len(pushed_commits(before, after))} pushed commit(s), "
        f"skipped {skipped} unparsable merge commit(s)."
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
