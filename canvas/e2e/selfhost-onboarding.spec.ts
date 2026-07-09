import { test, expect } from "@playwright/test";
import type { Page, Route } from "@playwright/test";

/**
 * Self-host onboarding scene e2e — the §10.2 A/B/C scenarios that are
 * drivable against a local canvas (design SSOT
 * molecule-selfhost-onboarding-scene, tracking molecule-core#3496).
 *
 * The workspace-server API is mocked at the network layer (route
 * interception), so the spec is deterministic against ANY local stack state:
 * it needs only the canvas dev server at PLAYWRIGHT_BASE_URL (default
 * http://localhost:3000 — same convention as chat-desktop.spec.ts) and never
 * mutates a live backend. Wire-order (A5) is asserted from the intercepted
 * requests themselves; provision progress is driven by flipping the mocked
 * /workspaces row (the scene's 5s polling fallback picks it up — the
 * websocket is simply unreachable under interception, which is exactly the
 * fallback path's contract).
 */

const ROOT_ID = "platform-root-e2e";

const TEMPLATES = [
  {
    id: "tpl-claude",
    name: "Claude Code",
    runtime: "claude-code",
    registry_backed: true,
    registry_providers: [
      {
        name: "anthropic-api",
        display_name: "Anthropic API",
        auth_env: ["ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"],
      },
      { name: "minimax", display_name: "MiniMax", auth_env: ["MINIMAX_API_KEY"] },
    ],
    registry_models: [
      { id: "claude-opus-4-7", name: "Claude Opus 4.7", provider: "anthropic-api" },
      { id: "claude-sonnet-4-6", name: "Claude Sonnet 4.6", provider: "anthropic-api" },
      { id: "MiniMax-M2.7", provider: "minimax" },
    ],
  },
  {
    id: "tpl-codex",
    name: "Codex",
    runtime: "codex",
    registry_backed: true,
    registry_providers: [
      { name: "openai-api", display_name: "OpenAI", auth_env: ["OPENAI_API_KEY"] },
    ],
    registry_models: [{ id: "gpt-5.4", provider: "openai-api" }],
  },
  // displayable:false must NEVER surface in the runtime dropdown (A4).
  { id: "tpl-hidden", name: "Hidden", runtime: "crewai", displayable: false },
];

interface RecordedCall {
  method: string;
  path: string;
  body: unknown;
}

interface MockState {
  rootStatus: string;
  rootRuntime: string;
  lastSampleError: string;
  secrets: Array<{ key: string; has_value: boolean }>;
  calls: RecordedCall[];
  /** What an ensure POST does to the mocked root, after a short delay. */
  onEnsure: (state: MockState) => void;
}

function makeState(overrides: Partial<MockState> = {}): MockState {
  return {
    rootStatus: "offline",
    rootRuntime: "claude-code",
    lastSampleError: "",
    secrets: [],
    calls: [],
    onEnsure: (state) => {
      state.rootStatus = "online";
    },
    ...overrides,
  };
}

function rootRow(state: MockState) {
  return {
    id: ROOT_ID,
    name: "Org Concierge",
    role: "platform",
    tier: 4,
    status: state.rootStatus,
    agent_card: null,
    url: "",
    parent_id: null,
    kind: "platform",
    active_tasks: 0,
    last_error_rate: 0,
    last_sample_error: state.lastSampleError,
    uptime_seconds: 0,
    current_task: "",
    runtime: state.rootRuntime,
    x: 0,
    y: 0,
    collapsed: false,
    budget_limit: null,
  };
}

/** Fulfill with JSON + permissive CORS (the canvas calls the platform origin
 *  cross-origin with credentials:"include"; intercepted responses still pass
 *  through the browser's CORS checks). */
async function fulfillJSON(route: Route, body: unknown, status = 200) {
  const origin = route.request().headers()["origin"] ?? "*";
  await route.fulfill({
    status,
    contentType: "application/json",
    headers: {
      "Access-Control-Allow-Origin": origin,
      "Access-Control-Allow-Credentials": "true",
      "Access-Control-Allow-Methods": "GET,POST,PUT,PATCH,DELETE,OPTIONS",
      "Access-Control-Allow-Headers": "*",
    },
    body: JSON.stringify(body),
  });
}

function isPreflight(route: Route): boolean {
  return route.request().method() === "OPTIONS";
}

async function installMocks(page: Page, state: MockState) {
  // Catch-alls FIRST (Playwright matches last-registered first): shell
  // side-fetches the scene doesn't depend on.
  await page.route("**/requests/pending*", (r) => fulfillJSON(r, []));
  await page.route("**/canvas/viewport", (r) => fulfillJSON(r, {}));
  await page.route("**/cp/**", (r) => fulfillJSON(r, {}));
  await page.route("**/workspaces/**", async (r) => {
    if (isPreflight(r)) return fulfillJSON(r, {}, 204);
    const url = new URL(r.request().url());
    if (r.request().method() === "PATCH") {
      const body = r.request().postDataJSON() as { runtime?: string };
      // Record only the scene's runtime PATCH — the canvas store also
      // PATCHes auto-layout positions ({x,y}) on hydrate, which is
      // unrelated background noise for the §4 wire-order assertion.
      if (typeof body.runtime === "string") {
        state.calls.push({ method: "PATCH", path: url.pathname, body });
        state.rootRuntime = body.runtime;
      }
      return fulfillJSON(r, rootRow(state));
    }
    return fulfillJSON(r, []);
  });

  await page.route("**/org/identity", (r) =>
    fulfillJSON(r, {
      name: "",
      slug: "",
      org_id: "",
      platform_managed_available: false,
    }),
  );
  await page.route("**/templates", (r) => fulfillJSON(r, TEMPLATES));
  await page.route("**/settings/secrets", async (r) => {
    if (isPreflight(r)) return fulfillJSON(r, {}, 204);
    if (r.request().method() === "PUT") {
      const body = r.request().postDataJSON() as { key: string; value: string };
      state.calls.push({ method: "PUT", path: "/settings/secrets", body });
      state.secrets = state.secrets.filter((s) => s.key !== body.key);
      state.secrets.push({ key: body.key, has_value: true });
      return fulfillJSON(r, {});
    }
    return fulfillJSON(
      r,
      state.secrets.map((s) => ({ key: s.key, has_value: s.has_value })),
    );
  });
  await page.route("**/workspaces", async (r) => {
    if (isPreflight(r)) return fulfillJSON(r, {}, 204);
    return fulfillJSON(r, [rootRow(state)]);
  });
  await page.route("**/admin/org/platform-agent/ensure", async (r) => {
    if (isPreflight(r)) return fulfillJSON(r, {}, 204);
    const body = r.request().postDataJSON();
    state.calls.push({
      method: "POST",
      path: "/admin/org/platform-agent/ensure",
      body,
    });
    state.rootStatus = "provisioning";
    // Resolve the provision shortly after — the scene's poll fallback
    // observes the transition on its next 5s tick.
    setTimeout(() => state.onEnsure(state), 1_500);
    return fulfillJSON(r, { status: "repaired", provisioning: true });
  });
}

const sceneSel = "[data-testid='selfhost-setup-scene']";

test.describe("Self-host onboarding scene", () => {
  test.beforeEach(async ({ page }) => {
    await page.setViewportSize({ width: 1280, height: 800 });
  });

  test("A · golden path: blocks fullscreen, cascades dropdown-only, exact wire order, dismisses on online", async ({
    page,
  }) => {
    const state = makeState();
    await installMocks(page, state);
    await page.goto("/");

    // A2 — the scene renders and BLOCKS: a fullscreen modal covering the
    // viewport, nothing behind it reachable.
    const scene = page.locator(sceneSel);
    await expect(scene).toBeVisible({ timeout: 15_000 });
    await expect(scene).toHaveAttribute("aria-modal", "true");
    const box = await scene.boundingBox();
    expect(box).not.toBeNull();
    // Fullscreen overlay: covers (essentially) the whole 1280x800 viewport
    // (tolerance for a browser scrollbar gutter).
    expect(box!.width).toBeGreaterThanOrEqual(1260);
    expect(box!.height).toBeGreaterThanOrEqual(790);

    // A3 — fixed brand name, NO name input anywhere in the scene DOM.
    await expect(scene).toContainText("Enter OS Agent");
    expect(await scene.locator("input, textarea").count()).toBe(0);

    // Step 2 — runtime dropdown derived from /templates; displayable:false
    // absent; the root's runtime pre-selected.
    await page.getByTestId("scene-continue").click();
    const runtimeSelect = page.getByTestId("scene-runtime-select");
    await expect(runtimeSelect).toHaveValue("claude-code");
    const runtimeOptions = await runtimeSelect
      .locator("option")
      .allTextContents();
    expect(runtimeOptions).toEqual([
      "— select runtime —",
      "Claude Code",
      "Codex",
    ]);

    // A4 cascade — runtime pick re-derives the provider list; downstream
    // picks reset on upstream change; zero free-text inputs.
    await runtimeSelect.selectOption("codex");
    await page.getByTestId("scene-continue").click();
    const providerSelect = page.getByTestId("provider-select");
    await expect(providerSelect).toHaveValue("");
    expect(await providerSelect.locator("option").allTextContents()).toEqual([
      "— select provider —",
      "OpenAI",
    ]);
    await providerSelect.selectOption("registry|openai-api");
    await expect(page.getByTestId("model-select")).toHaveValue("gpt-5.4");
    expect(await page.getByTestId("scene-step-model").locator("input").count()).toBe(0);

    // Step 4 — key name from the provider's auth_env, masked input.
    await page.getByTestId("scene-continue").click();
    await expect(page.getByTestId("scene-step-key")).toContainText(
      "OPENAI_API_KEY",
    );
    const keyInput = page.getByTestId("scene-key-input");
    await expect(keyInput).toHaveAttribute("type", "password");
    await keyInput.fill("sk-test-e2e-key");
    await page.getByTestId("scene-continue").click();

    // Step 5 — review carries the fixed name; Configure fires the §4 wire
    // sequence in exactly key-PUT → runtime-PATCH → ensure order.
    await expect(page.getByTestId("scene-step-review")).toContainText(
      "Enter OS Agent",
    );
    await page.getByTestId("scene-configure").click();
    await expect(page.getByTestId("scene-progress")).toBeVisible({
      timeout: 10_000,
    });
    expect(state.calls).toEqual([
      {
        method: "PUT",
        path: "/settings/secrets",
        body: { key: "OPENAI_API_KEY", value: "sk-test-e2e-key" },
      },
      {
        method: "PATCH",
        path: `/workspaces/${ROOT_ID}`,
        body: { runtime: "codex" },
      },
      {
        method: "POST",
        path: "/admin/org/platform-agent/ensure",
        body: { name: "Enter OS Agent", model: "gpt-5.4", force: true },
      },
    ]);

    // Provision converges online → the scene auto-dismisses (poll fallback).
    await expect(scene).toHaveCount(0, { timeout: 15_000 });
    // G4 — the retired wizard's flag is never written.
    const onboardingKeys = await page.evaluate(() =>
      Object.keys(window.localStorage).filter((k) => k.includes("onboarding")),
    );
    expect(onboardingKeys).toEqual([]);
  });

  test("B · mid-flow refresh resumes from derived server state", async ({
    page,
  }) => {
    const state = makeState();
    await installMocks(page, state);
    await page.goto("/");
    const scene = page.locator(sceneSel);
    await expect(scene).toBeVisible({ timeout: 15_000 });

    // Advance to step 3, then refresh: nothing is persisted client-side, so
    // the scene re-derives from server state (still unconfigured ⇒ form).
    await page.getByTestId("scene-continue").click();
    await page.getByTestId("scene-continue").click();
    await expect(page.getByTestId("scene-step-model")).toBeVisible();
    await page.reload();
    await expect(scene).toBeVisible({ timeout: 15_000 });
    await expect(page.getByTestId("scene-step-welcome")).toBeVisible();

    // Refresh during provisioning ⇒ re-enters the PROGRESS view, not the
    // start of the flow (B2).
    state.rootStatus = "provisioning";
    await page.reload();
    await expect(page.getByTestId("scene-progress")).toBeVisible({
      timeout: 15_000,
    });

    // The provision completing while watching dismisses the scene.
    state.rootStatus = "online";
    await expect(scene).toHaveCount(0, { timeout: 15_000 });
  });

  test("C · wrong key: humanized credential error returns to the key step; corrected key converges", async ({
    page,
  }) => {
    const state = makeState({
      onEnsure: (s) => {
        // First provision attempt fails on the bad credential…
        s.rootStatus = "failed";
        s.lastSampleError =
          "workspace has no usable LLM credential (MISSING_BYOK_CREDENTIAL, molecule-core#1994)";
        // …subsequent attempts (corrected key) converge online.
        s.onEnsure = (s2) => {
          s2.rootStatus = "online";
        };
      },
    });
    await installMocks(page, state);
    await page.goto("/");
    await expect(page.locator(sceneSel)).toBeVisible({ timeout: 15_000 });

    // Walk the golden path with the (about-to-fail) key on the default
    // runtime — no PATCH expected anywhere in this scenario.
    await page.getByTestId("scene-continue").click();
    await page.getByTestId("scene-continue").click();
    await page
      .getByTestId("provider-select")
      .selectOption("registry|anthropic-api");
    await page.getByTestId("model-select").selectOption("claude-opus-4-7");
    await page.getByTestId("scene-continue").click();
    await page.getByTestId("scene-key-input").fill("sk-ant-wrong");
    await page.getByTestId("scene-continue").click();
    await page.getByTestId("scene-configure").click();

    // §8 mapping: humanized copy (no raw JSON), back at the key step.
    const banner = page.getByTestId("scene-key-banner");
    await expect(banner).toBeVisible({ timeout: 20_000 });
    await expect(banner).toContainText("is missing or didn't match");
    await expect(banner).not.toContainText("{");

    // Re-enter the corrected key → retry converges online and dismisses.
    await page.getByTestId("scene-key-input").fill("sk-ant-corrected");
    await page.getByTestId("scene-continue").click();
    await page.getByTestId("scene-configure").click();
    await expect(page.locator(sceneSel)).toHaveCount(0, { timeout: 20_000 });

    // The corrected key was re-written; ensure ran twice; still no PATCH.
    const puts = state.calls.filter((c) => c.method === "PUT");
    expect(puts.map((c) => (c.body as { value: string }).value)).toEqual([
      "sk-ant-wrong",
      "sk-ant-corrected",
    ]);
    expect(
      state.calls.filter((c) => c.method === "POST"),
    ).toHaveLength(2);
    expect(state.calls.some((c) => c.method === "PATCH")).toBe(false);
  });
});
