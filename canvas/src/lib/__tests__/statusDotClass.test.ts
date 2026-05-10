// @vitest-environment jsdom
/**
 * Tests for statusDotClass — maps a workspace status string to the
 * CSS tailwind class used on the status indicator dot.
 */
import { describe, it, expect } from "vitest";
import { statusDotClass } from "../design-tokens";

describe("statusDotClass", () => {
  it('returns "bg-emerald-400" for "online"', () => {
    expect(statusDotClass("online")).toBe("bg-emerald-400");
  });

  it('returns "bg-zinc-500" for "offline"', () => {
    expect(statusDotClass("offline")).toBe("bg-zinc-500");
  });

  it('returns "bg-indigo-400" for "paused"', () => {
    expect(statusDotClass("paused")).toBe("bg-indigo-400");
  });

  it('returns "bg-amber-400" for "degraded"', () => {
    expect(statusDotClass("degraded")).toBe("bg-amber-400");
  });

  it('returns "bg-red-400" for "failed"', () => {
    expect(statusDotClass("failed")).toBe("bg-red-400");
  });

  it('returns "bg-sky-400 motion-safe:animate-pulse" for "provisioning"', () => {
    expect(statusDotClass("provisioning")).toBe("bg-sky-400 motion-safe:animate-pulse");
  });

  it('returns "bg-amber-300" for "not_configured"', () => {
    expect(statusDotClass("not_configured")).toBe("bg-amber-300");
  });

  it("falls back to bg-zinc-500 for unknown status strings", () => {
    expect(statusDotClass("unknown")).toBe("bg-zinc-500");
    expect(statusDotClass("")).toBe("bg-zinc-500");
    expect(statusDotClass("ONLINE")).toBe("bg-zinc-500"); // case-sensitive
    expect(statusDotClass(" online")).toBe("bg-zinc-500"); // whitespace-sensitive
    expect(statusDotClass("online\n")).toBe("bg-zinc-500");
  });

  it("is a pure function — same input always returns same output", () => {
    const result = statusDotClass("online");
    for (let i = 0; i < 5; i++) {
      expect(statusDotClass("online")).toBe(result);
    }
  });
});
