// @vitest-environment jsdom
/**
 * Tests for MonitorPanel — the OSS monitoring dashboard (A2A traffic, topology,
 * HITL). Asserts: real data renders the live series + real counts; an empty
 * activity_logs renders the HONEST empty state (not a fabricated curve); the
 * window toggle refetches with the new window; and the HITL queue reuses
 * RequestsInbox for both kinds.
 *
 * Mock style mirrors RequestsInbox.test.tsx: vi.hoisted refs + file-level
 * vi.mock for @/lib/api, the socket hook, the embedded RequestsInbox, the CSS
 * module, and the concierge icons.
 */
import React from "react";
import { render, screen, fireEvent, cleanup, act, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { MonitorPanel } from "../MonitorPanel";
import type { A2ATrafficResponse, TopologySummary } from "@/lib/monitor";

const { mockApiGet } = vi.hoisted(() => ({
  mockApiGet: vi.fn<(path: string) => Promise<unknown>>(),
}));

vi.mock("@/lib/api", () => ({ api: { get: mockApiGet, post: vi.fn() } }));
vi.mock("@/hooks/useSocketEvent", () => ({ useSocketEvent: vi.fn() }));
vi.mock("@/components/concierge/RequestsInbox", () => ({
  RequestsInbox: ({ kind }: { kind: string }) => <div data-testid={`requests-inbox-${kind}`} />,
}));
vi.mock("../Monitor.module.css", () => ({ default: {} }));
vi.mock("@/components/concierge/icons", () => ({
  IcQueue: () => <svg />, IcOrgMap: () => <svg />, IcBell: () => <svg />, IcClock: () => <svg />,
}));

const emptyTraffic: A2ATrafficResponse = {
  window: "24h",
  bucket_seconds: 3600,
  buckets: Array.from({ length: 24 }, (_, i) => ({ ts: `2026-06-29T${String(i).padStart(2, "0")}:00:00Z`, count: 0 })),
  rps_now: 0,
  rps_peak: 0,
  rps_peak_at: null,
  total: 0,
};

const realTraffic: A2ATrafficResponse = {
  ...emptyTraffic,
  buckets: emptyTraffic.buckets.map((b, i) => ({ ...b, count: i === 23 ? 4 : i === 20 ? 9 : 0 })),
  rps_now: 4 / 3600,
  rps_peak: 9 / 3600,
  rps_peak_at: "2026-06-29T20:00:00Z",
  total: 13,
};

const summary: TopologySummary = { total: 4, agents: 2, teams: 2, platform: 1 };

const workspaces = [
  { id: "p", name: "Org", kind: "platform", parent_id: null, status: "online", role: "" },
  { id: "team", name: "Eng", kind: "workspace", parent_id: "p", status: "online", role: "lead" },
  { id: "a1", name: "Coder", kind: "workspace", parent_id: "team", status: "online", role: "" },
  { id: "a2", name: "Solo", kind: "workspace", parent_id: "p", status: "failed", role: "" },
];

function wire(traffic: A2ATrafficResponse | "reject") {
  mockApiGet.mockImplementation((path: string) => {
    if (path.startsWith("/monitor/a2a-traffic")) {
      return traffic === "reject" ? Promise.reject(new Error("401")) : Promise.resolve(traffic);
    }
    if (path === "/monitor/topology-summary") return Promise.resolve(summary);
    if (path === "/workspaces") return Promise.resolve(workspaces);
    return Promise.resolve(null);
  });
}

beforeEach(() => {
  mockApiGet.mockReset();
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.resetModules();
});

describe("MonitorPanel — real data", () => {
  it("renders the live traffic total, real counts, topology tree, and both inboxes", async () => {
    wire(realTraffic);
    await act(async () => { render(<MonitorPanel />); });

    // default window fetch
    expect(mockApiGet).toHaveBeenCalledWith("/monitor/a2a-traffic?window=24h");
    expect(mockApiGet).toHaveBeenCalledWith("/monitor/topology-summary");
    expect(mockApiGet).toHaveBeenCalledWith("/workspaces");

    await waitFor(() => expect(screen.getByTestId("traffic-total").textContent).toBe("13"));
    // No empty pill when there is real traffic.
    expect(screen.queryByTestId("traffic-empty")).toBeNull();

    // Real topology counts from the summary endpoint.
    expect(screen.getByTestId("count-agents").textContent).toBe("2");
    expect(screen.getByTestId("count-teams").textContent).toBe("2");

    // Real topology tree from /workspaces (4 nodes, none fabricated).
    await waitFor(() => expect(screen.getAllByTestId("topology-node")).toHaveLength(4));
    expect(screen.getByText("Org")).toBeTruthy();
    expect(screen.getByText("Coder")).toBeTruthy();

    // HITL reuses RequestsInbox for both kinds.
    expect(screen.getByTestId("requests-inbox-task")).toBeTruthy();
    expect(screen.getByTestId("requests-inbox-approval")).toBeTruthy();
  });
});

describe("MonitorPanel — honest empty state", () => {
  it("shows the no-traffic pill and a zero total for an empty series", async () => {
    wire(emptyTraffic);
    await act(async () => { render(<MonitorPanel />); });

    await waitFor(() => expect(screen.getByTestId("traffic-empty")).toBeTruthy());
    expect(screen.getByTestId("traffic-total").textContent).toBe("0");
  });

  it("shows the empty state (not a fake curve) when the API call fails", async () => {
    wire("reject");
    await act(async () => { render(<MonitorPanel />); });
    await waitFor(() => expect(screen.getByTestId("traffic-empty")).toBeTruthy());
    expect(screen.getByTestId("traffic-total").textContent).toBe("0");
  });
});

describe("MonitorPanel — window toggle", () => {
  it("refetches /monitor/a2a-traffic with the new window on toggle", async () => {
    wire(realTraffic);
    await act(async () => { render(<MonitorPanel />); });
    await waitFor(() => expect(screen.getByTestId("traffic-total").textContent).toBe("13"));

    await act(async () => { fireEvent.click(screen.getByTestId("window-7d")); });
    expect(mockApiGet).toHaveBeenCalledWith("/monitor/a2a-traffic?window=7d");
  });
});
