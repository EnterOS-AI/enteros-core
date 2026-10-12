/**
 * Playwright config for staging canvas E2E.
 *
 * Separate from playwright.config.ts (local dev) so:
 *   - globalSetup / globalTeardown don't run for every local `pnpm test`
 *   - Retries + timeouts can be longer (staging is remote + shared)
 *   - baseURL is dynamic (set by globalSetup → STAGING_TENANT_URL)
 *
 * Invoked by the e2e-staging-canvas Gitea Actions workflow:
 *   npx playwright test --config=playwright.staging.config.ts
 */

import { defineConfig } from "@playwright/test";

export default defineConfig({
  testDir: "./e2e",
  // Only the staging-*.spec.ts files run under this config. The smoke +
  // unit specs (chat-separation, filestab-smoke, etc.) stay on the local
  // config so they don't hit staging.
  testMatch: /staging-.*\.spec\.ts/,
  // Global setup provisions the org and a real workspace host; keep the
  // suite budget independent of provider-specific cold-boot latency.
  timeout: 120_000,
  expect: { timeout: 15_000 },
  fullyParallel: false,
  // No blanket retries. Every spec here polls a REAL signal (workspace online,
  // rendered-bubble stability, panel content via expect.poll) with its own
  // bounded safety net, so a failure is deterministic — retry-and-hope would
  // only mask a genuine bug and burn ~6 min each on the single shared staging
  // runner (the greeting + tabs specs each retried 3× at ~6.1m against a
  // deterministic missing-input red, wasting ~18 min per spec). A genuinely
  // transient Chromium page.goto net::ERR_NETWORK_CHANGED gets one bounded
  // in-test navigation retry; every other failure still escapes immediately.
  retries: 0,
  // One worker: the setup provisions exactly one org/workspace, and
  // parallel specs would fight over the shared workspace selector state.
  workers: 1,
  globalSetup: "./e2e/staging-setup.ts",
  globalTeardown: "./e2e/staging-teardown.ts",
  use: {
    // STAGING_TENANT_URL gets written to process.env in global setup, but
    // Playwright resolves baseURL before setup runs. We read it inside
    // each spec instead — don't hard-code here.
    headless: true,
    screenshot: "only-on-failure",
    video: "retain-on-failure",
    trace: "retain-on-failure",
    navigationTimeout: 45_000,
    actionTimeout: 15_000,
  },
  reporter: [
    ["list"],
    ["html", { outputFolder: "playwright-report-staging", open: "never" }],
  ],
  projects: [{ name: "chromium", use: { browserName: "chromium" } }],
});
