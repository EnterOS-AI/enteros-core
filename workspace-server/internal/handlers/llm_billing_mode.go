package handlers

// llm_billing_mode.go — per-workspace LLM billing mode resolution (internal#691).
//
// The resolver answers a single question at provision time:
//   "Should we strip CLAUDE_CODE_OAUTH_TOKEN + every vendor key from this
//    workspace's env, force-route to the CP proxy, and bill org credits?"
//
// That question used to be a single env-var read inside applyPlatformManagedLLMEnv:
//
//   os.Getenv("MOLECULE_LLM_BILLING_MODE") == "platform_managed"  → strip
//
// where MOLECULE_LLM_BILLING_MODE was an ORG-level value, fetched from CP's
// tenant_config and exported into the workspace-server process at boot. That
// shape made it impossible to mix billing modes across workspaces in the same
// org: turning the org dial to `byok` so one workspace could keep its OAuth
// stops the strip for EVERY workspace in the org. Turning it to `platform_managed`
// blocks every workspace's own OAuth/vendor keys.
//
// The resolver replaces the env-var read with a per-workspace lookup:
//
//   workspaces.llm_billing_mode (per-workspace override, NULLABLE)
//     ?? organizations.llm_billing_mode (org default, fetched via tenant_config)
//     ?? "platform_managed" (closed default — the existing implicit default)
//
// Default-closed contract — non-negotiable per the RFC Safety axis:
//
//   - workspace row missing (sql.ErrNoRows)         → fall through to org default
//   - DB error on the lookup                         → "platform_managed" + propagated error
//   - workspace override = NULL                      → fall through to org default
//   - workspace override = unknown string            → "platform_managed" (default-closed)
//   - org default = NULL / empty / unknown string    → "platform_managed" (closed default)
//   - org default = recognized non-pm string + ws null → org default (byok/disabled honored)
//
// The ONLY way to resolve to "byok" or "disabled" is an explicit, recognized
// string in the workspace override OR the org default. A NULL JOIN, transient
// resolver error, or garbled enum value MUST NOT silently flip a workspace
// off of platform_managed — that would shadow the org's billing policy and
// is the exact failure mode the RFC's Safety hot-spot calls out.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sync"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/crypto"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/providers"
)

// providerManifest is the parsed provider registry, loaded once. The registry
// is embedded (go:embed, no network) and immutable for the process lifetime, so
// a single Load is safe to memoize. A load failure is cached too (registryErr):
// it can only happen on a malformed embedded YAML, which is a build-time defect
// the verify-providers-gen + sync gates already catch, so failing closed
// (treat as "cannot derive" → platform default) is correct and we don't retry.
var (
	providerRegistryOnce     sync.Once
	providerRegistryManifest *providers.Manifest
	providerRegistryErr      error
)

func providerRegistry() (*providers.Manifest, error) {
	providerRegistryOnce.Do(func() {
		providerRegistryManifest, providerRegistryErr = providers.LoadManifest()
		if providerRegistryErr != nil {
			log.Printf("llm_billing_mode: FATAL — provider registry failed to load: %v (billing will default-closed to platform_managed)", providerRegistryErr)
		}
	})
	return providerRegistryManifest, providerRegistryErr
}

// Constants mirror molecule-controlplane/internal/credits/llm_billing.go.
// Kept as string literals (not imports) because workspace-server has no
// build-time dependency on the CP module; the values are stable wire
// strings used in the tenant_config response, the workspaces.llm_billing_mode
// column check constraint, and the CP route bodies.
const (
	LLMBillingModePlatformManaged = "platform_managed"
	LLMBillingModeBYOK            = "byok"
	LLMBillingModeDisabled        = "disabled"
)

// BillingModeSource describes which layer of the resolution stack supplied
// the final mode. Surfaced via the admin route for operator debug
// ("why is this workspace being stripped?") per the RFC Observability axis.
type BillingModeSource string

const (
	BillingModeSourceWorkspaceOverride BillingModeSource = "workspace_override"
	BillingModeSourceOrgDefault        BillingModeSource = "org_default"
	BillingModeSourceConstantFallback  BillingModeSource = "constant_fallback"
	// BillingModeSourceDerivedProvider means the mode was DERIVED from the
	// workspace's (runtime, model) via the provider registry — the SSOT
	// (internal#718 P2-B). IsPlatform(derived) → platform_managed, else byok.
	// This is the highest-precedence source after an explicit operator override
	// and SUPERSEDES the prior stored-LLM_PROVIDER read (#1966).
	BillingModeSourceDerivedProvider BillingModeSource = "derived_provider"
	// BillingModeSourceDerivedDefault means the registry could not derive a
	// provider for the (runtime, model) — no model, unknown runtime,
	// unregistered/ambiguous model — so the mode defaulted closed to
	// platform_managed (CTO-confirmed "unset → platform default"). Distinct from
	// derived_provider so operators can see "we defaulted" vs "we derived
	// platform".
	BillingModeSourceDerivedDefault BillingModeSource = "derived_default"
)

// BillingModeResolution is the structured answer the admin GET route returns
// and the strip gate logs at INFO. The same struct is the unit-test fixture
// shape, so the resolver test asserts both the mode AND the source per case
// (catches a bug where the right mode is returned via the wrong layer).
type BillingModeResolution struct {
	WorkspaceID       string            `json:"workspace_id"`
	ResolvedMode      string            `json:"resolved_mode"`
	WorkspaceOverride *string           `json:"workspace_override"` // nil = inherit
	OrgDefault        string            `json:"org_default"`        // RETIRED as a billing source (internal#718 P2-B); always platform_managed, kept for wire-compat
	Source            BillingModeSource `json:"source"`
	// ProviderSelection surfaces the DERIVED provider name (internal#718 P2-B)
	// when the mode came from the registry derivation — the literal provider the
	// (runtime, model) resolved to (e.g. "platform", "kimi-coding", "openai"), or
	// the raw model id when derivation failed. nil when an explicit operator
	// override or the empty-id default decided. Lets the admin route answer "why
	// is this workspace byok?" with the derived provider, not a stored value.
	ProviderSelection *string `json:"provider_selection"`
}

// defaultClosedBillingMode is the mode the resolver falls back to when it
// cannot DERIVE a provider (no model, unknown runtime, unregistered/ambiguous
// model, registry-load failure, or the pre-provision empty-id path).
//
// Historically this was an UNCONDITIONAL platform_managed ("unset → platform
// default", CTO 2026-05-27). That is correct on SaaS: an undecided workspace
// bills the platform proxy. But on a SELF-HOSTED stack there IS no Molecule
// proxy and no credit ledger (PlatformManagedProxyConfigured() == false), so a
// platform_managed default is unreachable — the provision path would inject no
// usable credential and fail closed (MISSING_PLATFORM_PROXY). On self-host the
// honest default is byok: the tenant must bring their own provider key, and the
// resolved mode should say so rather than advertise an impossible mode.
//
// Strictly gated on the no-proxy condition: when a proxy IS configured (SaaS),
// this returns platform_managed exactly as before — SaaS behavior is unchanged.
// This only changes the FALLBACK; an explicit operator override and a
// successfully-derived provider are decided before this is ever consulted.
func defaultClosedBillingMode() string {
	if PlatformManagedProxyConfigured() {
		return LLMBillingModePlatformManaged
	}
	return LLMBillingModeBYOK
}

// isKnownBillingMode is the enum-recognizer for the resolver's default-closed
// branch. Returning false for an unknown string forces the resolver to fall
// through to the next layer (or the constant fallback) — NEVER to honor a
// garbled value as if it were valid. This is what makes a row with mode='byokk'
// (typo) resolve to platform_managed instead of accidentally to byok.
func isKnownBillingMode(s string) bool {
	switch s {
	case LLMBillingModePlatformManaged, LLMBillingModeBYOK, LLMBillingModeDisabled:
		return true
	default:
		return false
	}
}

// readWorkspaceBillingOverride reads the OPTIONAL explicit operator override
// (workspaces.llm_billing_mode). Returns:
//
//	(mode, true,  nil) — a recognized override is set → operator pinned the mode
//	("",   false, nil) — NULL / garbled / row-missing → no explicit override
//	("",   false, err) — DB error → caller defaults closed + propagates
//
// internal#718 P2-B retires the org rung; this column is the ONLY stored
// billing signal that survives, and ONLY as an explicit override on top of the
// derived provider (CTO 2026-05-27).
func readWorkspaceBillingOverride(ctx context.Context, workspaceID string) (string, bool, error) {
	var wsOverride sql.NullString
	err := db.DB.QueryRowContext(ctx,
		`SELECT llm_billing_mode FROM workspaces WHERE id = $1`,
		workspaceID,
	).Scan(&wsOverride)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("resolve workspace llm_billing_mode override for %s: %w", workspaceID, err)
	}
	if wsOverride.Valid && isKnownBillingMode(wsOverride.String) {
		return wsOverride.String, true, nil
	}
	return "", false, nil
}

// ResolveLLMBillingModeDerived is the SSOT billing-mode resolver (internal#718
// P2-B). It DERIVES the provider from (runtime, model) via the provider
// registry and decides platform-vs-byok from IsPlatform(derived) — it does NOT
// read a stored LLM_PROVIDER (superseding #1966's stored-read approach) and
// does NOT read the org rung (retired, CTO 2026-05-27).
//
// Precedence (highest first):
//
//  1. EXPLICIT operator override (workspaces.llm_billing_mode, a recognized
//     value). The only stored billing signal that survives — an escape hatch,
//     not the primary signal.
//  2. DERIVE: providers.DeriveProvider(runtime, model, availableAuthEnv).
//     - resolves to the closed `platform` provider → platform_managed
//     - resolves to any other (BYOK/third-party) provider → byok  ← THE FIX
//  3. DEFAULT-CLOSED: derive fails (no model, unknown runtime, unregistered or
//     ambiguous model) → platform_managed (CTO "unset → platform default"). A
//     derive failure NEVER silently flips a workspace to byok (which would
//     strip the platform creds it may legitimately need).
//
// availableAuthEnv is the set of auth-env-var NAMES present for the workspace
// (never secret values) — the same disambiguation input DeriveProvider uses to
// split anthropic-oauth from anthropic-api. May be nil.
//
// A returned error never prevents a decision: ResolvedMode is always a valid
// enum value (default-closed). The error is informational (log + surface).
func ResolveLLMBillingModeDerived(ctx context.Context, workspaceID, runtime, model string, availableAuthEnv []string) (BillingModeResolution, error) {
	res := BillingModeResolution{
		WorkspaceID: workspaceID,
		// OrgDefault is retired as a billing source (internal#718 P2-B). Kept on
		// the struct for wire-compat (admin route / CP mirror) but always the
		// closed constant — never consulted in the decision.
		OrgDefault: LLMBillingModePlatformManaged,
	}

	// Pre-provision context (no workspace row yet): no override to read, default
	// closed. (DeriveProvider could still run from the passed runtime/model, but
	// the no-id path historically does no DB work and the strip gate only runs
	// post-create, so keep it a pure default to preserve that contract.)
	if workspaceID == "" {
		res.ResolvedMode = defaultClosedBillingMode()
		res.Source = BillingModeSourceDerivedDefault
		return res, nil
	}

	// Precedence 1: explicit operator override.
	if mode, ok, err := readWorkspaceBillingOverride(ctx, workspaceID); err != nil {
		// DB error — default closed AND propagate (never flip on a transient error).
		res.ResolvedMode = LLMBillingModePlatformManaged
		res.Source = BillingModeSourceConstantFallback
		return res, err
	} else if ok {
		m := mode
		res.WorkspaceOverride = &m
		res.ResolvedMode = mode
		res.Source = BillingModeSourceWorkspaceOverride
		return res, nil
	}

	// Precedence 2: DERIVE the provider from (runtime, model).
	manifest, mErr := providerRegistry()
	if mErr != nil || manifest == nil {
		// Registry unavailable (malformed embedded YAML — a build-time defect the
		// gates catch). Default closed (byok on self-host where no proxy exists).
		res.ResolvedMode = defaultClosedBillingMode()
		res.Source = BillingModeSourceDerivedDefault
		return res, mErr
	}
	provider, dErr := manifest.DeriveProvider(runtime, model, availableAuthEnv)
	if dErr != nil {
		// No model / unknown runtime / unregistered / ambiguous → default closed.
		// NOT an error to the caller: an unregistered model is a legitimate
		// "we can't say it's BYOK, so bill the platform default" outcome, and the
		// only-registered gate at the create/config API is where an unregistered
		// model is rejected loudly. Here we just fail closed for safety. On a
		// self-hosted stack (no proxy configured) the safe default is byok, since
		// platform_managed is unreachable there.
		res.ResolvedMode = defaultClosedBillingMode()
		res.Source = BillingModeSourceDerivedDefault
		sel := model
		if sel != "" {
			res.ProviderSelection = &sel
		}
		return res, nil
	}
	derivedName := provider.Name
	res.ProviderSelection = &derivedName
	res.Source = BillingModeSourceDerivedProvider
	if provider.IsPlatform() {
		res.ResolvedMode = LLMBillingModePlatformManaged
	} else {
		// A specific (non-platform) vendor was derived → bring-your-own-key.
		res.ResolvedMode = LLMBillingModeBYOK
	}
	return res, nil
}

// ResolveLLMBillingMode is the legacy-signature resolver retained for callers
// that do not have (runtime, model) in hand (the admin GET/PUT route and the
// secrets remote-pull path). It reads the workspace's stored runtime + model +
// available auth env from the DB and delegates to the DERIVED resolver
// (internal#718 P2-B) — the orgMode parameter is RETIRED (the org rung is no
// longer a billing source) and is ignored; it stays in the signature only to
// avoid churning the two callers in this PR. The architectural test asserts no
// remaining code path gates on os.Getenv("MOLECULE_LLM_BILLING_MODE") for the
// strip decision (that env is no longer read into the decision at all).
//
// Returning an error does NOT prevent the caller from making a decision —
// the returned mode is always a valid enum value (default-closed to
// platform_managed) so the caller can proceed without a separate fail-closed
// branch. The error is informational: log it, surface it to operators, but
// the strip-gate decision is already safe.
func ResolveLLMBillingMode(ctx context.Context, workspaceID, orgMode string) (BillingModeResolution, error) {
	_ = orgMode // org rung retired (internal#718 P2-B); parameter ignored.

	if workspaceID == "" {
		// Pre-provision context (templating, validation): default closed, no DB.
		return ResolveLLMBillingModeDerived(ctx, "", "", "", nil)
	}

	// Precedence 1: explicit operator override. Read it FIRST so an overridden
	// workspace short-circuits without the extra runtime/secrets reads (and so
	// the query order is override → runtime → secrets, matching the derived
	// resolver's own override-first precedence).
	if mode, ok, err := readWorkspaceBillingOverride(ctx, workspaceID); err != nil {
		return BillingModeResolution{
			WorkspaceID:  workspaceID,
			OrgDefault:   LLMBillingModePlatformManaged,
			ResolvedMode: LLMBillingModePlatformManaged,
			Source:       BillingModeSourceConstantFallback,
		}, err
	} else if ok {
		m := mode
		return BillingModeResolution{
			WorkspaceID:       workspaceID,
			OrgDefault:        LLMBillingModePlatformManaged,
			ResolvedMode:      mode,
			WorkspaceOverride: &m,
			Source:            BillingModeSourceWorkspaceOverride,
		}, nil
	}

	// Precedence 2: DERIVE. Read the stored (runtime, model, available-auth-env)
	// so the derived resolver can DeriveProvider for callers that don't carry
	// them (admin route, secrets remote-pull). A read miss/error degrades
	// gracefully: pass the empty/partial inputs through — DeriveProvider then
	// errors and the derived resolver defaults closed to platform_managed.
	//
	// ResolveLLMBillingModeDerived re-reads the override (NULL again here) before
	// deriving; that one extra cheap read keeps the derived resolver a complete,
	// independently-callable SSOT rather than splitting its precedence across two
	// functions.
	runtime, model, authEnv := readWorkspaceDeriveInputs(ctx, workspaceID)
	return ResolveLLMBillingModeDerived(ctx, workspaceID, runtime, model, authEnv)
}

// readWorkspaceDeriveInputs loads the workspace's stored runtime + selected
// model + the auth-env-var NAMES present in its secrets — the inputs
// DeriveProvider needs. Best-effort: any read error returns whatever was
// gathered (the derived resolver fails closed on incomplete inputs). The model
// is the MODEL workspace_secret (the canvas-picked id, written by setModelSecret
// / Create); runtime is the workspaces.runtime column (defaults claude-code).
// availableAuthEnv is the subset of secret KEYS that are recognized provider
// auth-env names (never values), so DeriveProvider's auth-env tie-break can fire
// the same way it does on the provision path.
func readWorkspaceDeriveInputs(ctx context.Context, workspaceID string) (runtime, model string, availableAuthEnv []string) {
	var rt sql.NullString
	if err := db.DB.QueryRowContext(ctx,
		`SELECT runtime FROM workspaces WHERE id = $1`, workspaceID,
	).Scan(&rt); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("llm_billing_mode: read runtime for %s: %v (deriving with empty runtime)", workspaceID, err)
		}
	}
	runtime = rt.String
	if runtime == "" {
		// Mirror the DB column default so an unset runtime still derives.
		runtime = "claude-code"
	}

	// Gather model + auth-env-name keys from workspace_secrets in one pass.
	authSet := authEnvNameSet()
	rows, err := db.DB.QueryContext(ctx,
		`SELECT key, encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = $1`,
		workspaceID,
	)
	if err != nil {
		log.Printf("llm_billing_mode: read secrets for %s: %v (deriving with no model/auth-env)", workspaceID, err)
		return runtime, model, availableAuthEnv
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var v []byte
		var ver int
		if rows.Scan(&k, &v, &ver) != nil {
			continue
		}
		if k == "MODEL" {
			if dec, derr := crypto.DecryptVersioned(v, ver); derr == nil {
				model = string(dec)
			}
			continue
		}
		// Only the KEY matters for auth-env disambiguation (the value is the
		// secret; we never decrypt it for this purpose). Record recognized
		// provider auth-env names.
		if _, ok := authSet[k]; ok {
			availableAuthEnv = append(availableAuthEnv, k)
		}
	}
	if err := rows.Err(); err != nil {
		log.Printf("llm_billing_mode: read secrets rows error for %s: %v (deriving with partial model/auth-env)", workspaceID, err)
	}
	return runtime, model, availableAuthEnv
}

// authEnvNameSet is the union of every provider's auth_env names in the
// registry — the recognized set readWorkspaceDeriveInputs filters secret keys
// against. Loaded once from the registry so it stays in sync with the SSOT (no
// hardcoded auth-env vocabulary). Registry-load failure yields an empty set
// (derive then runs without the auth-env tie-break, which only matters for the
// oauth-vs-api overlap; safe — it errors to default-closed rather than guessing).
var (
	authEnvNameSetOnce sync.Once
	authEnvNameSetVal  map[string]struct{}
)

func authEnvNameSet() map[string]struct{} {
	authEnvNameSetOnce.Do(func() {
		authEnvNameSetVal = map[string]struct{}{}
		m, err := providerRegistry()
		if err != nil || m == nil {
			return
		}
		for _, p := range m.Providers {
			for _, e := range p.AuthEnv {
				authEnvNameSetVal[e] = struct{}{}
			}
		}
	})
	return authEnvNameSetVal
}

// availableAuthEnvNames returns the recognized provider auth-env-var NAMES
// present (non-empty) in envVars — the DeriveProvider auth-env tie-break input.
// Never returns secret VALUES, only the env-var names. Used by the provision
// path (applyPlatformManagedLLMEnv), which already has the workspace env in
// hand, so it derives without a secrets DB round-trip.
func availableAuthEnvNames(envVars map[string]string) []string {
	authSet := authEnvNameSet()
	var out []string
	for k, v := range envVars {
		if v == "" {
			continue
		}
		if _, ok := authSet[k]; ok {
			out = append(out, k)
		}
	}
	return out
}

// derefOrEmpty returns the pointed-to string or "" for a nil pointer. Used in
// log lines that surface an optional *string field.
func derefOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// SetWorkspaceLLMBillingMode writes the override column. Pass mode=="" to
// clear (set to NULL = inherit). Validates the mode against the enum set
// so the route handler doesn't have to duplicate validation; a garbled
// mode round-trips as an explicit 400 from the caller, not a CHECK-
// constraint error from the DB driver.
func SetWorkspaceLLMBillingMode(ctx context.Context, workspaceID, mode string) error {
	if workspaceID == "" {
		return errors.New("SetWorkspaceLLMBillingMode: workspace id required")
	}
	if mode == "" {
		// NULL = inherit. Caller asked to clear the override.
		res, err := db.DB.ExecContext(ctx,
			`UPDATE workspaces SET llm_billing_mode = NULL WHERE id = $1`,
			workspaceID,
		)
		if err != nil {
			return fmt.Errorf("clear workspace llm_billing_mode for %s: %w", workspaceID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("clear workspace llm_billing_mode rows affected %s: %w", workspaceID, err)
		}
		if n == 0 {
			return sql.ErrNoRows
		}
		return nil
	}
	if !isKnownBillingMode(mode) {
		return fmt.Errorf("unknown billing mode %q (allowed: %s, %s, %s)",
			mode, LLMBillingModePlatformManaged, LLMBillingModeBYOK, LLMBillingModeDisabled)
	}
	res, err := db.DB.ExecContext(ctx,
		`UPDATE workspaces SET llm_billing_mode = $1 WHERE id = $2`,
		mode, workspaceID,
	)
	if err != nil {
		return fmt.Errorf("set workspace llm_billing_mode for %s: %w", workspaceID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set workspace llm_billing_mode rows affected %s: %w", workspaceID, err)
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
