package handlers

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// requireWorkspaceToken admits a request when the token-existence query
// FAILS. These tests pin the distinction that the fix turns on:
//
//	"no token yet"  (query succeeds, count 0) → ALLOWED  (real bootstrap)
//	"cannot tell"   (query errors)            → REFUSED  (503, retryable)
//
// Three directions are asserted, not one:
//
//   - refuses what it must     TestRequireWorkspaceToken_DBErrorIsRefused
//   - admits what it must      TestRequireWorkspaceToken_GenuineBootstrapStillAllowed
//   - is inert until enabled   TestRequireWorkspaceToken_FlagOff_DBErrorStillAdmits
//
// A handler that refuses everything fails the second. One that allows
// everything fails the first. One that ignores the flag fails the third.
// No single mutation survives this file.

const failClosedWS = "ws-failclosed-0001"

// tokenCountQuery is the HasLiveInstanceToken probe.
const tokenCountQuery = "SELECT COUNT\\(\\*\\) FROM workspace_auth_tokens"

func registerRequest(t *testing.T, wsID string, headers map[string]string) (*httptest.ResponseRecorder, *gin.Context) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := `{"id":"` + wsID + `","url":"http://example.com","agent_card":{"name":"test"}}`
	c.Request = httptest.NewRequest("POST", "/registry/register", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		c.Request.Header.Set(k, v)
	}
	return w, c
}

// ---------- "cannot tell" must be refused ----------

func TestRequireWorkspaceToken_DBErrorIsRefused(t *testing.T) {
	t.Setenv(registryTokenCheckFailClosedEnv, "1")

	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRegistryHandler(newTestBroadcaster())

	mock.ExpectQuery(tokenCountQuery).
		WithArgs(failClosedWS).
		WillReturnError(errors.New("connection reset by peer"))

	w, c := registerRequest(t, failClosedWS, nil)
	handler.Register(c)

	if w.Code == http.StatusOK {
		t.Fatalf("FAIL-OPEN REGRESSION: register succeeded despite an unverifiable "+
			"token check (body %s)", w.Body.String())
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 (retryable), got %d: %s", w.Code, w.Body.String())
	}
}

func TestRequireWorkspaceToken_DBErrorRefusesEvenWithABearer(t *testing.T) {
	t.Setenv(registryTokenCheckFailClosedEnv, "1")

	// A presented bearer must not rescue the request: we still cannot tell
	// whether it is the right one, so admitting it would be the same bypass
	// wearing a credential.
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRegistryHandler(newTestBroadcaster())

	mock.ExpectQuery(tokenCountQuery).
		WithArgs(failClosedWS).
		WillReturnError(errors.New("connection reset by peer"))

	w, c := registerRequest(t, failClosedWS, map[string]string{
		"Authorization": "Bearer some-plausible-looking-token",
	})
	handler.Register(c)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRequireWorkspaceToken_DBErrorIsRetryableNot401(t *testing.T) {
	t.Setenv(registryTokenCheckFailClosedEnv, "1")

	// 401 is terminal in the runtime's posture and would wedge a workspace
	// "online but braindead". The status code is load-bearing, not cosmetic.
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRegistryHandler(newTestBroadcaster())

	mock.ExpectQuery(tokenCountQuery).
		WithArgs(failClosedWS).
		WillReturnError(errors.New("transient"))

	w, c := registerRequest(t, failClosedWS, nil)
	handler.Register(c)

	if w.Code == http.StatusUnauthorized {
		t.Fatal("must not return 401 for an unverifiable check — 401 is terminal " +
			"and wedges the workspace; 503 is the retryable signal")
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", w.Code)
	}
}

// ---------- "no token yet" must still be allowed ----------

func TestRequireWorkspaceToken_GenuineBootstrapStillAllowed(t *testing.T) {
	// The blast-radius control. A genuinely new workspace has never minted a
	// token and cannot present one; refusing it would make first boot
	// impossible. The query SUCCEEDS and returns 0 — we CAN tell, and the
	// answer is "no token yet".
	//
	// This is the test that a blanket "refuse on anything unusual" fix fails.
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRegistryHandler(newTestBroadcaster())

	mock.ExpectQuery(tokenCountQuery).
		WithArgs(failClosedWS).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	w, c := registerRequest(t, failClosedWS, nil)
	handler.Register(c)

	if w.Code == http.StatusServiceUnavailable || w.Code == http.StatusUnauthorized {
		t.Fatalf("BOOTSTRAP BROKEN: a first-ever registration with no live token "+
			"must still be admitted, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------- pre-flip: the shipped default must be today's behaviour ----------

func TestRequireWorkspaceToken_FlagOff_DBErrorStillAdmits(t *testing.T) {
	// The ship-dark guarantee. Until an operator sets the flag, an
	// unverifiable check admits the request exactly as it does on main.
	// This is what keeps reno-stars and the existing suite green.
	t.Setenv(registryTokenCheckFailClosedEnv, "")

	mock := setupTestDB(t)
	setupTestRedis(t)
	handler := NewRegistryHandler(newTestBroadcaster())

	mock.ExpectQuery(tokenCountQuery).
		WithArgs(failClosedWS).
		WillReturnError(errors.New("connection reset by peer"))

	w, c := registerRequest(t, failClosedWS, nil)
	handler.Register(c)

	if w.Code == http.StatusServiceUnavailable {
		t.Fatalf("flag off must preserve the pre-change fail-open, got 503: %s",
			w.Body.String())
	}
}

func TestRegistryTokenCheckFailClosed_DefaultsToOff(t *testing.T) {
	t.Setenv(registryTokenCheckFailClosedEnv, "")
	if registryTokenCheckFailClosed() {
		t.Fatal("fail-closed must ship dark — turning it on by default reddens " +
			"74 existing tests and is a separate, measured migration")
	}
}

func TestRegistryTokenCheckFailClosed_SettableOn(t *testing.T) {
	// Paired with the default test so a predicate hardwired to false cannot
	// pass both — that mutation would make the whole fix inert.
	for _, v := range []string{"1", "true", "TRUE", "t"} {
		t.Setenv(registryTokenCheckFailClosedEnv, v)
		if !registryTokenCheckFailClosed() {
			t.Errorf("value %q must enable fail-closed", v)
		}
	}
}

func TestRegistryTokenCheckFailClosed_UnparseableStaysOff(t *testing.T) {
	for _, v := range []string{"yes", "on", "maybe", "2"} {
		t.Setenv(registryTokenCheckFailClosedEnv, v)
		if registryTokenCheckFailClosed() {
			t.Errorf("unparseable value %q must fall back to OFF", v)
		}
	}
}
