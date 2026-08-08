import { dirname } from "path";
import { fileURLToPath } from "url";
import { FlatCompat } from "@eslint/eslintrc";

const __filename = fileURLToPath(import.meta.url);
const __dirname = dirname(__filename);

const compat = new FlatCompat({
  baseDirectory: __dirname,
});

const eslintConfig = [
  {
    ignores: [
      ".next/**",
      "coverage/**",
      "out/**",
      "build/**",
      "next-env.d.ts",
    ],
  },
  ...compat.extends("next/core-web-vitals", "next/typescript"),
  {
    rules: {
      "@typescript-eslint/no-explicit-any": "warn",
      "@typescript-eslint/no-require-imports": "warn",
      "prefer-const": "warn",
      "react-hooks/rules-of-hooks": "warn",
      "react/display-name": "warn",
      "react/no-unescaped-entities": "warn",
    },
  },
  {
    // Playwright's signature is waitForFunction(pageFunction, arg, options).
    // Called with only TWO arguments, an options object lands in the `arg`
    // slot and `timeout`/`polling` are silently discarded — the wait then runs
    // unbounded until the test-level timeout. It type-checks either way, so
    // nothing else catches it. Measured before the fix in
    // e2e/helpers/canvas.ts: 29937ms under a 30s test timeout and 44947ms
    // under a 45s one, for a call that asked for 10s.
    // Pass the arg explicitly (`null` when unused) to bind the options.
    //
    // ── WHY THIS IS AN ARITY RULE AND NOT A "2nd arg is an object" RULE ──
    // #5107 shipped the narrow form
    //     [arguments.length=2][arguments.1.type="ObjectExpression"]
    // which only sees an options object written INLINE at the call. Measured
    // against a 10-shape corpus, it caught 2 of 10 recurrences. Everything
    // that puts one layer of syntax between the call and the braces escaped
    // it: options held in a variable (the most likely real recurrence —
    // `waitForFunction(fn, opts)`), `{...} as const`, `{...} satisfies T`,
    // `...rest` argument spread, a destructured `const { waitForFunction } =
    // page`, a computed `page["waitForFunction"]`, and the plain 1-argument
    // `waitForFunction(fn)` which is just as unbounded and was never covered.
    // A syntactic selector cannot resolve what an identifier holds, so
    // widening by "is the 2nd argument options-shaped" is unwinnable — no
    // amount of esquery makes `opts` reveal itself, that needs type
    // information (a typed rule with parserOptions.project).
    //
    // Inverting the test removes the need for type information entirely:
    // the CORRECT call is always exactly three arguments, because the arg
    // slot must be filled for the options slot to exist. So require arity 3
    // and every shape above is caught by construction — verified 10/10 on
    // the corpus, with both correct 3-arg forms (inline options and
    // variable options) clean.
    //
    // The trade this makes: a legitimate `waitForFunction(fn, arg)` that
    // deliberately wants the DEFAULT timeout is now flagged too. That is
    // intentional — spelling it `waitForFunction(fn, arg, {})` costs two
    // characters and makes "I meant the default" a written decision rather
    // than something indistinguishable from this bug. There are no such
    // call sites today.
    //
    // ── THE THIRD SLOT: WHO ACTUALLY COVERS IT ──
    // Arity cannot see a 3-argument call whose THIRD argument is not really
    // options — the arg and options TRANSPOSED,
    // `waitForFunction(fn, { timeout: 1 }, null)`. That is the same unbounded
    // wait, and it needs type information.
    //
    // An earlier version of this comment said TypeScript "can and does"
    // reject it and called that slot covered. THAT WAS FALSE, and it was
    // false in exactly the way this whole rule exists to prevent: asserting a
    // guard rather than measuring it. tsconfig.json carries
    // `"exclude": ["node_modules", "e2e", "playwright.config.ts"]`, there is
    // no `typecheck` script, no workflow ran `tsc`, and `playwright test`
    // transpiles via esbuild without type-checking. NOTHING type-checked this
    // tree. A deliberate `const x: number = "nope"` in e2e/helpers/canvas.ts
    // was invisible to every check in the repository.
    //
    // It is true NOW because it was MADE true: canvas/tsconfig.e2e.json
    // type-checks e2e/**, and .gitea/scripts/canvas-static-gate.sh runs it
    // and asserts it reached every e2e file. Measured against Playwright
    // 1.59's types, tsc rejects:
    //   • `waitForFunction(fn, { timeout: 1 }, null)`  → TS2769 (null is not
    //     assignable to PageWaitForFunctionOptions | undefined)
    //   • `waitForFunction(fn, null, ...rest)` where rest is `unknown[]`
    //     → TS2769; and where rest is a typed tuple → TS2554 (arg count).
    //     This closes the one spread shape the arity rule cannot count.
    //
    // ── WHAT IS STILL NOT COVERED (do not assume otherwise) ──
    //  • `waitForFunction(fn, opts, undefined)` — an explicit `undefined` IS
    //    assignable to the optional options parameter, so tsc accepts it, and
    //    the arity is 3 so the rule above accepts it. Closed by the SECOND
    //    selector below rather than left open.
    //  • `waitForFunction(fn, opts, x as any)` / `as never` — a type
    //    assertion defeats the type checker by construction. This is a
    //    general property of `as`, not a special weakness here, but it is the
    //    remaining way to write the transposed shape and have nothing object.
    //  • A wrapper that takes options and forwards them wrongly to
    //    waitForFunction from a file OUTSIDE e2e/** — this config block and
    //    tsconfig.e2e.json are both scoped to e2e/**.
    //  • `page[someVariable](fn, opts)` — a fully dynamic method name.
    // The negative controls in .gitea/scripts/canvas-static-gate.sh pin the
    // covered shapes PER SHAPE (not by count) so a future edit to these
    // selectors cannot silently narrow them back.
    files: ["e2e/**/*.{ts,tsx}"],
    rules: {
      "no-restricted-syntax": [
        "error",
        {
          selector: [
            'CallExpression[callee.property.name="waitForFunction"]:not([arguments.length=3])',
            'CallExpression[callee.property.value="waitForFunction"]:not([arguments.length=3])',
            'CallExpression[callee.name="waitForFunction"]:not([arguments.length=3])',
          ].join(", "),
          message:
            "waitForFunction must be called with all THREE arguments: (pageFunction, arg, options). With two, the options object lands in the `arg` slot and `timeout`/`polling` are silently discarded, so the wait runs unbounded to the test budget instead of throwing. Write waitForFunction(fn, null, { timeout, polling }), or waitForFunction(fn, arg, {}) if you genuinely want the default timeout.",
        },
        {
          // The arity rule is satisfied by `waitForFunction(fn, opts,
          // undefined)`, and so is tsc (`undefined` is assignable to an
          // optional parameter) — so the options slot is present, empty, and
          // unbound. Write `{}` if you mean the default timeout; it is the
          // form the message above already tells you to use.
          //
          // NOTE the `[arguments.2.type="Identifier"]` guard is load-bearing,
          // not decoration. esquery stringifies a missing attribute, so a
          // bare `[arguments.2.name="undefined"]` matches every call whose
          // third argument has no `.name` at all — including the CORRECT
          // `waitForFunction(fn, null, { timeout })`. Verified: without the
          // type pin this selector flagged all four probe shapes instead of
          // one.
          selector: [
            'CallExpression[callee.property.name="waitForFunction"][arguments.2.type="Identifier"][arguments.2.name="undefined"]',
            'CallExpression[callee.property.value="waitForFunction"][arguments.2.type="Identifier"][arguments.2.name="undefined"]',
            'CallExpression[callee.name="waitForFunction"][arguments.2.type="Identifier"][arguments.2.name="undefined"]',
          ].join(", "),
          message:
            "waitForFunction's options argument is an explicit `undefined`, so nothing binds the timeout and the wait is unbounded, the same defect as passing only two arguments. Pass real options, e.g. { timeout, polling }, or `{}` if you genuinely want the default timeout.",
        },
      ],
    },
  },
];

export default eslintConfig;
