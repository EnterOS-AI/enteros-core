package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/lib/pq"
)

const renameTestID = "aaaaaaaa-0000-4000-8000-000000000001"

func patchWorkspaceName(t *testing.T, h *WorkspaceHandler, id string, name any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{"name": name})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: id}}
	c.Request = httptest.NewRequest(http.MethodPatch, "/workspaces/"+id, strings.NewReader(string(body)))
	c.Request.Header.Set("Content-Type", "application/json")
	h.Update(c)
	return w
}

func TestWorkspaceRename_RejectsInvalidNamesBeforeWriting(t *testing.T) {
	for _, name := range []any{nil, 42, true, []string{"brand"}, map[string]string{"name": "brand"}, "", " \t ", "line\nbreak", "nul\x00byte", "tab\tname", strings.Repeat("x", 256)} {
		t.Run(fmt.Sprintf("%v", name), func(t *testing.T) {
			setupTestDB(t) // Strict unexpected-query guard: validation must not touch the DB.
			emitter := &capturingEmitter{}
			h := NewWorkspaceHandler(emitter, nil, "", "")
			w := patchWorkspaceName(t, h, renameTestID, name)
			if w.Code != http.StatusBadRequest || len(emitter.events) != 0 {
				t.Fatalf("status=%d, events=%v, body=%s", w.Code, emitter.events, w.Body)
			}
		})
	}
}

func TestWorkspaceRename_ConfirmedWriteAndBroadcast(t *testing.T) {
	mock := setupTestDB(t)
	t.Cleanup(func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})
	emitter := &capturingEmitter{}
	h := NewWorkspaceHandler(emitter, nil, "", "")
	mock.ExpectQuery("SELECT EXISTS").WithArgs(renameTestID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	// Exactly one in-place write; no provision, delete, config or history writes.
	mock.ExpectExec("UPDATE workspaces SET name = \\$2, updated_at = now\\(\\) WHERE id = \\$1 AND status != 'removed'").
		WithArgs(renameTestID, "Personal Brand 工作区").WillReturnResult(sqlmock.NewResult(0, 1))
	w := patchWorkspaceName(t, h, renameTestID, " Personal Brand 工作区 ")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d: %s", w.Code, w.Body)
	}
	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result["workspace_id"] != renameTestID || result["name"] != "Personal Brand 工作区" || result["needs_restart"] != nil {
		t.Fatalf("unexpected rename response: %v", result)
	}
	if len(emitter.events) != 1 || emitter.events[0].workspaceID != renameTestID || emitter.events[0].eventType != "WORKSPACE_UPDATED" {
		t.Fatalf("events=%v", emitter.events)
	}
	if emitter.events[0].payload.(gin.H)["name"] != "Personal Brand \u5de5\u4f5c\u533a" {
		t.Fatalf("payload=%v", emitter.events[0].payload)
	}
}

func TestWorkspaceRename_StorageFailuresNeverReportSuccess(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(sqlmock.Sqlmock)
		status int
	}{
		{"lookup failure", func(m sqlmock.Sqlmock) {
			m.ExpectQuery("SELECT EXISTS").WillReturnError(errors.New("database unavailable"))
		}, http.StatusInternalServerError},
		{"missing workspace", func(m sqlmock.Sqlmock) {
			m.ExpectQuery("SELECT EXISTS").WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))
		}, http.StatusNotFound},
		{"sibling name collision", func(m sqlmock.Sqlmock) {
			m.ExpectExec("UPDATE workspaces SET name").WillReturnError(&pq.Error{Code: "23505", Constraint: workspacesUniqueIndexName})
		}, http.StatusConflict},
		{"other database failure", func(m sqlmock.Sqlmock) {
			m.ExpectExec("UPDATE workspaces SET name").WillReturnError(errors.New("write failed"))
		}, http.StatusInternalServerError},
		{"concurrent removal", func(m sqlmock.Sqlmock) {
			m.ExpectExec("UPDATE workspaces SET name").WillReturnResult(sqlmock.NewResult(0, 0))
		}, http.StatusNotFound},
		{"unreadable write result", func(m sqlmock.Sqlmock) {
			m.ExpectExec("UPDATE workspaces SET name").WillReturnResult(sqlmock.NewErrorResult(errors.New("result unavailable")))
		}, http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := setupTestDB(t)
			t.Cleanup(func() {
				if err := mock.ExpectationsWereMet(); err != nil {
					t.Fatal(err)
				}
			})
			emitter := &capturingEmitter{}
			h := NewWorkspaceHandler(emitter, nil, "", "")
			if tc.name != "lookup failure" && tc.name != "missing workspace" {
				mock.ExpectQuery("SELECT EXISTS").WithArgs(renameTestID).
					WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
			}
			tc.setup(mock)
			w := patchWorkspaceName(t, h, renameTestID, "Personal Brand")
			if w.Code != tc.status || len(emitter.events) != 0 {
				t.Fatalf("status=%d, want=%d, events=%v, body=%s", w.Code, tc.status, emitter.events, w.Body)
			}
		})
	}
}
