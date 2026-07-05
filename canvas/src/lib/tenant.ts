/**
 * Tenant slug derivation for SaaS-mode canvas.
 *
 * When canvas is served at <slug>.moleculesai.app the org slug comes from
 * the browser's hostname. When served anywhere else (localhost, Vercel
 * preview URL, direct vercel.app) we fall back to a configured slug
 * (NEXT_PUBLIC_DEFAULT_ORG_SLUG) or an empty string — API calls without
 * a slug hit the control plane's non-tenant routes.
 */

// SaaSHostSuffix is the domain this canvas is the tenant UI for. Parent
// domain with a leading dot; the hostname must end with this to be
// recognized as a tenant subdomain. Defaults to `.moleculesai.app` but
// is overridable via NEXT_PUBLIC_SAAS_HOST_SUFFIX for multi-brand or
// staging environments.
//
// NOTE: this is ONLY the "is this a tenant host?" gate — it is NOT used to
// EXTRACT the slug. The org slug is the leftmost DNS label (see
// getTenantSlug), which is correct regardless of how many labels the base has
// (prod `<slug>.moleculesai.app` OR staging `<slug>.staging.moleculesai.app`).
export const SaaSHostSuffix =
  process.env.NEXT_PUBLIC_SAAS_HOST_SUFFIX ?? ".moleculesai.app";

// centralHosts are the platform's OWN subdomains — our consoles and API — and
// must NEVER be treated as a tenant org slug. If the canvas lands on one of
// these it must derive an empty slug (and not send a bogus X-Molecule-Org-Slug
// header). This is the canvas-OWNED source of truth for the CENTRAL-host
// subset; it deliberately does NOT duplicate the control plane's full
// anti-impersonation blocklist (molecule-controlplane internal/reserved) — the
// canvas only needs "is this one of our own consoles?". Kept honest by the
// derivation below + __tests__/tenant.test.ts.
const centralHosts = [
  "app",
  "www",
  "api",
  "admin",
  "cp",
  "dashboard",
  "billing",
  "status",
  "docs",
];

// Per-environment console prefixes. The staging console is served at
// `staging-app.moleculesai.app` and the staging API at
// `staging-api.moleculesai.app` — i.e. `<prefix>-<centralHost>` directly under
// the apex, NOT `app.staging.moleculesai.app`. So every central host has an
// env-prefixed twin that must ALSO be reserved. We DERIVE these (host × prefix)
// rather than hand-listing `staging-app`/`staging-api`: that way adding a new
// central host — or a new environment — can never silently forget its staging
// twin. That forgotten twin is exactly what made staging-app.moleculesai.app
// resolve to a phantom tenant named "staging-app" and render the org view.
// Add a new environment here (e.g. "preview") to cover ALL of its consoles.
const envConsolePrefixes = ["staging"];

// reservedSubdomains = central hosts ∪ their env-prefixed console twins.
// Single source of truth so the staging consoles cannot drift out of the set.
const reservedSubdomains = new Set<string>(
  centralHosts.flatMap((h) => [
    h,
    ...envConsolePrefixes.map((prefix) => `${prefix}-${h}`),
  ]),
);

/**
 * getTenantSlug returns the tenant slug for the current request.
 *
 * Client-side: reads window.location.hostname.
 * Server-side (SSR / build): reads NEXT_PUBLIC_DEFAULT_ORG_SLUG, which is
 *   unset in production SaaS (we never SSR tenant pages without a host)
 *   but useful for local dev when the app is served at localhost:3000.
 *
 * Returns "" if no slug can be derived — callers must handle that case
 * (usually by redirecting to app.moleculesai.app for signup/org picker).
 */
export function getTenantSlug(): string {
  if (typeof window === "undefined") {
    return process.env.NEXT_PUBLIC_DEFAULT_ORG_SLUG ?? "";
  }
  const host = window.location.hostname.toLowerCase();
  if (!host.endsWith(SaaSHostSuffix)) {
    return process.env.NEXT_PUBLIC_DEFAULT_ORG_SLUG ?? "";
  }
  // The tenant slug is the FIRST (leftmost) DNS label — NOT a suffix-strip of
  // SaaSHostSuffix. Suffix-stripping breaks on any multi-label base: on staging
  // the tenant host is `<slug>.staging.moleculesai.app` while SaaSHostSuffix
  // defaults to `.moleculesai.app` (the override was wired into nothing), so the
  // old `host.slice(0, len - suffixLen)` yielded `<slug>.staging` — a nonexistent
  // slug the canvas then sent as `X-Molecule-Org-Slug: <slug>.staging`, which the
  // control plane resolves verbatim and 404s (the in-browser /workspaces 404).
  // The org label is always the leftmost label regardless of base depth, so the
  // first-label derivation is correct on prod AND staging with no per-env var.
  // Mirrors the /orgs "Open" link first-label fix (core#2509).
  const slug = host.split(".")[0];
  if (reservedSubdomains.has(slug)) return "";
  return slug;
}

/**
 * isSaaSTenant reports whether the canvas is running as the UI for a
 * SaaS tenant (served at <slug>.moleculesai.app). Use for client-side
 * UX branches that should behave differently on SaaS vs self-hosted —
 * e.g. the workspace tier picker hides T1/T2/T3 sandbox tiers because
 * every SaaS workspace gets its own EC2 VM (inherently T4 Full Access).
 *
 * SSR-safe: returns false on the server to avoid hydration drift; call
 * sites should tolerate a flip from false→true on first client render.
 */
export function isSaaSTenant(): boolean {
  if (typeof window === "undefined") return false;
  return getTenantSlug() !== "";
}
