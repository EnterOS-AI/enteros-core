package handlers

// workspace_provision_forbidden_env.go — Layer 1 of the RFC#523
// tenant-workspace forbidden-env guardrail (task #146).
//
// Threat model: tenant workspaces (per-customer EC2 / container)
// run untrusted agent-controlled code and MUST NEVER receive
// operator-fleet-scope credentials. A leak from one tenant
// workspace to operator scope would escalate "compromise of one
// agent" into "compromise of the whole platform."
//
// The existing forensic #145 guard (provisioner.scmWriteTokenKeys
// in buildContainerEnv / CPProvisioner.Start) strips SCM-write
// tokens at the FINAL container-env-build step — silent strip,
// no signal back to the caller. RFC#523 adds a FAIL-CLOSED layer
// EARLIER in the provision pipeline: when the resolved env-set
// at prepareProvisionContext-time contains any forbidden var
// name, the provision is aborted with a structured error so the
// operator sees the leak immediately instead of running with a
// silently-stripped env.
//
// Layer placement (3-layer defense-in-depth, RFC#523 §"Proposed guardrail"):
//   - L1 (this file): provisioner-side abort BEFORE container start
//   - L2 (standalone template entrypoint/start scripts): in-container
//     env-grep + exit 1 — defense-in-depth if L1 is bypassed
//   - L3 (.gitea/workflows/lint-forbidden-env-keys.yml): CI lint that
//     scans Go code under workspace-server/ for new writers that
//     would inject a forbidden key
//
// Open-source-template compatibility (memory
// `feedback_open_source_templates_no_hardcoded_org_internals`):
// the forbidden-key set is GENERIC (no molecule-AI-specific
// hostnames or org names). A third-party fork can replace this
// set with its own operator-scope key names without editing any
// template.

import (
	"fmt"
	"sort"
	"strings"
)

// forbiddenTenantEnvKeys is the set of environment variable names
// that MUST NOT reach a tenant workspace container. The check is
// by exact key name — value-shape leaks (40-byte hex strings, etc)
// are out of scope here; the separate secret-scan workflow covers
// that class.
//
// Categories (RFC#523):
//   - SCM-write tokens: same as provisioner.scmWriteTokenKeys, kept
//     in sync. Listed again here so a future split of the two
//     denylists is auditable diff.
//   - Control-plane admin tokens: any token that grants control-plane
//     admin API access.
//   - Secret-store operator tokens: bootstrap-scope tokens for the
//     central secret store.
//   - Infra-platform tokens: deploy / fleet-management credentials.
//   - Retired operator-host pointers: legacy MOLECULE_OPERATOR_HOST-style
//     variables are denied so old configuration cannot reintroduce a
//     privileged host address into an untrusted workspace.
//
// Per-agent persona PATs (e.g. AGENT_DEV_A_TOKEN style names —
// not operator-fleet scope) are NOT on this list. The guard
// checks the env VAR NAME, not the token VALUE, so a per-agent
// scoped token under a per-agent var name passes through.
var forbiddenTenantEnvKeys = map[string]struct{}{
	// SCM-write — kept in sync with provisioner.scmWriteTokenKeys.
	"GITEA_TOKEN":     {},
	"GITEA_PAT":       {},
	"GITHUB_TOKEN":    {},
	"GITHUB_PAT":      {},
	"GH_TOKEN":        {},
	"GITLAB_TOKEN":    {},
	"GL_TOKEN":        {},
	"BITBUCKET_TOKEN": {},

	// Control-plane admin tokens.
	"CP_ADMIN_API_TOKEN":        {},
	"CP_PROMOTE_PROD_API_TOKEN": {},
	"CP_ADMIN_TOKEN":            {},
	"ADMIN_TOKEN":               {},
	"MOLECULE_ADMIN_TOKEN":      {},

	// Control-plane shared/provision secrets (security-audit M5). One
	// fleet-wide value gating /cp/workspaces/* + re-served by
	// /cp/tenants/config — never per-tenant, so it must not sit in an
	// untrusted box's env where a single leak exposes the platform-wide
	// router gate.
	"PROVISION_SHARED_SECRET":   {},
	"MOLECULE_CP_SHARED_SECRET": {},

	// Platform at-rest encryption master key (security-audit H1). A single
	// global AES-256 key shipped to a box nullifies at-rest encryption as a
	// tenant-isolation control; the box must hold at most a per-tenant data
	// key it cannot use to reach other tenants.
	"SECRETS_ENCRYPTION_KEY": {},

	// Source-host read PAT for control-plane/private templates
	// (security-audit C2). An org-wide Gitea read PAT on a box exfiltrates
	// the whole control-plane source + every tenant's private template;
	// template fetch must be platform-proxied, not PAT-on-box.
	"MOLECULE_TEMPLATE_REPO_TOKEN": {},

	// Static cloud IAM keys (security-audit H2). A long-lived AWS access
	// key in a workspace is fleet-wide and remains forbidden even though the
	// current Molecule deployment no longer uses AWS/ECR.
	"AWS_ACCESS_KEY_ID":     {},
	"AWS_SECRET_ACCESS_KEY": {},
	"AWS_SESSION_TOKEN":     {},

	// Stale org-wide registry pull PAT (security-audit L1). Vestigial but
	// denied so it can never be repointed at a live registry from a box.
	"GHCR_PULL_TOKEN": {},

	// Secret-store operator tokens (Infisical SSOT — operator scope only).
	"INFISICAL_OPERATOR_TOKEN":  {},
	"INFISICAL_BOOTSTRAP_TOKEN": {},
	// Universal-auth machine-identity creds: the broad CP boot-fetch /
	// bootstrap identity (security-audit H8) must never reach a box.
	"INFISICAL_CLIENT_ID":     {},
	"INFISICAL_CLIENT_SECRET": {},

	// Infra-platform tokens.
	"RAILWAY_TOKEN":              {},
	"RAILWAY_PERSONAL_API_TOKEN": {},
	"HETZNER_TOKEN":              {},
	"HETZNER_API_TOKEN":          {},
}

// forbiddenTenantEnvPrefixes are key-name PREFIXES that match
// operator-scope env vars. Matched in addition to the exact-key
// set above. Useful for "MOLECULE_OPERATOR_*" style families
// where new members get added without re-editing the deny set.
//
// Kept as a tiny set on purpose — over-broad prefix matching is
// the failure mode this layer's exact-key set is designed to
// avoid. Add a prefix here only when the family is closed
// (every member is operator-scope; no legitimate tenant-scope
// member exists or will).
var forbiddenTenantEnvPrefixes = []string{
	"MOLECULE_OPERATOR_",
}

// isForbiddenTenantEnvKey reports whether an env var name is on
// the forbidden-for-tenant-workspaces list (either by exact match
// in forbiddenTenantEnvKeys or by prefix in
// forbiddenTenantEnvPrefixes).
//
// Exported-style helper kept package-private — the deny set is
// internal to the workspace-server package; external callers must
// go through the provision pipeline, which means the abort path
// fires for them too.
func isForbiddenTenantEnvKey(key string) bool {
	if _, ok := forbiddenTenantEnvKeys[key]; ok {
		return true
	}
	for _, prefix := range forbiddenTenantEnvPrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// findForbiddenTenantEnvKeys scans the resolved env-set and
// returns the sorted list of forbidden keys present. Empty slice
// (not nil — easier for callers to JSON-encode) when none match.
//
// Deterministic order: the result feeds the user-facing error
// message and the structured-extra payload that goes to the
// canvas Events tab. Sorting makes the message stable across
// Go's randomized map iteration.
//
// PROVENANCE NOTE: this helper checks by env-var name ONLY and is
// unaware of where each value came from. Production provision code
// uses findForbiddenTenantEnvKeysFromGlobals instead, restricting
// the check to keys originating from the operator-controlled
// global_secrets table — see the doc-comment on that function and
// the RFC#523 Layer 1 block in prepareProvisionContext. This name-
// only helper is kept for the workspace_secrets-write CI lint
// (Layer 3) and for tests that pin the deny-set definition.
func findForbiddenTenantEnvKeys(envVars map[string]string) []string {
	if len(envVars) == 0 {
		return []string{}
	}
	found := make([]string, 0)
	for k := range envVars {
		if isForbiddenTenantEnvKey(k) {
			found = append(found, k)
		}
	}
	sort.Strings(found)
	return found
}

// findPrivilegedTenantAdminEnvKeys scans the final env-set for tenant admin /
// control-plane admin key names. Unlike the broader RFC#523 global-secret check,
// these keys have no workspace-authored exception: normal workspaces must never
// receive tenant admin credentials, regardless of whether a late hook or config
// path introduced the name.
func findPrivilegedTenantAdminEnvKeys(envVars map[string]string) []string {
	if len(envVars) == 0 {
		return []string{}
	}
	found := make([]string, 0)
	for _, key := range []string{
		"ADMIN_TOKEN",
		"MOLECULE_ADMIN_TOKEN",
		"CP_ADMIN_API_TOKEN",
		"CP_ADMIN_TOKEN",
		"CP_PROMOTE_PROD_API_TOKEN",
	} {
		if _, ok := envVars[key]; ok {
			found = append(found, key)
		}
	}
	sort.Strings(found)
	return found
}

// findForbiddenTenantEnvKeysFromGlobals is the provenance-aware
// variant used by RFC#523 Layer 1 in prepareProvisionContext. It
// restricts the forbidden-key scan to keys whose value originated
// from the operator-controlled `global_secrets` table.
//
// Fixes the over-fire reported by CTO empirical 2026-05-20: a user
// who explicitly pastes their own scoped GitHub PAT under
// GITHUB_TOKEN into the canvas Secrets tab (a `workspace_secrets`
// row) was being blocked alongside the genuine operator-bleed case.
// RFC#523's threat model (issue molecule-ai/internal#523 §"Threat
// model") names operator-scope tokens injected via operator-side
// stores; user-authored workspace_secrets is out of scope.
//
// globalSecretKeys is the set returned as the second value from
// loadWorkspaceSecrets. A key that exists in BOTH stores is treated
// as workspace_secrets (user override wins) — loadWorkspaceSecrets
// drops the global flag when the workspace row is read.
//
// Empty/nil globalSecretKeys means no operator-side source was
// loaded (e.g. tests, or table empty); the scan returns no hits.
// Deterministic sort order, same as findForbiddenTenantEnvKeys.
func findForbiddenTenantEnvKeysFromGlobals(envVars map[string]string, globalSecretKeys map[string]struct{}) []string {
	if len(envVars) == 0 || len(globalSecretKeys) == 0 {
		return []string{}
	}
	found := make([]string, 0)
	for k := range globalSecretKeys {
		if _, present := envVars[k]; !present {
			// Defensive: a key flagged as global-origin must also
			// be in the resolved env-set. If not, skip — the
			// loadWorkspaceSecrets contract guarantees this never
			// happens, but the helper stays total.
			continue
		}
		if isForbiddenTenantEnvKey(k) {
			found = append(found, k)
		}
	}
	sort.Strings(found)
	return found
}

// formatForbiddenTenantEnvError builds the safe-canned user-facing
// message for a provision aborted because forbidden env keys are
// present in the resolved env-set. The message names the
// offending keys (key names are not secret — the values would be,
// but only names are surfaced) and points at the RFC.
//
// Same shape as formatMissingEnvError so the canvas Events tab
// renders both classes consistently.
func formatForbiddenTenantEnvError(keys []string) string {
	if len(keys) == 0 {
		// Defensive: caller should not invoke with empty input,
		// but keep the function total.
		return "provision aborted: forbidden operator-scope env vars present (RFC#523)"
	}
	if len(keys) == 1 {
		return fmt.Sprintf(
			"provision aborted: env var %q is operator-scope and must not reach tenant workspaces (RFC#523) — remove it from workspace_secrets / global_secrets and retry",
			keys[0],
		)
	}
	return fmt.Sprintf(
		"provision aborted: env vars %s are operator-scope and must not reach tenant workspaces (RFC#523) — remove them from workspace_secrets / global_secrets and retry",
		strings.Join(keys, ", "),
	)
}
