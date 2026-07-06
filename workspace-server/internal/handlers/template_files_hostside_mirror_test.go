package handlers

// Host-side /configs mirror read-back (#206 molecules-server).
//
// A molecules-server (local-docker) tenant runs docker.sock-less: the Files API
// can't docker-exec into the runtime container, and the AWS EIC path is EC2-only,
// so a /configs read fell through to the empty host-side TEMPLATE dir and 404'd
// with a misleading 59-byte "container offline, no template" body even though the
// config WAS delivered (config.yaml + prompts). These tests pin the fix: when a
// per-workspace host-side mirror exists (persisted by CPProvisioner at provision),
// ReadFile/ListFiles serve the REAL config from it; when it's genuinely missing a
// file, they fail loud + clear; and the mirror is scoped to root=/configs only.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

const hostSideRealConfig = "name: OpenClaw Agent\nruntime: openclaw\nmodel: minimax:MiniMax-M2.7\nprompt_files:\n  - prompts/concierge.md\n"

// seedHostSideMirror creates the per-workspace /configs mirror under baseDir and
// writes the given files (relpath -> content) into it. Returns the mirror dir.
func seedHostSideMirror(t *testing.T, baseDir, wsID string, files map[string]string) string {
	t.Helper()
	mirror := provisioner.HostSideConfigsDir(baseDir, wsID)
	if mirror == "" {
		t.Fatalf("HostSideConfigsDir returned empty for base=%q ws=%q", baseDir, wsID)
	}
	for rel, content := range files {
		dest := filepath.Join(mirror, rel)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(dest), err)
		}
		if err := os.WriteFile(dest, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", dest, err)
		}
	}
	return mirror
}

func expectWorkspaceRow(mock sqlmock.Sqlmock, wsID, name, instanceID, runtime string) {
	mock.ExpectQuery(`SELECT name, COALESCE\(instance_id, ''\), COALESCE\(runtime, ''\) FROM workspaces WHERE id =`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"name", "instance_id", "runtime"}).
			AddRow(name, instanceID, runtime))
}

// TestReadFile_HostSideMirror_ServesRealConfig: a docker-less molecules-server
// tenant with a populated mirror returns the REAL config.yaml, not the 59-byte
// "container offline, no template" stub.
func TestReadFile_HostSideMirror_ServesRealConfig(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	const wsID = "19723ad9-569a-5c02-be68-7f96fbcfd6c8"
	// instance_id is a container NAME (molecules-server), NOT an EC2 id — so the
	// handler stays on the docker/host path, not the EIC tunnel.
	expectWorkspaceRow(mock, wsID, "test2222 Agent", "mol-ws-test2222-19723ad9569a", "openclaw")

	base := t.TempDir()
	seedHostSideMirror(t, base, wsID, map[string]string{"config.yaml": hostSideRealConfig})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}, {Key: "path", Value: "/config.yaml"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/"+wsID+"/files/config.yaml?root=/configs", nil)

	// docker nil (molecules-server), hostStateDir wired to the mirror base.
	(&TemplatesHandler{hostStateDir: base}).ReadFile(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 from host-side mirror, got %d: %s", w.Code, w.Body.String())
	}
	var got struct {
		Path    string `json:"path"`
		Content string `json:"content"`
		Size    int    `json:"size"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad JSON: %v (body=%s)", err, w.Body.String())
	}
	if got.Content != hostSideRealConfig {
		t.Errorf("content mismatch:\n got %q\nwant %q", got.Content, hostSideRealConfig)
	}
	if got.Size != len(hostSideRealConfig) {
		t.Errorf("size = %d, want %d", got.Size, len(hostSideRealConfig))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestReadFile_HostSideMirror_MissingFileFailsLoud: mirror present (config WAS
// delivered) but the requested file isn't in it → a CLEAR 404, NOT the
// misleading "container offline, no template" stub.
func TestReadFile_HostSideMirror_MissingFileFailsLoud(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	const wsID = "ws-mirror-missing"
	expectWorkspaceRow(mock, wsID, "Agent", "mol-ws-x", "openclaw")

	base := t.TempDir()
	// Mirror dir exists (config.yaml present) but not the requested file.
	seedHostSideMirror(t, base, wsID, map[string]string{"config.yaml": hostSideRealConfig})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}, {Key: "path", Value: "/prompts/nope.md"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/"+wsID+"/files/prompts/nope.md?root=/configs", nil)

	(&TemplatesHandler{hostStateDir: base}).ReadFile(c)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if want := "file not found in workspace /configs"; !strings.Contains(body, want) {
		t.Errorf("expected clear %q error, got %s", want, body)
	}
	// It must NOT be the misleading legacy stub.
	if strings.Contains(body, "no template") {
		t.Errorf("must not return the misleading 'no template' stub; got %s", body)
	}
}

// TestReadFile_HostSideMirror_DisabledFallsThrough: with hostStateDir unset the
// handler preserves the legacy behavior (falls through to the template dir).
func TestReadFile_HostSideMirror_DisabledFallsThrough(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	const wsID = "ws-nomirror"
	expectWorkspaceRow(mock, wsID, "User Named Agent", "mol-ws-y", "openclaw")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}, {Key: "path", Value: "/config.yaml"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/"+wsID+"/files/config.yaml?root=/configs", nil)

	// hostStateDir empty + configsDir empty → legacy 59-byte stub preserved.
	(&TemplatesHandler{}).ReadFile(c)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "no template") {
		t.Errorf("expected legacy fall-through, got %s", w.Body.String())
	}
}

// TestReadFile_HostSideMirror_OnlyConfigsRoot: the mirror is authoritative ONLY
// for root=/configs. A /workspace read must NOT be served from it.
func TestReadFile_HostSideMirror_OnlyConfigsRoot(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	const wsID = "ws-root-scope"
	expectWorkspaceRow(mock, wsID, "Agent", "mol-ws-z", "openclaw")

	base := t.TempDir()
	seedHostSideMirror(t, base, wsID, map[string]string{"config.yaml": hostSideRealConfig})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}, {Key: "path", Value: "/config.yaml"}}
	// root=/workspace — NOT /configs, so the mirror must be bypassed.
	c.Request = httptest.NewRequest("GET", "/workspaces/"+wsID+"/files/config.yaml?root=/workspace", nil)

	(&TemplatesHandler{hostStateDir: base}).ReadFile(c)

	// No docker, no template dir, mirror not consulted for /workspace → legacy 404.
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 (mirror bypassed for /workspace), got %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "in workspace /configs") {
		t.Errorf("mirror must not serve /workspace reads; got %s", w.Body.String())
	}
}

// TestListFiles_HostSideMirror_ListsConfigs: a docker-less tenant lists the real
// /configs tree from the mirror instead of returning the empty "[]".
func TestListFiles_HostSideMirror_ListsConfigs(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	const wsID = "ws-list-mirror"
	expectWorkspaceRow(mock, wsID, "Agent", "mol-ws-list", "openclaw")

	base := t.TempDir()
	seedHostSideMirror(t, base, wsID, map[string]string{
		"config.yaml":         hostSideRealConfig,
		"prompts/concierge.md": "# persona",
	})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest("GET", "/workspaces/"+wsID+"/files?root=/configs&depth=2", nil)

	(&TemplatesHandler{hostStateDir: base}).ListFiles(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var got []struct {
		Path string `json:"path"`
		Size int64  `json:"size"`
		Dir  bool   `json:"dir"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad JSON: %v (body=%s)", err, w.Body.String())
	}
	var sawConfig, sawPrompt bool
	for _, e := range got {
		switch e.Path {
		case "config.yaml":
			sawConfig = true
		case filepath.FromSlash("prompts/concierge.md"):
			sawPrompt = true
		}
	}
	if !sawConfig {
		t.Errorf("expected config.yaml in listing; got %s", w.Body.String())
	}
	if !sawPrompt {
		t.Errorf("expected prompts/concierge.md in listing; got %s", w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}
