package handlers

// M6 wiring proof. FIX 0-3 landed as pure functions with ZERO callers, so
// every one of them was dead code that tested green — the milestone's own doc
// recorded them as "written and negative-controlled; all four await wiring".
//
// These tests assert the WIRING, not the functions: that the org-import path
// actually consults the template cache, actually merges the template config as
// a base, and actually lets a template contribute plugins. A unit test of
// mergeTemplateConfigBase cannot tell you whether anything calls it — that gap
// is exactly why a live workspace was running `name: Hermes Agent` with no
// plugins and no schedules while all four fixes sat merged in main.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeM6Template(t *testing.T, root, name, configYAML string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(configYAML), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return dir
}

// --- FIX 0: cache-first resolution -----------------------------------------

func TestWiring_FIX0_OrgHandlerCarriesTemplateCacheDir(t *testing.T) {
	h := (&OrgHandler{}).WithTemplateCacheDir("/tmp/cache")
	if h.templateCacheDir != "/tmp/cache" {
		t.Fatalf("WithTemplateCacheDir did not set the field: %q", h.templateCacheDir)
	}
}

func TestWiring_FIX0_CacheWinsOverConfigs(t *testing.T) {
	configs := t.TempDir()
	cache := t.TempDir()
	writeM6Template(t, configs, "seo-agent", "name: from-configs\n")
	writeM6Template(t, cache, "seo-agent", "name: from-cache\n")

	got := resolveOrgNodeTemplateDir(configs, cache, "seo-agent")
	if !strings.HasPrefix(got, cache) {
		t.Fatalf("expected the CACHE copy to win (fetched is fresher than baked), got %q", got)
	}
}

func TestWiring_FIX0_FallsBackToConfigsWhenCacheEmpty(t *testing.T) {
	configs := t.TempDir()
	writeM6Template(t, configs, "seo-agent", "name: from-configs\n")
	if got := resolveOrgNodeTemplateDir(configs, "", "seo-agent"); !strings.HasPrefix(got, configs) {
		t.Fatalf("empty cache must fall back to configs, got %q", got)
	}
}

func TestWiring_FIX0_EscapeRefusedOnBothRoots(t *testing.T) {
	configs := t.TempDir()
	cache := t.TempDir()
	for _, bad := range []string{"../etc", "/etc", "a/../../etc"} {
		if got := resolveOrgNodeTemplateDir(configs, cache, bad); got != "" {
			t.Fatalf("containment breach for %q: %q", bad, got)
		}
	}
}

// --- FIX 1+2: template config as the base ----------------------------------

func TestWiring_FIX1_TemplateOnlyKeysSurvive(t *testing.T) {
	tmpl := []byte("providers:\n  anthropic: {}\nmodels:\n  - claude\nenv:\n  FOO: bar\n")
	gen := []byte("name: SEO\nruntime: claude-code\ntier: 2\n")
	out, err := mergeTemplateConfigBase(tmpl, gen)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	for _, key := range []string{"providers", "models", "env"} {
		if !strings.Contains(string(out), key+":") {
			t.Fatalf("template-only key %q was dropped — that is the whole point of FIX 1:\n%s", key, out)
		}
	}
	if !strings.Contains(string(out), "runtime: claude-code") {
		t.Fatalf("node-owned key lost:\n%s", out)
	}
}

func TestWiring_FIX2_TemplateMayNotRepinModelOrProvider(t *testing.T) {
	// SSOT no-re-pin rule: an empty model is a VISIBLE not_configured, whereas
	// a template-supplied one silently substitutes someone else's choice.
	tmpl := []byte("model: sneaky-model\nprovider: sneaky\nname: catalog-name\nproviders:\n  x: {}\n")
	gen := []byte("name: SEO\nruntime: claude-code\n")
	out, err := mergeTemplateConfigBase(tmpl, gen)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	s := string(out)
	if strings.Contains(s, "sneaky") {
		t.Fatalf("template re-pinned model/provider:\n%s", s)
	}
	if strings.Contains(s, "catalog-name") {
		t.Fatalf("template renamed the node:\n%s", s)
	}
	if !strings.Contains(s, "name: SEO") {
		t.Fatalf("node name lost:\n%s", s)
	}
}

// --- FIX 3: template-declared plugins --------------------------------------

func TestWiring_FIX3_TemplatePluginsContributeAndNodeCanDecline(t *testing.T) {
	tmplCfg := []byte("plugins:\n  - seo-all\n  - source: gitea://o/r#v1\n")
	got := templateDeclaredPlugins(tmplCfg)
	if len(got) != 2 || got[0] != "seo-all" || got[1] != "gitea://o/r#v1" {
		t.Fatalf("both the bare-string and {source,config} forms must be read, got %v", got)
	}

	merged := mergePluginsWithTemplate(got, []string{"ecc"}, nil)
	if !m6Contains(merged, "seo-all") || !m6Contains(merged, "ecc") {
		t.Fatalf("template + defaults should both appear: %v", merged)
	}

	// A node must be able to DECLINE an inherited plugin, or inheritance is a trap.
	declined := mergePluginsWithTemplate(got, []string{"ecc"}, []string{"!seo-all"})
	if m6Contains(declined, "seo-all") {
		t.Fatalf("node opt-out ignored: %v", declined)
	}
}

func m6Contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
