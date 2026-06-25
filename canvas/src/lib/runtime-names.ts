// Friendly display names for workspace runtimes.
// Used in the chat indicator, details tab, and anywhere else we surface
// the runtime to the user.

import { WORKSPACE_KIND } from "@/lib/workspace-kind";

const RUNTIME_NAMES: Record<string, string> = {
  "claude-code": "Claude Code",
  codex: "Codex",
  "google-adk": "Google ADK",
  hermes: "Hermes",
  openclaw: "OpenClaw",
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
