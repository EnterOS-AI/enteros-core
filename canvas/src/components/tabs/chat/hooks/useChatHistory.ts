"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { api } from "@/lib/api";
import { type ChatMessage, appendMessageDeduped as appendMessageDedupedFn } from "../types";

const INITIAL_HISTORY_LIMIT = 10;
const OLDER_HISTORY_BATCH = 20;
// Mobile-chat audit F4: prevent the in-memory message buffer from growing
// without bound on long-lived chats. Server-side pagination still works;
// we just discard the oldest messages client-side to bound re-render cost
// and scroll jank on low-end mobile. A virtualized list is the larger fix.
const MAX_MESSAGES = 500;

// Reconcile from the DB copy of chat-history every 10s. This is the
// fail-safe for the WS delivery race diagnosed in core#2598: a reply can
// be persisted on the server (and therefore visible in Agent Comms / the
// activity log) before the canvas WebSocket subscriber is listening, so
// the AGENT_MESSAGE/A2A_RESPONSE frame is missed and My Chat stays empty
// until a manual reload. A short polling reconcile catches any missed
// persisted replies without changing the live WS-driven path.
const RECONCILE_INTERVAL_MS = 10_000;

async function loadMessagesFromDB(
  workspaceId: string,
  limit: number,
  beforeTs?: string,
): Promise<{ messages: ChatMessage[]; error: string | null; reachedEnd: boolean }> {
  try {
    const params = new URLSearchParams({ limit: String(limit) });
    if (beforeTs) params.set("before_ts", beforeTs);
    // The server emits ChatMessage with a snake_case `tool_trace` field
    // (Go json tag). Map it onto the camelCase `toolTrace` the renderer
    // reads so a rehydrated agent turn shows its tool chain (core#2636).
    const resp = await api.get<{
      messages: (ChatMessage & { tool_trace?: ChatMessage["toolTrace"] })[];
      reached_end: boolean;
    }>(`/workspaces/${workspaceId}/chat-history?${params.toString()}`);
    const messages: ChatMessage[] = (resp.messages ?? []).map((m) =>
      m.tool_trace?.length ? { ...m, toolTrace: m.tool_trace } : m,
    );
    return {
      messages,
      error: null,
      reachedEnd: resp.reached_end,
    };
  } catch (err) {
    return {
      messages: [],
      error: err instanceof Error ? err.message : "Failed to load chat history",
      reachedEnd: true,
    };
  }
}

/** Merge a freshly-fetched batch of persisted messages into the existing
 *  in-memory list. Messages are keyed by their identity tuple
 *  (timestamp+role+content) and overwritten with the server copy (so
 *  fields like `toolTrace` stay in sync), then the combined set is
 *  re-sorted by timestamp. New replies that were missed by the WebSocket
 *  path therefore appear in the correct order without discarding older
 *  history the user has already lazy-loaded.
 *
 *  Why key on the tuple, not `m.id`: this reconcile re-fetches the same
 *  chat-history window every 10s and on every WS reconnect. If it keyed
 *  on a server id that was NOT stable across fetches, the same row would
 *  land under a new key each poll and the whole window would be
 *  re-appended — the "My Chat" doubling bug (36→72→…). The backend now
 *  mints a STABLE per-row id (activity_logs PK + bubble kind), but keying
 *  on the intrinsic tuple keeps this dedup correct even if a store ever
 *  regresses to a per-fetch id. The row's created_at timestamp is per-row
 *  unique and role separates the user/agent bubbles of one row, so
 *  distinct messages keep distinct keys. */
function mergeReconciledMessages(
  existing: ChatMessage[],
  fetched: ChatMessage[],
): ChatMessage[] {
  const keyOf = (m: ChatMessage) => `${m.timestamp}|${m.role}|${m.content}`;
  const map = new Map<string, ChatMessage>();
  for (const m of existing) map.set(keyOf(m), m);
  for (const m of fetched) map.set(keyOf(m), m);
  const merged = Array.from(map.values()).sort(
    (a, b) => new Date(a.timestamp).getTime() - new Date(b.timestamp).getTime(),
  );
  return merged.slice(-MAX_MESSAGES);
}

export interface ScrollAnchor {
  savedDistanceFromBottom: number;
  expectFirstIdNotEqual: string | null;
}

export function useChatHistory(
  workspaceId: string,
  containerRef?: React.RefObject<HTMLDivElement | null>,
) {
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [loadingOlder, setLoadingOlder] = useState(false);
  const [hasMore, setHasMore] = useState(true);

  const fetchTokenRef = useRef(0);
  const workspaceIdRef = useRef(workspaceId);
  const oldestMessageRef = useRef<ChatMessage | null>(null);
  const hasMoreRef = useRef(true);
  const inflightRef = useRef(false);
  const scrollAnchorRef = useRef<ScrollAnchor | null>(null);

  useEffect(() => {
    workspaceIdRef.current = workspaceId;
  }, [workspaceId]);

  useEffect(() => {
    oldestMessageRef.current = messages[0] ?? null;
  }, [messages]);

  useEffect(() => {
    hasMoreRef.current = hasMore;
  }, [hasMore]);

  const loadInitial = useCallback(() => {
    setLoading(true);
    setLoadError(null);
    setHasMore(true);
    fetchTokenRef.current += 1;
    const myToken = fetchTokenRef.current;
    return loadMessagesFromDB(workspaceId, INITIAL_HISTORY_LIMIT).then(
      ({ messages: msgs, error: fetchErr, reachedEnd }) => {
        if (fetchTokenRef.current !== myToken) return;
        setMessages(msgs);
        setLoadError(fetchErr);
        setHasMore(!reachedEnd);
        setLoading(false);
      },
    );
  }, [workspaceId]);

  useEffect(() => {
    loadInitial();
  }, [loadInitial]);

  const loadOlder = useCallback(async () => {
    if (inflightRef.current || !hasMoreRef.current) return;
    const oldest = oldestMessageRef.current;
    if (!oldest) return;
    const container = containerRef?.current;
    // Scroll anchoring is only possible when a container ref is wired;
    // otherwise still load older messages instead of silently no-oping
    // (mc#2908 F9).
    if (container) {
      scrollAnchorRef.current = {
        savedDistanceFromBottom: container.scrollHeight - container.scrollTop,
        expectFirstIdNotEqual: oldest.id,
      };
    } else {
      scrollAnchorRef.current = null;
    }
    inflightRef.current = true;
    fetchTokenRef.current += 1;
    const myToken = fetchTokenRef.current;
    setLoadingOlder(true);
    try {
      const { messages: older, reachedEnd } = await loadMessagesFromDB(
        workspaceId,
        OLDER_HISTORY_BATCH,
        oldest.timestamp,
      );
      if (fetchTokenRef.current !== myToken) {
        scrollAnchorRef.current = null;
        return;
      }
      if (older.length > 0) {
        setMessages((prev) => [...older, ...prev].slice(-MAX_MESSAGES));
      } else {
        scrollAnchorRef.current = null;
      }
      setHasMore(!reachedEnd);
    } finally {
      setLoadingOlder(false);
      inflightRef.current = false;
    }
  }, [workspaceId, containerRef]);

  const reconcile = useCallback(async () => {
    // Silent reconcile: don't flip loading/loadingOlder flags. The user is
    // already looking at the conversation; briefly flashing a spinner would
    // be worse than the missed-reply bug we're fixing. Failures are ignored
    // — the next interval or WS reconnect will retry.
    //
    // STALE-WORKSPACE GUARD (core#2598 Researcher #14648 / CR2 #14653):
    // a reconcile can be in flight when the user switches to a different
    // workspace. We capture the workspace id that was current when the
    // fetch started and drop the result if it has changed. Reading the
    // current id from a ref lets a stale callback see the latest workspace
    // after a rerender, and keeping this separate from fetchTokenRef avoids
    // colliding with an in-flight loadInitial/loadOlder.
    const startedForWorkspace = workspaceIdRef.current;
    try {
      const { messages: fetched } = await loadMessagesFromDB(
        workspaceId,
        INITIAL_HISTORY_LIMIT,
      );
      if (workspaceIdRef.current !== startedForWorkspace) return;
      if (fetched.length === 0) return;
      setMessages((prev) => mergeReconciledMessages(prev, fetched));
    } catch {
      // Intentionally swallow: this is a background safety net, not a
      // user-initiated fetch. A transient API failure must not spam the UI.
    }
  }, [workspaceId]);

  // Background reconcile: catch replies that landed while the WebSocket
  // subscriber was not yet listening (core#2598). Runs on a short cadence
  // so a missed frame is repaired within human-perceptible time, and also
  // exposes `reconcile` so ChatTab can fire it immediately on reconnect.
  useEffect(() => {
    const id = setInterval(() => {
      void reconcile();
    }, RECONCILE_INTERVAL_MS);
    return () => clearInterval(id);
  }, [reconcile]);

  return {
    messages,
    loading,
    loadError,
    loadingOlder,
    hasMore,
    loadInitial,
    loadOlder,
    reconcile,
    appendMessageDeduped: (msg: ChatMessage) =>
      setMessages((prev) => appendMessageDedupedFn(prev, msg).slice(-MAX_MESSAGES)),
    setMessages,
    scrollAnchorRef,
  };
}
