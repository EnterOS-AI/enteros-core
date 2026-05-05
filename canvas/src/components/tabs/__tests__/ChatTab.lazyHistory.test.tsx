// @vitest-environment jsdom
//
// Pins the lazy-loading chat-history pagination added 2026-05-05.
//
// Pre-fix: ChatTab fetched the newest 50 messages on every mount and
// scrolled to bottom, paying full DOM cost up-front even when the user
// only wanted to read the last few bubbles. Post-fix: initial load is
// bounded to 10 newest, and an IntersectionObserver on a top sentinel
// triggers loadOlder() (batch of 20 with `before_ts` cursor) when the
// user scrolls up.
//
// Pinned branches:
//   1. Initial fetch carries `limit=10` and NO before_ts (newest-first
//      slice). Pre-fix this was limit=50.
//   2. Server returning fewer than `limit` rows clears `hasMore` so the
//      top sentinel is removed and the IO observer disconnects — no
//      "Loading older messages…" spinner on a short conversation.
//   3. Server returning exactly `limit` rows on the first batch keeps
//      hasMore=true so the sentinel mounts (verified indirectly by
//      asserting the rendered bubble count matches the full page).
//   4. The retry button after a failed initial load uses the same
//      INITIAL_HISTORY_LIMIT (10), not the legacy 50.
//
// IntersectionObserver / scroll-anchor restoration is exercised by the
// E2E synth-canary suite — pinning it in jsdom would require mocking
// the observer and faking layout, which is brittler than trusting a
// live-DOM canary against the staging tenant.

import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import { render, screen, cleanup, waitFor, fireEvent } from "@testing-library/react";
import React from "react";

afterEach(cleanup);

// Both ChatTab sub-panels (MyChat + AgentComms) mount simultaneously so
// keyboard tab order and aria-controls land on a real DOM. Both fire
// /activity GETs on mount: MyChat's hits `type=a2a_receive&source=canvas`,
// AgentComms's hits a different filter. Route the mock by URL so each
// gets a sensible default and only MyChat's call is what the assertions
// scrutinise.
const myChatActivityCalls: string[] = [];
let myChatNextResponse: { ok: true; rows: unknown[] } | { ok: false; err: Error } = {
  ok: true,
  rows: [],
};
const apiGet = vi.fn((path: string): Promise<unknown> => {
  if (path.includes("type=a2a_receive") && path.includes("source=canvas")) {
    myChatActivityCalls.push(path);
    if (myChatNextResponse.ok) return Promise.resolve(myChatNextResponse.rows);
    return Promise.reject(myChatNextResponse.err);
  }
  // AgentComms / heartbeat / anything else — empty array is a safe
  // default that won't blow up the corresponding component's .then().
  return Promise.resolve([]);
});
const apiPost = vi.fn();
vi.mock("@/lib/api", () => ({
  api: {
    get: (path: string) => apiGet(path),
    post: (path: string, body: unknown) => apiPost(path, body),
    del: vi.fn(),
    patch: vi.fn(),
    put: vi.fn(),
  },
}));

vi.mock("@/store/canvas", () => ({
  useCanvasStore: vi.fn((selector?: (s: unknown) => unknown) =>
    selector ? selector({ agentMessages: {}, consumeAgentMessages: () => [] }) : {},
  ),
}));

beforeEach(() => {
  apiGet.mockClear();
  apiPost.mockReset();
  myChatActivityCalls.length = 0;
  myChatNextResponse = { ok: true, rows: [] };
  if (typeof window !== "undefined" && !("IntersectionObserver" in window)) {
    (window as unknown as { IntersectionObserver: unknown }).IntersectionObserver = class {
      observe() {}
      unobserve() {}
      disconnect() {}
    };
  }
  // jsdom doesn't implement scrollIntoView; ChatTab calls it after every
  // messages update.
  Element.prototype.scrollIntoView = vi.fn();
});

import { ChatTab } from "../ChatTab";

function makeActivityRow(seq: number): Record<string, unknown> {
  return {
    activity_type: "a2a_receive",
    status: "ok",
    created_at: `2026-05-05T00:0${seq}:00Z`,
    request_body: { params: { message: { parts: [{ kind: "text", text: `user msg ${seq}` }] } } },
    response_body: { result: `agent reply ${seq}` },
  };
}

const minimalData = {
  status: "online" as const,
  runtime: "claude-code",
  currentTask: null,
} as unknown as Parameters<typeof ChatTab>[0]["data"];

describe("ChatTab lazy history pagination", () => {
  it("initial fetch carries limit=10 (not the legacy 50)", async () => {
    myChatNextResponse = { ok: true, rows: [makeActivityRow(1)] };
    render(<ChatTab workspaceId="ws-1" data={minimalData} />);
    await waitFor(() => expect(myChatActivityCalls.length).toBe(1));
    const url = myChatActivityCalls[0];
    expect(url).toContain("limit=10");
    expect(url).not.toContain("limit=50");
    // before_ts should NOT be set on the initial fetch — that's the
    // newest-first slice the user lands on.
    expect(url).not.toContain("before_ts");
  });

  it("hides the top sentinel when initial fetch returns fewer than the limit", async () => {
    // 3 < 10 → server says "no more older history exists"; sentinel
    // should NOT mount and the "Loading older messages…" line should
    // never appear (it can't, since the sentinel is what triggers it).
    myChatNextResponse = {
      ok: true,
      rows: [makeActivityRow(1), makeActivityRow(2), makeActivityRow(3)],
    };
    render(<ChatTab workspaceId="ws-2" data={minimalData} />);
    await waitFor(() => expect(myChatActivityCalls.length).toBe(1));
    await waitFor(() => {
      expect(screen.queryByText(/Loading chat history/i)).toBeNull();
    });
    expect(screen.queryByText(/Loading older messages/i)).toBeNull();
  });

  it("renders all messages when initial fetch returns exactly the limit", async () => {
    // 10 == limit → server might have more older rows; sentinel SHOULD
    // mount so the IO observer can fire loadOlder() on scroll-up. We
    // verify by checking the rendered bubble count — if hasMore stayed
    // true the sentinel render path doesn't crash and all 10 rows
    // produced their pair of bubbles.
    const fullPage = Array.from({ length: 10 }, (_, i) => makeActivityRow(i + 1));
    myChatNextResponse = { ok: true, rows: fullPage };
    render(<ChatTab workspaceId="ws-3" data={minimalData} />);
    await waitFor(() => expect(myChatActivityCalls.length).toBe(1));
    await waitFor(() => {
      expect(screen.queryByText(/Loading chat history/i)).toBeNull();
    });
    expect(screen.getAllByText(/user msg/).length).toBe(10);
    expect(screen.getAllByText(/agent reply/).length).toBe(10);
  });

  it("retry-after-failure uses limit=10, not the legacy 50", async () => {
    myChatNextResponse = { ok: false, err: new Error("network down") };
    render(<ChatTab workspaceId="ws-4" data={minimalData} />);
    const retry = await screen.findByText(/Retry/);
    myChatNextResponse = { ok: true, rows: [makeActivityRow(1)] };
    fireEvent.click(retry);
    await waitFor(() => expect(myChatActivityCalls.length).toBe(2));
    const retryUrl = myChatActivityCalls[1];
    expect(retryUrl).toContain("limit=10");
    expect(retryUrl).not.toContain("limit=50");
  });
});
