//go:build integration
// +build integration

// light_runs_integration_test.go — the run ledger's upsert rules (contract C3)
// on real Postgres. The unit tests pin the statements' text; these pin what
// the statements do to the row: a finished run never goes back to running,
// the run's own report beats a row core recorded from its Job (and a late
// report cannot reopen one), the first finished report wins, and text with a
// NUL is stored rather than refused.
//
// Run as light_schema_integration_test.go says. NOT SAFE FOR t.Parallel().

package handlers

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"testing"
	"time"
)

// seedLightWorkspaceWithID inserts a hermes workspace with a fixed id (the
// handler tests use lightTestWS), replacing any row a previous test left.
func seedLightWorkspaceWithID(t *testing.T, conn *sql.DB, id, name, status, lightState string) {
	t.Helper()
	var ls interface{}
	if lightState != "" {
		ls = lightState
	}
	if _, err := conn.Exec(`DELETE FROM workspaces WHERE id = $1`, id); err != nil {
		t.Fatalf("pre-clean %s: %v", id, err)
	}
	if _, err := conn.Exec(`
		INSERT INTO workspaces (id, name, status, runtime, light_state, light_state_at)
		VALUES ($1, $2, $3, 'hermes', $4, now())`, id, name, status, ls); err != nil {
		t.Fatalf("seed %s: %v", name, err)
	}
}

type lightRunOnDisk struct {
	status, source, output, errText string
	exit                            sql.NullInt64
	finished                        sql.NullTime
}

func readLightRunRow(t *testing.T, conn *sql.DB, ws, runID string) lightRunOnDisk {
	t.Helper()
	var r lightRunOnDisk
	if err := conn.QueryRow(`
		SELECT status, source, output, error, exit_code, finished_at FROM light_runs
		WHERE workspace_id = $1 AND run_id = $2`, ws, runID,
	).Scan(&r.status, &r.source, &r.output, &r.errText, &r.exit, &r.finished); err != nil {
		t.Fatalf("read run %s: %v", runID, err)
	}
	return r
}

func lightRunFixture() (start, fin time.Time) {
	start = time.Date(2026, 11, 13, 9, 0, 7, 0, time.UTC)
	return start, start.Add(3 * time.Minute)
}

func TestIntegration_LightRuns_FinishedNeverGoesBackToRunning(t *testing.T) {
	conn := integrationDB_Light(t)
	ctx := context.Background()
	seedLightWorkspaceWithID(t, conn, lightTestWS, "test-light-runs-finished", "hibernated", "parked")
	start, fin := lightRunFixture()
	one := 1

	started := lightRunRow{RunID: "r1", Schedule: "daily", Status: "running", Source: "report", StartedAt: &start}
	if ok, err := upsertLightRunReport(ctx, lightTestWS, "started", started); !ok || err != nil {
		t.Fatalf("started: ok=%v err=%v", ok, err)
	}
	if ok, err := upsertLightRunReport(ctx, lightTestWS, "started", started); !ok || err != nil {
		t.Fatalf("a retried started on a running row must be accepted: ok=%v err=%v", ok, err)
	}
	if r := readLightRunRow(t, conn, lightTestWS, "r1"); r.status != "running" || r.source != "report" {
		t.Fatalf("after started: %+v", r)
	}
	failed := lightRunRow{RunID: "r1", Schedule: "daily", Status: "failed", Source: "report", StartedAt: &start, FinishedAt: &fin, ExitCode: &one, Output: "partial", Error: "boom"}
	if ok, err := upsertLightRunReport(ctx, lightTestWS, "finished", failed); !ok || err != nil {
		t.Fatalf("finished: ok=%v err=%v", ok, err)
	}
	if r := readLightRunRow(t, conn, lightTestWS, "r1"); r.status != "failed" || r.exit.Int64 != 1 || r.output != "partial" || r.errText != "boom" || !r.finished.Time.Equal(fin) {
		t.Fatalf("a finished report must set the run's outcome, got %+v", r)
	}
	if ok, err := upsertLightRunReport(ctx, lightTestWS, "started", started); ok || err != nil {
		t.Fatalf("started after finished: ok=%v err=%v, want not accepted", ok, err)
	}
	if r := readLightRunRow(t, conn, lightTestWS, "r1"); r.status != "failed" {
		t.Fatalf("a finished run went back to %q", r.status)
	}

	// The handler end to end: 409 LIGHT_RUN_FINISHED for a started after finished.
	setupTestRedis(t)
	lh := NewLightHandler(NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir()))
	if w := lightCall(lh.ReportRun, http.MethodPost, "/", startedBody, asWorkspaceToken); w.Code != http.StatusOK {
		t.Fatalf("ReportRun started = %d %s", w.Code, w.Body.String())
	}
	if w := lightCall(lh.ReportRun, http.MethodPost, "/", finishedBody("succeeded", "ok", "", 0), asWorkspaceToken); w.Code != http.StatusOK {
		t.Fatalf("ReportRun finished = %d %s", w.Code, w.Body.String())
	}
	w := lightCall(lh.ReportRun, http.MethodPost, "/", startedBody, asWorkspaceToken)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "LIGHT_RUN_FINISHED") {
		t.Fatalf("ReportRun started after finished = %d %s, want 409 LIGHT_RUN_FINISHED", w.Code, w.Body.String())
	}
	if r := readLightRunRow(t, conn, lightTestWS, "light-be7540cfd160-1a2b3c4d-29384756"); r.status != "succeeded" || r.output != "ok" {
		t.Fatalf("after the handler round trip: %+v", r)
	}
}

func TestIntegration_LightRuns_ReportBeatsJobButNotTheReverse(t *testing.T) {
	conn := integrationDB_Light(t)
	ctx := context.Background()
	seedLightWorkspaceWithID(t, conn, lightTestWS, "test-light-runs-job", "hibernated", "parked")
	start, fin := lightRunFixture()
	zero := 0

	// A row core recorded from the run's Job (it looked failed) is overwritten
	// by the run's own finished report: the report is the authority.
	if _, err := conn.Exec(`INSERT INTO light_runs (workspace_id, run_id, schedule, status, source, error) VALUES ($1, 'r2', 'daily', 'failed', 'job', 'OOMKilled (exit 137)')`, lightTestWS); err != nil {
		t.Fatal(err)
	}
	done := lightRunRow{RunID: "r2", Schedule: "daily", Status: "succeeded", Source: "report", StartedAt: &start, FinishedAt: &fin, ExitCode: &zero, Output: "done"}
	if ok, err := upsertLightRunReport(ctx, lightTestWS, "finished", done); !ok || err != nil {
		t.Fatalf("finished over a job row: ok=%v err=%v", ok, err)
	}
	if r := readLightRunRow(t, conn, lightTestWS, "r2"); r.status != "succeeded" || r.source != "report" || r.errText != "" || r.output != "done" {
		t.Fatalf("the run's report must beat a job-derived row, got %+v", r)
	}

	// The reverse: a run core recorded as ended (a pause cancelled it, or its
	// Job finished) is not reopened by a late started report.
	for _, status := range []string{"cancelled", "failed"} {
		runID := "r3-" + status
		if _, err := conn.Exec(`INSERT INTO light_runs (workspace_id, run_id, schedule, status, source) VALUES ($1, $2, 'daily', $3, 'job')`, lightTestWS, runID, status); err != nil {
			t.Fatal(err)
		}
		late := lightRunRow{RunID: runID, Schedule: "daily", Status: "running", Source: "report", StartedAt: &start}
		if ok, err := upsertLightRunReport(ctx, lightTestWS, "started", late); ok || err != nil {
			t.Fatalf("started on a %s job row: ok=%v err=%v, want not accepted", status, ok, err)
		}
		if r := readLightRunRow(t, conn, lightTestWS, runID); r.status != status || r.source != "job" {
			t.Fatalf("a late started reopened a %s run: %+v", status, r)
		}
	}
}

func TestIntegration_LightRuns_FirstFinishedReportWins(t *testing.T) {
	conn := integrationDB_Light(t)
	ctx := context.Background()
	seedLightWorkspaceWithID(t, conn, lightTestWS, "test-light-runs-first", "hibernated", "parked")
	start, fin := lightRunFixture()
	one, zero := 1, 0

	first := lightRunRow{RunID: "r1", Schedule: "daily", Status: "failed", Source: "report", StartedAt: &start, FinishedAt: &fin, ExitCode: &one, Output: "partial", Error: "boom"}
	if ok, err := upsertLightRunReport(ctx, lightTestWS, "finished", first); !ok || err != nil {
		t.Fatalf("first finished: ok=%v err=%v", ok, err)
	}
	second := lightRunRow{RunID: "r1", Schedule: "daily", Status: "succeeded", Source: "report", StartedAt: &start, FinishedAt: &fin, ExitCode: &zero, Output: "other"}
	if ok, err := upsertLightRunReport(ctx, lightTestWS, "finished", second); !ok || err != nil {
		t.Fatalf("a repeated finished must be a no-op success: ok=%v err=%v", ok, err)
	}
	if r := readLightRunRow(t, conn, lightTestWS, "r1"); r.status != "failed" || r.output != "partial" || r.exit.Int64 != 1 {
		t.Fatalf("the first finished report must win, got %+v", r)
	}

	// A finished report without finished_at takes core's clock.
	timedOut := lightRunRow{RunID: "r4", Schedule: "daily", Status: "timed_out", Source: "report", StartedAt: &start}
	if ok, err := upsertLightRunReport(ctx, lightTestWS, "finished", timedOut); !ok || err != nil {
		t.Fatalf("finished without finished_at: ok=%v err=%v", ok, err)
	}
	if r := readLightRunRow(t, conn, lightTestWS, "r4"); r.status != "timed_out" || !r.finished.Valid {
		t.Fatalf("finished_at must default to now(), got %+v", r)
	}
}

// Postgres text refuses U+0000; a report carrying one must still record the
// run's outcome (review 21716, R2).
func TestIntegration_LightRuns_NulInOutputAndErrorIsStored(t *testing.T) {
	conn := integrationDB_Light(t)
	seedLightWorkspaceWithID(t, conn, lightTestWS, "test-light-runs-nul", "hibernated", "parked")
	setupTestRedis(t)
	lh := NewLightHandler(NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir()))
	if w := lightCall(lh.ReportRun, http.MethodPost, "/", startedBody, asWorkspaceToken); w.Code != http.StatusOK {
		t.Fatalf("started = %d %s", w.Code, w.Body.String())
	}
	// JSON's \u0000 decodes to a NUL byte.
	body := strings.Replace(finishedBody("failed", "OUT", "ERR", 1), `"OUT"`, `"tool said a\u0000b"`, 1)
	body = strings.Replace(body, `"ERR"`, `"bad\u0000err"`, 1)
	w := lightCall(lh.ReportRun, http.MethodPost, "/", body, asWorkspaceToken)
	if w.Code != http.StatusOK {
		t.Fatalf("finished with a NUL = %d %s, want 200", w.Code, w.Body.String())
	}
	r := readLightRunRow(t, conn, lightTestWS, "light-be7540cfd160-1a2b3c4d-29384756")
	if r.status != "failed" || r.output != "tool said a\uFFFDb" || r.errText != "bad\uFFFDerr" {
		t.Fatalf("stored %+v, want the outcome with U+FFFD in place of each NUL", r)
	}
}
