package handlers

import (
	"database/sql"
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

const continuousReadSQL = `SELECT encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = \$1 AND key = \$2`

func continuousRowOff(t *testing.T) *sqlmock.Rows {
	t.Helper()
	enc, err := crypto.Encrypt([]byte("false"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	return sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}).
		AddRow(enc, crypto.CurrentEncryptionVersion())
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

func TestGetContinuous_DefaultIsOn(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	mock.ExpectQuery(continuousReadSQL).WithArgs(continuousTestWS, continuousSecretKey).WillReturnError(sql.ErrNoRows)

	w, c := continuousRequest("GET", "")
	NewSecretsHandler(nil).GetContinuous(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	resp := decodeContinuous(t, w)
	if resp["continuous"] != true || resp["source"] != "default" {
		t.Errorf("expected continuous=true source=default, got %v", resp)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestGetContinuous_RowTurnsItOff(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	mock.ExpectQuery(continuousReadSQL).WithArgs(continuousTestWS, continuousSecretKey).WillReturnRows(continuousRowOff(t))

	w, c := continuousRequest("GET", "")
	NewSecretsHandler(nil).GetContinuous(c)

	resp := decodeContinuous(t, w)
	if w.Code != http.StatusOK || resp["continuous"] != false || resp["source"] != "workspace_secrets" {
		t.Errorf("expected 200 continuous=false source=workspace_secrets, got %d %v", w.Code, resp)
	}
}

func TestSetContinuous_OffWritesRowAndRestarts(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	restarted := make(chan string, 1)
	handler := NewSecretsHandler(func(id string) { restarted <- id })

	mock.ExpectQuery(continuousReadSQL).WithArgs(continuousTestWS, continuousSecretKey).WillReturnError(sql.ErrNoRows)
	mock.ExpectExec(`INSERT INTO workspace_secrets`).
		WithArgs(continuousTestWS, continuousSecretKey, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	w, c := continuousRequest("PUT", `{"continuous":false}`)
	handler.SetContinuous(c)

	if w.Code != http.StatusOK || decodeContinuous(t, w)["status"] != "saved" {
		t.Fatalf("expected 200 saved, got %d: %s", w.Code, w.Body.String())
	}
	select {
	case id := <-restarted:
		if id != continuousTestWS {
			t.Errorf("restart called with %s", id)
		}
	case <-time.After(500 * time.Millisecond):
		t.Error("restart was not triggered")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestSetContinuous_OnDeletesRowAndRestarts(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	restarted := make(chan string, 1)
	handler := NewSecretsHandler(func(id string) { restarted <- id })

	mock.ExpectQuery(continuousReadSQL).WithArgs(continuousTestWS, continuousSecretKey).WillReturnRows(continuousRowOff(t))
	mock.ExpectExec(`DELETE FROM workspace_secrets WHERE workspace_id = \$1 AND key = \$2`).
		WithArgs(continuousTestWS, continuousSecretKey).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w, c := continuousRequest("PUT", `{"continuous":true}`)
	handler.SetContinuous(c)

	if w.Code != http.StatusOK || decodeContinuous(t, w)["status"] != "saved" {
		t.Fatalf("expected 200 saved, got %d: %s", w.Code, w.Body.String())
	}
	select {
	case <-restarted:
	case <-time.After(500 * time.Millisecond):
		t.Error("restart was not triggered")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestSetContinuous_UnchangedWritesAndRestartsNothing(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	restarted := make(chan string, 1)
	handler := NewSecretsHandler(func(id string) { restarted <- id })

	// Already on (no row); PUT on again. No Exec is expected — sqlmock fails
	// the test if the handler issues one.
	mock.ExpectQuery(continuousReadSQL).WithArgs(continuousTestWS, continuousSecretKey).WillReturnError(sql.ErrNoRows)

	w, c := continuousRequest("PUT", `{"continuous":true}`)
	handler.SetContinuous(c)

	if w.Code != http.StatusOK || decodeContinuous(t, w)["status"] != "unchanged" {
		t.Fatalf("expected 200 unchanged, got %d: %s", w.Code, w.Body.String())
	}
	select {
	case id := <-restarted:
		t.Errorf("an unchanged PUT must not restart (restarted %s)", id)
	case <-time.After(200 * time.Millisecond):
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestSetContinuous_RejectsMissingValue(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	for _, body := range []string{`{}`, `{"continuous":null}`, `not json`} {
		w, c := continuousRequest("PUT", body)
		NewSecretsHandler(nil).SetContinuous(c)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body %q: expected 400, got %d", body, w.Code)
		}
	}
}

func TestContinuous_InvalidID(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	for _, fn := range []func(*SecretsHandler, *gin.Context){(*SecretsHandler).GetContinuous, (*SecretsHandler).SetContinuous} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Params = gin.Params{{Key: "id", Value: "not-a-uuid"}}
		c.Request = httptest.NewRequest("PUT", "/workspaces/not-a-uuid/continuous", strings.NewReader(`{"continuous":false}`))
		c.Request.Header.Set("Content-Type", "application/json")
		fn(NewSecretsHandler(nil), c)
		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400 for bad UUID, got %d", w.Code)
		}
	}
}
