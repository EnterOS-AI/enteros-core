// @vitest-environment jsdom
//
// REGRESSION GUARD — concierge duplicate-greeting RENDER-DUP (the ACTUAL bug).
// ============================================================================
// This guards the REAL mechanism a user hit on a fresh org (evidence a48f6bbf,
// verified against org test11): the backend stores EXACTLY ONE greeting, but on
// a SLOW cold first turn (~30s openclaw) the canvas double-RENDERS it because
// the ONE greeting reaches the UI as TWO copies whose timestamps differ by the
// turn duration:
//
//   (a) the persisted-history copy — server ingest-ts T,   id "<rowid>:agent"
//   (b) the live HTTP/WS reply      — client-ts   T+30s,   a random client UUID
//
// The two ids live in DIFFERENT id-spaces (deterministic "<rowid>:agent" vs a
// client UUID), so the id-keyed reconcile merge never collides them, and the
// timestamps are >3s apart, so appendMessageDeduped's short (3s) window can't
// collapse them either. Before the fix, BOTH copies rendered → the user saw the
// greeting twice. The fix (commit 15032a31) added a 60s optimistic-collapse to
// mergeReconciledMessages (useChatHistory.ts) that drops the optimistic/live
// copy once its authoritative DB copy has arrived within the clock-skew window.
//
// This guard DRIVES THE REAL merge logic (mergeReconciledMessages via the
// useChatHistory hook's reconcile(), and the real appendMessageDeduped from
// types.ts) — it is NOT a synthetic invariant checker. It:
//   • PASSES on current main (with 15032a31): exactly ONE greeting renders, and
//   • FAILS on the pre-15032a31 logic: TWO greetings render (locked in-file via
//     a verbatim copy of the stale merge, and demonstrated empirically by
//     swapping useChatHistory.ts back to its parent b6fbda16 — see the PR).
//
// It complements PR #3428 (which guards a DIFFERENT mechanism — re-greet via a
// missing contextId — that does not reproduce the render-dup). Test-only: no
// product code is touched.
// ============================================================================

import { describe, it, expect, vi, beforeEach } from "vitest";
import { renderHook, waitFor, act } from "@testing-library/react";

const apiGetMock = vi.fn<(path: string, opts?: unknown) => Promise<unknown>>();

vi.mock("@/lib/api", () => ({
  api: { get: (path: string, opts?: unknown) => apiGetMock(path, opts) },
}));

import { useChatHistory } from "../useChatHistory";
import { type ChatMessage, appendMessageDeduped } from "../../types";

// A realistic concierge opening line — the ONE greeting the backend stores once.
const GREETING =
  "Hi! I'm your concierge. I can help you spin up teams and workspaces — what would you like to build?";

// The slow-cold turn takes ~30s: the persisted copy carries the server ingest
// timestamp (turn start-ish), the live HTTP reply is stamped ~30s later by the
// client clock. Anchor to Date.now() so BOTH code paths under test behave
// deterministically:
//   - mergeReconciledMessages' collapse is MESSAGE-GAP based (db.ts vs live.ts),
//   - appendMessageDeduped's dedupe is WALL-CLOCK-NOW based (existing.ts vs now),
// so relative timestamps exercise each one faithfully.
const TURN_MS = 30_000;
const isoAgo = (ms: number) => new Date(Date.now() - ms).toISOString();

// (a) persisted-history copy: deterministic "<activity_logs rowID>:agent" id,
//     server ingest-ts (T, i.e. ~30s ago at the moment the live reply lands).
const dbGreeting = (): ChatMessage => ({
  id: "4210:agent",
  role: "agent",
  content: GREETING,
  timestamp: isoAgo(TURN_MS),
});

// (b) live-appended copy: client-minted random UUID, client-ts (T+30s ≈ now).
const liveGreeting = (): ChatMessage => ({
  id: crypto.randomUUID(),
  role: "agent",
  content: GREETING,
  timestamp: isoAgo(0),
});

const greetingCount = (msgs: readonly { role: string; content: string }[]) =>
  msgs.filter((m) => m.role === "agent" && m.content === GREETING).length;

beforeEach(() => {
  apiGetMock.mockReset();
});

describe("concierge slow-cold greeting: render-dup regression guard (REAL merge logic)", () => {
  it("scenario is a SLOW turn (>3s gap) — the case a fast-turn test cannot catch", () => {
    const gap = Date.parse(liveGreeting().timestamp) - Date.parse(dbGreeting().timestamp);
    // The two copies are ~30s apart — well past appendMessageDeduped's 3s window.
    expect(gap).toBeGreaterThan(3000);
    expect(gap).toBeGreaterThanOrEqual(TURN_MS - 1000);
  });

  it("FIX (live→reconcile): the live copy collapses into its DB copy → EXACTLY ONE greeting renders", async () => {
    // Fresh org: initial chat-history is empty.
    apiGetMock.mockResolvedValue({ messages: [], reached_end: true });
    const { result } = renderHook(() => useChatHistory("ws-slowcold-a"));
    await waitFor(() => expect(result.current.loading).toBe(false));

    // The slow HTTP/WS reply lands ~30s later → the canvas appends the LIVE
    // bubble (random UUID, client-ts) through the REAL appendMessageDeduped path.
    act(() => {
      result.current.appendMessageDeduped(liveGreeting());
    });
    expect(result.current.messages).toHaveLength(1); // only the live copy so far

    // The ≤10s DB reconcile then brings the PERSISTED copy (server-ts T,
    // "<rowid>:agent"). reconcile() runs the REAL mergeReconciledMessages.
    apiGetMock.mockResolvedValue({ messages: [dbGreeting()], reached_end: true });
    await act(async () => {
      await result.current.reconcile();
    });

    // THE GUARD: exactly ONE greeting bubble renders. The 60s optimistic-collapse
    // drops the live copy in favour of its authoritative DB copy.
    expect(greetingCount(result.current.messages)).toBe(1);
    expect(result.current.messages).toHaveLength(1);
    expect(result.current.messages[0].id).toBe("4210:agent"); // the authoritative row survives
  });

  it("FIX (reconcile→live): DB copy first, slow live reply after → still EXACTLY ONE greeting", async () => {
    // The persisted copy arrives first via reconcile.
    apiGetMock.mockResolvedValue({ messages: [dbGreeting()], reached_end: true });
    const { result } = renderHook(() => useChatHistory("ws-slowcold-b"));
    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.messages).toHaveLength(1);

    // The slow live reply lands ~30s later and is appended. appendMessageDeduped's
    // 3s window is TOO SHORT to collapse a 30s gap, so momentarily BOTH exist —
    // this is precisely why the merge-level collapse is the real fix.
    act(() => {
      result.current.appendMessageDeduped(liveGreeting());
    });
    expect(result.current.messages).toHaveLength(2); // 3s dedupe cannot catch a 30s gap

    // ...but the next reconcile's mergeReconciledMessages collapses it → ONE.
    apiGetMock.mockResolvedValue({ messages: [dbGreeting()], reached_end: true });
    await act(async () => {
      await result.current.reconcile();
    });
    expect(greetingCount(result.current.messages)).toBe(1);
    expect(result.current.messages[0].id).toBe("4210:agent");
  });

  it("does NOT over-collapse: a genuine second, DISTINCT greeting still renders (no message loss)", async () => {
    // Guard against the fix being a blunt de-dupe: two DISTINCT persisted rows
    // with the same greeting text (a real re-greet on turn 2) must BOTH survive —
    // the collapse only ever drops an OPTIMISTIC copy, never a second DB row.
    apiGetMock.mockResolvedValue({
      messages: [
        { id: "4210:agent", role: "agent", content: GREETING, timestamp: isoAgo(TURN_MS) },
        { id: "4288:agent", role: "agent", content: GREETING, timestamp: isoAgo(0) },
      ],
      reached_end: true,
    });
    const { result } = renderHook(() => useChatHistory("ws-slowcold-c"));
    await waitFor(() => expect(result.current.loading).toBe(false));
    await act(async () => {
      await result.current.reconcile();
    });
    expect(greetingCount(result.current.messages)).toBe(2); // two DB rows → both kept
  });
});

// ── The real appendMessageDeduped (types.ts): why the 3s window is not enough ──
// Drives the exported dedupe helper directly to document its boundary: it only
// collapses copies within ~3s of NOW, so a fast turn is covered but the slow
// cold turn is NOT — which is exactly why the fix had to live in the reconcile
// merge (60s gap window), not here. A fast-turn test would pass on the STALE
// bundle and miss the render-dup entirely.
describe("appendMessageDeduped (types.ts) — 3s window covers a FAST turn only", () => {
  it("does NOT dedupe the slow-cold 30s-gap copy (the render-dup escapes here)", () => {
    const out = appendMessageDeduped([dbGreeting()], liveGreeting());
    expect(greetingCount(out)).toBe(2);
  });

  it("DOES dedupe a fast-turn (<3s) duplicate — the case a fast test would rely on", () => {
    const fastDb: ChatMessage = { id: "4210:agent", role: "agent", content: GREETING, timestamp: isoAgo(1000) };
    const fastLive: ChatMessage = { id: crypto.randomUUID(), role: "agent", content: GREETING, timestamp: isoAgo(0) };
    const out = appendMessageDeduped([fastDb], fastLive);
    expect(greetingCount(out)).toBe(1);
  });
});

// ── FAIL-BEFORE lock: the EXACT pre-15032a31 merge renders TWO greetings ───────
// Verbatim copy of mergeReconciledMessages at parent commit b6fbda16 (BEFORE the
// fix 15032a31 added the 60s optimistic-collapse). Kept ONLY as the documented
// regression reference — product code is untouched. Applied to the SAME fixtures
// the hook-driven guard above uses, the stale logic renders TWO greetings,
// proving (1) the scenario genuinely triggers the bug and (2) the guard
// fails-before / passes-after. (Also demonstrated empirically in the PR by
// git-checking-out useChatHistory.ts at b6fbda16 and re-running this file.)
const MAX_MESSAGES = 500;
function stalePreFixMerge(existing: ChatMessage[], fetched: ChatMessage[]): ChatMessage[] {
  const keyOf = (m: ChatMessage) => m.id || `${m.timestamp}|${m.role}|${m.content}`;
  const map = new Map<string, ChatMessage>();
  for (const m of existing) map.set(keyOf(m), m);
  for (const m of fetched) map.set(keyOf(m), m);
  const merged = Array.from(map.values()).sort(
    (a, b) => new Date(a.timestamp).getTime() - new Date(b.timestamp).getTime(),
  );
  return merged.slice(-MAX_MESSAGES);
}

describe("fail-before: the stale (pre-15032a31) merge renders TWO greetings", () => {
  it("stale merge keeps BOTH copies (id-spaces differ, no collapse) — the bug", () => {
    const rendered = stalePreFixMerge([liveGreeting()], [dbGreeting()]);
    expect(greetingCount(rendered)).toBe(2); // ← the duplicate-greeting the user saw
  });

  it("current merge (via the REAL hook) collapses the SAME inputs to ONE — the fix", async () => {
    apiGetMock.mockResolvedValue({ messages: [], reached_end: true });
    const { result } = renderHook(() => useChatHistory("ws-ab"));
    await waitFor(() => expect(result.current.loading).toBe(false));
    act(() => {
      result.current.appendMessageDeduped(liveGreeting());
    });
    apiGetMock.mockResolvedValue({ messages: [dbGreeting()], reached_end: true });
    await act(async () => {
      await result.current.reconcile();
    });
    expect(greetingCount(result.current.messages)).toBe(1);
  });
});
