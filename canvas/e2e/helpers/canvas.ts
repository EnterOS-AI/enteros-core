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
 * Robustly select a workspace node on the map. Waits for the node to exist,
 * for the canvas to settle (fitView done, so the node is not under the fixed
 * top chrome), then clicks it. Targeting by `data-testid` keeps the click off
 * the hidden ConciergeShell copy that also renders the workspace name.
 */
export async function clickWorkspaceNode(page: Page, workspaceName: string): Promise<void> {
  await page.waitForSelector(".react-flow__node", { timeout: 10_000 });
  await settleCanvas(page);
  await page.getByTestId(`workspace-node-${workspaceName}`).click();
}
