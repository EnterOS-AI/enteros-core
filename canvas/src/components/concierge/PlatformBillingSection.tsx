"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import { api } from "@/lib/api";
import { showToast } from "@/components/Toaster";
import {
  ProviderModelSelector,
  buildProviderCatalog,
  buildProviderCatalogFromRegistry,
  findProviderForModel,
  isPlatformManagedProvider,
  type SelectorModel,
  type SelectorValue,
  type ProviderEntry,
  type RegistryProvider,
  type RegistryModel,
} from "@/components/ProviderModelSelector";
import s from "./Concierge.module.css";

/**
 * PlatformBillingSection — provider/model + BYOK opt-in for the org's
 * platform agent (the concierge).
 *
 * Default (and the recommended state) is `platform_managed`: all LLM
 * traffic is metered through the Molecule proxy and billed to org
 * credits. The user can opt the platform agent into `byok` by picking a
 * provider + model from the registry SSOT and supplying the matching
 * per-provider key. We are NOT hardcoded to Anthropic — the platform
 * agent can run on any registry-served provider (MiniMax, Kimi, GLM,
 * DeepSeek, the platform proxy, …), exactly like a normal workspace.
 *
 * SSOT: the provider→model catalog comes from the SAME source the
 * CreateWorkspaceDialog / ConfigTab use — GET /templates, whose rows
 * carry `registry_providers` + `registry_models` (the provider-registry
 * SSOT). We pick the template row matching the platform agent's runtime
 * (GET /workspaces/:id → {runtime}) and build the catalog via
 * buildProviderCatalogFromRegistry (or the legacy buildProviderCatalog
 * over template models[] when the runtime isn't registry-backed).
 *
 * Save (mirrors CreateWorkspaceDialog's create payload + ConfigTab's
 * model write path):
 *   PUT  /workspaces/:id/model                    {model}          — set model
 *   PUT  /workspaces/:id/secrets                  {key: MODEL_PROVIDER, value: <vendor>}
 *   PUT  /workspaces/:id/secrets                  {key: <required_env>, value}  — BYOK key
 *   PUT  /admin/workspaces/:id/llm-billing-mode   {mode: "byok"|"platform_managed"}
 *
 * The MODEL_PROVIDER secret is how the workspace path forces the
 * provider (same secret the e2e test agent used to run the platform
 * agent on MiniMax: model `MiniMax-M2.7` + MODEL_PROVIDER=minimax).
 *
 * Self-hosted (no Molecule proxy): GET /org/identity returns
 * `platform_managed_available: false`. In that mode the platform-managed
 * "Platform (proxy)" provider is HIDDEN (it cannot work — there is no hosted
 * Molecule proxy / credit ledger), the default selection is the first BYOK
 * provider, and the billing write is always `byok`. A 404 / error on the
 * signal is treated as UNAVAILABLE (self-host safety: a missing signal means we
 * cannot confirm a proxy exists). On SaaS (`platform_managed_available: true`)
 * the behavior is unchanged — Platform offered + default platform_managed.
 *
 * Graceful when the backend has no billing/registry endpoint: each read
 * fails silently and the UI shows the default state for the resolved mode.
 */

// Mirrors workspace-server BillingModeResolution (see llm-billing-section.tsx).
interface BillingModeResolution {
  workspace_id: string;
  resolved_mode: "platform_managed" | "byok" | "disabled";
  workspace_override: string | null;
  org_default: "platform_managed" | "byok" | "disabled";
  source: "workspace_override" | "org_default" | "constant_fallback";
}

// Subset of a GET /templates row used here — same fields CreateWorkspaceDialog
// reads. Keyed by `runtime`; registry_* fields are additive (absent on older
// backends / non-registry runtimes).
interface TemplateSpec {
  id: string;
  name?: string;
  runtime?: string;
  model?: string;
  models?: SelectorModel[];
  registry_backed?: boolean;
  registry_providers?: RegistryProvider[];
  registry_models?: RegistryModel[];
}

type Choice = "platform_managed" | "byok";

interface Props {
  platformId: string;
}

export function PlatformBillingSection({ platformId }: Props) {
  // Billing-mode resolution (defaults to platform-managed until the read
  // resolves; a 404 keeps us on this default).
  const [resolution, setResolution] = useState<BillingModeResolution | null>(null);

  // Whether a Molecule LLM proxy is configured (GET /org/identity →
  // platform_managed_available). Starts true (SaaS-shaped) so the initial render
  // — before the signal resolves — matches the SaaS contract; the read flips it
  // false on self-host. A 404 / error on the signal flips it false too
  // (self-host safety: a missing signal can't confirm a proxy exists). When
  // false: hide the "Platform (proxy)" provider, default to a BYOK provider, and
  // always write byok.
  const [platformManagedAvailable, setPlatformManagedAvailable] = useState(true);

  // Registry catalog source — the platform agent's runtime + the /templates
  // rows it indexes into.
  const [runtime, setRuntime] = useState<string>("");
  const [templateSpecs, setTemplateSpecs] = useState<TemplateSpec[]>([]);
  // The model currently saved on the platform agent (GET /workspaces/:id/model)
  // — used to pre-select the provider/model on mount.
  const [currentModel, setCurrentModel] = useState<string>("");

  // Selector state (provider + model) and the per-provider BYOK key.
  const [llmSelection, setLLMSelection] = useState<SelectorValue>({
    providerId: "",
    model: "",
    envVars: [],
  });
  const [apiKey, setApiKey] = useState("");

  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [ok, setOk] = useState<string | null>(null);

  // ── reads ────────────────────────────────────────────────────────────────
  const loadBilling = useCallback(() => {
    let cancelled = false;
    api
      .get<BillingModeResolution>(`/admin/workspaces/${platformId}/llm-billing-mode`)
      .then((r) => {
        if (cancelled || !r) return;
        setResolution(r);
      })
      .catch(() => {
        // No billing endpoint / not reachable — keep platform-managed default.
        if (!cancelled) setResolution(null);
      });
    return () => {
      cancelled = true;
    };
  }, [platformId]);

  useEffect(() => loadBilling(), [loadBilling]);

  // Capability signal — is a Molecule LLM proxy configured? Open / no-auth, read
  // pre-login exactly like /org/identity (it IS /org/identity). 404 / error →
  // treat as UNAVAILABLE (self-host safety: a missing signal can't confirm a
  // proxy, so hide Platform rather than risk offering an unworkable mode).
  useEffect(() => {
    let cancelled = false;
    api
      .get<{ platform_managed_available?: boolean }>("/org/identity")
      .then((r) => {
        if (!cancelled) setPlatformManagedAvailable(r?.platform_managed_available === true);
      })
      .catch(() => {
        if (!cancelled) setPlatformManagedAvailable(false);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  // Runtime + current model of the platform agent (best-effort; both fall
  // back to empty so the registry catalog still loads its default provider).
  useEffect(() => {
    let cancelled = false;
    api
      .get<{ runtime?: string }>(`/workspaces/${platformId}`)
      .then((r) => {
        if (!cancelled) setRuntime((r?.runtime || "").trim());
      })
      .catch(() => {
        if (!cancelled) setRuntime("");
      });
    api
      .get<{ model?: string }>(`/workspaces/${platformId}/model`)
      .then((r) => {
        if (!cancelled) setCurrentModel((r?.model || "").trim());
      })
      .catch(() => {
        if (!cancelled) setCurrentModel("");
      });
    return () => {
      cancelled = true;
    };
  }, [platformId]);

  // Registry catalog source — GET /templates (same source CreateWorkspaceDialog
  // / ConfigTab use). Graceful empty on 404.
  useEffect(() => {
    let cancelled = false;
    api
      .get<TemplateSpec[]>("/templates")
      .then((rows) => {
        if (!cancelled) setTemplateSpecs(Array.isArray(rows) ? rows : []);
      })
      .catch(() => {
        if (!cancelled) setTemplateSpecs([]);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  // ── registry catalog (SSOT) ────────────────────────────────────────────────
  // The /templates row backing the platform agent's runtime. Matches by the
  // row's runtime field (or its id for base runtime templates), mirroring
  // CreateWorkspaceDialog's BASE_RUNTIME match.
  const sourceSpec = useMemo<TemplateSpec | null>(() => {
    if (!runtime) return templateSpecs[0] ?? null;
    return (
      templateSpecs.find((sp) => {
        const r = (sp.runtime ?? sp.id).trim().toLowerCase();
        return r === runtime.toLowerCase();
      }) ??
      templateSpecs[0] ??
      null
    );
  }, [runtime, templateSpecs]);

  const registryBacked = useMemo(
    () =>
      sourceSpec?.registry_backed === true &&
      (sourceSpec.registry_models?.length ?? 0) > 0,
    [sourceSpec],
  );

  const llmModels = useMemo<SelectorModel[]>(() => {
    if (registryBacked) {
      return (sourceSpec?.registry_models ?? []).map((m) => ({
        id: m.id,
        name: m.name,
        ...(m.provider ? { provider: m.provider } : {}),
      }));
    }
    return sourceSpec?.models?.length ? sourceSpec.models : [];
  }, [registryBacked, sourceSpec]);

  const catalog: ProviderEntry[] = useMemo(
    () =>
      registryBacked
        ? buildProviderCatalogFromRegistry(
            sourceSpec?.registry_providers ?? [],
            sourceSpec?.registry_models ?? [],
          )
        : buildProviderCatalog(llmModels),
    [registryBacked, sourceSpec, llmModels],
  );

  // On self-host (no proxy) the platform-managed "Platform (proxy)" provider
  // cannot work, so filter it out of the selectable catalog entirely. On SaaS
  // (platformManagedAvailable) the full catalog is used unchanged. Empty-after-
  // filter is tolerated (the no-catalog note renders) — a self-host registry
  // would normally still carry BYOK providers.
  const visibleCatalog = useMemo<ProviderEntry[]>(
    () =>
      platformManagedAvailable
        ? catalog
        : catalog.filter((p) => !isPlatformManagedProvider(p)),
    [catalog, platformManagedAvailable],
  );

  const selectedProvider = useMemo(
    () => visibleCatalog.find((p) => p.id === llmSelection.providerId) ?? null,
    [visibleCatalog, llmSelection.providerId],
  );

  // Pre-select provider+model from the platform agent's current model once the
  // catalog is available. Prefer the matching provider for the saved model;
  // fall back to the platform provider, then the first catalog entry.
  useEffect(() => {
    if (visibleCatalog.length === 0) return;
    setLLMSelection((prev) => {
      // Don't clobber an in-progress user selection.
      if (prev.providerId) return prev;
      const matched = currentModel ? findProviderForModel(visibleCatalog, currentModel) : null;
      // Prefer the platform provider only when it's actually available (SaaS);
      // on self-host it's been filtered out of visibleCatalog, so fall straight
      // through to the first (BYOK) entry as the default.
      const platform = platformManagedAvailable
        ? visibleCatalog.find((p) => p.vendor === "platform")
        : undefined;
      const next = matched ?? platform ?? visibleCatalog[0];
      const model =
        next.models.find((m) => m.id === currentModel)?.id ??
        (next.wildcard ? currentModel : next.models[0]?.id ?? "");
      return { providerId: next.id, model, envVars: next.envVars };
    });
  }, [visibleCatalog, currentModel, platformManagedAvailable]);

  // ── writes ────────────────────────────────────────────────────────────────
  const setMode = (mode: Choice) =>
    api.put<BillingModeResolution>(`/admin/workspaces/${platformId}/llm-billing-mode`, {
      mode,
    });

  // Whether the chosen provider is platform-managed (no key, billed to org
  // credits via the proxy) vs a BYOK provider (needs a per-provider key + the
  // proxy stops metering). This single derivation — not a separate radio —
  // drives the whole card: the user picks "Platform" in the dropdown for
  // managed billing, or any other provider to bring their own key.
  // Self-host (no proxy) can NEVER be platform-managed: the platform provider is
  // filtered out of visibleCatalog, but force the derivation false too so the
  // billing-mode write is always byok in this mode regardless of any stale
  // selection (belt-and-suspenders for the "never platform_managed on self-host"
  // invariant).
  const providerIsPlatformManaged =
    platformManagedAvailable && isPlatformManagedProvider(selectedProvider);
  const requiredEnv = selectedProvider?.envVars[0] ?? "";

  const save = async () => {
    if (!selectedProvider) {
      setError("Pick a provider and model first");
      return;
    }
    if (!llmSelection.model.trim()) {
      setError("Model is required");
      return;
    }
    // BYOK providers require the per-provider key; platform-managed ones don't.
    if (!providerIsPlatformManaged && requiredEnv && !apiKey.trim()) {
      setError(`${requiredEnv} is required for this provider`);
      return;
    }
    setSaving(true);
    setError(null);
    setOk(null);
    try {
      // 1. Set the platform agent's model (mirror ConfigTab's PUT /model).
      await api.put(`/workspaces/${platformId}/model`, {
        model: llmSelection.model.trim(),
      });
      // 2. Force the provider via the MODEL_PROVIDER secret (same mechanism the
      //    e2e/test agent used to run the platform agent on MiniMax). Vendor is
      //    the registry-derived provider key from the selected entry.
      await api.put(`/workspaces/${platformId}/secrets`, {
        key: "MODEL_PROVIDER",
        value: selectedProvider.vendor,
      });
      // 3. BYOK providers: write the per-provider key as a workspace secret.
      //    Platform-managed providers have no key (the credential is internal
      //    plumbing injected by the provisioner) — skip the secret write.
      if (!providerIsPlatformManaged && requiredEnv && apiKey.trim()) {
        await api.put(`/workspaces/${platformId}/secrets`, {
          key: requiredEnv,
          value: apiKey.trim(),
        });
      }
      // 4. Flip billing mode. A platform-managed provider is effectively
      //    platform_managed; a BYOK provider flips to byok so the proxy stops
      //    metering this agent.
      const nextMode: Choice = providerIsPlatformManaged ? "platform_managed" : "byok";
      await setMode(nextMode);

      setApiKey("");
      if (providerIsPlatformManaged) {
        showToast("Platform-managed provider set for the platform agent", "success");
        setOk("Saved. Restart the platform agent to apply.");
      } else {
        showToast("BYOK enabled for the platform agent", "success");
        setOk("BYOK enabled. Restart the platform agent to apply.");
      }
      loadBilling();
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to save");
    } finally {
      setSaving(false);
    }
  };

  const currentMode = resolution?.resolved_mode ?? "platform_managed";
  const saveDisabled =
    saving ||
    !selectedProvider ||
    !llmSelection.model.trim() ||
    (!providerIsPlatformManaged && !!requiredEnv && !apiKey.trim());

  return (
    <div className={s.scard}>
      <div className={s.scardHead}>
        <div className={s.scardTitle}>LLM billing — platform agent</div>
        <div className={s.scardDesc}>
          {platformManagedAvailable ? (
            <>
              Pick the provider + model the org concierge runs on. Choose{" "}
              <strong>Platform</strong> to meter through the Molecule proxy on
              your org credits (the default), or any other provider to bring your
              own key. Current resolved mode: <strong>{currentMode}</strong>.
            </>
          ) : (
            <>
              Bring your own provider key — this self-hosted deployment has no
              Molecule proxy, so platform-managed billing (org credits) is
              unavailable. Pick the provider + model the org concierge runs on
              and supply the matching key. Current resolved mode:{" "}
              <strong>{currentMode}</strong>.
            </>
          )}
        </div>
      </div>

      <div className={s.keyRow}>
        {visibleCatalog.length === 0 ? (
          <div className={s.keyNote}>
            No provider catalog available yet (the registry endpoint did not
            respond). Provider/model selection will appear once the backend is
            reachable.
          </div>
        ) : (
          <>
            <ProviderModelSelector
              models={llmModels}
              catalog={visibleCatalog}
              value={llmSelection}
              onChange={(next) => {
                setLLMSelection(next);
                setApiKey("");
                setError(null);
                setOk(null);
              }}
              idPrefix="platform-billing-llm"
              variant="stack"
              allowCustomModelEscape
            />
            {providerIsPlatformManaged ? (
              <div className={s.keyNote}>
                Platform-managed — metered through the Molecule proxy and billed
                to your org credits. No API key required; the credential is
                injected by the platform.
              </div>
            ) : (
              requiredEnv && (
                <>
                  <label className={s.keyLabel} htmlFor="byok-provider-key">
                    {requiredEnv}
                  </label>
                  <div className={s.keyInputRow}>
                    <input
                      id="byok-provider-key"
                      type="password"
                      className={s.keyInput}
                      placeholder="paste key…"
                      value={apiKey}
                      autoComplete="off"
                      onChange={(e) => setApiKey(e.target.value)}
                      disabled={saving}
                    />
                  </div>
                  <div className={s.keyNote}>
                    BYOK — LLM traffic goes directly to your provider and is
                    billed to your account. Stored encrypted as a workspace
                    secret (<code>{requiredEnv}</code>) and never exposed to the
                    browser. Restart the platform agent to apply.
                  </div>
                </>
              )
            )}
            <div className={s.keyInputRow}>
              <button
                type="button"
                className={`${s.btn} ${s.primary}`}
                disabled={saveDisabled}
                onClick={() => void save()}
              >
                {saving ? "Saving…" : "Save"}
              </button>
            </div>
          </>
        )}
      </div>

      {error && <div className={`${s.sMsg} ${s.sMsgErr}`}>{error}</div>}
      {ok && <div className={`${s.sMsg} ${s.sMsgOk}`}>{ok}</div>}
    </div>
  );
}
