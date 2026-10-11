import { test, expect, type Page, type WebSocketRoute } from "@playwright/test";

const root = {
  id: "chat-rendering-root", name: "Org Concierge", role: "platform",
  kind: "platform", tier: 4, status: "online", runtime: "hermes",
  url: "http://localhost:44911", parent_id: null, active_tasks: 0,
  current_task: "", x: 0, y: 0, collapsed: false,
};
const legacyGreeting = "A few things you can try:\n• Research AI news\n• Set up a dev team\n• Show your capabilities";
const markdownGreeting = "A few things you can try:\n\n- Research AI news\n- Set up a dev team\n- Show your capabilities\n\nUse `provision_workspace`.";

// Exercise the real shell and Markdown renderer without changing live chat
// history or contacting an LLM. Failures are returned through the normal API.
async function mockChat(page: Page, content: string, reply: { status: number; body: unknown; socketBody?: unknown } = {
  status: 502, body: { error: "failed to reach workspace agent" },
}) {
  let socket: WebSocketRoute | undefined;
  await page.routeWebSocket("**/ws*", (ws) => { socket = ws; });
  await page.route((url) => !url.pathname.startsWith("/_next/") && (
    url.pathname.startsWith("/workspaces") || url.pathname.startsWith("/cp/") ||
    url.pathname.startsWith("/org/") || url.pathname.startsWith("/settings/") ||
    url.pathname.startsWith("/requests/") || url.pathname === "/templates" ||
    url.pathname === "/connections" || url.pathname === "/canvas/viewport"
  ), async (route) => {
    const path = new URL(route.request().url()).pathname;
    const method = route.request().method();
    let body: unknown = [];
    let status = 200;
    if (path === "/workspaces") body = [root];
    else if (path.endsWith("/chat-history")) body = {
      messages: [{ id: "greeting:agent", role: "agent", content, timestamp: "2026-10-10T20:00:00Z" }],
      reached_end: true,
    };
    else if (path.endsWith("/a2a") && method === "POST") {
      status = reply.status;
      body = reply.body;
      if (reply.socketBody) {
        expect(socket).toBeDefined();
        const messageId = route.request().postDataJSON().params.message.messageId;
        // Match the real platform order: transport activity is broadcast
        // before the A2A body and before the originating POST completes.
        socket!.send(JSON.stringify({
          event: "ACTIVITY_LOGGED", workspace_id: root.id,
          payload: { activity_type: "a2a_receive", method: "message/send", status: "ok", duration_ms: 100, message_id: messageId },
        }));
        socket!.send(JSON.stringify({
          event: "A2A_RESPONSE", workspace_id: root.id,
          payload: { method: "message/send", response_body: reply.socketBody, message_id: messageId },
        }));
      }
    } else if (path === "/org/identity") body = { name: "Chat test", org_id: "", slug: "", platform_managed_available: false };
    else if (path === "/cp/auth/me") status = 401;
    else if (path === `/workspaces/${root.id}`) body = root;
    await route.fulfill({
      status: method === "OPTIONS" ? 204 : status,
      contentType: "application/json",
      headers: {
        "Access-Control-Allow-Origin": route.request().headers().origin ?? "*",
        "Access-Control-Allow-Credentials": "true",
        "Access-Control-Allow-Headers": "*",
        "Access-Control-Allow-Methods": "GET,POST,PUT,PATCH,DELETE,OPTIONS",
      },
      body: method === "OPTIONS" ? "" : JSON.stringify(body),
    });
  });
}

for (const viewport of [{ width: 816, height: 714 }, { width: 773, height: 530 }]) {
  test(`preserves stored line breaks and exposes send failures at ${viewport.width}x${viewport.height}`, async ({ page }) => {
    await page.setViewportSize(viewport);
    await mockChat(page, legacyGreeting);
    await page.goto("/en");
    const chat = page.getByTestId("chat-panel");
    const greeting = chat.locator("p").filter({ hasText: "A few things you can try:" });
    await expect(greeting).toBeVisible();
    // Measure actual line positions; textContent alone would miss collapsed
    // whitespace (the screenshot regression).
    const lineTops = await greeting.evaluate((element) => {
      const node = element.firstChild!;
      const text = node.textContent!;
      return [0, ...Array.from(text.matchAll(/•/g), (match) => match.index!)].map((offset) => {
        const range = document.createRange();
        range.setStart(node, offset);
        range.setEnd(node, offset + 1);
        return range.getBoundingClientRect().top;
      });
    });
    for (let i = 1; i < lineTops.length; i++) expect(lineTops[i]).toBeGreaterThan(lineTops[i - 1]);
    const composer = chat.getByRole("textbox", { name: "Message to agent" });
    await expect(composer).toBeInViewport();
    await composer.fill("hi");
    await chat.getByRole("button", { name: "Send", exact: true }).click();
    const alert = chat.getByRole("alert");
    await expect(alert).toContainText("Failed to send message");
    await expect(alert).toBeInViewport();
    await expect(composer).toBeInViewport();
  });
}

test("renders new greeting examples as a semantic Markdown list", async ({ page }) => {
  await mockChat(page, markdownGreeting);
  await page.goto("/en");
  const chat = page.getByTestId("chat-panel");
  await expect(chat.getByRole("listitem")).toHaveCount(3);
  await expect(chat.locator("code")).toHaveText("provision_workspace");
});

for (const failure of [
  { name: "JSON-RPC error", body: { error: { code: -32603, message: "Provider unavailable" } }, message: "Provider unavailable" },
  { name: "empty completion", body: { result: { status: { state: "completed" } } }, message: "The agent returned no reply" },
  { name: "failed task", body: { result: { status: { state: "failed", message: { parts: [{ kind: "text", text: "Model unavailable" }] } } } }, message: "Model unavailable" },
]) {
  test(`shows ${failure.name} from HTTP 200 and allows another send`, async ({ page }) => {
    await mockChat(page, markdownGreeting, { status: 200, body: failure.body, socketBody: failure.body });
    await page.goto("/en");
    const chat = page.getByTestId("chat-panel");
    const composer = chat.getByRole("textbox", { name: "Message to agent" });
    await composer.fill("hi");
    await chat.getByRole("button", { name: "Send", exact: true }).click();
    await expect(chat.getByRole("alert")).toContainText(failure.message);
    await expect(chat.locator("p").filter({ hasText: failure.message })).toHaveCount(0);
    await expect(composer).toBeEnabled();
    await composer.fill("try again");
    await expect(chat.getByRole("button", { name: "Send", exact: true })).toBeEnabled();
  });
}
