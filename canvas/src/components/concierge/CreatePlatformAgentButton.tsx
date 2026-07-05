"use client";

import { useCallback, useState } from "react";
import { api } from "@/lib/api";
import { useCanvasStore } from "@/store/canvas";
import type { WorkspaceData } from "@/store/socket";
import s from "./Concierge.module.css";

/**
 * CreatePlatformAgentButton — the "Create / repair platform agent" CTA shown in
 * the concierge "No platform agent yet" empty states (ConciergeShell Home chat +
 * Settings → Platform agent configuration). It turns those dead-ends into a
 * one-click create + a concierge-repair tool.
 *
 * CORE-ONLY by design: it calls CORE's OWN same-origin endpoint
 * `POST /admin/org/platform-agent/ensure` (the workspace-server), which derives
 * the platform-agent id IN core, runs the idempotent install, and triggers the
 * provision. It makes ZERO control-plane (`/cp/*`, MOLECULE_CP_URL) calls — the
 * whole point of the feature (molecule-core is OSS and must never depend on the
 * proprietary control plane). `api.post` targets the workspace-server, not the
 * control plane.
 *
 * Idempotent + repair-capable (the endpoint decides): a missing/degraded
 * concierge is created/repaired; a healthy one is a no-op.
 *
 * On success it re-hydrates the canvas so the freshly-installed kind='platform'
 * node surfaces immediately; the live socket then keeps its status fresh, and
 * the empty state (which is conditioned on `platformRoot`) unmounts this button
 * once the concierge appears — so there is no idle-state flash to reset.
 */
export function CreatePlatformAgentButton() {
  const [provisioning, setProvisioning] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const onClick = useCallback(async () => {
    setError(null);
    setProvisioning(true);
    try {
      // CORE endpoint (workspace-server), same-origin. NOT a /cp/* call.
      await api.post("/admin/org/platform-agent/ensure", {});
      // Surface the new kind='platform' row immediately; the socket keeps it live.
      const ws = await api.get<WorkspaceData[]>("/workspaces");
      useCanvasStore.getState().hydrate(ws);
      // Intentionally leave `provisioning` true: a successful create makes the
      // concierge appear (platformRoot), which unmounts this component.
    } catch (e) {
      setProvisioning(false);
      setError(
        e instanceof Error && e.message
          ? e.message
          : "Could not create the platform agent. Please retry.",
      );
    }
  }, []);

  return (
    <div
      className={s.greetChips}
      style={{ flexDirection: "column", alignItems: "center", gap: 12 }}
    >
      <button
        type="button"
        data-testid="create-platform-agent"
        className={`${s.btn} ${s.primary}`}
        onClick={onClick}
        disabled={provisioning}
        aria-busy={provisioning}
      >
        {provisioning ? "Provisioning concierge…" : "Create / repair platform agent"}
      </button>
      {error && (
        <div role="alert" className={s.scardDesc} style={{ color: "var(--red)" }}>
          {error}
        </div>
      )}
    </div>
  );
}
