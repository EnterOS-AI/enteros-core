//go:build integration

package handlers

// The fleet read against a REAL Postgres (#4981 §1).
//
// A mock cannot prove any of the three things that matter here:
//
//  1. that the degraded class — declared, swapped, failed non-empty — is REPORTED
//     by the degraded arm and ABSENT from the not-live arm. That is a statement
//     about two SQL predicates over the same rows, and it is the exact class the
//     retired liveness rule got wrong.
//  2. that the SQL filter and the Go rule (reportIsDegraded) agree. They are two
//     expressions of one contract rule and nothing but a real query can compare
//     them.
//  3. that the not-live arm is actually PLANNED against
//     workspace_plugin_install_reports_not_live. An index nothing plans against is
//     the very problem this endpoint exists to fix, so "the endpoint exists" is
//     not the acceptance criterion — "the index is used" is.
//
// Each assertion is paired with a negative control.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// fleetTestRouter mounts List on a real handle. Auth is NOT mounted here: the
// route's admin gate is a router-level concern (middleware.AdminAuth, asserted by
// the middleware's own suite); what needs a database is the query.
func fleetTestRouter(conn *sql.DB) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewAdminPluginInstallReportsHandler(conn)
	r.GET("/admin/plugin-install-reports", h.List)
	return r
}

type fleetResponse struct {
	Reports []struct {
		WorkspaceID   string   `json:"workspace_id"`
		WorkspaceName string   `json:"workspace_name"`
		Declared      bool     `json:"declared"`
		Swapped       bool     `json:"swapped"`
		Failed        []string `json:"failed"`
		Live          bool     `json:"live"`
		Degraded      bool     `json:"degraded"`
	} `json:"reports"`
	Count        int    `json:"count"`
	Status       string `json:"status"`
	Limit        int    `json:"limit"`
	Truncated    bool   `json:"truncated"`
	OutcomeRule  string `json:"outcome_rule"`
	DegradedRule string `json:"degraded_rule"`
}

func fleetGet(t *testing.T, conn *sql.DB, query string) fleetResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/admin/plugin-install-reports"+query, nil)
	w := httptest.NewRecorder()
	fleetTestRouter(conn).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", query, w.Code, w.Body.String())
	}
	var out fleetResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (body %s)", err, w.Body.String())
	}
	return out
}

func fleetIDs(resp fleetResponse) map[string]bool {
	found := map[string]bool{}
	for _, r := range resp.Reports {
		found[r.WorkspaceID] = true
	}
	return found
}

// seedFleetFixture creates one workspace of each class and returns their ids:
// live-clean, not-live (staged, never promoted), and live-but-degraded (the
// runtime's partial promotion).
func seedFleetFixture(t *testing.T, conn *sql.DB, suffix string) (live, notLive, degraded string) {
	t.Helper()
	ctx := context.Background()
	live = seedNamedReportWorkspace(t, conn, "fleet-live-"+suffix)
	notLive = seedNamedReportWorkspace(t, conn, "fleet-not-live-"+suffix)
	degraded = seedNamedReportWorkspace(t, conn, "fleet-degraded-"+suffix)

	if err := persistPluginInstallReport(ctx, conn, live,
		reportBody(true, true, []string{"scheduler"}, []string{}, []string{}, "/configs/plugins")); err != nil {
		t.Fatal(err)
	}
	if err := persistPluginInstallReport(ctx, conn, notLive,
		reportBody(true, false, []string{"scheduler", "mgmt-mcp"}, []string{}, []string{}, "/configs/plugins")); err != nil {
		t.Fatal(err)
	}
	// THE class the old rule got wrong: 5 of 6 promoted, one source failed.
	if err := persistPluginInstallReport(ctx, conn, degraded,
		reportBody(true, true,
			[]string{"scheduler", "mgmt-mcp", "digest", "memory", "canvas"},
			[]string{},
			[]string{"gitea://molecule-ai/molecule-ai-plugin-lark#deadbeef"},
			"/configs/plugins")); err != nil {
		t.Fatal(err)
	}
	return live, notLive, degraded
}

// THE assertion. The not-live arm finds the box that staged and never promoted,
// and does NOT contain the live box or the partially-promoted one.
func TestIntegration_AdminPluginInstallReports_NotLiveArmFindsOnlyNotLive(t *testing.T) {
	conn := settingsTestDB(t)
	live, notLive, degraded := seedFleetFixture(t, conn, "notlivearm")

	resp := fleetGet(t, conn, "?status=not_live&limit=1000")
	found := fleetIDs(resp)

	if !found[notLive] {
		t.Error("the not-live workspace must be reported by the not_live arm")
	}
	if found[live] {
		t.Error("a live workspace must not appear in the not_live arm")
	}
	if found[degraded] {
		t.Error("a PARTIALLY-PROMOTED workspace is live — it must not appear in the not_live arm. " +
			"This is the exact class the retired `failed == []` rule got wrong.")
	}
	for _, r := range resp.Reports {
		if r.Live {
			t.Errorf("%s is in the not_live arm with live=true — the arm and the rule disagree",
				r.WorkspaceID)
		}
	}
	// The row must be usable without a second lookup.
	for _, r := range resp.Reports {
		if r.WorkspaceID == notLive && r.WorkspaceName == "" {
			t.Error("a fleet row must name the workspace, not just its uuid")
		}
	}

	// Negative control: the seeded fixture must actually contain a box the retired
	// rule would have mis-filed here, or this test is not exercising the hazard.
	var retiredWouldFile bool
	if err := conn.QueryRow(`
		SELECT declared AND NOT (declared AND swapped AND failed = '[]'::jsonb)
		  FROM workspace_plugin_install_reports WHERE workspace_id = $1`,
		degraded).Scan(&retiredWouldFile); err != nil {
		t.Fatal(err)
	}
	if !retiredWouldFile {
		t.Fatal("control is not exercising the hazard: the retired rule must file the degraded box as not-live")
	}
}

// The degraded arm reports the class the not-live sweep cannot see.
func TestIntegration_AdminPluginInstallReports_DegradedArmReportsPartialPromotion(t *testing.T) {
	conn := settingsTestDB(t)
	live, notLive, degraded := seedFleetFixture(t, conn, "degradedarm")

	resp := fleetGet(t, conn, "?status=degraded&limit=1000")
	found := fleetIDs(resp)

	if !found[degraded] {
		t.Fatal("a promoted tree with a failed source must be reported by the degraded arm")
	}
	if found[live] {
		t.Error("a clean live workspace is not degraded")
	}
	if found[notLive] {
		t.Error("a box that never promoted is NOT LIVE, not degraded — calling it degraded softens an outage into a caveat")
	}
	for _, r := range resp.Reports {
		if !r.Live || !r.Degraded {
			t.Errorf("%s in the degraded arm must read live=true degraded=true, got live=%v degraded=%v",
				r.WorkspaceID, r.Live, r.Degraded)
		}
		if len(r.Failed) == 0 {
			t.Errorf("%s is degraded with an empty failed list — the diagnostic was lost in transit",
				r.WorkspaceID)
		}
	}

	// Negative control: without a dedicated degraded arm this box is invisible.
	// Prove the not-live arm does not report it — that is WHY this arm exists.
	notLiveResp := fleetGet(t, conn, "?status=not_live&limit=1000")
	if fleetIDs(notLiveResp)[degraded] {
		t.Fatal("control failed: if the not_live arm already reported it, the degraded arm answers nothing new")
	}
}

// The SQL predicate and reportIsDegraded are two expressions of one contract rule.
// Compare them over the whole (declared × swapped × failed-shape) space rather
// than trusting that they were written by the same person on the same day.
func TestIntegration_AdminPluginInstallReports_SQLFilterAgreesWithTheGoRule(t *testing.T) {
	conn := settingsTestDB(t)
	ctx := context.Background()

	type fixture struct {
		id       string
		declared bool
		swapped  bool
		failed   []string
	}
	var fixtures []fixture
	i := 0
	for _, declared := range []bool{false, true} {
		for _, swapped := range []bool{false, true} {
			for _, failed := range [][]string{{}, {"one"}, {"one", "two"}} {
				id := seedNamedReportWorkspace(t, conn, fmt.Sprintf("fleet-matrix-%d", i))
				if err := persistPluginInstallReport(ctx, conn, id,
					reportBody(declared, swapped, []string{"x"}, []string{}, failed, "/p")); err != nil {
					t.Fatal(err)
				}
				fixtures = append(fixtures, fixture{id, declared, swapped, failed})
				i++
			}
		}
	}

	sqlDegraded := fleetIDs(fleetGet(t, conn, "?status=degraded&limit=1000"))
	sqlNotLive := fleetIDs(fleetGet(t, conn, "?status=not_live&limit=1000"))

	for _, f := range fixtures {
		wantDegraded := reportIsDegraded(f.declared, f.swapped, f.failed)
		if sqlDegraded[f.id] != wantDegraded {
			t.Errorf("declared=%v swapped=%v failed=%#v: SQL says degraded=%v, reportIsDegraded says %v",
				f.declared, f.swapped, f.failed, sqlDegraded[f.id], wantDegraded)
		}
		wantNotLive := f.declared && !reportIsLive(f.declared, f.swapped)
		if sqlNotLive[f.id] != wantNotLive {
			t.Errorf("declared=%v swapped=%v: SQL says not_live=%v, the rule says %v",
				f.declared, f.swapped, sqlNotLive[f.id], wantNotLive)
		}
	}
	// Negative control against a vacuous pass: the matrix must actually have
	// populated both arms, or agreeing on nothing is not agreement.
	if len(sqlDegraded) == 0 || len(sqlNotLive) == 0 {
		t.Fatalf("control failed: the matrix produced no rows for one arm (degraded=%d not_live=%d)",
			len(sqlDegraded), len(sqlNotLive))
	}

}

// The response is bounded, and says so when it is.
func TestIntegration_AdminPluginInstallReports_LimitTruncatesAndSaysSo(t *testing.T) {
	conn := settingsTestDB(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		id := seedNamedReportWorkspace(t, conn, fmt.Sprintf("fleet-limit-%d", i))
		if err := persistPluginInstallReport(ctx, conn, id,
			reportBody(true, false, []string{"x"}, []string{}, []string{}, "/p")); err != nil {
			t.Fatal(err)
		}
	}

	resp := fleetGet(t, conn, "?status=not_live&limit=2")
	if resp.Count != 2 {
		t.Errorf("limit=2 must return 2 rows, got %d", resp.Count)
	}
	if !resp.Truncated {
		t.Error("a response that reached the cap must say truncated=true, or the operator reads a capped count as the whole outage")
	}
	// Negative control: an uncapped read of the same rows must NOT be truncated,
	// or `truncated` is a constant rather than a signal.
	full := fleetGet(t, conn, "?status=not_live&limit=1000")
	if full.Truncated {
		t.Fatalf("control failed: an uncapped read must not report truncated (count=%d)", full.Count)
	}
	if full.Count < 3 {
		t.Fatalf("control failed: the uncapped read must see more rows than the capped one (%d)", full.Count)
	}
}

// The echoed rules must reach the wire, so a reader learns why a row is in the
// arm it is in without going to find the contract.
func TestIntegration_AdminPluginInstallReports_EchoesTheContractRules(t *testing.T) {
	conn := settingsTestDB(t)
	resp := fleetGet(t, conn, "?status=not_live&limit=1")
	if resp.OutcomeRule != "live iff declared && swapped" {
		t.Errorf("outcome_rule is not the corrected rule: %q", resp.OutcomeRule)
	}
	if resp.DegradedRule != "degraded iff live && failed != []" {
		t.Errorf("degraded_rule is missing or wrong: %q", resp.DegradedRule)
	}
}

// --- the index this whole issue is about -----------------------------------

// The acceptance criterion. workspace_plugin_install_reports_not_live was created
// for this query and nothing planned against it. EXPLAIN the query the handler
// ACTUALLY runs (fleetReportQuery, not a retyped copy) and require the partial
// index by name.
//
// Seeded to a size where the planner has a real choice: at a handful of rows
// Postgres correctly prefers a sequential scan of one page, and a test that
// passes only because seqscan was disabled proves the index is USABLE, not that
// it is USED. Both are asserted below, in that order.
func TestIntegration_AdminPluginInstallReports_NotLiveArmUsesThePartialIndex(t *testing.T) {
	conn := settingsTestDB(t)
	ctx := context.Background()

	// Bulk-seed enough rows that a sequential scan is the expensive option.
	// Cleaned up by name prefix so the fixture cannot leak into other tests.
	t.Cleanup(func() {
		conn.Exec(`DELETE FROM workspaces WHERE name LIKE 'fleet-bulk-%'`)
	})
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO workspaces (id, name, status)
		SELECT gen_random_uuid(), 'fleet-bulk-' || g, 'online'
		  FROM generate_series(1, 20000) g`); err != nil {
		t.Fatalf("bulk seed workspaces: %v", err)
	}
	// 1 in 200 is not live — the realistic shape, and the shape that makes the
	// partial index worth having: a tiny index over a large table.
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO workspace_plugin_install_reports
			(workspace_id, declared, plugins_dir, installed, skipped, failed, swapped, reported_at)
		SELECT w.id, true, '/configs/plugins', '["x"]'::jsonb, '[]'::jsonb, '[]'::jsonb,
		       (substring(w.name from 12)::int % 200) <> 0,
		       NOW() - (substring(w.name from 12)::int || ' seconds')::interval
		  FROM workspaces w WHERE w.name LIKE 'fleet-bulk-%'`); err != nil {
		t.Fatalf("bulk seed reports: %v", err)
	}
	// ANALYZE BOTH tables. With stale workspaces stats the planner underestimates
	// the join side and picks a hash join that seq-scans every workspace and then
	// sorts — a plan that uses the index and still reads the whole fleet. The
	// realistic plan is a nested loop driven by the partial index, which is also
	// the one that needs no sort because the index is already reported_at DESC.
	if _, err := conn.ExecContext(ctx, `ANALYZE workspace_plugin_install_reports, workspaces`); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	explain := func(t *testing.T) string {
		t.Helper()
		rows, err := conn.QueryContext(ctx,
			"EXPLAIN "+fleetReportQuery(notLiveFleetPredicate), defaultListLimit)
		if err != nil {
			t.Fatalf("explain: %v", err)
		}
		defer rows.Close()
		var b strings.Builder
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			b.WriteString(line)
			b.WriteString("\n")
		}
		return b.String()
	}

	plan := explain(t)
	t.Logf("EXPLAIN (not_live arm):\n%s", plan)
	if !strings.Contains(plan, "workspace_plugin_install_reports_not_live") {
		t.Errorf("the not_live arm is NOT planned against the partial index — "+
			"an index nothing plans against is the problem this endpoint exists to fix.\nplan:\n%s", plan)
	}
	if strings.Contains(plan, "Seq Scan on workspace_plugin_install_reports") {
		t.Errorf("the not_live arm sequentially scans the reports table:\n%s", plan)
	}

	// Negative control #1: the DEGRADED arm has no partial index behind it, so its
	// plan must NOT name this index. If it did, the assertion above would be
	// satisfied by something other than the predicate match — i.e. proving nothing.
	drows, err := conn.QueryContext(ctx,
		"EXPLAIN "+fleetReportQuery(degradedFleetPredicate), defaultListLimit)
	if err != nil {
		t.Fatal(err)
	}
	var dplan strings.Builder
	for drows.Next() {
		var line string
		if err := drows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		dplan.WriteString(line + "\n")
	}
	drows.Close()
	t.Logf("EXPLAIN (degraded arm, control):\n%s", dplan.String())
	if strings.Contains(dplan.String(), "workspace_plugin_install_reports_not_live") {
		t.Fatal("control failed: the not-live index must not serve the degraded arm — " +
			"the plan check above would then be insensitive to the predicate")
	}
}
