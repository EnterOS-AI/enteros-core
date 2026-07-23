/**
 * Pure logic for the self-host setup scene (SelfHostSetupScene.tsx).
 *
 * Everything here is a side-effect-free function so the scene component can
 * stay a thin orchestrator and this file carries the branchy logic under
 * direct unit test. SSOT consumption contract (design SSOT §5.2): this module
 * imports ONLY the approved mirror/leaf modules — WORKSPACE_STATUS,
 * WORKSPACE_ERROR_CODES, isExternalLikeRuntime, and the ProviderModelSelector
 * DATA layer — and introduces no new status/kind/runtime/error string
 * literals of its own.
 */
import {
  buildProviderCatalog,
  buildProviderCatalogFromRegistry,
  findProviderForModel,
  isPlatformManagedProvider,
  type ProviderEntry,
  type RegistryModel,
  type RegistryProvider,
  type SelectorModel,
} from "@/components/ProviderModelSelector";
import { isExternalLikeRuntime } from "@/lib/externalRuntimes";
import { WORKSPACE_ERROR_CODES } from "@/lib/workspace-error-codes";
import { WORKSPACE_STATUS } from "@/lib/workspace-status";
import { WORKSPACE_KIND } from "@/lib/workspace-kind";

/** Operator-ruled fixed brand name for the platform agent (design SSOT §3
 *  step 1) — the scene ships NO name input; this constant is the payload. */
export const PLATFORM_AGENT_NAME = "Enter OS Agent";

/** One row of GET /templates as the scene consumes it (the ConfigTab.tsx
 *  template-fetch shape; registry_* fields are additive/optional). */
export interface SceneTemplateRow {
  id?: string;
  name?: string;
  runtime?: string;
  /** Runtime's human label from the provider registry SSOT
   *  (providers.yaml runtimes.<rt>.display_name). The runtime picker labels
   *  by this — NOT by `name`, which is the *template* name and collides when
   *  two templates share a runtime (e.g. platform-agent + seo-agent both
   *  claude-code, which used to surface the runtime as "SEO Agent"). */
  runtime_display_name?: string;
  models?: SelectorModel[];
  registry_backed?: boolean;
  registry_providers?: RegistryProvider[];
  registry_models?: RegistryModel[];
  displayable?: boolean;
}

/** A runtime choice derived from /templates — one entry per runtime. */
export interface SceneRuntimeOption {
  value: string;
  label: string;
  models: SelectorModel[];
  registryBacked: boolean;
  registryProviders: RegistryProvider[];
  registryModels: RegistryModel[];
}

/**
 * Bucket GET /templates rows by runtime — the ConfigTab.tsx pattern
 * (honors `displayable:false`; richer payload wins when two templates share
 * a runtime), NOT a hardcoded list. Additionally filters external-like
 * runtimes (isExternalLikeRuntime): BYO-compute runtimes have no
 * platform-owned container and are not offerable concierge roots (design
 * SSOT §3 step 2).
 */
export function bucketTemplatesByRuntime(
  rows: SceneTemplateRow[],
): SceneRuntimeOption[] {
  const byRuntime = new Map<string, SceneRuntimeOption>();
  for (const r of rows) {
    const v = (r.runtime || "").trim();
    if (!v) continue;
    // Honor an explicit opt-out; absent/true means show it.
    if (r.displayable === false) continue;
    if (isExternalLikeRuntime(v)) continue;
    const models = Array.isArray(r.models) ? r.models : [];
    const registryProviders = Array.isArray(r.registry_providers)
      ? r.registry_providers
      : [];
    const registryModels = Array.isArray(r.registry_models)
      ? r.registry_models
      : [];
    const registryBacked = r.registry_backed === true && registryModels.length > 0;
    // Prefer the richer payload: a registry-backed entry, then more template
    // models (same scoring ConfigTab uses).
    const score = (o: SceneRuntimeOption) =>
      (o.registryBacked ? 1000 : 0) + o.models.length;
    const candidate: SceneRuntimeOption = {
      value: v,
      // Label from the registry runtime display_name (SSOT), NOT the template
      // name — two templates can share a runtime, so `r.name` would surface a
      // template (e.g. "SEO Agent") as the runtime. Fall back to the runtime
      // slug when the registry has no display_name for it.
      label: r.runtime_display_name || v,
      models,
      registryBacked,
      registryProviders,
      registryModels,
    };
    const existing = byRuntime.get(v);
    if (!existing || score(candidate) > score(existing)) {
      byRuntime.set(v, candidate);
    }
  }
  return Array.from(byRuntime.values());
}

/**
 * Build the provider catalog for one runtime option, feeding the
 * ProviderModelSelector DATA layer: registry-backed path
 * (buildProviderCatalogFromRegistry) with the legacy buildProviderCatalog
 * fallback when registry_models are absent (older backends / non-registry
 * runtimes).
 *
 * Scene-specific hardening (design SSOT §3: "no free-text entry anywhere"):
 *  - platform-managed providers are dropped defensively (the server already
 *    strips them on self-host, templates_registry.go:75-121);
 *  - wildcard model ids are stripped and the wildcard flag cleared, so the
 *    selector can never enter its free-text mode;
 *  - providers left with zero concrete models are dropped.
 */
export function buildSceneCatalog(option: SceneRuntimeOption): ProviderEntry[] {
  const raw = option.registryBacked
    ? buildProviderCatalogFromRegistry(
        option.registryProviders,
        option.registryModels,
      )
    : buildProviderCatalog(option.models);
  return raw
    .filter((p) => !isPlatformManagedProvider(p))
    .map((p) => ({
      ...p,
      models: p.models.filter((m) => !m.id.includes("*")),
      wildcard: false,
    }))
    .filter((p) => p.models.length > 0);
}

/**
 * Union of every auth-env key name any offerable provider can require —
 * derived from the same catalog the pickers use (never a hardcoded list).
 * This is the "is any LLM key configured?" vocabulary for the gate's
 * has_value scan.
 */
export function collectAuthEnvNames(
  options: SceneRuntimeOption[],
): Set<string> {
  const names = new Set<string>();
  for (const option of options) {
    for (const provider of buildSceneCatalog(option)) {
      for (const env of provider.envVars) names.add(env);
    }
  }
  return names;
}

/** True when any configured (has_value) secret key is a known LLM auth env. */
export function hasConfiguredLLMKey(
  configuredKeys: ReadonlySet<string>,
  authEnvNames: ReadonlySet<string>,
): boolean {
  for (const key of configuredKeys) {
    if (authEnvNames.has(key)) return true;
  }
  return false;
}

/**
 * True when the platform root's status shows it is (or has recently been)
 * a running agent — i.e. it is NOT an unconfigured first-run root. Maps the
 * G2 "has never been online" gate onto what is client-derivable: online /
 * degraded / paused / hibernating / hibernated all require a successful
 * provision at some point; offline / failed / provisioning / awaiting_agent /
 * removed do not.
 */
export function statusIndicatesConfiguredRoot(status: string): boolean {
  return (
    status === WORKSPACE_STATUS.Online ||
    status === WORKSPACE_STATUS.Degraded ||
    status === WORKSPACE_STATUS.Paused ||
    status === WORKSPACE_STATUS.Hibernating ||
    status === WORKSPACE_STATUS.Hibernated
  );
}

/** Which scene view a fresh (or refreshed) session resumes into, derived
 *  purely from the root's server-side status (stateless resume — no
 *  localStorage anywhere; design SSOT §2 "dismissal = derived state"). */
export type ResumeView = "form" | "progress" | "failed";

export function deriveResumeView(status: string): ResumeView {
  if (status === WORKSPACE_STATUS.Provisioning) return "progress";
  if (status === WORKSPACE_STATUS.Failed) return "failed";
  return "form";
}

/** Is this watched status terminal for the provision watch? */
export function isWatchTerminal(status: string): boolean {
  return (
    status === WORKSPACE_STATUS.Online || status === WORKSPACE_STATUS.Failed
  );
}

/** Minimal row shape the watch/poll path reads off GET /workspaces. */
export interface PlatformRowLike {
  id: string;
  kind?: string;
  status: string;
  last_sample_error?: string;
}

/** Find the platform root in a GET /workspaces payload: by id when known,
 *  falling back to the kind='platform' marker (the ConciergeShell signal). */
export function pickPlatformRow<T extends PlatformRowLike>(
  rows: T[],
  rootId: string | null,
): T | null {
  if (rootId !== null) {
    const byId = rows.find((r) => r.id === rootId);
    if (byId) return byId;
  }
  return rows.find((r) => r.kind === WORKSPACE_KIND.Platform) ?? null;
}

/** Back-fill helper: when a model id is known but no provider is selected
 *  (derived-state resume), recover the {provider, model} selection via
 *  findProviderForModel — the ProviderModelSelector adopt-mode pattern. */
export function deriveSelectionForModel(
  catalog: ProviderEntry[],
  modelId: string,
): { providerId: string; model: string; envVars: string[] } | null {
  const provider = findProviderForModel(catalog, modelId);
  if (!provider) return null;
  return { providerId: provider.id, model: modelId, envVars: provider.envVars };
}

// ─── Error humanization (design SSOT §8) ────────────────────────────────────

/** Scan free text (a 422 body, an api Error message, or last_sample_error)
 *  for any known workspace error code. Codes are mutually non-substrings, so
 *  first-match is deterministic. Returns null when no code is present. */
export function extractErrorCode(text: string): string | null {
  for (const code of Object.values(WORKSPACE_ERROR_CODES)) {
    if (text.includes(code)) return code;
  }
  return null;
}

/**
 * Pull a short human-readable reason out of an api-module error message.
 * The api module throws `Error("API POST /path: 500 {\"error\":\"...\"}")`
 * — never render that raw. Prefers the JSON body's error/message field,
 * falls back to the text after the status code, then the whole message.
 */
export function extractHumanReason(message: string): string {
  const jsonStart = message.indexOf("{");
  if (jsonStart >= 0) {
    try {
      const parsed = JSON.parse(message.slice(jsonStart)) as {
        error?: unknown;
        message?: unknown;
      };
      if (typeof parsed.error === "string" && parsed.error) return parsed.error;
      if (typeof parsed.message === "string" && parsed.message) {
        return parsed.message;
      }
    } catch {
      // fall through to the prefix strip
    }
  }
  const m = /^API [A-Z]+ \S+: \d+\s*([\s\S]*)$/.exec(message);
  if (m && m[1].trim()) return m[1].trim();
  return message.trim();
}

export interface SetupErrorView {
  /** Human copy per the §8 table — never raw JSON. */
  copy: string;
  /** True for the wrong-key class: the scene returns to the API-key step. */
  returnToKeyStep: boolean;
}

export interface SetupErrorContext {
  /** Display label of the selected runtime (for the model-mismatch copy). */
  runtimeLabel: string;
  /** Display label of the selected provider (for the credential copy). */
  providerLabel: string;
}

/**
 * Map a wire error (422 body code, WORKSPACE_PROVISION_FAILED socket extra,
 * or a raw last_sample_error / network Error message) to §8 scene copy.
 */
export function humanizeSetupError(
  src: { code?: string | null; message?: string | null },
  ctx: SetupErrorContext,
): SetupErrorView {
  const message = src.message ?? "";
  const code = src.code ?? extractErrorCode(message);
  switch (code) {
    case WORKSPACE_ERROR_CODES.ModelRequired:
    case WORKSPACE_ERROR_CODES.UnregisteredModelForRuntime:
    case WORKSPACE_ERROR_CODES.MissingModel:
      return {
        copy: `That model isn't available for ${ctx.runtimeLabel || "the selected runtime"} — pick one from the list.`,
        returnToKeyStep: false,
      };
    case WORKSPACE_ERROR_CODES.MissingByokCredential:
      return {
        copy: `The API key for ${ctx.providerLabel || "the selected provider"} is missing or didn't match — re-enter it.`,
        returnToKeyStep: true,
      };
    case WORKSPACE_ERROR_CODES.MissingPlatformProxy:
      return {
        copy: "That model needs the Enter OS hosted proxy (not available self-hosted) — pick a bring-your-own-key model.",
        returnToKeyStep: false,
      };
    case WORKSPACE_ERROR_CODES.RuntimeUnsupported:
    case WORKSPACE_ERROR_CODES.RuntimeUnresolved:
    case WORKSPACE_ERROR_CODES.DerivedProviderNotInRegistry:
      return {
        copy: "That runtime/model combination isn't available — pick options from the lists.",
        returnToKeyStep: false,
      };
    default: {
      const reason = extractHumanReason(message);
      return {
        copy: `Couldn't set up the platform agent — ${reason || "the platform did not respond"}.`,
        returnToKeyStep: false,
      };
    }
  }
}

// ─── Focus trap (fullscreen blocking scene, WCAG 2.1.2) ─────────────────────

const FOCUSABLE_SELECTOR =
  'button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), a[href], [tabindex]:not([tabindex="-1"])';

/** All keyboard-focusable elements inside the scene container, DOM order. */
export function focusableElements(container: HTMLElement): HTMLElement[] {
  return Array.from(
    container.querySelectorAll<HTMLElement>(FOCUSABLE_SELECTOR),
  );
}

/** Wrap Tab / Shift+Tab inside the container. Call from the container's
 *  keydown handler; a no-op for every other key. */
export function handleFocusTrapKeyDown(
  container: HTMLElement,
  e: {
    key: string;
    shiftKey: boolean;
    preventDefault: () => void;
  },
): void {
  if (e.key !== "Tab") return;
  const focusables = focusableElements(container);
  if (focusables.length === 0) {
    e.preventDefault();
    return;
  }
  const first = focusables[0];
  const last = focusables[focusables.length - 1];
  const active = container.ownerDocument.activeElement;
  const inside = active instanceof HTMLElement && container.contains(active);
  if (e.shiftKey) {
    if (!inside || active === first) {
      e.preventDefault();
      last.focus();
    }
  } else if (!inside || active === last) {
    e.preventDefault();
    first.focus();
  }
}

/** Move focus to the first focusable element in the container (used on step
 *  transitions so keyboard users land inside the new view). */
export function focusFirstElement(container: HTMLElement): void {
  const focusables = focusableElements(container);
  if (focusables.length > 0) {
    focusables[0].focus();
  } else {
    container.focus();
  }
}
