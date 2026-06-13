import { test, expect } from "@playwright/test";
import type { Page } from "@playwright/test";
import { startEchoRuntime } from "./fixtures/echo-runtime";
import {
  seedWorkspace,
  startHeartbeat,
  cleanupWorkspace,
  seedChatHistory,
} from "./fixtures/chat-seed";

const PLATFORM_URL = process.env.E2E_PLATFORM_URL ?? "http://localhost:8080";
const API = process.env.E2E_API_URL ?? PLATFORM_URL;
const ADMIN_TOKEN = process.env.E2E_ADMIN_TOKEN ?? process.env.ADMIN_TOKEN;

/** Enter the Org-map view so the Canvas (React Flow graph) mounts. */
async function enterMapView(page: Page): Promise<void> {
  const btn = page.getByTestId("nav-map");
  await expect(btn, "rail button nav-map missing").toBeVisible({ timeout: 10_000 });
  await btn.click();
}

/** Open the seeded workspace's Chat side panel. */
async function openChatPanel(page: Page, workspaceName: string): Promise<void> {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.goto("/");
  await enterMapView(page);
  await page.waitForSelector(".react-flow__node", { timeout: 10_000 });

  // Dismiss onboarding guide if present.
  const skipGuide = page.getByText("Skip guide");
  if (await skipGuide.isVisible().catch(() => false)) {
    await skipGuide.click();
  }

  // Scope to the map-side panel (#2587) so we don't accidentally hit the
  // hidden ConciergeShell copy of ChatTab.
  await page.getByTestId(`workspace-node-${workspaceName}`).click();
  await page.locator("#tab-chat").click();
  await page.waitForSelector("#panel-chat [data-testid='chat-panel']:visible", {
    timeout: 5_000,
  });
  await expect(page.locator("#panel-chat textarea").first()).toBeEnabled({
    timeout: 15_000,
  });
}

/** Post a message to the workspace via the A2A proxy so activity rows exist.
 *  `token` should be an org/admin token for canvas-origin rows (source_id NULL),
 *  or the target workspace's own auth token for agent-origin rows
 *  (source_id = workspace_id). */
async function postA2AMessage(workspaceId: string, token: string, text: string) {
  const res = await fetch(`${PLATFORM_URL}/workspaces/${workspaceId}/a2a`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      Authorization: `Bearer ${token}`,
    },
    body: JSON.stringify({
      method: "message/send",
      params: {
        message: {
          role: "user",
          parts: [{ kind: "text", text }],
        },
      },
    }),
  });
  if (!res.ok) {
    throw new Error(`A2A post failed: ${res.status} ${await res.text()}`);
  }
}

test.describe("Chat Sub-Tabs", () => {
  let cleanup: () => Promise<void> = async () => {};
  let workspaceId = "";
  let workspaceName = "";

  test.beforeAll(async () => {
    const echo = await startEchoRuntime();
    const ws = await seedWorkspace(echo.baseURL);
    workspaceId = ws.id;
    workspaceName = ws.name;
    const stopHeartbeat = startHeartbeat(ws.id, ws.authToken);

    cleanup = async () => {
      stopHeartbeat();
      await echo.stop();
    };
  });

  test.afterAll(async () => {
    await cleanupWorkspace(workspaceId);
    await cleanup();
  });

  test.beforeEach(async ({ page }) => {
    await openChatPanel(page, workspaceName);
  });

  test("chat tab shows My Chat and Agent Comms sub-tabs", async ({ page }) => {
    const panel = page.locator("#panel-chat");
    await expect(panel.getByRole("button", { name: "My Chat" })).toBeVisible();
    await expect(panel.getByRole("button", { name: "Agent Comms" })).toBeVisible();
  });

  test("My Chat is selected by default", async ({ page }) => {
    const myChatBtn = page
      .locator("#panel-chat")
      .getByRole("button", { name: "My Chat" });
    await expect(myChatBtn).toHaveAttribute("aria-selected", "true");
  });

  test("switching to Agent Comms shows different content", async ({ page }) => {
    const panel = page.locator("#panel-chat");
    await panel.getByRole("button", { name: "Agent Comms" }).click();

    // Agent Comms should be selected and My Chat's textarea should not be visible.
    await expect(
      panel.getByRole("button", { name: "Agent Comms" }),
    ).toHaveAttribute("aria-selected", "true");
    await expect(panel.locator("textarea").first()).not.toBeVisible();
  });

  test("My Chat has input box, Agent Comms does not", async ({ page }) => {
    const panel = page.locator("#panel-chat");

    // My Chat has the textarea.
    await expect(panel.locator("textarea").first()).toBeVisible();

    // Switch to Agent Comms.
    await panel.getByRole("button", { name: "Agent Comms" }).click();
    await expect(panel.locator("textarea").first()).not.toBeVisible();
  });

  test("switching back to My Chat preserves messages", async ({ page }) => {
    const panel = page.locator("#panel-chat");

    // Send a message so there is content to preserve.
    const textarea = panel.locator("textarea").first();
    await textarea.fill("Persistence check");
    await page.getByRole("button", { name: /Send/ }).first().click();
    await expect(
      panel.getByText("Echo: Persistence check"),
    ).toBeVisible({ timeout: 15_000 });

    // Switch to Agent Comms and back.
    await panel.getByRole("button", { name: "Agent Comms" }).click();
    await panel.getByRole("button", { name: "My Chat" }).click();

    // Message should still be there.
    await expect(panel.getByText("Persistence check", { exact: true })).toBeVisible();
    await expect(panel.getByText("Echo: Persistence check")).toBeVisible();
  });
});

test.describe("Activity API Source Filter", () => {
  let cleanup: () => Promise<void> = async () => {};
  let workspaceId = "";
  let authToken = "";

  test.beforeAll(async () => {
    if (!ADMIN_TOKEN) {
      throw new Error(
        "Activity source-filter tests require E2E_ADMIN_TOKEN or ADMIN_TOKEN to seed canvas-origin rows",
      );
    }

    const echo = await startEchoRuntime();
    const ws = await seedWorkspace(echo.baseURL);
    workspaceId = ws.id;
    authToken = ws.authToken;
    const stopHeartbeat = startHeartbeat(ws.id, ws.authToken);

    // Seed BOTH source classes deterministically:
    //  - admin/org token → callerID is empty → source_id NULL (canvas-origin).
    //  - workspace token → callerID resolves to the workspace → source_id non-null (agent-origin).
    await postA2AMessage(workspaceId, ADMIN_TOKEN, "canvas source probe");
    await postA2AMessage(workspaceId, authToken, "agent source probe");

    cleanup = async () => {
      stopHeartbeat();
      await echo.stop();
    };
  });

  test.afterAll(async () => {
    await cleanupWorkspace(workspaceId);
    await cleanup();
  });

  test("source=canvas returns only canvas-initiated entries", async ({ request }) => {
    const res = await request.get(
      `${API}/workspaces/${workspaceId}/activity?source=canvas`,
    );
    expect(res.ok()).toBeTruthy();
    const entries = (await res.json()) as Array<{ source_id: unknown }>;
    expect(Array.isArray(entries)).toBeTruthy();
    // False-green guard: an empty array would make the loop below pass vacuously.
    expect(entries.length).toBeGreaterThan(0);
    for (const e of entries) {
      expect(e.source_id).toBeNull();
    }
  });

  test("source=agent returns only agent-initiated entries", async ({ request }) => {
    const res = await request.get(
      `${API}/workspaces/${workspaceId}/activity?source=agent`,
    );
    expect(res.ok()).toBeTruthy();
    const entries = (await res.json()) as Array<{ source_id: unknown }>;
    expect(Array.isArray(entries)).toBeTruthy();
    // False-green guard: an empty array would make the loop below pass vacuously.
    expect(entries.length).toBeGreaterThan(0);
    for (const e of entries) {
      expect(e.source_id).not.toBeNull();
    }
  });

  test("source=invalid returns 400", async ({ request }) => {
    const res = await request.get(
      `${API}/workspaces/${workspaceId}/activity?source=bogus`,
    );
    expect(res.status()).toBe(400);
  });

  test("source+type filters combine correctly", async ({ request }) => {
    const res = await request.get(
      `${API}/workspaces/${workspaceId}/activity?type=a2a_receive&source=canvas`,
    );
    expect(res.ok()).toBeTruthy();
    const entries = (await res.json()) as Array<{
      activity_type: string;
      source_id: unknown;
    }>;
    expect(Array.isArray(entries)).toBeTruthy();
    // False-green guard: an empty array would make the loop below pass vacuously.
    expect(entries.length).toBeGreaterThan(0);
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

  test.beforeAll(async () => {
    const echo = await startEchoRuntime();
    const ws = await seedWorkspace(echo.baseURL);
    workspaceId = ws.id;
    workspaceName = ws.name;
    const stopHeartbeat = startHeartbeat(ws.id, ws.authToken);

    // Pre-seed chat history so the My Chat panel shows deterministic content.
    // Include double quotes to regression-test shell-safe JSON quoting in
    // seedChatHistory (CR2 #11517).
    await seedChatHistory(workspaceId, [
      { role: "user", content: 'Hello from seed with "quotes"' },
      { role: "agent", content: 'Hello back from seed with "quotes"' },
    ]);

    cleanup = async () => {
      stopHeartbeat();
      await echo.stop();
    };
  });

  test.afterAll(async () => {
    await cleanupWorkspace(workspaceId);
    await cleanup();
  });

  test.beforeEach(async ({ page }) => {
    await openChatPanel(page, workspaceName);
  });

  test("seeded chat history appears in My Chat", async ({ page }) => {
    const panel = page.locator("#panel-chat");
    await expect(panel.getByText('Hello from seed with "quotes"')).toBeVisible({ timeout: 5_000 });
    await expect(panel.getByText('Hello back from seed with "quotes"')).toBeVisible({ timeout: 5_000 });
  });

  test("My Chat empty state is not shown when history exists", async ({ page }) => {
    const panel = page.locator("#panel-chat");
    await expect(panel.getByText("No messages yet")).not.toBeVisible();
  });
});

test.describe("No JS Errors", () => {
  let cleanup: () => Promise<void> = async () => {};
  let workspaceId = "";
  let workspaceName = "";

  test.beforeAll(async () => {
    const echo = await startEchoRuntime();
    const ws = await seedWorkspace(echo.baseURL);
    workspaceId = ws.id;
    workspaceName = ws.name;
    const stopHeartbeat = startHeartbeat(ws.id, ws.authToken);

    cleanup = async () => {
      stopHeartbeat();
      await echo.stop();
    };
  });

  test.afterAll(async () => {
    await cleanupWorkspace(workspaceId);
    await cleanup();
  });

  test("page loads without errors with chat sub-tabs", async ({ page }) => {
    const errors: string[] = [];
    page.on("pageerror", (err) => errors.push(err.message));

    await openChatPanel(page, workspaceName);

    // Switch between tabs.
    const panel = page.locator("#panel-chat");
    await panel.getByRole("button", { name: "Agent Comms" }).click();
    await panel.getByRole("button", { name: "My Chat" }).click();

    const critical = errors.filter(
      (e) => !e.includes("WebSocket") && !e.includes("favicon") && !e.includes("hydration"),
    );
    expect(critical).toEqual([]);
  });
});
