package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/desktopgateway"
	"github.com/gin-gonic/gin"
)

type fakeDesktopGW struct {
	png            []byte
	inputErr       error
	screenshotErr  error
	inputs         []json.RawMessage
	ensureAddr     string
	ensureErr      error
	ensureRunCalls int
	viewerConn     int
	viewerDisc     int
	navigatedTo    []string
	navigateErr    error
}

func (f *fakeDesktopGW) Screenshot(context.Context, string) ([]byte, error) {
	return f.png, f.screenshotErr
}
func (f *fakeDesktopGW) Input(_ context.Context, _ string, a json.RawMessage) error {
	f.inputs = append(f.inputs, a)
	return f.inputErr
}
func (f *fakeDesktopGW) EnsureRunning(context.Context, string) (string, error) {
	f.ensureRunCalls++
	return f.ensureAddr, f.ensureErr
}
func (f *fakeDesktopGW) ViewerConnected(context.Context, string)    { f.viewerConn++ }
func (f *fakeDesktopGW) ViewerDisconnected(context.Context, string) { f.viewerDisc++ }
func (f *fakeDesktopGW) HumanNavigate(_ context.Context, _, url string) error {
	f.navigatedTo = append(f.navigatedTo, url)
	return f.navigateErr
}

func newDesktopReq(method, path, body string) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
	c.Params = gin.Params{{Key: "id", Value: "w1"}}
	return c, w
}

func TestDesktopRoute_UnavailableWhenNoGateway(t *testing.T) {
	h := &WorkspaceHandler{}
	c, w := newDesktopReq("GET", "/desktop/screenshot", "")
	h.DesktopScreenshot(c)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("no gateway: want 503, got %d", w.Code)
	}
}

// GET /desktop/control with no gateway must return 503 so the status endpoint
// agrees with /input — otherwise desktop_wait_for_control thinks it may drive
// but every /input POST gets 503 (§8).
func TestDesktopRoute_ControlStatusUnavailableWhenNoGateway(t *testing.T) {
	h := &WorkspaceHandler{}
	c, w := newDesktopReq("GET", "/desktop/control", "")
	h.DesktopControlStatus(c)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("no gateway: want 503, got %d", w.Code)
	}
}

func TestDesktopRoute_Screenshot(t *testing.T) {
	h := &WorkspaceHandler{}
	h.SetDesktopGateway(&fakeDesktopGW{png: []byte("PNG")})
	c, w := newDesktopReq("GET", "/desktop/screenshot", "")
	h.DesktopScreenshot(c)
	if w.Code != http.StatusOK || w.Body.String() != "PNG" {
		t.Fatalf("screenshot: got %d %q", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("content-type = %q", ct)
	}
}

// A human holding control -> 409, so the in-container tool knows to pause (§8).
func TestDesktopRoute_InputHumanInControlIs409(t *testing.T) {
	h := &WorkspaceHandler{}
	h.SetDesktopGateway(&fakeDesktopGW{inputErr: desktopgateway.ErrHumanInControl})
	c, w := newDesktopReq("POST", "/desktop/input", `{"type":"click","x":1,"y":1}`)
	h.DesktopInput(c)
	if w.Code != http.StatusConflict {
		t.Fatalf("human-in-control: want 409, got %d", w.Code)
	}
}

func TestDesktopRoute_InputForwardsAndReturns204(t *testing.T) {
	gw := &fakeDesktopGW{}
	h := &WorkspaceHandler{}
	h.SetDesktopGateway(gw)
	c, _ := newDesktopReq("POST", "/desktop/input", `{"type":"click","x":10,"y":20}`)
	h.DesktopInput(c)
	// c.Status(204) sets the pending status on the gin writer; the engine
	// flushes it in production. In a direct handler call, read it off the
	// writer rather than the recorder (which only sees flushed writes).
	if c.Writer.Status() != http.StatusNoContent {
		t.Fatalf("input: want 204, got %d", c.Writer.Status())
	}
	if len(gw.inputs) != 1 {
		t.Fatalf("input not forwarded to gateway")
	}
}

// A URL bar navigate must be REFUSED (503) when the desktop backend isn't wired.
func TestDesktopNavigate_UnavailableWithoutGateway(t *testing.T) {
	h := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws1"}}
	c.Request = httptest.NewRequest("POST", "/x", bytes.NewBufferString(`{"url":"https://example.com","token":"t"}`))
	h.DesktopNavigate(c)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil gateway -> want 503, got %d", w.Code)
	}
}

// Security gate: a caller who does NOT hold display control cannot navigate the
// browser — no active lock -> 403, and the gateway is never asked to navigate.
// (A view-only spectator or a stale token lands here too.)
func TestDesktopNavigate_RejectsWhenNoControlHeld(t *testing.T) {
	mock := setupTestDB(t)
	h := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	gw := &fakeDesktopGW{}
	h.SetDesktopGateway(gw)
	mock.ExpectQuery(`SELECT controller, controlled_by, expires_at FROM workspace_display_control_locks WHERE workspace_id = \$1 AND expires_at > now\(\)`).
		WithArgs("ws1").
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws1"}}
	c.Request = httptest.NewRequest("POST", "/x", bytes.NewBufferString(`{"url":"https://example.com","token":"whatever"}`))
	h.DesktopNavigate(c)

	if w.Code != http.StatusForbidden {
		t.Fatalf("no control lock -> want 403, got %d: %s", w.Code, w.Body.String())
	}
	if len(gw.navigatedTo) != 0 {
		t.Fatalf("a rejected caller must NOT reach the gateway, got navigations %v", gw.navigatedTo)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// A malformed body is a 400 before any control lookup or navigation.
func TestDesktopNavigate_BadBody(t *testing.T) {
	h := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	gw := &fakeDesktopGW{}
	h.SetDesktopGateway(gw)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws1"}}
	c.Request = httptest.NewRequest("POST", "/x", bytes.NewBufferString(`not json`))
	h.DesktopNavigate(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad body -> want 400, got %d", w.Code)
	}
	if len(gw.navigatedTo) != 0 {
		t.Fatalf("bad body must not navigate, got %v", gw.navigatedTo)
	}
}
