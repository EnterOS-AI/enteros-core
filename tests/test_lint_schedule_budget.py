"""Tests for `.gitea/scripts/lint_schedule_budget.py` — the zero-cron ratchet.

The no-nightly-CI rule: no workflow may declare a live `schedule:`/`cron:`
trigger. Positive (active schedule -> exit 1) + negative (clean -> exit 0)
cases, plus the two robustness properties that matter:

  * commented-out `# schedule:` cruft must NOT trip the ratchet (the rule is
    about what actually fires, not the word "cron" in a comment);
  * the YAML 1.1 `on:` -> boolean-True key quirk must not silently defeat it.

Run:
    python3 -m pytest tests/test_lint_schedule_budget.py -v

Dependencies: stdlib + PyYAML. No network.
"""
from __future__ import annotations

import subprocess
import sys
import textwrap
from pathlib import Path

import pytest  # noqa: F401  (declares the dep)

REPO_ROOT = Path(__file__).resolve().parents[1]
SCRIPT = REPO_ROOT / ".gitea" / "scripts" / "lint_schedule_budget.py"


def _run(workflow_dir: Path) -> subprocess.CompletedProcess:
    return subprocess.run(
        [sys.executable, str(SCRIPT), "--workflow-dir", str(workflow_dir)],
        capture_output=True,
        text=True,
    )


def _write(workflow_dir: Path, name: str, content: str) -> Path:
    workflow_dir.mkdir(parents=True, exist_ok=True)
    p = workflow_dir / name
    p.write_text(textwrap.dedent(content).lstrip())
    return p


CLEAN = """
    name: clean-per-pr-gate
    on:
      pull_request:
        branches: [main, staging]
      push:
        branches: [main, staging]
      workflow_dispatch:
    jobs:
      x:
        runs-on: ubuntu-latest
        steps:
          - run: echo ok
"""

ACTIVE_SCHEDULE = """
    name: bad-nightly
    on:
      pull_request:
        branches: [main]
      schedule:
        - cron: '0 7 * * *'
    jobs:
      x:
        runs-on: ubuntu-latest
        steps:
          - run: echo nope
"""

# schedule trigger commented out (the 2026-06-27 gitea-disable cruft shape)
COMMENTED_SCHEDULE = """
    name: disabled-but-clean
    on:
      # DISABLED: schedule/cron removed; manual only.
      # schedule:
      #   - cron: '0 7 * * *'
      workflow_dispatch:
    jobs:
      x:
        runs-on: ubuntu-latest
        steps:
          - run: echo manual-only
"""

# `on:` parses as boolean True under YAML 1.1 — the lint must still see it.
LIST_FORM_SCHEDULE = """
    name: bad-list-form
    on: [push, schedule]
    jobs:
      x:
        runs-on: ubuntu-latest
        steps:
          - run: echo nope
"""


def test_clean_workflow_passes(tmp_path):
    _write(tmp_path, "clean.yml", CLEAN)
    r = _run(tmp_path)
    assert r.returncode == 0, r.stdout + r.stderr
    assert "OK" in r.stdout


def test_active_schedule_fails(tmp_path):
    _write(tmp_path, "bad.yml", ACTIVE_SCHEDULE)
    r = _run(tmp_path)
    assert r.returncode == 1, r.stdout + r.stderr
    assert "bad.yml" in r.stdout


def test_commented_schedule_passes(tmp_path):
    """Commented-out schedule cruft must NOT be flagged — only live triggers."""
    _write(tmp_path, "disabled.yml", COMMENTED_SCHEDULE)
    r = _run(tmp_path)
    assert r.returncode == 0, r.stdout + r.stderr


def test_list_form_schedule_fails(tmp_path):
    """`on: [push, schedule]` (with the YAML on->True quirk) is still caught."""
    _write(tmp_path, "listform.yml", LIST_FORM_SCHEDULE)
    r = _run(tmp_path)
    assert r.returncode == 1, r.stdout + r.stderr
    assert "listform.yml" in r.stdout


def test_aggregates_multiple_offenders(tmp_path):
    _write(tmp_path, "ok.yml", CLEAN)
    _write(tmp_path, "bad1.yml", ACTIVE_SCHEDULE)
    _write(tmp_path, "bad2.yml", LIST_FORM_SCHEDULE)
    r = _run(tmp_path)
    assert r.returncode == 1, r.stdout + r.stderr
    assert "bad1.yml" in r.stdout
    assert "bad2.yml" in r.stdout
    assert "ok.yml" not in r.stdout
