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
 * This is not belt-and-braces on top of Resource Timing — it decides two
 * distinctions Resource Timing provably cannot make, both measured in
 * e2e/css-diagnostic.spec.ts:
 *
 *   - a connection REFUSED (the request never reached the server) and a
 *     connection RESET BEFORE HEADERS (it reached a server that then dropped
 *     it) settle to byte-identical CSSOM and timing evidence —
 *     `THREW SecurityError` / `status=0 firstByte=NEVER transfer=0B`. Only
 *     ERR_CONNECTION_REFUSED vs ERR_CONNECTION_RESET tells them apart, and
 *     that IS the fork core#5106 is stuck on.
 *   - a mid-body RST and a graceful FIN that stopped short of Content-Length
 *     both give `status=200` with bytes on the wire and a readable, 0-rule
 *     sheet. ERR_CONNECTION_RESET vs ERR_CONTENT_LENGTH_MISMATCH tells them
 *     apart.
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
const recorded = new WeakMap<Page, RecordedFailure[]>();

/**
 * One `requestfailed` event, kept STRUCTURED rather than pre-rendered to a
 * string. The string form is what humans read; the fields are what the guard
 * below decides on, and a decision that has to re-parse its own prose is a
 * decision waiting to silently stop matching.
 */
export type RecordedFailure = {
  resourceType: string;
  method: string;
  url: string;
  /** Verbatim Chromium error, e.g. "net::ERR_NETWORK_CHANGED". */
  errorText: string;
};

export type NetworkFailureLog =
  | { attached: false }
  | { attached: true; failures: RecordedFailure[] };

/** The one rendering of a failure, used by the diagnostic and by the guard. */
export function formatFailure(f: RecordedFailure): string {
  return `${f.resourceType} ${f.method} ${f.url} -> ${f.errorText}`;
}

/** What the recorder saw for this page, or that it was never attached. */
export function networkFailuresFor(page: Page): NetworkFailureLog {
  const failures = recorded.get(page);
  return failures ? { attached: true, failures } : { attached: false };
}

/* ------------------------------------------------------------------------- *
 * The ASSERTING half.
 *
 * Everything above only RECORDS. Until 2026-08-11 nothing ever read the
 * recording unless some OTHER assertion had already failed, because the only
 * consumer was the message `waitForNodeHittable` throws. That masking is why
 * this defect read as 2 occurrences when it had happened 25 times: measured
 * over 2675 completed `E2E Chat` runs (2026-07-10 → 2026-08-11), 24 runs
 * carried the unstyled-canvas signature and a 25th was erased by a re-run.
 * In every one of the three runs where the recorder existed, the same burst
 * also killed THREE woff2 fonts, and no assertion anywhere noticed.
 *
 * So: judge the recording on EVERY test, and say so on every test.
 * ------------------------------------------------------------------------- */

/**
 * Path prefix of Next.js immutable build output. Everything under it is
 * content-hashed and served by the canvas server itself, so a `net::ERR_*` on
 * one is never something a spec asked for — no spec routes or aborts this
 * prefix (checked across every file in canvas/e2e).
 */
const STATIC_ASSET_PREFIX = "/_next/static/";

/**
 * The one net error a CLIENT causes on purpose. `page.route(...).abort()` and a
 * navigation that cancels an in-flight subresource both surface as
 * ERR_ABORTED, so it is not evidence that anything was wrong with delivery.
 * Reported, never fatal. Every other `net::ERR_*` means the request left and
 * did not come back.
 *
 * Measured before choosing this list: across the 80 `E2E Chat` failure logs
 * still retrievable on 2026-08-11, the ONLY net error class that ever appears
 * is ERR_NETWORK_CHANGED (10 occurrences, all on `/_next/static/**`).
 */
const CLIENT_CAUSED = new Set(["net::ERR_ABORTED"]);

function pathnameOf(url: string): string {
  try {
    return new URL(url).pathname;
  } catch {
    return "";
  }
}

/** Is this a failed request for the app's own immutable build output? */
export function isStaticAssetFailure(f: RecordedFailure): boolean {
  return (
    pathnameOf(f.url).startsWith(STATIC_ASSET_PREFIX) &&
    f.errorText.startsWith("net::ERR_") &&
    !CLIENT_CAUSED.has(f.errorText)
  );
}

export type NetEvidenceVerdict = {
  /** false => the test must be failed. */
  ok: boolean;
  /** Emitted on EVERY test, pass or fail. Grep `[net-evidence]`. */
  report: string;
  /** Set iff `ok` is false. The text the test is failed with. */
  message?: string;
};

/**
 * Decide, from a recording alone, whether this page load is evidence of a
 * delivery failure. Pure and exported so both directions are unit-testable
 * without a browser — see e2e/net-evidence-guard.spec.ts.
 *
 * The whole point is to separate two states the lane could not previously tell
 * apart, and which have OPPOSITE owners:
 *
 *   net::ERR_* on /_next/static/**   -> the asset was served by a server that
 *                                       owns its port; the CONNECTION died.
 *                                       Environmental. Not a product bug.
 *   no net::ERR_*, but the sheet is  -> the asset was NOT served, or was served
 *   readable with 0 rules / a status    empty. A real product bug.
 *
 * An UNATTACHED recorder is neither, and must never be read as the first
 * clause of a clean bill of health: "no failures recorded" for a page nobody
 * was watching is a vacuous pass. It fails.
 */
export function judgeNetEvidence(log: NetworkFailureLog, testTitle: string): NetEvidenceVerdict {
  if (!log.attached) {
    const message =
      "[net-evidence] recorder=NOT-ATTACHED :: " +
      testTitle +
      "\n  No requestfailed recorder was ever installed on this page, so the " +
      "absence of net::ERR_* below is not evidence that nothing failed — it is " +
      "evidence that nobody was watching. Import `test` from " +
      "e2e/helpers/net-evidence instead of @playwright/test.";
    return { ok: false, report: message, message };
  }

  const statics = log.failures.filter(isStaticAssetFailure);
  const header =
    `[net-evidence] recorder=attached failed-requests=${log.failures.length} ` +
    `static-asset-net-errors=${statics.length} :: ${testTitle}`;
  const detail = log.failures.map((f) => `    ${formatFailure(f)}`).join("\n");
  const report = log.failures.length === 0 ? header : `${header}\n${detail}`;
  if (statics.length === 0) return { ok: true, report };

  const message =
    `STATIC_ASSET_NET_ERROR: ${statics.length} same-origin request(s) under ` +
    `${STATIC_ASSET_PREFIX} died at the network layer during this test.\n  ` +
    statics.map(formatFailure).join("\n  ") +
    "\n\n  This is core#5106. The asset WAS being served — this lane's own " +
    "preflight curls it, requires HTTP 200 and greps the rule out of the body " +
    "— and the connection carrying it died mid-flight. Read it as " +
    "ENVIRONMENTAL, not as a product defect:\n" +
    "    net::ERR_* present            -> served, connection died\n" +
    "    absent, sheet readable with 0 -> NOT served: a real product bug\n" +
    "      rules and an HTTP status\n" +
    "  Do NOT paper over this with `retries`. Quote this block, the run id and " +
    "the runner into core#5106 — the whole reason it is asserted rather than " +
    "merely logged is so the rate stays countable.\n\n  Full recording:\n" +
    report;
  return { ok: false, report, message };
}

/**
 * `test` with the network-failure recorder attached to every `page`, AND the
 * verdict on that recording enforced after every test.
 *
 * Drop-in replacement for `import { test, expect } from "@playwright/test"`.
 *
 * Why an auto FIXTURE and not `test.afterEach()`: a hook registered at import
 * time in a shared helper attaches to whichever file's suite happens to be
 * loading when the module is first evaluated. Node caches the module, so the
 * second and every later spec file that imports it re-uses the cached exports
 * and never re-runs the registration — the guard would silently cover exactly
 * one of the six spec files this lane runs. An auto fixture is per-test by
 * construction and cannot be under-attached that way.
 */
export const test = base.extend<{ netEvidenceGuard: void }>({
  page: async ({ page }, use) => {
    const failures: RecordedFailure[] = [];
    recorded.set(page, failures);
    page.on("requestfailed", (request) => {
      // errorText is the raw Chromium net error ("net::ERR_CONNECTION_RESET",
      // "net::ERR_INSUFFICIENT_RESOURCES", "net::ERR_ABORTED", ...). It is the
      // whole point of this recorder, so record it verbatim — never
      // paraphrased, never bucketed.
      failures.push({
        resourceType: request.resourceType(),
        method: request.method(),
        url: request.url(),
        errorText: request.failure()?.errorText ?? "(failure() was null)",
      });
    });
    await use(page);
  },

  // `auto` so it runs for every test in every file that imports this `test`,
  // without the spec having to remember to ask for it. It depends on `page`
  // deliberately: that both guarantees a recorder exists to judge and orders
  // this teardown BEFORE the page is disposed.
  netEvidenceGuard: [
    async ({ page }, use, testInfo) => {
      await use();
      const verdict = judgeNetEvidence(networkFailuresFor(page), testInfo.title);
      // Emitted on EVERY test, pass or fail. This line is the measurement: it
      // turns "how often does core#5106 happen" from an archaeology exercise
      // into `grep -c STATIC_ASSET_NET_ERROR` over the job log.
      process.stdout.write(verdict.report + "\n");
      if (!verdict.ok) {
        await testInfo.attach("net-evidence", {
          body: verdict.message ?? verdict.report,
          contentType: "text/plain",
        });
        throw new Error(verdict.message);
      }
    },
    { auto: true },
  ],
});

export { expect } from "@playwright/test";
