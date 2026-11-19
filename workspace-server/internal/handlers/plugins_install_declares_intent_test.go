package handlers

// plugins_install_declares_intent_test.go — an install MUST record the
// workspace's DECLARED intent on EVERY delivery path (molecule-core#5071).
//
// THE BUG THIS GUARDS
//
// POST /workspaces/:id/plugins has two delivery legs:
//
//	push — a running container / EC2 instance: deliverToContainer succeeds.
//	pull — no push target: reMaterialize, the box fetches on its next boot.
//
// Only the PULL leg wrote workspace_declared_plugins. The PUSH leg wrote
// workspace_plugins (what IS installed) and nothing else. Because
// desiredPluginSources unions declared+installed and lets INSTALLED win on a
// destination collision, MOLECULE_DECLARED_PLUGINS still came out right — so
// the drift was INVISIBLE for as long as the installed row survived.
//
// It is not harmless. Every consumer that reads the DECLARATION ALONE reverts
// to the stale pin:
//
//   - ReconcileWorkspacePlugins iterates declared and delivers d.SourceRaw
//     whenever the plugin is absent from the box (rebuilt config volume,
//     de-baked image, a boot fetch that failed for that one plugin) or has no
//     installed row.
//   - desiredPluginSources falls back to the declaration the moment the
//     installed row goes away (uninstall, restore, re-create).
//
// Live instance: workspace c4ebf3dc had gmail-channel-molecule declared at
// #v0.1.4, and no config the client could write ever moved it.
//
// FAIL-BEFORE / PASS-AFTER: on the pre-change handler the push leg fires no
// workspace_declared_plugins statement at all, so ExpectationsWereMet reports
// the unfulfilled expectation and TestInstallPushLeg_DeclaresIntent fails.

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

const (
	repinWorkspace = "ws-repin-5071"
	repinSource    = "local://browser-automation"
	repinPlugin    = "browser-automation"
)

var errDeclareBoom = errors.New("declare failed (simulated)")

// driveInstallPush runs one Install through the PUSH leg (EC2 instance id →
// EIC stub, so no docker daemon is needed). The caller programs the sqlmock
// expectations first; this only drives the handler.
func driveInstallPush(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	registry := t.TempDir()
	stagePluginRegistry(t, registry, repinPlugin)
	stubInstallPluginViaEIC(t, func(context.Context, string, string, string, string) error { return nil })

	h := NewPluginsHandler(registry, nil, func(string) {}).
		WithRuntimeLookup(func(string) (string, error) { return "claude-code", nil }).
		WithInstanceIDLookup(func(string) (string, error) { return "i-0e0951a3cfd9bbf75", nil })

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: repinWorkspace}}
	c.Request = httptest.NewRequest("POST", "/workspaces/"+repinWorkspace+"/plugins", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	h.Install(c)
	drainTestAsync()
	return w
}

// TestInstallPushLeg_RefreshesExistingDeclarationOnly — Option B
// (molecule-core#5075): push-leg Install issues an UPDATE on
// workspace_declared_plugins BEFORE delivery (so re-pinning an already
// declared plugin updates its source_raw and never reverts on reconcile),
// while NOT inserting a new declaration row for an undeclared plugin (so
// push-leg Uninstall removes workspace_plugins cleanly without resurrecting
// ad-hoc user-installed plugins).
func TestInstallPushLeg_RefreshesExistingDeclarationOnly(t *testing.T) {
	for _, tc := range []struct {
		name         string
		rowsAffected int64
	}{
		{name: "undeclared_zero_rows", rowsAffected: 0},
		{name: "declared_repin_one_row", rowsAffected: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock, cleanup := withMockDB(t)
			defer cleanup()
			expectAllowlistAllowAll(mock)

			mock.ExpectExec(`UPDATE workspace_declared_plugins`).
				WithArgs(repinWorkspace, repinPlugin, repinSource).
				WillReturnResult(sqlmock.NewResult(0, tc.rowsAffected))
			mock.ExpectExec(`INSERT INTO workspace_plugins`).
				WithArgs(repinWorkspace, repinPlugin, repinSource, sqlmock.AnyArg(), sqlmock.AnyArg()).
				WillReturnResult(sqlmock.NewResult(1, 1))

			w := driveInstallPush(t, `{"source":"`+repinSource+`"}`)
			if w.Code != http.StatusOK {
				t.Fatalf("install returned %d, want 200: %s", w.Code, w.Body.String())
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("push-leg install did not satisfy DB expectations: %v", err)
			}
		})
	}
}

// TestInstallPushLeg_FailsWhenRefreshFails — if refreshing an existing
// declaration fails after container push, push-leg Install fails loudly rather
// than answering 200 while a stale declaration silently reverts the pin later.
func TestInstallPushLeg_FailsWhenRefreshFails(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()
	expectAllowlistAllowAll(mock)

	mock.ExpectExec(`UPDATE workspace_declared_plugins`).
		WillReturnError(errDeclareBoom)

	w := driveInstallPush(t, `{"source":"`+repinSource+`"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("install returned %d, want 500 on failed refresh: %s", w.Code, w.Body.String())
	}
}

// TestInstallPushLeg_RefusesPrivilegedPluginBeforeDelivery — Option B keeps the
// entitlement gate in checkPrivilegedPluginEntitlement ahead of push delivery.
func TestInstallPushLeg_RefusesPrivilegedPluginBeforeDelivery(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()
	expectAllowlistAllowAll(mock)

	mock.ExpectQuery(`SELECT COALESCE\(kind, 'workspace'\) FROM workspaces`).
		WithArgs(repinWorkspace).
		WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("workspace"))

	registry := t.TempDir()
	stagePluginRegistry(t, registry, conciergePlatformMCPName)
	eicCalled := false
	stubInstallPluginViaEIC(t, func(context.Context, string, string, string, string) error {
		eicCalled = true
		return nil
	})

	h := NewPluginsHandler(registry, nil, func(string) {}).
		WithRuntimeLookup(func(string) (string, error) { return "claude-code", nil }).
		WithInstanceIDLookup(func(string) (string, error) { return "i-0e0951a3cfd9bbf75", nil })

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: repinWorkspace}}
	c.Request = httptest.NewRequest("POST", "/workspaces/"+repinWorkspace+"/plugins", bytes.NewBufferString(`{"source":"local://`+conciergePlatformMCPName+`"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h.Install(c)
	drainTestAsync()

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("install returned %d, want 500 on non-platform privileged plugin: %s", w.Code, w.Body.String())
	}
	if eicCalled {
		t.Fatalf("EIC delivery ran despite privileged-plugin entitlement refusal")
	}
}

// TestInstallPullLeg_StillDeclaresViaUpsert — on errNoPushTarget, reMaterialize
// still upserts workspace_declared_plugins via recordDeclaredPlugin so boot-install
// materializes the plugin without firing a redundant UPDATE first.
func TestInstallPullLeg_StillDeclaresViaUpsert(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()
	expectAllowlistAllowAll(mock)

	mock.ExpectExec(`INSERT INTO workspace_declared_plugins`).
		WithArgs(repinWorkspace, repinPlugin, repinSource).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`INSERT INTO workspace_plugins`).
		WithArgs(repinWorkspace, repinPlugin, repinSource, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	registry := t.TempDir()
	stagePluginRegistry(t, registry, repinPlugin)

	h := NewPluginsHandler(registry, nil, func(string) {}).
		WithRuntimeLookup(func(string) (string, error) { return "claude-code", nil }).
		WithInstanceIDLookup(func(string) (string, error) { return "", nil })

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: repinWorkspace}}
	c.Request = httptest.NewRequest("POST", "/workspaces/"+repinWorkspace+"/plugins", bytes.NewBufferString(`{"source":"`+repinSource+`"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h.Install(c)
	drainTestAsync()

	if w.Code != http.StatusOK {
		t.Fatalf("pull-leg install returned %d, want 200: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("pull-leg install did not upsert declaration: %v", err)
	}
}

// TestDesiredSet_DeclarationAloneCarriesTheRePin — WHY the declare matters.
//
// Once the declaration has been moved to the new pin, the workspace boots the
// new pin even with NO installed row at all (uninstall, restore, re-create,
// a reconcile that has not recorded tracking yet). Before the fix the
// declaration still held the OLD ref here, so this same situation silently
// served the stale version — the revert path.
func TestDesiredSet_DeclarationAloneCarriesTheRePin(t *testing.T) {
	const newPin = "gitea://molecule-ai/gmail-channel-molecule#v0.2.0"

	mock, cleanup := withMockDB(t)
	defer cleanup()
	expectDesiredSet(mock,
		[]DeclaredPlugin{{PluginName: "gmail-channel-molecule", SourceRaw: newPin}},
		nil, // installed row gone — only the declaration is left
	)

	got, err := desiredPluginSources(context.Background(), repinWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{newPin}; !reflect.DeepEqual(got, want) {
		t.Fatalf("desired set = %v, want %v — the declaration must carry the re-pin on its own", got, want)
	}
}

// TestDesiredSet_StaleDeclarationReverts — the failure mode, pinned as a
// characterisation test so nobody "fixes" installed-wins and reintroduces it.
//
// This is exactly the pre-fix state of workspace c4ebf3dc: installed had been
// moved, the declaration had not. With the installed row absent the workspace
// silently serves the OLD ref. The guarantee the install-time declare buys us
// is that this state is no longer REACHABLE through the install pipeline.
func TestDesiredSet_StaleDeclarationReverts(t *testing.T) {
	const oldPin = "gitea://molecule-ai/gmail-channel-molecule#v0.1.4"

	mock, cleanup := withMockDB(t)
	defer cleanup()
	expectDesiredSet(mock,
		[]DeclaredPlugin{{PluginName: "gmail-channel-molecule", SourceRaw: oldPin}},
		nil,
	)

	got, err := desiredPluginSources(context.Background(), repinWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{oldPin}; !reflect.DeepEqual(got, want) {
		t.Fatalf("desired set = %v, want %v", got, want)
	}
}
