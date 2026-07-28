package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/desktopgateway"
	"github.com/gin-gonic/gin"
)

type fakeDesktopGW struct {
	png           []byte
	inputErr      error
	screenshotErr error
	inputs        []json.RawMessage
}

func (f *fakeDesktopGW) Screenshot(context.Context, string) ([]byte, error) {
	return f.png, f.screenshotErr
}
func (f *fakeDesktopGW) Input(_ context.Context, _ string, a json.RawMessage) error {
	f.inputs = append(f.inputs, a)
	return f.inputErr
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
