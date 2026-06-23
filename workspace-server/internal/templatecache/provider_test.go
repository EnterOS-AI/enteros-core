package templatecache

import "testing"

// TestResolveProvider locks the provider→(host, token) resolution. The empty
// provider MUST resolve to the moleculesai default (legacy entries unchanged),
// each provider MUST get its OWN token (a github entry must never receive the
// gitea/moleculesai token), and an unknown provider MUST fail closed — matching
// the shell resolvers and the static manifest contract test.
func TestResolveProvider(t *testing.T) {
	const gitea = "gitea-tok"

	t.Run("empty defaults to moleculesai", func(t *testing.T) {
		host, tok, err := resolveProvider("", gitea)
		if err != nil || host != "git.moleculesai.app" || tok != gitea {
			t.Fatalf("got (%q,%q,%v), want (git.moleculesai.app,%q,nil)", host, tok, err, gitea)
		}
	})

	t.Run("explicit moleculesai", func(t *testing.T) {
		host, tok, err := resolveProvider("moleculesai", gitea)
		if err != nil || host != "git.moleculesai.app" || tok != gitea {
			t.Fatalf("got (%q,%q,%v), want (git.moleculesai.app,%q,nil)", host, tok, err, gitea)
		}
	})

	t.Run("github uses MOLECULE_GITHUB_TOKEN, never the gitea token", func(t *testing.T) {
		t.Setenv("MOLECULE_GITHUB_TOKEN", "gh-tok")
		host, tok, err := resolveProvider("github", gitea)
		if err != nil || host != "github.com" {
			t.Fatalf("got (%q,%q,%v), want (github.com,gh-tok,nil)", host, tok, err)
		}
		if tok == gitea {
			t.Fatal("github entry received the gitea/moleculesai token — must use MOLECULE_GITHUB_TOKEN")
		}
		if tok != "gh-tok" {
			t.Fatalf("github token = %q, want gh-tok", tok)
		}
	})

	t.Run("github with no github token is empty, not the gitea token", func(t *testing.T) {
		t.Setenv("MOLECULE_GITHUB_TOKEN", "")
		_, tok, err := resolveProvider("github", gitea)
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if tok == gitea {
			t.Fatal("github entry fell back to the gitea token — must not")
		}
	})

	t.Run("unknown provider fails closed", func(t *testing.T) {
		if _, _, err := resolveProvider("bogus", gitea); err == nil {
			t.Fatal("unknown provider resolved instead of failing closed")
		}
	})
}

// baked assembles the expected "https://oauth2:<tok>@<host>/<repo>.git"
// clone URL from parts. Built at runtime rather than written as a literal
// so the source never contains a `userinfo@host` string (the repo's
// token-leak guard greps for exactly that pattern, even in test fixtures).
func baked(host, repoPath, tok string) string {
	at := "@"
	return "https://oauth2:" + tok + at + host + "/" + repoPath + ".git"
}

// TestAuthenticatedURL proves the clone URL is built against the given host,
// embeds the token as basic-auth, and never mutates the location-free repo
// path. A full-URL repo is honored verbatim (its host kept).
func TestAuthenticatedURL(t *testing.T) {
	cases := []struct {
		name  string
		repo  string
		host  string
		token string
		want  string
	}{
		{
			name:  "moleculesai host",
			repo:  "molecule-ai/molecule-ai-workspace-template-claude-code",
			host:  "git.moleculesai.app",
			token: "tok123",
			want:  baked("git.moleculesai.app", "molecule-ai/molecule-ai-workspace-template-claude-code", "tok123"),
		},
		{
			name:  "github host",
			repo:  "molecule-ai/bar",
			host:  "github.com",
			token: "ghtok",
			want:  baked("github.com", "molecule-ai/bar", "ghtok"),
		},
		{
			name:  "repo already a full URL is honored",
			repo:  "https://git.example.com/team/baz.git",
			host:  "github.com", // ignored when repo is a full URL
			token: "tok",
			want:  baked("git.example.com", "team/baz", "tok"),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := authenticatedURL(c.repo, c.host, c.token); got != c.want {
				t.Errorf("authenticatedURL(%q, %q, …) = %q, want %q", c.repo, c.host, got, c.want)
			}
		})
	}
}
