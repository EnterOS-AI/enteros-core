// @vitest-environment jsdom
/**
 * Gate matrix for the self-host setup scene (gate.ts) — the
 * fail-closed-to-invisible contract: the scene renders only on POSITIVE
 * confirmation of every signal; any error/ambiguity resolves to hidden.
 */
import { beforeEach, describe, expect, it, vi } from "vitest";

// defaultGateDeps wiring is tested against mocked api/tenant modules.
vi.mock("@/lib/api", () => ({
  api: { get: vi.fn(), post: vi.fn(), put: vi.fn(), patch: vi.fn(), del: vi.fn() },
}));
vi.mock("@/lib/tenant", () => ({ getTenantSlug: vi.fn(() => "") }));

import { api } from "@/lib/api";
import { getTenantSlug } from "@/lib/tenant";
import { useCanvasStore } from "@/store/canvas";
import {
  defaultGateDeps,
  evaluateSelfHostSetupGate,
  type GateDeps,
} from "../gate";

const TEMPLATES = [
  {
    id: "tpl-claude",
    name: "Claude Code",
    runtime: "claude-code",
    registry_backed: true,
    registry_providers: [
      { name: "anthropic-api", display_name: "Anthropic API", auth_env: ["ANTHROPIC_API_KEY"] },
    ],
    registry_models: [{ id: "claude-opus-4-7", provider: "anthropic-api" }],
  },
];

function deps(overrides: Partial<GateDeps> = {}): GateDeps {
  return {
    getSlug: () => "",
    fetchIdentity: async () => ({ org_id: "" }),
    fetchTemplates: async () => TEMPLATES,
    fetchSecrets: async () => [],
    getPlatformNode: () => ({
      id: "root-1",
      data: { status: "offline", runtime: "claude-code", lastSampleError: "" },
    }),
    ...overrides,
  };
}

describe("evaluateSelfHostSetupGate — hidden paths (fail-closed)", () => {
  it("hides when a tenant slug is derived (SaaS host)", async () => {
    expect(
      await evaluateSelfHostSetupGate(deps({ getSlug: () => "acme" })),
    ).toEqual({ show: false });
  });

  it("hides when /org/identity errors", async () => {
    expect(
      await evaluateSelfHostSetupGate(
        deps({ fetchIdentity: async () => Promise.reject(new Error("timeout")) }),
      ),
    ).toEqual({ show: false });
  });

  it("hides when org_id is set (CP-provisioned tenant)", async () => {
    expect(
      await evaluateSelfHostSetupGate(
        deps({ fetchIdentity: async () => ({ org_id: "org-123" }) }),
      ),
    ).toEqual({ show: false });
  });

  it("hides on an ambiguous identity payload (org_id absent / body null)", async () => {
    expect(
      await evaluateSelfHostSetupGate(deps({ fetchIdentity: async () => ({}) })),
    ).toEqual({ show: false });
    expect(
      await evaluateSelfHostSetupGate(
        deps({
          fetchIdentity: async () =>
            null as unknown as { org_id?: unknown },
        }),
      ),
    ).toEqual({ show: false });
  });

  it("hides when templates or secrets fetch fails", async () => {
    expect(
      await evaluateSelfHostSetupGate(
        deps({ fetchTemplates: async () => Promise.reject(new Error("503")) }),
      ),
    ).toEqual({ show: false });
    expect(
      await evaluateSelfHostSetupGate(
        deps({ fetchSecrets: async () => Promise.reject(new Error("503")) }),
      ),
    ).toEqual({ show: false });
  });

  it("hides on non-array templates/secrets payloads (ambiguity)", async () => {
    expect(
      await evaluateSelfHostSetupGate(
        deps({
          fetchTemplates: async () =>
            null as unknown as ReturnType<GateDeps["fetchTemplates"]> extends Promise<infer T> ? T : never,
        }),
      ),
    ).toEqual({ show: false });
    expect(
      await evaluateSelfHostSetupGate(
        deps({
          fetchSecrets: async () =>
            null as unknown as Array<{ key: string; has_value?: boolean }>,
        }),
      ),
    ).toEqual({ show: false });
  });

  it("hides when no offerable runtime can be derived from /templates", async () => {
    expect(
      await evaluateSelfHostSetupGate(
        deps({
          fetchTemplates: async () => [
            { runtime: "claude-code", displayable: false },
          ],
        }),
      ),
    ).toEqual({ show: false });
  });

  it("hides when the platform root has been online (active-ish statuses)", async () => {
    for (const status of ["online", "degraded", "paused", "hibernating", "hibernated"]) {
      expect(
        await evaluateSelfHostSetupGate(
          deps({
            getPlatformNode: () => ({ id: "root-1", data: { status } }),
          }),
        ),
      ).toEqual({ show: false });
    }
  });

  it("hides when an LLM key is already configured (root present)", async () => {
    expect(
      await evaluateSelfHostSetupGate(
        deps({
          fetchSecrets: async () => [
            { key: "ANTHROPIC_API_KEY", has_value: true },
          ],
        }),
      ),
    ).toEqual({ show: false });
  });

  it("hides when an LLM key is configured even with the root missing", async () => {
    expect(
      await evaluateSelfHostSetupGate(
        deps({
          getPlatformNode: () => null,
          fetchSecrets: async () => [
            { key: "ANTHROPIC_API_KEY", has_value: true },
          ],
        }),
      ),
    ).toEqual({ show: false });
  });
});

describe("evaluateSelfHostSetupGate — shown paths", () => {
  it("shows for an unconfigured root and captures the scene context", async () => {
    const result = await evaluateSelfHostSetupGate(
      deps({
        fetchSecrets: async () => [
          // Non-LLM key + a value-less LLM row: neither counts as configured.
          { key: "GITHUB_TOKEN", has_value: true },
          { key: "ANTHROPIC_API_KEY", has_value: false },
          { key: "MYSTERY" },
        ],
      }),
    );
    expect(result.show).toBe(true);
    if (!result.show) throw new Error("unreachable");
    expect(result.context).toEqual({
      rootId: "root-1",
      rootRuntime: "claude-code",
      rootStatus: "offline",
      rootLastError: "",
      runtimeOptions: expect.arrayContaining([
        expect.objectContaining({ value: "claude-code" }),
      ]),
      configuredKeys: new Set(["GITHUB_TOKEN"]),
    });
  });

  it("defaults absent node fields to empty strings", async () => {
    const result = await evaluateSelfHostSetupGate(
      deps({ getPlatformNode: () => ({ id: "root-1", data: {} }) }),
    );
    expect(result.show).toBe(true);
    if (!result.show) throw new Error("unreachable");
    expect(result.context.rootRuntime).toBe("");
    expect(result.context.rootStatus).toBe("");
    expect(result.context.rootLastError).toBe("");
  });

  it("shows defensively when the platform root is missing entirely", async () => {
    const result = await evaluateSelfHostSetupGate(
      deps({ getPlatformNode: () => null }),
    );
    expect(result.show).toBe(true);
    if (!result.show) throw new Error("unreachable");
    expect(result.context.rootId).toBeNull();
    expect(result.context.rootRuntime).toBe("");
  });
});

describe("defaultGateDeps wiring", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    useCanvasStore.setState({ nodes: [] });
  });

  it("routes fetches through the shared api module (platformAuthHeaders inside)", async () => {
    const get = api.get as ReturnType<typeof vi.fn>;
    get.mockResolvedValue([]);
    await defaultGateDeps.fetchIdentity();
    await defaultGateDeps.fetchTemplates();
    await defaultGateDeps.fetchSecrets();
    expect(get.mock.calls.map((c) => c[0])).toEqual([
      "/org/identity",
      "/templates",
      "/settings/secrets",
    ]);
  });

  it("reads the tenant slug via getTenantSlug", () => {
    (getTenantSlug as ReturnType<typeof vi.fn>).mockReturnValue("acme");
    expect(defaultGateDeps.getSlug()).toBe("acme");
  });

  it("resolves the platform node off the store by the kind marker (ConciergeShell signal)", () => {
    useCanvasStore.setState({
      nodes: [
        {
          id: "w1",
          position: { x: 0, y: 0 },
          data: { kind: "workspace", status: "online" },
        },
        {
          id: "root-1",
          position: { x: 0, y: 0 },
          data: {
            kind: "platform",
            status: "offline",
            runtime: "claude-code",
            lastSampleError: "err",
          },
        },
      ] as never,
    });
    expect(defaultGateDeps.getPlatformNode()).toEqual({
      id: "root-1",
      data: { status: "offline", runtime: "claude-code", lastSampleError: "err" },
    });
  });

  it("returns null when no platform node exists in the store", () => {
    expect(defaultGateDeps.getPlatformNode()).toBeNull();
  });
});
