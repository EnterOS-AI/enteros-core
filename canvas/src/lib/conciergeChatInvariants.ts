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

/**
 * The agent greetings that are NOT legitimate, over an ORDERED transcript.
 *
 * Counting "every agent bubble that looks like a greeting" is WRONG, and it was
 * a real false RED (run 487714): a user who literally types "hi" a second time
 * gets a perfectly correct conversational reply — "Hey! 👋 How can I help you
 * today?" — which is short, bullet-free and starts with "Hey", so isPureGreeting
 * classifies it as a greeting. Nothing is broken; the concierge answered the
 * message it was sent. Because "My Chat" is one long-lived conversation, a naive
 * count over the whole transcript then sees two "greetings" and fails.
 *
 * The two things that ARE the bug:
 *   1. The SAME greeting comes back — the opening greeting rendered twice (a
 *      client render/persistence race) or re-sent verbatim on a later turn.
 *      Caught by similarity to the opening greeting.
 *   2. The concierge GREETS INSTEAD OF ANSWERING — a greeting-shaped reply to a
 *      SUBSTANTIVE user turn. This is the arm that actually catches the
 *      missing-stable-contextId re-greet, which re-greets on EVERY turn and so
 *      necessarily lands on a substantive one.
 *
 * So a later greeting is legitimate only when it is not a repeat of the opening
 * greeting AND the user's own preceding turn was itself a greeting. The repeat
 * check is applied FIRST, so a duplicated greeting is a violation even when the
 * user did just say "hi".
 *
 * On the LIMITS of check 1 — do not overclaim it: contentSimilarity is Jaccard
 * over token sets, and two SHORT greetings share almost no tokens, so a re-greet
 * that is reworded rather than repeated scores far below the threshold ("Hey
 * there! 👋 I'm the org concierge — your front door..." vs "Hey there! Welcome
 * aboard — how can I help?" scores 0.03). Check 1 catches a REPEAT, not a
 * reword. A reworded greeting in reply to a user's literal "hi" is therefore
 * accepted — which is correct, because it is indistinguishable from a genuine
 * answer. A reworded greeting on any substantive turn is still caught by
 * check 2, which is what makes the re-greet bug detectable at all.
 *
 * @param opts.openingIsExpected  true (default) for a FULL transcript: the
 *        first agent greeting is the expected opening one and is never
 *        returned. Pass false for a WINDOWED transcript whose real opening
 *        greeting lies outside the window — then the first greeting we see gets
 *        no free pass and must justify itself like any other, so a re-greet
 *        inside the window is not swallowed as "the opening".
 */
export function unexpectedGreetings(
  messages: SimpleMessage[],
  opts: { openingIsExpected?: boolean; threshold?: number } = {},
): SimpleMessage[] {
  const openingIsExpected = opts.openingIsExpected ?? true;
  const threshold = opts.threshold ?? 0.7;
  const bad: SimpleMessage[] = [];
  // The first greeting seen doubles as the similarity anchor for later ones,
  // whether or not it gets the "expected opening" free pass.
  let anchor: SimpleMessage | null = null;
  let lastUser: SimpleMessage | null = null;

  for (const m of messages) {
    if (m.role === "user") {
      lastUser = m;
      continue;
    }
    if (m.role !== "agent" || !isPureGreeting(m.content)) continue;

    const isFirst = anchor === null;
    if (isFirst) anchor = m;

    // The opening greeting of a fresh chat is expected exactly once.
    if (isFirst && openingIsExpected) continue;

    // (1) the same greeting again — always a violation, even in reply to "hi".
    if (!isFirst && contentSimilarity(anchor!.content, m.content) >= threshold) {
      bad.push(m);
      continue;
    }
    // (2) greeted instead of answering a substantive turn.
    if (lastUser === null || !isPureGreeting(lastUser.content)) bad.push(m);
  }
  return bad;
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
  // Invariant 2: no re-greet. A greeting after the opening one is a violation
  // when it REPEATS the opening greeting, or when it answers a substantive user
  // turn with a greeting. A greeting-shaped reply to a user who themselves just
  // said "hi" is a correct answer, not a re-greet — see unexpectedGreetings.
  // requireGreeting=false means "an arbitrary window", i.e. the real opening
  // greeting may lie OUTSIDE it — so no greeting in the window gets the
  // expected-opening free pass, or a re-greet would be swallowed as the opening.
  const reGreets = unexpectedGreetings(messages, { openingIsExpected: requireGreeting });
  if (reGreets.length > 0) {
    const shown = reGreets.map((g) => `"${g.content.slice(0, 60)}"`).join("; ");
    violations.push(
      `RE_GREET: ${reGreets.length} unexpected greeting message(s) — the concierge repeated its greeting or greeted instead of answering (the missing-stable-contextId bug): ${shown}`,
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

/** Result of attributing rendered agent bubbles to a single chat turn. */
export interface TurnAttribution {
  /** Agent bubbles that appeared AFTER the baseline — i.e. produced by the turn. */
  replies: SimpleMessage[];
  /** True when every baseline bubble is still intact, in order, at the front. */
  prefixIntact: boolean;
  /** Baseline positions that changed or vanished (empty when prefixIntact). */
  prefixDrift: { index: number; before: string; after: string }[];
}

/**
 * Attribute agent bubbles to ONE turn, by diffing the transcript against a
 * baseline captured immediately BEFORE that turn was sent.
 *
 * Why this exists — the two failure modes it replaces, both proven on run
 * 499907:
 *
 *  1. COUNTING THE WHOLE TRANSCRIPT IS WRONG. The staging specs share ONE org
 *     and ONE long-lived concierge "My Chat" (see staging-setup.ts globalSetup +
 *     workers:1), so by the time a late-sorting spec runs, the transcript already
 *     carries the earlier specs' turns. An `agents.length === 1` model therefore
 *     asserts something false about the world it runs in and false-REDs on
 *     perfectly correct behaviour. Attribution is relative to a baseline, so
 *     pre-existing history is simply not this turn's business.
 *
 *  2. DEDUPING BY EXACT CONTENT CANNOT SEE A SEMANTIC DOUBLE-REPLY. If the agent
 *     answers ONE "hi" TWICE with differently-worded replies ("Hey again! What
 *     can I help you with?" / "Hey! What can I do for you?"), an exact-content
 *     dedupe returns [] and the guard passes — while the user plainly sees the
 *     concierge answer twice. Counting the replies ATTRIBUTABLE TO THE TURN
 *     catches it regardless of wording: one turn must yield exactly one reply.
 *
 * The chat transcript is append-only, so the baseline must survive as a PREFIX.
 * A copy of an OLD bubble re-inserted mid-transcript (the render-dup symptom
 * applied to history rather than the live turn) breaks that prefix, so it is
 * reported via `prefixDrift` instead of being silently absorbed into `replies`.
 *
 * Comparison is on NORMALIZED content, so cosmetic markdown/whitespace re-render
 * cannot masquerade as drift. Non-agent bubbles are ignored on both sides.
 *
 * @param before  transcript captured BEFORE the turn was sent (any roles)
 * @param after   transcript captured AFTER the turn settled (any roles)
 */
export function agentRepliesForTurn(before: SimpleMessage[], after: SimpleMessage[]): TurnAttribution {
  const beforeAgents = before.filter((m) => m.role === "agent");
  const afterAgents = after.filter((m) => m.role === "agent");

  const prefixDrift: { index: number; before: string; after: string }[] = [];
  for (let i = 0; i < beforeAgents.length; i++) {
    const b = normalizeContent(beforeAgents[i].content);
    const a = i < afterAgents.length ? normalizeContent(afterAgents[i].content) : "";
    if (a !== b) {
      prefixDrift.push({
        index: i,
        before: beforeAgents[i].content,
        after: i < afterAgents.length ? afterAgents[i].content : "<missing>",
      });
    }
  }

  return {
    // Everything past the baseline length is new. When the prefix has drifted the
    // slice is not trustworthy on its own — hence prefixIntact, which callers
    // must assert BEFORE reading a meaning into `replies`.
    replies: afterAgents.slice(beforeAgents.length),
    prefixIntact: prefixDrift.length === 0,
    prefixDrift,
  };
}
