/**
 * Chat Sub-Tabs / Data Flow / Activity Filter / No JS Errors e2e
 * (core#2764).
 *
 * Refactored to use the deterministic E2E seed fixtures (startEchoRuntime,
 * seedWorkspace, startHeartbeat, seedChatHistory, cleanupWorkspace) so the
 * suite is hermetic: every test starts with one external workspace, an
 * echo runtime, and (where useful) pre-seeded chat history. The prior
 * implementation called `/workspaces` and `test.skip()` when none existed
 * — that path silently false-greened on a fresh CI runner with no
 * provisioned tenants.
 *
 * No `test.skip(...)` and no `if (workspaces.length === 0) return;` in
 * this file. Tests fail loud on setup error instead.
 */

import { test, expect } from "@playwright/test";
import type { Page } from "@playwright/test";
import { startEchoRuntime } from "./fixtures/echo-runtime";
import {
  seedWorkspace,
  startHeartbeat,
  cleanupWorkspace,
  seedChatHistory,
} from "./fixtures/chat-seed";

const API = process.env.E2E_API_URL ?? "http://localhost:8080";

/** Enter the Org-map view so the Canvas (React Flow graph) mounts. */
async function enterMapView(page: Page): Promise<void> {
  const btn = page.getByTestId("nav-map");
  await expect(btn, "rail button nav-map missing").toBeVisible({ timeout: 10_000 });
  await btn.click();
}

/** Shared setup: spin up echo runtime, seed a workspace, start heartbeat. */
async function seedExternalWorkspace(): Promise<{
  cleanup: () => Promise<void>;
  workspaceId: string;
  workspaceName: string;
}> {
  const echo = await startEchoRuntime();
  const ws = await seedWorkspace(echo.baseURL);
  const stopHeartbeat = startHeartbeat(ws.id, ws.authToken);
  return {
    cleanup: async () => {
      stopHeartbeat();
      await echo.stop();
    },
    workspaceId: ws.id,
    workspaceName: ws.name,
  };
}

/** Navigate the seeded workspace into the chat sub-tab view. */
async function enterSeededChatTab(page: Page, workspaceName: string): Promise<void> {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.goto("/");
  await enterMapView(page);
  await page.waitForSelector(".react-flow__node", { timeout: 10_000 });
  // Dismiss onboarding guide if present.
  const skipGuide = page.getByText("Skip guide");
  if (await skipGuide.isVisible().catch(() => false)) {
    await skipGuide.click();
  }
  // Click the seeded workspace node by its exact name label (scoped to the
  // React Flow canvas — the hidden ConciergeShell mounts a matching div,
  // so an unscoped getByText .first() can resolve to the invisible concierge
  // copy).
  await page.getByTestId(`workspace-node-${workspaceName}`).click();
  // Click the side-panel Chat tab.
  await page.locator("#tab-chat").click();
  // All chat selectors are scoped to #panel-chat (the map SidePanel tabpanel).
  await page.waitForSelector("#panel-chat [data-testid='chat-panel']:visible", { timeout: 5_000 });
  // Wait for the workspace to flip online and the textarea to be enabled.
  await expect(page.locator("#panel-chat textarea").first()).toBeEnabled({ timeout: 15_000 });
}

test.describe("Chat Sub-Tabs", () => {
  let cleanup: () => Promise<void> = async () => {};
  let workspaceName = "";

  test.beforeAll(async () => {
    const seed = await seedExternalWorkspace();
    cleanup = seed.cleanup;
    workspaceName = seed.workspaceName;
  });

  test.afterAll(async () => {
    await cleanup();
  });

  test.beforeEach(async ({ page }) => {
    await enterSeededChatTab(page, workspaceName);
  });

  test("chat tab shows My Chat and Agent Comms sub-tabs", async ({ page }) => {
    const panel = page.locator("#panel-chat");
    await expect(panel.getByRole("button", { name: "My Chat" })).toBeVisible({ timeout: 5_000 });
    await expect(panel.getByRole("button", { name: "Agent Comms" })).toBeVisible({ timeout: 5_000 });
  });

  test("My Chat is selected by default", async ({ page }) => {
    const panel = page.locator("#panel-chat");
    const myChatBtn = panel.getByRole("button", { name: "My Chat" });
    await expect(myChatBtn).toBeVisible();
    // My Chat sub-tab should have the active styling (border-blue-500).
    await expect(myChatBtn).toHaveClass(/border-blue-500/);
  });

  test("switching to Agent Comms shows different content", async ({ page }) => {
    const panel = page.locator("#panel-chat");
    await panel.getByRole("button", { name: "Agent Comms" }).click();
    // Agent Comms should show its own surface — either the empty state
    // ("No agent-to-agent communications") or any rendered comms rows.
    const empty = panel.getByText("No agent-to-agent communications");
    const commsBubbles = panel.locator("[class*=cyan]");
    const hasEmpty = await empty.isVisible().catch(() => false);
    const hasMessages = (await commsBubbles.count()) > 0;
    expect(hasEmpty || hasMessages).toBeTruthy();
  });

  test("My Chat has input box, Agent Comms does not", async ({ page }) => {
    const panel = page.locator("#panel-chat");
    // My Chat should have a visible textarea for the user input.
    await expect(panel.locator("textarea")).toBeVisible();
    // Switch to Agent Comms.
    await panel.getByRole("button", { name: "Agent Comms" }).click();
    // Agent Comms should NOT have a visible textarea.
    await expect(panel.locator("textarea")).not.toBeVisible();
  });

  test("switching back to My Chat preserves messages", async ({ page }) => {
    const panel = page.locator("#panel-chat");
    // The seeded workspace has no chat history yet, so the "My Chat" view
    // shows the empty state. Capture whether the empty state is present.
    const empty = panel.getByText("No messages yet");
    const hasContentBefore = await empty.isVisible().catch(() => false) ||
      (await panel.locator("[class*=blue-600]").count()) > 0;
    // Switch to Agent Comms and back.
    await panel.getByRole("button", { name: "Agent Comms" }).click();
    await panel.getByRole("button", { name: "My Chat" }).click();
    // Same content state should be there after the round-trip.
    const hasContentAfter = await empty.isVisible().catch(() => false) ||
      (await panel.locator("[class*=blue-600]").count()) > 0;
    expect(hasContentBefore).toBe(hasContentAfter);
  });
});

test.describe("Activity API Source Filter", () => {
  let cleanup: () => Promise<void> = async () => {};
  let workspaceId = "";

  test.beforeAll(async () => {
    const seed = await seedExternalWorkspace();
    cleanup = seed.cleanup;
    workspaceId = seed.workspaceId;
  });

  test.afterAll(async () => {
    await cleanupWorkspace(workspaceId);
    await cleanup();
  });

  test("source=canvas returns only canvas-initiated entries", async ({ request }) => {
    const res = await request.get(`${API}/workspaces/${workspaceId}/activity?source=canvas`);
    expect(res.ok()).toBeTruthy();
    const entries = await res.json();
    expect(Array.isArray(entries)).toBeTruthy();
    for (const e of entries) {
      expect(e.source_id).toBeNull();
    }
  });

  test("source=agent returns only agent-initiated entries", async ({ request }) => {
    const res = await request.get(`${API}/workspaces/${workspaceId}/activity?source=agent`);
    expect(res.ok()).toBeTruthy();
    const entries = await res.json();
    expect(Array.isArray(entries)).toBeTruthy();
    for (const e of entries) {
      if (e.source_id !== undefined) {
        expect(e.source_id).not.toBeNull();
      }
    }
  });

  test("source=invalid returns 400", async ({ request }) => {
    const res = await request.get(`${API}/workspaces/${workspaceId}/activity?source=bogus`);
    expect(res.status()).toBe(400);
  });

  test("source+type filters combine correctly", async ({ request }) => {
    const res = await request.get(
      `${API}/workspaces/${workspaceId}/activity?type=a2a_receive&source=canvas`,
    );
    expect(res.ok()).toBeTruthy();
    const entries = await res.json();
    expect(Array.isArray(entries)).toBeTruthy();
    for (const e of entries) {
      expect(e.activity_type).toBe("a2a_receive");
      expect(e.source_id).toBeNull();
    }
  });
});

test.describe("Data Flow — Initial Prompt in Chat", () => {
  let cleanup: () => Promise<void> = async () => {};
  let workspaceId = "";
  let workspaceName = "";
  const USER_PROMPT = "Hello seeded agent — what is the platform?";
  const AGENT_REPLY = "Echo: Hello seeded agent — what is the platform?";

  test.beforeAll(async () => {
    const seed = await seedExternalWorkspace();
    cleanup = seed.cleanup;
    workspaceId = seed.workspaceId;
    workspaceName = seed.workspaceName;
    // Pre-seed a user message + the canonical agent echo reply so My
    // Chat has content to assert on (without depending on a real LLM
    // round-trip, which would be slow and flaky in CI).
    await seedChatHistory(workspaceId, [
      { role: "user", content: USER_PROMPT },
      { role: "agent", content: AGENT_REPLY },
    ]);
  });

  test.afterAll(async () => {
    await cleanupWorkspace(workspaceId);
    await cleanup();
  });

  test.beforeEach(async ({ page }) => {
    await enterSeededChatTab(page, workspaceName);
  });

  test("user prompt appears in My Chat", async ({ page }) => {
    const panel = page.locator("#panel-chat");
    // My Chat should be the default sub-tab — seeded history must be visible.
    await expect(panel.getByText(USER_PROMPT, { exact: true })).toBeVisible({ timeout: 5_000 });
    await expect(panel.getByText(AGENT_REPLY, { exact: true })).toBeVisible({ timeout: 5_000 });
    // Empty state should NOT be present (we seeded history).
    await expect(panel.getByText("No messages yet")).not.toBeVisible();
  });

  test("user message bubble and agent message bubble both render", async ({ page }) => {
    const panel = page.locator("#panel-chat");
    // User bubbles use the blue styling; agent bubbles use the dark
    // zinc styling. Both must render so the chat-history seed is
    // actually surfacing through the panel.
    const userBubbles = panel.locator('[class*="bg-blue-600"]');
    const agentBubbles = panel.locator('[class*="bg-zinc-800"]');
    expect(await userBubbles.count()).toBeGreaterThan(0);
    expect(await agentBubbles.count()).toBeGreaterThan(0);
  });
});

test.describe("No JS Errors", () => {
  let cleanup: () => Promise<void> = async () => {};
  let workspaceName = "";

  test.beforeAll(async () => {
    const seed = await seedExternalWorkspace();
    cleanup = seed.cleanup;
    workspaceName = seed.workspaceName;
  });

  test.afterAll(async () => {
    await cleanup();
  });

  test("page loads without errors when navigating chat sub-tabs", async ({ page }) => {
    const errors: string[] = [];
    page.on("pageerror", (err) => errors.push(err.message));

    await page.setViewportSize({ width: 1280, height: 800 });
    await page.goto("/");
    await enterMapView(page);
    await page.waitForSelector(".react-flow__node", { timeout: 10_000 });
    const skipGuide = page.getByText("Skip guide");
    if (await skipGuide.isVisible().catch(() => false)) {
      await skipGuide.click();
    }
    await page.getByTestId(`workspace-node-${workspaceName}`).click();
    await page.locator("#tab-chat").click();
    await page.waitForSelector("#panel-chat [data-testid='chat-panel']:visible", { timeout: 5_000 });

    const panel = page.locator("#panel-chat");
    // Switch between sub-tabs to surface any sub-tab transition errors.
    await panel.getByRole("button", { name: "Agent Comms" }).click();
    await panel.getByRole("button", { name: "My Chat" }).click();

    // Filter out the categories of errors we treat as benign in E2E
    // (transient WebSocket reconnects, missing favicon, dev-mode hydration
    // warnings). Anything else is a regression.
    const critical = errors.filter(
      (e) =>
        !e.includes("WebSocket") &&
        !e.includes("favicon") &&
        !e.includes("hydration"),
    );
    expect(critical).toEqual([]);
  });
});
