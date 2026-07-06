package handlers

// plugins_backend_dispatch_test.go — regression coverage for the
// molecules-server (local-docker) PLUGIN install/uninstall parity fix.
//
// Root cause (core#182 tie-in): the CP local-docker provisioner persists the
// workspace's CONTAINER NAME into workspaces.instance_id (e.g.
// "mol-ws-<slug>-<hex>"; local_docker_workspace.go returns
// WorkspaceInstance{InstanceID: name}). The plugins handler dispatched on
// `instance_id != ""` and sent EVERY workspace with a non-empty instance_id
// down the AWS-only EC2-Instance-Connect (EIC) SSH path. On a local-docker
// tenant that has no AWS creds, EIC hangs and the request 90-120s-times-out →
// 502 — blocking the Lark-channel live test and every plugin install on
// local-docker tenants.
//
// The fix gates the EIC branch on isEC2InstanceID (a real "i-<hex>" id), so a
// container-name instance_id falls through to the docker path (or, with no
// docker client wired, fails LOUD with 503 instead of the silent 90s EIC
// timeout). This mirrors the Files API fix in files_backend_dispatch.go.
//
// These tests are deterministic and require NO live Docker/AWS/staging: they
// stub installPluginViaEIC / uninstallPluginViaEIC to fail the test loudly if
// they are ever reached for a local-docker container-name instance_id. They
// FAIL against pre-fix main (which routes mol-ws-* to EIC) and PASS after.

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// localDockerInstanceID is the container-name shape the CP local-docker
// provisioner persists into workspaces.instance_id (verified against the live
// self-host DB in files_backend_dispatch.go). Non-empty, NOT an "i-<hex>" id.
const localDockerInstanceID = "mol-ws-test5-a9f3044fa3f2"

// TestPluginInstall_LocalDocker_DoesNotHitEIC is the load-bearing regression:
// a workspace whose instance_id is a local-docker CONTAINER NAME must NOT push
// the plugin over the AWS EIC tunnel. installPluginViaEIC is stubbed to fail
// the test if it is ever called. With no docker client wired (docker-less
// tenant, #206) the docker-push is RETIRED, so the install delivers by PULL
// (declare + re-materialize) — NOT a 90s EIC hang, NOT the old 503 dead-end.
//
// Pre-fix this test FAILS: instance_id != "" routed straight into
// installPluginViaEIC (the stub fires t.Errorf).
func TestPluginInstall_LocalDocker_DoesNotHitEIC(t *testing.T) {
	registry := t.TempDir()
	stagePluginRegistry(t, registry, "browser-automation")

	stubInstallPluginViaEIC(t, func(_ context.Context, instanceID, _, _, _ string) error {
		t.Errorf("installPluginViaEIC called with instanceID=%q for a local-docker workspace; "+
			"EIC must be gated on a real EC2 id (isEC2InstanceID)", instanceID)
		return nil
	})

	mock, cleanup := withMockDB(t)
	defer cleanup()
	expectAllowlistAllowAll(mock)
	mock.ExpectExec(`INSERT INTO workspace_declared_plugins`).WillReturnResult(sqlmock.NewResult(0, 1))

	// docker == nil (CP-provisioner mode, no reachable daemon in this unit test).
	h := NewPluginsHandler(registry, nil, nil).
		WithRuntimeLookup(func(string) (string, error) { return "claude-code", nil }).
		WithInstanceIDLookup(func(string) (string, error) { return localDockerInstanceID, nil })

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "c7244ed9-f623-4cba-8873-020e5c9fe104"}}
	c.Request = httptest.NewRequest(
		"POST",
		"/workspaces/c7244ed9-f623-4cba-8873-020e5c9fe104/plugins",
		bytes.NewBufferString(`{"source":"local://browser-automation"}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	h.Install(c)

	// The docker-less tenant delivers by PULL (200), not EIC, not 503. The key
	// assertion remains the EIC stub above (must never fire).
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (docker-less pull), got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"delivery":"pull"`) {
		t.Errorf("body should report pull delivery; got %s", w.Body.String())
	}
}

// TestPluginUninstall_LocalDocker_DoesNotHitEIC is the uninstall sibling:
// a container-name instance_id must NOT reach uninstallPluginViaEIC.
func TestPluginUninstall_LocalDocker_DoesNotHitEIC(t *testing.T) {
	stubUninstallPluginViaEIC(t, func(_ context.Context, instanceID, _, _ string) error {
		t.Errorf("uninstallPluginViaEIC called with instanceID=%q for a local-docker workspace; "+
			"EIC must be gated on a real EC2 id (isEC2InstanceID)", instanceID)
		return nil
	})

	// docker == nil; instance_id is a local-docker container name.
	h := NewPluginsHandler(t.TempDir(), nil, nil).
		WithRuntimeLookup(func(string) (string, error) { return "claude-code", nil }).
		WithInstanceIDLookup(func(string) (string, error) { return localDockerInstanceID, nil })

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "c7244ed9-f623-4cba-8873-020e5c9fe104"},
		{Key: "name", Value: "browser-automation"},
	}
	c.Request = httptest.NewRequest(
		"DELETE",
		"/workspaces/c7244ed9-f623-4cba-8873-020e5c9fe104/plugins/browser-automation",
		nil,
	)

	h.Uninstall(c)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 (local-docker, no docker client wired), got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "local-docker") {
		t.Errorf("503 body should explain the local-docker no-docker-client case; got %s", w.Body.String())
	}
}
