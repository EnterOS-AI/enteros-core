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
