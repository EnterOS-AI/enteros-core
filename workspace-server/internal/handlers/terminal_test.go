package handlers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// stubNoLocalShell forces localShellCommand to report "no shell", so the
// docker-nil local path lands on a deterministic 503 ("no shell available")
// instead of attempting a WebSocket upgrade that can't complete in a plain
// httptest request. Tests that only want to assert ROUTING (reached the local
// branch, not remote/401/403) use this so the 503 stays a stable, meaningful
// signal after the handleLocalShellConnect change. Restored on cleanup.
func stubNoLocalShell(t *testing.T) {
	t.Helper()
	prev := localShellCommand
	localShellCommand = func() ([]string, error) { return nil, errNoShellForTest }
	t.Cleanup(func() { localShellCommand = prev })
}

var errNoShellForTest = fmt.Errorf("no shell (test stub)")

// TestHandleConnect_RoutesToRemote asserts HandleConnect picks the CP path
// when the workspace row carries an instance_id. The WS upgrade fails in
// a unit test (plain HTTP request, no ws handshake), but that's after the
// DB lookup — so unmet sqlmock expectations is the routing assertion.
func TestHandleConnect_RoutesToRemote(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	mock.ExpectQuery("SELECT COALESCE").
		WithArgs("ws-remote").
		WillReturnRows(sqlmock.NewRows([]string{"instance_id"}).AddRow("i-abc123"))

	h := NewTerminalHandler(nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-remote"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-remote/terminal", nil)

	h.HandleConnect(c)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations (router didn't hit CP branch): %v", err)
	}
}

// TestHandleConnect_RoutesToLocal asserts HandleConnect stays on the local
// Docker path when instance_id is empty.
func TestHandleConnect_RoutesToLocal(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	stubNoLocalShell(t)

	// DB: workspace row with NULL instance_id → COALESCE returns "".
	mock.ExpectQuery("SELECT COALESCE").
		WithArgs("ws-local").
		WillReturnRows(sqlmock.NewRows([]string{"instance_id"}).AddRow(""))

	// nil docker client: local path now routes to the local-shell handler
	// (in-container shell). With localShellCommand stubbed to "no shell", it
	// deterministically 503s — which still confirms we took the local branch.
	h := NewTerminalHandler(nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-local"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-local/terminal", nil)

	h.HandleConnect(c)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("local branch should 503 when Docker is unavailable; got %d", w.Code)
	}
}

// TestTerminalConnect_KI005_RejectsUnauthorizedCrossWorkspace tests the KI-005
// regression fix: workspace A must NOT be able to open a terminal on workspace B's
// container, even with a valid bearer token, unless they share a parent/child
// relationship. The vulnerability existed because HandleConnect only checked
// WorkspaceAuth (valid bearer → any :id) without the CanCommunicate hierarchy guard.
func TestTerminalConnect_KI005_RejectsUnauthorizedCrossWorkspace(t *testing.T) {
	mock := setupTestDB(t)
	// Stub CanCommunicate so it always returns false (no relationship).
	// Reset after test to avoid polluting other tests.
	prev := canCommunicateCheck
	canCommunicateCheck = func(callerID, targetID string) bool { return false }
	defer func() { canCommunicateCheck = prev }()

	// Token lookup: ws-caller's token is valid. ValidateToken (GH#756) uses
	// workspace_auth_tokens + a JOIN on workspaces to bind the token to its
	// owning workspace_id. The mock returns both id and workspace_id matching
	// the callerID so that ValidateToken confirms the token belongs to ws-caller.
	rows := sqlmock.NewRows([]string{"id", "workspace_id"}).AddRow("tok-1", "ws-caller")
	mock.ExpectQuery(`SELECT t\.id, t\.workspace_id\s+FROM workspace_auth_tokens t`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(rows)
	// ValidateToken fires a best-effort last_used_at UPDATE after
	// successful validation. Accept it so ExpectationsWereMet passes.
	mock.ExpectExec(`UPDATE workspace_auth_tokens SET last_used_at`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	h := NewTerminalHandler(nil) // nil docker → local path
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-target"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-target/terminal", nil)
	c.Request.Header.Set("X-Workspace-ID", "ws-caller")
	c.Request.Header.Set("Authorization", "Bearer valid-token-for-ws-caller")

	h.HandleConnect(c)

	if w.Code != http.StatusForbidden {
		t.Errorf("cross-workspace terminal: got %d, want 403 (%s)", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestKI005_SelfAccess_AlwaysAllowed — when callerID equals the target workspace
// ID the request always passes (self-access: workspace's own token reaches its
// own terminal without needing the hierarchy check).
func TestKI005_SelfAccess_AlwaysAllowed(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	stubNoLocalShell(t)

	mock.ExpectQuery("SELECT COALESCE").
		WithArgs("ws-self").
		WillReturnRows(sqlmock.NewRows([]string{"instance_id"}).AddRow(""))

	h := NewTerminalHandler(nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-self"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-self/terminal", nil)
	// Self-access: X-Workspace-ID matches the route param, no auth needed.
	c.Request.Header.Set("X-Workspace-ID", "ws-self")

	h.HandleConnect(c)

	// Self-access passes without any token check or CanCommunicate query.
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("self-access: expected 503 (Docker unavailable), got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestKI005_CanCommunicatePeer_Allowed — when the caller and target are siblings
// (share a parent), CanCommunicate returns true and the terminal access is granted.
func TestKI005_CanCommunicatePeer_Allowed(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	stubNoLocalShell(t)

	// DB: caller workspace row for token validation.
	mock.ExpectQuery("SELECT t.id, t.workspace_id").
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "workspace_id"}).
			AddRow("tok-caller", "ws-peer-a"))

	// DB: caller and target are siblings → CanCommunicate queries both.
	mock.ExpectQuery("SELECT id, parent_id FROM workspaces WHERE id").
		WithArgs("ws-peer-a").
		WillReturnRows(sqlmock.NewRows([]string{"id", "parent_id"}).
			AddRow("ws-peer-a", "org-lead"))
	mock.ExpectQuery("SELECT id, parent_id FROM workspaces WHERE id").
		WithArgs("ws-peer-b").
		WillReturnRows(sqlmock.NewRows([]string{"id", "parent_id"}).
			AddRow("ws-peer-b", "org-lead"))

	// DB: target workspace has no instance_id → local Docker path.
	mock.ExpectQuery("SELECT COALESCE").
		WithArgs("ws-peer-b").
		WillReturnRows(sqlmock.NewRows([]string{"instance_id"}).AddRow(""))

	h := NewTerminalHandler(nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-peer-b"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-peer-b/terminal", nil)
	c.Request.Header.Set("X-Workspace-ID", "ws-peer-a")
	c.Request.Header.Set("Authorization", "Bearer peer-token")

	h.HandleConnect(c)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("peer access: expected 503 (Docker unavailable), got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestKI005_CanCommunicateNonPeer_Forbidden — when caller and target have
// different parents (not siblings, not root-level), CanCommunicate returns
// false and the terminal access is blocked with 403.
func TestKI005_CanCommunicateNonPeer_Forbidden(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	// DB: caller workspace row for token validation.
	mock.ExpectQuery("SELECT t.id, t.workspace_id").
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "workspace_id"}).
			AddRow("tok-attacker", "ws-attacker"))

	// DB: caller and target have different parents → CanCommunicate denies.
	mock.ExpectQuery("SELECT id, parent_id FROM workspaces WHERE id").
		WithArgs("ws-attacker").
		WillReturnRows(sqlmock.NewRows([]string{"id", "parent_id"}).
			AddRow("ws-attacker", "org-a"))
	mock.ExpectQuery("SELECT id, parent_id FROM workspaces WHERE id").
		WithArgs("ws-victim").
		WillReturnRows(sqlmock.NewRows([]string{"id", "parent_id"}).
			AddRow("ws-victim", "org-b"))

	h := NewTerminalHandler(nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-victim"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-victim/terminal", nil)
	c.Request.Header.Set("X-Workspace-ID", "ws-attacker")
	c.Request.Header.Set("Authorization", "Bearer attacker-token")

	h.HandleConnect(c)

	if w.Code != http.StatusForbidden {
		t.Errorf("cross-workspace: expected 403, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestKI005_TokenMismatch_Unauthorized — when the bearer token belongs to a
// different workspace than the claimed X-Workspace-ID, ValidateToken fails
// and the request is rejected with 401 before CanCommunicate is checked.
func TestKI005_TokenMismatch_Unauthorized(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	// DB: token belongs to a different workspace than claimed — ValidateToken
	// returns ErrInvalidToken (workspaceID mismatch).
	mock.ExpectQuery("SELECT t.id, t.workspace_id").
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "workspace_id"}))

	h := NewTerminalHandler(nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-target"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-target/terminal", nil)
	c.Request.Header.Set("X-Workspace-ID", "ws-claimed")
	c.Request.Header.Set("Authorization", "Bearer wrong-workspace-token")

	h.HandleConnect(c)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("token mismatch: expected 401, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestKI005_NoXWorkspaceIDHeader_LegacyAllowed — when no X-Workspace-ID header
// is present (legacy canvas, direct browser access), the hierarchy check is
// skipped and the request proceeds to the container (standard WorkspaceAuth
// gates apply upstream).
func TestKI005_NoXWorkspaceIDHeader_LegacyAllowed(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	stubNoLocalShell(t)

	// DB: no instance_id → local Docker path.
	mock.ExpectQuery("SELECT COALESCE").
		WithArgs("ws-legacy").
		WillReturnRows(sqlmock.NewRows([]string{"instance_id"}).AddRow(""))

	h := NewTerminalHandler(nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-legacy"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-legacy/terminal", nil)
	// No X-Workspace-ID header: legacy access, no hierarchy check.

	h.HandleConnect(c)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("legacy access: expected 503 (Docker unavailable), got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestOpenTunnelCmd_BuildsArgv guards against silent drift in the EIC
// tunnel invocation (e.g. someone flipping --local-port to --port).
func TestOpenTunnelCmd_BuildsArgv(t *testing.T) {
	cmd := openTunnelCmd(eicSSHOptions{
		InstanceID: "i-0abc",
		Region:     "us-east-2",
		LocalPort:  2222,
	})
	want := []string{
		"aws", "--region", "us-east-2",
		"ec2-instance-connect", "open-tunnel",
		"--instance-id", "i-0abc",
		"--local-port", "2222",
	}
	if len(cmd.Args) != len(want) {
		t.Fatalf("argv length: got %v want %v", cmd.Args, want)
	}
	for i := range want {
		if cmd.Args[i] != want[i] {
			t.Errorf("argv[%d] = %q, want %q", i, cmd.Args[i], want[i])
		}
	}
}

// TestSSHCommandCmd_BuildsArgv guards against drift in the ssh-client
// invocation — specifically the user@host shape and the inline options
// that defeat host-key + known_hosts friction.
func TestSSHCommandCmd_BuildsArgv(t *testing.T) {
	cmd := sshCommandCmd(eicSSHOptions{
		OSUser:         "ubuntu",
		LocalPort:      2222,
		PrivateKeyPath: "/tmp/k",
	})
	want := []string{
		"ssh",
		"-i", "/tmp/k",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=10",
		"-o", "ServerAliveInterval=30",
		"-o", "ServerAliveCountMax=3",
		"-p", "2222",
		"ubuntu@127.0.0.1",
	}
	if len(cmd.Args) != len(want) {
		t.Fatalf("argv length: got %v want %v", cmd.Args, want)
	}
	for i := range want {
		if cmd.Args[i] != want[i] {
			t.Errorf("argv[%d] = %q, want %q", i, cmd.Args[i], want[i])
		}
	}
}

// TestTerminalConnect_KI005_AllowsOwnTerminal tests the flip side of KI-005:
// a workspace must still be able to access its own terminal. The CanCommunicate
// fast-path returns true when callerID == targetID.
func TestTerminalConnect_KI005_AllowsOwnTerminal(t *testing.T) {
	mock := setupTestDB(t)
	stubNoLocalShell(t)
	mock.ExpectQuery("SELECT COALESCE").
		WithArgs("ws-alice").
		WillReturnRows(sqlmock.NewRows([]string{"instance_id"}).AddRow(""))

	// CanCommunicate fast-path: callerID == targetID → returns true without DB.
	prev := canCommunicateCheck
	canCommunicateCheck = func(callerID, targetID string) bool { return callerID == targetID }
	defer func() { canCommunicateCheck = prev }()

	h := NewTerminalHandler(nil) // nil docker → 503 if reached
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-alice"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-alice/terminal", nil)
	c.Request.Header.Set("X-Workspace-ID", "ws-alice")
	c.Request.Header.Set("Authorization", "Bearer valid-token")

	h.HandleConnect(c)

	// Got 503 (nil docker) instead of 403 — means CanCommunicate passed
	// and we reached the Docker path, which is correct.
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("own-terminal pass-through: got %d, want 503 nil-docker (%s)", w.Code, w.Body.String())
	}
}

// TestTerminalConnect_KI005_SkipsCheckWithoutHeader tests the allowlist path:
// callers that don't send X-Workspace-ID (canvas/molecli with bearer-only auth)
// skip the CanCommunicate check entirely and fall through to the Docker auth path.
// We assert they get the nil-docker 503 instead of 403.
func TestTerminalConnect_KI005_SkipsCheckWithoutHeader(t *testing.T) {
	mock := setupTestDB(t)
	stubNoLocalShell(t)
	mock.ExpectQuery("SELECT COALESCE").
		WithArgs("ws-any").
		WillReturnRows(sqlmock.NewRows([]string{"instance_id"}).AddRow(""))

	h := NewTerminalHandler(nil) // nil docker → 503 if reached
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-any"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-any/terminal", nil)
	// No X-Workspace-ID header → KI-005 check is skipped

	h.HandleConnect(c)

	// Got 503 (nil docker) instead of 403 — means KI-005 check was skipped
	// and we reached the Docker path, which is correct.
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("no X-Workspace-ID: got %d, want 503 nil-docker (%s)", w.Code, w.Body.String())
	}
}

// TestTerminalConnect_KI005_RejectsInvalidToken tests that an invalid bearer
// token when X-Workspace-ID is set results in 401 Unauthorized.
// ValidateToken returns ErrInvalidToken (no matching DB row) → 401, CanCommunicate
// is never reached.
func TestTerminalConnect_KI005_RejectsInvalidToken(t *testing.T) {
	setupTestDB(t) // provides a mock DB; no expectations set → ValidateToken query returns error
	canCommunicateCalled := false
	prev := canCommunicateCheck
	canCommunicateCheck = func(callerID, targetID string) bool {
		canCommunicateCalled = true
		return true
	}
	defer func() { canCommunicateCheck = prev }()

	h := NewTerminalHandler(nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-target"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-target/terminal", nil)
	c.Request.Header.Set("X-Workspace-ID", "ws-caller")
	c.Request.Header.Set("Authorization", "Bearer invalid-token")

	h.HandleConnect(c)

	if canCommunicateCalled {
		t.Error("CanCommunicate should not be called with an invalid token")
	}
	// ValidateToken returns ErrInvalidToken (token not in DB or bound to wrong workspace).
	// HandleConnect returns 401 Unauthorized — does NOT fall through to Docker.
	if w.Code != http.StatusUnauthorized {
		t.Errorf("invalid token: got %d, want 401 Unauthorized (%s)", w.Code, w.Body.String())
	}
}

// TestTerminalConnect_KI005_AllowsSiblingWorkspace tests the sibling path:
// two workspaces with the same parent ID should be allowed to communicate.
// ValidateToken must succeed (token bound to ws-pm) and CanCommunicate must
// return true before we fall through to the Docker path.
func TestTerminalConnect_KI005_AllowsSiblingWorkspace(t *testing.T) {
	mock := setupTestDB(t)
	stubNoLocalShell(t)
	prev := canCommunicateCheck
	canCommunicateCheck = func(callerID, targetID string) bool {
		// Simulate sibling: same parent
		return callerID == "ws-pm" && targetID == "ws-dev"
	}
	defer func() { canCommunicateCheck = prev }()

	// ValidateToken: token is bound to ws-pm (the callerID). Returns id + workspace_id.
	rows := sqlmock.NewRows([]string{"id", "workspace_id"}).AddRow("tok-pm", "ws-pm")
	mock.ExpectQuery(`SELECT t\.id, t\.workspace_id\s+FROM workspace_auth_tokens t`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(rows)
	// Best-effort last_used_at UPDATE.
	mock.ExpectExec(`UPDATE workspace_auth_tokens SET last_used_at`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT COALESCE").
		WithArgs("ws-dev").
		WillReturnRows(sqlmock.NewRows([]string{"instance_id"}).AddRow(""))

	h := NewTerminalHandler(nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-dev"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-dev/terminal", nil)
	c.Request.Header.Set("X-Workspace-ID", "ws-pm")
	c.Request.Header.Set("Authorization", "Bearer valid-token-for-ws-pm")

	h.HandleConnect(c)

	// ValidateToken passed + CanCommunicate=true → reached Docker path → 503 nil-docker.
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("sibling access: got %d, want 503 nil-docker (%s)", w.Code, w.Body.String())
	}
}

// TestKI005_OrgToken_SkipsValidateToken verifies that when WorkspaceAuth already
// validated an org token (org_token_id set in gin context), the X-Workspace-ID
// claim is trusted without a workspace_auth_tokens lookup. The hierarchy is still
// enforced by canCommunicateCheck. Regression guard for the A2A routing regression
// introduced in GH#1885: internal routing uses org tokens which are not in
// workspace_auth_tokens, so ValidateToken would always fail for them.
func TestKI005_OrgToken_SkipsValidateToken(t *testing.T) {
	mock := setupTestDB(t) // no ValidateToken ExpectQuery — none should fire
	stubNoLocalShell(t)
	mock.ExpectQuery("SELECT COALESCE").
		WithArgs("ws-target").
		WillReturnRows(sqlmock.NewRows([]string{"instance_id"}).AddRow(""))
	prev := canCommunicateCheck
	canCommunicateCheck = func(callerID, targetID string) bool {
		// Simulate platform agent → target workspace (same org).
		return callerID == "ws-platform" && targetID == "ws-target"
	}
	defer func() { canCommunicateCheck = prev }()

	h := NewTerminalHandler(nil)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-target"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-target/terminal", nil)
	c.Request.Header.Set("X-Workspace-ID", "ws-platform")
	c.Request.Header.Set("Authorization", "Bearer org-token-abc123")
	// Simulate WorkspaceAuth having validated the org token (orgtoken.Validate
	// succeeded). HandleConnect must skip ValidateToken and trust the claim.
	c.Set("org_token_id", "tok-org-abc")

	h.HandleConnect(c)

	// Org token path: ValidateToken skipped → canCommunicateCheck=true →
	// falls through to Docker path → 503 nil-docker (no Docker client).
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("org-token A2A: got %d, want 503 nil-docker (%s)", w.Code, w.Body.String())
	}
}

// TestSSHCommandCmd_ConnectTimeoutPresent pins the user-experience guard
// against ssh-handshake-hang. Without ConnectTimeout, ssh waits forever
// for the remote sshd's banner — which masquerades as a "silently dead"
// shell to the user, because the workspace-server's local PTY is in
// cooked + echo mode before ssh finishes its handshake, so the canvas
// echoes the user's keystrokes back without ever reaching remote bash,
// and Cloudflare eventually closes the WebSocket on idle (~100s) with
// no error frame to surface what went wrong.
//
// Repro 2026-04-30: a 60s probe at hongmingwang's hermes /terminal
// endpoint after the heartbeat-fix redeploy showed only the local-PTY
// echo of a single 'X' typed mid-handshake. Workspace EC2 was up and
// heartbeating but its sshd was unresponsive; ssh hung indefinitely.
//
// Behavior-based: matches the literal `-o ConnectTimeout=N` arg pair so
// this stays pinned even if the rest of the args reorder. Does not pin
// the exact value — operators may tune it — but does pin presence.
func TestSSHCommandCmd_ConnectTimeoutPresent(t *testing.T) {
	t.Parallel()

	cmd := sshCommandCmd(eicSSHOptions{
		InstanceID:     "i-test",
		OSUser:         "ubuntu",
		Region:         "us-east-2",
		LocalPort:      2222,
		PrivateKeyPath: "/tmp/test-key",
	})

	args := cmd.Args
	found := false
	for i, a := range args {
		if a != "-o" {
			continue
		}
		if i+1 >= len(args) {
			continue
		}
		val := args[i+1]
		if len(val) >= len("ConnectTimeout=") &&
			val[:len("ConnectTimeout=")] == "ConnectTimeout=" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("sshCommandCmd is missing `-o ConnectTimeout=N` — without it, "+
			"ssh hangs forever when the workspace EC2's sshd is unresponsive "+
			"and the canvas terminal silently dies on Cloudflare's idle WS "+
			"timeout with no error message reaching the user. See terminal.go "+
			"sshCommandCmd comment (2026-04-30 hongmingwang hermes). args=%v",
			args)
	}
}

// TestParseDescribeInstancesOutput_RunningAndAZ verifies the AWS CLI JSON
// parser extracts state and availability zone.
func TestParseDescribeInstancesOutput_RunningAndAZ(t *testing.T) {
	out := []byte(`{"Reservations":[{"Instances":[{"State":{"Name":"running"},"Placement":{"AvailabilityZone":"us-east-2c"}}]}]}`)
	info, err := parseDescribeInstancesOutput(out)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if info.State != "running" {
		t.Errorf("state=%q, want running", info.State)
	}
	if info.AZ != "us-east-2c" {
		t.Errorf("az=%q, want us-east-2c", info.AZ)
	}
}

// TestParseDescribeInstancesOutput_NoInstance errors when the instance is
// absent from the describe-instances response.
func TestParseDescribeInstancesOutput_NoInstance(t *testing.T) {
	out := []byte(`{"Reservations":[]}`)
	_, err := parseDescribeInstancesOutput(out)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected instance-not-found error, got %v", err)
	}
}

// TestSendSSHPublicKeyImpl_GatesNotRunning verifies the EIC key push is
// rejected after the bounded wait when the instance never reaches running.
func TestSendSSHPublicKeyImpl_GatesNotRunning(t *testing.T) {
	prev := describeEC2InstanceForEIC
	describeEC2InstanceForEIC = func(ctx context.Context, region, instanceID string) (eicInstanceState, error) {
		return eicInstanceState{State: "stopped", AZ: "us-east-2a"}, nil
	}
	defer func() { describeEC2InstanceForEIC = prev }()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := sendSSHPublicKeyImpl(ctx, "us-east-2", "i-123", "ubuntu", "pk")
	if err == nil || !strings.Contains(err.Error(), "running") {
		t.Fatalf("expected not-running error, got %v", err)
	}
}

// TestSendSSHPublicKeyImpl_WaitsForRunning verifies the bounded wait loop
// polls DescribeInstances until the instance reaches running + AZ.
func TestSendSSHPublicKeyImpl_WaitsForRunning(t *testing.T) {
	prevDescribe := describeEC2InstanceForEIC
	calls := 0
	describeEC2InstanceForEIC = func(ctx context.Context, region, instanceID string) (eicInstanceState, error) {
		calls++
		if calls < 2 {
			return eicInstanceState{State: "pending", AZ: ""}, nil
		}
		return eicInstanceState{State: "running", AZ: "us-east-2d"}, nil
	}
	defer func() { describeEC2InstanceForEIC = prevDescribe }()

	prevExec := eicExecCommandContext
	captured := []string{}
	eicExecCommandContext = func(ctx context.Context, name string, arg ...string) *exec.Cmd {
		captured = append([]string{name}, arg...)
		return exec.CommandContext(ctx, "true")
	}
	defer func() { eicExecCommandContext = prevExec }()

	if err := sendSSHPublicKeyImpl(context.Background(), "us-east-2", "i-123", "ubuntu", "pk"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls < 2 {
		t.Fatalf("expected describe polling, got %d calls", calls)
	}
	got := strings.Join(captured, " ")
	if !strings.Contains(got, "--availability-zone us-east-2d") {
		t.Errorf("send-ssh-public-key args missing AZ; got %q", got)
	}
}

// TestSendSSHPublicKeyImpl_PassesAZ verifies the resolved AZ is forwarded as
// --availability-zone on the send-ssh-public-key command.
func TestSendSSHPublicKeyImpl_PassesAZ(t *testing.T) {
	prevDescribe := describeEC2InstanceForEIC
	describeEC2InstanceForEIC = func(ctx context.Context, region, instanceID string) (eicInstanceState, error) {
		return eicInstanceState{State: "running", AZ: "us-east-2b"}, nil
	}
	defer func() { describeEC2InstanceForEIC = prevDescribe }()

	prevExec := eicExecCommandContext
	captured := []string{}
	eicExecCommandContext = func(ctx context.Context, name string, arg ...string) *exec.Cmd {
		captured = append([]string{name}, arg...)
		return exec.CommandContext(ctx, "true")
	}
	defer func() { eicExecCommandContext = prevExec }()

	if err := sendSSHPublicKeyImpl(context.Background(), "us-east-2", "i-123", "ubuntu", "pk"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := strings.Join(captured, " ")
	if !strings.Contains(got, "--availability-zone us-east-2b") {
		t.Errorf("send-ssh-public-key args missing AZ; got %q", got)
	}
}

// TestLocalShellCommand_PrefersBashThenSh verifies the shell resolver returns
// the first available shell in bash→sh preference order, mirroring the
// docker-exec fallback. On any POSIX CI box /bin/sh exists, so we always get a
// non-empty argv; when /bin/bash is present it must be chosen first.
func TestLocalShellCommand_PrefersBashThenSh(t *testing.T) {
	argv, err := localShellCommand()
	if err != nil {
		t.Fatalf("localShellCommand: unexpected error on a POSIX host: %v", err)
	}
	if len(argv) == 0 {
		t.Fatalf("localShellCommand returned empty argv")
	}
	if argv[0] != "/bin/bash" && argv[0] != "/bin/sh" {
		t.Errorf("localShellCommand chose %q, want /bin/bash or /bin/sh", argv[0])
	}
	if _, statErr := os.Stat("/bin/bash"); statErr == nil {
		if argv[0] != "/bin/bash" {
			t.Errorf("localShellCommand chose %q but /bin/bash exists — bash must win", argv[0])
		}
	}
}

// TestHandleLocalConnect_NilDocker_DoesNotServiceUnavailable is the core
// regression guard for FIX #1: on a molecules-server (local-docker) tenant the
// workspace-server runs inside the workspace container with NO docker client,
// and the canvas terminal must NOT 503 "Docker not available". Instead it must
// route to the local-shell path. We assert the response is anything BUT the old
// 503 (a real ws upgrade can't complete against a plain httptest request, so
// the handler returns without writing a 503).
func TestHandleLocalConnect_NilDocker_DoesNotServiceUnavailable(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	mock.ExpectQuery("SELECT COALESCE").
		WithArgs("ws-local-shell").
		WillReturnRows(sqlmock.NewRows([]string{"instance_id"}).AddRow(""))

	h := NewTerminalHandler(nil) // nil docker → in-container local-shell path
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-local-shell"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-local-shell/terminal", nil)

	h.HandleConnect(c)

	if w.Code == http.StatusServiceUnavailable &&
		strings.Contains(w.Body.String(), "Docker not available") {
		t.Errorf("nil-docker local terminal still 503s 'Docker not available' — FIX #1 regressed: %s", w.Body.String())
	}
}

// TestHandleLocalShellConnect_GivesWorkingShell drives the docker-nil local
// path end-to-end over a real WebSocket: it connects, runs a command in the
// spawned PTY shell, and asserts the shell's output comes back over the socket.
// This proves the in-container terminal actually works on a molecules-server
// tenant (the canvas terminal was fully broken before FIX #1).
func TestHandleLocalShellConnect_GivesWorkingShell(t *testing.T) {
	if _, err := localShellCommand(); err != nil {
		t.Skipf("no local shell on this host: %v", err)
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewTerminalHandler(nil)
	r.GET("/ws/:id/terminal", func(c *gin.Context) {
		h.handleLocalShellConnect(c, c.Param("id"))
	})
	srv := httptest.NewServer(r)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/ws-x/terminal"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Origin": {"http://localhost:3000"}})
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer conn.Close()

	const marker = "MOLECULE_LOCAL_SHELL_OK_9931"
	if err := conn.WriteMessage(websocket.TextMessage, []byte("echo "+marker+"\n")); err != nil {
		t.Fatalf("ws write: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	_ = conn.SetReadDeadline(deadline)
	var got strings.Builder
	for time.Now().Before(deadline) {
		_, data, rErr := conn.ReadMessage()
		if rErr != nil {
			break
		}
		got.WriteString(string(data))
		// The marker echoes twice (terminal echo of the typed line + the echo
		// command's own stdout). One occurrence on its own line is enough proof
		// the PTY ran the command.
		if strings.Contains(got.String(), marker) {
			return
		}
	}
	t.Fatalf("did not observe %q in local shell output; got: %q", marker, got.String())
}
