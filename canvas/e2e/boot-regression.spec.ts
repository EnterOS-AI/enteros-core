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
  runPsql,
  cleanupWorkspace,
  type SeededWorkspace,
} from "./fixtures/chat-seed";

/** Enter the Org-map view so the Canvas (React Flow graph) mounts — same
 * helper as chat-desktop.spec.ts (the default view is the concierge shell,
 * where workspace nodes are not clickable). */
async function enterMapView(page: Page): Promise<void> {
  const btn = page.getByTestId("nav-map");
  await expect(btn, "rail button nav-map missing").toBeVisible({ timeout: 10_000 });
  await btn.click();
}

const PLATFORM_URL = process.env.E2E_PLATFORM_URL ?? "http://localhost:8080";

let echo: EchoRuntime;
let ws: SeededWorkspace;

test.beforeAll(async () => {
  echo = await startEchoRuntime();
  ws = await seedWorkspace(echo.baseURL);
});

test.afterAll(async () => {
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
  await page.waitForSelector(".react-flow__node", { timeout: 10_000 });
  await page.getByTestId(`workspace-node-${ws.name}`).click();

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
  await page.waitForSelector(".react-flow__node", { timeout: 10_000 });
  await page.getByTestId(`workspace-node-${ws.name}`).click();
  await page.locator("#tab-chat").click();
  await page.waitForSelector("#panel-chat [data-testid='chat-panel']:visible", {
    timeout: 5_000,
  });

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
  await expect(chat.getByText(message)).toBeVisible({ timeout: 10_000 });
});
