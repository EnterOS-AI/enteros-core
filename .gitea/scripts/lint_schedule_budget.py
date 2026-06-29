#!/usr/bin/env python3
"""lint_schedule_budget — the zero-cron ratchet for molecule-core CI.

The principal's rule: NO nightly/scheduled e2e (or any) CI. Every test and
gate runs PER-PR and/or as the prod-merge gate (push:main/staging) — never
on a timer. Scheduled runs are how breakage reaches prod uncaught: nobody
blocks on a nightly, so a red schedule sits unwatched while a green PR-gate
would have frozen the merge.

This lint fails the build if ANY `.gitea/workflows/*.yml` declares an ACTIVE
`schedule:` trigger (i.e. an `on.schedule` with one or more `cron:` entries).
It is the ratchet that keeps the repo at zero scheduled workflows after the
schedule-strip sweeps (#3344 + the staging-e2e consolidation).

Robustness notes:
  * It parses YAML, so commented-out `# schedule:` / `# - cron:` blocks (the
    disabled-trigger cruft left by the 2026-06-27 gitea-disable) do NOT trip
    it — only a live, parser-visible trigger does. That is intentional: the
    rule is about what actually fires, not about the word "cron" appearing in
    a comment or a script (e.g. "the 30-min cron" prose, schedules_handler
    tests, etc.).
  * GitHub/Gitea Actions parse the top-level key `on:` — but YAML 1.1 (PyYAML)
    loads the bare token `on` as the boolean `True`. We look the mapping up
    under both keys so the check is not silently defeated by that quirk.

Usage:
  python3 .gitea/scripts/lint_schedule_budget.py
      Lint the default `.gitea/workflows` dir. Exit 1 on any violation.

  python3 .gitea/scripts/lint_schedule_budget.py --workflow-dir <path>
      Lint a custom directory (used by tests/test_lint_schedule_budget.py).
"""

from __future__ import annotations

import argparse
import glob
import os
import sys
from typing import Any

import yaml  # PyYAML — installed by the workflow before this runs.


def _on_block(doc: Any) -> Any:
    """Return the `on:` mapping/list/str for a parsed workflow doc.

    YAML 1.1 loads the bare key `on:` as the boolean True, so check both.
    """
    if not isinstance(doc, dict):
        return None
    if "on" in doc:
        return doc["on"]
    if True in doc:
        return doc[True]
    return None


def has_active_schedule(doc: Any) -> bool:
    """True iff the workflow declares a live `schedule:` trigger."""
    on = _on_block(doc)
    if isinstance(on, dict):
        sched = on.get("schedule")
        # A non-empty schedule (list of {cron: ...}) is a live trigger.
        return bool(sched)
    if isinstance(on, list):
        return "schedule" in on
    if isinstance(on, str):
        return on == "schedule"
    return False


def find_violations(workflow_dir: str) -> list[str]:
    offenders: list[str] = []
    patterns = (
        os.path.join(workflow_dir, "*.yml"),
        os.path.join(workflow_dir, "*.yaml"),
    )
    paths = sorted({p for pat in patterns for p in glob.glob(pat)})
    for path in paths:
        try:
            with open(path, encoding="utf-8") as fh:
                doc = yaml.safe_load(fh)
        except yaml.YAMLError as exc:  # pragma: no cover - surfaced loudly
            print(f"::error file={path}::could not parse workflow YAML: {exc}")
            offenders.append(path)
            continue
        if has_active_schedule(doc):
            offenders.append(path)
    return offenders


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--workflow-dir",
        default=".gitea/workflows",
        help="directory of workflow YAML files to lint",
    )
    args = parser.parse_args(argv)

    if not os.path.isdir(args.workflow_dir):
        print(f"::error::workflow dir not found: {args.workflow_dir}")
        return 2

    offenders = find_violations(args.workflow_dir)
    if offenders:
        print(
            "::error::zero-cron ratchet FAILED — these workflows declare an "
            "active schedule:/cron: trigger, which the no-nightly-CI rule "
            "forbids. Replace the schedule with pull_request + push:main/"
            "staging + workflow_dispatch, or retire the workflow:"
        )
        for path in offenders:
            print(f"  - {path}")
        return 1

    n = len(
        sorted(
            {
                p
                for pat in ("*.yml", "*.yaml")
                for p in glob.glob(os.path.join(args.workflow_dir, pat))
            }
        )
    )
    print(f"zero-cron ratchet OK — 0 active schedule triggers across {n} workflows.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
