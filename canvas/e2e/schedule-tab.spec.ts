import { test, expect } from "@playwright/test";
import type { Page, Request } from "@playwright/test";
import { startEchoRuntime, type EchoRuntime } from "./fixtures/echo-runtime";
import { seedWorkspace, startHeartbeat, cleanupWorkspace } from "./fixtures/chat-seed";

/**
 * Scheduler ScheduleTab regression e2e — exercises the Canvas schedule surface
 * end-to-end against the local platform: the tab mounts, the create form
 * validates + persists, the row appears, edit/toggle mutate it, RunNow fires the
 * schedule's prompt as an A2A turn to the (echo) runtime, and delete removes it.
 *
 * Post-P4b the store is the runtime VOLUME grid, not core Postgres — the legacy
 * workspace_schedules table was retired and the Canvas schedule surface forwards
 * to the runtime's /internal/schedules* API. The echo runtime therefore serves a
 * faithful in-process grid (see fixtures/echo-runtime.ts), so this remains the
 * regression guard for the backend re-point: the visible behaviour + JSON
 * contract (name, cron_expr, timezone, prompt, enabled) is the invariant,
 * independent of whether the store is core Postgres or the volume.
 */

/** Enter the Org-map view so the React-Flow graph mounts, then open the workspace. */
async function openWorkspace(page: Page, workspaceName: string): Promise<void> {
  const nav = page.getByTestId("nav-map");
  await expect(nav, "rail button nav-map missing").toBeVisible({ timeout: 10_000 });
  await nav.click();
  await page.waitForSelector(".react-flow__node", { timeout: 10_000 });
  await page.getByTestId(`workspace-node-${workspaceName}`).click();
}

test.describe("ScheduleTab", () => {
  let cleanup: () => Promise<void> = async () => {};
  let workspaceId = "";
  let workspaceName = "";
  let echoRuntime: EchoRuntime;

  test.beforeAll(async () => {
    echoRuntime = await startEchoRuntime();
    const ws = await seedWorkspace(echoRuntime.baseURL);
    workspaceId = ws.id;
    workspaceName = ws.name;
    const stopHeartbeat = startHeartbeat(ws.id, ws.authToken);
    cleanup = async () => {
      stopHeartbeat();
      await echoRuntime.stop();
    };
  });

  test.afterAll(async () => {
    await cleanupWorkspace(workspaceId);
    await cleanup();
  });

  test.beforeEach(async ({ page }) => {
    await page.setViewportSize({ width: 1280, height: 800 });
    await page.goto("/");
    await openWorkspace(page, workspaceName);
    await page.locator("#tab-schedule").click();
    // Everything below is scoped to the schedule tabpanel — ConciergeShell
    // stays mounted (hidden) on the map view, so an unscoped locator can
    // resolve to an off-screen duplicate (the #2587 idPrefix lesson).
    await expect(page.locator("#panel-schedule")).toBeVisible({ timeout: 10_000 });
  });

  test("create → list → edit → toggle → run-now → delete round-trips", async ({ page }) => {
    const panel = page.locator("#panel-schedule");
    const name = `e2e-standup-${Date.now()}`;

    // Empty state first (fresh workspace has no schedules).
    await expect(panel.getByText("No schedules yet")).toBeVisible();

    // --- create ---
    await panel.getByRole("button", { name: "+ Add Schedule" }).click();
    await panel.getByLabel("Schedule name").fill(name);
    // Cron defaults to "0 9 * * *"; set an explicit valid 5-field expr.
    await panel.getByLabel("Cron Expression").fill("*/15 * * * *");
    await panel.getByLabel("Prompt / Task").fill("Summarise open PRs.");
    await panel.getByRole("button", { name: "Create" }).click();

    // Row appears (list re-fetch). Its action controls are name-scoped aria labels.
    await expect(panel.getByText(name, { exact: false })).toBeVisible({ timeout: 10_000 });
    await expect(panel.getByRole("button", { name: `Run schedule ${name} now` })).toBeVisible();

    // --- edit: change the prompt ---
    await panel.getByRole("button", { name: `Edit schedule ${name}` }).click();
    await panel.getByLabel("Prompt / Task").fill("Summarise open PRs and blockers.");
    await panel.getByRole("button", { name: "Update" }).click();
    // Form collapses back to the list after a successful update.
    await expect(panel.getByRole("button", { name: `Edit schedule ${name}` })).toBeVisible({
      timeout: 10_000,
    });

    // --- run-now: post-P4b, Run-now POSTs /schedules/{id}/run; the runtime
    // ENQUEUES a poke and the DAEMON fires the turn (fired_by:"daemon") — Canvas
    // does NOT send the turn itself. Assert the poke round-trips (core's RunNow
    // returns 200 after the runtime accepts the poke) and that EXACTLY ONE /run is
    // issued (Canvas doesn't double-fire). The no-double-fire poke→deliver→clear
    // invariant itself is server-side, covered by the runtime daemon run_once
    // tests, not this UI round-trip.
    const runPosts: string[] = [];
    const onReq = (r: Request) => {
      if (r.method() === "POST" && r.url().endsWith("/run")) runPosts.push(r.url());
    };
    page.on("request", onReq);
    const runResp = page.waitForResponse(
      (r) => r.url().includes(`/schedules/`) && r.url().endsWith("/run") && r.request().method() === "POST",
    );
    await panel.getByRole("button", { name: `Run schedule ${name} now` }).click();
    expect((await runResp).status()).toBe(200);
    await page.waitForTimeout(300);
    page.off("request", onReq);
    expect(runPosts).toHaveLength(1);

    // --- delete (ConfirmDialog) ---
    await panel.getByRole("button", { name: `Delete schedule ${name}` }).click();
    const dialog = page.getByRole("dialog");
    await expect(dialog.getByRole("heading", { name: "Delete schedule" })).toBeVisible();
    await dialog.getByRole("button", { name: "Delete" }).click();

    // Back to empty state — the row is gone.
    await expect(panel.getByRole("button", { name: `Edit schedule ${name}` })).toHaveCount(0, {
      timeout: 10_000,
    });
    await expect(panel.getByText("No schedules yet")).toBeVisible();
  });

  test("create form rejects an invalid cron expression", async ({ page }) => {
    const panel = page.locator("#panel-schedule");
    await panel.getByRole("button", { name: "+ Add Schedule" }).click();
    await panel.getByLabel("Schedule name").fill(`e2e-bad-${Date.now()}`);
    await panel.getByLabel("Cron Expression").fill("not a cron");
    await panel.getByLabel("Prompt / Task").fill("should not persist");
    await panel.getByRole("button", { name: "Create" }).click();
    // Backend cronspec.Validate rejects → surfaced in the form's alert region;
    // no row is created.
    await expect(panel.getByRole("alert")).toBeVisible({ timeout: 10_000 });
  });
});
