package handlers

// workspace_delete_completes_test.go — two ways a delete could stop half way
// (review of 2026-11-19, internal#1316).
//
// 1. The client hangs up. Every step of a delete ran on the request context, so
//    a caller that gave up (an SDK timeout during the wait for an in-flight
//    provision, a closed tab) cancelled the teardown and the purge. The rows
//    were already marked removed, and a removed row answers 410 to a retry, so
//    nothing could finish the delete: the orphan sweeper later stopped the
//    workspace keep-state and its volume, seed Secret and Service stayed.
//
// 2. The id is not in canonical form. validateWorkspaceID accepts every form
//    uuid.Parse does (upper case, braces, urn:uuid:), Postgres compares them as
//    equal, but the in-flight provision list is keyed by the string, so such a
//    delete did not wait for the provision it was racing.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// hangUpCPProv is a control plane whose erase is in flight when the client
// gives up: it cancels the request context, then reports what context the
// handler had handed it.
type hangUpCPProv struct {
	*trackingCPProv
	hangUp       context.CancelFunc
	ctxErrAtStop error
}

func (p *hangUpCPProv) StopAndPrune(ctx context.Context, workspaceID string) error {
	p.hangUp()
	p.ctxErrAtStop = ctx.Err()
	return p.trackingCPProv.StopAndPrune(ctx, workspaceID)
}

func TestDelete_FinishesAfterTheClientHangsUp(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	reqCtx, hangUp := context.WithCancel(context.Background())
	defer hangUp()
	cp := &hangUpCPProv{trackingCPProv: &trackingCPProv{}, hangUp: hangUp}
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
	mock.ExpectExec(`DELETE FROM workspaces WHERE id = ANY`).
		WillReturnResult(sqlmock.NewResult(1, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: deleteEraseWS}}
	c.Request = httptest.NewRequest(http.MethodDelete, "/workspaces/"+deleteEraseWS+"?confirm=true&purge=true", nil).WithContext(reqCtx)
	c.Request.Header.Set("X-Confirm-Name", "Erase Me")

	handler.Delete(c)

	if reqCtx.Err() == nil {
		t.Fatal("the test's client never hung up: the erase was not reached")
	}
	if cp.ctxErrAtStop != nil {
		t.Errorf("the erase ran on a context the client's hang-up had already cancelled (%v): the control plane call is abandoned and the volume stays", cp.ctxErrAtStop)
	}
	if len(cp.pruned) != 1 || cp.pruned[0] != deleteEraseWS {
		t.Errorf("erased = %v, want [%s]", cp.pruned, deleteEraseWS)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the purge did not finish after the client hung up: %v", err)
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if w.Code != http.StatusOK || resp["status"] != "purged" {
		t.Errorf("handler outcome = %d %s, want 200 purged", w.Code, w.Body.String())
	}
}

func TestInflightCPProvisions_FindsAProvisionByAnyFormOfItsID(t *testing.T) {
	const canonical = "dddddddd-000b-0000-0000-00000000abcd"
	done := trackCPProvision(canonical)
	defer done()

	for _, form := range []string{
		canonical,
		"DDDDDDDD-000B-0000-0000-00000000ABCD",
		"{dddddddd-000b-0000-0000-00000000abcd}",
		"urn:uuid:dddddddd-000b-0000-0000-00000000abcd",
		"dddddddd000b0000000000000000abcd",
	} {
		if n := len(inflightCPProvisions(form)); n != 1 {
			t.Errorf("inflightCPProvisions(%q) found %d provision(s), want 1: a delete addressed this way would not wait for it", form, n)
		}
	}
	if n := len(inflightCPProvisions("eeeeeeee-000b-0000-0000-00000000abcd")); n != 0 {
		t.Errorf("another workspace's id found %d provision(s), want 0", n)
	}
}

// A provision registered under a non-canonical id is found by the canonical one.
func TestTrackCPProvision_RegistersUnderTheCanonicalID(t *testing.T) {
	done := trackCPProvision("DDDDDDDD-000C-0000-0000-00000000ABCD")
	if n := len(inflightCPProvisions("dddddddd-000c-0000-0000-00000000abcd")); n != 1 {
		t.Errorf("found %d provision(s) by the canonical id, want 1", n)
	}
	done()
	if n := len(inflightCPProvisions("dddddddd-000c-0000-0000-00000000abcd")); n != 0 {
		t.Errorf("found %d provision(s) after it returned, want 0", n)
	}
}
