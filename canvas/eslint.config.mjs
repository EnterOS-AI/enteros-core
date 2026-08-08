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
    // ── WHAT IS STILL NOT COVERED (do not assume otherwise) ──
    //  • A 3-argument call whose THIRD argument is not really options
    //    (e.g. the arg and options transposed). Arity cannot see it — but
    //    TypeScript can and does: the options parameter is typed, so tsc
    //    rejects a non-options value there. That slot is covered by the
    //    type checker, not by this rule.
    //  • A wrapper that takes options and forwards them wrongly to
    //    waitForFunction from a file outside e2e/** (this config block is
    //    scoped to e2e/**/*.ts).
    //  • `page[someVariable](fn, opts)` — a fully dynamic method name.
    // The negative control in .gitea/scripts/canvas-eslint-gate.sh pins the
    // covered shapes so a future edit to this selector cannot silently
    // narrow it back.
    files: ["e2e/**/*.ts"],
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
      ],
    },
  },
];

export default eslintConfig;
