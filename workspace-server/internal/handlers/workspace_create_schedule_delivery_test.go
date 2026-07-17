package handlers

// workspace_create_schedule_delivery_test.go — end-to-end wiring proof for the
// P4b DIRECT workspace-create leg of the scheduler-as-trigger-plugin RFC §8A P3
// template-seeding seam (issue #4411). #4444 wired
// renderTemplateSchedulesYAML + appendYAMLBlockChecked into the ORG-IMPORT
// delivery path only (org_import.go); this suite proves the SAME render now
// runs on WorkspaceHandler.Create, closing the asymmetry: a directly-created
// workspace whose TEMPLATE declares `schedules:` must reach the provisioner
// Start() with those schedules rendered into the DELIVERED
// cfg.ConfigFiles["config.yaml"] as a top-level `schedules:` block (the
// runtime's seed_schedules_from_workspace_config reads exactly this), inlining
// prompt_file bodies.
//
// The render on the Create path reads the TEMPLATE's on-disk config.yaml as the
// base (configFiles is nil for a templated create — the delivered config.yaml
// is otherwise the template copy) and hands the combined file to the
// provisioner via ConfigFiles, which OVERRIDES the template copy. So the
// delivered config.yaml must carry BOTH the template's own keys AND the
// appended schedules block.
//
// Negative-control evidence (recorded in the PR): on the pre-change code Create
// never calls renderTemplateSchedulesYAML, so configFiles stays nil for a
// templated create and cfg.ConfigFiles["config.yaml"] is EMPTY — the golden
// test's "delivered config.yaml carries the schedules" assertion fails at the
// ok-check. The no-schedules control proves the block is never appended when
// the template declares none (byte-identical delivery). The source-order arm
// lives in scheduler_declare_before_provision_gate_test.go
// (renderTemplateSchedulesYAML before provisionWorkspaceAuto).

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
)

// writeCreateScheduleTemplate materializes a resolvable template dir under
// configsDir/<name>/ with a config.yaml. scheduleBlock is appended verbatim
// (already-indented YAML) when non-empty; extraFiles are written relative to
// the template dir (for prompt_file bodies). Returns the template name.
func writeCreateScheduleTemplate(t *testing.T, configsDir, name, scheduleBlock string, extraFiles map[string]string) string {
	t.Helper()
	dir := filepath.Join(configsDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir template: %v", err)
	}
	base := "name: Sched Template\nruntime: claude-code\nruntime_config:\n  model: MiniMax-M2.7\n"
	if scheduleBlock != "" {
		base += scheduleBlock
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(base), 0o644); err != nil {
		t.Fatalf("write template config.yaml: %v", err)
	}
	for rel, body := range extraFiles {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir extra: %v", err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("write extra %s: %v", rel, err)
		}
	}
	return name
}

// driveScheduledCreate runs WorkspaceHandler.Create for a templated,
// non-external workspace with a wired capture provisioner and returns the
// WorkspaceConfig delivered to Start. The template lives at
// configsDir/<templateName>. Model minimax/MiniMax-M2.7 derives to the closed
// `platform` provider for claude-code, so the create-boundary BYOK gate stays
// query-free. sqlmock runs unordered; only the load-bearing statements are
// expected — benign unmatched statements (template UPDATE, MODEL mint, provider
// pin, canvas layout, declared-plugin upsert, kind lookup, legacy DB seed)
// error non-fatally inside the handler by design.
func driveScheduledCreate(t *testing.T, configsDir, templateName string) provisioner.WorkspaceConfig {
	t.Helper()

	gin.SetMode(gin.TestMode)
	mock := setupTestDB(t)
	mock.MatchExpectationsInOrder(false)
	setupTestRedis(t)

	// Platform-managed LLM env so the provision prep passes its fail-closed
	// credential gates (same recipe as workspace_provision_shared_test.go /
	// org_import_schedule_delivery_test.go).
	t.Setenv("MOLECULE_LLM_BASE_URL", "https://api.example.test/api/v1/internal/llm/openai/v1")
	t.Setenv("MOLECULE_LLM_USAGE_TOKEN", "tenant-admin-token")
	t.Setenv("MOLECULE_DEPLOY_MODE", "saas")

	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", configsDir)
	capture := &captureCPProv{}
	wh.SetCPProvisioner(capture)

	// ── Parent-goroutine load-bearing statements (Create body) ──
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO workspaces`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	// ── Provision-goroutine statements (prepare + env assembly + Start) ──
	mock.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM global_secrets`).
		WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"}))
	mock.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM workspace_secrets`).
		WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"}))
	mock.ExpectQuery(`FROM workspace_declared_plugins`).
		WillReturnRows(sqlmock.NewRows([]string{"plugin_name", "source_raw"}))
	mock.ExpectQuery(`FROM workspace_plugins`).
		WillReturnRows(sqlmock.NewRows([]string{"plugin_name", "source_raw"}))
	mock.ExpectExec(`UPDATE workspaces SET instance_id`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	body := `{"name":"Sched Create","runtime":"claude-code","model":"minimax/MiniMax-M2.7","template":"` +
		templateName + `","parent_id":"parent-ws"}`
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/workspaces", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	wh.Create(c)
	if w.Code != http.StatusCreated {
		t.Fatalf("Create: status want 201, got %d: %s", w.Code, w.Body.String())
	}
	// Drain the provision goroutine BEFORE reading the capture (and before
	// setupTestDB's cleanup swaps db.DB back).
	wh.waitAsyncForTest()
	return capture.startedCfg(t)
}

// TestWorkspaceCreate_DeliversRenderedSchedulesToProvisioner — (a) golden: a
// directly-created workspace whose template declares 2 schedules (one inline
// prompt, one prompt_file) reaches Start with a delivered config.yaml that
// carries the `schedules:` block, the prompt_file BODY inlined, alongside the
// template's own keys.
func TestWorkspaceCreate_DeliversRenderedSchedulesToProvisioner(t *testing.T) {
	configsDir := t.TempDir()
	promptBody := "Daily digest:\n- check the queue: status\n- summarize\n"
	scheduleBlock := "schedules:\n" +
		"  - name: morning-inline\n" +
		"    cron_expr: \"0 9 * * *\"\n" +
		"    timezone: America/Vancouver\n" +
		"    prompt: Say good morning\n" +
		"  - name: daily-from-file\n" +
		"    cron_expr: \"30 18 * * 1-5\"\n" +
		"    prompt_file: daily.md\n" +
		"    enabled: false\n"
	name := writeCreateScheduleTemplate(t, configsDir, "sched-golden", scheduleBlock,
		map[string]string{"daily.md": promptBody})

	cfg := driveScheduledCreate(t, configsDir, name)

	delivered, ok := cfg.ConfigFiles["config.yaml"]
	if !ok || len(delivered) == 0 {
		// Reachable pre-change fail arm: without the render wiring configFiles
		// stays nil for a templated create.
		t.Fatal("no config.yaml in delivered ConfigFiles — the render was not wired into the Create path")
	}
	// The delivered file must retain the template's own keys AND carry the
	// appended block — proving base+block assembly, not a bare block that would
	// clobber the template config.
	if !strings.Contains(string(delivered), "runtime: claude-code") {
		t.Errorf("delivered config.yaml dropped the template base:\n%s", delivered)
	}
	entries := parseRenderedSchedules(t, delivered)
	if len(entries) != 2 {
		t.Fatalf("delivered config.yaml carries %d schedules, want 2:\n%s", len(entries), delivered)
	}
	first, second := entries[0], entries[1]
	if first["name"] != "morning-inline" || first["cron"] != "0 9 * * *" ||
		first["timezone"] != "America/Vancouver" || first["prompt"] != "Say good morning" ||
		first["enabled"] != true {
		t.Errorf("inline entry wrong: %#v", first)
	}
	if second["name"] != "daily-from-file" || second["cron"] != "30 18 * * 1-5" ||
		second["enabled"] != false || second["timezone"] != "UTC" {
		t.Errorf("file entry identity wrong: %#v", second)
	}
	// The load-bearing golden bit: the prompt_file BODY is inlined, and no
	// prompt_file ref ships (it would dangle inside the container).
	if second["prompt"] != promptBody {
		t.Errorf("prompt_file content not inlined: got %q want %q", second["prompt"], promptBody)
	}
	if strings.Contains(string(delivered), "prompt_file") {
		t.Errorf("delivered config.yaml must never carry prompt_file refs:\n%s", delivered)
	}
}

// TestWorkspaceCreate_NoSchedules_DeliversNoScheduleBlock — (b) negative
// control: a template with NO schedules must leave the delivered config.yaml
// untouched by this seam — configFiles is never allocated by the render path
// (byte-identical to the pre-P4b delivery), so no `schedules:` key appears.
func TestWorkspaceCreate_NoSchedules_DeliversNoScheduleBlock(t *testing.T) {
	configsDir := t.TempDir()
	name := writeCreateScheduleTemplate(t, configsDir, "sched-none", "", nil)

	cfg := driveScheduledCreate(t, configsDir, name)

	// The render seam allocates configFiles ONLY when it appends a block. With
	// no template schedules it never runs, so the delivered ConfigFiles carries
	// no config.yaml from this path (the provisioner delivers the template copy
	// via TemplatePath instead — byte-identical to today).
	if delivered, ok := cfg.ConfigFiles["config.yaml"]; ok {
		if strings.Contains(string(delivered), "schedules:") {
			t.Errorf("schedules block must be ABSENT for a no-schedules template:\n%s", delivered)
		}
	}
	if cfg.TemplatePath == "" {
		t.Errorf("no-schedules templated create must still deliver the template via TemplatePath")
	}
}

// TestWorkspaceCreate_ScheduleGuardsHoldOnCreatePath — (c) the #4444 render
// guards hold END-TO-END on the direct-create path (same shared
// renderTemplateSchedulesYAML): a schedule whose prompt_file opens with an
// indented first line renders PORTABLY (yaml.v3 block-scalar boot-brick class,
// PR #4444 CRITICAL) so the assembled config.yaml still parses, and a
// non-kebab-named schedule is SKIPPED (runtime name-contract) while the valid
// siblings survive. This re-proves the guards on the Create leg without
// reimplementing them.
func TestWorkspaceCreate_ScheduleGuardsHoldOnCreatePath(t *testing.T) {
	configsDir := t.TempDir()
	// Indented first line — the reviewer's PyYAML-breaking class.
	indentedBody := "  Please review:\n- item one\n    indented code\ndone"
	scheduleBlock := "schedules:\n" +
		"  - name: indented-review\n" +
		"    cron_expr: \"0 18 * * *\"\n" +
		"    prompt_file: review.md\n" +
		"  - name: Bad Name\n" + // space + uppercase → runtime would silently skip
		"    cron_expr: \"0 9 * * *\"\n" +
		"    prompt: skip me\n" +
		"  - name: plain-sibling\n" +
		"    cron_expr: \"*/5 * * * *\"\n" +
		"    prompt: still here\n"
	name := writeCreateScheduleTemplate(t, configsDir, "sched-guards", scheduleBlock,
		map[string]string{"review.md": indentedBody})

	cfg := driveScheduledCreate(t, configsDir, name)

	delivered, ok := cfg.ConfigFiles["config.yaml"]
	if !ok || len(delivered) == 0 {
		t.Fatal("no config.yaml delivered")
	}
	// Boot-brick guard: the ASSEMBLED document must parse (pre-fix emission of
	// the indented-first-line prompt fails right here).
	entries := parseRenderedSchedules(t, delivered)
	// The non-kebab "Bad Name" must be skipped; the two contract-valid entries
	// survive.
	if len(entries) != 2 {
		t.Fatalf("want 2 surviving schedules (non-kebab skipped), got %d:\n%s", len(entries), delivered)
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e["name"].(string)] = true
	}
	if !names["indented-review"] || !names["plain-sibling"] {
		t.Errorf("valid siblings missing: %#v", entries)
	}
	if names["Bad Name"] || strings.Contains(string(delivered), "Bad Name") {
		t.Errorf("non-contract name leaked into delivered config.yaml:\n%s", delivered)
	}
	// Indented-first-line prompt content preserved byte-exact, and the
	// known-broken block-scalar indicator is absent.
	for _, e := range entries {
		if e["name"] == "indented-review" && e["prompt"] != indentedBody {
			t.Errorf("indented prompt_file body not preserved: %q", e["prompt"])
		}
	}
	if strings.Contains(string(delivered), "|4") {
		t.Errorf("delivered config.yaml carries the PyYAML-breaking block-scalar indicator:\n%s", delivered)
	}
}
