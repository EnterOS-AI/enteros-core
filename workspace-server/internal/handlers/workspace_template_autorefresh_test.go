package handlers

// internal#3211: a create/provision for a KNOWN non-claude-code runtime
// (e.g. hermes) whose workspace template is a cache MISS at provision
// time must AUTO-REFRESH that template into the cache and re-resolve BEFORE
// seeding — and, on a persistent miss, FAIL LOUD naming the runtime's own
// template. It must NEVER silently substitute a claude-code default for a
// non-claude-code runtime (the prior behavior, which the on-disk
// runtimeSeedMismatchAbort guard only caught in Docker mode, not SaaS/CP).

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTemplateDir creates <root>/<name>/config.yaml with the given runtime
// so resolveWorkspaceTemplatePath sees a real template directory.
func writeTemplateDir(t *testing.T, root, name, runtime string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	cfg := "name: " + name + "\nruntime: " + runtime + "\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}
}

func TestResolveTemplateWithRefreshOnMiss(t *testing.T) {
	t.Run("hit: resolves without invoking refresh", func(t *testing.T) {
		configsDir := t.TempDir()
		cacheDir := t.TempDir()
		writeTemplateDir(t, cacheDir, "hermes", "hermes")

		refreshed := 0
		h := &WorkspaceHandler{
			configsDir:           configsDir,
			cacheDir:             cacheDir,
			refreshTemplateCache: func(ctx context.Context) error { refreshed++; return nil },
		}

		got, err := h.resolveTemplateWithRefreshOnMiss(context.Background(), "hermes", "hermes")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := filepath.Join(cacheDir, "hermes")
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
		if refreshed != 0 {
			t.Fatalf("refresh invoked %d times on a cache HIT, want 0", refreshed)
		}
	})

	t.Run("miss for known non-claude runtime: refresh is invoked and the template resolves", func(t *testing.T) {
		configsDir := t.TempDir()
		cacheDir := t.TempDir()

		// hermes is initially absent from BOTH roots (cache miss). The
		// stubbed refresh "fetches" it into the cache the way
		// templatecache.RefreshWorkspaceTemplates would.
		refreshed := 0
		h := &WorkspaceHandler{
			configsDir: configsDir,
			cacheDir:   cacheDir,
			refreshTemplateCache: func(ctx context.Context) error {
				refreshed++
				writeTemplateDir(t, cacheDir, "hermes", "hermes")
				return nil
			},
		}

		got, err := h.resolveTemplateWithRefreshOnMiss(context.Background(), "hermes", "hermes")
		if err != nil {
			t.Fatalf("unexpected error after refresh: %v", err)
		}
		if refreshed != 1 {
			t.Fatalf("refresh invoked %d times, want exactly 1", refreshed)
		}
		want := filepath.Join(cacheDir, "hermes")
		if got != want {
			t.Fatalf("got %q, want %q (the runtime's OWN template, never claude-code)", got, want)
		}
		// The seeded config must declare the requested runtime, never claude-code.
		seeded := seededConfigRuntime(got, nil)
		if seeded != "hermes" {
			t.Fatalf("seeded runtime = %q, want hermes (no claude-code substitution)", seeded)
		}
	})

	t.Run("persistent miss for known non-claude runtime: FAIL LOUD, never claude-code", func(t *testing.T) {
		configsDir := t.TempDir()
		cacheDir := t.TempDir()
		// A claude-code-default IS available — the prior code would have fallen
		// back to it. The fix must refuse instead.
		writeTemplateDir(t, configsDir, "claude-code-default", "claude-code")

		refreshed := 0
		h := &WorkspaceHandler{
			configsDir: configsDir,
			cacheDir:   cacheDir,
			// Refresh runs but the template is still not fetched (e.g. private
			// repo the refresh couldn't reach) — the persistent-miss case.
			refreshTemplateCache: func(ctx context.Context) error { refreshed++; return nil },
		}

		got, err := h.resolveTemplateWithRefreshOnMiss(context.Background(), "hermes", "hermes")
		if err == nil {
			t.Fatalf("expected a loud error on persistent miss, got path %q (this is the #3211 silent-fallback bug)", got)
		}
		if got != "" {
			t.Fatalf("expected empty path on failure, got %q", got)
		}
		if refreshed != 1 {
			t.Fatalf("refresh invoked %d times, want exactly 1", refreshed)
		}
		// The error must name the runtime's own template and the runtime, and
		// must NOT have substituted the claude-code default that was on disk.
		if !strings.Contains(err.Error(), "hermes") {
			t.Fatalf("error %q does not name the runtime's own template", err.Error())
		}
		if strings.Contains(got, "claude-code") {
			t.Fatalf("resolved a claude-code path %q for a hermes runtime — the exact #3211 regression", got)
		}
	})

	t.Run("refresh failure for known non-claude runtime: FAIL LOUD", func(t *testing.T) {
		h := &WorkspaceHandler{
			configsDir:           t.TempDir(),
			cacheDir:             t.TempDir(),
			refreshTemplateCache: func(ctx context.Context) error { return context.DeadlineExceeded },
		}
		got, err := h.resolveTemplateWithRefreshOnMiss(context.Background(), "hermes", "hermes")
		if err == nil {
			t.Fatalf("expected a loud error when refresh fails, got path %q", got)
		}
		if !strings.Contains(err.Error(), "hermes") {
			t.Fatalf("error %q does not name the runtime", err.Error())
		}
	})

	t.Run("no refresh wired for known non-claude runtime: FAIL LOUD (unit-test/self-host degrade)", func(t *testing.T) {
		configsDir := t.TempDir()
		// claude-code-default present on disk: the fix must still refuse to
		// substitute it for hermes even with no refresh mechanism.
		writeTemplateDir(t, configsDir, "claude-code-default", "claude-code")
		h := &WorkspaceHandler{
			configsDir: configsDir,
			cacheDir:   t.TempDir(),
			// refreshTemplateCache intentionally nil.
		}
		got, err := h.resolveTemplateWithRefreshOnMiss(context.Background(), "hermes", "hermes")
		if err == nil {
			t.Fatalf("expected a loud error when no refresh is wired, got path %q", got)
		}
		if !strings.Contains(err.Error(), "hermes") {
			t.Fatalf("error %q does not name the runtime", err.Error())
		}
	})

	t.Run("miss for claude-code: NO refresh, caller falls back (empty path, no error)", func(t *testing.T) {
		refreshed := 0
		h := &WorkspaceHandler{
			configsDir:           t.TempDir(),
			cacheDir:             t.TempDir(),
			refreshTemplateCache: func(ctx context.Context) error { refreshed++; return nil },
		}
		got, err := h.resolveTemplateWithRefreshOnMiss(context.Background(), "some-cc-variant", "claude-code")
		if err != nil {
			t.Fatalf("claude-code miss must not error (caller handles fallback): %v", err)
		}
		if got != "" {
			t.Fatalf("claude-code miss must return empty path, got %q", got)
		}
		if refreshed != 0 {
			t.Fatalf("refresh invoked %d times for claude-code, want 0 (no behavior change for claude-code)", refreshed)
		}
	})

	t.Run("miss for external-like runtime: NO refresh, caller falls back", func(t *testing.T) {
		refreshed := 0
		h := &WorkspaceHandler{
			configsDir:           t.TempDir(),
			cacheDir:             t.TempDir(),
			refreshTemplateCache: func(ctx context.Context) error { refreshed++; return nil },
		}
		got, err := h.resolveTemplateWithRefreshOnMiss(context.Background(), "whatever", "external")
		if err != nil {
			t.Fatalf("external runtime miss must not error: %v", err)
		}
		if got != "" || refreshed != 0 {
			t.Fatalf("external runtime: got=%q refreshed=%d, want empty/0", got, refreshed)
		}
	})

	t.Run("path traversal is rejected before any refresh", func(t *testing.T) {
		refreshed := 0
		h := &WorkspaceHandler{
			configsDir:           t.TempDir(),
			cacheDir:             t.TempDir(),
			refreshTemplateCache: func(ctx context.Context) error { refreshed++; return nil },
		}
		if _, err := h.resolveTemplateWithRefreshOnMiss(context.Background(), "../escape", "hermes"); err == nil {
			t.Fatal("expected traversal to be rejected")
		}
		if refreshed != 0 {
			t.Fatalf("refresh invoked %d times on a traversal-rejected path, want 0", refreshed)
		}
	})
}

func TestRuntimeRequiresOwnTemplate(t *testing.T) {
	cases := map[string]bool{
		"hermes":                  true,
		"codex":                   true,
		"openclaw":                true,
		"claude-code":             false,
		"external":                false,
		"mock":                    false,
		"":                        false,
		"totally-unknown-runtime": false, // unknown → coerced to claude-code elsewhere; not a NAMED non-claude runtime
	}
	for runtime, want := range cases {
		if got := runtimeRequiresOwnTemplate(runtime); got != want {
			t.Errorf("runtimeRequiresOwnTemplate(%q) = %v, want %v", runtime, got, want)
		}
	}
}
