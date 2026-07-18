package handlers

import (
	"bytes"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

const sensitiveWorkspaceListFailure = "sensitive-workspace-list-failure"

func workspaceListTestRow(id string, tier driver.Value) []driver.Value {
	return []driver.Value{
		id, "Agent " + id, "worker", tier, "online", []byte("null"), "",
		nil, 0, 1, 0.0, "", 0, "", "claude-code", "", 0.0, 0.0, false,
		nil, int64(0), false, true, []byte(`{}`), "workspace", []byte(`[]`),
	}
}

func captureWorkspaceListLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	previousWriter := log.Writer()
	previousFlags := log.Flags()
	previousPrefix := log.Prefix()

	var output bytes.Buffer
	log.SetOutput(&output)
	log.SetFlags(0)
	log.SetPrefix("")
	t.Cleanup(func() {
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
		log.SetPrefix(previousPrefix)
	})
	return &output
}

func assertGenericWorkspaceListFailure(t *testing.T, recorder *httptest.ResponseRecorder, logs string) {
	t.Helper()
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 without a partial or false-zero roster, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), sensitiveWorkspaceListFailure) {
		t.Fatalf("response leaked the database failure: %s", recorder.Body.String())
	}
	if strings.Contains(logs, sensitiveWorkspaceListFailure) {
		t.Fatalf("logs leaked the database failure: %s", logs)
	}

	var response map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("parse error response: %v", err)
	}
	if response["error"] != "failed to list workspaces" {
		t.Fatalf("expected generic list error, got %q", response["error"])
	}
}

func TestWorkspaceList_RowScanFailureFailsClosed(t *testing.T) {
	tests := []struct {
		name string
		rows [][]driver.Value
	}{
		{
			name: "first row",
			rows: [][]driver.Value{
				workspaceListTestRow("bad-first", sensitiveWorkspaceListFailure),
				workspaceListTestRow("valid-second", int64(1)),
			},
		},
		{
			name: "middle row",
			rows: [][]driver.Value{
				workspaceListTestRow("valid-first", int64(1)),
				workspaceListTestRow("bad-middle", sensitiveWorkspaceListFailure),
				workspaceListTestRow("valid-third", int64(1)),
			},
		},
		{
			name: "all rows",
			rows: [][]driver.Value{
				workspaceListTestRow("bad-first", sensitiveWorkspaceListFailure),
				workspaceListTestRow("bad-second", sensitiveWorkspaceListFailure),
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			logs := captureWorkspaceListLogs(t)
			mock := setupTestDB(t)
			handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

			rows := sqlmock.NewRows(wsColumns)
			for _, row := range test.rows {
				rows.AddRow(row...)
			}
			mock.ExpectQuery("SELECT w.id, w.name").WillReturnRows(rows)

			recorder := httptest.NewRecorder()
			context, _ := gin.CreateTestContext(recorder)
			context.Request = httptest.NewRequest(http.MethodGet, "/workspaces", nil)

			handler.List(context)

			assertGenericWorkspaceListFailure(t, recorder, logs.String())
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("unmet sqlmock expectations: %v", err)
			}
		})
	}
}

func TestWorkspaceList_RowIterationFailureFailsClosed(t *testing.T) {
	logs := captureWorkspaceListLogs(t)
	mock := setupTestDB(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	rows := sqlmock.NewRows(wsColumns).
		AddRow(workspaceListTestRow("valid-first", int64(1))...).
		AddRow(workspaceListTestRow("unreadable-second", int64(1))...).
		RowError(1, errors.New(sensitiveWorkspaceListFailure))
	mock.ExpectQuery("SELECT w.id, w.name").WillReturnRows(rows)

	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodGet, "/workspaces", nil)

	handler.List(context)

	assertGenericWorkspaceListFailure(t, recorder, logs.String())
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

func TestWorkspaceList_QueryFailureFailsClosed(t *testing.T) {
	logs := captureWorkspaceListLogs(t)
	mock := setupTestDB(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery("SELECT w.id, w.name").WillReturnError(errors.New(sensitiveWorkspaceListFailure))

	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodGet, "/workspaces", nil)

	handler.List(context)

	assertGenericWorkspaceListFailure(t, recorder, logs.String())
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

func TestWorkspaceList_ValidEmptyRosterRemainsEmptyArray(t *testing.T) {
	mock := setupTestDB(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery("SELECT w.id, w.name").WillReturnRows(sqlmock.NewRows(wsColumns))

	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodGet, "/workspaces", nil)

	handler.List(context)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected valid empty roster to return 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if strings.TrimSpace(recorder.Body.String()) != "[]" {
		t.Fatalf("expected an empty JSON array, got %s", recorder.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}
