#!/usr/bin/env python3
"""A test run that executed nothing must never be read as a pass.

WHY THIS FILE EXISTS
--------------------
`go test` exits 0 when a package's tests stop compiling into the binary — a
build tag flipped, a file renamed, a `-run` regex that matches nothing, a
directory that moved. It also exits 0 when every test in a package calls
`t.Skip`. Both are green, and both executed no assertion at all.

`CI / Platform (Go)` already carried a non-vacuity check for this, inline in
the "Race + verbose per-package gate" step. It had two holes:

  1. It accepted ``--- SKIP:`` as proof of execution::

         grep -qE '^--- (PASS|FAIL|SKIP): ' /tmp/test-handlers.log

     A package in which every test skips satisfied that grep. That is
     precisely the state the check exists to detect, so the check passed on
     its own failure mode.

  2. It covered two packages — ``internal/handlers`` and
     ``internal/pendinguploads``. The other 59 packages in workspace-server
     had no such protection at all, including the ones whose tests are gated
     on an environment variable and are therefore one typo away from
     skipping wholesale in silence.

This script replaces the grep with a parse of ``go test -json`` and applies
the assertion to every package in the module.

WHAT COUNTS AS EXECUTION
------------------------
A top-level test that reports ``pass`` or ``fail``. A ``skip`` does not
count — that is the entire point. A package is EXECUTED if at least one of
its top-level tests passed or failed.

Three other package states are distinguished, because collapsing them is how
a gate ends up reporting success while checking nothing:

  NO_TEST_FILES   `go test` printed "[no test files]". The package has no
                  _test.go at all. Not a defect; counted and reported.
  ALL_SKIP        Tests exist and ran, and every one of them skipped. FAILS,
                  unless the package is named in the allowlist with a reason.
  NO_TEST_EVENTS  Tests were expected but the JSON stream carries no test
                  event for the package and Go did not say "[no test files]".
                  This is the original vacuous-pass shape. FAILS.

WHY THIS GATE CANNOT ITSELF PASS VACUOUSLY
------------------------------------------
A checker that greps a log which was never produced, or iterates a package
set that came out empty, reports success while inspecting nothing. Every
mechanism this script depends on is therefore asserted before it is used:

  * the JSON log must exist and be non-empty;
  * the expected-package list (``go list ./...``) must be non-empty;
  * BOTH must contain at least ``--min-packages`` entries. Comparing two
    empty sets succeeds — the floor is what makes the comparison mean
    something;
  * every expected package must appear in the JSON stream, so a package that
    silently vanished from the run is a failure rather than an absence;
  * at least one package must be EXECUTED.

The last one is the load-bearing line. Without it, a run in which every
package is NO_TEST_FILES would satisfy every other rule.

STALE ALLOWLIST ENTRIES FAIL
----------------------------
An allowlist that is only ever appended to stops describing reality. If an
allowlisted package is found to be executing tests, that is a failure telling
you to delete the line, not a silent pass. Same for an entry naming a package
that no longer exists.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
from collections import Counter, defaultdict

# A package-level output line Go emits for a directory with no _test.go files.
NO_TEST_FILES_MARKER = "[no test files]"

EXECUTED = "EXECUTED"
NO_TEST_FILES = "NO_TEST_FILES"
ALL_SKIP = "ALL_SKIP"
NO_TEST_EVENTS = "NO_TEST_EVENTS"

VERDICT_ACTIONS = ("pass", "fail", "skip")


class PackageResult:
    __slots__ = ("name", "top", "sub", "pkg_action", "no_test_files", "output")

    def __init__(self, name: str) -> None:
        self.name = name
        self.top: Counter = Counter()
        self.sub: Counter = Counter()
        self.pkg_action: str | None = None
        self.no_test_files = False
        self.output: list[str] = []

    @property
    def executed(self) -> int:
        """Top-level verdicts that prove a test body ran. SKIP is excluded."""
        return self.top["pass"] + self.top["fail"]

    @property
    def total_test_events(self) -> int:
        return sum(self.top.values()) + sum(self.sub.values())

    @property
    def state(self) -> str:
        if self.executed > 0:
            return EXECUTED
        if self.total_test_events > 0:
            return ALL_SKIP
        if self.no_test_files:
            return NO_TEST_FILES
        return NO_TEST_EVENTS


def parse_json_log(path: str) -> tuple[dict[str, PackageResult], list[str]]:
    """Parse a `go test -json` stream. Returns (packages, malformed_lines)."""
    packages: dict[str, PackageResult] = {}
    malformed: list[str] = []
    with open(path, "r", encoding="utf-8", errors="replace") as fh:
        for raw in fh:
            line = raw.strip()
            if not line:
                continue
            if not line.startswith("{"):
                # go emits build failures and toolchain errors as plain text.
                malformed.append(line)
                continue
            try:
                event = json.loads(line)
            except ValueError:
                malformed.append(line)
                continue
            pkg = event.get("Package")
            if not pkg:
                continue
            result = packages.get(pkg)
            if result is None:
                result = packages[pkg] = PackageResult(pkg)
            action = event.get("Action")
            test = event.get("Test")
            if action == "output":
                text = event.get("Output") or ""
                if test is None and NO_TEST_FILES_MARKER in text:
                    result.no_test_files = True
                result.output.append(text)
                continue
            if action not in VERDICT_ACTIONS:
                continue
            if test is None:
                result.pkg_action = action
            elif "/" in test:
                result.sub[action] += 1
            else:
                result.top[action] += 1
    return packages, malformed


def read_list_file(path: str) -> list[str]:
    out = []
    with open(path, "r", encoding="utf-8") as fh:
        for raw in fh:
            line = raw.strip()
            if line and not line.startswith("#"):
                out.append(line)
    return out


def read_allowlist(path: str | None) -> tuple[dict[str, str], list[str]]:
    """Return ({package: reason}, errors).

    A bare package path with no reason is rejected. "Explicitly listed" means
    the next reader can see WHY without archaeology.
    """
    if not path:
        return {}, []
    if not os.path.exists(path):
        return {}, [f"allowlist file not found: {path}"]
    entries: dict[str, str] = {}
    errors: list[str] = []
    with open(path, "r", encoding="utf-8") as fh:
        for lineno, raw in enumerate(fh, 1):
            line = raw.strip()
            if not line or line.startswith("#"):
                continue
            if "#" not in line:
                errors.append(
                    f"{path}:{lineno}: '{line}' has no reason. Every entry must read "
                    f"'<import/path>  # why this package legitimately skips wholesale, "
                    f"and where it DOES execute'."
                )
                continue
            pkg, reason = line.split("#", 1)
            pkg, reason = pkg.strip(), reason.strip()
            if not pkg or not reason:
                errors.append(f"{path}:{lineno}: '{line}' is missing a package or a reason.")
                continue
            entries[pkg] = reason
    return entries, errors


def short(pkg: str, trim: str) -> str:
    if trim and trim in pkg:
        return pkg.split(trim, 1)[-1]
    return pkg


def render(packages: dict[str, PackageResult], trim: str, show_failures: bool) -> None:
    """Reproduce a human-readable per-package summary.

    `go test -json` replaces go's own `ok  pkg  1.2s` lines, so the CI log
    would otherwise show nothing at all for this step. This prints strictly
    more than go did: the per-package verdict tally is what the gate is
    deciding on, so it belongs in the log next to the decision.
    """
    print("::group::Per-package test tally (pass/fail/skip, top-level tests)")
    width = max((len(short(p, trim)) for p in packages), default=10)
    for name in sorted(packages):
        r = packages[name]
        state = r.state
        mark = {
            EXECUTED: "ok  ",
            NO_TEST_FILES: "----",
            ALL_SKIP: "SKIP",
            NO_TEST_EVENTS: "????",
        }[state]
        if state == NO_TEST_FILES:
            detail = "[no test files]"
        else:
            detail = "pass=%d fail=%d skip=%d" % (
                r.top["pass"],
                r.top["fail"],
                r.top["skip"],
            )
            if r.sub:
                detail += "  (subtests pass=%d fail=%d skip=%d)" % (
                    r.sub["pass"],
                    r.sub["fail"],
                    r.sub["skip"],
                )
        print("  %s %-*s  %s" % (mark, width, short(name, trim), detail))
    print("::endgroup::")

    if not show_failures:
        return
    failing = [n for n, r in packages.items() if r.pkg_action == "fail"]
    for name in sorted(failing):
        print("::group::FAILED package output: %s" % short(name, trim))
        sys.stdout.write("".join(packages[name].output))
        print("::endgroup::")


def main(argv: list[str]) -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--json-log", required=True, help="file holding a `go test -json` stream")
    ap.add_argument(
        "--expected-packages",
        required=True,
        help="file holding the output of `go list ./...` -- every line must appear in the stream",
    )
    ap.add_argument(
        "--allowlist",
        default=None,
        help="file naming packages that legitimately skip wholesale, each with a reason",
    )
    ap.add_argument(
        "--min-packages",
        type=int,
        default=40,
        help="floor on packages inspected; a set that came out empty must not compare equal to another empty set",
    )
    ap.add_argument("--trim-prefix", default="", help="cosmetic prefix trim for log readability")
    ap.add_argument("--render", action="store_true", help="print the per-package tally")
    ap.add_argument("--no-failure-output", action="store_true")
    args = ap.parse_args(argv)

    errors: list[str] = []
    notes: list[str] = []

    # ── the gate's own preconditions ──────────────────────────────────────
    # Asserted before anything is inspected, because each one is a way for
    # this script to report success while checking nothing.
    if not os.path.exists(args.json_log):
        print(
            "::error::assert-tests-executed: the go test -json log %r does not exist. "
            "The gate inspected ZERO packages; that is a gate failure, not a pass." % args.json_log
        )
        return 1
    if os.path.getsize(args.json_log) == 0:
        print(
            "::error::assert-tests-executed: the go test -json log %r is EMPTY. "
            "The gate inspected ZERO packages; that is a gate failure, not a pass." % args.json_log
        )
        return 1
    if not os.path.exists(args.expected_packages):
        print(
            "::error::assert-tests-executed: the expected-package list %r does not exist "
            "(it should be `go list ./...`)." % args.expected_packages
        )
        return 1

    expected = read_list_file(args.expected_packages)
    packages, malformed = parse_json_log(args.json_log)

    if args.render:
        render(packages, args.trim_prefix, not args.no_failure_output)

    if len(expected) < args.min_packages:
        errors.append(
            "expected-package list %s holds %d package(s), below the floor of %d. "
            "`go list ./...` matched (almost) nothing, so any set comparison below would "
            "have succeeded vacuously." % (args.expected_packages, len(expected), args.min_packages)
        )
    if len(packages) < args.min_packages:
        errors.append(
            "the go test -json stream carries %d package(s), below the floor of %d. "
            "The run did not cover the module." % (len(packages), args.min_packages)
        )

    missing = sorted(set(expected) - set(packages))
    if missing:
        errors.append(
            "%d package(s) are in `go list ./...` but produced NO event in the test stream -- "
            "they were never run: %s" % (len(missing), ", ".join(short(m, args.trim_prefix) for m in missing))
        )

    allowlist, allow_errors = read_allowlist(args.allowlist)
    errors.extend(allow_errors)

    if malformed:
        notes.append(
            "%d non-JSON line(s) in the stream (build errors are emitted as plain text); "
            "first: %s" % (len(malformed), malformed[0][:200])
        )

    # ── the per-package assertion ─────────────────────────────────────────
    by_state: dict[str, list[str]] = defaultdict(list)
    for name, result in packages.items():
        by_state[result.state].append(name)

    for name in sorted(by_state[NO_TEST_EVENTS]):
        errors.append(
            "%s produced NO test verdict at all, and go did not report '[no test files]'. "
            "Its tests did not compile into the binary -- check the build tags, the file names, "
            "and that the package path still matches." % short(name, args.trim_prefix)
        )

    unjustified_all_skip = []
    for name in sorted(by_state[ALL_SKIP]):
        r = packages[name]
        if name in allowlist:
            notes.append(
                "%s skipped wholesale (%d skip) - allowlisted: %s"
                % (short(name, args.trim_prefix), r.top["skip"], allowlist[name])
            )
            continue
        unjustified_all_skip.append(name)
        errors.append(
            "%s executed ZERO tests: %d top-level test(s), ALL of them skipped. A package in "
            "which everything skips is the vacuous pass this gate exists to catch. If the skip "
            "is legitimate, add the package to %s with a reason naming where it DOES execute; "
            "an implicit pass is not available."
            % (
                short(name, args.trim_prefix),
                r.top["skip"],
                args.allowlist or "(no --allowlist configured)",
            )
        )

    # An allowlist is a liability once it stops matching reality.
    for name, reason in sorted(allowlist.items()):
        if name not in packages:
            errors.append(
                "allowlist entry %s names a package that produced no event in this run -- it was "
                "renamed or deleted. Remove the line. (reason on file: %s)"
                % (short(name, args.trim_prefix), reason)
            )
        elif packages[name].state != ALL_SKIP:
            errors.append(
                "allowlist entry %s is STALE: the package is %s (pass=%d fail=%d skip=%d), not "
                "all-skip. It no longer needs an exemption -- delete the line so the allowlist "
                "keeps describing reality. (reason on file: %s)"
                % (
                    short(name, args.trim_prefix),
                    packages[name].state,
                    packages[name].top["pass"],
                    packages[name].top["fail"],
                    packages[name].top["skip"],
                    reason,
                )
            )

    executed_pkgs = len(by_state[EXECUTED])
    executed_tests = sum(packages[n].executed for n in by_state[EXECUTED])

    # The line that stops this whole script from being a no-op. Every rule
    # above is a "must not" — without a "must", a run in which nothing at all
    # ran satisfies all of them.
    if executed_pkgs == 0:
        errors.append(
            "ZERO packages executed a single test. Whatever else this run did, it proved nothing."
        )
    if executed_tests == 0:
        errors.append("ZERO top-level tests passed or failed across the entire module.")

    print(
        "assert-tests-executed: inspected %d package(s) | executed %d (%d top-level tests) | "
        "no-test-files %d | all-skip %d (%d allowlisted) | no-test-events %d"
        % (
            len(packages),
            executed_pkgs,
            executed_tests,
            len(by_state[NO_TEST_FILES]),
            len(by_state[ALL_SKIP]),
            len(by_state[ALL_SKIP]) - len(unjustified_all_skip),
            len(by_state[NO_TEST_EVENTS]),
        )
    )
    for note in notes:
        print("  note: %s" % note)

    if errors:
        for err in errors:
            print("::error::assert-tests-executed: %s" % err)
        print(
            "::error::assert-tests-executed FAILED with %d finding(s). A green test run that "
            "executed nothing is the defect this gate exists to refuse." % len(errors)
        )
        return 1

    print(
        "assert-tests-executed: every package either executed a real test or is accounted for. OK"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
