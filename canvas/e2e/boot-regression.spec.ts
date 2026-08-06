/**
 * boot-regression.spec.ts — per-PR guards for the two local-dev boot fixes
 * that previously only failed at runtime (PR #4460 / #4461):
 *
 * 1. Dev-mode loopback registration: MOLECULE_ENV=development (this lane's
 *    platform env) must accept a /registry/register whose URL is a loopback
 *    IP literal — the provisioner itself assigns http://127.0.0.1:<port>
 *    advertise URLs on a local host. Regression shape: validateAgentURL
 *    re-blocks loopback → every local boot's NET/Register step 400s until
 *    heartbeat backfill, and the boot screen reds at 7/8. (This suite's own
 *    seed fixture historically bypassed registration via psql because of
 *    that block — this test pins the front door open in dev mode.)
 *
 * 2. Provisioning-phase boot telemetry rendering: a BOOT_STEP posted while
 *    the workspace is `provisioning` (the platform emits step 1
 *    "PWR / Provision compute" during docker provisioning) must surface in
 *    the BootSequenceScreen keycap grid AND the watchdog log. Regression
 *    shape: ingestion, broadcast, canvas store, or BootSequenceScreen stops
 *    carrying pre-runtime steps → first boots regress to minutes of
 *    "waiting for boot telemetry".
 */

import { test, expect } from "@playwright/test";
import type { Page } from "@playwright/test";
import { startEchoRuntime, type EchoRuntime } from "./fixtures/echo-runtime";
import {
  seedWorkspace,
  startHeartbeat,
  runPsql,
  cleanupWorkspace,
  type SeededWorkspace,
} from "./fixtures/chat-seed";
// This spec used to carry its OWN copy of enterMapView and then raw-click the
// workspace node with `page.getByTestId(...).click()`. That bypassed
// helpers/canvas.ts — the helper written specifically to stop clicking a React
// Flow node while it is still animating into place — so every node click here
// raced the canvas entrance and failed as "element is not stable" whenever the
// runner was loaded. One copy of the helper, used by every spec.
import { enterMapView, clickWorkspaceNode, ensureWorkspaceNodeSelected } from "./helpers/canvas";

const PLATFORM_URL = process.env.E2E_PLATFORM_URL ?? "http://localhost:8080";

let echo: EchoRuntime;
let ws: SeededWorkspace;
let stopHeartbeat: (() => void) | undefined;

test.beforeAll(async () => {
  echo = await startEchoRuntime();
  ws = await seedWorkspace(echo.baseURL);
  // The delivery-while-unmounted specs below navigate between top-level views,
  // which takes long enough for the platform's stale sweep to flip an
  // un-heartbeated external workspace out of `online` — the SidePanel then
  // renders the boot screen instead of the tab strip. Keep it genuinely alive
  // rather than papering over the flip afterwards.
  stopHeartbeat = startHeartbeat(ws.id, ws.authToken);
});

test.afterAll(async () => {
  if (stopHeartbeat) stopHeartbeat();
  if (ws) await cleanupWorkspace(ws.id);
  if (echo) await echo.stop();
});

test("dev mode accepts loopback IP registration (front door, no psql bypass)", async () => {
  // A loopback IP LITERAL, not the name-exempt "localhost" — exactly the
  // advertise URL shape the local provisioner assigns.
  const loopbackURL = `http://127.0.0.1:${new URL(echo.baseURL).port}`;
  const res = await fetch(`${PLATFORM_URL}/registry/register`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      Authorization: `Bearer ${ws.authToken}`,
    },
    body: JSON.stringify({
      id: ws.id,
      url: loopbackURL,
      delivery_mode: "push",
      agent_card: {
        name: ws.name,
        url: loopbackURL,
        capabilities: {},
      },
    }),
  });
  const body = await res.text();
  expect(
    res.status,
    `register with loopback IP must succeed in MOLECULE_ENV=development ` +
      `(got ${res.status}: ${body}) — a 400 "blocked address: loopback" here ` +
      `means the validateAgentURL dev carve-out regressed and every local ` +
      `boot will red its NET/Register step again`,
  ).toBe(200);
});

test("provisioning-phase BOOT_STEP renders on the boot screen", async ({ page }) => {
  // Flip the seeded workspace to `provisioning` so the canvas swaps the
  // panel tabs for BootSequenceScreen.
  runPsql(`UPDATE workspaces SET status = 'provisioning' WHERE id = '${ws.id}'`);

  await page.setViewportSize({ width: 1280, height: 800 });
  await page.goto("/");
  await enterMapView(page);
  await clickWorkspaceNode(page, ws.name);

  // Pre-telemetry: the watchdog is attached but idle.
  await expect(page.getByText("waiting for boot telemetry")).toBeVisible({
    timeout: 10_000,
  });

  // Post the exact step the platform's provisioner emits (cmd/server wiring:
  // step 1 of 8, PWR / Provision compute) through the real ingestion path.
  const message = "building hermes runtime image — a first boot can take several minutes";
  const res = await fetch(`${PLATFORM_URL}/workspaces/${ws.id}/boot-event`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      Authorization: `Bearer ${ws.authToken}`,
    },
    body: JSON.stringify({
      step: 1,
      total: 8,
      key: "PWR",
      label: "Provision compute",
      status: "running",
      message,
    }),
  });
  expect(res.status, `boot-event ingestion must accept the provisioning step`).toBe(200);

  // The step must reach the keycap grid AND the watchdog log via the live
  // WebSocket — no reload.
  await expect(page.getByText("Provision compute")).toBeVisible({ timeout: 10_000 });
  await expect(page.getByText(message)).toBeVisible({ timeout: 10_000 });

  // Restore online so afterAll cleanup and other specs see a settled row.
  runPsql(`UPDATE workspaces SET status = 'online' WHERE id = '${ws.id}'`);
});

test("agent /notify delivery reaches the canvas chat (self-initiated reply leg)", async ({ page }) => {
  // The runtime's digest reply-forwarder (workspace-runtime
  // idle_digest/reply_forwarder.py) and send_message_to_user both deliver
  // agent-initiated messages through POST /workspaces/:id/notify. This test
  // guards the platform+canvas half of that chain per pull: a workspace-token
  // notify must land as a chat bubble over the live WebSocket. Regression
  // shape: notify ingestion, broadcast, or ChatTab rendering breaks → every
  // self-initiated agent message (digest replies, proactive updates) silently
  // vanishes while request-response chat still works.
  runPsql(`UPDATE workspaces SET status = 'online' WHERE id = '${ws.id}'`);

  await page.setViewportSize({ width: 1280, height: 800 });
  await page.goto("/");
  await enterMapView(page);
  await clickWorkspaceNode(page, ws.name);
  await page.locator("#tab-chat").click();
  await page.waitForSelector("#panel-chat [data-testid='chat-panel']:visible", {
    timeout: 5_000,
  });

  // The panel has now hydrated from GET /chat-history. From here on, cut that
  // endpoint off so the LIVE WebSocket is the only path left that can put the
  // bubble on screen.
  //
  // Without this the test did not test what it says it tests. useChatHistory
  // runs a background reconcile against /chat-history every
  // RECONCILE_INTERVAL_MS (10_000), so a notify whose live frame never reached
  // this panel still rendered — about 9.9s later, from the DB. Measured on the
  // pre-fix build: the AGENT_MESSAGE frame arrived in the page at +1414ms and
  // the bubble appeared at +11291ms. The assertion budget below is 10s, so the
  // suite was deciding pass/fail on which side of a 10s poll tick the reconcile
  // landed — the E2E Chat flake — while reporting "reaches the canvas chat over
  // the live WebSocket" either way. `reconcile()` swallows fetch errors by
  // design (it is a background safety net), so aborting these requests cannot
  // colour the UI; it only removes the fallback that was masking the live leg.
  await page.route("**/chat-history**", (route) => route.abort());

  const message = `digest reply delivery e2e ${Date.now()}`;
  const res = await fetch(`${PLATFORM_URL}/workspaces/${ws.id}/notify`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      Authorization: `Bearer ${ws.authToken}`,
    },
    body: JSON.stringify({ message }),
  });
  expect(res.status, "workspace-token notify must be accepted").toBe(200);

  const chat = page.locator("#panel-chat [data-testid='chat-panel']:visible");
  await expect(
    chat.getByText(message),
    "a workspace-token notify must reach THIS panel over the live WebSocket " +
      "(/chat-history is blocked, so the 10s reconcile poll cannot deliver it)",
  ).toBeVisible({ timeout: 10_000 });
});


/* ────────────────────────────────────────────────────────────────────────────
 * Delivery while NO chat view is mounted.
 *
 * `agentMessages` in the canvas store is a LIVE HAND-OFF to a mounted chat
 * view, not a durable mailbox. A queue that outlives its consumer gets replayed
 * when a chat view next mounts — on top of the copy that same mount hydrates
 * from GET /chat-history — and the user sees the message twice.
 *
 * This direction had ZERO coverage: the whole lane stayed green (33/33) through
 * a change that duplicated every agent message delivered while the user was
 * looking at another view, because no spec ever left the chat and came back.
 * Both unmount paths are covered — leaving the Org map entirely (Settings), and
 * switching the SidePanel off the chat tab — and each ends at a DIFFERENT
 * remounting consumer (the map SidePanel ChatTab, and the Home view's ChatTab
 * which follows `selectedNodeId`).
 * ──────────────────────────────────────────────────────────────────────────── */

/** Every mounted ChatTab in the document, regardless of view. */
function chatPanels(page: Page) {
  return page.locator("[data-testid='chat-panel']");
}

/** Open the map SidePanel's chat for the seeded workspace.
 *  Uses ensureWorkspaceNodeSelected, NOT clickWorkspaceNode: the node is a
 *  toggle and `selectedNodeId` survives a view change, so re-clicking it after
 *  returning to the map would CLOSE the panel. */
async function openMapChat(page: Page): Promise<void> {
  await enterMapView(page);
  await ensureWorkspaceNodeSelected(page, ws.name);
  await page.locator("#tab-chat").click();
  await page.waitForSelector("#panel-chat [data-testid='chat-panel']:visible", {
    timeout: 10_000,
  });
  await expect(page.locator("#panel-chat textarea").first()).toBeEnabled({ timeout: 15_000 });
}

/** Count chat bubbles carrying EXACTLY this text across the WHOLE document —
 *  deliberately not scoped to the visible panel, so a duplicate rendered into
 *  any other mounted ChatTab is caught too. */
async function countBubbles(page: Page, message: string): Promise<number> {
  return page.evaluate(
    (msg) =>
      Array.from(document.querySelectorAll("p")).filter(
        (el) => el.children.length === 0 && el.textContent?.trim() === msg,
      ).length,
    message,
  );
}

async function notifyWorkspace(message: string): Promise<void> {
  const res = await fetch(`${PLATFORM_URL}/workspaces/${ws.id}/notify`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      Authorization: `Bearer ${ws.authToken}`,
    },
    body: JSON.stringify({ message }),
  });
  expect(res.status, "workspace-token notify must be accepted").toBe(200);
}

/** Deliver `message` with no chat view mounted, and wait for the platform to
 *  have PERSISTED it. The persisted row is the copy the next mount is required
 *  to render, so polling the same endpoint the app reads is the real completion
 *  signal — it cannot pass before the delivery leg has finished. */
async function notifyWhileUnmountedAndPersist(page: Page, message: string): Promise<void> {
  await expect(
    chatPanels(page),
    "no ChatTab may be mounted when the notify is delivered — otherwise this " +
      "tests the live hand-off, not the unmounted path",
  ).toHaveCount(0, { timeout: 10_000 });

  await notifyWorkspace(message);

  await expect
    .poll(
      async () => {
        const res = await fetch(
          `${PLATFORM_URL}/workspaces/${ws.id}/chat-history?limit=10`,
          { headers: { Authorization: `Bearer ${ws.authToken}` } },
        );
        if (!res.ok) return -1;
        const body = (await res.json()) as { messages?: Array<{ content?: string }> };
        return (body.messages ?? []).filter((m) => m.content === message).length;
      },
      {
        message: "the notify must be persisted while every chat view is unmounted",
        timeout: 15_000,
      },
    )
    .toBe(1);
}

/** Require the message to render EXACTLY ONCE — measured at the moment both
 *  delivery paths for this mount have completed, not after the dust settles.
 *
 *  The distinction matters: `useChatHistory`'s 10s reconcile runs
 *  `mergeReconciledMessages`, whose window-free content-identity collapse folds
 *  a duplicated bubble back into its DB twin. So a duplicate is SELF-HEALING
 *  after ~10s — an assertion that merely polls until the count reaches 1 will
 *  sit there and wait the bug out, and report green on a build where the user
 *  plainly sees the message twice for ten seconds. (Measured: exactly that,
 *  12.6s vs 1.9s, on the pre-fix store.)
 *
 *  So the count is taken once `hydration` — the GET /chat-history issued by
 *  THIS mount — has resolved, plus two animation frames for React to commit.
 *  By then the live queue replay (synchronous, on mount) and the hydrated copy
 *  have both been applied, and neither a clock nor a retry is involved. */
async function expectExactlyOneBubble(
  page: Page,
  message: string,
  hydration: Promise<unknown>,
): Promise<void> {
  await hydration;
  await expect(
    page.getByText(message).first(),
    "a notify delivered while every chat view was unmounted must appear after remount",
  ).toBeVisible({ timeout: 15_000 });
  // Two frames = React has committed both delivery paths. A frame callback is
  // a real scheduling signal, not a sleep.
  await page.evaluate(
    () => new Promise<void>((r) => requestAnimationFrame(() => requestAnimationFrame(() => r()))),
  );
  expect(
    await countBubbles(page, message),
    "the message must render exactly ONCE. Two copies means an undrained " +
      "agentMessages queue was replayed on top of the hydrated /chat-history " +
      "copy — the 10s reconcile would eventually collapse them, but the user " +
      "sees the duplicate until it does.",
  ).toBe(1);
}

/** The GET /chat-history this mount is about to issue. Register BEFORE the
 *  action that mounts the chat view. */
function hydrationOf(page: Page): Promise<unknown> {
  return page.waitForResponse(
    (r) => r.url().includes("/chat-history") && r.url().includes(ws.id),
    { timeout: 20_000 },
  );
}

test("notify delivered while on Settings renders ONCE back on the map", async ({ page }) => {
  // The path that hits ORDINARY agents, not just the concierge. Leaving the Org
  // map unmounts <Canvas/>, the SidePanel and therefore this workspace's
  // ChatTab, so the store has no consumer to hand the frame to.
  runPsql(`UPDATE workspaces SET status = 'online' WHERE id = '${ws.id}'`);
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.goto("/");
  await openMapChat(page);

  await page.getByTestId("nav-settings").click();

  const message = `notify while on settings ${Date.now()}`;
  await notifyWhileUnmountedAndPersist(page, message);

  const hydration = hydrationOf(page);
  await openMapChat(page);
  await expectExactlyOneBubble(page, message, hydration);
});

test("notify delivered with the SidePanel off chat renders ONCE in the Home chat", async ({
  page,
}) => {
  // Second unmount path, second remounting consumer. Switching the SidePanel to
  // a non-chat tab unmounts the ChatTab while staying on the map; the message is
  // then delivered with nothing mounted anywhere, and the HOME view's ChatTab —
  // which follows `selectedNodeId`, and which used to double as this
  // workspace's accidental drain from behind a display:none — is what mounts to
  // render it.
  runPsql(`UPDATE workspaces SET status = 'online' WHERE id = '${ws.id}'`);
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.goto("/");
  await openMapChat(page);

  await page.locator("#tab-details").click();

  const message = `notify with panel off chat ${Date.now()}`;
  await notifyWhileUnmountedAndPersist(page, message);

  const hydration = hydrationOf(page);
  await page.getByTestId("nav-home").click();
  await expect(chatPanels(page), "the Home view must mount its ChatTab").toHaveCount(1, {
    timeout: 15_000,
  });
  await expectExactlyOneBubble(page, message, hydration);
});
