// Package desktopcontrol is the desktop-sidecar control server: the "actuator"
// layer of the computer-use 3-layer split (design RFC §9). It runs INSIDE the
// desktop sidecar container and exposes a small authenticated HTTP API that the
// platform-side gateway proxies to:
//
//	GET  /screenshot -> PNG grabbed directly from the framebuffer (agent eyes)
//	POST /input      -> a single action (click/type/key/scroll) via xdotool
//	GET  /healthz    -> liveness (no auth)
//
// It is deliberately DUMB: no coordinate math, no browser knowledge, no lock
// awareness (arbitration lives in the platform gateway). The fixed-resolution
// coordinate contract (§3) is enforced only as a safety net — a click outside
// the pinned geometry is rejected, so a mis-scaled coordinate surfaces as a 400
// instead of landing on the wrong pixel.
//
// Auth (§6.5): EVERY request to /screenshot and /input must carry the
// per-sidecar bearer token. There is no "internal caller" exemption here — the
// sidecar authenticates inbound independently, so a same-network peer cannot
// inject synthetic input into another workspace's desktop.
package desktopcontrol

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// maxScrollAmount bounds a single scroll action's magnitude. The actuator
// issues one xdotool button click per unit, so this caps the per-request work
// at a fixed, small number of clicks — preventing an unbounded amount from
// becoming a near-infinite CPU loop.
const maxScrollAmount = 100

// Geometry is the pinned desktop resolution (the §3 coordinate contract).
type Geometry struct {
	Width  int
	Height int
}

// Action is one computer-use action. Mirrors the SSOT contract's action enum
// (design §4). Exactly one action per /input request.
type Action struct {
	Type   string `json:"type"`             // screenshot|click|type|key|scroll
	X      int    `json:"x,omitempty"`      // click target (0 <= x < Width)
	Y      int    `json:"y,omitempty"`      // click target (0 <= y < Height)
	Button string `json:"button,omitempty"` // left|right|middle (default left)
	Text   string `json:"text,omitempty"`   // for type
	Keys   string `json:"keys,omitempty"`   // for key, e.g. "ctrl+v", "Return"
	Amount int    `json:"amount,omitempty"` // for scroll (signed; +down/-up)
}

// Actuator performs actions against the real X display. Behind an interface so
// the HTTP/auth/validation layer is testable without a running desktop.
type Actuator interface {
	Screenshot(ctx context.Context) (png []byte, err error)
	Click(ctx context.Context, x, y int, button string) error
	Type(ctx context.Context, text string) error
	Key(ctx context.Context, keys string) error
	Scroll(ctx context.Context, amount int) error
}

// Server is the control-server HTTP surface.
type Server struct {
	token string
	geom  Geometry
	act   Actuator
}

// NewServer builds the control server. token is the per-sidecar bearer secret
// (must be non-empty in production); geom is the pinned resolution.
func NewServer(token string, geom Geometry, act Actuator) *Server {
	return &Server{token: token, geom: geom, act: act}
}

// Handler returns the mux with auth applied to the sensitive routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.Handle("GET /screenshot", s.auth(http.HandlerFunc(s.handleScreenshot)))
	mux.Handle("POST /input", s.auth(http.HandlerFunc(s.handleInput)))
	return mux
}

// auth enforces the per-sidecar bearer token, fail-closed. A missing token
// config is treated as "deny everything" so a misconfigured server can never
// serve an unauthenticated /input.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.token == "" {
			http.Error(w, "control server not configured with a token", http.StatusUnauthorized)
			return
		}
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleScreenshot(w http.ResponseWriter, r *http.Request) {
	png, err := s.act.Screenshot(r.Context())
	if err != nil {
		http.Error(w, "screenshot failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(png)
}

func (s *Server) handleInput(w http.ResponseWriter, r *http.Request) {
	var a Action
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&a); err != nil {
		http.Error(w, "bad input json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.validate(a); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.dispatch(r.Context(), a); err != nil {
		http.Error(w, "input failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// validate enforces the action enum and the fixed-geometry coordinate contract.
func (s *Server) validate(a Action) error {
	switch a.Type {
	case "click":
		if a.X < 0 || a.X >= s.geom.Width || a.Y < 0 || a.Y >= s.geom.Height {
			return fmt.Errorf("click (%d,%d) outside the pinned %dx%d display — reject a mis-scaled coordinate rather than click the wrong pixel", a.X, a.Y, s.geom.Width, s.geom.Height)
		}
		switch a.Button {
		case "", "left", "right", "middle":
		default:
			return fmt.Errorf("unsupported button %q", a.Button)
		}
	case "type":
		if a.Text == "" {
			return fmt.Errorf("type action requires non-empty text")
		}
	case "key":
		if a.Keys == "" {
			return fmt.Errorf("key action requires keys (e.g. \"ctrl+v\")")
		}
	case "scroll":
		if a.Amount == 0 {
			return fmt.Errorf("scroll action requires a non-zero amount")
		}
		// Bound the magnitude: the actuator issues one xdotool click per unit
		// (exec_actuator.go), so an unbounded amount (e.g. 2e9) is a near-infinite
		// CPU loop — a trivial local DoS. maxScrollAmount clicks is far past any
		// legitimate single scroll gesture; a caller wanting more sends multiple
		// actions. (Reviewer scroll-DoS nit.)
		if a.Amount > maxScrollAmount || a.Amount < -maxScrollAmount {
			return fmt.Errorf("scroll amount %d out of range (max magnitude %d)", a.Amount, maxScrollAmount)
		}
	case "screenshot":
		// no-op action shape; screenshots normally use GET /screenshot.
	default:
		return fmt.Errorf("unsupported action type %q (want click|type|key|scroll|screenshot)", a.Type)
	}
	return nil
}

func (s *Server) dispatch(ctx context.Context, a Action) error {
	switch a.Type {
	case "click":
		btn := a.Button
		if btn == "" {
			btn = "left"
		}
		return s.act.Click(ctx, a.X, a.Y, btn)
	case "type":
		return s.act.Type(ctx, a.Text)
	case "key":
		return s.act.Key(ctx, a.Keys)
	case "scroll":
		return s.act.Scroll(ctx, a.Amount)
	case "screenshot":
		_, err := s.act.Screenshot(ctx)
		return err
	}
	return fmt.Errorf("unreachable: unvalidated action %q", a.Type)
}
