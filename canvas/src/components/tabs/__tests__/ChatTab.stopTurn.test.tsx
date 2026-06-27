// @vitest-environment jsdom
//
// Pins app#84 — the My Chat thinking indicator must expose a Stop button
// that calls the same A2A tasks/cancel path used by the canvas Stop-All
// button, so a user can break an in-flight agent turn from chat.

import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import { render, screen, cleanup, waitFor, fireEvent } from "@testing-library/react";
import React from "react";

afterEach(cleanup);

const apiPost = vi.fn();
vi.mock("@/lib/api", () => ({
  api: {
    get: vi.fn((path: string) => {
      if (path.includes("/chat-history")) {
        return Promise.resolve({ messages: [], reached_end: true });
      }
      return Promise.resolve([]);
    }),
    post: (path: string, body: unknown) => apiPost(path, body),
    del: vi.fn(),
    patch: vi.fn(),
    put: vi.fn(),
  },
}));

vi.mock("@/store/canvas", () => ({
  useCanvasStore: vi.fn((selector?: (s: unknown) => unknown) =>
    selector ? selector({ wsStatus: "connected", agentMessages: {}, consumeAgentMessages: () => [] }) : {},
  ),
}));

beforeEach(() => {
  apiPost.mockReset();
  (window as unknown as { IntersectionObserver: unknown }).IntersectionObserver = vi.fn(() => ({
    observe: vi.fn(),
    unobserve: vi.fn(),
    disconnect: vi.fn(),
  }));
  Element.prototype.scrollIntoView = vi.fn();
});

import { ChatTab } from "../ChatTab";

const onlineData = {
  status: "online" as const,
  runtime: "claude-code",
  currentTask: null,
} as unknown as Parameters<typeof ChatTab>[0]["data"];

const busyData = {
  status: "online" as const,
  runtime: "claude-code",
  currentTask: { id: "task-123", state: "working" },
} as unknown as Parameters<typeof ChatTab>[0]["data"];

describe("ChatTab Stop control (app#84)", () => {
  it("does not render a Stop button when the agent is idle", async () => {
    render(<ChatTab workspaceId="ws-idle" data={onlineData} />);
    await waitFor(() => expect(screen.queryByText(/Loading chat history/i)).toBeNull());
    expect(screen.queryByRole("button", { name: /Stop current agent turn/i })).toBeNull();
  });

  it("renders a Stop button while an agent turn is in flight", async () => {
    render(<ChatTab workspaceId="ws-busy" data={busyData} />);
    await waitFor(() => expect(screen.queryByText(/Loading chat history/i)).toBeNull());
    const stopBtn = screen.getByRole("button", { name: /Stop current agent turn/i });
    expect(stopBtn).toBeDefined();
    expect(stopBtn.textContent).toBe("Stop");
  });

  it("calls tasks/cancel for the current workspace when Stop is clicked", async () => {
    apiPost.mockResolvedValue({});
    render(<ChatTab workspaceId="ws-busy" data={busyData} />);
    await waitFor(() => expect(screen.queryByText(/Loading chat history/i)).toBeNull());
    const stopBtn = screen.getByRole("button", { name: /Stop current agent turn/i });
    fireEvent.click(stopBtn);
    await waitFor(() =>
      expect(apiPost).toHaveBeenCalledWith("/workspaces/ws-busy/a2a", {
        method: "tasks/cancel",
        params: {},
      }),
    );
  });

  it("disables the Stop button and shows Stopping while tasks/cancel is in flight", async () => {
    let resolvePost: (() => void) | undefined;
    apiPost.mockImplementation(
      () => new Promise<void>((resolve) => { resolvePost = resolve; }),
    );
    render(<ChatTab workspaceId="ws-busy" data={busyData} />);
    await waitFor(() => expect(screen.queryByText(/Loading chat history/i)).toBeNull());
    const stopBtn = screen.getByRole("button", { name: /Stop current agent turn/i });
    fireEvent.click(stopBtn);
    await waitFor(() => expect(stopBtn.textContent).toBe("Stopping…"));
    expect(stopBtn.hasAttribute("disabled")).toBe(true);
    resolvePost!();
    await waitFor(() => expect(stopBtn.textContent).toBe("Stop"));
  });
});
