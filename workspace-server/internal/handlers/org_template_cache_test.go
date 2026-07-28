package handlers

// M6 / FIX 0: an org node's `template:` must resolve against the template CACHE
// as well as the configs dir.
//
// Org import checked configsDir only. On SaaS the real template arrives via the
// Gitea asset channel and lands in the CACHE — which is exactly the deployment
// where a node's `template:` matters most. The failure is silent: the node
// falls back to `<runtime>-default` and provisions from the wrong template.

import (
	"os"
	"path/filepath"
	"testing"
)

func mkTemplate(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("name: "+name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// THE FIX: a cache-only template resolves. Before this it returned "".
func TestOrgNodeTemplate_ResolvesFromTheCache(t *testing.T) {
	configs, cache := t.TempDir(), t.TempDir()
	want := mkTemplate(t, cache, "seo-agent")
	if got := resolveOrgNodeTemplateDir(configs, cache, "seo-agent"); got != want {
		t.Errorf("cache-only template did not resolve: got %q want %q", got, want)
	}
}

func TestOrgNodeTemplate_ResolvesFromConfigsWhenNotCached(t *testing.T) {
	configs, cache := t.TempDir(), t.TempDir()
	want := mkTemplate(t, configs, "seo-agent")
	if got := resolveOrgNodeTemplateDir(configs, cache, "seo-agent"); got != want {
		t.Errorf("configs template did not resolve: got %q want %q", got, want)
	}
}

// Cache wins — a fetched template is fresher than a baked one, and on a tenant
// whose image predates a template change the baked copy is the stale one.
func TestOrgNodeTemplate_CacheTakesPrecedenceOverConfigs(t *testing.T) {
	configs, cache := t.TempDir(), t.TempDir()
	mkTemplate(t, configs, "seo-agent")
	want := mkTemplate(t, cache, "seo-agent")
	if got := resolveOrgNodeTemplateDir(configs, cache, "seo-agent"); got != want {
		t.Errorf("cache must win: got %q want %q", got, want)
	}
}

// Containment applies to BOTH roots — adding a second root must not open a
// traversal the single-root version refused.
func TestOrgNodeTemplate_RefusesEscapesInEitherRoot(t *testing.T) {
	configs, cache := t.TempDir(), t.TempDir()
	outside := filepath.Join(filepath.Dir(cache), "outside-template")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		"../outside-template", "../../etc", "/etc", "a/../../outside-template",
	} {
		if got := resolveOrgNodeTemplateDir(configs, cache, bad); got != "" {
			t.Errorf("template %q escaped containment -> %q", bad, got)
		}
	}
}

func TestOrgNodeTemplate_EmptyAndMissingAreEmptyString(t *testing.T) {
	configs, cache := t.TempDir(), t.TempDir()
	for _, in := range []string{"", "does-not-exist"} {
		if got := resolveOrgNodeTemplateDir(configs, cache, in); got != "" {
			t.Errorf("%q should not resolve, got %q", in, got)
		}
	}
}

// A FILE named like a template is not a template dir — resolving to it would
// hand a file path to code that expects a directory.
func TestOrgNodeTemplate_AFileIsNotATemplateDir(t *testing.T) {
	configs, cache := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(cache, "seo-agent"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := resolveOrgNodeTemplateDir(configs, cache, "seo-agent"); got != "" {
		t.Errorf("a plain file must not resolve as a template dir: %q", got)
	}
}

// An empty cacheDir (self-host, nothing fetched) must not break the configs path.
func TestOrgNodeTemplate_EmptyCacheDirFallsBackCleanly(t *testing.T) {
	configs := t.TempDir()
	want := mkTemplate(t, configs, "seo-agent")
	if got := resolveOrgNodeTemplateDir(configs, "", "seo-agent"); got != want {
		t.Errorf("empty cacheDir broke the configs path: got %q want %q", got, want)
	}
}
