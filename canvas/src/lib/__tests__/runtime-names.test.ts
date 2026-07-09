/**
 * Tests for `runtimeDisplayName` — the friendly-name lookup that
 * surfaces the workspace runtime in the chat indicator, details
 * tab, and a few component labels. Tiny but high-touch: every
 * surface that shows "this workspace runs on X" goes through here.
 *
 * Issue: #1815 follow-up — `src/lib/runtime-names.ts` was at 0%
 * coverage despite being read by 3+ rendering paths.
 */
import { describe, it, expect } from "vitest";
import { runtimeDisplayName } from "../runtime-names";

describe("runtimeDisplayName", () => {
  it.each([
    ["claude-code", "Claude Code"],
    ["codex", "Codex"],
    ["hermes", "Hermes"],
    ["openclaw", "OpenClaw"],
    ["crewai", "CrewAI"],
    ["kimi", "Kimi"],
    ["kimi-cli", "Kimi CLI"],
  ])("known runtime %q maps to %q", (input, expected) => {
    expect(runtimeDisplayName(input)).toBe(expected);
  });

  it("crewai maps to a friendly name, not the bare id", () => {
    // Regression for the canvas↔CP-catalog drift (audit §3): crewai is a
    // first-class template-backed runtime in the CP catalog
    // (internal/providers/runtimes.yaml → "CrewAI Agent") but was missing from
    // the canvas projection, so it rendered as the raw "crewai". This asserts
    // the projection now covers it — it FAILS before the friendly name is added
    // (fallback returns the input string verbatim).
    expect(runtimeDisplayName("crewai")).toBe("CrewAI");
    expect(runtimeDisplayName("crewai")).not.toBe("crewai");
  });

  it("unknown runtime falls back to the input string verbatim", () => {
    // A future runtime not yet in the lookup map should render with
    // its own id — better than a generic placeholder for ops debugging.
    expect(runtimeDisplayName("custom-runtime-9000")).toBe(
      "custom-runtime-9000",
    );
  });

  it("empty string falls back to 'agent' (final default)", () => {
    // Any code path that loses the runtime field still renders SOMETHING;
    // the chat indicator never shows a blank label.
    expect(runtimeDisplayName("")).toBe("agent");
  });

  it("is case-sensitive — uppercase variants miss the lookup", () => {
    // The lookup keys are lowercase by convention. Pin the case
    // sensitivity explicitly so a future refactor that lowercases
    // the input "for safety" doesn't silently change behavior — the
    // upstream slug is already normalized lowercase.
    expect(runtimeDisplayName("Claude-Code")).toBe("Claude-Code");
    expect(runtimeDisplayName("CODEX")).toBe("CODEX");
  });
});
