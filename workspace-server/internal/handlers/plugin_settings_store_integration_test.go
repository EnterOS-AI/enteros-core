//go:build integration

package handlers

// Layer-6 storage against a REAL Postgres.
//
// The property this whole milestone exists to guarantee is one sentence: an
// operator's edit survives a re-provision. That cannot be proven with a mock —
// it depends on which columns a real UPSERT touches. So these run under the
// `integration` tag against INTEGRATION_DB_URL, alongside the other
// TestIntegration_* handlers tests.
//
// TestIntegration_PluginSettings_OverrideSurvivesReprovision is the assertion.
// TestIntegration_PluginSettings_SingleColumnWouldLoseTheEdit is its negative
// control: it reproduces the ONE-column design the four previous attempts used
// and shows the same edit being destroyed, so the survival above is a real
// property of the two-column split and not an accident of the test.

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"testing"

	_ "github.com/lib/pq"
)

func settingsTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("INTEGRATION_DB_URL")
	if dsn == "" {
		t.Skip("INTEGRATION_DB_URL unset")
	}
	conn, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := conn.Ping(); err != nil {
		t.Skipf("no database: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// seedSettingsWorkspace wraps the package's shared seedWorkspace helper with
// cleanup, so each test gets an isolated FK parent.
func seedSettingsWorkspace(t *testing.T, conn *sql.DB) string {
	t.Helper()
	id := seedWorkspace(t, conn, "plugin-settings-test")
	t.Cleanup(func() { conn.Exec(`DELETE FROM workspaces WHERE id = $1`, id) })
	return id
}

const testPlugin = "molecule-ai-plugin-scheduler"

// THE property. An operator edits one setting; the workspace re-provisions;
// the edit is still there and still wins.
func TestIntegration_PluginSettings_OverrideSurvivesReprovision(t *testing.T) {
	conn := settingsTestDB(t)
	ctx := context.Background()
	ws := seedSettingsWorkspace(t, conn)

	// Provision: the template says 30.
	if err := writeTemplateConfig(ctx, conn, ws, testPlugin,
		map[string]any{"poll_seconds": 30, "timezone": "UTC"}); err != nil {
		t.Fatal(err)
	}
	// Operator overrides one key.
	if _, err := patchOverrides(ctx, conn, ws, testPlugin,
		map[string]any{"poll_seconds": 5}, nil, "operator@molecule", -1); err != nil {
		t.Fatal(err)
	}
	// RE-PROVISION — the template is applied again, unchanged.
	if err := writeTemplateConfig(ctx, conn, ws, testPlugin,
		map[string]any{"poll_seconds": 30, "timezone": "UTC"}); err != nil {
		t.Fatal(err)
	}

	config, overrides, _, err := loadPluginSettings(ctx, conn, ws, testPlugin)
	if err != nil {
		t.Fatal(err)
	}
	eff := effectiveSettings(config, overrides)
	if eff["poll_seconds"] != float64(5) {
		t.Fatalf("THE EDIT DID NOT SURVIVE THE RE-PROVISION: poll_seconds = %v, want 5", eff["poll_seconds"])
	}
	// The un-overridden key still tracks the template.
	if eff["timezone"] != "UTC" {
		t.Errorf("timezone = %v, want UTC from the template layer", eff["timezone"])
	}

	// ...and GET can name the winning layer for BOTH kinds of key — the reason
	// provenance lives on `config` too, not only on `overrides`.
	resolved := resolveSettings(config, overrides)
	if resolved["poll_seconds"].Layer != layerOverride {
		t.Errorf("poll_seconds layer = %q, want %q", resolved["poll_seconds"].Layer, layerOverride)
	}
	if resolved["poll_seconds"].OverriddenFrom != float64(30) {
		t.Errorf("overridden_from = %v, want the masked template value 30", resolved["poll_seconds"].OverriddenFrom)
	}
	if resolved["timezone"].Layer != layerTemplate {
		t.Errorf("timezone layer = %q, want %q", resolved["timezone"].Layer, layerTemplate)
	}
}

// The negative control. Reproduces the ONE-column design and shows the same
// edit being destroyed — so the survival above is a property of the split, not
// of the test.
func TestIntegration_PluginSettings_SingleColumnWouldLoseTheEdit(t *testing.T) {
	conn := settingsTestDB(t)
	ctx := context.Background()
	ws := seedSettingsWorkspace(t, conn)

	// Same sequence, but the "edit" is written into `config` — the single
	// column every previous attempt used.
	if err := writeTemplateConfig(ctx, conn, ws, testPlugin, map[string]any{"poll_seconds": 30}); err != nil {
		t.Fatal(err)
	}
	if err := writeTemplateConfig(ctx, conn, ws, testPlugin, map[string]any{"poll_seconds": 5}); err != nil {
		t.Fatal(err) // stands in for "operator edit lands in config"
	}
	// Re-provision re-derives config from the template.
	if err := writeTemplateConfig(ctx, conn, ws, testPlugin, map[string]any{"poll_seconds": 30}); err != nil {
		t.Fatal(err)
	}

	config, overrides, _, err := loadPluginSettings(ctx, conn, ws, testPlugin)
	if err != nil {
		t.Fatal(err)
	}
	if got := effectiveSettings(config, overrides)["poll_seconds"]; got != float64(30) {
		t.Fatalf("expected the single-column design to LOSE the edit (30), got %v — "+
			"if this ever passes with 5, the negative control is broken and the "+
			"survival test above proves nothing", got)
	}
}

// Deleting an override reverts the key to the template, which is different
// from setting it to JSON null.
func TestIntegration_PluginSettings_DeleteOverrideRevertsToTemplate(t *testing.T) {
	conn := settingsTestDB(t)
	ctx := context.Background()
	ws := seedSettingsWorkspace(t, conn)

	if err := writeTemplateConfig(ctx, conn, ws, testPlugin, map[string]any{"poll_seconds": 30}); err != nil {
		t.Fatal(err)
	}
	if _, err := patchOverrides(ctx, conn, ws, testPlugin, map[string]any{"poll_seconds": 5}, nil, "op", -1); err != nil {
		t.Fatal(err)
	}
	if _, err := patchOverrides(ctx, conn, ws, testPlugin, nil, []string{"poll_seconds"}, "op", -1); err != nil {
		t.Fatal(err)
	}

	config, overrides, _, err := loadPluginSettings(ctx, conn, ws, testPlugin)
	if err != nil {
		t.Fatal(err)
	}
	if got := effectiveSettings(config, overrides)["poll_seconds"]; got != float64(30) {
		t.Errorf("after deleting the override the key should revert to 30, got %v", got)
	}
	if _, still := overrides["poll_seconds"]; still {
		t.Error("the override key should be gone, not set to null")
	}
}

// Compare-and-set: two operators reading the same version, one wins.
func TestIntegration_PluginSettings_ConcurrentEditIsRefusedNotLost(t *testing.T) {
	conn := settingsTestDB(t)
	ctx := context.Background()
	ws := seedSettingsWorkspace(t, conn)

	if err := writeTemplateConfig(ctx, conn, ws, testPlugin, map[string]any{"poll_seconds": 30}); err != nil {
		t.Fatal(err)
	}
	v1, err := patchOverrides(ctx, conn, ws, testPlugin, map[string]any{"poll_seconds": 5}, nil, "alice", 0)
	if err != nil {
		t.Fatal(err)
	}

	// Bob read v1 and writes — succeeds.
	if _, err := patchOverrides(ctx, conn, ws, testPlugin, map[string]any{"poll_seconds": 7}, nil, "bob", v1); err != nil {
		t.Fatalf("bob's edit at the current version should succeed: %v", err)
	}
	// Alice still holds v1 and writes — must be REFUSED, not silently applied.
	if _, err := patchOverrides(ctx, conn, ws, testPlugin, map[string]any{"poll_seconds": 9}, nil, "alice", v1); err != errOverridesVersionConflict {
		t.Fatalf("stale-version write should be refused with a conflict, got %v", err)
	}

	config, overrides, _, err := loadPluginSettings(ctx, conn, ws, testPlugin)
	if err != nil {
		t.Fatal(err)
	}
	if got := effectiveSettings(config, overrides)["poll_seconds"]; got != float64(7) {
		t.Errorf("bob's value should stand, got %v", got)
	}
}

// A re-provision that produces the SAME value must not churn the provenance
// stamp — otherwise every provision looks like an edit.
func TestIntegration_PluginSettings_UnchangedReprovisionDoesNotChurnProvenance(t *testing.T) {
	conn := settingsTestDB(t)
	ctx := context.Background()
	ws := seedSettingsWorkspace(t, conn)

	if err := writeTemplateConfig(ctx, conn, ws, testPlugin, map[string]any{"poll_seconds": 30}); err != nil {
		t.Fatal(err)
	}
	first, _, _, err := loadPluginSettings(ctx, conn, ws, testPlugin)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeTemplateConfig(ctx, conn, ws, testPlugin, map[string]any{"poll_seconds": 30}); err != nil {
		t.Fatal(err)
	}
	second, _, _, err := loadPluginSettings(ctx, conn, ws, testPlugin)
	if err != nil {
		t.Fatal(err)
	}
	if first["poll_seconds"].SetAt != second["poll_seconds"].SetAt {
		t.Errorf("provenance stamp churned on an unchanged re-provision: %q -> %q",
			first["poll_seconds"].SetAt, second["poll_seconds"].SetAt)
	}
	// ...and a real change DOES move it.
	if err := writeTemplateConfig(ctx, conn, ws, testPlugin, map[string]any{"poll_seconds": 31}); err != nil {
		t.Fatal(err)
	}
	third, _, _, err := loadPluginSettings(ctx, conn, ws, testPlugin)
	if err != nil {
		t.Fatal(err)
	}
	if third["poll_seconds"].SetAt == second["poll_seconds"].SetAt {
		t.Error("provenance stamp did NOT move on a real value change — the field is then useless")
	}
}

// The effective map is what the writer delivers to the box, so it must be plain
// values with no provenance wrapper leaking through.
func TestIntegration_PluginSettings_EffectiveMapIsDeliverable(t *testing.T) {
	conn := settingsTestDB(t)
	ctx := context.Background()
	ws := seedSettingsWorkspace(t, conn)

	if err := writeTemplateConfig(ctx, conn, ws, testPlugin,
		map[string]any{"poll_seconds": 30, "schedules": []any{map[string]any{"name": "a"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := patchOverrides(ctx, conn, ws, testPlugin, map[string]any{"poll_seconds": 5}, nil, "op", -1); err != nil {
		t.Fatal(err)
	}
	config, overrides, _, err := loadPluginSettings(ctx, conn, ws, testPlugin)
	if err != nil {
		t.Fatal(err)
	}
	body, err := renderPluginSettingsJSON(effectiveSettings(config, overrides))
	if err != nil {
		t.Fatal(err)
	}
	var round map[string]any
	if err := json.Unmarshal(body, &round); err != nil {
		t.Fatalf("delivered body is not valid JSON: %v", err)
	}
	if round["poll_seconds"] != float64(5) {
		t.Errorf("delivered poll_seconds = %v, want the override 5", round["poll_seconds"])
	}
	if _, leaked := round["value"]; leaked {
		t.Error("provenance wrapper leaked into the delivered file")
	}
	if _, ok := round["schedules"].([]any); !ok {
		t.Errorf("non-scalar template value did not survive: %#v", round["schedules"])
	}
}

// The property that makes layer 6 real ON THE BOX, not just in storage.
//
// TestIntegration_PluginSettings_OverrideSurvivesReprovision proves the DB
// keeps the edit. This proves the freshly provisioned workspace RECEIVES it —
// without the overlay, the DB would remember the override while the box quietly
// ran the template value, which is the same silent divergence this milestone
// keeps turning up.
func TestIntegration_PluginSettings_ReprovisionDeliversTheOverrideToTheBox(t *testing.T) {
	conn := settingsTestDB(t)
	ctx := context.Background()
	ws := seedSettingsWorkspace(t, conn)

	// Provision #1: render the template's plugin settings, as Create does.
	rendered, err := renderPluginSettingsFiles([]byte(`
plugins:
  - source: gitea://molecule-ai/molecule-ai-plugin-scheduler#v0.2.0
    config:
      poll_seconds: 30
      timezone: UTC
`), "ws")
	if err != nil {
		t.Fatal(err)
	}
	files, _ := mergePluginSettingsIntoConfigFiles(nil, rendered)
	if _, n := applyPluginSettingsLayers(ctx, conn, ws, files); n != 0 {
		t.Fatalf("no overrides exist yet, so nothing should be overlaid; got %d", n)
	}

	// Operator overrides one key.
	if _, err := patchOverrides(ctx, conn, ws, testPlugin,
		map[string]any{"poll_seconds": 5}, nil, "operator", -1); err != nil {
		t.Fatal(err)
	}

	// Provision #2 — the SAME template render, a fresh file map.
	rendered2, err := renderPluginSettingsFiles([]byte(`
plugins:
  - source: gitea://molecule-ai/molecule-ai-plugin-scheduler#v0.2.0
    config:
      poll_seconds: 30
      timezone: UTC
`), "ws")
	if err != nil {
		t.Fatal(err)
	}
	files2, _ := mergePluginSettingsIntoConfigFiles(nil, rendered2)
	files2, n := applyPluginSettingsLayers(ctx, conn, ws, files2)
	if n != 1 {
		t.Fatalf("expected the override to be overlaid onto 1 file, got %d", n)
	}

	var delivered map[string]any
	key := pluginSettingsDirName + "/" + testPlugin + ".json"
	if err := json.Unmarshal(files2[key], &delivered); err != nil {
		t.Fatalf("delivered file is not valid JSON: %v", err)
	}
	if delivered["poll_seconds"] != float64(5) {
		t.Errorf("THE BOX WOULD HAVE RECEIVED THE TEMPLATE VALUE: poll_seconds = %v, want the override 5",
			delivered["poll_seconds"])
	}
	if delivered["timezone"] != "UTC" {
		t.Errorf("un-overridden key should still track the template: %v", delivered["timezone"])
	}
}
