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

  NO_TEST_FILES     `go list` reports no _test.go at all (a cmd/ dir with one
                    main.go). Not a defect; counted and reported.
  ALL_SKIP          Tests exist and ran, and every one of them skipped. FAILS,
                    unless the package is allowlisted with a reason.
  TESTS_TAGGED_OUT  The package HAS _test.go files and a build constraint
                    excluded every one of them, so nothing ran. FAILS, same
                    allowlist escape.
  NO_TEST_EVENTS    Tests compile in, yet the stream carries no verdict for
                    the package. The original vacuous-pass shape. FAILS.

Those four are told apart using `go list` metadata, NOT go's console prose.
The first cut of this script looked for the "[no test files]" marker in the
output stream and was wrong: under `-coverprofile` go prints
`pkg  coverage: 0.0% of statements` and no marker, so three test-free cmd/
packages were misread as defects and the gate reddened its own first CI run.

WHY THIS GATE CANNOT ITSELF PASS VACUOUSLY
------------------------------------------
A checker that greps a log which was never produced, or iterates a package
set that came out empty, reports success while inspecting nothing. Every
mechanism this script depends on is therefore asserted before it is used:

  * the JSON log must exist and be non-empty;
  * the `go list` manifest must be non-empty, and at least one package in it
    must own a compiled-in test file;
  * BOTH must contain at least ``--min-packages`` entries. Comparing two
    empty sets succeeds — the floor is what makes the comparison mean
    something;
  * every package in the manifest must appear in the JSON stream, so a
    package that silently vanished from the run is a failure, not an absence;
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

EXECUTED = "EXECUTED"
NO_TEST_FILES = "NO_TEST_FILES"
ALL_SKIP = "ALL_SKIP"
NO_TEST_EVENTS = "NO_TEST_EVENTS"
TESTS_TAGGED_OUT = "TESTS_TAGGED_OUT"

VERDICT_ACTIONS = ("pass", "fail", "skip")


class PackageFacts:
    """What `go list` says a package HAS, independent of what the run did.

    Whether a package owns test files is not something to infer from go's
    console prose. The first cut of this script looked for the "[no test
    files]" marker in the output stream, and it was WRONG: under
    `-coverprofile` go emits `pkg  coverage: 0.0% of statements` and no
    marker at all, so all three test-free cmd/ packages were misread as
    "tests did not compile in" and the gate failed its own first CI run
    (run 632196 / job 932832). The marker is a rendering detail that
    changes with flags; `go list` metadata is not.

    ntest        test files that DO compile in under the active build tags
    nignored     _test.go files excluded BY a build constraint
    """

    __slots__ = ("ntest", "nignored")

    def __init__(self, ntest: int, nignored: int) -> None:
        self.ntest = ntest
        self.nignored = nignored


class PackageResult:
    __slots__ = ("name", "top", "sub", "pkg_action", "facts", "output")

    def __init__(self, name: str) -> None:
        self.name = name
        self.top: Counter = Counter()
        self.sub: Counter = Counter()
        self.pkg_action: str | None = None
        self.facts: PackageFacts | None = None
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
        # Nothing ran. Whether that is fine depends on what the package OWNS,
        # which only `go list` can say.
        f = self.facts
        if f is None or f.ntest > 0:
            return NO_TEST_EVENTS
        if f.nignored > 0:
            return TESTS_TAGGED_OUT
        return NO_TEST_FILES


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
                result.output.append(event.get("Output") or "")
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


def read_manifest(path: str) -> tuple[dict[str, PackageFacts], list[str]]:
    """Parse the `go list` manifest. Returns ({import path: facts}, errors).

    Produced by:

        go list -f '{{.ImportPath}} {{len .TestGoFiles}} {{len .XTestGoFiles}} {{join .IgnoredGoFiles ","}}' ./...

    Whitespace-separated on purpose: import paths and counts contain no
    spaces, IgnoredGoFiles are comma-joined, and no workflow in this repo
    carries a literal tab -- one that existed only inside a YAML block
    scalar would be an easy byte for a reformat to eat.

    IgnoredGoFiles is how a package whose test files are ALL excluded by a
    build constraint stays distinguishable from a package that simply has
    none -- the first is the tests-stopped-compiling-in defect, the second
    is a cmd/ dir holding one main.go.
    """
    facts: dict[str, PackageFacts] = {}
    errors: list[str] = []
    with open(path, "r", encoding="utf-8") as fh:
        for lineno, raw in enumerate(fh, 1):
            line = raw.rstrip("\n")
            if not line.strip() or line.lstrip().startswith("#"):
                continue
            parts = line.split()
            if len(parts) < 3:
                errors.append("%s:%d: unparseable manifest line %r" % (path, lineno, line[:120]))
                continue
            parts += [""] * (4 - len(parts))
            try:
                ntest = int(parts[1]) + int(parts[2])
            except ValueError:
                errors.append("%s:%d: non-numeric test-file count in %r" % (path, lineno, line[:120]))
                continue
            nignored = len([f for f in parts[3].split(",") if f.strip().endswith("_test.go")])
            del parts[4:]
            facts[parts[0].strip()] = PackageFacts(ntest, nignored)
    return facts, errors


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
            TESTS_TAGGED_OUT: "TAGD",
        }[state]
        if state == NO_TEST_FILES:
            detail = "no test files (go list)"
        elif state == TESTS_TAGGED_OUT:
            detail = "%d _test.go file(s) excluded by build constraints" % r.facts.nignored
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
        "--package-manifest",
        required=True,
        help="manifest from `go list -f '{{.ImportPath}} {{len .TestGoFiles}} "
             "{{len .XTestGoFiles}} {{join .IgnoredGoFiles \",\"}}' ./...`",
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
    if not os.path.exists(args.package_manifest):
        print(
            "::error::assert-tests-executed: the package manifest %r does not exist "
            "(it should be the `go list` TSV)." % args.package_manifest
        )
        return 1

    facts, manifest_errors = read_manifest(args.package_manifest)
    errors.extend(manifest_errors)
    expected = sorted(facts)
    packages, malformed = parse_json_log(args.json_log)
    # A package go list never heard of cannot be classified, so say so rather
    # than defaulting it to something harmless.
    for name, result in packages.items():
        result.facts = facts.get(name)

    if args.render:
        render(packages, args.trim_prefix, not args.no_failure_output)

    if len(expected) < args.min_packages:
        errors.append(
            "package manifest %s holds %d package(s), below the floor of %d. "
            "`go list ./...` matched (almost) nothing, so any set comparison below would "
            "have succeeded vacuously." % (args.package_manifest, len(expected), args.min_packages)
        )
    with_tests = sum(1 for f in facts.values() if f.ntest > 0)
    if facts and with_tests == 0:
        errors.append(
            "the manifest says NO package in the module owns a compiled-in test file. "
            "Either the -f template is wrong or the build tags excluded everything; "
            "either way the per-package assertion below would have nothing to assert on."
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
        f = packages[name].facts
        if f is None:
            errors.append(
                "%s appears in the test stream but NOT in the `go list` manifest, so nothing "
                "can say whether it should have run. The two inputs disagree about what the "
                "module contains; do not read this run as a pass."
                % short(name, args.trim_prefix)
            )
            continue
        errors.append(
            "%s owns %d compiled-in test file(s) but produced NO test verdict at all. Its tests "
            "did not run -- check that they still compile in (build tags, file names) and that "
            "the package path still matches." % (short(name, args.trim_prefix), f.ntest)
        )

    for name in sorted(by_state[TESTS_TAGGED_OUT]):
        f = packages[name].facts
        if name in allowlist:
            notes.append(
                "%s has all %d of its _test.go files excluded by build constraints - allowlisted: %s"
                % (short(name, args.trim_prefix), f.nignored, allowlist[name])
            )
            continue
        errors.append(
            "%s has %d _test.go file(s), and a build constraint excluded EVERY one of them, so "
            "the package ran nothing. This is the tests-stopped-compiling-in shape: `go test` "
            "exits 0 and the package looks covered. If the tag gating is deliberate, add the "
            "package to %s with a reason naming the lane that sets the tag."
            % (
                short(name, args.trim_prefix),
                f.nignored,
                args.allowlist or "(no --allowlist configured)",
            )
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
        elif packages[name].state not in (ALL_SKIP, TESTS_TAGGED_OUT):
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
        "no-test-files %d | all-skip %d (%d allowlisted) | no-test-events %d | tagged-out %d"
        % (
            len(packages),
            executed_pkgs,
            executed_tests,
            len(by_state[NO_TEST_FILES]),
            len(by_state[ALL_SKIP]),
            len(by_state[ALL_SKIP]) - len(unjustified_all_skip),
            len(by_state[NO_TEST_EVENTS]),
            len(by_state[TESTS_TAGGED_OUT]),
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
