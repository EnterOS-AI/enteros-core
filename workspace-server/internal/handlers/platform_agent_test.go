package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
// helper test — no Docker daemon required. It deliberately does NOT implement
// ExecRead, so conciergeIdentityPresent's type-assertion misses and a running
// container is treated as already-identified (skip) — the legacy behaviour.
type stubBootProv struct {
	running    bool
	calledWith string
}

func (s *stubBootProv) IsRunning(_ context.Context, id string) (bool, error) {
	s.calledWith = id
	return s.running, nil
}

// stubBootProvExec adds ExecRead so the boot helper can probe for the concierge
// identity on a RUNNING container — the path that restarts a running-but-vanilla
// concierge so it picks up the seeded overlay.
type stubBootProvExec struct {
	stubBootProv
	systemPrompt string // returned for /configs/system-prompt.md; "" with execErr to simulate a probe miss
	execErr      error
}

func (s *stubBootProvExec) ExecRead(_ context.Context, _ /*container*/, _ /*path*/ string) ([]byte, error) {
	if s.execErr != nil {
		return nil, s.execErr
	}
	return []byte(s.systemPrompt), nil
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

// TestMaybeProvisionPlatformAgentOnBoot_RestartsRunningButVanilla: a RUNNING
// concierge whose /configs/system-prompt.md lacks the identity (a pre-overlay
// boot) is restarted ONCE so the provision path re-seeds the concierge config.
func TestMaybeProvisionPlatformAgentOnBoot_RestartsRunningButVanilla(t *testing.T) {
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT id, status FROM workspaces WHERE kind = 'platform'`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow(bootPlatformID, "online"))

	// Running, but ExecRead of system-prompt.md returns vanilla content (no
	// "Org Concierge") → identity absent → restart.
	prov := &stubBootProvExec{stubBootProv: stubBootProv{running: true}, systemPrompt: "generic coding assistant"}
	done := make(chan string, 1)
	MaybeProvisionPlatformAgentOnBoot(context.Background(), db.DB, prov, func(id string) { done <- id })

	select {
	case got := <-done:
		if got != bootPlatformID {
			t.Errorf("RestartByID called with %q, want %q", got, bootPlatformID)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("RestartByID was not called for a running-but-vanilla concierge")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestMaybeProvisionPlatformAgentOnBoot_SkipsRunningWithIdentity: a RUNNING
// concierge that already carries the Org-Concierge identity is left alone.
func TestMaybeProvisionPlatformAgentOnBoot_SkipsRunningWithIdentity(t *testing.T) {
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT id, status FROM workspaces WHERE kind = 'platform'`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow(bootPlatformID, "online"))

	prov := &stubBootProvExec{stubBootProv: stubBootProv{running: true}, systemPrompt: "# You are Molecule AI Agent — the Org Concierge"}
	called := make(chan string, 1)
	MaybeProvisionPlatformAgentOnBoot(context.Background(), db.DB, prov, func(id string) { called <- id })

	select {
	case got := <-called:
		t.Fatalf("RestartByID should not have been called (identity present), got %q", got)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestConciergeIdentityFiles asserts the overlay: a system-prompt.md carrying
// the Org-Concierge identity, and a config.yaml that gains the platform
// mcp_servers entry — appended idempotently onto the base config.
func TestConciergeIdentityFiles(t *testing.T) {
	base := []byte("name: \"Org Concierge\"\nruntime: claude-code\nmodel: \"sonnet\"\n")
	files := conciergeIdentityFiles("Molecule AI Agent", base)

	sp, ok := files["system-prompt.md"]
	if !ok {
		t.Fatal("overlay missing system-prompt.md")
	}
	for _, want := range []string{"Molecule AI Agent", "Org Concierge", "platform agent", "delegate", "approv"} {
		if !strings.Contains(string(sp), want) {
			t.Errorf("system-prompt.md missing %q", want)
		}
	}

	cfg, ok := files["config.yaml"]
	if !ok {
		t.Fatal("overlay missing config.yaml (mcp_servers should have been appended)")
	}
	// Pins the REAL image contract (pilot RCA 2026-06-10): the bin on PATH
	// + management mode — NOT the /opt node path the image never shipped,
	// and NOT default (a2a) mode which has zero admin tools.
	for _, want := range []string{"mcp_servers:", "name: platform", "command: molecule-platform-mcp", "MOLECULE_MCP_MODE: management", "runtime: claude-code"} {
		if !strings.Contains(string(cfg), want) {
			t.Errorf("config.yaml missing %q\n--- got ---\n%s", want, cfg)
		}
	}
	if strings.Contains(string(cfg), "/opt/molecule-mcp-server") {
		t.Error("stale /opt path resurfaced — the image ships the molecule-mcp bin, not /opt/molecule-mcp-server")
	}

	// The standalone fragment ships ALWAYS, carrying the same declaration —
	// the base-independent path that survives the SaaS restart-provision
	// (where no base config is resolvable).
	frag, ok := files[conciergeMCPFragmentFile]
	if !ok {
		t.Fatalf("overlay missing %s (the base-independent MCP declaration)", conciergeMCPFragmentFile)
	}
	for _, want := range []string{"name: platform", "command: molecule-platform-mcp", "MOLECULE_MCP_MODE: management"} {
		if !strings.Contains(string(frag), want) {
			t.Errorf("%s missing %q", conciergeMCPFragmentFile, want)
		}
	}

	// Idempotent: re-applying onto an already-patched config does NOT add a
	// second mcp_servers block and does NOT emit a config.yaml overlay (nothing
	// to change), so the count of "mcp_servers:" stays exactly one.
	files2 := conciergeIdentityFiles("Molecule AI Agent", cfg)
	if _, present := files2["config.yaml"]; present {
		t.Error("re-apply should NOT re-emit config.yaml when mcp_servers is already present")
	}
	if n := strings.Count(string(cfg), "mcp_servers:"); n != 1 {
		t.Errorf("mcp_servers: appears %d times, want exactly 1", n)
	}

	// No base config (couldn't read one): identity still lands; no config.yaml
	// — but the fragment STILL ships, so the MCP declaration reaches the
	// container even when every base resolution misses (the exact SaaS
	// restart-provision gap that booted the pilot concierge toolless).
	only := conciergeIdentityFiles("Org Concierge", nil)
	if _, present := only["system-prompt.md"]; !present {
		t.Error("system prompt must land even with no base config")
	}
	if _, present := only["config.yaml"]; present {
		t.Error("no config.yaml overlay when there is no base to append onto")
	}
	if _, present := only[conciergeMCPFragmentFile]; !present {
		t.Errorf("%s must ship even with no base config", conciergeMCPFragmentFile)
	}
}

// TestConciergePlatformMCPEnv asserts the platform-MCP env wiring: ADMIN_TOKEN →
// MOLECULE_API_KEY, PLATFORM_URL → MOLECULE_API_URL fallback, and that an
// already-present value is never clobbered.
func TestConciergePlatformMCPEnv(t *testing.T) {
	t.Run("wires from ADMIN_TOKEN + PLATFORM_URL", func(t *testing.T) {
		t.Setenv("ADMIN_TOKEN", "admintok")
		t.Setenv("MOLECULE_API_URL", "")
		t.Setenv("PLATFORM_URL", "http://platform:8080")
		t.Setenv("MOLECULE_ORG_ID", "org-123")
		env := map[string]string{}
		conciergePlatformMCPEnv(env)
		if env["MOLECULE_API_KEY"] != "admintok" {
			t.Errorf("MOLECULE_API_KEY = %q, want admintok", env["MOLECULE_API_KEY"])
		}
		if env["MOLECULE_API_URL"] != "http://platform:8080" {
			t.Errorf("MOLECULE_API_URL = %q, want platform url fallback", env["MOLECULE_API_URL"])
		}
		if env["MOLECULE_ORG_ID"] != "org-123" {
			t.Errorf("MOLECULE_ORG_ID = %q, want org-123", env["MOLECULE_ORG_ID"])
		}
	})

	t.Run("does not clobber existing values", func(t *testing.T) {
		t.Setenv("ADMIN_TOKEN", "admintok")
		env := map[string]string{"MOLECULE_API_KEY": "preset"}
		conciergePlatformMCPEnv(env)
		if env["MOLECULE_API_KEY"] != "preset" {
			t.Errorf("MOLECULE_API_KEY overwritten to %q, want preset preserved", env["MOLECULE_API_KEY"])
		}
	})

	t.Run("MOLECULE_API_URL prefers explicit over PLATFORM_URL", func(t *testing.T) {
		t.Setenv("MOLECULE_API_URL", "http://explicit:9000")
		t.Setenv("PLATFORM_URL", "http://platform:8080")
		env := map[string]string{}
		conciergePlatformMCPEnv(env)
		if env["MOLECULE_API_URL"] != "http://explicit:9000" {
			t.Errorf("MOLECULE_API_URL = %q, want the explicit env", env["MOLECULE_API_URL"])
		}
	})
}

// TestApplyConciergeProvisionConfig_OnlyPlatformGetsOrgMCP locks the security
// invariant the user requires: ONLY the tenant-native concierge (kind='platform')
// receives the org/platform MCP + the org-admin token. An ordinary workspace must
// NOT get the platform MCP config, the system prompt, or MOLECULE_API_KEY (the
// org-admin credential) natively — otherwise any workspace could drive org-admin
// actions (create_workspace, set_secret, …). Gate is keyed off the DB kind column
// (SSOT, protected by the one-platform-root CHECK constraint).
func TestApplyConciergeProvisionConfig_OnlyPlatformGetsOrgMCP(t *testing.T) {
	t.Setenv("ADMIN_TOKEN", "secret-org-admin")
	t.Setenv("PLATFORM_URL", "http://platform:8080")
	h := &WorkspaceHandler{}
	const kindQuery = `SELECT COALESCE\(kind, 'workspace'\) FROM workspaces WHERE id =`

	t.Run("ordinary workspace gets NO org MCP and NO admin token", func(t *testing.T) {
		mock := setupTestDB(t)
		mock.ExpectQuery(kindQuery).WithArgs("ws-ordinary").
			WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("workspace"))
		env := map[string]string{}
		cf := map[string][]byte{"config.yaml": []byte("runtime: claude-code\n")}
		out := h.applyConciergeProvisionConfig(context.Background(), "ws-ordinary", "", cf, env, "Worker")
		if _, ok := env["MOLECULE_API_KEY"]; ok {
			t.Errorf("SECURITY: ordinary workspace leaked MOLECULE_API_KEY (org-admin token): %v", env)
		}
		if _, ok := env["MOLECULE_ORG_API_KEY"]; ok {
			t.Errorf("SECURITY: ordinary workspace leaked MOLECULE_ORG_API_KEY: %v", env)
		}
		if _, ok := out["system-prompt.md"]; ok {
			t.Error("ordinary workspace was given the concierge system prompt")
		}
		if strings.Contains(string(out["config.yaml"]), "mcp_servers") {
			t.Error("SECURITY: ordinary workspace was given the platform mcp_servers config")
		}
		if _, ok := out[conciergeMCPFragmentFile]; ok {
			t.Errorf("SECURITY: ordinary workspace was given %s", conciergeMCPFragmentFile)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
	})

	t.Run("platform agent DOES get the org MCP and admin token", func(t *testing.T) {
		mock := setupTestDB(t)
		mock.ExpectQuery(kindQuery).WithArgs("ws-concierge").
			WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("platform"))
		env := map[string]string{}
		cf := map[string][]byte{"config.yaml": []byte("runtime: claude-code\n")}
		out := h.applyConciergeProvisionConfig(context.Background(), "ws-concierge", "", cf, env, "Molecule AI Agent")
		if env["MOLECULE_API_KEY"] != "secret-org-admin" {
			t.Errorf("concierge did not receive the org-admin token; env=%v", env)
		}
		if env["MOLECULE_ORG_API_KEY"] != "secret-org-admin" {
			t.Errorf("management tools auth env (MOLECULE_ORG_API_KEY) missing; env=%v", env)
		}
		if _, ok := out["system-prompt.md"]; !ok {
			t.Error("concierge did not receive the system prompt")
		}
		if !strings.Contains(string(out["config.yaml"]), "mcp_servers") {
			t.Error("concierge did not receive the platform mcp_servers config")
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
	})
}

// TestConciergeDeclaredModelIsRegistered is the fail-closed-at-CI guard for
// core#2594: the platform agent's declared model MUST stay registered for its
// runtime. If a registry/providers.yaml change ever drops it, this test (not a
// silent prod fallback) catches it — ensureConciergeModel leaves the model
// unset on a validation miss, which then fails the provision closed.
func TestConciergeDeclaredModelIsRegistered(t *testing.T) {
	if ok, why := validateRegisteredModelForRuntime(conciergeRuntime, conciergeDeclaredModel); !ok {
		t.Fatalf("concierge declared model %q is NOT registered for runtime %q: %s",
			conciergeDeclaredModel, conciergeRuntime, why)
	}
	if ok, why := validateDerivedProviderInRegistry(conciergeRuntime, conciergeDeclaredModel); !ok {
		t.Fatalf("concierge declared model %q has no derivable registry provider for runtime %q: %s",
			conciergeDeclaredModel, conciergeRuntime, why)
	}
}

// TestEnsureConciergeModel_SeedsEnvAndPersistsWhenAbsent verifies ensureConciergeModel
// seeds the container model env AND writes the MODEL secret when none is stored
// (core#2594). The SELECT returns no row → it must INSERT the declared model.
func TestEnsureConciergeModel_SeedsEnvAndPersistsWhenAbsent(t *testing.T) {
	mock := setupTestDB(t)
	const wsID = "concierge-ws-1"

	// No stored MODEL yet.
	mock.ExpectQuery(`SELECT encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = \$1 AND key = 'MODEL'`).
		WithArgs(wsID).
		WillReturnError(sql.ErrNoRows)
	// setModelSecret upserts the declared model.
	mock.ExpectExec(`INSERT INTO workspace_secrets`).
		WithArgs(wsID, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	h := &WorkspaceHandler{}
	envVars := map[string]string{}
	h.ensureConciergeModel(context.Background(), wsID, envVars)

	if got := envVars["MODEL"]; got != conciergeDeclaredModel {
		t.Errorf("MODEL env = %q, want %q", got, conciergeDeclaredModel)
	}
	if got := envVars["MOLECULE_MODEL"]; got != conciergeDeclaredModel {
		t.Errorf("MOLECULE_MODEL env = %q, want %q", got, conciergeDeclaredModel)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestEnsureConciergeModel_RespectsExistingModel is the SEED-ONLY regression
// guard (CTO 2026-06-12): when a MODEL secret already exists — ESPECIALLY a
// DIFFERENT, customer-picked one (e.g. they switched the concierge to
// kimi-for-coding for BYOK) — ensureConciergeModel must NOT touch it: no write,
// and it must NOT force the declared default back into the env. Pre-fix it
// re-asserted conciergeDeclaredModel on every provision, silently reverting the
// customer's choice. encryption_version=0 = raw bytes (crypto disabled in test).
func TestEnsureConciergeModel_RespectsExistingModel(t *testing.T) {
	mock := setupTestDB(t)
	const wsID = "concierge-ws-2"
	const customerModel = "kimi-for-coding" // the customer's explicit pick

	mock.ExpectQuery(`SELECT encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = \$1 AND key = 'MODEL'`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}).
			AddRow([]byte(customerModel), 0))
	// NO ExpectExec — any write would be an unmet/unexpected expectation.

	h := &WorkspaceHandler{}
	envVars := map[string]string{}
	h.ensureConciergeModel(context.Background(), wsID, envVars)

	// Must NOT have overwritten the env with the declared default — the customer's
	// stored model wins and is wired by loadWorkspaceSecrets/applyRuntimeModelEnv,
	// not by this seed-only helper.
	if got := envVars["MODEL"]; got == conciergeDeclaredModel {
		t.Errorf("MODEL env was forced to the declared default %q — must respect the customer's stored %q", conciergeDeclaredModel, customerModel)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}
