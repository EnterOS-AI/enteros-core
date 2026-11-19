import { defineConfig } from "@playwright/test";

export default defineConfig({
  testDir: "./e2e",
  timeout: 30_000,
  expect: { timeout: 10_000 },
  fullyParallel: false,
  workers: 1,
  retries: 0,
  // Fail CLOSED when an explicit spec selection matches zero tests.
  // Playwright defaults this to true, so `playwright test e2e/chat-*.spec.ts`
  // would exit 0 (green) if those files were renamed/moved/deleted — a
  // false-green that would silently gut the e2e-chat gate after a refactor.
  // forbidOnly likewise stops a stray `test.only` from green-ing the suite
  // while skipping every other case.
  passWithNoTests: false,
  forbidOnly: !!process.env.CI,
  use: {
    baseURL: process.env.PLAYWRIGHT_BASE_URL || "http://localhost:3000",
    headless: true,
    screenshot: "only-on-failure",
    // CI runs Chromium in a separate network namespace. Loopback forwarding
    // keeps the same app origins and reaches per-test HTTP/WebSocket servers.
    connectOptions: process.env.PLAYWRIGHT_WS_ENDPOINT
      ? {
          wsEndpoint: process.env.PLAYWRIGHT_WS_ENDPOINT,
          exposeNetwork: "<loopback>",
        }
      : undefined,
  },
  projects: [
    { name: "chromium", use: { browserName: "chromium" } },
  ],
});
