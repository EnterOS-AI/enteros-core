"use client";

import { useEffect, useRef, useState } from "react";
import { api } from "@/lib/api";

interface DisplayStatus {
  available: boolean;
  reason?: string;
  mode?: string;
  status?: string;
  protocol?: string;
  width?: number;
  height?: number;
}

interface DisplayControlStatus {
  controller: "none" | "user" | "agent";
  controlled_by?: string;
  expires_at?: string;
}

interface Props {
  workspaceId: string;
}

export function DisplayTab({ workspaceId }: Props) {
  const [status, setStatus] = useState<DisplayStatus | null>(null);
  const [control, setControl] = useState<DisplayControlStatus | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [controlError, setControlError] = useState<string | null>(null);
  const [controlBusy, setControlBusy] = useState(false);
  const requestGeneration = useRef(0);

  useEffect(() => {
    const generation = requestGeneration.current + 1;
    requestGeneration.current = generation;
    let cancelled = false;
    setStatus(null);
    setControl(null);
    setError(null);
    setControlError(null);
    setControlBusy(false);
    async function load() {
      try {
        const displayStatus = await api.get<DisplayStatus>(`/workspaces/${workspaceId}/display`);
        if (cancelled || requestGeneration.current !== generation) return;
        setStatus(displayStatus);
        if (displayStatus.reason === "display_not_enabled") return;
        try {
          const displayControl = await api.get<DisplayControlStatus>(`/workspaces/${workspaceId}/display/control`);
          if (!cancelled && requestGeneration.current === generation) setControl(displayControl);
        } catch (err) {
          if (!cancelled && requestGeneration.current === generation) {
            setControl(null);
            setControlError("Display control unavailable");
          }
        }
      } catch (err) {
        if (!cancelled && requestGeneration.current === generation) setError("The display status could not be loaded.");
      }
    }
    load();
    return () => {
      cancelled = true;
    };
  }, [workspaceId]);

  const acquireControl = async () => {
    const generation = requestGeneration.current;
    const controlPath = `/workspaces/${workspaceId}/display/control`;
    setControlBusy(true);
    setControlError(null);
    try {
      const next = await api.post<DisplayControlStatus>(`${controlPath}/acquire`, {
        controller: "user",
        ttl_seconds: 300,
      });
      if (requestGeneration.current !== generation) return;
      setControl(next);
    } catch (err) {
      if (requestGeneration.current !== generation) return;
      setControlError("Failed to take control");
      try {
        const latest = await api.get<DisplayControlStatus>(controlPath);
        if (requestGeneration.current !== generation) return;
        setControl(latest);
      } catch {
        if (requestGeneration.current !== generation) return;
        setControl(null);
      }
    } finally {
      if (requestGeneration.current === generation) setControlBusy(false);
    }
  };

  if (error) {
    return (
      <div className="p-5">
        <div className="rounded-lg border border-red-500/20 bg-red-950/20 p-4">
          <h3 className="text-sm font-medium text-red-200">Display status unavailable</h3>
          <p className="mt-2 text-[11px] leading-relaxed text-red-200/75">{error}</p>
        </div>
      </div>
    );
  }

  if (!status) {
    return (
      <div className="p-5">
        <div className="h-24 rounded-lg border border-line/40 bg-surface-sunken/30 motion-safe:animate-pulse" />
      </div>
    );
  }

  if (!status.available) {
    const isNotEnabled = status.reason === "display_not_enabled";
    return (
      <div className="flex min-h-full flex-col items-center justify-center bg-surface-sunken/30 p-8 text-center">
        <svg
          width="72"
          height="72"
          viewBox="0 0 72 72"
          fill="none"
          aria-hidden="true"
          className="mb-4 text-ink-mid"
        >
          <rect x="12" y="14" width="48" height="36" rx="4" stroke="currentColor" strokeWidth="2.5" opacity="0.65" />
          <path d="M28 58h16M36 50v8M16 16l40 40" stroke="currentColor" strokeWidth="3" strokeLinecap="round" />
        </svg>
        <h3 className="mb-1.5 text-sm font-medium text-ink">
          {isNotEnabled ? "Display is not enabled for this workspace." : "Display session is not ready."}
        </h3>
        <p className="max-w-xs text-[11px] leading-relaxed text-ink-mid">
          {isNotEnabled
            ? "Recreate this workspace with display enabled to view and take over its desktop."
            : "This workspace has display configuration, but the desktop session infrastructure is not configured yet."}
        </p>
        {!isNotEnabled && (
          <>
            <dl className="mt-5 grid grid-cols-2 gap-x-4 gap-y-2 text-left text-[11px]">
              <dt className="text-ink-mid">Mode</dt>
              <dd className="font-mono text-ink">{status.mode || "unknown"}</dd>
              <dt className="text-ink-mid">Status</dt>
              <dd className="font-mono text-ink">{status.status || "unknown"}</dd>
            </dl>
            <div className="mt-5 w-full max-w-xs border-t border-line/50 pt-4">
              {control ? (
                <div className="flex items-center justify-between gap-3 text-left">
                  <div className="min-w-0">
                    <p className="text-[11px] font-medium text-ink">
                      {control.controller === "none"
                        ? "No active controller"
                        : `Controlled by ${displayControlActorLabel(control)}`}
                    </p>
                    {control.expires_at && (
                      <p className="mt-1 truncate font-mono text-[10px] text-ink-mid">
                        Until {new Date(control.expires_at).toLocaleTimeString()}
                      </p>
                    )}
                    {controlError && <p className="mt-1 text-[10px] leading-snug text-red-200">{controlError}</p>}
                  </div>
                  {control.controller === "none" && (
                    <button
                      type="button"
                      onClick={acquireControl}
                      disabled={controlBusy}
                      className="h-8 shrink-0 rounded border border-line bg-surface px-3 text-[11px] font-medium text-ink hover:bg-surface-elevated disabled:cursor-not-allowed disabled:opacity-60"
                    >
                      Take control
                    </button>
                  )}
                </div>
              ) : (
                <div className="text-left">
                  {!controlError && (
                    <div className="h-8 rounded border border-line/40 bg-surface-sunken/30 motion-safe:animate-pulse" />
                  )}
                  {controlError && <p className="mt-2 text-[10px] leading-snug text-red-200">{controlError}</p>}
                </div>
              )}
            </div>
          </>
        )}
      </div>
    );
  }

  return null;
}

function displayControlActorLabel(control: DisplayControlStatus): string {
  if (control.controller === "agent") return "Agent";
  if (control.controlled_by === "admin-token") return "Admin";
  if (control.controlled_by?.startsWith("org-token:")) return "Automation";
  return "User";
}
