package wiring

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestBuild_NilWhenURLUnset pins the operator-friendly default: no
// MEMORY_PLUGIN_URL → nil bundle → all callers fall through to legacy
// behavior with no surprises.
func TestBuild_NilWhenURLUnset(t *testing.T) {
	t.Setenv("MEMORY_PLUGIN_URL", "")
	if got := Build(nil); got != nil {
		t.Errorf("expected nil bundle when MEMORY_PLUGIN_URL unset, got %+v", got)
	}
}

// TestBuild_NonNilWhenURLSet pins that the bundle is constructed even
// when the plugin's /v1/health probe fails — we don't want workspace-
// server boot to depend on a transiently unavailable plugin.
func TestBuild_NonNilWhenURLSet(t *testing.T) {
	t.Setenv("MEMORY_PLUGIN_URL", "http://127.0.0.1:1") // bogus port = probe will fail
	db, _, _ := sqlmock.New()
	defer db.Close()
	bundle := Build(db)
	if bundle == nil {
		t.Fatal("expected non-nil bundle when MEMORY_PLUGIN_URL is set")
	}
	if bundle.Plugin == nil {
		t.Error("Plugin must be wired")
	}
	if bundle.Resolver == nil {
		t.Error("Resolver must be wired")
	}
}

// TestNamespaceCleanupFn_NilBundle pins the nil-safe path: callers
// that pass `bundle.NamespaceCleanupFn()` unconditionally don't need
// to nil-check the bundle separately.
func TestNamespaceCleanupFn_NilBundle(t *testing.T) {
	var b *Bundle // nil receiver
	if got := b.NamespaceCleanupFn(); got != nil {
		t.Errorf("nil bundle must return nil cleanup fn, got non-nil")
	}
}

// TestNamespaceCleanupFn_NilPlugin: bundle exists but plugin is nil —
// also returns nil cleanup fn (defensive in case of partial wiring).
func TestNamespaceCleanupFn_NilPlugin(t *testing.T) {
	b := &Bundle{} // both fields nil
	if got := b.NamespaceCleanupFn(); got != nil {
		t.Errorf("bundle with nil plugin must return nil cleanup fn")
	}
}

// TestNamespaceCleanupFn_HitsPluginAtCorrectNamespace is the real
// integration gate for the closure: it spins up an httptest.Server
// that records every DELETE request, points MEMORY_PLUGIN_URL at it,
// runs Build(), then invokes the returned closure and asserts the
// server saw `DELETE /v1/namespaces/workspace:<id>`.
//
// This replaces two earlier tests that exercised parallel
// implementations rather than the production closure (caught in
// self-review).
func TestNamespaceCleanupFn_HitsPluginAtCorrectNamespace(t *testing.T) {
	var (
		mu          sync.Mutex
		gotPaths    []string
		gotMethods  []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPaths = append(gotPaths, r.URL.Path)
		gotMethods = append(gotMethods, r.Method)
		mu.Unlock()
		switch r.URL.Path {
		case "/v1/health":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok","version":"1.0.0","capabilities":[]}`))
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(srv.Close)

	t.Setenv("MEMORY_PLUGIN_URL", srv.URL)
	db, _, _ := sqlmock.New()
	defer db.Close()

	bundle := Build(db)
	if bundle == nil {
		t.Fatal("Build returned nil with MEMORY_PLUGIN_URL set")
	}
	cleanup := bundle.NamespaceCleanupFn()
	if cleanup == nil {
		t.Fatal("NamespaceCleanupFn returned nil with non-nil Plugin")
	}

	cleanup(context.Background(), "abc-123")

	mu.Lock()
	defer mu.Unlock()
	// Two requests expected: /v1/health probe at Boot + DELETE for cleanup.
	foundDelete := false
	for i, p := range gotPaths {
		if gotMethods[i] == "DELETE" && p == "/v1/namespaces/workspace:abc-123" {
			foundDelete = true
		}
	}
	if !foundDelete {
		t.Errorf("expected DELETE /v1/namespaces/workspace:abc-123, got %v",
			pathsAndMethods(gotPaths, gotMethods))
	}
}

// TestNamespaceCleanupFn_PluginErrorDoesNotPanic exercises the failure
// path for real: server returns 500 on DELETE; the closure must log
// and return without propagating. Replaces the parallel-implementation
// version that didn't actually test the production code.
func TestNamespaceCleanupFn_PluginErrorDoesNotPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/health" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok","version":"1.0.0","capabilities":[]}`))
			return
		}
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	t.Setenv("MEMORY_PLUGIN_URL", srv.URL)
	db, _, _ := sqlmock.New()
	defer db.Close()

	bundle := Build(db)
	cleanup := bundle.NamespaceCleanupFn()

	// Must not panic, must not propagate the 500. Recovering with
	// defer is belt-and-suspenders — production calls this from a
	// for-loop in workspace_crud.go that has no recover.
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("cleanup panicked on plugin 500: %v", r)
		}
	}()
	cleanup(context.Background(), "ws-1")
}

// captureLogs runs fn with log output captured into a buffer, returns the
// captured text. Restores the prior log destination on exit.
func captureLogs(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	fn()
	return buf.String()
}

// TestBuild_WarnsWhenCutoverWithoutPluginURL pins the silent-misconfig
// guard: an operator who flips MEMORY_V2_CUTOVER=true without also
// pointing MEMORY_PLUGIN_URL at a plugin server has just disabled the
// cutover with no error visible. Without this WARN, the only signal
// is "the legacy table is still being written to" — invisible to
// every operator who doesn't explicitly check.
func TestBuild_WarnsWhenCutoverWithoutPluginURL(t *testing.T) {
	t.Setenv("MEMORY_V2_CUTOVER", "true")
	t.Setenv("MEMORY_PLUGIN_URL", "")
	out := captureLogs(t, func() {
		if got := Build(nil); got != nil {
			t.Errorf("expected nil bundle, got %+v", got)
		}
	})
	if !strings.Contains(out, "MEMORY_V2_CUTOVER=true") || !strings.Contains(out, "MEMORY_PLUGIN_URL is unset") {
		t.Errorf("expected loud WARN about half-configured cutover; got log:\n%s", out)
	}
}

// TestBuild_NoWarnWhenNeitherSet pins the happy default: an operator
// running without the v2 plugin should not see scary warnings.
func TestBuild_NoWarnWhenNeitherSet(t *testing.T) {
	t.Setenv("MEMORY_V2_CUTOVER", "")
	t.Setenv("MEMORY_PLUGIN_URL", "")
	out := captureLogs(t, func() { _ = Build(nil) })
	if strings.Contains(out, "MEMORY_V2_CUTOVER") {
		t.Errorf("expected no MEMORY_V2_CUTOVER warning when env is unset; got log:\n%s", out)
	}
}

// TestBuild_LoudWarnWhenCutoverAndProbeFails pins the second
// half-config case: cutover is on AND plugin URL is set, but the
// /v1/health probe fails (server down or wrong URL). Without this
// loud WARN, the operator sees only the generic "probe failed" line
// that gets emitted even when cutover is OFF — hiding the fact that
// real cutover writes will quietly fall back via circuit breaker.
func TestBuild_LoudWarnWhenCutoverAndProbeFails(t *testing.T) {
	t.Setenv("MEMORY_V2_CUTOVER", "true")
	t.Setenv("MEMORY_PLUGIN_URL", "http://127.0.0.1:1") // bogus port
	db, _, _ := sqlmock.New()
	defer db.Close()
	out := captureLogs(t, func() { _ = Build(db) })
	if !strings.Contains(out, "MEMORY_V2_CUTOVER=true") || !strings.Contains(out, "probe failed") {
		t.Errorf("expected loud WARN about cutover-with-failing-probe; got log:\n%s", out)
	}
}

// TestBuild_QuietProbeFailWhenCutoverOff: the operator is in PRE-cutover
// mode (plugin URL set, cutover off — they're warming up the plugin).
// A failing probe in this state is not a misconfig — it should log the
// generic message, NOT the loud cutover-specific one (so log noise
// doesn't drown out real cutover misconfigs in dashboards).
func TestBuild_QuietProbeFailWhenCutoverOff(t *testing.T) {
	t.Setenv("MEMORY_V2_CUTOVER", "")
	t.Setenv("MEMORY_PLUGIN_URL", "http://127.0.0.1:1")
	db, _, _ := sqlmock.New()
	defer db.Close()
	out := captureLogs(t, func() { _ = Build(db) })
	if strings.Contains(out, "MEMORY_V2_CUTOVER=true") {
		t.Errorf("expected no cutover-specific warning when cutover is off; got log:\n%s", out)
	}
	if !strings.Contains(out, "probe failed") {
		t.Errorf("expected generic probe-failed log; got log:\n%s", out)
	}
}

func pathsAndMethods(paths, methods []string) []string {
	out := make([]string, len(paths))
	for i := range paths {
		out[i] = methods[i] + " " + paths[i]
	}
	return out
}
