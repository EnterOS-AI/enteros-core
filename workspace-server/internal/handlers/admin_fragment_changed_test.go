package handlers

// admin_fragment_changed_test.go — fix (c): the fragment-merge trigger endpoint
// and its helpers. The async reconcile+restart orchestration is composed of
// independently-tested units (ReconcileWorkspacePlugins, pluginFragmentStale,
// platformConciergeReconcileShouldSkipRestart); these tests cover the endpoint's
// synchronous surface (validation, workspace resolution, response) + the two
// new query/staleness helpers.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/plugins"
	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

func postFragmentChanged(t *testing.T, h *AdminPluginDriftHandler, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/admin/plugin-fragment-changed", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h.FragmentChanged(c)
	return w
}

func TestListWorkspacesDeclaringPlugin(t *testing.T) {
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT wdp.workspace_id\s+FROM workspace_declared_plugins wdp\s+JOIN workspaces w ON w.id = wdp.workspace_id\s+WHERE wdp.plugin_name = \$1\s+AND w.status IN \('online', 'provisioning'\)`).
		WithArgs("mcp").
		WillReturnRows(sqlmock.NewRows([]string{"workspace_id"}).AddRow("ws-1").AddRow("ws-2"))

	got, err := listWorkspacesDeclaringPlugin(context.Background(), "mcp")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "ws-1" || got[1] != "ws-2" {
		t.Fatalf("got %v, want [ws-1 ws-2]", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestPluginFragmentStaleForWorkspace(t *testing.T) {
	h := NewPluginsHandler(t.TempDir(), nil, nil)

	// moved branch tip → stale
	mock := setupTestDB(t)
	stubResolveSourceSHA(t, func(_ context.Context, _ plugins.PluginResolver, _ string) (string, error) {
		return "newsha1111", nil
	})
	mock.ExpectQuery(`SELECT plugin_name, source_raw, installed_sha FROM workspace_plugins WHERE workspace_id`).
		WithArgs("ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"plugin_name", "source_raw", "installed_sha"}).
			AddRow("mcp", "gitea://o/r#main", "oldsha0000"))
	if !h.PluginFragmentStaleForWorkspace(context.Background(), "ws-1", "mcp") {
		t.Fatal("expected stale=true for a moved branch pin")
	}

	// plugin not recorded on the workspace → not stale
	mock.ExpectQuery(`SELECT plugin_name, source_raw, installed_sha FROM workspace_plugins WHERE workspace_id`).
		WithArgs("ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"plugin_name", "source_raw", "installed_sha"}))
	if h.PluginFragmentStaleForWorkspace(context.Background(), "ws-1", "mcp") {
		t.Fatal("expected stale=false when the plugin is not recorded")
	}
}

func TestFragmentChanged_MissingPluginName_400(t *testing.T) {
	h := NewAdminPluginDriftHandler(NewPluginsHandler(t.TempDir(), nil, nil))
	w := postFragmentChanged(t, h, `{"plugin_name":"  "}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestFragmentChanged_NilPluginsHandler_503(t *testing.T) {
	h := NewAdminPluginDriftHandler(nil)
	w := postFragmentChanged(t, h, `{"plugin_name":"mcp"}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
}

func TestFragmentChanged_NoAffectedWorkspaces_202(t *testing.T) {
	mock := setupTestDB(t)
	h := NewAdminPluginDriftHandler(NewPluginsHandler(t.TempDir(), nil, nil))
	// No online/provisioning workspace declares the plugin → 202, reconciling=0,
	// no async goroutines spawned (empty loop).
	mock.ExpectQuery(`SELECT wdp.workspace_id\s+FROM workspace_declared_plugins`).
		WithArgs("mcp").
		WillReturnRows(sqlmock.NewRows([]string{"workspace_id"}))

	w := postFragmentChanged(t, h, `{"plugin_name":"mcp"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		PluginName  string `json:"plugin_name"`
		Reconciling int    `json:"reconciling"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body parse: %v", err)
	}
	if resp.Reconciling != 0 || resp.PluginName != "mcp" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}
