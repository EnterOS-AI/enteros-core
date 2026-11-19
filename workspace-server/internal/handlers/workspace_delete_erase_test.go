package handlers

// workspace_delete_erase_test.go — which deletes erase the workspace's saved
// data on the control plane.
//
// ?purge=true hard-deletes the workspace row and everything keyed on it. After
// that nothing can attach the workspace's volume again: a new workspace gets a
// new id and a new volume. A purge that did not also ask for the erase
// therefore left the volume behind for good — on Kubernetes an identity claim
// that counts against the namespace's persistentvolumeclaims quota until the
// tenant can provision nothing (molecule-core#5193 is that wall).
//
// MEASURED on the staging canary, 2026-11-19: every agent deleted with
// ?purge=true alone kept its claim Bound, with no deletionTimestamp, 100 s
// later; two such claims were still in the namespace, with no workspace row.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

const deleteEraseWS = "dddddddd-000a-0000-0000-000000000000"

// deleteLeafOnCP drives DELETE /workspaces/<id>?confirm=true<query> for a leaf
// workspace on a control-plane backend, and returns what the backend was asked
// to do and the response's status field. Only the reads and writes the handler
// cannot proceed without are scripted; its best-effort statements (audit rows,
// broadcasts, per-table purge deletes) fail against the mock and are logged.
func deleteLeafOnCP(t *testing.T, query string, purges bool) (*trackingCPProv, string) {
	t.Helper()
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	cp := &trackingCPProv{}
	handler.SetCPProvisioner(cp)

	expectWorkspaceDeleteLookup(mock, deleteEraseWS, "Erase Me", 0, "online")
	mock.ExpectQuery("SELECT id, name FROM workspaces WHERE parent_id").
		WithArgs(deleteEraseWS).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name"}))
	mock.ExpectQuery("WITH RECURSIVE descendants").
		WithArgs(deleteEraseWS).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectExec("UPDATE workspaces SET status =").
		WillReturnResult(sqlmock.NewResult(1, 1))
	if purges {
		mock.ExpectExec(`DELETE FROM workspaces WHERE id = ANY`).
			WillReturnResult(sqlmock.NewResult(1, 1))
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: deleteEraseWS}}
	c.Request = httptest.NewRequest(http.MethodDelete, "/workspaces/"+deleteEraseWS+"?confirm=true"+query, nil)
	c.Request.Header.Set("X-Confirm-Name", "Erase Me")

	handler.Delete(c)

	if w.Code != http.StatusOK {
		t.Fatalf("DELETE ?confirm=true%s = %d: %s", query, w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
	status, _ := resp["status"].(string)
	return cp, status
}

// A purge leaves no row to ever reach the volume from, so it erases the volume.
func TestDelete_PurgeErasesTheSavedData(t *testing.T) {
	cp, status := deleteLeafOnCP(t, "&purge=true", true)
	if status != "purged" {
		t.Errorf("status = %q, want purged", status)
	}
	if want := []string{deleteEraseWS}; !reflect.DeepEqual(cp.pruned, want) {
		t.Errorf("the control plane was asked to erase %v, want %v — a purged workspace's volume can never be attached again, so keeping it only leaks it", cp.pruned, want)
	}
}

// internal#734: the user's own choice on the delete dialog.
func TestDelete_EraseDataErasesTheSavedData(t *testing.T) {
	cp, status := deleteLeafOnCP(t, "&erase_data=true", false)
	if status != "removed" {
		t.Errorf("status = %q, want removed", status)
	}
	if want := []string{deleteEraseWS}; !reflect.DeepEqual(cp.pruned, want) {
		t.Errorf("the control plane was asked to erase %v, want %v", cp.pruned, want)
	}
}

// A plain delete keeps the row (status removed) and the data: the erase stays
// opt-in.
func TestDelete_PlainDeleteKeepsTheSavedData(t *testing.T) {
	cp, status := deleteLeafOnCP(t, "", false)
	if status != "removed" {
		t.Errorf("status = %q, want removed", status)
	}
	if len(cp.pruned) != 0 {
		t.Errorf("a plain delete asked the control plane to erase %v", cp.pruned)
	}
	if want := []string{deleteEraseWS}; !reflect.DeepEqual(cp.stoppedSnapshot(), want) {
		t.Errorf("stopped = %v, want %v", cp.stoppedSnapshot(), want)
	}
}

// purge=false and erase_data=false are not requests to erase.
func TestDelete_OnlyTheLiteralTrueErases(t *testing.T) {
	for _, q := range []string{"&purge=false", "&erase_data=false", "&purge=1", "&erase_data=yes"} {
		cp, _ := deleteLeafOnCP(t, q, false)
		if len(cp.pruned) != 0 {
			t.Errorf("%s asked the control plane to erase %v", q, cp.pruned)
		}
	}
}
