import { expect, type Page } from "@playwright/test";
import { networkFailuresFor } from "./net-evidence";

/** Enter the Org-map view so the Canvas (React Flow graph) mounts. */
export async function enterMapView(page: Page): Promise<void> {
  const btn = page.getByTestId("nav-map");
  await expect(btn, "rail button nav-map missing").toBeVisible({ timeout: 10_000 });
  await btn.click();
}

/**
 * Wait for the React Flow canvas to stop moving before interacting with a node.
 *
 * React Flow's `fitView` pans/zooms the viewport with a requestAnimationFrame
 * transition (NOT the Web Animations API), so `getAnimations()` alone cannot
 * see it. While it runs, a node's on-screen position slides beneath the fixed
 * top chrome (the topbar `<header>`, the floating toolbar, the sr-only
 * `aria-live` status region), and those overlays intercept the pointer —
 * Playwright then retries the click until the 30s actionability timeout. That
 * was the E2E Chat node-click flake (2026-07-25): whether fitView had settled
 * by hit-test time depended on CI scheduling, so the same beforeEach passed or
 * timed out at random.
 *
 * We poll the `.react-flow__viewport` transform until it holds steady across a
 * few frames (fitView finished), then let any FINITE CSS animations/transitions
 * (e.g. the toolbar's margin-left slide) run to completion. No fixed sleep and
 * no force-click — it waits exactly as long as the canvas is actually moving.
 *
 * This settles the VIEWPORT. It is necessary but NOT sufficient: see
 * `waitForNodeHittable` for the per-node condition a click actually needs.
 */
export async function settleCanvas(page: Page): Promise<void> {
  await page.waitForFunction(
    () => {
      const vp = document.querySelector(".react-flow__viewport") as HTMLElement | null;
      if (!vp) return false;
      // waitForFunction re-runs in the SAME page context each poll, so this
      // per-page scratch state persists across polls to detect a steady value.
      const store = window as unknown as { __rfXf?: string; __rfSteady?: number };
      const cur = vp.style.transform || "";
      if (cur === "") return false; // fitView has not applied a transform yet
      if (store.__rfXf === cur) {
        store.__rfSteady = (store.__rfSteady ?? 0) + 1;
      } else {
        store.__rfXf = cur;
        store.__rfSteady = 0;
      }
      return (store.__rfSteady ?? 0) >= 3; // ~3 stable polls => fitView done
    },
    // The `null` is REQUIRED, not stylistic. Playwright's signature is
    // waitForFunction(pageFunction, arg, options): with only two arguments the
    // options object is taken as the page function's ARG, so `timeout` and
    // `polling` are silently discarded and the wait becomes unbounded —
    // measured at 29937ms under a 30s test timeout and 44947ms under a 45s one
    // (it tracks whatever the test budget is), instead of throwing at 10s.
    // Passing `arg` explicitly is what binds the options.
    null,
    { timeout: 10_000, polling: 100 },
  );
  await page.evaluate(() =>
    Promise.all(
      document
        .getAnimations()
        .filter((a) => a.effect?.getComputedTiming().iterations !== Infinity)
        .map((a) => a.finished.catch(() => {})),
    ),
  );
}

/**
 * Wait for the EXACT precondition a click on a workspace node requires:
 *
 *   1. the node's own bounding box is identical across two consecutive
 *      animation frames (it has stopped moving/resizing), and
 *   2. `document.elementFromPoint()` at the node's centre resolves to the node
 *      itself or one of its descendants (nothing is covering the click point).
 *
 * Why this is not covered by `settleCanvas` alone: `settleCanvas` watches the
 * `.react-flow__viewport` transform and awaits animations ONCE. But the node
 * has its own entrance motion — `app/globals.css` gives `.react-flow__node`
 * a 300ms `node-appear` keyframe animation AND a 350ms `transform` transition
 * on an OVERSHOOTING curve (`--mol-easing-bounce-out`,
 * `cubic-bezier(0.2, 0.8, 0.2, 1.05)`) — and `<Canvas/>` is mount-gated on the
 * map view, so that entrance replays on EVERY entry to the map. A click fired
 * into that window is what Playwright reports as `element is not stable`.
 * The occlusion half is the other observed mode: until `fitView` finishes, the
 * node's centre can sit under the fixed topbar (measured: y = -148 at the
 * instant `.react-flow__node` first exists), so the hit test resolves to
 * `<header>` and Playwright retries until its actionability budget expires.
 *
 * Both are real, observable page conditions, so we wait on THEM — not on a
 * clock, not on a retry, and not by force-clicking (a force-click would fire
 * the event at a point the user could never actually hit, which is exactly the
 * kind of assertion that stops discriminating).
 *
 * On timeout this throws with the measured blocker (the intercepting element,
 * the node box, and whether React Flow's stylesheet is in effect) rather than
 * a bare actionability timeout — the E2E Chat failure on run 622119 was
 * `<rect ...> from <svg data-testid="rf__background">  subtree intercepts
 * pointer events`, which is only POSSIBLE when
 * `@xyflow/react/dist/style.css` (`.react-flow__background { pointer-events:
 * none; z-index: -1 }`) has not been applied to the page. That diagnosis
 * should be in the failure message, not reconstructed afterwards.
 */
export async function waitForNodeHittable(page: Page, testId: string): Promise<void> {
  try {
    await page.waitForFunction(
      (id: string) => {
        const el = document.querySelector(`[data-testid="${id}"]`) as HTMLElement | null;
        if (!el) return false;
        const r = el.getBoundingClientRect();
        if (r.width === 0 || r.height === 0) return false;
        // Per-page scratch state persists across polls (same page context), so
        // comparing against the previous frame detects a settled box. With
        // polling:"raf" these are consecutive animation frames — the same
        // definition of "stable" Playwright's own actionability check uses.
        // Keyed BY testId: two concurrent waits on the same page (e.g. a spec
        // that settles two nodes) would otherwise overwrite each other's
        // previous-frame box and each read the other's, so both could report
        // "settled" on a frame where neither was.
        const store = window as unknown as { __molNodeBox?: Record<string, string> };
        if (!store.__molNodeBox) store.__molNodeBox = {};
        const key = [r.x, r.y, r.width, r.height].map((n) => n.toFixed(2)).join(",");
        const settled = store.__molNodeBox[id] === key;
        store.__molNodeBox[id] = key;
        if (!settled) return false;
        const hit = document.elementFromPoint(r.x + r.width / 2, r.y + r.height / 2);
        return !!hit && (hit === el || el.contains(hit));
      },
      testId,
      { timeout: 15_000, polling: "raf" },
    );
  } catch (waitErr) {
    // The diagnostic below is best-effort: it runs against a page that has
    // just failed a wait, so it can itself throw (navigation, closed context).
    // Never let that swallow the real timeout — that would turn a precise
    // failure into an unrelated one.
    let diag: string;
    try {
      diag = await collectHittabilityDiagnostic(page, testId);
    } catch (diagErr) {
      diag =
        `(diagnostic unavailable: ${diagErr instanceof Error ? diagErr.message : String(diagErr)})\n  ` +
        `original wait failure: ${waitErr instanceof Error ? waitErr.message : String(waitErr)}`;
    }
    throw new Error(
      `workspace node "${testId}" never became hittable (box settled across two frames AND ` +
        `elementFromPoint at its centre resolving to the node) within 15s.\n  ${diag}`,
    );
  }
}

/** Measure WHY a node is not hittable: its box, what is actually at its centre,
 *  and whether React Flow's stylesheet is in effect.
 *
 *  THIS MESSAGE IS EVIDENCE, NOT PROSE. On the recorded core#5106 failures the
 *  Playwright report artifact was not retained, so this string was the only
 *  surviving evidence — and it has now sent investigators the wrong way twice:
 *
 *    1. It appended an instruction to import the rule from
 *       `canvas/src/app/globals.css` — a BUILD fix — while the real failure was
 *       a dead socket. (Removed by #5105.)
 *    2. It described the failure as `dev-server chunk delivery` after this lane
 *       had stopped using `next dev` and moved to a production `node server.js`.
 *       (Removed by #5105; recorded here because the phrase outlived the code in
 *       archived job logs and kept asset-delivery theories alive afterwards.)
 *
 *  A third of the same kind is corrected below: "fix delivery of that asset" is
 *  refuted by this lane's own preflight, which proves the file is served. Every
 *  claim this function makes about a LAYER must be one the evidence in the same
 *  message supports; anything else belongs in the issue, not in the failure.
 *
 *  Split out so the caller can guard it — see waitForNodeHittable. Exported so
 *  e2e/css-diagnostic.spec.ts can drive it against deliberately-broken
 *  stylesheet deliveries and assert what it says; a diagnostic that is only ever
 *  exercised by the failure it describes is a diagnostic nobody has tested. */
export async function collectHittabilityDiagnostic(page: Page, testId: string): Promise<string> {
  const inPage = await page.evaluate((id: string) => {
      const el = document.querySelector(`[data-testid="${id}"]`) as HTMLElement | null;
      const bg = document.querySelector(".react-flow__background") as HTMLElement | null;
      const describe = (n: Element | null) =>
        n
          ? `<${n.tagName.toLowerCase()}${n.id ? ` id="${n.id}"` : ""}` +
            `${n.getAttribute("data-testid") ? ` data-testid="${n.getAttribute("data-testid")}"` : ""}` +
            ` class="${(n.getAttribute("class") ?? "").slice(0, 120)}">`
          : "null";
      if (!el) return `node [data-testid="${id}"] is not in the DOM`;
      const r = el.getBoundingClientRect();
      const hit = document.elementFromPoint(r.x + r.width / 2, r.y + r.height / 2);
      // `.react-flow__background` is `pointer-events: none` in
      // @xyflow/react's stylesheet. If it is not, the stylesheet is missing
      // and EVERY canvas interaction is unreliable — say so explicitly.
      const bgPE = bg ? getComputedStyle(bg).pointerEvents : "(no .react-flow__background)";
      // CSSOM state semantics on Chromium, measured (not assumed) — these are
      // what make the numbers below diagnostic rather than suggestive:
      //
      //   still in flight (headers delayed, body trickled, body STALLED)
      //     -> ABSENT from document.styleSheets, and link.sheet === null
      //   terminally failed at the network/security layer (connection reset,
      //     request aborted, blocked by CSP)
      //     -> PRESENT in document.styleSheets, cssRules THROWS SecurityError
      //   HTTP-level failure (404, 500, empty 200, truncated body)
      //     -> PRESENT, cssRules readable, 0 rules
      //
      // So `total` counts only sheets that have SETTLED, and `readable < total`
      // is proof of a DEAD sheet — never of a slow one. A sheet that is merely
      // still loading lowers `total`; it can never lower `readable`. Do not
      // read a low `readable` as "it hasn't arrived yet".
      //
      // The rule search over those sheets is a THREE-state question, not a
      // boolean, and reporting it as a boolean was a forced false negative
      // (core#5106, run 632484): the enumerator skips a sheet whose `cssRules`
      // throws, so when the rule lives in the sheet that DIED — which is
      // exactly the failure being diagnosed — the search reports
      // "rule present = false", indistinguishable from a build that never
      // shipped the rule at all. The two demand opposite fixes (fix delivery
      // vs. fix the build), so the message must never conflate them.
      //
      //   found                          -> the rule IS in a readable sheet
      //   not found, every sheet readable-> genuinely ABSENT (a build defect)
      //   not found, some sheet unreadable-> UNKNOWN; it may be in the dead one
      let ruleSheets = 0;
      let unreadableSheets = 0;
      let rulePresent = false;
      for (const sheet of Array.from(document.styleSheets)) {
        let rules: CSSRuleList | null = null;
        try {
          rules = sheet.cssRules;
        } catch {
          unreadableSheets++; // terminally failed (or genuinely cross-origin)
          continue;
        }
        if (!rules) continue;
        ruleSheets++;
        for (const r of Array.from(rules)) {
          if (r.cssText.includes(".react-flow__background")) rulePresent = true;
        }
      }
      const ruleVerdict = rulePresent
        ? "PRESENT (found in a readable sheet)"
        : unreadableSheets > 0
          ? `UNKNOWN — not in the ${ruleSheets} readable sheet(s), and ${unreadableSheets} ` +
            "settled sheet(s) could not be enumerated, so the rule may well be in one of " +
            "them. This is NOT evidence the rule is missing from the build."
          : `ABSENT — all ${ruleSheets} settled sheet(s) were readable and none contains ` +
            "it, so the rule really is not in the document (a BUILD defect, not a " +
            "delivery one).";
      // Resource Timing per asset. The CSSOM says a sheet DIED; it cannot say
      // how, and "how" is what core#5106 is stuck on — a healthy server that
      // served this exact file 49s earlier lost one request. These entries are
      // the only IN-PAGE source that separates the shapes, and each shape
      // points at a different layer. Measured, not assumed — see
      // e2e/css-diagnostic.spec.ts, which produces every one of them on
      // purpose:
      //
      //   no entry at all            -> the request has not SETTLED. Entries are
      //                                 buffered on completion, so absence is
      //                                 itself the "still in flight" proof.
      //   status>0 but the body is   -> the server accepted, answered, and began
      //     short of Content-Length     streaming; the connection then broke
      //                                 MID-BODY. An endpoint aborted a live
      //                                 response.
      //   status=0, responseStart=0, -> the connection died BEFORE a single
      //     transferSize=0              response byte: refused/reset at setup,
      //                                 or the client could not get a socket.
      //
      // TWO LIMITS OF THIS CHANNEL, both measured, both the reason the other
      // two channels exist rather than being redundant with this one:
      //
      //   1. It CANNOT tell "the request never reached the server" from "the
      //      server got it and reset before answering". A refused connection
      //      and a reset-before-headers both settle to
      //      status=0/firstByte=NEVER/transferSize=0 — byte-identical. Only
      //      net::ERR_CONNECTION_REFUSED vs net::ERR_CONNECTION_RESET (the
      //      CDP channel, see helpers/net-evidence.ts) and the server probe's
      //      silence vs its "CONNECTION DIED" line separate them. That is
      //      exactly the fork core#5106 is stuck on, so read all three.
      //   2. It cannot tell a mid-body RST from a graceful FIN that stopped
      //      short of Content-Length: both are status=200 with bytes on the
      //      wire and a readable, 0-rule sheet. The net error does —
      //      ERR_CONNECTION_RESET vs ERR_CONTENT_LENGTH_MISMATCH.
      //
      // Buffer capacity is not a hazard here: the resource buffer holds 250
      // entries and drops NEW ones once full (verified — a 400-request flood
      // after the stylesheets left both stylesheet entries intact and the
      // final entries missing). So a late flood can never turn "this sheet
      // died" into "this sheet never settled".
      const timings = new Map<string, string>();
      for (const raw of performance.getEntriesByType("resource")) {
        // `responseStatus` is Chromium-supported but was added to the DOM lib
        // later than the rest of PerformanceResourceTiming, so widen locally
        // rather than pin a lib version for one field.
        const e = raw as PerformanceResourceTiming & { responseStatus?: number };
        timings.set(
          e.name,
          `status=${e.responseStatus ?? "(unavailable)"} ` +
            `start=+${e.startTime.toFixed(0)}ms firstByte=${
              e.responseStart ? `+${e.responseStart.toFixed(0)}ms` : "NEVER"
            } lastByte=+${e.responseEnd.toFixed(0)}ms ` +
            `transfer=${e.transferSize}B encodedBody=${e.encodedBodySize}B ` +
            `proto=${e.nextHopProtocol || "(none)"}`,
        );
      }
      // Per-link inventory. The aggregate counts above cannot distinguish the
      // three settled/unsettled states from each other, and reading them wrong
      // sends the fix at the wrong layer — so record, for every stylesheet-ish
      // link, the five facts that DO separate them: href, rel/as,
      // `link.sheet === null`, whether `cssRules` throws (and with what), and
      // the rule count. `sheet=null` => still loading; `throws` => dead at the
      // network/security layer; `rules=0` => served but empty/truncated.
      const linkInventory = Array.from(document.querySelectorAll("link"))
        .filter(
          (l) =>
            (l.getAttribute("rel") ?? "").includes("stylesheet") ||
            (l.getAttribute("as") ?? "") === "style",
        )
        .map((l) => {
          const rel = l.getAttribute("rel") ?? "(no rel)";
          const as = l.getAttribute("as");
          const href = l.getAttribute("href") ?? "(no href)";
          const sheet = (l as HTMLLinkElement).sheet;
          let state: string;
          if (!sheet) {
            // Only meaningful for rel=stylesheet; a preload never gets a sheet.
            state = rel.includes("stylesheet")
              ? "sheet=null (IN FLIGHT — not yet settled)"
              : "sheet=null (preload; never becomes a sheet on its own)";
          } else {
            try {
              const n = sheet.cssRules.length;
              state =
                n === 0
                  ? "sheet=present cssRules=readable rules=0 (SERVED BUT EMPTY/TRUNCATED)"
                  : `sheet=present cssRules=readable rules=${n}`;
            } catch (e) {
              const err = e as Error;
              // A throw means EITHER the sheet died at the network/security
              // layer OR it is genuinely cross-origin (which is not a defect).
              // Every canvas stylesheet is served same-origin from
              // /_next/static, so resolve the href and say which this is
              // instead of asserting the alarming one.
              let sameOrigin = true;
              try {
                sameOrigin = new URL(l.getAttribute("href") ?? "", location.href).origin === location.origin;
              } catch {
                sameOrigin = false;
              }
              state =
                `sheet=present cssRules=THREW ${err?.name ?? "Error"}: ` +
                `${(err?.message ?? String(e)).slice(0, 90)} ` +
                (sameOrigin
                  ? "(same-origin => DEAD: reset/aborted/CSP-blocked, NOT slow)"
                  : "(CROSS-ORIGIN => unreadable by design, not necessarily a failure)");
            }
          }
          let absolute = href;
          try {
            absolute = new URL(href, location.href).href;
          } catch {
            /* keep the raw attribute */
          }
          const timing =
            timings.get(absolute) ??
            "(no Resource Timing entry — the request has NOT settled)";
          return `${rel}${as ? `(as=${as})` : ""} ${href}\n      ${state}\n      timing: ${timing}`;
        });
      // `elementFromPoint` returns null for a point OUTSIDE the viewport, which
      // is a different finding from "something is covering the node" and was
      // mis-annotated as occlusion on run 632484 (centre computed to
      // (-26, -648)). Say which it is; the two have different causes.
      const cx = r.x + r.width / 2;
      const cy = r.y + r.height / 2;
      const centreInViewport =
        cx >= 0 && cy >= 0 && cx <= window.innerWidth && cy <= window.innerHeight;
      const occludedByBackground = !!hit && !!bg && (hit === bg || bg.contains(hit));
      return [
        `node box = {x:${r.x.toFixed(1)} y:${r.y.toFixed(1)} w:${r.width.toFixed(1)} h:${r.height.toFixed(1)}}`,
        `node centre = (${cx.toFixed(1)}, ${cy.toFixed(1)}) in a ${window.innerWidth}x${window.innerHeight} viewport` +
          (centreInViewport ? "" : "  <-- OUTSIDE the viewport"),
        `hit target at node centre = ${describe(hit)}` +
          (hit
            ? hit === el || el.contains(hit)
              ? "  <-- the node itself: it was hittable at the instant this was measured, " +
                "so the blocker was transient (the box was still moving, or the occluder " +
                "has since gone)"
              : occludedByBackground
                ? "  <-- the React Flow background is on top of the node and is swallowing the click"
                : "  <-- something else is covering the node"
            : centreInViewport
              ? "  <-- nothing at all is at that point inside the viewport"
              : "  <-- null because the centre is OUTSIDE the viewport, NOT because " +
                "anything intercepted the click. The node was panned/zoomed off-screen; " +
                "look at the viewport transform and the node box, not at occlusion."),
        `react-flow background pointer-events = ${bgPE}` +
          (bgPE === "none"
            ? ""
            : "  <-- @xyflow/react/dist/style.css is NOT applied to this page, so the " +
              "React Flow background is pointer-eventful and the canvas geometry is " +
              "unstyled" +
              (occludedByBackground ? " — and it is what the hit test resolved to" : "")),
        `stylesheets = ${document.styleSheets.length} settled (${ruleSheets} readable, ` +
          `${unreadableSheets} unreadable), .react-flow__background rule = ${ruleVerdict}` +
          (unreadableSheets > 0
            ? "  <-- readable < settled: at least one settled stylesheet is " +
              "unreadable. For a SAME-ORIGIN sheet that means DEAD " +
              "(reset/aborted/CSP-blocked) — it is NOT a slow sheet, because a " +
              "still-loading sheet is absent from styleSheets entirely and so " +
              "lowers `settled`, never `readable`. Nothing in the spec can wait " +
              "this out.\n  DO NOT read this as 'the asset is not being served' " +
              "(core#5106): this lane's own preflight curls that exact file, " +
              "requires HTTP 200 AND greps the rule out of the body, and it " +
              "passed ~0.03s after server readiness in every recorded instance " +
              "— then ~33 further page loads over the same origin succeeded " +
              "before this one failed. It is a PER-REQUEST failure on a server " +
              "that owns its port and serves the file. Read the per-link " +
              "inventory below (which sheet, which state), its `timing:` line " +
              "(did a response header ever arrive?), the network failures at the " +
              "end (which net::ERR_*), and canvas.log for the server probe's " +
              "heartbeat and any CONNECTION DIED line."
            : rulePresent && bgPE !== "none"
              ? "  <-- rule IS in the document but not in effect"
              : ""),
        `stylesheet links:\n    ${linkInventory.join("\n    ")}`,
        `viewport transform = ${
          (document.querySelector(".react-flow__viewport") as HTMLElement | null)?.style.transform || "(none)"
        }`,
      ].join("\n  ");
  }, testId);

  // The exact net::ERR_* is not observable from inside the page — see
  // helpers/net-evidence.ts. Append it here, and distinguish "nothing failed"
  // from "nobody was recording": an empty list printed for an unattached page
  // would be a vacuous all-clear in the one message these failures are read
  // from.
  const net = networkFailuresFor(page);
  const netSection = !net.attached
    ? "network failures = (NOT RECORDED — this spec does not import `test` from " +
      "e2e/helpers/net-evidence, so no net::ERR_* was captured. Absence here is not " +
      "evidence that no request failed.)"
    : net.failures.length === 0
      ? "network failures = none (recorder was attached for the whole test)"
      : `network failures (${net.failures.length}, recorder attached):\n    ` +
        net.failures.join("\n    ");
  return `${inPage}\n  ${netSection}`;
}

/**
 * Robustly select a workspace node on the map. Waits for the node to exist,
 * for the canvas to settle (fitView done), and for the node itself to be
 * settled and unoccluded, then clicks it. Targeting by `data-testid` keeps the
 * click off the hidden ConciergeShell copy that also renders the workspace
 * name.
 */
export async function clickWorkspaceNode(page: Page, workspaceName: string): Promise<void> {
  const testId = `workspace-node-${workspaceName}`;
  await page.waitForSelector(".react-flow__node", { timeout: 10_000 });
  await settleCanvas(page);
  await waitForNodeHittable(page, testId);
  await page.getByTestId(testId).click();
}

/**
 * Open a workspace node's context menu. Same preconditions a left click needs
 * — the node settled and unoccluded — because a right click is hit-tested
 * identically; a context-menu click fired into the canvas entrance lands on
 * the React Flow background just as a left click does.
 */
export async function rightClickWorkspaceNode(page: Page, workspaceName: string): Promise<void> {
  const testId = `workspace-node-${workspaceName}`;
  await page.waitForSelector(".react-flow__node", { timeout: 10_000 });
  await settleCanvas(page);
  await waitForNodeHittable(page, testId);
  await page.getByTestId(testId).click({ button: "right" });
}

/**
 * Select a workspace node only if it is not ALREADY selected.
 *
 * The node is a toggle: it carries `aria-pressed`, and clicking a selected node
 * DESELECTS it and closes the SidePanel. `selectedNodeId` survives a top-view
 * change, so after `Settings -> Org map` the panel is already open with
 * `aria-pressed="true"` — an unconditional `clickWorkspaceNode` there closes the
 * panel instead of opening it. Any spec that navigates away from the map and
 * back must use this, not `clickWorkspaceNode`.
 *
 * `aria-pressed` is the app's own published selection state, so this reads the
 * real signal rather than guessing from elapsed time or from the panel's
 * presence.
 */
export async function ensureWorkspaceNodeSelected(page: Page, workspaceName: string): Promise<void> {
  const testId = `workspace-node-${workspaceName}`;
  await page.waitForSelector(".react-flow__node", { timeout: 10_000 });
  await settleCanvas(page);
  const node = page.getByTestId(testId);
  await expect(node, `workspace node ${testId} missing`).toBeVisible({ timeout: 10_000 });
  if ((await node.getAttribute("aria-pressed")) === "true") return;
  await waitForNodeHittable(page, testId);
  await node.click();
  await expect(node, `clicking ${testId} did not select it`).toHaveAttribute(
    "aria-pressed",
    "true",
    { timeout: 10_000 },
  );
}
