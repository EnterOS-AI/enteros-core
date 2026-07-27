package desktopcontrol

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeActuator struct {
	png       []byte
	err       error
	clicks    []struct{ X, Y int; Button string }
	typed     []string
	keys      []string
	scrolls   []int
	shots     int
}

func (f *fakeActuator) Screenshot(context.Context) ([]byte, error) {
	f.shots++
	return f.png, f.err
}
func (f *fakeActuator) Click(_ context.Context, x, y int, b string) error {
	f.clicks = append(f.clicks, struct{ X, Y int; Button string }{x, y, b})
	return f.err
}
func (f *fakeActuator) Type(_ context.Context, t string) error { f.typed = append(f.typed, t); return f.err }
func (f *fakeActuator) Key(_ context.Context, k string) error  { f.keys = append(f.keys, k); return f.err }
func (f *fakeActuator) Scroll(_ context.Context, n int) error  { f.scrolls = append(f.scrolls, n); return f.err }

const testToken = "sekret-token"

func newTestServer(act Actuator) http.Handler {
	return NewServer(testToken, Geometry{Width: 1280, Height: 800}, act).Handler()
}

func do(t *testing.T, h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestControl_AuthFailClosed(t *testing.T) {
	h := newTestServer(&fakeActuator{})
	// No token -> 401 on sensitive routes.
	if rr := do(t, h, "GET", "/screenshot", "", ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("screenshot no-token: got %d, want 401", rr.Code)
	}
	if rr := do(t, h, "POST", "/input", "", `{"type":"click","x":1,"y":1}`); rr.Code != http.StatusUnauthorized {
		t.Fatalf("input no-token: got %d, want 401", rr.Code)
	}
	// Wrong token -> 401.
	if rr := do(t, h, "GET", "/screenshot", "nope", ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("screenshot wrong-token: got %d, want 401", rr.Code)
	}
	// Health needs no token.
	if rr := do(t, h, "GET", "/healthz", "", ""); rr.Code != http.StatusOK {
		t.Fatalf("healthz: got %d, want 200", rr.Code)
	}
}

func TestControl_EmptyServerTokenDeniesEverything(t *testing.T) {
	h := NewServer("", Geometry{Width: 1280, Height: 800}, &fakeActuator{}).Handler()
	if rr := do(t, h, "POST", "/input", "anything", `{"type":"click","x":1,"y":1}`); rr.Code != http.StatusUnauthorized {
		t.Fatalf("misconfigured (empty token) server must deny: got %d, want 401", rr.Code)
	}
}

func TestControl_Screenshot(t *testing.T) {
	act := &fakeActuator{png: []byte("\x89PNG-bytes")}
	h := newTestServer(act)
	rr := do(t, h, "GET", "/screenshot", testToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("screenshot: got %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("content-type = %q, want image/png", ct)
	}
	if rr.Body.String() != "\x89PNG-bytes" {
		t.Fatalf("screenshot body mismatch")
	}
}

func TestControl_ScreenshotError(t *testing.T) {
	h := newTestServer(&fakeActuator{err: errors.New("scrot boom")})
	if rr := do(t, h, "GET", "/screenshot", testToken, ""); rr.Code != http.StatusInternalServerError {
		t.Fatalf("screenshot error: got %d, want 500", rr.Code)
	}
}

func TestControl_InputDispatch(t *testing.T) {
	act := &fakeActuator{}
	h := newTestServer(act)

	if rr := do(t, h, "POST", "/input", testToken, `{"type":"click","x":100,"y":200,"button":"right"}`); rr.Code != http.StatusNoContent {
		t.Fatalf("click: got %d, want 204 (%s)", rr.Code, rr.Body.String())
	}
	if len(act.clicks) != 1 || act.clicks[0].X != 100 || act.clicks[0].Y != 200 || act.clicks[0].Button != "right" {
		t.Fatalf("click not dispatched correctly: %+v", act.clicks)
	}
	// Default button is left.
	_ = do(t, h, "POST", "/input", testToken, `{"type":"click","x":5,"y":5}`)
	if act.clicks[1].Button != "left" {
		t.Fatalf("default button = %q, want left", act.clicks[1].Button)
	}

	_ = do(t, h, "POST", "/input", testToken, `{"type":"type","text":"hello"}`)
	if len(act.typed) != 1 || act.typed[0] != "hello" {
		t.Fatalf("type not dispatched: %v", act.typed)
	}
	_ = do(t, h, "POST", "/input", testToken, `{"type":"key","keys":"ctrl+v"}`)
	if len(act.keys) != 1 || act.keys[0] != "ctrl+v" {
		t.Fatalf("key not dispatched: %v", act.keys)
	}
	_ = do(t, h, "POST", "/input", testToken, `{"type":"scroll","amount":-3}`)
	if len(act.scrolls) != 1 || act.scrolls[0] != -3 {
		t.Fatalf("scroll not dispatched: %v", act.scrolls)
	}
}

// The coordinate contract safety net: a click outside the pinned geometry is
// rejected with 400 and NOT dispatched — a mis-scaled coordinate surfaces as an
// error instead of clicking the wrong pixel (design §3).
func TestControl_ClickOutOfBoundsRejected(t *testing.T) {
	act := &fakeActuator{}
	h := newTestServer(act)
	for _, body := range []string{
		`{"type":"click","x":1280,"y":10}`, // x == width (out)
		`{"type":"click","x":10,"y":800}`,  // y == height (out)
		`{"type":"click","x":-1,"y":10}`,   // negative
	} {
		if rr := do(t, h, "POST", "/input", testToken, body); rr.Code != http.StatusBadRequest {
			t.Fatalf("%s: got %d, want 400", body, rr.Code)
		}
	}
	if len(act.clicks) != 0 {
		t.Fatalf("out-of-bounds clicks must NOT be dispatched, got %+v", act.clicks)
	}
}

func TestControl_UnknownAndMalformed(t *testing.T) {
	act := &fakeActuator{}
	h := newTestServer(act)
	if rr := do(t, h, "POST", "/input", testToken, `{"type":"frobnicate"}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown action: got %d, want 400", rr.Code)
	}
	if rr := do(t, h, "POST", "/input", testToken, `{"type":"type"}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("empty type text: got %d, want 400", rr.Code)
	}
	if rr := do(t, h, "POST", "/input", testToken, `not json`); rr.Code != http.StatusBadRequest {
		t.Fatalf("malformed json: got %d, want 400", rr.Code)
	}
}
