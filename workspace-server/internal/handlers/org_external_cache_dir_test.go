package handlers

// The `!external` cache must be relocatable off the baked image tree.
//
// By default it lands under <rootDir>/.external-cache, and rootDir is
// /org-templates — which is COPIED INTO THE IMAGE. Writing platform-mutated
// state into a read-only tree is why core#4889 had to chown /org-templates
// just to make an org template importable. Chowning an immutable tree is the
// symptom; relocating the writes is the cure.

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestExternalCacheBase_DefaultsToTheTemplateTree(t *testing.T) {
	t.Setenv(externalCacheDirEnv, "")
	root := "/org-templates"
	want := filepath.Join(root, externalCacheDirName)
	if got := externalCacheBase(root); got != want {
		t.Errorf("default must be unchanged for anyone not setting the env: got %q want %q", got, want)
	}
}

func TestExternalCacheBase_EnvRelocatesItOffTheImageTree(t *testing.T) {
	t.Setenv(externalCacheDirEnv, "/var/lib/molecule/external-cache")
	got := externalCacheBase("/org-templates")
	if got != "/var/lib/molecule/external-cache" {
		t.Errorf("env override ignored: %q", got)
	}
	// The whole point: nothing lands under the baked tree.
	if strings.HasPrefix(got, "/org-templates") {
		t.Errorf("cache still inside the image tree: %q", got)
	}
}

func TestExternalCacheBase_BlankAndWhitespaceFallBack(t *testing.T) {
	root := "/org-templates"
	want := filepath.Join(root, externalCacheDirName)
	for _, v := range []string{"", "   ", "\t"} {
		t.Setenv(externalCacheDirEnv, v)
		if got := externalCacheBase(root); got != want {
			t.Errorf("value %q should fall back to the default, got %q", v, got)
		}
	}
}

// A relocated cache must still separate repos — the per-repo subdir is what
// keeps two templates' fetches from colliding.
func TestExternalCacheBase_StillNamespacesPerRepo(t *testing.T) {
	t.Setenv(externalCacheDirEnv, t.TempDir())
	base := externalCacheBase("/org-templates")
	a := filepath.Join(base, safeRepoCacheDir("git.moleculesai.app", "molecule-ai/one"))
	b := filepath.Join(base, safeRepoCacheDir("git.moleculesai.app", "molecule-ai/two"))
	if a == b {
		t.Fatalf("two repos collapsed onto one cache dir: %q", a)
	}
}
