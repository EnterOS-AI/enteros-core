// @vitest-environment jsdom
//
// Regression tests for the org concierge (platform agent) Config tab — the
// "moonshot -> minimax" DISPLAY fix.
//
// The platform agent (kind='platform', runtime=claude-code) legitimately ships
// NO editable platform config.yaml: its model is INHERITED from the SSOT default
// (MOLECULE_LLM_DEFAULT_MODEL) and seeded as the MODEL/MOLECULE_MODEL container
// env by core's ensureConciergeModel, surfaced to this form via GET /model.
//
// Two bugs this suite pins (both observed on prod reno-stars):
//   1. the Config tab showed a scary red "No config.yaml found" banner for the
//      concierge, which has none BY DESIGN; and
//   2. the model line showed the dead template pin (moonshot/kimi-k2.6) instead
//      of the resolved runtime model (minimax/...). The resolved model arrives
//      via GET /workspaces/:id/model and MUST win over any (absent or stale-
//      pinned) config.yaml.

import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import { render, screen, cleanup, waitFor } from "@testing-library/react";
import React from "react";

afterEach(cleanup);

// ── API mock ──────────────────────────────────────────────────────────
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

// ── Canvas store mock ────────────────────────────────────────────────
// Unlike the other ConfigTab suites, this one carries a `nodes` array with a
// kind='platform' node so ConfigTab's platform-agent detection
// (s.nodes.find(...).data.kind === WORKSPACE_KIND.Platform) fires. Both the
// selector-call form and getState() return the same state object.
const platformState = {
  nodes: [{ id: "ws-test", data: { kind: "platform" } }],
  restartWorkspace: vi.fn(),
  updateNodeData: vi.fn(),
};
vi.mock("@/store/canvas", () => ({
  useCanvasStore: Object.assign(
    (selector: (s: unknown) => unknown) => selector(platformState),
    { getState: () => platformState },
  ),
}));

import { ConfigTab } from "../ConfigTab";

function wirePlatformAgent(opts: {
  workspaceModel?: string;
  configYamlContent?: string | null; // null = 404 (no editable config.yaml)
}) {
  apiGet.mockImplementation((path: string) => {
    if (path === "/workspaces/ws-test") {
      return Promise.resolve({ runtime: "claude-code", tier: 2 });
    }
    if (path === "/workspaces/ws-test/model") {
      return Promise.resolve({
        model: opts.workspaceModel ?? "",
        source: "workspace_secrets",
      });
    }
    if (path === "/workspaces/ws-test/files/config.yaml") {
      if (opts.configYamlContent === null) {
        return Promise.reject(new Error("not found"));
      }
      return Promise.resolve({ content: opts.configYamlContent ?? "" });
    }
    if (path === "/templates") {
      return Promise.resolve([
        { id: "t-cc", name: "Claude Code", runtime: "claude-code", models: [] },
      ]);
    }
    return Promise.reject(new Error(`unmocked api.get: ${path}`));
  });
  apiPatch.mockResolvedValue({});
  apiPut.mockResolvedValue({});
}

beforeEach(() => {
  apiGet.mockReset();
  apiPatch.mockReset();
  apiPut.mockReset();
});

describe("ConfigTab — platform agent (org concierge) model display", () => {
  it("does NOT show 'No config.yaml found' for the concierge (it has none by design)", async () => {
    wirePlatformAgent({
      workspaceModel: "minimax/MiniMax-M2",
      configYamlContent: null, // concierge ships no editable config.yaml
    });

    render(<ConfigTab workspaceId="ws-test" />);

    // Wait for load to settle (Model input renders once /templates resolves).
    await waitFor(() => screen.getByLabelText("Model"));
    expect(screen.queryByText(/No config\.yaml found/i)).toBeNull();
  });

  it("shows the RESOLVED model (minimax), never the dead moonshot pin", async () => {
    wirePlatformAgent({
      workspaceModel: "minimax/MiniMax-M2",
      configYamlContent: null,
    });

    render(<ConfigTab workspaceId="ws-test" />);

    const modelInput = (await waitFor(() =>
      screen.getByLabelText("Model"),
    )) as HTMLInputElement;
    expect(modelInput.value).toBe("minimax/MiniMax-M2");
    expect(modelInput.value).not.toMatch(/moonshot/i);
    // Belt-and-suspenders: moonshot must not appear anywhere in the panel.
    expect(screen.queryByText(/moonshot/i)).toBeNull();
  });

  it("surfaces the inherited-model note instead of the scary banner", async () => {
    wirePlatformAgent({
      workspaceModel: "minimax/MiniMax-M2",
      configYamlContent: null,
    });

    render(<ConfigTab workspaceId="ws-test" />);

    await waitFor(() => screen.getByLabelText("Model"));
    expect(
      screen.getByText(/inherits its model from the platform default/i),
    ).toBeTruthy();
  });

  it("does NOT surface the hardcoded DEFAULT_CONFIG version ('1.0.0') for the concierge", async () => {
    // The concierge ships no editable config.yaml, so the form falls into the
    // no-config branch and spreads DEFAULT_CONFIG — whose version "1.0.0" is a
    // UI placeholder, NOT a real concierge version (no SSOT backs it). It must
    // be blanked rather than imply a version the platform never declared.
    wirePlatformAgent({
      workspaceModel: "minimax/MiniMax-M2",
      configYamlContent: null,
    });

    render(<ConfigTab workspaceId="ws-test" />);

    await waitFor(() => screen.getByLabelText("Model"));
    const versionInput = screen.getByLabelText("Version") as HTMLInputElement;
    expect(versionInput.value).toBe("");
    expect(versionInput.value).not.toBe("1.0.0");
  });

  it("GET /model (resolved minimax) WINS over a stale moonshot config.yaml pin", async () => {
    // Even if the on-box config.yaml still carries the dead vendor pin, the
    // resolved MODEL secret (GET /model) must be what the form displays.
    wirePlatformAgent({
      workspaceModel: "minimax/MiniMax-M2",
      configYamlContent:
        "runtime: claude-code\nmodel: moonshot/kimi-k2.6\nruntime_config:\n  model: moonshot/kimi-k2.6\n",
    });

    render(<ConfigTab workspaceId="ws-test" />);

    const modelInput = (await waitFor(() =>
      screen.getByLabelText("Model"),
    )) as HTMLInputElement;
    expect(modelInput.value).toBe("minimax/MiniMax-M2");
    expect(modelInput.value).not.toMatch(/moonshot/i);
    // config.yaml is present here, so the red error must also be absent.
    expect(screen.queryByText(/No config\.yaml found/i)).toBeNull();
  });
});
