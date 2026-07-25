// @vitest-environment jsdom
/**
 * Unit tests for the setup scene's pure logic (setup-scene-lib.ts) —
 * 100% line+branch per the operator coverage ruling (design SSOT §10.1).
 */
import { describe, expect, it, vi } from "vitest";
import { WORKSPACE_ERROR_CODES } from "@/lib/workspace-error-codes";
import { WORKSPACE_STATUS } from "@/lib/workspace-status";
import {
  PLATFORM_AGENT_NAME,
  bucketTemplatesByRuntime,
  buildSceneCatalog,
  collectAuthEnvNames,
  deriveResumeView,
  deriveSelectionForModel,
  extractErrorCode,
  extractHumanReason,
  focusFirstElement,
  focusableElements,
  handleFocusTrapKeyDown,
  hasConfiguredLLMKey,
  humanizeSetupError,
  isWatchTerminal,
  pickPlatformRow,
  statusIndicatesConfiguredRoot,
  type SceneRuntimeOption,
} from "../setup-scene-lib";

const registryOption: SceneRuntimeOption = {
  value: "claude-code",
  label: "Claude Code",
  models: [],
  registryBacked: true,
  registryProviders: [
    {
      name: "anthropic-api",
      display_name: "Anthropic API",
      auth_env: ["ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"],
    },
    { name: "minimax", display_name: "MiniMax", auth_env: ["MINIMAX_API_KEY"] },
  ],
  registryModels: [
    { id: "claude-opus-4-7", name: "Claude Opus 4.7", provider: "anthropic-api" },
    { id: "MiniMax-M2.7", provider: "minimax" },
  ],
};

const legacyOption: SceneRuntimeOption = {
  value: "hermes",
  label: "Hermes",
  models: [
    { id: "nousresearch/hermes-4-70b", required_env: ["NOUS_API_KEY"] },
  ],
  registryBacked: false,
  registryProviders: [],
  registryModels: [],
};

describe("PLATFORM_AGENT_NAME", () => {
  it("is the operator-ruled fixed brand name", () => {
    expect(PLATFORM_AGENT_NAME).toBe("Enter OS Agent");
  });
});

describe("bucketTemplatesByRuntime", () => {
  it("buckets rows by runtime, honoring displayable:false and skipping blank/external-like runtimes", () => {
    const out = bucketTemplatesByRuntime([
      { id: "a", name: "Claude Code", runtime: "claude-code", runtime_display_name: "Claude Code", models: [{ id: "m1" }] },
      { id: "b", runtime: "  " }, // blank after trim → skipped
      { id: "c", runtime: undefined }, // absent → skipped
      { id: "d", name: "Hidden", runtime: "codex", displayable: false }, // opt-out
      { id: "e", name: "External", runtime: "external" }, // external-like → skipped
      { id: "f", name: "Kimi", runtime: "kimi-cli" }, // external-like → skipped
    ]);
    expect(out.map((o) => o.value)).toEqual(["claude-code"]);
    expect(out[0].label).toBe("Claude Code");
    expect(out[0].models).toEqual([{ id: "m1" }]);
  });

  it("falls back to the runtime id as label and tolerates non-array payload fields", () => {
    const out = bucketTemplatesByRuntime([
      {
        runtime: "codex",
        models: undefined,
        registry_providers: undefined,
        registry_models: undefined,
      },
    ]);
    expect(out).toEqual([
      {
        value: "codex",
        label: "codex",
        models: [],
        registryBacked: false,
        registryProviders: [],
        registryModels: [],
      },
    ]);
  });

  it("prefers the richer payload when two templates share a runtime (registry-backed wins)", () => {
    const out = bucketTemplatesByRuntime([
      { id: "plain", name: "Plain", runtime: "claude-code", models: [{ id: "m1" }, { id: "m2" }] },
      {
        id: "rich",
        name: "Rich",
        runtime: "claude-code",
        registry_backed: true,
        registry_models: [{ id: "claude-opus-4-7", provider: "anthropic-api" }],
        registry_providers: [{ name: "anthropic-api" }],
      },
    ]);
    expect(out).toHaveLength(1);
    // Label comes from the runtime registry (runtime_display_name), NOT the
    // template name; neither row carries one here, so it falls back to the
    // runtime id. The richer row winning is proven by registryBacked.
    expect(out[0].label).toBe("claude-code");
    expect(out[0].registryBacked).toBe(true);
  });

  it("labels a runtime by its registry display_name, never a template name (SEO-agent regression)", () => {
    // Two templates share the claude-code runtime; one is the private
    // "SEO Agent". The runtime picker must show "Claude Code" (the registry
    // display_name), NOT whichever template wins the bucket — the bug where
    // claude-code surfaced as "SEO Agent".
    const out = bucketTemplatesByRuntime([
      { id: "concierge", name: "Platform Agent (Org Concierge)", runtime: "claude-code", runtime_display_name: "Claude Code", models: [{ id: "m1" }] },
      {
        id: "seo",
        name: "SEO Agent",
        runtime: "claude-code",
        runtime_display_name: "Claude Code",
        registry_backed: true,
        registry_models: [{ id: "claude-opus-4-8", provider: "anthropic-api" }],
        registry_providers: [{ name: "anthropic-api" }],
      },
    ]);
    expect(out).toHaveLength(1);
    expect(out[0].value).toBe("claude-code");
    expect(out[0].label).toBe("Claude Code");
    expect(out[0].label).not.toBe("SEO Agent");
  });

  it("keeps the first row on a score tie (no churn between equal templates)", () => {
    const out = bucketTemplatesByRuntime([
      { id: "first", name: "First", runtime: "codex", models: [{ id: "m1" }] },
      { id: "second", name: "Second", runtime: "codex", models: [{ id: "m2" }] },
    ]);
    // First row wins the tie — proven by its models surviving (the label is
    // now runtime-derived and identical for both rows, so it can't witness
    // which row won).
    expect(out[0].models).toEqual([{ id: "m1" }]);
  });

  it("treats registry_backed=true with zero registry models as NOT registry-backed", () => {
    const out = bucketTemplatesByRuntime([
      { runtime: "codex", registry_backed: true, registry_models: [] },
    ]);
    expect(out[0].registryBacked).toBe(false);
  });
});

describe("buildSceneCatalog", () => {
  it("uses the registry data layer for registry-backed runtimes", () => {
    const catalog = buildSceneCatalog(registryOption);
    expect(catalog.map((p) => p.label)).toEqual(["Anthropic API", "MiniMax"]);
    expect(catalog[0].envVars).toEqual([
      "ANTHROPIC_API_KEY",
      "ANTHROPIC_AUTH_TOKEN",
    ]);
    expect(catalog[0].models.map((m) => m.id)).toEqual(["claude-opus-4-7"]);
  });

  it("falls back to the legacy heuristic catalog when registry_models are absent", () => {
    const catalog = buildSceneCatalog(legacyOption);
    expect(catalog).toHaveLength(1);
    expect(catalog[0].vendor).toBe("nousresearch");
    expect(catalog[0].envVars).toEqual(["NOUS_API_KEY"]);
  });

  it("drops platform-managed providers defensively", () => {
    const catalog = buildSceneCatalog({
      ...registryOption,
      registryProviders: [
        { name: "platform", display_name: "Platform", auth_env: ["MOLECULE_LLM_USAGE_TOKEN"] },
        ...registryOption.registryProviders,
      ],
      registryModels: [
        { id: "platform-model", provider: "platform" },
        ...registryOption.registryModels,
      ],
    });
    expect(catalog.map((p) => p.vendor)).toEqual(["anthropic-api", "minimax"]);
  });

  it("strips wildcard models + clears the wildcard flag so free-text mode is unreachable", () => {
    const catalog = buildSceneCatalog({
      ...legacyOption,
      models: [
        { id: "openrouter/*", required_env: ["OPENROUTER_API_KEY"] },
        { id: "nousresearch/hermes-4-70b", required_env: ["NOUS_API_KEY"] },
      ],
    });
    // The openrouter bucket had ONLY a wildcard model → dropped entirely.
    expect(catalog.map((p) => p.vendor)).toEqual(["nousresearch"]);
    expect(catalog.every((p) => p.wildcard === false)).toBe(true);
    expect(
      catalog.flatMap((p) => p.models).some((m) => m.id.includes("*")),
    ).toBe(false);
  });
});

describe("collectAuthEnvNames / hasConfiguredLLMKey", () => {
  it("unions every offerable provider's auth envs", () => {
    const names = collectAuthEnvNames([registryOption, legacyOption]);
    expect([...names].sort()).toEqual([
      "ANTHROPIC_API_KEY",
      "ANTHROPIC_AUTH_TOKEN",
      "MINIMAX_API_KEY",
      "NOUS_API_KEY",
    ]);
  });

  it("hasConfiguredLLMKey matches only known auth envs", () => {
    const auth = new Set(["ANTHROPIC_API_KEY"]);
    expect(hasConfiguredLLMKey(new Set(["GITHUB_TOKEN"]), auth)).toBe(false);
    expect(
      hasConfiguredLLMKey(new Set(["GITHUB_TOKEN", "ANTHROPIC_API_KEY"]), auth),
    ).toBe(true);
    expect(hasConfiguredLLMKey(new Set(), auth)).toBe(false);
  });
});

describe("statusIndicatesConfiguredRoot", () => {
  it.each([
    WORKSPACE_STATUS.Online,
    WORKSPACE_STATUS.Degraded,
    WORKSPACE_STATUS.Paused,
    WORKSPACE_STATUS.Hibernating,
    WORKSPACE_STATUS.Hibernated,
  ])("true for %s (requires a past successful provision)", (status) => {
    expect(statusIndicatesConfiguredRoot(status)).toBe(true);
  });

  it.each([
    WORKSPACE_STATUS.Offline,
    WORKSPACE_STATUS.Failed,
    WORKSPACE_STATUS.Provisioning,
    WORKSPACE_STATUS.AwaitingAgent,
    WORKSPACE_STATUS.Removed,
    "",
  ])("false for %s", (status) => {
    expect(statusIndicatesConfiguredRoot(status)).toBe(false);
  });
});

describe("deriveResumeView", () => {
  it("provisioning resumes into progress", () => {
    expect(deriveResumeView(WORKSPACE_STATUS.Provisioning)).toBe("progress");
  });
  it("failed resumes into the error view", () => {
    expect(deriveResumeView(WORKSPACE_STATUS.Failed)).toBe("failed");
  });
  it("anything else starts the form", () => {
    expect(deriveResumeView(WORKSPACE_STATUS.Offline)).toBe("form");
    expect(deriveResumeView("")).toBe("form");
  });
});

describe("isWatchTerminal", () => {
  it("only online/failed terminate the watch", () => {
    expect(isWatchTerminal(WORKSPACE_STATUS.Online)).toBe(true);
    expect(isWatchTerminal(WORKSPACE_STATUS.Failed)).toBe(true);
    expect(isWatchTerminal(WORKSPACE_STATUS.Provisioning)).toBe(false);
  });
});

describe("pickPlatformRow", () => {
  const rows = [
    { id: "w1", kind: "workspace", status: "online" },
    { id: "root", kind: "platform", status: "offline" },
  ];
  it("prefers the id match when rootId is known", () => {
    expect(pickPlatformRow(rows, "w1")?.id).toBe("w1");
  });
  it("falls back to the kind='platform' marker when the id misses", () => {
    expect(pickPlatformRow(rows, "gone")?.id).toBe("root");
  });
  it("uses the kind marker when rootId is null", () => {
    expect(pickPlatformRow(rows, null)?.id).toBe("root");
  });
  it("returns null when no platform row exists", () => {
    expect(pickPlatformRow([{ id: "w1", status: "online" }], null)).toBeNull();
  });
});

describe("deriveSelectionForModel", () => {
  const catalog = buildSceneCatalog(registryOption);
  it("back-fills the provider for a known model (adopt-mode)", () => {
    expect(deriveSelectionForModel(catalog, "MiniMax-M2.7")).toEqual({
      providerId: "registry|minimax",
      model: "MiniMax-M2.7",
      envVars: ["MINIMAX_API_KEY"],
    });
  });
  it("returns null for an unknown model", () => {
    expect(deriveSelectionForModel(catalog, "nope")).toBeNull();
  });
});

describe("extractErrorCode", () => {
  it("finds a known code embedded in free text", () => {
    expect(
      extractErrorCode("provision abort: MISSING_PLATFORM_PROXY (issue 2162)"),
    ).toBe(WORKSPACE_ERROR_CODES.MissingPlatformProxy);
  });
  it("returns null when no code is present", () => {
    expect(extractErrorCode("something else entirely")).toBeNull();
  });
});

describe("extractHumanReason", () => {
  it("prefers the JSON body's error field", () => {
    expect(
      extractHumanReason('API POST /x: 500 {"error":"lookup failed"}'),
    ).toBe("lookup failed");
  });
  it("falls back to the JSON body's message field", () => {
    expect(
      extractHumanReason('API POST /x: 500 {"message":"boom"}'),
    ).toBe("boom");
  });
  it("strips the api prefix when the JSON body has neither field", () => {
    expect(extractHumanReason('API POST /x: 503 {"code":9}')).toBe('{"code":9}');
  });
  it("strips the api prefix when the body is not JSON", () => {
    expect(extractHumanReason("API GET /y: 500 server exploded")).toBe(
      "server exploded",
    );
  });
  it("handles malformed JSON tails", () => {
    expect(extractHumanReason("API GET /y: 500 {not-json")).toBe("{not-json");
  });
  it("returns an api-prefixed message with an empty tail as-is", () => {
    expect(extractHumanReason("API GET /y: 500")).toBe("API GET /y: 500");
  });
  it("passes through plain messages", () => {
    expect(extractHumanReason("  fetch failed  ")).toBe("fetch failed");
  });
});

describe("humanizeSetupError (§8 mapping)", () => {
  const ctx = { runtimeLabel: "Codex", providerLabel: "OpenAI" };

  it.each([
    WORKSPACE_ERROR_CODES.ModelRequired,
    WORKSPACE_ERROR_CODES.UnregisteredModelForRuntime,
    WORKSPACE_ERROR_CODES.MissingModel,
  ])("%s → model-unavailable copy naming the runtime", (code) => {
    const view = humanizeSetupError({ code }, ctx);
    expect(view.copy).toBe(
      "That model isn't available for Codex — pick one from the list.",
    );
    expect(view.returnToKeyStep).toBe(false);
  });

  it("falls back to a generic runtime label when none is known", () => {
    const view = humanizeSetupError(
      { code: WORKSPACE_ERROR_CODES.ModelRequired },
      { runtimeLabel: "", providerLabel: "" },
    );
    expect(view.copy).toContain("the selected runtime");
  });

  it("MISSING_BYOK_CREDENTIAL → wrong-key copy + returnToKeyStep", () => {
    const view = humanizeSetupError(
      { code: WORKSPACE_ERROR_CODES.MissingByokCredential },
      ctx,
    );
    expect(view.copy).toBe(
      "The API key for OpenAI is missing or didn't match — re-enter it.",
    );
    expect(view.returnToKeyStep).toBe(true);
  });

  it("MISSING_BYOK_CREDENTIAL falls back to a generic provider label", () => {
    const view = humanizeSetupError(
      { code: WORKSPACE_ERROR_CODES.MissingByokCredential },
      { runtimeLabel: "", providerLabel: "" },
    );
    expect(view.copy).toContain("the selected provider");
  });

  it("MISSING_PLATFORM_PROXY → hosted-proxy copy", () => {
    const view = humanizeSetupError(
      { code: WORKSPACE_ERROR_CODES.MissingPlatformProxy },
      ctx,
    );
    expect(view.copy).toContain("Enter OS hosted proxy");
    expect(view.returnToKeyStep).toBe(false);
  });

  it.each([
    WORKSPACE_ERROR_CODES.RuntimeUnsupported,
    WORKSPACE_ERROR_CODES.RuntimeUnresolved,
    WORKSPACE_ERROR_CODES.DerivedProviderNotInRegistry,
  ])("%s → runtime/model-combination copy", (code) => {
    const view = humanizeSetupError({ code }, ctx);
    expect(view.copy).toBe(
      "That runtime/model combination isn't available — pick options from the lists.",
    );
    expect(view.returnToKeyStep).toBe(false);
  });

  it("scans the message for a code when none is passed explicitly", () => {
    const view = humanizeSetupError(
      { message: "workspace abort MISSING_BYOK_CREDENTIAL routing=byok" },
      ctx,
    );
    expect(view.returnToKeyStep).toBe(true);
  });

  it("unknown errors → humanized generic copy, never raw JSON", () => {
    const view = humanizeSetupError(
      { message: 'API POST /admin/x: 500 {"error":"lookup failed"}' },
      ctx,
    );
    expect(view.copy).toBe(
      "Couldn't set up the platform agent — lookup failed.",
    );
    expect(view.returnToKeyStep).toBe(false);
  });

  it("empty message → 'did not respond' copy", () => {
    const view = humanizeSetupError({}, ctx);
    expect(view.copy).toBe(
      "Couldn't set up the platform agent — the platform did not respond.",
    );
  });
});

describe("focus trap helpers", () => {
  function makeContainer(html: string): HTMLDivElement {
    const div = document.createElement("div");
    div.innerHTML = html;
    document.body.appendChild(div);
    return div;
  }

  it("focusableElements skips disabled controls and negative tabindex", () => {
    const c = makeContainer(
      '<button id="a">A</button><button disabled>D</button><span tabindex="-1">S</span><input id="b" />',
    );
    expect(focusableElements(c).map((el) => el.id)).toEqual(["a", "b"]);
    c.remove();
  });

  it("ignores non-Tab keys", () => {
    const c = makeContainer('<button id="a">A</button>');
    const prevent = vi.fn();
    handleFocusTrapKeyDown(c, { key: "Enter", shiftKey: false, preventDefault: prevent });
    expect(prevent).not.toHaveBeenCalled();
    c.remove();
  });

  it("prevents default when nothing is focusable", () => {
    const c = makeContainer("<p>text only</p>");
    const prevent = vi.fn();
    handleFocusTrapKeyDown(c, { key: "Tab", shiftKey: false, preventDefault: prevent });
    expect(prevent).toHaveBeenCalledOnce();
    c.remove();
  });

  it("Tab on the last element wraps to the first", () => {
    const c = makeContainer('<button id="a">A</button><button id="b">B</button>');
    (c.querySelector("#b") as HTMLElement).focus();
    const prevent = vi.fn();
    handleFocusTrapKeyDown(c, { key: "Tab", shiftKey: false, preventDefault: prevent });
    expect(prevent).toHaveBeenCalledOnce();
    expect(document.activeElement?.id).toBe("a");
    c.remove();
  });

  it("Tab in the middle is left to the browser", () => {
    const c = makeContainer('<button id="a">A</button><button id="b">B</button>');
    (c.querySelector("#a") as HTMLElement).focus();
    const prevent = vi.fn();
    handleFocusTrapKeyDown(c, { key: "Tab", shiftKey: false, preventDefault: prevent });
    expect(prevent).not.toHaveBeenCalled();
    c.remove();
  });

  it("Shift+Tab on the first element wraps to the last", () => {
    const c = makeContainer('<button id="a">A</button><button id="b">B</button>');
    (c.querySelector("#a") as HTMLElement).focus();
    const prevent = vi.fn();
    handleFocusTrapKeyDown(c, { key: "Tab", shiftKey: true, preventDefault: prevent });
    expect(prevent).toHaveBeenCalledOnce();
    expect(document.activeElement?.id).toBe("b");
    c.remove();
  });

  it("Shift+Tab in the middle is left to the browser", () => {
    const c = makeContainer('<button id="a">A</button><button id="b">B</button>');
    (c.querySelector("#b") as HTMLElement).focus();
    const prevent = vi.fn();
    handleFocusTrapKeyDown(c, { key: "Tab", shiftKey: true, preventDefault: prevent });
    expect(prevent).not.toHaveBeenCalled();
    c.remove();
  });

  it("pulls focus inside when it is outside the container (both directions)", () => {
    const outside = document.createElement("button");
    document.body.appendChild(outside);
    const c = makeContainer('<button id="a">A</button><button id="b">B</button>');
    outside.focus();
    const prevent = vi.fn();
    handleFocusTrapKeyDown(c, { key: "Tab", shiftKey: false, preventDefault: prevent });
    expect(document.activeElement?.id).toBe("a");
    outside.focus();
    handleFocusTrapKeyDown(c, { key: "Tab", shiftKey: true, preventDefault: prevent });
    expect(document.activeElement?.id).toBe("b");
    expect(prevent).toHaveBeenCalledTimes(2);
    outside.remove();
    c.remove();
  });

  it("focusFirstElement focuses the first control, or the container itself when empty", () => {
    const c = makeContainer('<button id="a">A</button>');
    focusFirstElement(c);
    expect(document.activeElement?.id).toBe("a");
    c.remove();

    const empty = makeContainer("<p>none</p>");
    empty.tabIndex = -1;
    empty.id = "empty";
    focusFirstElement(empty);
    expect(document.activeElement?.id).toBe("empty");
    empty.remove();
  });
});
