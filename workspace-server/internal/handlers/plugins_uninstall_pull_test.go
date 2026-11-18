package handlers

// plugins_uninstall_pull_test.go — the docker-less plugin UNINSTALL (#206/#230).
//
// Install already delivers by PULL when a workspace has no push target — a
// local-docker container-name instance_id with no docker client wired, or no
// running container and no instance_id: it declares the plugin and restarts,
// and the runtime's boot materializer fetches it. Uninstall answered 503 in
// exactly those two states, so such a workspace could gain a plugin and never
// lose it. These tests pin the pull uninstall that replaces the 503:
//
//   - it deletes BOTH rows (workspace_plugins + workspace_declared_plugins), so
//     desiredPluginSources stops emitting the plugin;
//   - a failed delete is a 500 and schedules no restart;
//   - a docker-capable workspace still installs AND uninstalls through docker:
//     pull is the no-push-target fallback, not a policy that overrides docker;
//   - the #173 guard: a docker-less tenant never reaches the AWS EIC transport.
//
// All deterministic; no live Docker / AWS / staging. The real-Postgres check of
// the desired set is plugins_uninstall_pull_integration_test.go.

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/docker/docker/client"
	"github.com/gin-gonic/gin"
)

// TestPullUninstall_NoRestartFunc_DeletesRowsReportsNotRestarting pins the
// pullUninstall primitive when no restart hook is wired: it still drops BOTH DB
// rows (the deletion IS the uninstall) and reports restarting=false.
func TestPullUninstall_NoRestartFunc_DeletesRowsReportsNotRestarting(t *testing.T) {
	mock := setupTestDB(t)
	mock.ExpectExec(`DELETE FROM workspace_plugins WHERE workspace_id`).
		WithArgs("ws-x", "demo").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`DELETE FROM workspace_declared_plugins WHERE workspace_id`).
		WithArgs("ws-x", "demo").
		WillReturnResult(sqlmock.NewResult(0, 1))

	h := NewPluginsHandler(t.TempDir(), nil, nil) // restartFunc nil

	scheduled, err := h.pullUninstall(context.Background(), "ws-x", "demo")
	if err != nil {
		t.Fatalf("pullUninstall err: %v", err)
	}
	if scheduled {
		t.Errorf("restarting must be false with no restartFunc wired")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("both DB rows must be deleted: %v", err)
	}
}

// TestPluginUninstall_Pull_RowDeleteFailureIs500: on the pull path the row
// deletes ARE the uninstall, so a failed delete must surface as a 500 and must
// not schedule the restart — answering 200 would promise a removal that the
// surviving row undoes on the next boot. Covers both entry points (container
// name with no docker client; no container and no instance id) and both
// deletes.
func TestPluginUninstall_Pull_RowDeleteFailureIs500(t *testing.T) {
	const wsID = "c7244ed9-f623-4cba-8873-020e5c9fe104"
	cases := []struct {
		name          string
		instanceID    string
		failInstalled bool // false: the installed delete succeeds, the declared delete fails
	}{
		{"container name, installed-row delete fails", localDockerInstanceID, true},
		{"no instance id, declared-row delete fails", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubUninstallPluginViaEIC(t, func(_ context.Context, instanceID, _, _ string) error {
				t.Errorf("uninstallPluginViaEIC called (instanceID=%q) on the pull path", instanceID)
				return nil
			})

			mock := setupTestDB(t)
			if tc.failInstalled {
				mock.ExpectExec(`DELETE FROM workspace_plugins WHERE workspace_id`).
					WithArgs(wsID, "browser-automation").
					WillReturnError(errors.New("db unavailable"))
			} else {
				mock.ExpectExec(`DELETE FROM workspace_plugins WHERE workspace_id`).
					WithArgs(wsID, "browser-automation").
					WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectExec(`DELETE FROM workspace_declared_plugins WHERE workspace_id`).
					WithArgs(wsID, "browser-automation").
					WillReturnError(errors.New("db unavailable"))
			}

			restarted := make(chan string, 1)
			instanceID := tc.instanceID
			h := NewPluginsHandler(t.TempDir(), nil, func(id string) { restarted <- id }).
				WithRuntimeLookup(func(string) (string, error) { return "claude-code", nil }).
				WithInstanceIDLookup(func(string) (string, error) { return instanceID, nil })

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Params = gin.Params{
				{Key: "id", Value: wsID},
				{Key: "name", Value: "browser-automation"},
			}
			c.Request = httptest.NewRequest("DELETE", "/workspaces/"+wsID+"/plugins/browser-automation", nil)

			h.Uninstall(c)
			drainTestAsync()

			if w.Code != http.StatusInternalServerError {
				t.Fatalf("a failed row delete must be a 500, got %d: %s", w.Code, w.Body.String())
			}
			select {
			case id := <-restarted:
				t.Errorf("restart scheduled for %s after a failed row delete", id)
			default:
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unexpected DB call sequence: %v", err)
			}
		})
	}
}

// fakeDockerDaemon stands in for the Docker Engine API on a loopback listener.
// It answers the self-host container lookups (GET /containers/ws-<id>/json,
// made by provisioner.RunningContainerName) with 404, so dispatch falls through
// to the instance_id branch, and every other call with 500, recording each
// request as "METHOD /path". A request addressed to the instance container is
// the proof that the handler routed to docker; the 500 then ends the flow before
// anything needs a real daemon (exec attach, archive upload).
type fakeDockerDaemon struct {
	mu   sync.Mutex
	seen []string
}

func newFakeDockerDaemon(t *testing.T) (*client.Client, *fakeDockerDaemon) {
	t.Helper()
	d := &fakeDockerDaemon{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		d.seen = append(d.seen, r.Method+" "+r.URL.Path)
		d.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/containers/ws-") {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"No such container"}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"fake docker daemon: not implemented"}`))
	}))
	t.Cleanup(srv.Close)
	cli, err := client.NewClientWithOpts(client.WithHost("tcp://" + srv.Listener.Addr().String()))
	if err != nil {
		t.Fatalf("docker client for the fake daemon: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli, d
}

// requests returns a copy of every request recorded so far.
func (d *fakeDockerDaemon) requests() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.seen...)
}

// sawContainer reports whether any recorded request addressed containerName.
func (d *fakeDockerDaemon) sawContainer(containerName string) bool {
	for _, r := range d.requests() {
		if strings.Contains(r, "/containers/"+containerName+"/") {
			return true
		}
	}
	return false
}

func (d *fakeDockerDaemon) reset() {
	d.mu.Lock()
	d.seen = nil
	d.mu.Unlock()
}

// TestPlugin_DockerCapableContainerName_InstallAndUninstallUseDocker pins that
// pull stays a fallback. When the workspace-server HAS a docker client, a
// local-docker workspace (instance_id is a "mol-ws-*" container name) installs
// AND uninstalls through docker, as on main. Routing it to pull instead would
// stop host-side local:// plugins landing, because the box cannot re-pull a
// host-side source (plugins_reconcile.go). An earlier revision of this PR made
// pull the default here (PLUGIN_DELIVERY_PULL_ONLY); this test fails against it.
func TestPlugin_DockerCapableContainerName_InstallAndUninstallUseDocker(t *testing.T) {
	stubInstallPluginViaEIC(t, func(_ context.Context, instanceID, _, _, _ string) error {
		t.Errorf("installPluginViaEIC called (instanceID=%q) for a local-docker container name", instanceID)
		return nil
	})
	stubUninstallPluginViaEIC(t, func(_ context.Context, instanceID, _, _ string) error {
		t.Errorf("uninstallPluginViaEIC called (instanceID=%q) for a local-docker container name", instanceID)
		return nil
	})

	cli, daemon := newFakeDockerDaemon(t)
	registry := t.TempDir()
	stagePluginRegistry(t, registry, "browser-automation")

	const wsID = "c7244ed9-f623-4cba-8873-020e5c9fe104"

	// The allowlist gate runs before delivery. No other DB expectation is set:
	// on the docker path the fake daemon fails the copy before anything is
	// recorded.
	mock := setupTestDB(t)
	expectAllowlistAllowAll(mock)

	h := NewPluginsHandler(registry, cli, func(string) {}).
		WithRuntimeLookup(func(string) (string, error) { return "claude-code", nil }).
		WithInstanceIDLookup(func(string) (string, error) { return localDockerInstanceID, nil })

	// ---- INSTALL leg.
	wIn := httptest.NewRecorder()
	ci, _ := gin.CreateTestContext(wIn)
	ci.Params = gin.Params{{Key: "id", Value: wsID}}
	ci.Request = httptest.NewRequest("POST", "/workspaces/"+wsID+"/plugins",
		bytes.NewBufferString(`{"source":"local://browser-automation"}`))
	ci.Request.Header.Set("Content-Type", "application/json")
	h.Install(ci)
	drainTestAsync()

	if !daemon.sawContainer(localDockerInstanceID) {
		t.Errorf("install never reached docker for container %s; daemon saw %v; response %d: %s",
			localDockerInstanceID, daemon.requests(), wIn.Code, wIn.Body.String())
	}
	if strings.Contains(wIn.Body.String(), `"delivery":"pull"`) {
		t.Errorf("a docker-capable workspace must not install by pull; got %d: %s", wIn.Code, wIn.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("install leg must consult the allowlist and record nothing else: %v", err)
	}

	// ---- UNINSTALL leg.
	daemon.reset()
	wUn := httptest.NewRecorder()
	cu, _ := gin.CreateTestContext(wUn)
	cu.Params = gin.Params{
		{Key: "id", Value: wsID},
		{Key: "name", Value: "browser-automation"},
	}
	cu.Request = httptest.NewRequest("DELETE", "/workspaces/"+wsID+"/plugins/browser-automation", nil)
	h.Uninstall(cu)
	drainTestAsync()

	if !daemon.sawContainer(localDockerInstanceID) {
		t.Errorf("uninstall never reached docker for container %s; daemon saw %v; response %d: %s",
			localDockerInstanceID, daemon.requests(), wUn.Code, wUn.Body.String())
	}
	if strings.Contains(wUn.Body.String(), `"delivery":"pull"`) {
		t.Errorf("a docker-capable workspace must not uninstall by pull; got %d: %s", wUn.Code, wUn.Body.String())
	}
}

// TestPluginPullPath_NeverHitsDockerOrEIC_Guard173 is the load-bearing #173
// regression guard. On a docker-less molecules-server tenant (h.docker == nil,
// instance_id is a "mol-ws-*" container name), NEITHER install NOR uninstall may
// reach the AWS EIC transport — that is the 90s-hang→502 we retired. Both EIC
// stubs fail the test loudly if ever called; both operations must return 200 +
// delivery:pull.
func TestPluginPullPath_NeverHitsDockerOrEIC_Guard173(t *testing.T) {
	stubInstallPluginViaEIC(t, func(_ context.Context, instanceID, _, _, _ string) error {
		t.Errorf("installPluginViaEIC called (instanceID=%q) — docker-less tenant must PULL, never EIC-push", instanceID)
		return nil
	})
	stubUninstallPluginViaEIC(t, func(_ context.Context, instanceID, _, _ string) error {
		t.Errorf("uninstallPluginViaEIC called (instanceID=%q) — docker-less tenant must PULL, never EIC-uninstall", instanceID)
		return nil
	})

	registry := t.TempDir()
	stagePluginRegistry(t, registry, "browser-automation")

	const wsID = "c7244ed9-f623-4cba-8873-020e5c9fe104"

	// ---- INSTALL leg: the pull path records into BOTH the DECLARED set
	// (re-materialize) and the INSTALLED set (best-effort tracking row), returns
	// pull. MatchExpectationsInOrder(false) is set by expectAllowlistAllowAll.
	mock := setupTestDB(t)
	expectAllowlistAllowAll(mock)
	mock.ExpectExec(`INSERT INTO workspace_declared_plugins`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO workspace_plugins`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	h := NewPluginsHandler(registry, nil, func(string) {}). // docker == nil
								WithRuntimeLookup(func(string) (string, error) { return "claude-code", nil }).
								WithInstanceIDLookup(func(string) (string, error) { return localDockerInstanceID, nil })

	wInstall := httptest.NewRecorder()
	ci, _ := gin.CreateTestContext(wInstall)
	ci.Params = gin.Params{{Key: "id", Value: wsID}}
	ci.Request = httptest.NewRequest("POST", "/workspaces/"+wsID+"/plugins",
		bytes.NewBufferString(`{"source":"local://browser-automation"}`))
	ci.Request.Header.Set("Content-Type", "application/json")
	h.Install(ci)
	drainTestAsync()

	if wInstall.Code != http.StatusOK || !strings.Contains(wInstall.Body.String(), `"delivery":"pull"`) {
		t.Fatalf("install must deliver by pull (200), got %d: %s", wInstall.Code, wInstall.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("install must record the declared row: %v", err)
	}

	// ---- UNINSTALL leg: drops BOTH rows, returns pull.
	mock.ExpectExec(`DELETE FROM workspace_plugins WHERE workspace_id`).
		WithArgs(wsID, "browser-automation").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`DELETE FROM workspace_declared_plugins WHERE workspace_id`).
		WithArgs(wsID, "browser-automation").
		WillReturnResult(sqlmock.NewResult(0, 1))

	wUn := httptest.NewRecorder()
	cu, _ := gin.CreateTestContext(wUn)
	cu.Params = gin.Params{
		{Key: "id", Value: wsID},
		{Key: "name", Value: "browser-automation"},
	}
	cu.Request = httptest.NewRequest("DELETE", "/workspaces/"+wsID+"/plugins/browser-automation", nil)
	h.Uninstall(cu)
	drainTestAsync()

	if wUn.Code != http.StatusOK || !strings.Contains(wUn.Body.String(), `"delivery":"pull"`) {
		t.Fatalf("uninstall must deliver by pull (200), got %d: %s", wUn.Code, wUn.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("uninstall must delete both DB rows: %v", err)
	}
}
