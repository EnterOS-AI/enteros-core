// @vitest-environment jsdom
/**
 * ScheduleTab — Enter OS Light (docs/api-protocol/light-api.md): the Light
 * card per state, the read-only parked list, the runs list, GET /light
 * polling, and the error-code sentences.
 */
import React from "react";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({ get: vi.fn(), post: vi.fn(), patch: vi.fn(), del: vi.fn(), put: vi.fn() }));
vi.mock("@/lib/api", () => ({ api, PLATFORM_URL: "", platformAuthHeaders: () => ({}) }));

const dialogs = vi.hoisted(() => ({}) as Record<string, { open: boolean; onConfirm: () => void; onCancel: () => void }>);
vi.mock("@/components/ConfirmDialog", () => ({
  ConfirmDialog: (p: { open: boolean; title: string; onConfirm: () => void; onCancel: () => void }) => {
    dialogs[p.title] = { open: p.open, onConfirm: p.onConfirm, onCancel: p.onCancel };
    return null;
  },
}));

import { ScheduleTab } from "../ScheduleTab";
import { useCanvasStore, type WorkspaceNodeData } from "@/store/canvas";
import type { Node } from "@xyflow/react";

const WS = "ws-1";

function nodeData(over: Partial<WorkspaceNodeData> = {}): WorkspaceNodeData {
  return {
    name: "Reporter", status: "online", tier: 2, agentCard: null, activeTasks: 0, collapsed: false, role: "",
    lastErrorRate: 0, lastSampleError: "", url: "", parentId: null, currentTask: "", runtime: "hermes",
    needsRestart: false, budgetLimit: null, kind: "workspace", lightState: null, ...over,
  };
}

function schedule(over: Record<string, unknown> = {}) {
  return {
    name: "daily-report", cron: "0 9 * * *", timezone: "Asia/Tokyo", prompt: "Write the daily report.", enabled: true,
    suspended: false, suspended_reason: null, suspended_at: null, consecutive_failures: 0, cronjob: "light-x",
    next_run_at: "2026-11-14T00:00:00Z", ...over,
  };
}

function lightInfo(over: Record<string, unknown> = {}) {
  return {
    workspace_id: WS, light_state: "parked", light_state_at: new Date().toISOString(), refresh_pending: false,
    pending_actions: [], last_error: null, schedules: [schedule()], ...over,
  };
}

function run(over: Record<string, unknown> = {}) {
  return {
    run_id: "light-x-1", schedule: "daily-report", status: "succeeded", source: "report",
    scheduled_for: "2026-11-13T00:00:00Z", started_at: "2026-11-13T00:00:07Z", finished_at: "2026-11-13T00:03:41Z",
    exit_code: 0, output: "Report sent.", error: "", created_at: "2026-11-13T00:00:07Z", activity_count: 0,
    activity_path: `/workspaces/${WS}/activity?run_id=light-x-1`, ...over,
  };
}

function apiError(status: number, body: Record<string, unknown>) {
  const e = new Error(`API POST: ${status} ${JSON.stringify(body)}`) as Error & { status: number; bodyText: string };
  e.status = status;
  e.bodyText = JSON.stringify(body);
  return e;
}

type Deferred<T> = { promise: Promise<T>; resolve: (v: T) => void; reject: (e: unknown) => void };
function deferred<T>(): Deferred<T> {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}

// Programmable GET answers.
let lightAnswer: () => unknown;
let runsAnswer: (path: string) => unknown;
let schedulesAnswer: () => unknown;
let activityAnswer: () => unknown;

function routeGets() {
  api.get.mockImplementation(async (path: string) => {
    if (path === `/workspaces/${WS}/light`) return lightAnswer();
    if (path.startsWith(`/workspaces/${WS}/light/runs?`)) return runsAnswer(path);
    if (path === `/workspaces/${WS}/schedules`) return schedulesAnswer();
    if (path.startsWith(`/workspaces/${WS}/activity`)) return activityAnswer();
    throw new Error(`unexpected GET ${path}`);
  });
}

const gets = (pred: (p: string) => boolean) => api.get.mock.calls.filter(([p]) => pred(p as string)).length;
const lightGets = () => gets((p) => p === `/workspaces/${WS}/light`);
const scheduleGets = () => gets((p) => p.includes("/schedules"));
const a2aCalls = () =>
  [...api.get.mock.calls, ...api.post.mock.calls].filter(([p]) => (p as string).includes("/a2a")).length;

function putNode(over: Partial<WorkspaceNodeData> = {}) {
  const node: Node<WorkspaceNodeData> = { id: WS, type: "workspaceNode", position: { x: 0, y: 0 }, data: nodeData(over) };
  useCanvasStore.setState({ nodes: [node] });
}

/** Feeds the tab from the store, as WorkspacePanelTabs does. */
function Harness() {
  const node = useCanvasStore((s) => s.nodes.find((n) => n.id === WS));
  return node ? <ScheduleTab workspaceId={WS} data={node.data} /> : null;
}

const storeLightState = () => useCanvasStore.getState().nodes[0]?.data.lightState;

async function flush() {
  await act(async () => {
    for (let i = 0; i < 6; i++) await Promise.resolve();
  });
}

const prevTZ = process.env.TZ;
beforeEach(() => {
  process.env.TZ = "Asia/Tokyo";
  for (const f of Object.values(api)) f.mockReset();
  for (const k of Object.keys(dialogs)) delete dialogs[k];
  lightAnswer = () => lightInfo();
  runsAnswer = () => ({ runs: [], next_before: null });
  schedulesAnswer = () => [];
  activityAnswer = () => [];
  routeGets();
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  process.env.TZ = prevTZ;
});

describe("not a Light agent", () => {
  it("offers Enable Light on a hermes agent and never polls GET /light", async () => {
    putNode({ lightState: null, runtime: "hermes" });
    render(<Harness />);
    expect(await screen.findByRole("button", { name: "Enable Light" })).toBeTruthy();
    expect(screen.getByText(/parks this agent between its scheduled runs/)).toBeTruthy();
    await flush();
    expect(lightGets()).toBe(0);
  });

  it("enables Light: one POST, the button disabled while it is in flight, the node becomes open", async () => {
    putNode({ lightState: null });
    const d = deferred<unknown>();
    api.post.mockReturnValue(d.promise);
    lightAnswer = () => lightInfo({ light_state: "open", schedules: [] });
    render(<Harness />);
    const btn = await screen.findByRole("button", { name: "Enable Light" });
    fireEvent.click(btn);
    fireEvent.click(btn);
    expect(api.post).toHaveBeenCalledTimes(1);
    expect(api.post).toHaveBeenCalledWith(`/workspaces/${WS}/light/enable`);
    expect((screen.getByRole("button", { name: "Enabling…" }) as HTMLButtonElement).disabled).toBe(true);
    await act(async () => { d.resolve({ status: "enabled", light_state: "open", restart: "started" }); });
    await flush();
    expect(storeLightState()).toBe("open");
    expect(await screen.findByText("Light is on. The agent restarts to apply it.")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Park now" })).toBeTruthy();
  });

  it("explains a refused enable by its code", async () => {
    putNode({ lightState: null });
    api.post.mockRejectedValue(apiError(409, { error: "paused", code: "LIGHT_AGENT_PAUSED" }));
    render(<Harness />);
    fireEvent.click(await screen.findByRole("button", { name: "Enable Light" }));
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toMatch(/paused\. Resume it, then enable Light/);
  });

  it("says Light needs a hermes agent for any other runtime", async () => {
    putNode({ lightState: null, runtime: "claude-code" });
    render(<Harness />);
    expect(await screen.findByText("Light needs a hermes agent.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Enable Light" })).toBeNull();
  });

  it("offers nothing on the org's platform agent", async () => {
    putNode({ lightState: null, runtime: "hermes", kind: "platform" });
    render(<Harness />);
    expect(await screen.findByText(/platform agent cannot be a Light agent/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Enable Light" })).toBeNull();
  });

  it("shows the runs list only when the agent has runs", async () => {
    putNode({ lightState: null });
    render(<Harness />);
    await screen.findByRole("button", { name: "Enable Light" });
    await flush();
    expect(gets((p) => p.startsWith(`/workspaces/${WS}/light/runs?`))).toBe(1);
    expect(screen.queryByTestId("light-runs")).toBeNull();
    cleanup();

    runsAnswer = () => ({ runs: [run()], next_before: null });
    render(<Harness />);
    expect(await screen.findByTestId("light-runs")).toBeTruthy();
  });
});

describe("open", () => {
  beforeEach(() => {
    putNode({ lightState: "open" });
    lightAnswer = () => lightInfo({ light_state: "open" });
  });

  it("explains the open state and keeps the agent's own editable list", async () => {
    schedulesAnswer = () => [{ id: "daily-report", workspace_id: WS, name: "daily-report", cron_expr: "0 9 * * *", timezone: "UTC",
      prompt: "p", enabled: true, last_run_at: null, next_run_at: null, run_count: 0, last_status: "", last_error: "", created_at: "" }];
    render(<Harness />);
    expect(await screen.findByText(
      "Open. Scheduled runs are paused while the agent is open; it parks itself after 30 minutes without activity.",
    )).toBeTruthy();
    expect(await screen.findByRole("button", { name: /edit schedule daily-report/i })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Disable Light" })).toBeTruthy();
  });

  it("park now sends one POST and disables every action while it is in flight", async () => {
    const d = deferred<unknown>();
    api.post.mockReturnValue(d.promise);
    render(<Harness />);
    const park = await screen.findByRole("button", { name: "Park now" });
    fireEvent.click(park);
    fireEvent.click(park);
    expect(api.post).toHaveBeenCalledTimes(1);
    expect(api.post).toHaveBeenCalledWith(`/workspaces/${WS}/light/park`);
    expect((screen.getByRole("button", { name: "Parking…" }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole("button", { name: "Disable Light" }) as HTMLButtonElement).disabled).toBe(true);
    lightAnswer = () => lightInfo({ light_state: "parking" });
    await act(async () => { d.resolve({ light_state: "parking", schedules: [], removed: [] }); });
    await flush();
    expect(storeLightState()).toBe("parking");
    expect(screen.getByText(/Parking: the schedules are handed/)).toBeTruthy();
  });

  it("maps a refused park to its sentence, LIGHT_INVALID with its list", async () => {
    api.post.mockRejectedValue(apiError(422, {
      error: "invalid", code: "LIGHT_INVALID",
      invalid: [{ schedule: "daily-report", field: "cron", reason: "six fields" }],
    }));
    render(<Harness />);
    fireEvent.click(await screen.findByRole("button", { name: "Park now" }));
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe("Some schedules cannot run as Light schedules: daily-report (cron: six fields).");
  });

  it("disable asks first, then sends one POST and the node stops being a Light agent", async () => {
    api.post.mockResolvedValue({ status: "disabled", light_state: null, deleted: { cronjobs: 1 }, restart: "started" });
    render(<Harness />);
    fireEvent.click(await screen.findByRole("button", { name: "Disable Light" }));
    expect(dialogs["Disable Light"].open).toBe(true);
    expect(api.post).not.toHaveBeenCalled();
    await act(async () => { dialogs["Disable Light"].onConfirm(); });
    await flush();
    expect(api.post).toHaveBeenCalledWith(`/workspaces/${WS}/light/disable`);
    expect(storeLightState()).toBeNull();
    expect(await screen.findByRole("button", { name: "Enable Light" })).toBeTruthy();
  });
});

describe("parked", () => {
  beforeEach(() => {
    putNode({ lightState: "parked", status: "hibernated" });
  });

  it("names the next run in the viewer's local time with the zone", async () => {
    render(<Harness />);
    // 00:00 UTC is 09:00 in Tokyo.
    const line = await screen.findByText(/^Parked\. Next run /);
    expect(line.textContent).toMatch(/9:00 AM GMT\+9 \(daily-report\)\.$/);
  });

  it("reads 'No upcoming run' and 'no upcoming run' when no schedule is due", async () => {
    lightAnswer = () => lightInfo({ schedules: [schedule({ next_run_at: null, enabled: false })] });
    render(<Harness />);
    expect(await screen.findByText("Parked. No upcoming run.")).toBeTruthy();
    expect(screen.getByText("Next: no upcoming run")).toBeTruthy();
  });

  it("lists the schedules read-only from GET /light and never calls GET /schedules or /a2a", async () => {
    vi.useFakeTimers();
    lightAnswer = () => lightInfo({
      schedules: [
        schedule(),
        schedule({ name: "weekly", cron: "30 8 * * 1-5", timezone: "", prompt: "Plan the week.", enabled: false, next_run_at: null }),
        schedule({ name: "flaky", cron: "0 7 * * *", prompt: "Check the feed.", suspended: true, suspended_reason: "three failed runs in a row", consecutive_failures: 3, next_run_at: null }),
      ],
    });
    api.post.mockResolvedValue({ run_id: "light-x-m1" });
    render(<Harness />);
    await flush();
    const list = screen.getByTestId("light-schedules");
    expect(within(list).getByText("Open the agent to edit schedules.")).toBeTruthy();
    expect(within(list).getByText("Daily at 09:00 Asia/Tokyo")).toBeTruthy();
    expect(within(list).getByText("Weekdays at 08:30 UTC")).toBeTruthy();
    expect(within(list).getByText("Write the daily report.")).toBeTruthy();
    expect(within(list).getByText(/Suspended: three failed runs in a row \(3 failed runs in a row\)/)).toBeTruthy();
    expect(within(list).getAllByText("off")).toHaveLength(1);
    expect(within(list).queryByRole("button", { name: /edit schedule/i })).toBeNull();
    expect(within(list).queryByRole("button", { name: /delete schedule/i })).toBeNull();
    expect(screen.queryByRole("button", { name: /add schedule/i })).toBeNull();
    // Run now, an expand, and a minute of polling: still nothing reaches the agent.
    fireEvent.click(within(list).getByRole("button", { name: "Run schedule weekly now" }));
    await flush();
    await act(async () => { await vi.advanceTimersByTimeAsync(60_000); });
    expect(scheduleGets()).toBe(0);
    expect(a2aCalls()).toBe(0);
    expect(api.post).toHaveBeenCalledWith(`/workspaces/${WS}/light/run`, { schedule: "weekly" });
  });

  it("run now starts a run, says so, and refreshes the runs list", async () => {
    const d = deferred<unknown>();
    api.post.mockReturnValue(d.promise);
    render(<Harness />);
    const btn = await screen.findByRole("button", { name: "Run schedule daily-report now" });
    await waitFor(() => expect(gets((p) => p.includes("/light/runs?"))).toBe(1));
    fireEvent.click(btn);
    fireEvent.click(btn);
    expect(api.post).toHaveBeenCalledTimes(1);
    expect((screen.getByRole("button", { name: "Run schedule daily-report now" }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole("button", { name: "Open" }) as HTMLButtonElement).disabled).toBe(true);
    await act(async () => { d.resolve({ run_id: "light-x-m1" }); });
    expect(await screen.findByText(/Started a run of daily-report \(light-x-m1\)/)).toBeTruthy();
    await waitFor(() => expect(gets((p) => p.includes("/light/runs?"))).toBe(2));
  });

  it("open and disable each send one POST and disable every action while in flight", async () => {
    const opening = deferred<unknown>();
    api.post.mockReturnValue(opening.promise);
    render(<Harness />);
    const open = await screen.findByRole("button", { name: "Open" });
    fireEvent.click(open);
    fireEvent.click(open);
    expect(api.post).toHaveBeenCalledTimes(1);
    expect((screen.getByRole("button", { name: "Opening…" }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole("button", { name: "Disable Light" }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole("button", { name: "Run schedule daily-report now" }) as HTMLButtonElement).disabled).toBe(true);
    await act(async () => { opening.resolve({ light_state: "opening", cancelled: [] }); });
    cleanup();

    putNode({ lightState: "parked", status: "hibernated" });
    api.post.mockReset();
    const disabling = deferred<unknown>();
    api.post.mockReturnValue(disabling.promise);
    render(<Harness />);
    fireEvent.click(await screen.findByRole("button", { name: "Disable Light" }));
    act(() => {
      dialogs["Disable Light"].onConfirm();
      dialogs["Disable Light"].onConfirm();
    });
    expect(api.post).toHaveBeenCalledTimes(1);
    expect(api.post).toHaveBeenCalledWith(`/workspaces/${WS}/light/disable`);
    expect((screen.getByRole("button", { name: "Open" }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole("button", { name: "Disable Light" }) as HTMLButtonElement).disabled).toBe(true);
    await act(async () => { disabling.resolve({ status: "disabled", light_state: null, restart: "woken" }); });
    expect(await screen.findByText("Light is off. The agent is waking up as an ordinary agent.")).toBeTruthy();
  });

  it("maps run-now refusals with their extras", async () => {
    api.post.mockRejectedValue(apiError(409, { error: "x", code: "LIGHT_RUN_ACTIVE", run_id: "light-x-7" }));
    render(<Harness />);
    fireEvent.click(await screen.findByRole("button", { name: "Run schedule daily-report now" }));
    expect((await screen.findByRole("alert")).textContent).toBe("That schedule already has a run going (light-x-7).");
  });

  it("open says how many queued runs it cancelled and polls until the agent is open", async () => {
    vi.useFakeTimers();
    render(<Harness />);
    await flush();
    lightAnswer = () => lightInfo({ light_state: "opening" });
    api.post.mockResolvedValue({ light_state: "opening", cancelled: ["light-x-29384756", "light-y-29384756"] });
    fireEvent.click(screen.getByRole("button", { name: "Open" }));
    await flush();
    expect(api.post).toHaveBeenCalledWith(`/workspaces/${WS}/light/open`);
    expect(screen.getByText("Opening. 2 queued runs that had not started were cancelled.")).toBeTruthy();
    expect(screen.getByText(/Opening: the schedules are paused/)).toBeTruthy();
    const before = lightGets();
    await act(async () => { await vi.advanceTimersByTimeAsync(3_000); });
    expect(lightGets()).toBe(before + 1);
    lightAnswer = () => lightInfo({ light_state: "open" });
    await act(async () => { await vi.advanceTimersByTimeAsync(3_000); });
    expect(storeLightState()).toBe("open");
    expect(screen.getByText(/^Open\. Scheduled runs are paused/)).toBeTruthy();
  });

  it("shows last_error and a queued refresh", async () => {
    lightAnswer = () => lightInfo({
      refresh_pending: true,
      last_error: { code: "LIGHT_REFRESH_OPEN_FAILED", message: "the refresh open failed", at: "2026-11-13T09:00:00Z" },
    });
    render(<Harness />);
    expect(await screen.findByText(/A refresh could not bring the agent up/)).toBeTruthy();
    expect(screen.getByText("Core said: the refresh open failed")).toBeTruthy();
    expect(screen.getByText("The agent will open briefly to pick up a change, then park again.")).toBeTruthy();
  });

  it("falls back to the server's message for a last_error code it does not know", async () => {
    lightAnswer = () => lightInfo({ last_error: { code: "LIGHT_NEW_THING", message: "something new happened", at: null } });
    render(<Harness />);
    expect(await screen.findByText("something new happened")).toBeTruthy();
  });
});

describe("transitions", () => {
  it("parking: buttons disabled until the park has been stuck for three minutes", async () => {
    putNode({ lightState: "parking" });
    lightAnswer = () => lightInfo({ light_state: "parking", light_state_at: new Date(Date.now() - 60_000).toISOString() });
    render(<Harness />);
    await screen.findByText(/Parking: the schedules are handed/);
    // The list is read-only from GET /light; run now works on a parked agent only.
    expect(screen.getByText("Daily at 09:00 Asia/Tokyo")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Run schedule daily-report now" })).toBeNull();
    expect((screen.getByRole("button", { name: "Open" }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole("button", { name: "Disable Light" }) as HTMLButtonElement).disabled).toBe(true);
    cleanup();

    lightAnswer = () => lightInfo({ light_state: "parking", light_state_at: new Date(Date.now() - 4 * 60_000).toISOString() });
    render(<Harness />);
    await screen.findByText(/Parking has not finished for over three minutes/);
    expect((screen.getByRole("button", { name: "Open" }) as HTMLButtonElement).disabled).toBe(false);
    expect((screen.getByRole("button", { name: "Disable Light" }) as HTMLButtonElement).disabled).toBe(false);
  });

  it("opening: buttons disabled, and the agent's own schedule API is left alone", async () => {
    putNode({ lightState: "opening", status: "provisioning" });
    lightAnswer = () => lightInfo({ light_state: "opening" });
    render(<Harness />);
    await screen.findByText(/Opening: the schedules are paused/);
    expect((screen.getByRole("button", { name: "Park now" }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole("button", { name: "Disable Light" }) as HTMLButtonElement).disabled).toBe(true);
    expect(screen.getByText("Daily at 09:00 Asia/Tokyo")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Run schedule daily-report now" })).toBeNull();
    expect(scheduleGets()).toBe(0);
  });
});

describe("GET /light polling", () => {
  beforeEach(() => { vi.useFakeTimers(); });

  it("polls every 10 s, every 3 s while parking, back to 10 s once parked, and stops on unmount", async () => {
    putNode({ lightState: "open" });
    lightAnswer = () => lightInfo({ light_state: "open" });
    const { unmount } = render(<Harness />);
    await flush();
    expect(lightGets()).toBe(1);
    await act(async () => { await vi.advanceTimersByTimeAsync(9_999); });
    expect(lightGets()).toBe(1);
    await act(async () => { await vi.advanceTimersByTimeAsync(1); });
    expect(lightGets()).toBe(2);

    lightAnswer = () => lightInfo({ light_state: "parking" });
    api.post.mockResolvedValue({ light_state: "parking", schedules: [], removed: [] });
    fireEvent.click(screen.getByRole("button", { name: "Park now" }));
    await flush();
    expect(lightGets()).toBe(3); // confirmed at once
    await act(async () => { await vi.advanceTimersByTimeAsync(3_000); });
    expect(lightGets()).toBe(4);
    lightAnswer = () => lightInfo({ light_state: "parked" });
    await act(async () => { await vi.advanceTimersByTimeAsync(3_000); });
    expect(lightGets()).toBe(5);
    expect(screen.getByText(/^Parked\. Next run/)).toBeTruthy();
    // The transition ended: no more 3 s polls.
    await act(async () => { await vi.advanceTimersByTimeAsync(9_999); });
    expect(lightGets()).toBe(5);
    await act(async () => { await vi.advanceTimersByTimeAsync(1); });
    expect(lightGets()).toBe(6);

    unmount();
    await act(async () => { await vi.advanceTimersByTimeAsync(120_000); });
    expect(lightGets()).toBe(6);
  });

  it("never overlaps two GET /light calls however slow the server is", async () => {
    putNode({ lightState: "parked", status: "hibernated" });
    let calls = 0;
    const slow = deferred<unknown>();
    lightAnswer = () => (++calls === 1 ? lightInfo() : slow.promise);
    render(<Harness />);
    await flush();
    await act(async () => { await vi.advanceTimersByTimeAsync(10_000); });
    expect(lightGets()).toBe(2);
    await act(async () => { await vi.advanceTimersByTimeAsync(120_000); });
    expect(lightGets()).toBe(2);
    await act(async () => { slow.resolve(lightInfo()); });
    await flush();
    await act(async () => { await vi.advanceTimersByTimeAsync(10_000); });
    expect(lightGets()).toBe(3);
  });

  it("refetches when the node's light_state moves on, and corrects a stale node", async () => {
    putNode({ lightState: "open" });
    lightAnswer = () => lightInfo({ light_state: "open" });
    render(<Harness />);
    await flush();
    expect(lightGets()).toBe(1);
    lightAnswer = () => lightInfo({ light_state: "parked" });
    act(() => { useCanvasStore.getState().updateNodeData(WS, { lightState: "parking" }); });
    await flush();
    expect(lightGets()).toBe(2);
    // GET /light knew better than the event: the node follows it.
    expect(storeLightState()).toBe("parked");
    expect(screen.getByText(/^Parked\. Next run/)).toBeTruthy();
  });
});

describe("runs list", () => {
  beforeEach(() => {
    putNode({ lightState: "parked", status: "hibernated" });
  });

  it("shows each run's status, source, times, exit code and error", async () => {
    runsAnswer = () => ({
      runs: [
        run({ run_id: "r-ok", activity_count: 2, activity_path: `/workspaces/${WS}/activity?run_id=r-ok` }),
        run({ run_id: "r-job", status: "timed_out", source: "job", exit_code: 124, output: "", error: "deadline exceeded",
          started_at: null, finished_at: "2026-11-13T01:00:00Z" }),
        run({ run_id: "r-live", status: "running", finished_at: null, exit_code: null, output: "" }),
      ],
      next_before: null,
    });
    render(<Harness />);
    const ok = await screen.findByTestId("light-run-r-ok");
    expect(within(ok).getByText("succeeded")).toBeTruthy();
    expect(within(ok).getByText("3m 34s")).toBeTruthy();
    expect(within(ok).getByText("0")).toBeTruthy(); // exit code
    expect(within(ok).getByText(/9:00 AM GMT\+9/)).toBeTruthy(); // scheduled, local time
    const job = screen.getByTestId("light-run-r-job");
    expect(within(job).getByText("timed out")).toBeTruthy();
    expect(within(job).getByText("recorded from Kubernetes")).toBeTruthy();
    expect(within(job).getByText("not started")).toBeTruthy();
    expect(within(job).getByText("124")).toBeTruthy();
    expect(within(job).getByText("Error: deadline exceeded")).toBeTruthy();
    const live = screen.getByTestId("light-run-r-live");
    expect(within(live).getByText("running")).toBeTruthy();
    expect(within(live).getByText("still running")).toBeTruthy();
  });

  it("expands a run to its output tail and the activity rows it wrote", async () => {
    runsAnswer = () => ({ runs: [run({ run_id: "r-ok", activity_count: 2, output: "line 1\nReport sent.",
      activity_path: `/workspaces/${WS}/activity?run_id=r-ok` })], next_before: null });
    activityAnswer = () => [
      { id: "a1", workspace_id: WS, activity_type: "agent_log", source_id: null, target_id: null, method: null,
        summary: "Fetched the numbers", request_body: null, response_body: null, duration_ms: null, status: "ok",
        error_detail: null, created_at: "2026-11-13T00:01:00Z" },
      { id: "a2", workspace_id: WS, activity_type: "a2a_send", source_id: null, target_id: null, method: "message/send",
        summary: null, request_body: null, response_body: null, duration_ms: null, status: "ok", error_detail: null,
        created_at: "2026-11-13T00:02:00Z" },
    ];
    render(<Harness />);
    const row = await screen.findByTestId("light-run-r-ok");
    const toggle = within(row).getByRole("button", { name: /Show output and activity \(2 activity rows\)/ });
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    fireEvent.click(toggle);
    expect(toggle.getAttribute("aria-expanded")).toBe("true");
    expect(within(row).getByText(/Report sent\./)).toBeTruthy();
    const list = await within(row).findByRole("list", { name: "Activity of run r-ok" });
    expect(within(list).getByText(/Fetched the numbers/)).toBeTruthy();
    expect(within(list).getByText(/message\/send/)).toBeTruthy();
    expect(api.get).toHaveBeenCalledWith(`/workspaces/${WS}/activity?run_id=r-ok`);
  });

  it("loads more with next_before and filters by schedule", async () => {
    runsAnswer = (path) => {
      const q = new URLSearchParams(path.split("?")[1]);
      if (q.get("before") === "cursor-1") return { runs: [run({ run_id: "r-old" })], next_before: null };
      if (q.get("schedule") === "weekly") return { runs: [run({ run_id: "r-weekly", schedule: "weekly" })], next_before: null };
      return { runs: [run({ run_id: "r-new" })], next_before: "cursor-1" };
    };
    lightAnswer = () => lightInfo({ schedules: [schedule(), schedule({ name: "weekly" })] });
    render(<Harness />);
    await screen.findByTestId("light-run-r-new");
    expect(api.get).toHaveBeenCalledWith(`/workspaces/${WS}/light/runs?limit=20`);
    fireEvent.click(screen.getByRole("button", { name: "Load more" }));
    expect(await screen.findByTestId("light-run-r-old")).toBeTruthy();
    expect(api.get).toHaveBeenCalledWith(`/workspaces/${WS}/light/runs?limit=20&before=cursor-1`);
    expect(screen.queryByRole("button", { name: "Load more" })).toBeNull();

    fireEvent.change(screen.getByLabelText("Schedule"), { target: { value: "weekly" } });
    expect(await screen.findByTestId("light-run-r-weekly")).toBeTruthy();
    expect(screen.queryByTestId("light-run-r-new")).toBeNull();
    expect(api.get).toHaveBeenCalledWith(`/workspaces/${WS}/light/runs?limit=20&schedule=weekly`);
  });
});
