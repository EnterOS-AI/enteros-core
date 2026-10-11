#!/usr/bin/env python3
"""Assertions for the canvas static-analysis gate (canvas-static-gate.sh).

Four modes, one per assertion the gate makes:

  --coverage              Every protected file appears in the ESLint report.
                          `eslint <path>` exits 0 when it matched nothing, so a
                          green run proves nothing on its own.

  --negative-control      The ESLint guard still fires, PER SHAPE. Each corpus
                          function is named for the shape it encodes; the
                          finding's line is mapped back to that name. Comparing
                          only the TOTAL would let an edit that loses one bad
                          shape and gains one good one sum to the same number
                          and pass.

  --tsc-coverage          Every protected file appears in `tsc --listFiles`,
                          and tsc reported no errors. `tsc -p cfg` exits 0 when
                          the project resolves to zero files.

  --tsc-negative-control  tsc actually REJECTS the transposed third argument.
                          Coverage proves the files were read; this proves the
                          type checker objects to the shape the eslint rule's
                          comment says it covers.

Each mode exits non-zero on failure so `set -e` in the caller stops the build.
"""

from __future__ import annotations

import argparse
import os
import re
import sys
import json

FUNC_RE = re.compile(r"function\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(")


def load_json(path: str):
    with open(path, encoding="utf-8") as fh:
        return json.load(fh)


def read_expected(path: str) -> set:
    with open(path, encoding="utf-8") as fh:
        return {line.strip() for line in fh if line.strip()}


def rel(p: str, root: str) -> str:
    return os.path.relpath(p, root).replace(os.sep, "/")


# ---------------------------------------------------------------- coverage
def check_coverage(report_path, canvas_dir, expected_path) -> int:
    report = load_json(report_path)
    expected = read_expected(expected_path)
    linted = {rel(e["filePath"], canvas_dir) for e in report}

    errors = sum(f["errorCount"] for f in report)
    warnings = sum(f["warningCount"] for f in report)
    print("   files linted:        %d" % len(linted))
    print("   protected files:     %d expected, %d present in report"
          % (len(expected), len(expected & linted)))
    print("   findings:            %d error(s), %d warning(s)" % (errors, warnings))

    if errors:
        print("")
        print("   -- error-severity findings --")
        for entry in report:
            if not entry["errorCount"]:
                continue
            print("   canvas/%s" % rel(entry["filePath"], canvas_dir))
            for m in entry["messages"]:
                if m.get("severity") != 2:
                    continue
                print("     %s:%s  %s" % (m.get("line"), m.get("column"), m.get("ruleId")))
                for line in str(m.get("message", "")).splitlines():
                    print("         %s" % line)

    if warnings:
        counts: dict = {}
        for entry in report:
            for m in entry["messages"]:
                if m.get("severity") == 1:
                    k = str(m.get("ruleId"))
                    counts[k] = counts.get(k, 0) + 1
        print("")
        print("   -- warnings (non-gating; see the note in canvas-static-gate.sh) --")
        for r, n in sorted(counts.items(), key=lambda kv: -kv[1]):
            print("   %5d  %s" % (n, r))

    missing = sorted(expected - linted)
    if missing:
        print("")
        print("FAIL: ESLint did NOT lint these protected files:")
        for m in missing:
            print("        canvas/%s" % m)
        print("      The waitForFunction guard is scoped to e2e/**. If those files")
        print("      are not in the report, the guard did not run on them and a")
        print("      green result means nothing. Check `ignores` in")
        print("      canvas/eslint.config.mjs and the gate's working directory.")
        return 1

    if not linted:
        print("FAIL: ESLint reported ZERO files. Refusing to pass vacuously.")
        return 1
    return 0


# -------------------------------------------------------- negative control
def corpus_shapes(corpus_path: str):
    """line number -> function name, for every line declaring a function."""
    by_line = {}
    with open(corpus_path, encoding="utf-8") as fh:
        for i, line in enumerate(fh, start=1):
            m = FUNC_RE.search(line)
            if m:
                by_line[i] = m.group(1)
    return by_line


def check_negative_control(report_path, corpus_path, rule) -> int:
    report = load_json(report_path)
    if not report:
        print("FAIL: negative control produced an EMPTY report. ESLint did not")
        print("      lint the generated corpus at all, so the guard was never")
        print("      exercised.")
        return 1

    by_line = corpus_shapes(corpus_path)
    expect_bad = {n for n in by_line.values() if n.startswith("bad_")}
    expect_good = {n for n in by_line.values() if n.startswith("good_")}
    if not expect_bad:
        print("FAIL: corpus declares no bad_* shapes — nothing to prove.")
        return 1

    fired, severities, unmapped = set(), set(), []
    for entry in report:
        for m in entry["messages"]:
            if m.get("ruleId") != rule:
                continue
            name = by_line.get(m.get("line"))
            if name is None:
                unmapped.append(m.get("line"))
                continue
            fired.add(name)
            severities.add(m.get("severity"))

    print("   corpus: %d bad shape(s), %d good shape(s)"
          % (len(expect_bad), len(expect_good)))
    print("   guard fired on %d/%d bad shapes" % (len(fired & expect_bad), len(expect_bad)))

    ok = True
    missed = sorted(expect_bad - fired)
    if missed:
        print("")
        print("FAIL: the waitForFunction guard did NOT fire on %d known-bad shape(s):"
              % len(missed))
        for n in missed:
            print("        %s" % n)
        print("      The rule was removed, downgraded or NARROWED in")
        print("      canvas/eslint.config.mjs, or the corpus drifted from it.")
        ok = False

    false_pos = sorted(fired & expect_good)
    if false_pos:
        print("")
        print("FAIL: the guard fired on %d shape(s) that are CORRECT:" % len(false_pos))
        for n in false_pos:
            print("        %s" % n)
        print("      The selector is over-matching; it would reject valid calls.")
        ok = False

    if unmapped:
        print("FAIL: %d finding(s) on lines with no function declaration: %s"
              % (len(unmapped), unmapped[:5]))
        print("      The corpus and the line->shape mapping have drifted.")
        ok = False

    if fired and severities != {2}:
        print("FAIL: guard fired at severity %s, expected error (2)." % severities)
        print("      A warning does not fail a build; that is rung 3 without rung 4.")
        ok = False

    return 0 if ok else 1


# ------------------------------------------------------------ tsc coverage
def check_tsc_coverage(listfiles_path, canvas_dir, expected_path, errors_path) -> int:
    expected = read_expected(expected_path)
    seen = set()
    with open(listfiles_path, encoding="utf-8", errors="replace") as fh:
        for line in fh:
            line = line.strip()
            if not line or "error TS" in line:
                continue
            try:
                r = rel(line, canvas_dir)
            except ValueError:
                continue
            # --listFiles also prints every .d.ts pulled in from node_modules
            # (typescript's own lib, @playwright/test, @types/node). Counting
            # those makes the "files in program" number meaningless as a
            # coverage signal, so restrict it to first-party sources.
            if r.startswith("..") or "node_modules/" in r:
                continue
            seen.add(r)

    in_program = expected & seen
    print("   first-party files in tsc program: %d" % len(seen))
    print("   protected files:      %d expected, %d in program"
          % (len(expected), len(in_program)))

    errs = []
    if errors_path and os.path.exists(errors_path):
        with open(errors_path, encoding="utf-8", errors="replace") as fh:
            errs = [l.rstrip() for l in fh if l.strip()]
    print("   type errors:          %d" % len(errs))
    if errs:
        print("")
        print("   -- tsc errors --")
        for line in errs[:40]:
            print("     %s" % line)

    ok = True
    missing = sorted(expected - seen)
    if missing:
        print("")
        print("FAIL: tsc did NOT type-check these protected files:")
        for m in missing:
            print("        canvas/%s" % m)
        print("      canvas/tsconfig.e2e.json exists precisely because the main")
        print("      tsconfig.json EXCLUDES e2e. If files are missing from the")
        print("      program, the type check is not covering the tree this gate")
        print("      claims it covers, and `tsc -p` exits 0 over zero files.")
        ok = False

    if not seen:
        print("FAIL: tsc program contains ZERO files. Refusing to pass vacuously.")
        ok = False

    if errs:
        ok = False

    return 0 if ok else 1


# --------------------------------------------------- tsc negative control
def check_tsc_negative_control(log_path, expect_files) -> int:
    with open(log_path, encoding="utf-8", errors="replace") as fh:
        log = fh.read()

    ok = True
    for f in expect_files:
        needle = f.replace("/", os.sep) if os.sep != "/" else f
        hit = ("error TS" in log) and (f in log or needle in log)
        print("   tsc rejects %-45s -> %s" % (f, "yes" if hit else "NO"))
        if not hit:
            ok = False

    if not ok:
        print("")
        print("FAIL: tsc did NOT reject the transposed third argument.")
        print("      canvas/eslint.config.mjs states that the arity rule cannot")
        print("      see a 3-argument call with the arg and options transposed,")
        print("      and that the TYPE CHECKER covers that slot. That claim is")
        print("      now false: either the tsc project stopped including the")
        print("      probe, or Playwright's options parameter loosened so the")
        print("      shape type-checks. Fix the coverage or fix the comment -")
        print("      an unbacked claim in that file is what this gate exists to")
        print("      prevent.")
        return 1
    return 0


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--coverage")
    ap.add_argument("--negative-control")
    ap.add_argument("--tsc-coverage")
    ap.add_argument("--tsc-negative-control")
    ap.add_argument("--canvas-dir")
    ap.add_argument("--expected")
    ap.add_argument("--corpus")
    ap.add_argument("--tsc-errors")
    ap.add_argument("--expect-rule", default="no-restricted-syntax")
    ap.add_argument("--expect-files", nargs="*", default=[])
    a = ap.parse_args()

    if a.coverage:
        if not (a.canvas_dir and a.expected):
            ap.error("--coverage needs --canvas-dir and --expected")
        return check_coverage(a.coverage, a.canvas_dir, a.expected)
    if a.negative_control:
        if not a.corpus:
            ap.error("--negative-control needs --corpus")
        return check_negative_control(a.negative_control, a.corpus, a.expect_rule)
    if a.tsc_coverage:
        if not (a.canvas_dir and a.expected):
            ap.error("--tsc-coverage needs --canvas-dir and --expected")
        return check_tsc_coverage(a.tsc_coverage, a.canvas_dir, a.expected, a.tsc_errors)
    if a.tsc_negative_control:
        return check_tsc_negative_control(a.tsc_negative_control, a.expect_files)
    ap.error("pick a mode")


if __name__ == "__main__":
    sys.exit(main())
