package handlers

// files_backend_dispatch_test.go — regression coverage for the
// molecules-server (local-docker) Files API parity fix.
//
// Root cause: the CP local-docker provisioner persists the workspace's
// CONTAINER NAME into workspaces.instance_id (e.g. "mol-ws-<slug>-<hex>"),
// so the Files API's `instanceID != ""` dispatch wrongly routed local-docker
// workspaces down the AWS-only EIC SSH tunnel and 500'd with
// "eic tunnel setup: send-ssh-public-key: ... context deadline exceeded".
// The fix gates the EIC branch on isEC2InstanceID (a real "i-<hex>" id), so a
// container-name instance_id falls through to the docker-exec / host path.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// TestIsEC2InstanceID pins the backend discriminator: real EC2 ids (and the
// "i-..." fixtures the EIC dispatch tests use) are AWS; local-docker
// container names and the empty string are NOT.
func TestIsEC2InstanceID(t *testing.T) {
	cases := []struct {
		id   string
		want bool
	}{
		// Real AWS EC2 ids → EIC.
		{"i-0e0951a3cfd9bbf75", true},
		{"i-0abc1234", true},
		// EIC-dispatch test fixtures used across the suite → must stay EIC.
		{"i-test", true},
		{"i-abc123", true},
		{"i-test-1", true},
		// molecules-server (local-docker) container-name instance_ids → docker.
		{"mol-ws-test5-a9f3044fa3f2", false},
		{"mol-ws-agents-team-cd62fe70582d", false},
		{"reader-agent", false},
		// External / unprovisioned workspaces → not EIC (unchanged behavior).
		{"", false},
		{"i-", false}, // prefix only, never a real id or container name
	}
	for _, tc := range cases {
		if got := isEC2InstanceID(tc.id); got != tc.want {
			t.Errorf("isEC2InstanceID(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

// TestReadFile_LocalDocker_DoesNotHitEIC is the core regression: a workspace
// whose instance_id is a local-docker CONTAINER NAME (non-empty, not "i-...")
// must NOT dispatch to the EIC tunnel. withEICTunnel is stubbed to fail the
// test loudly if it is ever called. With no docker client wired, the read
// falls through to the host-side template dir and returns config.yaml — the
// exact path the template-delivery gate exercises when it reads config.yaml
// on a molecules-server tenant.
//
// Pre-fix this test would FAIL: instance_id != "" routed straight into
// readFileViaEIC → withEICTunnel (the stub would fire t.Errorf), and the
// handler returned 500 instead of the file.
func TestReadFile_LocalDocker_DoesNotHitEIC(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	tmpDir := t.TempDir()
	tmplDir := filepath.Join(tmpDir, "reader-agent")
	if err := os.MkdirAll(tmplDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmplDir, "config.yaml"), []byte("name: Reader Agent\ntier: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	handler := NewTemplatesHandler(tmpDir, nil, nil)

	// The molecules-server container-name shape the CP local-docker
	// provisioner persists (local_docker_workspace.go returns InstanceID=name).
	mock.ExpectQuery(`SELECT name, COALESCE\(instance_id, ''\), COALESCE\(runtime, ''\) FROM workspaces WHERE id =`).
		WithArgs("ws-localdocker").
		WillReturnRows(sqlmock.NewRows([]string{"name", "instance_id", "runtime"}).
			AddRow("Reader Agent", "mol-ws-reader-agent-a9f3044fa3f2", "claude-code"))

	// Fail loudly if the EIC tunnel is ever entered for this local-docker id.
	prev := withEICTunnel
	withEICTunnel = func(ctx context.Context, instanceID string, fn func(s eicSSHSession) error) error {
		t.Errorf("withEICTunnel called with instanceID=%q for a local-docker workspace; EIC must be gated on a real EC2 id", instanceID)
		return errors.New("EIC must not be reached for local-docker")
	}
	defer func() { withEICTunnel = prev }()

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-localdocker"},
		{Key: "path", Value: "/config.yaml"},
	}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-localdocker/files/config.yaml", nil)

	handler.ReadFile(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (host-template fallback, EIC skipped), got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not JSON: %v (body=%s)", err, w.Body.String())
	}
	if resp["path"] != "config.yaml" {
		t.Errorf("expected path 'config.yaml', got %v", resp["path"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}
