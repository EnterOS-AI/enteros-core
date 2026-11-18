// @vitest-environment jsdom
//
// Enter OS Light in the Chat tab: a parked or parking agent answers chat
// with 409 and is not woken by it, so the tab shows a banner with "Open",
// disables the composer, never posts to /a2a, and turns a 409 that still
// happens into the same banner instead of a generic error.

import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import { render, screen, cleanup, waitFor, fireEvent, act } from "@testing-library/react";
import React from "react";

afterEach(cleanup);

const apiPost = vi.fn();
vi.mock("@/lib/api", () => ({
  api: {
    get: vi.fn((path: string) => {
      if (path.includes("/chat-history")) return Promise.resolve({ messages: [], reached_end: true });
      return Promise.resolve([]);
    }),
    post: (path: string, body?: unknown) => apiPost(path, body),
    del: vi.fn(),
    patch: vi.fn(),
    put: vi.fn(),
  },
}));

const store = vi.hoisted(() => ({
  updateNodeData: vi.fn(),
  restartWorkspace: vi.fn(),
  nodes: [] as Array<{ id: string; data: { lightState?: string | null } }>,
}));
vi.mock("@/store/canvas", () => {
  const state = {
    wsStatus: "connected",
    agentMessages: {},
    consumeAgentMessages: () => [],
    agentMessageConsumers: {},
    retainAgentMessages: () => {},
    releaseAgentMessages: () => {},
    get nodes() { return store.nodes; },
    updateNodeData: store.updateNodeData,
    restartWorkspace: store.restartWorkspace,
    setPanelTab: vi.fn(),
  };
  const hook = (selector?: (s: unknown) => unknown) => (selector ? selector(state) : state);
  hook.getState = () => state;
  return { useCanvasStore: hook };
});

beforeEach(() => {
  apiPost.mockReset();
  store.updateNodeData.mockReset();
  store.nodes = [];
  (window as unknown as { IntersectionObserver: unknown }).IntersectionObserver = vi.fn(() => ({
    observe: vi.fn(), unobserve: vi.fn(), disconnect: vi.fn(),
  }));
  Element.prototype.scrollIntoView = vi.fn();
});

import { ChatTab } from "../ChatTab";

type Data = Parameters<typeof ChatTab>[0]["data"];
const WS = "ws-light";
const PARKED_TEXT = "This Light agent is parked: it runs its schedules while asleep. Open it to chat.";

function data(over: Record<string, unknown>): Data {
  return { name: "Reporter", status: "online", runtime: "hermes", currentTask: "", lightState: null, ...over } as unknown as Data;
}

function apiError(status: number, body: Record<string, unknown>) {
  const e = new Error(`API POST: ${status}`) as Error & { status: number; bodyText: string };
  e.status = status;
  e.bodyText = JSON.stringify(body);
  return e;
}

const a2aPosts = () => apiPost.mock.calls.filter(([p]) => String(p).includes("/a2a")).length;

async function ready() {
  await waitFor(() => expect(screen.queryByText(/Loading chat history/i)).toBeNull());
}

function typeAndSend(text: string) {
  const box = screen.getByLabelText("Message to agent") as HTMLTextAreaElement;
  fireEvent.change(box, { target: { value: text } });
  fireEvent.keyDown(box, { key: "Enter" });
}

describe("ChatTab — a parked Light agent", () => {
  it("shows the banner, disables the composer, and hides the generic error and its Restart", async () => {
    render(<ChatTab workspaceId={WS} data={data({ status: "hibernated", lightState: "parked" })} />);
    await ready();
    expect(screen.getByText(PARKED_TEXT)).toBeTruthy();
    expect(screen.getByRole("button", { name: "Open" })).toBeTruthy();
    const box = screen.getByLabelText("Message to agent") as HTMLTextAreaElement;
    expect(box.disabled).toBe(true);
    expect(box.placeholder).toBe("Open the agent to chat");
    expect((screen.getByRole("button", { name: "Send" }) as HTMLButtonElement).disabled).toBe(true);
    expect(screen.queryByText(/Agent is hibernated/)).toBeNull();
    expect(screen.queryByRole("button", { name: /^restart$/i })).toBeNull();
  });

  it("never posts to /a2a, even with a stale task in flight", async () => {
    render(<ChatTab workspaceId={WS} data={data({ status: "hibernated", lightState: "parked", currentTask: "old task" })} />);
    await ready();
    typeAndSend("hello");
    expect(screen.queryByRole("button", { name: /Stop current agent turn/i })).toBeNull();
    expect(a2aPosts()).toBe(0);
  });

  it("is the same while the agent is being parked and its pod is still up", async () => {
    render(<ChatTab workspaceId={WS} data={data({ status: "online", lightState: "parking" })} />);
    await ready();
    expect(screen.getByText(PARKED_TEXT)).toBeTruthy();
    expect((screen.getByLabelText("Message to agent") as HTMLTextAreaElement).disabled).toBe(true);
    typeAndSend("hello");
    expect(a2aPosts()).toBe(0);
  });

  it("Open sends one POST /light/open, disabled while in flight, and moves the node on", async () => {
    store.nodes = [{ id: WS, data: { lightState: "parked" } }];
    let resolve!: (v: unknown) => void;
    apiPost.mockImplementation(() => new Promise((r) => { resolve = r; }));
    render(<ChatTab workspaceId={WS} data={data({ status: "hibernated", lightState: "parked" })} />);
    await ready();
    const open = screen.getByRole("button", { name: "Open" });
    fireEvent.click(open);
    fireEvent.click(open);
    expect(apiPost).toHaveBeenCalledTimes(1);
    expect(apiPost).toHaveBeenCalledWith(`/workspaces/${WS}/light/open`, undefined);
    expect((screen.getByRole("button", { name: "Opening…" }) as HTMLButtonElement).disabled).toBe(true);
    await act(async () => { resolve({ light_state: "opening", cancelled: [] }); });
    expect(store.updateNodeData).toHaveBeenCalledWith(WS, { lightState: "opening" });
  });

  it("explains a refused open", async () => {
    apiPost.mockRejectedValue(apiError(409, { error: "x", code: "LIGHT_TRANSITION_IN_PROGRESS" }));
    render(<ChatTab workspaceId={WS} data={data({ status: "online", lightState: "parking" })} />);
    await ready();
    fireEvent.click(screen.getByRole("button", { name: "Open" }));
    expect((await screen.findByRole("alert")).textContent).toMatch(/Another Light step is running/);
  });

  it("shows progress, not an error, while the agent is opening", async () => {
    render(<ChatTab workspaceId={WS} data={data({ status: "hibernated", lightState: "opening" })} />);
    await ready();
    expect(screen.getByText("Opening the agent. Chat is available once it is up.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Open" })).toBeNull();
    expect(screen.queryByText(/Agent is hibernated/)).toBeNull();
  });
});

describe("ChatTab — a send that meets 409 LIGHT_AGENT_PARKED", () => {
  it.each([
    ["LIGHT_AGENT_PARKED", "parked"],
    ["LIGHT_AGENT_PARKING", "parking"],
  ])("%s shows the Light banner, not a generic error, and corrects the node", async (code, state) => {
    store.nodes = [{ id: WS, data: { lightState: "open" } }];
    apiPost.mockRejectedValue(apiError(409, { error: "parked", code, light_state: state }));
    render(<ChatTab workspaceId={WS} data={data({ status: "online", lightState: "open" })} />);
    await ready();
    typeAndSend("hello");
    expect(await screen.findByText(PARKED_TEXT)).toBeTruthy();
    expect(screen.queryByText(/Failed to send message/)).toBeNull();
    expect((screen.getByLabelText("Message to agent") as HTMLTextAreaElement).disabled).toBe(true);
    expect(store.updateNodeData).toHaveBeenCalledWith(WS, { lightState: state });
  });

  it("any other 409 keeps the generic error", async () => {
    apiPost.mockRejectedValue(apiError(409, { error: "busy", code: "SOMETHING_ELSE" }));
    render(<ChatTab workspaceId={WS} data={data({ status: "online", lightState: null })} />);
    await ready();
    typeAndSend("hello");
    expect(await screen.findByText(/Failed to send message/)).toBeTruthy();
    expect(screen.queryByText(PARKED_TEXT)).toBeNull();
  });
});
