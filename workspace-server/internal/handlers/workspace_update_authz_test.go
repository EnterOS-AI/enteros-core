package handlers

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

func TestWorkspaceUpdate_WorkspaceTokenCannotPatchInfrastructure(t *testing.T) {
	testCases := []struct {
		name string
		body string
	}{
		{name: "tier", body: `{"tier":4}`},
		{name: "parent", body: `{"parent_id":"bbbbbbbb-0000-0000-0000-000000000000"}`},
		{name: "runtime", body: `{"runtime":"codex"}`},
		{name: "host directory", body: `{"workspace_dir":"/srv/molecule"}`},
		{name: "compute", body: `{"compute":{"provider":"local"}}`},
		{name: "mixed cosmetic and infrastructure", body: `{"name":"owned","tier":4}`},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			mock := setupTestDB(t)
			setupTestRedis(t)
			handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Params = gin.Params{{Key: "id", Value: "aaaaaaaa-0000-0000-0000-000000000001"}}
			c.Set("caller_credential_class", "workspace-token")
			c.Request = httptest.NewRequest(http.MethodPatch, "/workspaces/aaaaaaaa-0000-0000-0000-000000000001", bytes.NewBufferString(tc.body))
			c.Request.Header.Set("Content-Type", "application/json")

			handler.Update(c)

			if w.Code != http.StatusForbidden {
				t.Fatalf("PATCH %s with workspace token: want 403, got %d: %s", tc.body, w.Code, w.Body.String())
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("infrastructure rejection must happen before database work: %v", err)
			}
		})
	}
}

func TestWorkspaceUpdate_WorkspaceTokenCanPatchCosmeticFields(t *testing.T) {
	const workspaceID = "aaaaaaaa-0000-0000-0000-000000000002"
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery("SELECT EXISTS").
		WithArgs(workspaceID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectExec("UPDATE workspaces SET name").
		WithArgs(workspaceID, "Updated Name").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: workspaceID}}
	c.Set("caller_credential_class", "workspace-token")
	c.Request = httptest.NewRequest(http.MethodPatch, "/workspaces/"+workspaceID, bytes.NewBufferString(`{"name":"Updated Name"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Update(c)

	if w.Code != http.StatusOK {
		t.Fatalf("cosmetic PATCH with workspace token: want 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceUpdate_AdminAndSessionCanPatchInfrastructure(t *testing.T) {
	for _, credentialClass := range []string{"admin-token", "cp-session"} {
		t.Run(credentialClass, func(t *testing.T) {
			const workspaceID = "aaaaaaaa-0000-0000-0000-000000000003"
			mock := setupTestDB(t)
			setupTestRedis(t)
			handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

			mock.ExpectQuery("SELECT EXISTS").
				WithArgs(workspaceID).
				WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
			mock.ExpectExec("UPDATE workspaces SET tier").
				WithArgs(workspaceID, float64(3)).
				WillReturnResult(sqlmock.NewResult(0, 1))

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Params = gin.Params{{Key: "id", Value: workspaceID}}
			c.Set("caller_credential_class", credentialClass)
			c.Request = httptest.NewRequest(http.MethodPatch, "/workspaces/"+workspaceID, bytes.NewBufferString(`{"tier":3}`))
			c.Request.Header.Set("Content-Type", "application/json")

			handler.Update(c)

			if w.Code != http.StatusOK {
				t.Fatalf("infrastructure PATCH with %s: want 200, got %d: %s", credentialClass, w.Code, w.Body.String())
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
