#!/usr/bin/env bash
# Canvas ESLint gate — runs ESLint over the canvas package and FAILS the build
# on any error-severity finding.
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
# So the guard against a defect that had just shipped could not fire anywhere
# except a manual `npm run lint`. A rule that exists and matches is rungs 1
# and 2; running in CI is rung 3 and failing the build is rung 4, and only
# rung 4 is a gate. This script is rungs 3 and 4.
#
# ── WHY IT CANNOT PASS VACUOUSLY ─────────────────────────────────────────
# `eslint <path>` exits 0 when it matched no files at all, so "the step is
# green" and "the step checked something" are different claims. Two assertions
# separate them:
#
#   1. COVERAGE — every tracked canvas/e2e/**/*.ts file must appear in the
#      ESLint report. Derived from `git ls-files`, not a hardcoded count, so
#      it tracks the tree. If an ignore pattern, a config `files:` scope or a
#      changed working directory ever stops ESLint from reaching the protected
#      files, this fails instead of reporting a clean run over nothing.
#
#   2. NEGATIVE CONTROL — a generated corpus of known-bad call shapes is
#      linted with the repository's real config, and the guard must fire on
#      every one of them. A green run therefore proves the rule is still
#      present, still error-severity and still matching, not merely that no
#      new code tripped it. Delete the rule and this step goes red.
#
# The corpus is generated into a temp dir rather than committed, because a
# committed fixture full of deliberate violations would itself fail the very
# lint this script runs.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CANVAS_DIR="$REPO_ROOT/canvas"
ESLINT="$CANVAS_DIR/node_modules/eslint/bin/eslint.js"

if [ ! -f "$ESLINT" ]; then
  echo "❌ eslint not installed at canvas/node_modules — run npm ci in canvas/ first."
  exit 1
fi

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# ──────────────────────────────────────────────────────────────────────
# 1. Lint the canvas package.
# ──────────────────────────────────────────────────────────────────────
#
# Scope is the WHOLE canvas package, not just e2e/. Measured on
# 4b6833ed7 (2026-08-08) the package is already error-clean — 512 files,
# 0 errors, 183 warnings — so there is no pre-existing backlog to fix and
# none to silence, and narrowing to e2e/ would buy nothing.
#
# Errors gate; warnings do not. That is not a loophole being left open: the
# config DELIBERATELY sets six rules to "warn" (no-explicit-any,
# no-require-imports, prefer-const, rules-of-hooks, display-name,
# no-unescaped-entities) and 183 findings sit under them today. Turning those
# into build failures is a separate decision about a separate backlog, and
# making it silently here would either break the build or pressure someone
# into bulk-suppression. The waitForFunction guard is "error", so it gates.
# If you want the warnings to gate too, fix the 183 first, then add
# --max-warnings=0 — do not add it before.

echo "── Linting canvas package ──"
REPORT="$WORK/report.json"
ESLINT_STATUS=0
# ONE pass. The JSON report is both the machine-readable input to the
# assertions below and the source the summary is rendered from, so there is no
# second invocation to disagree with the first.
( cd "$CANVAS_DIR" && node "$ESLINT" . --format json --output-file "$REPORT" ) || ESLINT_STATUS=$?

# ──────────────────────────────────────────────────────────────────────
# 2. COVERAGE ASSERTION — the protected files were actually linted.
# ──────────────────────────────────────────────────────────────────────

echo "── Asserting ESLint reached every tracked canvas/e2e/**/*.ts file ──"
git -C "$REPO_ROOT" ls-files -- 'canvas/e2e/**/*.ts' 'canvas/e2e/*.ts' \
  | sed 's|^canvas/||' | sort -u > "$WORK/expected.txt"

EXPECTED_COUNT=$(wc -l < "$WORK/expected.txt" | tr -d ' ')
if [ "$EXPECTED_COUNT" -eq 0 ]; then
  echo "❌ No tracked canvas/e2e/**/*.ts files found. Either the tree moved or"
  echo "   the glob is wrong — refusing to report success over an empty set."
  exit 1
fi

python3 "$REPO_ROOT/.gitea/scripts/canvas_eslint_report.py" \
  --report "$REPORT" \
  --canvas-dir "$CANVAS_DIR" \
  --expected "$WORK/expected.txt"

# ──────────────────────────────────────────────────────────────────────
# 3. NEGATIVE CONTROL — the guard still fires on known-bad shapes.
# ──────────────────────────────────────────────────────────────────────
#
# Shapes below are the recurrence corpus from the #5107 review. The first two
# are what the original selector caught; the rest are the ones that escaped it
# and are covered by the arity form. Keep this list in lockstep with the
# "WHAT IS STILL NOT COVERED" note in canvas/eslint.config.mjs.

echo "── Negative control: the waitForFunction guard must fire on all bad shapes ──"
PROBE_DIR="$CANVAS_DIR/e2e/.eslint-negative-control"
rm -rf "$PROBE_DIR"
mkdir -p "$PROBE_DIR"
# Remove the probe even if a later assertion exits non-zero — it must never be
# left behind in a developer's working tree.
trap 'rm -rf "$WORK" "$PROBE_DIR"' EXIT

cat > "$PROBE_DIR/shapes.ts" <<'PROBE'
/* Generated by .gitea/scripts/canvas-eslint-gate.sh — not committed. */
declare const page: { waitForFunction: (...a: unknown[]) => Promise<void> };
const fn = () => true;
const opts = { timeout: 10_000, polling: 100 };
const rest: unknown[] = [null, opts];
export async function bad1() { await page.waitForFunction(fn, { timeout: 10_000 }); }
export async function bad2() { await page.waitForFunction(fn, { ...opts }); }
export async function bad3() { await page.waitForFunction(fn, opts); }
export async function bad4() { await page.waitForFunction(fn, { timeout: 1 } as const); }
export async function bad5() { await page.waitForFunction(fn, { timeout: 1 } satisfies Record<string, number>); }
export async function bad6() { await page.waitForFunction(fn, ...rest); }
export async function bad7() { const { waitForFunction } = page; await waitForFunction(fn, opts); }
export async function bad8() { await page.waitForFunction(fn); }
export async function bad9() { await page["waitForFunction"](fn, opts); }
export async function bad10() { await page?.waitForFunction?.(fn, opts); }
export async function good1() { await page.waitForFunction(fn, null, { timeout: 10_000, polling: 100 }); }
export async function good2() { await page.waitForFunction(fn, null, opts); }
PROBE

PROBE_REPORT="$WORK/probe.json"
( cd "$CANVAS_DIR" && node "$ESLINT" "e2e/.eslint-negative-control/shapes.ts" \
    --format json --output-file "$PROBE_REPORT" ) || true

python3 "$REPO_ROOT/.gitea/scripts/canvas_eslint_report.py" \
  --negative-control "$PROBE_REPORT" \
  --expect-rule no-restricted-syntax \
  --expect-bad 10 \
  --expect-good 2

echo ""
if [ "$ESLINT_STATUS" -ne 0 ]; then
  echo "🚫 ESLint reported error-severity findings in canvas/ (exit $ESLINT_STATUS)."
  echo "   They are listed above. Fix them; do not add a blanket disable."
  exit 1
fi

echo "✅ Canvas ESLint gate passed: package error-clean, all $EXPECTED_COUNT protected"
echo "   e2e files linted, waitForFunction guard verified firing on 10/10 bad shapes."
