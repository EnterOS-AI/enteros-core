// @vitest-environment jsdom
import { describe, it, expect } from "vitest";
import { createMessage, type ChatMessage } from "../chat/types";

// The decision chip is driven purely by ChatMessage.decision; this pins the
// data shape the REQUEST_RESPONDED handler builds (core#2636) without
// mounting the full ChatTab (which needs the socket/session harness).
describe("decision message shape (core#2636 My Chat decision chip)", () => {
  it("a user approval becomes a system message tagged decision=approved", () => {
    const base = createMessage("system", 'You approved “Test approval”');
    const msg: ChatMessage = { ...base, decision: "approved" };
    expect(msg.role).toBe("system");
    expect(msg.decision).toBe("approved");
    expect(msg.content).toContain("approved");
    expect(msg.content).toContain("Test approval");
  });

  it("a rejection carries decision=rejected", () => {
    const msg: ChatMessage = { ...createMessage("system", "You rejected the request"), decision: "rejected" };
    expect(msg.decision).toBe("rejected");
  });

  it("createMessage omits decision by default (normal turns aren't chips)", () => {
    expect(createMessage("agent", "hi").decision).toBeUndefined();
  });
});
