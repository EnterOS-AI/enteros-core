import { createServer, type Server } from "node:http";
import { type AddressInfo } from "node:net";
import { expect } from "@playwright/test";
import {
  isStaticAssetFailure,
  judgeNetEvidence,
  networkFailuresFor,
  test,
  type RecordedFailure,
} from "./helpers/net-evidence";

/**
 * Proof, in BOTH directions, for the guard in helpers/net-evidence.ts.
 *
 * Background (core#5106). The `requestfailed` recorder has existed since
 * 2026-08-08 and RECORDED WITHOUT EVER ASSERTING: its contents were quoted only
 * into the message `waitForNodeHittable` throws, i.e. only once some other
 * assertion had already failed. Measured over 2675 completed `E2E Chat` runs
 * (2026-07-10 → 2026-08-11): 24 runs carried the signature, plus a 25th whose
 * log a re-run erased. In all three runs where the recorder existed, the same
 * burst also killed three woff2 fonts and NOTHING asserted on them.
 *
 * A guard that only speaks when something else already failed is not a guard.
 * So the recording is now judged after every test — and the two ways that could
 * be worthless are both closed here:
 *
 *   1. VACUOUS PASS. A guard that asserts over an empty recorder, or over a
 *      page nobody was recording, reports success having checked nothing. The
 *      unattached case is asserted to FAIL, not to pass quietly (`no
 *      failures recorded` and `no recorder` must never render the same).
 *   2. A GUARD THAT NEVER RUNS. `staticAssetNetError reds a passing test`
 *      below is marked `test.fail()`: its body asserts nothing that can fail,
 *      so the ONLY thing that can produce the expected failure is the guard
 *      firing in teardown. Delete the guard, weaken its predicate, or stop it
 *      attaching to this file, and Playwright reports "Expected to fail, but
 *      passed" and this lane goes red. Verified in both directions on
 *      @playwright/test 1.59.1.
 *
 * `judgeNetEvidence` is pure and exported, so the interesting decisions are
 * asserted here without a browser; the two browser tests prove the WIRING —
 * that a real Chromium `requestfailed` on a real `/_next/static/**` URL
 * reaches that function, and that a clean load does not.
 */

const failure = (url: string, errorText: string, resourceType = "stylesheet"): RecordedFailure => ({
  resourceType,
  method: "GET",
  url,
  errorText,
});

const attached = (...failures: RecordedFailure[]) => ({ attached: true as const, failures });

test.describe("net-evidence guard: what it fires on", () => {
  test("a net::ERR_* on /_next/static/** fails the test", () => {
    const v = judgeNetEvidence(
      attached(failure("http://localhost:48555/_next/static/css/cb112c7bbb18ddba.css", "net::ERR_NETWORK_CHANGED")),
      "t",
    );
    expect(v.ok).toBe(false);
    expect(v.message).toContain("STATIC_ASSET_NET_ERROR");
    expect(v.message).toContain("net::ERR_NETWORK_CHANGED");
    // The message must tell the reader which of the two states this is, because
    // the two have opposite owners and core#5106 spent a month on the wrong one.
    expect(v.message).toContain("served, connection died");
    expect(v.message).toContain("NOT served: a real product bug");
  });

  test("fonts count too — the class that went unnoticed for three runs", () => {
    // Every captured recurrence killed three woff2 fonts alongside the
    // stylesheets. Nothing asserted on them, so they were never evidence.
    const v = judgeNetEvidence(
      attached(
        failure("http://localhost:48555/_next/static/media/b019eb2df708d403-s.p.woff2", "net::ERR_NETWORK_CHANGED", "font"),
      ),
      "t",
    );
    expect(v.ok).toBe(false);
  });

  test("any net::ERR_* class fires, not just the one class seen so far", () => {
    // ERR_NETWORK_CHANGED is the only class this lane has ever produced (10
    // occurrences across the 80 retrievable failure logs on 2026-08-11). The
    // predicate is deliberately NOT pinned to it: pinning would make the guard
    // stop matching the moment the environment changed shape, which is the one
    // moment it would be worth having.
    for (const err of ["net::ERR_CONNECTION_RESET", "net::ERR_INSUFFICIENT_RESOURCES", "net::ERR_FAILED"]) {
      const v = judgeNetEvidence(attached(failure("http://h/_next/static/chunks/x.js", err, "script")), "t");
      expect(v.ok, err).toBe(false);
    }
  });
});

test.describe("net-evidence guard: what it must NOT fire on", () => {
  test("a clean recording passes, and still reports its count", () => {
    const v = judgeNetEvidence(attached(), "t");
    expect(v.ok).toBe(true);
    expect(v.report).toContain("recorder=attached");
    expect(v.report).toContain("failed-requests=0");
    expect(v.report).toContain("static-asset-net-errors=0");
  });

  test("a failure OUTSIDE /_next/static is not this defect", () => {
    // boot-regression.spec.ts aborts **/chat-history** on purpose. A guard that
    // reds on that would be uninstalled within a day.
    const v = judgeNetEvidence(attached(failure("http://h/api/chat-history?x=1", "net::ERR_FAILED", "fetch")), "t");
    expect(v.ok).toBe(true);
  });

  test("ERR_ABORTED on a static asset is client-caused, so it is reported and not fatal", () => {
    // route.abort() and a navigation that cancels an in-flight subresource both
    // surface as ERR_ABORTED. That is the client's own doing, not a delivery
    // failure, and treating it as one would make this lane flakier, not less.
    const v = judgeNetEvidence(attached(failure("http://h/_next/static/css/a.css", "net::ERR_ABORTED")), "t");
    expect(v.ok).toBe(true);
    expect(v.report).toContain("failed-requests=1");
    expect(v.report).toContain("static-asset-net-errors=0");
    expect(v.report).toContain("net::ERR_ABORTED"); // reported, just not fatal
  });

  test("an HTTP-level failure carries no net::ERR_* and so is NOT excused as environmental", () => {
    // The whole diagnostic value of the guard: a sheet that 404s, or 200s
    // empty, produces a readable 0-rule sheet and NO requestfailed event at
    // all. It must fall through to the product-bug branch — never be absorbed
    // into "the connection died".
    const v = judgeNetEvidence(attached(), "t");
    expect(v.ok).toBe(true);
    expect(isStaticAssetFailure(failure("http://h/_next/static/css/a.css", "(failure() was null)"))).toBe(false);
  });
});

test.describe("net-evidence guard: anti-vacuity", () => {
  test("an UNATTACHED recorder fails — it must never read as an all-clear", () => {
    const v = judgeNetEvidence({ attached: false }, "t");
    expect(v.ok).toBe(false);
    expect(v.message).toContain("recorder=NOT-ATTACHED");
    expect(v.message).toContain("not evidence that nothing failed");
  });

  test("the recorder really is attached to the page this suite is given", async ({ page }) => {
    // The counterpart to the unit test above: prove the fixture in this file
    // actually installs the recorder, so the passing verdict every other test
    // in this lane gets is a judgement over a real recording rather than over
    // an absent one.
    const log = networkFailuresFor(page);
    expect(log.attached).toBe(true);
  });
});

/**
 * A server whose page loads exactly one stylesheet under `/_next/static/`,
 * either served cleanly or killed before a single response byte — the shape
 * measured in CI (`status=0 firstByte=NEVER proto=(none)`).
 */
function startServer(kill: boolean): Promise<{ url: string; close: () => Promise<void> }> {
  const server: Server = createServer((req, res) => {
    const path = (req.url ?? "/").split("?")[0];
    if (path === "/") {
      res.writeHead(200, { "content-type": "text/html" });
      res.end(
        `<!doctype html><html><head>` +
          `<link rel="stylesheet" href="/_next/static/css/28d382c4518ff26a.css">` +
          `</head><body>ok</body></html>`,
      );
      return;
    }
    if (path === "/_next/static/css/28d382c4518ff26a.css") {
      const body = ".react-flow__background{pointer-events:none}";
      res.writeHead(200, { "content-type": "text/css", "content-length": Buffer.byteLength(body) });
      if (kill) res.socket?.resetAndDestroy();
      else res.end(body);
      return;
    }
    res.writeHead(404).end();
  });
  return new Promise((resolve) => {
    server.listen(0, "127.0.0.1", () => {
      const { port } = server.address() as AddressInfo;
      resolve({
        url: `http://127.0.0.1:${port}/`,
        close: () =>
          new Promise<void>((done) => {
            server.closeAllConnections();
            server.close(() => done());
          }),
      });
    });
  });
}

test.describe("net-evidence guard: end-to-end through a real Chromium", () => {
  test("a dead /_next/static request reds an otherwise-passing test", async ({ page }) => {
    // EXPECTED TO FAIL, and the failure must come from the guard's teardown and
    // from NOWHERE ELSE — otherwise `test.fail()` is satisfied by the body
    // breaking and the proof is worthless. So the body waits only on the RAW
    // recorder (has any request failed at all?), never on `isStaticAssetFailure`
    // or `judgeNetEvidence`: a mutation to either must leave this body passing,
    // so that the missing teardown failure is what Playwright reports.
    //
    // MEASURED, both mutants, @playwright/test 1.59.1:
    //   isStaticAssetFailure -> false        => "Expected to fail, but passed"
    //   the auto fixture removed from `test` => "Expected to fail, but passed"
    test.fail();
    const server = await startServer(true);
    try {
      await page.goto(server.url, { waitUntil: "domcontentloaded" });
      await expect
        .poll(() => {
          const log = networkFailuresFor(page);
          return log.attached ? log.failures.length : 0;
        }, { timeout: 15_000 })
        .toBeGreaterThan(0);
    } finally {
      await server.close();
    }
  });

  test("the same page served cleanly stays green", async ({ page }) => {
    // The other direction, and the one that matters for trusting the lane: a
    // healthy load of the identical document must not trip the guard. Without
    // this, `test.fail()` above would be satisfied by a guard that fires on
    // everything.
    const server = await startServer(false);
    try {
      await page.goto(server.url, { waitUntil: "load" });
      const sheets = await page.evaluate(() => document.styleSheets.length);
      expect(sheets).toBe(1);
      const log = networkFailuresFor(page);
      expect(log.attached).toBe(true);
      expect(log.attached && log.failures.filter(isStaticAssetFailure)).toEqual([]);
      expect(judgeNetEvidence(log, "t").ok).toBe(true);
    } finally {
      await server.close();
    }
  });
});
