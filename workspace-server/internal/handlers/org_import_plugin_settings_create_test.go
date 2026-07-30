package handlers

// org_import_plugin_settings_create_test.go — the CREATE half of
// plugins[].config delivery.
//
// #4947 made /org/import re-deliver declared plugin settings to a workspace
// that ALREADY exists. It did nothing for the create path, and the create path
// was silently dropping the same data: the declaration loop projects entries to
// their SOURCE strings (pluginEntrySources) and never looks at `config:`, so a
// brand-new workspace was provisioned with no plugin-settings file at all.
//
// Found in production 2026-07-30 on the founder-org canary: importing a node
// that declared
//
//	plugins:
//	  - source: gitea://molecule-ai/molecule-ai-plugin-scheduler#v0.2.0
//	    config:
//	      schedules: [...]
//
// produced a workspace whose schedule grid read `[]`. Re-running the SAME
// import then populated it via the skip path — first import loses the config,
// second one fixes it, with nothing telling an operator to run it twice.
//
// The property under test is therefore NOT "does the renderer work" (covered by
// plugin_settings_delivery_test.go) but "does the org-import CREATE path put the
// rendered file into the bundle handed to the provisioner". That is the seam
// that was missing, so it is asserted at the provisioner boundary — the same
// place org_import_schedule_delivery_test.go asserts the config.yaml leg.
//
// Every refusal has a paired positive case, so a failure says which direction
// broke.

import (
	"encoding/json"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
)

const schedulerSettingsRelPath = "plugin-settings/molecule-ai-plugin-scheduler.json"

// drivePluginConfigImport runs createWorkspaceTree for one leaf whose plugin
// entries are supplied by the caller, and returns the WorkspaceConfig delivered
// to the provisioner. Mirrors driveScheduledImport's sqlmock choreography;
// expectations are deliberately loose (MatchExpectationsInOrder(false), benign
// unmatched statements error non-fatally inside the handlers by design) because
// the assertion lives on the delivered bundle, not on the SQL.
func drivePluginConfigImport(
	t *testing.T, defaultsPlugins, nodePlugins []templatePluginEntry,
) provisioner.WorkspaceConfig {
	t.Helper()

	mock := setupTestDB(t)
	mock.MatchExpectationsInOrder(false)
	setupTestRedis(t)

	t.Setenv("MOLECULE_LLM_BASE_URL", "https://api.example.test/api/v1/internal/llm/openai/v1")
	t.Setenv("MOLECULE_LLM_USAGE_TOKEN", "tenant-admin-token")
	t.Setenv("MOLECULE_DEPLOY_MODE", "saas")

	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	capture := &captureCPProv{}
	wh.SetCPProvisioner(capture)
	h := &OrgHandler{workspace: wh, broadcaster: broadcaster}

	mock.ExpectQuery(`INSERT INTO workspaces`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ws-plugin-config-leaf"))
	mock.ExpectExec(`INSERT INTO workspace_secrets`).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`INSERT INTO canvas_layouts`).
		WillReturnResult(sqlmock.NewResult(1, 1))

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

	ws := OrgWorkspace{
		Name:    "Plugin Config Leaf",
		Runtime: "claude-code",
		Model:   "anthropic:claude-opus-4-7",
		Plugins: nodePlugins,
	}

	results := []map[string]interface{}{}
	provisionSem := make(chan struct{}, 1)
	defaults := OrgDefaults{Tier: 3, Plugins: defaultsPlugins}
	if err := h.createWorkspaceTree(ws, nil, 0, 0, 0, 0, defaults, "", &results, provisionSem); err != nil {
		t.Fatalf("createWorkspaceTree: %v", err)
	}
	wh.waitAsyncForTest()

	return capture.startedCfg(t)
}

// deliveredSchedulerSettings decodes the scheduler's settings file out of the
// delivered bundle, failing the test when it is absent — the absence IS the
// production bug, so it must never read as an empty pass.
func deliveredSchedulerSettings(t *testing.T, cfg provisioner.WorkspaceConfig) map[string]any {
	t.Helper()
	body, ok := cfg.ConfigFiles[schedulerSettingsRelPath]
	if !ok {
		keys := make([]string, 0, len(cfg.ConfigFiles))
		for k := range cfg.ConfigFiles {
			keys = append(keys, k)
		}
		t.Fatalf("%s absent from delivered ConfigFiles (have %v)", schedulerSettingsRelPath, keys)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("delivered settings are not JSON: %v\n%s", err, body)
	}
	return out
}

// firstScheduleName pulls settings.schedules[0].name, asserting the list holds
// exactly one entry — so a layer that silently MERGED instead of replacing is a
// failure, not a pass.
func firstScheduleName(t *testing.T, settings map[string]any) string {
	t.Helper()
	list, ok := settings["schedules"].([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("settings.schedules = %#v, want exactly 1 entry", settings["schedules"])
	}
	entry, ok := list[0].(map[string]any)
	if !ok {
		t.Fatalf("schedule entry is not an object: %#v", list[0])
	}
	name, _ := entry["name"].(string)
	return name
}

// TestOrgImport_NodePluginConfig_IsDeliveredToProvisioner — the positive arm.
func TestOrgImport_NodePluginConfig_IsDeliveredToProvisioner(t *testing.T) {
	cfg := drivePluginConfigImport(t, nil, []templatePluginEntry{{
		Source: SchedulerPluginSource,
		Config: map[string]any{"schedules": []any{map[string]any{
			"name":      "hourly-digest",
			"cron_expr": "0 * * * *",
			"prompt":    "Summarize the last hour of activity.",
			"enabled":   true,
		}}},
	}})

	settings := deliveredSchedulerSettings(t, cfg)
	list, _ := settings["schedules"].([]any)
	if len(list) != 1 {
		t.Fatalf("settings.schedules = %#v, want 1 entry", settings["schedules"])
	}
	entry, _ := list[0].(map[string]any)
	if entry["name"] != "hourly-digest" || entry["cron_expr"] != "0 * * * *" {
		t.Errorf("delivered schedule entry wrong: %#v", entry)
	}
}

// TestOrgImport_BareStringPlugin_DeliversNoSettingsFile — negative control on
// the config axis. Every fleet template today uses the bare-string form; it must
// stay byte-identical to before (no settings file appears out of nowhere).
func TestOrgImport_BareStringPlugin_DeliversNoSettingsFile(t *testing.T) {
	cfg := drivePluginConfigImport(t, nil, []templatePluginEntry{{Source: SchedulerPluginSource}})

	for rel := range cfg.ConfigFiles {
		if rel != "config.yaml" {
			t.Errorf("unexpected delivered file %q for a config-less plugin entry", rel)
		}
	}
}

// TestOrgImport_DefaultsPluginConfig_IsDelivered — org `defaults.plugins` is the
// second precedence layer and was dropped by the same projection.
func TestOrgImport_DefaultsPluginConfig_IsDelivered(t *testing.T) {
	cfg := drivePluginConfigImport(t, []templatePluginEntry{{
		Source: SchedulerPluginSource,
		Config: map[string]any{"schedules": []any{map[string]any{"name": "from-defaults"}}},
	}}, nil)

	if got := firstScheduleName(t, deliveredSchedulerSettings(t, cfg)); got != "from-defaults" {
		t.Errorf("defaults layer not delivered, got %q", got)
	}
}

// TestOrgImport_NodePluginConfig_OverridesDefaults — precedence. A node that
// configures the same plugin as `defaults:` must win, matching the declaration
// merge's TEMPLATE -> DEFAULTS -> NODE order. Without last-wins layering the
// defaults' grid would silently ship to a node that replaced it.
func TestOrgImport_NodePluginConfig_OverridesDefaults(t *testing.T) {
	cfg := drivePluginConfigImport(t,
		[]templatePluginEntry{{
			Source: SchedulerPluginSource,
			Config: map[string]any{"schedules": []any{map[string]any{"name": "from-defaults"}}},
		}},
		[]templatePluginEntry{{
			Source: SchedulerPluginSource,
			Config: map[string]any{"schedules": []any{map[string]any{"name": "from-node"}}},
		}},
	)

	if got := firstScheduleName(t, deliveredSchedulerSettings(t, cfg)); got != "from-node" {
		t.Errorf("node layer must override defaults, got %q", got)
	}
}

// TestOrgImport_OptOutMarkerCarriesNoSettings — "!source" / "-source" are
// REMOVALS in the merge grammar. A removal marker that somehow carries a config
// must not materialise a settings file for the plugin it is removing.
func TestOrgImport_OptOutMarkerCarriesNoSettings(t *testing.T) {
	cfg := drivePluginConfigImport(t, nil, []templatePluginEntry{{
		Source: "!" + SchedulerPluginSource,
		Config: map[string]any{"schedules": []any{map[string]any{"name": "must-not-ship"}}},
	}})

	if _, ok := cfg.ConfigFiles[schedulerSettingsRelPath]; ok {
		t.Error("an opt-out marker must not deliver a settings file")
	}
}

// TestOrgImport_NodeOptOutRemovesInheritedSettings — the case that merely
// SKIPPING the marker does not cover.
//
// `defaults:` configures the scheduler; the node DECLINES it with "!source".
// Skip-only leaves the defaults' file in place, so the workspace ships live
// config for a plugin it deliberately does not have. Nothing on the box opens
// it — which is precisely why this would never be noticed.
func TestOrgImport_NodeOptOutRemovesInheritedSettings(t *testing.T) {
	cfg := drivePluginConfigImport(t,
		[]templatePluginEntry{{
			Source: SchedulerPluginSource,
			Config: map[string]any{"schedules": []any{map[string]any{"name": "declined"}}},
		}},
		[]templatePluginEntry{{Source: "!" + SchedulerPluginSource}},
	)

	if _, ok := cfg.ConfigFiles[schedulerSettingsRelPath]; ok {
		t.Error("a declined plugin must not keep the settings file its inherited layer rendered")
	}
}

// --- renderPluginSettingsFromEntries, directly -------------------------------

func TestRenderPluginSettingsFromEntries_LastLayerWins(t *testing.T) {
	out := renderPluginSettingsFromEntries("ws",
		[]templatePluginEntry{{Source: SchedulerPluginSource, Config: map[string]any{"k": "template"}}},
		[]templatePluginEntry{{Source: SchedulerPluginSource, Config: map[string]any{"k": "defaults"}}},
		[]templatePluginEntry{{Source: SchedulerPluginSource, Config: map[string]any{"k": "node"}}},
	)
	body, ok := out[schedulerSettingsRelPath]
	if !ok {
		t.Fatalf("no settings rendered, got keys %v", out)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if got["k"] != "node" {
		t.Errorf("last layer must win, got %v", got["k"])
	}
}

func TestRenderPluginSettingsFromEntries_NoConfigIsNil(t *testing.T) {
	if out := renderPluginSettingsFromEntries("ws",
		[]templatePluginEntry{{Source: SchedulerPluginSource}},
		nil,
	); out != nil {
		t.Errorf("config-less entries must render nothing, got %v", out)
	}
}

func TestRenderPluginSettingsFromEntries_DistinctPluginsBothRendered(t *testing.T) {
	out := renderPluginSettingsFromEntries("ws", []templatePluginEntry{
		{Source: SchedulerPluginSource, Config: map[string]any{"a": 1}},
		{Source: "gitea://molecule-ai/molecule-ai-plugin-ecc#v1", Config: map[string]any{"b": 2}},
	})
	if len(out) != 2 {
		t.Fatalf("want 2 files, got %d: %v", len(out), out)
	}
}

func TestRenderPluginSettingsFromEntries_OptOutRemovesEarlierLayer(t *testing.T) {
	out := renderPluginSettingsFromEntries("ws",
		[]templatePluginEntry{{Source: SchedulerPluginSource, Config: map[string]any{"k": "template"}}},
		[]templatePluginEntry{{Source: "-" + SchedulerPluginSource}},
	)
	if out != nil {
		t.Errorf("opt-out must clear the inherited settings, got %v", out)
	}
}

// A same-layer removal is order-independent: removals run before renders inside
// a layer, so the sort cannot decide the outcome.
func TestRenderPluginSettingsFromEntries_SameLayerOptOutBeatsDeclaration(t *testing.T) {
	out := renderPluginSettingsFromEntries("ws", []templatePluginEntry{
		{Source: SchedulerPluginSource, Config: map[string]any{"k": "v"}},
		{Source: "!" + SchedulerPluginSource},
	})
	if _, ok := out[schedulerSettingsRelPath]; ok {
		t.Error("a removal in the same layer must win over the declaration")
	}
}

// An opt-out must not take unrelated plugins down with it.
func TestRenderPluginSettingsFromEntries_OptOutIsScopedToItsTarget(t *testing.T) {
	out := renderPluginSettingsFromEntries("ws",
		[]templatePluginEntry{
			{Source: SchedulerPluginSource, Config: map[string]any{"a": 1}},
			{Source: "gitea://molecule-ai/molecule-ai-plugin-ecc#v1", Config: map[string]any{"b": 2}},
		},
		[]templatePluginEntry{{Source: "!" + SchedulerPluginSource}},
	)
	if _, ok := out[schedulerSettingsRelPath]; ok {
		t.Error("declined plugin's settings must be gone")
	}
	if _, ok := out["plugin-settings/molecule-ai-plugin-ecc.json"]; !ok {
		t.Errorf("an unrelated plugin's settings must survive, got %v", out)
	}
}

// templateConfigPluginEntriesFrom must fail SOFT: a malformed template config
// cannot be allowed to abort an org import.
func TestTemplateConfigPluginEntriesFrom_MalformedYAMLIsSkipped(t *testing.T) {
	if got := templateConfigPluginEntriesFrom([]byte("plugins: [oops\n  - broken"), "ws"); got != nil {
		t.Errorf("malformed template config must yield no entries, got %v", got)
	}
}

func TestTemplateConfigPluginEntriesFrom_ReadsBothForms(t *testing.T) {
	got := templateConfigPluginEntriesFrom([]byte(
		"plugins:\n  - bare-name\n  - source: "+SchedulerPluginSource+"\n    config:\n      k: v\n"), "ws")
	if len(got) != 2 {
		t.Fatalf("want 2 entries, got %d: %#v", len(got), got)
	}
	if got[0].Source != "bare-name" || len(got[0].Config) != 0 {
		t.Errorf("bare-string entry wrong: %#v", got[0])
	}
	if got[1].Source != SchedulerPluginSource || got[1].Config["k"] != "v" {
		t.Errorf("object entry wrong: %#v", got[1])
	}
}
