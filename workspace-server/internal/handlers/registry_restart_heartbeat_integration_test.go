//go:build integration
// +build integration

// registry_restart_heartbeat_integration_test.go — real-Postgres proof, through
// the real Heartbeat handler, that a heartbeat promotes a provisioning row to
// online only when the row has a URL (see "A HEARTBEAT FROM A POD BEING
// STOPPED" in registry.go). A restart clears the URL before it re-provisions;
// the pod it is stopping can still beat. Run as registry_auth_integration_test.go
// says (INTEGRATION_DB_URL, -tags=integration).

package handlers

import (
	"net/http"
	"testing"
)

func TestIntegration_Heartbeat_PromotesProvisioningOnlyWithARegisteredURL(t *testing.T) {
	conn := integrationAuthDB(t)
	setupTestRedis(t)
	h := NewRegistryHandler(newTestBroadcaster())

	// The row a restart left: provisioning, its URL cleared. The beat comes
	// from the pod being stopped; the new pod has not registered.
	stopping := insertWorkspace(t, conn, "restarting-ws", "provisioning", "")
	if w := postHeartbeat(t, h, stopping); w.Code != http.StatusOK {
		t.Fatalf("heartbeat = %d %s", w.Code, w.Body.String())
	}
	if got := statusOf(t, conn, stopping); got != "provisioning" {
		t.Fatalf("a heartbeat promoted a provisioning row without a URL to %q; it must wait for the new pod's registration", got)
	}

	// A row its pod registered (the URL is on it): the heartbeat promotes it.
	registered := insertWorkspaceWithURL(t, conn, "registered-ws", "provisioning", "http://ws-registered.example:8000")
	if w := postHeartbeat(t, h, registered); w.Code != http.StatusOK {
		t.Fatalf("heartbeat = %d %s", w.Code, w.Body.String())
	}
	if got := statusOf(t, conn, registered); got != "online" {
		t.Fatalf("a registered provisioning row stayed %q after a heartbeat, want online", got)
	}
}
