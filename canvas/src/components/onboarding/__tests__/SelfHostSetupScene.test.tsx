// @vitest-environment jsdom
/**
 * SelfHostSetupScene component tests — gate matrix, step cascade, fixed-name
 * payload, §4 wire order, §8 error mapping, derived-state resume, focus-trap
 * a11y, watch (store + socket + poll) — 100% line+branch per §10.1.
 */
import {
  afterEach,
  beforeEach,
  describe,
  expect,
  it,
  vi,
  type Mock,
} from "vitest";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";

vi.mock("@/lib/api", () => ({
  api: { get: vi.fn(), post: vi.fn(), put: vi.fn(), patch: vi.fn(), del: vi.fn() },
}));
vi.mock("@/lib/tenant", () => ({ getTenantSlug: vi.fn(() => "") }));

import { api } from "@/lib/api";
import { getTenantSlug } from "@/lib/tenant";
import { useCanvasStore } from "@/store/canvas";
import {
  _resetSocketEventListenersForTests,
  emitSocketEvent,
} from "@/store/socket-events";
import {
  SelfHostSetupScene,
  SLOW_PROVISION_HINT_MS,
  WATCH_POLL_MS,
} from "../SelfHostSetupScene";

// ── Fixtures ─────────────────────────────────────────────────────────────────

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
  {
    id: "tpl-hermes",
    name: "Hermes",
    runtime: "hermes",
    registry_backed: true,
    registry_providers: [
      { name: "byok-minimax", display_name: "MiniMax", auth_env: ["MINIMAX_API_KEY"] },
      { name: "platform", display_name: "Platform", auth_env: [] },
    ],
    registry_models: [
      { id: "minimax:MiniMax-M2.7", provider: "byok-minimax" },
      { id: "minimax/MiniMax-M2.7", provider: "platform" },
    ],
  },
  {
    // Legacy (non-registry) template whose only provider declares NO auth
    // env — exercises the buildProviderCatalog fallback + the key-less step 4.
    id: "tpl-openclaw",
    name: "OpenClaw",
    runtime: "openclaw",
    models: [{ id: "custom/local-model" }],
  },
];

function platformNode(data: Record<string, unknown> = {}) {
  return {
    id: "root-1",
    position: { x: 0, y: 0 },
    data: {
      kind: "platform",
      status: "offline",
      runtime: "hermes",
      lastSampleError: "",
      name: "Org Concierge",
      ...data,
    },
  };
}

function seedNodes(nodes: unknown[]) {
  useCanvasStore.setState({ nodes: nodes as never });
}

interface ApiState {
  identity: unknown;
  templates: unknown;
  secrets: unknown;
  workspaces: unknown;
}

function routeApi(overrides: Partial<ApiState> = {}) {
  const state: ApiState = {
    identity: { org_id: "" },
    templates: TEMPLATES,
    secrets: [],
    workspaces: [],
    ...overrides,
  };
  const calls: Array<{ method: string; path: string; body: unknown }> = [];
  (api.get as Mock).mockImplementation(async (path: string) => {
    if (path === "/org/identity") return state.identity;
    if (path === "/templates") return state.templates;
    if (path === "/settings/secrets") return state.secrets;
    if (path === "/workspaces") return state.workspaces;
    throw new Error(`unexpected GET ${path}`);
  });
  (api.put as Mock).mockImplementation(async (path: string, body: unknown) => {
    calls.push({ method: "PUT", path, body });
    return {};
  });
  (api.patch as Mock).mockImplementation(async (path: string, body: unknown) => {
    calls.push({ method: "PATCH", path, body });
    return {};
  });
  (api.post as Mock).mockImplementation(async (path: string, body: unknown) => {
    calls.push({ method: "POST", path, body });
    return {};
  });
  return { state, calls };
}

/** Flush the gate's promise chain + effects (real-timer variant). */
async function flush() {
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, 0));
  });
}

function scene(): HTMLElement {
  return screen.getByTestId("selfhost-setup-scene");
}

/** Walk the form to a given step (assumes the default fixture + a rendered,
 *  visible scene sitting at step 1 with runtime preselected). */
async function walkToKeyStep(opts: { runtime?: string } = {}) {
  fireEvent.click(screen.getByTestId("scene-continue")); // → step 2
  if (opts.runtime) {
    fireEvent.change(screen.getByTestId("scene-runtime-select"), {
      target: { value: opts.runtime },
    });
  }
  fireEvent.click(screen.getByTestId("scene-continue")); // → step 3
  const providerSelect = screen.getByTestId("provider-select");
  const firstProvider = (
    providerSelect.querySelectorAll("option")[1] as HTMLOptionElement
  ).value;
  fireEvent.change(providerSelect, { target: { value: firstProvider } });
  const modelSelect = screen.queryByTestId("model-select");
  if (modelSelect && (modelSelect as HTMLSelectElement).value === "") {
    const firstModel = (
      modelSelect.querySelectorAll("option")[1] as HTMLOptionElement
    ).value;
    fireEvent.change(modelSelect, { target: { value: firstModel } });
  }
  fireEvent.click(screen.getByTestId("scene-continue")); // → step 4
}

async function walkToReviewWithKey(key: string, opts: { runtime?: string } = {}) {
  await walkToKeyStep(opts);
  fireEvent.change(screen.getByTestId("scene-key-input"), {
    target: { value: key },
  });
  fireEvent.click(screen.getByTestId("scene-continue")); // → step 5
}

beforeEach(() => {
  vi.clearAllMocks();
  (getTenantSlug as Mock).mockReturnValue("");
  seedNodes([platformNode()]);
  _resetSocketEventListenersForTests();
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

// ── Gate matrix ──────────────────────────────────────────────────────────────

describe("gate matrix", () => {
  it("renders nothing while the gate is still evaluating (no flash)", () => {
    (api.get as Mock).mockImplementation(() => new Promise(() => {}));
    render(<SelfHostSetupScene />);
    expect(screen.queryByTestId("selfhost-setup-scene")).toBeNull();
  });

  it("never renders when a tenant slug is derived", async () => {
    routeApi();
    (getTenantSlug as Mock).mockReturnValue("acme");
    render(<SelfHostSetupScene />);
    await flush();
    expect(screen.queryByTestId("selfhost-setup-scene")).toBeNull();
  });

  it("never renders when org_id is set (SaaS)", async () => {
    routeApi({ identity: { org_id: "org-123" } });
    render(<SelfHostSetupScene />);
    await flush();
    expect(screen.queryByTestId("selfhost-setup-scene")).toBeNull();
  });

  it("never renders when /org/identity errors (fail-closed)", async () => {
    routeApi();
    (api.get as Mock).mockRejectedValue(new Error("network down"));
    render(<SelfHostSetupScene />);
    await flush();
    expect(screen.queryByTestId("selfhost-setup-scene")).toBeNull();
  });

  it("still resolves the pre-gate hold when gate evaluation REJECTS (spinner must never strand)", async () => {
    // getSlug runs before the gate's internal try/catch, so a throwing slug
    // provider rejects evaluateSelfHostSetupGate itself — the .catch arm in
    // the scene must still flip selfHostGateResolved or the shell's pre-gate
    // loading screen spins forever (worse than the legacy no-concierge view).
    routeApi();
    useCanvasStore.getState().setSelfHostGateResolved(false);
    (getTenantSlug as Mock).mockImplementationOnce(() => {
      throw new Error("slug provider exploded");
    });
    render(<SelfHostSetupScene />);
    await flush();
    expect(screen.queryByTestId("selfhost-setup-scene")).toBeNull();
    expect(useCanvasStore.getState().selfHostGateResolved).toBe(true);
  });

  it("does NOT touch the gate-resolved flag when unmounted before the rejection lands", async () => {
    routeApi();
    useCanvasStore.getState().setSelfHostGateResolved(false);
    (getTenantSlug as Mock).mockImplementationOnce(() => {
      throw new Error("slug provider exploded");
    });
    const { unmount } = render(<SelfHostSetupScene />);
    unmount(); // cancelled=true before the .catch settles
    await flush();
    expect(useCanvasStore.getState().selfHostGateResolved).toBe(false);
  });

  it("never renders when the platform root is online (configured)", async () => {
    routeApi();
    seedNodes([platformNode({ status: "online" })]);
    render(<SelfHostSetupScene />);
    await flush();
    expect(screen.queryByTestId("selfhost-setup-scene")).toBeNull();
  });

  it("never renders when an LLM key is already configured", async () => {
    routeApi({ secrets: [{ key: "MINIMAX_API_KEY", has_value: true }] });
    render(<SelfHostSetupScene />);
    await flush();
    expect(screen.queryByTestId("selfhost-setup-scene")).toBeNull();
  });

  it("renders (blocking dialog) for an unconfigured root", async () => {
    routeApi();
    render(<SelfHostSetupScene />);
    await flush();
    const dialog = scene();
    expect(dialog.getAttribute("role")).toBe("dialog");
    expect(dialog.getAttribute("aria-modal")).toBe("true");
    expect(screen.getByTestId("scene-step-welcome")).toBeTruthy();
  });

  it("renders defensively when the platform root is missing entirely", async () => {
    routeApi();
    seedNodes([]);
    render(<SelfHostSetupScene />);
    await flush();
    expect(screen.getByTestId("selfhost-setup-scene")).toBeTruthy();
  });

  it("aborts cleanly when unmounted before the gate resolves", async () => {
    routeApi();
    const { unmount } = render(<SelfHostSetupScene />);
    unmount();
    await flush();
    expect(screen.queryByTestId("selfhost-setup-scene")).toBeNull();
  });
});

// ── Welcome: fixed brand name, no name input ─────────────────────────────────

describe("welcome step (fixed name)", () => {
  it("shows 'Enter OS Agent' and renders NO input anywhere in the DOM", async () => {
    routeApi();
    render(<SelfHostSetupScene />);
    await flush();
    expect(scene().textContent).toContain("Enter OS Agent");
    expect(scene().querySelectorAll("input, textarea")).toHaveLength(0);
  });
});

// ── Runtime step + cascade ───────────────────────────────────────────────────

describe("runtime step + cascade", () => {
  it("derives runtime options from /templates and pre-selects the root's runtime", async () => {
    routeApi();
    render(<SelfHostSetupScene />);
    await flush();
    fireEvent.click(screen.getByTestId("scene-continue"));
    const select = screen.getByTestId("scene-runtime-select") as HTMLSelectElement;
    expect(select.value).toBe("hermes");
    const optionValues = Array.from(select.querySelectorAll("option")).map(
      (o) => o.value,
    );
    expect(optionValues).toEqual(["", "claude-code", "codex", "hermes", "openclaw"]);
  });

  it("leaves the runtime unselected (Continue disabled) when the root's runtime is not offerable", async () => {
    routeApi();
    seedNodes([platformNode({ runtime: "weird-runtime", status: undefined, lastSampleError: undefined })]);
    render(<SelfHostSetupScene />);
    await flush();
    fireEvent.click(screen.getByTestId("scene-continue"));
    const select = screen.getByTestId("scene-runtime-select") as HTMLSelectElement;
    expect(select.value).toBe("");
    expect(
      (screen.getByTestId("scene-continue") as HTMLButtonElement).disabled,
    ).toBe(true);
  });

  it("runtime change resets provider+model; provider change resets model", async () => {
    routeApi();
    render(<SelfHostSetupScene />);
    await flush();
    fireEvent.click(screen.getByTestId("scene-continue")); // → 2
    fireEvent.change(screen.getByTestId("scene-runtime-select"), {
      target: { value: "claude-code" },
    });
    fireEvent.click(screen.getByTestId("scene-continue")); // → 3 (claude-code)

    // Pick Anthropic (2 models → explicit pick required), then a model.
    const provider = screen.getByTestId("provider-select") as HTMLSelectElement;
    fireEvent.change(provider, { target: { value: "registry|anthropic-api" } });
    let model = screen.getByTestId("model-select") as HTMLSelectElement;
    expect(model.value).toBe("");
    expect(
      (screen.getByTestId("scene-continue") as HTMLButtonElement).disabled,
    ).toBe(true);
    fireEvent.change(model, { target: { value: "claude-opus-4-7" } });
    expect(
      (screen.getByTestId("scene-continue") as HTMLButtonElement).disabled,
    ).toBe(false);

    // Provider change → model reset (single-model provider auto-picks).
    fireEvent.change(provider, { target: { value: "registry|minimax" } });
    model = screen.getByTestId("model-select") as HTMLSelectElement;
    expect(model.value).toBe("MiniMax-M2.7");
    // …and back to a multi-model provider → model reset to empty.
    fireEvent.change(provider, { target: { value: "registry|anthropic-api" } });
    model = screen.getByTestId("model-select") as HTMLSelectElement;
    expect(model.value).toBe("");
    fireEvent.change(model, { target: { value: "claude-opus-4-7" } });

    // Runtime change upstream resets BOTH downstream picks.
    fireEvent.click(screen.getByTestId("scene-back")); // → 2
    fireEvent.change(screen.getByTestId("scene-runtime-select"), {
      target: { value: "codex" },
    });
    fireEvent.click(screen.getByTestId("scene-continue")); // → 3
    const provider2 = screen.getByTestId("provider-select") as HTMLSelectElement;
    expect(provider2.value).toBe("");
    expect(
      Array.from(provider2.querySelectorAll("option")).map((o) => o.textContent),
    ).toEqual(["— select provider —", "OpenAI"]);
    // No free-text path anywhere (§3): the model control is a <select>, and
    // no custom-escape option exists.
    expect(screen.queryByTestId("model-input")).toBeNull();
    expect(scene().textContent).not.toContain("Custom (type model id)");
  });
});

describe("back navigation", () => {
  it("Back walks 4 → 3 → 2 → 1 with picks intact", async () => {
    routeApi();
    render(<SelfHostSetupScene />);
    await flush();
    await walkToKeyStep();
    fireEvent.click(screen.getByTestId("scene-back")); // 4 → 3
    expect(screen.getByTestId("scene-step-model")).toBeTruthy();
    fireEvent.click(screen.getByTestId("scene-back")); // 3 → 2
    expect(screen.getByTestId("scene-step-runtime")).toBeTruthy();
    expect(
      (screen.getByTestId("scene-runtime-select") as HTMLSelectElement).value,
    ).toBe("hermes");
    fireEvent.click(screen.getByTestId("scene-back")); // 2 → 1
    expect(screen.getByTestId("scene-step-welcome")).toBeTruthy();
  });
});

// ── API-key step ─────────────────────────────────────────────────────────────

describe("API-key step", () => {
  it("names the key from the provider's auth_env, masks input, and gates Continue", async () => {
    routeApi();
    render(<SelfHostSetupScene />);
    await flush();
    await walkToKeyStep();
    const input = screen.getByTestId("scene-key-input") as HTMLInputElement;
    expect(input.type).toBe("password");
    expect(scene().textContent).toContain("MINIMAX_API_KEY");
    expect(
      (screen.getByTestId("scene-continue") as HTMLButtonElement).disabled,
    ).toBe(true);
    fireEvent.change(input, { target: { value: "minimax-key" } });
    expect(screen.queryByTestId("scene-key-format-hint")).toBeNull();
    expect(
      (screen.getByTestId("scene-continue") as HTMLButtonElement).disabled,
    ).toBe(false);
  });

  it("shows a NON-blocking format hint for a malformed key", async () => {
    routeApi();
    render(<SelfHostSetupScene />);
    await flush();
    await walkToKeyStep({ runtime: "claude-code" });
    fireEvent.change(screen.getByTestId("scene-key-input"), {
      target: { value: "not-a-key" },
    });
    expect(screen.getByTestId("scene-key-format-hint").textContent).toContain(
      "sk-ant-",
    );
    // Non-blocking: legit third-party keys ride Anthropic-compatible env
    // names, so Continue stays enabled.
    expect(
      (screen.getByTestId("scene-continue") as HTMLButtonElement).disabled,
    ).toBe(false);
  });

  it("supports providers that declare no auth env (key step is a no-op, wire skips the PUT)", async () => {
    const { calls } = routeApi();
    render(<SelfHostSetupScene />);
    await flush();
    await walkToKeyStep({ runtime: "openclaw" });
    expect(screen.getByTestId("scene-key-none").textContent).toContain(
      "does not declare an API-key requirement",
    );
    expect(
      (screen.getByTestId("scene-continue") as HTMLButtonElement).disabled,
    ).toBe(false);
    fireEvent.click(screen.getByTestId("scene-continue")); // → review
    expect(screen.getByTestId("scene-step-review").textContent).toContain(
      "unchanged",
    );
    await act(async () => {
      fireEvent.click(screen.getByTestId("scene-configure"));
      await new Promise((resolve) => setTimeout(resolve, 0));
    });
    // No secret to write → PATCH (runtime changed) + ensure only.
    expect(calls.map((c) => c.method)).toEqual(["PATCH", "POST"]);
  });
});

// ── Wire order (§4) ──────────────────────────────────────────────────────────

describe("wire order", () => {
  it("runtime unchanged: PUT secret → ensure, NO PATCH; exact fixed-name body", async () => {
    const { calls } = routeApi();
    render(<SelfHostSetupScene />);
    await flush();
    await walkToReviewWithKey("minimax-key");
    await act(async () => {
      fireEvent.click(screen.getByTestId("scene-configure"));
      await new Promise((resolve) => setTimeout(resolve, 0));
    });
    expect(calls).toEqual([
      {
        method: "PUT",
        path: "/settings/secrets",
        body: { key: "MINIMAX_API_KEY", value: "minimax-key" },
      },
      {
        method: "POST",
        path: "/admin/org/platform-agent/ensure",
        body: { name: "Enter OS Agent", model: "minimax:MiniMax-M2.7", force: true },
      },
    ]);
    expect(screen.getByTestId("scene-progress")).toBeTruthy();
  });

  it("runtime changed: PUT → PATCH {runtime} → ensure, in exactly that order", async () => {
    const { calls } = routeApi();
    render(<SelfHostSetupScene />);
    await flush();
    await walkToReviewWithKey("sk-openai-key", { runtime: "codex" });
    await act(async () => {
      fireEvent.click(screen.getByTestId("scene-configure"));
      await new Promise((resolve) => setTimeout(resolve, 0));
    });
    expect(calls).toEqual([
      {
        method: "PUT",
        path: "/settings/secrets",
        body: { key: "OPENAI_API_KEY", value: "sk-openai-key" },
      },
      { method: "PATCH", path: "/workspaces/root-1", body: { runtime: "codex" } },
      {
        method: "POST",
        path: "/admin/org/platform-agent/ensure",
        body: { name: "Enter OS Agent", model: "gpt-5.4", force: true },
      },
    ]);
  });

  it("missing root: no PATCH; ensure carries the runtime for the created path", async () => {
    const { calls } = routeApi();
    seedNodes([]);
    render(<SelfHostSetupScene />);
    await flush();
    await walkToReviewWithKey("sk-openai-key", { runtime: "codex" });
    await act(async () => {
      fireEvent.click(screen.getByTestId("scene-configure"));
      await new Promise((resolve) => setTimeout(resolve, 0));
    });
    expect(calls.map((c) => c.method)).toEqual(["PUT", "POST"]);
    expect(calls[1].body).toEqual({
      name: "Enter OS Agent",
      model: "gpt-5.4",
      force: true,
      runtime: "codex",
    });
  });

  it("double-clicking Configure fires exactly ONE wire sequence (debounce)", async () => {
    routeApi();
    (api.put as Mock).mockImplementation(() => new Promise(() => {}));
    render(<SelfHostSetupScene />);
    await flush();
    await walkToReviewWithKey("sk-ant-key");
    const btn = screen.getByTestId("scene-configure");
    await act(async () => {
      fireEvent.click(btn);
      fireEvent.click(btn);
    });
    expect(api.put).toHaveBeenCalledTimes(1);
  });
});

// ── Error states (§8) ────────────────────────────────────────────────────────

describe("error states", () => {
  it("ensure 500 → humanized copy (never raw JSON) + Retry re-runs ensure alone", async () => {
    const { calls } = routeApi();
    (api.post as Mock).mockRejectedValueOnce(
      new Error('API POST /admin/org/platform-agent/ensure: 500 {"error":"lookup failed"}'),
    );
    render(<SelfHostSetupScene />);
    await flush();
    await walkToReviewWithKey("sk-ant-key");
    await act(async () => {
      fireEvent.click(screen.getByTestId("scene-configure"));
      await new Promise((resolve) => setTimeout(resolve, 0));
    });
    const error = screen.getByTestId("scene-error");
    expect(error.textContent).toContain(
      "Couldn't set up the platform agent — lookup failed.",
    );
    expect(error.textContent).not.toContain("{");

    (api.post as Mock).mockImplementation(async (path: string, body: unknown) => {
      calls.push({ method: "POST", path, body });
      return {};
    });
    await act(async () => {
      fireEvent.click(screen.getByTestId("scene-retry"));
      await new Promise((resolve) => setTimeout(resolve, 0));
    });
    expect(calls[calls.length - 1]).toEqual({
      method: "POST",
      path: "/admin/org/platform-agent/ensure",
      body: { name: "Enter OS Agent", model: "minimax:MiniMax-M2.7", force: true },
    });
    expect(screen.getByTestId("scene-progress")).toBeTruthy();
  });

  it("422 UNREGISTERED_MODEL_FOR_RUNTIME → model-unavailable copy naming the runtime", async () => {
    routeApi();
    (api.post as Mock).mockRejectedValue(
      new Error(
        'API POST /admin/org/platform-agent/ensure: 422 {"error":"model not registered","code":"UNREGISTERED_MODEL_FOR_RUNTIME"}',
      ),
    );
    render(<SelfHostSetupScene />);
    await flush();
    await walkToReviewWithKey("sk-ant-key");
    await act(async () => {
      fireEvent.click(screen.getByTestId("scene-configure"));
      await new Promise((resolve) => setTimeout(resolve, 0));
    });
    expect(screen.getByTestId("scene-error").textContent).toContain(
      "That model isn't available for Hermes — pick one from the list.",
    );
  });

  it("non-Error rejections are stringified (configure + retry paths)", async () => {
    routeApi();
    (api.put as Mock).mockRejectedValueOnce("socket hangup");
    render(<SelfHostSetupScene />);
    await flush();
    await walkToReviewWithKey("sk-ant-key");
    await act(async () => {
      fireEvent.click(screen.getByTestId("scene-configure"));
      await new Promise((resolve) => setTimeout(resolve, 0));
    });
    expect(screen.getByTestId("scene-error").textContent).toContain("socket hangup");

    (api.post as Mock).mockRejectedValueOnce("retry hangup");
    await act(async () => {
      fireEvent.click(screen.getByTestId("scene-retry"));
      await new Promise((resolve) => setTimeout(resolve, 0));
    });
    expect(screen.getByTestId("scene-error").textContent).toContain("retry hangup");
  });

  it("double-clicking Retry fires exactly one ensure (debounce)", async () => {
    routeApi();
    (api.post as Mock).mockRejectedValueOnce(new Error("boom"));
    render(<SelfHostSetupScene />);
    await flush();
    await walkToReviewWithKey("sk-ant-key");
    await act(async () => {
      fireEvent.click(screen.getByTestId("scene-configure"));
      await new Promise((resolve) => setTimeout(resolve, 0));
    });
    (api.post as Mock).mockClear();
    (api.post as Mock).mockImplementation(() => new Promise(() => {}));
    const retry = screen.getByTestId("scene-retry");
    await act(async () => {
      fireEvent.click(retry);
      fireEvent.click(retry);
    });
    expect(api.post).toHaveBeenCalledTimes(1);
  });

  it("'Adjust setup' returns from the error view to the form", async () => {
    routeApi();
    (api.post as Mock).mockRejectedValueOnce(new Error("boom"));
    render(<SelfHostSetupScene />);
    await flush();
    await walkToReviewWithKey("sk-ant-key");
    await act(async () => {
      fireEvent.click(screen.getByTestId("scene-configure"));
      await new Promise((resolve) => setTimeout(resolve, 0));
    });
    fireEvent.click(screen.getByTestId("scene-adjust"));
    expect(screen.getByTestId("scene-step-welcome")).toBeTruthy();
  });
});

// ── Provision watch: store, socket, poll ─────────────────────────────────────

describe("provision watch", () => {
  async function configureAndWatch() {
    render(<SelfHostSetupScene />);
    await flush();
    await walkToReviewWithKey("sk-ant-key");
    await act(async () => {
      fireEvent.click(screen.getByTestId("scene-configure"));
      await new Promise((resolve) => setTimeout(resolve, 0));
    });
    expect(screen.getByTestId("scene-progress")).toBeTruthy();
  }

  it("dismisses the scene when the store's platform node goes online", async () => {
    routeApi();
    await configureAndWatch();
    await act(async () => {
      seedNodes([platformNode({ status: "provisioning" })]); // non-terminal → keeps watching
    });
    expect(screen.getByTestId("scene-progress")).toBeTruthy();
    await act(async () => {
      seedNodes([platformNode({ status: "online" })]);
    });
    expect(screen.queryByTestId("selfhost-setup-scene")).toBeNull();
  });

  it("store 'failed' with MISSING_BYOK_CREDENTIAL returns to the key step; corrected key re-PUTs", async () => {
    const { calls } = routeApi();
    await configureAndWatch();
    await act(async () => {
      seedNodes([
        platformNode({
          status: "failed",
          lastSampleError:
            "no usable LLM credential (MISSING_BYOK_CREDENTIAL, molecule-core#1994)",
        }),
      ]);
    });
    const banner = screen.getByTestId("scene-key-banner");
    // Provider label comes verbatim from the catalog (incl. its model-count
    // decoration) — the §8 copy names the provider the user actually picked.
    expect(banner.textContent).toContain(
      "The API key for MiniMax is missing or didn't match — re-enter it.",
    );
    // The session already wrote this key once → "already configured" state.
    const input = screen.getByTestId("scene-key-input") as HTMLInputElement;
    expect(input.placeholder).toContain("already configured");
    expect(input.value).toBe("");
    // Untouched key + Continue → review shows "unchanged"; the wire sequence
    // would skip the PUT.
    fireEvent.click(screen.getByTestId("scene-continue"));
    expect(screen.getByTestId("scene-step-review").textContent).toContain("unchanged");
    fireEvent.click(screen.getByTestId("scene-back"));
    // Re-enter a corrected key → retry converges: PUT fires again.
    fireEvent.change(screen.getByTestId("scene-key-input"), {
      target: { value: "minimax-corrected" },
    });
    fireEvent.click(screen.getByTestId("scene-continue"));
    // Reset the failed node so the fresh watch doesn't immediately re-fail.
    await act(async () => {
      seedNodes([platformNode({ status: "provisioning" })]);
    });
    calls.length = 0;
    await act(async () => {
      fireEvent.click(screen.getByTestId("scene-configure"));
      await new Promise((resolve) => setTimeout(resolve, 0));
    });
    expect(calls.map((c) => c.method)).toEqual(["PUT", "POST"]);
    expect(calls[0].body).toEqual({
      key: "MINIMAX_API_KEY",
      value: "minimax-corrected",
    });
  });

  it("maps the WORKSPACE_PROVISION_FAILED socket code (ignoring unrelated events)", async () => {
    routeApi();
    await configureAndWatch();
    await act(async () => {
      emitSocketEvent({
        event: "AGENT_MESSAGE",
        workspace_id: "root-1",
        timestamp: "",
        payload: { code: "MISSING_PLATFORM_PROXY" },
      });
      emitSocketEvent({
        event: "WORKSPACE_PROVISION_FAILED",
        workspace_id: "someone-else",
        timestamp: "",
        payload: { code: "MISSING_PLATFORM_PROXY", error: "x" },
      });
    });
    expect(screen.getByTestId("scene-progress")).toBeTruthy();
    await act(async () => {
      emitSocketEvent({
        event: "WORKSPACE_PROVISION_FAILED",
        workspace_id: "root-1",
        timestamp: "",
        payload: { code: "MISSING_PLATFORM_PROXY", error: "platform arm absent" },
      });
    });
    expect(screen.getByTestId("scene-error").textContent).toContain(
      "Enter OS hosted proxy",
    );
  });

  it("socket events with non-string code/error fall back to generic copy; null rootId accepts any workspace_id", async () => {
    routeApi();
    seedNodes([]); // no platform root → gate context has rootId=null
    render(<SelfHostSetupScene />);
    await flush();
    await walkToReviewWithKey("sk-ant-key", { runtime: "codex" });
    await act(async () => {
      fireEvent.click(screen.getByTestId("scene-configure"));
      await new Promise((resolve) => setTimeout(resolve, 0));
    });
    expect(screen.getByTestId("scene-progress")).toBeTruthy();
    await act(async () => {
      emitSocketEvent({
        event: "WORKSPACE_PROVISION_FAILED",
        workspace_id: "whatever-id",
        timestamp: "",
        payload: { code: 42, error: { nested: true } },
      });
    });
    expect(screen.getByTestId("scene-error").textContent).toContain(
      "the platform did not respond",
    );

    // Retry with rootId=null: ensure carries the runtime for the created
    // path; an Error rejection maps back into the error view.
    (api.post as Mock).mockRejectedValueOnce(new Error("ensure exploded"));
    await act(async () => {
      fireEvent.click(screen.getByTestId("scene-retry"));
      await new Promise((resolve) => setTimeout(resolve, 0));
    });
    expect((api.post as Mock).mock.calls.at(-1)).toEqual([
      "/admin/org/platform-agent/ensure",
      { name: "Enter OS Agent", model: "gpt-5.4", force: true, runtime: "codex" },
    ]);
    expect(screen.getByTestId("scene-error").textContent).toContain(
      "ensure exploded",
    );
  });

  it("polling fallback: tolerates bad payloads and converges on online", async () => {
    vi.useFakeTimers();
    const { state } = routeApi();
    render(<SelfHostSetupScene />);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    await walkToReviewWithKey("sk-ant-key");
    await act(async () => {
      fireEvent.click(screen.getByTestId("scene-configure"));
      await vi.advanceTimersByTimeAsync(0);
    });
    // Tick 1: non-array payload → ignored.
    state.workspaces = null;
    await act(async () => {
      await vi.advanceTimersByTimeAsync(WATCH_POLL_MS);
    });
    expect(screen.getByTestId("scene-progress")).toBeTruthy();
    // Tick 2: request rejects → ignored.
    (api.get as Mock).mockRejectedValueOnce(new Error("blip"));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(WATCH_POLL_MS);
    });
    expect(screen.getByTestId("scene-progress")).toBeTruthy();
    // Tick 3: no platform row → ignored.
    state.workspaces = [{ id: "w1", kind: "workspace", status: "online" }];
    await act(async () => {
      await vi.advanceTimersByTimeAsync(WATCH_POLL_MS);
    });
    expect(screen.getByTestId("scene-progress")).toBeTruthy();
    // Tick 4: platform row online → dismiss.
    state.workspaces = [{ id: "root-1", kind: "platform", status: "online" }];
    await act(async () => {
      await vi.advanceTimersByTimeAsync(WATCH_POLL_MS);
    });
    expect(screen.queryByTestId("selfhost-setup-scene")).toBeNull();
  });

  it("polling fallback surfaces a failed row's last_sample_error (and tolerates its absence)", async () => {
    vi.useFakeTimers();
    const { state } = routeApi();
    render(<SelfHostSetupScene />);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    await walkToReviewWithKey("sk-ant-key");
    await act(async () => {
      fireEvent.click(screen.getByTestId("scene-configure"));
      await vi.advanceTimersByTimeAsync(0);
    });
    state.workspaces = [
      {
        id: "root-1",
        kind: "platform",
        status: "failed",
        last_sample_error: "abort MISSING_PLATFORM_PROXY",
      },
    ];
    await act(async () => {
      await vi.advanceTimersByTimeAsync(WATCH_POLL_MS);
    });
    expect(screen.getByTestId("scene-error").textContent).toContain(
      "Enter OS hosted proxy",
    );

    // Second scene instance: failed row WITHOUT last_sample_error → generic.
    cleanup();
    seedNodes([platformNode()]);
    state.workspaces = [];
    render(<SelfHostSetupScene />);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    await walkToReviewWithKey("sk-ant-key");
    await act(async () => {
      fireEvent.click(screen.getByTestId("scene-configure"));
      await vi.advanceTimersByTimeAsync(0);
    });
    state.workspaces = [{ id: "root-1", kind: "platform", status: "failed" }];
    await act(async () => {
      await vi.advanceTimersByTimeAsync(WATCH_POLL_MS);
    });
    expect(screen.getByTestId("scene-error").textContent).toContain(
      "the platform did not respond",
    );
  });

  it("shows the slow-provision hint after the threshold", async () => {
    vi.useFakeTimers();
    const { state } = routeApi();
    state.workspaces = [platformNode({ status: "provisioning" })].map((n) => ({
      id: n.id,
      kind: "platform",
      status: "provisioning",
    }));
    render(<SelfHostSetupScene />);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    await walkToReviewWithKey("sk-ant-key");
    await act(async () => {
      fireEvent.click(screen.getByTestId("scene-configure"));
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(screen.queryByTestId("scene-slow-hint")).toBeNull();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(SLOW_PROVISION_HINT_MS);
    });
    expect(screen.getByTestId("scene-slow-hint").textContent).toContain(
      "pulling the runtime image",
    );
  });
});

// ── Derived-state resume (stateless — no localStorage) ──────────────────────

describe("derived-state resume", () => {
  it("root provisioning → resumes straight into the progress view", async () => {
    routeApi();
    seedNodes([platformNode({ status: "provisioning" })]);
    render(<SelfHostSetupScene />);
    await flush();
    expect(screen.getByTestId("scene-progress")).toBeTruthy();
    // The watching view now hands off to the Enter OS boot sequence (keycaps +
    // watchdog log) rather than a bare "Setting up" spinner card.
    expect(scene().textContent).toContain("Booting");
  });

  it("a transient node drop mid-watch KEEPS the boot sequence (no flicker to spinner)", async () => {
    // Regression (#15): the platform-node selector returns null when a store
    // update transiently ships a nodes array lacking the Platform node. The
    // scene holds the last-known node so the boot UI does NOT drop back to the
    // bare spinner card mid-provision.
    vi.useFakeTimers();
    routeApi();
    seedNodes([platformNode({ status: "provisioning" })]);
    render(<SelfHostSetupScene />);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    // node present → the boot sequence renders (the `&& platformNode` true side).
    expect(screen.getByTestId("boot-sequence-screen")).toBeTruthy();
    // node vanishes mid-watch (transient churn) → the last-known node keeps the
    // boot sequence on screen instead of flickering to the spinner card.
    await act(async () => {
      seedNodes([]);
    });
    expect(screen.getByTestId("boot-sequence-screen")).toBeTruthy();
    expect(scene().textContent).toContain("Booting");
    // The slow-provision hint still overlays the boot sequence past the
    // threshold (the early-return path preserves it).
    await act(async () => {
      await vi.advanceTimersByTimeAsync(SLOW_PROVISION_HINT_MS);
    });
    expect(screen.getByTestId("scene-slow-hint")).toBeTruthy();
  });

  it("watching with NO platform node ever seen falls back to the spinner card (+ slow hint)", async () => {
    // The node-less-from-start path (never any Platform node this session):
    // there is nothing to hold, so the guard's false side falls through to the
    // bare spinner card rather than handing BootSequenceScreen a null node.
    vi.useFakeTimers();
    const { state } = routeApi();
    seedNodes([]); // no platform root at any point → rootId=null, node never seen
    state.workspaces = [{ id: "root-1", kind: "platform", status: "provisioning" }];
    render(<SelfHostSetupScene />);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    // Walk the form and configure so we enter the watching phase with no node.
    await walkToReviewWithKey("sk-openai-key", { runtime: "codex" });
    await act(async () => {
      fireEvent.click(screen.getByTestId("scene-configure"));
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(screen.queryByTestId("boot-sequence-screen")).toBeNull();
    expect(screen.getByTestId("scene-progress")).toBeTruthy();
    expect(scene().textContent).toContain("Provisioning");
    // and the card's own slow-provision hint still fires past the threshold.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(SLOW_PROVISION_HINT_MS);
    });
    expect(screen.getByTestId("scene-slow-hint")).toBeTruthy();
  });

  it("surfaces the slow-provision hint over the boot sequence after the threshold", async () => {
    vi.useFakeTimers();
    routeApi();
    seedNodes([platformNode({ status: "provisioning" })]);
    render(<SelfHostSetupScene />);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    // resumed straight into the boot sequence (node present), hint not yet shown
    expect(scene().textContent).toContain("Booting");
    expect(screen.queryByTestId("scene-slow-hint")).toBeNull();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(SLOW_PROVISION_HINT_MS);
    });
    // the §8 slow-provision hint overlays the boot sequence (early-return path)
    expect(screen.getByTestId("scene-slow-hint")).toBeTruthy();
  });

  it("root failed → resumes into the humanized error view; Adjust setup restarts the form", async () => {
    routeApi();
    seedNodes([
      platformNode({
        status: "failed",
        lastSampleError: "abort MISSING_PLATFORM_PROXY (core#2162)",
      }),
    ]);
    render(<SelfHostSetupScene />);
    await flush();
    expect(screen.getByTestId("scene-error").textContent).toContain(
      "Enter OS hosted proxy",
    );
    fireEvent.click(screen.getByTestId("scene-adjust"));
    expect(screen.getByTestId("scene-step-welcome")).toBeTruthy();
  });

  it("root offline → starts the form at the welcome step", async () => {
    routeApi();
    render(<SelfHostSetupScene />);
    await flush();
    expect(screen.getByTestId("scene-step-welcome")).toBeTruthy();
    // No localStorage involvement anywhere in the scene.
    expect(Object.keys(localStorage)).toHaveLength(0);
  });
});

// ── Watching-phase boot failure + focus trap (#7, #8) ───────────────────────

describe("watching-phase boot failure + a11y", () => {
  it("a failed BOOT_STEP (presentation-only, status still provisioning) flips to the error/retry card (#8)", async () => {
    // BootSequenceScreen paints its own red "Boot failed" banner off a failed
    // boot step, but the node's aggregate status is still `provisioning` — the
    // step is presentation-only. Without the scene detecting it, the user is
    // stranded on a dead red screen. The scene must flip to its retry card.
    routeApi();
    seedNodes([platformNode({ status: "provisioning" })]);
    render(<SelfHostSetupScene />);
    await flush();
    // Resumed straight into the boot sequence (still provisioning).
    expect(screen.getByTestId("boot-sequence-screen")).toBeTruthy();
    // A boot step fails while the node status stays `provisioning`.
    await act(async () => {
      seedNodes([
        platformNode({
          status: "provisioning",
          bootSteps: [
            { step: 3, total: 8, key: "RT", label: "Start runtime", status: "failed", message: "runtime crashed on boot" },
          ],
        }),
      ]);
    });
    // The scene left the boot screen for its own error/retry card, carrying the
    // step's message as the humanized reason.
    expect(screen.queryByTestId("boot-sequence-screen")).toBeNull();
    const error = screen.getByTestId("scene-error");
    expect(error.textContent).toContain("runtime crashed on boot");
    expect(screen.getByTestId("scene-retry")).toBeTruthy();
  });

  it("boot steps present but none failed keep the boot screen (no false error flip) (#8)", async () => {
    // The `.find(failed) ?? null` path: a non-empty bootSteps array with only
    // running/ok steps must NOT flip to the error card — the boot screen stays.
    routeApi();
    seedNodes([
      platformNode({
        status: "provisioning",
        bootSteps: [
          { step: 1, total: 8, key: "PLG", label: "Install plugins", status: "ok" },
          { step: 2, total: 8, key: "ID", label: "Load identity", status: "running" },
        ],
      }),
    ]);
    render(<SelfHostSetupScene />);
    await flush();
    expect(screen.getByTestId("boot-sequence-screen")).toBeTruthy();
    expect(screen.queryByTestId("scene-error")).toBeNull();
  });

  it("a failed BOOT_STEP with no message falls back to the step label (#8)", async () => {
    routeApi();
    seedNodes([platformNode({ status: "provisioning" })]);
    render(<SelfHostSetupScene />);
    await flush();
    await act(async () => {
      seedNodes([
        platformNode({
          status: "provisioning",
          bootSteps: [
            { step: 4, total: 8, key: "MCP", label: "Management MCP", status: "failed" },
          ],
        }),
      ]);
    });
    expect(screen.getByTestId("scene-error").textContent).toContain(
      "Boot failed at Management MCP",
    );
  });

  it("the watching boot screen has the focus-trap handler wired (#7)", async () => {
    // Regression (#7): the watching early-return container omitted the
    // onKeyDown focus-trap handler the main modal return has, so the trap was
    // dead the whole provision window. While provisioning the boot screen's
    // ENTER OS key is disabled, so the focusable set is empty and the trap
    // preventDefaults the Tab. If the handler were missing (the regression),
    // the keydown would go unhandled and NOT be defaultPrevented.
    routeApi();
    seedNodes([platformNode({ status: "provisioning" })]);
    render(<SelfHostSetupScene />);
    await flush();
    const dialog = scene();
    expect(screen.getByTestId("boot-sequence-screen")).toBeTruthy();
    const tab = new KeyboardEvent("keydown", {
      key: "Tab",
      bubbles: true,
      cancelable: true,
    });
    dialog.dispatchEvent(tab);
    // The handler ran and trapped the Tab (empty focusable set → preventDefault).
    expect(tab.defaultPrevented).toBe(true);
  });
});

// ── Focus trap + a11y ────────────────────────────────────────────────────────

describe("focus trap + a11y", () => {
  it("autofocuses the first control and wraps Tab/Shift+Tab inside the dialog", async () => {
    routeApi();
    render(<SelfHostSetupScene />);
    await flush();
    // Autofocus on mount → the welcome step's only button.
    expect((document.activeElement as HTMLElement).dataset.testid).toBe(
      "scene-continue",
    );
    fireEvent.click(screen.getByTestId("scene-continue")); // → step 2 (select+back+continue)
    const dialog = scene();
    const continueBtn = screen.getByTestId("scene-continue") as HTMLButtonElement;
    continueBtn.focus();
    fireEvent.keyDown(dialog, { key: "Tab" });
    expect((document.activeElement as HTMLElement).id).toBe(
      "scene-runtime-select",
    );
    fireEvent.keyDown(dialog, { key: "Tab", shiftKey: true });
    expect((document.activeElement as HTMLElement).dataset.testid).toBe(
      "scene-continue",
    );
  });

  it("is labelled as a modal dialog with a heading", async () => {
    routeApi();
    render(<SelfHostSetupScene />);
    await flush();
    const dialog = scene();
    expect(dialog.getAttribute("aria-labelledby")).toBe("selfhost-setup-title");
    expect(
      dialog.querySelector("#selfhost-setup-title")?.textContent,
    ).toContain("Set up your platform agent");
  });
});
