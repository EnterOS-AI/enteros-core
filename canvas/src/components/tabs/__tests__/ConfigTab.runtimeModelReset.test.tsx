// @vitest-environment jsdom
//
// Regression: ux/configtab-runtime-resets-model.
//
// Bug: in the Config tab, changing the Runtime did NOT reset the Model. The
// backend validates the (runtime, model) pair atomically on Save — if the
// stale model isn't registered for the newly-selected runtime it returns 422
// (`model "X" is not a registered model for runtime "Y"`) and the runtime
// SILENTLY rolls back. Repro: switch to `google-adk` while the model is a
// claude-code-only model (e.g. moonshot/kimi-k2.6) → 422 → runtime stays
// claude-code. The user thinks they switched but nothing changed.
//
// Fix: on runtime change the form resets the model to a valid default for the
// new runtime, constrains the model dropdown to that runtime's registered
// models, makes the reset visible, and blocks Save on an invalid pair for
// registry-backed runtimes. This suite pins:
//   (a) changing runtime resets the model to one valid for the new runtime,
//   (b) an invalid (runtime, model) pair can't be submitted (Save disabled),
//   both for google-adk.

import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import { render, screen, cleanup, waitFor, fireEvent } from "@testing-library/react";
import React from "react";

afterEach(cleanup);

const apiGet = vi.fn();
const apiPatch = vi.fn();
const apiPut = vi.fn();
vi.mock("@/lib/api", () => ({
  api: {
    get: (path: string) => apiGet(path),
    patch: (path: string, body: unknown) => apiPatch(path, body),
    put: (path: string, body: unknown) => apiPut(path, body),
    post: vi.fn(),
    del: vi.fn(),
  },
}));

vi.mock("@/store/canvas", () => ({
  useCanvasStore: Object.assign(
    (selector: (s: unknown) => unknown) =>
      selector({ restartWorkspace: vi.fn(), updateNodeData: vi.fn(), nodes: [] }),
    {
      getState: () => ({
        restartWorkspace: vi.fn().mockResolvedValue(undefined),
        updateNodeData: vi.fn(),
        nodes: [],
      }),
    },
  ),
}));

vi.mock("../AgentCardSection", () => ({
  AgentCardSection: () => <div data-testid="agent-card-stub" />,
}));

import { ConfigTab, modelIdsForRuntime, defaultModelForRuntime } from "../ConfigTab";

// /templates payload mirroring registry_gen.go `Runtimes`:
//   - claude-code (registry-backed): includes moonshot/kimi-k2.6
//   - google-adk (registry-backed): platform:gemini-2.5-pro / -flash only
const TEMPLATES = [
  {
    id: "t-claude-code",
    name: "Claude Code",
    runtime: "claude-code",
    registry_backed: true,
    registry_providers: [
      { name: "platform", display_name: "Platform" },
    ],
    registry_models: [
      { id: "moonshot/kimi-k2.6", name: "Kimi K2.6", provider: "platform" },
      { id: "anthropic/claude-opus-4-8", name: "Claude Opus 4.8", provider: "platform" },
    ],
  },
  {
    id: "t-google-adk",
    name: "Google ADK",
    runtime: "google-adk",
    registry_backed: true,
    registry_providers: [
      { name: "platform", display_name: "Platform" },
    ],
    registry_models: [
      { id: "platform:gemini-2.5-pro", name: "Gemini 2.5 Pro", provider: "platform" },
      { id: "platform:gemini-2.5-flash", name: "Gemini 2.5 Flash", provider: "platform" },
    ],
  },
];

// A claude-code workspace running moonshot/kimi-k2.6 — the exact repro state.
function wireClaudeCodeWorkspace() {
  apiGet.mockImplementation((path: string) => {
    if (path === "/workspaces/ws-cc") return Promise.resolve({ runtime: "claude-code", tier: 2 });
    if (path === "/workspaces/ws-cc/model")
      return Promise.resolve({ model: "moonshot/kimi-k2.6", source: "workspace_secrets" });
    if (path === "/workspaces/ws-cc/files/config.yaml")
      return Promise.resolve({
        content: "name: cc\nruntime: claude-code\nruntime_config:\n  model: moonshot/kimi-k2.6\n",
      });
    if (path === "/templates") return Promise.resolve(TEMPLATES);
    return Promise.reject(new Error(`unmocked api.get: ${path}`));
  });
  apiPut.mockResolvedValue({});
  apiPatch.mockResolvedValue({});
}

beforeEach(() => {
  apiGet.mockReset();
  apiPatch.mockReset();
  apiPut.mockReset();
});

describe("modelIdsForRuntime / defaultModelForRuntime helpers", () => {
  it("returns the registry model ids for a registry-backed runtime", () => {
    const opt = {
      value: "google-adk",
      label: "Google ADK",
      models: [],
      providers: [],
      registryBacked: true,
      registryProviders: [],
      registryModels: [
        { id: "platform:gemini-2.5-pro" },
        { id: "platform:gemini-2.5-flash" },
      ],
    };
    expect(modelIdsForRuntime(opt)).toEqual([
      "platform:gemini-2.5-pro",
      "platform:gemini-2.5-flash",
    ]);
    expect(defaultModelForRuntime(opt)).toBe("platform:gemini-2.5-pro");
  });

  it("falls back to template models[] for a non-registry runtime and skips wildcards", () => {
    const opt = {
      value: "hermes",
      label: "Hermes",
      models: [{ id: "openrouter/*" }, { id: "nousresearch/hermes-4-70b" }],
      providers: [],
      registryBacked: false,
      registryProviders: [],
      registryModels: [],
    };
    expect(modelIdsForRuntime(opt)).toEqual(["nousresearch/hermes-4-70b"]);
    expect(defaultModelForRuntime(opt)).toBe("nousresearch/hermes-4-70b");
  });

  it("returns empty for a null / model-less runtime", () => {
    expect(modelIdsForRuntime(null)).toEqual([]);
    expect(defaultModelForRuntime(undefined)).toBe("");
  });
});

describe("ConfigTab — runtime change resets model (google-adk)", () => {
  it("(a) switching claude-code → google-adk resets the model to a valid google-adk model", async () => {
    wireClaudeCodeWorkspace();
    render(<ConfigTab workspaceId="ws-cc" />);

    const runtimeSelect = (await waitFor(() =>
      screen.getByRole("combobox", { name: /runtime/i }),
    )) as HTMLSelectElement;
    // Wait for /templates to populate so registry_models are available.
    await waitFor(() =>
      expect(
        Array.from(runtimeSelect.options).map((o) => o.value),
      ).toContain("google-adk"),
    );

    // Sanity: the model dropdown currently shows the claude-code model.
    const modelSelectBefore = (await waitFor(() =>
      screen.getByTestId("model-select"),
    )) as HTMLSelectElement;
    expect(modelSelectBefore.value).toBe("moonshot/kimi-k2.6");

    // Switch runtime to google-adk.
    fireEvent.change(runtimeSelect, { target: { value: "google-adk" } });

    // The model must reset to a google-adk-registered model, and the dropdown
    // must only offer google-adk models (no kimi).
    const modelSelectAfter = (await waitFor(() =>
      screen.getByTestId("model-select"),
    )) as HTMLSelectElement;
    await waitFor(() =>
      expect(modelSelectAfter.value).toBe("platform:gemini-2.5-pro"),
    );
    const optsAfter = Array.from(modelSelectAfter.options).map((o) => o.value);
    expect(optsAfter).toContain("platform:gemini-2.5-pro");
    expect(optsAfter).not.toContain("moonshot/kimi-k2.6");

    // The reset is VISIBLE.
    const note = await waitFor(() => screen.getByTestId("model-reset-note"));
    expect(note.textContent).toMatch(/platform:gemini-2\.5-pro/);
    expect(note.textContent).toMatch(/moonshot\/kimi-k2\.6/);
  });

  it("(a') Save after the runtime switch PUTs a valid google-adk model — never the stale kimi model", async () => {
    wireClaudeCodeWorkspace();
    render(<ConfigTab workspaceId="ws-cc" />);

    const runtimeSelect = (await waitFor(() =>
      screen.getByRole("combobox", { name: /runtime/i }),
    )) as HTMLSelectElement;
    await waitFor(() =>
      expect(
        Array.from(runtimeSelect.options).map((o) => o.value),
      ).toContain("google-adk"),
    );

    fireEvent.change(runtimeSelect, { target: { value: "google-adk" } });
    await waitFor(() =>
      expect(
        (screen.getByTestId("model-select") as HTMLSelectElement).value,
      ).toBe("platform:gemini-2.5-pro"),
    );

    fireEvent.click(screen.getByRole("button", { name: /save & restart/i }));

    // runtime PATCH carries google-adk; model PUT carries a google-adk model.
    await waitFor(() =>
      expect(apiPatch).toHaveBeenCalledWith(
        "/workspaces/ws-cc",
        expect.objectContaining({ runtime: "google-adk" }),
      ),
    );
    await waitFor(() =>
      expect(apiPut).toHaveBeenCalledWith("/workspaces/ws-cc/model", {
        model: "platform:gemini-2.5-pro",
      }),
    );
    // The stale claude-code model must NEVER be PUT for the new runtime.
    const modelPuts = apiPut.mock.calls.filter(
      ([p]) => p === "/workspaces/ws-cc/model",
    );
    for (const [, body] of modelPuts) {
      expect((body as { model: string }).model).not.toBe("moonshot/kimi-k2.6");
    }
  });

  it("(b) an invalid (runtime, model) pair can't be submitted — Save is disabled", async () => {
    // Drive the form into a raw-YAML invalid pair: runtime=google-adk with a
    // claude-code-only model. This is the path the selector won't produce but
    // a raw edit can. Save must be blocked (modelPairInvalid) so the 422 +
    // silent rollback can't happen.
    apiGet.mockImplementation((path: string) => {
      if (path === "/workspaces/ws-bad") return Promise.resolve({ runtime: "google-adk", tier: 2 });
      if (path === "/workspaces/ws-bad/model")
        return Promise.resolve({ model: "platform:gemini-2.5-pro", source: "workspace_secrets" });
      if (path === "/workspaces/ws-bad/files/config.yaml")
        return Promise.resolve({
          content:
            "name: bad\nruntime: google-adk\nruntime_config:\n  model: platform:gemini-2.5-pro\n",
        });
      if (path === "/templates") return Promise.resolve(TEMPLATES);
      return Promise.reject(new Error(`unmocked api.get: ${path}`));
    });
    apiPut.mockResolvedValue({});
    apiPatch.mockResolvedValue({});

    render(<ConfigTab workspaceId="ws-bad" />);

    const runtimeSelect = (await waitFor(() =>
      screen.getByRole("combobox", { name: /runtime/i }),
    )) as HTMLSelectElement;
    await waitFor(() =>
      expect(
        Array.from(runtimeSelect.options).map((o) => o.value),
      ).toContain("google-adk"),
    );

    // Flip into raw-YAML mode and inject the invalid pair.
    fireEvent.click(screen.getByLabelText(/raw yaml/i));
    const rawEditor = (await waitFor(() =>
      screen.getByLabelText(/raw yaml editor/i),
    )) as HTMLTextAreaElement;
    fireEvent.change(rawEditor, {
      target: {
        value:
          "name: bad\nruntime: google-adk\nruntime_config:\n  model: moonshot/kimi-k2.6\n",
      },
    });

    // Save & Restart must be disabled — the (google-adk, moonshot/kimi-k2.6)
    // pair is invalid for this registry-backed runtime.
    const saveBtn = screen.getByRole("button", { name: /save & restart/i });
    await waitFor(() => expect((saveBtn as HTMLButtonElement).disabled).toBe(true));

    // And no model PUT can fire while it stays invalid.
    fireEvent.click(saveBtn);
    const modelPuts = apiPut.mock.calls.filter(
      ([p]) => p === "/workspaces/ws-bad/model",
    );
    expect(modelPuts).toHaveLength(0);
  });
});
