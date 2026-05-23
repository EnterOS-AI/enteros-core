// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

const { mockGet, mockPost } = vi.hoisted(() => ({ mockGet: vi.fn(), mockPost: vi.fn() }));

vi.mock("@/lib/api", () => ({
  api: {
    get: mockGet,
    post: mockPost,
  },
}));

import { DisplayTab } from "../DisplayTab";

describe("DisplayTab", () => {
  beforeEach(() => {
    cleanup();
    mockGet.mockReset();
    mockPost.mockReset();
  });

  it("renders unavailable state for non-display workspaces", async () => {
    mockGet.mockResolvedValueOnce({
      available: false,
      reason: "display_not_enabled",
    });

    render(<DisplayTab workspaceId="ws-no-display" />);

    await waitFor(() => {
      expect(screen.getByText("Display is not enabled for this workspace.")).toBeTruthy();
    });
    expect(mockGet).toHaveBeenCalledWith("/workspaces/ws-no-display/display");
    expect(mockGet).not.toHaveBeenCalledWith("/workspaces/ws-no-display/display/control");
  });

  it("renders control acquisition for display-configured workspaces", async () => {
    mockGet
      .mockResolvedValueOnce({
        available: false,
        reason: "display_session_unavailable",
        mode: "desktop-control",
        status: "not_configured",
      })
      .mockResolvedValueOnce({
        controller: "none",
      });
    mockPost.mockResolvedValueOnce({
      controller: "user",
      controlled_by: "admin-token",
      expires_at: "2026-05-23T08:48:27Z",
    });

    render(<DisplayTab workspaceId="ws-display" />);

    await waitFor(() => {
      expect(screen.getByRole("button", { name: "Take control" })).toBeTruthy();
    });
    expect(mockGet).toHaveBeenCalledWith("/workspaces/ws-display/display");
    expect(mockGet).toHaveBeenCalledWith("/workspaces/ws-display/display/control");

    fireEvent.click(screen.getByRole("button", { name: "Take control" }));

    await waitFor(() => {
      expect(screen.getByText("Controlled by Admin")).toBeTruthy();
    });
    expect(mockPost).toHaveBeenCalledWith("/workspaces/ws-display/display/control/acquire", {
      controller: "user",
      ttl_seconds: 300,
    });
  });

  it("renders the desktop stream when a display session is available", async () => {
    mockGet
      .mockResolvedValueOnce({
        available: true,
        mode: "desktop-control",
        protocol: "dcv",
        width: 1920,
        height: 1080,
        viewer_url: "https://display.example.test/session/ws-display",
      })
      .mockResolvedValueOnce({
        controller: "none",
      });

    render(<DisplayTab workspaceId="ws-display" />);

    await waitFor(() => {
      expect(screen.getByTitle("Workspace desktop")).toBeTruthy();
    });
    const frame = screen.getByTitle("Workspace desktop") as HTMLIFrameElement;
    expect(frame.src).toBe("https://display.example.test/session/ws-display");
    expect(screen.getByRole("button", { name: "Take control" })).toBeTruthy();
  });

  it("releases user display control", async () => {
    mockGet
      .mockResolvedValueOnce({
        available: true,
        mode: "desktop-control",
        protocol: "dcv",
        viewer_url: "https://display.example.test/session/ws-display",
      })
      .mockResolvedValueOnce({
        controller: "user",
        controlled_by: "admin-token",
        expires_at: "2026-05-23T08:48:27Z",
      });
    mockPost.mockResolvedValueOnce({
      controller: "none",
    });

    render(<DisplayTab workspaceId="ws-display" />);

    await waitFor(() => {
      expect(screen.getByRole("button", { name: "Release" })).toBeTruthy();
    });

    fireEvent.click(screen.getByRole("button", { name: "Release" }));

    await waitFor(() => {
      expect(screen.getByRole("button", { name: "Take control" })).toBeTruthy();
    });
    expect(mockPost).toHaveBeenCalledWith("/workspaces/ws-display/display/control/release", {});
  });

  it("renders active display control locks as observe-only", async () => {
    mockGet
      .mockResolvedValueOnce({
        available: false,
        reason: "display_session_unavailable",
        mode: "desktop-control",
        status: "not_configured",
      })
      .mockResolvedValueOnce({
        controller: "agent",
        controlled_by: "sidecar",
        expires_at: "2026-05-23T08:48:27Z",
      });

    render(<DisplayTab workspaceId="ws-display" />);

    await waitFor(() => {
      expect(screen.getByText("Controlled by Agent")).toBeTruthy();
    });
    expect(screen.queryByRole("button", { name: "Release" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Take control" })).toBeNull();
    expect(mockPost).not.toHaveBeenCalled();
  });

  it("labels org-token display control locks as automation", async () => {
    mockGet
      .mockResolvedValueOnce({
        available: false,
        reason: "display_session_unavailable",
        mode: "desktop-control",
        status: "not_configured",
      })
      .mockResolvedValueOnce({
        controller: "user",
        controlled_by: "org-token:abc123",
        expires_at: "2026-05-23T08:48:27Z",
      });

    render(<DisplayTab workspaceId="ws-display" />);

    await waitFor(() => {
      expect(screen.getByText("Controlled by Automation")).toBeTruthy();
    });
    expect(screen.queryByText("org-token:abc123")).toBeNull();
    expect(screen.queryByRole("button", { name: "Take control" })).toBeNull();
  });

  it("refreshes display control state after failed acquisition", async () => {
    mockGet
      .mockResolvedValueOnce({
        available: false,
        reason: "display_session_unavailable",
        mode: "desktop-control",
        status: "not_configured",
      })
      .mockResolvedValueOnce({
        controller: "none",
      })
      .mockResolvedValueOnce({
        controller: "agent",
        controlled_by: "sidecar",
        expires_at: "2026-05-23T08:48:27Z",
      });
    mockPost.mockRejectedValueOnce(new Error("API POST /workspaces/ws-display/display/control/acquire: 409 conflict"));

    render(<DisplayTab workspaceId="ws-display" />);

    await waitFor(() => {
      expect(screen.getByRole("button", { name: "Take control" })).toBeTruthy();
    });

    fireEvent.click(screen.getByRole("button", { name: "Take control" }));

    await waitFor(() => {
      expect(screen.getByText("Controlled by Agent")).toBeTruthy();
    });
    expect(screen.getByText("Failed to take control")).toBeTruthy();
    expect(mockGet).toHaveBeenCalledWith("/workspaces/ws-display/display/control");
    expect(mockGet).toHaveBeenCalledTimes(3);
    expect(mockPost).toHaveBeenCalledWith("/workspaces/ws-display/display/control/acquire", {
      controller: "user",
      ttl_seconds: 300,
    });
  });

  it("keeps display status visible without takeover actions when control status fails", async () => {
    mockGet
      .mockResolvedValueOnce({
        available: false,
        reason: "display_session_unavailable",
        mode: "desktop-control",
        status: "not_configured",
      })
      .mockRejectedValueOnce(new Error("API GET /workspaces/ws-display/display/control: 401 unauthorized"));

    render(<DisplayTab workspaceId="ws-display" />);

    await waitFor(() => {
      expect(screen.getByText("Display session is not ready.")).toBeTruthy();
    });
    expect(screen.queryByRole("button", { name: "Take control" })).toBeNull();
    expect(screen.getByText("Display control unavailable")).toBeTruthy();
  });

  it("does not render raw display status errors", async () => {
    mockGet.mockRejectedValueOnce(new Error("API GET /workspaces/ws-display/display: 500 secret backend details"));

    render(<DisplayTab workspaceId="ws-display" />);

    await waitFor(() => {
      expect(screen.getByText("Display status unavailable")).toBeTruthy();
    });
    expect(screen.queryByText(/secret backend details/)).toBeNull();
  });

  it("ignores stale acquire responses after workspace changes", async () => {
    const acquire = deferred<{ controller: "user"; controlled_by: string; expires_at: string }>();
    mockGet
      .mockResolvedValueOnce({
        available: false,
        reason: "display_session_unavailable",
        mode: "desktop-control",
        status: "not_configured",
      })
      .mockResolvedValueOnce({
        controller: "none",
      })
      .mockResolvedValueOnce({
        available: false,
        reason: "display_session_unavailable",
        mode: "desktop-control",
        status: "not_configured",
      })
      .mockResolvedValueOnce({
        controller: "none",
      });
    mockPost.mockReturnValueOnce(acquire.promise);

    const { rerender } = render(<DisplayTab workspaceId="ws-a" />);

    await waitFor(() => {
      expect(screen.getByRole("button", { name: "Take control" })).toBeTruthy();
    });
    fireEvent.click(screen.getByRole("button", { name: "Take control" }));

    rerender(<DisplayTab workspaceId="ws-b" />);

    await waitFor(() => {
      expect(mockGet).toHaveBeenCalledWith("/workspaces/ws-b/display/control");
    });
    await waitFor(() => {
      expect(screen.getByRole("button", { name: "Take control" })).toBeTruthy();
    });

    acquire.resolve({
      controller: "user",
      controlled_by: "admin-token",
      expires_at: "2026-05-23T08:48:27Z",
    });
    await acquire.promise;

    await waitFor(() => {
      expect(screen.queryByText("Controlled by Admin")).toBeNull();
    });
    expect(screen.getByRole("button", { name: "Take control" })).toBeTruthy();
  });
});

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}
