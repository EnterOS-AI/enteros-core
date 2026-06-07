"use client";

import { useMemo, useState } from "react";
import { useCanvasStore, type TopView } from "@/store/canvas";
import { useTheme } from "@/lib/theme-provider";
import { Canvas } from "@/components/Canvas";
import { Legend } from "@/components/Legend";
import { CommunicationOverlay } from "@/components/CommunicationOverlay";
import s from "./Concierge.module.css";
import {
  IcHome, IcOrgMap, IcSettings, IcSearch, IcBell, IcSun, IcMoon, IcChevDown,
  IcQueue, IcCaret, IcMolecule, IcCheck, IcSchedule, IcWorkspace, IcWarn,
  IcSend, IcHistory, IcDots, IcClock, IcTrash, IcChat,
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

export function ConciergeShell() {
  const nodes = useCanvasStore((st) => st.nodes);
  const topView = useCanvasStore((st) => st.topView);
  const setTopView = useCanvasStore((st) => st.setTopView);
  const selectNode = useCanvasStore((st) => st.selectNode);
  const selectedNodeId = useCanvasStore((st) => st.selectedNodeId);
  const { resolvedTheme, setTheme } = useTheme();

  const [railOpen, setRailOpen] = useState(false);
  const [sbTab, setSbTab] = useState<SbTab>("agents");
  const [collapsed, setCollapsed] = useState<Record<string, boolean>>({});

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
    () => roots.find((r) => /concierge|platform/i.test(`${r.data.role ?? ""} ${r.data.name ?? ""}`)) ?? roots[0] ?? null,
    [roots],
  );

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
          <span className={s.rootTag}>root</span>
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
          <button className={`${s.navbtn} ${topView === "home" ? s.active : ""}`} title="Home" onClick={() => nav("home")}>
            <span className={s.ico}><IcHome /></span><span className={s.lbl}>Home</span>
          </button>
          <button className={`${s.navbtn} ${topView === "map" ? s.active : ""}`} title="Org map" onClick={() => nav("map")}>
            <span className={s.ico}><IcOrgMap /></span><span className={s.lbl}>Org map</span>
          </button>
          <div className={s.spacer} />
          <button className={`${s.navbtn} ${topView === "settings" ? s.active : ""}`} title="Settings" onClick={() => nav("settings")}>
            <span className={s.ico}><IcSettings /></span><span className={s.lbl}>Settings</span>
          </button>
        </nav>

        <div className={s.main}>
          {/* TOPBAR */}
          <header className={s.topbar}>
            <div className={s.org}>
              <div className={s.orgBadge}>M</div>
              <span className={s.orgName}>Molecule AI</span>
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
                  <button className={`${s.sbTab} ${sbTab === "agents" ? s.active : ""}`} onClick={() => setSbTab("agents")}>Agents</button>
                  <button className={`${s.sbTab} ${sbTab === "tasks" ? s.active : ""}`} onClick={() => setSbTab("tasks")}>
                    Tasks<span className={s.cnt}>2</span>
                  </button>
                  <button className={`${s.sbTab} ${sbTab === "approvals" ? s.active : ""}`} onClick={() => setSbTab("approvals")}>
                    Approvals<span className={s.cnt}>1</span>
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
                        <div className={s.act}><span className={s.actTime}>9:05 PM</span><div className={`${s.actLine} ${s.grn}`}><div className={s.actText}><b>SEO Researcher</b> published draft</div></div></div>
                        <div className={s.act}><span className={s.actTime}>9:02 PM</span><div className={s.actLine}><div className={s.actText}>Delegated task to <b>Backend Engineer</b></div></div></div>
                        <div className={s.act}><span className={s.actTime}>8:58 PM</span><div className={`${s.actLine} ${s.grn}`}><div className={s.actText}>Scheduled <b>weekly publish</b></div></div></div>
                        <div className={s.act}><span className={s.actTime}>8:54 PM</span><div className={s.actLine}><div className={s.actText}>Created workspace <b>SEO Researcher</b></div></div></div>
                      </div>
                    </>
                  )}
                  {sbTab === "tasks" && (
                    <>
                      <div className={s.task}>
                        <div className={s.taskRow}>
                          <div className={`${s.taskIc} ${s.run}`}><IcClock /></div>
                          <div className={s.taskMeta}><div className={s.taskT}>Build weekly publish pipeline</div><div className={s.taskS}>Backend Engineer<span className={s.pip} />in progress</div></div>
                        </div>
                      </div>
                      <div className={s.task}>
                        <div className={s.taskRow}>
                          <div className={`${s.taskIc} ${s.sched}`}><IcSchedule /></div>
                          <div className={s.taskMeta}><div className={s.taskT}>Weekly publish · Mondays 9am</div><div className={s.taskS}>SEO Researcher<span className={s.pip} />scheduled</div></div>
                        </div>
                      </div>
                      <div className={`${s.task} ${s.isDone}`}>
                        <div className={s.taskRow}>
                          <div className={`${s.taskIc} ${s.done}`}><IcCheck /></div>
                          <div className={s.taskMeta}><div className={s.taskT}>Draft launch post: 10 SEO tips</div><div className={s.taskS}>SEO Researcher<span className={s.pip} />done</div></div>
                        </div>
                      </div>
                    </>
                  )}
                  {sbTab === "approvals" && (
                    <div className={s.apprCard}>
                      <div className={s.apprRow}>
                        <div className={s.apprIc}><IcTrash /></div>
                        <div className={s.apprMeta}><div className={s.apprT}>Delete workspace <code>old-test</code></div><div className={s.apprS}>Requested by PM · destructive</div></div>
                      </div>
                      <div className={s.apprActions}>
                        <button className={`${s.btn} ${s.approve} ${s.flex}`}>Approve</button>
                        <button className={`${s.btn} ${s.deny} ${s.flex}`}>Deny</button>
                      </div>
                    </div>
                  )}
                </div>
              </aside>

              {/* CHAT */}
              <section className={s.chat}>
                <div className={s.chatHead}>
                  <div className={s.chAv}><IcChat /></div>
                  <div className={s.chMeta}>
                    <div className={s.chTitle}>{platformRoot?.data.name ?? "Org Concierge"}</div>
                    <div className={s.chSub}><span className={s.sdot} />online · platform agent</div>
                  </div>
                  <div className={s.chTools}>
                    <button className={s.iconPill} title="History"><IcHistory /></button>
                    <button className={s.iconPill} title="More"><IcDots /></button>
                  </div>
                </div>
                <div className={s.chatScroll}>
                  <div className={s.chatInner}>
                    <div className={`${s.msg} ${s.user}`}>
                      <div className={s.msgAv}>HW</div>
                      <div className={s.bubbleWrap}><div className={s.bubble}>Spin up an SEO team that publishes a blog post weekly</div></div>
                    </div>
                    <div className={`${s.msg} ${s.bot}`}>
                      <div className={s.msgAv}><IcChat /></div>
                      <div className={s.bubbleWrap}>
                        <div className={s.bubble}>On it. I&apos;ve created a dedicated <b>SEO Researcher</b> workspace and set up a recurring publish schedule so a fresh post ships every week — no manual nudging needed.</div>
                        <div className={s.actionCard}>
                          <div className={s.acIc}><IcWorkspace /></div>
                          <div className={s.acMeta}><div className={s.acLabel}>Action · workspace</div><div className={s.acTitle}>Created workspace <span className={s.pill}>SEO Researcher</span></div></div>
                          <div className={s.acCheck}><IcCheck /></div>
                        </div>
                        <div className={s.actionCard}>
                          <div className={s.acIc}><IcSchedule /></div>
                          <div className={s.acMeta}><div className={s.acLabel}>Action · schedule</div><div className={s.acTitle}>Scheduled <span className={s.pill}>weekly publish · Mon 9am</span></div></div>
                          <div className={s.acCheck}><IcCheck /></div>
                        </div>
                        <div className={s.bubble}>One thing needs your sign-off before I continue — the leftover <b>old-test</b> workspace overlaps with the new team and should be removed.</div>
                        <div className={s.reqCard}>
                          <div className={s.reqTop}>
                            <div className={s.reqIc}><IcWarn /></div>
                            <div className={s.reqMeta}>
                              <div className={s.reqLabel}>Approval required</div>
                              <div className={s.reqTitle}>Approve destructive action: delete workspace <code>old-test</code>?</div>
                              <div className={s.reqDesc}>This permanently removes the workspace, its agents and run history. This cannot be undone.</div>
                            </div>
                          </div>
                          <div className={s.reqActions}>
                            <button className={`${s.btn} ${s.approve}`}>Approve</button>
                            <button className={`${s.btn} ${s.deny}`}>Deny</button>
                          </div>
                        </div>
                      </div>
                    </div>
                  </div>
                </div>
                <div className={s.composer}>
                  <div className={s.composerInner}>
                    <div className={s.inputBox}>
                      <div className={s.inputTop}>
                        <textarea className={s.msgInput} rows={1} placeholder="Message your concierge" />
                        <button className={s.send} title="Send"><IcSend /></button>
                      </div>
                      <div className={s.inputBottom}>
                        <span className={s.hint}><kbd>↵</kbd>&nbsp;send</span>
                      </div>
                    </div>
                  </div>
                </div>
              </section>
            </div>

            {/* ORG MAP VIEW — the live canvas */}
            <div className={`${s.view} ${topView === "map" ? s.active : ""}`}>
              {topView === "map" && (
                <div className={s.canvasMount}>
                  <main aria-label="Agent canvas" style={{ position: "absolute", inset: 0 }}>
                    <Canvas />
                  </main>
                  <Legend />
                  <CommunicationOverlay />
                </div>
              )}
            </div>

            {/* SETTINGS VIEW */}
            <div className={`${s.view} ${topView === "settings" ? s.active : ""}`}>
              <div className={s.ph}>
                <IcSettings />
                <h2>Settings</h2>
                <p>Org-level settings live here. For now, per-workspace config stays on the Org map (select a node).</p>
              </div>
            </div>
          </div>
        </div>
      </div>
    </div>
  );
}
