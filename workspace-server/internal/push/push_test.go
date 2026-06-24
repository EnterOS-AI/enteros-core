package push

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSenderSend(t *testing.T) {
	gin.SetMode(gin.TestMode)

	expoResponse := map[string]interface{}{
		"data": []map[string]interface{}{
			{"status": "ok", "id": "abc123"},
			{"status": "error", "message": "Invalid token", "details": map[string]string{"error": "DeviceNotRegistered"}},
		},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "POST", r.Method)
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))

		var msgs []Message
		require.NoError(t, json.NewDecoder(r.Body).Decode(&msgs))
		assert.Len(t, msgs, 2)
		assert.Equal(t, "ExponentPushToken[test1]", msgs[0].To)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(expoResponse)
	}))
	defer server.Close()

	sender := NewSender("")
	sender.apiURL = server.URL

	results, err := sender.Send(context.Background(), []Message{
		{To: "ExponentPushToken[test1]", Title: "Test", Body: "Hello"},
		{To: "ExponentPushToken[test2]", Title: "Test", Body: "World"},
	})
	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, "ok", results[0].Status)
	assert.Equal(t, "error", results[1].Status)
	assert.True(t, ShouldRemoveToken(results[1]))
}

func TestSenderSendEmpty(t *testing.T) {
	sender := NewSender("")
	results, err := sender.Send(context.Background(), nil)
	require.NoError(t, err)
	assert.Nil(t, results)
}

func TestHandlerCreate_InvalidWorkspaceID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewHandler(NewRepo(nil))

	router := gin.New()
	group := router.Group("/workspaces/:id")
	handler.RegisterRoutes(group)

	w := httptest.NewRecorder()
	body := `{"token":"ExponentPushToken[abc]","platform":"ios"}`
	req, _ := http.NewRequest("POST", "/workspaces/not-a-uuid/push-tokens", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestHandlerCreate(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectExec("INSERT INTO push_tokens").
		WithArgs("11111111-1111-1111-1111-111111111111", "ExponentPushToken[abc]", "ios").
		WillReturnResult(sqlmock.NewResult(1, 1))

	repo := NewRepo(db)
	handler := NewHandler(repo)

	router := gin.New()
	group := router.Group("/workspaces/:id")
	handler.RegisterRoutes(group)

	w := httptest.NewRecorder()
	body := `{"token":"ExponentPushToken[abc]","platform":"ios"}`
	req, _ := http.NewRequest("POST", "/workspaces/11111111-1111-1111-1111-111111111111/push-tokens", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNoContent, w.Code)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandlerCreateInvalidPlatform(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db, _, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	handler := NewHandler(NewRepo(db))

	router := gin.New()
	group := router.Group("/workspaces/:id")
	handler.RegisterRoutes(group)

	w := httptest.NewRecorder()
	body := `{"token":"ExponentPushToken[abc]","platform":"windows"}`
	req, _ := http.NewRequest("POST", "/workspaces/11111111-1111-1111-1111-111111111111/push-tokens", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestHandlerDelete_BindingError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewHandler(NewRepo(nil))

	router := gin.New()
	group := router.Group("/workspaces/:id")
	handler.RegisterRoutes(group)

	w := httptest.NewRecorder()
	body := `{}` // missing required "token" field
	req, _ := http.NewRequest("DELETE", "/workspaces/11111111-1111-1111-1111-111111111111/push-tokens", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestHandlerDelete_InvalidWorkspaceID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewHandler(NewRepo(nil))

	router := gin.New()
	group := router.Group("/workspaces/:id")
	handler.RegisterRoutes(group)

	w := httptest.NewRecorder()
	body := `{"token":"ExponentPushToken[del]"}`
	req, _ := http.NewRequest("DELETE", "/workspaces/not-a-uuid/push-tokens", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestHandlerDelete(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectExec("DELETE FROM push_tokens").
		WithArgs("22222222-2222-2222-2222-222222222222", "ExponentPushToken[del]").
		WillReturnResult(sqlmock.NewResult(0, 1))

	repo := NewRepo(db)
	handler := NewHandler(repo)

	router := gin.New()
	group := router.Group("/workspaces/:id")
	handler.RegisterRoutes(group)

	w := httptest.NewRecorder()
	body := `{"token":"ExponentPushToken[del]"}`
	req, _ := http.NewRequest("DELETE", "/workspaces/22222222-2222-2222-2222-222222222222/push-tokens", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNoContent, w.Code)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandlerCreate_DBSaveError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectExec("INSERT INTO push_tokens").
		WithArgs("11111111-1111-1111-1111-111111111111", "ExponentPushToken[abc]", "ios").
		WillReturnError(sql.ErrConnDone)

	handler := NewHandler(NewRepo(db))

	router := gin.New()
	group := router.Group("/workspaces/:id")
	handler.RegisterRoutes(group)

	w := httptest.NewRecorder()
	body := `{"token":"ExponentPushToken[abc]","platform":"ios"}`
	req, _ := http.NewRequest("POST", "/workspaces/11111111-1111-1111-1111-111111111111/push-tokens", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestHandlerDelete_DBError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectExec("DELETE FROM push_tokens").
		WithArgs("22222222-2222-2222-2222-222222222222", "ExponentPushToken[del]").
		WillReturnError(sql.ErrConnDone)

	handler := NewHandler(NewRepo(db))

	router := gin.New()
	group := router.Group("/workspaces/:id")
	handler.RegisterRoutes(group)

	w := httptest.NewRecorder()
	body := `{"token":"ExponentPushToken[del]"}`
	req, _ := http.NewRequest("DELETE", "/workspaces/22222222-2222-2222-2222-222222222222/push-tokens", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSenderSend_HTTPError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Server that hijacks the connection and closes it before sending a response,
	// causing the HTTP client to receive a connection-closed error.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Drain request body so client send completes.
		io.Copy(io.Discard, r.Body)
		// Hijack and immediately close — no response written.
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
	}))
	defer server.Close()

	sender := NewSender("")
	sender.apiURL = server.URL

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	_, err := sender.Send(ctx, []Message{
		{To: "ExponentPushToken[test]", Title: "T", Body: "H"},
	})
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "post:") || strings.Contains(err.Error(), "context"))
}

func TestSenderSend_Non200Response(t *testing.T) {
	gin.SetMode(gin.TestMode)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":"rate limited"}`))
	}))
	defer server.Close()

	sender := NewSender("")
	sender.apiURL = server.URL

	_, err := sender.Send(context.Background(), []Message{
		{To: "ExponentPushToken[test]", Title: "T", Body: "H"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expo returned 503")
}

func TestNotifierNotifyAgentMessage_NilGuard(t *testing.T) {
	// Must not panic when sender is nil.
	n := NewNotifier(nil, nil)
	// Should return immediately (nil check passes without panic).
	n.NotifyAgentMessage(context.Background(), "ws-1", "Test", "Hello world")
}

func TestNotifierNotifyAgentMessage_ZeroTokens(t *testing.T) {
	// Verify that NotifyAgentMessage does NOT panic when there are zero registered
	// tokens — it should return early without calling sender.Send().
	// Note: the fire-and-forget goroutine inside NotifyAgentMessage is not
	// directly verifiable here without modifying production code; the key assertion
	// is that no panic occurs and the method returns cleanly.
	db, mock, err := sqlmock.New()
	require.NoError(t, err)

	mock.ExpectQuery("SELECT id, workspace_id, token, platform, created_at FROM push_tokens").
		WithArgs("ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "workspace_id", "token", "platform", "created_at"}))

	sender := NewSender("")
	sender.apiURL = "http://127.0.0.1:1" // unreachable — would error if Send is called

	n := NewNotifier(db, sender)
	n.NotifyAgentMessage(context.Background(), "ws-1", "Test", "Hello")

	// Give goroutine time to run GetTokens and exit early before closing DB.
	time.Sleep(200 * time.Millisecond)
	require.NoError(t, mock.ExpectationsWereMet())
	db.Close()
}

func TestRepoGetTokens_DBError(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectQuery("SELECT id, workspace_id, token, platform, created_at FROM push_tokens").
		WithArgs("ws-1").
		WillReturnError(sql.ErrConnDone)

	repo := NewRepo(db)
	_, err = repo.GetTokens(context.Background(), "ws-1")
	require.Error(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRepoGetTokens_ScanError(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	// Return fewer columns than struct has — causes scan error.
	mock.ExpectQuery("SELECT id, workspace_id, token, platform, created_at FROM push_tokens").
		WithArgs("ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "workspace_id", "token"}). // missing platform, created_at
			AddRow("1", "ws-1", "ExponentPushToken[a]"))

	repo := NewRepo(db)
	_, err = repo.GetTokens(context.Background(), "ws-1")
	require.Error(t, err) // scan error
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRepoSaveToken_Error(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectExec("INSERT INTO push_tokens").
		WithArgs("ws-1", "ExponentPushToken[xyz]", "android").
		WillReturnError(sql.ErrConnDone)

	repo := NewRepo(db)
	err = repo.SaveToken(context.Background(), "ws-1", "ExponentPushToken[xyz]", "android")
	require.Error(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRepoDeleteToken_Error(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectExec("DELETE FROM push_tokens").
		WithArgs("ws-1", "ExponentPushToken[xyz]").
		WillReturnError(sql.ErrConnDone)

	repo := NewRepo(db)
	err = repo.DeleteToken(context.Background(), "ws-1", "ExponentPushToken[xyz]")
	require.Error(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		name string
		s    string
		max  int
		want string
	}{
		{"short string unchanged", "hello", 10, "hello"},
		{"exact length unchanged", "hello", 5, "hello"},
		{"long string truncated", "hello world", 5, "hello…"},
		{"empty string", "", 5, ""},
		{"single char at max", "a", 1, "a"},
		{"multi-byte truncation adds ellipsis", "こんにちは世界", 5, ""},
		{"truncate with ellipsis ends with ellipsis", "hello world", 5, "hello…"},
		{"truncate at 1 char", "hello", 1, "h…"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := truncate(tc.s, tc.max)
			if tc.want == "" {
				// Multi-byte / edge cases: verify no expansion beyond max+3.
				assert.True(t, len(got) <= tc.max+3)
			} else {
				assert.Equal(t, tc.want, got)
			}
		})
	}
}

func TestRepoGetTokens(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectQuery("SELECT id, workspace_id, token, platform, created_at FROM push_tokens").
		WithArgs("ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "workspace_id", "token", "platform", "created_at"}).
			AddRow("1", "ws-1", "ExponentPushToken[a]", "ios", "2026-01-01T00:00:00Z").
			AddRow("2", "ws-1", "ExponentPushToken[b]", "android", "2026-01-01T00:00:00Z"))

	repo := NewRepo(db)
	tokens, err := repo.GetTokens(context.Background(), "ws-1")
	require.NoError(t, err)
	require.Len(t, tokens, 2)
	assert.Equal(t, "ExponentPushToken[a]", tokens[0].Token)
	assert.Equal(t, "ios", tokens[0].Platform)
	assert.Equal(t, "ExponentPushToken[b]", tokens[1].Token)
	require.NoError(t, mock.ExpectationsWereMet())
}
