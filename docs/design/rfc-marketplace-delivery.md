# RFC: Marketplace template/plugin delivery (entitlement-brokered, encrypted, automatic)

**Status:** Phase 1 design draft for CTO/driver sign-off
**Author:** CEO Assistant (on CTO direction, 2026-06-15) + Dev Engineer A (Kimi) (Phase 1 buildable spec)
**Related:** RFC #2843 (decouple config/skill delivery from Secrets Manager — the
public/token fetch this generalizes), CP #828 (the interim platform-token path),
template repos (claude-code/hermes/codex public; seo-agent private — our dogfood)

## 1. Summary

Molecule will run a **marketplace**: third-party developers publish **templates
and plugins** (via repo) that other orgs install into their workspaces. Sellers
must be able to keep their work **private and IP-protected** ("private for some
people"), with **encrypted** storage + delivery, and buyers must receive what
they're **entitled to** — **automatically**, at scale (design target: **~10K
published plugins, high daily install volume**).

The delivery path we have today does **not** meet this. RFC #2843 added two modes
to the runtime fetcher: **public-fetch** (tokenless, for our OPEN templates) and
**token-fetch** (a single platform-wide `MOLECULE_TEMPLATE_REPO_TOKEN`, CP #828,
for our OWN private templates). The platform token is **legitimate only because
the platform is the sole "seller" of its own templates**. As a *marketplace*
primitive it fails on every axis:

- **No per-seller isolation** — one token reads *all* private repos; a single
  leak exposes every seller's IP. Sellers won't publish.
- **No entitlement gating** — a fetch succeeds because the token exists, not
  because the org **licensed/purchased** the plugin.
- **No artifact encryption** — IP sits readable to anything holding the token.
- **Manual + O(plugins)** — minting/scoping per template is human work; it does
  not survive 10K plugins.

**Proposal:** a **delivery broker** + **entitlement service** + **encrypted
artifact store**, with **no standing god-credential** in workspaces and
**automatic** purchase→entitlement→delivery. The platform's own templates become
a *special case* of the same system (we are seller #0).

## 2. Goals / non-goals

**Goals**
- Per-seller IP isolation; a compromise of one tenant/box never exposes other
  sellers' artifacts.
- **Entitlement-gated** delivery: an org receives a plugin/template **iff** it
  holds a current entitlement (purchase / subscription / free-grant).
- **Encrypted** artifacts at rest and in delivery; sellers' source is never
  readable by infra operators by default.
- **Automatic** end-to-end: publish → buy → entitlement → delivered on next
  provision/restart. Zero per-plugin manual ops.
- **Revocation + versioning**: unpublish/refund/expiry → next fetch denied;
  buyers pin a version; sellers ship updates.
- **Scale**: ~10K plugins, high install volume — horizontal, cache/CDN-friendly,
  no per-install human step.

**Non-goals (this RFC)**
- Billing/payments mechanics (separate; this RFC consumes an entitlement signal).
- The marketplace UI/discovery.
- Replacing the **public-fetch** path for our OPEN templates (it stays).

## 3. Design

### 3.1 Components
| Component | Responsibility |
|---|---|
| **Entitlement service** | SoT: `(org_id, plugin_id, version) → entitled?` (purchase/sub/free/grant), with expiry + revocation. |
| **Delivery broker** | Authenticates the requesting **workspace's own identity** (its workspace token / org identity), checks entitlement, returns a **short-lived, scoped, signed artifact URL** (or streams the decrypted bytes). Stateless; entitlement-cache. |
| **Encrypted artifact store** | Published artifacts stored encrypted (envelope encryption; per-seller or per-artifact data keys wrapped by a KMS CMK). Object store + CDN for signed-URL delivery. |
| **Publish pipeline** | Seller repo → CI packages the template/plugin → encrypts → registers `(plugin_id, version, seller, checksum)` → uploads to the artifact store. |

### 3.2 Delivery flow (provision/restart)
1. Workspace provisions/reconciles → asks the broker: *"deliver the assets org X
   is entitled to for this workspace."*
2. Broker authenticates the workspace's **own** identity (not a shared token),
   resolves the org's entitlements, and for each entitled `(plugin, version)`
   returns a **short-lived signed URL** (minutes TTL, scoped to that artifact).
3. Workspace fetches via the signed URL (CDN); artifact is decrypted for the
   entitled fetch (broker-side, or per-buyer envelope key).
4. No long-lived, broadly-scoped credential ever lives in the box.

### 3.3 The platform as "seller #0"
Our own templates are modeled as entitlements every org holds (free-grant for
the open ones; platform-internal for private like seo-agent). This means:
- The **public-fetch** path (RFC #2843) remains for our OPEN templates — cheapest
  path, no broker needed.
- Our OWN private templates migrate from the **#828 platform token** to the
  broker (as a free platform-internal entitlement) once the broker exists.
- We **dogfood** the marketplace with our own seo-agent before any third party.

### 3.4 Revocation, versioning, integrity
- Entitlement revoke (unpublish / refund / expiry) → broker denies next fetch;
  signed URLs are short-lived so access ends quickly.
- Buyers pin a version; sellers publish new versions; reconcile-on-boot
  (RFC #2843) picks up the entitled version.
- Artifact checksum verified post-fetch; signed manifests prevent tampering.

## 4. Phase 1: `template` field decoupling (platform-owned templates only)

Before the full broker exists, we will decouple a workspace's **runtime engine**
from its **template identity/assets** by adding an explicit `template` field.
This unblocks e.g. `runtime=claude-code` with `template=seo-agent`.
Phase 1 is intentionally **platform-owned templates only**; it uses the existing
#833 platform-token path as a temporary backend, but structures the code so the
broker can replace it without re-plumbing call sites.

**This section is the concrete buildable design the CTO must approve before
coding starts.** Implementation tracking: molecule-core#2980,
molecule-controlplane#846; detailed sub-RFC in molecule-core#2977.

### 4.1 What changes (Phase 1 buildable spec)

| Area | Change |
|---|---|
| DB | Add nullable `workspaces.template` column; `NULL` = runtime fallback. |
| Model | `Workspace.Template *string`; persist `CreateWorkspacePayload.Template`. |
| Resolver | Single `resolveTemplateAssets(ctx, template, runtime, workspaceID)` chokepoint in `runtime_registry.go`. |
| Write boundary | Validate `template` against manifest allowlist at create + `PATCH /workspaces/:id/template`. |
| Fetch boundary | Resolver allowlist check; unknown template fails closed. |
| CP wire | Forward `Template` and `TemplateAssets` in `cpProvisionRequest`. |
| Backfill | Idempotent `WHERE template IS NULL`; exact workspace-ID allowlist or `workspace_config.data->>template`; JRS `28f97a7f` canary. |
| Readiness | Probe `/configs/system-prompt.md` + `config.yaml`; `MISSING_ASSETS` fail-closed retry. |

### 4.2 Security model
- **`template` is an allowlist, never a free string.** It keys into the
  **manifest registry** (the same SSOT that #2959 pins to immutable commits).
  A value not in the manifest is rejected at the WRITE boundary (create/PATCH)
  **and** at the fetch boundary (defense-in-depth). It never falls through to a
  constructed path.
- **Platform-owned templates only.** The allowlist for Phase 1 is the set of
  platform-owned manifest entries (open templates + our private templates like
  seo-agent). No third-party or arbitrary private repo may be named.
- **Single chokepoint: `resolveTemplateAssets(template, runtime, workspace)`.**
  All asset resolution for a `template` value goes through this function. In
  Phase 1 it returns the #833 platform-token fetch identity; in Phase 2 the same
  chokepoint swaps to brokered entitlement + signed URLs. No other call site
  holds the platform token or constructs a template fetch URL.
- **No standing god-credential in the workspace.** The platform token is held
  server-side by the chokepoint, scoped read-only to platform-owned template
  repos, and never exposed to the box. The workspace receives only the final
  assets (or a short-lived signed URL once the broker lands).
- **Tenant isolation.** The fetch uses only the template-scoped read-only token;
  it must never escalate to the requesting workspace's tenant secrets and must
  never let one tenant's `template` value read another tenant's data.
- **SSRF guard.** If `template` ever influences a fetch URL, the HTTP path must
  apply the #2132 posture: dial-time IP guard, no redirects, explicit allowlist.

### 4.3 Workspace model and migration

Migration (idempotent, additive):

```sql
ALTER TABLE workspaces
    ADD COLUMN IF NOT EXISTS template TEXT;
```

- `NULL` means "no installed template — use runtime fallback". This is the
  steady state for every existing workspace and for bare `{"name":...}` creates.
- `models.Workspace` gains `Template *string `json:"template,omitempty" db:"template"``
  (or `sql.NullString`).
- `models.CreateWorkspacePayload.Template` already exists; persist it when
  non-empty.
- Create insert SQL becomes:
  ```sql
  INSERT INTO workspaces (..., runtime, template, status, ...)
  VALUES ($5, NULLIF($6, ''), 'provisioning', ...)
  ```

### 4.4 Single asset-resolution chokepoint

```go
// TemplateAssetResolution is the only thing callers of the asset channel need.
// In Phase 1 it carries a Gitea identity; in Phase 2 it can carry a broker-signed
// URL or an entitlement-bound fetcher.
type TemplateAssetResolution struct {
    Identity string // "<owner>/<repo>@<ref>" (Phase 1) or signed URL (Phase 2)
}

// resolveTemplateAssets maps a workspace's template/runtime to the manifest-
// registered asset source. It is the ONLY place that:
//   1. looks up templateRepoByName,
//   2. validates the allowlist,
//   3. decides whether to use the #833 platform-token path (Phase 1) or a
//      brokered entitlement (Phase 2).
func resolveTemplateAssets(
    ctx context.Context,
    template, runtime, workspaceID string,
) (TemplateAssetResolution, error) {
    if template != "" {
        rr, ok := templateRepoByName[template]
        if !ok {
            return TemplateAssetResolution{},
                fmt.Errorf("template %q is not in the manifest allowlist", template)
        }
        return TemplateAssetResolution{Identity: rr.Repo + "@" + rr.Ref}, nil
    }
    rr, ok := templateRepoByName[runtime]
    if !ok {
        // external / kimi / kimi-cli / mock: no template assets.
        return TemplateAssetResolution{}, nil
    }
    return TemplateAssetResolution{Identity: rr.Repo + "@" + rr.Ref}, nil
}
```

Rules:
1. If `template` is set and known, use it.
2. If `template` is set and unknown, fail closed.
3. If `template` is unset, fall back to the current `runtime` lookup.
4. `runtime` is authoritative for the engine; `template` is authoritative for
   assets. Precedence is acyclic.

Call sites:
- `workspace_provision.go` `buildProvisionerConfig` sets
  `cfg.TemplateIdentity = resolveTemplateAssets(...).Identity`.
- Restart/reconcile paths populate `payload.Template` from the DB row.

### 4.5 Create, restart, and PATCH paths

**Create path:** `workspace.go:Create` already accepts `template`. Validate it
against `templateRepoByName` at the write boundary and persist it (NULL when
empty). Runtime/model resolution from `config.yaml` stays unchanged.

**Restart path:** `workspace_restart.go` reads the stored `template` from the DB
and sets `payload.Template` when rebuilding `CreateWorkspacePayload`.

**PATCH /workspaces/:id/template:**
```
PATCH /workspaces/:id/template
{ "template": "seo-agent" }
```
- Validates `template` is a known manifest entry (fail-closed).
- Updates `workspaces.template`.
- Returns `{ "status": "updated", "needs_restart": true }`.
- Does **not** change `runtime`.
- Rejects cross-engine template changes in Phase 1.

### 4.6 Control-plane provision wire

- `provisioner.WorkspaceConfig` gains `Template string`.
- `cp_provisioner.go` `cpProvisionRequest` gains
  `Template string `json:"template,omitempty"`` and forwards existing
  `TemplateAssets`.
- `molecule-controlplane` `wsProvisionRequest` gains `Template string`.
- The CP stores `template` in its workspace record/metadata and echoes it back
  in status/reconcile responses.
- CP image selection still uses `runtime` (seo-agent uses the claude-code image
  via the manifest `"runtime": "claude-code"` mapping).

### 4.7 Backfill migration (SEO workspaces)

Two-part, fully idempotent backfill:

1. **Data-driven backfill** — workspaces that already recorded a template in
   `workspace_config.data`:
   ```sql
   UPDATE workspaces w
   SET template = NULLIF(TRIM(c.data->>'template'), '')
   FROM workspace_config c
   WHERE c.workspace_id = w.id
     AND w.template IS NULL
     AND NULLIF(TRIM(c.data->>'template'), '') IS NOT NULL
     AND EXISTS (
       SELECT 1 FROM manifest_allowed_templates m
       WHERE m.name = NULLIF(TRIM(c.data->>'template'), '')
     );
   ```

2. **SEO explicit-allowlist backfill** — one-off idempotent script for known SEO
   workspace IDs, starting with JRS `28f97a7f`. Never a loose string match on
   name/env/role.

Safety properties:
- **Idempotency:** gate on `WHERE template IS NULL`.
- **Tight predicate:** exact workspace-ID allowlist or exact
  `workspace_config.data->>template` signal.
- **Canary first:** JRS `28f97a7f`, verify, then fleet.
- **Reversible:** record changed set; companion script can reset `template = NULL`
  if needed.

### 4.8 Readiness gate and mid-flight changes

- **Probe-verified readiness.** Assets must be present at `/configs/system-prompt.md`
  (the #2955 lesson) and `config.yaml`. If missing, abort with `MISSING_ASSETS`
  and retry on next reconcile (same pattern as `MISSING_MODEL`, core#2594).
- **Fill-absent-only.** Asset delivery never overwrites files already present in
  `/configs/*` (#141 / #833).
- **Template change mid-flight** triggers a controlled re-fetch + restart inside
  the existing #2929 settle window. Fetch is idempotent and keyed on the CURRENT
  record value.
- **Manifest pins must be merged commits.** `template`→manifest resolution
  inherits the #2959 ancestor-of-default-branch gate.

### 4.9 JRS verification

After backfill + restart/re-provision of JRS `28f97a7f`:
- `resolveTemplateAssets("seo-agent", "claude-code", "28f97a7f")` resolves to
  `molecule-ai/molecule-ai-workspace-template-seo-agent@<pin>`.
- Template asset fetcher returns `agent-skills/seo-all/**`.
- Workspace boots with non-stub `/configs/config.yaml` and `agent_card.skills > 0`.
- Smoke check: `/seo-*` slash commands are registered.

### 4.10 Test plan

**Unit / integration (molecule-core)**
- `TestResolveTemplateAssets`: template set known, template set unknown fails
  closed, template empty runtime known, template empty external/kimi returns empty.
- `TestCreateWorkspace_PersistsTemplate`: create with `template=seo-agent` stores
  `template=seo-agent`, `runtime=claude-code`; unknown template rejected.
- `TestRestartWorkspace_UsesStoredTemplate`: restart reads `template` from DB.
- `TestPatchTemplate`: rejects unknown, updates known, returns `needs_restart`,
  rejects cross-engine.
- Migration test: backfill from `workspace_config.data->>template` works and
  does not clobber manually-set rows.
- Readiness test: missing probe path aborts with `MISSING_ASSETS`.

**E2E**
- Staging SEO workspace created with `template=seo-agent` boots with skills.
- JRS `28f97a7f` after tagging + restart: `agent_card.skills > 0`.
- Existing plain `claude-code` workspace without `template` continues to use
  `claude-code-default`.

### 4.11 Rollout

1. Land molecule-core PR: model + migration + resolver + restart + `PATCH /template`
   + backfill + tests.
2. Land molecule-controlplane PR: accept/store `template`.
3. Run backfill in prod (canary JRS `28f97a7f` first).
4. Trigger restart/re-provision for JRS; verify skills.
5. Tag remaining SEO workspaces from explicit allowlist and repeat verification.
6. Update RFC #2948 issue to mark Phase 1 complete and link Phase 2 design.

### 4.12 Top-3 decisions before coding

1. **The broker chokepoint:** `resolveTemplateAssets(ctx, template, runtime, workspaceID)`
   lives in `runtime_registry.go`. It is the sole caller of `templateRepoByName`,
   the sole place that knows about the #833 platform-token path, and the only
   seam the Phase 2 entitlement broker needs to wrap.
2. **The SEO backfill predicate:** idempotent `WHERE template IS NULL`, exact
   workspace-ID allowlist (JRS `28f97a7f` first) or exact
   `workspace_config.data->>template` signal, canary → fleet, resumable and
   reversible with a recorded changed-set.
3. **The readiness gate:** probe-verified assets at `/configs/system-prompt.md` /
   `config.yaml`; `MISSING_ASSETS` fail-closed + retry; mid-flight `template`
   changes use the #2929 settle window.

## 5. Relationship to RFC #2843 / #828
- **Public-fetch** (open templates): unchanged, keep.
- **#828 platform token** (our own private templates): **interim**. Legitimate
  today (we are sole seller), but **must not** become the marketplace mechanism.
  Superseded by the broker (our private templates → platform-internal
  entitlements) once it lands.
- The runtime fetcher already abstracts the source; adding a **broker fetch
  mode** alongside public/token is the runtime change.

## 6. Security
- **No standing god-credential** in workspaces — per-fetch authz, short-lived
  scoped signed URLs only.
- **Encryption at rest** (KMS-wrapped per-artifact data keys); operators can't
  read seller source by default; audit every decrypt/deliver.
- Per-seller blast-radius isolation; key compromise scoped to one seller.
- Entitlement checks are server-side; the workspace cannot self-assert
  entitlement.

## 7. Scale (~10K plugins, high install volume)
- Broker is stateless + horizontally scaled; entitlement reads cached.
- Delivery via signed-URL + CDN — bytes don't flow through the broker.
- Publish pipeline is per-seller-CI (parallel); no central manual step.
- Zero per-plugin human ops by construction (the failure mode this RFC exists
  to prevent).

## 8. Rollout (phased)
1. **Phase 0 (now, parallel):** ship #828 to deliver our OWN private templates
   (seo-agent → JRS) — interim, our-own-templates only. Unblocks the customer.
2. **Phase 1:** add the `template` field decoupling described in §4; keep using
   the #833 platform-token path behind the `resolveTemplateAssets` chokepoint;
   backfill SEO workspaces; dogfood with seo-agent. This is the design section
   the CTO must approve before coding starts.
3. **Phase 2:** entitlement service + broker + encrypted store; migrate our own
   private templates onto it; deprecate the #828 platform token for private
   delivery.
4. **Phase 3:** third-party publish pipeline + per-seller encryption keys +
   billing/entitlement integration + marketplace UI.

## 9. Alternatives considered
- **Per-seller long-lived tokens** injected per workspace: O(sellers) credentials,
  still no entitlement gating, still no encryption, still manual provisioning —
  rejected.
- **Keep the single platform token, add ACLs on the repo host:** no encryption,
  no entitlement semantics, repo-host-specific, doesn't scale to per-buyer —
  rejected.
- **Bake plugins into images:** breaks "seller owns/updates their plugin",
  no per-buyer entitlement — rejected.

## 10. Open questions
- Encryption model: per-seller data keys vs per-buyer envelope (re-encrypt per
  install)? KMS choice + key rotation.
- Entitlement SoT: new service vs extend CP; how billing emits the entitlement.
- Broker placement: CP endpoint vs dedicated service; CDN/object-store choice.
- Plugin vs template: same delivery primitive, or plugin-system-specific install?
- Trust/quality: seller verification, malware scanning, sandboxing of 3rd-party
  plugin code at install.
