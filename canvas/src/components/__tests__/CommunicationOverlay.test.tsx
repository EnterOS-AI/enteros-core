// @vitest-environment jsdom
/**
 * CommunicationOverlay tests — pin the rate-limit fix shipped 2026-05-04.
 *
 * The overlay polls /workspaces/:id/activity?limit=5 for each online
 * workspace. Pre-fix it (a) polled regardless of visibility and (b)
 * fanned out to 6 workspaces every 10s. With 8+ workspaces a user
 * triggered sustained 429s (server-side rate limit is 600 req/min/IP).
 *
 * These tests pin:
 *  1. Fan-out cap of 3 — even with 6 online nodes, only 3 fetches
 *  2. Visibility gate — when collapsed, no polling
 *
 * If a future refactor pushes either dial back up, CI fails before
 * the regression hits a paying tenant.
 */
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, cleanup, act, fireEvent } from "@testing-library/react";

// ── Mocks (hoisted before imports) ────────────────────────────────────────────

vi.mock("@/lib/api", () => ({
  api: { get: vi.fn() },
}));

// Six online nodes — enough to verify the cap of 3.
const mockStoreState = {
  selectedNodeId: null as string | null,
  nodes: [
    { id: "ws-1", data: { status: "online", name: "ws-1" } },
    { id: "ws-2", data: { status: "online", name: "ws-2" } },
    { id: "ws-3", data: { status: "online", name: "ws-3" } },
    { id: "ws-4", data: { status: "online", name: "ws-4" } },
    { id: "ws-5", data: { status: "online", name: "ws-5" } },
    { id: "ws-6", data: { status: "online", name: "ws-6" } },
    { id: "ws-offline", data: { status: "offline", name: "off" } },
  ],
};

vi.mock("@/store/canvas", () => ({
  useCanvasStore: vi.fn(
    (selector: (s: typeof mockStoreState) => unknown) =>
      selector(mockStoreState)
  ),
}));

// design-tokens has named exports — keep the shape minimal.
vi.mock("@/lib/design-tokens", () => ({
  COMM_TYPE_LABELS: {
    a2a_send: "→",
    a2a_receive: "←",
    task_update: "✓",
  },
}));

// ── Imports (after mocks) ─────────────────────────────────────────────────────

import { api } from "@/lib/api";
import { CommunicationOverlay } from "../CommunicationOverlay";

const mockGet = vi.mocked(api.get);

// ── Setup ─────────────────────────────────────────────────────────────────────

beforeEach(() => {
  vi.useFakeTimers();
  mockGet.mockReset();
  mockGet.mockResolvedValue([]);
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

// ── Tests ─────────────────────────────────────────────────────────────────────

describe("CommunicationOverlay — fan-out cap", () => {
  it("polls at most 3 of 6 online workspaces (rate-limit floor)", async () => {
    await act(async () => {
      render(<CommunicationOverlay />);
    });
    // Mount fires the first poll synchronously (no interval tick yet).
    // Pre-fix: 6 calls. Post-fix: 3.
    expect(mockGet).toHaveBeenCalledTimes(3);
    // Verify the calls are for the FIRST 3 online nodes (slice order).
    expect(mockGet).toHaveBeenCalledWith("/workspaces/ws-1/activity?limit=5");
    expect(mockGet).toHaveBeenCalledWith("/workspaces/ws-2/activity?limit=5");
    expect(mockGet).toHaveBeenCalledWith("/workspaces/ws-3/activity?limit=5");
  });

  it("never polls offline workspaces", async () => {
    await act(async () => {
      render(<CommunicationOverlay />);
    });
    expect(mockGet).not.toHaveBeenCalledWith(
      "/workspaces/ws-offline/activity?limit=5",
    );
  });
});

describe("CommunicationOverlay — visibility gate", () => {
  it("uses 30s interval cadence (was 10s pre-fix)", async () => {
    await act(async () => {
      render(<CommunicationOverlay />);
    });
    expect(mockGet).toHaveBeenCalledTimes(3); // initial mount poll

    // Advance 10s — pre-fix this would fire another poll. Post-fix: silent.
    await act(async () => {
      vi.advanceTimersByTime(10_000);
    });
    expect(mockGet).toHaveBeenCalledTimes(3);

    // Advance to 30s — interval fires.
    await act(async () => {
      vi.advanceTimersByTime(20_000);
    });
    expect(mockGet).toHaveBeenCalledTimes(6); // +3 from second tick
  });
});
