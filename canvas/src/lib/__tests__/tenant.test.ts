/**
 * @vitest-environment jsdom
 */
import { describe, it, expect, vi, afterEach } from 'vitest';
import { getTenantSlug } from '../tenant';

afterEach(() => {
  vi.unstubAllGlobals();
});

// Shim window.location.hostname for each case.
function setHost(host: string) {
  Object.defineProperty(window, 'location', {
    value: { hostname: host },
    writable: true,
  });
}

describe('getTenantSlug', () => {
  it('returns slug for tenant subdomain', () => {
    setHost('acme.moleculesai.app');
    expect(getTenantSlug()).toBe('acme');
  });

  it('is case-insensitive', () => {
    setHost('ACME.MoleculesAI.app');
    expect(getTenantSlug()).toBe('acme');
  });

  it('returns empty for reserved subdomains', () => {
    for (const s of ['app', 'www', 'api', 'admin']) {
      setHost(`${s}.moleculesai.app`);
      expect(getTenantSlug()).toBe('');
    }
  });

  it('returns empty for non-SaaS hosts', () => {
    setHost('localhost');
    expect(getTenantSlug()).toBe('');
  });

  it('returns empty for vercel preview URL', () => {
    setHost('molecule-canvas-abc123.vercel.app');
    expect(getTenantSlug()).toBe('');
  });

  it('returns empty for apex', () => {
    setHost('moleculesai.app');
    // doesn't end with "." + suffix
    expect(getTenantSlug()).toBe('');
  });

  // Regression: the staging 2-level subdomain. The old suffix-strip against the
  // (unwired) default SaaSHostSuffix `.moleculesai.app` yielded `acme.staging`,
  // which the canvas sent as `X-Molecule-Org-Slug: acme.staging` → CP 404 (the
  // in-browser /workspaces 404). First-label derivation returns the real slug.
  it('returns the leftmost label on a 2-level staging subdomain', () => {
    setHost('acme.staging.moleculesai.app');
    expect(getTenantSlug()).toBe('acme');
  });

  it('is case-insensitive on a staging subdomain', () => {
    setHost('ACME.Staging.MoleculesAI.app');
    expect(getTenantSlug()).toBe('acme');
  });

  it('returns empty for reserved subdomains on staging (first-label guard)', () => {
    for (const s of ['app', 'www', 'api', 'admin']) {
      setHost(`${s}.staging.moleculesai.app`);
      expect(getTenantSlug()).toBe('');
    }
  });

  // Regression (durable): the CENTRAL staging console lives at
  // staging-app.moleculesai.app (first DNS label "staging-app"), and the staging
  // API at staging-api.moleculesai.app — `<prefix>-<host>` directly under the
  // apex, NOT `app.staging.moleculesai.app`. Before the derived reserved set,
  // only bare "app"/"api" were reserved, so staging-app first-labelled to a
  // phantom tenant "staging-app" and the canvas rendered the org/tenant view on
  // the central staging host. This must stay empty.
  it('returns empty for the env-prefixed central consoles (staging-app / staging-api …)', () => {
    for (const s of [
      'staging-app',
      'staging-api',
      'staging-cp',
      'staging-admin',
      'staging-dashboard',
    ]) {
      setHost(`${s}.moleculesai.app`);
      expect(getTenantSlug(), `${s} must not resolve to a tenant`).toBe('');
    }
  });

  // Drift guard: every central host must have its staging twin reserved BY
  // CONSTRUCTION — you cannot add "app" without also reserving "staging-app".
  // If someone drops a host from centralHosts (or the derivation), the twin
  // stops being reserved and this trips. This is the tripwire that keeps the
  // staging consoles from silently drifting back out of the set.
  it('reserves the staging twin of every central host (no env-prefix drift)', () => {
    const centralHosts = [
      'app',
      'www',
      'api',
      'admin',
      'cp',
      'dashboard',
      'billing',
      'status',
      'docs',
    ];
    for (const h of centralHosts) {
      setHost(`staging-${h}.moleculesai.app`);
      expect(getTenantSlug(), `staging-${h} should be reserved`).toBe('');
    }
  });
});
