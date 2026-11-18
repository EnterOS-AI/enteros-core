//go:build integration

package handlers

// plugins_uninstall_pull_integration_test.go — the docker-less PULL uninstall
// against a REAL Postgres.
//
// What a pull uninstall promises is a statement about desiredPluginSources: the
// set workspace_provision stamps into MOLECULE_DECLARED_PLUGINS and the
// runtime's boot materializer rebuilds /configs/plugins from. sqlmock can only
// show which DELETEs were issued, not what the next provision would declare, so
// this drives the real handler against real rows and reads the real set back.
//
// The NEGATIVE CONTROL runs in the same test: the push paths' shape (delete only
// the workspace_plugins row) leaves a plugin in the desired set, because its
// declared row survives. That is why the pull path deletes both rows, and it
// shows the final assertion is able to fail.
//
// Runs in CI under .gitea/workflows/handlers-postgres-integration.yml
// (`-tags=integration -run '^TestIntegration_'`); skips without
// INTEGRATION_DB_URL.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	mdb "git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"github.com/gin-gonic/gin"
)

// pullUninstallTestSource is the source declarePlugin/installPlugin record for
// a plugin name.
func pullUninstallTestSource(name string) string { return "gitea://molecule-ai/" + name + "#main" }

func containsSource(srcs []string, src string) bool {
	for _, s := range srcs {
		if s == src {
			return true
		}
	}
	return false
}

// THE PROPERTY. After a pull Uninstall of a user plugin, the next provision's
// desired set no longer names it, and still names the scheduler that every
// provision declares (which is what keeps MOLECULE_DECLARED_PLUGINS non-empty,
// so the runtime's full-replace swap actually runs and drops the directory).
func TestIntegration_PluginUninstallPull_DropsPluginKeepsScheduler(t *testing.T) {
	conn := settingsTestDB(t)
	ctx := context.Background()
	ws := seedSettingsWorkspace(t, conn)

	// The handler and desiredPluginSources go through the package-level db.DB.
	// Not parallel-safe (global swap); restored on cleanup.
	prev := mdb.DB
	mdb.DB = conn
	t.Cleanup(func() { mdb.DB = prev })

	// The scheduler, exactly as every provision declares it.
	if err := ensureSchedulerPluginDeclared(ctx, ws); err != nil {
		t.Fatalf("declare scheduler: %v", err)
	}
	// Two user plugins with the rows the pull Install path writes: declared
	// (reMaterialize) + installed (recordWorkspacePluginInstall).
	const target, control = "browser-automation", "lark-channel"
	for _, name := range []string{target, control} {
		declarePlugin(t, conn, ws, name)
		installPlugin(t, conn, ws, name)
	}

	desired := func() []string {
		t.Helper()
		srcs, err := desiredPluginSources(ctx, ws)
		if err != nil {
			t.Fatalf("desiredPluginSources: %v", err)
		}
		return srcs
	}

	// Every fixture row must have taken, or the assertions below prove nothing.
	before := desired()
	for _, src := range []string{SchedulerPluginSource, pullUninstallTestSource(target), pullUninstallTestSource(control)} {
		if !containsSource(before, src) {
			t.Fatalf("fixture did not take: %q missing from the desired set %v", src, before)
		}
	}

	// NEGATIVE CONTROL: the push-path shape, installed row only.
	uninstallPlugin(t, conn, ws, control)
	if !containsSource(desired(), pullUninstallTestSource(control)) {
		t.Fatal("control is not exercising the hazard: an installed-row-only delete removed the plugin from the desired set")
	}
	t.Logf("negative control reproduced: after an installed-row-only delete the desired set still names %s (its declared row survives)", control)

	// THE PATH UNDER TEST: a local-docker container name with no docker client
	// wired, where Uninstall used to answer 503.
	restarted := make(chan string, 1)
	h := NewPluginsHandler(t.TempDir(), nil, func(id string) { restarted <- id }).
		WithRuntimeLookup(func(string) (string, error) { return "claude-code", nil }).
		WithInstanceIDLookup(func(string) (string, error) { return localDockerInstanceID, nil })

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: ws}, {Key: "name", Value: target}}
	c.Request = httptest.NewRequest(http.MethodDelete, "/workspaces/"+ws+"/plugins/"+target, nil)
	h.Uninstall(c)
	drainTestAsync()

	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"delivery":"pull"`) {
		t.Fatalf("pull uninstall: want 200 with delivery=pull, got %d: %s", w.Code, w.Body.String())
	}
	select {
	case <-restarted:
	default:
		t.Error("pull uninstall did not schedule the restart that applies it")
	}

	after := desired()
	if containsSource(after, pullUninstallTestSource(target)) {
		t.Fatalf("%s is still in the desired set after a pull uninstall: %v", target, after)
	}
	if !containsSource(after, SchedulerPluginSource) {
		t.Fatalf("the scheduler must stay in the desired set: %v", after)
	}
	for _, table := range []string{"workspace_plugins", "workspace_declared_plugins"} {
		var n int
		if err := conn.QueryRowContext(ctx,
			`SELECT count(*) FROM `+table+` WHERE workspace_id = $1 AND plugin_name = $2`,
			ws, target).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Fatalf("%s still holds %d row(s) for %s after a pull uninstall", table, n, target)
		}
	}
}

// The settings decision across a PULL uninstall (plugin_settings_identity.go):
// the override row is RETAINED; the name stops resolving while neither the
// declared nor the installed row exists; a reinstall makes it resolve again
// with the override still winning.
func TestIntegration_PluginUninstallPull_SettingsRetainedAndResolveAgainOnReinstall(t *testing.T) {
	conn := settingsTestDB(t)
	ctx := context.Background()
	ws := seedSettingsWorkspace(t, conn)

	prev := mdb.DB
	mdb.DB = conn
	t.Cleanup(func() { mdb.DB = prev })

	const name = "browser-automation"
	declarePlugin(t, conn, ws, name)
	installPlugin(t, conn, ws, name)
	if err := writeTemplateConfig(ctx, conn, ws, name, map[string]any{"poll_seconds": 30}); err != nil {
		t.Fatal(err)
	}
	if _, err := patchOverrides(ctx, conn, ws, name,
		map[string]any{"poll_seconds": 5}, nil, "operator@molecule", -1); err != nil {
		t.Fatal(err)
	}

	h := NewPluginsHandler(t.TempDir(), nil, nil)
	if _, err := h.pullUninstall(ctx, ws, name); err != nil {
		t.Fatalf("pullUninstall: %v", err)
	}

	// Unlike the push paths, the declared row is gone too, so the name no
	// longer resolves (the settings endpoints answer 404) until a reinstall.
	if _, err := resolvePluginInstallName(ctx, conn, ws, name); !errors.Is(err, errPluginNotOnWorkspace) {
		t.Fatalf("after a pull uninstall the name must not resolve, got %v", err)
	}
	// The settings row itself is retained.
	if _, overrides, _, err := loadPluginSettings(ctx, conn, ws, name); err != nil {
		t.Fatal(err)
	} else if len(overrides) == 0 {
		t.Fatal("a pull uninstall destroyed the operator's override")
	}

	// REINSTALL (the pull Install path writes both rows again).
	declarePlugin(t, conn, ws, name)
	installPlugin(t, conn, ws, name)
	if _, err := resolvePluginInstallName(ctx, conn, ws, name); err != nil {
		t.Fatalf("a reinstalled plugin must resolve again: %v", err)
	}
	config, overrides, _, err := loadPluginSettings(ctx, conn, ws, name)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := effectiveSettings(config, overrides)["poll_seconds"].(float64); !ok || v != 5 {
		t.Fatalf("the override must still win after reinstall: poll_seconds=%v, want 5",
			effectiveSettings(config, overrides)["poll_seconds"])
	}
}
