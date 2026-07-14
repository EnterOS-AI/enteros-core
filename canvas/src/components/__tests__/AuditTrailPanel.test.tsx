// @vitest-environment jsdom
/**
 * AuditTrailPanel contract tests.
 *
 * The response fixtures intentionally match AuditHandler.Query so a Canvas
 * model drift (field names, pagination, or chain scope) fails here.
 */
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, cleanup, fireEvent, act } from "@testing-library/react";

vi.mock("@/lib/api", () => ({
  api: { get: vi.fn() },
}));

import { api } from "@/lib/api";
import {
  formatAuditRelativeTime,
  AuditEntryRow,
  AuditTrailPanel,
} from "../AuditTrailPanel";
import type { AuditEvent, AuditResponse } from "@/types/audit";

const mockGet = vi.mocked(api.get);
const NOW = 1_745_000_000_000;

function makeEvent(overrides: Partial<AuditEvent> = {}): AuditEvent {
  return {
    id: "audit-1",
    timestamp: new Date(NOW - 120_000).toISOString(),
    agent_id: "research-agent",
    session_id: "session-1",
    operation: "tool_call",
    input_hash: null,
    output_hash: null,
    model_used: "browser.open",
    human_oversight_flag: false,
    risk_flag: false,
    prev_hmac: null,
    hmac: "test-hmac",
    workspace_id: "ws-a",
    ...overrides,
  };
}

function makeResponse(
  events: AuditEvent[],
  total = events.length,
  chainValid: boolean | null = true
): AuditResponse {
  return { events, total, chain_valid: chainValid };
}

describe("formatAuditRelativeTime", () => {
  it("returns 'just now' when diff < 60 s", () => {
    expect(formatAuditRelativeTime(new Date(NOW - 30_000).toISOString(), NOW)).toBe("just now");
  });

  it("returns minutes for minute-scale diffs", () => {
    expect(formatAuditRelativeTime(new Date(NOW - 3 * 60_000).toISOString(), NOW)).toBe("3m ago");
  });

  it("returns hours for hour-scale diffs", () => {
    expect(formatAuditRelativeTime(new Date(NOW - 2 * 3_600_000).toISOString(), NOW)).toBe("2h ago");
  });

  it("returns a locale date for diffs of at least one day", () => {
    const result = formatAuditRelativeTime(new Date(NOW - 25 * 3_600_000).toISOString(), NOW);
    expect(result).not.toMatch(/ago/);
    expect(result.length).toBeGreaterThan(0);
  });
});

describe("AuditEntryRow", () => {
  afterEach(() => cleanup());

  it.each(["task_start", "llm_call", "tool_call", "task_end"])(
    "renders the %s operation badge",
    (operation) => {
      render(<AuditEntryRow entry={makeEvent({ operation })} now={NOW} />);
      expect(screen.getByText(operation)).toBeTruthy();
    }
  );

  it("renders the agent, model/tool, and relative timestamp", () => {
    render(
      <AuditEntryRow
        entry={makeEvent({ agent_id: "agent-alpha", model_used: "claude-sonnet" })}
        now={NOW}
      />
    );
    expect(screen.getByText("agent-alpha")).toBeTruthy();
    expect(screen.getByText("claude-sonnet")).toBeTruthy();
    expect(screen.getByText("2m ago")).toBeTruthy();
  });

  it("falls back to the session ID when model_used is null", () => {
    render(
      <AuditEntryRow
        entry={makeEvent({ model_used: null, session_id: "session-fallback" })}
        now={NOW}
      />
    );
    expect(screen.getByText("Session session-fallback")).toBeTruthy();
  });

  it("surfaces oversight and risk flags without rendering ledger secrets", () => {
    render(
      <AuditEntryRow
        entry={makeEvent({
          human_oversight_flag: true,
          risk_flag: true,
          input_hash: "private-input-hash",
          hmac: "private-ledger-hmac",
        })}
        now={NOW}
      />
    );
    expect(screen.getByText(/human oversight/)).toBeTruthy();
    expect(screen.getByText(/risk flagged/)).toBeTruthy();
    expect(screen.queryByText("private-input-hash")).toBeNull();
    expect(screen.queryByText("private-ledger-hmac")).toBeNull();
  });
});

describe("AuditTrailPanel", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
    cleanup();
  });

  it("shows loading state while the request is in flight", () => {
    mockGet.mockReturnValue(new Promise(() => {}));
    render(<AuditTrailPanel workspaceId="ws-a" />);
    expect(screen.getByRole("status").textContent).toMatch(/loading audit trail/i);
  });

  it("renders the canonical events/total/chain_valid handler envelope", async () => {
    mockGet.mockResolvedValue(makeResponse([
      makeEvent({ agent_id: "agent-from-handler", operation: "tool_call" }),
    ]));

    render(<AuditTrailPanel workspaceId="ws-a" />);
    await act(async () => { await Promise.resolve(); });

    expect(screen.getByText("agent-from-handler")).toBeTruthy();
    expect(screen.getByText("tool_call")).toBeTruthy();
    expect(screen.getByText("browser.open")).toBeTruthy();
    expect(screen.getByText(/1 of 1 event loaded.*all loaded/i)).toBeTruthy();
  });

  it("shows an honest empty state for an empty events array", async () => {
    mockGet.mockResolvedValue(makeResponse([], 0, null));
    render(<AuditTrailPanel workspaceId="ws-a" />);
    await act(async () => { await Promise.resolve(); });
    expect(screen.getByText("No audit events yet")).toBeTruthy();
    expect(screen.getByText("No stored audit events are available for this workspace.")).toBeTruthy();
  });

  it("shows one ledger warning when the handler reports an invalid chain", async () => {
    mockGet.mockResolvedValue(makeResponse([makeEvent()], 1, false));
    render(<AuditTrailPanel workspaceId="ws-a" />);
    await act(async () => { await Promise.resolve(); });
    expect(screen.getByRole("alert").textContent).toMatch(/integrity check failed/i);
  });

  it("does not claim tampering when chain verification is unavailable", async () => {
    mockGet.mockResolvedValue(makeResponse([makeEvent()], 1, null));
    render(<AuditTrailPanel workspaceId="ws-a" />);
    await act(async () => { await Promise.resolve(); });
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("paginates with offset and never sends unsupported cursor/event_type params", async () => {
    mockGet
      .mockResolvedValueOnce(makeResponse([makeEvent({ id: "audit-1", agent_id: "alpha" })], 2))
      .mockResolvedValueOnce(makeResponse([makeEvent({ id: "audit-2", agent_id: "beta" })], 2, null));

    render(<AuditTrailPanel workspaceId="ws-a" />);
    await act(async () => { await Promise.resolve(); });
    fireEvent.click(screen.getByRole("button", { name: /load more/i }));
    await act(async () => { await Promise.resolve(); });

    expect(screen.getByText("alpha")).toBeTruthy();
    expect(screen.getByText("beta")).toBeTruthy();
    expect(screen.queryByRole("button", { name: /load more/i })).toBeNull();

    const firstPath = mockGet.mock.calls[0][0] as string;
    const secondPath = mockGet.mock.calls[1][0] as string;
    expect(firstPath).toContain("limit=50");
    expect(firstPath).not.toContain("cursor=");
    expect(firstPath).not.toContain("event_type=");
    expect(secondPath).toContain("offset=1");
    expect(secondPath).not.toContain("cursor=");
    expect(secondPath).not.toContain("event_type=");
  });

  it("refreshes from offset zero and replaces the loaded page", async () => {
    mockGet
      .mockResolvedValueOnce(makeResponse([makeEvent({ agent_id: "before-refresh" })]))
      .mockResolvedValueOnce(makeResponse([makeEvent({ id: "audit-2", agent_id: "after-refresh" })]));

    render(<AuditTrailPanel workspaceId="ws-a" />);
    await act(async () => { await Promise.resolve(); });
    fireEvent.click(screen.getByRole("button", { name: /refresh audit trail/i }));
    await act(async () => { await Promise.resolve(); });

    expect(screen.queryByText("before-refresh")).toBeNull();
    expect(screen.getByText("after-refresh")).toBeTruthy();
    expect(mockGet.mock.calls[1][0]).not.toContain("offset=");
  });

  it("shows the API error without misrepresenting it as an empty success", async () => {
    mockGet.mockRejectedValue(new Error("Network timeout"));
    render(<AuditTrailPanel workspaceId="ws-a" />);
    await act(async () => { await Promise.resolve(); });
    expect(screen.getByRole("alert").textContent).toContain("Network timeout");
    expect(screen.getByText("Audit events unavailable")).toBeTruthy();
    expect(screen.queryByText("No audit events yet")).toBeNull();
  });

  it("keeps existing events when loading the next page fails", async () => {
    mockGet
      .mockResolvedValueOnce(makeResponse([makeEvent({ agent_id: "already-loaded" })], 2))
      .mockRejectedValueOnce(new Error("Next page failed"));

    render(<AuditTrailPanel workspaceId="ws-a" />);
    await act(async () => { await Promise.resolve(); });
    fireEvent.click(screen.getByRole("button", { name: /load more/i }));
    await act(async () => { await Promise.resolve(); });

    expect(screen.getByText("already-loaded")).toBeTruthy();
    expect(screen.getByRole("alert").textContent).toContain("Next page failed");
  });
});
