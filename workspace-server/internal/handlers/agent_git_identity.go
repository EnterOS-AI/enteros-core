package handlers

import (
	"regexp"
	"strings"
)

// gitIdentitySlugPattern collapses any run of non-alphanumeric characters
// into a single hyphen when deriving an email localpart from a workspace
// name. Dots, parentheses, unicode dashes, whitespace — all get squashed.
var gitIdentitySlugPattern = regexp.MustCompile(`[^a-z0-9]+`)

// gitIdentityEmailDomain is the @-part of generated agent emails. These
// addresses are not deliverable — they're identity markers only. Using
// the project's canonical domain keeps them attributable without looking
// like they belong to a real human inbox. If this changes, also update
// docs/authorship.md (when it exists).
const gitIdentityEmailDomain = "agents.moleculesai.app"

// gitAskpassHelperPath is the in-container path of the askpass helper
// installed by every workspace runtime image (workspace/Dockerfile in
// molecule-core; scripts/git-askpass.sh → /usr/local/bin/molecule-askpass
// in each external template-* repo). The helper reads GIT_HTTP_USERNAME
// / GIT_HTTP_PASSWORD (falling back to GITEA_USER / GITEA_TOKEN) from
// env and emits them on the git credential-prompt protocol. Setting
// GIT_ASKPASS to this path is what wires container-side HTTPS git auth
// to the persona credentials already arriving via workspace_secrets,
// with no on-disk .gitconfig / .git-credentials mutation required.
const gitAskpassHelperPath = "/usr/local/bin/molecule-askpass"

// applyAgentGitIdentity sets GIT_AUTHOR_* / GIT_COMMITTER_* env vars so
// every commit from this workspace container carries a distinct author
// in `git log` and `git blame`. Git reads these env vars before falling
// back to `git config user.name` / `user.email`, so this works even if
// the container's git config is untouched.
//
// Idempotent + respectful: if any of the four variables is already set
// (e.g. by an operator-supplied workspace_secret), the existing value
// wins — this function only fills in the defaults.
//
// The workspace name is the display name from org.yaml ("Frontend
// Engineer", "Product Marketing Manager", "Research Lead"). The email
// localpart is the slugified form of that name. Empty workspace names
// leave the env untouched — we don't want to emit
// `unknown@agents.moleculesai.app` for a provisioning glitch that
// dropped the name.
func applyAgentGitIdentity(envVars map[string]string, workspaceName string) {
	if envVars == nil {
		return
	}
	workspaceName = strings.TrimSpace(workspaceName)
	if workspaceName == "" {
		return
	}

	authorName := "Molecule AI " + workspaceName
	slug := slugifyForEmail(workspaceName)
	authorEmail := slug + "@" + gitIdentityEmailDomain

	setIfEmpty(envVars, "GIT_AUTHOR_NAME", authorName)
	setIfEmpty(envVars, "GIT_AUTHOR_EMAIL", authorEmail)
	setIfEmpty(envVars, "GIT_COMMITTER_NAME", authorName)
	setIfEmpty(envVars, "GIT_COMMITTER_EMAIL", authorEmail)

	applyGitAskpass(envVars)
}

// applyGitAskpass points git at the in-image askpass helper so that any
// HTTPS git operation against a remote without a pre-configured
// credential.helper picks up the persona credentials already present in
// the container env (GIT_HTTP_USERNAME / GIT_HTTP_PASSWORD, or
// GITEA_USER / GITEA_TOKEN as fallback — the latter pair is what
// loadPersonaEnvFile delivers from the operator-host bootstrap kit).
//
// Idempotent: if GIT_ASKPASS is already set (e.g. by an operator-
// supplied workspace_secret or an env-mutator plugin), the existing
// value wins. This lets a workspace opt out by setting GIT_ASKPASS=""
// or pointing at a different helper.
//
// No vendor-specific behaviour lives in this function — the host the
// credentials apply to is determined entirely by the deployer choosing
// when to populate GIT_HTTP_USERNAME / GIT_HTTP_PASSWORD (or
// GITEA_USER / GITEA_TOKEN). The helper script itself is generic and
// has no hardcoded hostnames, so it's safe to ship inside the
// open-source workspace template images alongside the platform-managed
// claude-code image.
func applyGitAskpass(envVars map[string]string) {
	if envVars == nil {
		return
	}
	setIfEmpty(envVars, "GIT_ASKPASS", gitAskpassHelperPath)
}

// slugifyForEmail collapses a workspace name to a safe email localpart:
// lowercase, non-alphanumeric runs → single hyphen, stripped at edges.
// "Frontend Engineer" → "frontend-engineer".
// "Product Marketing Manager" → "product-marketing-manager".
// "UIUX Designer" → "uiux-designer".
func slugifyForEmail(name string) string {
	lowered := strings.ToLower(name)
	slug := gitIdentitySlugPattern.ReplaceAllString(lowered, "-")
	return strings.Trim(slug, "-")
}

func setIfEmpty(m map[string]string, key, val string) {
	if _, ok := m[key]; ok {
		return
	}
	m[key] = val
}
