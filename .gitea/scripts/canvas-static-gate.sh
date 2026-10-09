#!/usr/bin/env bash
# Canvas static-analysis gate. TWO checks, both of which FAIL the build:
#
#   1. ESLint over the canvas package  — carries the waitForFunction guard.
#   2. tsc --noEmit over canvas/e2e    — the ONLY thing that type-checks that
#                                        tree, and what covers the transposed
#                                        third argument the arity rule cannot.
#
# (Named canvas-eslint-gate.sh until it grew the type-check half. A script
# whose name describes half of what it does is the same class of problem this
# gate exists to catch.)
#
# ── WHY THIS EXISTS ──────────────────────────────────────────────────────
# canvas/eslint.config.mjs has carried a `no-restricted-syntax` guard against
# the unbounded-waitForFunction defect since #5107. It was wired to NOTHING:
#
#   • no workflow ran `eslint` — CI's canvas-build did `npm ci` / `npm run
#     build` / `vitest run` and stopped there;
#   • `next build` lints only ESLINT_DEFAULT_DIRS
#     (app, pages, components, lib, src) and canvas sets no `eslint.dirs`
#     override, so canvas/e2e — the ONLY tree the rule is scoped to — was
#     outside everything `npm run build` looks at;
#   • .githooks/pre-commit is scoped to canvas/src/.
#
# The same hole existed for types, and was found only in review: tsconfig.json
# EXCLUDES e2e, there is no `typecheck` script, no workflow ran `tsc`, and
# `playwright test` transpiles via esbuild without type-checking. So the
# rule's claim that "TypeScript covers the third argument" was false — not
# because TypeScript cannot, but because it was never run. Check 2 makes the
# claim true instead of deleting it.
#
# A rule that exists and matches is rungs 1 and 2; running in CI is rung 3 and
# failing the build is rung 4, and only rung 4 is a gate. This script is rungs
# 3 and 4 for both halves.
#
# ── WHY IT CANNOT PASS VACUOUSLY ─────────────────────────────────────────
# `eslint <path>` exits 0 when it matched no files, and `tsc -p <cfg>` exits 0
# when the project resolves to zero files. "The step is green" and "the step
# checked something" are different claims. Four assertions separate them:
#
#   1. ESLINT COVERAGE — every tracked canvas/e2e/**/*.{ts,tsx} file must
#      appear in the ESLint report. Derived from `git ls-files`, not a
#      hardcoded count.
#   2. TSC COVERAGE — every one of those same files must appear in
#      `tsc --listFiles`. Same derivation, so the two halves cannot disagree
#      about what "the protected files" means.
#   3. ESLINT NEGATIVE CONTROL — a generated corpus of known-bad call shapes
#      is linted with the repository's real config, and the guard must fire on
#      every one, PER SHAPE. Checking only the total would let an edit that
#      loses one bad shape and gains one good one still sum to the same number
#      and pass.
#   4. TSC NEGATIVE CONTROL — the transposed shape must actually be rejected
#      by tsc. Without this, check 2 proves the files were read but not that
#      the type checker objects to the thing we claim it objects to.
#
# Corpora are generated into the tree and removed, never committed: a
# committed fixture full of deliberate violations would fail the very checks
# this script runs.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CANVAS_DIR="$REPO_ROOT/canvas"
ESLINT="$CANVAS_DIR/node_modules/eslint/bin/eslint.js"
TSC="$CANVAS_DIR/node_modules/typescript/bin/tsc"
REPORTER="$REPO_ROOT/.gitea/scripts/canvas_gate_report.py"

# The two probe directories are named DIFFERENTLY ON PURPOSE, and it is not
# cosmetic — TypeScript's `include` globs skip dot-directories, ESLint's do
# not. Verified: a file in e2e/.dotprobe/ is absent from `tsc --listFiles`
# while an identical file in e2e/__plainprobe/ is present.
#
#   • the ESLint corpus is DOTTED so tsc ignores it. It deliberately contains
#     shapes that do not type-check (`page["waitForFunction"]` on a narrow
#     type, a spread of unknown[]), so if tsc could see it, check 3 would go
#     red on the gate's own fixture.
#   • the tsc corpus is UNDOTTED so tsc CAN see it — that is the entire point
#     of check 4. Dotted, it is silently excluded and the "tsc rejects the
#     transposed shape" assertion fails closed, which is how this was found.
ESLINT_PROBE="$CANVAS_DIR/e2e/.eslint-negative-control"
TSC_PROBE="$CANVAS_DIR/e2e/__tsc-negative-control"

for tool in "$ESLINT" "$TSC"; do
  if [ ! -f "$tool" ]; then
    echo "FAIL: $tool not installed — run npm ci in canvas/ first."
    exit 1
  fi
done

WORK="$(mktemp -d)"

# Probe dirs are removed UP FRONT as well as on exit. A run cancelled between
# writing a probe and its trap firing would otherwise leave the corpus in the
# tree, and the NEXT run's package-wide lint (which happens before the probe
# section) would report the stale corpus as a real finding — a confusing false
# red in a file nobody wrote. Fail-closed and self-healing either way, but the
# cleanup belongs at both ends.
cleanup() { rm -rf "$WORK" "$ESLINT_PROBE" "$TSC_PROBE"; }
rm -rf "$ESLINT_PROBE" "$TSC_PROBE"
trap cleanup EXIT

# The protected-file list is derived ONCE and shared by both halves, so the
# ESLint scope and the tsc scope cannot silently drift apart.
#
# The glob covers .tsx as well as .ts. No .tsx exists under e2e/ today, but
# both the eslint `files:` scope and this list used to say .ts only, which
# meant a future e2e/foo.spec.tsx would have been unguarded AND not reported
# as missing — the coverage assertion would have been blind to exactly the
# file that slipped the rule.
git -C "$REPO_ROOT" ls-files -- 'canvas/e2e/**/*.ts' 'canvas/e2e/*.ts' \
                               'canvas/e2e/**/*.tsx' 'canvas/e2e/*.tsx' \
  | sed 's|^canvas/||' | sort -u > "$WORK/expected.txt"

EXPECTED_COUNT=$(wc -l < "$WORK/expected.txt" | tr -d ' ')
if [ "$EXPECTED_COUNT" -eq 0 ]; then
  echo "FAIL: no tracked canvas/e2e/**/*.{ts,tsx} files found. Either the tree"
  echo "      moved or the glob is wrong — refusing to report success over an"
  echo "      empty set."
  exit 1
fi

# ──────────────────────────────────────────────────────────────────────
# 1. ESLint over the canvas package.
# ──────────────────────────────────────────────────────────────────────
#
# Scope is the WHOLE canvas package, not just e2e/. Measured on 4b6833ed7 the
# package is already error-clean — 512 files, 0 errors, 183 warnings — so
# there is no pre-existing backlog to fix and none to silence.
#
# Errors gate; warnings do not. That is not a loophole: the config
# DELIBERATELY sets six rules to "warn" and 183 findings sit under them.
# Turning those into build failures is a separate decision about a separate
# backlog. The waitForFunction guard is "error", so it gates. If you want the
# warnings to gate too, fix the 183 first, then add --max-warnings=0.

echo "== 1/4 ESLint over the canvas package =="
REPORT="$WORK/report.json"
ESLINT_STATUS=0
( cd "$CANVAS_DIR" && node "$ESLINT" . --format json --output-file "$REPORT" ) || ESLINT_STATUS=$?

python3 "$REPORTER" --coverage "$REPORT" --canvas-dir "$CANVAS_DIR" \
                    --expected "$WORK/expected.txt"

# ──────────────────────────────────────────────────────────────────────
# 2. ESLint negative control — the guard still fires, shape by shape.
# ──────────────────────────────────────────────────────────────────────
#
# Keep this corpus in lockstep with the "WHAT IS STILL NOT COVERED" note in
# canvas/eslint.config.mjs. Each bad shape is ONE line and the function name
# is the shape's identity, so the reporter can assert per shape rather than
# per count.

echo "== 2/4 ESLint negative control (per shape) =="
mkdir -p "$ESLINT_PROBE"
cat > "$ESLINT_PROBE/shapes.ts" <<'PROBE'
/* Generated by .gitea/scripts/canvas-static-gate.sh — not committed. */
declare const page: { waitForFunction: (...a: unknown[]) => Promise<void> };
const fn = () => true;
const opts = { timeout: 10_000, polling: 100 };
const rest: unknown[] = [null, opts];
export async function bad_inline_object() { await page.waitForFunction(fn, { timeout: 10_000 }); }
export async function bad_object_spread() { await page.waitForFunction(fn, { ...opts }); }
export async function bad_variable_options() { await page.waitForFunction(fn, opts); }
export async function bad_as_const() { await page.waitForFunction(fn, { timeout: 1 } as const); }
export async function bad_satisfies() { await page.waitForFunction(fn, { timeout: 1 } satisfies Record<string, number>); }
export async function bad_argument_spread() { await page.waitForFunction(fn, ...rest); }
export async function bad_destructured() { const { waitForFunction } = page; await waitForFunction(fn, opts); }
export async function bad_single_arg() { await page.waitForFunction(fn); }
export async function bad_computed_member() { await page["waitForFunction"](fn, opts); }
export async function bad_optional_chain() { await page?.waitForFunction?.(fn, opts); }
export async function bad_explicit_undefined_options() { await page.waitForFunction(fn, opts, undefined); }
export async function good_inline_options() { await page.waitForFunction(fn, null, { timeout: 10_000, polling: 100 }); }
export async function good_variable_options() { await page.waitForFunction(fn, null, opts); }
export async function good_empty_options() { await page.waitForFunction(fn, null, {}); }
PROBE

PROBE_REPORT="$WORK/probe.json"
( cd "$CANVAS_DIR" && node "$ESLINT" "e2e/.eslint-negative-control/shapes.ts" \
    --format json --output-file "$PROBE_REPORT" ) || true

python3 "$REPORTER" --negative-control "$PROBE_REPORT" \
                    --corpus "$ESLINT_PROBE/shapes.ts" \
                    --expect-rule no-restricted-syntax

# ──────────────────────────────────────────────────────────────────────
# 3. tsc --noEmit over canvas/e2e.
# ──────────────────────────────────────────────────────────────────────
#
# canvas/tsconfig.e2e.json exists because the main tsconfig.json EXCLUDES
# e2e. Measured at the time of writing: 23 files, 0 errors. playwright.config.ts
# is deliberately NOT in that project — including it surfaces one pre-existing
# unrelated error (TS2769, `passWithNoTests` is not a valid Playwright config
# key). That is a real but separate bug; this gate does not fix it and does
# not silence it.

echo "== 3/4 tsc --noEmit over canvas/e2e =="
TSC_LIST="$WORK/tsc-files.txt"
TSC_OUT="$WORK/tsc.log"
TSC_STATUS=0
( cd "$CANVAS_DIR" && node "$TSC" -p tsconfig.e2e.json --noEmit --listFiles > "$TSC_LIST" 2>"$TSC_OUT" ) || TSC_STATUS=$?

# --listFiles prints every file in the program on stdout; diagnostics go to
# stdout too, so errors are recovered from the same stream.
grep -hE "error TS" "$TSC_LIST" "$TSC_OUT" 2>/dev/null > "$WORK/tsc-errors.txt" || true

python3 "$REPORTER" --tsc-coverage "$TSC_LIST" --canvas-dir "$CANVAS_DIR" \
                    --expected "$WORK/expected.txt" \
                    --tsc-errors "$WORK/tsc-errors.txt"

# ──────────────────────────────────────────────────────────────────────
# 4. tsc negative control — the transposed shape is actually rejected.
# ──────────────────────────────────────────────────────────────────────
#
# Check 3 proves tsc READ the protected files. It does not prove tsc OBJECTS
# to the shape the eslint rule's comment says it covers. Without this, the
# "TypeScript covers the third slot" claim could quietly become false again —
# a Playwright typings change that loosened the options parameter to `any`
# would be invisible.

echo "== 4/4 tsc negative control (transposed third argument) =="
mkdir -p "$TSC_PROBE"
cat > "$TSC_PROBE/transposed.ts" <<'PROBE'
/* Generated by .gitea/scripts/canvas-static-gate.sh — not committed. */
import type { Page } from "@playwright/test";
const loose: unknown[] = [null, { timeout: 1 }];
export async function transposedNull(p: Page) {
  await p.waitForFunction(() => true, { timeout: 1 }, null);
}
export async function untypedSpread(p: Page) {
  await p.waitForFunction(() => true, null, ...loose);
}
PROBE

TSC_PROBE_OUT="$WORK/tsc-probe.log"
( cd "$CANVAS_DIR" && node "$TSC" -p tsconfig.e2e.json --noEmit > "$TSC_PROBE_OUT" 2>&1 ) || true

python3 "$REPORTER" --tsc-negative-control "$TSC_PROBE_OUT" \
                    --expect-files "e2e/__tsc-negative-control/transposed.ts"

# ──────────────────────────────────────────────────────────────────────
# Verdict
# ──────────────────────────────────────────────────────────────────────

echo ""
FAILED=0
if [ "$ESLINT_STATUS" -ne 0 ]; then
  echo "BLOCKED: ESLint reported error-severity findings in canvas/ (exit $ESLINT_STATUS)."
  echo "         They are listed above. Fix them; do not add a blanket disable."
  FAILED=1
fi
if [ "$TSC_STATUS" -ne 0 ]; then
  echo "BLOCKED: tsc reported type errors in canvas/e2e (exit $TSC_STATUS)."
  echo "         They are listed above. This tree was unchecked until recently,"
  echo "         so a new error here is a real defect, not legacy noise."
  FAILED=1
fi
[ "$FAILED" -ne 0 ] && exit 1

echo "OK: canvas static gate passed."
echo "    ESLint: package error-clean; all $EXPECTED_COUNT protected e2e files linted;"
echo "            waitForFunction guard verified firing per shape."
echo "    tsc:    canvas/e2e type-clean; all $EXPECTED_COUNT protected files in the program;"
echo "            transposed third argument verified rejected."
