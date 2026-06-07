"use client";

import { useCallback, useEffect, useState } from "react";
import { api } from "@/lib/api";
import { showToast } from "@/components/Toaster";
import s from "./Concierge.module.css";

/**
 * PlatformBillingSection — BYOK opt-in for the org's platform agent.
 *
 * Default (and the recommended state) is `platform_managed`: all LLM
 * traffic is metered through the Molecule proxy and billed to org
 * credits. The user can opt the platform agent into `byok` by supplying
 * the org's own ANTHROPIC_API_KEY; we write the key as a workspace
 * secret, then flip the workspace's llm_billing_mode to byok.
 *
 * Integration contract (per-tenant workspace-server, same origin as the
 * other canvas /admin + /workspaces calls):
 *   GET  /admin/workspaces/:id/llm-billing-mode   → BillingModeResolution
 *   PUT  /admin/workspaces/:id/llm-billing-mode   {mode: "byok"|"platform_managed"|null}
 *   PUT  /workspaces/:id/secrets                  {key, value}
 *
 * The endpoints are the same ones the per-node Config tab uses
 * (llm-billing-section.tsx, secrets-section.tsx) — not invented here.
 *
 * Graceful when the backend has no billing endpoint: the read fails
 * silently and the UI shows the default platform-managed state.
 */

// Mirrors workspace-server BillingModeResolution (see llm-billing-section.tsx).
interface BillingModeResolution {
  workspace_id: string;
  resolved_mode: "platform_managed" | "byok" | "disabled";
  workspace_override: string | null;
  org_default: "platform_managed" | "byok" | "disabled";
  source: "workspace_override" | "org_default" | "constant_fallback";
}

type Choice = "platform_managed" | "byok";

interface Props {
  platformId: string;
}

export function PlatformBillingSection({ platformId }: Props) {
  // Default the UI to platform-managed (the documented default) until the
  // read resolves; if the read 404s we stay on this default.
  const [resolution, setResolution] = useState<BillingModeResolution | null>(null);
  const [choice, setChoice] = useState<Choice>("platform_managed");
  const [apiKey, setApiKey] = useState("");
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [ok, setOk] = useState<string | null>(null);

  const load = useCallback(() => {
    let cancelled = false;
    api
      .get<BillingModeResolution>(`/admin/workspaces/${platformId}/llm-billing-mode`)
      .then((r) => {
        if (cancelled || !r) return;
        setResolution(r);
        // Only mirror an explicit byok override; platform_managed remains
        // the default selection (an "inherit→byok org default" still shows
        // platform-managed here since the platform agent's own override is
        // what this toggle writes).
        if (r.workspace_override === "byok") setChoice("byok");
        else setChoice("platform_managed");
      })
      .catch(() => {
        // No billing endpoint / not reachable — keep platform-managed default.
        if (!cancelled) setResolution(null);
      });
    return () => {
      cancelled = true;
    };
  }, [platformId]);

  useEffect(() => load(), [load]);

  const setMode = async (mode: Choice) => {
    await api.put<BillingModeResolution>(
      `/admin/workspaces/${platformId}/llm-billing-mode`,
      { mode },
    );
  };

  const selectPlatformManaged = async () => {
    setChoice("platform_managed");
    setError(null);
    setOk(null);
    if (resolution?.workspace_override === "byok") {
      // Was BYOK — flip the override back to platform-managed.
      setSaving(true);
      try {
        await setMode("platform_managed");
        showToast("Switched to platform-managed billing", "success");
        setOk("Platform-managed. LLM usage is billed to org credits.");
        load();
      } catch (e) {
        setError(e instanceof Error ? e.message : "Failed to switch billing mode");
      } finally {
        setSaving(false);
      }
    }
  };

  const selectByok = () => {
    setChoice("byok");
    setError(null);
    setOk(null);
  };

  const saveByok = async () => {
    if (!apiKey.trim()) return;
    setSaving(true);
    setError(null);
    setOk(null);
    try {
      // 1. Store the org's key as a workspace secret on the platform agent.
      await api.put(`/workspaces/${platformId}/secrets`, {
        key: "ANTHROPIC_API_KEY",
        value: apiKey,
      });
      // 2. Flip billing mode to byok so the proxy stops metering this agent.
      await setMode("byok");
      setApiKey("");
      showToast("BYOK enabled for the platform agent", "success");
      setOk("BYOK enabled. Restart the platform agent to apply.");
      load();
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to enable BYOK");
    } finally {
      setSaving(false);
    }
  };

  const currentMode = resolution?.resolved_mode ?? "platform_managed";

  return (
    <div className={s.scard}>
      <div className={s.scardHead}>
        <div className={s.scardTitle}>LLM billing — platform agent</div>
        <div className={s.scardDesc}>
          Choose how the org concierge (platform agent) pays for model usage.
          Current resolved mode: <strong>{currentMode}</strong>.
        </div>
      </div>

      <div className={s.optList}>
        <label
          className={`${s.opt} ${choice === "platform_managed" ? s.optActive : ""}`}
        >
          <input
            type="radio"
            name="platform-billing"
            checked={choice === "platform_managed"}
            onChange={() => void selectPlatformManaged()}
            disabled={saving}
            style={{ position: "absolute", opacity: 0, pointerEvents: "none" }}
          />
          <span className={s.optRadio} aria-hidden="true" />
          <span className={s.optBody}>
            <span className={s.optTitle}>
              Platform-managed
              <span className={`${s.optTag} ${s.optTagCur}`}>default</span>
            </span>
            <span className={s.optDesc}>
              Metered through the Molecule proxy and billed to your org
              credits. No key required — recommended for most orgs.
            </span>
          </span>
        </label>

        <label className={`${s.opt} ${choice === "byok" ? s.optActive : ""}`}>
          <input
            type="radio"
            name="platform-billing"
            checked={choice === "byok"}
            onChange={selectByok}
            disabled={saving}
            style={{ position: "absolute", opacity: 0, pointerEvents: "none" }}
          />
          <span className={s.optRadio} aria-hidden="true" />
          <span className={s.optBody}>
            <span className={s.optTitle}>Use my own API key (BYOK)</span>
            <span className={s.optDesc}>
              Supply your org&apos;s own ANTHROPIC_API_KEY. LLM traffic goes
              directly to Anthropic and is billed to your account.
            </span>
          </span>
        </label>
      </div>

      {choice === "byok" && (
        <div className={s.keyRow}>
          <label className={s.keyLabel} htmlFor="byok-anthropic-key">
            ANTHROPIC_API_KEY
          </label>
          <div className={s.keyInputRow}>
            <input
              id="byok-anthropic-key"
              type="password"
              className={s.keyInput}
              placeholder="sk-ant-..."
              value={apiKey}
              autoComplete="off"
              onChange={(e) => setApiKey(e.target.value)}
              disabled={saving}
            />
            <button
              type="button"
              className={`${s.btn} ${s.primary}`}
              disabled={saving || !apiKey.trim()}
              onClick={() => void saveByok()}
            >
              {saving ? "Saving…" : "Enable BYOK"}
            </button>
          </div>
          <div className={s.keyNote}>
            The key is stored encrypted as a workspace secret (<code>ANTHROPIC_API_KEY</code>)
            and never exposed to the browser. Restart the platform agent to apply.
          </div>
        </div>
      )}

      {error && <div className={`${s.sMsg} ${s.sMsgErr}`}>{error}</div>}
      {ok && <div className={`${s.sMsg} ${s.sMsgOk}`}>{ok}</div>}
    </div>
  );
}
