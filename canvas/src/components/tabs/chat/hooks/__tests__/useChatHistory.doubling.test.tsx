// @vitest-environment jsdom
//
// Regression for the "My Chat" doubling bug. The reconcile re-fetches the
// same chat-history window every 10s and on every WS reconnect, merging it
// via mergeReconciledMessages. The root cause was a backend that minted a
// fresh id per row per fetch: keying the merge on `m.id` then never
// collided, so the whole window was re-appended and the visible list
// doubled on every poll (36→72→…). The store now returns a STABLE per-row
// id (activity_logs PK + bubble kind), so the merge keys on that id and
// dedupes correctly. These tests pin both halves of the contract:
//
//   1. repeated reconciles of the same window keep the list flat (the
//      stable id dedupes), and
//   2. two DISTINCT messages that happen to share (timestamp, role,
//      content) but carry different stable ids BOTH survive — an id key
//      preserves them where a (timestamp+role+content) tuple key would
//      silently drop one (message loss).

import { describe, it, expect, vi, beforeEach } from "vitest";
import { renderHook, waitFor, act } from "@testing-library/react";

const apiGetMock = vi.fn<(path: string, opts?: unknown) => Promise<unknown>>();

vi.mock("@/lib/api", () => ({
  api: { get: (path: string, opts?: unknown) => apiGetMock(path, opts) },
}));

import { useChatHistory } from "../useChatHistory";

type Msg = {
  id: string;
  role: "user" | "agent" | "system";
  content: string;
  timestamp: string;
};

// A stable chat-history window: two turns (row-1, row-2), each a
// user+agent pair sharing the row's created_at timestamp.
const window0: Msg[] = [
  { id: "row-1:user", role: "user", content: "u1", timestamp: "2026-06-27T00:00:00.000Z" },
  { id: "row-1:agent", role: "agent", content: "a1", timestamp: "2026-06-27T00:00:00.000Z" },
  { id: "row-2:user", role: "user", content: "u2", timestamp: "2026-06-27T00:01:00.000Z" },
  { id: "row-2:agent", role: "agent", content: "a2", timestamp: "2026-06-27T00:01:00.000Z" },
];

beforeEach(() => {
  apiGetMock.mockReset();
});

describe("useChatHistory — My Chat doubling regression", () => {
  it("repeated reconciles of the same window do not grow the list (stable ids)", async () => {
    apiGetMock.mockResolvedValue({ messages: window0, reached_end: true });

    const { result } = renderHook(() => useChatHistory("ws-dbl"));
    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.messages).toHaveLength(4);

    for (let i = 0; i < 5; i++) {
      await act(async () => {
        await result.current.reconcile();
      });
    }

    // Five re-fetches of the identical window must NOT append anything:
    // the stable per-row id collides on every merge.
    expect(result.current.messages).toHaveLength(4);
    expect(result.current.messages.map((m) => m.id)).toEqual([
      "row-1:user",
      "row-1:agent",
      "row-2:user",
      "row-2:agent",
    ]);
  });

  it("keeps two distinct messages that share timestamp+role+content but differ by id (no silent drop)", async () => {
    // Two SEPARATE persisted rows produced the same short user text ("ok")
    // at the same created_at. They are genuinely distinct messages and must
    // both remain visible. A (timestamp, role, content) tuple key collapses
    // them into one — message loss. The stable per-row id keeps them apart.
    const collidingWindow: Msg[] = [
      { id: "row-7:user", role: "user", content: "ok", timestamp: "2026-06-27T00:05:00.000Z" },
      { id: "row-8:user", role: "user", content: "ok", timestamp: "2026-06-27T00:05:00.000Z" },
    ];
    apiGetMock.mockResolvedValue({ messages: collidingWindow, reached_end: true });

    const { result } = renderHook(() => useChatHistory("ws-collide"));
    await waitFor(() => expect(result.current.loading).toBe(false));

    // Both distinct rows survive the initial load...
    expect(result.current.messages).toHaveLength(2);
    expect(result.current.messages.map((m) => m.id).sort()).toEqual([
      "row-7:user",
      "row-8:user",
    ]);

    // ...and survive repeated reconciles without either doubling (stable id
    // dedupes) or collapsing (distinct ids are preserved).
    for (let i = 0; i < 3; i++) {
      await act(async () => {
        await result.current.reconcile();
      });
    }
    expect(result.current.messages).toHaveLength(2);
    expect(result.current.messages.map((m) => m.id).sort()).toEqual([
      "row-7:user",
      "row-8:user",
    ]);
  });

  it("collapses an optimistic bubble into its reconciled DB copy (BUG 2: no duplicate render)", async () => {
    // The optimistic/live bubble (client-minted UUID id) and its reconciled DB
    // copy ("<rowID>:user|agent") live in two id-spaces, so the id-keyed merge
    // never collides them — before the fix BOTH rendered (a stable ×2). The
    // reconcile must collapse the optimistic copy into the authoritative DB one.
    apiGetMock.mockResolvedValue({ messages: [], reached_end: true });
    const { result } = renderHook(() => useChatHistory("ws-opt"));
    await waitFor(() => expect(result.current.loading).toBe(false));

    // Optimistic user + agent bubbles: client-minted ids with NO ":user"/":agent".
    const optimistic: Msg[] = [
      { id: "6f1c-uuid-user", role: "user", content: "hello", timestamp: "2026-06-27T00:10:00.000Z" },
      { id: "9a2b-uuid-agent", role: "agent", content: "hi there", timestamp: "2026-06-27T00:10:01.000Z" },
    ];
    act(() => {
      result.current.setMessages(optimistic as unknown as Parameters<typeof result.current.setMessages>[0]);
    });
    expect(result.current.messages).toHaveLength(2);

    // Reconcile brings the persisted DB copies (server ids, same content, ~same time).
    const dbWindow: Msg[] = [
      { id: "row-9:user", role: "user", content: "hello", timestamp: "2026-06-27T00:10:00.500Z" },
      { id: "row-9:agent", role: "agent", content: "hi there", timestamp: "2026-06-27T00:10:01.500Z" },
    ];
    apiGetMock.mockResolvedValue({ messages: dbWindow, reached_end: true });
    await act(async () => {
      await result.current.reconcile();
    });

    // Both optimistic bubbles collapsed into their DB copies → exactly 2, all DB ids.
    expect(result.current.messages).toHaveLength(2);
    expect(result.current.messages.map((m) => m.id).sort()).toEqual([
      "row-9:agent",
      "row-9:user",
    ]);
  });
});
