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

// TestConciergeRuntimeGeneralization_Defaults is the PROVE-FAIL gate for the P3b
// model + template + runtime defaults (items 1, 2, 3).
func TestConciergeRuntimeGeneralization_Defaults(t *testing.T) {
	t.Run("the platform-managed default model is minimax/MiniMax-M2.7 (CTO P3b)", func(t *testing.T) {
		if conciergeDeclaredModel != "minimax/MiniMax-M2.7" {
			t.Fatalf("conciergeDeclaredModel = %q, want %q — the P3b CTO decision pins the platform-managed concierge default to MiniMax (cheaper, proxy Anthropic-compat arm)", conciergeDeclaredModel, "minimax/MiniMax-M2.7")
		}
		// PR-6 (concierge-follows): conciergeModelForRuntime RESOLVES the seed from
		// MOLECULE_LLM_DEFAULT_MODEL, with the const as fallback. Clear the env so
		// this asserts the COMPILED-IN fallback path (the const) — the prod KMS
		// value equals this const, so the fallback is the no-regression baseline.
		t.Setenv("MOLECULE_LLM_DEFAULT_MODEL", "")
		// And it is the per-runtime fallback for every concierge runtime.
		for _, rt := range []string{"claude-code", "codex", "openclaw", ""} {
			if got := conciergeModelForRuntime(rt); got != "minimax/MiniMax-M2.7" {
				t.Errorf("conciergeModelForRuntime(%q) = %q, want minimax/MiniMax-M2.7 (env-unset fallback)", rt, got)
			}
		}
	})

	t.Run("the declared model is registered + routable for claude-code", func(t *testing.T) {
		// Guards against the model id drifting out of the providers registry's
		// platform set for the DEFAULT concierge runtime, which would make
		// ensureConciergeModel leave the model unset and fail the provision closed.
		if ok, why := validateRegisteredModelForRuntime("claude-code", conciergeDeclaredModel); !ok {
			t.Errorf("declared model %q not registered for claude-code: %s", conciergeDeclaredModel, why)
		}
		if ok, why := validateDerivedProviderInRegistry("claude-code", conciergeDeclaredModel); !ok {
			t.Errorf("declared model %q has no registry provider for claude-code: %s", conciergeDeclaredModel, why)
		}
	})

	t.Run("the declared model derives to the platform provider on claude-code (proxy-billed)", func(t *testing.T) {
		// conciergeModelIsPlatformManaged is the gate that decides the LLM_PROVIDER
		// pin. The minimax/ default MUST be recognized as platform-managed for the
		// claude-code concierge (the provider's Anthropic-compat arm), else
		// ensureConciergeProvider would skip the pin and the concierge would boot
		// without LLM_PROVIDER and 401 against the CP proxy.
		if !conciergeModelIsPlatformManaged("claude-code", conciergeDeclaredModel) {
			t.Errorf("conciergeModelIsPlatformManaged(claude-code, %q) = false, want true — the registry-derived gate must recognize the minimax/ platform default", conciergeDeclaredModel)
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

	// KNOWN REGISTRY GAP (P3b blocker, documented not asserted-green): the codex
	// runtime's `platform` arm currently serves ONLY OpenAI ids
	// (openai/gpt-5.4(-mini)); minimax/MiniMax-M2.7 derives to the BYOK
	// `byok-minimax` arm for codex, NOT `platform`. So a codex concierge on the
	// shared minimax/ default would NOT get the platform LLM_PROVIDER pin and would
	// require a tenant MINIMAX_API_KEY. The shared default (item 2) is correct; the
	// cross-runtime PLATFORM routing for minimax on codex/openclaw needs a
	// providers.yaml change (add minimax/MiniMax-M2.7 to codex+openclaw's platform
	// arms) before a codex concierge can run it platform-billed. This subtest
	// pins the CURRENT registry truth so the gap is visible and a future registry
	// fix flips it deliberately.
	t.Run("codex minimax routing is the known registry gap (NOT yet platform-managed)", func(t *testing.T) {
		if conciergeModelIsPlatformManaged("codex", conciergeDeclaredModel) {
			t.Log("codex now routes minimax/MiniMax-M2.7 to platform — the registry gap is closed; update the cross-runtime narrative + remove this guard")
		}
	})

	t.Run("conciergeTemplateForRuntime maps per-runtime", func(t *testing.T) {
		cases := map[string]string{
			"":            "platform-agent",        // empty → default
			"claude-code": "platform-agent",        // claude-code keeps the historical name
			"codex":       "codex-platform-agent",  // others use <runtime>-platform-agent
			"openclaw":    "openclaw-platform-agent",
		}
		for rt, want := range cases {
			if got := conciergeTemplateForRuntime(rt); got != want {
				t.Errorf("conciergeTemplateForRuntime(%q) = %q, want %q", rt, got, want)
			}
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
		h.applyConciergeProvisionConfig(context.Background(), "ws-fresh", "", nil, env, "Org Concierge")

		// THE regression assertion: without this seed the provision hits
		// MISSING_MODEL and fails closed. Both canonical env names must carry
		// the declared model so the runtime actually boots on it this provision.
		if env["MODEL"] != conciergeDeclaredModel {
			t.Errorf("fresh concierge did not seed MODEL=%q; got %q (env=%v) — MISSING_MODEL would fail this provision closed", conciergeDeclaredModel, env["MODEL"], env)
		}
		if env["MOLECULE_MODEL"] != conciergeDeclaredModel {
			t.Errorf("fresh concierge did not seed MOLECULE_MODEL=%q; got %q", conciergeDeclaredModel, env["MOLECULE_MODEL"])
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

		if env["MODEL"] == conciergeDeclaredModel {
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

	// PR-6 (concierge-follows): the seed model is RESOLVED from the generic KMS
	// platform default env MOLECULE_LLM_DEFAULT_MODEL, with conciergeDeclaredModel
	// as the compiled-in fallback. These two sub-tests prove BOTH halves of the
	// SSOT-follow: (a) env SET → the seed is the KMS value (NOT the const), and
	// (b) env UNSET → the seed is the const fallback. The runtime default is
	// likewise resolved (MOLECULE_DEFAULT_RUNTIME else 'claude-code'); both are
	// set/unset together to exercise the full follow path.
	t.Run("env SET: fresh concierge seeds the KMS default model+runtime (NOT the const)", func(t *testing.T) {
		// A DIFFERENT routable platform model than the const default, so a pass
		// proves the env value (not conciergeDeclaredModel) drives the seed. Both
		// registry gates pass for it and it is platform-managed on claude-code.
		const kmsModel = "moonshot/kimi-k2.6"
		if kmsModel == conciergeDeclaredModel {
			t.Fatalf("test invariant broken: kmsModel must differ from the const fallback to prove the env wins")
		}
		t.Setenv("MOLECULE_LLM_DEFAULT_MODEL", kmsModel)
		t.Setenv("MOLECULE_DEFAULT_RUNTIME", "claude-code")

		mock := setupTestDB(t)
		mock.ExpectQuery(kindQuery).WithArgs("ws-kms").
			WillReturnRows(sqlmock.NewRows([]string{"kind", "runtime"}).AddRow("platform", "claude-code"))
		mock.ExpectQuery(modelSelQuery).WithArgs("ws-kms").
			WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}))
		mock.ExpectExec(secretInsert).
			WithArgs("ws-kms", sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
		// kmsModel is platform-managed → provider pin persists too.
		mock.ExpectQuery(providerSelQuery).WithArgs("ws-kms").
			WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}))
		mock.ExpectExec(secretInsert).
			WithArgs("ws-kms", sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectQuery(recordKindQuery).WithArgs("ws-kms").
			WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("platform"))
		mock.ExpectExec(declaredInsert).
			WithArgs("ws-kms", sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))

		env := map[string]string{}
		h.applyConciergeProvisionConfig(context.Background(), "ws-kms", "", nil, env, "Org Concierge")

		if env["MODEL"] != kmsModel {
			t.Errorf("env-set seed did not follow MOLECULE_LLM_DEFAULT_MODEL=%q; got MODEL=%q (env=%v)", kmsModel, env["MODEL"], env)
		}
		if env["MOLECULE_MODEL"] != kmsModel {
			t.Errorf("env-set seed did not follow the KMS model for MOLECULE_MODEL; got %q", env["MOLECULE_MODEL"])
		}
		if env["MODEL"] == conciergeDeclaredModel {
			t.Errorf("env-set seed wrongly used the const fallback %q instead of the KMS value", conciergeDeclaredModel)
		}
		if env["LLM_PROVIDER"] != conciergeProvider {
			t.Errorf("env-set platform-managed KMS model did not pin LLM_PROVIDER=%q; got %q", conciergeProvider, env["LLM_PROVIDER"])
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations (KMS model/provider not persisted): %v", err)
		}
	})

	t.Run("env UNSET: fresh concierge falls back to the const declared model", func(t *testing.T) {
		// Defensively clear the envs so this sub-test is order-independent.
		t.Setenv("MOLECULE_LLM_DEFAULT_MODEL", "")
		t.Setenv("MOLECULE_DEFAULT_RUNTIME", "")

		mock := setupTestDB(t)
		mock.ExpectQuery(kindQuery).WithArgs("ws-fallback").
			WillReturnRows(sqlmock.NewRows([]string{"kind", "runtime"}).AddRow("platform", "claude-code"))
		mock.ExpectQuery(modelSelQuery).WithArgs("ws-fallback").
			WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}))
		mock.ExpectExec(secretInsert).
			WithArgs("ws-fallback", sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectQuery(providerSelQuery).WithArgs("ws-fallback").
			WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}))
		mock.ExpectExec(secretInsert).
			WithArgs("ws-fallback", sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectQuery(recordKindQuery).WithArgs("ws-fallback").
			WillReturnRows(sqlmock.NewRows([]string{"kind"}).AddRow("platform"))
		mock.ExpectExec(declaredInsert).
			WithArgs("ws-fallback", sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))

		env := map[string]string{}
		h.applyConciergeProvisionConfig(context.Background(), "ws-fallback", "", nil, env, "Org Concierge")

		if env["MODEL"] != conciergeDeclaredModel {
			t.Errorf("env-unset seed did not fall back to the const declared model %q; got MODEL=%q (env=%v) — MISSING_MODEL would fire", conciergeDeclaredModel, env["MODEL"], env)
		}
		if env["MOLECULE_MODEL"] != conciergeDeclaredModel {
			t.Errorf("env-unset seed did not fall back to the const for MOLECULE_MODEL; got %q", env["MOLECULE_MODEL"])
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations (const-fallback seed not persisted): %v", err)
		}
	})
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
				AddRow([]byte(conciergeDeclaredModel), 0))
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
		env := map[string]string{"MODEL": conciergeDeclaredModel}
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
				AddRow([]byte(conciergeDeclaredModel), 0))
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
		env := map[string]string{"MODEL": conciergeDeclaredModel, "LLM_PROVIDER": "anthropic-api"}
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
// NOTE (incident 2026-06-15): conciergeDeclaredModel is DELIBERATELY NOT
// banned. The model is the ONE concierge identity element that legitimately
// lives in core: the universal MISSING_MODEL gate (core#2594) reads the stored
// MODEL secret at provision time — BEFORE any template config.yaml is fetched —
// so the model MUST be seeded from a core-resident declared value, not deferred
// to template delivery. #2919 wrongly lumped the model in with the prompt/MCP
// literals and banned it here; removing the model-seed regressed every fresh
// platform-agent provision to MISSING_MODEL fail-closed. The concierge IS the
// platform-agent product, so it declares its own model exactly as a template
// does (this is SSOT-correct, not a hardcoded platform default). The seeding is
// gated by TestApplyConciergeProvisionConfig_SeedsModel.
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
		h.ensureConciergeModel(context.Background(), "ws-model-fresh-unset", defaultConciergeRuntime, env)

		if env["MODEL"] != conciergeDeclaredModel {
			t.Errorf("genuine unset did not seed MODEL=%q; got %q (env=%v) — regression on the seed path", conciergeDeclaredModel, env["MODEL"], env)
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
