/**
 * Playwright global setup for the staging canvas E2E.
 *
 * Provisions a fresh staging org per run (POST /cp/admin/orgs), fetches
 * the per-tenant admin token, provisions one hermes workspace, waits
 * for online, then exports:
 *
 *   STAGING_TENANT_URL     https://<slug>.staging.moleculesai.app
 *   STAGING_WORKSPACE_ID   UUID of the hermes workspace
 *   STAGING_TENANT_TOKEN   per-tenant admin bearer (for spec requests)
 *   STAGING_SLUG           org slug (used by teardown)
 *
 * Required env:
 *   MOLECULE_CP_URL        default: https://staging-api.moleculesai.app
 *   MOLECULE_ADMIN_TOKEN   CP admin bearer sourced from Infisical staging
 *                          /shared/controlplane-admin. Drives provision +
 *                          tenant-token retrieval + teardown via a
 *                          single credential.
 *   STAGING_TENANT_DOMAIN  default: staging.moleculesai.app — the
 *                          DNS suffix the CP provisioner writes for
 *                          staging tenants. Override only when
 *                          running this harness against a non-default
 *                          zone.
 */

import type { FullConfig } from "@playwright/test";
import { writeFileSync } from "fs";
import { join } from "path";

const CP_URL = process.env.MOLECULE_CP_URL || "https://staging-api.moleculesai.app";
const ADMIN_TOKEN = process.env.MOLECULE_ADMIN_TOKEN;
const STAGING = process.env.CANVAS_E2E_STAGING === "1";
// Tenant DNS zone for staging. The configured CP provider registers DNS as
// `<slug>.staging.moleculesai.app`. The previous default of plain
// `moleculesai.app` matched prod tenant naming and silently broke
// every staging E2E at the TLS readiness step — DNS literally didn't
// resolve, fetch threw NXDOMAIN, waitFor saw null on every poll, and
// the harness wedged at TLS_TIMEOUT_MS instead of failing loud.
const TENANT_DOMAIN = process.env.STAGING_TENANT_DOMAIN || "staging.moleculesai.app";

// Provisioning backend for the throwaway staging org. This is THE input that
// wires the CP LLM proxy env (MOLECULE_LLM_BASE_URL / MOLECULE_LLM_USAGE_TOKEN
// / MOLECULE_LLM_ANTHROPIC_BASE_URL) into the tenant so a platform_managed
// agent can boot (post workspace-server #2162 fail-closed).
//
// Mechanism: CP's org-create validates `provider` against the cloudprovider
// SSOT and persists it as organizations.provider. The molecules-server
// (local-docker) provisioner injects the platform proxy env into the tenant's
// environment at provision (workspace-server derives it from the CP base per
// llm-proxy-https-only). An EMPTY provider falls back to the CP DefaultProvider
// (the closed AWS/SaaS path) which does NOT carry the proxy env on staging —
// that omission is why the canvas staging agent fail-closed with
// MISSING_PLATFORM_PROXY and the tabs/greeting specs had to be relaxed. The
// molecules-server lifecycle e2e (workspace_lifecycle_test.go adminCreateOrg)
// has ALWAYS sent provider=molecules-server and its agent boots + serves A2A
// fine; this ports that exact wiring to the canvas path so the agent boots
// here too and the agent-dependent assertions run for real.
const E2E_PROVIDER = process.env.E2E_PROVIDER || "molecules-server";

// Workspace runtime/model/provider for the tab-UI agent. Defaults to the
// platform-default pairing the molecules-server lifecycle e2e uses and that is
// PROVEN to boot to online + serve on staging (claude-code + minimax/MiniMax
// via the platform proxy — verified live: online in ~30s). The old hermes /
// moonshot-kimi pairing does NOT boot on staging within the provision window
// (empirically it stays failed/uptime=0 for 14+ min — a hermes-runtime install
// problem, NOT the proxy env, which IS present now that the org uses
// provider=molecules-server). The canvas tabs are runtime-agnostic (chat,
// activity, config, traces… render for any runtime), and testing the PLATFORM-
// DEFAULT runtime a real tenant actually boots is more representative than a
// niche runtime that can't come online. Env-overridable so the SSOT default can
// drive it (E2E_RUNTIME/E2E_MODEL, same knobs as the lifecycle e2e).
const E2E_WS_RUNTIME = process.env.E2E_RUNTIME || "claude-code";
const E2E_WS_MODEL = process.env.E2E_MODEL || "minimax/MiniMax-M2.7";
const E2E_WS_PROVIDER = process.env.E2E_WS_PROVIDER || "platform";
const E2E_WS_TIER = Number(process.env.E2E_TIER || "1");

// Tenant cold boot on staging regularly takes 12-15 min when the
// workspace-server Docker image isn't already cached on the AMI. Raised
// to 20 min to match tests/e2e/test_staging_full_saas.sh (PR #1930)
// after repeated "tenant provision: timed out after 900s" flakes
// were blocking staging→main syncs on 2026-04-24.
const PROVISION_TIMEOUT_MS = 20 * 60 * 1000;
const WORKSPACE_ONLINE_TIMEOUT_MS = 20 * 60 * 1000;

// TLS readiness depends on (1) Cloudflare DNS propagation through the
// edge, (2) the tenant's CF Tunnel registering the new hostname, (3)
// CF's edge ACME cert provisioning + cache. Each of these layers can
// add 1-3 min on its own under heavy staging load. Bumped 10→15 min
// after a burst of canary failures correlated with CP changes (#2090).
// Stays below the 20-min PROVISION_TIMEOUT envelope so a genuinely-
// stuck tenant fails-loud at the provision step rather than
// masquerading as a TLS issue. Kept aligned with
// tests/e2e/test_staging_full_saas.sh.
const TLS_TIMEOUT_MS = 15 * 60 * 1000;

async function jsonFetch(
  url: string,
  init: RequestInit = {},
): Promise<{ status: number; body: any }> {
  const res = await fetch(url, {
    ...init,
    headers: { "Content-Type": "application/json", ...(init.headers || {}) },
  });
  let body: any = null;
  try {
    body = await res.json();
  } catch {
    /* non-JSON */
  }
  return { status: res.status, body };
}

async function waitFor<T>(
  op: () => Promise<T | null>,
  deadlineMs: number,
  intervalMs: number,
  desc: string,
): Promise<T> {
  const deadline = Date.now() + deadlineMs;
  while (Date.now() < deadline) {
    const v = await op();
    if (v !== null) return v;
    await new Promise((r) => setTimeout(r, intervalMs));
  }
  throw new Error(`${desc}: timed out after ${Math.round(deadlineMs / 1000)}s`);
}

function makeSlug(): string {
  const y = new Date().toISOString().slice(0, 10).replace(/-/g, "");
  const rand = Math.random().toString(36).slice(2, 8);
  return `e2e-canvas-${y}-${rand}`.slice(0, 32);
}

export default async function globalSetup(_config: FullConfig): Promise<void> {
  if (!STAGING) {
    console.log("[staging-setup] CANVAS_E2E_STAGING not set, skipping");
    return;
  }
  if (!ADMIN_TOKEN) {
    throw new Error(
      "MOLECULE_ADMIN_TOKEN required (Infisical staging /shared/controlplane-admin)",
    );
  }

  const slug = makeSlug();
  const adminAuth = { Authorization: `Bearer ${ADMIN_TOKEN}` };
  console.log(`[staging-setup] Using slug=${slug}`);

  // Write the state file FIRST, before any CP call. Teardown (both
  // Playwright globalTeardown and the workflow safety-net) reads this
  // file to identify the slug it must clean up. If we wait until the
  // end of setup to write it (the previous behavior), a crash during
  // any of steps 1-6 leaves the org orphaned in CP with no record on
  // disk — forcing the workflow safety-net into a pattern-sweep over
  // every `e2e-canvas-<date>-*` org, which races with concurrent
  // canvas-E2E runs and deletes their live tenants. Race observed
  // 2026-04-30 on PR #2264 staging→main: three real-test runs killed
  // each other's tenants mid-test, surfacing as `getaddrinfo ENOTFOUND`
  // when CP cleaned up the just-deleted DNS record.
  const stateFile = join(process.cwd(), ".playwright-staging-state.json");
  writeFileSync(stateFile, JSON.stringify({ slug }, null, 2));

  // 1. Create org via admin endpoint — no WorkOS session needed
  const create = await jsonFetch(`${CP_URL}/cp/admin/orgs`, {
    method: "POST",
    headers: adminAuth,
    body: JSON.stringify({
      slug,
      name: `E2E Canvas ${slug}`,
      owner_user_id: `e2e-runner:${slug}`,
      // Route provisioning through molecules-server so the tenant carries the
      // CP LLM proxy env and the platform_managed agent boots (see E2E_PROVIDER
      // note above). Without this the org falls to the AWS/SaaS DefaultProvider
      // that omits the proxy env → agent fail-closes → agent-dependent specs
      // could not run.
      provider: E2E_PROVIDER,
    }),
  });
  if (create.status >= 400) {
    throw new Error(
      `POST /cp/admin/orgs ${create.status}: ${JSON.stringify(create.body)}`,
    );
  }
  console.log(`[staging-setup] Org created: ${slug}`);

  // 2. Wait for tenant running (admin-orgs list is the status source).
  //
  // The CP /cp/admin/orgs endpoint returns each org with an
  // `instance_status` field (handlers/admin.go:adminOrgSummary,
  // sourced from `org_instances.status`). NOT `status` — there's no
  // top-level `status` on the row at all. A previous version of this
  // test polled `row.status`, which was always undefined, so this
  // waitFor never resolved truthy and the harness invariably timed
  // out at 1200s — masking real CP bugs (see #242 chain) AND
  // surviving real CP fixes alike.
  // Capture the org UUID alongside the running check — every request
  // we send to the tenant URL after this point needs an
  // X-Molecule-Org-Id header (see workspace-server middleware/tenant_guard.go).
  // Without it, TenantGuard returns 404 ("must not be inferable by
  // probing other orgs' machines"). The CP returns the id on the
  // admin-orgs row; capture it here while we're already polling.
  let orgID = "";
  await waitFor<boolean>(
    async () => {
      const r = await jsonFetch(`${CP_URL}/cp/admin/orgs`, { headers: adminAuth });
      if (r.status !== 200) return null;
      const row = (r.body?.orgs || []).find((o: any) => o.slug === slug);
      if (!row) return null;
      if (row.instance_status === "running") {
        orgID = row.id;
        return true;
      }
      if (row.instance_status === "failed") {
        // Dump every diagnostic field the admin row carries — boot stage,
        // last error, terraform/SSM state, etc. The bare slug message used
        // to surface ZERO context, so triaging a failed provision meant
        // re-running locally to repro. Now the failure log carries enough
        // to point at the right subsystem (CP/AWS/SSM/runtime) without a
        // second round-trip.
        throw new Error(
          `provision failed: ${slug} — admin-orgs row: ${JSON.stringify(row)}`,
        );
      }
      return null;
    },
    PROVISION_TIMEOUT_MS,
    15_000,
    "tenant provision",
  );
  if (!orgID) {
    throw new Error(`expected admin-orgs row to carry id, got empty for slug=${slug}`);
  }
  console.log(`[staging-setup] Tenant running (org_id=${orgID})`);

  // 3. Fetch per-tenant admin token
  const tokRes = await jsonFetch(
    `${CP_URL}/cp/admin/orgs/${slug}/admin-token`,
    { headers: adminAuth },
  );
  if (tokRes.status !== 200 || !tokRes.body?.admin_token) {
    throw new Error(
      `tenant-token fetch ${tokRes.status}: ${JSON.stringify(tokRes.body)}`,
    );
  }
  const tenantToken: string = tokRes.body.admin_token;
  const tenantURL = `https://${slug}.${TENANT_DOMAIN}`;
  console.log(`[staging-setup] Tenant URL: ${tenantURL}`);

  // 4. TLS readiness
  await waitFor<boolean>(
    async () => {
      try {
        const res = await fetch(`${tenantURL}/health`, {
          signal: AbortSignal.timeout(5000),
        });
        return res.ok ? true : null;
      } catch {
        return null;
      }
    },
    TLS_TIMEOUT_MS,
    5_000,
    "tenant TLS",
  );

  // 5. Provision workspace
  //
  // tenantAuth carries TWO headers, both required:
  //   - Authorization: Bearer <admin-token>  — wsAdmin middleware gate
  //   - X-Molecule-Org-Id: <uuid>           — TenantGuard cross-org gate
  // Missing the org-id header silently 404s every non-allowlisted
  // route, with no body and no security headers. The 404 is intentional
  // (existence-non-inference) which makes it look like a missing route.
  const tenantAuth = {
    "Authorization": `Bearer ${tenantToken}`,
    "X-Molecule-Org-Id": orgID,
  };
  // Retry workspace creation on transient 5xx / timeout — staging CP can
  // return 502/503/504 under load and a single-shot failure kills the
  // entire E2E run. 3 attempts with 3s exponential backoff (3s, 6s, 12s)
  // gives ~21s total budget, well inside the 20-min provision envelope.
  let workspaceId = "";
  for (let attempt = 1; attempt <= 3; attempt++) {
    const ws = await jsonFetch(`${tenantURL}/workspaces`, {
      method: "POST",
      headers: tenantAuth,
      body: JSON.stringify({
        name: "E2E Canvas Test",
        runtime: E2E_WS_RUNTIME,
        tier: E2E_WS_TIER,
        model: E2E_WS_MODEL,
        // provider=platform resolves the agent to the platform LLM proxy
        // (MOLECULE_LLM_USAGE_TOKEN), which the tenant now carries via the
        // molecules-server org (E2E_PROVIDER). This is the exact platform-
        // managed pairing the lifecycle e2e boots + serves with.
        provider: E2E_WS_PROVIDER,
      }),
    });
    if (ws.status >= 200 && ws.status < 300 && ws.body?.id) {
      workspaceId = ws.body.id as string;
      break;
    }
    const isTransient = ws.status >= 500 || ws.status === 0;
    if (!isTransient || attempt === 3) {
      throw new Error(`Workspace create ${ws.status} (attempt ${attempt}): ${JSON.stringify(ws.body)}`);
    }
    const backoff = 3000 * Math.pow(2, attempt - 1);
    console.log(`[staging-setup] Workspace create transient ${ws.status}, retrying in ${backoff}ms...`);
    await new Promise((r) => setTimeout(r, backoff));
  }
  console.log(`[staging-setup] Workspace created: ${workspaceId}`);

  // 6. Wait for workspace online — the agent MUST boot.
  //
  // With provider=molecules-server (step 1) the tenant now carries the CP LLM
  // proxy env, so the platform_managed hermes agent boots normally — exactly
  // like the molecules-server lifecycle e2e, whose agent reaches online and
  // serves A2A. This gate therefore requires the REAL agent-booted signal
  // (status === "online") and no longer tolerates the #2162 pre-start
  // credential-abort: that "renderable but dead agent" shape was only a
  // symptom of the MISSING proxy env, which is now injected. Requiring online
  // is what re-enables the agent-dependent tab + greeting assertions to run
  // against a live agent (see staging-tabs.spec.ts, staging-slow-cold-
  // greeting.spec.ts) instead of the relaxed "renders + no crash" contract.
  //
  // Hermes cold-boot takes 10-13 min on slow apt days (apt + uv + hermes
  // install + npm browser-tools). The controlplane bootstrap-watcher deadline
  // can fire at 5 min and set status=failed prematurely; heartbeat then flips
  // failed → online once install.sh finishes. So a `failed` row with
  // uptime_seconds==0 AND no last_sample_error is a TRANSIENT still-booting
  // shape — we keep polling the real signal (never treat it as success). The
  // WORKSPACE_ONLINE_TIMEOUT_MS envelope (20 min ≈ ~2× the worst cold boot) is
  // a safety net we never wait out on the happy path: the loop breaks the
  // instant status flips to online. A genuine stuck-never-boots agent fails
  // loud at that envelope with the full body for triage.
  //
  // A `failed` carrying a last_sample_error, OR a non-zero uptime (the agent
  // started then crashed — image pull, panic, PYTHONPATH, etc.), is a REAL
  // boot regression and hard-throws immediately so triage gets the detail
  // without waiting for the polling timeout. Genuine *infra* provision failure
  // is already caught loud one step earlier at the org level
  // (instance_status === "failed").
  await waitFor<boolean>(
    async () => {
      const r = await jsonFetch(`${tenantURL}/workspaces/${workspaceId}`, {
        headers: tenantAuth,
      });
      if (r.status !== 200) return null;
      if (r.body?.status === "online") {
        return true;
      }
      if (r.body?.status === "failed") {
        const uptime = Number(r.body?.uptime_seconds ?? 0);
        const sampleErr = r.body?.last_sample_error;
        // Real boot regression: the agent ran and crashed (nonzero uptime) or
        // reported a concrete error. Fail loud immediately with the detail.
        if (uptime > 0 || sampleErr) {
          throw new Error(
            `[staging-setup] workspace ${workspaceId} boot FAILED: ` +
              `${sampleErr || "(no last_sample_error)"} ` +
              `uptime_seconds=${uptime}. This is a real boot regression, not the ` +
              `pre-#2162 credential gap (the proxy env is now injected via ` +
              `provider=${E2E_PROVIDER}). full body: ${JSON.stringify(r.body)}`,
          );
        }
        // uptime==0 AND no error → still-booting transient (bootstrap-watcher
        // fired before install.sh finished; heartbeat will flip to online).
        // Keep polling the real signal.
        console.warn(
          `[staging-setup] workspace ${workspaceId} transient 'failed' ` +
            `(uptime_seconds=0, no last_sample_error) — still booting, re-polling ` +
            `for online. full body: ${JSON.stringify(r.body)}`,
        );
        return null;
      }
      return null;
    },
    WORKSPACE_ONLINE_TIMEOUT_MS,
    10_000,
    "workspace online",
  );
  console.log(`[staging-setup] Workspace online`);

  // 7. Hand state off to tests + teardown — overwrite the slug-only
  // bootstrap state with the full state spec tests need.
  //
  // FAIL-CLOSED handoff: every field the spec reads must be non-empty. If
  // any is missing here, the spec's env-presence guard would throw with a
  // generic "did setup run?" message that hides WHICH field was lost. Catch
  // it at the source — a partial provision must hard-fail setup, never hand
  // off a half-built state that the spec then has to diagnose (or worse,
  // skip). This is the loud, fail-closed contract: STAGING was requested,
  // so an incomplete provision is an error, not a skip.
  const handoff = { slug, tenantURL, workspaceId, tenantToken };
  const missingFields = Object.entries(handoff)
    .filter(([, v]) => !v)
    .map(([k]) => k);
  if (missingFields.length > 0) {
    throw new Error(
      `[staging-setup] provision incomplete — empty handoff field(s): ` +
        `${missingFields.join(", ")}. Refusing to hand off a partial state ` +
        `that would surface downstream as an opaque spec failure.`,
    );
  }
  writeFileSync(stateFile, JSON.stringify(handoff, null, 2));
  process.env.STAGING_SLUG = slug;
  process.env.STAGING_TENANT_URL = tenantURL;
  process.env.STAGING_WORKSPACE_ID = workspaceId;
  process.env.STAGING_TENANT_TOKEN = tenantToken;
  // The workspace agent is now REQUIRED to have booted (step 6 only returns on
  // status===online, else it throws), so there is no longer an offline signal
  // to export. The agent-dependent specs run their full strong contract
  // unconditionally against the live agent.
  // The ephemeral org's UUID — exported so specs that route through the CP
  // edge can send X-Molecule-Org-Id (workspace-server TenantGuard). The
  // tabs harness (staging-tabs.spec.ts) and the take-control gate
  // (staging-display.spec.ts) both need it: TenantGuard rejects any
  // browser request that lacks X-Molecule-Org-Id with 401, which
  // surfaces as a hidden-Echo-node / "Failed to load" failure mode
  // inside the panels (run 353448/job 478063 @ sha 57ff36de).
  process.env.STAGING_ORG_ID = orgID;
  console.log(`[staging-setup] Ready — ${stateFile}`);

  // 8. (core#2261 Gap 1) Resolve the STANDING desktop-capable org, if one is
  // configured, for the live take-control e2e (staging-display.spec.ts).
  //
  // This block is FULLY env-gated and additive: it provisions NOTHING and is
  // a no-op unless STAGING_DISPLAY_SLUG is set. We deliberately do NOT spin a
  // desktop workspace inside this shared setup — a desktop AMI boots in
  // ~12-15 min and would tax every tabs run. Instead an operator stands up
  // one always-on desktop org once (a CTO cost item) and points
  // STAGING_DISPLAY_SLUG + STAGING_DISPLAY_WORKSPACE_ID at it. Here we just
  // resolve that standing org's tenant URL, admin token, and org id so the
  // display spec can reach it. Fail-closed: if STAGING_DISPLAY_SLUG is set but
  // we can't resolve its token/id, we THROW — the gate must never silently
  // fall back to the (non-desktop) ephemeral org and pass.
  const displaySlug = process.env.STAGING_DISPLAY_SLUG;
  if (displaySlug) {
    console.log(`[staging-setup] Resolving standing desktop org: ${displaySlug}`);

    // org id for the standing slug (admin-orgs row carries it + status).
    const orgsRes = await jsonFetch(`${CP_URL}/cp/admin/orgs`, { headers: adminAuth });
    if (orgsRes.status !== 200) {
      throw new Error(
        `STAGING_DISPLAY_SLUG=${displaySlug} set, but GET /cp/admin/orgs returned ` +
          `${orgsRes.status} — cannot resolve the standing desktop org for the ` +
          `take-control gate.`,
      );
    }
    const displayRow = (orgsRes.body?.orgs || []).find(
      (o: any) => o.slug === displaySlug,
    );
    if (!displayRow?.id) {
      throw new Error(
        `STAGING_DISPLAY_SLUG=${displaySlug} not found in /cp/admin/orgs — the ` +
          `standing desktop org for the take-control gate does not exist. Provision ` +
          `it as a standing desktop-capable tenant or unset STAGING_DISPLAY_SLUG/` +
          `STAGING_DISPLAY_WORKSPACE_ID to skip the gate.`,
      );
    }
    if (displayRow.instance_status !== "running") {
      throw new Error(
        `Standing desktop org ${displaySlug} is '${displayRow.instance_status}', ` +
          `not 'running' — the take-control gate needs a live desktop tenant. ` +
          `full row: ${JSON.stringify(displayRow)}`,
      );
    }

    const displayTokRes = await jsonFetch(
      `${CP_URL}/cp/admin/orgs/${displaySlug}/admin-token`,
      { headers: adminAuth },
    );
    if (displayTokRes.status !== 200 || !displayTokRes.body?.admin_token) {
      throw new Error(
        `admin-token fetch for standing desktop org ${displaySlug} returned ` +
          `${displayTokRes.status}: ${JSON.stringify(displayTokRes.body)}`,
      );
    }

    process.env.STAGING_DISPLAY_ORG_ID = displayRow.id;
    process.env.STAGING_DISPLAY_TENANT_URL = `https://${displaySlug}.${TENANT_DOMAIN}`;
    process.env.STAGING_DISPLAY_TENANT_TOKEN = displayTokRes.body.admin_token;
    console.log(
      `[staging-setup] Standing desktop org resolved: ${displaySlug} ` +
        `(org_id=${displayRow.id}, url=${process.env.STAGING_DISPLAY_TENANT_URL})`,
    );
  }
}
