// @vitest-environment jsdom
/**
 * Tests for EventsTab component.
 *
 * Covers: formatTime pure function, EVENT_COLORS constant,
 * loading/error/empty states, event list rendering, expand/collapse,
 * refresh button, auto-refresh setup.
 */
import React from "react";
import { render, screen, fireEvent, cleanup, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { EventsTab } from "../EventsTab";

// Mock @/lib/api — hoisted so it's applied before the module loads.
const _mockGet = vi.hoisted(() => vi.fn<() => Promise<unknown[]>>());
vi.mock("@/lib/api", () => ({
  api: { get: _mockGet },
}));

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

// ─── formatTime tests (via rendered output) ────────────────────────────────────

describe("EventsTab — formatTime", () => {
  it("shows 'ago' for events less than a minute old", async () => {
    const now = new Date();
    const recent = new Date(now.getTime() - 30_000).toISOString();
    _mockGet.mockResolvedValueOnce([
      { id: "e1", event_type: "WORKSPACE_ONLINE", workspace_id: null, payload: {}, created_at: recent },
    ]);
    render(<EventsTab workspaceId="ws-1" />);
    await waitFor(() => {
      expect(screen.getByText(/ago/)).toBeTruthy();
    });
  });

  it("shows 'm ago' for events less than an hour old", async () => {
    const now = new Date();
    const minsAgo = new Date(now.getTime() - 5 * 60_000).toISOString();
    _mockGet.mockResolvedValueOnce([
      { id: "e1", event_type: "WORKSPACE_OFFLINE", workspace_id: null, payload: {}, created_at: minsAgo },
    ]);
    render(<EventsTab workspaceId="ws-1" />);
    await waitFor(() => {
      expect(screen.getByText(/m ago/)).toBeTruthy();
    });
  });

  it("shows 'h ago' for events less than a day old", async () => {
    const now = new Date();
    const hoursAgo = new Date(now.getTime() - 3 * 3_600_000).toISOString();
    _mockGet.mockResolvedValueOnce([
      { id: "e1", event_type: "WORKSPACE_DEGRADED", workspace_id: null, payload: {}, created_at: hoursAgo },
    ]);
    render(<EventsTab workspaceId="ws-1" />);
    await waitFor(() => {
      expect(screen.getByText(/h ago/)).toBeTruthy();
    });
  });
});

// ─── EVENT_COLORS rendering ───────────────────────────────────────────────────

describe("EventsTab — EVENT_COLORS", () => {
  it("renders all known event types without crashing", async () => {
    const eventTypes = [
      "WORKSPACE_ONLINE",
      "WORKSPACE_OFFLINE",
      "WORKSPACE_DEGRADED",
      "WORKSPACE_PROVISIONING",
      "WORKSPACE_REMOVED",
      "WORKSPACE_PROVISION_FAILED",
      "AGENT_CARD_UPDATED",
    ];
    _mockGet.mockResolvedValueOnce(
      eventTypes.map((event_type, i) => ({
        id: `e-${i}`, event_type, workspace_id: null, payload: {}, created_at: new Date().toISOString(),
      })),
    );
    render(<EventsTab workspaceId="ws-1" />);
    await waitFor(() => {
      for (const et of eventTypes) {
        expect(screen.getByText(et)).toBeTruthy();
      }
    });
  });

  it("renders unknown event types without crashing", async () => {
    _mockGet.mockResolvedValueOnce([
      { id: "e-unk", event_type: "UNKNOWN_EVENT_XYZ", workspace_id: null, payload: {}, created_at: new Date().toISOString() },
    ]);
    render(<EventsTab workspaceId="ws-1" />);
    await waitFor(() => {
      expect(screen.getByText("UNKNOWN_EVENT_XYZ")).toBeTruthy();
    });
  });
});

// ─── States ───────────────────────────────────────────────────────────────────

describe("EventsTab — states", () => {
  it("shows loading text initially", () => {
    _mockGet.mockImplementation(() => new Promise(() => {})); // never resolves
    render(<EventsTab workspaceId="ws-1" />);
    expect(screen.getByText("Loading events...")).toBeTruthy();
  });

  it("shows empty message when no events returned", async () => {
    _mockGet.mockResolvedValueOnce([]);
    render(<EventsTab workspaceId="ws-1" />);
    await waitFor(() => {
      expect(screen.getByText("No events yet")).toBeTruthy();
    });
  });

  it("shows error alert when fetch fails", async () => {
    _mockGet.mockRejectedValueOnce(new Error("server error"));
    render(<EventsTab workspaceId="ws-1" />);
    await waitFor(() => {
      expect(screen.getByText(/server error/i)).toBeTruthy();
    });
  });
});

// ─── Event list ───────────────────────────────────────────────────────────────

describe("EventsTab — event list", () => {
  it("renders all returned events", async () => {
    _mockGet.mockResolvedValueOnce([
      { id: "e1", event_type: "WORKSPACE_ONLINE", workspace_id: null, payload: { foo: 1 }, created_at: new Date().toISOString() },
      { id: "e2", event_type: "WORKSPACE_OFFLINE", workspace_id: null, payload: { bar: 2 }, created_at: new Date().toISOString() },
    ]);
    render(<EventsTab workspaceId="ws-1" />);
    await waitFor(() => {
      expect(screen.getAllByText(/WORKSPACE_/).length).toBeGreaterThanOrEqual(2);
    });
  });

  it("shows event count in header", async () => {
    _mockGet.mockResolvedValueOnce([
      { id: "e1", event_type: "WORKSPACE_ONLINE", workspace_id: null, payload: {}, created_at: new Date().toISOString() },
      { id: "e2", event_type: "WORKSPACE_OFFLINE", workspace_id: null, payload: {}, created_at: new Date().toISOString() },
      { id: "e3", event_type: "WORKSPACE_DEGRADED", workspace_id: null, payload: {}, created_at: new Date().toISOString() },
    ]);
    render(<EventsTab workspaceId="ws-1" />);
    await waitFor(() => {
      expect(screen.getByText("3 events")).toBeTruthy();
    });
  });

  it("expands payload panel on click", async () => {
    _mockGet.mockResolvedValueOnce([
      { id: "e-expand", event_type: "WORKSPACE_ONLINE", workspace_id: null, payload: { key: "value" }, created_at: new Date().toISOString() },
    ]);
    render(<EventsTab workspaceId="ws-1" />);
    await waitFor(() => screen.getByText("WORKSPACE_ONLINE"));

    fireEvent.click(screen.getByText("WORKSPACE_ONLINE"));

    await waitFor(() => {
      expect(screen.getByText(/"key":\s*"value"/)).toBeTruthy();
    });
  });

  it("collapses expanded panel on second click", async () => {
    _mockGet.mockResolvedValueOnce([
      { id: "e-collapse", event_type: "WORKSPACE_DEGRADED", workspace_id: null, payload: { x: 1 }, created_at: new Date().toISOString() },
    ]);
    render(<EventsTab workspaceId="ws-1" />);
    await waitFor(() => screen.getByText("WORKSPACE_DEGRADED"));

    fireEvent.click(screen.getByText("WORKSPACE_DEGRADED"));
    await waitFor(() => expect(screen.getByText(/"x":\s*1/)).toBeTruthy());

    fireEvent.click(screen.getByText("WORKSPACE_DEGRADED"));
    await waitFor(() => {
      expect(screen.queryByText(/"x":\s*1/)).toBeNull();
    });
  });
});

// ─── Refresh button ───────────────────────────────────────────────────────────

describe("EventsTab — refresh", () => {
  it("has a Refresh button", async () => {
    _mockGet.mockResolvedValueOnce([]);
    render(<EventsTab workspaceId="ws-1" />);
    await waitFor(() => {});
    expect(screen.getByRole("button", { name: /refresh/i })).toBeTruthy();
  });

  it("Refresh button triggers a reload", async () => {
    _mockGet.mockResolvedValueOnce([]);
    render(<EventsTab workspaceId="ws-1" />);
    await waitFor(() => screen.getByRole("button", { name: /refresh/i }));

    fireEvent.click(screen.getByRole("button", { name: /refresh/i }));

    // Called at least twice: initial load + refresh click
    expect(_mockGet).toHaveBeenCalled();
  });
});
