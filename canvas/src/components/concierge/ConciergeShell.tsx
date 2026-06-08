"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import { useCanvasStore, type TopView } from "@/store/canvas";
import { WORKSPACE_KIND } from "@/lib/workspace-kind";
import { useTheme } from "@/lib/theme-provider";
import { api } from "@/lib/api";
import { showToast } from "@/components/Toaster";
import type { ActivityEntry } from "@/types/activity";
import { Canvas } from "@/components/Canvas";
import { CommunicationOverlay } from "@/components/CommunicationOverlay";
import { ChatTab } from "@/components/tabs/ChatTab";
import { WorkspacePanelTabs } from "@/components/WorkspacePanelTabs";
import { SettingsTabs } from "@/components/settings";
import s from "./Concierge.module.css";
import {
  IcHome, IcOrgMap, IcSettings, IcSearch, IcBell, IcSun, IcMoon, IcChevDown,
  IcQueue, IcCaret, IcMolecule, IcClock, IcCheck, IcTrash, IcChat,
} from "./icons";

/* ── status → concept palette ─────────────────────────────────────────── */
function statusInfo(status: string): { color: string; label: string } {
  switch (status) {
    case "online": return { color: "var(--green)", label: "online" };
    case "provisioning":
    case "starting": return { color: "var(--amber)", label: "starting" };
    case "degraded": return { color: "var(--amber)", label: "degraded" };
    case "building": return { color: "var(--amber)", label: "building" };
    case "failed": return { color: "var(--red)", label: "failed" };
    case "paused": return { color: "var(--accent-2)", label: "paused" };
    default: return { color: "var(--grey)", label: status || "idle" };
  }
}

const AV_GRADIENTS = [
  "linear-gradient(150deg,#a78bfa,#7c3aed)",
  "linear-gradient(150deg,#60a5fa,#3b82f6)",
  "linear-gradient(150deg,#34d399,#10b981)",
  "linear-gradient(150deg,#fbbf77,#f59e0b)",
  "linear-gradient(150deg,#5eead4,#14b8a6)",
  "linear-gradient(150deg,#f0a36b,#e8638a)",
];
function initials(name: string): string {
  const parts = name.trim().split(/\s+/).filter(Boolean);
  if (parts.length === 0) return "?";
  if (parts.length === 1) return parts[0].slice(0, 2).toUpperCase();
  return (parts[0][0] + parts[parts.length - 1][0]).toUpperCase();
}
function gradientFor(id: string): string {
  let h = 0;
  for (let i = 0; i < id.length; i++) h = (h * 31 + id.charCodeAt(i)) >>> 0;
  return AV_GRADIENTS[h % AV_GRADIENTS.length];
}

type SbTab = "agents" | "tasks" | "approvals";

interface PendingApproval {
  id: string;
  workspace_id: string;
  workspace_name: string;
  action: string;
  reason: string | null;
  status: string;
  created_at: string;
}
interface UserTask {
  id: string;
  workspace_id: string;
  workspace_name: string;
  title: string;
  detail: string | null;
  status: string;
  created_at: string;
}

/** ISO timestamp → "9:05 PM" (local). Empty string on a bad/missing value. */
function clockTime(iso: string | null | undefined): string {
  if (!iso) return "";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  return d.toLocaleTimeString([], { hour: "numeric", minute: "2-digit" });
}

/** A human action label from an activity row. */
function activityText(a: ActivityEntry): string {
  if (a.summary) return a.summary;
  const verb = a.activity_type?.replace(/_/g, " ") ?? "activity";
  return a.method ? `${verb} · ${a.method}` : verb;
}

export function ConciergeShell() {
  const nodes = useCanvasStore((st) => st.nodes);
  const topView = useCanvasStore((st) => st.topView);
  const setTopView = useCanvasStore((st) => st.setTopView);
  const selectNode = useCanvasStore((st) => st.selectNode);
  const selectedNodeId = useCanvasStore((st) => st.selectedNodeId);
  const { resolvedTheme, setTheme } = useTheme();

  const [railOpen, setRailOpen] = useState(false);
  const [sbTab, setSbTab] = useState<SbTab>("agents");
  const [settingsTab, setSettingsTab] = useState<"platform" | "org">("platform");
  const [collapsed, setCollapsed] = useState<Record<string, boolean>>({});

  // Dynamic org name for the topbar. Sourced from GET /org/identity
  // ({name} ← MOLECULE_ORG_NAME, added by a parallel backend change).
  // Falls back to "Molecule AI" when the endpoint 404s / errors or
  // returns an empty name, so the topbar never breaks before the backend
  // lands.
  const [orgName, setOrgName] = useState("Molecule AI");
  useEffect(() => {
    let cancelled = false;
    api
      .get<{ name?: string }>("/org/identity")
      .then((r) => {
        const name = (r?.name || "").trim();
        if (!cancelled && name) setOrgName(name);
      })
      .catch(() => {
        // No endpoint / not reachable — keep the "Molecule AI" fallback.
      });
    return () => {
      cancelled = true;
    };
  }, []);

  // Build the agent hierarchy from live nodes.
  const { roots, childrenOf } = useMemo(() => {
    const childrenOf = new Map<string, typeof nodes>();
    const roots: typeof nodes = [];
    for (const n of nodes) {
      const p = n.data.parentId;
      if (p) {
        const arr = childrenOf.get(p) ?? [];
        arr.push(n);
        childrenOf.set(p, arr);
      } else {
        roots.push(n);
      }
    }
    return { roots, childrenOf };
  }, [nodes]);

  const platformRoot = useMemo(
    () =>
      // Resolve the platform agent by the authoritative kind='platform' marker
      // only — the backend in this branch always returns kind
      // (COALESCE(w.kind,'workspace')) and the map-side filter
      // (canvas-topology/Canvas/Toolbar) is kind-only, so the shell must not
      // disagree via a name/role heuristic. Fall back to the first root only as
      // graceful degradation if no node is tagged platform.
      roots.find((r) => r.data.kind === WORKSPACE_KIND.Platform) ??
      roots[0] ??
      null,
    [roots],
  );

  const platformId = platformRoot?.id ?? null;

  // ── live data: approvals + user-tasks (org-wide), activity (platform agent) ──
  const [approvals, setApprovals] = useState<PendingApproval[]>([]);
  const [userTasks, setUserTasks] = useState<UserTask[]>([]);
  const [activity, setActivity] = useState<ActivityEntry[]>([]);
  const [deciding, setDeciding] = useState<string | null>(null);
  const [resolving, setResolving] = useState<string | null>(null);

  const loadApprovals = useCallback(() => {
    api.get<PendingApproval[]>("/approvals/pending")
      .then((r) => setApprovals(r ?? []))
      .catch(() => setApprovals([]));
  }, []);
  const loadUserTasks = useCallback(() => {
    api.get<UserTask[]>("/user-tasks/pending")
      .then((r) => setUserTasks(r ?? []))
      .catch(() => setUserTasks([]));
  }, []);
  useEffect(() => { loadApprovals(); loadUserTasks(); }, [loadApprovals, loadUserTasks]);

  useEffect(() => {
    if (!platformId) return;
    let cancelled = false;
    api.get<ActivityEntry[]>(`/workspaces/${platformId}/activity?limit=12`)
      .then((r) => { if (!cancelled) setActivity(r ?? []); })
      .catch(() => { if (!cancelled) setActivity([]); });
    return () => { cancelled = true; };
  }, [platformId]);

  const decide = useCallback(async (a: PendingApproval, decision: "approved" | "denied") => {
    if (deciding) return;
    setDeciding(a.id);
    try {
      await api.post(`/workspaces/${a.workspace_id}/approvals/${a.id}/decide`, {
        decision, decided_by: "human",
      });
      showToast(decision === "approved" ? "Approved" : "Denied", decision === "approved" ? "success" : "info");
      setApprovals((prev) => prev.filter((x) => x.id !== a.id));
    } catch {
      showToast("Failed to record decision", "error");
    } finally {
      setDeciding(null);
    }
  }, [deciding]);

  const resolveTask = useCallback(async (t: UserTask, status: "done" | "dismissed") => {
    if (resolving) return;
    setResolving(t.id);
    try {
      await api.post(`/workspaces/${t.workspace_id}/user-tasks/${t.id}/resolve`, {
        status, resolved_by: "human",
      });
      showToast(status === "done" ? "Marked done" : "Dismissed", status === "done" ? "success" : "info");
      setUserTasks((prev) => prev.filter((x) => x.id !== t.id));
    } catch {
      showToast("Failed to resolve task", "error");
    } finally {
      setResolving(null);
    }
  }, [resolving]);

  const nav = (v: TopView) => setTopView(v);

  /* ── agents tree (recursive) ──────────────────────────────────────── */
  function renderNode(n: (typeof nodes)[number], depth: number) {
    const kids = childrenOf.get(n.id) ?? [];
    const hasKids = kids.length > 0;
    const isCollapsed = collapsed[n.id];
    const st = statusInfo(n.data.status);
    const isRoot = depth === 0;
    const isPlatform = n.id === platformRoot?.id;
    const q = (n.data.activeTasks as number) ?? 0;
    const row = (
      <div
        role="button"
        tabIndex={0}
        data-testid="agent-tree-node"
        data-node-name={n.data.name}
        data-platform={isPlatform ? "true" : "false"}
        data-depth={depth}
        className={`${s.ws} ${selectedNodeId === n.id ? s.active : ""}`}
        onClick={() => selectNode(n.id)}
        onKeyDown={(e) => {
          if (e.key === "Enter" || e.key === " ") {
            e.preventDefault();
            selectNode(n.id);
          }
        }}
      >
        <div className={s.wsAv} style={{ background: gradientFor(n.id) }}>
          {initials(n.data.name)}
          <span className={s.dot} style={{ background: st.color }} />
        </div>
        <div className={s.wsMeta}>
          <div className={s.wsName}>{n.data.name}</div>
          <div className={s.wsSub}>
            <span className={s.wsRole}>{isPlatform ? "platform" : n.data.role || "agent"}</span>
            <span className={s.wsStatus} style={{ color: st.color }}>
              <span className={s.sdot} style={{ background: st.color }} />
              {st.label}
            </span>
          </div>
        </div>
        {isRoot && isPlatform ? (
          <span data-testid="agent-tree-root-tag" className={s.rootTag}>root</span>
        ) : (
          <span className={`${s.wsQ} ${q === 0 ? s.zero : ""}`} title="Tasks in queue">
            <IcQueue />
            {q}
          </span>
        )}
        {hasKids && (
          <button
            className={s.wsCaret}
            title="Expand / collapse"
            onClick={(e) => {
              e.stopPropagation();
              setCollapsed((c) => ({ ...c, [n.id]: !c[n.id] }));
            }}
            style={{ transform: isCollapsed ? "none" : "rotate(90deg)", transition: "transform .18s" }}
          >
            <IcCaret />
          </button>
        )}
      </div>
    );
    return (
      <div key={n.id} className={s.tnode}>
        {row}
        {hasKids && !isCollapsed && (
          <div className={s.treeChildren}>
            {kids.map((k) => renderNode(k, depth + 1))}
          </div>
        )}
      </div>
    );
  }

  return (
    <div className={s.root}>
      <div className={`${s.app} ${railOpen ? s.railOpen : ""}`}>
        {/* ICON RAIL */}
        <nav className={s.rail}>
          <div className={s.railTop}>
            <div className={s.logo} title="Toggle sidebar" onClick={() => setRailOpen((o) => !o)}>
              <IcMolecule />
            </div>
            <span className={s.railWordmark}>Molecule</span>
            <button className={s.railToggle} title="Collapse sidebar" onClick={() => setRailOpen((o) => !o)}>
              <IcOrgMap />
            </button>
          </div>
          <button data-testid="nav-home" className={`${s.navbtn} ${topView === "home" ? s.active : ""}`} title="Home" onClick={() => nav("home")}>
            <span className={s.ico}><IcHome /></span><span className={s.lbl}>Home</span>
          </button>
          <button data-testid="nav-map" className={`${s.navbtn} ${topView === "map" ? s.active : ""}`} title="Org map" onClick={() => nav("map")}>
            <span className={s.ico}><IcOrgMap /></span><span className={s.lbl}>Org map</span>
          </button>
          <div className={s.spacer} />
          <button data-testid="nav-settings" className={`${s.navbtn} ${topView === "settings" ? s.active : ""}`} title="Settings" onClick={() => nav("settings")}>
            <span className={s.ico}><IcSettings /></span><span className={s.lbl}>Settings</span>
          </button>
        </nav>

        <div className={s.main}>
          {/* TOPBAR */}
          <header className={s.topbar}>
            <div className={s.org}>
              <div className={s.orgBadge}>{initials(orgName).slice(0, 1)}</div>
              <span data-testid="topbar-org-name" className={s.orgName}>{orgName}</span>
              <span className={s.chev}><IcChevDown /></span>
            </div>
            <div className={s.topbarRight}>
              <button className={s.iconPill} title="Search"><IcSearch /></button>
              <button className={s.iconPill} title="Notifications"><IcBell /></button>
              <button
                className={s.themeToggle}
                title="Toggle theme"
                onClick={() => setTheme(resolvedTheme === "dark" ? "light" : "dark")}
              >
                {resolvedTheme === "dark" ? <IcMoon /> : <IcSun />}
              </button>
              <div className={s.avatar} title="You">HW</div>
            </div>
          </header>

          <div className={s.viewArea}>
            {/* HOME VIEW */}
            <div className={`${s.view} ${topView === "home" ? s.active : ""}`}>
              <aside className={s.homeSidebar}>
                <div className={s.sbTabs}>
                  <button data-testid="home-subtab-agents" className={`${s.sbTab} ${sbTab === "agents" ? s.active : ""}`} onClick={() => setSbTab("agents")}>Agents</button>
                  <button data-testid="home-subtab-tasks" className={`${s.sbTab} ${sbTab === "tasks" ? s.active : ""}`} onClick={() => setSbTab("tasks")}>
                    Tasks{userTasks.length > 0 && <span className={s.cnt}>{userTasks.length}</span>}
                  </button>
                  <button data-testid="home-subtab-approvals" className={`${s.sbTab} ${sbTab === "approvals" ? s.active : ""}`} onClick={() => setSbTab("approvals")}>
                    Approvals{approvals.length > 0 && <span className={s.cnt}>{approvals.length}</span>}
                  </button>
                </div>
                <div className={s.sbBody}>
                  {sbTab === "agents" && (
                    <>
                      <div className={s.wsList}>
                        {roots.length === 0 && (
                          <div className={s.empty}>No agents yet. Ask the concierge to spin up a team.</div>
                        )}
                        {roots.map((r) => renderNode(r, 0))}
                      </div>
                      <div className={s.sbSection}>Recent activity</div>
                      <div>
                        {activity.length === 0 && (
                          <div className={s.empty}>No recent activity yet.</div>
                        )}
                        {activity.map((a) => {
                          const ok = a.status !== "error" && a.status !== "failed";
                          return (
                            <div key={a.id} className={s.act}>
                              <span className={s.actTime}>{clockTime(a.created_at)}</span>
                              <div className={`${s.actLine} ${ok ? s.grn : ""}`}>
                                <div className={s.actText}>{activityText(a)}</div>
                              </div>
                            </div>
                          );
                        })}
                      </div>
                    </>
                  )}
                  {sbTab === "tasks" && (
                    <>
                      {userTasks.length === 0 && (
                        <div className={s.empty}>Nothing needs you right now. When an agent needs you to do something, it shows up here.</div>
                      )}
                      {userTasks.map((t) => (
                        <div key={t.id} className={s.task}>
                          <div className={s.taskRow}>
                            <div className={`${s.taskIc} ${s.run}`}><IcClock /></div>
                            <div className={s.taskMeta}>
                              <div className={s.taskT}>{t.title}</div>
                              <div className={s.taskS}>
                                {t.workspace_name}<span className={s.pip} />asked {clockTime(t.created_at)}
                              </div>
                              {t.detail && (
                                <div style={{ fontSize: 12, color: "var(--tx-3)", marginTop: 6, lineHeight: 1.45 }}>
                                  {t.detail}
                                </div>
                              )}
                            </div>
                          </div>
                          <div className={s.taskActions}>
                            <button className={`${s.tbtn} ${s.done}`} disabled={resolving === t.id} onClick={() => resolveTask(t, "done")}>
                              <IcCheck />Done
                            </button>
                            <button className={s.tbtn} disabled={resolving === t.id} onClick={() => resolveTask(t, "dismissed")}>
                              Dismiss
                            </button>
                          </div>
                        </div>
                      ))}
                    </>
                  )}
                  {sbTab === "approvals" && (
                    <>
                      {approvals.length === 0 && (
                        <div className={s.empty}>No pending approvals. Destructive actions await sign-off here.</div>
                      )}
                      {approvals.map((a) => (
                        <div key={a.id} className={s.apprCard} style={{ marginBottom: 7 }}>
                          <div className={s.apprRow}>
                            <div className={s.apprIc}><IcTrash /></div>
                            <div className={s.apprMeta}>
                              <div className={s.apprT}>{a.action.replace(/_/g, " ")} <code>{a.workspace_name}</code></div>
                              <div className={s.apprS}>{a.reason || "destructive"}</div>
                            </div>
                          </div>
                          <div className={s.apprActions}>
                            <button className={`${s.btn} ${s.approve} ${s.flex}`} disabled={deciding === a.id} onClick={() => decide(a, "approved")}>
                              {deciding === a.id ? "…" : "Approve"}
                            </button>
                            <button className={`${s.btn} ${s.deny} ${s.flex}`} disabled={deciding === a.id} onClick={() => decide(a, "denied")}>
                              {deciding === a.id ? "…" : "Deny"}
                            </button>
                          </div>
                        </div>
                      ))}
                    </>
                  )}
                </div>
              </aside>

              {/* CHAT — reuses the EXACT canonical chat the Org-map SidePanel
                  renders (My Chat / Agent Comms sub-tabs, attachments, history,
                  delivery-mode handling), pointed at the platform agent. A thin
                  concierge-styled header keeps the Home look; the ChatTab body
                  below is identical to the map path so features can't drift. */}
              {platformId && platformRoot ? (
                <section className={s.chat}>
                  <div className={s.chatHead}>
                    <div className={s.chAv}><IcChat /></div>
                    <div className={s.chMeta}>
                      <div className={s.chTitle}>{platformRoot.data.name ?? "Org Concierge"}</div>
                      <div className={s.chSub}>
                        {(() => {
                          const online =
                            platformRoot.data.status === "online" ||
                            platformRoot.data.status === "degraded";
                          return (
                            <>
                              <span
                                className={s.sdot}
                                style={{ background: online ? "var(--green)" : "var(--grey)" }}
                              />
                              {online ? "online" : statusInfo(platformRoot.data.status ?? "").label} · platform agent
                            </>
                          );
                        })()}
                      </div>
                    </div>
                  </div>
                  <div className={s.embedChat}>
                    <ChatTab key={platformId} workspaceId={platformId} data={platformRoot.data} />
                  </div>
                </section>
              ) : (
                <section className={s.chat}>
                  <div className={s.greetWrap}>
                    <div className={s.greet}>
                      <span className={s.stamp}>✷</span> No platform agent yet
                    </div>
                  </div>
                </section>
              )}
            </div>

            {/* ORG MAP VIEW — the live canvas */}
            <div className={`${s.view} ${topView === "map" ? s.active : ""}`}>
              {topView === "map" && (
                <div className={s.canvasMount}>
                  <main aria-label="Agent canvas" style={{ position: "absolute", inset: 0 }}>
                    <Canvas />
                  </main>
                  <CommunicationOverlay />
                </div>
              )}
            </div>

            {/* SETTINGS VIEW */}
            <div className={`${s.view} ${topView === "settings" ? s.active : ""}`}>
              <div className={s.settingsScroll}>
                <div className={s.settingsInner}>
                  <div className={s.settingsHead}>
                    <h1>Settings</h1>
                    <p>
                      Org-level settings for the platform concierge. Configure the
                      concierge exactly like any workspace — config.yaml, plugins
                      and skills, container/compute, display, channels, schedule
                      and secrets — plus how it pays for model usage and org
                      identity.
                    </p>
                  </div>

                  {/* Two tabs instead of one long sheet: Platform agent
                      configuration vs Org & canvas settings. Reuses the same
                      .sbTabs purple-underline tab style as the Home sub-tabs. */}
                  <div className={s.sbTabs} role="tablist" aria-label="Settings sections">
                    <button
                      type="button"
                      role="tab"
                      data-testid="settings-tab-platform"
                      aria-selected={settingsTab === "platform"}
                      className={`${s.sbTab} ${settingsTab === "platform" ? s.active : ""}`}
                      onClick={() => setSettingsTab("platform")}
                    >
                      Platform agent configuration
                    </button>
                    <button
                      type="button"
                      role="tab"
                      data-testid="settings-tab-org"
                      aria-selected={settingsTab === "org"}
                      className={`${s.sbTab} ${settingsTab === "org" ? s.active : ""}`}
                      onClick={() => setSettingsTab("org")}
                    >
                      Org &amp; canvas settings
                    </button>
                  </div>

                  {/* Platform agent configuration — the FULL workspace tab UI
                      (Config, Plugins/Skills, Container, Display, Details,
                      Activity, Terminal, Channels, Schedule, Files, Memory,
                      Traces, Events, Audit), reusing the exact same
                      WorkspacePanelTabs the Org-map SidePanel renders so the two
                      surfaces can't drift. Pointed at the platform agent; the
                      panel owns its own local active-tab state so it doesn't
                      fight the map's node selection. */}
                  {settingsTab === "platform" && (
                    <div data-testid="settings-pane-platform" className={s.scard}>
                      <div className={s.scardHead}>
                        <div className={s.scardDesc}>
                          Update the concierge like any workspace: its config.yaml,
                          plugins &amp; skills, container/compute, display, channels,
                          schedule and more.
                        </div>
                      </div>
                      {platformRoot ? (
                        <div className={s.embedPanel}>
                          <WorkspacePanelTabs key={platformRoot.id} node={platformRoot} defaultTab="config" />
                        </div>
                      ) : (
                        <div className={s.scardDesc}>
                          No platform agent yet. Spin one up from Home to configure it.
                        </div>
                      )}
                    </div>
                  )}

                  {settingsTab === "org" && (
                    <div data-testid="settings-pane-org" className={s.scard}>
                      <div className={s.scardHead}>
                        <div className={s.scardDesc}>
                          Secrets, workspace tokens, org API keys and organization
                          identity. These also live behind the gear in the top bar.
                        </div>
                      </div>
                      {platformId && (
                        <div className={s.embedSettings}>
                          <SettingsTabs workspaceId={platformId} />
                        </div>
                      )}
                    </div>
                  )}
                </div>
              </div>
            </div>
          </div>
        </div>
      </div>
    </div>
  );
}
