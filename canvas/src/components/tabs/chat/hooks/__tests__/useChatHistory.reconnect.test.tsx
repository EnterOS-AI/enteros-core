// @vitest-environment jsdom
//
// Regression for core#2598: a persisted agent reply can land on the
// server before the canvas WebSocket subscriber is listening, so the
// live AGENT_MESSAGE/A2A_RESPONSE frame is missed and My Chat stays
// empty until a manual reload. useChatHistory now reconciles against
// the DB copy of chat-history on a short interval and exposes a
// `reconcile()` function that ChatTab fires immediately on reconnect.
//
// This test simulates the missed-frame scenario: the initial history
// load returns only the user message, then a later reconcile fetches
// the persisted agent reply and merges it into the conversation.

import { describe, it, expect, vi, beforeEach } from "vitest";
import { renderHook, waitFor, act } from "@testing-library/react";

const apiGetMock = vi.fn<
  (path: string, opts?: unknown) => Promise<unknown>
>();

vi.mock("@/lib/api", () => ({
  api: {
    get: (path: string, opts?: unknown) => apiGetMock(path, opts),
  },
}));

import { useChatHistory } from "../useChatHistory";

beforeEach(() => {
  apiGetMock.mockReset();
});

describe("useChatHistory — core#2598 reconcile", () => {
  it("reconciles a persisted reply that was missed by the WebSocket path", async () => {
    const userMsg = {
      id: "msg-user-2598",
      role: "user",
      content: "Persistence test",
      timestamp: "2026-06-27T00:00:00.000Z",
    };
    const agentMsg = {
      id: "msg-agent-2598",
      role: "agent",
      content: "Echo: Persistence test",
      timestamp: "2026-06-27T00:00:01.000Z",
    };

    // Initial load: only the user message is visible (the agent reply
    // was persisted but the WS frame carrying it was already emitted
    // before this chat panel subscribed).
    apiGetMock.mockResolvedValueOnce({
      messages: [userMsg],
      reached_end: true,
    });

    const { result } = renderHook(() => useChatHistory("ws-2598"));

    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.messages).toHaveLength(1);
    expect(result.current.messages[0].content).toBe("Persistence test");

    // Reconcile now sees the persisted reply in chat-history.
    apiGetMock.mockResolvedValueOnce({
      messages: [userMsg, agentMsg],
      reached_end: true,
    });

    await act(async () => {
      await result.current.reconcile();
    });

    expect(result.current.messages).toHaveLength(2);
    expect(result.current.messages[1].role).toBe("agent");
    expect(result.current.messages[1].content).toBe(
      "Echo: Persistence test",
    );
  });

  it("does not duplicate messages that are already rendered", async () => {
    const userMsg = {
      id: "msg-user-dedup",
      role: "user",
      content: "hello",
      timestamp: "2026-06-27T00:00:00.000Z",
    };

    apiGetMock.mockResolvedValueOnce({
      messages: [userMsg],
      reached_end: true,
    });

    const { result } = renderHook(() => useChatHistory("ws-dedup"));
    await waitFor(() => expect(result.current.loading).toBe(false));

    // Reconcile returns the same message again.
    apiGetMock.mockResolvedValueOnce({
      messages: [userMsg],
      reached_end: true,
    });

    await act(async () => {
      await result.current.reconcile();
    });

    expect(result.current.messages).toHaveLength(1);
  });

  it("reconcile during a slow initial load does not block loading", async () => {
    // CR2 #14653 regression: reconcile must not share the loadInitial/
    // loadOlder fetch-generation token. If it did, calling reconcile while
    // the initial load is still in flight would bump the token and cause
    // loadInitial to discard its own result before setLoading(false).
    const userMsg = {
      id: "msg-slow-load",
      role: "user",
      content: "still loading?",
      timestamp: "2026-06-27T00:00:00.000Z",
    };

    let resolveInitial: ((value: unknown) => void) | undefined;

    // Initial load is deferred so we can race reconcile against it.
    apiGetMock.mockImplementationOnce((path: string) => {
      if (path.includes("/workspaces/ws-slow/")) {
        return new Promise((resolve) => {
          resolveInitial = resolve;
        });
      }
      return Promise.resolve({ messages: [], reached_end: true });
    });

    // Reconcile returns empty and should not disturb the pending initial load.
    apiGetMock.mockResolvedValueOnce({ messages: [], reached_end: true });

    const { result } = renderHook(() => useChatHistory("ws-slow"));

    await act(async () => {
      await result.current.reconcile();
    });

    // The initial load is still pending; reconcile must not have caused it
    // to be abandoned.
    expect(result.current.loading).toBe(true);

    await act(async () => {
      resolveInitial?.({ messages: [userMsg], reached_end: true });
    });

    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.messages).toHaveLength(1);
    expect(result.current.messages[0].content).toBe("still loading?");
  });

  it("drops a stale reconcile after switching workspaces (no cross-workspace leak)", async () => {
    // Researcher #14648 regression: without a stale-workspace guard, a
    // reconcile fetch started for workspace A could resolve after the user
    // switched to workspace B and merge A's messages into B's conversation.
    const aReply = {
      id: "msg-a-stale",
      role: "agent",
      content: "workspace A reply",
      timestamp: "2026-06-27T00:00:01.000Z",
    };
    const bReply = {
      id: "msg-b-live",
      role: "agent",
      content: "workspace B reply",
      timestamp: "2026-06-27T00:00:02.000Z",
    };

    let resolveA: ((value: unknown) => void) | undefined;

    // Initial load for A returns empty so we can get to a stable state quickly.
    apiGetMock.mockResolvedValueOnce({ messages: [], reached_end: true });

    const { result, rerender } = renderHook(
      ({ workspaceId }: { workspaceId: string }) => useChatHistory(workspaceId),
      { initialProps: { workspaceId: "ws-A" } },
    );

    await waitFor(() => expect(result.current.loading).toBe(false));

    // Reconcile for A: block on a deferred response so we can switch
    // workspaces before it lands.
    apiGetMock.mockImplementationOnce((path: string) => {
      if (path.includes("/workspaces/ws-A/")) {
        return new Promise((resolve) => {
          resolveA = resolve;
        });
      }
      return Promise.resolve({ messages: [], reached_end: true });
    });

    act(() => {
      void result.current.reconcile();
    });

    // Switch to workspace B before A's reconcile resolves. The reconcile
    // closure remembers workspace A; when it resolves it must see the
    // workspace has changed and drop the result.
    apiGetMock.mockResolvedValueOnce({ messages: [bReply], reached_end: true });
    rerender({ workspaceId: "ws-B" });
    await waitFor(() => expect(result.current.loading).toBe(false));

    expect(result.current.messages).toHaveLength(1);
    expect(result.current.messages[0].content).toBe("workspace B reply");

    // Now resolve the stale A fetch.
    await act(async () => {
      resolveA?.({ messages: [aReply], reached_end: true });
      // Let the microtask queue drain so the stale reconcile's setMessages
      // attempt (which should be dropped) is processed.
      await Promise.resolve();
      await Promise.resolve();
    });

    // A's message must NOT have leaked into B.
    expect(result.current.messages).toHaveLength(1);
    expect(result.current.messages[0].content).toBe("workspace B reply");
  });
});
