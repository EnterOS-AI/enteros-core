package handlers

// provider_derive_helpers.go — provider-registry derivation helpers shared by
// the provision path (applyPlatformManagedLLMEnv), the secrets bypass-key gate,
// and the templates-registry projection.
//
// The per-workspace `llm_billing_mode` field (and its override/resolver
// machinery) was removed (2026-06-30): the platform-vs-BYOK decision now derives
// purely from the workspace's own selection via the provider registry
// (providers.DeriveProvider). A workspace routes to the metered CP proxy iff its
// resolved provider is the closed `platform` arm (IsPlatform); any other resolved
// provider is BYOK (direct vendor key). When a provider cannot be derived (no
// model / unknown runtime / unregistered / ambiguous) the deployment falls back
// to platform iff a proxy is wired (PlatformManagedProxyConfigured), else BYOK on
// self-host. There is NO stored billing-mode signal anywhere — the decision is a
// pure function of (runtime, model, available auth env). These helpers supply the
// derivation inputs; the decision itself lives in applyPlatformManagedLLMEnv.

import (
	"context"
	"database/sql"
	"errors"
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

// providerRegistry loads the embedded providers manifest once and caches it.
// Defined as a variable (not a named function) so tests can swap in a mock
// without restarting the process — required for fail-closed coverage of the
// registry-unavailable path (workspace_provision_derive_test.go).
var providerRegistry = func() (*providers.Manifest, error) {
	providerRegistryOnce.Do(func() {
		providerRegistryManifest, providerRegistryErr = providers.LoadManifest()
		if providerRegistryErr != nil {
			log.Printf("provider_derive: FATAL — provider registry failed to load: %v (provider derivation will default-closed to platform when a proxy is wired)", providerRegistryErr)
		}
	})
	return providerRegistryManifest, providerRegistryErr
}

// readWorkspaceDeriveInputs loads the workspace's stored runtime + selected
// model + the auth-env-var NAMES present in its secrets — the inputs
// DeriveProvider needs. Best-effort: any read error returns whatever was
// gathered (the caller fails closed on incomplete inputs). The model is the
// MODEL workspace_secret (the canvas-picked id, written by setModelSecret /
// Create); runtime is the workspaces.runtime column (defaults to the platform
// default runtime on new rows).
// availableAuthEnv is the subset of secret KEYS that are recognized provider
// auth-env names (never values), so DeriveProvider's auth-env tie-break can fire
// the same way it does on the provision path.
func readWorkspaceDeriveInputs(ctx context.Context, workspaceID string) (runtime, model string, availableAuthEnv []string) {
	var rt sql.NullString
	if err := db.DB.QueryRowContext(ctx,
		`SELECT runtime FROM workspaces WHERE id = $1`, workspaceID,
	).Scan(&rt); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("provider_derive: read runtime for %s: %v (deriving with empty runtime)", workspaceID, err)
		}
	}
	runtime = rt.String
	if runtime == "" {
		// Mirror the DB column default so an unset runtime still derives. The
		// default FOLLOWS the platform default SSOT (MOLECULE_DEFAULT_RUNTIME,
		// KMS-injected) via bareCreateDefaultRuntime instead of a baked runtime
		// literal.
		runtime = bareCreateDefaultRuntime()
	}

	// Gather model + auth-env-name keys from workspace_secrets in one pass.
	authSet := authEnvNameSet()
	rows, err := db.DB.QueryContext(ctx,
		`SELECT key, encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = $1`,
		workspaceID,
	)
	if err != nil {
		log.Printf("provider_derive: read secrets for %s: %v (deriving with no model/auth-env)", workspaceID, err)
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
		log.Printf("provider_derive: read secrets rows error for %s: %v (deriving with partial model/auth-env)", workspaceID, err)
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
