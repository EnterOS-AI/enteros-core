/**
 * External-like (BYO-compute) runtime detection.
 *
 * Mirrors the backend's isExternalLikeRuntime() in
 * workspace-server/internal/handlers/runtime_registry.go.
 *
 * These runtimes have no platform-owned container — the operator installs
 * the agent CLI locally and calls /registry/register. They share UX
 * behaviour: no Files tab, no Terminal tab, no Docker config, and the
 * connection modal shows copy-paste snippets.
 *
 * This Set is the SSOT for canvas's "external-like" projection: the two
 * BYO-compute meta-runtimes here (kimi, kimi-cli) are the ones runtime-names.ts
 * surfaces alongside the CP runtime catalog, so the friendly-name map is a
 * documented projection (catalog runtimes + these) rather than a drifting list.
 * "external" is the generic BYO-compute handle and carries no friendly name.
 */

const EXTERNAL_LIKE_RUNTIMES = new Set([
  "external",
  "kimi",
  "kimi-cli",
]);

export function isExternalLikeRuntime(runtime: string | undefined): boolean {
  return !!runtime && EXTERNAL_LIKE_RUNTIMES.has(runtime);
}
