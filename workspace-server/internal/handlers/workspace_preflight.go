package handlers

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// requiredEnvSchema is the subset of config.yaml we read to decide which env
// vars must be present before a container launch. It maps the YAML path
// `runtime_config.required_env: [...]` which is the same shape the workspace
// adapter's preflight reads inside the container
// (molecule-ai-workspace-runtime/molecule_runtime/preflight.py).
//
// Mirroring the check server-side lets us fail fast with a readable error
// instead of letting the container crash-loop and the workspace sit in
// `provisioning` until a sweeper or the user intervenes.
type requiredEnvSchema struct {
	RuntimeConfig struct {
		RequiredEnv []string `yaml:"required_env"`
	} `yaml:"runtime_config"`
}

// missingRequiredEnv returns the list of env var names declared in the
// workspace's config.yaml under `runtime_config.required_env` that are NOT
// present (or are empty) in the assembled envVars map. Returns an empty
// slice when the config declares no requirements or when all are satisfied.
//
// A parse failure returns no missing vars — config.yaml shape is enforced by
// the in-container preflight, and the server's job here is only to catch the
// common "forgot to add the OAuth token secret" footgun, not to be a second
// config validator.
func missingRequiredEnv(configFiles map[string][]byte, envVars map[string]string) []string {
	if len(configFiles) == 0 {
		return nil
	}
	raw, ok := configFiles["config.yaml"]
	if !ok || len(raw) == 0 {
		return nil
	}
	var schema requiredEnvSchema
	if err := yaml.Unmarshal(raw, &schema); err != nil {
		// Safe default: the in-container preflight is the source of truth
		// for config.yaml shape, so we don't block the provision here. But
		// log at WARN so operators can notice a template with malformed
		// YAML — otherwise a silently-skipped preflight is invisible.
		log.Printf("Preflight: WARN — config.yaml unparseable, skipping required_env check: %v", err)
		return nil
	}
	if len(schema.RuntimeConfig.RequiredEnv) == 0 {
		return nil
	}
	var missing []string
	for _, name := range schema.RuntimeConfig.RequiredEnv {
		if v, ok := envVars[name]; !ok || v == "" {
			missing = append(missing, name)
		}
	}
	return missing
}

// provisionInjectedEnvPrefixes / provisionInjectedEnvKeys enumerate the env
// names the provision pipeline assembles AFTER loadWorkspaceSecrets — model +
// provider resolution (applyRuntimeModelEnv / applyPlatformManagedLLMEnv),
// role + parent propagation, per-agent git identity + HTTP creds
// (applyAgentGitIdentity / applyAgentGitHTTPCreds), the declared-plugin list,
// and the external env-mutator chain (gh-identity, which contributes
// role-derived attribution vars).
//
// The create boundary CANNOT see any of them: the workspace has no assembled
// env yet. So a required_env entry in this set is UNDECIDABLE at create and is
// excluded from the create-time gate — the provision-time preflight
// (missingRequiredEnv over the fully assembled env) remains the authority for
// those, exactly as before. Everything else must come from a secret at org
// (global_secrets) or workspace (workspace_secrets) scope, both of which ARE
// readable at create — that is the decidable class the gate covers, and the
// class the reno-stars TENANT_* refusal fell into (molecule-core#5035).
//
// Prefixes, not just exact names, because the mutator chain is plugin-supplied
// and its exact key set is not knowable from this repo. Erring toward "the
// platform might supply it" keeps the gate from 422-ing a create that would
// have succeeded; the cost of a miss is only that the refusal happens at
// provision time instead of at POST — which, with #5035's read fix, is now
// visible either way.
var provisionInjectedEnvPrefixes = []string{
	"MOLECULE_",
	"GIT_",
	"GITHUB_",
	"GITEA_",
	"GH_",
	"ANTHROPIC_",
	"OPENAI_",
	"CLAUDE_",
}

var provisionInjectedEnvKeys = map[string]struct{}{
	"MODEL":                {},
	"MODEL_PROVIDER":       {},
	"LLM_PROVIDER":         {},
	"HERMES_DEFAULT_MODEL": {},
	"PARENT_ID":            {},
	"TZ":                   {},
}

// isProvisionInjectedEnv reports whether the provision pipeline may set `name`
// itself, making its absence at the create boundary uninformative.
func isProvisionInjectedEnv(name string) bool {
	if _, ok := provisionInjectedEnvKeys[name]; ok {
		return true
	}
	for _, p := range provisionInjectedEnvPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// createBoundaryMissingRequiredEnv returns the required_env entries that a
// POST /workspaces can already prove will not be satisfiable — i.e. the ones
// missing from the org/workspace secret env AND that no provision-time
// injector can supply (see isProvisionInjectedEnv).
//
// molecule-core#5035: without this, a create whose template declares
// unsatisfiable required_env returned 201 Created and the refusal landed ~2
// seconds LATER inside the provision goroutine, as a `failed` row. The caller
// was told nothing. This is the same check (missingRequiredEnv) against the
// same config source, moved to the boundary where the caller can still act on
// it.
//
// Fails OPEN in every ambiguous case — no config.yaml in the delivered files
// and none on disk at templatePath → nil. The provision-time preflight is
// unchanged and remains the backstop.
func createBoundaryMissingRequiredEnv(configFiles map[string][]byte, templatePath string, envVars map[string]string) []string {
	src := configFiles
	if _, ok := src["config.yaml"]; !ok {
		src = nil
	}
	if src == nil && templatePath != "" {
		if b, err := os.ReadFile(filepath.Join(templatePath, "config.yaml")); err == nil {
			src = map[string][]byte{"config.yaml": b}
		}
	}
	if src == nil {
		return nil
	}
	var decidable []string
	for _, name := range missingRequiredEnv(src, envVars) {
		if isProvisionInjectedEnv(name) {
			continue
		}
		decidable = append(decidable, name)
	}
	return decidable
}

// formatMissingEnvError builds the user-facing message for a provision
// failure caused by unset required env vars. Kept stable because it's
// rendered verbatim in the canvas Events tab and Details banner.
func formatMissingEnvError(missing []string) string {
	if len(missing) == 1 {
		return fmt.Sprintf(
			"missing required env var %q — add it under Config → Env Vars (or as a Global secret) and retry",
			missing[0],
		)
	}
	return fmt.Sprintf(
		"missing required env vars %s — add them under Config → Env Vars (or as Global secrets) and retry",
		strings.Join(missing, ", "),
	)
}

// formatMissingBYOKCredentialError builds the user-facing message for a
// provision failure caused by a non-platform (byok/subscription) workspace
// that has no usable LLM credential of its own (internal#711). The platform's
// scope:global LLM credentials are NOT a valid fallback for a non-platform
// workspace — resolving to them would bill the platform's Anthropic credits —
// so the provision fails closed here rather than starting the workspace on
// stripped/absent creds. Rendered verbatim in the canvas Events tab.
func formatMissingBYOKCredentialError() string {
	return "this workspace selected a BYOK provider (its own vendor key) but has no LLM credential of its own. " +
		"Add a workspace-scoped credential (e.g. CLAUDE_CODE_OAUTH_TOKEN or your provider's API key) under " +
		"Config → Secrets, or select a platform-managed model under Config → Model, then retry. The platform's " +
		"shared LLM credentials are not used for non-platform workspaces."
}

// formatMissingPlatformProxyError builds the user-facing message for a
// provision failure caused by a platform-managed workspace whose control-plane
// proxy environment is absent (#2162). The platform-managed path requires
// MOLECULE_LLM_BASE_URL + MOLECULE_LLM_USAGE_TOKEN (or their OPENAI_*
// fallbacks) to inject a usable credential; without them the workspace must
// NOT start credential-less.
func formatMissingPlatformProxyError() string {
	return "this workspace is configured for platform-managed LLM billing but the control-plane proxy is not ready. " +
		"The required platform proxy env (MOLECULE_LLM_BASE_URL + MOLECULE_LLM_USAGE_TOKEN) is absent. " +
		"This is usually a transient boot-race; retry in 30 seconds. If it persists, verify the platform proxy " +
		"is configured for this tenant/runtime and contact the platform team."
}

// formatMissingModelError builds the user-facing message for a provision that
// reaches launch with no resolved model (core#2594). The platform no longer
// substitutes a default model (the MOLECULE_LLM_DEFAULT_MODEL env fail-open was
// removed); a model is required input. Rather than letting the runtime fall back
// to its hardcoded anthropic:claude-opus-4-7, the provision fails closed here.
func formatMissingModelError() string {
	return "this workspace reached provisioning with no model set. " +
		"The platform does not pick a default model — choose one under Config → Model " +
		"(or set the MODEL workspace secret) and retry. Provisioning is refused rather " +
		"than silently running an arbitrary model."
}
