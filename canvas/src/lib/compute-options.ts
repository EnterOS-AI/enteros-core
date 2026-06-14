// Cloud-provider + instance-type metadata (core#2489).
//
// SSOT lives in the workspace-server (workspace_compute.go's allowlist + defaults)
// and is fetched at runtime from GET /compute/metadata (public, workspace-
// independent endpoint — the data is platform constraints, not org secrets), so
// the UI can never offer a (provider, instance-type) the PATCH validation then
// rejects with a 400. The constants below are ONLY a minimal offline fallback
// used until the fetch resolves (or if it fails) — they mirror the server SSOT
// but are not the source of truth. When the fetch succeeds, its data replaces
// them entirely.
//
// Response shape (workspace-server):
//   { providers: [{ id: "aws", label: "AWS (default)", default_instance: "t3.medium",
//                    instances: ["t3.medium", ...],
//                    display_default: "t3.xlarge" }, ...] }

export type ComputeOptions = {
  providers: string[];
  instanceTypes: Record<string, string[]>;
  defaults: Record<string, string>;
  displayDefaults: Record<string, string>;
  labels: Record<string, string>;
};

export const FALLBACK_COMPUTE_OPTIONS: ComputeOptions = {
  providers: ["aws", "hetzner", "gcp"],
  instanceTypes: {
    aws: ["t3.medium", "t3.large", "t3.xlarge", "t3.2xlarge", "m6i.large", "m6i.xlarge", "c6i.xlarge"],
    hetzner: ["cpx11", "cpx21", "cpx31", "cpx41", "cpx51", "cax11", "cax21", "cax31", "cax41"],
    gcp: ["e2-small", "e2-medium", "e2-standard-2", "e2-standard-4", "e2-standard-8"],
  },
  defaults: { aws: "t3.medium", hetzner: "cpx31", gcp: "e2-standard-2" },
  displayDefaults: { aws: "t3.xlarge", hetzner: "cpx41", gcp: "e2-standard-4" },
  labels: { aws: "AWS (default)", gcp: "GCP", hetzner: "Hetzner" },
};

export const normalizeProvider = (p?: string): string =>
  p === "gcp" || p === "hetzner" ? p : "aws";

export const instanceTypesForProvider = (opts: ComputeOptions, p?: string): string[] =>
  opts.instanceTypes[normalizeProvider(p)] ??
  opts.instanceTypes.aws ??
  FALLBACK_COMPUTE_OPTIONS.instanceTypes.aws;

export const defaultInstanceForProvider = (opts: ComputeOptions, p?: string): string =>
  opts.defaults[normalizeProvider(p)] ?? FALLBACK_COMPUTE_OPTIONS.defaults.aws;

export const displayDefaultForProvider = (opts: ComputeOptions, p?: string): string =>
  opts.displayDefaults[normalizeProvider(p)] ?? FALLBACK_COMPUTE_OPTIONS.displayDefaults.aws;

export const providerLabel = (opts: ComputeOptions, p?: string): string =>
  opts.labels[normalizeProvider(p)] ?? FALLBACK_COMPUTE_OPTIONS.labels.aws;

// Build ComputeOptions from the workspace-server /compute/metadata response.
// Returns null when the payload is not well-formed, so callers can keep the
// fallback.
export function parseComputeOptions(resp: unknown): ComputeOptions | null {
  if (!resp || typeof resp !== "object") return null;
  const { providers } = resp as { providers?: unknown };
  if (!Array.isArray(providers) || providers.length === 0) return null;

  const providerIds: string[] = [];
  const instanceTypes: Record<string, string[]> = {};
  const defaults: Record<string, string> = {};
  const displayDefaults: Record<string, string> = {};
  const labels: Record<string, string> = {};

  for (const p of providers) {
    if (!p || typeof p !== "object") continue;
    const { id, label, default_instance, display_default, instances } = p as {
      id?: unknown;
      label?: unknown;
      default_instance?: unknown;
      display_default?: unknown;
      instances?: unknown;
    };
    if (typeof id !== "string" || !id) continue;
    providerIds.push(id);
    if (typeof label === "string" && label) labels[id] = label;
    if (typeof default_instance === "string" && default_instance) defaults[id] = default_instance;
    if (typeof display_default === "string" && display_default) displayDefaults[id] = display_default;
    if (Array.isArray(instances) && instances.length > 0) {
      instanceTypes[id] = instances.filter((i): i is string => typeof i === "string" && Boolean(i));
    }
  }

  if (providerIds.length === 0) return null;

  return {
    providers: providerIds,
    instanceTypes: Object.keys(instanceTypes).length > 0 ? instanceTypes : FALLBACK_COMPUTE_OPTIONS.instanceTypes,
    defaults: Object.keys(defaults).length > 0 ? defaults : FALLBACK_COMPUTE_OPTIONS.defaults,
    displayDefaults: Object.keys(displayDefaults).length > 0 ? displayDefaults : FALLBACK_COMPUTE_OPTIONS.displayDefaults,
    labels: Object.keys(labels).length > 0 ? labels : FALLBACK_COMPUTE_OPTIONS.labels,
  };
}
