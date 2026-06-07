"use client";

import { useEffect, useMemo, useState } from "react";
import { useCanvasStore } from "@/store/canvas";
import { api } from "@/lib/api";
import { ChatTab } from "@/components/tabs/ChatTab";
import { statusDotClass } from "@/lib/design-tokens";

/**
 * Org Concierge home — the chat-first view onto the org. The user talks to
 * the platform agent (the org-root, kind='platform' workspace) like a
 * chatbot and it orchestrates the org on their behalf. A left rail lists
 * the org's agents so the structure is visible alongside the conversation.
 *
 * This reuses the canvas's existing ChatTab (history + socket + send
 * plumbing) pointed at the platform agent, so it is a real conversation,
 * not a mock. The node-graph lives behind the "Map" tab.
 */
export function ConciergeHome() {
  const nodes = useCanvasStore((s) => s.nodes);
  const selectAgent = useCanvasStore((s) => s.selectNode);

  // The platform agent is the org root. Prefer the server resolver's id
  // (GET /registry/platform-agent), then fall back to a root-node
  // heuristic so the view still works on a stack without the resolver.
  const [resolvedId, setResolvedId] = useState<string | null>(null);
  useEffect(() => {
    let cancelled = false;
    api
      .get<{ id?: string; workspace_id?: string }>("/registry/platform-agent")
      .then((r) => {
        if (cancelled) return;
        setResolvedId(r?.id ?? r?.workspace_id ?? null);
      })
      .catch(() => {
        /* resolver absent on this stack — fall back to the root heuristic */
      });
    return () => {
      cancelled = true;
    };
  }, []);

  // Root candidates = top-level workspaces (no parent). Prefer a platform/
  // concierge-looking root, then a root that actually has children, then
  // simply the first root.
  const platformNode = useMemo(() => {
    if (resolvedId) {
      const byId = nodes.find((n) => n.id === resolvedId);
      if (byId) return byId;
    }
    const roots = nodes.filter((n) => !n.data.parentId);
    const looksPlatform = roots.find((n) =>
      /concierge|platform|org root/i.test(
        `${n.data.role ?? ""} ${n.data.name ?? ""}`,
      ),
    );
    if (looksPlatform) return looksPlatform;
    const withKids = roots.find((r) =>
      nodes.some((n) => n.data.parentId === r.id),
    );
    return withKids ?? roots[0] ?? null;
  }, [nodes, resolvedId]);

  // Agents rail — every workspace, roots first, each child indented one
  // level so the hierarchy reads at a glance.
  const railAgents = useMemo(() => {
    const depthOf = (id: string | null | undefined): number => {
      let d = 0;
      let cur = id;
      const byId = new Map(nodes.map((n) => [n.id, n] as const));
      while (cur) {
        const parent = byId.get(cur)?.data.parentId;
        if (!parent) break;
        d += 1;
        cur = parent;
      }
      return d;
    };
    return nodes
      .map((n) => ({ node: n, depth: depthOf(n.id) }))
      .sort((a, b) => {
        if (a.depth !== b.depth) return a.depth - b.depth;
        return (a.node.data.name ?? "").localeCompare(b.node.data.name ?? "");
      });
  }, [nodes]);

  return (
    <main
      aria-label="Org Concierge home"
      className="fixed inset-0 top-0 flex bg-surface text-ink pt-14"
    >
      {/* Left rail — Agents */}
      <aside className="flex w-[280px] shrink-0 flex-col border-r border-line/60 bg-surface-sunken/60">
        <div className="px-4 py-3 border-b border-line/50">
          <div className="text-[11px] font-mono uppercase tracking-[0.12em] text-ink-soft">
            Agents
          </div>
          <div className="mt-0.5 text-[12px] text-ink-mid">
            {railAgents.length} in this org
          </div>
        </div>
        <div className="flex-1 overflow-y-auto py-2">
          {railAgents.length === 0 && (
            <div className="px-4 py-6 text-[12px] text-ink-soft">
              No agents yet. Ask the concierge to spin up a team.
            </div>
          )}
          {railAgents.map(({ node, depth }) => {
            const isPlatform = node.id === platformNode?.id;
            return (
              <button
                key={node.id}
                onClick={() => selectAgent(node.id)}
                style={{ paddingLeft: 16 + depth * 14 }}
                className="group flex w-full items-center gap-2.5 px-4 py-2 text-left hover:bg-surface-card/60 transition-colors"
              >
                <span
                  className={`h-2 w-2 shrink-0 rounded-full ${statusDotClass(
                    node.data.status,
                  )}`}
                />
                <span className="min-w-0 flex-1">
                  <span className="block truncate text-[13px] text-ink">
                    {node.data.name}
                  </span>
                  {(node.data.role || isPlatform) && (
                    <span className="block truncate text-[10px] font-mono uppercase tracking-[0.04em] text-accent/80">
                      {isPlatform ? "PLATFORM · ROOT" : node.data.role}
                    </span>
                  )}
                </span>
              </button>
            );
          })}
        </div>
      </aside>

      {/* Center — concierge conversation */}
      <section className="flex min-w-0 flex-1 flex-col">
        <header className="flex items-center gap-3 border-b border-line/50 px-5 py-3">
          <div className="flex h-8 w-8 items-center justify-center rounded-lg bg-accent/15 text-accent">
            <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
              <path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z" />
            </svg>
          </div>
          <div className="min-w-0">
            <div className="truncate text-[14px] font-semibold text-ink">
              {platformNode?.data.name ?? "Org Concierge"}
            </div>
            <div className="text-[11px] text-ink-mid">
              {platformNode
                ? "Talk to your platform agent — it orchestrates the org for you."
                : "Platform agent not available on this stack yet."}
            </div>
          </div>
        </header>

        <div className="min-h-0 flex-1">
          {platformNode ? (
            <ChatTab
              key={platformNode.id}
              workspaceId={platformNode.id}
              data={platformNode.data}
            />
          ) : (
            <div className="flex h-full items-center justify-center px-6 text-center">
              <div className="max-w-sm">
                <div className="text-[14px] font-medium text-ink">
                  No platform agent yet
                </div>
                <p className="mt-1 text-[12px] text-ink-mid">
                  Once an org root (the concierge) is provisioned it shows up
                  here and you can chat with it. Switch to the{" "}
                  <span className="text-accent">Map</span> tab to see the
                  workspace graph.
                </p>
              </div>
            </div>
          )}
        </div>
      </section>
    </main>
  );
}
