package handlers

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/ws"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// newSocketHandlerWithDB creates a SocketHandler with buffered Hub channels.
// The DB is set up via setupTestDB (called before this function in each test).
func newSocketHandlerWithDB(t *testing.T, hub *ws.Hub) *SocketHandler {
	t.Helper()
	if hub == nil {
		hub = &ws.Hub{
			Register:   make(chan *ws.Client, 1),
			Unregister: make(chan *ws.Client, 1),
		}
	}
	return NewSocketHandler(hub)
}

// socketRequest builds a test request for the WebSocket connect endpoint.
func socketRequest(method, path, workspaceID, authHeader string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	if workspaceID != "" {
		req.Header.Set("X-Workspace-ID", workspaceID)
	}
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	return req
}

// ─────────────────────────────────────────────────────────────────────────────
// Auth gate: workspace connections always require a bearer
// ─────────────────────────────────────────────────────────────────────────────

func TestSocketHandler_Workspace_MissingBearer_Returns401WithoutDBProbe(t *testing.T) {
	mock := setupTestDB(t)
	handler := newSocketHandlerWithDB(t, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = socketRequest("GET", "/ws", "ws-missing-bearer", "")

	handler.HandleConnect(c)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("missing bearer: expected 401, got %d", w.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Auth gate: invalid workspace bearer → 401
// ─────────────────────────────────────────────────────────────────────────────

// ─────────────────────────────────────────────────────────────────────────────
// Auth gate: workspace HAS live token, invalid Bearer → 401
// ─────────────────────────────────────────────────────────────────────────────

func TestSocketHandler_AuthGate_HasLiveToken_InvalidBearer_Returns401(t *testing.T) {
	mock := setupTestDB(t)
	handler := newSocketHandlerWithDB(t, nil)

	wsID := "ws-invalid-token"
	badToken := "not-a-valid-token"

	// ValidateToken: lookupTokenByHash returns ErrNoRows for an unknown token.
	// Any token hash is fine since the token doesn't exist — use AnyArg.
	mock.ExpectQuery(`SELECT t\.id, t\.workspace_id.*FROM workspace_auth_tokens t.*JOIN`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = socketRequest("GET", "/ws", wsID, "Bearer "+badToken)

	handler.HandleConnect(c)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("hasLive but invalid bearer: expected 401, got %d", w.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Auth gate: workspace HAS live token, VALID Bearer → upgrade attempted.
// The WebSocket upgrade itself will fail in httptest (gorilla/websocket
// cannot write a real HTTP/1.1 handshake to httptest.ResponseRecorder), but
// the auth gate is passed so we verify no 401/500 was returned before the
// upgrade failure. This is the workspace-agent success path.
// ─────────────────────────────────────────────────────────────────────────────

func TestSocketHandler_AuthGate_HasLiveToken_ValidBearer_AuthPassed(t *testing.T) {
	mock := setupTestDB(t)
	handler := newSocketHandlerWithDB(t, nil)

	wsID := "ws-valid-token"
	goodToken := "valid-ws-token-123"

	// ValidateToken: token found and workspace is not removed.
	// sha256TokenHash returns []byte; rational matcher compares as string.
	mock.ExpectQuery(`SELECT t\.id, t\.workspace_id.*FROM workspace_auth_tokens t.*JOIN`).
		WithArgs(sha256TokenHash(goodToken)).
		WillReturnRows(sqlmock.NewRows([]string{"token_id", "workspace_id"}).
			AddRow("tok-abc", wsID))
	mock.ExpectExec(`UPDATE workspace_auth_tokens SET last_used_at`).
		WithArgs("tok-abc").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = socketRequest("GET", "/ws", wsID, "Bearer "+goodToken)

	handler.HandleConnect(c)

	// The WebSocket upgrade fails in httptest (httptest.ResponseRecorder is not
	// a real TCP connection), but the auth gate itself succeeded — we should
	// NOT see a 401 or 500 response code. The actual code depends on the
	// upgrade error handling; the critical assertion is that auth passed.
	if w.Code == http.StatusUnauthorized || w.Code == http.StatusInternalServerError {
		t.Errorf("valid token: auth should have passed; got %d", w.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Canvas client (no X-Workspace-ID): the global stream requires privileged
// authentication before the upgrade.
// ─────────────────────────────────────────────────────────────────────────────

func TestSocketHandler_CanvasClient_Anonymous_Returns401(t *testing.T) {
	mock := setupTestDB(t)
	handler := newSocketHandlerWithDB(t, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = socketRequest("GET", "/ws", "", "")

	handler.HandleConnect(c)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("anonymous canvas client: expected 401, got %d", w.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

func TestSocketHandler_CanvasClient_AdminSubprotocol_AuthPassed(t *testing.T) {
	setupTestDB(t)
	t.Setenv("ADMIN_TOKEN", "canvas-admin-secret")
	handler := newSocketHandlerWithDB(t, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = socketRequest("GET", "/ws", "", "")
	c.Request.Header.Set("Sec-WebSocket-Protocol", websocketAuthProtocol("canvas-admin-secret"))

	handler.HandleConnect(c)

	if w.Code == http.StatusUnauthorized || w.Code == http.StatusInternalServerError {
		t.Errorf("admin subprotocol: auth should have passed; got %d", w.Code)
	}
}

func TestSocketHandler_CanvasClient_AdminSubprotocol_RealUpgrade(t *testing.T) {
	setupTestDB(t)
	t.Setenv("ADMIN_TOKEN", "canvas-admin-secret")
	t.Setenv("CORS_ORIGINS", "")
	hub := ws.NewHub(nil)
	go hub.Run()
	t.Cleanup(hub.Close)

	router := gin.New()
	router.GET("/ws", NewSocketHandler(hub).HandleConnect)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	dialer := websocket.Dialer{
		Subprotocols: []string{
			websocketAuthProtocol("canvas-admin-secret"),
			websocketProtocolSentinel,
		},
	}
	conn, response, err := dialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/ws", nil)
	if err != nil {
		if response != nil {
			t.Fatalf("authenticated WebSocket upgrade failed: %v (status=%d)", err, response.StatusCode)
		}
		t.Fatalf("authenticated WebSocket upgrade failed: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if got := conn.Subprotocol(); got != websocketProtocolSentinel {
		t.Fatalf("selected subprotocol = %q, want non-secret sentinel %q", got, websocketProtocolSentinel)
	}
	if got := response.Header.Get("Sec-WebSocket-Protocol"); got != websocketProtocolSentinel {
		t.Fatalf("handshake Sec-WebSocket-Protocol = %q, want %q", got, websocketProtocolSentinel)
	}
	if strings.Contains(response.Header.Get("Sec-WebSocket-Protocol"), websocketAuthProtocolPrefix) {
		t.Fatal("handshake echoed the credential-bearing subprotocol")
	}
}

func TestSocketHandler_CanvasClient_VerifiedCPSession_AuthPassed(t *testing.T) {
	setupTestDB(t)
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("slug"); got != "socket-test-tenant" {
			t.Errorf("tenant slug = %q, want socket-test-tenant", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"member":true,"user_id":"user-socket-test"}`))
	}))
	t.Cleanup(cp.Close)
	t.Setenv("CP_UPSTREAM_URL", cp.URL)
	t.Setenv("MOLECULE_ORG_SLUG", "socket-test-tenant")
	handler := newSocketHandlerWithDB(t, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = socketRequest("GET", "/ws", "", "")
	c.Request.Header.Set("Cookie", "session=socket-test-unique")

	handler.HandleConnect(c)

	if w.Code == http.StatusUnauthorized || w.Code == http.StatusInternalServerError {
		t.Errorf("verified CP session: auth should have passed; got %d", w.Code)
	}
}

func TestSocketHandler_CanvasClient_OrgToken_AuthPassed(t *testing.T) {
	mock := setupTestDB(t)
	handler := newSocketHandlerWithDB(t, nil)
	orgToken := "mole_org_socket_test"

	mock.ExpectQuery(`SELECT id, prefix, org_id, expires_at FROM org_api_tokens`).
		WithArgs(sha256TokenHash(orgToken)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "prefix", "org_id", "expires_at"}).
			AddRow("org-token-id", "mole_org", "org-id", nil))
	mock.ExpectExec(`UPDATE org_api_tokens SET last_used_at`).
		WithArgs("org-token-id").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = socketRequest("GET", "/ws", "", "Bearer "+orgToken)

	handler.HandleConnect(c)

	if w.Code == http.StatusUnauthorized || w.Code == http.StatusInternalServerError {
		t.Errorf("org token: auth should have passed; got %d", w.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

func TestSocketHandler_CanvasClient_WorkspaceBearerCannotOpenGlobalStream(t *testing.T) {
	mock := setupTestDB(t)
	handler := newSocketHandlerWithDB(t, nil)
	workspaceToken := "valid-but-workspace-scoped"

	mock.ExpectQuery(`SELECT id, prefix, org_id, expires_at FROM org_api_tokens`).
		WithArgs(sha256TokenHash(workspaceToken)).
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = socketRequest("GET", "/ws", "", "Bearer "+workspaceToken)

	handler.HandleConnect(c)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("workspace bearer on global stream: expected 401, got %d", w.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

func TestSocketHandler_CanvasClient_MalformedAuthSubprotocol_Returns401(t *testing.T) {
	setupTestDB(t)
	t.Setenv("ADMIN_TOKEN", "canvas-admin-secret")
	handler := newSocketHandlerWithDB(t, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = socketRequest("GET", "/ws", "", "")
	c.Request.Header.Set("Sec-WebSocket-Protocol", websocketAuthProtocolPrefix+"not-hex")

	handler.HandleConnect(c)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("malformed auth protocol: expected 401, got %d", w.Code)
	}
}

func TestSocketHandler_CanvasClient_ConflictingCredentials_Returns401(t *testing.T) {
	setupTestDB(t)
	t.Setenv("ADMIN_TOKEN", "header-admin-secret")
	handler := newSocketHandlerWithDB(t, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = socketRequest("GET", "/ws", "", "Bearer header-admin-secret")
	c.Request.Header.Set("Sec-WebSocket-Protocol", websocketAuthProtocol("different-secret"))

	handler.HandleConnect(c)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("conflicting credentials: expected 401, got %d", w.Code)
	}
}

func TestSocketHandler_CanvasClient_DuplicateAuthProtocols_Returns401(t *testing.T) {
	setupTestDB(t)
	t.Setenv("ADMIN_TOKEN", "canvas-admin-secret")
	handler := newSocketHandlerWithDB(t, nil)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = socketRequest("GET", "/ws", "", "")
	c.Request.Header.Add("Sec-WebSocket-Protocol", websocketAuthProtocol("canvas-admin-secret"))
	c.Request.Header.Add("Sec-WebSocket-Protocol", websocketAuthProtocol("canvas-admin-secret"))

	handler.HandleConnect(c)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("duplicate auth protocols: expected 401, got %d", w.Code)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Malformed bearer values never trigger a legacy/pre-token bypass.
// ─────────────────────────────────────────────────────────────────────────────

func TestSocketHandler_AuthGate_HasLiveToken_EmptyBearer_Returns401(t *testing.T) {
	setupTestDB(t)
	handler := newSocketHandlerWithDB(t, nil)

	wsID := "ws-has-live-token-empty-bearer"

	// Authorization header is "Bearer " (empty token after "Bearer ").
	// wsauth.BearerTokenFromHeader strips "Bearer " and gets "".
	// ValidateToken is called with "" → returns ErrInvalidToken before DB hit.
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = socketRequest("GET", "/ws", wsID, "Bearer ")

	handler.HandleConnect(c)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("empty bearer after Bearer prefix: expected 401, got %d", w.Code)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// helpers
// ─────────────────────────────────────────────────────────────────────────────

// sha256TokenHash returns the SHA256 hash of a plaintext token, matching what
// wsauth.ValidateToken does internally before querying the DB.
func sha256TokenHash(plaintext string) []byte {
	h := sha256.Sum256([]byte(plaintext))
	return h[:]
}

func websocketAuthProtocol(plaintext string) string {
	return websocketAuthProtocolPrefix + hex.EncodeToString([]byte(plaintext))
}
