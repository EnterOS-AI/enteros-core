// Friendly display names for workspace runtimes.
// Used in the chat indicator, details tab, and anywhere else we surface
// the runtime to the user.
//
// This map is a DOCUMENTED PROJECTION of two upstream SSOTs — not an
// independently-maintained list that is free to drift:
//   1. The CP runtime catalog (molecule-controlplane
//      internal/providers/runtimes.yaml) — the template-backed runtimes a
//      workspace can run on: claude-code, codex, hermes, and openclaw.
//      Canvas surfaces the short brand label; the catalog's display_name
//      is the long form (e.g. "Claude Code Agent" → "Claude Code").
//   2. The external-like / BYO-compute meta-runtimes — kimi and kimi-cli — the
//      set defined in ./externalRuntimes.ts, itself a mirror of the backend
//      externalLikeRuntimes SSOT (workspace-server
//      internal/handlers/runtime_registry.go). These have no platform-owned
//      container or template repo (operator brings the compute).
// A runtime missing here renders its raw id (see runtimeDisplayName), so a gap
// is only cosmetic — but keeping this in step with the two SSOTs above avoids a
// silent label drift (an unmapped runtime rendering as its bare id).

import { WORKSPACE_KIND } from "@/lib/workspace-kind";

const RUNTIME_NAMES: Record<string, string> = {
  // CP runtime catalog (runtimes.yaml) — template-backed runtimes.
  "claude-code": "Claude Code",
  codex: "Codex",
  hermes: "Hermes",
  openclaw: "OpenClaw",
  // External-like / BYO-compute meta-runtimes (SSOT: ./externalRuntimes.ts).
  kimi: "Kimi",
  "kimi-cli": "Kimi CLI",
};

export function runtimeDisplayName(runtime: string): string {
  return RUNTIME_NAMES[runtime] || runtime || "agent";
}

// Label for the "Processing with …" chat indicator. The platform agent (the
// org concierge) is a PRODUCT surface, not a "Claude Code" agent — it must
// surface its PLATFORM identity, never the underlying runtime it happens to run
// on. Ordinary workspaces show their runtime.
export function processingAgentLabel(
  kind: string | undefined,
  runtime: string,
): string {
  if (kind === WORKSPACE_KIND.Platform) return "the Org Concierge";
  return runtimeDisplayName(runtime);
}
