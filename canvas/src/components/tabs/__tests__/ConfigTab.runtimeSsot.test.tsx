// @vitest-environment jsdom
//
// Regression: project_canvas_runtime_dropdown_ssot_fix — a backend-served
// workspace's Config tab showed the wrong runtime (a stale first-option
// default) because a hardcoded frontend allowlist
// (SUPPORTED_RUNTIME_VALUES) dropped a runtime from the /templates-derived
// options even though the backend served it. A Save from that state would
// PATCH runtime to the wrong value and break the agent.
//
// The fix: the dropdown is SSOT-driven — it trusts GET /templates (which the
// backend already gates to the manifest maintained set) and hides a runtime
// only when its row carries `displayable: false`. This pins: a backend-served
// workspace shows its runtime selected, and a displayable:false template is
// not offered.
import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import { render, screen, cleanup, waitFor } from "@testing-library/react";
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
    (selector: (s: unknown) => unknown) => selector({ restartWorkspace: vi.fn(), updateNodeData: vi.fn() }),
    { getState: () => ({ restartWorkspace: vi.fn(), updateNodeData: vi.fn() }) },
  ),
}));

vi.mock("../AgentCardSection", () => ({
  AgentCardSection: () => <div data-testid="agent-card-stub" />,
}));

import { ConfigTab } from "../ConfigTab";

function wireApi(templates: Array<{ id: string; name?: string; runtime?: string; models?: unknown[]; displayable?: boolean }>) {
  apiGet.mockImplementation((path: string) => {
    if (path === "/workspaces/ws-crewai") return Promise.resolve({ runtime: "crewai" });
    if (path === "/workspaces/ws-crewai/model") return Promise.resolve({ model: "crew-model" });
    if (path === "/workspaces/ws-crewai/files/config.yaml") return Promise.resolve({ content: "name: crew\nruntime: crewai\n" });
    if (path === "/templates") return Promise.resolve(templates);
    return Promise.reject(new Error(`unmocked api.get: ${path}`));
  });
}

beforeEach(() => {
  apiGet.mockReset();
  apiPatch.mockReset();
  apiPut.mockReset();
});

describe("ConfigTab — runtime dropdown is /templates-driven", () => {
  it("shows a backend-served runtime selected in the runtime dropdown (#ssot-fix)", async () => {
    wireApi([
      { id: "claude-code", name: "Claude Code", runtime: "claude-code", models: [] },
      { id: "crewai", name: "CrewAI", runtime: "crewai", models: [] },
    ]);
    render(<ConfigTab workspaceId="ws-crewai" />);
    const select = await waitFor(() => screen.getByRole("combobox", { name: /runtime/i }));
    expect((select as HTMLSelectElement).value).toBe("crewai");
    const opts = Array.from((select as HTMLSelectElement).options).map((o) => o.value);
    expect(opts).toContain("crewai");
  });

  it("hides a template flagged displayable:false", async () => {
    wireApi([
      { id: "crewai", name: "CrewAI", runtime: "crewai", models: [] },
      { id: "legacy", name: "Legacy", runtime: "legacy", models: [], displayable: false },
    ]);
    render(<ConfigTab workspaceId="ws-crewai" />);
    const select = await waitFor(() => screen.getByRole("combobox", { name: /runtime/i }));
    const opts = Array.from((select as HTMLSelectElement).options).map((o) => o.value);
    expect(opts).toContain("crewai");
    expect(opts).not.toContain("legacy");
  });
});
