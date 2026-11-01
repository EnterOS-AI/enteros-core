package handlers

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/crypto"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

const continuousTestWS = "00000000-0000-0000-0000-0000000000c1"

const (
	continuousExistsSQL    = `SELECT 1 FROM workspaces WHERE id = \$1`
	continuousGlobalSQL    = `SELECT encrypted_value, encryption_version FROM global_secrets WHERE key = \$1`
	continuousWorkspaceSQL = `SELECT encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = \$1 AND key = \$2`
	continuousDeleteSQL    = `DELETE FROM workspace_secrets WHERE workspace_id = \$1 AND key = \$2`
)

func encryptedRow(t *testing.T, plain string) *sqlmock.Rows {
	t.Helper()
	enc, err := crypto.Encrypt([]byte(plain))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	return sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}).
		AddRow(enc, crypto.CurrentEncryptionVersion())
}

// encryptedAs matches an encrypted_value argument whose plaintext is the string.
type encryptedAs string

func (want encryptedAs) Match(v driver.Value) bool {
	enc, ok := v.([]byte)
	if !ok {
		return false
	}
	plain, err := crypto.DecryptVersioned(enc, crypto.CurrentEncryptionVersion())
	return err == nil && string(plain) == string(want)
}

func expectContinuousWorkspace(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(continuousExistsSQL).WithArgs(continuousTestWS).
		WillReturnRows(sqlmock.NewRows([]string{"one"}).AddRow(1))
}

// expectContinuousRows queues the global read, then the workspace read; ""
// means no row.
func expectContinuousRows(t *testing.T, mock sqlmock.Sqlmock, global, workspace string) {
	t.Helper()
	g := mock.ExpectQuery(continuousGlobalSQL).WithArgs(continuousSecretKey)
	if global == "" {
		g.WillReturnError(sql.ErrNoRows)
	} else {
		g.WillReturnRows(encryptedRow(t, global))
	}
	w := mock.ExpectQuery(continuousWorkspaceSQL).WithArgs(continuousTestWS, continuousSecretKey)
	if workspace == "" {
		w.WillReturnError(sql.ErrNoRows)
	} else {
		w.WillReturnRows(encryptedRow(t, workspace))
	}
}

func expectContinuousKind(mock sqlmock.Sqlmock, kind string) {
	mock.ExpectQuery(`SELECT COALESCE\(kind`).WithArgs(continuousTestWS).
		WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow(kind))
}

func expectContinuousUpsert(mock sqlmock.Sqlmock, plain string) {
	mock.ExpectExec(`INSERT INTO workspace_secrets`).
		WithArgs(continuousTestWS, continuousSecretKey, encryptedAs(plain), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
}

func expectContinuousDelete(mock sqlmock.Sqlmock) {
	mock.ExpectExec(continuousDeleteSQL).WithArgs(continuousTestWS, continuousSecretKey).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

func continuousRequest(method, body string) (*httptest.ResponseRecorder, *gin.Context) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: continuousTestWS}}
	c.Request = httptest.NewRequest(method, "/workspaces/"+continuousTestWS+"/continuous", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return w, c
}

func decodeContinuous(t *testing.T, w *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v (%s)", err, w.Body.String())
	}
	return resp
}

func restartRecorder() (chan string, *ContinuousHandler) {
	restarted := make(chan string, 1)
	return restarted, NewContinuousHandler(func(id string) { restarted <- id })
}

func expectRestart(t *testing.T, restarted chan string) {
	t.Helper()
	select {
	case id := <-restarted:
		if id != continuousTestWS {
			t.Errorf("restart called with %s", id)
		}
	case <-time.After(500 * time.Millisecond):
		t.Error("restart was not triggered")
	}
}

func expectNoRestart(t *testing.T, restarted chan string) {
	t.Helper()
	select {
	case id := <-restarted:
		t.Errorf("restart must not fire (restarted %s)", id)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestContinuousFromValue_MatchesRuntime(t *testing.T) {
	// molecule-ai-workspace-runtime workspace_mode.py: only an explicitly
	// falsy value turns it off.
	for _, v := range []string{"0", "false", "no", "off", "FALSE", " Off "} {
		if continuousFromValue(v) {
			t.Errorf("%q must read as off", v)
		}
	}
	for _, v := range []string{"", "1", "true", "yes", "on", "anything"} {
		if !continuousFromValue(v) {
			t.Errorf("%q must read as on", v)
		}
	}
}

func TestContinuousGet(t *testing.T) {
	cases := []struct {
		name, global, workspace string
		wantOn                  bool
		wantSource              string
	}{
		{"default is on", "", "", true, "default"},
		{"workspace row turns it off", "", "false", false, "workspace_secrets"},
		{"global row turns it off", "false", "", false, "global_secrets"},
		{"workspace row beats global row", "false", "true", true, "workspace_secrets"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := setupTestDB(t)
			setupTestRedis(t)
			expectContinuousWorkspace(mock)
			expectContinuousRows(t, mock, tc.global, tc.workspace)

			w, c := continuousRequest("GET", "")
			NewContinuousHandler(nil).Get(c)

			resp := decodeContinuous(t, w)
			if w.Code != http.StatusOK || resp["continuous"] != tc.wantOn || resp["source"] != tc.wantSource {
				t.Errorf("expected 200 continuous=%v source=%s, got %d %v", tc.wantOn, tc.wantSource, w.Code, resp)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet sqlmock expectations: %v", err)
			}
		})
	}
}

func TestContinuousSet_WritesTheRowThatYieldsTheValue(t *testing.T) {
	cases := []struct {
		name, global, workspace string
		want                    bool
		upsert                  string // plaintext written; "" means the row is deleted
		wantSource              string
	}{
		{"off from default writes false", "", "", false, "false", "workspace_secrets"},
		{"on over a workspace off deletes the row", "", "false", true, "", "default"},
		{"on over a global off writes true", "false", "", true, "true", "workspace_secrets"},
		{"off over a workspace on under a global off deletes the row", "false", "true", false, "", "global_secrets"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := setupTestDB(t)
			setupTestRedis(t)
			restarted, handler := restartRecorder()
			expectContinuousWorkspace(mock)
			expectContinuousRows(t, mock, tc.global, tc.workspace)
			if tc.upsert != "" {
				expectContinuousUpsert(mock, tc.upsert)
			} else {
				expectContinuousDelete(mock)
			}
			expectContinuousKind(mock, "workspace")

			body := `{"continuous":false}`
			if tc.want {
				body = `{"continuous":true}`
			}
			w, c := continuousRequest("PUT", body)
			handler.Set(c)

			resp := decodeContinuous(t, w)
			if w.Code != http.StatusOK || resp["status"] != "saved" || resp["source"] != tc.wantSource || resp["restart"] != "started" {
				t.Fatalf("expected 200 saved source=%s restart=started, got %d %v", tc.wantSource, w.Code, resp)
			}
			expectRestart(t, restarted)
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet sqlmock expectations: %v", err)
			}
		})
	}
}

func TestContinuousSet_UnchangedWritesAndRestartsNothing(t *testing.T) {
	cases := []struct {
		name, global, workspace, body string
	}{
		{"on by default", "", "", `{"continuous":true}`},
		{"off through the global row", "false", "", `{"continuous":false}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := setupTestDB(t)
			setupTestRedis(t)
			restarted, handler := restartRecorder()
			// No Exec is queued: sqlmock fails any write the handler issues.
			expectContinuousWorkspace(mock)
			expectContinuousRows(t, mock, tc.global, tc.workspace)

			w, c := continuousRequest("PUT", tc.body)
			handler.Set(c)

			if w.Code != http.StatusOK || decodeContinuous(t, w)["status"] != "unchanged" {
				t.Fatalf("expected 200 unchanged, got %d: %s", w.Code, w.Body.String())
			}
			expectNoRestart(t, restarted)
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet sqlmock expectations: %v", err)
			}
		})
	}
}

// A change on the org's platform root is saved but does not restart it
// (core#2573): that restart re-provisions the org root.
func TestContinuousSet_PlatformRootSavesWithoutRestart(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	restarted, handler := restartRecorder()
	expectContinuousWorkspace(mock)
	expectContinuousRows(t, mock, "", "")
	expectContinuousUpsert(mock, "false")
	expectContinuousKind(mock, "platform")

	w, c := continuousRequest("PUT", `{"continuous":false}`)
	handler.Set(c)

	resp := decodeContinuous(t, w)
	if w.Code != http.StatusOK || resp["status"] != "saved" || resp["restart"] != "skipped" {
		t.Fatalf("expected 200 saved restart=skipped, got %d %v", w.Code, resp)
	}
	expectNoRestart(t, restarted)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// A workspace writing its own switch is not restarted mid-turn (core#2573).
func TestContinuousSet_SelfWriteSavesWithoutRestart(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	restarted, handler := restartRecorder()
	expectContinuousWorkspace(mock)
	expectContinuousRows(t, mock, "", "")
	expectContinuousUpsert(mock, "false")

	w, c := continuousRequest("PUT", `{"continuous":false}`)
	c.Request.Header.Set("X-Workspace-ID", continuousTestWS)
	handler.Set(c)

	resp := decodeContinuous(t, w)
	if w.Code != http.StatusOK || resp["status"] != "saved" || resp["restart"] != "skipped" {
		t.Fatalf("expected 200 saved restart=skipped, got %d %v", w.Code, resp)
	}
	expectNoRestart(t, restarted)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// The concierge's management MCP uses the tenant admin token; it may not write
// its own platform root (core#2566).
func TestContinuousSet_AdminTokenOnPlatformRootIsRefused(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	restarted, handler := restartRecorder()
	expectContinuousWorkspace(mock)
	expectContinuousKind(mock, "platform")

	w, c := continuousRequest("PUT", `{"continuous":false}`)
	c.Set("caller_is_admin_token", true)
	handler.Set(c)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
	expectNoRestart(t, restarted)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestContinuous_UnknownWorkspaceIs404(t *testing.T) {
	for _, method := range []string{"GET", "PUT"} {
		t.Run(method, func(t *testing.T) {
			mock := setupTestDB(t)
			setupTestRedis(t)
			mock.ExpectQuery(continuousExistsSQL).WithArgs(continuousTestWS).WillReturnError(sql.ErrNoRows)

			w, c := continuousRequest(method, `{"continuous":false}`)
			handler := NewContinuousHandler(nil)
			if method == "GET" {
				handler.Get(c)
			} else {
				handler.Set(c)
			}

			if w.Code != http.StatusNotFound {
				t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet sqlmock expectations: %v", err)
			}
		})
	}
}

func TestContinuousSet_RejectsMissingValue(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	for _, body := range []string{`{}`, `{"continuous":null}`, `not json`} {
		w, c := continuousRequest("PUT", body)
		NewContinuousHandler(nil).Set(c)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body %q: expected 400, got %d", body, w.Code)
		}
	}
}

func TestContinuous_InvalidID(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	for _, fn := range []func(*ContinuousHandler, *gin.Context){(*ContinuousHandler).Get, (*ContinuousHandler).Set} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Params = gin.Params{{Key: "id", Value: "not-a-uuid"}}
		c.Request = httptest.NewRequest("PUT", "/workspaces/not-a-uuid/continuous", strings.NewReader(`{"continuous":false}`))
		c.Request.Header.Set("Content-Type", "application/json")
		fn(NewContinuousHandler(nil), c)
		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400 for bad UUID, got %d", w.Code)
		}
	}
}
