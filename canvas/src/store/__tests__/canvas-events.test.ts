import { describe, it, expect, beforeEach, vi } from "vitest";
import { handleCanvasEvent, resetProvisioningSequence, __pendingOnlineSizeForTest } from "../canvas-events";
import type { WSMessage } from "../socket";
import type { WorkspaceNodeData } from "../canvas";
import type { Node, Edge } from "@xyflow/react";

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function makeNode(
  id: string,
  overrides: Partial<WorkspaceNodeData> = {}
): Node<WorkspaceNodeData> {
  return {
    id,
    type: "workspaceNode",
    position: { x: 0, y: 0 },
    data: {
      name: `Node-${id}`,
      status: "online",
      tier: 1,
      agentCard: null,
      activeTasks: 0,
      collapsed: false,
      role: "agent",
      lastErrorRate: 0,
      lastSampleError: "",
      url: "http://localhost:9000",
      parentId: null,
      currentTask: "",
      needsRestart: false,
      runtime: "",
      budgetLimit: null,
      ...overrides,
    },
  };
}

function makeMsg(
  overrides: Partial<WSMessage> & { event: string; workspace_id: string }
): WSMessage {
  return {
    timestamp: new Date().toISOString(),
    payload: {},
    ...overrides,
  };
}

// Build a fresh get/set pair each test
function makeStore(
  nodes: Node<WorkspaceNodeData>[] = [],
  edges: Edge[] = [],
  selectedNodeId: string | null = null,
  agentMessages: Record<string, Array<{ id: string; content: string; timestamp: string }>> = {},
  liveAnnouncement = ""
) {
  const state = { nodes, edges, selectedNodeId, agentMessages, liveAnnouncement };
  const get = () => state;
  const set = vi.fn((partial: Record<string, unknown>) => {
    Object.assign(state, partial);
  });
  return { state, get, set };
}

// ---------------------------------------------------------------------------
// WORKSPACE_ONLINE
// ---------------------------------------------------------------------------

describe("handleCanvasEvent – WORKSPACE_ONLINE", () => {
  it("sets status to 'online' for a matching node", () => {
    const node = makeNode("ws-1", { status: "offline" });
    const { state, get, set } = makeStore([node]);

    handleCanvasEvent(makeMsg({ event: "WORKSPACE_ONLINE", workspace_id: "ws-1" }), get, set);

    expect(set).toHaveBeenCalledOnce();
    const updated = (set.mock.calls[0][0] as { nodes: Node<WorkspaceNodeData>[] }).nodes;
    expect(updated.find((n) => n.id === "ws-1")!.data.status).toBe("online");
  });

  it("is a no-op when workspace_id does not match any node", () => {
    const node = makeNode("ws-1");
    const { get, set } = makeStore([node]);

    handleCanvasEvent(makeMsg({ event: "WORKSPACE_ONLINE", workspace_id: "unknown" }), get, set);

    expect(set).not.toHaveBeenCalled();
  });

  it("does not mutate other nodes", () => {
    const nodes = [makeNode("ws-1", { status: "offline" }), makeNode("ws-2", { status: "offline" })];
    const { get, set } = makeStore(nodes);

    handleCanvasEvent(makeMsg({ event: "WORKSPACE_ONLINE", workspace_id: "ws-1" }), get, set);

    const updated = (set.mock.calls[0][0] as { nodes: Node<WorkspaceNodeData>[] }).nodes;
    expect(updated.find((n) => n.id === "ws-2")!.data.status).toBe("offline");
  });

  it("caps the unknown-workspace buffer so a dropped PROVISIONING cannot grow it forever", () => {
    resetProvisioningSequence();
    const { get, set } = makeStore([]);
    for (let i = 0; i < 1100; i++) {
      handleCanvasEvent(makeMsg({ event: "WORKSPACE_ONLINE", workspace_id: `ws-${i}` }), get, set);
    }
    expect(__pendingOnlineSizeForTest()).toBe(1000);
  });
});

// ---------------------------------------------------------------------------
// WORKSPACE_OFFLINE
// ---------------------------------------------------------------------------

describe("handleCanvasEvent – WORKSPACE_OFFLINE", () => {
  it("sets status to 'offline' for a matching node", () => {
    const node = makeNode("ws-1", { status: "online" });
    const { get, set } = makeStore([node]);

    handleCanvasEvent(makeMsg({ event: "WORKSPACE_OFFLINE", workspace_id: "ws-1" }), get, set);

    expect(set).toHaveBeenCalledOnce();
    const updated = (set.mock.calls[0][0] as { nodes: Node<WorkspaceNodeData>[] }).nodes;
    expect(updated.find((n) => n.id === "ws-1")!.data.status).toBe("offline");
  });

  it("still calls set even when workspace_id does not match (maps over all nodes)", () => {
    const node = makeNode("ws-1");
    const { get, set } = makeStore([node]);

    handleCanvasEvent(makeMsg({ event: "WORKSPACE_OFFLINE", workspace_id: "nope" }), get, set);

    // set is called because it maps over all nodes (no early-exit guard)
    expect(set).toHaveBeenCalledOnce();
    const updated = (set.mock.calls[0][0] as { nodes: Node<WorkspaceNodeData>[] }).nodes;
    expect(updated[0].data.status).toBe("online"); // unchanged
  });
});

// ---------------------------------------------------------------------------
// WORKSPACE_DEGRADED
// ---------------------------------------------------------------------------

describe("handleCanvasEvent – WORKSPACE_DEGRADED", () => {
  it("sets status, lastErrorRate, and lastSampleError", () => {
    const node = makeNode("ws-1", { status: "online" });
    const { get, set } = makeStore([node]);

    handleCanvasEvent(
      makeMsg({
        event: "WORKSPACE_DEGRADED",
        workspace_id: "ws-1",
        payload: { error_rate: 0.42, sample_error: "timeout connecting to DB" },
      }),
      get,
      set
    );

    const updated = (set.mock.calls[0][0] as { nodes: Node<WorkspaceNodeData>[] }).nodes;
    const data = updated.find((n) => n.id === "ws-1")!.data;
    expect(data.status).toBe("degraded");
    expect(data.lastErrorRate).toBe(0.42);
    expect(data.lastSampleError).toBe("timeout connecting to DB");
  });

  it("defaults error_rate to 0 and sample_error to '' when missing from payload", () => {
    const node = makeNode("ws-1");
    const { get, set } = makeStore([node]);

    handleCanvasEvent(
      makeMsg({ event: "WORKSPACE_DEGRADED", workspace_id: "ws-1" }),
      get,
      set
    );

    const updated = (set.mock.calls[0][0] as { nodes: Node<WorkspaceNodeData>[] }).nodes;
    const data = updated.find((n) => n.id === "ws-1")!.data;
    expect(data.lastErrorRate).toBe(0);
    expect(data.lastSampleError).toBe("");
  });
});

// ---------------------------------------------------------------------------
// WORKSPACE_PROVISIONING
// ---------------------------------------------------------------------------

describe("handleCanvasEvent – WORKSPACE_PROVISIONING", () => {
  // Reset the monotonic sequence counter before each test so positions are
  // deterministic regardless of test execution order.
  beforeEach(() => {
    resetProvisioningSequence();
  });

  it("creates a new node when workspace_id is unknown", () => {
    const { get, set } = makeStore([]);

    handleCanvasEvent(
      makeMsg({
        event: "WORKSPACE_PROVISIONING",
        workspace_id: "ws-new",
        payload: { name: "Brand New", tier: 3 },
      }),
      get,
      set
    );

    const newNodes = (set.mock.calls[0][0] as { nodes: Node<WorkspaceNodeData>[] }).nodes;
    expect(newNodes).toHaveLength(1);
    const n = newNodes[0];
    expect(n.id).toBe("ws-new");
    expect(n.type).toBe("workspaceNode");
    expect(n.position).toEqual({ x: 100, y: 100 });
    expect(n.data.name).toBe("Brand New");
    expect(n.data.tier).toBe(3);
    expect(n.data.status).toBe("provisioning");
  });

  it("uses defaults for name and tier when payload is sparse", () => {
    const { get, set } = makeStore([]);

    handleCanvasEvent(
      makeMsg({ event: "WORKSPACE_PROVISIONING", workspace_id: "ws-x", payload: {} }),
      get,
      set
    );

    const newNodes = (set.mock.calls[0][0] as { nodes: Node<WorkspaceNodeData>[] }).nodes;
    expect(newNodes[0].data.name).toBe("New Workspace");
    expect(newNodes[0].data.tier).toBe(1);
  });

  it("updates an existing node to provisioning (restart path)", () => {
    const node = makeNode("ws-1", { status: "online", currentTask: "old task", needsRestart: true });
    const { get, set } = makeStore([node]);

    handleCanvasEvent(
      makeMsg({
        event: "WORKSPACE_PROVISIONING",
        workspace_id: "ws-1",
        payload: { name: "PM" },
      }),
      get,
      set
    );

    const updated = (set.mock.calls[0][0] as { nodes: Node<WorkspaceNodeData>[] }).nodes;
    // Must not create a duplicate node
    expect(updated).toHaveLength(1);
    const data = updated[0].data;
    expect(data.status).toBe("provisioning");
    expect(data.needsRestart).toBe(false);
    expect(data.currentTask).toBe("");
  });

  it("assigns unique grid positions across 4 columns then wraps to second row", () => {
    // Grid: COL_SPACING=320, ROW_SPACING=160, ORIGIN=(100,100), COLS=4
    const { get, set } = makeStore([]);
    const ids = ["ws-a", "ws-b", "ws-c", "ws-d", "ws-e"];

    for (const id of ids) {
      handleCanvasEvent(
        makeMsg({ event: "WORKSPACE_PROVISIONING", workspace_id: id, payload: {} }),
        get,
        set
      );
    }

    const finalNodes = (set.mock.calls[4][0] as { nodes: Node<WorkspaceNodeData>[] }).nodes;
    const pos = (id: string) => finalNodes.find((n) => n.id === id)!.position;
    expect(pos("ws-a")).toEqual({ x: 100,  y: 100 }); // idx 0
    expect(pos("ws-b")).toEqual({ x: 420,  y: 100 }); // idx 1
    expect(pos("ws-c")).toEqual({ x: 740,  y: 100 }); // idx 2
    expect(pos("ws-d")).toEqual({ x: 1060, y: 100 }); // idx 3
    expect(pos("ws-e")).toEqual({ x: 100,  y: 260 }); // idx 4 — second row
  });

  it("does NOT reuse a grid slot after a node is removed (collision regression)", () => {
    // This is the core bug: nodes.length drops on delete, causing the next
    // provisioned node to share a position with an existing one.
    //
    //   Before fix: Provision A(0), B(1), C(2) → Remove A → Provision D → idx=2 → COLLISION with C
    //   After fix:  D gets idx=3 → unique slot (1060, 100)
    const { get, set } = makeStore([]);

    // Provision A, B, C
    for (const id of ["ws-a", "ws-b", "ws-c"]) {
      handleCanvasEvent(
        makeMsg({ event: "WORKSPACE_PROVISIONING", workspace_id: id, payload: {} }),
        get,
        set
      );
    }

    // Remove A — with the old bug this drops nodes.length to 2
    handleCanvasEvent(makeMsg({ event: "WORKSPACE_REMOVED", workspace_id: "ws-a" }), get, set);

    // Provision D — must land at idx=3, NOT idx=2 (which would collide with C)
    handleCanvasEvent(
      makeMsg({ event: "WORKSPACE_PROVISIONING", workspace_id: "ws-d", payload: {} }),
      get,
      set
    );

    const lastNodes = (set.mock.calls[set.mock.calls.length - 1][0] as { nodes: Node<WorkspaceNodeData>[] }).nodes;
    const dPos = lastNodes.find((n) => n.id === "ws-d")!.position;
    const cPos = lastNodes.find((n) => n.id === "ws-c")!.position;

    // D must not share C's position
    expect(dPos).not.toEqual(cPos);
    // D should land at idx=3: (100 + 3*320, 100) = (1060, 100)
    expect(dPos).toEqual({ x: 1060, y: 100 });
  });

  it("does not increment the sequence counter on the restart path", () => {
    // Restart (existing node re-provisioned) must not burn a sequence slot.
    // After: provision A (slot 0), restart A (no slot consumed), provision B → slot 1.
    const { get, set } = makeStore([]);

    // Provision A → idx 0
    handleCanvasEvent(
      makeMsg({ event: "WORKSPACE_PROVISIONING", workspace_id: "ws-a", payload: {} }),
      get,
      set
    );

    // Restart A — ws-a already exists, so restart path runs; counter must stay at 1
    handleCanvasEvent(
      makeMsg({ event: "WORKSPACE_PROVISIONING", workspace_id: "ws-a", payload: {} }),
      get,
      set
    );

    // Provision B → must get idx 1, not idx 2
    handleCanvasEvent(
      makeMsg({ event: "WORKSPACE_PROVISIONING", workspace_id: "ws-b", payload: {} }),
      get,
      set
    );

    const lastNodes = (set.mock.calls[set.mock.calls.length - 1][0] as { nodes: Node<WorkspaceNodeData>[] }).nodes;
    const bPos = lastNodes.find((n) => n.id === "ws-b")!.position;
    expect(bPos).toEqual({ x: 420, y: 100 }); // idx 1 = (100 + 320, 100)
  });

  it("uses finalX/finalY from payload when parentId is set and parent exists in store", () => {
    // Org-import child lands with explicit coords — these are server-computed
    // parent-relative positions. The handler must trust them verbatim.
    const parent = makeNode("parent-root", { name: "Root" });
    const { get, set } = makeStore([parent]);

    handleCanvasEvent(
      makeMsg({
        event: "WORKSPACE_PROVISIONING",
        workspace_id: "child-org",
        payload: {
          name: "Org Child",
          tier: 2,
          parent_id: "parent-root",
          x: 500,
          y: 300,
        },
      }),
      get,
      set
    );

    const newNodes = (set.mock.calls[0][0] as { nodes: Node<WorkspaceNodeData>[] }).nodes;
    expect(newNodes).toHaveLength(2);
    const child = newNodes.find((n) => n.id === "child-org")!;

    // Must use the server-provided coords, not grid
    expect(child.position).toEqual({ x: 500, y: 300 });
    // Must bind parentId so RF renders it nested inside the parent card
    expect(child.parentId).toBe("parent-root");
    expect(child.data.parentId).toBe("parent-root");
    expect(child.data.name).toBe("Org Child");
    expect(child.data.status).toBe("provisioning");
  });

  it("uses grid position when parentId is set but parent is NOT in store yet", () => {
    // Rare WS-reorder: child event arrives before parent's PROVISIONING event.
    // Must not crash — uses grid slot as fallback. Parent will reparent
    // the child when it lands.
    const { get, set } = makeStore([]);

    handleCanvasEvent(
      makeMsg({
        event: "WORKSPACE_PROVISIONING",
        workspace_id: "orphan-child",
        payload: {
          name: "Orphan",
          parent_id: "unknown-parent",
          x: 999,
          y: 888,
        },
      }),
      get,
      set
    );

    const newNodes = (set.mock.calls[0][0] as { nodes: Node<WorkspaceNodeData>[] }).nodes;
    const child = newNodes.find((n) => n.id === "orphan-child")!;

    // Must NOT use finalX/finalY — parent isn't in store so grid slot is used
    expect(child.position).not.toEqual({ x: 999, y: 888 });
    // Grid slot for idx 0: (100, 100)
    expect(child.position).toEqual({ x: 100, y: 100 });
    // parentId is NOT set on the node when parent is unknown:
    // the node will be reparented when the parent eventually lands
    expect(child.data.parentId).not.toBe("unknown-parent");
  });

  it("no-op cascade: parent in store but no finalX/Y → grid position, no parentId", () => {
    // Parent exists but payload has no x/y → must not crash, uses grid slot.
    // parentId is NOT set because we don't have parent-relative coords.
    const parent = makeNode("parent-exists");
    const { get, set } = makeStore([parent]);

    handleCanvasEvent(
      makeMsg({
        event: "WORKSPACE_PROVISIONING",
        workspace_id: "child-no-coords",
        payload: {
          name: "No Coords",
          parent_id: "parent-exists",
          // no x or y
        },
      }),
      get,
      set
    );

    const newNodes = (set.mock.calls[0][0] as { nodes: Node<WorkspaceNodeData>[] }).nodes;
    const child = newNodes.find((n) => n.id === "child-no-coords")!;

    // Grid slot for idx 0: (100, 100)
    expect(child.position).toEqual({ x: 100, y: 100 });
    // parentId stays null (not undefined) when no finalX/Y — server has no
    // position for this node, and the handler initialises parentId=null
    expect(child.parentId).toBeUndefined();
    expect(child.data.parentId).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// WORKSPACE_REMOVED
// ---------------------------------------------------------------------------

describe("handleCanvasEvent – WORKSPACE_REMOVED", () => {
  it("removes the node from the list", () => {
    const nodes = [makeNode("ws-1"), makeNode("ws-2")];
    const { get, set } = makeStore(nodes);

    handleCanvasEvent(makeMsg({ event: "WORKSPACE_REMOVED", workspace_id: "ws-1" }), get, set);

    const { nodes: updatedNodes } = set.mock.calls[0][0] as { nodes: Node<WorkspaceNodeData>[]; edges: Edge[] };
    expect(updatedNodes.find((n) => n.id === "ws-1")).toBeUndefined();
    expect(updatedNodes.find((n) => n.id === "ws-2")).toBeDefined();
  });

  it("reparents children to the removed node's parent", () => {
    const parent = makeNode("parent");
    const mid = makeNode("mid", { parentId: "parent" });
    const child = makeNode("child", { parentId: "mid" });
    const { get, set } = makeStore([parent, mid, child]);

    // Remove mid — child should be reparented to parent
    handleCanvasEvent(makeMsg({ event: "WORKSPACE_REMOVED", workspace_id: "mid" }), get, set);

    const { nodes: updatedNodes } = set.mock.calls[0][0] as { nodes: Node<WorkspaceNodeData>[] };
    const updatedChild = updatedNodes.find((n) => n.id === "child")!;
    expect(updatedChild.data.parentId).toBe("parent");
    expect(updatedChild.parentId).toBe("parent"); // RF binding re-pointed
  });

  it("reparents children to null when root node is removed", () => {
    const root = makeNode("root");
    const child = makeNode("child", { parentId: "root" });
    const { get, set } = makeStore([root, child]);

    handleCanvasEvent(makeMsg({ event: "WORKSPACE_REMOVED", workspace_id: "root" }), get, set);

    const { nodes: updatedNodes } = set.mock.calls[0][0] as { nodes: Node<WorkspaceNodeData>[] };
    const updatedChild = updatedNodes.find((n) => n.id === "child")!;
    expect(updatedChild.data.parentId).toBeNull();
    expect(updatedChild.parentId).toBeUndefined();
  });

  it("removes edges connected to the removed workspace", () => {
    const nodes = [makeNode("ws-1"), makeNode("ws-2")];
    const edges: Edge[] = [
      { id: "e1", source: "ws-1", target: "ws-2" },
      { id: "e2", source: "ws-3", target: "ws-1" },
      { id: "e3", source: "ws-2", target: "ws-3" },
    ];
    const { get, set } = makeStore(nodes, edges);

    handleCanvasEvent(makeMsg({ event: "WORKSPACE_REMOVED", workspace_id: "ws-1" }), get, set);

    const { edges: updatedEdges } = set.mock.calls[0][0] as { edges: Edge[] };
    expect(updatedEdges).toHaveLength(1);
    expect(updatedEdges[0].id).toBe("e3");
  });

  it("clears selectedNodeId when the selected node is removed", () => {
    const node = makeNode("ws-1");
    const { get, set } = makeStore([node], [], "ws-1");

    handleCanvasEvent(makeMsg({ event: "WORKSPACE_REMOVED", workspace_id: "ws-1" }), get, set);

    const { selectedNodeId } = set.mock.calls[0][0] as { selectedNodeId: string | null };
    expect(selectedNodeId).toBeNull();
  });

  it("preserves selectedNodeId when a different node is removed", () => {
    const nodes = [makeNode("ws-1"), makeNode("ws-2")];
    const { get, set } = makeStore(nodes, [], "ws-1");

    handleCanvasEvent(makeMsg({ event: "WORKSPACE_REMOVED", workspace_id: "ws-2" }), get, set);

    const { selectedNodeId } = set.mock.calls[0][0] as { selectedNodeId: string | null };
    expect(selectedNodeId).toBe("ws-1");
  });
});

// ---------------------------------------------------------------------------
// AGENT_CARD_UPDATED
// ---------------------------------------------------------------------------

describe("handleCanvasEvent – AGENT_CARD_UPDATED", () => {
  it("sets agentCard from the payload", () => {
    const node = makeNode("ws-1");
    const { get, set } = makeStore([node]);
    const card = { name: "My Agent", skills: [{ id: "code", name: "Coding" }] };

    handleCanvasEvent(
      makeMsg({
        event: "AGENT_CARD_UPDATED",
        workspace_id: "ws-1",
        payload: { agent_card: card },
      }),
      get,
      set
    );

    const updated = (set.mock.calls[0][0] as { nodes: Node<WorkspaceNodeData>[] }).nodes;
    expect(updated.find((n) => n.id === "ws-1")!.data.agentCard).toEqual(card);
  });

  it("sets agentCard to null when payload value is a non-object string", () => {
    const node = makeNode("ws-1");
    const { get, set } = makeStore([node]);

    handleCanvasEvent(
      makeMsg({
        event: "AGENT_CARD_UPDATED",
        workspace_id: "ws-1",
        payload: { agent_card: "bad-value" },
      }),
      get,
      set
    );

    const updated = (set.mock.calls[0][0] as { nodes: Node<WorkspaceNodeData>[] }).nodes;
    expect(updated.find((n) => n.id === "ws-1")!.data.agentCard).toBeNull();
  });

  it("sets agentCard to null when payload value is null", () => {
    const node = makeNode("ws-1", { agentCard: { name: "old" } });
    const { get, set } = makeStore([node]);

    handleCanvasEvent(
      makeMsg({
        event: "AGENT_CARD_UPDATED",
        workspace_id: "ws-1",
        payload: { agent_card: null },
      }),
      get,
      set
    );

    const updated = (set.mock.calls[0][0] as { nodes: Node<WorkspaceNodeData>[] }).nodes;
    expect(updated.find((n) => n.id === "ws-1")!.data.agentCard).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// TASK_UPDATED
// ---------------------------------------------------------------------------

describe("handleCanvasEvent – TASK_UPDATED", () => {
  it("sets currentTask and activeTasks", () => {
    const node = makeNode("ws-1");
    const { get, set } = makeStore([node]);

    handleCanvasEvent(
      makeMsg({
        event: "TASK_UPDATED",
        workspace_id: "ws-1",
        payload: { current_task: "Analysing code", active_tasks: 3 },
      }),
      get,
      set
    );

    const updated = (set.mock.calls[0][0] as { nodes: Node<WorkspaceNodeData>[] }).nodes;
    const data = updated.find((n) => n.id === "ws-1")!.data;
    expect(data.currentTask).toBe("Analysing code");
    expect(data.activeTasks).toBe(3);
  });

  it("defaults to empty string and 0 when payload fields are missing", () => {
    const node = makeNode("ws-1", { currentTask: "old task", activeTasks: 5 });
    const { get, set } = makeStore([node]);

    handleCanvasEvent(
      makeMsg({ event: "TASK_UPDATED", workspace_id: "ws-1", payload: {} }),
      get,
      set
    );

    const updated = (set.mock.calls[0][0] as { nodes: Node<WorkspaceNodeData>[] }).nodes;
    const data = updated.find((n) => n.id === "ws-1")!.data;
    expect(data.currentTask).toBe("");
    expect(data.activeTasks).toBe(0);
  });
});

// ---------------------------------------------------------------------------
// AGENT_MESSAGE
// ---------------------------------------------------------------------------

describe("handleCanvasEvent – AGENT_MESSAGE", () => {
  it("appends a message to agentMessages for the workspace", () => {
    const node = makeNode("ws-1");
    const { get, set } = makeStore([node], [], null, {});

    handleCanvasEvent(
      makeMsg({
        event: "AGENT_MESSAGE",
        workspace_id: "ws-1",
        payload: { message: "Hello from agent!" },
      }),
      get,
      set
    );

    expect(set).toHaveBeenCalledOnce();
    const { agentMessages } = set.mock.calls[0][0] as {
      agentMessages: Record<string, Array<{ id: string; content: string; timestamp: string }>>;
    };
    expect(agentMessages["ws-1"]).toHaveLength(1);
    expect(agentMessages["ws-1"][0].content).toBe("Hello from agent!");
    expect(typeof agentMessages["ws-1"][0].id).toBe("string");
    expect(typeof agentMessages["ws-1"][0].timestamp).toBe("string");
  });

  it("appends to existing messages rather than replacing them", () => {
    const node = makeNode("ws-1");
    const existing = [{ id: "old-id", content: "prior msg", timestamp: "2024-01-01T00:00:00Z" }];
    const { get, set } = makeStore([node], [], null, { "ws-1": existing });

    handleCanvasEvent(
      makeMsg({
        event: "AGENT_MESSAGE",
        workspace_id: "ws-1",
        payload: { message: "second message" },
      }),
      get,
      set
    );

    const { agentMessages } = set.mock.calls[0][0] as {
      agentMessages: Record<string, Array<{ id: string; content: string; timestamp: string }>>;
    };
    expect(agentMessages["ws-1"]).toHaveLength(2);
    expect(agentMessages["ws-1"][0].content).toBe("prior msg");
    expect(agentMessages["ws-1"][1].content).toBe("second message");
  });

  it("is a no-op when message content is empty", () => {
    const node = makeNode("ws-1");
    const { get, set } = makeStore([node]);

    handleCanvasEvent(
      makeMsg({
        event: "AGENT_MESSAGE",
        workspace_id: "ws-1",
        payload: { message: "" },
      }),
      get,
      set
    );

    expect(set).not.toHaveBeenCalled();
  });

  it("is a no-op when message is absent from payload", () => {
    const node = makeNode("ws-1");
    const { get, set } = makeStore([node]);

    handleCanvasEvent(
      makeMsg({ event: "AGENT_MESSAGE", workspace_id: "ws-1", payload: {} }),
      get,
      set
    );

    expect(set).not.toHaveBeenCalled();
  });

  // Attachment passthrough — the broadcast payload's `attachments` array
  // is the wire format the platform's Notify handler emits (activity.go:
  // 318-326). These tests pin the canvas-side filtering / shape coercion
  // so the chat reliably renders download chips for agent-sent files.

  it("passes through valid attachments onto the new message", () => {
    const node = makeNode("ws-1");
    const { get, set } = makeStore([node], [], null, {});
    const att = {
      uri: "workspace:/tmp/build.zip",
      name: "build.zip",
      mimeType: "application/zip",
      size: 12345,
    };

    handleCanvasEvent(
      makeMsg({
        event: "AGENT_MESSAGE",
        workspace_id: "ws-1",
        payload: { message: "see attached", attachments: [att] },
      }),
      get,
      set,
    );

    const { agentMessages } = set.mock.calls[0][0] as {
      agentMessages: Record<string, Array<{ content: string; attachments?: Array<{ uri: string; name: string; mimeType?: string; size?: number }> }>>;
    };
    const msg = agentMessages["ws-1"][0];
    expect(msg.attachments).toEqual([att]);
  });

  it("appends an attachments-only message (empty content) when at least one attachment present", () => {
    // Regression: previously the AGENT_MESSAGE handler short-circuited on
    // empty `message`, dropping a notify whose intent was "here's the
    // file" with no caption. The fix renders the bubble whenever EITHER
    // text or attachments are present.
    const node = makeNode("ws-1");
    const { get, set } = makeStore([node], [], null, {});

    handleCanvasEvent(
      makeMsg({
        event: "AGENT_MESSAGE",
        workspace_id: "ws-1",
        payload: {
          message: "",
          attachments: [{ uri: "workspace:/x.txt", name: "x.txt" }],
        },
      }),
      get,
      set,
    );

    expect(set).toHaveBeenCalledOnce();
    const { agentMessages } = set.mock.calls[0][0] as {
      agentMessages: Record<string, Array<{ content: string; attachments?: unknown[] }>>;
    };
    expect(agentMessages["ws-1"]).toHaveLength(1);
    expect(agentMessages["ws-1"][0].content).toBe("");
    expect(agentMessages["ws-1"][0].attachments).toHaveLength(1);
  });

  it("filters out attachments with empty uri or name (defence-in-depth for missing gin `dive`)", () => {
    // Server-side per-element validation rejects empty uri/name, but the
    // canvas defence-in-depth filter exists because the broadcast path
    // skips that handler — a malformed broadcast (or a future regression)
    // could still emit empty entries. Drop them rather than rendering
    // blank/broken chips.
    const node = makeNode("ws-1");
    const { get, set } = makeStore([node], [], null, {});

    handleCanvasEvent(
      makeMsg({
        event: "AGENT_MESSAGE",
        workspace_id: "ws-1",
        payload: {
          message: "ok",
          attachments: [
            { uri: "workspace:/good.txt", name: "good.txt" },
            { uri: "", name: "missing-uri" },
            { uri: "workspace:/missing-name", name: "" },
            { uri: "workspace:/wrong-types", name: 42 },  // non-string name
          ],
        },
      }),
      get,
      set,
    );

    const { agentMessages } = set.mock.calls[0][0] as {
      agentMessages: Record<string, Array<{ attachments?: Array<{ name: string }> }>>;
    };
    const atts = agentMessages["ws-1"][0].attachments!;
    expect(atts).toHaveLength(1);
    expect(atts[0].name).toBe("good.txt");
  });

  it("ignores non-array attachments payloads", () => {
    const node = makeNode("ws-1");
    const { get, set } = makeStore([node], [], null, {});

    handleCanvasEvent(
      makeMsg({
        event: "AGENT_MESSAGE",
        workspace_id: "ws-1",
        payload: { message: "hi", attachments: "not-an-array" },
      }),
      get,
      set,
    );

    const { agentMessages } = set.mock.calls[0][0] as {
      agentMessages: Record<string, Array<{ content: string; attachments?: unknown[] }>>;
    };
    expect(agentMessages["ws-1"][0].content).toBe("hi");
    // No attachments key when input was malformed (rather than [] which
    // would render an empty "0 files" header in some chat UIs).
    expect("attachments" in agentMessages["ws-1"][0]).toBe(false);
  });

  it("is a no-op when both message and attachments are empty", () => {
    const node = makeNode("ws-1");
    const { get, set } = makeStore([node]);

    handleCanvasEvent(
      makeMsg({
        event: "AGENT_MESSAGE",
        workspace_id: "ws-1",
        payload: { message: "", attachments: [] },
      }),
      get,
      set,
    );

    expect(set).not.toHaveBeenCalled();
  });
});

// ---------------------------------------------------------------------------
// A2A_RESPONSE
// ---------------------------------------------------------------------------

describe("handleCanvasEvent – A2A_RESPONSE", () => {
  it("extracts text from response_body and stores as agentMessage", () => {
    const node = makeNode("ws-1");
    const { get, set } = makeStore([node], [], null, {});

    handleCanvasEvent(
      makeMsg({
        event: "A2A_RESPONSE",
        workspace_id: "ws-1",
        payload: {
          response_body: {
            result: { parts: [{ kind: "text", text: "Here is my analysis" }] },
          },
          method: "message/send",
          duration_ms: 1500,
        },
      }),
      get,
      set
    );

    expect(set).toHaveBeenCalledOnce();
    const { agentMessages } = set.mock.calls[0][0] as {
      agentMessages: Record<string, Array<{ id: string; content: string; timestamp: string }>>;
    };
    expect(agentMessages["ws-1"]).toHaveLength(1);
    expect(agentMessages["ws-1"][0].content).toBe("Here is my analysis");
  });

  it("is a no-op when response_body is missing", () => {
    const node = makeNode("ws-1");
    const { get, set } = makeStore([node]);

    handleCanvasEvent(
      makeMsg({
        event: "A2A_RESPONSE",
        workspace_id: "ws-1",
        payload: { method: "message/send" },
      }),
      get,
      set
    );

    expect(set).not.toHaveBeenCalled();
  });

  it("is a no-op when response text is empty", () => {
    const node = makeNode("ws-1");
    const { get, set } = makeStore([node]);

    handleCanvasEvent(
      makeMsg({
        event: "A2A_RESPONSE",
        workspace_id: "ws-1",
        payload: {
          response_body: { result: { parts: [] } },
        },
      }),
      get,
      set
    );

    expect(set).not.toHaveBeenCalled();
  });
});

// ---------------------------------------------------------------------------
// Unknown event
// ---------------------------------------------------------------------------

describe("handleCanvasEvent – unknown event", () => {
  it("does not crash and does not call set", () => {
    const node = makeNode("ws-1");
    const { get, set } = makeStore([node]);

    expect(() =>
      handleCanvasEvent(
        makeMsg({ event: "TOTALLY_UNKNOWN_EVENT", workspace_id: "ws-1" }),
        get,
        set
      )
    ).not.toThrow();

    expect(set).not.toHaveBeenCalled();
  });

  it("handles an empty event string without crashing", () => {
    const { get, set } = makeStore([]);

    expect(() =>
      handleCanvasEvent(makeMsg({ event: "", workspace_id: "ws-1" }), get, set)
    ).not.toThrow();
  });
});

// ---------------------------------------------------------------------------
// Screen-reader live announcements
// ---------------------------------------------------------------------------

describe("handleCanvasEvent – liveAnnouncement", () => {
  it("announces WORKSPACE_ONLINE with node name", () => {
    const node = makeNode("ws-1", { name: "Alpha" });
    const { get, set, state } = makeStore([node]);

    handleCanvasEvent(
      makeMsg({ event: "WORKSPACE_ONLINE", workspace_id: "ws-1" }),
      get,
      set
    );

    expect(state.liveAnnouncement).toBe("Alpha is now online");
  });

  it("announces WORKSPACE_OFFLINE with node name", () => {
    const node = makeNode("ws-1", { name: "Beta" });
    const { get, set, state } = makeStore([node]);

    handleCanvasEvent(
      makeMsg({ event: "WORKSPACE_OFFLINE", workspace_id: "ws-1" }),
      get,
      set
    );

    expect(state.liveAnnouncement).toBe("Beta is now offline");
  });

  it("announces WORKSPACE_PAUSED with node name", () => {
    const node = makeNode("ws-1", { name: "Gamma" });
    const { get, set, state } = makeStore([node]);

    handleCanvasEvent(
      makeMsg({ event: "WORKSPACE_PAUSED", workspace_id: "ws-1" }),
      get,
      set
    );

    expect(state.liveAnnouncement).toBe("Gamma has been paused");
  });

  it("announces WORKSPACE_DEGRADED with node name", () => {
    const node = makeNode("ws-1", { name: "Delta" });
    const { get, set, state } = makeStore([node]);

    handleCanvasEvent(
      makeMsg({
        event: "WORKSPACE_DEGRADED",
        workspace_id: "ws-1",
        payload: { sample_error: "connection timeout" },
      }),
      get,
      set
    );

    expect(state.liveAnnouncement).toBe("Delta is degraded");
  });

  it("announces WORKSPACE_PROVISIONING for new workspace with payload name", () => {
    const { get, set, state } = makeStore([]);

    handleCanvasEvent(
      makeMsg({
        event: "WORKSPACE_PROVISIONING",
        workspace_id: "ws-new",
        payload: { name: "NewBot" },
      }),
      get,
      set
    );

    expect(state.liveAnnouncement).toBe("NewBot is provisioning");
  });

  it("announces WORKSPACE_PROVISIONING for new workspace with default name", () => {
    const { get, set, state } = makeStore([]);

    handleCanvasEvent(
      makeMsg({
        event: "WORKSPACE_PROVISIONING",
        workspace_id: "ws-new",
        payload: {},
      }),
      get,
      set
    );

    expect(state.liveAnnouncement).toBe("New Workspace is provisioning");
  });

  it("announces WORKSPACE_REMOVED with node name", () => {
    const node = makeNode("ws-1", { name: "Gamma" });
    const { get, set, state } = makeStore([node]);

    handleCanvasEvent(
      makeMsg({ event: "WORKSPACE_REMOVED", workspace_id: "ws-1" }),
      get,
      set
    );

    expect(state.liveAnnouncement).toBe("Gamma was removed");
  });

  it("announces WORKSPACE_PROVISION_FAILED with node name", () => {
    const node = makeNode("ws-1", { name: "Delta" });
    const { get, set, state } = makeStore([node]);

    handleCanvasEvent(
      makeMsg({
        event: "WORKSPACE_PROVISION_FAILED",
        workspace_id: "ws-1",
        payload: { error: "docker pull failed" },
      }),
      get,
      set
    );

    expect(state.liveAnnouncement).toBe("Delta provisioning failed");
  });

  it("does not announce for TASK_UPDATED", () => {
    const node = makeNode("ws-1", { name: "Alpha" });
    const { get, set, state } = makeStore([node]);

    handleCanvasEvent(
      makeMsg({
        event: "TASK_UPDATED",
        workspace_id: "ws-1",
        payload: { current_task: "building release", active_tasks: 1 },
      }),
      get,
      set
    );

    // TASK_UPDATED is noisy (every heartbeat); it should not announce
    expect(state.liveAnnouncement ?? "").toBe("");
  });

  it("does not announce for AGENT_MESSAGE", () => {
    const node = makeNode("ws-1", { name: "Alpha" });
    const { get, set, state } = makeStore([node]);

    handleCanvasEvent(
      makeMsg({
        event: "AGENT_MESSAGE",
        workspace_id: "ws-1",
        payload: { message: "hello from the agent" },
      }),
      get,
      set
    );

    expect(state.liveAnnouncement ?? "").toBe("");
  });

  it("uses payload name for ONLINE when node not found in store", () => {
    const { get, set, state } = makeStore([]);

    handleCanvasEvent(
      makeMsg({
        event: "WORKSPACE_ONLINE",
        workspace_id: "ws-1",
        payload: { name: "FromPayload" },
      }),
      get,
      set
    );

    // ONLINE when node doesn't exist just buffers _pendingOnline;
    // no announcement should be set
    expect(state.liveAnnouncement ?? "").toBe("");
  });
});

// ---------------------------------------------------------------------------
// BOOT_STEP — "Enter OS" boot sequence
// ---------------------------------------------------------------------------

describe("handleCanvasEvent – BOOT_STEP", () => {
  function bootMsg(
    workspaceId: string,
    payload: Record<string, unknown>
  ): WSMessage {
    return makeMsg({ event: "BOOT_STEP", workspace_id: workspaceId, payload });
  }

  it("appends a valid boot step to the workspace node's bootSteps", () => {
    const node = makeNode("ws-1", { status: "provisioning" });
    const { get, set, state } = makeStore([node]);

    handleCanvasEvent(
      bootMsg("ws-1", {
        step: 1,
        total: 8,
        key: "PWR",
        label: "Provision compute",
        status: "running",
        message: "allocating container…",
      }),
      get,
      set
    );

    const steps = state.nodes[0].data.bootSteps;
    expect(steps).toHaveLength(1);
    expect(steps![0]).toEqual({
      step: 1,
      total: 8,
      key: "PWR",
      label: "Provision compute",
      status: "running",
      message: "allocating container…",
      // Arrival timestamp — feeds the watchdog log's stable offsets. Live
      // events are client-clock-stamped (serverClock: false); the offset
      // math clamps deltas across the server/client clock boundary.
      at: expect.any(Number),
      serverClock: false,
    });
    // The same event also lands as the first watchdog log line.
    const log = state.nodes[0].data.bootLog!;
    expect(log).toHaveLength(1);
    expect(log[0].text).toBe("allocating container…");
    expect(log[0].status).toBe("running");
  });

  it("appends a NEW log line for each distinct heartbeat message (same step+status)", () => {
    const node = makeNode("ws-1", { status: "provisioning" });
    const { get, set, state } = makeStore([node]);

    const base = { step: 1, total: 8, key: "PWR", label: "Provision compute", status: "running" };
    handleCanvasEvent(bootMsg("ws-1", { ...base, message: "building image — 20s elapsed" }), get, set);
    handleCanvasEvent(bootMsg("ws-1", { ...base, message: "building image — 40s elapsed" }), get, set);
    // Identical re-broadcast must NOT duplicate.
    handleCanvasEvent(bootMsg("ws-1", { ...base, message: "building image — 40s elapsed" }), get, set);

    // Keycaps stay latest-per-step…
    expect(state.nodes[0].data.bootSteps).toHaveLength(1);
    expect(state.nodes[0].data.bootSteps![0].message).toBe("building image — 40s elapsed");
    // …while the watchdog log keeps the full history.
    const log = state.nodes[0].data.bootLog!;
    expect(log.map((l) => l.text)).toEqual([
      "building image — 20s elapsed",
      "building image — 40s elapsed",
    ]);
  });

  it("merges a running→ok transition in place (latest status per step wins)", () => {
    const node = makeNode("ws-1", {
      status: "provisioning",
      bootSteps: [
        { step: 1, total: 8, key: "PWR", label: "Provision compute", status: "running" },
      ],
    });
    const { get, set, state } = makeStore([node]);

    handleCanvasEvent(
      bootMsg("ws-1", { step: 1, total: 8, key: "PWR", label: "Provision compute", status: "ok" }),
      get,
      set
    );

    const steps = state.nodes[0].data.bootSteps!;
    expect(steps).toHaveLength(1);
    expect(steps[0].status).toBe("ok");
  });

  it("keeps distinct steps separate and ordered by arrival", () => {
    const node = makeNode("ws-1", { status: "provisioning" });
    const { get, set, state } = makeStore([node]);

    handleCanvasEvent(bootMsg("ws-1", { step: 1, total: 8, key: "PWR", label: "Provision compute", status: "ok" }), get, set);
    handleCanvasEvent(bootMsg("ws-1", { step: 2, total: 8, key: "RT", label: "Start runtime", status: "running" }), get, set);

    const steps = state.nodes[0].data.bootSteps!;
    expect(steps.map((s) => s.step)).toEqual([1, 2]);
    expect(steps.map((s) => s.status)).toEqual(["ok", "running"]);
  });

  it("drops malformed steps (bad status / out-of-range / missing fields)", () => {
    const node = makeNode("ws-1", { status: "provisioning" });
    const { get, set, state } = makeStore([node]);

    // bad status
    handleCanvasEvent(bootMsg("ws-1", { step: 1, total: 8, key: "PWR", label: "x", status: "bogus" }), get, set);
    // step < 1
    handleCanvasEvent(bootMsg("ws-1", { step: 0, total: 8, key: "PWR", label: "x", status: "ok" }), get, set);
    // total < step
    handleCanvasEvent(bootMsg("ws-1", { step: 5, total: 3, key: "PWR", label: "x", status: "ok" }), get, set);
    // missing key
    handleCanvasEvent(bootMsg("ws-1", { step: 1, total: 8, label: "x", status: "ok" }), get, set);

    expect(state.nodes[0].data.bootSteps ?? []).toHaveLength(0);
  });

  it("renders a status-derived log line when the runtime posts an EMPTY message", () => {
    // boot_event.go broadcasts `message` verbatim — plain status transitions
    // arrive as "" and must not become blank watchdog lines (2026-07-18
    // live-boot regression: half the log rendered as bare timestamps).
    const node = makeNode("ws-1", { status: "provisioning" });
    const { get, set, state } = makeStore([node]);

    handleCanvasEvent(
      bootMsg("ws-1", { step: 5, total: 8, key: "TOOL", label: "Enumerate tools", status: "ok", message: "" }),
      get,
      set
    );

    expect(state.nodes[0].data.bootLog!.map((l) => l.text)).toEqual(["Enumerate tools — done"]);
  });

  it("clears bootSteps/bootLog when WORKSPACE_PROVISIONING restarts a node", () => {
    // A restart starts a NEW boot generation (the server drops its trace on
    // the same event) — stale telemetry must not render the previous boot's
    // keycaps/failure banner over the fresh boot.
    resetProvisioningSequence();
    const node = makeNode("ws-1", {
      status: "failed",
      bootSteps: [
        { step: 5, total: 8, key: "TOOL", label: "Enumerate tools", status: "failed", message: "boom" },
      ],
      bootLog: [{ id: "0:5-failed", at: 100, t: 0, status: "failed", text: "boom" }],
    });
    const { get, set, state } = makeStore([node]);

    handleCanvasEvent(
      makeMsg({ event: "WORKSPACE_PROVISIONING", workspace_id: "ws-1", payload: { name: "WS" } }),
      get,
      set
    );

    const data = state.nodes[0].data;
    expect(data.status).toBe("provisioning");
    expect(data.bootSteps).toBeUndefined();
    expect(data.bootLog).toBeUndefined();
  });

  it("drops a BOOT_STEP for an unknown workspace (indeterminate-boot fallback)", () => {
    const { get, set, state } = makeStore([]);
    handleCanvasEvent(
      bootMsg("ws-missing", { step: 1, total: 8, key: "PWR", label: "x", status: "ok" }),
      get,
      set
    );
    expect(state.nodes).toHaveLength(0);
  });
});
