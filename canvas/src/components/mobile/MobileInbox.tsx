// MobileInbox — Tasks + Approvals on mobile (core#2697 Phase 1).
//
// The desktop home flow has Tasks + Approvals as first-class destinations
// (ConciergeShell sidebar, RequestsInbox); mobile had NO way to review or
// action pending requests — decision-on-the-go, the canonical mobile job, was
// missing entirely. This is the mobile-native equivalent: same data layer
// (GET /requests/pending?kind=… + POST /requests/{id}/respond) the desktop
// RequestsInbox uses, with a touch-first list + sub-tabs, live WS refresh, and
// optimistic decisions. (More-Info threads are desktop-only for v1 — flagged.)

import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import { api } from "@/lib/api";
import { fetchSession } from "@/lib/auth";
import { useSocketEvent } from "@/hooks/useSocketEvent";
import type { RequestRow } from "@/components/concierge/RequestsInbox";
import { MOBILE_FONT_SANS, usePalette } from "./palette";

type InboxKind = "task" | "approval";

// WS events that mutate the pending set — re-fetch on any of them so the
// inbox stays live (mirrors RequestsInbox's refresh triggers).
const REFRESH_EVENTS = new Set([
  "REQUEST_CREATED",
  "REQUEST_RESPONDED",
  "REQUEST_MESSAGE",
  "REQUEST_UPDATED",
]);

export function MobileInbox({ dark }: { dark: boolean }) {
  const p = usePalette(dark);
  const [kind, setKind] = useState<InboxKind>("approval");
  const [items, setItems] = useState<RequestRow[]>([]);
  const [loading, setLoading] = useState(true);
  const [acting, setActing] = useState<string | null>(null);

  // Responder identity — the same resolution RequestsInbox uses (session
  // user_id, "admin" placeholder when unauthenticated).
  const responderIdRef = useRef<string>("admin");
  useEffect(() => {
    let cancelled = false;
    fetchSession()
      .then((s) => { if (!cancelled && s?.user_id) responderIdRef.current = s.user_id; })
      .catch(() => {});
    return () => { cancelled = true; };
  }, []);

  // Guard against stale fetches: when the user switches tabs, a previous
  // in-flight fetch for the old kind must not overwrite the list after the
  // new tab's load clears it (CR2 #11478).
  const loadSeqRef = useRef(0);
  const load = useCallback(() => {
    const seq = ++loadSeqRef.current;
    // core#2766 / CR2 #11478: clear stale rows the moment the kind changes
    // so the user never sees approval cards under the Tasks tab (or
    // vice-versa) while the new list is still fetching.
    setItems([]);
    setLoading(true);
    api
      .get<RequestRow[]>(`/requests/pending?kind=${kind}`)
      .then((rows) => {
        if (seq !== loadSeqRef.current) return; // stale response
        setItems(Array.isArray(rows) ? rows : []);
      })
      .catch(() => {
        if (seq !== loadSeqRef.current) return; // stale response
        setItems([]);
      })
      .finally(() => {
        if (seq !== loadSeqRef.current) return; // stale response
        setLoading(false);
      });
  }, [kind]);

  useEffect(() => { load(); }, [load]);

  useSocketEvent((msg) => {
    if (msg?.event && REFRESH_EVENTS.has(msg.event)) load();
  });

  const respond = useCallback(
    async (r: RequestRow, action: "done" | "rejected" | "approved") => {
      if (acting) return;
      setActing(r.id);
      // Optimistic: drop the row immediately; restore on failure.
      const prev = items;
      setItems((cur) => cur.filter((x) => x.id !== r.id));
      try {
        await api.post(`/requests/${r.id}/respond`, {
          action,
          responder_type: "user",
          responder_id: responderIdRef.current,
        });
      } catch {
        setItems(prev); // restore on failure
      } finally {
        setActing(null);
      }
    },
    [acting, items],
  );

  const subTabs: { id: InboxKind; label: string }[] = useMemo(
    () => [
      { id: "approval", label: "Approvals" },
      { id: "task", label: "Tasks" },
    ],
    [],
  );

  return (
    <div
      style={{
        height: "100%",
        display: "flex",
        flexDirection: "column",
        background: p.bg,
        color: p.text,
        fontFamily: MOBILE_FONT_SANS,
      }}
    >
      {/* Header + sub-tabs */}
      <div style={{ padding: "14px 16px 0", borderBottom: `0.5px solid ${p.divider}` }}>
        <div style={{ fontSize: 17, fontWeight: 600, marginBottom: 10 }}>Inbox</div>
        <div role="tablist" aria-label="Inbox kind" style={{ display: "flex", gap: 18 }}>
          {subTabs.map((t) => {
            const on = kind === t.id;
            return (
              <button
                key={t.id}
                role="tab"
                aria-selected={on}
                onClick={() => setKind(t.id)}
                style={{
                  background: "none",
                  border: "none",
                  padding: "0 0 10px",
                  fontSize: 14,
                  fontWeight: on ? 600 : 500,
                  color: on ? p.text : p.text3,
                  borderBottom: on ? `2px solid ${p.accent}` : "2px solid transparent",
                  cursor: "pointer",
                  fontFamily: MOBILE_FONT_SANS,
                }}
              >
                {t.label}
              </button>
            );
          })}
        </div>
      </div>

      {/* List */}
      <div style={{ flex: 1, overflowY: "auto", padding: "12px 16px", display: "flex", flexDirection: "column", gap: 10 }}>
        {loading && items.length === 0 ? (
          <div style={{ color: p.text3, fontSize: 13, textAlign: "center", marginTop: 40 }}>Loading…</div>
        ) : items.length === 0 ? (
          <div style={{ color: p.text3, fontSize: 13, textAlign: "center", marginTop: 40 }}>
            {kind === "approval"
              ? "No pending approvals. Destructive actions await sign-off here."
              : "No pending tasks."}
          </div>
        ) : (
          items.map((r) => (
            <div
              key={r.id}
              data-testid="inbox-row"
              style={{
                background: p.surface,
                border: `0.5px solid ${p.border}`,
                borderRadius: 14,
                padding: 14,
                display: "flex",
                flexDirection: "column",
                gap: 8,
              }}
            >
              <div style={{ fontSize: 14, fontWeight: 600, color: p.text }}>{r.title}</div>
              {r.detail ? (
                <div style={{ fontSize: 12.5, color: p.text2, lineHeight: 1.4 }}>{r.detail}</div>
              ) : null}
              <div style={{ fontSize: 11, color: p.text3 }}>
                {(r.workspace_name || r.requester_id || "agent")}
                {r.status && r.status !== "pending" ? ` · ${r.status.replace(/_/g, " ")}` : ""}
              </div>
              <div style={{ display: "flex", gap: 8, marginTop: 4 }}>
                <button
                  type="button"
                  disabled={acting === r.id}
                  onClick={() => respond(r, r.kind === "approval" ? "approved" : "done")}
                  style={{
                    flex: 1,
                    padding: "9px 0",
                    borderRadius: 10,
                    border: "none",
                    background: p.green,
                    color: "#fff",
                    fontSize: 13,
                    fontWeight: 600,
                    cursor: acting === r.id ? "not-allowed" : "pointer",
                    opacity: acting === r.id ? 0.5 : 1,
                    fontFamily: MOBILE_FONT_SANS,
                  }}
                >
                  {r.kind === "approval" ? "Approve" : "Done"}
                </button>
                <button
                  type="button"
                  disabled={acting === r.id}
                  onClick={() => respond(r, "rejected")}
                  style={{
                    flex: 1,
                    padding: "9px 0",
                    borderRadius: 10,
                    border: `0.5px solid ${p.failed}`,
                    background: "transparent",
                    color: p.failed,
                    fontSize: 13,
                    fontWeight: 600,
                    cursor: acting === r.id ? "not-allowed" : "pointer",
                    opacity: acting === r.id ? 0.5 : 1,
                    fontFamily: MOBILE_FONT_SANS,
                  }}
                >
                  Reject
                </button>
              </div>
            </div>
          ))
        )}
      </div>
    </div>
  );
}
