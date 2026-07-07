// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { activityRowToMessages, type ActivityRowForHydration } from "../historyHydration";

function makeRow(overrides: Partial<ActivityRowForHydration> = {}): ActivityRowForHydration {
  return {
    activity_type: "a2a_receive",
    status: "ok",
    created_at: "2026-04-25T18:00:00.000Z",
    request_body: null,
    response_body: null,
    ...overrides,
  };
}

describe("activityRowToMessages", () => {
  // The bug that prompted extracting this helper: every call to
  // createMessage() inside the loader stamped `timestamp: new Date()`,
  // so every reload re-stamped every historical user bubble to the
  // render moment. Two messages sent hours apart both rendered with
  // the same "now" clock. These tests pin the override so a future
  // refactor that drops the spread-and-override fails loudly.

  describe("timestamp preservation (regression cover)", () => {
    beforeEach(() => {
      // Freeze the wall clock to a value that's CLEARLY different from
      // any row.created_at the tests use, so any test that doesn't
      // override the timestamp will mint "2030-…" and the assertion
      // against "2026-…" will fail unmistakably.
      vi.useFakeTimers();
      vi.setSystemTime(new Date("2030-01-01T00:00:00.000Z"));
    });
    afterEach(() => vi.useRealTimers());

    it("user message timestamp pins to row.created_at, NOT new Date()", () => {
      const row = makeRow({
        created_at: "2026-04-25T18:00:00.000Z",
        request_body: { params: { message: { parts: [{ kind: "text", text: "hello from earlier today" }] } } },
      });
      const msgs = activityRowToMessages(row);
      const user = msgs.find((m) => m.role === "user")!;
      expect(user.timestamp).toBe("2026-04-25T18:00:00.000Z");
      // Negative assertion: the wall clock is set to 2030. If the
      // override regresses, timestamp will start with "2030-".
      expect(user.timestamp.startsWith("2030")).toBe(false);
    });

    it("agent message timestamp pins to row.created_at, NOT new Date()", () => {
      const row = makeRow({
        created_at: "2026-04-25T18:05:00.000Z",
        response_body: { result: "agent reply" },
      });
      const msgs = activityRowToMessages(row);
      const agent = msgs.find((m) => m.role === "agent")!;
      expect(agent.timestamp).toBe("2026-04-25T18:05:00.000Z");
      expect(agent.timestamp.startsWith("2030")).toBe(false);
    });

    it("two rows with distinct created_at produce two distinct timestamps (the user-visible bug)", () => {
      // The actual screenshot symptom: two user messages sent hours
      // apart both rendered with the same render-moment timestamp.
      // If activityRowToMessages defers to new Date(), both messages
      // here will share the frozen 2030 wall clock instead of their
      // distinct created_at values.
      const a = activityRowToMessages(
        makeRow({
          created_at: "2026-04-25T14:00:00.000Z",
          request_body: { params: { message: { parts: [{ kind: "text", text: "first" }] } } },
        }),
      );
      const b = activityRowToMessages(
        makeRow({
          created_at: "2026-04-25T21:01:58.000Z",
          request_body: { params: { message: { parts: [{ kind: "text", text: "second" }] } } },
        }),
      );
      expect(a[0].timestamp).toBe("2026-04-25T14:00:00.000Z");
      expect(b[0].timestamp).toBe("2026-04-25T21:01:58.000Z");
      expect(a[0].timestamp).not.toBe(b[0].timestamp);
    });
  });

  describe("user-message extraction", () => {
    it("emits a user message when request_body has text", () => {
      const row = makeRow({
        request_body: { params: { message: { parts: [{ kind: "text", text: "hi agent" }] } } },
      });
      const msgs = activityRowToMessages(row);
      expect(msgs).toHaveLength(1);
      expect(msgs[0].role).toBe("user");
      expect(msgs[0].content).toBe("hi agent");
    });

    it("surfaces internal self-messages as a system notice (legacy prefix fallback), never as a user bubble", () => {
      // The heartbeat self-trigger ("Delegation results are ready...")
      // is NOT a real user message. This untagged row (no source_type)
      // exercises the legacy text-prefix fallback: it must be surfaced as
      // role=system / systemKind=notice — NOT dropped, NOT a user bubble
      // (the bug where the user saw the internal prompt as blue/right).
      const row = makeRow({
        request_body: { params: { message: { parts: [{ kind: "text", text: "Delegation results are ready..." }] } } },
      });
      const msgs = activityRowToMessages(row);
      expect(msgs.find((m) => m.role === "user")).toBeUndefined();
      const sys = msgs.find((m) => m.role === "system");
      expect(sys).toBeDefined();
      expect(sys!.systemKind).toBe("notice");
      expect(sys!.content).toContain("Delegation results are ready");
    });

    it("classifies internal self-messages by the params.metadata.source_type marker (primary path)", () => {
      // The role-based SSOT path: a row carrying source_type=self-harvester
      // is classified system/notice by the marker, NOT by the text (which
      // here does not match any legacy prefix).
      const row = makeRow({
        request_body: {
          params: {
            metadata: { source_type: "self-harvester" },
            message: { parts: [{ kind: "text", text: "wake up and check your work" }] },
          },
        },
      });
      const msgs = activityRowToMessages(row);
      expect(msgs.find((m) => m.role === "user")).toBeUndefined();
      const sys = msgs.find((m) => m.role === "system");
      expect(sys).toBeDefined();
      expect(sys!.systemKind).toBe("notice");
    });

    it("classifies the concierge warmup (source_type=self-warmup) as a system notice, never a user bubble", () => {
      // The platform-fired readiness probe carries source_type=self-warmup on
      // both the params.metadata sibling and the message.metadata fallback. It
      // is a heartbeat internal that used to leak as a blue user bubble.
      const row = makeRow({
        request_body: {
          params: {
            metadata: { source_type: "self-warmup" },
            message: {
              parts: [{ kind: "text", text: "Platform readiness check — no action needed." }],
              metadata: { concierge_warmup: true, source_type: "self-warmup" },
            },
          },
        },
      });
      const msgs = activityRowToMessages(row);
      expect(msgs.find((m) => m.role === "user")).toBeUndefined();
      const sys = msgs.find((m) => m.role === "system");
      expect(sys).toBeDefined();
      expect(sys!.systemKind).toBe("notice");
    });

    it("emits no user message when request_body is null", () => {
      const row = makeRow({ request_body: null });
      const msgs = activityRowToMessages(row);
      expect(msgs.find((m) => m.role === "user")).toBeUndefined();
    });

    // Reviewer follow-up: the pre-fix loader didn't extract user-side
    // file parts, so a chat reload after a session where the user
    // dragged in a file showed the text bubble but lost the chip.
    // Symmetric to the agent attachment hydration below.

    it("hydrates user-side file attachments from request_body.params.message.parts", () => {
      const row = makeRow({
        request_body: {
          params: {
            message: {
              parts: [
                { kind: "text", text: "here's the screenshot" },
                {
                  kind: "file",
                  file: {
                    name: "shot.png",
                    mimeType: "image/png",
                    uri: "workspace:/uploads/shot.png",
                    size: 4096,
                  },
                },
              ],
            },
          },
        },
      });
      const msgs = activityRowToMessages(row);
      const user = msgs.find((m) => m.role === "user")!;
      expect(user.content).toBe("here's the screenshot");
      expect(user.attachments).toEqual([
        { name: "shot.png", mimeType: "image/png", uri: "workspace:/uploads/shot.png", size: 4096 },
      ]);
    });

    it("emits an attachments-only user bubble when text is empty (drag-drop without caption)", () => {
      // Some users drop a file with no message — the bubble should
      // still render so the file appears in history. Pre-fix the
      // empty userText short-circuited and the row was dropped.
      const row = makeRow({
        request_body: {
          params: {
            message: {
              parts: [
                { kind: "file", file: { name: "report.pdf", uri: "workspace:/uploads/report.pdf" } },
              ],
            },
          },
        },
      });
      const msgs = activityRowToMessages(row);
      expect(msgs).toHaveLength(1);
      expect(msgs[0].role).toBe("user");
      expect(msgs[0].content).toBe("");
      expect(msgs[0].attachments).toHaveLength(1);
      expect(msgs[0].attachments![0].name).toBe("report.pdf");
    });

    it("internal-self message with attachments is surfaced as a system notice, never as a user bubble", () => {
      // Defence-in-depth: if a future internal trigger DID carry
      // attachments, they ride on the system note rather than being
      // misattributed to the user (who never touched them).
      const row = makeRow({
        request_body: {
          params: {
            message: {
              parts: [
                { kind: "text", text: "Delegation results are ready..." },
                { kind: "file", file: { name: "x.zip", uri: "workspace:/x.zip" } },
              ],
            },
          },
        },
      });
      const msgs = activityRowToMessages(row);
      expect(msgs.find((m) => m.role === "user")).toBeUndefined();
      const sys = msgs.find((m) => m.role === "system");
      expect(sys).toBeDefined();
      expect(sys!.systemKind).toBe("notice");
    });
  });

  describe("agent-message extraction", () => {
    it("emits an agent message from response_body.result string", () => {
      const row = makeRow({ response_body: { result: "agent says hi" } });
      const msgs = activityRowToMessages(row);
      expect(msgs).toHaveLength(1);
      expect(msgs[0].role).toBe("agent");
      expect(msgs[0].content).toBe("agent says hi");
    });

    it("emits role=system when status=error", () => {
      // System role gets distinct rendering (red bubble) for failed
      // calls; if this regresses errors will look like normal agent
      // replies and the user won't realise something went wrong.
      const row = makeRow({
        status: "error",
        response_body: { result: "delegation failed" },
      });
      const msgs = activityRowToMessages(row);
      expect(msgs[0].role).toBe("system");
    });

    it("attaches file parts hydrated from response_body (parts at root)", () => {
      // The notify-with-attachments shape: response_body =
      // {result: "<text>", parts: [{kind:"file", ...}]}. If
      // extractFilesFromTask doesn't see the attachments here, a chat
      // reload after an agent attached a file would lose the chips.
      const row = makeRow({
        response_body: {
          result: "Done — see attached.",
          parts: [
            { kind: "file", file: { name: "build.zip", uri: "workspace:/tmp/build.zip", size: 12345 } },
          ],
        },
      });
      const msgs = activityRowToMessages(row);
      const agent = msgs.find((m) => m.role === "agent")!;
      expect(agent.attachments).toEqual([
        { name: "build.zip", uri: "workspace:/tmp/build.zip", size: 12345 },
      ]);
    });

    it("emits no agent message when response_body is null", () => {
      const row = makeRow({ response_body: null });
      const msgs = activityRowToMessages(row);
      expect(msgs.find((m) => m.role === "agent" || m.role === "system")).toBeUndefined();
    });

    it("emits no agent message when response_body has neither text nor files", () => {
      const row = makeRow({ response_body: { unrelated: "metadata" } });
      const msgs = activityRowToMessages(row);
      expect(msgs.find((m) => m.role === "agent")).toBeUndefined();
    });
  });

  describe("end-to-end shape", () => {
    it("a single row with both user request and agent reply emits two messages with the same timestamp", () => {
      // Mirrors the canonical canvas-source row: user types something,
      // agent replies, both stored on the same activity_logs row. UI
      // renders them as the user bubble immediately followed by the
      // agent bubble — keep that pairing intact.
      const row = makeRow({
        created_at: "2026-04-25T18:00:00.000Z",
        request_body: { params: { message: { parts: [{ kind: "text", text: "what's 2+2?" }] } } },
        response_body: { result: "4" },
      });
      const msgs = activityRowToMessages(row);
      expect(msgs).toHaveLength(2);
      expect(msgs[0].role).toBe("user");
      expect(msgs[0].content).toBe("what's 2+2?");
      expect(msgs[0].timestamp).toBe("2026-04-25T18:00:00.000Z");
      expect(msgs[1].role).toBe("agent");
      expect(msgs[1].content).toBe("4");
      expect(msgs[1].timestamp).toBe("2026-04-25T18:00:00.000Z");
    });
  });
});
