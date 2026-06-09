package handlers

// workspace_provider_switch_test.go — deterministic coverage for the in-place
// cloud-provider switch in the Update (PATCH /workspaces/:id) handler.
//
// The switch is DESTRUCTIVE (it recreates the box on a new cloud) and its
// safety hinges on ORDER + ABORT, which these tests pin without touching a real
// cloud (sqlmock DB + the scriptedCPStop fake from workspace_restart_stop_retry_test):
//
//   1. On a provider change, the OLD box is deprovisioned (cpProv.Stop) BEFORE
//      the compute row is overwritten — otherwise the later restart's
//      provider-aware deprovision would target the NEW cloud and ORPHAN the old
//      (still-billing) box. The sqlmock query ORDER pins "read old provider →
//      [Stop] → UPDATE compute".
//   2. If the old-box deprovision FAILS, the handler ABORTS (502) and does NOT
//      overwrite compute — leaving the row pointed at the recoverable old box
//      (an unexpected UPDATE would fail sqlmock's expectations).
//   3. A non-switch compute edit (same provider) does NOT deprovision anything.

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

func newPatchContext(t *testing.T, id, body string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: id}}
	req := httptest.NewRequest("PATCH", "/workspaces/"+id, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	return c, w
}

const switchTestWSID = "cccccccc-0001-0000-0000-000000000000"

func newSwitchTestHandler(t *testing.T, cp *scriptedCPStop) *WorkspaceHandler {
	t.Helper()
	h := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	h.cpProv = cp
	return h
}

// 1. aws → hetzner: deprovision the OLD box, THEN overwrite compute (200).
func TestWorkspaceUpdate_ProviderSwitch_DeprovisionsOldBeforeUpdate(t *testing.T) {
	mock := setupTestDB(t)
	cp := &scriptedCPStop{} // Stop succeeds
	h := newSwitchTestHandler(t, cp)

	// Ordered expectations pin: EXISTS → read OLD provider (aws) → UPDATE compute.
	// The cpProv.Stop deprovision runs (in code) AFTER the provider read and
	// BEFORE the UPDATE — exactly the orphan-safe order.
	mock.ExpectQuery("SELECT EXISTS").WithArgs(switchTestWSID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery("compute->>'provider'").WithArgs(switchTestWSID).
		WillReturnRows(sqlmock.NewRows([]string{"provider"}).AddRow("aws"))
	mock.ExpectExec("UPDATE workspaces SET compute").
		WillReturnResult(sqlmock.NewResult(0, 1))

	c, w := newPatchContext(t, switchTestWSID,
		`{"compute":{"instance_type":"cpx31","provider":"hetzner","volume":{"root_gb":30}}}`)
	h.Update(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on a successful switch, got %d: %s", w.Code, w.Body.String())
	}
	if cp.calls != 1 {
		t.Fatalf("expected the OLD box to be deprovisioned exactly once on a provider switch; got %d Stop calls", cp.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet/unexpected DB queries (ordering broken?): %v", err)
	}
}

// 2. Deprovision FAILS → abort (502) + compute NOT overwritten (no UPDATE).
func TestWorkspaceUpdate_ProviderSwitch_AbortsWhenDeprovisionFails(t *testing.T) {
	shrinkRetryBackoff(t) // don't burn the 1s/2s/4s retry backoff
	mock := setupTestDB(t)
	// All retry attempts fail → cpStopWithRetryErr returns an error → abort.
	cp := &scriptedCPStop{errs: []error{
		fmt.Errorf("cp 503"), fmt.Errorf("cp 503"), fmt.Errorf("cp 503"),
	}}
	h := newSwitchTestHandler(t, cp)

	mock.ExpectQuery("SELECT EXISTS").WithArgs(switchTestWSID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery("compute->>'provider'").WithArgs(switchTestWSID).
		WillReturnRows(sqlmock.NewRows([]string{"provider"}).AddRow("aws"))
	// NO UPDATE expectation: if the handler overwrote compute after a failed
	// deprovision (the orphan bug), sqlmock would flag the unexpected query.

	c, w := newPatchContext(t, switchTestWSID,
		`{"compute":{"instance_type":"cpx31","provider":"hetzner","volume":{"root_gb":30}}}`)
	h.Update(c)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 when the old-box deprovision fails, got %d: %s", w.Code, w.Body.String())
	}
	if cp.calls == 0 {
		t.Fatal("expected at least one Stop attempt before aborting")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		// A failure here means an UNEXPECTED UPDATE ran — i.e. compute was
		// overwritten after a failed deprovision → the orphan bug is back.
		t.Fatalf("compute must NOT be overwritten when deprovision fails (orphan-prevention): %v", err)
	}
}

// 3. Same provider (no switch): no deprovision; compute is updated normally.
func TestWorkspaceUpdate_NoProviderSwitch_DoesNotDeprovision(t *testing.T) {
	mock := setupTestDB(t)
	cp := &scriptedCPStop{}
	h := newSwitchTestHandler(t, cp)

	mock.ExpectQuery("SELECT EXISTS").WithArgs(switchTestWSID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery("compute->>'provider'").WithArgs(switchTestWSID).
		WillReturnRows(sqlmock.NewRows([]string{"provider"}).AddRow("aws"))
	mock.ExpectExec("UPDATE workspaces SET compute").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// provider stays aws (only the instance size changes) → no switch, no Stop.
	c, w := newPatchContext(t, switchTestWSID,
		`{"compute":{"instance_type":"t3.large","provider":"aws","volume":{"root_gb":60}}}`)
	h.Update(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if cp.calls != 0 {
		t.Fatalf("a non-switching compute edit must NOT deprovision the box; got %d Stop calls", cp.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet/unexpected DB queries: %v", err)
	}
}
