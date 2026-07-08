/**
 * Self-host setup-scene gate (design SSOT §2) — decides whether the
 * fullscreen SelfHostSetupScene may render.
 *
 * FAIL-CLOSED-TO-INVISIBLE: the scene renders ONLY on positive confirmation
 * of every signal below; any fetch error, timeout, or ambiguous payload
 * resolves to "do not render". The root mount ships to SaaS too, so the
 * failure mode of this gate MUST be "scene doesn't appear on self-host",
 * never "a SaaS tenant is stuck behind a setup screen".
 *
 * Signals (all must confirm):
 *   G1a  getTenantSlug() === ""              (client-derived, tenant.ts)
 *   G1b  GET /org/identity → org_id === ""   (server-declared; disambiguates
 *        the SaaS apex / Vercel-preview hosts that also derive an empty slug)
 *   G2   the kind='platform' store node (the ConciergeShell signal) has
 *        never been online AND no LLM key is configured (GET /settings/secrets
 *        has_value scan against the /templates-derived auth-env vocabulary).
 *        A missing root entirely is a defensive SHOW (ensure's 'created'
 *        path covers it) — still requiring G1 + the key scan to confirm.
 *
 * Dismissal is DERIVED STATE, not a flag: no localStorage anywhere. Once the
 * root reports an active status (or an LLM key exists), the gate re-derives
 * to hidden on every load. Mid-flow refresh resumes for free — a root left
 * `provisioning`/`failed` by an interrupted setup re-enters the progress /
 * error view (deriveResumeView).
 */
import { api } from "@/lib/api";
import { getTenantSlug } from "@/lib/tenant";
import { useCanvasStore } from "@/store/canvas";
import { WORKSPACE_KIND } from "@/lib/workspace-kind";
import {
  bucketTemplatesByRuntime,
  collectAuthEnvNames,
  hasConfiguredLLMKey,
  statusIndicatesConfiguredRoot,
  type SceneRuntimeOption,
  type SceneTemplateRow,
} from "./setup-scene-lib";

/** The platform-root fields the gate reads off the canvas store node. */
export interface GatePlatformNode {
  id: string;
  data: {
    status?: string;
    runtime?: string;
    lastSampleError?: string;
  };
}

/** Everything the scene needs to run, captured at gate time so the scene
 *  does not re-fetch what the gate already confirmed. */
export interface GateContext {
  /** Platform root id, or null when the row is missing entirely (defensive
   *  edge — ensure's 'created' path handles it). */
  rootId: string | null;
  /** The root row's current runtime ("" when the row is missing). */
  rootRuntime: string;
  /** The root row's status at gate time ("" when the row is missing). */
  rootStatus: string;
  /** The root row's last sampled error at gate time. */
  rootLastError: string;
  /** Offerable runtimes derived from GET /templates (never hardcoded). */
  runtimeOptions: SceneRuntimeOption[];
  /** Secret keys with has_value=true at gate time. */
  configuredKeys: Set<string>;
}

export type GateResult = { show: false } | { show: true; context: GateContext };

export const GATE_HIDDEN: GateResult = { show: false };

/** Injected dependencies — the component passes defaultGateDeps; tests pass
 *  plain fakes so every branch is drivable without module mocks. */
export interface GateDeps {
  getSlug: () => string;
  fetchIdentity: () => Promise<{ org_id?: unknown }>;
  fetchTemplates: () => Promise<SceneTemplateRow[]>;
  fetchSecrets: () => Promise<Array<{ key: string; has_value?: boolean }>>;
  getPlatformNode: () => GatePlatformNode | null;
}

export const defaultGateDeps: GateDeps = {
  getSlug: () => getTenantSlug(),
  fetchIdentity: () => api.get<{ org_id?: unknown }>("/org/identity"),
  fetchTemplates: () => api.get<SceneTemplateRow[]>("/templates"),
  fetchSecrets: () =>
    api.get<Array<{ key: string; has_value?: boolean }>>("/settings/secrets"),
  getPlatformNode: () => {
    // The same kind='platform' resolution ConciergeShell.tsx uses (the
    // authoritative marker, no name/role heuristic). Read from the live
    // store — the scene is mounted only after first /workspaces hydration.
    const nodes = useCanvasStore.getState().nodes;
    const node = nodes.find(
      (n) => (n.data as { kind?: string }).kind === WORKSPACE_KIND.Platform,
    );
    if (!node) return null;
    const data = node.data as {
      status?: string;
      runtime?: string;
      lastSampleError?: string;
    };
    return {
      id: node.id,
      data: {
        status: data.status,
        runtime: data.runtime,
        lastSampleError: data.lastSampleError,
      },
    };
  },
};

export async function evaluateSelfHostSetupGate(
  deps: GateDeps,
): Promise<GateResult> {
  // G1a — client-derived self-host signal.
  if (deps.getSlug() !== "") return GATE_HIDDEN;

  // G1b — server-declared. Any error/timeout/ambiguity ⇒ hidden.
  let identity: { org_id?: unknown };
  try {
    identity = await deps.fetchIdentity();
  } catch {
    return GATE_HIDDEN;
  }
  if (!identity || identity.org_id !== "") return GATE_HIDDEN;

  // G2 inputs — templates (runtime/provider vocabulary) + secrets scan.
  let templateRows: SceneTemplateRow[];
  let secrets: Array<{ key: string; has_value?: boolean }>;
  try {
    [templateRows, secrets] = await Promise.all([
      deps.fetchTemplates(),
      deps.fetchSecrets(),
    ]);
  } catch {
    return GATE_HIDDEN;
  }
  if (!Array.isArray(templateRows) || !Array.isArray(secrets)) {
    return GATE_HIDDEN;
  }
  const runtimeOptions = bucketTemplatesByRuntime(templateRows);
  // No offerable runtime ⇒ the scene cannot collect a valid configuration —
  // render the normal UI instead of a dead-end setup screen.
  if (runtimeOptions.length === 0) return GATE_HIDDEN;

  const configuredKeys = new Set(
    secrets.filter((s) => s.has_value === true).map((s) => s.key),
  );
  const hasKey = hasConfiguredLLMKey(
    configuredKeys,
    collectAuthEnvNames(runtimeOptions),
  );

  const node = deps.getPlatformNode();
  if (node) {
    const status = node.data.status ?? "";
    if (statusIndicatesConfiguredRoot(status)) return GATE_HIDDEN;
    if (hasKey) return GATE_HIDDEN;
    return {
      show: true,
      context: {
        rootId: node.id,
        rootRuntime: node.data.runtime ?? "",
        rootStatus: status,
        rootLastError: node.data.lastSampleError ?? "",
        runtimeOptions,
        configuredKeys,
      },
    };
  }

  // Platform root missing entirely — defensive SHOW (the seed is
  // unconditional on self-host, so this is an edge; ensure's 'created' path
  // still converges it).
  if (hasKey) return GATE_HIDDEN;
  return {
    show: true,
    context: {
      rootId: null,
      rootRuntime: "",
      rootStatus: "",
      rootLastError: "",
      runtimeOptions,
      configuredKeys,
    },
  };
}
