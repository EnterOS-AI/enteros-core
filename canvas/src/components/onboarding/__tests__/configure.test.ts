/**
 * Wire-sequence tests for configure.ts — the §4 ordering is load-bearing
 * (key PUT → conditional runtime PATCH → ensure POST) and asserted here
 * with exact call order + exact bodies.
 */
import { describe, expect, it, vi } from "vitest";
import { runConfigureSequence, runEnsure, type ConfigureClient } from "../configure";

// The default client parameter is exercised by the SelfHostSetupScene
// component tests (which mock @/lib/api); here we inject a recorder.
function makeRecorder() {
  const calls: Array<{ method: string; path: string; body: unknown }> = [];
  const client: ConfigureClient = {
    put: vi.fn(async (path: string, body?: unknown) => {
      calls.push({ method: "PUT", path, body });
      return {};
    }),
    patch: vi.fn(async (path: string, body?: unknown) => {
      calls.push({ method: "PATCH", path, body });
      return {};
    }),
    post: vi.fn(async (path: string, body?: unknown) => {
      calls.push({ method: "POST", path, body });
      return {};
    }),
  };
  return { calls, client };
}

describe("runConfigureSequence", () => {
  it("runs PUT secret → PATCH runtime → POST ensure in exactly that order when the runtime changed", async () => {
    const { calls, client } = makeRecorder();
    await runConfigureSequence(
      {
        rootId: "root-1",
        seededRuntime: "claude-code",
        runtime: "codex",
        model: "gpt-5.4",
        keyName: "OPENAI_API_KEY",
        keyValue: "sk-test",
        skipKeyWrite: false,
      },
      client,
    );
    expect(calls).toEqual([
      {
        method: "PUT",
        path: "/settings/secrets",
        body: { key: "OPENAI_API_KEY", value: "sk-test" },
      },
      {
        method: "PATCH",
        path: "/workspaces/root-1",
        body: { runtime: "codex" },
      },
      {
        method: "POST",
        path: "/admin/org/platform-agent/ensure",
        body: { name: "Enter OS Agent", model: "gpt-5.4", force: true },
      },
    ]);
  });

  it("omits the PATCH when the runtime pick equals the seeded runtime (D2 conditional PATCH)", async () => {
    const { calls, client } = makeRecorder();
    await runConfigureSequence(
      {
        rootId: "root-1",
        seededRuntime: "claude-code",
        runtime: "claude-code",
        model: "claude-opus-4-7",
        keyName: "ANTHROPIC_API_KEY",
        keyValue: "sk-ant-x",
        skipKeyWrite: false,
      },
      client,
    );
    expect(calls.map((c) => c.method)).toEqual(["PUT", "POST"]);
  });

  it("skips the secret PUT when the key is already configured and untouched", async () => {
    const { calls, client } = makeRecorder();
    await runConfigureSequence(
      {
        rootId: "root-1",
        seededRuntime: "claude-code",
        runtime: "codex",
        model: "gpt-5.4",
        keyName: "OPENAI_API_KEY",
        keyValue: "",
        skipKeyWrite: true,
      },
      client,
    );
    expect(calls.map((c) => c.method)).toEqual(["PATCH", "POST"]);
  });

  it("missing root (defensive): no PATCH possible; ensure carries the runtime for the 'created' path", async () => {
    const { calls, client } = makeRecorder();
    await runConfigureSequence(
      {
        rootId: null,
        seededRuntime: "",
        runtime: "codex",
        model: "gpt-5.4",
        keyName: "OPENAI_API_KEY",
        keyValue: "sk-test",
        skipKeyWrite: false,
      },
      client,
    );
    expect(calls.map((c) => c.method)).toEqual(["PUT", "POST"]);
    expect(calls[1].body).toEqual({
      name: "Enter OS Agent",
      model: "gpt-5.4",
      force: true,
      runtime: "codex",
    });
  });

  it("propagates a failing step and never fires the later ones (order is load-bearing)", async () => {
    const { calls, client } = makeRecorder();
    (client.put as ReturnType<typeof vi.fn>).mockRejectedValueOnce(
      new Error("boom"),
    );
    await expect(
      runConfigureSequence(
        {
          rootId: "root-1",
          seededRuntime: "claude-code",
          runtime: "codex",
          model: "gpt-5.4",
          keyName: "OPENAI_API_KEY",
          keyValue: "sk-test",
          skipKeyWrite: false,
        },
        client,
      ),
    ).rejects.toThrow("boom");
    expect(calls).toEqual([]); // recorder logs after the rejection → nothing ran
    expect(client.patch).not.toHaveBeenCalled();
    expect(client.post).not.toHaveBeenCalled();
  });
});

describe("runEnsure", () => {
  it("posts the fixed brand name + force:true, omitting an empty model (resume-Retry path)", async () => {
    const { calls, client } = makeRecorder();
    await runEnsure("", null, client);
    expect(calls).toEqual([
      {
        method: "POST",
        path: "/admin/org/platform-agent/ensure",
        body: { name: "Enter OS Agent", force: true },
      },
    ]);
  });

  it("includes model and (for the created path) runtime when provided", async () => {
    const { calls, client } = makeRecorder();
    await runEnsure("claude-opus-4-7", "claude-code", client);
    expect(calls[0].body).toEqual({
      name: "Enter OS Agent",
      model: "claude-opus-4-7",
      force: true,
      runtime: "claude-code",
    });
  });
});
