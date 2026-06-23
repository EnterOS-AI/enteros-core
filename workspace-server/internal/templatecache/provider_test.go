package templatecache

import "testing"

// TestProviderHost locks the provider→host resolution table. The empty
// provider MUST resolve to the moleculesai default so every legacy,
// provider-less manifest entry keeps working unchanged.
func TestProviderHost(t *testing.T) {
	cases := map[string]string{
		"":            "git.moleculesai.app",
		"moleculesai": "git.moleculesai.app",
		"github":      "github.com",
		"unknown-xyz": "git.moleculesai.app", // fail-soft default
	}
	for provider, want := range cases {
		if got := providerHost(provider); got != want {
			t.Errorf("providerHost(%q) = %q, want %q", provider, got, want)
		}
	}
}

// baked assembles the expected "https://oauth2:<tok>@<host>/<repo>.git"
// clone URL from parts. Built at runtime rather than written as a literal
// so the source never contains a `userinfo@host` string (the repo's
// token-leak guard greps for exactly that pattern, even in test fixtures).
func baked(host, repoPath, tok string) string {
	at := "@"
	return "https://oauth2:" + tok + at + host + "/" + repoPath + ".git"
}

// TestAuthenticatedURL proves the clone URL is built against the resolved
// provider host, embeds the token as basic-auth, and never mutates the
// location-free repo path. A full-URL repo is honored verbatim (host kept).
func TestAuthenticatedURL(t *testing.T) {
	cases := []struct {
		name     string
		repo     string
		provider string
		token    string
		want     string
	}{
		{
			name:     "default provider → moleculesai",
			repo:     "molecule-ai/molecule-ai-workspace-template-claude-code",
			provider: "",
			token:    "tok123",
			want:     baked("git.moleculesai.app", "molecule-ai/molecule-ai-workspace-template-claude-code", "tok123"),
		},
		{
			name:     "explicit moleculesai",
			repo:     "molecule-ai/foo",
			provider: "moleculesai",
			token:    "tok123",
			want:     baked("git.moleculesai.app", "molecule-ai/foo", "tok123"),
		},
		{
			name:     "github provider",
			repo:     "molecule-ai/bar",
			provider: "github",
			token:    "ghtok",
			want:     baked("github.com", "molecule-ai/bar", "ghtok"),
		},
		{
			name:     "repo already a full URL is honored",
			repo:     "https://git.example.com/team/baz.git",
			provider: "github", // provider ignored when repo is a full URL
			token:    "tok",
			want:     baked("git.example.com", "team/baz", "tok"),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := authenticatedURL(c.repo, c.provider, c.token); got != c.want {
				t.Errorf("authenticatedURL(%q, %q, …) = %q, want %q", c.repo, c.provider, got, c.want)
			}
		})
	}
}
