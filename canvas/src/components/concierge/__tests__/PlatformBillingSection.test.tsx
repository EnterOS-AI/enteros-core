// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import {
  render,
  screen,
  waitFor,
  cleanup,
  fireEvent,
} from "@testing-library/react";
import { PlatformBillingSection } from "../PlatformBillingSection";

// Tests for PlatformBillingSection (concierge Settings — SSOT provider+model
// BYOK for the platform agent). Locks in the rebuilt contract:
//  - reads GET /admin/workspaces/:id/llm-billing-mode + /workspaces/:id +
//    /workspaces/:id/model + /templates on mount
//  - there are NO mode radios: the provider DROPDOWN drives everything —
//    picking "Platform" = platform-managed (no key), any other provider = BYOK.
//    Pre-selects the platform provider by default (platform-managed).
//  - defaults to platform-managed when the billing read fails (no endpoint)
//  - the provider/model dropdown is SSOT-driven (registry_providers/
//    registry_models from /templates) — NOT hardcoded to Anthropic
//  - the key field is labelled with the SELECTED provider's required_env
//    (MINIMAX_API_KEY for MiniMax, ANTHROPIC_API_KEY for Anthropic, …)
//  - saving sets model (PUT /model), forces the provider (MODEL_PROVIDER
//    secret), writes the per-provider key secret, then flips billing-mode
//  - selecting "Platform" + Save PUTs {mode: "platform_managed"}

// Pick the registry provider whose option label matches `re`, in a
// ProviderModelSelector <select data-testid="provider-select">.
function selectProvider(providerSelect: HTMLElement, re: RegExp) {
  const opt = Array.from(providerSelect.querySelectorAll("option")).find((o) =>
    re.test(o.textContent ?? ""),
  ) as HTMLOptionElement;
  fireEvent.change(providerSelect, { target: { value: opt.value } });
}

const apiGet = vi.fn();
const apiPut = vi.fn();

vi.mock("@/lib/api", () => ({
  api: {
    get: (...args: unknown[]) => apiGet(...args),
    put: (...args: unknown[]) => apiPut(...args),
    post: vi.fn().mockResolvedValue({}),
    del: vi.fn().mockResolvedValue({}),
    patch: vi.fn().mockResolvedValue({}),
  },
}));

vi.mock("@/components/Toaster", () => ({
  showToast: vi.fn(),
}));

// A registry-backed /templates payload for the claude-code runtime carrying
// two providers (Anthropic API = BYOK key ANTHROPIC_API_KEY; MiniMax = BYOK
// key MINIMAX_API_KEY) plus the platform-managed proxy provider.
const TEMPLATES = [
  {
    id: "claude-code-default",
    name: "Claude Code",
    runtime: "claude-code",
    registry_backed: true,
    registry_providers: [
      { name: "platform", display_name: "Platform", billing_mode: "platform_managed", auth_env: [] },
      { name: "anthropic", display_name: "Anthropic API", billing_mode: "byok", auth_env: ["ANTHROPIC_API_KEY"] },
      { name: "minimax", display_name: "MiniMax", billing_mode: "byok", auth_env: ["MINIMAX_API_KEY"] },
    ],
    registry_models: [
      { id: "claude-sonnet-4-6", name: "Claude Sonnet 4.6", provider: "platform" },
      { id: "claude-opus-4-8", name: "Claude Opus 4.8", provider: "anthropic" },
      { id: "MiniMax-M2.7", name: "MiniMax M2.7", provider: "minimax" },
    ],
  },
];

// Wire the per-path GET mock used by most tests. billingOverride lets a test
// start the agent already on byok. platformManagedAvailable defaults true (SaaS:
// GET /org/identity → platform_managed_available); pass false to simulate a
// self-hosted deployment with no Molecule proxy.
function mockGets(opts?: {
  billingOverride?: string | null;
  model?: string;
  platformManagedAvailable?: boolean;
}) {
  const pmAvailable = opts?.platformManagedAvailable ?? true;
  apiGet.mockImplementation((path: string) => {
    if (path === "/org/identity") {
      return Promise.resolve({
        name: "Test Org",
        platform_managed_available: pmAvailable,
      });
    }
    if (path.endsWith("/llm-billing-mode")) {
      return Promise.resolve({
        workspace_id: "plat-1",
        resolved_mode: opts?.billingOverride === "byok" ? "byok" : "platform_managed",
        workspace_override: opts?.billingOverride ?? null,
        org_default: "platform_managed",
        source: opts?.billingOverride ? "workspace_override" : "org_default",
      });
    }
    if (path.endsWith("/model")) {
      return Promise.resolve({ model: opts?.model ?? "" });
    }
    if (path === "/templates") {
      return Promise.resolve(TEMPLATES);
    }
    if (path.startsWith("/workspaces/")) {
      return Promise.resolve({ runtime: "claude-code", tier: 4 });
    }
    return Promise.resolve({});
  });
}

beforeEach(() => {
  vi.clearAllMocks();
});

afterEach(() => {
  cleanup();
});

describe("PlatformBillingSection — SSOT provider+model BYOK", () => {
  it("reads billing mode on mount and defaults to platform-managed (no key field, no radios)", async () => {
    mockGets();

    render(<PlatformBillingSection platformId="plat-1" />);

    await waitFor(() => {
      expect(apiGet).toHaveBeenCalledWith(
        "/admin/workspaces/plat-1/llm-billing-mode",
      );
    });

    // No mode radios at all — the dropdown drives the mode.
    expect(screen.queryByRole("radio")).toBeNull();
    // The platform provider is pre-selected → platform-managed note shown,
    // and no per-provider key field.
    expect(
      await screen.findByText(/metered through the Molecule proxy/i),
    ).toBeTruthy();
    expect(screen.queryByLabelText(/API_KEY/)).toBeNull();
  });

  it("stays on platform-managed default when the billing endpoint is absent", async () => {
    apiGet.mockImplementation((path: string) => {
      // Proxy IS configured (SaaS) — only the billing-mode endpoint is absent.
      if (path === "/org/identity")
        return Promise.resolve({ platform_managed_available: true });
      if (path.endsWith("/llm-billing-mode")) {
        return Promise.reject(new Error("404 not found"));
      }
      if (path === "/templates") return Promise.resolve(TEMPLATES);
      if (path.endsWith("/model")) return Promise.resolve({ model: "" });
      return Promise.resolve({ runtime: "claude-code" });
    });

    render(<PlatformBillingSection platformId="plat-2" />);

    await waitFor(() => expect(apiGet).toHaveBeenCalled());

    // Catalog still loads from /templates → platform provider pre-selected →
    // platform-managed note, no key field.
    expect(
      await screen.findByText(/metered through the Molecule proxy/i),
    ).toBeTruthy();
    expect(screen.queryByLabelText(/API_KEY/)).toBeNull();
  });

  it("provider/model dropdown is SSOT-driven (not hardcoded Anthropic)", async () => {
    mockGets();

    render(<PlatformBillingSection platformId="plat-1" />);
    await waitFor(() => expect(apiGet).toHaveBeenCalledWith("/templates"));

    // The shared ProviderModelSelector renders a provider <select> carrying
    // every registry provider — proof the catalog is SSOT-driven. No radio
    // gate: the dropdown is shown immediately.
    const providerSelect = await screen.findByTestId("provider-select");
    const labels = Array.from(providerSelect.querySelectorAll("option")).map(
      (o) => o.textContent,
    );
    expect(labels.join(" ")).toMatch(/MiniMax/);
    expect(labels.join(" ")).toMatch(/Anthropic API/);
    expect(labels.join(" ")).toMatch(/Platform/);
  });

  it("key field is labelled with the selected provider's required_env (MiniMax)", async () => {
    mockGets();

    render(<PlatformBillingSection platformId="plat-1" />);
    await waitFor(() => expect(apiGet).toHaveBeenCalledWith("/templates"));

    const providerSelect = await screen.findByTestId("provider-select");
    selectProvider(providerSelect, /MiniMax/);

    // Key field is labelled MINIMAX_API_KEY — driven by required_env, not
    // hardcoded to ANTHROPIC_API_KEY.
    expect(await screen.findByLabelText("MINIMAX_API_KEY")).toBeTruthy();
    expect(screen.queryByLabelText("ANTHROPIC_API_KEY")).toBeNull();
  });

  it("saving a BYOK provider sets model, MODEL_PROVIDER secret, the key secret, then flips billing-mode", async () => {
    mockGets();
    apiPut.mockResolvedValue({});

    render(<PlatformBillingSection platformId="plat-1" />);
    await waitFor(() => expect(apiGet).toHaveBeenCalledWith("/templates"));

    const providerSelect = await screen.findByTestId("provider-select");
    selectProvider(providerSelect, /MiniMax/);

    // Pick the MiniMax model.
    const modelSelect = await screen.findByTestId("model-select");
    fireEvent.change(modelSelect, { target: { value: "MiniMax-M2.7" } });

    const keyInput = await screen.findByLabelText("MINIMAX_API_KEY");
    fireEvent.change(keyInput, { target: { value: "mm-test-key" } });

    fireEvent.click(screen.getByRole("button", { name: /^save$/i }));

    await waitFor(() => {
      expect(apiPut).toHaveBeenCalledWith("/workspaces/plat-1/model", {
        model: "MiniMax-M2.7",
      });
      expect(apiPut).toHaveBeenCalledWith("/workspaces/plat-1/secrets", {
        key: "MODEL_PROVIDER",
        value: "minimax",
      });
      expect(apiPut).toHaveBeenCalledWith("/workspaces/plat-1/secrets", {
        key: "MINIMAX_API_KEY",
        value: "mm-test-key",
      });
      expect(apiPut).toHaveBeenCalledWith(
        "/admin/workspaces/plat-1/llm-billing-mode",
        { mode: "byok" },
      );
    });
  });

  it("selecting Platform + Save flips billing-mode back to platform_managed", async () => {
    // Agent starts on byok (MiniMax) so the dropdown pre-selects MiniMax.
    mockGets({ billingOverride: "byok", model: "MiniMax-M2.7" });
    apiPut.mockResolvedValue({});

    render(<PlatformBillingSection platformId="plat-1" />);

    // Pre-selected to the BYOK MiniMax provider → key field present.
    const providerSelect = await screen.findByTestId("provider-select");
    await screen.findByLabelText("MINIMAX_API_KEY");

    // Switch to the Platform (platform-managed) provider and save.
    selectProvider(providerSelect, /Platform/);
    await screen.findByText(/metered through the Molecule proxy/i);
    fireEvent.click(screen.getByRole("button", { name: /^save$/i }));

    await waitFor(() => {
      expect(apiPut).toHaveBeenCalledWith(
        "/admin/workspaces/plat-1/llm-billing-mode",
        { mode: "platform_managed" },
      );
    });
  });
});

// Self-hosted: GET /org/identity → platform_managed_available=false. The
// "Platform (proxy)" option is HIDDEN, the default is a BYOK provider, the copy
// drops the proxy/credits promise, and the billing write is always byok.
describe("PlatformBillingSection — self-hosted (no Molecule proxy)", () => {
  it("hides the Platform (proxy) provider and defaults to a BYOK provider", async () => {
    mockGets({ platformManagedAvailable: false });

    render(<PlatformBillingSection platformId="plat-1" />);
    await waitFor(() => expect(apiGet).toHaveBeenCalledWith("/org/identity"));

    const providerSelect = await screen.findByTestId("provider-select");
    const labels = Array.from(providerSelect.querySelectorAll("option")).map(
      (o) => o.textContent ?? "",
    );
    // Platform is filtered out; BYOK providers remain.
    expect(labels.join(" ")).not.toMatch(/Platform/);
    expect(labels.join(" ")).toMatch(/Anthropic API/);
    expect(labels.join(" ")).toMatch(/MiniMax/);

    // The platform-managed note (proxy/credits) is NOT shown — a BYOK provider
    // is pre-selected, so its required-env key field appears instead.
    expect(screen.queryByText(/metered through the Molecule proxy/i)).toBeNull();
    // Self-host copy explains the proxy is unavailable.
    expect(
      screen.getByText(/self-hosted deployment has no\s+Molecule proxy/i),
    ).toBeTruthy();
  });

  it("treats a 404 / error on /org/identity as unavailable (hide Platform)", async () => {
    // /org/identity rejects → self-host safety: hide Platform, default BYOK.
    apiGet.mockImplementation((path: string) => {
      if (path === "/org/identity")
        return Promise.reject(new Error("404 not found"));
      if (path.endsWith("/llm-billing-mode"))
        return Promise.resolve({
          workspace_id: "plat-1",
          resolved_mode: "platform_managed",
          workspace_override: null,
          org_default: "platform_managed",
          source: "org_default",
        });
      if (path === "/templates") return Promise.resolve(TEMPLATES);
      if (path.endsWith("/model")) return Promise.resolve({ model: "" });
      return Promise.resolve({ runtime: "claude-code" });
    });

    render(<PlatformBillingSection platformId="plat-1" />);
    await waitFor(() => expect(apiGet).toHaveBeenCalledWith("/org/identity"));

    const providerSelect = await screen.findByTestId("provider-select");
    const labels = Array.from(providerSelect.querySelectorAll("option")).map(
      (o) => o.textContent ?? "",
    );
    expect(labels.join(" ")).not.toMatch(/Platform/);
    expect(screen.queryByText(/metered through the Molecule proxy/i)).toBeNull();
  });

  it("saving writes byok (never platform_managed) on self-host", async () => {
    mockGets({ platformManagedAvailable: false });
    apiPut.mockResolvedValue({});

    render(<PlatformBillingSection platformId="plat-1" />);
    await waitFor(() => expect(apiGet).toHaveBeenCalledWith("/org/identity"));

    const providerSelect = await screen.findByTestId("provider-select");
    selectProvider(providerSelect, /MiniMax/);

    const modelSelect = await screen.findByTestId("model-select");
    fireEvent.change(modelSelect, { target: { value: "MiniMax-M2.7" } });

    const keyInput = await screen.findByLabelText("MINIMAX_API_KEY");
    fireEvent.change(keyInput, { target: { value: "mm-test-key" } });

    fireEvent.click(screen.getByRole("button", { name: /^save$/i }));

    await waitFor(() => {
      expect(apiPut).toHaveBeenCalledWith(
        "/admin/workspaces/plat-1/llm-billing-mode",
        { mode: "byok" },
      );
    });
    // The platform_managed write must NEVER happen on self-host.
    expect(apiPut).not.toHaveBeenCalledWith(
      "/admin/workspaces/plat-1/llm-billing-mode",
      { mode: "platform_managed" },
    );
  });
});
