#!/usr/bin/env python3
"""lint_env_coupling_dismissal — mechanize the no-flaky / env-coupling SOP.

Operator principle (docs/engineering/testing-strategy.md
§"No flaky, no environmental: every CI/e2e red is a deterministic coupling
bug"): there is NO "flaky" and NO "environmental." Every CI/e2e red is a
DETERMINISTIC coupling bug (one of five classes: A missing-input / B race /
C false-ready / D shared-live-state / E near-capacity-fixed-timeout). A test
that depends on the environment being fast/quiet/unchanging/perfectly-timed is
broken — fix the coupling, never tolerate the variance.

This lint makes two slices of that principle mechanical at PR time.

CHECK 1 — banned-dismissal in COE / incident / postmortem docs
--------------------------------------------------------------
A retro/COE/incident/postmortem markdown doc that dismisses a failure as
"flaky" / "environmental" (and friends) MUST also state a deterministic root
cause. Dismissing a red as flaky/environmental without a named root cause +
fix is banned (a review/COE must never wave a red away). Scanned files: any
markdown whose path OR name contains `incident`, `postmortem`, or `coe`.

CHECK 2 — fixed sleep in a NEW/changed e2e wait
-----------------------------------------------
An ADDED line in a changed e2e file that introduces a fixed
sleep/`setTimeout`/`time.Sleep` as a readiness/settle wait must be paired with
a real-signal poll (or carry an explicit escape marker). A fixed sleep near
capacity is class-E coupling: green on a quiet box, red under load. The success
path must poll the REAL signal and proceed instantly; a fixed deadline is only
legitimate as a 10x safety net you never wait out. Scanned files: changed files
whose path OR name contains `e2e` (the added lines come from the git diff).

Escape hatch
------------
A genuinely-intended wait / a correctly-labelled retro can suppress a finding
with an inline `lint-allow: env-coupling` marker (on the offending line for
CHECK 2, anywhere in the doc for CHECK 1). Use sparingly; it is greppable.

Inputs
------
- CHANGED_FILES (optional, newline-separated): the PR's changed paths. When
  absent the lint shells out to `git diff --name-only <BASE>...HEAD`.
- BASE_REF / BASE_SHA (optional): the merge base to diff against (default
  origin/main; falls back to HEAD~1). Added lines for CHECK 2 come from
  `git diff --unified=0`.

The scanners (scan_doc_text / scan_added_lines) are pure functions with no
network or git dependency — see tests/test_lint_env_coupling_dismissal.py.

Exit codes: 0 = clean, 1 = violation(s), 2 = internal/usage error.
"""
from __future__ import annotations

import os
import re
import subprocess
import sys

ALLOW_MARKER = "lint-allow: env-coupling"

# --- CHECK 1 vocabulary -----------------------------------------------------

# Dismissal phrases: calling a failure flaky/environmental/transient/etc.
# Word-boundaried, case-insensitive. Kept tight to avoid false positives on
# ordinary prose ("the environment variable", "racing to fix").
_DISMISSAL = re.compile(
    r"""
    \b(
        flak(?:y|ey|iness)
      | environmental
      | env(?:ironment)?[ -]?(?:issue|blip|hiccup|glitch|flake|noise)
      | transient[ -](?:failure|error|env|blip)
      | spurious[ -](?:failure|red)
      | intermittent[ -](?:failure|red)
      | non[- ]?deterministic[ -](?:test|failure|red)
      | not[ -]repro(?:ducible)?
      | works[ -]on[ -]retry
      | passed[ -]on[ -]re[- ]?run
    )\b
    """,
    re.IGNORECASE | re.VERBOSE,
)

# Deterministic-root-cause markers. Presence of ANY one in the same doc means
# the author engaged with the root cause rather than merely dismissing.
_ROOTCAUSE = re.compile(
    r"""
    (
        root[ -]?cause
      | deterministic (?:bug|coupling|cause)
      | coupling (?:bug|class)
      | missing[ -]input
      | false[ -]ready
      | shared[ -]live[ -]state
      | near[ -]capacity
      | race (?:condition|window)
      | class\s+[A-E]\b
      | \bclass[ -][A-E]\b
    )
    """,
    re.IGNORECASE | re.VERBOSE,
)

_DOC_NAME = re.compile(r"(incident|postmortem|post-mortem|\bcoe\b|coe[-_])", re.IGNORECASE)

# --- CHECK 2 patterns -------------------------------------------------------

# A fixed sleep / timer used as a wait. Captures the common shapes across
# Go / Python / JS / shell.
_FIXED_SLEEP = re.compile(
    r"""
    (
        time\.Sleep\s*\(                 # Go
      | time\.sleep\s*\(                 # Python
      | asyncio\.sleep\s*\(              # Python async
      | \bsetTimeout\s*\(               # JS
      | await\s+sleep\s*\(              # JS/TS helper
      | \bsleep\s+[0-9]                  # shell `sleep 5`
    )
    """,
    re.VERBOSE,
)

# Real-signal poll markers — presence in the same file means the fixed sleep is
# (plausibly) part of a poll loop rather than a bare settle wait.
_POLL_MARKER = re.compile(
    r"""
    (
        poll
      | wait[_ ]?for
      | waitFor
      | \buntil\b
      | eventually
      | assert[_ ]?eventually
      | require\.Eventually
      | \bretry\b
      | backoff
      | deadline
    )
    """,
    re.IGNORECASE | re.VERBOSE,
)

_E2E_PATH = re.compile(r"e2e", re.IGNORECASE)


def scan_doc_text(text: str) -> list[str]:
    """Return the dismissal phrases used WITHOUT a root-cause marker.

    Empty list = clean. A doc that contains the allow-marker is always clean.
    A doc that contains a root-cause marker is clean (the author engaged with
    the root cause). Otherwise every distinct dismissal phrase is a violation.
    """
    if ALLOW_MARKER in text:
        return []
    hits = {m.group(0).strip().lower() for m in _DISMISSAL.finditer(text)}
    if not hits:
        return []
    if _ROOTCAUSE.search(text):
        return []
    return sorted(hits)


def scan_added_lines(path: str, added_lines: list[str]) -> list[str]:
    """Return the added e2e lines that introduce a fixed sleep with no poll.

    A line is a violation when it introduces a fixed sleep AND neither the
    line itself carries the allow-marker NOR any added line in the same file
    contains a real-signal poll marker. Non-e2e paths are never flagged.
    """
    if not _E2E_PATH.search(path):
        return []
    # A poll signal must come from a NON-sleep line — otherwise a "// wait for
    # the box to redeploy" comment on the bare-sleep line itself would mask the
    # very violation we are looking for.
    file_has_poll = any(
        _POLL_MARKER.search(ln) for ln in added_lines if not _FIXED_SLEEP.search(ln)
    )
    out = []
    for ln in added_lines:
        if not _FIXED_SLEEP.search(ln):
            continue
        if ALLOW_MARKER in ln:
            continue
        if file_has_poll:
            continue
        out.append(ln.strip())
    return out


# --- git plumbing (impure; not exercised by unit tests) ---------------------


def _run(cmd: list[str]) -> str:
    return subprocess.run(cmd, capture_output=True, text=True, check=False).stdout


def _base_ref() -> str:
    ref = os.environ.get("BASE_SHA") or os.environ.get("BASE_REF") or ""
    if ref:
        return ref
    # Prefer origin/main; fall back to the previous commit.
    if _run(["git", "rev-parse", "--verify", "--quiet", "origin/main"]).strip():
        return "origin/main"
    return "HEAD~1"


def _changed_files(base: str) -> list[str]:
    env = os.environ.get("CHANGED_FILES", "")
    if env.strip():
        return [ln.strip() for ln in env.splitlines() if ln.strip()]
    out = _run(["git", "diff", "--name-only", f"{base}...HEAD"])
    return [ln.strip() for ln in out.splitlines() if ln.strip()]


def _added_lines(base: str, path: str) -> list[str]:
    out = _run(["git", "diff", "--unified=0", f"{base}...HEAD", "--", path])
    added = []
    for ln in out.splitlines():
        if ln.startswith("+") and not ln.startswith("+++"):
            added.append(ln[1:])
    return added


def _read(path: str) -> str:
    try:
        with open(path, encoding="utf-8", errors="replace") as f:
            return f.read()
    except OSError:
        return ""


def main() -> int:
    base = _base_ref()
    changed = _changed_files(base)

    doc_fails: list[tuple[str, list[str]]] = []
    sleep_fails: list[tuple[str, list[str]]] = []

    for path in changed:
        low = path.lower()
        if low.endswith(".md") and _DOC_NAME.search(os.path.basename(low) or low) and os.path.isfile(path):
            v = scan_doc_text(_read(path))
            if v:
                doc_fails.append((path, v))
        if _E2E_PATH.search(low):
            added = _added_lines(base, path)
            v = scan_added_lines(path, added)
            if v:
                sleep_fails.append((path, v))

    if not doc_fails and not sleep_fails:
        print(f"OK: no flaky/environmental dismissal or fixed-sleep e2e coupling "
              f"in {len(changed)} changed file(s).")
        return 0

    if doc_fails:
        print("FAIL: a COE/incident/postmortem doc dismisses a red as "
              "flaky/environmental WITHOUT a deterministic root cause "
              "(docs/engineering/testing-strategy.md §No flaky, no environmental):")
        for path, phrases in doc_fails:
            print(f"  - {path}: {', '.join(phrases)}")
        print("  Name the coupling class (A missing-input / B race / C false-ready /")
        print("  D shared-live-state / E near-capacity-timeout) and the fix — or, if")
        print("  this is a correctly-labelled retro, add a `lint-allow: env-coupling`")
        print("  marker.")
    if sleep_fails:
        print("FAIL: a changed e2e file introduces a fixed sleep/timer with no "
              "real-signal poll (class-E near-capacity coupling):")
        for path, lines in sleep_fails:
            for ln in lines:
                print(f"  - {path}: {ln}")
        print("  Poll the REAL ready signal and proceed instantly; keep a fixed")
        print("  deadline only as a 10x safety net you never wait out. If this IS a")
        print("  poll/backoff loop, add a poll marker (poll/wait_for/until/Eventually)")
        print("  or a `lint-allow: env-coupling` marker on the line.")
    return 1


if __name__ == "__main__":
    sys.exit(main())
