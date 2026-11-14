// @vitest-environment jsdom
/**
 * WorkspaceNode — Enter OS Light: the "Light" badge (next run in local time
 * for a parked agent), the "Parked" status instead of the hibernated pod's,
 * and GET /light read only for Light nodes, on mount and when light_state
 * changes.
 */
import React from "react";
import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("@xyflow/react", () => ({
  Handle: ({ "aria-label": ariaLabel }: { "aria-label"?: string }) => <div aria-label={ariaLabel}>handle</div>,
  Position: { Top: "top", Bottom: "bottom", Left: "left", Right: "right" },
}));
vi.mock("@/components/Tooltip", () => ({
  Tooltip: ({ text, children }: { text: string; children: React.ReactNode }) => (
    <div title={text} data-testid="tooltip">{children}</div>
  ),
}));
vi.mock("@/components/Toaster", () => ({ showToast: vi.fn() }));
vi.mock("@/components/canvas/useOrgDeployState", () => ({
  useOrgDeployState: () => ({ isActivelyProvisioning: false, isDeployingRoot: false, isLockedChild: false, descendantProvisioningCount: 0 }),
}));
vi.mock("@/store/canvas-topology", () => ({ getConfigurationStatus: () => "configured", getConfigurationError: () => null }));
vi.mock("@/store/canvas", () => {
  const store = {
    nodes: [], selectedNodeId: null, dragOverNodeId: null, selectedNodeIds: new Set<string>(),
    selectNode: vi.fn(), openContextMenu: vi.fn(), toggleNodeSelection: vi.fn(), nestNode: vi.fn(), restartWorkspace: vi.fn(),
  };
  const hook = (sel: (s: typeof store) => unknown) => sel(store);
  return { useCanvasStore: Object.assign(hook, { getState: () => store }) };
});

const get = vi.hoisted(() => vi.fn());
vi.mock("@/lib/api", () => ({ api: { get, post: vi.fn(), patch: vi.fn(), del: vi.fn(), put: vi.fn() } }));

import { WorkspaceNode } from "../WorkspaceNode";

type Data = Record<string, unknown>;
function data(over: Data = {}): Data {
  return {
    name: "Reporter", role: "", tier: 2, status: "online", parentId: null, activeTasks: 0, needsRestart: false,
    currentTask: "", lastSampleError: "", collapsed: false, agentCard: null, runtime: "hermes", lightState: null, ...over,
  };
}
function node(over: Data = {}) {
  const props = { id: "ws-1", data: data(over) } as unknown as Parameters<typeof WorkspaceNode>[0];
  return <WorkspaceNode {...props} />;
}

function lightInfo(nextRunAts: Array<string | null>) {
  return {
    light_state: "parked", light_state_at: "2026-11-13T09:00:04Z", refresh_pending: false, pending_actions: [], last_error: null,
    schedules: nextRunAts.map((t, i) => ({ name: `s${i}`, cron: "0 9 * * *", timezone: "Asia/Tokyo", prompt: "p", enabled: true,
      suspended: false, suspended_reason: null, suspended_at: null, consecutive_failures: 0, cronjob: null, next_run_at: t })),
  };
}

const lightGets = () => get.mock.calls.filter(([p]) => p === "/workspaces/ws-1/light").length;
async function flush() {
  await act(async () => { for (let i = 0; i < 5; i++) await Promise.resolve(); });
}

const prevTZ = process.env.TZ;
beforeEach(() => {
  process.env.TZ = "Asia/Tokyo";
  get.mockReset();
});
afterEach(() => {
  cleanup();
  vi.useRealTimers();
  process.env.TZ = prevTZ;
});

describe("WorkspaceNode — Light badge", () => {
  it("a parked agent shows its earliest next run in local time with the zone", async () => {
    // 00:00 UTC is 09:00 in Tokyo; the later run must not win.
    get.mockResolvedValue(lightInfo(["2026-11-15T00:00:00Z", null, "2026-11-14T00:00:00Z"]));
    render(node({ status: "hibernated", lightState: "parked" }));
    await flush();
    const badge = screen.getByTestId("light-badge");
    expect(badge.textContent).toMatch(/^Light · next Sat, Nov 14, 9:00 AM GMT\+9$/);
    expect(lightGets()).toBe(1);
  });

  it("a parked agent with no upcoming run reads 'Light · parked'", async () => {
    get.mockResolvedValue(lightInfo([null]));
    render(node({ status: "hibernated", lightState: "parked" }));
    await flush();
    expect(screen.getByTestId("light-badge").textContent).toBe("Light · parked");
  });

  it.each(["open", "parking", "opening"])("a %s agent shows the state's name and reads nothing", async (state) => {
    render(node({ lightState: state }));
    await flush();
    expect(screen.getByTestId("light-badge").textContent).toBe(`Light · ${state}`);
    expect(lightGets()).toBe(0);
  });

  it("an ordinary agent has no badge and reads nothing", async () => {
    render(node({ lightState: null }));
    await flush();
    expect(screen.queryByTestId("light-badge")).toBeNull();
    expect(get).not.toHaveBeenCalled();
  });

  it("reads GET /light on mount and when light_state changes, not on a re-render", async () => {
    get.mockResolvedValue(lightInfo(["2026-11-14T00:00:00Z"]));
    const view = render(node({ status: "hibernated", lightState: "parked" }));
    await flush();
    expect(lightGets()).toBe(1);
    view.rerender(node({ status: "hibernated", lightState: "parked", activeTasks: 0, name: "Reporter" }));
    await flush();
    expect(lightGets()).toBe(1);
    view.rerender(node({ status: "provisioning", lightState: "opening" }));
    await flush();
    view.rerender(node({ status: "online", lightState: "open" }));
    await flush();
    expect(lightGets()).toBe(1);
    view.rerender(node({ status: "hibernated", lightState: "parked" }));
    await flush();
    expect(lightGets()).toBe(2);
  });

  it("reads once more after the shown next run has passed, so the badge moves on", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-11-13T23:59:00Z"));
    get.mockResolvedValueOnce(lightInfo(["2026-11-14T00:00:00Z"])).mockResolvedValue(lightInfo(["2026-11-15T00:00:00Z"]));
    render(node({ status: "hibernated", lightState: "parked" }));
    await flush();
    expect(lightGets()).toBe(1);
    await act(async () => { await vi.advanceTimersByTimeAsync(60_000); });
    expect(lightGets()).toBe(1); // the run's minute has not passed by the grace yet
    await act(async () => { await vi.advanceTimersByTimeAsync(5_000); });
    await flush();
    expect(lightGets()).toBe(2);
    expect(screen.getByTestId("light-badge").textContent).toMatch(/Nov 15/);
  });
});

describe("WorkspaceNode — Parked status", () => {
  it("a parked agent's status reads Parked with the tooltip", async () => {
    get.mockResolvedValue(lightInfo([null]));
    render(node({ status: "hibernated", lightState: "parked" }));
    await flush();
    const label = screen.getByText("Parked");
    expect(label.closest("[data-testid=tooltip]")?.getAttribute("title")).toBe("Asleep between scheduled runs");
    expect(screen.getByTestId("workspace-node-Reporter").getAttribute("aria-label")).toBe(
      "Reporter workspace — parked, asleep between scheduled runs",
    );
  });

  it("a hibernated ordinary agent does not read Parked", () => {
    render(node({ status: "hibernated", lightState: null }));
    expect(screen.queryByText("Parked")).toBeNull();
  });
});
