package handlers

import (
	"testing"
)

// applyAgentGitIdentity is the platform-level chokepoint for per-agent
// commit authorship. These tests pin the generated name/email format
// and the operator-override semantics (workspace_secrets wins).

func TestApplyAgentGitIdentity_FillsFourVars(t *testing.T) {
	env := map[string]string{}
	applyAgentGitIdentity(env, "Frontend Engineer")

	cases := map[string]string{
		"GIT_AUTHOR_NAME":     "Molecule AI Frontend Engineer",
		"GIT_AUTHOR_EMAIL":    "frontend-engineer@agents.moleculesai.app",
		"GIT_COMMITTER_NAME":  "Molecule AI Frontend Engineer",
		"GIT_COMMITTER_EMAIL": "frontend-engineer@agents.moleculesai.app",
	}
	for k, want := range cases {
		if got := env[k]; got != want {
			t.Errorf("%s: got %q, want %q", k, got, want)
		}
	}
}

func TestApplyAgentGitIdentity_RespectsOperatorOverride(t *testing.T) {
	// If a workspace_secret already provides GIT_AUTHOR_NAME (the secret
	// loader runs before us), that operator intent wins. We only fill in
	// what isn't already set.
	env := map[string]string{
		"GIT_AUTHOR_NAME":  "Custom Name",
		"GIT_AUTHOR_EMAIL": "custom@example.com",
	}
	applyAgentGitIdentity(env, "Backend Engineer")

	if env["GIT_AUTHOR_NAME"] != "Custom Name" {
		t.Errorf("GIT_AUTHOR_NAME should not be overwritten, got %q", env["GIT_AUTHOR_NAME"])
	}
	if env["GIT_AUTHOR_EMAIL"] != "custom@example.com" {
		t.Errorf("GIT_AUTHOR_EMAIL should not be overwritten, got %q", env["GIT_AUTHOR_EMAIL"])
	}
	// The COMMITTER pair wasn't pre-set, so defaults fill it in.
	if env["GIT_COMMITTER_NAME"] != "Molecule AI Backend Engineer" {
		t.Errorf("GIT_COMMITTER_NAME should be filled, got %q", env["GIT_COMMITTER_NAME"])
	}
}

func TestApplyAgentGitIdentity_EmptyNameIsNoop(t *testing.T) {
	// A provisioning glitch where the workspace name arrived empty
	// shouldn't emit `unknown@agents.moleculesai.app` — those commits
	// are worse than no identity at all (they look like a real misconfig
	// rather than a recoverable state).
	env := map[string]string{}
	applyAgentGitIdentity(env, "")
	if len(env) != 0 {
		t.Errorf("empty name should leave env untouched, got %v", env)
	}
	// Whitespace-only name also counts as empty.
	applyAgentGitIdentity(env, "   ")
	if len(env) != 0 {
		t.Errorf("whitespace name should leave env untouched, got %v", env)
	}
}

func TestApplyAgentGitIdentity_NilMapIsSafe(t *testing.T) {
	// Defensive: never panic on a nil map (buildProvisionerConfig signature
	// doesn't guarantee non-nil). Tests the explicit nil-check.
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("applyAgentGitIdentity panicked on nil map: %v", r)
		}
	}()
	applyAgentGitIdentity(nil, "PM")
}

func TestApplyAgentGitIdentity_SetsGitAskpass(t *testing.T) {
	// GIT_ASKPASS is what wires container-side HTTPS git auth to the
	// persona credentials (GITEA_USER/GITEA_TOKEN, etc.) that
	// loadPersonaEnvFile delivers via workspace_secrets. Without this,
	// `git push` inside the container would fall through to interactive
	// prompts (impossible) or a missing credential.helper (401).
	env := map[string]string{}
	applyAgentGitIdentity(env, "Frontend Engineer")
	if env["GIT_ASKPASS"] != "/usr/local/bin/molecule-askpass" {
		t.Errorf("GIT_ASKPASS: got %q, want %q",
			env["GIT_ASKPASS"], "/usr/local/bin/molecule-askpass")
	}
}

func TestApplyAgentGitIdentity_RespectsAskpassOverride(t *testing.T) {
	// A workspace_secret or env-mutator plugin must be able to point at
	// a custom askpass helper without us clobbering it. Symmetric with
	// the GIT_AUTHOR_NAME override test above.
	env := map[string]string{
		"GIT_ASKPASS": "/opt/custom/askpass",
	}
	applyAgentGitIdentity(env, "Backend Engineer")
	if env["GIT_ASKPASS"] != "/opt/custom/askpass" {
		t.Errorf("GIT_ASKPASS should not be overwritten, got %q", env["GIT_ASKPASS"])
	}
}

func TestApplyAgentGitIdentity_AskpassSkippedOnEmptyName(t *testing.T) {
	// The empty-name early-return covers GIT_ASKPASS too — a provisioning
	// glitch that dropped the workspace name shouldn't half-configure the
	// container (identity vars empty but askpass wired). All-or-nothing.
	env := map[string]string{}
	applyAgentGitIdentity(env, "")
	if _, ok := env["GIT_ASKPASS"]; ok {
		t.Errorf("empty name should not set GIT_ASKPASS, got %q", env["GIT_ASKPASS"])
	}
}

func TestApplyGitAskpass_NilMapIsSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("applyGitAskpass panicked on nil map: %v", r)
		}
	}()
	applyGitAskpass(nil)
}

func TestSlugifyForEmail(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Frontend Engineer", "frontend-engineer"},
		{"Product Marketing Manager", "product-marketing-manager"},
		{"UIUX Designer", "uiux-designer"},
		{"PM", "pm"},
		{"SEO Growth Analyst", "seo-growth-analyst"},
		{"Social Media Brand", "social-media-brand"},
		// Odd cases: multiple spaces, punctuation, edge hyphens.
		{"  Extra  Spaces  ", "extra-spaces"},
		{"Role (with parens)", "role-with-parens"},
		{"em—dash", "em-dash"},
		{"---weird---", "weird"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := slugifyForEmail(tc.in); got != tc.want {
				t.Errorf("slugifyForEmail(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
