/**
 * lib/light.ts — the canvas side of docs/api-protocol/light-api.md: state
 * parsing, the GET /light and GET /light/runs parsers, the error-code table,
 * and the local-time helpers.
 */
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import {
  earliestNextRun,
  formatLightDuration,
  formatLightTime,
  isStuckPark,
  lightBadgeText,
  lightBlocksWake,
  lightCodeSentence,
  lightErrorSentence,
  lightLastErrorSentence,
  parseLightError,
  parseLightInfo,
  parseLightRunsPage,
  parseLightState,
  workspaceStatusLabel,
  type LightSchedule,
} from "../light";

function sched(over: Partial<LightSchedule> = {}): LightSchedule {
  return {
    name: "daily",
    cron: "0 9 * * *",
    timezone: "UTC",
    prompt: "p",
    enabled: true,
    suspended: false,
    suspended_reason: null,
    suspended_at: null,
    consecutive_failures: 0,
    cronjob: null,
    next_run_at: null,
    ...over,
  };
}

/** An ApiError as lib/api.ts throws it: status + raw body text. */
function apiError(status: number, body: unknown, message = `API POST /x: ${status}`): Error {
  const e = new Error(message) as Error & { status?: number; bodyText?: string };
  e.status = status;
  e.bodyText = typeof body === "string" ? body : JSON.stringify(body);
  return e;
}

describe("parseLightState", () => {
  it.each(["open", "parking", "parked", "opening"])("accepts %s", (s) => {
    expect(parseLightState(s)).toBe(s);
  });
  it.each([null, undefined, "", "closed", 3, {}])("maps %p to null", (v) => {
    expect(parseLightState(v)).toBeNull();
  });
});

describe("lightBlocksWake", () => {
  it("is true only while parking or parked", () => {
    expect(lightBlocksWake("parked")).toBe(true);
    expect(lightBlocksWake("parking")).toBe(true);
    expect(lightBlocksWake("open")).toBe(false);
    expect(lightBlocksWake("opening")).toBe(false);
    expect(lightBlocksWake(null)).toBe(false);
    expect(lightBlocksWake(undefined)).toBe(false);
  });
});

describe("parseLightInfo", () => {
  it("reads the documented body", () => {
    const info = parseLightInfo({
      workspace_id: "ws",
      light_state: "parked",
      light_state_at: "2026-11-13T09:00:04Z",
      refresh_pending: true,
      pending_actions: [],
      last_error: { code: "LIGHT_AGENT_BUSY", message: "busy", at: "2026-11-13T09:00:00Z" },
      schedules: [{ name: "daily", cron: "0 9 * * *", timezone: "America/Vancouver", prompt: "Write.", enabled: true, suspended: true,
        suspended_reason: "three failed runs in a row", suspended_at: "2026-11-12T09:00:00Z", consecutive_failures: 3,
        cronjob: "light-x", next_run_at: null }],
    });
    expect(info.light_state).toBe("parked");
    expect(info.light_state_at).toBe("2026-11-13T09:00:04Z");
    expect(info.refresh_pending).toBe(true);
    expect(info.last_error).toEqual({ code: "LIGHT_AGENT_BUSY", message: "busy", at: "2026-11-13T09:00:00Z" });
    expect(info.schedules[0]).toMatchObject({ name: "daily", suspended: true, consecutive_failures: 3, next_run_at: null });
  });

  it("tolerates a body that is not the documented shape", () => {
    const info = parseLightInfo([{ id: "sch-1" }]);
    expect(info).toEqual({ light_state: null, light_state_at: null, refresh_pending: false, last_error: null, schedules: [] });
    expect(parseLightInfo({ schedules: [null, 3, { name: 5 }, { name: "ok" }] }).schedules.map((s) => s.name)).toEqual(["ok"]);
  });
});

describe("parseLightRunsPage", () => {
  it("reads runs and the cursor", () => {
    const page = parseLightRunsPage({
      runs: [{ run_id: "r1", schedule: "daily", status: "succeeded", source: "job", scheduled_for: null, started_at: "2026-11-13T09:00:07Z",
        finished_at: "2026-11-13T09:03:41Z", exit_code: 0, output: "ok", error: "", created_at: "2026-11-13T09:00:07Z",
        activity_count: 2, activity_path: "/workspaces/ws/activity?run_id=r1" }],
      next_before: "cursor",
    });
    expect(page.next_before).toBe("cursor");
    expect(page.runs[0]).toMatchObject({ run_id: "r1", source: "job", exit_code: 0, activity_count: 2 });
  });

  it("tolerates junk", () => {
    expect(parseLightRunsPage([1, 2])).toEqual({ runs: [], next_before: null });
    expect(parseLightRunsPage({ runs: [{ schedule: "no id" }], next_before: 7 })).toEqual({ runs: [], next_before: null });
  });
});

describe("error mapping", () => {
  it("reads code and extras from the body lib/api.ts keeps", () => {
    const p = parseLightError(apiError(409, { error: "the agent is working", code: "LIGHT_AGENT_BUSY", active_tasks: 2 }));
    expect(p).toMatchObject({ status: 409, code: "LIGHT_AGENT_BUSY", message: "the agent is working" });
    expect(p.body.active_tasks).toBe(2);
  });

  it.each([
    ["LIGHT_RUNTIME_UNSUPPORTED", { runtime: "claude-code" }, /hermes agents only.*claude-code/],
    ["LIGHT_UNSUPPORTED", {}, /cannot run Light agents/],
    ["LIGHT_AGENT_PAUSED", {}, /paused\. Resume it/],
    ["LIGHT_PLATFORM_AGENT", {}, /platform agent cannot be a Light agent/],
    ["LIGHT_AGENT_BUSY", { active_tasks: 2 }, /working on a task/],
    ["LIGHT_AGENT_NOT_AWAKE", { status: "offline" }, /not online \(it is offline\)/],
    ["LIGHT_AGENT_NOT_READY", { status: "online" }, /starting or restarting.*Try again/],
    ["LIGHT_SCHEDULES_UNREADABLE", {}, /not a list of schedules/],
    ["LIGHT_TRANSITION_IN_PROGRESS", {}, /Another Light step/],
    ["LIGHT_RESTART_REQUIRED", {}, /not running with Light switched on/],
    ["LIGHT_INVALID", { invalid: [{ schedule: "daily", field: "cron", reason: "six fields" }, { schedule: "w", field: "timezone", reason: "unknown" }] },
      /daily \(cron: six fields\); w \(timezone: unknown\)/],
    ["LIGHT_TOO_LARGE", {}, /too large to park/],
    ["LIGHT_ENV_UNSUPPORTED", { key: "POD_IP" }, /POD_IP cannot be copied/],
    ["LIGHT_RUN_ACTIVE", { run_id: "light-x-1" }, /already has a run going \(light-x-1\)/],
    ["LIGHT_RUN_EXISTS", { run_id: "light-x-2" }, /started a run this second \(light-x-2\)/],
    ["LIGHT_AGENT_AWAKE", {}, /still stopping/],
    ["LIGHT_TENANT_SUSPENDED", {}, /suspended for billing/],
    ["LIGHT_SCHEDULE_NOT_FOUND", {}, /no schedule with that name/],
    ["LIGHT_BACKEND_UNAVAILABLE", {}, /control plane did not answer/],
  ])("%s has its own sentence", (code, extras, want) => {
    expect(lightCodeSentence(code, extras as Record<string, unknown>, "server text")).toMatch(want);
  });

  it("falls back to the server's message for a code it does not know", () => {
    expect(lightErrorSentence(apiError(409, { error: "the server says why", code: "LIGHT_SOMETHING_NEW" }))).toBe("the server says why");
  });

  it("never shows the raw API error text when the body has a message", () => {
    const s = lightErrorSentence(apiError(422, { error: "x", code: "LIGHT_INVALID", invalid: [] }));
    expect(s).not.toMatch(/API POST/);
  });

  it("falls back to the error's own message without a JSON body", () => {
    expect(lightErrorSentence(apiError(502, "<html>bad gateway</html>", "API POST /x: 502 <html>"))).toMatch(/502/);
    expect(lightErrorSentence(new TypeError("Failed to fetch"))).toMatch(/Failed to fetch/);
  });

  it("maps last_error codes from the doc's table, the busy undo included", () => {
    expect(lightLastErrorSentence({ code: "LIGHT_AGENT_BUSY", message: "m", at: null })).toMatch(/park was undone because the agent was working/);
    expect(lightLastErrorSentence({ code: "LIGHT_PARK_INCOMPLETE", message: "m", at: null })).toMatch(/keeps retrying/);
    expect(lightLastErrorSentence({ code: "LIGHT_OPEN_FAILED", message: "m", at: null })).toMatch(/failed to start/);
    expect(lightLastErrorSentence({ code: "LIGHT_REFRESH_OPEN_FAILED", message: "m", at: null })).toMatch(/parked again/);
    expect(lightLastErrorSentence({ code: "LIGHT_TENANT_SUSPENDED", message: "m", at: null })).toMatch(/suspended for billing/);
    expect(lightLastErrorSentence({ code: "LIGHT_UNKNOWN", message: "the server's words", at: null })).toBe("the server's words");
  });
});

describe("times", () => {
  const prevTZ = process.env.TZ;
  beforeEach(() => { process.env.TZ = "Asia/Tokyo"; });
  afterEach(() => { process.env.TZ = prevTZ; });

  it("shows the viewer's local time with the zone", () => {
    // 00:00 UTC is 09:00 in Tokyo.
    const s = formatLightTime("2026-11-14T00:00:00Z");
    expect(s).toMatch(/9:00/);
    expect(s).toMatch(/GMT\+9/);
  });

  it("reads 'no upcoming run' for a null next_run_at", () => {
    expect(formatLightTime(null)).toBe("no upcoming run");
    expect(formatLightTime(undefined)).toBe("no upcoming run");
    expect(formatLightTime("not a time")).toBe("no upcoming run");
  });

  it("formats durations", () => {
    expect(formatLightDuration("2026-11-13T09:00:07Z", "2026-11-13T09:03:41Z")).toBe("3m 34s");
    expect(formatLightDuration("2026-11-13T09:00:00Z", "2026-11-13T09:00:09Z")).toBe("9s");
    expect(formatLightDuration("2026-11-13T09:00:00Z", "2026-11-13T10:02:00Z")).toBe("1h 2m");
    expect(formatLightDuration(null, "2026-11-13T09:00:09Z")).toBeNull();
    expect(formatLightDuration("2026-11-13T09:00:09Z", "2026-11-13T09:00:00Z")).toBeNull();
  });

  it("picks the earliest upcoming run", () => {
    const list = [
      sched({ name: "late", next_run_at: "2026-11-14T09:00:00Z" }),
      sched({ name: "none", next_run_at: null }),
      sched({ name: "soon", next_run_at: "2026-11-13T10:00:00Z" }),
    ];
    expect(earliestNextRun(list)?.name).toBe("soon");
    expect(earliestNextRun([sched({ next_run_at: null })])).toBeNull();
  });

  it("calls a park stuck after three minutes", () => {
    const now = Date.parse("2026-11-13T09:10:00Z");
    expect(isStuckPark({ light_state: "parking", light_state_at: "2026-11-13T09:06:59Z" }, now)).toBe(true);
    expect(isStuckPark({ light_state: "parking", light_state_at: "2026-11-13T09:07:30Z" }, now)).toBe(false);
    expect(isStuckPark({ light_state: "parked", light_state_at: "2026-11-13T09:00:00Z" }, now)).toBe(false);
    expect(isStuckPark({ light_state: "parking", light_state_at: null }, now)).toBe(false);
  });
});

describe("labels", () => {
  const prevTZ = process.env.TZ;
  beforeEach(() => { process.env.TZ = "Asia/Tokyo"; });
  afterEach(() => { process.env.TZ = prevTZ; });

  it("badge text per state", () => {
    expect(lightBadgeText("open", null)).toBe("Light · open");
    expect(lightBadgeText("parking", null)).toBe("Light · parking");
    expect(lightBadgeText("opening", null)).toBe("Light · opening");
    expect(lightBadgeText("parked", null)).toBe("Light · parked");
    expect(lightBadgeText("parked", [sched({ next_run_at: null })])).toBe("Light · parked");
    expect(lightBadgeText("parked", [sched({ next_run_at: "2026-11-14T00:00:00Z" })])).toMatch(/^Light · next .*9:00.*GMT\+9/);
  });

  it("a parked agent's status reads Parked, with the tooltip", () => {
    expect(workspaceStatusLabel("hibernated", "parked")).toEqual({ label: "Parked", tooltip: "Asleep between scheduled runs" });
    expect(workspaceStatusLabel("hibernated", null)).toEqual({ label: "hibernated" });
    expect(workspaceStatusLabel("online", "open")).toEqual({ label: "online" });
    // Only the sleeping status is relabelled: a failure stays visible.
    expect(workspaceStatusLabel("failed", "parked")).toEqual({ label: "failed" });
  });
});
