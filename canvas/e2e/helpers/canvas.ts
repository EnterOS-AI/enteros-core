import { expect, type Page } from "@playwright/test";

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
 *  and whether React Flow's stylesheet is in effect. Split out so the caller can
 *  guard it — see waitForNodeHittable. */
async function collectHittabilityDiagnostic(page: Page, testId: string): Promise<string> {
  return page.evaluate((id: string) => {
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
      let ruleSheets = 0;
      let rulePresent = false;
      for (const sheet of Array.from(document.styleSheets)) {
        let rules: CSSRuleList | null = null;
        try {
          rules = sheet.cssRules;
        } catch {
          continue; // terminally failed (or genuinely cross-origin)
        }
        if (!rules) continue;
        ruleSheets++;
        for (const r of Array.from(rules)) {
          if (r.cssText.includes(".react-flow__background")) rulePresent = true;
        }
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
          return `${rel}${as ? `(as=${as})` : ""} ${href}\n      ${state}`;
        });
      return [
        `node box = {x:${r.x.toFixed(1)} y:${r.y.toFixed(1)} w:${r.width.toFixed(1)} h:${r.height.toFixed(1)}}`,
        `hit target at node centre = ${describe(hit)}`,
        `react-flow background pointer-events = ${bgPE}` +
          (bgPE === "none"
            ? ""
            : "  <-- @xyflow/react/dist/style.css is NOT applied to this page; " +
              "the React Flow background is swallowing pointer events"),
        `stylesheets = ${document.styleSheets.length} settled (${ruleSheets} readable), ` +
          `.react-flow__background rule present = ${rulePresent}` +
          (ruleSheets < document.styleSheets.length
            ? "  <-- readable < settled: at least one settled stylesheet is " +
              "unreadable. For a SAME-ORIGIN sheet that means DEAD " +
              "(reset/aborted/CSP-blocked) — it is NOT a slow sheet, because a " +
              "still-loading sheet is absent from styleSheets entirely and so " +
              "lowers `settled`, never `readable`. Read the per-link inventory " +
              "below for which sheet and which state, then fix delivery of that " +
              "asset; nothing in the spec can wait this out."
            : rulePresent
              ? "  <-- rule IS in the document but not in effect"
              : ""),
        `stylesheet links:\n    ${linkInventory.join("\n    ")}`,
        `viewport transform = ${
          (document.querySelector(".react-flow__viewport") as HTMLElement | null)?.style.transform || "(none)"
        }`,
      ].join("\n  ");
  }, testId);
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
