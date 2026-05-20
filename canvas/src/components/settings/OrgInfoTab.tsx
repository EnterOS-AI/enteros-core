'use client';

import { useCallback, useEffect, useState } from 'react';
import { api } from '@/lib/api';
import { fetchSession, type Session } from '@/lib/auth';
import { getTenantSlug } from '@/lib/tenant';
import { Spinner } from '@/components/Spinner';

/**
 * Organization-identity surface inside SettingsPanel.
 *
 * Closes a chronic UX gap where users (and our own AI agents) had to
 * call /cp/auth/me or /cp/orgs from browser devtools to read their
 * org_id UUID. Now: a copy-buttoned view of name + slug + UUID for the
 * currently-active org, plus a switcher list when the user belongs to
 * multiple orgs.
 *
 * Data path:
 *   1. fetchSession() → /cp/auth/me → current org_id
 *   2. api.get('/cp/orgs') → list of all orgs the user belongs to
 *   3. Match by id === session.org_id; fall back to host-slug match
 *      if the session probe loses the race.
 *
 * Read-only — this tab never mutates. Org creation/switching lives at
 * /orgs (the post-signup landing page).
 */

interface Org {
  id: string;
  slug: string;
  name: string;
  status?: string;
}

// /cp/orgs may return a bare array or {orgs: []} — see orgs/page.tsx
// for the same defensive unwrap.
type OrgsResponse = Org[] | { orgs?: Org[] };

export function OrgInfoTab() {
  const [orgs, setOrgs] = useState<Org[] | null>(null);
  const [session, setSession] = useState<Session | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const [sess, body] = await Promise.all([
          fetchSession().catch(() => null),
          api.get<OrgsResponse>('/cp/orgs'),
        ]);
        if (cancelled) return;
        setSession(sess);
        setOrgs(Array.isArray(body) ? body : body.orgs ?? []);
      } catch (e) {
        if (!cancelled) setError(e instanceof Error ? e.message : 'Failed to load org info');
      } finally {
        if (!cancelled) setLoading(false);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  const tenantSlug = getTenantSlug();
  const currentOrg =
    orgs?.find((o) => session && o.id === session.org_id) ??
    orgs?.find((o) => tenantSlug && o.slug === tenantSlug) ??
    null;
  const otherOrgs = orgs?.filter((o) => o.id !== currentOrg?.id) ?? [];

  if (loading) {
    return (
      <div
        role="status"
        aria-live="polite"
        className="flex items-center justify-center gap-2 py-6 text-ink-mid text-xs"
      >
        <Spinner /> Loading organization…
      </div>
    );
  }
  if (error) {
    return (
      <div className="p-4">
        <div className="px-3 py-2 bg-red-950/40 border border-red-800/50 rounded-lg text-[10px] text-bad">
          {error}
        </div>
      </div>
    );
  }
  if (!currentOrg) {
    return (
      <div className="p-4">
        <p className="text-xs text-ink-mid">
          No organization found for this session. If this is unexpected, sign out and back in, or visit{' '}
          <a href="/orgs" className="underline">/orgs</a>.
        </p>
      </div>
    );
  }

  return (
    <div className="p-4 space-y-4">
      <div>
        <h3 className="text-sm font-semibold text-ink mb-1">Current Organization</h3>
        <p className="text-[10px] text-ink-mid leading-relaxed">
          IDs you can paste into API calls, support tickets, or CLI arguments. The UUID never changes;
          the slug is the URL subdomain.
        </p>
      </div>
      <OrgIdentityCard org={currentOrg} highlighted />
      {otherOrgs.length > 0 && (
        <div className="space-y-2 pt-2">
          <h4 className="text-[11px] font-semibold text-ink-mid uppercase tracking-wider">
            Your other organizations ({otherOrgs.length})
          </h4>
          {otherOrgs.map((o) => (
            <OrgIdentityCard key={o.id} org={o} />
          ))}
        </div>
      )}
    </div>
  );
}

function OrgIdentityCard({ org, highlighted }: { org: Org; highlighted?: boolean }) {
  return (
    <div
      className={`rounded-lg border p-3 space-y-2 ${
        highlighted ? 'border-accent/40 bg-accent-strong/5' : 'border-line/40 bg-surface-card/40'
      }`}
      data-testid={`org-card-${org.slug}`}
    >
      <div className="flex items-baseline justify-between gap-2">
        <span className="text-[12px] font-medium text-ink truncate">{org.name}</span>
        {org.status && (
          <span className="text-[9px] text-ink-mid uppercase tracking-wider shrink-0">{org.status}</span>
        )}
      </div>
      <IdentityRow label="Slug" value={org.slug} />
      <IdentityRow label="UUID" value={org.id} mono />
    </div>
  );
}

function IdentityRow({ label, value, mono }: { label: string; value: string; mono?: boolean }) {
  const [copied, setCopied] = useState(false);
  const onCopy = useCallback(() => {
    // Best-effort: jsdom + old Safari throw synchronously on writeText.
    try {
      navigator.clipboard.writeText(value);
    } catch {
      /* user can still triple-click select */
    }
    setCopied(true);
    setTimeout(() => setCopied(false), 2000);
  }, [value]);
  return (
    <div className="flex items-center gap-2">
      <span className="text-[10px] text-ink-mid w-10 shrink-0">{label}</span>
      <code
        className={`flex-1 text-[11px] text-ink bg-surface-sunken/60 px-2 py-1 rounded select-all break-all ${
          mono ? 'font-mono' : ''
        }`}
      >
        {value}
      </code>
      <button
        type="button"
        onClick={onCopy}
        aria-label={`Copy ${label}`}
        className="shrink-0 px-2 py-1 bg-surface-card/60 hover:bg-surface-card border border-line/40 rounded text-[10px] text-ink-mid hover:text-ink transition-colors focus:outline-none focus-visible:ring-2 focus-visible:ring-accent focus-visible:ring-offset-1"
      >
        {copied ? 'Copied' : 'Copy'}
      </button>
    </div>
  );
}
