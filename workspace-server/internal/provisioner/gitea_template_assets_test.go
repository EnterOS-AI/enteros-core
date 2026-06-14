package provisioner

// gitea_template_assets_test.go — tests for the real Gitea
// TemplateAssetFetcher (RFC #2843 #24, PR-B). The previous
// #2855 SCAFFOLD tests covered the interface contract; this
// file covers the PRODUCTION impl.
//
// Tests use httptest.NewServer to serve a real .tar.gz
// generated in-memory (no real Gitea instance needed). The
// dispatch's required test surface:
//   - happy path: assert ALL asset paths incl agent-skills are
//     returned (must FAIL if skills dropped)
//   - allowlist filter: non-allowlisted paths are excluded
//   - fail-closed: transport / extract errors surface as errors
//   - identity parsing: malformed identities return errors

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestGiteaTemplateAssetFetcher_HappyPath pins the production
// contract: a real .tar.gz archive containing config.yaml +
// prompts/ + agent-skills/ is fetched, parsed, and returned
// as a map with all three namespaces populated. The dispatch
// explicitly calls out: "must FAIL if skills dropped" — this
// test is the load-bearing check for that.
func TestGiteaTemplateAssetFetcher_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify the request shape.
		if r.URL.Path != "/api/v1/repos/molecule-ai/workspace-template-seo/archive/main.tar.gz" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "token the-token" {
			t.Errorf("unexpected Authorization header: %q", got)
		}
		// Serve a real .tar.gz with config.yaml + prompts/system.md +
		// agent-skills/seo-audit/SKILL.md wrapped in the
		// "<repo>-<sha>" top-level dir Gitea uses.
		w.Header().Set("Content-Type", "application/gzip")
		w.WriteHeader(http.StatusOK)
		gz := gzip.NewWriter(w)
		tw := tar.NewWriter(gz)
		mustWriteTar(t, tw, "workspace-template-seo-abcd1234/config.yaml", []byte("# SEO config\n"))
		mustWriteTar(t, tw, "workspace-template-seo-abcd1234/prompts/system.md", []byte("# System prompt\n"))
		mustWriteTar(t, tw, "workspace-template-seo-abcd1234/agent-skills/seo-audit/SKILL.md", []byte("# SEO skill\n"))
		mustWriteTar(t, tw, "workspace-template-seo-abcd1234/agent-skills/seo-audit/manifest.yaml", []byte("name: seo-audit\n"))
		_ = tw.Close()
		_ = gz.Close()
	}))
	defer srv.Close()

	f := NewGiteaTemplateAssetFetcher(srv.URL, "the-token", srv.Client())
	assets, err := f.Load(context.Background(), "molecule-ai/workspace-template-seo@main")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// All 3 allowlisted namespaces must be present. Per the
	// dispatch: "must FAIL if skills dropped" — assert skill
	// files explicitly.
	mustHaveKey(t, assets, "config.yaml")
	mustHaveKey(t, assets, "prompts/system.md")
	mustHaveKey(t, assets, "agent-skills/seo-audit/SKILL.md")
	mustHaveKey(t, assets, "agent-skills/seo-audit/manifest.yaml")
	if len(assets) != 4 {
		t.Errorf("expected 4 assets, got %d: %v", len(assets), keysOf(assets))
	}
}

// TestGiteaTemplateAssetFetcher_AllowsOnlyAllowlistedPaths pins
// the blast-radius guard. A .tar.gz that contains both
// allowlisted AND non-allowlisted paths (e.g. CLAUDE.md,
// MEMORY.md, .claude/sessions/foo) must have the non-allowlisted
// entries EXCLUDED from the returned map (the consumer's
// IsCPTemplateAssetPath check enforces the same invariant —
// the fetcher pre-filters as a perf + audit-log win).
func TestGiteaTemplateAssetFetcher_AllowsOnlyAllowlistedPaths(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		gz := gzip.NewWriter(w)
		tw := tar.NewWriter(gz)
		// Allowlisted.
		mustWriteTar(t, tw, "repo-sha/config.yaml", []byte("ok"))
		mustWriteTar(t, tw, "repo-sha/prompts/x.md", []byte("ok"))
		mustWriteTar(t, tw, "repo-sha/agent-skills/skill-x/SKILL.md", []byte("ok"))
		// NOT allowlisted — must be excluded.
		mustWriteTar(t, tw, "repo-sha/CLAUDE.md", []byte("agent-owned"))
		mustWriteTar(t, tw, "repo-sha/MEMORY.md", []byte("agent-owned"))
		mustWriteTar(t, tw, "repo-sha/USER.md", []byte("agent-owned"))
		mustWriteTar(t, tw, "repo-sha/.claude/sessions/foo.json", []byte("agent-owned"))
		mustWriteTar(t, tw, "repo-sha/adapter.py", []byte("not-template-asset"))
		_ = tw.Close()
		_ = gz.Close()
	}))
	defer srv.Close()

	f := NewGiteaTemplateAssetFetcher(srv.URL, "the-token", srv.Client())
	assets, err := f.Load(context.Background(), "owner/repo@main")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Allowlisted keys present.
	mustHaveKey(t, assets, "config.yaml")
	mustHaveKey(t, assets, "prompts/x.md")
	mustHaveKey(t, assets, "agent-skills/skill-x/SKILL.md")
	// Non-allowlisted keys EXCLUDED.
	mustNotHaveKey(t, assets, "CLAUDE.md")
	mustNotHaveKey(t, assets, "MEMORY.md")
	mustNotHaveKey(t, assets, "USER.md")
	mustNotHaveKey(t, assets, ".claude/sessions/foo.json")
	mustNotHaveKey(t, assets, "adapter.py")
	if len(assets) != 3 {
		t.Errorf("expected 3 allowlisted assets, got %d: %v", len(assets), keysOf(assets))
	}
}

// TestGiteaTemplateAssetFetcher_FailsClosedOnHTTPError pins
// the fail-closed contract. A non-200 response (401, 404, 500)
// returns an error, NOT a silently-empty result.
func TestGiteaTemplateAssetFetcher_FailsClosedOnHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"internal server error"}`))
	}))
	defer srv.Close()

	f := NewGiteaTemplateAssetFetcher(srv.URL, "the-token", srv.Client())
	_, err := f.Load(context.Background(), "owner/repo@main")
	if err == nil {
		t.Fatal("expected error on 500 response, got nil (fail-closed violated)")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention the HTTP status, got: %v", err)
	}
}

// TestGiteaTemplateAssetFetcher_FailsClosedOnTransportError pins
// the fail-closed contract for transport-layer failures (DNS
// failure, connection refused, etc.). The fetcher must NOT
// silently return an empty map.
func TestGiteaTemplateAssetFetcher_FailsClosedOnTransportError(t *testing.T) {
	// Use a port that nothing is listening on (reserved by IANA
	// for "tcpmux"; 1 is "tcpmux" too). The dial will fail.
	f := NewGiteaTemplateAssetFetcher("http://127.0.0.1:1", "the-token", &http.Client{Timeout: 100 * time.Millisecond})
	_, err := f.Load(context.Background(), "owner/repo@main")
	if err == nil {
		t.Fatal("expected error on transport failure, got nil (fail-closed violated)")
	}
}

// TestGiteaTemplateAssetFetcher_RejectsEmptyToken pins the
// security guard: an empty token (which would otherwise be
// sent as "Authorization: token " with no credential) is
// rejected at construction time. A forgotten token init would
// otherwise silently fail-against-anonymous-requests, which
// Gitea would 401 on — better to fail loud at Load time.
func TestGiteaTemplateAssetFetcher_RejectsEmptyToken(t *testing.T) {
	f := NewGiteaTemplateAssetFetcher("http://example.com", "", nil)
	_, err := f.Load(context.Background(), "owner/repo@main")
	if err == nil {
		t.Fatal("expected error on empty token, got nil (security guard violated)")
	}
	if !strings.Contains(err.Error(), "token") {
		t.Errorf("error should mention token, got: %v", err)
	}
}

// TestParseTemplateIdentity pins the identity parser. Format:
// "<owner>/<repo>@<ref>". Malformed identities return errors.
func TestParseTemplateIdentity(t *testing.T) {
	cases := []struct {
		name      string
		identity  string
		wantOwner string
		wantRepo  string
		wantRef   string
		wantErr   bool
	}{
		{"simple", "owner/repo@main", "owner", "repo", "main", false},
		{"with-sha-ref", "owner/repo@abcd1234", "owner", "repo", "abcd1234", false},
		{"with-tag-ref", "owner/repo@v1.2.3", "owner", "repo", "v1.2.3", false},
		{"nested-owner", "molecule-ai/workspace-template-seo@main", "molecule-ai", "workspace-template-seo", "main", false},
		{"empty", "", "", "", "", true},
		{"no-at", "owner/repo", "", "", "", true},
		{"empty-ref", "owner/repo@", "", "", "", true},
		{"no-slash", "owner@main", "", "", "", true},
		{"empty-owner", "/repo@main", "", "", "", true},
		{"empty-repo", "owner/@main", "", "", "", true},
		{"extra-at", "owner/repo@main@extra", "", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			owner, repo, ref, err := parseTemplateIdentity(c.identity)
			if c.wantErr {
				if err == nil {
					t.Errorf("expected error for %q, got nil", c.identity)
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error for %q: %v", c.identity, err)
				return
			}
			if owner != c.wantOwner {
				t.Errorf("owner = %q, want %q", owner, c.wantOwner)
			}
			if repo != c.wantRepo {
				t.Errorf("repo = %q, want %q", repo, c.wantRepo)
			}
			if ref != c.wantRef {
				t.Errorf("ref = %q, want %q", ref, c.wantRef)
			}
		})
	}
}

// TestStripArchiveTopDir pins the top-level dir stripper. Gitea
// wraps entries in "<repo>-<sha>/<relpath>"; we want just the
// relpath. Top-level entries (no slash) and traversal attempts
// (../) are rejected.
func TestStripArchiveTopDir(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		wantOk bool
		want   string
	}{
		{"normal", "repo-sha/config.yaml", true, "config.yaml"},
		{"nested", "repo-sha/prompts/system.md", true, "prompts/system.md"},
		{"deep", "repo-sha/agent-skills/seo/SKILL.md", true, "agent-skills/seo/SKILL.md"},
		{"top-level-no-slash", "config.yaml", false, ""},
		{"empty", "", false, ""},
		{"slash-only", "/", false, ""},
		{"dot-only", "repo-sha/.", false, ""},
		{"traversal", "repo-sha/../etc/passwd", false, ""},
		{"deep-traversal", "repo-sha/prompts/../../etc", false, ""},
		{"dotdot-in-name", "repo-sha/foo..bar", true, "foo..bar"}, // not a traversal, just an unusual filename
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := stripArchiveTopDir(c.in)
			if ok != c.wantOk {
				t.Errorf("ok = %v, want %v (input %q)", ok, c.wantOk, c.in)
				return
			}
			if ok && got != c.want {
				t.Errorf("got = %q, want %q (input %q)", got, c.want, c.in)
			}
		})
	}
}

// ---- Test helpers ----

// mustWriteTar writes a single tar entry with the given name +
// data. Errors are reported via t (so the test fails cleanly).
// Wrapped in a helper to keep the test bodies focused on the
// content (not the tar boilerplate).
func mustWriteTar(t *testing.T, tw *tar.Writer, name string, data []byte) {
	t.Helper()
	hdr := &tar.Header{
		Name:     name,
		Mode:     0o644,
		Size:     int64(len(data)),
		Typeflag: tar.TypeReg,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("tar WriteHeader %s: %v", name, err)
	}
	if _, err := tw.Write(data); err != nil {
		t.Fatalf("tar Write %s: %v", name, err)
	}
}

// mustHaveKey asserts assets contains key. Failure reports the
// key + the full set so the test message is informative.
func mustHaveKey(t *testing.T, assets map[string][]byte, key string) {
	t.Helper()
	if _, ok := assets[key]; !ok {
		t.Errorf("expected key %q in assets, got keys: %v", key, keysOf(assets))
	}
}

// mustNotHaveKey asserts assets does NOT contain key. The
// blast-radius guard's load-bearing assertion.
func mustNotHaveKey(t *testing.T, assets map[string][]byte, key string) {
	t.Helper()
	if _, ok := assets[key]; ok {
		t.Errorf("did NOT expect key %q in assets (not in allowlist), but it's there: %v", key, keysOf(assets))
	}
}

// keysOf returns a stable-ish view of the map's keys for
// assertion error messages. Local helper to avoid coupling to
// test-fixture conventions elsewhere.
func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Compile-time check: archive/tar is imported for the test
// server's .tar.gz generation. (Avoids a "imported and not
// used" lint in case future refactors move the helper.)
var _ = tar.TypeReg

// Compile-time check: bytes is imported for the test sentinel.
// Avoids unused-import in future refactors.
var _ = bytes.NewReader

// Compile-time check: io is imported for io.LimitReader.
// Avoids unused-import in future refactors.
var _ = io.LimitReader
