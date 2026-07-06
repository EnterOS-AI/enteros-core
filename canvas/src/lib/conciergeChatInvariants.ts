// conciergeChatInvariants.ts
//
// Pure, framework-free invariant checker for the concierge "My Chat" flow.
// Shared by BOTH the deterministic unit test
// (src/lib/__tests__/conciergeChatInvariants.test.ts) and the real-flow
// staging E2E (e2e/staging-concierge-greeting.spec.ts). Keeping the assertion
// logic in one pure function is the SSOT for "what correct concierge chat looks
// like" and lets the unit test prove — deterministically, with no network —
// that the checker FAILS on the duplicate-greeting bug and PASSES once the fix
// lands.
//
// THE BUG THIS GUARDS (concierge "identical greeting twice" / re-greet-every-turn):
//   A canvas-origin `message/send` that reaches the runtime WITHOUT a stable
//   `contextId` makes the a2a-sdk mint a fresh context_id per request, so a
//   runtime that keys its native session on it opens a NEW session every turn →
//   the concierge re-greets with no prior context. The user sees the same
//   greeting again instead of an answer. Fixed by threading/injecting a stable
//   per-conversation contextId (canvas chatContext.ts client half + the
//   workspace-server a2a_proxy.go ensureCanvasSessionContextID server belt).
//
// The three invariants (from the live symptom on a fresh org):
//   1. A fresh concierge chat opens with EXACTLY ONE greeting.
//   2. A follow-up ('hi' then a distinct question) yields a reply that is
//      CONVERSATIONAL — NOT another greeting (no re-greet-every-turn).
//   3. NO duplicate messages in the session (dedupe by role + normalized
//      content) — the literal "identical greeting twice".

export type ChatRole = "user" | "agent" | "system";

export interface SimpleMessage {
  role: ChatRole;
  content: string;
}

export interface DuplicateGroup {
  role: ChatRole;
  content: string;
  count: number;
}

export interface InvariantResult {
  ok: boolean;
  violations: string[];
  greetingCount: number;
  greetings: string[];
  duplicates: DuplicateGroup[];
}

/** Normalize message content for content-equality dedupe: strip markdown
 *  emphasis noise is intentionally NOT done (two bubbles are "the same" only
 *  when their rendered text matches), just whitespace-collapse + lowercase so
 *  a trailing-newline / case difference doesn't hide a true duplicate. */
export function normalizeContent(s: string): string {
  return (s || "").replace(/\s+/g, " ").trim().toLowerCase();
}

// A greeting opener at the START of the message ("Hi", "Hey there!", "Hello",
// "Welcome", "Greetings"). Anchored to ^ so an answer that merely CONTAINS
// "...what can I help you with?" as a sign-off is not misread as a greeting.
const GREETING_OPENER = /^\s*(hi|hey|hello|welcome|greetings)\b/i;
// The concierge's self-introduction ("I'm the org concierge", "I am your
// orchestrator", "your front door to ..."). Bounded gap so it stays an intro,
// not any far-apart co-occurrence.
const CONCIERGE_INTRO = /\b(i['’]m|i am)\b[^.!?\n]{0,50}\b(concierge|orchestrator|front door)\b/i;
// A bullet/enumeration marks a SUBSTANTIVE answer (e.g. the capability list the
// concierge gives for "what can you do?"), never a bare greeting.
const HAS_BULLET_LIST = /(^|\n)\s*(?:[-*•]|\d+\.)\s/;
// A bare greeting is short; a real answer runs long.
const GREETING_MAX_LEN = 240;

/**
 * True when a message is a bare concierge GREETING (an opener / self-intro with
 * no substantive body), as opposed to a real answer. Robust to LLM wording:
 * keys on an anchored opener OR an explicit concierge self-introduction, and
 * excludes anything long or list-bearing (a substantive answer).
 */
export function isPureGreeting(text: string): boolean {
  const t = (text || "").trim();
  if (!t) return false;
  if (t.length > GREETING_MAX_LEN) return false; // substantive answer, not a greeting
  if (HAS_BULLET_LIST.test(t)) return false; // enumerated answer, not a greeting
  return GREETING_OPENER.test(t) || CONCIERGE_INTRO.test(t);
}

/** Token set of a message for coarse similarity (used to flag a near-identical
 *  re-greet whose wording drifted just enough to dodge exact-dedupe). */
function tokenSet(text: string): Set<string> {
  return new Set(
    normalizeContent(text)
      .replace(/[^a-z0-9\s]/g, " ")
      .split(/\s+/)
      .filter((w) => w.length > 2),
  );
}

/** Jaccard similarity of two messages' token sets, 0..1. */
export function contentSimilarity(a: string, b: string): number {
  const sa = tokenSet(a);
  const sb = tokenSet(b);
  if (sa.size === 0 || sb.size === 0) return 0;
  let inter = 0;
  for (const w of sa) if (sb.has(w)) inter++;
  return inter / (sa.size + sb.size - inter);
}

/** Group messages by (role, normalized content) and return any content that
 *  appears more than once — the literal duplicate-message symptom. Trivial
 *  acks (< 3 normalized chars) are ignored so a genuine repeated "ok"/"hi"
 *  from the user is not mistaken for a rendering/persistence duplicate. */
export function findDuplicates(messages: SimpleMessage[]): DuplicateGroup[] {
  const groups = new Map<string, DuplicateGroup>();
  for (const m of messages) {
    const norm = normalizeContent(m.content);
    if (norm.length < 3) continue;
    // Delimiter is the unit-separator control char (U+001F) — it never occurs
    // in normalized message text, so (role, content) can't collide across the
    // role boundary. Must NOT be a NUL byte (would make git treat the file as
    // binary).
    const key = `${m.role}${norm}`;
    const g = groups.get(key);
    if (g) g.count++;
    else groups.set(key, { role: m.role, content: m.content.trim(), count: 1 });
  }
  return [...groups.values()].filter((g) => g.count > 1);
}

/**
 * Check the three concierge-chat invariants against an ORDERED message list
 * (oldest → newest), as returned by the workspace-server /chat-history endpoint
 * or scraped from the rendered DOM. Returns a structured result so callers
 * (unit test + E2E) assert on `ok` and surface `violations` on failure.
 *
 * @param messages     ordered chat messages for the session
 * @param opts.requireGreeting  when true (default), a session with ZERO
 *        greetings is a violation (a fresh concierge chat MUST greet once).
 *        Set false to only assert "no re-greet / no dup" over an arbitrary
 *        window.
 */
export function checkConciergeInvariants(
  messages: SimpleMessage[],
  opts: { requireGreeting?: boolean } = {},
): InvariantResult {
  const requireGreeting = opts.requireGreeting ?? true;
  const violations: string[] = [];

  const agentMsgs = messages.filter(
    (m) => m.role === "agent" && m.content.trim().length > 0,
  );
  const greetings = agentMsgs.filter((m) => isPureGreeting(m.content));
  const greetingCount = greetings.length;

  // Invariant 1: exactly one greeting on a fresh chat.
  if (requireGreeting && greetingCount === 0) {
    violations.push(
      "NO_GREETING: the concierge never greeted — a fresh My Chat must open with exactly one greeting.",
    );
  }
  // Invariant 2: no re-greet-every-turn. More than one bare greeting means the
  // agent greeted again instead of answering — the exact re-greet symptom.
  if (greetingCount > 1) {
    violations.push(
      `RE_GREET: ${greetingCount} greeting messages found (expected exactly 1). The concierge re-greeted instead of continuing the conversation — the missing-stable-contextId bug.`,
    );
  }

  // Invariant 3: no duplicate messages by (role, normalized content).
  const duplicates = findDuplicates(messages);
  if (duplicates.length > 0) {
    const shown = duplicates
      .map((d) => `${d.role} ×${d.count}: "${d.content.slice(0, 60)}"`)
      .join("; ");
    violations.push(`DUPLICATE_MESSAGE: ${shown}`);
  }

  return {
    ok: violations.length === 0,
    violations,
    greetingCount,
    greetings: greetings.map((g) => g.content),
    duplicates,
  };
}

/**
 * Focused check for the "send 'hi' → get ONE conversational reply, not a
 * re-greet" invariant, given the greeting text and the reply to a DISTINCT
 * follow-up question. Flags the reply as a re-greet when it is a bare greeting
 * OR too similar to the opening greeting.
 */
export function isReGreet(greeting: string, followupReply: string, threshold = 0.7): boolean {
  if (isPureGreeting(followupReply)) return true;
  return contentSimilarity(greeting, followupReply) >= threshold;
}
