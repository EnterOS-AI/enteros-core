package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type fakeComputerGateway struct {
	png    []byte
	inputs []json.RawMessage
	err    error
}

func (f *fakeComputerGateway) Screenshot(context.Context, string) ([]byte, error) {
	return f.png, f.err
}
func (f *fakeComputerGateway) Input(_ context.Context, _ string, a json.RawMessage) error {
	f.inputs = append(f.inputs, a)
	return f.err
}

func TestToolComputer_UnavailableWhenNoGateway(t *testing.T) {
	h := &MCPHandler{}
	if _, err := h.toolComputer(context.Background(), "w1", map[string]interface{}{"action": "click"}); err == nil {
		t.Fatalf("want error when no gateway wired")
	}
}

func TestToolComputer_ClickProxiesAction(t *testing.T) {
	f := &fakeComputerGateway{}
	h := &MCPHandler{}
	h.SetComputerGateway(f)

	// JSON numbers arrive as float64 (as from the MCP arg decode).
	out, err := h.toolComputer(context.Background(), "w1", map[string]interface{}{
		"action": "click", "x": float64(100), "y": float64(200), "button": "right",
	})
	if err != nil || out != "ok" {
		t.Fatalf("click: out=%q err=%v", out, err)
	}
	if len(f.inputs) != 1 {
		t.Fatalf("want 1 proxied input, got %d", len(f.inputs))
	}
	var a struct {
		Type          string `json:"type"`
		X, Y          int
		Button        string `json:"button"`
	}
	if err := json.Unmarshal(f.inputs[0], &a); err != nil {
		t.Fatal(err)
	}
	if a.Type != "click" || a.X != 100 || a.Y != 200 || a.Button != "right" {
		t.Fatalf("bad proxied action: %+v", a)
	}
}

func TestToolComputer_ScreenshotReturnsDataURI(t *testing.T) {
	f := &fakeComputerGateway{png: []byte("\x89PNGdata")}
	h := &MCPHandler{}
	h.SetComputerGateway(f)

	out, err := h.toolComputer(context.Background(), "w1", map[string]interface{}{"action": "screenshot"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "data:image/png;base64,") {
		t.Fatalf("screenshot result not a data URI (prefix): %.40q", out)
	}
}

func TestToolComputer_UnknownActionAndError(t *testing.T) {
	h := &MCPHandler{}
	h.SetComputerGateway(&fakeComputerGateway{})
	if _, err := h.toolComputer(context.Background(), "w1", map[string]interface{}{"action": "frobnicate"}); err == nil {
		t.Fatalf("unknown action must error")
	}
	// Gateway error propagates.
	h.SetComputerGateway(&fakeComputerGateway{err: errors.New("boom")})
	if _, err := h.toolComputer(context.Background(), "w1", map[string]interface{}{"action": "type", "text": "x"}); err == nil {
		t.Fatalf("gateway error must propagate")
	}
}
