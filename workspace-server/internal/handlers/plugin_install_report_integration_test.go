//go:build integration

package handlers

// The persist/read round-trip, against a REAL Postgres.
//
// A mock cannot prove any of this. "The latest report replaces the previous one"
// is a statement about ON CONFLICT; "a never-reported workspace is
// distinguishable from a clean report" is a statement about zero rows vs a row;
// and "the lists come back as lists, not null" is a statement about jsonb
// round-tripping. All three are the difference between an operator getting an
// answer and getting a plausible-looking non-answer.
//
// Every assertion is paired with a NEGATIVE CONTROL that reproduces the unfixed
// behaviour, so a green here is evidence rather than an accident of shape.

import (
	"context"
	"database/sql"
	"testing"
)

func reportBody(declared, swapped bool, installed, skipped, failed []string, dir string) pluginInstallReportBody {
	return pluginInstallReportBody{
		Declared:   &declared,
		PluginsDir: dir,
		Installed:  installed,
		Skipped:    skipped,
		Failed:     failed,
		Swapped:    &swapped,
	}
}

// seedNamedReportWorkspace seeds one workspace with a CALLER-CHOSEN name.
// seedSettingsWorkspace uses a fixed name, and workspaces_parent_name_uniq (the
// #2872 partial unique index on (COALESCE(parent_id,…), name)) refuses a second
// row with the same one — so any test needing two workspaces at once must name
// them itself.
func seedNamedReportWorkspace(t *testing.T, conn *sql.DB, name string) string {
	t.Helper()
	id := seedWorkspace(t, conn, name)
	t.Cleanup(func() { conn.Exec(`DELETE FROM workspaces WHERE id = $1`, id) })
	return id
}

// A workspace that has NEVER reported must be distinguishable from one that
// reported a clean install. Rendering "never reported" as an empty-but-successful
// report is precisely the class of lie this table exists to stop telling.
func TestIntegration_PluginInstallReport_NeverReportedIsNotAnEmptyReport(t *testing.T) {
	conn := settingsTestDB(t)
	ws := seedSettingsWorkspace(t, conn)

	row, err := loadPluginInstallReport(context.Background(), conn, ws)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if row != nil {
		t.Fatalf("a workspace that never reported must read as nil, got %+v", row)
	}
}

func TestIntegration_PluginInstallReport_RoundTripsAndDerivesLiveness(t *testing.T) {
	conn := settingsTestDB(t)
	ctx := context.Background()
	ws := seedSettingsWorkspace(t, conn)

	body := reportBody(true, true,
		[]string{"gitea://molecule-ai/molecule-ai-plugin-scheduler#v0.2.0"},
		[]string{},
		[]string{},
		"/configs/plugins")
	if err := persistPluginInstallReport(ctx, conn, ws, body); err != nil {
		t.Fatalf("persist: %v", err)
	}

	row, err := loadPluginInstallReport(ctx, conn, ws)
	if err != nil || row == nil {
		t.Fatalf("load: row=%v err=%v", row, err)
	}
	if !row.Declared || !row.Swapped {
		t.Errorf("declared/swapped did not round-trip: %+v", row)
	}
	if len(row.Installed) != 1 || row.Installed[0] != body.Installed[0] {
		t.Errorf("installed did not round-trip: %#v", row.Installed)
	}
	if row.PluginsDir != "/configs/plugins" {
		t.Errorf("plugins_dir did not round-trip: %q", row.PluginsDir)
	}
	if !row.Live {
		t.Error("declared+swapped must derive live=true")
	}
	if row.Degraded {
		t.Error("nothing failed — a clean promotion must not read degraded")
	}
	if row.OutcomeRule == "" {
		t.Error("the outcome rule must be echoed so live=false explains itself")
	}
	if row.ReportedAt.IsZero() {
		t.Error("reported_at must be stamped")
	}
}

// THE production shape: everything staged, nothing promoted. Counting `installed`
// calls this a success; the contract rule calls it what it is.
func TestIntegration_PluginInstallReport_StagedButNotSwappedReadsNotLive(t *testing.T) {
	conn := settingsTestDB(t)
	ctx := context.Background()
	ws := seedSettingsWorkspace(t, conn)

	staged := []string{"a", "b", "c", "d", "e", "f"}
	if err := persistPluginInstallReport(ctx, conn, ws,
		reportBody(true, false, staged, []string{}, []string{}, "/configs/plugins")); err != nil {
		t.Fatalf("persist: %v", err)
	}
	row, err := loadPluginInstallReport(ctx, conn, ws)
	if err != nil || row == nil {
		t.Fatalf("load: row=%v err=%v", row, err)
	}
	if row.Live {
		t.Fatal("swapped=false must read as NOT live no matter how much was staged")
	}
	if len(row.Installed) != len(staged) {
		t.Errorf("the staged list must still be readable — it is the diagnostic: %#v", row.Installed)
	}
	// Negative control: the naive predicate an operator (or a future reader) would
	// reach for reports the opposite.
	naiveSaysLive := len(row.Installed) > 0
	if !naiveSaysLive {
		t.Fatal("control is not exercising the hazard: the naive predicate must say live")
	}
}

// The runtime's PARTIAL-PROMOTION shape, round-tripped: swapped=true with a
// non-empty failed list. molecule_runtime/plugin_sources.py carries the live dirs
// forward, logs "promoting the N that succeeded", swaps, and reports exactly this.
// The old rule folded `failed == []` into liveness and so read a box with 5 of 6
// plugins running as live=false — a false alarm on a healthy workspace, which is
// the same lie this table exists to end, with the sign flipped.
func TestIntegration_PluginInstallReport_PartialPromotionReadsLiveAndDegraded(t *testing.T) {
	conn := settingsTestDB(t)
	ctx := context.Background()
	ws := seedSettingsWorkspace(t, conn)

	installed := []string{"scheduler", "mgmt-mcp", "digest", "memory", "canvas"}
	failed := []string{"gitea://molecule-ai/molecule-ai-plugin-lark#deadbeef"}
	if err := persistPluginInstallReport(ctx, conn, ws,
		reportBody(true, true, installed, []string{}, failed, "/configs/plugins")); err != nil {
		t.Fatalf("persist: %v", err)
	}
	row, err := loadPluginInstallReport(ctx, conn, ws)
	if err != nil || row == nil {
		t.Fatalf("load: row=%v err=%v", row, err)
	}
	if !row.Live {
		t.Error("the tree WAS promoted — 5 of 6 plugins are running, so this box is live")
	}
	if !row.Degraded {
		t.Error("a promoted tree missing a declared source must read degraded, not live-and-clean")
	}
	if len(row.Failed) != 1 {
		t.Errorf("the failed source must stay readable — it is the diagnostic: %#v", row.Failed)
	}

	// Negative control: the retired rule calls this healthy box not-live.
	if retired := row.Declared && row.Swapped && len(row.Failed) == 0; retired {
		t.Fatal("control is not exercising the hazard: the retired `failed == []` rule must disagree")
	}

	// And the fleet query an operator runs must agree with the API: this workspace
	// is NOT one of the broken ones. A partial index that disagrees with `live` is
	// how an operator ends up debugging a box the API already called healthy.
	var inNotLive bool
	if err := conn.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM workspace_plugin_install_reports
		               WHERE workspace_id = $1 AND declared AND NOT swapped)`, ws).Scan(&inNotLive); err != nil {
		t.Fatal(err)
	}
	if inNotLive {
		t.Error("a partially-promoted workspace must not appear in the not-live fleet query")
	}
}

// `declared:false` must survive as false rather than being lost — it is what
// separates "core never asked" from "core asked and nothing landed".
func TestIntegration_PluginInstallReport_NothingDeclaredIsRecordedAsSuch(t *testing.T) {
	conn := settingsTestDB(t)
	ctx := context.Background()
	ws := seedSettingsWorkspace(t, conn)

	if err := persistPluginInstallReport(ctx, conn, ws,
		reportBody(false, false, nil, nil, nil, "")); err != nil {
		t.Fatalf("persist: %v", err)
	}
	row, err := loadPluginInstallReport(ctx, conn, ws)
	if err != nil || row == nil {
		t.Fatalf("load: row=%v err=%v", row, err)
	}
	if row.Declared {
		t.Error("declared=false must round-trip as false")
	}
	if row.Live {
		t.Error("nothing declared cannot be live")
	}
	// nil lists must come back as EMPTY, never nil: a reader must not have to tell
	// "reported no failures" from "reported nothing about failures".
	if row.Installed == nil || row.Skipped == nil || row.Failed == nil {
		t.Errorf("nil lists must normalise to empty: %+v", row)
	}
}

// LATEST-per-workspace, not a log. A wedged workspace reboots repeatedly; the row
// must be replaced, not accumulated.
func TestIntegration_PluginInstallReport_LatestReplacesPrevious(t *testing.T) {
	conn := settingsTestDB(t)
	ctx := context.Background()
	ws := seedSettingsWorkspace(t, conn)

	if err := persistPluginInstallReport(ctx, conn, ws,
		reportBody(true, false, []string{"old"}, []string{}, []string{"old"}, "/one")); err != nil {
		t.Fatalf("persist first: %v", err)
	}
	if err := persistPluginInstallReport(ctx, conn, ws,
		reportBody(true, true, []string{"new"}, []string{}, []string{}, "/two")); err != nil {
		t.Fatalf("persist second: %v", err)
	}

	var n int
	if err := conn.QueryRowContext(ctx,
		`SELECT count(*) FROM workspace_plugin_install_reports WHERE workspace_id = $1`, ws).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("want exactly 1 row (latest-per-workspace), got %d", n)
	}
	row, err := loadPluginInstallReport(ctx, conn, ws)
	if err != nil || row == nil {
		t.Fatalf("load: row=%v err=%v", row, err)
	}
	if !row.Live || row.PluginsDir != "/two" || len(row.Failed) != 0 {
		t.Errorf("the SECOND report must win entirely, got %+v", row)
	}
}

// The fleet question the partial index exists for: which workspaces booted with
// plugins declared and nothing live. This is the query an operator runs first.
func TestIntegration_PluginInstallReport_FleetNotLiveQueryFindsThem(t *testing.T) {
	conn := settingsTestDB(t)
	ctx := context.Background()
	good := seedNamedReportWorkspace(t, conn, "pir-live")
	bad := seedNamedReportWorkspace(t, conn, "pir-not-live")

	if err := persistPluginInstallReport(ctx, conn, good,
		reportBody(true, true, []string{"x"}, []string{}, []string{}, "/p")); err != nil {
		t.Fatal(err)
	}
	if err := persistPluginInstallReport(ctx, conn, bad,
		reportBody(true, false, []string{"x"}, []string{}, []string{}, "/p")); err != nil {
		t.Fatal(err)
	}

	rows, err := conn.QueryContext(ctx, `
		SELECT workspace_id FROM workspace_plugin_install_reports
		 WHERE declared AND NOT swapped`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		found[id] = true
	}
	if !found[bad] {
		t.Error("the not-live workspace must be findable by the fleet query")
	}
	if found[good] {
		t.Error("a live workspace must NOT appear in the not-live query")
	}
}

// A report for a workspace that does not exist must be refused by the FK rather
// than stored as an orphan — an orphan row would show up in the fleet query as a
// phantom broken workspace.
func TestIntegration_PluginInstallReport_UnknownWorkspaceIsRefused(t *testing.T) {
	conn := settingsTestDB(t)
	err := persistPluginInstallReport(context.Background(), conn,
		"00000000-0000-0000-0000-0000000000ff",
		reportBody(true, true, nil, nil, nil, "/p"))
	if err == nil {
		t.Fatal("a report for an unknown workspace must be refused by the foreign key")
	}
}
