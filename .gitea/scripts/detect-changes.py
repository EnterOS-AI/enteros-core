#!/usr/bin/env python3
"""Shared path-filter helper for Gitea Actions workflows.

Computes changed files against the PR base SHA or push-before SHA and writes
boolean outputs to GITHUB_OUTPUT. If the diff base is missing or untrusted, the
helper fails open by setting every output in the selected profile to true.
"""

from __future__ import annotations

import argparse
import os
import re
import subprocess
import sys
from pathlib import Path


PROFILES: dict[str, dict[str, str]] = {
    "ci": {
        "platform": r"^workspace-server/",
        "canvas": r"^canvas/",
        "python": r"^workspace/",
        "scripts": r"^tests/e2e/|^scripts/|^infra/scripts/",
    },
    "handlers-postgres": {
        "handlers": (
            r"^workspace-server/internal/handlers/"
            r"|^workspace-server/internal/wsauth/"
            r"|^workspace-server/migrations/"
            r"|^\.gitea/workflows/handlers-postgres-integration\.yml$"
        ),
    },
    "e2e-api": {
        "api": r"^workspace-server/|^tests/e2e/|^\.gitea/workflows/e2e-api\.yml$",
    },
}


def classify(profile: str, paths: list[str]) -> dict[str, bool]:
    patterns = PROFILES[profile]
    return {
        name: any(re.search(pattern, path) for path in paths)
        for name, pattern in patterns.items()
    }


def all_true(profile: str) -> dict[str, bool]:
    return {name: True for name in PROFILES[profile]}


def resolve_base(event_name: str, pr_base_sha: str, push_before: str) -> str:
    if event_name == "pull_request" and pr_base_sha:
        return pr_base_sha
    return push_before


def is_zero_sha(value: str) -> bool:
    return not value or bool(re.fullmatch(r"0+", value))


def run_git(args: list[str], *, timeout: int = 30) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        ["git", *args],
        check=False,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        timeout=timeout,
    )


def base_exists(base: str) -> bool:
    return run_git(["cat-file", "-e", base]).returncode == 0


def fetch_base(base: str) -> None:
    run_git(["fetch", "--depth=1", "origin", base])


def changed_paths(base: str) -> list[str] | None:
    proc = run_git(["diff", "--name-only", base, "HEAD"])
    if proc.returncode != 0:
        return None
    return [line for line in proc.stdout.splitlines() if line]


def write_outputs(values: dict[str, bool], output_path: str | None) -> None:
    lines = [f"{name}={'true' if value else 'false'}" for name, value in values.items()]
    if output_path:
        with Path(output_path).open("a", encoding="utf-8") as fh:
            for line in lines:
                fh.write(line + "\n")
    else:
        for line in lines:
            print(line)


def detect(profile: str, event_name: str, pr_base_sha: str, push_before: str) -> dict[str, bool]:
    base = resolve_base(event_name, pr_base_sha, push_before)
    if is_zero_sha(base):
        return all_true(profile)

    if not base_exists(base):
        fetch_base(base)
    if not base_exists(base):
        return all_true(profile)

    paths = changed_paths(base)
    if paths is None:
        return all_true(profile)
    return classify(profile, paths)


def parse_args(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--profile", required=True, choices=sorted(PROFILES))
    parser.add_argument("--event-name", default=os.environ.get("GITHUB_EVENT_NAME", ""))
    parser.add_argument("--pr-base-sha", default="")
    parser.add_argument("--push-before", default=os.environ.get("GITHUB_EVENT_BEFORE", ""))
    return parser.parse_args(argv)


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    values = detect(args.profile, args.event_name, args.pr_base_sha, args.push_before)
    write_outputs(values, os.environ.get("GITHUB_OUTPUT"))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
