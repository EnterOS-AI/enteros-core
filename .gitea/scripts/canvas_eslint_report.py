#!/usr/bin/env python3
"""Assertions over an ESLint JSON report for the canvas lint gate.

Two modes, both used by .gitea/scripts/canvas-eslint-gate.sh:

  --report/--expected     COVERAGE. Every path in the expected list must be
                          present in the ESLint report. `eslint <path>` exits 0
                          when it matched nothing, so a green run proves
                          nothing on its own; this turns "linted zero files"
                          into a failure instead of a pass.

  --negative-control      The guard itself still works. Lints a generated
                          corpus of known-bad call shapes and asserts the rule
                          fired on every bad one and on neither good one. This
                          is what stops the gate degrading into a step that
                          runs but no longer guards anything.
"""

from __future__ import annotations

import argparse
import json
import os
import sys


def load(path: str) -> list:
    with open(path, encoding="utf-8") as fh:
        return json.load(fh)


def check_coverage(report_path: str, canvas_dir: str, expected_path: str) -> int:
    report = load(report_path)
    with open(expected_path, encoding="utf-8") as fh:
        expected = {line.strip() for line in fh if line.strip()}

    linted = set()
    for entry in report:
        rel = os.path.relpath(entry["filePath"], canvas_dir).replace(os.sep, "/")
        linted.add(rel)

    missing = sorted(expected - linted)
    errors = sum(f["errorCount"] for f in report)
    warnings = sum(f["warningCount"] for f in report)

    print(f"   files linted:        {len(linted)}")
    print(f"   protected e2e files: {len(expected)} expected, "
          f"{len(expected & linted)} present in report")
    print(f"   findings:            {errors} error(s), {warnings} warning(s)")

    # Every ERROR in full — these are what fail the build, so they must be
    # readable in the job log without downloading an artifact.
    if errors:
        print("")
        print("-- error-severity findings --")
        for entry in report:
            if not entry["errorCount"]:
                continue
            rel = os.path.relpath(entry["filePath"], canvas_dir).replace(os.sep, "/")
            print(f"  canvas/{rel}")
            for m in entry["messages"]:
                if m.get("severity") != 2:
                    continue
                print(f"    {m.get('line')}:{m.get('column')}  "
                      f"{m.get('ruleId')}")
                for line in str(m.get("message", "")).splitlines():
                    print(f"        {line}")

    # Warnings are summarised per rule rather than listed. They do not gate,
    # and printing all of them buries the errors that do.
    if warnings:
        counts: dict[str, int] = {}
        for entry in report:
            for m in entry["messages"]:
                if m.get("severity") == 1:
                    counts[str(m.get("ruleId"))] = counts.get(str(m.get("ruleId")), 0) + 1
        print("")
        print("-- warnings (non-gating; see the note in canvas-eslint-gate.sh) --")
        for rule, n in sorted(counts.items(), key=lambda kv: -kv[1]):
            print(f"   {n:5d}  {rule}")

    if missing:
        print("")
        print("FAIL: ESLint did NOT lint these protected files:")
        for m in missing:
            print(f"     canvas/{m}")
        print("   The waitForFunction guard is scoped to e2e/**/*.ts. If those")
        print("   files are not in the report, the guard did not run on them and")
        print("   a green result means nothing. Check `ignores` in")
        print("   canvas/eslint.config.mjs and the gate's working directory.")
        return 1

    if not linted:
        print("FAIL: ESLint reported ZERO files. Refusing to pass vacuously.")
        return 1

    return 0


def check_negative_control(report_path: str, rule: str,
                           expect_bad: int, expect_good: int) -> int:
    report = load(report_path)
    if not report:
        print(f"FAIL: Negative control produced an EMPTY report. ESLint did not lint")
        print(f"   the generated corpus at all, so the guard was never exercised.")
        return 1

    # The corpus is a single file. Collect which exported function each finding
    # landed in by line order: bad* declarations come first, then good*.
    fired = [m for entry in report for m in entry["messages"]
             if m.get("ruleId") == rule]
    severities = {m["severity"] for m in fired}

    print(f"   guard findings: {len(fired)} (expected {expect_bad})")

    ok = True
    if len(fired) != expect_bad:
        print("")
        print(f"FAIL: The waitForFunction guard fired {len(fired)} time(s) on a corpus of")
        print(f"   {expect_bad} known-bad shapes and {expect_good} known-good ones.")
        print("   Either the rule was removed/downgraded/narrowed in")
        print("   canvas/eslint.config.mjs, or the corpus in canvas-eslint-gate.sh")
        print("   drifted from it. Both are real problems - the guard is not")
        print("   protecting what this gate claims it protects.")
        ok = False

    if fired and severities != {2}:
        print(f"FAIL: Guard fired at severity {severities}, expected error (2).")
        print("   A warning does not fail a build; that is rung 3 without rung 4.")
        ok = False

    other = [m for entry in report for m in entry["messages"]
             if m.get("ruleId") != rule and m.get("severity") == 2]
    if other:
        print("WARN:  Negative-control corpus also tripped unrelated error rules:")
        for m in other[:5]:
            print(f"     {m.get('ruleId')}: {m.get('message')}")
        print("   This does not fail the gate, but the corpus should isolate the")
        print("   guard - consider adjusting it so the signal stays clean.")

    return 0 if ok else 1


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--report")
    ap.add_argument("--canvas-dir")
    ap.add_argument("--expected")
    ap.add_argument("--negative-control")
    ap.add_argument("--expect-rule", default="no-restricted-syntax")
    ap.add_argument("--expect-bad", type=int, default=0)
    ap.add_argument("--expect-good", type=int, default=0)
    args = ap.parse_args()

    if args.negative_control:
        return check_negative_control(args.negative_control, args.expect_rule,
                                      args.expect_bad, args.expect_good)

    if not (args.report and args.canvas_dir and args.expected):
        ap.error("--report, --canvas-dir and --expected are required "
                 "unless --negative-control is given")
    return check_coverage(args.report, args.canvas_dir, args.expected)


if __name__ == "__main__":
    sys.exit(main())
