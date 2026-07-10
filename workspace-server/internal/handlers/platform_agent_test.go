package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
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
	// core#3496 D2: the kick-off first checks for a model signal — a MODEL
	// workspace_secret here (explicit, not the fail-open path) keeps this the
	// CONFIGURED-root kick-off test; the unconfigured-skip has its own test.
	mock.ExpectQuery(`SELECT 1 FROM workspace_secrets WHERE workspace_id = \$1 AND key = 'MODEL'`).
		WillReturnRows(sqlmock.NewRows([]string{"?column?"}).AddRow(1))

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

	// The running-but-vanilla path re-declares the management MCP plugin before
	// restarting so the post-restart boot-install sees the declaration.
	// The boot path calls seedTemplatePlugins → recordDeclaredPlugin, which runs
	// the SINGLE-column kind precheck (NOT applyConciergeProvisionConfig's 2-col
	// kind+runtime read), so the mock uses the 1-column shape here.
	const recordKindQuery = `SELECT COALESCE\(kind, 'workspace'\) FROM workspaces WHERE id =`
	mock.ExpectQuery(recordKindQuery).WithArgs(bootPlatformID).
		WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("platform"))
	mock.ExpectExec(`INSERT INTO workspace_declared_plugins`).
		WithArgs(bootPlatformID, conciergePlatformMCPName, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Running, but ExecRead of system-prompt.md returns a still-placeholdered
	// prompt ({{CONCIERGE_NAME}} not substituted) → identity absent → restart.
	prov := &stubBootProvExec{stubBootProv: stubBootProv{running: true}, systemPrompt: "# You are {{CONCIERGE_NAME}} — the Org Concierge"}
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
// TestSubstituteConciergeName asserts the {{CONCIERGE_NAME}} substitution
// used by applyConciergeProvisionConfig to bake the per-instance concierge
// name into the template-delivered system-prompt.md at provision time
// (RFC #2843 §10a; PR recommendation (a) — substitute, per the driver's
// "you decide in review" call). The runtime's build_system_prompt does NOT
// template prompt files, so this is the only place the per-instance name
// reaches the agent. Idempotent: re-substituting a name into a
// already-substituted prompt is a no-op (the placeholder is gone).
func TestSubstituteConciergeName(t *testing.T) {
	tmpl := []byte("# You are {{CONCIERGE_NAME}} — the Org Concierge\n\n" +
		"You are the organization's **platform agent**.\n")

	t.Run("replaces the placeholder with the per-instance name", func(t *testing.T) {
		got := substituteConciergeName(tmpl, "Molecule AI Agent")
		if !strings.Contains(string(got), "Molecule AI Agent") {
			t.Errorf("substituted prompt missing the name:\n%s", got)
		}
		if strings.Contains(string(got), "{{CONCIERGE_NAME}}") {
			t.Errorf("placeholder survived substitution:\n%s", got)
		}
	})

	t.Run("replaces all occurrences (not just the first)", func(t *testing.T) {
		multi := []byte("{{CONCIERGE_NAME}} sees {{CONCIERGE_NAME}} and only {{CONCIERGE_NAME}} acts.")
		got := substituteConciergeName(multi, "Mia")
		want := "Mia sees Mia and only Mia acts."
		if string(got) != want {
			t.Errorf("multi-occurrence substitution:\n got: %q\nwant: %q", got, want)
		}
	})

	t.Run("is a no-op when the placeholder is absent (idempotent re-provision)", func(t *testing.T) {
		alreadySubstituted := []byte("# You are Mia — the Org Concierge\n")
		got := substituteConciergeName(alreadySubstituted, "Mia")
		if string(got) != string(alreadySubstituted) {
			t.Errorf("idempotent re-provision changed the prompt:\n got: %q\nwant: %q", got, alreadySubstituted)
		}
	})

	t.Run("empty prompt is a no-op (don't panic on len=0)", func(t *testing.T) {
		if got := substituteConciergeName(nil, "Mia"); got != nil {
			t.Errorf("nil prompt should round-trip; got %q", got)
		}
		if got := substituteConciergeName([]byte{}, "Mia"); len(got) != 0 {
			t.Errorf("empty prompt should round-trip; got %q", got)
		}
	})
}

// TestConciergeIdentityPresent_PlaceholderAbsenceCheck is the PROVE-FAIL gate for
// the P3b identity probe generalization (item 4). conciergeIdentityPresent now
// keys on the ABSENCE of the literal {{CONCIERGE_NAME}} placeholder, NOT the
// presence of the substring "Org Concierge". Against the old check, a concierge
// renamed away from "Org Concierge" — e.g. "Aria" — would read as identity-ABSENT
// (false) even after substitution, restarting it on every boot (the boot-restart
// loop). This test proves the new behaviour:
//
//   - a substituted prompt with a NON-"Org Concierge" name → identity PRESENT (true);
//   - a prompt still carrying the literal {{CONCIERGE_NAME}} → identity ABSENT (false);
//   - an empty/probe-miss → identity ABSENT (false, safe re-seed).
func TestConciergeIdentityPresent_PlaceholderAbsenceCheck(t *testing.T) {
	ctx := context.Background()
	const id = "concierge-id"

	t.Run("substituted prompt with a non-'Org Concierge' name is identity-PRESENT", func(t *testing.T) {
		// This is the exact case the OLD strings.Contains(body,"Org Concierge")
		// check got WRONG: a renamed concierge whose prompt never says
		// "Org Concierge". The placeholder is gone → identity present.
		prov := &stubBootProvExec{systemPrompt: "# You are Aria — your organization's platform agent.\n"}
		if !conciergeIdentityPresent(ctx, prov, id) {
			t.Fatal("a substituted prompt for a renamed concierge (no 'Org Concierge' substring, no placeholder) MUST be identity-present; the old substring check would falsely return false → boot-restart loop")
		}
	})

	t.Run("prompt still carrying the literal {{CONCIERGE_NAME}} is identity-ABSENT", func(t *testing.T) {
		prov := &stubBootProvExec{systemPrompt: "# You are {{CONCIERGE_NAME}} — the Org Concierge\n"}
		if conciergeIdentityPresent(ctx, prov, id) {
			t.Fatal("an un-substituted prompt (placeholder still literal) MUST be identity-absent so the boot helper re-seeds it")
		}
	})

	t.Run("empty file is identity-ABSENT", func(t *testing.T) {
		prov := &stubBootProvExec{systemPrompt: "   \n"}
		if conciergeIdentityPresent(ctx, prov, id) {
			t.Fatal("an empty/whitespace prompt MUST be identity-absent")
		}
	})

	t.Run("probe error is identity-ABSENT (safe re-seed)", func(t *testing.T) {
		prov := &stubBootProvExec{execErr: errProbe}
		if conciergeIdentityPresent(ctx, prov, id) {
			t.Fatal("a probe error MUST be treated as identity-absent (re-seed via restart)")
		}
	})
}

// errProbe is a sentinel ExecRead error for the probe-miss subtest.
var errProbe = errProbeT("probe failed")

type errProbeT string

func (e errProbeT) Error() string { return string(e) }

// setConciergeModelResolver stubs conciergeModelResolver for a single test.
// Callers that expect a seed can return a model; callers that expect fail-closed
// can return an error.
func setConciergeModelResolver(t *testing.T, model string, err error) {
	t.Helper()
	old := conciergeModelResolver
	conciergeModelResolver = func(context.Context) (string, error) { return model, err }
	t.Cleanup(func() { conciergeModelResolver = old })
}

// TestConciergeRuntimeGeneralization_Defaults is the PROVE-FAIL gate for the P3b
// model + template + runtime defaults (items 1, 2, 3).
func TestConciergeRuntimeGeneralization_Defaults(t *testing.T) {
	t.Run("the ONE shared platform-default fallback model is minimax/MiniMax-M2.7 (CTO P3b)", func(t *testing.T) {
		if platformDefaultModelFallback != "minimax/MiniMax-M2.7" {
			t.Fatalf("platformDefaultModelFallback = %q, want %q — the single shared platform-default model (used only when the MOLECULE_LLM_DEFAULT_MODEL SSOT is unset) is MiniMax", platformDefaultModelFallback, "minimax/MiniMax-M2.7")
		}
	})

	t.Run("model resolution fails closed when the authoritative source is unavailable", func(t *testing.T) {
		setConciergeModelResolver(t, "", fmt.Errorf("cp unreachable"))
		mock := setupTestDB(t)
		mock.ExpectQuery(`SELECT encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = \$1 AND key = 'MODEL'`).
			WithArgs("ws-resolver-fail").
			WillReturnError(sql.ErrNoRows)
		env := map[string]string{}
		h := &WorkspaceHandler{}
		h.ensureConciergeModel(context.Background(), "ws-resolver-fail", defaultConciergeRuntime, env)
		if _, seeded := env["MODEL"]; seeded {
			t.Errorf("resolver failure seeded MODEL=%q — must fail closed", env["MODEL"])
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
	})

	t.Run("the declared model is registered + routable for claude-code", func(t *testing.T) {
		// Guards against the model id drifting out of the providers registry's
		// platform set for the DEFAULT concierge runtime, which would make
		// ensureConciergeModel leave the model unset and fail the provision closed.
		if ok, why := validateRegisteredModelForRuntime("claude-code", platformDefaultModelFallback); !ok {
			t.Errorf("declared model %q not registered for claude-code: %s", platformDefaultModelFallback, why)
		}
		if ok, why := validateDerivedProviderInRegistry("claude-code", platformDefaultModelFallback); !ok {
			t.Errorf("declared model %q has no registry provider for claude-code: %s", platformDefaultModelFallback, why)
		}
	})

	t.Run("the declared model derives to the platform provider on claude-code (proxy-billed)", func(t *testing.T) {
		// conciergeModelIsPlatformManaged is the gate that decides the LLM_PROVIDER
		// pin. The minimax/ default MUST be recognized as platform-managed for the
		// claude-code concierge (the provider's Anthropic-compat arm), else
		// ensureConciergeProvider would skip the pin and the concierge would boot
		// without LLM_PROVIDER and 401 against the CP proxy.
		if !conciergeModelIsPlatformManaged("claude-code", platformDefaultModelFallback) {
			t.Errorf("conciergeModelIsPlatformManaged(claude-code, %q) = false, want true — the registry-derived gate must recognize the minimax/ platform default", platformDefaultModelFallback)
		}
		// An empty model is treated as the (platform-managed) default.
		if !conciergeModelIsPlatformManaged("claude-code", "") {
			t.Error("an empty model must be treated as platform-managed (unresolved fresh payload)")
		}
		// A BYOK colon-form model must NOT be flagged platform-managed.
		if conciergeModelIsPlatformManaged("claude-code", "anthropic:claude-opus-4-8") {
			t.Error("a BYOK colon-form model must NOT be flagged platform-managed (would mis-route auth)")
		}
	})

	// CROSS-RUNTIME PARITY (gap CLOSED): the shared default minimax/MiniMax-M2.7
	// MUST derive to the platform-managed `platform` arm on EVERY concierge-capable
	// runtime, so a fresh concierge on any of them runs the shared default
	// proxy-billed (no tenant MINIMAX_API_KEY). This was historically a registry
	// gap for codex/openclaw (fixed earlier) and hermes (this change adds
	// minimax/* to hermes's platform arm in providers.yaml). If any runtime here
	// regresses to BYOK routing, the universal MISSING_MODEL gate would either fail
	// the provision closed or require a tenant key for the shared default.
	t.Run("the shared default routes platform-managed on all four runtimes", func(t *testing.T) {
		for _, runtime := range []string{"claude-code", "codex", "openclaw", "hermes"} {
			if !conciergeModelIsPlatformManaged(runtime, platformDefaultModelFallback) {
				t.Errorf("conciergeModelIsPlatformManaged(%q, %q) = false, want true — the shared platform default must route platform-managed on every runtime", runtime, platformDefaultModelFallback)
			}
		}
	})

	t.Run("conciergeTemplateForRuntime is runtime-agnostic (single platform-agent template)", func(t *testing.T) {
		// tenant-agent BUG 1 (P0): ONE runtime-agnostic concierge persona template
		// serves every runtime. The prior per-runtime "<runtime>-platform-agent"
		// names were never registered in the manifest → empty identity → no persona.
		cases := map[string]string{
			"":            "platform-agent", // empty → the one template
			"claude-code": "platform-agent",
			"codex":       "platform-agent", // was "codex-platform-agent" (unregistered)
			"openclaw":    "platform-agent", // was "openclaw-platform-agent" (unregistered)
			"hermes":      "platform-agent",
		}
		for rt, want := range cases {
			if got := conciergeTemplateForRuntime(rt); got != want {
				t.Errorf("conciergeTemplateForRuntime(%q) = %q, want %q", rt, got, want)
			}
		}
	})
}

// TestConciergeDefaultRuntime_EnvWinsOverConstAndBindsIntoInstall is the PROVE-FAIL
// positive control for the PR-6 (concierge-follows) runtime SSOT-follow, replacing
// the earlier tautological control (Researcher REQUEST_CHANGES 14231). The OLD
// positive control set MOLECULE_DEFAULT_RUNTIME="claude-code", which EQUALS the
// compiled-in fallback const defaultConciergeRuntime, so it would have PASSED even
// if conciergeDefaultRuntime ignored the env entirely and returned the const (a
// no-op assertion). It also never proved that the resolved runtime is THREADED into
// the installPlatformAgent DB INSERT ($3).
//
// This test fixes BOTH gaps using a NON-DEFAULT known runtime ("codex": in
// knownRuntimes' allowlist via fallbackRuntimes, and NOT equal to "claude-code"):
//
//	(a) ENV WINS over the const: with MOLECULE_DEFAULT_RUNTIME="codex" set,
//	    conciergeDefaultRuntime() resolves to "codex", NOT defaultConciergeRuntime.
//	    Because the env value differs from the const fallback, a pass PROVES the env
//	    drove the result: no longer a tautology.
//	(b) INSERT $3 BINDS the resolved runtime: installPlatformAgent called with an
//	    EMPTY runtime (the legacy/self-host path that falls back to
//	    conciergeDefaultRuntime) must stamp the env-resolved "codex" into the
//	    workspaces INSERT $3, NOT the "claude-code" const, and the matching
//	    runtime-agnostic "platform-agent" template into $4. The sqlmock WithArgs is the
//	    capture seam: it asserts the exact bound value of $3 (and $4), so a
//	    regression that hardcodes 'claude-code' in the INSERT VALUES (or ignores the
//	    env) fails here at unit-test time (no -tags=integration / Postgres needed).
//
// Sibling of the integration test's Case 1
// (TestIntegration_PlatformAgentInstall_RuntimeIsParameterAndNotClobbered), which
// proves the same binding against a real Postgres with an EXPLICIT runtime arg;
// this unit test additionally proves the ENV-RESOLUTION half (empty arg -> KMS env
// -> $3) that the integration test does not exercise.
func TestConciergeDefaultRuntime_EnvWinsOverConstAndBindsIntoInstall(t *testing.T) {
	// A NON-DEFAULT runtime in the isKnownRuntime allowlist (fallbackRuntimes),
	// chosen specifically so it differs from the const fallback: that difference
	// is what makes the assertions non-tautological.
	const nonDefaultRuntime = "codex"
	if nonDefaultRuntime == defaultConciergeRuntime {
		t.Fatalf("test invariant broken: nonDefaultRuntime (%q) must DIFFER from the const fallback (%q) to prove the env wins", nonDefaultRuntime, defaultConciergeRuntime)
	}
	if !isKnownRuntime(nonDefaultRuntime) {
		t.Fatalf("test invariant broken: %q must be a known runtime (isKnownRuntime allowlist) or installPlatformAgent's env resolution would reject it and fall back to the const", nonDefaultRuntime)
	}

	t.Run("(a) env WINS over the const fallback (no longer a tautology)", func(t *testing.T) {
		t.Setenv("MOLECULE_DEFAULT_RUNTIME", nonDefaultRuntime)
		if got := conciergeDefaultRuntime(); got != nonDefaultRuntime {
			t.Fatalf("conciergeDefaultRuntime() = %q, want %q: the env MUST win over the const fallback %q", got, nonDefaultRuntime, defaultConciergeRuntime)
		}
		if got := conciergeDefaultRuntime(); got == defaultConciergeRuntime {
			t.Fatalf("conciergeDefaultRuntime() returned the const fallback %q despite MOLECULE_DEFAULT_RUNTIME=%q: the env was ignored (tautology regression)", defaultConciergeRuntime, nonDefaultRuntime)
		}
	})

	t.Run("(b) installPlatformAgent binds the env-resolved NON-default runtime into INSERT $3 (and the matching template into $4)", func(t *testing.T) {
		t.Setenv("MOLECULE_DEFAULT_RUNTIME", nonDefaultRuntime)
		mock := setupTestDB(t)

		const platformID = "33333333-4444-5555-6666-777777777777"
		const paName = "Org Concierge runtime-follow"
		// The template that MUST accompany the resolved runtime (conciergeTemplateForRuntime).
		// Runtime-agnostic since BUG 1: this is "platform-agent" for every runtime.
		wantTemplate := conciergeTemplateForRuntime(nonDefaultRuntime) // "platform-agent"

		mock.ExpectBegin()
		// Step 0: downgrade any other platform root ($1 = platformID).
		mock.ExpectExec(`UPDATE workspaces SET kind = 'workspace'`).
			WithArgs(platformID).
			WillReturnResult(sqlmock.NewResult(0, 0))
		// Step 1: the upsert. THIS is the capture seam: $3 MUST be the env-resolved
		// NON-default runtime (not the 'claude-code' const), $4 the matching template.
		// Arg order matches the INSERT: ($1 id, $2 name, $3 runtime, $4 template).
		mock.ExpectExec(`INSERT INTO workspaces`).
			WithArgs(platformID, paName, nonDefaultRuntime, wantTemplate).
			WillReturnResult(sqlmock.NewResult(0, 1))
		// Step 1b: the privileged management MCP declaration must be written
		// before provisioning reads desiredPluginSources() and stamps
		// MOLECULE_DECLARED_PLUGINS.
		mock.ExpectExec(`INSERT INTO workspace_declared_plugins`).
			WithArgs(platformID, conciergePlatformMCPName, conciergePlatformMCPSource).
			WillReturnResult(sqlmock.NewResult(0, 1))
		// Step 2: capture old roots: none in this fixture.
		mock.ExpectQuery(`SELECT id FROM workspaces WHERE parent_id IS NULL AND id <>`).
			WithArgs(platformID).
			WillReturnRows(sqlmock.NewRows([]string{"id"}))
		mock.ExpectCommit()

		// EMPTY runtime arg -> installPlatformAgent falls back to conciergeDefaultRuntime(),
		// which (with the env set) resolves to the NON-default runtime. If the binding
		// regressed to a hardcoded 'claude-code' (or the env were ignored), the
		// WithArgs($3=codex) expectation below would be UNMET and this test fails.
		if err := installPlatformAgent(context.Background(), db.DB, platformID, paName, ""); err != nil {
			t.Fatalf("installPlatformAgent with empty runtime (env-resolved %q): %v", nonDefaultRuntime, err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("INSERT $3 did not bind the env-resolved runtime %q (or $4 the template %q): %v", nonDefaultRuntime, wantTemplate, err)
		}
	})
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
		conciergePlatformMCPEnv(env, "ws-self-1", "admintok")
		if env["MOLECULE_API_KEY"] != "admintok" {
			t.Errorf("MOLECULE_API_KEY = %q, want admintok", env["MOLECULE_API_KEY"])
		}
		if env["MOLECULE_API_URL"] != "http://platform:8080" {
			t.Errorf("MOLECULE_API_URL = %q, want platform url fallback", env["MOLECULE_API_URL"])
		}
		if env["MOLECULE_ORG_ID"] != "org-123" {
			t.Errorf("MOLECULE_ORG_ID = %q, want org-123", env["MOLECULE_ORG_ID"])
		}
		// SELF default for the management MCP (install_plugin /
		// get_conversation_history): the concierge's own workspace id
		// must ride the env so "act on MY OWN workspace" works zero-config.
		if env["MOLECULE_WORKSPACE_ID"] != "ws-self-1" {
			t.Errorf("MOLECULE_WORKSPACE_ID = %q, want ws-self-1", env["MOLECULE_WORKSPACE_ID"])
		}
	})

	t.Run("does not clobber existing values", func(t *testing.T) {
		t.Setenv("ADMIN_TOKEN", "admintok")
		env := map[string]string{"MOLECULE_API_KEY": "preset"}
		conciergePlatformMCPEnv(env, "ws-self-1", "admintok")
		if env["MOLECULE_API_KEY"] != "preset" {
			t.Errorf("MOLECULE_API_KEY overwritten to %q, want preset preserved", env["MOLECULE_API_KEY"])
		}
	})

	t.Run("MOLECULE_API_URL prefers explicit over PLATFORM_URL", func(t *testing.T) {
		t.Setenv("MOLECULE_API_URL", "http://explicit:9000")
		t.Setenv("PLATFORM_URL", "http://platform:8080")
		env := map[string]string{}
		conciergePlatformMCPEnv(env, "ws-self-1", "admintok")
		if env["MOLECULE_API_URL"] != "http://explicit:9000" {
			t.Errorf("MOLECULE_API_URL = %q, want the explicit env", env["MOLECULE_API_URL"])
		}
	})
}

// TestResolveConciergeAdminCredential_FallsBackToAdminToken pins the WS-C
// fail-safe: with no org anchor (self-host / local, MOLECULE_ORG_ID unset) there
// is no managed-org-token mint, so the concierge keeps the break-glass ADMIN_TOKEN
// — it must NEVER boot without an admin credential. The managed-token mint + rotate
// happy path runs against a real DB (orgtoken primitives are sqlmock-tested in
// internal/orgtoken; the concierge provision path is covered by the
// concierge-creates-workspace e2e).
func TestResolveConciergeAdminCredential_FallsBackToAdminToken(t *testing.T) {
	t.Run("no org id → break-glass ADMIN_TOKEN", func(t *testing.T) {
		t.Setenv("ADMIN_TOKEN", "break-glass-root")
		t.Setenv("MOLECULE_ORG_ID", "")
		if got := resolveConciergeAdminCredential(context.Background(), "ws-x"); got != "break-glass-root" {
			t.Fatalf("no org id: want ADMIN_TOKEN fallback, got %q", got)
		}
	})
	t.Run("no org id + no admin token → empty (local dev, MCP unauthenticated)", func(t *testing.T) {
		t.Setenv("ADMIN_TOKEN", "")
		t.Setenv("MOLECULE_ORG_ID", "")
		if got := resolveConciergeAdminCredential(context.Background(), "ws-x"); got != "" {
			t.Fatalf("want empty, got %q", got)
		}
	})
	t.Run("non-UUID org id → break-glass (no mint attempt; harness/self-host)", func(t *testing.T) {
		// The cp-stub replay harness uses MOLECULE_ORG_ID="harness-org-alpha"; the
		// org_api_tokens.org_id uuid column would reject it, so we must NOT attempt
		// the mint — fall back cleanly, no failed query.
		t.Setenv("ADMIN_TOKEN", "break-glass-root")
		t.Setenv("MOLECULE_ORG_ID", "harness-org-alpha")
		if got := resolveConciergeAdminCredential(context.Background(), "ws-x"); got != "break-glass-root" {
			t.Fatalf("non-UUID org id: want ADMIN_TOKEN fallback (no mint), got %q", got)
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
//
// Post RFC #2843 §10a: the concierge's identity (system prompt, model, MCP
// declaration) is delivered via the platform-agent template. The provision
// hook's only remaining work is (1) inject the platform-MCP env and (2) the
// {{CONCIERGE_NAME}} substitution in the template-delivered system-prompt.md.
// This test asserts both halves: the security boundary (only kind=platform gets
// the org-admin token) AND the new substitution behavior.
func TestApplyConciergeProvisionConfig_OnlyPlatformGetsOrgMCP(t *testing.T) {
	t.Setenv("ADMIN_TOKEN", "secret-org-admin")
	t.Setenv("PLATFORM_URL", "http://platform:8080")
	h := &WorkspaceHandler{}
	const kindQuery = `SELECT COALESCE\(kind, 'workspace'\), COALESCE\(runtime, ''\) FROM workspaces WHERE id =`
	const recordKindQuery = `SELECT COALESCE\(kind, 'workspace'\) FROM workspaces WHERE id =` // recordDeclaredPlugin precheck (1-col)
	// ensureConciergeModel (step 0, platform kind only) reads the stored MODEL
	// secret to decide seed-vs-respect. These subtests are about MCP/name, so
	// they stub an EXISTING model → ensureConciergeModel returns early (no
	// INSERT). The seed path itself is covered by
	// TestApplyConciergeProvisionConfig_SeedsModel.
	const modelSelQuery = `SELECT encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = \$1 AND key = 'MODEL'`
	// ensureConciergeProvider (step 0b, platform kind only) reads the stored
	// LLM_PROVIDER secret to decide seed-vs-respect. In these MCP/name subtests
	// the test env carries no MODEL (loadWorkspaceSecrets is not run), so the
	// provider gate (platform-managed model namespace) is not met and NO
	// LLM_PROVIDER INSERT fires — only the existence SELECT. The seed itself is
	// covered by TestApplyConciergeProvisionConfig_SeedsProvider.
	const providerSelQuery = `SELECT encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = \$1 AND key = 'LLM_PROVIDER'`
	const declaredInsert = `INSERT INTO workspace_declared_plugins`

	t.Run("ordinary workspace gets NO org MCP, NO admin token, NO substitution", func(t *testing.T) {
		mock := setupTestDB(t)
		mock.ExpectQuery(kindQuery).WithArgs("ws-ordinary").
			WillReturnRows(sqlmock.NewRows([]string{"kind", "runtime"}).AddRow("workspace", "claude-code"))
		env := map[string]string{}
		// Ordinary workspaces may have a non-concierge system-prompt.md in
		// configFiles; the hook must NOT touch it.
		cf := map[string][]byte{
			"config.yaml":      []byte("runtime: claude-code\n"),
			"system-prompt.md": []byte("{{CONCIERGE_NAME}} substitute me"),
		}
		out := h.applyConciergeProvisionConfig(context.Background(), "ws-ordinary", "", cf, env, "Worker")
		if _, ok := env["MOLECULE_API_KEY"]; ok {
			t.Errorf("SECURITY: ordinary workspace leaked MOLECULE_API_KEY (org-admin token): %v", env)
		}
		if _, ok := env["MOLECULE_ORG_API_KEY"]; ok {
			t.Errorf("SECURITY: ordinary workspace leaked MOLECULE_ORG_API_KEY: %v", env)
		}
		if strings.Contains(string(out["system-prompt.md"]), "Worker") {
			t.Errorf("ordinary workspace had its system-prompt substituted — the concierge hook must no-op for kind != platform; got:\n%s", out["system-prompt.md"])
		}
		// CR2 RC 11903 SA9003: the previous assertion
		// ('ordinary workspace had its system-prompt substituted — the
		// concierge hook must no-op for kind != platform') is the
		// load-bearing check. A separate '{{CONCIERGE_NAME}}'-survives
		// assertion would be tautological here (ordinary workspaces
		// legitimately carry the placeholder; the hook only runs for
		// kind=platform). Removed the dead if-block.
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
	})

	t.Run("platform agent gets org MCP env + admin token + {{CONCIERGE_NAME}} substitution", func(t *testing.T) {
		// The stored model is platform-managed, so ensureConciergeModel now takes
		// the RECONCILE path (re-resolve the SSOT). Stub the resolver to the SAME
		// stored model so reconcile is a no-op (no re-persist) — this subtest is
		// about the MCP/name wiring, not the model reconcile.
		setConciergeModelResolver(t, "moonshot/kimi-k2.6", nil)
		mock := setupTestDB(t)
		mock.ExpectQuery(kindQuery).WithArgs("ws-concierge").
			WillReturnRows(sqlmock.NewRows([]string{"kind", "runtime"}).AddRow("platform", "claude-code"))
		mock.ExpectQuery(modelSelQuery).WithArgs("ws-concierge").
			WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}).
				AddRow([]byte("moonshot/kimi-k2.6"), 0))
		// ensureConciergeProvider existence check (env has no MODEL here → no pin).
		mock.ExpectQuery(providerSelQuery).WithArgs("ws-concierge").
			WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}))
		// recordDeclaredPlugin: privileged-plugin kind precheck (→platform) + declared INSERT.
		mock.ExpectQuery(recordKindQuery).WithArgs("ws-concierge").
			WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("platform"))
		mock.ExpectExec(declaredInsert).
			WithArgs("ws-concierge", sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
		env := map[string]string{}
		cf := map[string][]byte{
			"config.yaml":      []byte("runtime: claude-code\nmodel: moonshot/kimi-k2.6\n"),
			"system-prompt.md": []byte("# You are {{CONCIERGE_NAME}} — the Org Concierge\n"),
		}
		out := h.applyConciergeProvisionConfig(context.Background(), "ws-concierge", "", cf, env, "Molecule AI Agent")
		if env["MOLECULE_API_KEY"] != "secret-org-admin" {
			t.Errorf("concierge did not receive the org-admin token; env=%v", env)
		}
		if env["MOLECULE_ORG_API_KEY"] != "secret-org-admin" {
			t.Errorf("management tools auth env (MOLECULE_ORG_API_KEY) missing; env=%v", env)
		}
		// The dispatch's recommendation (a): substitute the per-instance name
		// into the template-delivered system-prompt.md. Verify the
		// placeholder is gone and the name is baked in.
		if !strings.Contains(string(out["system-prompt.md"]), "Molecule AI Agent") {
			t.Errorf("{{CONCIERGE_NAME}} was not substituted with the per-instance name:\n%s", out["system-prompt.md"])
		}
		if strings.Contains(string(out["system-prompt.md"]), "{{CONCIERGE_NAME}}") {
			t.Errorf("{{CONCIERGE_NAME}} placeholder survived substitution:\n%s", out["system-prompt.md"])
		}
		// config.yaml must NOT have an mcp_servers block — the template's
		// mcp_servers.yaml overlay handles that, and the dispatch's explicit
		// directive is "PICK ONE, don't double-declare."
		if strings.Contains(string(out["config.yaml"]), "mcp_servers") {
			t.Errorf("config.yaml must not have an mcp_servers block (the seeded mcp_servers.yaml overlay handles it; got double-declaration):\n%s", out["config.yaml"])
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
	})

	t.Run("idempotent re-provision on the platform agent (no double-substitution)", func(t *testing.T) {
		// Reconcile no-op (stored == resolved SSOT) — see the subtest above.
		setConciergeModelResolver(t, "moonshot/kimi-k2.6", nil)
		mock := setupTestDB(t)
		mock.ExpectQuery(kindQuery).WithArgs("ws-concierge").
			WillReturnRows(sqlmock.NewRows([]string{"kind", "runtime"}).AddRow("platform", "claude-code"))
		mock.ExpectQuery(modelSelQuery).WithArgs("ws-concierge").
			WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}).
				AddRow([]byte("moonshot/kimi-k2.6"), 0))
		mock.ExpectQuery(providerSelQuery).WithArgs("ws-concierge").
			WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}))
		// recordDeclaredPlugin: privileged-plugin kind precheck (→platform) + declared INSERT.
		mock.ExpectQuery(recordKindQuery).WithArgs("ws-concierge").
			WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("platform"))
		mock.ExpectExec(declaredInsert).
			WithArgs("ws-concierge", sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
		env := map[string]string{}
		// Already-substituted prompt (a re-provision of a running concierge).
		cf := map[string][]byte{
			"system-prompt.md": []byte("# You are Molecule AI Agent — the Org Concierge\n"),
		}
		out := h.applyConciergeProvisionConfig(context.Background(), "ws-concierge", "", cf, env, "Molecule AI Agent")
		if !strings.Contains(string(out["system-prompt.md"]), "Molecule AI Agent") {
			t.Errorf("re-provision lost the name:\n%s", out["system-prompt.md"])
		}
		// Count of the name (must be exactly 1 — a naive re-substitute would
		// produce "Molecule AI Agent Molecule AI Agent" or similar).
		if n := strings.Count(string(out["system-prompt.md"]), "Molecule AI Agent"); n != 1 {
			t.Errorf("name appears %d times in re-provisioned prompt; want 1 (idempotent):\n%s", n, out["system-prompt.md"])
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
	})
}

// TestApplyConciergeProvisionConfig_SeedsModel is the CI regression gate for
// the 2026-06-15 incident: #2919 removed the concierge model-seed, so every
// fresh platform-agent provision reached the universal MISSING_MODEL gate
// (core#2594) with no stored model and failed closed ("reached provisioning
// with no model set"). It passed CI because no test (and no e2e) provisions a
// fresh platform agent through the model path. This test asserts the seed
// fires for a model-less platform agent, is SEED-ONLY (respects a customer's
// later pick), and never touches ordinary workspaces.
func TestApplyConciergeProvisionConfig_SeedsModel(t *testing.T) {
	h := &WorkspaceHandler{}
	const kindQuery = `SELECT COALESCE\(kind, 'workspace'\), COALESCE\(runtime, ''\) FROM workspaces WHERE id =`
	const recordKindQuery = `SELECT COALESCE\(kind, 'workspace'\) FROM workspaces WHERE id =` // recordDeclaredPlugin precheck (1-col)
	const modelSelQuery = `SELECT encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = \$1 AND key = 'MODEL'`
	const providerSelQuery = `SELECT encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = \$1 AND key = 'LLM_PROVIDER'`
	const declaredInsert = `INSERT INTO workspace_declared_plugins`
	const secretInsert = `INSERT INTO workspace_secrets`

	t.Run("fresh platform agent with NO stored model gets the declared model seeded + persisted", func(t *testing.T) {
		mock := setupTestDB(t)
		mock.ExpectQuery(kindQuery).WithArgs("ws-fresh").
			WillReturnRows(sqlmock.NewRows([]string{"kind", "runtime"}).AddRow("platform", "claude-code"))
		// No MODEL row yet (first boot) → readStoredModelSecret returns "".
		mock.ExpectQuery(modelSelQuery).WithArgs("ws-fresh").
			WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}))
		// Seed path must PERSIST the declared model.
		mock.ExpectExec(secretInsert).
			WithArgs("ws-fresh", sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
		// ensureConciergeProvider: no LLM_PROVIDER yet → existence SELECT empty;
		// the just-seeded MODEL (moonshot/…) meets the platform namespace gate,
		// so the provider pin is PERSISTED too.
		mock.ExpectQuery(providerSelQuery).WithArgs("ws-fresh").
			WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}))
		mock.ExpectExec(secretInsert).
			WithArgs("ws-fresh", sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))

		// recordDeclaredPlugin: privileged-plugin kind precheck (→platform) + declared INSERT.
		mock.ExpectQuery(recordKindQuery).WithArgs("ws-fresh").
			WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("platform"))
		mock.ExpectExec(declaredInsert).
			WithArgs("ws-fresh", sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
		env := map[string]string{}
		// Stub the authoritative resolver so this test is hermetic regardless of
		// whether the process env looks like SaaS (MOLECULE_ORG_ID + ADMIN_TOKEN)
		// or self-hosted. The resolver returning the declared model exercises the
		// successful-seed path end-to-end.
		setConciergeModelResolver(t, platformDefaultModelFallback, nil)
		h.applyConciergeProvisionConfig(context.Background(), "ws-fresh", "", nil, env, "Org Concierge")

		// THE regression assertion: without this seed the provision hits
		// MISSING_MODEL and fails closed. Both canonical env names must carry
		// the declared model so the runtime actually boots on it this provision.
		if env["MODEL"] != platformDefaultModelFallback {
			t.Errorf("fresh concierge did not seed MODEL=%q; got %q (env=%v) — MISSING_MODEL would fail this provision closed", platformDefaultModelFallback, env["MODEL"], env)
		}
		if env["MOLECULE_MODEL"] != platformDefaultModelFallback {
			t.Errorf("fresh concierge did not seed MOLECULE_MODEL=%q; got %q", platformDefaultModelFallback, env["MOLECULE_MODEL"])
		}
		// Companion provider pin: the concierge can't run a turn without it
		// (moonshot/… derives a non-registry provider name → adapter fail-closes).
		if env["LLM_PROVIDER"] != conciergeProvider {
			t.Errorf("fresh concierge did not seed LLM_PROVIDER=%q; got %q (env=%v) — concierge would boot not_configured", conciergeProvider, env["LLM_PROVIDER"], env)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations (MODEL or LLM_PROVIDER secret not persisted): %v", err)
		}
	})

	t.Run("SEED-ONLY: an existing customer model is respected, never overwritten", func(t *testing.T) {
		mock := setupTestDB(t)
		mock.ExpectQuery(kindQuery).WithArgs("ws-picked").
			WillReturnRows(sqlmock.NewRows([]string{"kind", "runtime"}).AddRow("platform", "claude-code"))
		// Customer already picked a model — stored MODEL secret present.
		mock.ExpectQuery(modelSelQuery).WithArgs("ws-picked").
			WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}).
				AddRow([]byte("anthropic:claude-opus-4-8"), 0))
		// NO model ExpectExec: ensureConciergeModel must return early (no re-seed,
		// no INSERT) — re-asserting the default would silently revert the pick.
		// ensureConciergeProvider runs its existence SELECT, but the test env
		// carries no MODEL and the customer's model is non-platform-namespace, so
		// NO provider pin fires either.
		mock.ExpectQuery(providerSelQuery).WithArgs("ws-picked").
			WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}))

		// recordDeclaredPlugin: privileged-plugin kind precheck (→platform) + declared INSERT.
		mock.ExpectQuery(recordKindQuery).WithArgs("ws-picked").
			WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("platform"))
		mock.ExpectExec(declaredInsert).
			WithArgs("ws-picked", sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
		env := map[string]string{}
		h.applyConciergeProvisionConfig(context.Background(), "ws-picked", "", nil, env, "Org Concierge")

		if env["MODEL"] == platformDefaultModelFallback {
			t.Errorf("seed-only violated: ensureConciergeModel overwrote the customer's model with the declared default")
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations (an unexpected INSERT means it re-seeded over the customer's pick): %v", err)
		}
	})

	t.Run("ordinary workspace never seeds a model (no model queries at all)", func(t *testing.T) {
		mock := setupTestDB(t)
		mock.ExpectQuery(kindQuery).WithArgs("ws-ordinary").
			WillReturnRows(sqlmock.NewRows([]string{"kind", "runtime"}).AddRow("workspace", "claude-code"))
		// No model SELECT/INSERT expected — the hook returns before step 0.

		env := map[string]string{}
		h.applyConciergeProvisionConfig(context.Background(), "ws-ordinary", "", nil, env, "Worker")

		if _, ok := env["MODEL"]; ok {
			t.Errorf("ordinary workspace had a model seeded — the concierge model-seed must be platform-kind only; env=%v", env)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations (ordinary workspace ran a model query): %v", err)
		}
	})

	// #3267 follow-up #2: the concierge model is resolved from an AUTHORITATIVE
	// source (CP on SaaS, operator env on self-hosted). It MUST fail closed if the
	// source is missing/unreachable. The const is intentionally NOT a fallback.
	t.Run("authoritative resolver returns a non-const model → seed follows it", func(t *testing.T) {
		// A DIFFERENT routable platform model than the const default, so a pass
		// proves the resolved value (not platformDefaultModelFallback) drives the seed.
		const resolvedModel = "moonshot/kimi-k2.6"
		if resolvedModel == platformDefaultModelFallback {
			t.Fatalf("test invariant broken: resolvedModel must differ from the const to prove the resolver wins")
		}
		setConciergeModelResolver(t, resolvedModel, nil)
		t.Setenv("MOLECULE_DEFAULT_RUNTIME", "claude-code")

		mock := setupTestDB(t)
		mock.ExpectQuery(kindQuery).WithArgs("ws-resolved").
			WillReturnRows(sqlmock.NewRows([]string{"kind", "runtime"}).AddRow("platform", "claude-code"))
		mock.ExpectQuery(modelSelQuery).WithArgs("ws-resolved").
			WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}))
		mock.ExpectExec(secretInsert).
			WithArgs("ws-resolved", sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
		// resolvedModel is platform-managed → provider pin persists too.
		mock.ExpectQuery(providerSelQuery).WithArgs("ws-resolved").
			WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}))
		mock.ExpectExec(secretInsert).
			WithArgs("ws-resolved", sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectQuery(recordKindQuery).WithArgs("ws-resolved").
			WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("platform"))
		mock.ExpectExec(declaredInsert).
			WithArgs("ws-resolved", sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))

		env := map[string]string{}
		h.applyConciergeProvisionConfig(context.Background(), "ws-resolved", "", nil, env, "Org Concierge")

		if env["MODEL"] != resolvedModel {
			t.Errorf("resolved seed did not follow resolver model %q; got MODEL=%q (env=%v)", resolvedModel, env["MODEL"], env)
		}
		if env["MOLECULE_MODEL"] != resolvedModel {
			t.Errorf("resolved seed did not set MOLECULE_MODEL=%q; got %q", resolvedModel, env["MOLECULE_MODEL"])
		}
		if env["MODEL"] == platformDefaultModelFallback {
			t.Errorf("resolved seed wrongly used the const fallback %q instead of the authoritative value", platformDefaultModelFallback)
		}
		if env["LLM_PROVIDER"] != conciergeProvider {
			t.Errorf("resolved platform-managed model did not pin LLM_PROVIDER=%q; got %q", conciergeProvider, env["LLM_PROVIDER"])
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations (resolved model/provider not persisted): %v", err)
		}
	})

	t.Run("authoritative resolver returns empty → seed fails closed", func(t *testing.T) {
		setConciergeModelResolver(t, "", nil)
		t.Setenv("MOLECULE_DEFAULT_RUNTIME", "claude-code")

		mock := setupTestDB(t)
		mock.ExpectQuery(kindQuery).WithArgs("ws-empty-resolve").
			WillReturnRows(sqlmock.NewRows([]string{"kind", "runtime"}).AddRow("platform", "claude-code"))
		// No MODEL row yet, so ensureConciergeModel calls the resolver. The resolver
		// returns empty → fail closed. No model INSERT, and because the model stays
		// empty the provider pin also stays off (conciergeModelIsPlatformManaged
		// treats empty as platform-managed, but ensureConciergeProvider only pins
		// when there is no stored LLM_PROVIDER; the empty-model branch still fires
		// the provider pin in production, but here there is no stored provider and
		// the env MODEL is empty, so the pin WILL be attempted and should succeed).
		// Actually: ensureConciergeModel leaves env["MODEL"] empty. Then
		// ensureConciergeProvider sees empty model → platform-managed → pins. So we
		// expect the provider SELECT + INSERT.
		mock.ExpectQuery(modelSelQuery).WithArgs("ws-empty-resolve").
			WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}))
		mock.ExpectQuery(providerSelQuery).WithArgs("ws-empty-resolve").
			WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}))
		mock.ExpectExec(secretInsert).
			WithArgs("ws-empty-resolve", sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectQuery(recordKindQuery).WithArgs("ws-empty-resolve").
			WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("platform"))
		mock.ExpectExec(declaredInsert).
			WithArgs("ws-empty-resolve", sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))

		env := map[string]string{}
		h.applyConciergeProvisionConfig(context.Background(), "ws-empty-resolve", "", nil, env, "Org Concierge")

		if _, seeded := env["MODEL"]; seeded {
			t.Errorf("empty resolver result seeded MODEL=%q — must fail closed", env["MODEL"])
		}
		if _, seeded := env["MOLECULE_MODEL"]; seeded {
			t.Errorf("empty resolver result seeded MOLECULE_MODEL=%q — must fail closed", env["MOLECULE_MODEL"])
		}
		// Provider pin for empty/unresolved model is intentional; see comment above.
		if env["LLM_PROVIDER"] != conciergeProvider {
			t.Errorf("empty unresolved model did not pin LLM_PROVIDER=%q; got %q", conciergeProvider, env["LLM_PROVIDER"])
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
	})

	t.Run("self-hosted path reads MOLECULE_LLM_DEFAULT_MODEL when org creds are absent", func(t *testing.T) {
		// Force self-hosted mode by clearing the SaaS creds. The resolver then reads
		// the operator-supplied env as the SSOT.
		t.Setenv("MOLECULE_ORG_ID", "")
		t.Setenv("ADMIN_TOKEN", "")
		const selfHostedModel = "moonshot/kimi-k2.6"
		t.Setenv("MOLECULE_LLM_DEFAULT_MODEL", selfHostedModel)
		t.Setenv("MOLECULE_DEFAULT_RUNTIME", "claude-code")

		mock := setupTestDB(t)
		mock.ExpectQuery(kindQuery).WithArgs("ws-selfhosted").
			WillReturnRows(sqlmock.NewRows([]string{"kind", "runtime"}).AddRow("platform", "claude-code"))
		mock.ExpectQuery(modelSelQuery).WithArgs("ws-selfhosted").
			WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}))
		mock.ExpectExec(secretInsert).
			WithArgs("ws-selfhosted", sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectQuery(providerSelQuery).WithArgs("ws-selfhosted").
			WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}))
		mock.ExpectExec(secretInsert).
			WithArgs("ws-selfhosted", sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectQuery(recordKindQuery).WithArgs("ws-selfhosted").
			WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("platform"))
		mock.ExpectExec(declaredInsert).
			WithArgs("ws-selfhosted", sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))

		env := map[string]string{}
		h.applyConciergeProvisionConfig(context.Background(), "ws-selfhosted", "", nil, env, "Org Concierge")

		if env["MODEL"] != selfHostedModel {
			t.Errorf("self-hosted seed did not follow MOLECULE_LLM_DEFAULT_MODEL=%q; got MODEL=%q (env=%v)", selfHostedModel, env["MODEL"], env)
		}
		if env["MOLECULE_MODEL"] != selfHostedModel {
			t.Errorf("self-hosted seed did not set MOLECULE_MODEL=%q; got %q", selfHostedModel, env["MOLECULE_MODEL"])
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
	})

	t.Run("self-hosted path with no env falls back to the shared platform default", func(t *testing.T) {
		// SSOT unset on a self-hosted boot → the ONE shared platformDefaultModelFallback
		// is seeded (NOT fail-closed), so a fresh concierge still has a routable,
		// platform-managed model. The fallback follows the SEEDING mock sequence
		// (it persists the resolved MODEL secret), identical to the env-present case.
		t.Setenv("MOLECULE_ORG_ID", "")
		t.Setenv("ADMIN_TOKEN", "")
		t.Setenv("MOLECULE_LLM_DEFAULT_MODEL", "")
		t.Setenv("MOLECULE_DEFAULT_RUNTIME", "claude-code")

		mock := setupTestDB(t)
		mock.ExpectQuery(kindQuery).WithArgs("ws-selfhosted-missing").
			WillReturnRows(sqlmock.NewRows([]string{"kind", "runtime"}).AddRow("platform", "claude-code"))
		mock.ExpectQuery(modelSelQuery).WithArgs("ws-selfhosted-missing").
			WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}))
		mock.ExpectExec(secretInsert).
			WithArgs("ws-selfhosted-missing", sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectQuery(providerSelQuery).WithArgs("ws-selfhosted-missing").
			WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}))
		mock.ExpectExec(secretInsert).
			WithArgs("ws-selfhosted-missing", sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectQuery(recordKindQuery).WithArgs("ws-selfhosted-missing").
			WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("platform"))
		mock.ExpectExec(declaredInsert).
			WithArgs("ws-selfhosted-missing", sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))

		env := map[string]string{}
		h.applyConciergeProvisionConfig(context.Background(), "ws-selfhosted-missing", "", nil, env, "Org Concierge")

		if env["MODEL"] != platformDefaultModelFallback {
			t.Errorf("missing self-hosted env did not seed the shared fallback MODEL=%q; got %q (env=%v)", platformDefaultModelFallback, env["MODEL"], env)
		}
		if env["MOLECULE_MODEL"] != platformDefaultModelFallback {
			t.Errorf("missing self-hosted env did not seed MOLECULE_MODEL=%q; got %q", platformDefaultModelFallback, env["MOLECULE_MODEL"])
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
	})
}

// TestEnsureConciergeModel_ReconcilesPlatformManagedToSSOT is the PROVE-FAIL gate
// for the one-shot-seed drift fix (audit BREAK A). ensureConciergeModel used to
// return early on ANY existing MODEL secret, freezing an already-seeded concierge
// on whatever the platform default was at first boot — so a later SSOT bump (the
// moonshot/kimi-k2.6 → minimax/MiniMax-M2.7 migration) never propagated. The
// reconcile path now: (1) re-resolves the SSOT and overwrites a PLATFORM-MANAGED
// default to it, (2) respects a genuine BYOK customer pick, (3) keeps the existing
// model on a resolver blip, and (4) ALWAYS re-asserts MODEL == MOLECULE_MODEL to
// kill the cross-provision split. Calls ensureConciergeModel DIRECTLY to isolate
// the behavior.
func TestEnsureConciergeModel_ReconcilesPlatformManagedToSSOT(t *testing.T) {
	h := &WorkspaceHandler{}
	const modelSelQuery = `SELECT encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = \$1 AND key = 'MODEL'`
	const secretInsert = `INSERT INTO workspace_secrets`
	const ssot = "minimax/MiniMax-M2.7"

	t.Run("platform-managed default reconciles to the SSOT (the M-bump propagates)", func(t *testing.T) {
		setConciergeModelResolver(t, ssot, nil)
		mock := setupTestDB(t)
		// Stored model is the OLD platform default (moonshot) → platform-managed.
		mock.ExpectQuery(modelSelQuery).WithArgs("ws-recon").
			WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}).
				AddRow([]byte("moonshot/kimi-k2.6"), 0))
		// Reconcile overwrites → the SSOT model is persisted.
		mock.ExpectExec(secretInsert).
			WithArgs("ws-recon", sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))

		// Simulate applyRuntimeModelEnv having already run with the stale stored model.
		env := map[string]string{"MODEL": "moonshot/kimi-k2.6", "MOLECULE_MODEL": "moonshot/kimi-k2.6"}
		h.ensureConciergeModel(context.Background(), "ws-recon", "claude-code", env)

		if env["MODEL"] != ssot || env["MOLECULE_MODEL"] != ssot {
			t.Fatalf("platform-managed default not reconciled to SSOT; env=%v (want both %q)", env, ssot)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations (reconciled MODEL not persisted): %v", err)
		}
	})

	t.Run("platform-managed but SSOT unchanged → no redundant DB write, env preserved", func(t *testing.T) {
		setConciergeModelResolver(t, ssot, nil)
		mock := setupTestDB(t)
		// Stored model already equals the SSOT → resolver returns the same → no-op.
		mock.ExpectQuery(modelSelQuery).WithArgs("ws-noop").
			WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}).
				AddRow([]byte(ssot), 0))
		// No INSERT expected — an unchanged SSOT must not churn the secret store.

		env := map[string]string{"MODEL": ssot, "MOLECULE_MODEL": ssot}
		h.ensureConciergeModel(context.Background(), "ws-noop", "claude-code", env)

		if env["MODEL"] != ssot || env["MOLECULE_MODEL"] != ssot {
			t.Fatalf("unchanged-SSOT no-op mutated env; env=%v", env)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations (an unchanged SSOT must not persist): %v", err)
		}
	})

	t.Run("a genuine BYOK customer pick is NOT reconciled (respected untouched)", func(t *testing.T) {
		// Resolver would offer the SSOT, but a non-platform model is a customer pick.
		setConciergeModelResolver(t, ssot, nil)
		mock := setupTestDB(t)
		mock.ExpectQuery(modelSelQuery).WithArgs("ws-byok").
			WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}).
				AddRow([]byte("anthropic:claude-opus-4-8"), 0))
		// No INSERT expected — the customer's pick is preserved. envVars reflects
		// what applyRuntimeModelEnv already set (the reconciler does not touch it).

		env := map[string]string{"MODEL": "anthropic:claude-opus-4-8", "MOLECULE_MODEL": "anthropic:claude-opus-4-8"}
		h.ensureConciergeModel(context.Background(), "ws-byok", "claude-code", env)

		if env["MODEL"] != "anthropic:claude-opus-4-8" || env["MOLECULE_MODEL"] != "anthropic:claude-opus-4-8" {
			t.Fatalf("BYOK pick not respected; env=%v", env)
		}
		if env["MODEL"] == ssot {
			t.Fatal("BYOK customer pick was overwritten with the SSOT default — the platform must not clobber a customer choice")
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations (an unexpected INSERT means it reconciled a BYOK pick): %v", err)
		}
	})

	t.Run("reconcile keeps the existing model on a resolver blip (no break on transient CP error)", func(t *testing.T) {
		setConciergeModelResolver(t, "", fmt.Errorf("cp unreachable"))
		mock := setupTestDB(t)
		mock.ExpectQuery(modelSelQuery).WithArgs("ws-blip").
			WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}).
				AddRow([]byte("moonshot/kimi-k2.6"), 0))
		// No INSERT — keep the existing model rather than wedge the concierge.

		env := map[string]string{"MODEL": "moonshot/kimi-k2.6", "MOLECULE_MODEL": "moonshot/kimi-k2.6"}
		h.ensureConciergeModel(context.Background(), "ws-blip", "claude-code", env)

		if env["MODEL"] != "moonshot/kimi-k2.6" || env["MOLECULE_MODEL"] != "moonshot/kimi-k2.6" {
			t.Fatalf("resolver blip lost the existing model; env=%v", env)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations (a transient resolver error must not persist anything): %v", err)
		}
	})
}

// TestApplyConciergeProvisionConfig_MoleculeModelResolvesMinimaxNotMoonshot is
// the end-to-end DISPLAY regression for the moonshot->minimax issue. The concierge
// DISPLAY env the canvas Config tab surfaces is MOLECULE_MODEL; the bug was that a
// concierge re-baked MOLECULE_MODEL=moonshot/kimi-k2.6 (the dead template pin) even
// though its resolved runtime model was minimax — MODEL and MOLECULE_MODEL split.
//
// This drives the FULL provision hook (applyConciergeProvisionConfig → reconcile +
// provider pin) for a concierge whose stored MODEL is the stale platform-managed
// moonshot default, with the authoritative resolver returning the minimax SSOT. It
// asserts BOTH canonical env names land on minimax and that NEITHER is left on the
// moonshot pin — so MODEL and MOLECULE_MODEL agree and the Config tab shows minimax.
func TestApplyConciergeProvisionConfig_MoleculeModelResolvesMinimaxNotMoonshot(t *testing.T) {
	h := &WorkspaceHandler{}
	const kindQuery = `SELECT COALESCE\(kind, 'workspace'\), COALESCE\(runtime, ''\) FROM workspaces WHERE id =`
	const recordKindQuery = `SELECT COALESCE\(kind, 'workspace'\) FROM workspaces WHERE id =`
	const modelSelQuery = `SELECT encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = \$1 AND key = 'MODEL'`
	const providerSelQuery = `SELECT encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = \$1 AND key = 'LLM_PROVIDER'`
	const declaredInsert = `INSERT INTO workspace_declared_plugins`
	const secretInsert = `INSERT INTO workspace_secrets`

	const ssot = "minimax/MiniMax-M2.7"
	const deadPin = "moonshot/kimi-k2.6"

	setConciergeModelResolver(t, ssot, nil)
	t.Setenv("MOLECULE_DEFAULT_RUNTIME", "claude-code")

	mock := setupTestDB(t)
	mock.ExpectQuery(kindQuery).WithArgs("ws-disp").
		WillReturnRows(sqlmock.NewRows([]string{"kind", "runtime"}).AddRow("platform", "claude-code"))
	// Stale stored MODEL is the OLD platform-managed default (moonshot) — the
	// exact value the dead template pin used to seed.
	mock.ExpectQuery(modelSelQuery).WithArgs("ws-disp").
		WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}).
			AddRow([]byte(deadPin), 0))
	// Reconcile (#3355): platform-managed stale default → overwrite to the SSOT.
	mock.ExpectExec(secretInsert).
		WithArgs("ws-disp", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// ensureConciergeProvider: no LLM_PROVIDER yet → platform-managed minimax pins it.
	mock.ExpectQuery(providerSelQuery).WithArgs("ws-disp").
		WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}))
	mock.ExpectExec(secretInsert).
		WithArgs("ws-disp", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(recordKindQuery).WithArgs("ws-disp").
		WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("platform"))
	mock.ExpectExec(declaredInsert).
		WithArgs("ws-disp", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// applyRuntimeModelEnv runs BEFORE this hook with the stale stored model, so
	// both env names arrive on the dead moonshot pin — exactly the prod split.
	env := map[string]string{"MODEL": deadPin, "MOLECULE_MODEL": deadPin}
	h.applyConciergeProvisionConfig(context.Background(), "ws-disp", "", nil, env, "Org Concierge")

	if env["MODEL"] != ssot {
		t.Errorf("MODEL did not reconcile to the SSOT minimax; got %q want %q (env=%v)", env["MODEL"], ssot, env)
	}
	if env["MOLECULE_MODEL"] != ssot {
		t.Errorf("MOLECULE_MODEL (the Config-tab DISPLAY env) did not resolve to minimax; got %q want %q", env["MOLECULE_MODEL"], ssot)
	}
	if env["MODEL"] == deadPin || env["MOLECULE_MODEL"] == deadPin {
		t.Errorf("a dead moonshot pin survived reconcile: MODEL=%q MOLECULE_MODEL=%q — the DISPLAY would still show moonshot", env["MODEL"], env["MOLECULE_MODEL"])
	}
	// The whole point: MODEL and MOLECULE_MODEL must AGREE (no split).
	if env["MODEL"] != env["MOLECULE_MODEL"] {
		t.Errorf("MODEL/MOLECULE_MODEL split persists: MODEL=%q MOLECULE_MODEL=%q", env["MODEL"], env["MOLECULE_MODEL"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestEnsureConciergeProvider_EmptyModelPins is the direct-unit regression gate
// for the empty-model pin behavior. An unresolved (empty) MODEL on a
// fresh/rebuilt-from-DB concierge payload MUST be treated as the platform-managed
// default and pinned to LLM_PROVIDER=platform; otherwise the concierge boots with
// no provider pin and 401s against the CP LLM proxy. An explicit BYOK (non-empty,
// non-platform) model resolves on its own and MUST NOT be pinned.
//
// P3b: the pin gate is now registry-DERIVED (conciergeModelIsPlatformManaged →
// DeriveProvider(runtime, model).IsPlatform()), replacing the old hardcoded
// `strings.HasPrefix(model, "moonshot/")` prefix test — so the minimax/ default
// (and any registered platform model on any runtime) is recognized while the
// empty-model and BYOK-model behaviors above are preserved. This calls
// ensureConciergeProvider DIRECTLY (not via applyConciergeProvisionConfig) to
// isolate the gate.
func TestEnsureConciergeProvider_EmptyModelPins(t *testing.T) {
	h := &WorkspaceHandler{}
	const providerSelQuery = `SELECT encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = \$1 AND key = 'LLM_PROVIDER'`
	const secretInsert = `INSERT INTO workspace_secrets`

	t.Run("empty MODEL (rebuilt-from-DB payload) still pins platform", func(t *testing.T) {
		mock := setupTestDB(t)
		// No LLM_PROVIDER stored yet → existence SELECT empty → proceed to the gate.
		mock.ExpectQuery(providerSelQuery).WithArgs("ws-empty-model").
			WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}))
		// Empty model is the platform-managed default → the pin MUST persist.
		mock.ExpectExec(secretInsert).
			WithArgs("ws-empty-model", sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))

		env := map[string]string{} // no MODEL key — unresolved fresh/rebuilt payload
		h.ensureConciergeProvider(context.Background(), "ws-empty-model", defaultConciergeRuntime, env)

		if env["LLM_PROVIDER"] != conciergeProvider {
			t.Errorf("empty MODEL did not pin LLM_PROVIDER=%q; got %q (env=%v) — concierge would 401 against the CP LLM proxy", conciergeProvider, env["LLM_PROVIDER"], env)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations (LLM_PROVIDER pin not persisted): %v", err)
		}
	})

	t.Run("explicit BYOK non-platform model still skips the pin", func(t *testing.T) {
		mock := setupTestDB(t)
		// No LLM_PROVIDER stored yet → existence SELECT empty → proceed to the gate.
		mock.ExpectQuery(providerSelQuery).WithArgs("ws-byok-model").
			WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}))
		// NO ExpectExec: a non-empty BYOK model resolves on its own → no pin.

		env := map[string]string{"MODEL": "anthropic:claude-opus-4-8"}
		h.ensureConciergeProvider(context.Background(), "ws-byok-model", defaultConciergeRuntime, env)

		if _, ok := env["LLM_PROVIDER"]; ok {
			t.Errorf("explicit BYOK model wrongly pinned LLM_PROVIDER=%q — would mis-route a BYOK/self-host concierge", env["LLM_PROVIDER"])
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations (an unexpected INSERT means it pinned a BYOK model): %v", err)
		}
	})
}

// TestApplyConciergeProvisionConfig_SeedsProvider is the CI regression gate for
// the concierge non-response incident (prod 2026-06-18): the concierge booted
// online but configuration_status=not_configured because the runtime wheel
// derives provider="moonshot" from the model id "moonshot/kimi-k2.6" (a
// model-PREFIX on the `platform` provider, NOT a provider NAME), and the
// claude-code adapter fail-closes. The template config.yaml `provider:` field
// does not reach the on-box config, so core MUST seed the LLM_PROVIDER env pin
// (the highest-precedence, restart-surviving signal). Verified on prod test3:
// setting LLM_PROVIDER=platform flipped not_configured → ready + responding.
func TestApplyConciergeProvisionConfig_SeedsProvider(t *testing.T) {
	h := &WorkspaceHandler{}
	const kindQuery = `SELECT COALESCE\(kind, 'workspace'\), COALESCE\(runtime, ''\) FROM workspaces WHERE id =`
	const recordKindQuery = `SELECT COALESCE\(kind, 'workspace'\) FROM workspaces WHERE id =` // recordDeclaredPlugin precheck (1-col)
	const modelSelQuery = `SELECT encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = \$1 AND key = 'MODEL'`
	const providerSelQuery = `SELECT encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = \$1 AND key = 'LLM_PROVIDER'`
	const declaredInsert = `INSERT INTO workspace_declared_plugins`
	const secretInsert = `INSERT INTO workspace_secrets`

	t.Run("existing platform-managed concierge with NO provider gets LLM_PROVIDER=platform pinned", func(t *testing.T) {
		mock := setupTestDB(t)
		mock.ExpectQuery(kindQuery).WithArgs("ws-heal").
			WillReturnRows(sqlmock.NewRows([]string{"kind", "runtime"}).AddRow("platform", "claude-code"))
		// Existing platform model → ensureConciergeModel respects it (no INSERT).
		mock.ExpectQuery(modelSelQuery).WithArgs("ws-heal").
			WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}).
				AddRow([]byte(platformDefaultModelFallback), 0))
		// No LLM_PROVIDER yet → existence SELECT empty, then PERSIST the pin.
		mock.ExpectQuery(providerSelQuery).WithArgs("ws-heal").
			WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}))
		mock.ExpectExec(secretInsert).
			WithArgs("ws-heal", sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))

		// Simulate loadWorkspaceSecrets having populated MODEL into the env
		// (the production precondition for an existing-model concierge).
		// recordDeclaredPlugin: privileged-plugin kind precheck (→platform) + declared INSERT.
		mock.ExpectQuery(recordKindQuery).WithArgs("ws-heal").
			WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("platform"))
		mock.ExpectExec(declaredInsert).
			WithArgs("ws-heal", sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
		env := map[string]string{"MODEL": platformDefaultModelFallback}
		h.applyConciergeProvisionConfig(context.Background(), "ws-heal", "", nil, env, "Org Concierge")

		if env["LLM_PROVIDER"] != conciergeProvider {
			t.Errorf("existing platform-managed concierge did not get LLM_PROVIDER=%q pinned; got %q (env=%v)", conciergeProvider, env["LLM_PROVIDER"], env)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations (LLM_PROVIDER pin not persisted): %v", err)
		}
	})

	t.Run("SEED-ONLY: a customer-picked provider is respected, never overwritten", func(t *testing.T) {
		mock := setupTestDB(t)
		mock.ExpectQuery(kindQuery).WithArgs("ws-prov-picked").
			WillReturnRows(sqlmock.NewRows([]string{"kind", "runtime"}).AddRow("platform", "claude-code"))
		mock.ExpectQuery(modelSelQuery).WithArgs("ws-prov-picked").
			WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}).
				AddRow([]byte(platformDefaultModelFallback), 0))
		// Customer already pinned a provider in the canvas → existence SELECT
		// returns it → NO INSERT (respecting the pick).
		mock.ExpectQuery(providerSelQuery).WithArgs("ws-prov-picked").
			WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}).
				AddRow([]byte("anthropic-api"), 0))

		// recordDeclaredPlugin: privileged-plugin kind precheck (→platform) + declared INSERT.
		mock.ExpectQuery(recordKindQuery).WithArgs("ws-prov-picked").
			WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("platform"))
		mock.ExpectExec(declaredInsert).
			WithArgs("ws-prov-picked", sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
		env := map[string]string{"MODEL": platformDefaultModelFallback, "LLM_PROVIDER": "anthropic-api"}
		h.applyConciergeProvisionConfig(context.Background(), "ws-prov-picked", "", nil, env, "Org Concierge")

		if env["LLM_PROVIDER"] != "anthropic-api" {
			t.Errorf("seed-only violated: overwrote the customer's provider pick (got %q)", env["LLM_PROVIDER"])
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations (an unexpected INSERT means it re-pinned over the customer's pick): %v", err)
		}
	})

	t.Run("non-platform model namespace does NOT get a platform provider pin", func(t *testing.T) {
		mock := setupTestDB(t)
		mock.ExpectQuery(kindQuery).WithArgs("ws-byok").
			WillReturnRows(sqlmock.NewRows([]string{"kind", "runtime"}).AddRow("platform", "claude-code"))
		mock.ExpectQuery(modelSelQuery).WithArgs("ws-byok").
			WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}).
				AddRow([]byte("sonnet"), 0))
		// Existence SELECT runs; model "sonnet" resolves on its own (anthropic-
		// oauth alias), so the gate is NOT met → NO provider INSERT.
		mock.ExpectQuery(providerSelQuery).WithArgs("ws-byok").
			WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}))

		// recordDeclaredPlugin: privileged-plugin kind precheck (→platform) + declared INSERT.
		mock.ExpectQuery(recordKindQuery).WithArgs("ws-byok").
			WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("platform"))
		mock.ExpectExec(declaredInsert).
			WithArgs("ws-byok", sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
		env := map[string]string{"MODEL": "sonnet"}
		h.applyConciergeProvisionConfig(context.Background(), "ws-byok", "", nil, env, "Org Concierge")

		if _, ok := env["LLM_PROVIDER"]; ok {
			t.Errorf("non-platform model wrongly got LLM_PROVIDER pinned (%q) — would mis-route a BYOK/self-host concierge", env["LLM_PROVIDER"])
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
	})
}

// TestNoConciergeLiteralsInCore is the regression guard for the RFC #2843
// §10a de-hardcode: the concierge's PROMPT + MCP-wiring identity (system
// prompt template, MCP-servers block, identity files) MUST live in the
// platform-agent template, not as Go string literals in core. A future
// re-introduction of those consts would silently regress the SSOT; this test
// fails on a fresh build if any of the banned identifiers reappear as bare Go
// references in the package source.
//
// NOTE (incident 2026-06-15): platformDefaultModelFallback is DELIBERATELY NOT
// banned. It is the ONE shared platform-default model — a single last-resort
// fallback used ONLY when the MOLECULE_LLM_DEFAULT_MODEL SSOT
// (Infisical /shared/controlplane/llm, read from the CP on SaaS or the operator
// env on self-host) is unset. The authoritative model is ALWAYS the SSOT; this
// const merely keeps a fresh concierge from MISSING_MODEL on a dev/e2e/self-host
// boot with no SSOT configured. It is NOT a per-runtime hardcode (the prior
// divergent template defaults were removed) and NOT the primary seed source. A
// genuine CP outage still fails closed (defaultResolveConciergeModel). The seed
// path is gated by TestApplyConciergeProvisionConfig_SeedsModel.
//
// This is a grep over the package — brittle by design (intentionally so:
// the concierge-literal pattern was the exact failure mode of the
// pre-#10a code, and a re-introduction must be caught at CI time, not in
// code review).
func TestNoConciergeLiteralsInCore(t *testing.T) {
	banned := []string{
		"conciergeSystemPromptTmpl",
		"conciergeMCPServersBlock",
		"conciergeMCPFragmentFile",
		"conciergeIdentityFiles",
	}
	for _, id := range banned {
		// grep the source tree under workspace-server/internal/handlers for
		// the bare identifier. We allow the identifier to appear inside this
		// test (the regression guard itself) but nowhere else.
		out, err := exec.Command("grep", "-r", "--include=*.go",
			"-l", id, ".").CombinedOutput()
		if err != nil {
			// grep returns 1 when no matches — that's the PASS case.
			if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
				continue
			}
			t.Fatalf("grep failed: %v\n%s", err, out)
		}
		// If grep found matches, every file is either this test or the
		// production file's doc comment. Production file references must be
		// only in the doc-comment block that explains the de-hardcode (and
		// even there, the comments have been carefully worded to avoid
		// bare-identifier resolution). Allow this specific test file.
		for _, line := range bytes.Split(bytes.TrimSpace(out), []byte{'\n'}) {
			fname := string(bytes.TrimPrefix(line, []byte("./")))
			if fname == "platform_agent_test.go" {
				continue // regression guard itself
			}
			t.Errorf("concierge literal %q reappeared in %s — RFC #2843 §10a de-hardcode REGRESSED", id, fname)
		}
	}
}

// TestDefaultCreateParentID covers core#2697: new workspaces nest under the
// platform-agent root when one exists, else fall back to the SOLE plain root
// workspace (the JRS case — a lone SEO Agent at parent_id NULL), else "".
func TestDefaultCreateParentID(t *testing.T) {
	platQ := `SELECT id FROM workspaces WHERE COALESCE\(kind, 'workspace'\) = 'platform'`
	rootQ := `SELECT id FROM workspaces WHERE parent_id IS NULL`

	t.Run("prefers the platform-agent root when exactly one exists", func(t *testing.T) {
		mock := setupTestDB(t)
		mock.ExpectQuery(platQ).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("plat-1"))
		if got := defaultCreateParentID(context.Background()); got != "plat-1" {
			t.Fatalf("want plat-1, got %q", got)
		}
	})

	t.Run("falls back to the sole plain root when no platform-agent (JRS case)", func(t *testing.T) {
		mock := setupTestDB(t)
		mock.ExpectQuery(platQ).WillReturnRows(sqlmock.NewRows([]string{"id"})) // 0 platform
		mock.ExpectQuery(rootQ).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("seo-agent"))
		if got := defaultCreateParentID(context.Background()); got != "seo-agent" {
			t.Fatalf("want seo-agent (fallback to sole root), got %q", got)
		}
	})

	t.Run("returns empty when no platform and multiple roots (ambiguous)", func(t *testing.T) {
		mock := setupTestDB(t)
		mock.ExpectQuery(platQ).WillReturnRows(sqlmock.NewRows([]string{"id"}))
		mock.ExpectQuery(rootQ).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("r1").AddRow("r2"))
		if got := defaultCreateParentID(context.Background()); got != "" {
			t.Fatalf("want empty (ambiguous multi-root), got %q", got)
		}
	})
	t.Run("returns empty when MULTIPLE platform agents (ambiguous) — no root fallback (CR2 #2783)", func(t *testing.T) {
		mock := setupTestDB(t)
		// >1 platform → fail-soft empty; the root query must NOT run.
		mock.ExpectQuery(platQ).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("p1").AddRow("p2"))
		if got := defaultCreateParentID(context.Background()); got != "" {
			t.Fatalf("want empty (ambiguous multi-platform must NOT fall back to a root), got %q", got)
		}
	})
}

// TestRecordDeclaredPlugin_PrivilegedPluginEntitlement is the security gate for
// the org-management MCP plugin (RFC: rfc-platform-mcp-as-plugin). The privileged
// plugin carries the org-admin tool surface, so recordDeclaredPlugin — the single
// chokepoint every declaration path flows through — must REFUSE it for any
// non-platform workspace, regardless of how the declaration was sourced (template
// seed, org_import, or a user-authored workspace.yaml). This closes the
// privilege-escalation vector where a user workspace lists the plugin to mint
// itself org-admin tools.
func TestRecordDeclaredPlugin_PrivilegedPluginEntitlement(t *testing.T) {
	// recordDeclaredPlugin (plugins_tracking.go) runs the SINGLE-column kind
	// precheck — it does NOT read runtime — so its mock keeps the 1-column shape.
	const kindQuery = `SELECT COALESCE\(kind, 'workspace'\) FROM workspaces WHERE id =`
	const declaredInsert = `INSERT INTO workspace_declared_plugins`

	t.Run("platform concierge MAY declare the privileged management MCP", func(t *testing.T) {
		mock := setupTestDB(t)
		mock.ExpectQuery(kindQuery).WithArgs("ws-concierge").
			WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("platform"))
		mock.ExpectExec(declaredInsert).
			WithArgs("ws-concierge", conciergePlatformMCPName, sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
		if err := recordDeclaredPlugin(context.Background(), "ws-concierge", conciergePlatformMCPName, conciergePlatformMCPSource); err != nil {
			t.Fatalf("platform concierge declaration of the management MCP must succeed: %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
	})

	t.Run("non-platform workspace is REFUSED — no INSERT (privilege-escalation guard)", func(t *testing.T) {
		mock := setupTestDB(t)
		mock.ExpectQuery(kindQuery).WithArgs("ws-user").
			WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("workspace"))
		// NO ExpectExec: the gate MUST refuse before any INSERT fires.
		err := recordDeclaredPlugin(context.Background(), "ws-user", conciergePlatformMCPName, conciergePlatformMCPSource)
		if err == nil {
			t.Fatal("a non-platform workspace MUST NOT be able to declare the privileged management MCP plugin")
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations (an INSERT fired — that is the privilege escalation this gate must stop): %v", err)
		}
	})

	t.Run("an ordinary plugin skips the kind precheck entirely (no extra query)", func(t *testing.T) {
		mock := setupTestDB(t)
		// No kind precheck for non-privileged names — straight to the upsert.
		mock.ExpectExec(declaredInsert).
			WithArgs("ws-user", "browser-automation", sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
		if err := recordDeclaredPlugin(context.Background(), "ws-user", "browser-automation", "browser-automation"); err != nil {
			t.Fatalf("ordinary plugin declaration must succeed: %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
	})
}

// TestEnsureConciergeProvider_FailsClosedOnReadError is the CI regression gate
// for core#3162 (BYOK fail-open): a transient decrypt/read error on an EXISTING
// LLM_PROVIDER row used to be collapsed into "" and treated as "unset", which
// combined with a momentarily-empty MODEL could silently mis-pin a BYOK/self-host
// concierge onto the platform LLM proxy. The fix returns the error to the
// caller; ensureConciergeProvider MUST fail closed (return without seeding) so
// the next provision re-tries rather than silently mis-routing.
func TestEnsureConciergeProvider_FailsClosedOnReadError(t *testing.T) {
	h := &WorkspaceHandler{}
	const providerSelQuery = `SELECT encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = \$1 AND key = 'LLM_PROVIDER'`
	const secretInsert = `INSERT INTO workspace_secrets`

	t.Run("decrypt error on existing row fails closed (does NOT seed platform)", func(t *testing.T) {
		// Real encrypted_value bytes that cannot be decrypted by the current
		// key/algorithm: forces crypto.DecryptVersioned to return an error.
		// This is the realistic "the row exists but the ciphertext is unreadable"
		// case — exactly the failure mode that previously fell through to a
		// fail-OPEN platform pin.
		mock := setupTestDB(t)
		mock.ExpectQuery(providerSelQuery).WithArgs("ws-decrypt-fail").
			WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}).
				AddRow([]byte("corrupt-ciphertext-that-cannot-be-decrypted"), 0))
		// NO ExpectExec: the fail-closed path MUST NOT persist LLM_PROVIDER.

		env := map[string]string{} // empty MODEL — the mis-pin window
		h.ensureConciergeProvider(context.Background(), "ws-decrypt-fail", defaultConciergeRuntime, env)

		if _, pinned := env["LLM_PROVIDER"]; pinned {
			t.Errorf("transient decrypt error caused a platform provider pin (env=%v) — would mis-route a BYOK/self-host concierge", env)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations (a pin INSERT means the fail-closed path leaked): %v", err)
		}
	})

	t.Run("db scan error (non-ErrNoRows) fails closed", func(t *testing.T) {
		// A connection error / context-cancellation on the secret lookup. Not
		// the clean "row doesn't exist" case (sql.ErrNoRows) — this is a real
		// failure that must also fail closed.
		mock := setupTestDB(t)
		mock.ExpectQuery(providerSelQuery).WithArgs("ws-scan-fail").
			WillReturnError(fmt.Errorf("connection refused"))
		// NO ExpectExec: fail closed.

		env := map[string]string{}
		h.ensureConciergeProvider(context.Background(), "ws-scan-fail", defaultConciergeRuntime, env)

		if _, pinned := env["LLM_PROVIDER"]; pinned {
			t.Errorf("DB scan error caused a platform provider pin (env=%v)", env)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
	})

	t.Run("sql.ErrNoRows (genuine unset) proceeds to seed (no regression)", func(t *testing.T) {
		// The clean "no row exists" case MUST still let the caller proceed to
		// the platform-provider seed. This is the fresh-boot / cleared-secret
		// case the existing happy-path tests cover; we re-pin it here so a
		// future refactor of the error-path can break it loudly.
		mock := setupTestDB(t)
		mock.ExpectQuery(providerSelQuery).WithArgs("ws-fresh-unset").
			WillReturnError(sql.ErrNoRows)
		mock.ExpectExec(secretInsert).
			WithArgs("ws-fresh-unset", sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))

		env := map[string]string{} // empty MODEL → seed path
		h.ensureConciergeProvider(context.Background(), "ws-fresh-unset", defaultConciergeRuntime, env)

		if env["LLM_PROVIDER"] != conciergeProvider {
			t.Errorf("genuine unset did not pin LLM_PROVIDER=%q; got %q (env=%v) — regression on the seed path", conciergeProvider, env["LLM_PROVIDER"], env)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations (the seed path did not persist): %v", err)
		}
	})

	t.Run("existing stored provider is respected on successful read (no regression)", func(t *testing.T) {
		// Existing happy path: a stored provider row reads cleanly → caller
		// returns early without pinning. Pinned here as the regression
		// sentinel for the successful-read branch.
		mock := setupTestDB(t)
		mock.ExpectQuery(providerSelQuery).WithArgs("ws-existing-prov").
			WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}).
				AddRow([]byte("customer-picked-byok-provider"), 0))
		// NO ExpectExec: existing provider wins → no pin.

		env := map[string]string{}
		h.ensureConciergeProvider(context.Background(), "ws-existing-prov", defaultConciergeRuntime, env)

		if _, pinned := env["LLM_PROVIDER"]; pinned {
			t.Errorf("existing stored provider wrongly pinned platform (env=%v) — would override the customer pick", env)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
	})
}

// TestEnsureConciergeModel_FailsClosedOnReadError is the sibling regression
// gate for the MODEL half of the fail-open shape (paired with
// TestEnsureConciergeProvider_FailsClosedOnReadError). Same shape as core#3162:
// a transient decrypt/read error on an existing MODEL row used to be collapsed
// into "" and treated as "unset", which would silently overwrite the customer's
// model pick if the secret store later recovered (the seed path would re-fire
// on the next provision and the customer's choice would be lost without any
// error surfaced). The fix returns the error; ensureConciergeModel MUST fail
// closed (return without seeding) so the next provision re-tries rather than
// losing the customer's pick.
func TestEnsureConciergeModel_FailsClosedOnReadError(t *testing.T) {
	h := &WorkspaceHandler{}
	const modelSelQuery = `SELECT encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = \$1 AND key = 'MODEL'`
	const secretInsert = `INSERT INTO workspace_secrets`

	t.Run("decrypt error on existing MODEL row fails closed (does NOT seed default)", func(t *testing.T) {
		mock := setupTestDB(t)
		mock.ExpectQuery(modelSelQuery).WithArgs("ws-model-decrypt-fail").
			WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}).
				AddRow([]byte("corrupt-ciphertext-that-cannot-be-decrypted"), 0))
		// NO ExpectExec: the fail-closed path MUST NOT persist MODEL.

		env := map[string]string{}
		h.ensureConciergeModel(context.Background(), "ws-model-decrypt-fail", defaultConciergeRuntime, env)

		if _, seeded := env["MODEL"]; seeded {
			t.Errorf("transient decrypt error seeded MODEL=%q — would silently overwrite the customer's pick", env["MODEL"])
		}
		if _, seeded := env["MOLECULE_MODEL"]; seeded {
			t.Errorf("transient decrypt error seeded MOLECULE_MODEL=%q", env["MOLECULE_MODEL"])
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations (a MODEL INSERT means the fail-closed path leaked): %v", err)
		}
	})

	t.Run("db scan error (non-ErrNoRows) on MODEL fails closed", func(t *testing.T) {
		mock := setupTestDB(t)
		mock.ExpectQuery(modelSelQuery).WithArgs("ws-model-scan-fail").
			WillReturnError(fmt.Errorf("connection refused"))
		// NO ExpectExec: fail closed.

		env := map[string]string{}
		h.ensureConciergeModel(context.Background(), "ws-model-scan-fail", defaultConciergeRuntime, env)

		if _, seeded := env["MODEL"]; seeded {
			t.Errorf("DB scan error seeded MODEL=%q", env["MODEL"])
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
	})

	t.Run("sql.ErrNoRows (genuine unset MODEL) proceeds to seed (no regression)", func(t *testing.T) {
		mock := setupTestDB(t)
		mock.ExpectQuery(modelSelQuery).WithArgs("ws-model-fresh-unset").
			WillReturnError(sql.ErrNoRows)
		mock.ExpectExec(secretInsert).
			WithArgs("ws-model-fresh-unset", sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))

		env := map[string]string{}
		// Hermetic: the resolver is the authoritative source for the seed. Stub it
		// so this test is not coupled to SaaS env/CP reachability.
		setConciergeModelResolver(t, platformDefaultModelFallback, nil)
		h.ensureConciergeModel(context.Background(), "ws-model-fresh-unset", defaultConciergeRuntime, env)

		if env["MODEL"] != platformDefaultModelFallback {
			t.Errorf("genuine unset did not seed MODEL=%q; got %q (env=%v) — regression on the seed path", platformDefaultModelFallback, env["MODEL"], env)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations (the seed path did not persist): %v", err)
		}
	})

	t.Run("existing stored MODEL is respected on successful read (no regression)", func(t *testing.T) {
		mock := setupTestDB(t)
		mock.ExpectQuery(modelSelQuery).WithArgs("ws-model-existing").
			WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}).
				AddRow([]byte("anthropic:claude-opus-4-8"), 0))
		// NO ExpectExec: existing pick wins → no seed.

		env := map[string]string{}
		h.ensureConciergeModel(context.Background(), "ws-model-existing", defaultConciergeRuntime, env)

		if _, seeded := env["MODEL"]; seeded {
			t.Errorf("existing MODEL pick wrongly overwrote with default (env=%v)", env)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
	})
}

// TestDefaultResolveConciergeModel is the unit regression gate for #3267 follow-up #2:
// the concierge model MUST come from the control plane on SaaS tenants and from the
// operator env on self-hosted tenants; any missing/unreachable source fails closed.
func TestDefaultResolveConciergeModel(t *testing.T) {
	t.Run("SaaS path fetches MOLECULE_LLM_DEFAULT_MODEL from CP /cp/tenants/config", func(t *testing.T) {
		cpModel := "minimax/MiniMax-M2.7"
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/cp/tenants/config" {
				t.Errorf("unexpected CP path: %s", r.URL.Path)
			}
			if auth := r.Header.Get("Authorization"); auth != "Bearer cp-admin-token" {
				t.Errorf("unexpected Authorization header: %q", auth)
			}
			if org := r.Header.Get("X-Molecule-Org-Id"); org != "org-123" {
				t.Errorf("unexpected X-Molecule-Org-Id header: %q", org)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{
				"MOLECULE_LLM_DEFAULT_MODEL": cpModel,
			})
		}))
		defer ts.Close()

		t.Setenv("MOLECULE_ORG_ID", "org-123")
		t.Setenv("ADMIN_TOKEN", "cp-admin-token")
		t.Setenv("MOLECULE_CP_URL", ts.URL)

		got, err := defaultResolveConciergeModel(context.Background())
		if err != nil {
			t.Fatalf("defaultResolveConciergeModel returned error: %v", err)
		}
		if got != cpModel {
			t.Errorf("defaultResolveConciergeModel() = %q, want %q", got, cpModel)
		}
	})

	t.Run("SaaS path falls back to the shared platform default when CP returns no model key", func(t *testing.T) {
		// CP reachable (200) but the SSOT carries no platform default → the ONE
		// shared fallback (NOT a hard error). In prod the CP boot fail-closes on
		// an empty selector, so this only fires on a dev/e2e CP.
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]string{"other_key": "value"})
		}))
		defer ts.Close()

		t.Setenv("MOLECULE_ORG_ID", "org-123")
		t.Setenv("ADMIN_TOKEN", "cp-admin-token")
		t.Setenv("MOLECULE_CP_URL", ts.URL)

		got, err := defaultResolveConciergeModel(context.Background())
		if err != nil {
			t.Fatalf("CP-reachable-but-unconfigured must use the shared fallback, got error: %v", err)
		}
		if got != platformDefaultModelFallback {
			t.Errorf("defaultResolveConciergeModel() = %q, want shared fallback %q", got, platformDefaultModelFallback)
		}
	})

	t.Run("SaaS path fails closed on non-2xx CP response", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer ts.Close()

		t.Setenv("MOLECULE_ORG_ID", "org-123")
		t.Setenv("ADMIN_TOKEN", "cp-admin-token")
		t.Setenv("MOLECULE_CP_URL", ts.URL)

		got, err := defaultResolveConciergeModel(context.Background())
		if err == nil {
			t.Fatalf("expected error for 401, got model %q", got)
		}
		if !strings.Contains(err.Error(), "MISSING_MODEL") {
			t.Errorf("error %q does not contain MISSING_MODEL", err.Error())
		}
	})

	t.Run("self-hosted path reads MOLECULE_LLM_DEFAULT_MODEL env", func(t *testing.T) {
		t.Setenv("MOLECULE_ORG_ID", "")
		t.Setenv("ADMIN_TOKEN", "")
		t.Setenv("MOLECULE_LLM_DEFAULT_MODEL", "moonshot/kimi-k2.6")

		got, err := defaultResolveConciergeModel(context.Background())
		if err != nil {
			t.Fatalf("defaultResolveConciergeModel returned error: %v", err)
		}
		if got != "moonshot/kimi-k2.6" {
			t.Errorf("defaultResolveConciergeModel() = %q, want %q", got, "moonshot/kimi-k2.6")
		}
	})

	t.Run("self-hosted path falls back to the shared platform default when env is missing", func(t *testing.T) {
		// Self-hosted/local boot with no operator SSOT env → the ONE shared
		// fallback (NOT a hard error). This is the single platform default used
		// only when MOLECULE_LLM_DEFAULT_MODEL is unset.
		t.Setenv("MOLECULE_ORG_ID", "")
		t.Setenv("ADMIN_TOKEN", "")
		t.Setenv("MOLECULE_LLM_DEFAULT_MODEL", "")

		got, err := defaultResolveConciergeModel(context.Background())
		if err != nil {
			t.Fatalf("self-hosted SSOT-unset must use the shared fallback, got error: %v", err)
		}
		if got != platformDefaultModelFallback {
			t.Errorf("defaultResolveConciergeModel() = %q, want shared fallback %q", got, platformDefaultModelFallback)
		}
	})
}
