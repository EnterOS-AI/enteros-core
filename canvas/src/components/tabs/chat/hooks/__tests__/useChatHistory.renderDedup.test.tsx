// @vitest-environment jsdom
//
// Regression for task #187: DURABLE (window-free) concierge render-dedup.
//
// The optimistic/live agent bubble (client-minted UUID) and its reconciled DB
// copy ("<rowID>:agent") live in two id-spaces, so the id-keyed merge never
// collides them — the reconcile must collapse the optimistic copy into the
// authoritative DB one. The PREVIOUS fix did that only when the two timestamps
// were within a fixed 60s clock-skew window. That window is a fixed-timeout
// heuristic: on an arbitrarily slow COLD turn the gap between when the greeting
// was rendered optimistically and its persisted twin's created_at can exceed
// 60s, the window match fails, and BOTH bubbles render — the concierge greeting
// appears TWICE.
//
// These tests pin the window-free contract: dedup by STABLE CONTENT IDENTITY
// (role+content) with count-based one-to-one matching, so a greeting collapses
// to ONE copy no matter how slow the turn, while distinct rows and not-yet-
// persisted repeats are never lost. The >60s cases below FAIL on the old
// windowed code and PASS on the identity code.

import { describe, it, expect, vi, beforeEach } from "vitest";
import { renderHook, waitFor, act } from "@testing-library/react";

const apiGetMock = vi.fn<(path: string, opts?: unknown) => Promise<unknown>>();

vi.mock("@/lib/api", () => ({
  api: { get: (path: string, opts?: unknown) => apiGetMock(path, opts) },
}));

import { useChatHistory, mergeReconciledMessages } from "../useChatHistory";
import { stableAgentReplyId } from "../useChatSend";

type Msg = {
  id: string;
  role: "user" | "agent" | "system";
  content: string;
  timestamp: string;
};

const GREETING =
  "Hi! I'm your Org Concierge. I can spin up teammates and route work — what would you like to do?";

beforeEach(() => {
  apiGetMock.mockReset();
});

describe("mergeReconciledMessages — window-free durable dedup (#187)", () => {
  it("collapses a SLOW (>60s) greeting reconcile to ONE copy (old 60s window would double it)", () => {
    // Optimistic greeting rendered at T. Its persisted DB twin's created_at
    // reads 120s away — FAR outside the retired 60s clock-skew window.
    const optimistic: Msg = {
      id: "greet-client-uuid",
      role: "agent",
      content: GREETING,
      timestamp: "2026-07-04T00:10:00.000Z",
    };
    const dbCopy: Msg = {
      id: "row-1:agent",
      role: "agent",
      content: GREETING,
      timestamp: "2026-07-04T00:12:00.000Z", // +120s
    };

    const merged = mergeReconciledMessages(
      [optimistic] as never,
      [dbCopy] as never,
    );

    expect(merged).toHaveLength(1);
    expect(merged[0].id).toBe("row-1:agent");
  });

  it("still collapses when the DB copy reads EARLIER than the optimistic bubble (clock skew, >60s)", () => {
    // Server persists then broadcasts, so created_at can precede the client's
    // receive-time stamp. A directional timestamp compare would miss this; the
    // identity match does not care about direction OR magnitude.
    const optimistic: Msg = {
      id: "greet-client-uuid",
      role: "agent",
      content: GREETING,
      timestamp: "2026-07-04T00:12:00.000Z",
    };
    const dbCopy: Msg = {
      id: "row-1:agent",
      role: "agent",
      content: GREETING,
      timestamp: "2026-07-04T00:10:00.000Z", // 120s EARLIER
    };

    const merged = mergeReconciledMessages(
      [optimistic] as never,
      [dbCopy] as never,
    );

    expect(merged).toHaveLength(1);
    expect(merged[0].id).toBe("row-1:agent");
  });

  it("keeps TWO distinct DB rows that share content (doubling invariant — never drops an authoritative copy)", () => {
    const dbA: Msg = {
      id: "row-7:user",
      role: "user",
      content: "ok",
      timestamp: "2026-07-04T00:05:00.000Z",
    };
    const dbB: Msg = {
      id: "row-8:user",
      role: "user",
      content: "ok",
      timestamp: "2026-07-04T00:05:00.000Z",
    };

    const merged = mergeReconciledMessages([] as never, [dbA, dbB] as never);
    expect(merged.map((m) => m.id).sort()).toEqual(["row-7:user", "row-8:user"]);
  });

  it("preserves a SURPLUS optimistic repeat whose own DB row has not been fetched yet (no message loss)", () => {
    // One persisted "hi" (row-1) plus TWO optimistic "hi" bubbles: only ONE of
    // them is the twin of row-1; the other is a genuine repeat still awaiting
    // its own DB row. Count-matching drops exactly one and keeps the surplus.
    const dbCopy: Msg = {
      id: "row-1:agent",
      role: "agent",
      content: "hi",
      timestamp: "2026-07-04T00:00:00.000Z",
    };
    const optA: Msg = {
      id: "opt-a",
      role: "agent",
      content: "hi",
      timestamp: "2026-07-04T00:00:01.000Z",
    };
    const optB: Msg = {
      id: "opt-b",
      role: "agent",
      content: "hi",
      timestamp: "2026-07-04T00:03:00.000Z",
    };

    const merged = mergeReconciledMessages(
      [dbCopy, optA, optB] as never,
      [] as never,
    );
    // Keep the authoritative DB copy + exactly one surplus optimistic bubble.
    expect(merged).toHaveLength(2);
    expect(merged.some((m) => m.id === "row-1:agent")).toBe(true);
    expect(merged.filter((m) => m.id === "opt-a" || m.id === "opt-b")).toHaveLength(1);
  });
});

describe("useChatHistory — slow greeting reconcile shows ONE bubble (#187, hook)", () => {
  it("a reconcile arriving long after the optimistic greeting still renders exactly one greeting", async () => {
    // Empty initial load.
    apiGetMock.mockResolvedValue({ messages: [], reached_end: true });
    const { result } = renderHook(() => useChatHistory("ws-greet"));
    await waitFor(() => expect(result.current.loading).toBe(false));

    // Concierge greeting arrives via the live socket path (optimistic bubble
    // with a client-minted id, NO ":agent" suffix).
    const optimistic: Msg = {
      id: "greet-client-uuid",
      role: "agent",
      content: GREETING,
      timestamp: "2026-07-04T00:10:00.000Z",
    };
    act(() => {
      result.current.setMessages(
        [optimistic] as unknown as Parameters<typeof result.current.setMessages>[0],
      );
    });
    expect(result.current.messages).toHaveLength(1);

    // A background reconcile finally brings the persisted copy — but its
    // created_at is >60s from the optimistic render (a slow cold turn).
    const dbWindow: Msg[] = [
      {
        id: "row-1:agent",
        role: "agent",
        content: GREETING,
        timestamp: "2026-07-04T00:12:30.000Z", // +150s
      },
    ];
    apiGetMock.mockResolvedValue({ messages: dbWindow, reached_end: true });
    await act(async () => {
      await result.current.reconcile();
    });

    // Exactly ONE greeting, the authoritative DB copy — no time-window double.
    expect(result.current.messages).toHaveLength(1);
    expect(result.current.messages[0].id).toBe("row-1:agent");
    expect(result.current.messages[0].content).toBe(GREETING);
  });
});

describe("stableAgentReplyId — deterministic, window-free reply identity (#187)", () => {
  it("is deterministic for a given user turn and unique across turns", () => {
    expect(stableAgentReplyId("mid-1")).toBe(stableAgentReplyId("mid-1"));
    expect(stableAgentReplyId("mid-1")).not.toBe(stableAgentReplyId("mid-2"));
  });

  it("does NOT end in ':user'/':agent' so the reconcile treats it as optimistic", () => {
    // useChatHistory.isReconciledDbId keys off a trailing ":user"/":agent".
    expect(/:(?:user|agent)$/.test(stableAgentReplyId("mid-1"))).toBe(false);
  });
});
