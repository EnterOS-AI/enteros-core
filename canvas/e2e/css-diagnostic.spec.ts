import { createServer, type Server } from "node:http";
import { type AddressInfo, type Socket } from "node:net";
import { expect, type Page } from "@playwright/test";
// Same runner + requestfailed recorder the real specs use, because the
// net::ERR_* half of the diagnostic is only populated when it is attached.
import { test } from "./helpers/net-evidence";
import { collectHittabilityDiagnostic } from "./helpers/canvas";

/**
 * Regression guard for the E2E Chat failure MESSAGE (core#5106).
 *
 * `E2E Chat` fails intermittently with an unstyled canvas because a same-origin
 * `/_next/static/css` chunk dies in flight. The lane's only surviving evidence
 * is the string `waitForNodeHittable` throws — the Playwright report artifact
 * was not even retained on the one captured recurrence — so that string is
 * load-bearing infrastructure. And it was WRONG in a way that misdirects the
 * fix:
 *
 *     .react-flow__background rule present = false
 *
 * was emitted whenever ANY sheet threw, because the enumerator does
 * `catch { continue }` past a dead sheet. When the rule lives in the sheet that
 * died — precisely the observed failure (run 632484) — "not found" was printed
 * as "not there", which reads as a BUILD defect (the rule was never compiled
 * in) rather than a DELIVERY one (it was compiled in and the request died).
 * Those have opposite fixes, so the message must never conflate them.
 *
 * This spec serves stylesheets in every terminal state on purpose and asserts
 * what the diagnostic says about each. It needs no platform, no database and no
 * canvas build: a small http server on an ephemeral port is enough, because the
 * thing under test is the reader, not the app.
 *
 * The delivery lattice, and what each row is evidence OF (all measured here,
 * none assumed — the mapping is the entire reason the diagnostic is worth
 * reading):
 *
 *   shape                CSSOM                  timing                 net error
 *   ------------------   --------------------   --------------------   -------------------------
 *   dead before headers  THROWS SecurityError   status=0 firstByte=    ERR_CONNECTION_RESET
 *                                               NEVER transfer=0       (or _REFUSED — see below)
 *   truncated mid-body   readable, rules=0      status=200, firstByte  ERR_CONNECTION_RESET
 *                                               set, transfer>0
 *   short graceful FIN   readable, rules=0      status=200, firstByte  ERR_CONTENT_LENGTH_MISMATCH
 *                                               set, transfer>0
 *   still in flight      link.sheet === null    no entry (entries      (none yet)
 *                        absent from styleSheets buffer on settle)
 *   served cleanly       readable, with rules   status=200             (none)
 *
 * Read across, not down — no single column separates every row:
 *
 *   - A SecurityError is NOT "the body was cut short". A cut body is
 *     readable-with-0-rules. SecurityError means no usable response arrived at
 *     all, a materially different accusation to level at a server.
 *   - Rows 2 and 3 are identical in CSSOM AND in timing. Only the net error
 *     separates a reset transfer from a server that closed cleanly having sent
 *     less than it promised.
 *   - Row 1 is identical for a connection that was REFUSED (never reached a
 *     server) and one RESET before headers (reached one that dropped it).
 *     Only the net error, plus whether canvas/e2e/support/server-probe.cjs saw
 *     the request at all, separates those — and that is precisely the fork
 *     core#5106 is stuck on.
 */

const RF_RULE = ".react-flow__background{pointer-events:none;z-index:-1}";
const NODE_ID = "workspace-node-Diagnostic Fixture";

type Delivery = {
  /** Which served sheet carries the React Flow rule — or none of them. */
  ruleIn: "aux" | "layout" | "nowhere";
  /** Serve /layout.css as a terminal failure with no response header. */
  killLayout: boolean;
};

type Fixture = { url: string; close: () => Promise<void> };

/**
 * A server that produces, deliberately:
 *   /aux.css       — served cleanly
 *   /layout.css    — served cleanly, or reset before any response byte reaches
 *                    the client (the DEAD sheet: cssRules throws)
 *   /truncated.css — 200 + partial body flushed, then RESET (readable, 0 rules)
 *   /short-fin.css — 200 + partial body flushed, then closed GRACEFULLY —
 *                    same CSSOM and timing as /truncated.css, different net error
 *   /page.css      — headers sent, body never completed (IN FLIGHT forever)
 */
function startFixtureServer(delivery: Delivery): Promise<Fixture> {
  const stalled: Socket[] = [];
  const server: Server = createServer((req, res) => {
    const path = (req.url ?? "/").split("?")[0];
    if (path === "/") {
      res.writeHead(200, { "content-type": "text/html" });
      res.end(
        `<!doctype html><html><head>` +
          `<link rel="stylesheet" href="/aux.css">` +
          `<link rel="stylesheet" href="/layout.css">` +
          `<link rel="stylesheet" href="/truncated.css">` +
          `<link rel="stylesheet" href="/short-fin.css">` +
          `<link rel="stylesheet" href="/page.css">` +
          `<link rel="preload" as="style" href="/page.css">` +
          `</head><body style="margin:0">` +
          `<div class="react-flow__viewport" style="transform:translate(0px, 0px) scale(1)">` +
          `<div data-testid="${NODE_ID}" style="width:300px;height:73px">node</div>` +
          `</div><div class="react-flow__background"></div></body></html>`,
      );
      return;
    }
    if (path === "/aux.css") {
      const body = `.aux{color:blue}${delivery.ruleIn === "aux" ? RF_RULE : ""}`;
      res.writeHead(200, { "content-type": "text/css", "content-length": Buffer.byteLength(body) });
      res.end(body);
      return;
    }
    if (path === "/layout.css") {
      const body = `.layout{color:red}${delivery.ruleIn === "layout" ? RF_RULE : ""}`;
      if (!delivery.killLayout) {
        res.writeHead(200, { "content-type": "text/css", "content-length": Buffer.byteLength(body) });
        res.end(body);
        return;
      }
      // Reset in the SAME tick as writeHead, so nothing is ever flushed to the
      // client. Measured: this is what makes Blink build a sheet with no
      // origin-clean response behind it, which is the state whose `cssRules`
      // throws SecurityError — the exact state observed in CI run 632484.
      res.writeHead(200, { "content-type": "text/css", "content-length": Buffer.byteLength(body) });
      res.socket?.resetAndDestroy();
      return;
    }
    if (path === "/truncated.css") {
      // Headers + a few bytes FLUSHED, then reset later: a genuinely cut
      // transfer. Deliberately contrasted with /layout.css above — same
      // net::ERR_CONNECTION_RESET, completely different CSSOM state.
      const body = ".truncated{color:teal}";
      res.writeHead(200, { "content-type": "text/css", "content-length": Buffer.byteLength(body) + 500 });
      res.write(body.slice(0, 8));
      setTimeout(() => res.socket?.resetAndDestroy(), 40);
      return;
    }
    if (path === "/short-fin.css") {
      // Identical to /truncated.css except the socket is closed GRACEFULLY
      // (FIN, not RST). Chromium produces the same status=200 + bytes-on-wire
      // + readable-0-rule sheet, and a different net error. Kept next to it so
      // the pair is impossible to read as one shape.
      const body = ".shortfin{color:olive}";
      res.writeHead(200, { "content-type": "text/css", "content-length": Buffer.byteLength(body) + 500 });
      res.write(body.slice(0, 8));
      setTimeout(() => res.socket?.end(), 40);
      return;
    }
    if (path === "/page.css") {
      res.writeHead(200, { "content-type": "text/css", "content-length": 4096 });
      res.write(".page{}");
      if (res.socket) stalled.push(res.socket); // never end: stays in flight
      return;
    }
    res.writeHead(404).end();
  });
  return new Promise<Fixture>((resolve) => {
    server.listen(0, "127.0.0.1", () => {
      const { port } = server.address() as AddressInfo;
      resolve({
        url: `http://127.0.0.1:${port}/`,
        close: () =>
          new Promise<void>((done) => {
            for (const s of stalled) s.destroy();
            // `server.close()` alone waits for every idle keep-alive socket the
            // browser still holds — Node's 5s keepAliveTimeout — which would add
            // five seconds of dead time per test for nothing.
            server.closeAllConnections();
            server.close(() => done());
          }),
      });
    });
  });
}

/**
 * Load the fixture page and wait for BOTH deliberately-broken sheets to reach
 * their intended terminal state before reading the diagnostic. This waits on
 * the states themselves — never on a clock — so there is no way to assert
 * against a sheet that simply had not arrived yet.
 */
async function diagnose(page: Page, delivery: Delivery): Promise<string> {
  const fixture = await startFixtureServer(delivery);
  try {
    await page.goto(fixture.url, { waitUntil: "domcontentloaded" });
    await page.waitForFunction(
      (killed: boolean) => {
        const settled = (href: string, want: "readable" | "threw") => {
          const link = document.querySelector(`link[href="${href}"]`) as HTMLLinkElement | null;
          if (!link?.sheet) return false;
          try {
            void link.sheet.cssRules;
            return want === "readable";
          } catch {
            return want === "threw";
          }
        };
        return (
          settled("/layout.css", killed ? "threw" : "readable") &&
          settled("/truncated.css", "readable") &&
          settled("/short-fin.css", "readable")
        );
      },
      delivery.killLayout,
      { timeout: 15_000, polling: 50 },
    );
    return await collectHittabilityDiagnostic(page, NODE_ID);
  } finally {
    await fixture.close();
  }
}

test.describe("canvas hittability diagnostic", () => {
  test("rule inside a DEAD sheet => UNKNOWN, never ABSENT", async ({ page }) => {
    const diag = await diagnose(page, { ruleIn: "layout", killLayout: true });

    // The state that forced the false negative really did occur.
    expect(diag).toContain("cssRules=THREW SecurityError");
    expect(diag).toContain("1 unreadable");
    // ...and the verdict does not claim the rule is missing from the build.
    expect(diag).toContain(".react-flow__background rule = UNKNOWN");
    expect(diag).toContain("NOT evidence the rule is missing from the build");
  });

  test("rule absent from every READABLE sheet, none unreadable => ABSENT", async ({ page }) => {
    // Same page, same sheet count, same stalled page chunk, same truncated
    // sheet. The ONLY difference from the case above is that nothing is
    // unreadable — which is exactly what makes "not found" trustworthy here
    // and untrustworthy there.
    const diag = await diagnose(page, { ruleIn: "nowhere", killLayout: false });

    expect(diag).not.toContain("cssRules=THREW");
    expect(diag).toContain("0 unreadable");
    expect(diag).toContain(".react-flow__background rule = ABSENT");
    expect(diag).toContain("a BUILD defect, not a delivery one");
  });

  test("rule present in a readable sheet => PRESENT", async ({ page }) => {
    const diag = await diagnose(page, { ruleIn: "aux", killLayout: false });
    expect(diag).toContain(".react-flow__background rule = PRESENT");
  });

  test("the delivery shapes are told apart by Resource Timing and net::ERR_*", async ({ page }) => {
    const diag = await diagnose(page, { ruleIn: "layout", killLayout: true });

    // Served cleanly: settled, readable, with a status and a first byte.
    expect(diag).toMatch(/\/aux\.css\n\s+sheet=present cssRules=readable[\s\S]*?\n\s+timing: status=200 start=\+\d+ms firstByte=\+\d+ms/);
    // Dead: no response header ever arrived. status=0 / firstByte=NEVER is what
    // separates this from a cut transfer, and it is the shape CI observed.
    expect(diag).toMatch(/\/layout\.css\n\s+sheet=present cssRules=THREW[\s\S]*?\n\s+timing: status=0 start=\+\d+ms firstByte=NEVER/);
    // Cut transfer: the server DID answer and bytes DID arrive; the sheet is
    // readable and empty, not unreadable. Same net::ERR_CONNECTION_RESET as the
    // dead sheet above — which is exactly why the CSSOM state, not the net
    // error alone, decides what happened.
    expect(diag).toMatch(/\/truncated\.css\n\s+sheet=present cssRules=readable rules=0 \(SERVED BUT EMPTY\/TRUNCATED\)\n\s+timing: status=200 start=\+\d+ms firstByte=\+\d+ms/);
    // Still in flight: no Resource Timing entry is buffered until a load
    // settles, so the absence of an entry is the proof, not a gap in the
    // instrument.
    expect(diag).toMatch(/\/page\.css\n\s+sheet=null \(IN FLIGHT[\s\S]*?\n\s+timing: \(no Resource Timing entry/);
    // A graceful close that stopped short of Content-Length is INDISTINGUISHABLE
    // from the cut transfer above in both the CSSOM and the timing — same
    // status, same first byte, same readable-0-rule sheet.
    expect(diag).toMatch(/\/short-fin\.css\n\s+sheet=present cssRules=readable rules=0 \(SERVED BUT EMPTY\/TRUNCATED\)\n\s+timing: status=200 start=\+\d+ms firstByte=\+\d+ms/);
    // And the exact Chromium error, which no in-page API exposes at all — the
    // ONLY thing that separates the two rows above, and the reason this channel
    // is load-bearing rather than a second opinion.
    expect(diag).toContain("network failures (");
    expect(diag).toMatch(/\/layout\.css -> net::ERR_CONNECTION_RESET/);
    expect(diag).toMatch(/\/truncated\.css -> net::ERR_CONNECTION_RESET/);
    expect(diag).toMatch(/\/short-fin\.css -> net::ERR_CONTENT_LENGTH_MISMATCH/);
  });

  test("a page with no recorder says so, instead of reporting an empty all-clear", async ({ browser }) => {
    // A page created outside the `page` fixture never gets the requestfailed
    // listener. Printing "network failures = none" for it would be a vacuous
    // all-clear in the one message these failures are read from — the reader
    // would conclude no request failed when in fact nobody was watching.
    const context = await browser.newContext();
    const unwatched = await context.newPage();
    const fixture = await startFixtureServer({ ruleIn: "layout", killLayout: true });
    try {
      await unwatched.goto(fixture.url, { waitUntil: "domcontentloaded" });
      const diag = await collectHittabilityDiagnostic(unwatched, NODE_ID);
      expect(diag).toContain("network failures = (NOT RECORDED");
      expect(diag).toContain("Absence here is not evidence that no request failed");
      expect(diag).not.toContain("network failures = none");
    } finally {
      await fixture.close();
      await context.close();
    }
  });

  test("a null hit target OUTSIDE the viewport is not reported as occlusion", async ({ page }) => {
    const fixture = await startFixtureServer({ ruleIn: "aux", killLayout: false });
    try {
      await page.goto(fixture.url, { waitUntil: "domcontentloaded" });
      // Push the node off-screen exactly as a fitView clamped to maxZoom does
      // (run 632484: the centre computed to (-26, -648), and `hit = null` there
      // was annotated as the background swallowing the click).
      await page.evaluate((id: string) => {
        const node = document.querySelector(`[data-testid="${id}"]`) as HTMLElement;
        node.style.position = "absolute";
        node.style.left = "-4000px";
        node.style.top = "-4000px";
      }, NODE_ID);
      const diag = await collectHittabilityDiagnostic(page, NODE_ID);

      expect(diag).toContain("OUTSIDE the viewport");
      expect(diag).toContain("hit target at node centre = null");
      expect(diag).toContain("NOT because anything intercepted the click");
      expect(diag).not.toContain("swallowing the click");
    } finally {
      await fixture.close();
    }
  });
});
