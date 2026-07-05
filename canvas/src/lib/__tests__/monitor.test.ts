import { describe, expect, it } from "vitest";
import type { WorkspaceData } from "@/store/socket";
import {
  hasTraffic,
  maxBucketCount,
  sparklinePoints,
  formatRps,
  buildTopologyTree,
  type A2ATrafficResponse,
} from "@/lib/monitor";

function resp(total: number, counts: number[]): A2ATrafficResponse {
  return {
    window: "24h",
    bucket_seconds: 3600,
    buckets: counts.map((c, i) => ({ ts: `2026-06-29T${String(i).padStart(2, "0")}:00:00Z`, count: c })),
    rps_now: 0,
    rps_peak: 0,
    rps_peak_at: null,
    total,
  };
}

function ws(partial: Partial<WorkspaceData> & { id: string; name: string }): WorkspaceData {
  return {
    role: "",
    tier: 0,
    status: "online",
    agent_card: null,
    url: "",
    parent_id: null,
    active_tasks: 0,
    last_error_rate: 0,
    last_sample_error: "",
    uptime_seconds: 0,
    current_task: "",
    runtime: "",
    x: 0,
    y: 0,
    collapsed: false,
    budget_limit: null,
    ...partial,
  } as WorkspaceData;
}

describe("monitor helpers — honesty contract", () => {
  it("hasTraffic is false for an all-zero / empty series (no synthetic curve)", () => {
    expect(hasTraffic(null)).toBe(false);
    expect(hasTraffic(resp(0, [0, 0, 0]))).toBe(false);
  });

  it("hasTraffic is true once real traffic exists", () => {
    expect(hasTraffic(resp(5, [2, 0, 3]))).toBe(true);
  });

  it("maxBucketCount floors at 1 so a zero series scales flat", () => {
    expect(maxBucketCount([{ ts: "t", count: 0 }])).toBe(1);
    expect(maxBucketCount([{ ts: "t", count: 7 }, { ts: "t", count: 3 }])).toBe(7);
  });

  it("sparklinePoints maps an all-zero series to the bottom baseline", () => {
    const pts = sparklinePoints([
      { ts: "t", count: 0 },
      { ts: "t", count: 0 },
    ], 600, 140);
    // Both y values pinned to the height (bottom) — a flat empty line.
    expect(pts).toBe("0.00,140.00 600.00,140.00");
  });

  it("sparklinePoints puts the peak bucket at the top of the box", () => {
    const pts = sparklinePoints([
      { ts: "t", count: 0 },
      { ts: "t", count: 10 },
    ], 600, 140);
    // Second point (count 10 == max) → y 0 (top).
    expect(pts).toBe("0.00,140.00 600.00,0.00");
  });

  it("formatRps renders 2dp and is robust to nullish", () => {
    expect(formatRps(0)).toBe("0.00");
    expect(formatRps(0.05)).toBe("0.05");
    expect(formatRps(null)).toBe("0.00");
    expect(formatRps(undefined)).toBe("0.00");
  });
});

describe("buildTopologyTree — real graph, no fabrication", () => {
  it("nests children under parents and marks teams vs agents", () => {
    const tree = buildTopologyTree([
      ws({ id: "p", name: "Org", kind: "platform" }),
      ws({ id: "team", name: "Eng", kind: "workspace", parent_id: "p" }),
      ws({ id: "a1", name: "Coder", kind: "workspace", parent_id: "team" }),
      ws({ id: "a2", name: "Solo", kind: "workspace", parent_id: "p" }),
    ]);
    expect(tree).toHaveLength(1); // single platform root
    const root = tree[0];
    expect(root.id).toBe("p");
    expect(root.isTeam).toBe(true); // platform is always a team
    // children sorted by name: Eng (team), Solo (agent)
    expect(root.children.map((c) => c.id)).toEqual(["team", "a2"]);
    const team = root.children[0];
    expect(team.isTeam).toBe(true); // has a child
    expect(team.children[0].id).toBe("a1");
    expect(team.children[0].isTeam).toBe(false); // leaf agent
    const solo = root.children[1];
    expect(solo.isTeam).toBe(false); // leaf agent under platform
  });

  it("returns an empty tree for no workspaces", () => {
    expect(buildTopologyTree([])).toEqual([]);
  });

  it("surfaces an orphan (parent outside the set) as a root, never dropped", () => {
    const tree = buildTopologyTree([
      ws({ id: "x", name: "Orphan", parent_id: "missing" }),
    ]);
    expect(tree.map((n) => n.id)).toEqual(["x"]);
  });
});
