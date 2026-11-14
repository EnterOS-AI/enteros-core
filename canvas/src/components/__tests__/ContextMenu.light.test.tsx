// @vitest-environment jsdom
/**
 * ContextMenu — Enter OS Light: a parked or parking agent answers pause,
 * resume and restart with 409, so those items give way to "Open"; an open
 * agent offers "Park". Disabled states follow light-api.md.
 */
import React from "react";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const toast = vi.hoisted(() => vi.fn());
vi.mock("../Toaster", () => ({ showToast: toast }));

const post = vi.hoisted(() => vi.fn());
vi.mock("@/lib/api", () => ({ api: { post, get: vi.fn(), patch: vi.fn(), del: vi.fn(), put: vi.fn() } }));

type NodeData = { name: string; status: string; tier: number; role: string; parentId?: string | null; lightState?: string | null };
const state = vi.hoisted(() => ({
  contextMenu: null as null | { x: number; y: number; nodeId: string; nodeData: NodeData },
  closeContextMenu: vi.fn(),
  updateNodeData: vi.fn(),
  selectNode: vi.fn(),
  setPanelTab: vi.fn(),
  nestNode: vi.fn(),
  setPendingDelete: vi.fn(),
  setCollapsed: vi.fn(),
  arrangeChildren: vi.fn(),
  nodes: [] as Array<{ id: string; data: { parentId?: string | null } }>,
}));
vi.mock("@/store/canvas", () => ({
  useCanvasStore: Object.assign((sel: (s: typeof state) => unknown) => sel(state), { getState: () => state }),
}));

import { ContextMenu } from "../ContextMenu";

function open(nodeData: Partial<NodeData>) {
  state.contextMenu = { x: 10, y: 10, nodeId: "ws-1", nodeData: { name: "Reporter", status: "online", tier: 2, role: "", ...nodeData } };
  return render(<ContextMenu />);
}

const item = (name: string) => screen.queryByRole("menuitem", { name: new RegExp(`^\\S*\\s*${name}$`) }) as HTMLButtonElement | null;

function apiError(status: number, body: Record<string, unknown>) {
  const e = new Error(`API POST: ${status}`) as Error & { status: number; bodyText: string };
  e.status = status;
  e.bodyText = JSON.stringify(body);
  return e;
}

beforeEach(() => {
  post.mockReset();
  toast.mockReset();
  state.updateNodeData.mockReset();
  state.closeContextMenu.mockReset();
  vi.stubGlobal("requestAnimationFrame", (cb: FrameRequestCallback) => { cb(0); return 1; });
  vi.stubGlobal("cancelAnimationFrame", () => {});
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("ContextMenu — Light agents", () => {
  it("a parked agent offers Open and no Pause, Resume or Restart; its status reads Parked", () => {
    open({ status: "hibernated", lightState: "parked" });
    expect(item("Open")).toBeTruthy();
    expect(item("Open")!.disabled).toBe(false);
    expect(item("Pause")).toBeNull();
    expect(item("Resume")).toBeNull();
    expect(item("Restart")).toBeNull();
    expect(item("Park")).toBeNull();
    const status = screen.getByText("Parked");
    expect(status.getAttribute("title")).toBe("Asleep between scheduled runs");
    expect(screen.queryByText("hibernated")).toBeNull();
  });

  it("a parking agent shows Open disabled (it answers 409 until the park is stuck) and no Pause, Resume or Restart", () => {
    open({ status: "online", lightState: "parking" });
    expect(item("Open")!.disabled).toBe(true);
    expect(item("Pause")).toBeNull();
    expect(item("Restart")).toBeNull();
  });

  it("an open agent offers Park beside the usual lifecycle items", () => {
    open({ status: "online", lightState: "open" });
    expect(item("Park")!.disabled).toBe(false);
    expect(item("Pause")).toBeTruthy();
    expect(item("Restart")).toBeTruthy();
    expect(item("Open")).toBeNull();
  });

  it.each([
    ["offline", true],
    ["degraded", false],
  ])("Park on an open agent that is %s is disabled=%s (park needs it online or degraded)", (status, disabled) => {
    open({ status, lightState: "open" });
    expect(item("Park")!.disabled).toBe(disabled);
  });

  it("an opening agent shows Park disabled", () => {
    open({ status: "provisioning", lightState: "opening" });
    expect(item("Park")!.disabled).toBe(true);
  });

  it("an ordinary agent shows neither", () => {
    open({ status: "online", lightState: null });
    expect(item("Open")).toBeNull();
    expect(item("Park")).toBeNull();
    expect(item("Pause")).toBeTruthy();
  });

  it("Open sends one POST /light/open and moves the node to the answered state", async () => {
    let resolve!: (v: unknown) => void;
    post.mockImplementation(() => new Promise((r) => { resolve = r; }));
    open({ status: "hibernated", lightState: "parked" });
    fireEvent.click(item("Open")!);
    fireEvent.click(item("Open")!);
    expect(post).toHaveBeenCalledTimes(1);
    expect(post).toHaveBeenCalledWith("/workspaces/ws-1/light/open");
    expect(item("Open")!.disabled).toBe(true);
    await act(async () => { resolve({ light_state: "opening", cancelled: [] }); });
    expect(state.updateNodeData).toHaveBeenCalledWith("ws-1", { lightState: "opening" });
    expect(toast).toHaveBeenCalledWith("Opening the agent", "success");
    expect(state.closeContextMenu).toHaveBeenCalled();
  });

  it("Park sends POST /light/park; a refusal is explained by its code", async () => {
    post.mockRejectedValue(apiError(409, { error: "busy", code: "LIGHT_AGENT_BUSY", active_tasks: 1 }));
    open({ status: "online", lightState: "open" });
    await act(async () => { fireEvent.click(item("Park")!); });
    expect(post).toHaveBeenCalledWith("/workspaces/ws-1/light/park");
    expect(toast).toHaveBeenCalledWith("The agent is working on a task. Park it once the task is done.", "error");
    expect(state.updateNodeData).not.toHaveBeenCalled();
  });
});
