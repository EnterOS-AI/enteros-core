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
        const store = window as unknown as { __molNodeBox?: string };
        const key = [r.x, r.y, r.width, r.height].map((n) => n.toFixed(2)).join(",");
        const settled = store.__molNodeBox === key;
        store.__molNodeBox = key;
        if (!settled) return false;
        const hit = document.elementFromPoint(r.x + r.width / 2, r.y + r.height / 2);
        return !!hit && (hit === el || el.contains(hit));
      },
      testId,
      { timeout: 15_000, polling: "raf" },
    );
  } catch {
    const diag = await page.evaluate((id: string) => {
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
      return [
        `node box = {x:${r.x.toFixed(1)} y:${r.y.toFixed(1)} w:${r.width.toFixed(1)} h:${r.height.toFixed(1)}}`,
        `hit target at node centre = ${describe(hit)}`,
        `react-flow background pointer-events = ${bgPE}` +
          (bgPE === "none"
            ? ""
            : "  <-- @xyflow/react/dist/style.css is NOT applied to this page; " +
              "the React Flow background is swallowing pointer events"),
        `viewport transform = ${
          (document.querySelector(".react-flow__viewport") as HTMLElement | null)?.style.transform || "(none)"
        }`,
      ].join("\n  ");
    }, testId);
    throw new Error(
      `workspace node "${testId}" never became hittable (box settled across two frames AND ` +
        `elementFromPoint at its centre resolving to the node) within 15s.\n  ${diag}`,
    );
  }
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
