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

// Tests for PlatformBillingSection (concierge Settings — BYOK opt-in).
// Locks in the integration contract:
//  - reads GET /admin/workspaces/:id/llm-billing-mode on mount
//  - defaults to platform-managed when the read fails (no endpoint)
//  - enabling BYOK writes the key as a secret then flips billing-mode
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

beforeEach(() => {
  vi.clearAllMocks();
});

afterEach(() => {
  cleanup();
});

describe("PlatformBillingSection — BYOK opt-in", () => {
  it("reads billing mode on mount and defaults to platform-managed", async () => {
    apiGet.mockResolvedValueOnce({
      workspace_id: "plat-1",
      resolved_mode: "platform_managed",
      workspace_override: null,
      org_default: "platform_managed",
      source: "org_default",
    });

    render(<PlatformBillingSection platformId="plat-1" />);

    await waitFor(() => {
      expect(apiGet).toHaveBeenCalledWith(
        "/admin/workspaces/plat-1/llm-billing-mode",
      );
    });

    // Platform-managed radio is selected by default; no key field shown.
    const platformRadio = screen.getByRole("radio", {
      name: /platform-managed/i,
    }) as HTMLInputElement;
    expect(platformRadio.checked).toBe(true);
    expect(screen.queryByLabelText("ANTHROPIC_API_KEY")).toBeNull();
  });

  it("stays on platform-managed default when the billing endpoint is absent", async () => {
    apiGet.mockRejectedValueOnce(new Error("404 not found"));

    render(<PlatformBillingSection platformId="plat-2" />);

    await waitFor(() => expect(apiGet).toHaveBeenCalled());

    const platformRadio = screen.getByRole("radio", {
      name: /platform-managed/i,
    }) as HTMLInputElement;
    expect(platformRadio.checked).toBe(true);
  });

  it("enabling BYOK writes the secret then flips billing-mode to byok", async () => {
    apiGet.mockResolvedValueOnce({
      workspace_id: "plat-3",
      resolved_mode: "platform_managed",
      workspace_override: null,
      org_default: "platform_managed",
      source: "org_default",
    });
    apiPut.mockResolvedValue({});
    // The reload after save.
    apiGet.mockResolvedValueOnce({
      workspace_id: "plat-3",
      resolved_mode: "byok",
      workspace_override: "byok",
      org_default: "platform_managed",
      source: "workspace_override",
    });

    render(<PlatformBillingSection platformId="plat-3" />);
    await waitFor(() => expect(apiGet).toHaveBeenCalled());

    // Pick BYOK → key field appears.
    fireEvent.click(screen.getByRole("radio", { name: /use my own api key/i }));
    const keyInput = screen.getByLabelText("ANTHROPIC_API_KEY");
    fireEvent.change(keyInput, { target: { value: "sk-ant-test-key" } });

    fireEvent.click(screen.getByRole("button", { name: /enable byok/i }));

    await waitFor(() => {
      // 1. secret written
      expect(apiPut).toHaveBeenCalledWith("/workspaces/plat-3/secrets", {
        key: "ANTHROPIC_API_KEY",
        value: "sk-ant-test-key",
      });
      // 2. billing mode flipped
      expect(apiPut).toHaveBeenCalledWith(
        "/admin/workspaces/plat-3/llm-billing-mode",
        { mode: "byok" },
      );
    });
  });

  it("switching back to platform-managed from byok PUTs {mode: platform_managed}", async () => {
    apiGet.mockResolvedValueOnce({
      workspace_id: "plat-4",
      resolved_mode: "byok",
      workspace_override: "byok",
      org_default: "platform_managed",
      source: "workspace_override",
    });
    apiPut.mockResolvedValue({});
    apiGet.mockResolvedValueOnce({
      workspace_id: "plat-4",
      resolved_mode: "platform_managed",
      workspace_override: "platform_managed",
      org_default: "platform_managed",
      source: "workspace_override",
    });

    render(<PlatformBillingSection platformId="plat-4" />);

    // Starts on byok (mirrored from the override).
    await waitFor(() => {
      const byokRadio = screen.getByRole("radio", {
        name: /use my own api key/i,
      }) as HTMLInputElement;
      expect(byokRadio.checked).toBe(true);
    });

    fireEvent.click(screen.getByRole("radio", { name: /platform-managed/i }));

    await waitFor(() => {
      expect(apiPut).toHaveBeenCalledWith(
        "/admin/workspaces/plat-4/llm-billing-mode",
        { mode: "platform_managed" },
      );
    });
  });
});
