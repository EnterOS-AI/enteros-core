import type { APIResponse, Route } from "@playwright/test";

/**
 * Forward a response fetched by the staging 401 shim without turning normal
 * Playwright teardown into a test failure.
 *
 * A route handler can finish after Playwright has begun closing its page or
 * context. At that point Playwright disposes the fetched APIResponse and
 * route.fulfill throws even though the test assertions already completed.
 * Ignore only Playwright's two exact teardown error classes; every unrelated
 * route failure still fails loudly.
 */
export async function fulfillStagingFetchedResponse(
  route: Route,
  response: APIResponse,
): Promise<void> {
  try {
    await route.fulfill({ response });
  } catch (error) {
    const message = error instanceof Error ? error.message : "";
    const isTeardownRace =
      message.includes("Fetch response has been disposed") ||
      message.includes("Target page, context or browser has been closed");
    if (isTeardownRace) return;
    throw error;
  }
}
