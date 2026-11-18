package middleware

import (
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

const lightRunTestWS = "be7540cf-d160-4e2a-9b1c-0123456789ab"

// runLightRunID sends one request through LightRunID and returns what the
// handler saw on its context.
func runLightRunID(t *testing.T, headers map[string]string, script func(sqlmock.Sqlmock)) (runID, workspaceID string, tagged bool, status int) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { mockDB.Close() })
	if script != nil {
		script(mock)
	}
	r := gin.New()
	r.Use(LightRunID(mockDB))
	r.POST("/x", func(c *gin.Context) {
		runID, workspaceID, tagged = LightRunFromContext(c.Request.Context())
		c.Status(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
	return runID, workspaceID, tagged, w.Code
}

func expectTokenWorkspace(token string, workspaceID string) func(sqlmock.Sqlmock) {
	return func(mock sqlmock.Sqlmock) {
		sum := sha256.Sum256([]byte(token))
		mock.ExpectQuery(`SELECT t\.id, t\.workspace_id`).WithArgs(sum[:]).
			WillReturnRows(sqlmock.NewRows([]string{"id", "workspace_id"}).AddRow("tok-1", workspaceID))
	}
}

func TestLightRunIDTagsRequestsFromAWorkspaceToken(t *testing.T) {
	runID, ws, tagged, status := runLightRunID(t, map[string]string{
		"X-Molecule-Run-Id": "light-be7540cfd160-1a2b3c4d-29384756",
		"Authorization":     "Bearer the-run-token",
	}, expectTokenWorkspace("the-run-token", lightRunTestWS))
	if !tagged || runID != "light-be7540cfd160-1a2b3c4d-29384756" || ws != lightRunTestWS || status != http.StatusOK {
		t.Fatalf("tagged=%v run=%q ws=%q status=%d", tagged, runID, ws, status)
	}
}

func TestLightRunIDIgnoresOtherCredentials(t *testing.T) {
	// No bearer: no lookup, untagged.
	if _, _, tagged, status := runLightRunID(t, map[string]string{"X-Molecule-Run-Id": "light-x"}, nil); tagged || status != http.StatusOK {
		t.Errorf("no bearer: tagged=%v status=%d", tagged, status)
	}
	// An admin or org token is not a workspace token: the lookup misses.
	_, _, tagged, status := runLightRunID(t, map[string]string{"X-Molecule-Run-Id": "light-x", "Authorization": "Bearer admin-token"},
		func(mock sqlmock.Sqlmock) {
			mock.ExpectQuery(`SELECT t\.id, t\.workspace_id`).WillReturnRows(sqlmock.NewRows([]string{"id", "workspace_id"}))
		})
	if tagged || status != http.StatusOK {
		t.Errorf("non-workspace token: tagged=%v status=%d", tagged, status)
	}
}

func TestLightRunIDIgnoresAnUnsafeValue(t *testing.T) {
	for _, v := range []string{"has space", "a\tb", strings.Repeat("a", 129)} {
		// No DB expectation: an unsafe value must not even reach the lookup.
		if _, _, tagged, status := runLightRunID(t, map[string]string{"X-Molecule-Run-Id": v, "Authorization": "Bearer t"}, nil); tagged || status != http.StatusOK {
			t.Errorf("%q: tagged=%v status=%d", v, tagged, status)
		}
	}
}

func TestLightRunIDNeverFailsARequest(t *testing.T) {
	_, _, tagged, status := runLightRunID(t, map[string]string{"X-Molecule-Run-Id": "light-x", "Authorization": "Bearer t"},
		func(mock sqlmock.Sqlmock) {
			mock.ExpectQuery(`SELECT t\.id, t\.workspace_id`).WillReturnError(errors.New("db down"))
		})
	if tagged || status != http.StatusOK {
		t.Errorf("lookup error: tagged=%v status=%d, want an untagged 200", tagged, status)
	}
}

func TestValidLightRunID(t *testing.T) {
	for v, want := range map[string]bool{
		"light-be7540cfd160-1a2b3c4d-m1763000000": true,
		"light-gate-1":           true,
		"":                       false,
		"a b":                    false,
		"a\r\nX-Evil: 1":         false,
		strings.Repeat("a", 128): true,
		strings.Repeat("a", 129): false,
	} {
		if got := ValidLightRunID(v); got != want {
			t.Errorf("ValidLightRunID(%q) = %v, want %v", v, got, want)
		}
	}
}
