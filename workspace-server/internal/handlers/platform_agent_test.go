package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// TestInstallPlatformAgent_BadJSON rejects a payload missing the required id
// before touching the DB (binding:"required" on ID).
func TestInstallPlatformAgent_BadJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/admin/org/platform-agent",
		bytes.NewBufferString(`{"name":"Org Concierge"}`)) // no id
	c.Request.Header.Set("Content-Type", "application/json")

	InstallPlatformAgent(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("missing id: expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

// TestDefaultPlatformAgentName covers the dynamic "<org name> Agent" name and
// the legacy fallback. MOLECULE_ORG_NAME set → "<org> Agent"; unset → the
// "Org Concierge" default used by both the self-host seed and the CP install
// when no explicit name is passed.
func TestDefaultPlatformAgentName(t *testing.T) {
	t.Run("org name set", func(t *testing.T) {
		t.Setenv("MOLECULE_ORG_NAME", "Molecule AI")
		if got := defaultPlatformAgentName(); got != "Molecule AI Agent" {
			t.Errorf("defaultPlatformAgentName() = %q, want %q", got, "Molecule AI Agent")
		}
	})
	t.Run("org name empty → legacy fallback", func(t *testing.T) {
		t.Setenv("MOLECULE_ORG_NAME", "")
		if got := defaultPlatformAgentName(); got != "Org Concierge" {
			t.Errorf("defaultPlatformAgentName() = %q, want %q", got, "Org Concierge")
		}
	})
}

// TestOrgIdentity asserts the open /org/identity contract: {"name": <env>}.
func TestOrgIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("returns configured org name, slug and id (SaaS)", func(t *testing.T) {
		t.Setenv("MOLECULE_ORG_NAME", "Molecule AI")
		t.Setenv("MOLECULE_ORG_SLUG", "molecule-ai")
		t.Setenv("MOLECULE_ORG_ID", "11111111-2222-3333-4444-555555555555")
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/org/identity", nil)

		OrgIdentity(c)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		var body struct {
			Name  string `json:"name"`
			Slug  string `json:"slug"`
			OrgID string `json:"org_id"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal: %v (%s)", err, w.Body.String())
		}
		if body.Name != "Molecule AI" {
			t.Errorf("name = %q, want %q", body.Name, "Molecule AI")
		}
		if body.Slug != "molecule-ai" {
			t.Errorf("slug = %q, want %q", body.Slug, "molecule-ai")
		}
		if body.OrgID != "11111111-2222-3333-4444-555555555555" {
			t.Errorf("org_id = %q, want the configured uuid", body.OrgID)
		}
	})

	t.Run("name/slug/org_id empty when unset (self-host)", func(t *testing.T) {
		t.Setenv("MOLECULE_ORG_NAME", "")
		t.Setenv("MOLECULE_ORG_SLUG", "")
		t.Setenv("MOLECULE_ORG_ID", "")
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/org/identity", nil)

		OrgIdentity(c)

		var body struct {
			Name  string `json:"name"`
			Slug  string `json:"slug"`
			OrgID string `json:"org_id"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if body.Name != "" {
			t.Errorf("name = %q, want empty string", body.Name)
		}
		if body.Slug != "" {
			t.Errorf("slug = %q, want empty string", body.Slug)
		}
		if body.OrgID != "" {
			t.Errorf("org_id = %q, want empty string", body.OrgID)
		}
	})

	// platform_managed_available reflects whether a Molecule LLM proxy is wired
	// into the process env — true on SaaS (proxy base URL + usage token set),
	// false on self-host (neither set). The canvas reads it to hide/show the
	// "Platform (proxy)" billing option pre-login.
	t.Run("platform_managed_available true when proxy configured (SaaS)", func(t *testing.T) {
		t.Setenv("MOLECULE_LLM_BASE_URL", "https://proxy.example/v1")
		t.Setenv("MOLECULE_LLM_USAGE_TOKEN", "tok-test")
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/org/identity", nil)

		OrgIdentity(c)

		var body struct {
			PlatformManagedAvailable bool `json:"platform_managed_available"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal: %v (%s)", err, w.Body.String())
		}
		if !body.PlatformManagedAvailable {
			t.Errorf("platform_managed_available = false, want true (proxy configured)")
		}
	})

	t.Run("platform_managed_available false when no proxy (self-host)", func(t *testing.T) {
		// Clear every proxy env so neither the molecule nor openai alias is set.
		t.Setenv("MOLECULE_LLM_BASE_URL", "")
		t.Setenv("MOLECULE_LLM_USAGE_TOKEN", "")
		t.Setenv("OPENAI_BASE_URL", "")
		t.Setenv("OPENAI_API_KEY", "")
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/org/identity", nil)

		OrgIdentity(c)

		var body struct {
			PlatformManagedAvailable bool `json:"platform_managed_available"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal: %v (%s)", err, w.Body.String())
		}
		if body.PlatformManagedAvailable {
			t.Errorf("platform_managed_available = true, want false (no proxy / self-host)")
		}
	})

	t.Run("platform_managed_available true via openai alias env", func(t *testing.T) {
		// The proxy can also be wired via the OPENAI_* aliases (non-anthropic
		// runtimes). Either pair satisfies the signal.
		t.Setenv("MOLECULE_LLM_BASE_URL", "")
		t.Setenv("MOLECULE_LLM_USAGE_TOKEN", "")
		t.Setenv("OPENAI_BASE_URL", "https://proxy.example/v1")
		t.Setenv("OPENAI_API_KEY", "tok-test")
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/org/identity", nil)

		OrgIdentity(c)

		var body struct {
			PlatformManagedAvailable bool `json:"platform_managed_available"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal: %v (%s)", err, w.Body.String())
		}
		if !body.PlatformManagedAvailable {
			t.Errorf("platform_managed_available = false, want true (openai alias proxy env)")
		}
	})
}

// stubBootProv is a minimal localProvisionerIsRunning for the boot-provision
// helper test — no Docker daemon required.
type stubBootProv struct {
	running    bool
	calledWith string
}

func (s *stubBootProv) IsRunning(_ context.Context, id string) (bool, error) {
	s.calledWith = id
	return s.running, nil
}

const bootPlatformID = "11111111-2222-3333-4444-555555555555"

// TestMaybeProvisionPlatformAgentOnBoot_KicksOffWhenNotRunning: row present +
// container not running ⇒ RestartByID is invoked with the platform agent's id.
func TestMaybeProvisionPlatformAgentOnBoot_KicksOffWhenNotRunning(t *testing.T) {
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT id, status FROM workspaces WHERE kind = 'platform'`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow(bootPlatformID, "failed"))

	prov := &stubBootProv{running: false}
	done := make(chan string, 1)
	MaybeProvisionPlatformAgentOnBoot(context.Background(), db.DB, prov, func(id string) {
		done <- id
	})

	select {
	case got := <-done:
		if got != bootPlatformID {
			t.Errorf("RestartByID called with %q, want %q", got, bootPlatformID)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("RestartByID was not called within timeout")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestMaybeProvisionPlatformAgentOnBoot_SkipsWhenRunning: container already
// running ⇒ RestartByID is NOT called.
func TestMaybeProvisionPlatformAgentOnBoot_SkipsWhenRunning(t *testing.T) {
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT id, status FROM workspaces WHERE kind = 'platform'`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow(bootPlatformID, "online"))

	prov := &stubBootProv{running: true}
	called := make(chan string, 1)
	MaybeProvisionPlatformAgentOnBoot(context.Background(), db.DB, prov, func(id string) {
		called <- id
	})

	select {
	case got := <-called:
		t.Fatalf("RestartByID should not have been called, got %q", got)
	case <-time.After(200 * time.Millisecond):
		// expected: no call
	}
}

// TestMaybeProvisionPlatformAgentOnBoot_NoRowNoOp: no platform agent row ⇒
// no provision, no panic.
func TestMaybeProvisionPlatformAgentOnBoot_NoRowNoOp(t *testing.T) {
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT id, status FROM workspaces WHERE kind = 'platform'`).
		WillReturnError(sql.ErrNoRows)

	prov := &stubBootProv{running: false}
	called := make(chan string, 1)
	MaybeProvisionPlatformAgentOnBoot(context.Background(), db.DB, prov, func(id string) {
		called <- id
	})

	select {
	case got := <-called:
		t.Fatalf("RestartByID should not have been called, got %q", got)
	case <-time.After(200 * time.Millisecond):
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestMaybeProvisionPlatformAgentOnBoot_NilGuards: nil prov or nil restartFn ⇒
// no-op (no DB access, no panic).
func TestMaybeProvisionPlatformAgentOnBoot_NilGuards(t *testing.T) {
	mock := setupTestDB(t)
	// No ExpectQuery — the helper must return before touching the DB.
	MaybeProvisionPlatformAgentOnBoot(context.Background(), db.DB, nil, func(string) {})
	MaybeProvisionPlatformAgentOnBoot(context.Background(), db.DB, &stubBootProv{}, nil)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations (should have made no queries): %v", err)
	}
}
