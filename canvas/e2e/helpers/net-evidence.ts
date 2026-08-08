import { test as base, type Page } from "@playwright/test";

/**
 * Record the EXACT network error behind a failed request, for the canvas
 * diagnostic to quote.
 *
 * core#5106: a `<link rel="stylesheet">` for a same-origin `/_next/static/css`
 * chunk ends up in `document.styleSheets` with `cssRules` throwing
 * `SecurityError`. Measured (see e2e/css-diagnostic.spec.ts): that state means
 * the load terminated at the network layer — Blink builds the sheet from a
 * response that is not CORS-same-origin, and a failed load has no response at
 * all. So the CSSOM tells us the sheet died, and cannot tell us HOW.
 *
 * `net::ERR_*` is the missing datum, and Chromium only exposes it through the
 * CDP `Network.loadingFailed` event, which Playwright surfaces as
 * `page.on("requestfailed") -> request.failure().errorText`. There is no
 * in-page equivalent: Resource Timing carries the shape of the failure (see the
 * diagnostic in helpers/canvas.ts) but never its cause, and a `<link>` error
 * event carries nothing at all.
 *
 * The listener must be attached BEFORE the navigation that loads the
 * stylesheets, so it cannot be attached from inside a helper that only runs
 * after `page.goto`. Hence a fixture: specs opt in by importing `test` from
 * here instead of from `@playwright/test`, and every page they create records
 * automatically.
 */

/**
 * Presence in this map is itself meaningful: an ABSENT page means no recorder
 * was ever attached, which is NOT the same as "no request failed". The
 * diagnostic prints those two cases differently — reporting "no failures" for a
 * page nobody was watching would be a vacuous pass in the one message this
 * lane's failures are read from.
 */
const recorded = new WeakMap<Page, string[]>();

export type NetworkFailureLog =
  | { attached: false }
  | { attached: true; failures: string[] };

/** What the recorder saw for this page, or that it was never attached. */
export function networkFailuresFor(page: Page): NetworkFailureLog {
  const failures = recorded.get(page);
  return failures ? { attached: true, failures } : { attached: false };
}

/**
 * `test` with the network-failure recorder attached to every `page`.
 * Drop-in replacement for `import { test, expect } from "@playwright/test"`.
 */
export const test = base.extend({
  page: async ({ page }, use) => {
    const failures: string[] = [];
    recorded.set(page, failures);
    page.on("requestfailed", (request) => {
      // errorText is the raw Chromium net error ("net::ERR_CONNECTION_RESET",
      // "net::ERR_INSUFFICIENT_RESOURCES", "net::ERR_ABORTED", ...). It is the
      // whole point of this recorder, so record it verbatim — never
      // paraphrased, never bucketed.
      failures.push(
        `${request.resourceType()} ${request.method()} ${request.url()} ` +
          `-> ${request.failure()?.errorText ?? "(failure() was null)"}`,
      );
    });
    await use(page);
  },
});

export { expect } from "@playwright/test";
