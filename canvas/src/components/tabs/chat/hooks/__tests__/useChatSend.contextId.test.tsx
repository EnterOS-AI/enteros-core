// @vitest-environment jsdom
//
// tenant-agent BUG 3 (client half): every chat message MUST carry a STABLE
// per-conversation `contextId`. Without it the runtime a2a-sdk mints a fresh
// context_id per request and any session keyed on it (openclaw SessionManager,
// the base RuntimeA2AExecutor's native thread_id) resets every turn → the agent re-greets.
//
// The contextId (the session key) is the ONLY continuity signal the client
// ships. The canvas MUST NOT force-inject prior turns via a
// `metadata: { history }` blob — the runtime gets continuity from the resumed
// native session (keyed on contextId), and older/other history is retrieved
// ONLY when the agent CHOOSES to call the platform-workspace MCP. This suite
// asserts BOTH halves: contextId is present + stable, and NO history is sent.

import { describe, it, expect, vi, beforeEach } from "vitest";
import { renderHook, act } from "@testing-library/react";

const apiPostMock = vi.fn<
  (url: string, body?: unknown, opts?: unknown) => Promise<unknown>
>();
vi.mock("@/lib/api", () => ({
  api: {
    post: (url: string, body?: unknown, opts?: unknown) =>
      apiPostMock(url, body, opts),
    get: vi.fn(),
  },
}));

vi.mock("../../uploads", () => ({
  uploadChatFiles: vi.fn(),
  FileTooLargeError: class FileTooLargeError extends Error {},
}));

import { useChatSend } from "../useChatSend";
import { getConversationId, rotateConversationId } from "../chatContext";

beforeEach(() => {
  apiPostMock.mockReset();
  try {
    window.localStorage.clear();
  } catch {
    /* ignore */
  }
});

function bodyOf(callIndex: number): any {
  return apiPostMock.mock.calls[callIndex][1] as any;
}
function ctxOf(callIndex: number): string {
  return bodyOf(callIndex).params.message.contextId;
}

describe("useChatSend — stable conversation contextId (tenant-agent BUG 3)", () => {
  it("threads a contextId on every send, STABLE across turns", async () => {
    apiPostMock.mockImplementation(() => new Promise(() => {})); // hang (reply pending)

    const { result } = renderHook(() => useChatSend("ws-ctx", {}));

    await act(async () => {
      await result.current.sendMessage("turn 1");
      await Promise.resolve();
    });
    await act(async () => {
      await result.current.sendMessage("turn 2");
      await Promise.resolve();
    });

    const c1 = ctxOf(0);
    const c2 = ctxOf(1);
    expect(c1).toBeTruthy();
    expect(c1).toBe(c2); // STABLE across turns — the whole point
    expect(c1).toBe("canvas-ws-ctx"); // workspace-scoped
    // Matches the persisted conversation id.
    expect(c1).toBe(getConversationId("ws-ctx"));
  });

  it("rotates the contextId on a new session so the next send starts fresh", async () => {
    apiPostMock.mockImplementation(() => new Promise(() => {}));

    const { result } = renderHook(() => useChatSend("ws-rot", {}));

    await act(async () => {
      await result.current.sendMessage("before new session");
      await Promise.resolve();
    });
    const before = ctxOf(0);

    // Simulate "New session" (ChatTab calls rotateConversationId).
    rotateConversationId("ws-rot");

    await act(async () => {
      await result.current.sendMessage("after new session");
      await Promise.resolve();
    });
    const after = ctxOf(1);

    expect(before).toBeTruthy();
    expect(after).toBeTruthy();
    expect(after).not.toBe(before); // fresh conversation → fresh agent context
  });

  it("does NOT force-inject prior turns — no metadata/history in the a2a payload", async () => {
    apiPostMock.mockImplementation(() => new Promise(() => {}));

    // Even after several turns (which would previously have been packed into a
    // metadata.history blob), the payload must carry ONLY the current message.
    const { result } = renderHook(() => useChatSend("ws-nohist", {}));

    await act(async () => {
      await result.current.sendMessage("first turn");
      await Promise.resolve();
    });
    await act(async () => {
      await result.current.sendMessage("second turn");
      await Promise.resolve();
    });

    const body = bodyOf(1);
    // The session key is still threaded (continuity comes from the resumed
    // native session, not from injected history).
    expect(body.params.message.contextId).toBeTruthy();
    // No history force-feed anywhere in the request.
    expect(body.params.metadata).toBeUndefined();
    expect("history" in body.params).toBe(false);
    expect(body.params.message.history).toBeUndefined();
    // The only parts sent are the current message's own parts.
    expect(body.params.message.parts).toEqual([
      { kind: "text", text: "second turn" },
    ]);
  });
});
