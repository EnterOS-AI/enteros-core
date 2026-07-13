// Deterministic proof that the concierge-chat invariant checker CATCHES the
// duplicate-greeting / re-greet bug and PASSES on correct behaviour. Runs in
// the canvas vitest lane (no network) so the guard's discriminating power is
// pinned regardless of live-staging state.
//
// The transcripts use REAL concierge text captured from a live staging org
// (grd1013.staging.moleculesai.app) so the fixtures match production wording,
// not an invented straw-man.

import { describe, it, expect } from "vitest";
import {
  checkConciergeInvariants,
  isPureGreeting,
  isReGreet,
  unexpectedGreetings,
  contentSimilarity,
  findDuplicates,
  type SimpleMessage,
} from "../conciergeChatInvariants";

// Real concierge greeting (captured live).
const GREETING =
  "Hey there! 👋 I'm the org concierge — your front door to everything in this organization. What can I help you with today?";
// Real substantive answer to "what can you do?" (captured live) — must NOT be
// classified as a greeting.
const CAPABILITY_ANSWER =
  "I'm your org orchestrator — here's what I can do:\n\n" +
  "**Organization & Team Management**\n" +
  "- Create, list, and manage workspaces/agents across your org\n" +
  "- Provision new team members or agents on demand\n" +
  "- Handle workspace secrets, budgets, and settings";
// Real answer that ends with a "what can I help with?" sign-off — must NOT be a
// greeting just because it offers help at the end.
const SIGNOFF_ANSWER =
  "Done — your name is Sam, noted. I'll remember it going forward. What can I help you with?";

describe("isPureGreeting", () => {
  it("classifies bare greetings / self-intros as greetings", () => {
    expect(isPureGreeting(GREETING)).toBe(true);
    expect(isPureGreeting("Hey! 👋 Go ahead whenever you're ready — what do you need?")).toBe(true);
    expect(isPureGreeting("Hi! 👋")).toBe(true);
    expect(isPureGreeting("Hey again! 👋 What can I do for you?")).toBe(true);
  });

  it("does NOT misclassify substantive answers as greetings", () => {
    expect(isPureGreeting(CAPABILITY_ANSWER)).toBe(false); // long + bullet list
    expect(isPureGreeting(SIGNOFF_ANSWER)).toBe(false); // help offer is a sign-off, not an opener
    expect(isPureGreeting("Sam! You just told me a minute ago. 😄")).toBe(false);
  });
});

// The transcript that FALSE-REDDED the Canvas tabs E2E on main (run 487714).
// "My Chat" is one long-lived conversation, so by the time the render spec runs
// it already carries the previous spec's turns. The user types 'hi' a SECOND
// time and the concierge answers it correctly — a short reply that happens to
// open with "Hey". Counting greeting-SHAPED bubbles saw two greetings and
// failed; nothing was actually wrong.
const REPLY_TO_A_SECOND_HI = "Hey! 👋 How can I help you today?";
const ACCUMULATED_MY_CHAT: SimpleMessage[] = [
  { role: "user", content: "hi" },
  { role: "agent", content: GREETING },
  { role: "user", content: "what can you do?" },
  { role: "agent", content: CAPABILITY_ANSWER },
  { role: "user", content: "hi" },
  { role: "agent", content: REPLY_TO_A_SECOND_HI },
];

describe("unexpectedGreetings — a greeting-shaped ANSWER is not a re-greet", () => {
  it("does not flag the concierge answering a user who literally said 'hi' again", () => {
    // The regression: this exact transcript must be CLEAN.
    expect(unexpectedGreetings(ACCUMULATED_MY_CHAT)).toEqual([]);
    expect(checkConciergeInvariants(ACCUMULATED_MY_CHAT).ok).toBe(true);
    // ...and it is only clean because of the user turn — not because the reply
    // stopped looking like a greeting. Guard that the premise still holds.
    expect(isPureGreeting(REPLY_TO_A_SECOND_HI)).toBe(true);
  });

  it("STILL flags the same greeting coming back — even in reply to a user's 'hi'", () => {
    // The duplicate-render / re-greet bug: the OPENING greeting itself returns.
    // A preceding user 'hi' must NOT excuse it — the repeat check comes first.
    const bug: SimpleMessage[] = [
      ...ACCUMULATED_MY_CHAT.slice(0, 4),
      { role: "user", content: "hi" },
      { role: "agent", content: GREETING },
    ];
    expect(unexpectedGreetings(bug)).toHaveLength(1);
    expect(checkConciergeInvariants(bug).ok).toBe(false);
  });

  it("STILL flags greeting instead of ANSWERING a substantive question", () => {
    const bug: SimpleMessage[] = [
      { role: "user", content: "hi" },
      { role: "agent", content: GREETING },
      { role: "user", content: "list my workspaces" },
      { role: "agent", content: "Hi! 👋 What would you like to do?" },
    ];
    expect(unexpectedGreetings(bug)).toHaveLength(1);
    expect(checkConciergeInvariants(bug).ok).toBe(false);
  });

  it("STILL flags the greeting rendered twice back-to-back (no user turn between)", () => {
    const bug: SimpleMessage[] = [
      { role: "user", content: "hi" },
      { role: "agent", content: GREETING },
      { role: "agent", content: GREETING },
    ];
    expect(unexpectedGreetings(bug)).toHaveLength(1);
  });

  // The similarity check catches a REPEAT, not a REWORD — pin that limit
  // honestly rather than claiming a guarantee the code does not provide.
  // Jaccard over token sets is near-zero for two short greetings.
  it("similarity does NOT catch a REWORDED greeting (a repeat check, not a reword check)", () => {
    const reworded = "Hey there! Welcome aboard — how can I help?";
    expect(contentSimilarity(GREETING, reworded)).toBeLessThan(0.7);
    // ...so after a user's literal 'hi' it is accepted — correct, because it is
    // indistinguishable from a genuine answer.
    expect(
      unexpectedGreetings([
        { role: "user", content: "hi" },
        { role: "agent", content: GREETING },
        { role: "user", content: "hi" },
        { role: "agent", content: reworded },
      ]),
    ).toEqual([]);
    // ...but on a SUBSTANTIVE turn it is still caught by check (2). This is the
    // arm that makes the missing-stable-contextId re-greet detectable, since
    // that bug re-greets on EVERY turn and so necessarily lands on one.
    expect(
      unexpectedGreetings([
        { role: "user", content: "hi" },
        { role: "agent", content: GREETING },
        { role: "user", content: "list my workspaces" },
        { role: "agent", content: reworded },
      ]),
    ).toHaveLength(1);
  });

  // A WINDOWED transcript's real opening greeting lies outside the window, so
  // the first greeting in it must NOT get the expected-opening free pass — or a
  // genuine re-greet would be swallowed as "the opening".
  it("windowed transcript (requireGreeting:false): a re-greet is not swallowed as the opening", () => {
    const window: SimpleMessage[] = [
      { role: "user", content: "list my workspaces" },
      { role: "agent", content: "Hello! What would you like to do?" },
    ];
    // Full-transcript semantics would treat that as the expected opening.
    expect(unexpectedGreetings(window)).toEqual([]);
    // Windowed semantics must flag it — it greeted instead of answering.
    expect(unexpectedGreetings(window, { openingIsExpected: false })).toHaveLength(1);
    expect(checkConciergeInvariants(window, { requireGreeting: false }).ok).toBe(false);
  });
});

describe("checkConciergeInvariants — PASSES on correct behaviour", () => {
  it("fresh chat: one greeting, then a conversational answer, no dups", () => {
    const messages: SimpleMessage[] = [
      { role: "user", content: "hi" },
      { role: "agent", content: GREETING },
      { role: "user", content: "what can you do?" },
      { role: "agent", content: CAPABILITY_ANSWER },
    ];
    const r = checkConciergeInvariants(messages);
    expect(r.violations).toEqual([]);
    expect(r.ok).toBe(true);
    expect(r.greetingCount).toBe(1);
  });
});

describe("checkConciergeInvariants — FAILS on the duplicate-greeting bug (regression guard)", () => {
  it("re-greet: the follow-up turn gets the SAME greeting again", () => {
    const messages: SimpleMessage[] = [
      { role: "user", content: "hi" },
      { role: "agent", content: GREETING },
      { role: "user", content: "what can you do?" },
      { role: "agent", content: GREETING }, // BUG: re-greets instead of answering
    ];
    const r = checkConciergeInvariants(messages);
    expect(r.ok).toBe(false);
    expect(r.greetingCount).toBe(2);
    // Both symptoms are surfaced: a re-greet AND a literal duplicate message.
    expect(r.violations.some((v) => v.startsWith("RE_GREET"))).toBe(true);
    expect(r.violations.some((v) => v.startsWith("DUPLICATE_MESSAGE"))).toBe(true);
  });

  it("near-identical re-greet (wording drifted) is still caught", () => {
    const messages: SimpleMessage[] = [
      { role: "user", content: "hi" },
      { role: "agent", content: GREETING },
      { role: "user", content: "what can you do?" },
      {
        role: "agent",
        content:
          "Hi! 👋 I'm the org concierge — your front door to everything here. How can I help you?",
      },
    ];
    const r = checkConciergeInvariants(messages);
    expect(r.ok).toBe(false);
    expect(r.violations.some((v) => v.startsWith("RE_GREET"))).toBe(true);
  });

  it("literal duplicate greeting on fresh load (same greeting rendered twice)", () => {
    const messages: SimpleMessage[] = [
      { role: "user", content: "hi" },
      { role: "agent", content: GREETING },
      { role: "agent", content: GREETING }, // BUG: greeting doubled
    ];
    const r = checkConciergeInvariants(messages);
    expect(r.ok).toBe(false);
    expect(findDuplicates(messages).length).toBeGreaterThan(0);
  });
});

describe("isReGreet helper", () => {
  it("flags a bare greeting reply and a near-identical greeting", () => {
    expect(isReGreet(GREETING, GREETING)).toBe(true);
    expect(
      isReGreet(GREETING, "Hi! 👋 I'm the org concierge — your front door to everything here. How can I help?"),
    ).toBe(true);
  });
  it("does not flag a genuine conversational answer", () => {
    expect(isReGreet(GREETING, CAPABILITY_ANSWER)).toBe(false);
    expect(isReGreet(GREETING, SIGNOFF_ANSWER)).toBe(false);
  });
});
