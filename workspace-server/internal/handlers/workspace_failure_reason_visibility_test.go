package handlers

// workspace_failure_reason_visibility_test.go — molecule-core#5035.
//
// The platform RECORDS why a provision was refused (workspaces.last_sample_error,
// written by markProvisionFailed / the bootstrap + heartbeat degrade paths) but
// the tenant read API DROPPED it, so a refused provision was indistinguishable
// from a silent one. Org `reno-stars`: POST /workspaces returned 201, the row went
// to status=failed with compute={}, and the stored 177-char reason ("missing
// required env vars TENANT_NAME, … — add them under Config → Env Vars …") was
// invisible on GET /workspaces/{id}. It took weeks to characterise.
//
// Two arms:
//   (1) READ — GET /workspaces/:id must carry last_sample_error, and
//       GET /workspaces must keep carrying it (regression guard: List already
//       does, Get explicitly delete()d it).
//   (2) CREATE — a template whose runtime_config.required_env the org cannot
//       satisfy must 422 at POST naming the missing vars, instead of 201 +
//       a failed row minutes later. Plus a negative control so the gate cannot
//       over-fire on a satisfied template.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/crypto"
)

// The exact stored reason on reno-stars workspace e0abf57d-e2bb-4aee-8834-15d3e78f7977.
const renoStarsStoredReason = "missing required env vars TENANT_NAME, TENANT_DOMAIN, TENANT_DOMAIN_APEX, TENANT_DOMAIN_FULL, TENANT_TIMEZONE — add them under Config → Env Vars (or as Global secrets) and retry"

// workspaceRowColumns is the scanWorkspaceRow column order shared by the Get
// and List queries (same list the existing suites spell out inline).
var workspaceRowColumns = []string{
	"id", "name", "role", "tier", "status", "agent_card", "url",
	"parent_id", "active_tasks", "max_concurrent_tasks", "last_error_rate", "last_sample_error",
	"uptime_seconds", "current_task", "runtime", "workspace_dir", "x", "y", "collapsed",
	"budget_limit", "monthly_spend",
	"broadcast_enabled", "talk_to_user_enabled", "compute", "kind",
	"loaded_mcp_tools",
}

// ==================== (1) READ ====================

// TestWorkspaceGet_SurfacesLastSampleError — the #5035 headline. A failed
// workspace whose row carries the stored refusal reason must return it on the
// single-workspace read; otherwise `status: failed` is the ONLY signal and the
// operator has no way to learn WHY without control-plane access.
func TestWorkspaceGet_SurfacesLastSampleError(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	id := "e0abf57d-e2bb-4aee-8834-15d3e78f7977"
	mock.ExpectQuery("SELECT w.id, w.name").
		WithArgs(id).
		WillReturnRows(sqlmock.NewRows(workspaceRowColumns).
			AddRow(id, "SEO Agent", "", 4, "failed", []byte(`null`), "",
				nil, 0, 1, 0.0, renoStarsStoredReason, 0, "", "claude-code",
				"", 0.0, 0.0, false,
				nil, 0, false, true, []byte(`{}`), "workspace", []byte(`[]`),
			))
	mock.ExpectQuery(`SELECT last_outbound_at FROM workspaces`).
		WithArgs(id).
		WillReturnRows(sqlmock.NewRows([]string{"last_outbound_at"}).AddRow(nil))
	mock.ExpectQuery(`SELECT last_heartbeat_at FROM workspaces`).
		WithArgs(id).
		WillReturnRows(sqlmock.NewRows([]string{"last_heartbeat_at"}).AddRow(nil))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: id}}
	c.Request = httptest.NewRequest("GET", "/workspaces/"+id, nil)

	handler.Get(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	got, present := resp["last_sample_error"]
	if !present {
		t.Fatalf("#5035: last_sample_error ABSENT from GET /workspaces/:id — the platform recorded %q but the API dropped it, which is exactly the silent-failure defect. Body: %s",
			renoStarsStoredReason, w.Body.String())
	}
	if got != renoStarsStoredReason {
		t.Errorf("last_sample_error = %v, want the stored reason %q", got, renoStarsStoredReason)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestWorkspaceGet_LastSampleErrorEmptyWhenUnset — a healthy workspace still
// carries the field (empty string, matching the List shape and the COALESCE
// to the empty string in both queries) so clients can render unconditionally.
func TestWorkspaceGet_LastSampleErrorEmptyWhenUnset(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	id := "cccccccc-5035-0000-0000-000000000001"
	mock.ExpectQuery("SELECT w.id, w.name").
		WithArgs(id).
		WillReturnRows(sqlmock.NewRows(workspaceRowColumns).
			AddRow(id, "Healthy Agent", "", 4, "online", []byte(`null`), "http://localhost:8001",
				nil, 0, 1, 0.0, "", 10, "", "claude-code",
				"", 0.0, 0.0, false,
				nil, 0, false, true, []byte(`{}`), "workspace", []byte(`[]`),
			))
	mock.ExpectQuery(`SELECT last_outbound_at FROM workspaces`).
		WithArgs(id).
		WillReturnRows(sqlmock.NewRows([]string{"last_outbound_at"}).AddRow(nil))
	mock.ExpectQuery(`SELECT last_heartbeat_at FROM workspaces`).
		WithArgs(id).
		WillReturnRows(sqlmock.NewRows([]string{"last_heartbeat_at"}).AddRow(nil))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: id}}
	c.Request = httptest.NewRequest("GET", "/workspaces/"+id, nil)

	handler.Get(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	got, present := resp["last_sample_error"]
	if !present {
		t.Fatalf("last_sample_error must be present (empty) on a healthy workspace: %s", w.Body.String())
	}
	if got != "" {
		t.Errorf("last_sample_error = %v, want \"\"", got)
	}

	// The other #955 strips must be unaffected by the #5035 change.
	for _, stripped := range []string{"current_task", "workspace_dir", "budget_limit", "monthly_spend"} {
		if _, exists := resp[stripped]; exists {
			t.Errorf("%s must stay stripped from the public GET response", stripped)
		}
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestWorkspaceList_KeepsLastSampleError — regression guard for the roster read.
// List already serializes the column (scanWorkspaceRow sets it and List does not
// delete it); this pins that so a future "strip sensitive fields" pass on the
// list path cannot silently recreate #5035 on the other read endpoint.
func TestWorkspaceList_KeepsLastSampleError(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	broadcaster := newTestBroadcaster()
	handler := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())

	mock.ExpectQuery("SELECT w.id, w.name").
		WillReturnRows(sqlmock.NewRows(workspaceRowColumns).
			AddRow("e0abf57d-e2bb-4aee-8834-15d3e78f7977", "SEO Agent", "", 4, "failed", []byte(`null`), "",
				nil, 0, 1, 0.0, renoStarsStoredReason, 0, "", "claude-code",
				"", 0.0, 0.0, false, nil, int64(0), false, true, []byte(`{}`), "workspace", []byte(`[]`),
			))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/workspaces", nil)

	handler.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if len(resp) != 1 {
		t.Fatalf("expected exactly 1 workspace in the roster, got %d", len(resp))
	}
	if resp[0]["last_sample_error"] != renoStarsStoredReason {
		t.Errorf("List dropped last_sample_error: got %v, want %q", resp[0]["last_sample_error"], renoStarsStoredReason)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ==================== (2) CREATE ====================

// writeRequiredEnvTemplate materializes a resolvable template dir whose
// config.yaml declares runtime_config.required_env — the seo-agent shape.
func writeRequiredEnvTemplate(t *testing.T, configsDir, name string, required []string) string {
	t.Helper()
	dir := filepath.Join(configsDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir template: %v", err)
	}
	cfg := "name: SEO Agent\nruntime: claude-code\nruntime_config:\n  model: MiniMax-M2.7\n"
	if len(required) > 0 {
		cfg += "  required_env:\n"
		for _, r := range required {
			cfg += "    - " + r + "\n"
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("write template config.yaml: %v", err)
	}
	return name
}

// driveRequiredEnvCreate runs WorkspaceHandler.Create against a template dir,
// with the platform-managed LLM proxy configured (the SaaS/prod shape, which is
// what disables the self-host TENANT_* default filler). globalSecretKeys are the
// key names the tenant's global_secrets table reports.
//
// Mirrors driveScheduledCreate (workspace_create_schedule_delivery_test.go):
// sqlmock runs unordered and only the load-bearing statements are expected;
// benign unmatched statements error non-fatally inside the handler by design.
func driveRequiredEnvCreate(t *testing.T, configsDir, templateName string, globalSecretKeys []string) *httptest.ResponseRecorder {
	t.Helper()

	gin.SetMode(gin.TestMode)
	mock := setupTestDB(t)
	mock.MatchExpectationsInOrder(false)
	setupTestRedis(t)

	t.Setenv("MOLECULE_LLM_BASE_URL", "https://api.example.test/api/v1/internal/llm/openai/v1")
	t.Setenv("MOLECULE_LLM_USAGE_TOKEN", "tenant-admin-token")
	t.Setenv("MOLECULE_DEPLOY_MODE", "saas")

	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", configsDir)
	wh.SetCPProvisioner(&captureCPProv{})

	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO workspaces`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	// The create-boundary required-env preflight reads the SAME secret stores the
	// provision-time preflight does. Both queries may be issued more than once
	// (preflight + provision goroutine), so keep the rows re-servable.
	globalRows := sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"})
	for _, k := range globalSecretKeys {
		enc, encErr := crypto.Encrypt([]byte(k + "-value"))
		if encErr != nil {
			t.Fatalf("encrypt global secret fixture: %v", encErr)
		}
		globalRows.AddRow(k, enc, crypto.CurrentEncryptionVersion())
	}
	mock.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM global_secrets`).
		WillReturnRows(globalRows)
	mock.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM workspace_secrets`).
		WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"}))
	mock.ExpectQuery(`FROM workspace_declared_plugins`).
		WillReturnRows(sqlmock.NewRows([]string{"plugin_name", "source_raw"}))
	mock.ExpectQuery(`FROM workspace_plugins`).
		WillReturnRows(sqlmock.NewRows([]string{"plugin_name", "source_raw"}))
	mock.ExpectExec(`UPDATE workspaces SET instance_id`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE workspaces SET status`).WillReturnResult(sqlmock.NewResult(0, 1))

	body := `{"name":"SEO Agent","runtime":"claude-code","model":"minimax/MiniMax-M2.7","template":"` +
		templateName + `","tier":4}`
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/workspaces", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	wh.Create(c)
	wh.waitAsyncForTest()
	return w
}

// TestWorkspaceCreate_MissingTemplateRequiredEnvReturns422 — the reno-stars
// reproduction. The seo-agent template declares five TENANT_* required_env vars
// and the org has none of them. Pre-fix: 201 Created, then status=failed with
// compute={} and the reason buried in a column the read API dropped. The create
// boundary must refuse instead, naming the missing vars.
func TestWorkspaceCreate_MissingTemplateRequiredEnvReturns422(t *testing.T) {
	configsDir := t.TempDir()
	required := []string{"TENANT_NAME", "TENANT_DOMAIN", "TENANT_DOMAIN_APEX", "TENANT_DOMAIN_FULL", "TENANT_TIMEZONE"}
	name := writeRequiredEnvTemplate(t, configsDir, "seo-agent", required)

	w := driveRequiredEnvCreate(t, configsDir, name, nil)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("#5035: create with unsatisfiable template required_env returned %d, want 422. "+
			"A 201 here is the defect: the provisioner refuses seconds later and the caller is told nothing. Body: %s",
			w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse 422 body: %v", err)
	}
	if resp["code"] != "MISSING_REQUIRED_ENV" {
		t.Errorf("code = %v, want MISSING_REQUIRED_ENV", resp["code"])
	}
	errMsg, _ := resp["error"].(string)
	for _, want := range required {
		if !strings.Contains(errMsg, want) {
			t.Errorf("422 error message does not name missing var %q: %q", want, errMsg)
		}
	}
	missing, ok := resp["missing"].([]interface{})
	if !ok || len(missing) != len(required) {
		t.Fatalf("missing = %v, want a %d-element list naming every unsatisfied var", resp["missing"], len(required))
	}
}

// TestWorkspaceCreate_TemplateRequiredEnvSatisfiedReturns201 — negative control.
// The SAME template, with every required var present as a global secret, must
// still create. Without this arm the gate above could pass vacuously by
// rejecting every templated create.
func TestWorkspaceCreate_TemplateRequiredEnvSatisfiedReturns201(t *testing.T) {
	configsDir := t.TempDir()
	required := []string{"TENANT_NAME", "TENANT_DOMAIN", "TENANT_DOMAIN_APEX", "TENANT_DOMAIN_FULL", "TENANT_TIMEZONE"}
	name := writeRequiredEnvTemplate(t, configsDir, "seo-agent", required)

	w := driveRequiredEnvCreate(t, configsDir, name, required)

	if w.Code != http.StatusCreated {
		t.Fatalf("a template whose required_env IS satisfied at org scope must still create: got %d, want 201. Body: %s",
			w.Code, w.Body.String())
	}
}

// TestWorkspaceCreate_TemplateWithoutRequiredEnvReturns201 — second negative
// control: a template that declares no required_env is untouched by the gate.
func TestWorkspaceCreate_TemplateWithoutRequiredEnvReturns201(t *testing.T) {
	configsDir := t.TempDir()
	name := writeRequiredEnvTemplate(t, configsDir, "plain-agent", nil)

	w := driveRequiredEnvCreate(t, configsDir, name, nil)

	if w.Code != http.StatusCreated {
		t.Fatalf("a template with no required_env must create unchanged: got %d, want 201. Body: %s",
			w.Code, w.Body.String())
	}
}

// TestCreateMissingRequiredEnv_IgnoresPlatformInjectedVars — unit arm on the
// preflight helper itself. Vars the provision pipeline assembles AFTER the
// secret load (model, role, LLM proxy, git identity, plugin-mutator output) are
// NOT knowable at the create boundary, so they must never trip the gate — the
// provision-time preflight, which sees the fully assembled env, stays the
// authority for those.
func TestCreateMissingRequiredEnv_IgnoresPlatformInjectedVars(t *testing.T) {
	cases := []struct {
		name     string
		required []string
		env      map[string]string
		want     []string
	}{
		{
			name:     "tenant identity vars are decidable at create",
			required: []string{"TENANT_NAME", "TENANT_DOMAIN"},
			env:      map[string]string{},
			want:     []string{"TENANT_NAME", "TENANT_DOMAIN"},
		},
		{
			name:     "platform-injected vars never trip the gate",
			required: []string{"MODEL", "MOLECULE_LLM_BASE_URL", "GIT_AUTHOR_NAME", "GITHUB_TOKEN", "ANTHROPIC_API_KEY", "OPENAI_API_KEY", "PARENT_ID", "LLM_PROVIDER"},
			env:      map[string]string{},
			want:     nil,
		},
		{
			name:     "mixed: only the undecidable-at-provision-time ones remain",
			required: []string{"MODEL", "TENANT_TIMEZONE"},
			env:      map[string]string{},
			want:     []string{"TENANT_TIMEZONE"},
		},
		{
			name:     "a var supplied by a secret is satisfied",
			required: []string{"TENANT_NAME"},
			env:      map[string]string{"TENANT_NAME": "Reno Stars"},
			want:     nil,
		},
		{
			name:     "an empty-valued secret does not satisfy",
			required: []string{"TENANT_NAME"},
			env:      map[string]string{"TENANT_NAME": ""},
			want:     []string{"TENANT_NAME"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := "runtime_config:\n  required_env:\n"
			for _, r := range tc.required {
				cfg += "    - " + r + "\n"
			}
			got := createBoundaryMissingRequiredEnv(map[string][]byte{"config.yaml": []byte(cfg)}, "", tc.env)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("createBoundaryMissingRequiredEnv() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCreateMissingRequiredEnv_FailsOpenWithoutConfig — no config.yaml anywhere
// (SaaS create whose template assets arrive out-of-band) must never 422. The
// provision-time preflight remains the backstop.
func TestCreateMissingRequiredEnv_FailsOpenWithoutConfig(t *testing.T) {
	if got := createBoundaryMissingRequiredEnv(nil, "", map[string]string{}); got != nil {
		t.Errorf("expected no missing vars when no config.yaml is available, got %v", got)
	}
	if got := createBoundaryMissingRequiredEnv(nil, t.TempDir(), map[string]string{}); got != nil {
		t.Errorf("expected no missing vars when the template dir has no config.yaml, got %v", got)
	}
}
