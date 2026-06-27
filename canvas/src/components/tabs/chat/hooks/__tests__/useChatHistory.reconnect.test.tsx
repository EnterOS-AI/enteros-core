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
});
