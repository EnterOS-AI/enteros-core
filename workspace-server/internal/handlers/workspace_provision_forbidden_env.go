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
//   - L2 (workspace/entrypoint.sh + template-* start.sh): in-container
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
//   - Infra-platform tokens: deploy / fleet-management creds.
//   - Operator-host pointers: hostnames / addresses that identify
//     the operator host. Per the open-source-template rule these
//     are MOLECULE_OPERATOR_HOST style prefixes; the literal
//     prefix is matched but the test for membership reads from
//     this map, not from a hardcoded constant in the deny rule
//     itself.
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
	"CP_ADMIN_API_TOKEN": {},
	"CP_ADMIN_TOKEN":     {},

	// Secret-store operator tokens (Infisical SSOT — operator scope only).
	"INFISICAL_OPERATOR_TOKEN":  {},
	"INFISICAL_BOOTSTRAP_TOKEN": {},

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
