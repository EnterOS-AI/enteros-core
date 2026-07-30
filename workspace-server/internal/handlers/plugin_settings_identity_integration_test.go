//go:build integration

package handlers

// The two-identity landmine, and the uninstall/reinstall decision, against a
// REAL Postgres.
//
// Neither property can be proven with a mock. "The manifest name addresses a
// file nobody reads" is a statement about which rows exist in
// workspace_declared_plugins / workspace_plugins, and "an override survives an
// uninstall" is a statement about which rows an uninstall does NOT delete.
//
// Every assertion below is paired with a NEGATIVE CONTROL that reproduces the
// unfixed behaviour and shows it failing, so a green here is evidence and not
// an accident of the test's shape.

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

// The two names of ONE plugin. Both are real, both are live at once, and only
// the first addresses the settings channel.
const (
	identityInstallName  = "molecule-ai-plugin-scheduler" // plugin.name — the repo/dir
	identityManifestName = "molecule-scheduler"           // plugin.manifest.name — daemon provenance
)

func declarePlugin(t *testing.T, conn *sql.DB, ws, name string) {
	t.Helper()
	if _, err := conn.ExecContext(context.Background(), `
		INSERT INTO workspace_declared_plugins (workspace_id, plugin_name, source_raw)
		VALUES ($1, $2, $3)
		ON CONFLICT (workspace_id, plugin_name) DO NOTHING`,
		ws, name, "gitea://molecule-ai/"+name+"#main"); err != nil {
		t.Fatalf("declare %s: %v", name, err)
	}
}

func installPlugin(t *testing.T, conn *sql.DB, ws, name string) {
	t.Helper()
	if _, err := conn.ExecContext(context.Background(), `
		INSERT INTO workspace_plugins (workspace_id, plugin_name, source_raw)
		VALUES ($1, $2, $3)
		ON CONFLICT (workspace_id, plugin_name) DO NOTHING`,
		ws, name, "gitea://molecule-ai/"+name+"#main"); err != nil {
		t.Fatalf("install %s: %v", name, err)
	}
}

func uninstallPlugin(t *testing.T, conn *sql.DB, ws, name string) {
	t.Helper()
	// EXACTLY what PluginsHandler.Uninstall does (plugins_tracking.go): it
	// removes the INSTALLED row and leaves the DECLARED row alone. It does not
	// touch workspace_plugin_settings — which is the decision under test.
	if _, err := conn.ExecContext(context.Background(),
		`DELETE FROM workspace_plugins WHERE workspace_id = $1 AND plugin_name = $2`,
		ws, name); err != nil {
		t.Fatalf("uninstall %s: %v", name, err)
	}
}

// declarePluginWithSource declares a row whose plugin_name and source_raw
// DISAGREE — the real production shape for the scheduler, which
// ensureSchedulerPluginDeclared records under its MANIFEST name while carrying
// the repo source. declarePlugin above cannot express this: it derives the source
// from the name, so both always agree and the hazard is invisible.
func declarePluginWithSource(t *testing.T, conn *sql.DB, ws, name, source string) {
	t.Helper()
	if _, err := conn.ExecContext(context.Background(), `
		INSERT INTO workspace_declared_plugins (workspace_id, plugin_name, source_raw)
		VALUES ($1, $2, $3)
		ON CONFLICT (workspace_id, plugin_name) DO UPDATE SET source_raw = $3`,
		ws, name, source); err != nil {
		t.Fatalf("declare %s (source %s): %v", name, source, err)
	}
}

// THE PRODUCTION SHAPE, and the 404 it produced.
//
// ensureSchedulerPluginDeclared writes plugin_name = SchedulerPluginName
// ("molecule-scheduler", the MANIFEST name) with source_raw = the scheduler repo.
// Verified on a live prod concierge 2026-07-30: the allow-list therefore held
// "molecule-scheduler" and NOT "molecule-ai-plugin-scheduler", so a GET/PATCH on
// the install name — the only name the settings file, org-import re-delivery and
// the runtime's plugin.name lookup all use — returned 404.
//
// Resolution derives the name from source_raw, so the install name resolves and
// the manifest name stays refused. Both directions asserted: accepting the
// manifest name would put the operator back on a path to a file nothing opens.
func TestIntegration_PluginSettings_SchedulerDeclaredUnderManifestNameStillResolvesByInstallName(t *testing.T) {
	conn := settingsTestDB(t)
	ctx := context.Background()
	ws := seedSettingsWorkspace(t, conn)

	// Exactly what core writes at every provision.
	declarePluginWithSource(t, conn, ws, identityManifestName,
		"gitea://molecule-ai/molecule-ai-plugin-scheduler#v0.2.0")

	got, err := resolvePluginInstallName(ctx, conn, ws, identityInstallName)
	if err != nil {
		t.Fatalf("install name must resolve when the row is stored under the manifest name: %v", err)
	}
	if got != identityInstallName {
		t.Fatalf("resolved to %q, want %q", got, identityInstallName)
	}

	if _, err := resolvePluginInstallName(ctx, conn, ws, identityManifestName); !errors.Is(err, errPluginNotOnWorkspace) {
		t.Fatalf("the manifest name must STAY refused (it addresses a file nothing opens), got %v", err)
	}

	// And the refusal hint must name the install name, or the 404 is unactionable.
	known, err := workspacePluginInstallNames(ctx, conn, ws)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := known[identityInstallName]; !ok {
		t.Fatalf("allow-list must contain %q, got %v", identityInstallName, sortedNames(known))
	}
	if _, ok := known[identityManifestName]; ok {
		t.Fatalf("allow-list must NOT contain the manifest name, got %v", sortedNames(known))
	}
}

// A row whose source cannot be parsed must fall back to plugin_name rather than
// vanish. Deriving the name is only safe if it never DROPS a plugin: a
// hand-written bare local name has no repo to derive from, and an operator must
// still be able to address its settings.
func TestIntegration_PluginSettings_UnparseableSourceFallsBackToStoredName(t *testing.T) {
	conn := settingsTestDB(t)
	ctx := context.Background()
	ws := seedSettingsWorkspace(t, conn)

	declarePluginWithSource(t, conn, ws, "hand-rolled-plugin", "")

	got, err := resolvePluginInstallName(ctx, conn, ws, "hand-rolled-plugin")
	if err != nil {
		t.Fatalf("a source-less row must still resolve by its stored name: %v", err)
	}
	if got != "hand-rolled-plugin" {
		t.Fatalf("resolved to %q, want %q", got, "hand-rolled-plugin")
	}
}

// THE PIN. The install name resolves; the manifest name is REFUSED.
func TestIntegration_PluginSettings_LookupPinnedToInstallName(t *testing.T) {
	conn := settingsTestDB(t)
	ctx := context.Background()
	ws := seedSettingsWorkspace(t, conn)
	declarePlugin(t, conn, ws, identityInstallName)

	got, err := resolvePluginInstallName(ctx, conn, ws, identityInstallName)
	if err != nil {
		t.Fatalf("install name must resolve, got %v", err)
	}
	if got != identityInstallName {
		t.Fatalf("resolved to %q, want %q", got, identityInstallName)
	}

	// The manifest name is a real name for this same plugin — and it must NOT
	// be accepted, because plugin-settings/molecule-scheduler.json is a file
	// the runtime never opens.
	if _, err := resolvePluginInstallName(ctx, conn, ws, identityManifestName); !errors.Is(err, errPluginNotOnWorkspace) {
		t.Fatalf("manifest name must be refused with errPluginNotOnWorkspace, got %v", err)
	}
}

// NEGATIVE CONTROL for the pin: without resolution — i.e. using the request
// parameter verbatim, which is what the code did before — the manifest name
// writes a real override row and a real settings path, and BOTH diverge from
// the ones core writes and the runtime reads. This is the silent no-op the pin
// exists to prevent; it must be demonstrably reachable.
func TestIntegration_PluginSettings_UnpinnedManifestNameWritesAFileNobodyReads(t *testing.T) {
	conn := settingsTestDB(t)
	ctx := context.Background()
	ws := seedSettingsWorkspace(t, conn)
	declarePlugin(t, conn, ws, identityInstallName)

	// What PROVISION writes and the runtime reads.
	if err := writeTemplateConfig(ctx, conn, ws, identityInstallName,
		map[string]any{"poll_seconds": 30}); err != nil {
		t.Fatal(err)
	}
	realPath, err := pluginSettingsRelPath(identityInstallName)
	if err != nil {
		t.Fatal(err)
	}

	// What an UNPINNED PATCH keyed on the manifest name would do: it succeeds.
	if _, err := patchOverrides(ctx, conn, ws, identityManifestName,
		map[string]any{"poll_seconds": 5}, nil, "operator@molecule", -1); err != nil {
		t.Fatalf("control: the unpinned write is expected to SUCCEED (that is the bug): %v", err)
	}
	phantomPath, err := pluginSettingsRelPath(identityManifestName)
	if err != nil {
		t.Fatal(err)
	}
	if phantomPath == realPath {
		t.Fatal("control is not exercising the hazard: both names produced the same path")
	}

	// The edit landed under the WRONG key: the plugin the runtime actually
	// reads still resolves to the template value, so the operator's 200 OK
	// changed nothing on the box.
	config, overrides, _, err := loadPluginSettings(ctx, conn, ws, identityInstallName)
	if err != nil {
		t.Fatal(err)
	}
	if len(overrides) != 0 {
		t.Fatalf("control: expected NO override under the install name, got %v", overrides)
	}
	if v := effectiveSettings(config, overrides)["poll_seconds"]; v.(float64) != 30 {
		t.Fatalf("control: effective value should still be the template's 30, got %v", v)
	}
	t.Logf("negative control reproduced: operator edit landed in %q while the runtime reads %q", phantomPath, realPath)
}

// THE UNINSTALL/REINSTALL DECISION: overrides are RETAINED. An uninstall is
// about the plugin; the override is about the operator's intent.
func TestIntegration_PluginSettings_OverridesSurviveUninstallReinstall(t *testing.T) {
	conn := settingsTestDB(t)
	ctx := context.Background()
	ws := seedSettingsWorkspace(t, conn)
	declarePlugin(t, conn, ws, identityInstallName)
	installPlugin(t, conn, ws, identityInstallName)

	if err := writeTemplateConfig(ctx, conn, ws, identityInstallName,
		map[string]any{"poll_seconds": 30, "timezone": "UTC"}); err != nil {
		t.Fatal(err)
	}
	if _, err := patchOverrides(ctx, conn, ws, identityInstallName,
		map[string]any{"poll_seconds": 5}, nil, "operator@molecule", -1); err != nil {
		t.Fatal(err)
	}

	uninstallPlugin(t, conn, ws, identityInstallName)

	// Still addressable while uninstalled: the DECLARED row survives, which is
	// what lets an operator read/edit settings between an uninstall and the
	// reconcile that puts the plugin back.
	if _, err := resolvePluginInstallName(ctx, conn, ws, identityInstallName); err != nil {
		t.Fatalf("a declared-but-uninstalled plugin must still resolve: %v", err)
	}

	// REINSTALL: the template layer is re-derived wholesale, exactly as a
	// re-provision does.
	installPlugin(t, conn, ws, identityInstallName)
	if err := writeTemplateConfig(ctx, conn, ws, identityInstallName,
		map[string]any{"poll_seconds": 30, "timezone": "UTC"}); err != nil {
		t.Fatal(err)
	}

	config, overrides, _, err := loadPluginSettings(ctx, conn, ws, identityInstallName)
	if err != nil {
		t.Fatal(err)
	}
	if len(overrides) == 0 {
		t.Fatal("the operator's override was destroyed by uninstall/reinstall")
	}
	eff := effectiveSettings(config, overrides)
	if v := eff["poll_seconds"]; v.(float64) != 5 {
		t.Fatalf("override must still win after reinstall: poll_seconds=%v, want 5", v)
	}
	if v := eff["timezone"]; v.(string) != "UTC" {
		t.Fatalf("un-overridden template key should still be present: %v", v)
	}
}

// NEGATIVE CONTROL for the retention decision: the OTHER choice — deleting the
// settings row on uninstall — destroys the edit. Reinstall then silently
// returns the template value, which is the failure this milestone keeps
// finding. Proving it here is what makes "retain" a decision rather than an
// accident of what the code happens to do.
func TestIntegration_PluginSettings_DeletingOnUninstallWouldLoseTheEdit(t *testing.T) {
	conn := settingsTestDB(t)
	ctx := context.Background()
	ws := seedSettingsWorkspace(t, conn)
	declarePlugin(t, conn, ws, identityInstallName)
	installPlugin(t, conn, ws, identityInstallName)

	if err := writeTemplateConfig(ctx, conn, ws, identityInstallName,
		map[string]any{"poll_seconds": 30}); err != nil {
		t.Fatal(err)
	}
	if _, err := patchOverrides(ctx, conn, ws, identityInstallName,
		map[string]any{"poll_seconds": 5}, nil, "operator@molecule", -1); err != nil {
		t.Fatal(err)
	}

	// The REJECTED semantic: uninstall also drops the settings row.
	uninstallPlugin(t, conn, ws, identityInstallName)
	if _, err := conn.ExecContext(ctx,
		`DELETE FROM workspace_plugin_settings WHERE workspace_id = $1 AND plugin_name = $2`,
		ws, identityInstallName); err != nil {
		t.Fatal(err)
	}

	installPlugin(t, conn, ws, identityInstallName)
	if err := writeTemplateConfig(ctx, conn, ws, identityInstallName,
		map[string]any{"poll_seconds": 30}); err != nil {
		t.Fatal(err)
	}

	config, overrides, _, err := loadPluginSettings(ctx, conn, ws, identityInstallName)
	if err != nil {
		t.Fatal(err)
	}
	if len(overrides) != 0 {
		t.Fatal("control is not exercising the rejected semantic: the override survived a delete-on-uninstall")
	}
	if v := effectiveSettings(config, overrides)["poll_seconds"]; v.(float64) != 30 {
		t.Fatalf("control: expected the template value back, got %v", v)
	}
	t.Log("negative control reproduced: delete-on-uninstall silently reverts the operator's 5 to the template's 30")
}

// A workspace with NO plugins resolves nothing — the resolver must not fall
// back to the requested name when the allow-list is empty, which would restore
// the phantom-write hole for every fresh workspace.
func TestIntegration_PluginSettings_EmptyWorkspaceResolvesNothing(t *testing.T) {
	conn := settingsTestDB(t)
	ctx := context.Background()
	ws := seedSettingsWorkspace(t, conn)

	if _, err := resolvePluginInstallName(ctx, conn, ws, identityInstallName); !errors.Is(err, errPluginNotOnWorkspace) {
		t.Fatalf("empty workspace must refuse every name, got %v", err)
	}
}

// An unsafe name is refused on SHAPE before any query — a path separator in
// :plugin must never reach pluginSettingsRelPath's join.
func TestIntegration_PluginSettings_UnsafeNameRefusedBeforeLookup(t *testing.T) {
	conn := settingsTestDB(t)
	ctx := context.Background()
	ws := seedSettingsWorkspace(t, conn)

	for _, bad := range []string{"", ".", "..", "../escape", "dir/name"} {
		if _, err := resolvePluginInstallName(ctx, conn, ws, bad); err == nil {
			t.Fatalf("unsafe name %q must be refused", bad)
		} else if errors.Is(err, errPluginNotOnWorkspace) {
			t.Fatalf("unsafe name %q must fail on shape, not on lookup", bad)
		}
	}
}
