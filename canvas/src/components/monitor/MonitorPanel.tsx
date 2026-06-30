"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { api } from "@/lib/api";
import type { WorkspaceData } from "@/store/socket";
import { useSocketEvent } from "@/hooks/useSocketEvent";
import { RequestsInbox } from "@/components/concierge/RequestsInbox";
import { IcQueue, IcOrgMap, IcBell, IcClock } from "@/components/concierge/icons";
import {
  MONITOR_WINDOWS,
  type MonitorWindow,
  type A2ATrafficResponse,
  type TopologySummary,
  type TopologyTreeNode,
  hasTraffic,
  sparklinePoints,
  formatRps,
  buildTopologyTree,
} from "@/lib/monitor";
import s from "./Monitor.module.css";

/* The chart is drawn in a fixed 0..W × 0..H viewBox and stretched to the
   container width via preserveAspectRatio="none". */
const CHART_W = 600;
const CHART_H = 140;

/** Concept-palette status colour for the topology dots (mirrors the shell). */
function statusColor(status: string): string {
  switch (status) {
    case "online":
      return "var(--green)";
    case "provisioning":
    case "starting":
    case "building":
    case "degraded":
      return "var(--amber)";
    case "failed":
      return "var(--red)";
    default:
      return "var(--grey)";
  }
}

/**
 * MonitorPanel — the OSS org-dashboard monitoring surface. Three panels, all
 * fed by REAL core APIs (no mock, no CP import):
 *
 *   (a) Live A2A traffic from GET /monitor/a2a-traffic with a 1H/24H/7D/30D
 *       window toggle and an honest empty-state (pre-customer volume is
 *       near-zero, so a zero series renders as exactly that — never a faked
 *       curve).
 *   (b) Topology — real agent/team counts from GET /monitor/topology-summary
 *       plus the live parent/child graph from GET /workspaces.
 *   (c) HITL queue — the existing RequestsInbox (Tasks + Approvals) with its
 *       working approve/reject/resolve buttons, reused verbatim.
 *
 * Live updates ride the shared WS bus: ACTIVITY_LOGGED refreshes the traffic
 * series + topology; the embedded RequestsInbox subscribes to REQUEST_* itself.
 */
export function MonitorPanel() {
  const [window, setWindow] = useState<MonitorWindow>("24h");
  const [traffic, setTraffic] = useState<A2ATrafficResponse | null>(null);
  const [trafficLoading, setTrafficLoading] = useState(true);
  const [summary, setSummary] = useState<TopologySummary | null>(null);
  const [tree, setTree] = useState<TopologyTreeNode[]>([]);
  const [hitlTab, setHitlTab] = useState<"task" | "approval">("task");
  const [taskCount, setTaskCount] = useState(0);
  const [apprCount, setApprCount] = useState(0);

  // Generation guard so a slow response for a previous window can't overwrite
  // the current one after a fast toggle.
  const trafficGenRef = useRef(0);

  const loadTraffic = useCallback((w: MonitorWindow) => {
    const gen = ++trafficGenRef.current;
    setTrafficLoading(true);
    api
      .get<A2ATrafficResponse>(`/monitor/a2a-traffic?window=${w}`)
      .then((r) => {
        if (gen !== trafficGenRef.current) return;
        setTraffic(r);
        setTrafficLoading(false);
      })
      .catch(() => {
        if (gen !== trafficGenRef.current) return;
        // On error (e.g. no admin token in a bare dev shell) show the honest
        // empty state rather than a fabricated series.
        setTraffic(null);
        setTrafficLoading(false);
      });
  }, []);

  const loadTopology = useCallback(() => {
    api
      .get<TopologySummary>(`/monitor/topology-summary`)
      .then((r) => setSummary(r))
      .catch(() => setSummary(null));
    api
      .get<WorkspaceData[]>(`/workspaces`)
      .then((r) => setTree(buildTopologyTree(r ?? [])))
      .catch(() => setTree([]));
  }, []);

  useEffect(() => {
    loadTraffic(window);
  }, [window, loadTraffic]);

  useEffect(() => {
    loadTopology();
  }, [loadTopology]);

  // Live refresh on the shared WS bus. ACTIVITY_LOGGED → new A2A row, so
  // refresh the current window; WORKSPACE_* → topology changed. Debounced so a
  // burst of events triggers at most one refetch per 800ms.
  const debounceRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  useSocketEvent((msg) => {
    const isActivity = msg.event === "ACTIVITY_LOGGED";
    const isTopology = msg.event.startsWith("WORKSPACE_");
    if (!isActivity && !isTopology) return;
    if (debounceRef.current) clearTimeout(debounceRef.current);
    debounceRef.current = setTimeout(() => {
      if (isActivity) loadTraffic(window);
      loadTopology();
    }, 800);
  });
  useEffect(() => () => {
    if (debounceRef.current) clearTimeout(debounceRef.current);
  }, []);

  const points = traffic ? sparklinePoints(traffic.buckets, CHART_W, CHART_H) : "";
  const showEmpty = !trafficLoading && !hasTraffic(traffic);

  return (
    <div className={s.grid}>
      {/* (a) LIVE A2A TRAFFIC */}
      <section className={s.card} data-testid="monitor-traffic">
        <div className={s.cardHead}>
          <div className={s.cardTitle}>
            <IcQueue />
            Live A2A traffic
          </div>
          <div className={s.toggle} role="tablist" aria-label="Traffic window">
            {MONITOR_WINDOWS.map((w) => (
              <button
                key={w.value}
                type="button"
                role="tab"
                aria-selected={window === w.value}
                data-testid={`window-${w.value}`}
                className={`${s.toggleBtn} ${window === w.value ? s.active : ""}`}
                onClick={() => setWindow(w.value)}
              >
                {w.label}
              </button>
            ))}
          </div>
        </div>

        <div className={s.stats}>
          <div className={s.stat}>
            <span className={s.statVal} data-testid="traffic-total">{traffic?.total ?? 0}</span>
            <span className={s.statLbl}>messages</span>
          </div>
          <div className={s.stat}>
            <span className={s.statVal}>{formatRps(traffic?.rps_now)}</span>
            <span className={s.statLbl}>req/s now</span>
          </div>
          <div className={s.stat}>
            <span className={s.statVal}>{formatRps(traffic?.rps_peak)}</span>
            <span className={s.statLbl}>req/s peak</span>
          </div>
        </div>

        <div className={s.chartWrap}>
          <svg
            className={s.chart}
            viewBox={`0 0 ${CHART_W} ${CHART_H}`}
            preserveAspectRatio="none"
            data-testid="traffic-chart"
            aria-hidden="true"
          >
            <line x1="0" y1={CHART_H - 0.5} x2={CHART_W} y2={CHART_H - 0.5} className={s.chartBase} />
            {points && (
              <polygon className={s.chartArea} points={`0,${CHART_H} ${points} ${CHART_W},${CHART_H}`} />
            )}
            {points && <polyline className={s.chartLine} points={points} />}
          </svg>
          {showEmpty && (
            <div className={s.emptyOverlay}>
              <span className={s.emptyPill} data-testid="traffic-empty">
                No agent-to-agent traffic yet
              </span>
            </div>
          )}
        </div>
      </section>

      <div className={s.row2}>
        {/* (b) TOPOLOGY */}
        <section className={s.card} data-testid="monitor-topology">
          <div className={s.cardHead}>
            <div className={s.cardTitle}>
              <IcOrgMap />
              Topology
            </div>
            <span className={s.cardSub}>{summary?.total ?? 0} workspaces</span>
          </div>

          <div className={s.counts}>
            <div className={s.countCard}>
              <div className={s.countVal} data-testid="count-agents">{summary?.agents ?? 0}</div>
              <div className={s.countLbl}>Agents</div>
            </div>
            <div className={s.countCard}>
              <div className={s.countVal} data-testid="count-teams">{summary?.teams ?? 0}</div>
              <div className={s.countLbl}>Teams</div>
            </div>
          </div>

          {tree.length === 0 ? (
            <div className={s.empty} data-testid="topology-empty">
              No workspaces yet. Ask the concierge to spin up a team and it appears here.
            </div>
          ) : (
            <div className={s.tree} data-testid="topology-tree">
              {tree.map((n) => (
                <TopologyRow key={n.id} node={n} depth={0} />
              ))}
            </div>
          )}
        </section>

        {/* (c) HITL QUEUE — reuses RequestsInbox verbatim */}
        <section className={s.card} data-testid="monitor-hitl">
          <div className={s.cardHead}>
            <div className={s.cardTitle}>
              {hitlTab === "task" ? <IcClock /> : <IcBell />}
              Human-in-the-loop
            </div>
            <div className={s.hitlTabs} role="tablist" aria-label="HITL queue">
              <button
                type="button"
                role="tab"
                aria-selected={hitlTab === "task"}
                data-testid="hitl-tab-task"
                className={`${s.hitlTab} ${hitlTab === "task" ? s.active : ""}`}
                onClick={() => setHitlTab("task")}
              >
                Tasks{taskCount > 0 && <span className={s.hitlCount}>{taskCount}</span>}
              </button>
              <button
                type="button"
                role="tab"
                aria-selected={hitlTab === "approval"}
                data-testid="hitl-tab-approval"
                className={`${s.hitlTab} ${hitlTab === "approval" ? s.active : ""}`}
                onClick={() => setHitlTab("approval")}
              >
                Approvals{apprCount > 0 && <span className={s.hitlCount}>{apprCount}</span>}
              </button>
            </div>
          </div>

          {/* Both inboxes stay mounted so their pending-count badges stay live;
              only the active one is visible. Each owns its fetch + optimistic
              update + WS refresh + working action buttons. */}
          <div className={s.hitlBody}>
            <div style={{ display: hitlTab === "task" ? "block" : "none" }}>
              <RequestsInbox kind="task" onCountChange={setTaskCount} />
            </div>
            <div style={{ display: hitlTab === "approval" ? "block" : "none" }}>
              <RequestsInbox kind="approval" onCountChange={setApprCount} />
            </div>
          </div>
        </section>
      </div>
    </div>
  );
}

/** One row of the real topology tree. Teams get a kind badge; agents a muted
 *  pill; both show a status dot. Children render indented recursively. */
function TopologyRow({ node, depth }: { node: TopologyTreeNode; depth: number }) {
  return (
    <>
      <div className={s.treeNode} style={{ paddingLeft: 8 + depth * 16 }} data-testid="topology-node">
        <span className={s.tdot} style={{ background: statusColor(node.status) }} />
        <span className={s.tname}>{node.name}</span>
        {node.isTeam ? (
          <span className={s.tbadge}>{node.kind === "platform" ? "org" : "team"}</span>
        ) : (
          <span className={s.tagent}>agent</span>
        )}
        {node.role && <span className={s.trole}>{node.role}</span>}
      </div>
      {node.children.map((c) => (
        <TopologyRow key={c.id} node={c} depth={depth + 1} />
      ))}
    </>
  );
}
