// @vitest-environment jsdom
//
// Regression for the "My Chat" doubling bug. The reconcile re-fetches the
// same chat-history window every 10s and on every WS reconnect, merging it
// via mergeReconciledMessages. The root cause was a backend that minted a
// fresh id per row per fetch: keying the merge on `m.id` then never
// collided, so the whole window was re-appended and the visible list
// doubled on every poll (36→72→…). The store now returns a STABLE per-row
// id; the frontend also keys the merge on the (timestamp, role, content)
// identity tuple as defense-in-depth. These tests pin both:
//
//   1. repeated reconciles of the same window keep the list flat, and
//   2. even if a fetch returns DIFFERENT ids for the same logical messages
//      (a regressed/unstable backend id), the merge still does not grow —
//      which is exactly what the old `m.id`-first key failed to do.

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

    // Five re-fetches of the identical window must NOT append anything.
    expect(result.current.messages).toHaveLength(4);
  });

  it("does not double even if the backend re-mints ids for the same rows (tuple-keyed defense)", async () => {
    apiGetMock.mockResolvedValueOnce({ messages: window0, reached_end: true });

    const { result } = renderHook(() => useChatHistory("ws-remint"));
    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.messages).toHaveLength(4);

    // Each reconcile returns the SAME logical messages but with FRESH ids,
    // simulating a store that regressed to a per-fetch id. With the old
    // `m.id`-first key this grew the list by 4 every poll; the tuple key
    // must keep it flat.
    let mint = 0;
    for (let r = 0; r < 5; r++) {
      const reminted = window0.map((m) => ({ ...m, id: `remint-${mint++}-${m.role}` }));
      apiGetMock.mockResolvedValueOnce({ messages: reminted, reached_end: true });
      await act(async () => {
        await result.current.reconcile();
      });
    }

    expect(result.current.messages).toHaveLength(4);
    // Sanity: the surviving copies are the reminted ones (fetched overwrites
    // existing under the same tuple key), and content is intact.
    expect(result.current.messages.map((m) => m.content)).toEqual(["u1", "a1", "u2", "a2"]);
  });
});
