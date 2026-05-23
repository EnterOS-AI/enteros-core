"use client";

import { useEffect, useState } from "react";
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

interface Props {
  workspaceId: string;
}

export function DisplayTab({ workspaceId }: Props) {
  const [status, setStatus] = useState<DisplayStatus | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    setStatus(null);
    setError(null);
    api
      .get<DisplayStatus>(`/workspaces/${workspaceId}/display`)
      .then((data) => {
        if (!cancelled) setStatus(data);
      })
      .catch((err) => {
        if (!cancelled) setError(err instanceof Error ? err.message : "Display status unavailable");
      });
    return () => {
      cancelled = true;
    };
  }, [workspaceId]);

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
          <dl className="mt-5 grid grid-cols-2 gap-x-4 gap-y-2 text-left text-[11px]">
            <dt className="text-ink-mid">Mode</dt>
            <dd className="font-mono text-ink">{status.mode || "unknown"}</dd>
            <dt className="text-ink-mid">Status</dt>
            <dd className="font-mono text-ink">{status.status || "unknown"}</dd>
          </dl>
        )}
      </div>
    );
  }

  return null;
}
