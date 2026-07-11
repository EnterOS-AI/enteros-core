package handlers

// plugins_reconcile_stale_test.go — content-aware reconcile (fix (b)).
//
// The online-transition reconcile must re-deliver a branch-pinned (track=none)
// plugin whose upstream tip has moved past the installed SHA — the case the
// drift sweeper (tag:/sha: only) never covers — while NEVER churning on an
// immutable pin, a missing baseline, or a transient resolve failure.

import (
	"context"
	"errors"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/plugins"
	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

type installedRow struct {
	name   string
	source string
	sha    string // "" → NULL installed_sha (no content baseline)
}

// expectDeclaredWithRecords programs the two reconcile read queries, letting the
// installed rows carry source_raw + installed_sha so the content-staleness path
// can be exercised (the name-only expectDeclared helper always emits NULL SHAs).
func expectDeclaredWithRecords(mock sqlmock.Sqlmock, declared []DeclaredPlugin, installed []installedRow) {
	dRows := sqlmock.NewRows([]string{"plugin_name", "source_raw"})
	for _, d := range declared {
		dRows.AddRow(d.PluginName, d.SourceRaw)
	}
	mock.ExpectQuery(`SELECT plugin_name, source_raw\s+FROM workspace_declared_plugins`).
		WillReturnRows(dRows)

	iRows := sqlmock.NewRows([]string{"plugin_name", "source_raw", "installed_sha"})
	for _, r := range installed {
		if r.sha == "" {
			iRows.AddRow(r.name, r.source, nil)
		} else {
			iRows.AddRow(r.name, r.source, r.sha)
		}
	}
	mock.ExpectQuery(`SELECT plugin_name, source_raw, installed_sha FROM workspace_plugins WHERE workspace_id`).
		WillReturnRows(iRows)
}

// stubPresentOnBox makes pluginPresentOnBox return true via the SaaS EIC path so
// the reconcile reaches the content-staleness check.
func stubPresentOnBox(t *testing.T, h *PluginsHandler) {
	t.Helper()
	h.instanceIDLookup = func(string) (string, error) { return "i-present", nil }
	orig := readPluginManifestViaEIC
	readPluginManifestViaEIC = func(ctx context.Context, instanceID, runtime, pluginName string) ([]byte, error) {
		return []byte("name: seo-all\n"), nil
	}
	t.Cleanup(func() { readPluginManifestViaEIC = orig })
}

// stubResolveSourceSHA overrides the branch-tip resolver seam.
func stubResolveSourceSHA(t *testing.T, fn func(context.Context, plugins.PluginResolver, string) (string, error)) {
	t.Helper()
	orig := resolveSourceSHA
	resolveSourceSHA = fn
	t.Cleanup(func() { resolveSourceSHA = orig })
}

// TestPluginFragmentStale_Guards exercises the fail-closed guards directly.
func TestPluginFragmentStale_Guards(t *testing.T) {
	h := NewPluginsHandler(t.TempDir(), nil, nil)

	cases := []struct {
		name              string
		source            string
		installed         string
		resolveSHA        string
		resolveErr        error
		wantStale         bool
		wantResolveCalled bool
	}{
		{name: "empty baseline never stale", source: "gitea://o/r#main", installed: "", resolveSHA: "x", wantStale: false, wantResolveCalled: false},
		{name: "immutable tag pin owned by sweeper", source: "github://o/r#tag:v1.0.0", installed: "abc", resolveSHA: "def", wantStale: false, wantResolveCalled: false},
		{name: "immutable sha pin owned by sweeper", source: "github://o/r#sha:abcdef", installed: "abc", resolveSHA: "def", wantStale: false, wantResolveCalled: false},
		{name: "resolve error never churns", source: "gitea://o/r#main", installed: "abc", resolveErr: errors.New("boom"), wantStale: false, wantResolveCalled: true},
		{name: "resolve empty never churns", source: "gitea://o/r#main", installed: "abc", resolveSHA: "", wantStale: false, wantResolveCalled: true},
		{name: "unchanged tip not stale", source: "gitea://o/r#main", installed: "abc", resolveSHA: "abc", wantStale: false, wantResolveCalled: true},
		{name: "moved tip is stale", source: "gitea://o/r#main", installed: "abc", resolveSHA: "def", wantStale: true, wantResolveCalled: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var called bool
			stubResolveSourceSHA(t, func(_ context.Context, _ plugins.PluginResolver, _ string) (string, error) {
				called = true
				return tc.resolveSHA, tc.resolveErr
			})
			got := h.pluginFragmentStale(context.Background(), tc.source, tc.installed)
			if got != tc.wantStale {
				t.Errorf("pluginFragmentStale = %v, want %v", got, tc.wantStale)
			}
			if called != tc.wantResolveCalled {
				t.Errorf("resolveSourceSHA called = %v, want %v (guards must short-circuit before a network fetch)", called, tc.wantResolveCalled)
			}
		})
	}
}

// TestReconcile_BranchPinStale_ReDelivers: a present branch-pinned plugin whose
// upstream tip moved must be re-delivered and its SHA re-recorded.
func TestReconcile_BranchPinStale_ReDelivers(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()
	h, delivered := newReconcileHandler(t)
	stubPresentOnBox(t, h)

	var called bool
	stubResolveSourceSHA(t, func(_ context.Context, _ plugins.PluginResolver, _ string) (string, error) {
		called = true
		return "newsha1111111111", nil // upstream tip moved past installed
	})

	expectDeclaredWithRecords(mock,
		[]DeclaredPlugin{{PluginName: "seo-all", SourceRaw: "local://seo-all"}},
		[]installedRow{{name: "seo-all", source: "local://seo-all", sha: "oldsha0000000000"}},
	)
	// Stale → re-deliver → re-record the freshly resolved SHA (loop guard).
	mock.ExpectExec(`INSERT INTO workspace_plugins`).WillReturnResult(sqlmock.NewResult(1, 1))

	h.ReconcileWorkspacePlugins(context.Background(), "ws-1")

	if !called {
		t.Fatal("expected resolveSourceSHA to be consulted for a track=none pin")
	}
	if len(*delivered) != 1 || (*delivered)[0] != "seo-all" {
		t.Fatalf("stale branch-pin must re-deliver, got %v", *delivered)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v", err)
	}
}

// TestReconcile_BranchPinUpToDate_NoOp: installed SHA == upstream tip → no-op.
func TestReconcile_BranchPinUpToDate_NoOp(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()
	h, delivered := newReconcileHandler(t)
	stubPresentOnBox(t, h)

	stubResolveSourceSHA(t, func(_ context.Context, _ plugins.PluginResolver, _ string) (string, error) {
		return "samesha000000000", nil // unchanged
	})

	expectDeclaredWithRecords(mock,
		[]DeclaredPlugin{{PluginName: "seo-all", SourceRaw: "local://seo-all"}},
		[]installedRow{{name: "seo-all", source: "local://seo-all", sha: "samesha000000000"}},
	)
	// No INSERT — up-to-date branch pin is a pure no-op.

	h.ReconcileWorkspacePlugins(context.Background(), "ws-1")

	if len(*delivered) != 0 {
		t.Fatalf("up-to-date branch-pin must be a no-op, delivered %v", *delivered)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v", err)
	}
}

// TestReconcile_ImmutableTagPin_NeverResolves: a tag: pin is owned by the drift
// sweeper — the reconcile must not even resolve it, and must not re-deliver.
func TestReconcile_ImmutableTagPin_NeverResolves(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()
	h, delivered := newReconcileHandler(t)
	stubPresentOnBox(t, h)

	var called bool
	stubResolveSourceSHA(t, func(_ context.Context, _ plugins.PluginResolver, _ string) (string, error) {
		called = true
		return "whatever00000000", nil
	})

	expectDeclaredWithRecords(mock,
		[]DeclaredPlugin{{PluginName: "seo-all", SourceRaw: "github://o/r#tag:v1.0.0"}},
		[]installedRow{{name: "seo-all", source: "github://o/r#tag:v1.0.0", sha: "installedsha0000"}},
	)
	// No INSERT — immutable pin present on box is a no-op.

	h.ReconcileWorkspacePlugins(context.Background(), "ws-1")

	if called {
		t.Error("resolveSourceSHA must NOT be called for a tag:/sha: pin (drift sweeper owns those)")
	}
	if len(*delivered) != 0 {
		t.Fatalf("immutable pin must be a no-op, delivered %v", *delivered)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v", err)
	}
}

// TestReconcile_ResolveError_NoChurn: a transient resolve failure must NOT
// re-deliver — it self-heals on a later beat.
func TestReconcile_ResolveError_NoChurn(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()
	h, delivered := newReconcileHandler(t)
	stubPresentOnBox(t, h)

	stubResolveSourceSHA(t, func(_ context.Context, _ plugins.PluginResolver, _ string) (string, error) {
		return "", errors.New("gitea unreachable")
	})

	expectDeclaredWithRecords(mock,
		[]DeclaredPlugin{{PluginName: "seo-all", SourceRaw: "local://seo-all"}},
		[]installedRow{{name: "seo-all", source: "local://seo-all", sha: "oldsha0000000000"}},
	)
	// No INSERT — resolve failure must not trigger a churny re-deliver.

	h.ReconcileWorkspacePlugins(context.Background(), "ws-1")

	if len(*delivered) != 0 {
		t.Fatalf("resolve error must not re-deliver, delivered %v", *delivered)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v", err)
	}
}
