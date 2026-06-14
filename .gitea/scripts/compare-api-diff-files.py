#!/usr/bin/env python3
"""Extract changed-file list from Gitea Compare API JSON response.

The Gitea Compare API (`/repos/{owner}/{repo}/compare/{base}...{head}`)
historically returned changed files nested inside each commit:
    {"commits": [{"files": [{"filename": "path/to/file"}]}]}

Newer Gitea versions (and the `...` branch-to-branch shape) ALSO
populate a top-level `files` array:
    {"files": [{"filename": "path/to/file"}], "commits": [...]}

This script handles BOTH shapes defensively: it checks the top-level
`files` first, then falls back to per-commit `files` extraction. This
matters because a regression that only checked one shape would silently
return an empty list and cause the harness-replays detect-changes step
to set `run=false` even on a PR that touches the path filter — a
false-green gate (the symptom that surfaced as core#2821 RC #11590 +
CR2 RC #11597 "detect-changes-actually-run").

SRE verification (2026-05-11, 751c98ce) saw `commits[0]['files']`
populated for the branch-to-branch Compare API. We preserve that
extraction path AND add the top-level `files` extraction so the
script doesn't break if a future Gitea version only populates one
of the two locations.

Usage:
    compare-api-diff-files.py < API_RESPONSE.json

Exits 0 with filenames on stdout, one per line (deduplicated, sorted).
Exits 1 on malformed input (caller treats as "no files").
"""
from __future__ import annotations

import sys
import json


def main() -> None:
    try:
        data = json.load(sys.stdin)
    except Exception:
        sys.exit(1)

    filenames: set[str] = set()

    # Path 1: top-level `files` (newer Gitea versions, and the
    # branch-to-branch `base...head` shape commonly used by detect-
    # changes in harness-replays.yml). Each entry is a file object
    # with at minimum a `filename` key.
    for f in (data.get("files") or []):
        if isinstance(f, dict):
            fn = f.get("filename", "") or f.get("new_path", "") or f.get("old_path", "")
            if fn:
                filenames.add(fn)
        elif isinstance(f, str) and f:
            # Some response shapes are just strings; accept those too.
            filenames.add(f)

    # Path 2: per-commit `files` (the shape documented at script
    # creation; still populated for at least the SRE-verified
    # branch-to-branch call). Only used as a fallback if Path 1
    # yielded nothing — if the top-level `files` had data, we trust
    # that and skip the per-commit walk to avoid double-listing the
    # same file across multiple commits.
    if not filenames:
        for commit in (data.get("commits") or []):
            if not isinstance(commit, dict):
                continue
            for f in (commit.get("files") or []):
                if isinstance(f, dict):
                    fn = f.get("filename", "") or f.get("new_path", "") or f.get("old_path", "")
                    if fn:
                        filenames.add(fn)
                elif isinstance(f, str) and f:
                    filenames.add(f)

    if filenames:
        sys.stdout.write("\n".join(sorted(filenames)))
        sys.stdout.write("\n")
    # else: empty stdout = no files, caller treats as empty list


if __name__ == "__main__":
    main()
