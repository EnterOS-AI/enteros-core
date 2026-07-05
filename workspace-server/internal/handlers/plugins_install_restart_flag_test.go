package handlers

// plugins_install_restart_flag_test.go — the installRequest.Restart flag
// (self-reprovision, design §5.2).
//
// POST /workspaces/:id/plugins historically ALWAYS scheduled a restart after
// delivery. The self-install tool needs an explicit opt-out ({"restart":
// false} → deliver + record only) while the absent-field default MUST stay
// restart-on (every existing caller relies on it). These tests drive the
// full Install handler through the EIC SaaS dispatch branch (no docker
// daemon needed) with a captured restartFunc:
//
//   - absent field   → restartFunc fires, response restarting=true
//   - restart:true   → restartFunc fires, response restarting=true
//   - restart:false  → restartFunc does NOT fire, response restarting=false
//
// Fail-before/pass-after: the restart:false case fails on the pre-change
// handler (restart always fired; no "restarting" response field).

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
)

// driveInstall posts `body` to the Install handler wired with an EIC stub
// and a counting restartFunc; returns the recorder and the restart count
// (read AFTER the async drain, so a scheduled restart is always observed).
func driveInstall(t *testing.T, body string) (*httptest.ResponseRecorder, int32) {
	t.Helper()
	registry := t.TempDir()
	stagePluginRegistry(t, registry, "browser-automation")

	stubInstallPluginViaEIC(t, func(ctx context.Context, instanceID, runtime, pluginName, stagedDir string) error {
		return nil
	})

	mock, cleanup := withMockDB(t)
	defer cleanup()
	expectAllowlistAllowAll(mock)

	var restarts int32
	h := NewPluginsHandler(registry, nil, func(string) {
		atomic.AddInt32(&restarts, 1)
	}).
		WithRuntimeLookup(func(string) (string, error) { return "claude-code", nil }).
		WithInstanceIDLookup(func(string) (string, error) { return "i-0e0951a3cfd9bbf75", nil })

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-restart-flag"}}
	c.Request = httptest.NewRequest(
		"POST",
		"/workspaces/ws-restart-flag/plugins",
		bytes.NewBufferString(body),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	h.Install(c)

	// The restart is scheduled via globalGoAsync — drain it before reading
	// the counter so "did not fire" means "will never fire", not "hasn't
	// fired yet" (RFC internal#524 Layer 1 drain contract).
	drainTestAsync()

	return w, atomic.LoadInt32(&restarts)
}

func decodeRestarting(t *testing.T, w *httptest.ResponseRecorder) bool {
	t.Helper()
	var resp struct {
		Status     string `json:"status"`
		Restarting *bool  `json:"restarting"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, w.Body.String())
	}
	if resp.Status != "installed" {
		t.Fatalf("status = %q, want installed (body=%s)", resp.Status, w.Body.String())
	}
	if resp.Restarting == nil {
		t.Fatalf("response missing \"restarting\" field (body=%s)", w.Body.String())
	}
	return *resp.Restarting
}

func TestPluginInstall_RestartDefaultOn(t *testing.T) {
	w, restarts := driveInstall(t, `{"source":"local://browser-automation"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if restarts != 1 {
		t.Errorf("restartFunc fired %d times, want 1 (absent restart field must keep the historical auto-restart)", restarts)
	}
	if !decodeRestarting(t, w) {
		t.Errorf("restarting = false, want true for the default install")
	}
}

func TestPluginInstall_RestartExplicitTrue(t *testing.T) {
	w, restarts := driveInstall(t, `{"source":"local://browser-automation","restart":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if restarts != 1 {
		t.Errorf("restartFunc fired %d times, want 1 for restart:true", restarts)
	}
	if !decodeRestarting(t, w) {
		t.Errorf("restarting = false, want true for restart:true")
	}
}

func TestPluginInstall_RestartFalseSuppressed(t *testing.T) {
	w, restarts := driveInstall(t, `{"source":"local://browser-automation","restart":false}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if restarts != 0 {
		t.Errorf("restartFunc fired %d times, want 0 — restart:false must deliver+record ONLY", restarts)
	}
	if decodeRestarting(t, w) {
		t.Errorf("restarting = true, want false for restart:false")
	}
}
