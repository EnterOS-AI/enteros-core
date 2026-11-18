//go:build integration
// +build integration

// workspace_light_state_integration_test.go — real-Postgres coverage of the two
// reads the canvas builds its Light UI on: light_state on every workspace
// object of GET /workspaces and GET /workspaces/:id, and next_run_at on every
// schedule of GET /workspaces/:id/light. sqlmock returns scripted columns
// whatever the SELECT says; only a real database proves the queries.
//
// Run as light_schema_integration_test.go says. NOT SAFE FOR t.Parallel().

package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestIntegration_LightStateOnWorkspaceListAndGet(t *testing.T) {
	conn := integrationDB_Light(t)
	parked := seedLightWorkspace(t, conn, "test-light-ls-parked", "hibernated", "parked")
	plain := seedLightWorkspace(t, conn, "test-light-ls-plain", "online", "")
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/workspaces", nil)
	handler.List(c)
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d %s", w.Code, w.Body.String())
	}
	var list []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("list body: %v", err)
	}
	byID := map[string]map[string]interface{}{}
	for _, ws := range list {
		byID[ws["id"].(string)] = ws
	}
	want := map[string]interface{}{parked: "parked", plain: nil}
	for id, state := range want {
		ws, ok := byID[id]
		if !ok {
			t.Fatalf("list is missing workspace %s", id)
		}
		if v, present := ws["light_state"]; !present || v != state {
			t.Errorf("list: %s light_state = %v (present %v), want %v", id, v, present, state)
		}
	}

	for id, state := range want {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Params = gin.Params{{Key: "id", Value: id}}
		c.Request = httptest.NewRequest(http.MethodGet, "/workspaces/"+id, nil)
		handler.Get(c)
		if w.Code != http.StatusOK {
			t.Fatalf("get %s = %d %s", id, w.Code, w.Body.String())
		}
		var ws map[string]interface{}
		if err := json.Unmarshal(w.Body.Bytes(), &ws); err != nil {
			t.Fatalf("get body: %v", err)
		}
		if v, present := ws["light_state"]; !present || v != state {
			t.Errorf("get: %s light_state = %v (present %v), want %v", id, v, present, state)
		}
	}
}

func TestIntegration_LightGetReportsNextRunAt(t *testing.T) {
	conn := integrationDB_Light(t)
	id := seedLightWorkspace(t, conn, "test-light-next-run", "hibernated", "parked")
	if _, err := conn.ExecContext(context.Background(), `
		INSERT INTO light_schedules (workspace_id, name, cron, timezone, prompt, enabled, suspended_reason)
		VALUES ($1, 'daily', '0 9 * * *', 'Asia/Tokyo', 'p', true, NULL),
		       ($1, 'off', '0 9 * * *', 'UTC', 'p', false, NULL),
		       ($1, 'suspended', '0 9 * * *', 'UTC', 'p', true, 'three failed runs in a row'),
		       ($1, 'utc-default', '45 9 * * *', '', 'p', true, NULL)`, id); err != nil {
		t.Fatalf("seed schedules: %v", err)
	}
	// 09:30 UTC is 18:30 in Tokyo (UTC+9 all year).
	pinLightClock(t, time.Date(2026, 11, 13, 9, 30, 0, 0, time.UTC))
	lh := NewLightHandler(NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir()))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: id}}
	c.Request = httptest.NewRequest(http.MethodGet, "/workspaces/"+id+"/light", nil)
	lh.Get(c)
	if w.Code != http.StatusOK {
		t.Fatalf("get = %d %s", w.Code, w.Body.String())
	}
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("get body: %v", err)
	}
	got := nextRunAtByName(t, body)
	for name, want := range map[string]interface{}{
		"daily":       "2026-11-14T00:00:00Z",
		"off":         nil,
		"suspended":   nil,
		"utc-default": "2026-11-13T09:45:00Z",
	} {
		if got[name] != want {
			t.Errorf("%s: next_run_at = %v, want %v", name, got[name], want)
		}
	}
}
