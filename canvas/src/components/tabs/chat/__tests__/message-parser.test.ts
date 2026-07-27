import { describe, it, expect } from "vitest";
import {
  extractRequestText,
  extractResponseText,
  extractAgentText,
  extractTextsFromParts,
  extractFilesFromTask,
  isInternalSelfRequest,
  SELF_SOURCE_TYPES,
} from "../message-parser";
import { SELF_SOURCE_TYPES as GENERATED_SELF_SOURCE_TYPES } from "../self-source-types.generated";

describe("extractRequestText", () => {
  it("extracts text from standard A2A request_body", () => {
    const body = {
      params: {
        message: {
          role: "user",
          parts: [{ kind: "text", text: "Hello agent" }],
        },
      },
    };
    expect(extractRequestText(body)).toBe("Hello agent");
  });

  it("returns empty string for null body", () => {
    expect(extractRequestText(null)).toBe("");
  });

  it("returns empty string for empty object", () => {
    expect(extractRequestText({})).toBe("");
  });

  it("returns empty string when params missing", () => {
    expect(extractRequestText({ other: "data" })).toBe("");
  });

  it("returns empty string when message missing", () => {
    expect(extractRequestText({ params: {} })).toBe("");
  });

  it("returns empty string when parts empty", () => {
    expect(extractRequestText({ params: { message: { parts: [] } } })).toBe("");
  });

  it("extracts first part text only", () => {
    const body = {
      params: {
        message: {
          parts: [
            { kind: "text", text: "First" },
            { kind: "text", text: "Second" },
          ],
        },
      },
    };
    expect(extractRequestText(body)).toBe("First");
  });

  it("handles non-text parts gracefully", () => {
    const body = {
      params: {
        message: {
          parts: [{ kind: "image", data: "base64..." }],
        },
      },
    };
    expect(extractRequestText(body)).toBe("");
  });

  // Regression: delegation.go stores request_body as {"task": "...", "delegation_id": "..."}.
  // extractRequestText was checking only the A2A params.message.parts path, so
  // outbound delegation messages were rendered as blank bubbles.
  // Fix: check body.task first (delegation format), then fall back to A2A.
  it("extracts text from body.task (delegation format)", () => {
    const body = {
      task: "Deploy the staging environment for this sprint's release",
      delegation_id: "delg_01jx8q4n3k",
    };
    expect(extractRequestText(body)).toBe(
      "Deploy the staging environment for this sprint's release"
    );
  });

  it("prefers body.task over A2A params when both present", () => {
    const body = {
      task: "Delegation text wins",
      params: {
        message: {
          parts: [{ kind: "text", text: "A2A text" }],
        },
      },
    };
    // body.task is checked first; delegation wins for delegation activities.
    expect(extractRequestText(body)).toBe("Delegation text wins");
  });

  it("falls back to A2A format when body.task is absent", () => {
    const body = {
      params: {
        message: {
          parts: [{ kind: "text", text: "A2A fallback" }],
        },
      },
    };
    expect(extractRequestText(body)).toBe("A2A fallback");
  });

  it("returns empty string when body.task is empty string", () => {
    const body = { task: "" };
    expect(extractRequestText(body)).toBe("");
  });

  it("returns empty string when body.task is not a string", () => {
    const body = { task: 42 };
    expect(extractRequestText(body)).toBe("");
  });
});

describe("extractResponseText", () => {
  it("extracts from result string", () => {
    expect(extractResponseText({ result: "Hello!" })).toBe("Hello!");
  });

  it("extracts from result.parts[].text", () => {
    const body = {
      result: {
        parts: [{ kind: "text", text: "Response text" }],
      },
    };
    expect(extractResponseText(body)).toBe("Response text");
  });

  it("extracts from result.parts[].root.text", () => {
    const body = {
      result: {
        parts: [{ root: { text: "Root text" } }],
      },
    };
    expect(extractResponseText(body)).toBe("Root text");
  });

  it("extracts from task field", () => {
    expect(extractResponseText({ task: "Task text" })).toBe("Task text");
  });

  it("returns empty for empty object", () => {
    expect(extractResponseText({})).toBe("");
  });

  it("returns empty when result has no parts", () => {
    expect(extractResponseText({ result: { other: true } })).toBe("");
  });

  // Regression: Claude Code (and other long-reply runtimes) emits
  // multi-part text replies. The previous implementation returned
  // only the first part, silently truncating the rest. Observed
  // 2026-04-25 on a 15k-char Wave 1 brief that rendered as just the
  // markdown table header.
  it("joins all text parts when result.parts has multiple", () => {
    const body = {
      result: {
        parts: [
          { kind: "text", text: "# Header" },
          { kind: "text", text: "| Col |" },
          { kind: "text", text: "| --- |" },
          { kind: "text", text: "| Row |" },
        ],
      },
    };
    expect(extractResponseText(body)).toBe("# Header\n| Col |\n| --- |\n| Row |");
  });

  it("joins all text parts across multiple artifacts", () => {
    const body = {
      result: {
        artifacts: [
          { parts: [{ kind: "text", text: "First artifact" }] },
          { parts: [{ kind: "text", text: "Second artifact" }] },
        ],
      },
    };
    expect(extractResponseText(body)).toBe("First artifact\nSecond artifact");
  });

  it("joins all .root.text variants when present", () => {
    const body = {
      result: {
        parts: [
          { root: { text: "alpha" } },
          { root: { text: "beta" } },
        ],
      },
    };
    expect(extractResponseText(body)).toBe("alpha\nbeta");
  });

  // Regression: when a response carries BOTH parts and artifacts
  // (Hermes tool-call replies do this — summary in parts, detail in
  // artifacts), the early-return-on-parts implementation silently
  // dropped the artifacts body. The collected-from-every-source
  // implementation must surface both.
  it("collects text from BOTH result.parts AND result.artifacts when both present", () => {
    const body = {
      result: {
        parts: [{ kind: "text", text: "Summary" }],
        artifacts: [
          { parts: [{ kind: "text", text: "Detail block one" }] },
          { parts: [{ kind: "text", text: "Detail block two" }] },
        ],
      },
    };
    expect(extractResponseText(body)).toBe("Summary\nDetail block one\nDetail block two");
  });

  // Regression: delegation.go stores response_body as
  // {"text": "...", "delegation_id": "..."} — no "result" wrapper.
  // Without body.text handling, extractResponseText returns "" for
  // delegate_result rows, causing the error UI to fire even when the
  // delegation succeeded (issue #159).
  it("extracts from body.text (delegation response_body shape)", () => {
    const body = {
      text: "PR #149: tier-check fails NO REVIEWS (author needs engineers/managers/ceo approval)",
      delegation_id: "delg_01jx8q4n3k",
    };
    expect(extractResponseText(body)).toBe(
      "PR #149: tier-check fails NO REVIEWS (author needs engineers/managers/ceo approval)"
    );
  });

  it("prefers body.result over body.text when both present", () => {
    const body = {
      result: { parts: [{ kind: "text", text: "A2A result wins" }] },
      text: "Delegation text",
    };
    // result path is checked first; A2A wins when both present.
    expect(extractResponseText(body)).toBe("A2A result wins");
  });

  it("returns empty string when body.text is empty string", () => {
    expect(extractResponseText({ text: "" })).toBe("");
  });

  it("extracts from body.response_preview (DELEGATION_COMPLETE WS event shape)", () => {
    const body = {
      response_preview: "PR #149: tier-check fails NO REVIEWS (author needs engineers/managers/ceo approval)",
    };
    expect(extractResponseText(body)).toBe(
      "PR #149: tier-check fails NO REVIEWS (author needs engineers/managers/ceo approval)"
    );
  });
});

describe("extractTextsFromParts", () => {
  it("extracts text parts with kind=text", () => {
    const parts = [
      { kind: "text", text: "Hello" },
      { kind: "text", text: "World" },
    ];
    expect(extractTextsFromParts(parts)).toBe("Hello\nWorld");
  });

  it("extracts text parts with type=text", () => {
    const parts = [{ type: "text", text: "Legacy format" }];
    expect(extractTextsFromParts(parts)).toBe("Legacy format");
  });

  it("returns null for non-array", () => {
    expect(extractTextsFromParts(null)).toBeNull();
    expect(extractTextsFromParts(undefined)).toBeNull();
    expect(extractTextsFromParts("string")).toBeNull();
  });

  it("returns null for empty array", () => {
    expect(extractTextsFromParts([])).toBeNull();
  });

  it("filters out non-text parts", () => {
    const parts = [
      { kind: "image", data: "..." },
      { kind: "text", text: "Only text" },
    ];
    expect(extractTextsFromParts(parts)).toBe("Only text");
  });
});

describe("extractFilesFromTask", () => {
  it("pulls A2A file parts out of a result", () => {
    const task = {
      parts: [
        { kind: "text", text: "here's the report" },
        {
          kind: "file",
          file: { name: "report.pdf", mimeType: "application/pdf", uri: "workspace:/reports/report.pdf", size: 4096 },
        },
      ],
    };
    const files = extractFilesFromTask(task);
    expect(files).toEqual([
      { name: "report.pdf", mimeType: "application/pdf", uri: "workspace:/reports/report.pdf", size: 4096 },
    ]);
  });

  it("recovers a filename from the URI when `name` is absent", () => {
    const task = {
      parts: [
        { kind: "file", file: { uri: "workspace:/workspace/out/graph.png" } },
      ],
    };
    const files = extractFilesFromTask(task);
    expect(files[0].name).toBe("graph.png");
  });

  it("skips file parts without a URI (inline bytes are not supported yet)", () => {
    const task = {
      parts: [
        { kind: "file", file: { name: "inline.bin", bytes: "AAA=" } },
      ],
    };
    expect(extractFilesFromTask(task)).toEqual([]);
  });

  it("walks artifacts[] so file parts nested inside artifact envelopes are found", () => {
    const task = {
      artifacts: [
        {
          parts: [
            { kind: "file", file: { name: "trace.log", uri: "workspace:/logs/trace.log" } },
          ],
        },
      ],
    };
    const files = extractFilesFromTask(task);
    expect(files[0]).toMatchObject({ name: "trace.log", uri: "workspace:/logs/trace.log" });
  });

  it("returns [] on malformed input rather than throwing", () => {
    expect(extractFilesFromTask({})).toEqual([]);
    expect(extractFilesFromTask({ parts: "not-an-array" } as unknown as Record<string, unknown>)).toEqual([]);
  });

  it("walks result.message.parts — the non-task reply shape some A2A servers use", () => {
    const task = {
      message: {
        parts: [
          { kind: "file", file: { name: "out.txt", uri: "workspace:/workspace/out.txt" } },
        ],
      },
    };
    const files = extractFilesFromTask(task);
    expect(files[0]).toMatchObject({ name: "out.txt", uri: "workspace:/workspace/out.txt" });
  });

  // a2a-sdk v1 protobuf flattens file parts: no `kind`, no nested `file`,
  // top-level `url` + `filename` + `mediaType` instead. Every workspace
  // runtime since the SDK migration emits this shape, so the canvas
  // chat parser must surface them or chips silently disappear from
  // agent replies. Pinning here so a parser refactor can't regress
  // back to v0-only and lose the new wire format.
  it("pulls v1 protobuf file parts (flat url/filename/mediaType, no kind)", () => {
    const task = {
      parts: [
        { kind: "text", text: "here's the screenshot" },
        {
          url: "workspace:/screenshots/run-42.png",
          filename: "run-42.png",
          mediaType: "image/png",
        },
      ],
    };
    const files = extractFilesFromTask(task);
    expect(files).toEqual([
      {
        name: "run-42.png",
        uri: "workspace:/screenshots/run-42.png",
        mimeType: "image/png",
        size: undefined,
      },
    ]);
  });

  it("recovers a filename from the URI on v1 file parts when filename is absent", () => {
    const task = {
      parts: [{ url: "workspace:/workspace/out/graph.png" }],
    };
    expect(extractFilesFromTask(task)[0].name).toBe("graph.png");
  });

  it("hydrates a notify-with-attachments response_body — both text caption AND file chips", () => {
    // Pins the exact wire shape the platform's Notify handler persists
    // when send_message_to_user passes attachments (activity.go writes
    // response_body = {"result": <message>, "parts": [{kind:"file",...}]}).
    // The chat history loader runs both extractors over this object on
    // reload — without this contract holding, refreshing the page after
    // an agent attached a file would lose either the caption or the chips.
    const responseBody = {
      result: "Done — see attached.",
      parts: [
        {
          kind: "file",
          file: {
            name: "build-output.zip",
            mimeType: "application/zip",
            uri: "workspace:/tmp/build-output.zip",
            size: 12345,
          },
        },
      ],
    };
    expect(extractResponseText(responseBody)).toBe("Done — see attached.");
    expect(extractFilesFromTask(responseBody)).toEqual([
      {
        name: "build-output.zip",
        mimeType: "application/zip",
        uri: "workspace:/tmp/build-output.zip",
        size: 12345,
      },
    ]);
  });
});

describe("isInternalSelfRequest self-source classification", () => {
  // A2A request_body shape the runtime persists: the source_type marker
  // rides on params.metadata (sibling of params.message).
  const selfBody = (sourceType: string) => ({
    params: {
      metadata: { source_type: sourceType },
      message: { parts: [{ kind: "text", text: "wake nudge" }] },
    },
  });

  // Enumeration of the self-source markers, used only to drive the per-marker
  // classification behavior tests below (each marker must be honored as an
  // internal self-message). This is NOT the drift guard — a hardcoded list here
  // can only agree with itself. The genuine SSOT guard is the vendor-parity
  // test at the end of this block (SELF_SOURCE_TYPES == the generated list),
  // backed by the SDK's contracts-codegen-drift gate on the upstream schema.
  const SELF_MARKERS = [
    "self-cron",
    "self-harvester",
    "self-idle",
    "self-scheduler",
    "self-goal-nudge",
    "self-delegation-result",
    "self-warmup",
    "self-restart-context",
    "self-first-boot-greet",
    "self-lifecycle",
    "self-stall",
    "self-nudge",
  ];

  it.each(SELF_MARKERS)(
    "classifies %s as an internal self-message (system notice, not user)",
    (marker) => {
      expect(SELF_SOURCE_TYPES.has(marker)).toBe(true);
      expect(isInternalSelfRequest(selfBody(marker), "wake nudge")).toBe(true);
    },
  );

  // Regression pins for the markers this change added (canvas had drifted
  // behind Go and was missing these five).
  it.each([
    "self-restart-context",
    "self-first-boot-greet",
    "self-lifecycle",
    "self-stall",
    "self-nudge",
  ])("newly-aligned marker %s is honored as self-source", (marker) => {
    expect(isInternalSelfRequest(selfBody(marker), "")).toBe(true);
  });

  // SSOT vendor-parity guard (RFC follow-up #29). The marker set is defined
  // ONCE in the SDK contract
  // (molecule-ai-sdk contracts/workspace-comms/self-source-types.schema.json)
  // and codegen'd into the Go `molcontracts.SelfSourceTypes` (consumed by
  // workspace-server/internal/messagestore) and the vendored TS list
  // ./self-source-types.generated.ts. This test asserts the runtime-facing
  // SELF_SOURCE_TYPES set is EXACTLY that generated list (both directions), so
  // message-parser.ts can never drift from the vendored SSOT. Upstream drift is
  // caught by the SDK's contracts-codegen-drift gate; the values below flow
  // from the schema, not a hand-written copy in this repo.
  //
  // (This replaces the previous guard that hand-parsed the Go `selfSourceTypes`
  // map literal from postgres_store.go — that map no longer exists; the Go
  // consumer now delegates to the generated `molcontracts.IsSelfSourceType`.)
  it("SELF_SOURCE_TYPES matches the generated self-source SSOT exactly (no drift)", () => {
    // Sanity: the generated list must be non-empty, else this guard could
    // vacuously pass if the vendored module were emptied.
    expect(GENERATED_SELF_SOURCE_TYPES.length).toBeGreaterThan(0);
    // The vendored list carries no duplicates.
    expect(new Set(GENERATED_SELF_SOURCE_TYPES).size).toBe(
      GENERATED_SELF_SOURCE_TYPES.length,
    );
    // Exact set equality, both directions: no marker in the generated list
    // missing from the runtime Set, and none present in the Set but absent
    // from the generated list.
    expect([...SELF_SOURCE_TYPES].sort()).toEqual(
      [...GENERATED_SELF_SOURCE_TYPES].sort(),
    );
  });

  it("treats a tagged non-self source_type as a genuine user turn", () => {
    const body = {
      params: {
        metadata: { source_type: "user-typed" },
        message: { parts: [{ kind: "text", text: "hello" }] },
      },
    };
    expect(isInternalSelfRequest(body, "hello")).toBe(false);
  });

  it("classifies an untagged legacy delegation-result row via the text prefix", () => {
    // No source_type marker (legacy row) → falls back to the deprecated
    // text-prefix classifier.
    expect(
      isInternalSelfRequest(null, "Delegation results are ready. Review them."),
    ).toBe(true);
  });

  it("leaves an untagged ordinary user message as a user turn", () => {
    expect(isInternalSelfRequest(null, "can you deploy staging?")).toBe(false);
  });
});
