package handlers

// plugins_uninstall_pull_test.go — coverage for the POST-BOOT plugin delivery
// being PULL-primary on the molecules-server / local-docker backend (#206/#230):
//
//   - delta A: the docker-less UNINSTALL delivers by PULL (drop BOTH DB rows +
//     restart) instead of the old 503 dead-end.
//   - delta B: pull is the PRIMARY model via deliveryModeForInstance +
//     PLUGIN_DELIVERY_PULL_ONLY (default on), not just an h.docker==nil fallback.
//   - delta C: deleteDeclaredPluginRow removes the plugin from the DESIRED set so
//     it can't re-materialize on the next boot.
//
// The #173 regression guard (TestPluginPullPath_NeverHitsDockerOrEIC_Guard173)
// locks out re-introduction of the AWS-EIC 90s hang: a docker-less
// molecules-server tenant must NEVER invoke installPluginViaEIC /
// uninstallPluginViaEIC on EITHER install or uninstall.
//
// All deterministic; no live Docker / AWS / staging.

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// TestDeliveryModeForInstance_PolicyMatrix pins delta B's explicit selector:
// EC2 always PUSHES (EIC, shape-gated); a molecules-server container-name id
// PULLS by default and only PUSHES when PLUGIN_DELIVERY_PULL_ONLY=false. This is
// the mechanism that makes pull the primary model rather than a mere fallback.
func TestDeliveryModeForInstance_PolicyMatrix(t *testing.T) {
	cases := []struct {
		name       string
		instanceID string
		pullOnly   string // "" leaves the env unset (default = pull-only ON)
		want       pluginDeliveryMode
	}{
		{"ec2 default", "i-0e0951a3cfd9bbf75", "", deliverByPush},
		{"ec2 pull-only-off", "i-0e0951a3cfd9bbf75", "false", deliverByPush},
		{"molecules-server default (pull primary)", "mol-ws-test5-a9f3044fa3f2", "", deliverByPull},
		{"molecules-server pull-only-off (push rollback)", "mol-ws-test5-a9f3044fa3f2", "false", deliverByPush},
		{"molecules-server pull-only-on explicit", "mol-ws-test5-a9f3044fa3f2", "true", deliverByPull},
		{"empty id default", "", "", deliverByPull},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.pullOnly != "" {
				t.Setenv("PLUGIN_DELIVERY_PULL_ONLY", tc.pullOnly)
			} else {
				// Ensure a clean default even if the process env carries a value.
				t.Setenv("PLUGIN_DELIVERY_PULL_ONLY", "")
			}
			if got := deliveryModeForInstance(tc.instanceID); got != tc.want {
				t.Errorf("deliveryModeForInstance(%q) pullOnly=%q = %v, want %v",
					tc.instanceID, tc.pullOnly, got, tc.want)
			}
		})
	}
}

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
