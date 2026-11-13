package handlers

// light_monitor_test.go — the Enter OS Light monitor: interrupted transitions,
// auto-park, refresh, deleted workspaces, silent runs, failure streaks,
// retired run tokens and retention.

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
)

const (
	lightTransitionsSQL = `SELECT id, light_state, status, EXTRACT\(EPOCH FROM \(now\(\) - COALESCE\(light_state_at, now\(\)\)\)\)::bigint\s+FROM workspaces\s+WHERE light_state IN \('parking', 'opening'\)`
	lightAutoParkSQL    = `SELECT id, COALESCE\(light_opened_by, 'owner'\)\s+FROM workspaces\s+WHERE light_state = 'open' AND status IN \('online', 'degraded'\)`
	lightInFlightSQL    = `SELECT COUNT\(\*\) FROM workspaces\s+WHERE light_state IN \('opening', 'open'\) AND light_opened_by = 'refresh'`
	lightRefreshSQL     = `SELECT w.id FROM workspaces w\s+JOIN LATERAL`
	lightRemovedSQL     = `SELECT id FROM workspaces WHERE status = 'removed' AND light_state IS NOT NULL`
	lightClearAfterSQL  = `UPDATE workspaces\s+SET light_state = NULL, light_state_at = now\(\), light_opened_by = NULL`
)

func newLightMonitorForTest(t *testing.T, api *fakeLightAPI) (*LightMonitor, *WorkspaceHandler) {
	t.Helper()
	_, wh := newLightTestHandler(t, api)
	m := NewLightMonitor(wh)
	return m, wh
}

func transitionRow(state, status, ageSeconds string) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "light_state", "status", "age"}).AddRow(lightTestWS, state, status, ageSeconds)
}

func TestSweepTransitionsFinishesAParkThatReachedHibernate(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	api := &fakeLightAPI{}
	m, _ := newLightMonitorForTest(t, api)
	mock.ExpectQuery(lightTransitionsSQL).WithArgs(lightSweepLimit).WillReturnRows(transitionRow("parking", "hibernated", "40"))
	expectLightSetState(mock, []string{"parking"}, "parked", 1)
	mock.ExpectExec(lightClearErrorSQL).WillReturnResult(sqlmock.NewResult(0, 0))

	res := m.SweepTransitions(context.Background())
	if res.Acted != 1 || res.Errors != 0 || !reflect.DeepEqual(api.Calls(), []string{"resume " + lightTestWS}) {
		t.Fatalf("res=%+v calls=%v", res, api.Calls())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestSweepTransitionsRestopsAnAgentStuckAwake(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	api := &fakeLightAPI{resume: []error{&provisioner.LightCPError{HTTPStatus: 409, Code: "LIGHT_AGENT_AWAKE"}}}
	m, wh := newLightMonitorForTest(t, api)
	cp := &trackingCPProv{}
	wh.cpProv = cp
	mock.ExpectQuery(lightTransitionsSQL).WillReturnRows(transitionRow("parking", "hibernated", "400"))

	res := m.SweepTransitions(context.Background())
	if res.Acted != 1 || !reflect.DeepEqual(trackedCPCalls(cp), []string{"stop " + lightTestWS}) {
		t.Fatalf("res=%+v cp=%v, want the stop re-issued", res, trackedCPCalls(cp))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet (no state change while the pod still runs): %v", err)
	}
}

func TestSweepTransitionsCompletesAnOpenAndReturnsAFailedParkToOpen(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	m, _ := newLightMonitorForTest(t, &fakeLightAPI{})
	mock.ExpectQuery(lightTransitionsSQL).WillReturnRows(transitionRow("opening", "online", "20"))
	expectLightSetState(mock, []string{"opening"}, "open", 1)
	mock.ExpectExec(lightClearErrorSQL).WillReturnResult(sqlmock.NewResult(0, 0))
	if res := m.SweepTransitions(context.Background()); res.Acted != 1 {
		t.Fatalf("opening+online: %+v", res)
	}

	mock.ExpectQuery(lightTransitionsSQL).WillReturnRows(transitionRow("parking", "failed", "20"))
	mock.ExpectExec(lightRecordErrorSQL).WithArgs(lightTestWS, "LIGHT_PARK_FAILED", sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	expectLightSetState(mock, []string{"parking"}, "open", 1)
	if res := m.SweepTransitions(context.Background()); res.Acted != 1 {
		t.Fatalf("parking+failed: %+v", res)
	}

	// A young park with its pod still up is left to its own goroutine.
	mock.ExpectQuery(lightTransitionsSQL).WillReturnRows(transitionRow("parking", "online", "30"))
	if res := m.SweepTransitions(context.Background()); res.Acted != 0 || res.Errors != 0 {
		t.Fatalf("young park: %+v", res)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestSweepAutoParkUsesTheIdleWindowAndSkipsALockedAgent(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	api := &fakeLightAPI{}
	m, _ := newLightMonitorForTest(t, api)
	mock.ExpectQuery(lightAutoParkSQL+`[\s\S]*make_interval\(mins => \$1\)`).WithArgs(30, lightSweepLimit).
		WillReturnRows(sqlmock.NewRows([]string{"id", "opened_by"}).AddRow(lightTestWS, "owner"))
	lock := lightOpLock(lightTestWS)
	lock.Lock()
	res := m.SweepAutoPark(context.Background())
	lock.Unlock()
	if res.Considered != 1 || res.Acted != 0 || len(api.Calls()) != 0 {
		t.Fatalf("res=%+v calls=%v, want the locked agent skipped", res, api.Calls())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestSweepRefreshRespectsTheInFlightCap(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	m, _ := newLightMonitorForTest(t, &fakeLightAPI{})
	mock.ExpectQuery(lightInFlightSQL).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
	if res := m.SweepRefresh(context.Background()); res.Considered != 0 {
		t.Fatalf("res=%+v, want nothing opened while two refreshes are in flight", res)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestSweepRefreshOpensAParkedAgentWithQueuedWork(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	api := &fakeLightAPI{}
	m, wh := newLightMonitorForTest(t, api)
	mock.ExpectQuery(lightInFlightSQL).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(lightRefreshSQL).WithArgs(pqArrayArg{"refresh"}, 1).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(lightTestWS))
	expectLightLoad(mock, "hibernated", "hermes", "workspace", "parked", 0, nil, nil)
	mock.ExpectExec(lightClaimOpeningSQL).WithArgs(lightTestWS, "refresh").WillReturnResult(sqlmock.NewResult(0, 1))

	res := m.SweepRefresh(context.Background())
	wh.waitAsyncForTest()
	if res.Acted != 1 || !reflect.DeepEqual(api.Calls(), []string{"pause " + lightTestWS}) {
		t.Fatalf("res=%+v calls=%v", res, api.Calls())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestSweepRemovedDeletesTheLightObjectsOfADeletedWorkspace(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	api := &fakeLightAPI{}
	m, _ := newLightMonitorForTest(t, api)
	mock.ExpectQuery(lightRemovedSQL).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(lightTestWS))
	mock.ExpectExec(lightClearAfterSQL).WithArgs(lightTestWS).WillReturnResult(sqlmock.NewResult(0, 1))
	if res := m.SweepRemoved(context.Background()); res.Acted != 1 || !reflect.DeepEqual(api.Calls(), []string{"delete " + lightTestWS}) {
		t.Fatalf("res=%+v calls=%v", res, api.Calls())
	}

	// A failed delete keeps the marker for the next sweep.
	api.deleteErr = &provisioner.LightCPError{HTTPStatus: 503, Code: "LIGHT_BACKEND_UNAVAILABLE"}
	mock.ExpectQuery(lightRemovedSQL).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(lightTestWS))
	if res := m.SweepRemoved(context.Background()); res.Acted != 0 || res.Errors != 1 {
		t.Fatalf("failed delete: %+v", res)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func ptr32(v int32) *int32  { return &v }
func sptr(v string) *string { return &v }

func TestRecordLightJobsRecordsSilentRuns(t *testing.T) {
	mock := setupTestDB(t)
	jobs := []provisioner.LightJob{
		{RunID: "light-a-b-1", Schedule: "daily", Phase: "pending"},
		{RunID: "light-a-b-2", Schedule: "daily", Phase: "running", ScheduledFor: sptr("2026-11-13T09:00:00Z"), StartedAt: sptr("2026-11-13T09:00:05Z")},
		{RunID: "light-a-b-3", Schedule: "daily", Phase: "failed", Reason: "OOMKilled", ExitCode: ptr32(137), FinishedAt: sptr("2026-11-13T08:01:00Z")},
		{RunID: "light-a-b-4", Schedule: "weekly", Phase: "failed", Reason: "DeadlineExceeded"},
		{RunID: "light-a-b-5", Schedule: "weekly", Phase: "succeeded", ExitCode: ptr32(0)},
	}
	mock.ExpectExec(`INSERT INTO light_runs \(workspace_id, run_id, schedule, status, source, scheduled_for, started_at\)\s+VALUES \(\$1, \$2, \$3, 'running', 'job', \$4, \$5\)`).
		WithArgs(lightTestWS, "light-a-b-2", "daily", sqlmock.AnyArg(), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	finished := `INSERT INTO light_runs \(workspace_id, run_id, schedule, status, source, scheduled_for, started_at, finished_at, exit_code, error\)[\s\S]*WHERE light_runs.status = 'running'`
	mock.ExpectExec(finished).WithArgs(lightTestWS, "light-a-b-3", "daily", "failed", nil, nil, sqlmock.AnyArg(), 137, "OOMKilled (exit 137)").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(finished).WithArgs(lightTestWS, "light-a-b-4", "weekly", "timed_out", nil, nil, sqlmock.AnyArg(), nil, "DeadlineExceeded").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(finished).WithArgs(lightTestWS, "light-a-b-5", "weekly", "succeeded", nil, nil, sqlmock.AnyArg(), 0, "").WillReturnResult(sqlmock.NewResult(0, 0))

	failed, unfinished := recordLightJobs(context.Background(), lightTestWS, jobs)
	if !reflect.DeepEqual(failed, []string{"daily", "weekly"}) || !unfinished {
		t.Fatalf("failed=%v unfinished=%v", failed, unfinished)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet (pending jobs write nothing; finished ones never overwrite a reported outcome): %v", err)
	}
}

func TestLightLeadingFailures(t *testing.T) {
	if n := lightLeadingFailures([]string{"failed", "timed_out", "failed", "succeeded"}); n != 3 {
		t.Errorf("= %d, want 3", n)
	}
	if n := lightLeadingFailures([]string{"failed", "succeeded", "failed", "failed"}); n != 1 {
		t.Errorf("= %d, want 1", n)
	}
	if n := lightLeadingFailures(nil); n != 0 {
		t.Errorf("= %d, want 0", n)
	}
}

// containsMatcher matches a string argument containing every part.
type containsMatcher []string

func (m containsMatcher) Match(v driver.Value) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	for _, p := range m {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}

const (
	lightStreakScheduleSQL = `SELECT suspended_reason, suspended_at FROM light_schedules WHERE workspace_id = \$1 AND name = \$2`
	lightStreakRunsSQL     = `SELECT status, error FROM light_runs\s+WHERE workspace_id = \$1 AND schedule = \$2 AND status IN \('succeeded', 'failed', 'timed_out'\)`
	lightStreakCountSQL    = `UPDATE light_schedules SET consecutive_failures = \$3`
	lightStreakClaimSQL    = `UPDATE light_schedules SET suspended_reason = \$3, suspended_at = now\(\), updated_at = now\(\)\s+WHERE workspace_id = \$1 AND name = \$2 AND suspended_reason IS NULL\s+RETURNING name`
)

func threeFailures() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"status", "error"}).
		AddRow("failed", "OOMKilled (exit 137)").AddRow("timed_out", "").AddRow("failed", "boom")
}

func TestThreeFailuresInARowSuspendTheScheduleOnceAndTellTheOwner(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	api := &fakeLightAPI{}
	_, wh := newLightTestHandler(t, api)
	mock.ExpectQuery(lightStreakScheduleSQL).WithArgs(lightTestWS, "daily").
		WillReturnRows(sqlmock.NewRows([]string{"suspended_reason", "suspended_at"}).AddRow(nil, nil))
	mock.ExpectQuery(lightStreakRunsSQL).WithArgs(lightTestWS, "daily", nil).WillReturnRows(threeFailures())
	mock.ExpectExec(lightStreakCountSQL).WithArgs(lightTestWS, "daily", 3).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(lightStreakClaimSQL).WithArgs(lightTestWS, "daily", "3 runs in a row failed").
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("daily"))
	mock.ExpectQuery(`SELECT name, talk_to_user_enabled FROM workspaces`).WithArgs(lightTestWS).
		WillReturnRows(sqlmock.NewRows([]string{"name", "talk_to_user_enabled"}).AddRow("Light agent", true))
	mock.ExpectExec(`INSERT INTO activity_logs \(workspace_id, activity_type, method, summary, response_body, status\)`).
		WithArgs(lightTestWS, containsMatcher{`"daily"`}, containsMatcher{"OOMKilled (exit 137)", "suspended"}).
		WillReturnResult(sqlmock.NewResult(1, 1))

	suspended, err := wh.evaluateLightStreak(context.Background(), lightTestWS, "daily")
	if err != nil || !suspended {
		t.Fatalf("evaluate = %v, %v", suspended, err)
	}
	if calls := api.Calls(); !reflect.DeepEqual(calls, []string{"suspend " + lightTestWS + " daily"}) {
		t.Errorf("CP calls = %v", calls)
	}

	// Already suspended: no CP call, no message.
	mock.ExpectQuery(lightStreakScheduleSQL).WithArgs(lightTestWS, "daily").
		WillReturnRows(sqlmock.NewRows([]string{"suspended_reason", "suspended_at"}).AddRow("3 runs in a row failed", nil))
	if suspended, err := wh.evaluateLightStreak(context.Background(), lightTestWS, "daily"); err != nil || suspended || len(api.Calls()) != 1 {
		t.Fatalf("second evaluation = %v, %v calls=%v", suspended, err, api.Calls())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestASuspendFailureWritesNothingAndIsRetried(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	api := &fakeLightAPI{suspendErr: &provisioner.LightCPError{HTTPStatus: 503, Code: "LIGHT_BACKEND_UNAVAILABLE"}}
	_, wh := newLightTestHandler(t, api)
	mock.ExpectQuery(lightStreakScheduleSQL).WillReturnRows(sqlmock.NewRows([]string{"suspended_reason", "suspended_at"}).AddRow(nil, nil))
	mock.ExpectQuery(lightStreakRunsSQL).WillReturnRows(threeFailures())
	mock.ExpectExec(lightStreakCountSQL).WillReturnResult(sqlmock.NewResult(0, 1))

	if suspended, err := wh.evaluateLightStreak(context.Background(), lightTestWS, "daily"); err == nil || suspended {
		t.Fatalf("evaluate = %v, %v, want the CP error and no suspension", suspended, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet (no claim, no message after a refused suspend): %v", err)
	}
}

func TestStreakCountsOnlyRunsSinceTheLastSuspension(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	_, wh := newLightTestHandler(t, &fakeLightAPI{})
	at := sqlmock.NewRows([]string{"suspended_reason", "suspended_at"}).AddRow(nil, time.Date(2026, 11, 13, 8, 0, 0, 0, time.UTC))
	mock.ExpectQuery(lightStreakScheduleSQL).WillReturnRows(at)
	mock.ExpectQuery(lightStreakRunsSQL+`\s+AND \(\$3::timestamptz IS NULL OR created_at > \$3\)`).
		WithArgs(lightTestWS, "daily", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"status", "error"}).AddRow("failed", "x"))
	mock.ExpectExec(lightStreakCountSQL).WithArgs(lightTestWS, "daily", 1).WillReturnResult(sqlmock.NewResult(0, 1))
	if suspended, err := wh.evaluateLightStreak(context.Background(), lightTestWS, "daily"); err != nil || suspended {
		t.Fatalf("evaluate = %v, %v", suspended, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestSweepRunsRevokesARetiredTokenOnlyOnceNoRunIsUnfinished(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	api := &fakeLightAPI{jobs: []provisioner.LightJob{{RunID: "light-a-b-9", Schedule: "daily", Phase: "pending"}}}
	m, _ := newLightMonitorForTest(t, api)
	runsSQL := `SELECT w.id, COALESCE\(w.light_retired_token_id::text, ''\) FROM workspaces w`
	mock.ExpectQuery(runsSQL).WillReturnRows(sqlmock.NewRows([]string{"id", "retired"}).AddRow(lightTestWS, "tok-old"))
	if res := m.SweepRuns(context.Background()); res.Acted != 0 {
		t.Fatalf("a pending run may still hold the retired token: %+v", res)
	}

	api.jobs = []provisioner.LightJob{}
	mock.ExpectQuery(runsSQL).WillReturnRows(sqlmock.NewRows([]string{"id", "retired"}).AddRow(lightTestWS, "tok-old"))
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE workspaces SET light_retired_token_id = NULL\s+WHERE id = \$1 AND light_retired_token_id = \$2`).
		WithArgs(lightTestWS, "tok-old").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE workspace_auth_tokens SET revoked_at = now\(\)`).WithArgs(lightTestWS, "tok-old").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	if res := m.SweepRuns(context.Background()); res.Acted != 1 {
		t.Fatalf("res = %+v, want the retired token revoked", res)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestPruneLightRunsKeepsThirtyDays(t *testing.T) {
	mock := setupTestDB(t)
	mock.ExpectExec(`DELETE FROM light_runs WHERE status <> 'running' AND created_at < now\(\) - make_interval\(secs => \$1\)`).
		WithArgs(int64(30 * 24 * 3600)).WillReturnResult(sqlmock.NewResult(0, 4))
	if n, err := pruneLightRuns(context.Background(), lightRunRetention); err != nil || n != 4 {
		t.Fatalf("prune = %d, %v", n, err)
	}
}

func TestLightMonitorEnabledOnlyWithAControlPlane(t *testing.T) {
	t.Setenv("LIGHT_MONITOR_DISABLED", "")
	if LightMonitorEnabled(false) || !LightMonitorEnabled(true) {
		t.Error("the monitor runs exactly when a control plane is wired")
	}
	t.Setenv("LIGHT_MONITOR_DISABLED", "true")
	if LightMonitorEnabled(true) {
		t.Error("LIGHT_MONITOR_DISABLED=true must keep it off")
	}
}

// A queued action of a kind this core knows rides the refresh cycle; one it
// does not know is left queued for a newer core. This is the seam a queued
// schedule edit will use.
func TestApplyLightPendingActionsRunsRegisteredAppliersOnly(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	_, wh := newLightTestHandler(t, &fakeLightAPI{})
	var got []string
	lightActionAppliers["test_kind"] = func(_ context.Context, _ *WorkspaceHandler, ws string, payload json.RawMessage) error {
		got = append(got, ws+" "+string(payload))
		return nil
	}
	lightActionAppliers["test_fails"] = func(context.Context, *WorkspaceHandler, string, json.RawMessage) error {
		return errors.New("grid refused it")
	}
	t.Cleanup(func() { delete(lightActionAppliers, "test_kind"); delete(lightActionAppliers, "test_fails") })

	mock.ExpectQuery(`SELECT id, kind, payload FROM light_pending_actions\s+WHERE workspace_id = \$1 AND applied_at IS NULL AND kind <> \$2`).
		WithArgs(lightTestWS, "refresh").
		WillReturnRows(sqlmock.NewRows([]string{"id", "kind", "payload"}).
			AddRow(int64(5), "future_kind", []byte(`{}`)).
			AddRow(int64(6), "test_kind", []byte(`{"name":"daily"}`)).
			AddRow(int64(7), "test_fails", []byte(`{}`)))
	mock.ExpectExec(`UPDATE light_pending_actions SET applied_at = now\(\), error = NULL WHERE id = \$1`).WithArgs(int64(6)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE light_pending_actions SET error = \$2 WHERE id = \$1`).WithArgs(int64(7), "grid refused it").WillReturnResult(sqlmock.NewResult(0, 1))

	wh.applyLightPendingActions(context.Background(), lightTestWS)
	if !reflect.DeepEqual(got, []string{lightTestWS + ` {"name":"daily"}`}) {
		t.Errorf("applied = %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet (the unknown kind stays queued, untouched): %v", err)
	}
}
