package handlers

// boot_event_test.go — handler-level tests for POST
// /workspaces/:id/boot-event (the "Enter OS" boot-sequence ingestion
// path). The handler has no DB dependency, so these exercise the full
// validate → BroadcastOnly happy path plus every reject branch without
// standing up Postgres.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/events"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// bootTestBroadcaster records BroadcastOnly calls so tests can assert the
// BOOT_STEP payload shape + fan-out without the real Redis/WS topology.
type bootTestBroadcaster struct {
	mu       sync.Mutex
	captured []chatSessionCapturedBroadcast // reuse the shared capture struct
}

func (b *bootTestBroadcaster) BroadcastOnly(workspaceID string, eventType string, payload interface{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	pm, _ := payload.(map[string]interface{})
	b.captured = append(b.captured, chatSessionCapturedBroadcast{
		WorkspaceID: workspaceID,
		EventType:   eventType,
		Payload:     pm,
	})
}

func (b *bootTestBroadcaster) RecordAndBroadcast(_ context.Context, eventType string, workspaceID string, payload interface{}) error {
	b.BroadcastOnly(workspaceID, eventType, payload)
	return nil
}

func postBootEvent(t *testing.T, h *BootEventHandler, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rr := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rr)
	c.Request = httptest.NewRequest(http.MethodPost, "/workspaces/"+id+"/boot-event", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: id}}
	h.Report(c)
	return rr
}

func TestBootEvent_HappyPath_BroadcastsBootStep(t *testing.T) {
	cb := &bootTestBroadcaster{}
	h := NewBootEventHandler(cb)
	id := uuid.New().String()

	rr := postBootEvent(t, h, id, `{"step":3,"total":8,"key":"MCP","label":"Connect management MCP","status":"running","message":"launching npx…"}`)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%s)", rr.Code, rr.Body.String())
	}
	if len(cb.captured) != 1 {
		t.Fatalf("expected 1 broadcast, got %d", len(cb.captured))
	}
	got := cb.captured[0]
	if got.EventType != string(events.EventBootStep) {
		t.Errorf("event type = %q, want %q", got.EventType, events.EventBootStep)
	}
	if got.WorkspaceID != id {
		t.Errorf("workspace id = %q, want %q", got.WorkspaceID, id)
	}
	// JSON numbers decode to float64 through the map[string]interface{} path.
	if got.Payload["step"] != 3 {
		t.Errorf("step = %v (%T), want int 3", got.Payload["step"], got.Payload["step"])
	}
	if got.Payload["key"] != "MCP" || got.Payload["label"] != "Connect management MCP" {
		t.Errorf("key/label mismatch: %+v", got.Payload)
	}
	if got.Payload["status"] != "running" {
		t.Errorf("status = %v, want running", got.Payload["status"])
	}
	if got.Payload["workspace_id"] != id {
		t.Errorf("workspace_id payload field = %v, want %q", got.Payload["workspace_id"], id)
	}
}

func TestBootEvent_Rejects(t *testing.T) {
	cases := []struct {
		name string
		id   string
		body string
	}{
		{"non-uuid id", "not-a-uuid", `{"step":1,"total":8,"key":"PWR","label":"Provision","status":"running"}`},
		{"bad status", uuid.New().String(), `{"step":1,"total":8,"key":"PWR","label":"Provision","status":"bogus"}`},
		{"missing step", uuid.New().String(), `{"total":8,"key":"PWR","label":"Provision","status":"ok"}`},
		{"step below 1", uuid.New().String(), `{"step":0,"total":8,"key":"PWR","label":"Provision","status":"ok"}`},
		{"total below step", uuid.New().String(), `{"step":5,"total":3,"key":"PWR","label":"Provision","status":"ok"}`},
		{"missing label", uuid.New().String(), `{"step":1,"total":8,"key":"PWR","status":"ok"}`},
		{"key too long", uuid.New().String(), `{"step":1,"total":8,"key":"WAYTOOLONG","label":"Provision","status":"ok"}`},
		{"malformed json", uuid.New().String(), `{not json`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cb := &bootTestBroadcaster{}
			h := NewBootEventHandler(cb)
			rr := postBootEvent(t, h, tc.id, tc.body)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d (body=%s)", rr.Code, rr.Body.String())
			}
			if len(cb.captured) != 0 {
				t.Fatalf("expected no broadcast on reject, got %d", len(cb.captured))
			}
		})
	}
}

func TestBootEvent_NilBroadcasterNoPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nil broadcaster must not panic, got: %v", r)
		}
	}()
	h := NewBootEventHandler(nil)
	rr := postBootEvent(t, h, uuid.New().String(),
		`{"step":1,"total":8,"key":"PWR","label":"Provision","status":"ok"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 with nil broadcaster, got %d", rr.Code)
	}
}
