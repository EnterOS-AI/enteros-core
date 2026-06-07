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
// BYOK opt-in for the platform agent). Locks in the rebuilt contract:
//  - reads GET /admin/workspaces/:id/llm-billing-mode + /workspaces/:id +
//    /workspaces/:id/model + /templates on mount
//  - defaults to platform-managed when the billing read fails (no endpoint)
//  - the BYOK provider/model dropdown is SSOT-driven (registry_providers/
//    registry_models from /templates) — NOT hardcoded to Anthropic
//  - the key field is labelled with the SELECTED provider's required_env
//    (MINIMAX_API_KEY for MiniMax, ANTHROPIC_API_KEY for Anthropic, …)
//  - saving sets model (PUT /model), forces the provider (MODEL_PROVIDER
//    secret), writes the per-provider key secret, then flips billing-mode
//  - switching back to platform-managed PUTs {mode: "platform_managed"}

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
// start the agent already on byok.
function mockGets(opts?: { billingOverride?: string | null; model?: string }) {
  apiGet.mockImplementation((path: string) => {
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
  it("reads billing mode on mount and defaults to platform-managed", async () => {
    mockGets();

    render(<PlatformBillingSection platformId="plat-1" />);

    await waitFor(() => {
      expect(apiGet).toHaveBeenCalledWith(
        "/admin/workspaces/plat-1/llm-billing-mode",
      );
    });

    const platformRadio = screen.getByRole("radio", {
      name: /platform-managed/i,
    }) as HTMLInputElement;
    expect(platformRadio.checked).toBe(true);
    // No key field shown until BYOK is chosen.
    expect(screen.queryByLabelText(/API_KEY/)).toBeNull();
  });

  it("stays on platform-managed default when the billing endpoint is absent", async () => {
    apiGet.mockImplementation((path: string) => {
      if (path.endsWith("/llm-billing-mode")) {
        return Promise.reject(new Error("404 not found"));
      }
      if (path === "/templates") return Promise.resolve(TEMPLATES);
      if (path.endsWith("/model")) return Promise.resolve({ model: "" });
      return Promise.resolve({ runtime: "claude-code" });
    });

    render(<PlatformBillingSection platformId="plat-2" />);

    await waitFor(() => expect(apiGet).toHaveBeenCalled());

    const platformRadio = screen.getByRole("radio", {
      name: /platform-managed/i,
    }) as HTMLInputElement;
    expect(platformRadio.checked).toBe(true);
  });

  it("BYOK provider/model dropdown is SSOT-driven (not hardcoded Anthropic)", async () => {
    mockGets();

    render(<PlatformBillingSection platformId="plat-1" />);
    await waitFor(() => expect(apiGet).toHaveBeenCalledWith("/templates"));

    fireEvent.click(screen.getByRole("radio", { name: /use my own provider/i }));

    // The shared ProviderModelSelector renders with a provider <select>
    // carrying all registry providers — proof the catalog is SSOT-driven.
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

    fireEvent.click(screen.getByRole("radio", { name: /use my own provider/i }));

    const providerSelect = await screen.findByTestId("provider-select");
    // Pick the MiniMax provider.
    const minimaxOption = Array.from(
      providerSelect.querySelectorAll("option"),
    ).find((o) => /MiniMax/.test(o.textContent ?? ""))! as HTMLOptionElement;
    fireEvent.change(providerSelect, { target: { value: minimaxOption.value } });

    // Key field is labelled MINIMAX_API_KEY — driven by required_env, not
    // hardcoded to ANTHROPIC_API_KEY.
    expect(await screen.findByLabelText("MINIMAX_API_KEY")).toBeTruthy();
    expect(screen.queryByLabelText("ANTHROPIC_API_KEY")).toBeNull();
  });

  it("saving BYOK sets model, MODEL_PROVIDER secret, the key secret, then flips billing-mode", async () => {
    mockGets();
    apiPut.mockResolvedValue({});

    render(<PlatformBillingSection platformId="plat-1" />);
    await waitFor(() => expect(apiGet).toHaveBeenCalledWith("/templates"));

    fireEvent.click(screen.getByRole("radio", { name: /use my own provider/i }));

    const providerSelect = await screen.findByTestId("provider-select");
    const minimaxOption = Array.from(
      providerSelect.querySelectorAll("option"),
    ).find((o) => /MiniMax/.test(o.textContent ?? ""))! as HTMLOptionElement;
    fireEvent.change(providerSelect, { target: { value: minimaxOption.value } });

    // Pick the MiniMax model.
    const modelSelect = await screen.findByTestId("model-select");
    fireEvent.change(modelSelect, { target: { value: "MiniMax-M2.7" } });

    const keyInput = await screen.findByLabelText("MINIMAX_API_KEY");
    fireEvent.change(keyInput, { target: { value: "mm-test-key" } });

    fireEvent.click(screen.getByRole("button", { name: /save provider/i }));

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

  it("switching back to platform-managed from byok PUTs {mode: platform_managed}", async () => {
    mockGets({ billingOverride: "byok" });
    apiPut.mockResolvedValue({});

    render(<PlatformBillingSection platformId="plat-1" />);

    await waitFor(() => {
      const byokRadio = screen.getByRole("radio", {
        name: /use my own provider/i,
      }) as HTMLInputElement;
      expect(byokRadio.checked).toBe(true);
    });

    fireEvent.click(screen.getByRole("radio", { name: /platform-managed/i }));

    await waitFor(() => {
      expect(apiPut).toHaveBeenCalledWith(
        "/admin/workspaces/plat-1/llm-billing-mode",
        { mode: "platform_managed" },
      );
    });
  });
});
