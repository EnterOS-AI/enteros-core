// @vitest-environment jsdom
//
// Tests for the provider → llm_billing_mode linkage (internal#703 Gap 2).
//
// What this pins: when the operator changes the PROVIDER in the Config
// tab, the workspace's llm_billing_mode must follow — a non-Platform
// provider sets billing_mode=byok; Platform sets platform_managed. Before
// this wiring, selecting "Claude Code subscription (OAuth)" or any vendor
// key wrote the credential env but left billing_mode=platform_managed, so
// CP kept injecting the platform proxy base URL and the OAuth token /
// vendor key was never used — BYOK silently no-op'd (the live jrs-auto
// SEO-Agent symptom in #703).
//
// The billing-mode PUT targets the same per-tenant endpoint the LLM
// Billing section uses: PUT /admin/workspaces/:id/llm-billing-mode with
// body {mode: "byok" | "platform_managed"}.

import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import { render, screen, cleanup, waitFor, fireEvent } from "@testing-library/react";
import React from "react";

afterEach(cleanup);

const apiGet = vi.fn();
const apiPatch = vi.fn();
const apiPut = vi.fn();
vi.mock("@/lib/api", () => ({
  api: {
    get: (path: string) => apiGet(path),
    patch: (path: string, body: unknown) => apiPatch(path, body),
    put: (path: string, body: unknown) => apiPut(path, body),
    post: vi.fn(),
    del: vi.fn(),
  },
}));

const storeUpdateNodeData = vi.fn();
const storeRestartWorkspace = vi.fn();
vi.mock("@/store/canvas", () => ({
  useCanvasStore: Object.assign(
    (selector: (s: unknown) => unknown) =>
      selector({ restartWorkspace: storeRestartWorkspace, updateNodeData: storeUpdateNodeData }),
    {
      getState: () => ({
        restartWorkspace: storeRestartWorkspace,
        updateNodeData: storeUpdateNodeData,
      }),
    },
  ),
}));

vi.mock("../AgentCardSection", () => ({
  AgentCardSection: () => <div data-testid="agent-card-stub" />,
}));

import { ConfigTab, billingModeForProvider } from "../ConfigTab";

function wireApi(opts: { providerValue?: string | "missing" }) {
  apiGet.mockImplementation((path: string) => {
    if (path === `/workspaces/ws-test`) {
      return Promise.resolve({ runtime: "hermes" });
    }
    if (path === `/workspaces/ws-test/model`) {
      return Promise.resolve({ model: "nousresearch/hermes-4-70b" });
    }
    if (path === `/workspaces/ws-test/provider`) {
      if (opts.providerValue === "missing") return Promise.reject(new Error("404"));
      return Promise.resolve({
        provider: opts.providerValue ?? "",
        source: opts.providerValue ? "workspace_secrets" : "default",
      });
    }
    if (path === `/workspaces/ws-test/files/config.yaml`) {
      return Promise.resolve({ content: "name: ws\nruntime: hermes\n" });
    }
    if (path === "/templates") return Promise.resolve([]);
    return Promise.reject(new Error(`unmocked api.get: ${path}`));
  });
}

function billingModeCalls() {
  return apiPut.mock.calls.filter(
    ([path]) => path === "/admin/workspaces/ws-test/llm-billing-mode",
  );
}

beforeEach(() => {
  apiGet.mockReset();
  apiPatch.mockReset();
  apiPut.mockReset();
  storeUpdateNodeData.mockReset();
  storeRestartWorkspace.mockReset();
});

describe("billingModeForProvider — pure mapping (internal#703 Gap 2)", () => {
  // Platform / empty → platform_managed. Empty means "no explicit
  // override → inherit", which resolves to platform on the backend, so
  // it must NOT flip the workspace into byok.
  it("maps Platform and empty to platform_managed", () => {
    expect(billingModeForProvider("platform")).toBe("platform_managed");
    expect(billingModeForProvider("")).toBe("platform_managed");
    expect(billingModeForProvider("  ")).toBe("platform_managed");
    expect(billingModeForProvider("PLATFORM")).toBe("platform_managed");
  });

  // Every non-Platform provider → byok. If this regresses to returning
  // platform_managed for a vendor, BYOK silently no-ops again (#703).
  it("maps non-Platform providers to byok", () => {
    expect(billingModeForProvider("anthropic-oauth")).toBe("byok"); // Claude Code subscription
    expect(billingModeForProvider("anthropic")).toBe("byok"); // Anthropic API key
    expect(billingModeForProvider("minimax")).toBe("byok");
    expect(billingModeForProvider("openrouter")).toBe("byok");
    expect(billingModeForProvider("openai")).toBe("byok");
  });
});

describe("ConfigTab — provider change drives billing_mode (internal#703 Gap 2)", () => {
  // The core fix: picking a non-Platform provider (here "anthropic-oauth"
  // = Claude Code subscription OAuth) from a fresh/empty provider must
  // PUT mode=byok to the per-tenant llm-billing-mode endpoint. This is
  // the exact path that was missing — the credential env saved but the
  // billing mode never followed, so the proxy stayed engaged.
  it("PUTs mode=byok when switching to a non-Platform provider", async () => {
    wireApi({ providerValue: "" });
    apiPut.mockResolvedValue({ status: "saved" });

    render(<ConfigTab workspaceId="ws-test" />);
    const input = await screen.findByTestId("provider-input");
    fireEvent.change(input, { target: { value: "anthropic-oauth" } });

    fireEvent.click(screen.getByRole("button", { name: /^save$/i }));

    await waitFor(() => {
      const calls = billingModeCalls();
      expect(calls.length).toBe(1);
      expect(calls[0][1]).toEqual({ mode: "byok" });
    });
    // Provider credential PUT still happens too (independent endpoint).
    expect(
      apiPut.mock.calls.some(([path]) => path === "/workspaces/ws-test/provider"),
    ).toBe(true);
  });

  // Switching FROM a byok provider back TO Platform must PUT
  // mode=platform_managed so the workspace re-engages the proxy and stops
  // expecting a (now-absent) vendor key.
  it("PUTs mode=platform_managed when switching back to Platform", async () => {
    wireApi({ providerValue: "anthropic-oauth" });
    apiPut.mockResolvedValue({ status: "saved" });

    render(<ConfigTab workspaceId="ws-test" />);
    const input = await screen.findByTestId("provider-input");
    await waitFor(() => expect((input as HTMLInputElement).value).toBe("anthropic-oauth"));
    fireEvent.change(input, { target: { value: "platform" } });

    fireEvent.click(screen.getByRole("button", { name: /^save$/i }));

    await waitFor(() => {
      const calls = billingModeCalls();
      expect(calls.length).toBe(1);
      expect(calls[0][1]).toEqual({ mode: "platform_managed" });
    });
  });

  // Changing between two BYOK vendors (minimax → openrouter) keeps
  // billing_mode=byok — the implied mode is unchanged, so re-PUTing it
  // would be a wasteful no-op that risks an extra restart. Must NOT fire.
  it("does NOT PUT billing-mode when the implied mode is unchanged", async () => {
    wireApi({ providerValue: "minimax" });
    apiPut.mockResolvedValue({ status: "saved" });

    render(<ConfigTab workspaceId="ws-test" />);
    const input = await screen.findByTestId("provider-input");
    await waitFor(() => expect((input as HTMLInputElement).value).toBe("minimax"));
    fireEvent.change(input, { target: { value: "openrouter" } });

    fireEvent.click(screen.getByRole("button", { name: /^save$/i }));

    await waitFor(() => {
      // Provider PUT fires (vendor changed)...
      expect(
        apiPut.mock.calls.some(([path]) => path === "/workspaces/ws-test/provider"),
      ).toBe(true);
    });
    // ...but billing-mode does NOT (byok → byok is a no-op).
    expect(billingModeCalls().length).toBe(0);
  });

  // A Save that doesn't touch the provider must not PUT billing-mode —
  // editing tier/name shouldn't disturb the workspace's billing mode.
  it("does NOT PUT billing-mode on a Save that leaves provider unchanged", async () => {
    wireApi({ providerValue: "anthropic-oauth" });
    apiPut.mockResolvedValue({ status: "saved" });

    render(<ConfigTab workspaceId="ws-test" />);
    await screen.findByTestId("provider-input");

    // Dirty an unrelated field so Save is enabled.
    const tierSelect = screen.getByLabelText(/tier/i) as HTMLSelectElement;
    fireEvent.change(tierSelect, { target: { value: "3" } });

    fireEvent.click(screen.getByRole("button", { name: /^save$/i }));

    await waitFor(() => {
      // Some PUT may fire (e.g. /model); just assert billing-mode did not.
      expect(billingModeCalls().length).toBe(0);
    });
  });

  // If the provider credential PUT itself fails, we must NOT set byok —
  // flipping billing_mode while the credential write failed would leave
  // the workspace expecting a key it doesn't have (worse than no-op).
  it("does NOT PUT billing-mode when the provider PUT fails", async () => {
    wireApi({ providerValue: "" });
    apiPut.mockImplementation((path: string) => {
      if (path === "/workspaces/ws-test/provider") return Promise.reject(new Error("boom"));
      return Promise.resolve({ status: "saved" });
    });

    render(<ConfigTab workspaceId="ws-test" />);
    const input = await screen.findByTestId("provider-input");
    fireEvent.change(input, { target: { value: "anthropic-oauth" } });

    fireEvent.click(screen.getByRole("button", { name: /^save$/i }));

    await waitFor(() => {
      // The provider-failure error is surfaced (getByText throws if absent).
      expect(screen.getByText(/provider update failed/i)).toBeTruthy();
    });
    expect(billingModeCalls().length).toBe(0);
  });

  // If the credential saved but the billing-mode PUT is rejected, the
  // user must be warned that BYOK may not take — a silent failure here
  // is precisely the #703 symptom we're fixing.
  it("surfaces an error when billing-mode PUT fails after a successful provider save", async () => {
    wireApi({ providerValue: "" });
    apiPut.mockImplementation((path: string) => {
      if (path === "/admin/workspaces/ws-test/llm-billing-mode") {
        return Promise.reject(new Error("403 forbidden"));
      }
      return Promise.resolve({ status: "saved" });
    });

    render(<ConfigTab workspaceId="ws-test" />);
    const input = await screen.findByTestId("provider-input");
    fireEvent.change(input, { target: { value: "anthropic-oauth" } });

    fireEvent.click(screen.getByRole("button", { name: /^save$/i }));

    await waitFor(() => {
      expect(screen.getByText(/switching billing mode failed/i)).toBeTruthy();
    });
  });
});
