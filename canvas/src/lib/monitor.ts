/**
 * Types + pure helpers for the OSS Monitor page (src/app/monitor +
 * src/components/monitor). The data shapes mirror the Go monitor handler
 * (workspace-server/internal/handlers/monitor.go) — the Go side is the SSOT.
 *
 * Everything here is pure (no React, no fetch) so it unit-tests in the node
 * vitest environment without a DOM. The honesty contract lives here too:
 * `hasTraffic` is the single predicate the chart uses to decide between the
 * real series and the "no traffic yet" empty state — there is no synthetic
 * fallback path.
 */

import type { WorkspaceData } from "@/store/socket";
import { WORKSPACE_KIND } from "@/lib/workspace-kind";

/** The selectable A2A-traffic windows — must match the Go `a2aWindows` keys. */
export type MonitorWindow = "1h" | "24h" | "7d" | "30d";

/** Window toggle options rendered by the chart (value → button label). */
export const MONITOR_WINDOWS: ReadonlyArray<{ value: MonitorWindow; label: string }> = [
  { value: "1h", label: "1H" },
  { value: "24h", label: "24H" },
  { value: "7d", label: "7D" },
  { value: "30d", label: "30D" },
];

/** One point on the A2A traffic series. `ts` is the bucket START (ISO, UTC). */
export interface A2ATrafficBucket {
  ts: string;
  count: number;
}

/** GET /monitor/a2a-traffic response (mirrors the Go gin.H shape). */
export interface A2ATrafficResponse {
  window: string;
  bucket_seconds: number;
  buckets: A2ATrafficBucket[];
  rps_now: number;
  rps_peak: number;
  rps_peak_at: string | null;
  total: number;
}

/** GET /monitor/topology-summary response. */
export interface TopologySummary {
  total: number;
  agents: number;
  teams: number;
  platform: number;
}

/** True iff the window has any real A2A traffic. The chart shows the honest
 *  empty state (NOT a fabricated curve) whenever this is false. */
export function hasTraffic(resp: A2ATrafficResponse | null): boolean {
  return !!resp && resp.total > 0;
}

/** Largest bucket count in the series, floored at 1 so an all-zero series
 *  scales to a flat baseline instead of dividing by zero. */
export function maxBucketCount(buckets: A2ATrafficBucket[]): number {
  let m = 0;
  for (const b of buckets) if (b.count > m) m = b.count;
  return Math.max(1, m);
}

/**
 * SVG polyline points for the traffic sparkline, mapped into a [0,width] ×
 * [0,height] box with y inverted (taller bar = higher count). An empty or
 * all-zero series produces a flat line along the bottom — the visual truth of
 * "no traffic", never an invented shape.
 */
export function sparklinePoints(
  buckets: A2ATrafficBucket[],
  width: number,
  height: number,
): string {
  if (buckets.length === 0) return "";
  const max = maxBucketCount(buckets);
  const n = buckets.length;
  return buckets
    .map((b, i) => {
      const x = n === 1 ? width / 2 : (i / (n - 1)) * width;
      const y = height - (b.count / max) * height;
      return `${x.toFixed(2)},${y.toFixed(2)}`;
    })
    .join(" ");
}

/** Format a requests-per-second value for display (2 dp). */
export function formatRps(n: number | null | undefined): string {
  if (n == null || !Number.isFinite(n)) return "0.00";
  return n.toFixed(2);
}

/** A node in the visual topology tree derived from the real workspace graph. */
export interface TopologyTreeNode {
  id: string;
  name: string;
  status: string;
  kind: string;
  role: string;
  isTeam: boolean;
  children: TopologyTreeNode[];
}

/**
 * Build a parent→children tree from the REAL workspace list (the same data
 * GET /workspaces serves). `isTeam` mirrors the Go TopologySummary definition:
 * a node is a team if it has children OR is kind=platform; otherwise it is an
 * agent leaf. No counts are fabricated — the tree is exactly the rows passed
 * in. Orphan rows (parent_id points outside the set) are surfaced as roots so
 * nothing is silently dropped.
 */
export function buildTopologyTree(workspaces: WorkspaceData[]): TopologyTreeNode[] {
  const byId = new Map(workspaces.map((w) => [w.id, w]));
  const childrenOf = new Map<string, WorkspaceData[]>();
  for (const w of workspaces) {
    const pid = w.parent_id;
    if (pid && byId.has(pid)) {
      const arr = childrenOf.get(pid) ?? [];
      arr.push(w);
      childrenOf.set(pid, arr);
    }
  }

  const toNode = (w: WorkspaceData): TopologyTreeNode => {
    const kids = childrenOf.get(w.id) ?? [];
    const kind = w.kind ?? WORKSPACE_KIND.Workspace;
    const isTeam = kind === WORKSPACE_KIND.Platform || kids.length > 0;
    return {
      id: w.id,
      name: w.name,
      status: w.status,
      kind,
      role: w.role ?? "",
      isTeam,
      children: kids
        .slice()
        .sort((a, b) => a.name.localeCompare(b.name))
        .map(toNode),
    };
  };

  return workspaces
    .filter((w) => !w.parent_id || !byId.has(w.parent_id))
    .sort((a, b) => a.name.localeCompare(b.name))
    .map(toNode);
}
