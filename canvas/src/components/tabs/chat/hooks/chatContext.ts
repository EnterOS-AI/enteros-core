// chatContext.ts — a STABLE per-conversation id threaded as the A2A
// `message.contextId` (tenant-agent BUG 3, client half).
//
// WHY: the canvas used to send each chat message with only a fresh random
// `messageId` and NO `contextId`. The a2a-sdk on the runtime then calls
// `_check_or_generate_context_id()` and mints a FRESH uuid per request — so any
// runtime that keys its native session on `context_id` (openclaw's
// SessionManager, and the base RuntimeA2AExecutor's native thread_id) opened a
// NEW session every turn → the agent re-greeted with no prior context.
//
// Threading a stable `contextId` per conversation makes the a2a-sdk REUSE it, so
// the runtime's session resumes across turns. This is the all-runtime client-side
// fix (the runtime SDK also derives a stable session id from WORKSPACE_ID as a
// belt, but the client should not force the sdk to invent one).
//
// The id is:
//   * STABLE across turns + across reloads (persisted, so a page refresh resumes
//     the same conversation rather than resetting the agent), and
//   * ROTATED on an explicit "New session" (startNewSession / SESSION_RESET), so a
//     new session gets a fresh agent context.
//
// Scoped per workspace so distinct workspaces never share a conversation key.

const storageKey = (workspaceId: string): string =>
  `mol.chat.contextId.${workspaceId}`;

function mint(workspaceId: string): string {
  const rand =
    typeof crypto !== "undefined" && typeof crypto.randomUUID === "function"
      ? crypto.randomUUID()
      : `${Date.now()}-${Math.random().toString(36).slice(2)}`;
  return `sess-${workspaceId}-${rand}`;
}

/**
 * Return the stable conversation id for a workspace, creating + persisting one on
 * first use. Safe on SSR / storage-denied contexts (falls back to a workspace-
 * scoped constant, still stable within the session).
 */
export function getConversationId(workspaceId: string): string {
  const key = storageKey(workspaceId);
  try {
    const existing = window.localStorage.getItem(key);
    // MIGRATION (2026-07-21): pre-unification builds minted a RANDOM default
    // (`conv-<wsid>-<rand>`), so browsers that ever opened the workspace are
    // stranded on a fragment forever. Legacy conv-* ids are old defaults —
    // fold them into the deterministic session. Post-unification EXPLICIT
    // rotations mint `sess-*`, which is preserved (user-chosen isolation).
    //
    // ACCEPTED TRADEOFF (review wf_8b04761b #3): a PRE-deploy explicit
    // "New session" also minted conv-* and is indistinguishable from a
    // legacy default, so it folds into the deterministic session too. The
    // harm is bounded — for these browsers the canvas-<wsid> runtime thread
    // is fresh (their history lived under conv-* ids), so they join a new
    // default rather than resuming the rotated-away context — while NOT
    // migrating would strand every legacy browser on a fragment forever.
    if (existing && !existing.startsWith("conv-")) return existing;
    // DEFAULT is the DETERMINISTIC workspace session id — the SAME value the
    // server belt (canvasSessionContextID) fills for contextId-less sends and
    // the platform stamps on its own turns (first-boot greeting,
    // restart-context). One workspace = one default session across browsers,
    // restarts, and boots (Langfuse showed 3 fragments, 2026-07-21); a RANDOM
    // id is minted only on an explicit "New session" (rotateConversationId).
    const id = `canvas-${workspaceId}`;
    window.localStorage.setItem(key, id);
    return id;
  } catch {
    // No storage (SSR / privacy mode): a workspace-scoped constant is still far
    // better than a fresh-per-request context_id — it stays stable within the
    // page's lifetime for that workspace.
    return `canvas-${workspaceId}`;
  }
}

/**
 * Rotate the conversation id for a workspace (called when a NEW chat session is
 * started). Returns the fresh id. Best-effort persistence.
 */
export function rotateConversationId(workspaceId: string): string {
  const id = mint(workspaceId);
  try {
    window.localStorage.setItem(storageKey(workspaceId), id);
  } catch {
    // ignore — the next getConversationId falls back to the scoped constant.
  }
  return id;
}
