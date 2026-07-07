/**
 * Pin tests for the workspace-status TS mirror (lib/workspace-status.ts).
 *
 * The literal values are duplicated here ON PURPOSE — this is the drift
 * canary: if someone edits the mirror without a matching Go-enum change
 * (or vice versa and then "fixes" the mirror wrong), this test forces a
 * conscious same-PR update. SSOT = workspace-server/internal/models/
 * workspace_status.go (10 wire values, migrations 043 + 046).
 */
import { describe, expect, it } from "vitest";
import {
  CANVAS_SYNTHETIC_STATUS,
  WORKSPACE_STATUS,
} from "../workspace-status";

describe("WORKSPACE_STATUS", () => {
  it("mirrors all 10 Go wire values exactly", () => {
    expect(WORKSPACE_STATUS).toEqual({
      Provisioning: "provisioning",
      Online: "online",
      Offline: "offline",
      Degraded: "degraded",
      Failed: "failed",
      Removed: "removed",
      Paused: "paused",
      Hibernated: "hibernated",
      Hibernating: "hibernating",
      AwaitingAgent: "awaiting_agent",
    });
    expect(Object.keys(WORKSPACE_STATUS)).toHaveLength(10);
  });
});

describe("CANVAS_SYNTHETIC_STATUS", () => {
  it("carries exactly the two documented canvas-only synthetics", () => {
    expect(CANVAS_SYNTHETIC_STATUS).toEqual({
      Starting: "starting",
      NotConfigured: "not_configured",
    });
  });

  it("is disjoint from the wire enum (synthetics must never look like wire values)", () => {
    const wire = new Set<string>(Object.values(WORKSPACE_STATUS));
    for (const synthetic of Object.values(CANVAS_SYNTHETIC_STATUS)) {
      expect(wire.has(synthetic)).toBe(false);
    }
  });
});
