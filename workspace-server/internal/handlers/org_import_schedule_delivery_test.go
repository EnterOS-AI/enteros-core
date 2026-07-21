package handlers

// org_import_schedule_delivery_test.go — end-to-end wiring proof for the
// scheduler-as-trigger-plugin RFC §8A P3 template-seeding seam (core leg,
// issue #4411 decision, 2026-07-17): an org-imported workspace whose org.yaml
// declares `schedules:` must reach the provisioner Start() with
//
//  1. the resolved schedules rendered into the DELIVERED
//     cfg.ConfigFiles["config.yaml"] as a top-level `schedules:` block (the
//     runtime's seed_schedules_from_workspace_config,
//     molecule-ai-workspace-runtime#318, reads exactly this), and
//  2. molecule-scheduler present in workspace_declared_plugins BEFORE the
//     provision goroutine assembles MOLECULE_DECLARED_PLUGINS — observed here
//     as (a) the declared-plugin upsert firing on the org-import path at all
//     (pre-fix it NEVER did; only schedules.go Create and
//     template_schedules.go declared) and (b) the assembled
//     MOLECULE_DECLARED_PLUGINS env carrying the pinned scheduler source at
//     Start()-time.
//
// Removing the renderTemplateSchedulesYAML call (or the
// ensureSchedulerPluginDeclared call) from org_import.go makes this test fail
// — verified during development; see also the source-order gate in
// scheduler_declare_before_provision_gate_test.go for the ordering arm.
//
// Negative control: the same import WITHOUT schedules must deliver a
// config.yaml with NO `schedules:` key (byte-level absence — the block is
// never appended, keeping no-schedule templates byte-identical to today).

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
)

// captureCPProv records the full WorkspaceConfig handed to Start so the test
// can assert on the DELIVERED ConfigFiles + EnvVars. Local to this file
// (same isolation rationale as trackingCPProv in
// workspace_provision_auto_test.go).
type captureCPProv struct {
	mu   sync.Mutex
	cfgs []provisioner.WorkspaceConfig
}

func (c *captureCPProv) Start(_ context.Context, cfg provisioner.WorkspaceConfig) (string, error) {
	c.mu.Lock()
	c.cfgs = append(c.cfgs, cfg)
	c.mu.Unlock()
	return "i-capture-" + cfg.WorkspaceID, nil
}
func (c *captureCPProv) Stop(_ context.Context, _ string) error         { return nil }
func (c *captureCPProv) StopAndPrune(_ context.Context, _ string) error { return nil }
func (c *captureCPProv) GetConsoleOutput(_ context.Context, _ string) (string, error) {
	return "", nil
}
func (c *captureCPProv) IsRunning(_ context.Context, _ string) (bool, error) { return true, nil }

func (c *captureCPProv) startedCfg(t *testing.T) provisioner.WorkspaceConfig {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.cfgs) != 1 {
		t.Fatalf("cpProv.Start called %d times, want exactly 1", len(c.cfgs))
	}
	return c.cfgs[0]
}

// driveScheduledImport runs createWorkspaceTree for one leaf with a wired
// capture provisioner and returns the WorkspaceConfig delivered to Start.
// withSchedule toggles the org.yaml schedules block (the negative-control
// axis). The sqlmock choreography expects only the load-bearing statements;
// benign unmatched statements (kind lookup, event log, secret mint) error
// non-fatally inside the handlers by design.
func driveScheduledImport(t *testing.T, withSchedule bool) provisioner.WorkspaceConfig {
	t.Helper()

	mock := setupTestDB(t)
	mock.MatchExpectationsInOrder(false)
	setupTestRedis(t)

	// Platform-managed LLM env so the provision prep passes its fail-closed
	// credential gates (same recipe as workspace_provision_shared_test.go).
	t.Setenv("MOLECULE_LLM_BASE_URL", "https://api.example.test/api/v1/internal/llm/openai/v1")
	t.Setenv("MOLECULE_LLM_USAGE_TOKEN", "tenant-admin-token")
	t.Setenv("MOLECULE_DEPLOY_MODE", "saas")

	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	capture := &captureCPProv{}
	wh.SetCPProvisioner(capture)
	h := &OrgHandler{workspace: wh, broadcaster: broadcaster}

	// ── Parent-goroutine statements (createWorkspaceTree itself) ──
	mock.ExpectQuery(`INSERT INTO workspaces`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ws-sched-leaf"))
	mock.ExpectExec(`INSERT INTO workspace_secrets`).
		WillReturnResult(sqlmock.NewResult(1, 1)) // MODEL persist (core#2594)
	mock.ExpectExec(`INSERT INTO canvas_layouts`).
		WillReturnResult(sqlmock.NewResult(1, 1))

	if withSchedule {
		// THE C2 assertion: the org-import path itself declares
		// molecule-scheduler (pre-provision). Args: (workspace_id, name, source).
		mock.ExpectExec(`INSERT INTO workspace_declared_plugins`).
			WithArgs(sqlmock.AnyArg(), SchedulerPluginName, SchedulerPluginSource).
			WillReturnResult(sqlmock.NewResult(1, 1))
	}

	// ── Provision-goroutine statements (prepare + env assembly) ──
	mock.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM global_secrets`).
		WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"}))
	mock.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM workspace_secrets`).
		WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"}))
	declaredRows := sqlmock.NewRows([]string{"plugin_name", "source_raw"})
	if withSchedule {
		// desiredPluginSources reads back what the parent goroutine declared
		// above (sqlmock has no real storage — the row mirrors the upsert this
		// test already asserted; the source-order gate test pins the
		// happens-before in code).
		declaredRows.AddRow(SchedulerPluginName, SchedulerPluginSource)
	}
	mock.ExpectQuery(`FROM workspace_declared_plugins`).WillReturnRows(declaredRows)
	mock.ExpectQuery(`FROM workspace_plugins`).
		WillReturnRows(sqlmock.NewRows([]string{"plugin_name", "source_raw"}))
	mock.ExpectExec(`UPDATE workspaces SET instance_id`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	ws := OrgWorkspace{
		Name:    "Scheduled Leaf",
		Runtime: "claude-code",
		Model:   "anthropic:claude-opus-4-7",
	}
	if withSchedule {
		ws.Schedules = []OrgSchedule{{
			Name:     "hourly-digest",
			CronExpr: "0 * * * *",
			Prompt:   "Summarize the last hour of activity.",
		}}
	}

	results := []map[string]interface{}{}
	provisionSem := make(chan struct{}, 1)
	if err := h.createWorkspaceTree(ws, nil, 0, 0, 0, 0, OrgDefaults{Tier: 3}, "", &results, provisionSem); err != nil {
		t.Fatalf("createWorkspaceTree: %v", err)
	}
	// Drain the provision goroutine BEFORE reading the capture (and before
	// setupTestDB's cleanup swaps db.DB back).
	wh.waitAsyncForTest()

	if withSchedule {
		// All load-bearing expectations — including the declared-plugin upsert
		// — must have fired.
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("load-bearing statements missing (declare-before-provision regressed): %v", err)
		}
	}
	return capture.startedCfg(t)
}

// TestOrgImport_DeliversRenderedSchedulesToProvisioner — the positive arm:
// delivered config.yaml carries the resolved schedules block; the assembled
// env carries the scheduler plugin source.
func TestOrgImport_DeliversRenderedSchedulesToProvisioner(t *testing.T) {
	cfg := driveScheduledImport(t, true)

	delivered, ok := cfg.ConfigFiles["config.yaml"]
	if !ok {
		t.Fatal("no config.yaml in delivered ConfigFiles")
	}
	entries := parseRenderedSchedules(t, delivered)
	if len(entries) != 1 {
		t.Fatalf("delivered config.yaml carries %d schedules, want 1:\n%s", len(entries), delivered)
	}
	got := entries[0]
	if got["name"] != "hourly-digest" || got["cron"] != "0 * * * *" ||
		got["prompt"] != "Summarize the last hour of activity." ||
		got["enabled"] != true || got["timezone"] != "UTC" {
		t.Errorf("delivered schedule entry wrong: %#v", got)
	}

	// The provision env must carry the scheduler source so first boot
	// installs the daemon (MOLECULE_DECLARED_PLUGINS → boot-install).
	if env := cfg.EnvVars["MOLECULE_DECLARED_PLUGINS"]; !strings.Contains(env, SchedulerPluginSource) {
		t.Errorf("MOLECULE_DECLARED_PLUGINS=%q does not carry the scheduler source %q", env, SchedulerPluginSource)
	}
}

// TestOrgImport_NoSchedules_DeliversNoScheduleBlock — the negative control:
// without org.yaml schedules, the delivered config.yaml must not gain a
// schedules key (the block is never appended — byte-identical assembly), and
// no scheduler source rides the declared-plugins env.
func TestOrgImport_NoSchedules_DeliversNoScheduleBlock(t *testing.T) {
	cfg := driveScheduledImport(t, false)

	delivered := string(cfg.ConfigFiles["config.yaml"])
	if delivered == "" {
		t.Fatal("no config.yaml delivered")
	}
	if strings.Contains(delivered, "schedules:") {
		t.Errorf("schedules block must be ABSENT for a no-schedules import:\n%s", delivered)
	}
	if env := cfg.EnvVars["MOLECULE_DECLARED_PLUGINS"]; strings.Contains(env, SchedulerPluginName) {
		t.Errorf("scheduler plugin must not be declared for a no-schedules import (env=%q)", env)
	}
}
