// @vitest-environment jsdom
/**
 * Tests for CreatePlatformAgentButton — the "Create / repair platform agent" CTA
 * in the concierge empty states.
 *
 * The load-bearing invariant: the button drives platform-agent create/repair via
 * CORE's OWN endpoint (POST /admin/org/platform-agent/ensure on the
 * workspace-server) and NEVER a control-plane (/cp/*) endpoint — molecule-core is
 * OSS and must not depend on the proprietary control plane.
 *
 * Mock style mirrors RequestsInbox.test.tsx: vi.hoisted refs + file-level
 * vi.mock for @/lib/api, the canvas store, and the CSS module.
 */
// NOTE: No @testing-library/jest-dom — this suite uses plain DOM APIs, matching
// the canvas test convention (TopBar/AgentCard tests).
import React from "react";
import { render, screen, fireEvent, cleanup, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { CreatePlatformAgentButton } from "../CreatePlatformAgentButton";

const { mockApiGet, mockApiPost, mockHydrate } = vi.hoisted(() => ({
  mockApiGet: vi.fn<(...args: unknown[]) => Promise<unknown>>(),
  mockApiPost: vi.fn<(...args: unknown[]) => Promise<unknown>>(),
  mockHydrate: vi.fn(),
}));

vi.mock("@/lib/api", () => ({ api: { get: mockApiGet, post: mockApiPost } }));
vi.mock("@/store/canvas", () => ({
  useCanvasStore: { getState: () => ({ hydrate: mockHydrate }) },
}));
vi.mock("../Concierge.module.css", () => ({ default: {} }));

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("CreatePlatformAgentButton", () => {
  it("calls the CORE ensure endpoint (not a /cp/* control-plane path) on click", async () => {
    mockApiPost.mockResolvedValue({ status: "created", platform_agent_id: "pa-1" });
    mockApiGet.mockResolvedValue([{ id: "pa-1", kind: "platform" }]);

    render(<CreatePlatformAgentButton />);
    fireEvent.click(screen.getByTestId("create-platform-agent"));

    await waitFor(() => expect(mockApiPost).toHaveBeenCalled());

    const postPath = mockApiPost.mock.calls[0][0] as string;
    expect(postPath).toBe("/admin/org/platform-agent/ensure");
    // Core-only invariant: never a control-plane call.
    expect(postPath).not.toMatch(/\/cp\//);
    expect(postPath).not.toMatch(/cp-proxy|MOLECULE_CP_URL/);
  });

  it("re-hydrates the canvas with GET /workspaces on success", async () => {
    const ws = [{ id: "pa-1", kind: "platform" }];
    mockApiPost.mockResolvedValue({ status: "created" });
    mockApiGet.mockResolvedValue(ws);

    render(<CreatePlatformAgentButton />);
    fireEvent.click(screen.getByTestId("create-platform-agent"));

    await waitFor(() => expect(mockHydrate).toHaveBeenCalledWith(ws));
    expect(mockApiGet).toHaveBeenCalledWith("/workspaces");
  });

  it("shows the provisioning label and disables the button while in flight", async () => {
    let resolvePost!: (v: unknown) => void;
    mockApiPost.mockReturnValue(new Promise((res) => { resolvePost = res; }));
    mockApiGet.mockResolvedValue([]);

    render(<CreatePlatformAgentButton />);
    const btn = screen.getByTestId("create-platform-agent") as HTMLButtonElement;
    fireEvent.click(btn);

    await waitFor(() => expect(btn.disabled).toBe(true));
    expect(btn.textContent).toMatch(/Provisioning/i);
    expect(btn.getAttribute("aria-busy")).toBe("true");

    resolvePost({ status: "created" });
    await waitFor(() => expect(mockHydrate).toHaveBeenCalled());
  });

  it("surfaces an error and re-enables the button when the ensure call fails", async () => {
    mockApiPost.mockRejectedValue(new Error("install failed"));

    render(<CreatePlatformAgentButton />);
    const btn = screen.getByTestId("create-platform-agent") as HTMLButtonElement;
    fireEvent.click(btn);

    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("install failed");
    expect(btn.disabled).toBe(false);
    // Failure path must not have re-hydrated.
    expect(mockHydrate).not.toHaveBeenCalled();
  });
});
