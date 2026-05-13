// @vitest-environment jsdom
/**
 * Tests for ScheduleTab component.
 *
 * Covers: cronToHuman pure function, relativeTime pure function,
 * loading/error/empty states, schedule list rendering.
 */
import React from "react";
import { render, screen, fireEvent, cleanup, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ScheduleTab } from "../ScheduleTab";

const _mockGet = vi.hoisted(() => vi.fn<() => Promise<unknown[]>>());
vi.mock("@/lib/api", () => ({
  api: { get: _mockGet },
}));

afterEach(() => {
  cleanup();
  _mockGet.mockReset();
});

// ─── cronToHuman tests ─────────────────────────────────────────────────────

describe("ScheduleTab — cronToHuman", () => {
  it('returns "Every minute" for "* * * * *"', async () => {
    _mockGet.mockResolvedValueOnce([
      { id: "s1", workspace_id: "ws-1", name: "Test", cron_expr: "* * * * *",
        timezone: "UTC", prompt: "", enabled: true, last_run_at: null, next_run_at: null,
        run_count: 0, last_status: "ok", last_error: "", created_at: new Date().toISOString() },
    ]);
    render(<ScheduleTab workspaceId="ws-1" />);
    expect(await screen.findByText("Every minute")).toBeTruthy();
  });

  it("returns 'Every X minutes' for '*/X * * * *'", async () => {
    _mockGet.mockResolvedValueOnce([
      { id: "s1", workspace_id: "ws-1", name: "Test", cron_expr: "*/15 * * * *",
        timezone: "UTC", prompt: "", enabled: true, last_run_at: null, next_run_at: null,
        run_count: 0, last_status: "ok", last_error: "", created_at: new Date().toISOString() },
    ]);
    render(<ScheduleTab workspaceId="ws-1" />);
    expect(await screen.findByText("Every 15 minutes")).toBeTruthy();
  });

  it("returns 'Every X hours' for '0 */X * * *'", async () => {
    _mockGet.mockResolvedValueOnce([
      { id: "s1", workspace_id: "ws-1", name: "Test", cron_expr: "0 */3 * * *",
        timezone: "UTC", prompt: "", enabled: true, last_run_at: null, next_run_at: null,
        run_count: 0, last_status: "ok", last_error: "", created_at: new Date().toISOString() },
    ]);
    render(<ScheduleTab workspaceId="ws-1" />);
    expect(await screen.findByText("Every 3 hours")).toBeTruthy();
  });

  it("returns 'Daily at HH:MM UTC' for daily schedules", async () => {
    _mockGet.mockResolvedValueOnce([
      { id: "s1", workspace_id: "ws-1", name: "Test", cron_expr: "30 14 * * *",
        timezone: "UTC", prompt: "", enabled: true, last_run_at: null, next_run_at: null,
        run_count: 0, last_status: "ok", last_error: "", created_at: new Date().toISOString() },
    ]);
    render(<ScheduleTab workspaceId="ws-1" />);
    expect(await screen.findByText("Daily at 14:30 UTC")).toBeTruthy();
  });

  it("returns 'Weekdays at HH:MM UTC' for weekday schedules", async () => {
    _mockGet.mockResolvedValueOnce([
      { id: "s1", workspace_id: "ws-1", name: "Test", cron_expr: "0 9 * * 1-5",
        timezone: "UTC", prompt: "", enabled: true, last_run_at: null, next_run_at: null,
        run_count: 0, last_status: "ok", last_error: "", created_at: new Date().toISOString() },
    ]);
    render(<ScheduleTab workspaceId="ws-1" />);
    expect(await screen.findByText("Weekdays at 09:00 UTC")).toBeTruthy();
  });

  it("falls back to raw expression for unrecognised patterns", async () => {
    _mockGet.mockResolvedValueOnce([
      { id: "s1", workspace_id: "ws-1", name: "Test", cron_expr: "0 0 1 * *",
        timezone: "UTC", prompt: "", enabled: true, last_run_at: null, next_run_at: null,
        run_count: 0, last_status: "ok", last_error: "", created_at: new Date().toISOString() },
    ]);
    render(<ScheduleTab workspaceId="ws-1" />);
    expect(await screen.findByText("0 0 1 * *")).toBeTruthy();
  });

  it("falls back to raw expression for malformed input", async () => {
    _mockGet.mockResolvedValueOnce([
      { id: "s1", workspace_id: "ws-1", name: "Test", cron_expr: "not a cron",
        timezone: "UTC", prompt: "", enabled: true, last_run_at: null, next_run_at: null,
        run_count: 0, last_status: "ok", last_error: "", created_at: new Date().toISOString() },
    ]);
    render(<ScheduleTab workspaceId="ws-1" />);
    expect(await screen.findByText("not a cron")).toBeTruthy();
  });
});

// ─── relativeTime tests ─────────────────────────────────────────────────────

describe("ScheduleTab — relativeTime", () => {
  it('shows "Last: never" when last_run_at is null', async () => {
    // Use mockResolvedValue (persistent) instead of mockResolvedValueOnce because
    // ScheduleTab's 10 s auto-refresh interval fires and calls fetchSchedules
    // a second time, consuming a one-time mock and clearing the DOM.
    _mockGet.mockResolvedValue([
      { id: "s1", workspace_id: "ws-1", name: "Test", cron_expr: "0 9 * * *",
        timezone: "UTC", prompt: "", enabled: true, last_run_at: null, next_run_at: null,
        run_count: 0, last_status: "ok", last_error: "", created_at: new Date().toISOString() },
    ]);
    render(<ScheduleTab workspaceId="ws-1" />);
    // Use "Last: never" to match the exact label text in ScheduleTab.tsx:349.
    // findByText("never") would throw on the multiple-match ambiguity since
    // "never" also appears in the "Next: never" span.
    expect(await screen.findByText("Last: never")).toBeTruthy();
  });
});

// ─── States ───────────────────────────────────────────────────────────────

describe("ScheduleTab — states", () => {
  it("shows empty message when no schedules", async () => {
    _mockGet.mockResolvedValueOnce([]);
    render(<ScheduleTab workspaceId="ws-1" />);
    expect(await screen.findByText("No schedules yet")).toBeTruthy();
  });
  // Note: ScheduleTab silently swallows fetch errors (no error state for
  // the initial load). Error state only exists for form-level actions
  // (save/delete/toggle) which require api.post/del/patch mocking.
});

// ─── Schedule list ─────────────────────────────────────────────────────────

describe("ScheduleTab — list", () => {
  it("renders schedule name", async () => {
    _mockGet.mockResolvedValueOnce([
      { id: "s1", workspace_id: "ws-1", name: "Nightly Run", cron_expr: "0 2 * * *",
        timezone: "UTC", prompt: "", enabled: true, last_run_at: null, next_run_at: null,
        run_count: 0, last_status: "ok", last_error: "", created_at: new Date().toISOString() },
    ]);
    render(<ScheduleTab workspaceId="ws-1" />);
    expect(await screen.findByText("Nightly Run")).toBeTruthy();
  });

  it("renders multiple schedules", async () => {
    _mockGet.mockResolvedValueOnce([
      { id: "s1", workspace_id: "ws-1", name: "Schedule A", cron_expr: "0 9 * * *",
        timezone: "UTC", prompt: "", enabled: true, last_run_at: null, next_run_at: null,
        run_count: 0, last_status: "ok", last_error: "", created_at: new Date().toISOString() },
      { id: "s2", workspace_id: "ws-1", name: "Schedule B", cron_expr: "*/15 * * * *",
        timezone: "UTC", prompt: "", enabled: false, last_run_at: null, next_run_at: null,
        run_count: 0, last_status: "ok", last_error: "", created_at: new Date().toISOString() },
    ]);
    render(<ScheduleTab workspaceId="ws-1" />);
    expect(await screen.findByText("Schedule A")).toBeTruthy();
    expect(await screen.findByText("Schedule B")).toBeTruthy();
  });
});
