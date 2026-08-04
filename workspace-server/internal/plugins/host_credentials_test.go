package plugins

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// molecule-core#4997 — the API/on-demand plugin-install path must resolve a
// PER-ORG forge credential the same way molecule_runtime's boot-install path
// does, instead of reaching only for the platform-wide template PAT.
//
// The precedence under test is the SSOT in molecule_runtime
// plugin_sources._host_token_map:
//
//	1. MOLECULE_GIT_TOKENS       — JSON {"<host>": "<token>"}   (wins)
//	2. MOLECULE_GIT_TOKEN__<HOST> — flat per-host form           (setdefault)
//	3. the gitea read token (TokenEnv) seeded for the base host  (setdefault)

// envOf builds an `os.Environ()`-shaped slice from pairs, so the pure helper
// can be exercised without mutating the process environment.
func envOf(pairs ...string) []string { return pairs }

func TestHostTokenFor_FlatPerHostKey(t *testing.T) {
	got := HostTokenFor(
		envOf("MOLECULE_GIT_TOKEN__GIT_MOLECULESAI_APP=per-org-token"),
		"https://git.moleculesai.app", "MOLECULE_TEMPLATE_REPO_TOKEN",
	)
	if got != "per-org-token" {
		t.Fatalf("flat per-host key not resolved: got %q, want %q", got, "per-org-token")
	}
}

func TestHostTokenFor_PerHostBeatsTemplateToken(t *testing.T) {
	// The per-org credential is the org's OWN identity and must win over the
	// platform-wide template PAT, which is explicitly forbidden from reading
	// third-party private repos.
	t.Setenv("MOLECULE_TEMPLATE_REPO_TOKEN", "platform-wide-template-pat")
	got := HostTokenFor(
		envOf(
			"MOLECULE_GIT_TOKEN__GIT_MOLECULESAI_APP=per-org-token",
			"MOLECULE_TEMPLATE_REPO_TOKEN=platform-wide-template-pat",
		),
		"https://git.moleculesai.app", "MOLECULE_TEMPLATE_REPO_TOKEN",
	)
	if got != "per-org-token" {
		t.Fatalf("per-org token did not win over the template PAT: got %q", got)
	}
}

func TestHostTokenFor_JSONMapBeatsFlatKey(t *testing.T) {
	got := HostTokenFor(
		envOf(
			`MOLECULE_GIT_TOKENS={"git.moleculesai.app":"json-token"}`,
			"MOLECULE_GIT_TOKEN__GIT_MOLECULESAI_APP=flat-token",
		),
		"https://git.moleculesai.app", "MOLECULE_TEMPLATE_REPO_TOKEN",
	)
	if got != "json-token" {
		t.Fatalf("JSON map did not win over the flat key: got %q", got)
	}
}

func TestHostTokenFor_JSONMapCarriesPort(t *testing.T) {
	// The flat form cannot express a port; the JSON map can, and the lookup
	// host is the netloc — so a ported forge is only reachable via the map.
	got := HostTokenFor(
		envOf(`MOLECULE_GIT_TOKENS={"127.0.0.1:8443":"ported-token"}`),
		"https://127.0.0.1:8443", "MOLECULE_TEMPLATE_REPO_TOKEN",
	)
	if got != "ported-token" {
		t.Fatalf("ported host not resolved from the JSON map: got %q", got)
	}
}

func TestHostTokenFor_TemplateTokenStillSeedsBaseHost(t *testing.T) {
	// Regression guard: platform-owned private template repos must keep
	// working when no per-org credential is present.
	got := HostTokenFor(
		envOf("MOLECULE_TEMPLATE_REPO_TOKEN=template-pat"),
		"https://git.moleculesai.app", "MOLECULE_TEMPLATE_REPO_TOKEN",
	)
	if got != "template-pat" {
		t.Fatalf("template token no longer seeds the base host: got %q", got)
	}
}

func TestHostTokenFor_TokenNeverOfferedToAnotherHost(t *testing.T) {
	// The whole point of #4997: a credential is scoped to the host it is
	// keyed to. A github.com credential must never be handed to our gitea.
	got := HostTokenFor(
		envOf("MOLECULE_GIT_TOKEN__GITHUB_COM=github-token"),
		"https://git.moleculesai.app", "UNSET_TOKEN_ENV_VAR",
	)
	if got != "" {
		t.Fatalf("credential leaked across hosts: got %q, want empty", got)
	}
}

func TestHostTokenFor_DashedHostNotRecoverableFromFlatForm(t *testing.T) {
	// Mirrors molecule_runtime exactly: the flat suffix is unfolded with
	// lower().replace("_", "."), so a host containing '-' can never be
	// produced from it. Folding FORWARD instead would make Go offer the token
	// to a host the runtime would not — a silent divergence. Such an operator
	// must use the JSON map.
	got := HostTokenFor(
		envOf("MOLECULE_GIT_TOKEN__GIT_MY_CORP_COM=dashed-token"),
		"https://git.my-corp.com", "UNSET_TOKEN_ENV_VAR",
	)
	if got != "" {
		t.Fatalf("flat key folded forward onto a dashed host: got %q, want empty", got)
	}
	// ...and it unfolds to the dotted host, as the runtime does.
	if got := HostTokenFor(
		envOf("MOLECULE_GIT_TOKEN__GIT_MY_CORP_COM=dashed-token"),
		"https://git.my.corp.com", "UNSET_TOKEN_ENV_VAR",
	); got != "dashed-token" {
		t.Fatalf("flat key did not unfold to the dotted host: got %q", got)
	}
}

func TestHostTokenFor_MalformedJSONMapIsIgnored(t *testing.T) {
	// Never fail on a malformed credential blob — fall through to the next
	// source, exactly as the runtime does.
	got := HostTokenFor(
		envOf(
			"MOLECULE_GIT_TOKENS={not valid json",
			"MOLECULE_GIT_TOKEN__GIT_MOLECULESAI_APP=flat-token",
		),
		"https://git.moleculesai.app", "MOLECULE_TEMPLATE_REPO_TOKEN",
	)
	if got != "flat-token" {
		t.Fatalf("malformed JSON map did not fall through: got %q", got)
	}
}

func TestHostTokenFor_EmptyValuesSkipped(t *testing.T) {
	got := HostTokenFor(
		envOf(
			"MOLECULE_GIT_TOKEN__GIT_MOLECULESAI_APP=",
			"MOLECULE_TEMPLATE_REPO_TOKEN=template-pat",
		),
		"https://git.moleculesai.app", "MOLECULE_TEMPLATE_REPO_TOKEN",
	)
	if got != "template-pat" {
		t.Fatalf("empty flat value should not mask the seed: got %q", got)
	}
}

func TestHostTokenFor_FileBaseURLHasNoCredential(t *testing.T) {
	if got := HostTokenFor(
		envOf("MOLECULE_TEMPLATE_REPO_TOKEN=template-pat"),
		"file:///tmp/repos", "MOLECULE_TEMPLATE_REPO_TOKEN",
	); got != "" {
		t.Fatalf("file:// base must carry no credential: got %q", got)
	}
	// The load-bearing case: a file:// URL WITH an authority component parses
	// to a real-looking host, so without the explicit scheme guard a token
	// keyed to that host would be handed to a local-path fetch.
	if got := HostTokenFor(
		envOf("MOLECULE_GIT_TOKEN__GIT_EXAMPLE_COM=leaked-token"),
		"file://git.example.com/repos", "",
	); got != "" {
		t.Fatalf("file:// with an authority must carry no credential: got %q", got)
	}
	// A bare local path likewise.
	if got := HostTokenFor(
		envOf("MOLECULE_TEMPLATE_REPO_TOKEN=template-pat"),
		"/srv/bare-repos", "MOLECULE_TEMPLATE_REPO_TOKEN",
	); got != "" {
		t.Fatalf("bare local path must carry no credential: got %q", got)
	}
}

func TestHostTokenFor_PortIsPartOfTheCredentialDomain(t *testing.T) {
	// A forge on a non-default port is a DIFFERENT credential domain. If the
	// port were stripped, a token minted for a dev forge on :8443 would be
	// offered to whatever answers on :443 at the same hostname.
	env := envOf(`MOLECULE_GIT_TOKENS={"git.example.com:8443":"ported-token"}`)

	if got := HostTokenFor(env, "https://git.example.com:8443", ""); got != "ported-token" {
		t.Fatalf("exact host:port must match: got %q", got)
	}
	if got := HostTokenFor(env, "https://git.example.com", ""); got != "" {
		t.Fatalf("portless host must NOT match a ported credential: got %q", got)
	}
	if got := HostTokenFor(env, "https://git.example.com:9999", ""); got != "" {
		t.Fatalf("different port must NOT match: got %q", got)
	}

	// ...and the converse: a portless credential is not offered to a ported host.
	if got := HostTokenFor(
		envOf("MOLECULE_GIT_TOKEN__GIT_EXAMPLE_COM=portless-token"),
		"https://git.example.com:8443", "",
	); got != "" {
		t.Fatalf("portless credential must NOT match a ported host: got %q", got)
	}
}

func TestHostTokenFor_UserinfoIsStrippedFromTheLookupHost(t *testing.T) {
	env := envOf("MOLECULE_GIT_TOKEN__GIT_MOLECULESAI_APP=per-org-token")

	// Userinfo in the base URL must not defeat the host match.
	if got := HostTokenFor(env, "https://someuser@git.moleculesai.app", ""); got != "per-org-token" {
		t.Errorf("userinfo defeated the host match: got %q", got)
	}

	// SECURITY: a host smuggled into the USERINFO must not win the match. The
	// real host is what follows the '@', and that is what gets the credential.
	if got := HostTokenFor(env, "https://git.moleculesai.app@evil.com", ""); got != "" {
		t.Fatalf("SECURITY: credential offered to evil.com via userinfo smuggling: got %q", got)
	}
	if got := HostTokenFor(
		envOf("MOLECULE_GIT_TOKEN__EVIL_COM=evil-token"),
		"https://git.moleculesai.app@evil.com", "",
	); got != "evil-token" {
		t.Errorf("the real host should be the lookup key: got %q", got)
	}

	// Pins LAST-'@' (RFC 3986) rather than first: with the first, the result
	// still contains an '@', matches no key, and the credential is dropped.
	if got := HostTokenFor(env, "https://user@sub@git.moleculesai.app", ""); got != "per-org-token" {
		t.Errorf("multi-'@' authority must resolve to the host after the LAST '@': got %q", got)
	}
}

func TestHostTokenFor_NaiveDashFoldingWouldMisroute(t *testing.T) {
	// The specific wrong implementation this guards: folding the host FORWARD
	// with BOTH '.' and '-' mapped to '_'. That makes git.my-corp.com and
	// git.my.corp.com collide on one key, so a credential for one forge is
	// silently offered to the other.
	if got := HostTokenFor(
		envOf("MOLECULE_GIT_TOKEN__GIT_MY_CORP_COM=corp-token"),
		"https://git.my-corp.com", "",
	); got != "" {
		t.Fatalf("dash-folding collision: token for git.my.corp.com offered to git.my-corp.com (got %q)", got)
	}
}

// roundTripFunc lets a test serve a portless https host without a network.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestGiteaResolver_UsesPerHostCredential is the end-to-end wiring proof: the
// resolver must actually consult the per-host layer, on the production-shaped
// portless host, with NO template token present.
func TestGiteaResolver_UsesPerHostCredential(t *testing.T) {
	t.Setenv("MOLECULE_TEMPLATE_REPO_TOKEN", "")
	t.Setenv("MOLECULE_GIT_TOKEN__GIT_MOLECULESAI_APP", "per-org-token")

	archive := makePluginTarball(t, "repo", map[string]string{
		"plugin.yaml": "name: repo\nversion: 1.0.0\n",
	})

	var downloaderToken, commitsAuth string
	r := &GiteaResolver{
		BaseURL:  "https://git.moleculesai.app",
		TokenEnv: "MOLECULE_TEMPLATE_REPO_TOKEN",
		ResolveRefClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			commitsAuth = req.Header.Get("Authorization")
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`[{"sha":"c162f12b0000000000000000000000000000abcd"}]`)),
				Request:    req,
			}, nil
		})},
		ArchiveDownloader: func(ctx context.Context, archiveURL, token, dstDir string) error {
			downloaderToken = token
			writeTarball(t, dstDir, archive)
			return nil
		},
	}

	if _, err := r.Fetch(context.Background(), "owner/repo#main", t.TempDir()); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if downloaderToken != "per-org-token" {
		t.Errorf("archive download used token %q, want the per-org token", downloaderToken)
	}
	if commitsAuth != "token per-org-token" {
		t.Errorf("commits request auth = %q, want the per-org token", commitsAuth)
	}
}

// TestGiteaResolver_ResolveRefUsesPerHostCredential covers the drift sweeper,
// which resolves refs through the same resolver — a private source would
// otherwise be permanently undriftable even once it installs.
func TestGiteaResolver_ResolveRefUsesPerHostCredential(t *testing.T) {
	t.Setenv("MOLECULE_TEMPLATE_REPO_TOKEN", "")
	t.Setenv("MOLECULE_GIT_TOKEN__GIT_MOLECULESAI_APP", "per-org-token")

	var auth string
	r := &GiteaResolver{
		BaseURL:  "https://git.moleculesai.app",
		TokenEnv: "MOLECULE_TEMPLATE_REPO_TOKEN",
		ResolveRefClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			auth = req.Header.Get("Authorization")
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`[{"sha":"c162f12b0000000000000000000000000000abcd"}]`)),
				Request:    req,
			}, nil
		})},
	}

	if _, err := r.ResolveRef(context.Background(), "owner/repo#main"); err != nil {
		t.Fatalf("ResolveRef: %v", err)
	}
	if auth != "token per-org-token" {
		t.Errorf("ResolveRef auth = %q, want the per-org token", auth)
	}
}
