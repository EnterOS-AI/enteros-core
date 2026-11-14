/**
 * Enter OS Light node state: WorkspaceNodeData.lightState hydrates from the
 * GET /workspaces `light_state` field and follows WORKSPACE_UPDATED events,
 * whose `name` and `light_state` keys apply independently.
 */
import { beforeEach, describe, expect, it, vi } from "vitest";

global.fetch = vi.fn(() =>
  Promise.resolve({ ok: true, json: () => Promise.resolve({}) } as Response),
);

const toast = vi.hoisted(() => vi.fn());
vi.mock("@/components/Toaster", () => ({ showToast: toast }));

import { useCanvasStore } from "../canvas";
import { __resetTombstonesForTest } from "../deleteTombstones";
import type { WorkspaceData, WSMessage } from "../socket";

function ws(overrides: Partial<WorkspaceData> & { id: string }): WorkspaceData {
  return {
    name: "WS",
    role: "agent",
    tier: 1,
    status: "online",
    agent_card: null,
    url: "",
    parent_id: null,
    active_tasks: 0,
    last_error_rate: 0,
    last_sample_error: "",
    uptime_seconds: 0,
    current_task: "",
    x: 0,
    y: 0,
    collapsed: false,
    runtime: "hermes",
    budget_limit: null,
    ...overrides,
  };
}

function updated(workspace_id: string, payload: Record<string, unknown>): WSMessage {
  return { event: "WORKSPACE_UPDATED", workspace_id, timestamp: new Date().toISOString(), payload };
}

const node = (id: string) => useCanvasStore.getState().nodes.find((n) => n.id === id)!;

beforeEach(() => {
  useCanvasStore.setState({ nodes: [], edges: [], selectedNodeId: null });
  __resetTombstonesForTest();
  toast.mockReset();
});

describe("lightState hydration", () => {
  it("maps light_state onto the node, null for an ordinary agent", () => {
    useCanvasStore.getState().hydrate([
      ws({ id: "light", name: "Light", status: "hibernated", light_state: "parked" }),
      ws({ id: "plain", name: "Plain", light_state: null }),
      ws({ id: "old", name: "Old" }), // an older server without the field
    ]);
    expect(node("light").data.lightState).toBe("parked");
    expect(node("plain").data.lightState).toBeNull();
    expect(node("old").data.lightState).toBeNull();
  });

  it("an unknown light_state value hydrates as null", () => {
    useCanvasStore.getState().hydrate([ws({ id: "x", light_state: "hovering" })]);
    expect(node("x").data.lightState).toBeNull();
  });
});

describe("WORKSPACE_UPDATED applies name and light_state independently", () => {
  beforeEach(() => {
    useCanvasStore.getState().hydrate([ws({ id: "a", name: "Agent", light_state: "parked", status: "hibernated" })]);
  });

  it("a payload with only light_state updates the state and keeps the name", () => {
    useCanvasStore.getState().applyEvent(updated("a", { light_state: "opening" }));
    expect(node("a").data.lightState).toBe("opening");
    expect(node("a").data.name).toBe("Agent");
  });

  it("light_state: null clears it", () => {
    useCanvasStore.getState().applyEvent(updated("a", { light_state: null }));
    expect(node("a").data.lightState).toBeNull();
  });

  it("a payload with only a name keeps the light state", () => {
    useCanvasStore.getState().applyEvent(updated("a", { name: "Renamed" }));
    expect(node("a").data.name).toBe("Renamed");
    expect(node("a").data.lightState).toBe("parked");
  });

  it("applies both keys of one payload", () => {
    useCanvasStore.getState().applyEvent(updated("a", { name: "Both", light_state: "open" }));
    expect(node("a").data).toMatchObject({ name: "Both", lightState: "open" });
  });

  it("a blank name does not stop the light_state from applying", () => {
    useCanvasStore.getState().applyEvent(updated("a", { name: " ", light_state: "open" }));
    expect(node("a").data).toMatchObject({ name: "Agent", lightState: "open" });
  });

  it("ignores a light_state value it does not know", () => {
    useCanvasStore.getState().applyEvent(updated("a", { light_state: "hovering" }));
    expect(node("a").data.lightState).toBe("parked");
  });

  it("a payload with neither key leaves the node object untouched", () => {
    const before = node("a");
    useCanvasStore.getState().applyEvent(updated("a", { unrelated: true }));
    expect(node("a")).toBe(before);
  });

  it("survives a full re-hydrate from /workspaces", () => {
    useCanvasStore.getState().applyEvent(updated("a", { light_state: "open" }));
    useCanvasStore.getState().hydrate([ws({ id: "a", name: "Agent", light_state: "open", status: "online" })]);
    expect(node("a").data.lightState).toBe("open");
    // The server row is authoritative on the next re-hydrate.
    useCanvasStore.getState().hydrate([ws({ id: "a", name: "Agent", light_state: "parking", status: "online" })]);
    expect(node("a").data.lightState).toBe("parking");
  });
});

describe("light_schedule_suspended", () => {
  it("shows a toast naming the schedule and the agent", () => {
    useCanvasStore.getState().hydrate([ws({ id: "a", name: "Reporter", light_state: "parked" })]);
    const before = node("a");
    useCanvasStore.getState().applyEvent(updated("a", { light_schedule_suspended: "daily-report" }));
    expect(toast).toHaveBeenCalledTimes(1);
    const [message] = toast.mock.calls[0];
    expect(message).toContain("daily-report");
    expect(message).toContain("Reporter");
    expect(node("a")).toBe(before);
  });

  it("ignores an empty schedule name", () => {
    useCanvasStore.getState().applyEvent(updated("a", { light_schedule_suspended: "" }));
    expect(toast).not.toHaveBeenCalled();
  });
});
