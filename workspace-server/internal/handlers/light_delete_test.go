package handlers

// light_delete_test.go — Enter OS Light: deleting a workspace deletes its
// control-plane Light objects (C4 DELETE) before any compute stops.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
)

// orderedLightAPI records its DeleteLight calls in the provisioner's call log,
// so a test sees the Light delete and the compute stop in one order.
type orderedLightAPI struct {
	*fakeLightAPI
	prov *inflightRecordingProv
}

func (o orderedLightAPI) DeleteLight(ctx context.Context, workspaceID string) (provisioner.LightDeleteResult, error) {
	o.prov.record("light-delete:" + workspaceID)
	return o.fakeLightAPI.DeleteLight(ctx, workspaceID)
}

const (
	lightDeleteRoot  = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	lightDeleteChild = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
)

const lightIDsQuery = `SELECT id FROM workspaces WHERE id = ANY\(\$1::uuid\[\]\) AND light_state IS NOT NULL`

const lightClearAfterDelete = `UPDATE workspaces\s+SET light_state = NULL, light_state_at = now\(\), light_opened_by = NULL,\s+light_run_token_id = NULL, light_retired_token_id = NULL\s+WHERE id = \$1`

// cascadeDeleteWithLight runs CascadeDelete(root) over root+child; script adds
// the Light expectations after the descendant walk and the removed-mark.
func cascadeDeleteWithLight(t *testing.T, api *fakeLightAPI, script func(sqlmock.Sqlmock)) (calls string, stopErrs []error, mock sqlmock.Sqlmock) {
	t.Helper()
	mock = setupTestDB(t)
	setupTestRedis(t)
	mock.ExpectQuery(`WITH RECURSIVE descendants AS`).
		WithArgs(lightDeleteRoot).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(lightDeleteChild))
	mock.ExpectExec(`UPDATE workspaces SET status = \$1, updated_at = now\(\) WHERE id = ANY`).
		WillReturnResult(sqlmock.NewResult(0, 2))
	script(mock)

	h := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	prov := &inflightRecordingProv{}
	h.provisioner = prov
	h.SetLightAPI(orderedLightAPI{fakeLightAPI: api, prov: prov})

	_, stopErrs, err := h.CascadeDelete(context.Background(), lightDeleteRoot, false)
	if err != nil {
		t.Fatalf("CascadeDelete: %v", err)
	}
	return strings.Join(prov.calls, " "), stopErrs, mock
}

func TestCascadeDeleteRemovesLightObjectsBeforeAnyStop(t *testing.T) {
	api := &fakeLightAPI{deleted: provisioner.LightDeleteResult{}}
	calls, stopErrs, mock := cascadeDeleteWithLight(t, api, func(mock sqlmock.Sqlmock) {
		mock.ExpectQuery(lightIDsQuery).
			WithArgs(pqArrayArg{lightDeleteRoot, lightDeleteChild}).
			WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(lightDeleteRoot))
		mock.ExpectExec(lightClearAfterDelete).WithArgs(lightDeleteRoot).
			WillReturnResult(sqlmock.NewResult(0, 1))
	})
	if len(stopErrs) != 0 {
		t.Fatalf("stopErrs = %v", stopErrs)
	}
	want := fmt.Sprintf("cancel:%[1]s cancel:%[2]s unwound:%[1]s unwound:%[2]s light-delete:%[1]s stop:%[2]s remove-volume:%[2]s stop:%[1]s remove-volume:%[1]s",
		lightDeleteRoot, lightDeleteChild)
	if calls != want {
		t.Fatalf("delete order:\n got: %s\nwant: %s\n(only the Light agent's objects are deleted, and before any compute stops)", calls, want)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestCascadeDeleteKeepsTheLightMarkerWhenTheDeleteFails(t *testing.T) {
	api := &fakeLightAPI{deleteErr: &provisioner.LightCPError{HTTPStatus: 503, Code: "LIGHT_BACKEND_UNAVAILABLE"}}
	calls, stopErrs, mock := cascadeDeleteWithLight(t, api, func(mock sqlmock.Sqlmock) {
		mock.ExpectQuery(lightIDsQuery).
			WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(lightDeleteRoot))
		// Scripted to prove it is NOT issued: the row must keep light_state.
		mock.ExpectExec(lightClearAfterDelete).WithArgs(lightDeleteRoot).
			WillReturnResult(sqlmock.NewResult(0, 1))
	})
	if len(stopErrs) != 1 || !errors.Is(stopErrs[0], provisioner.ErrLightCPUnavailable) || !strings.Contains(stopErrs[0].Error(), "light delete "+lightDeleteRoot) {
		t.Fatalf("stopErrs = %v, want one light delete failure (the delete answers 500)", stopErrs)
	}
	if !strings.Contains(calls, "stop:"+lightDeleteRoot) || !strings.Contains(calls, "stop:"+lightDeleteChild) {
		t.Fatalf("a failed Light delete must not keep the compute running: %s", calls)
	}
	if err := mock.ExpectationsWereMet(); err == nil || !strings.Contains(err.Error(), "light_state = NULL") {
		t.Fatalf("the Light columns were cleared although the Light objects remain (unmet = %v)", err)
	}
}

func TestCascadeDeleteWithoutLightAgentsMakesNoLightCall(t *testing.T) {
	api := &fakeLightAPI{}
	_, stopErrs, mock := cascadeDeleteWithLight(t, api, func(mock sqlmock.Sqlmock) {
		mock.ExpectQuery(lightIDsQuery).WillReturnRows(sqlmock.NewRows([]string{"id"}))
	})
	if len(stopErrs) != 0 || len(api.Calls()) != 0 {
		t.Fatalf("stopErrs=%v light calls=%v, want none", stopErrs, api.Calls())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestCascadeDeleteLightReadFailureFailsOpen(t *testing.T) {
	api := &fakeLightAPI{}
	calls, stopErrs, _ := cascadeDeleteWithLight(t, api, func(mock sqlmock.Sqlmock) {
		mock.ExpectQuery(lightIDsQuery).WillReturnError(errors.New("db blip"))
	})
	if len(stopErrs) != 0 || len(api.Calls()) != 0 {
		t.Fatalf("stopErrs=%v light calls=%v: a failed read must not block or fail the delete", stopErrs, api.Calls())
	}
	if !strings.Contains(calls, "stop:"+lightDeleteRoot) {
		t.Fatalf("the delete must still stop the compute: %s", calls)
	}
}

func TestCascadeDeleteOnATenantWithoutLightSupportSkipsTheCall(t *testing.T) {
	api := &fakeLightAPI{deleteErr: &provisioner.LightCPError{HTTPStatus: 501, Code: "LIGHT_UNSUPPORTED"}}
	_, stopErrs, mock := cascadeDeleteWithLight(t, api, func(mock sqlmock.Sqlmock) {
		mock.ExpectQuery(lightIDsQuery).
			WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(lightDeleteRoot))
		mock.ExpectExec(lightClearAfterDelete).WithArgs(lightDeleteRoot).
			WillReturnResult(sqlmock.NewResult(0, 1))
	})
	if len(stopErrs) != 0 {
		t.Fatalf("stopErrs = %v: a substrate that cannot run Light has nothing to delete", stopErrs)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}
