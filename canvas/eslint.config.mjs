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
    files: ["e2e/**/*.ts"],
    rules: {
      "no-restricted-syntax": [
        "error",
        {
          selector:
            'CallExpression[callee.property.name="waitForFunction"][arguments.length=2][arguments.1.type="ObjectExpression"]',
          message:
            "waitForFunction(fn, options) silently discards the options: the signature is (pageFunction, arg, options), so a 2nd-argument object is passed to the page function as `arg` and the wait becomes unbounded. Write waitForFunction(fn, null, { timeout, polling }).",
        },
      ],
    },
  },
];

export default eslintConfig;
