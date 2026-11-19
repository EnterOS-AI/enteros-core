package handlers

// light_monitor_test.go — the Enter OS Light monitor: interrupted transitions,
// failed refresh opens, interrupted opens, auto-park, refresh, deleted
// workspaces, silent runs, failure streaks, retired run tokens, retention, and
// the monitor's own bounds and liveness.

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/supervised"
)

const (
	lightTransitionsSQL       = `SELECT id, light_state, status, EXTRACT\(EPOCH FROM \(now\(\) - COALESCE\(light_state_at, now\(\)\)\)\)::bigint,\s+COALESCE\(light_opened_by, ''\)\s+FROM workspaces\s+WHERE status != 'removed' AND \(`
	lightAutoParkSQL          = `SELECT id, COALESCE\(light_opened_by, 'owner'\)\s+FROM workspaces\s+WHERE light_state = 'open' AND status IN \('online', 'degraded'\)`
	lightInFlightSQL          = `SELECT COUNT\(\*\) FROM workspaces\s+WHERE light_state IN \('opening', 'open'\) AND light_opened_by = 'refresh'\s+AND status NOT IN \('failed', 'paused', 'removed'\)`
	lightRefreshSQL           = `SELECT w.id FROM workspaces w\s+JOIN LATERAL`
	lightRemovedSQL           = `SELECT id FROM workspaces WHERE status = 'removed' AND light_state IS NOT NULL`
	lightClearAfterSQL        = `UPDATE workspaces\s+SET light_state = NULL, light_state_at = now\(\), light_opened_by = NULL`
	lightRunsSQL              = `SELECT w.id, COALESCE\(w.light_retired_token_id::text, ''\) FROM workspaces w`
	lightReparkSQL            = `UPDATE workspaces\s+SET light_state = 'parking', light_state_at = now\(\), light_park_trigger = 'refresh', light_park_watermark = 0,\s+status = 'hibernated', url = '', active_tasks = 0, updated_at = now\(\),`
	lightRestampOpenMarkerSQL = `UPDATE workspaces SET light_last_error_at = now\(\)\s+WHERE id = \$1 AND light_state = 'parked' AND light_last_error->>'code' = \$2`
	lightWakeClaimSQL         = `UPDATE workspaces SET status = \$1, url = '', updated_at = now\(\) WHERE id = \$2 AND status = 'hibernated'`
	lightKeepMarkerErrorSQL   = `UPDATE workspaces\s+SET light_last_error = jsonb_build_object\('code', \$2::text, 'message', \$3::text\), light_last_error_at = now\(\)\s+WHERE id = \$1 AND COALESCE\(light_last_error->>'code', ''\) <> \$4`
)

func newLightMonitorForTest(t *testing.T, api *fakeLightAPI) (*LightMonitor, *WorkspaceHandler) {
	t.Helper()
	_, wh := newLightTestHandler(t, api)
	m := NewLightMonitor(wh)
	return m, wh
}

// transitionRow is one row of the transitions query, for an agent the owner
// opened.
func transitionRow(state, status, ageSeconds string) *sqlmock.Rows {
	return transitionRowBy(state, status, ageSeconds, "owner")
}

func transitionRowBy(state, status, ageSeconds, openedBy string) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "light_state", "status", "age", "opened_by"}).AddRow(lightTestWS, state, status, ageSeconds, openedBy)
}

// expectTransitions scripts the transitions query.
func expectTransitions(mock sqlmock.Sqlmock, rows *sqlmock.Rows) {
	mock.ExpectQuery(lightTransitionsSQL).WithArgs(lightSweepLimit, "LIGHT_OPEN_INCOMPLETE", 120).WillReturnRows(rows)
}

// lightEventRecorder records what the handler broadcasts.
type lightEventRecorder struct {
	mu     sync.Mutex
	events []string
}

func (r *lightEventRecorder) add(eventType, workspaceID string, payload interface{}) {
	b, _ := json.Marshal(payload)
	r.mu.Lock()
	r.events = append(r.events, eventType+" "+workspaceID+" "+string(b))
	r.mu.Unlock()
}

func (r *lightEventRecorder) RecordAndBroadcast(_ context.Context, eventType, workspaceID string, payload interface{}) error {
	r.add(eventType, workspaceID, payload)
	return nil
}

func (r *lightEventRecorder) BroadcastOnly(workspaceID, eventType string, payload interface{}) {
	r.add(eventType, workspaceID, payload)
}

func (r *lightEventRecorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

func TestSweepTransitionsFinishesAParkThatReachedHibernate(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	api := &fakeLightAPI{}
	m, _ := newLightMonitorForTest(t, api)
	expectTransitions(mock, transitionRow("parking", "hibernated", "40"))
	expectLightSetState(mock, []string{"parking"}, "parked", 1)
	expectLightClearParkError(mock)

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
	expectTransitions(mock, transitionRow("parking", "hibernated", "400"))

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
	expectTransitions(mock, transitionRow("opening", "online", "20"))
	expectLightSetState(mock, []string{"opening"}, "open", 1)
	mock.ExpectExec(lightClearErrorSQL).WithArgs(lightTestWS).WillReturnResult(sqlmock.NewResult(0, 0))
	if res := m.SweepTransitions(context.Background()); res.Acted != 1 {
		t.Fatalf("opening+online: %+v", res)
	}

	expectTransitions(mock, transitionRow("parking", "failed", "20"))
	mock.ExpectExec(lightRecordErrorSQL).WithArgs(lightTestWS, "LIGHT_PARK_FAILED", sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	expectLightSetState(mock, []string{"parking"}, "open", 1)
	if res := m.SweepTransitions(context.Background()); res.Acted != 1 {
		t.Fatalf("parking+failed: %+v", res)
	}

	// A young park with its pod still up is left to its own goroutine.
	expectTransitions(mock, transitionRow("parking", "online", "30"))
	if res := m.SweepTransitions(context.Background()); res.Acted != 0 || res.Errors != 0 {
		t.Fatalf("young park: %+v", res)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// The open cases an interruption leaves (plan Step 3.13): a wake that never
// ran is dispatched again; an agent that failed to start goes back to open
// with LIGHT_OPEN_FAILED, for the owner to restart.
func TestSweepTransitionsCompletesAnOpen(t *testing.T) {
	t.Run("opening+hibernated: the wake is dispatched again", func(t *testing.T) {
		mock := setupTestDB(t)
		setupTestRedis(t)
		m, wh := newLightMonitorForTest(t, &fakeLightAPI{})
		wh.cpProv = &trackingCPProv{}
		expectTransitions(mock, transitionRow("opening", "hibernated", "20"))
		// WakeWorkspace's atomic claim, which the wake predicate admits for 'opening'.
		mock.ExpectExec(lightWakeClaimSQL).WithArgs("provisioning", lightTestWS).WillReturnResult(sqlmock.NewResult(0, 0))
		if res := m.SweepTransitions(context.Background()); res.Acted != 1 || res.Errors != 0 {
			t.Fatalf("res = %+v", res)
		}
		wh.waitAsyncForTest()
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet (the wake was not dispatched): %v", err)
		}
	})
	t.Run("opening+failed: back to open with LIGHT_OPEN_FAILED", func(t *testing.T) {
		mock := setupTestDB(t)
		setupTestRedis(t)
		api := &fakeLightAPI{}
		m, _ := newLightMonitorForTest(t, api)
		expectTransitions(mock, transitionRow("opening", "failed", "20"))
		mock.ExpectExec(lightRecordErrorSQL).WithArgs(lightTestWS, "LIGHT_OPEN_FAILED", sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
		expectLightSetState(mock, []string{"opening"}, "open", 1)
		if res := m.SweepTransitions(context.Background()); res.Acted != 1 || res.Errors != 0 {
			t.Fatalf("res = %+v", res)
		}
		if len(api.Calls()) != 0 {
			t.Errorf("CP called: %v (an owner-opened agent is not parked again)", api.Calls())
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet: %v", err)
		}
	})
}

// A park interrupted with the pod still up is continued as the original park:
// its trigger (an owner park clears suspensions), its refresh watermark, and
// the run token the interrupted attempt minted but never recorded is revoked
// (plan Step 3.13; review 21718 N1, N4, N5).
func TestSweepTransitionsContinuesAnInterruptedPark(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	api := &fakeLightAPI{putRes: provisioner.LightPutResult{Schedules: []provisioner.LightScheduleState{{Name: "daily", CronJob: "light-x-aaaaaaaa"}}}}
	_, wh, stops := newParkTestHandler(t, api, []volumeEntry{{Name: "daily", Cron: "0 9 * * *", Timezone: "UTC", Prompt: "p", Enabled: true}})
	m := NewLightMonitor(wh)
	expectTransitions(mock, transitionRow("parking", "online", "200"))
	expectLightLoadWith(mock, "online", "hermes", "workspace", "parking", 0, "tok-cur", "tok-old",
		lightLoadExtra{parkTrigger: "owner", parkWatermark: 7, pendingTokenID: "tok-lost"})
	mock.ExpectExec(lightRevokeTokenSQL).WithArgs("tok-lost", lightTestWS).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(lightDropPendingSQL).WithArgs(lightTestWS, "tok-lost").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(lightSecretReadSQL).WithArgs(lightTestWS, "MOLECULE_LIGHT").WillReturnRows(encryptedRow(t, "1"))
	mock.ExpectQuery(lightStoredSchedulesSQL).WithArgs(lightTestWS).
		WillReturnRows(lightStoredRows().AddRow("daily", "0 9 * * *", "UTC", "p", true, "3 runs in a row failed"))
	mock.ExpectQuery(`INSERT INTO workspace_auth_tokens \(workspace_id, token_hash, prefix, kind\)`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("tok-new"))
	expectLightAudit(mock)
	expectLightPendingToken(mock, "tok-new", nil)
	// Stored as the owner's park: the suspension is cleared ($8 true), and the
	// refreshes up to the claim's watermark are marked applied.
	expectParkAfterThePutAs(mock, [][]driver.Value{{"daily", "0 9 * * *", "UTC", "p", true, "light-x-aaaaaaaa", true}}, []string{"daily"}, 7)
	expectHibernateThenResume(mock)

	res := m.SweepTransitions(context.Background())
	if res.Acted != 1 || res.Errors != 0 {
		t.Fatalf("res = %+v", res)
	}
	api.mu.Lock()
	req := api.putReq
	api.mu.Unlock()
	if len(req.Schedules) != 1 || !*req.Schedules[0].Enabled {
		t.Errorf("PUT = %+v, want the owner's park to send the suspended schedule enabled", req.Schedules)
	}
	if calls := api.Calls(); !reflect.DeepEqual(calls, []string{"put " + lightTestWS, "resume " + lightTestWS}) {
		t.Errorf("CP calls = %v", calls)
	}
	if !reflect.DeepEqual(*stops, []string{lightTestWS}) {
		t.Errorf("stops = %v", *stops)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// A park stuck in 'hibernating' (the hibernate path died between its claim
// and marking the agent hibernated) is finished after five minutes: stop the
// pod again, mark it hibernated, resume (plan Step 3.13).
func TestSweepTransitionsRecoversAStuckHibernating(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	api := &fakeLightAPI{}
	m, wh := newLightMonitorForTest(t, api)
	cp := &trackingCPProv{}
	wh.cpProv = cp

	expectTransitions(mock, transitionRow("parking", "hibernating", "100"))
	if res := m.SweepTransitions(context.Background()); res.Acted != 0 || len(trackedCPCalls(cp)) != 0 {
		t.Fatalf("a young hibernate is left alone: res=%+v cp=%v", res, trackedCPCalls(cp))
	}

	expectTransitions(mock, transitionRow("parking", "hibernating", "400"))
	mock.ExpectExec(`UPDATE workspaces SET status = \$2, url = '', active_tasks = 0, updated_at = now\(\)\s+WHERE id = \$1 AND status = 'hibernating'`).
		WithArgs(lightTestWS, "hibernated").WillReturnResult(sqlmock.NewResult(0, 1))
	expectLightSetState(mock, []string{"parking"}, "parked", 1)
	expectLightClearParkError(mock)
	res := m.SweepTransitions(context.Background())
	if res.Acted != 1 || res.Errors != 0 {
		t.Fatalf("res = %+v", res)
	}
	if !reflect.DeepEqual(trackedCPCalls(cp), []string{"stop " + lightTestWS}) || !reflect.DeepEqual(api.Calls(), []string{"resume " + lightTestWS}) {
		t.Errorf("cp=%v calls=%v, want the stop re-issued, then the resume", trackedCPCalls(cp), api.Calls())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// A refresh open whose agent failed, or did not come up within fifteen
// minutes, is parked again on its previous snapshot so it stops holding a
// refresh slot (review 21718 R4); a refresh open still in time is left alone.
func TestSweepTransitionsParksAFailedRefreshOpenAgain(t *testing.T) {
	for _, tc := range []struct {
		name, state, status, age string
		repark                   bool
	}{
		{"open, pod failed", "open", "failed", "100", true},
		{"opening, pod failed", "opening", "failed", "100", true},
		{"opening, not up after 15 minutes", "opening", "provisioning", "1000", true},
		{"opening, still in time", "opening", "provisioning", "100", false},
		{"open, down for 15 minutes", "open", "offline", "1000", true},
		{"open, down but in time", "open", "offline", "100", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := setupTestDB(t)
			setupTestRedis(t)
			api := &fakeLightAPI{}
			m, wh := newLightMonitorForTest(t, api)
			cp := &trackingCPProv{}
			wh.cpProv = cp
			expectTransitions(mock, transitionRowBy(tc.state, tc.status, tc.age, "refresh"))
			if tc.repark {
				mock.ExpectExec(lightReparkSQL).WithArgs(lightTestWS, "LIGHT_REFRESH_OPEN_FAILED", sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
				expectLightSetState(mock, []string{"parking"}, "parked", 1)
				expectLightClearParkError(mock)
			}
			rec := recordSQL(t)
			res := m.SweepTransitions(context.Background())
			if !tc.repark {
				assertNoWrites(t, rec())
				if res.Acted != 0 || len(api.Calls()) != 0 || len(trackedCPCalls(cp)) != 0 {
					t.Fatalf("res=%+v calls=%v cp=%v, want nothing done", res, api.Calls(), trackedCPCalls(cp))
				}
				return
			}
			if res.Acted != 1 || res.Errors != 0 {
				t.Fatalf("res = %+v", res)
			}
			if !reflect.DeepEqual(trackedCPCalls(cp), []string{"stop " + lightTestWS}) || !reflect.DeepEqual(api.Calls(), []string{"resume " + lightTestWS}) {
				t.Errorf("cp=%v calls=%v, want the pod stopped and the CronJobs resumed", trackedCPCalls(cp), api.Calls())
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet: %v", err)
			}
		})
	}
}

// An open interrupted between its pause and its claim leaves the agent parked
// with its CronJobs paused and the open marker set; the monitor resumes them
// and clears the marker, or stamps the marker again when the resume fails
// (review 21718 O3).
func TestSweepTransitionsResumesAnInterruptedOpen(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	api := &fakeLightAPI{}
	m, _ := newLightMonitorForTest(t, api)
	expectTransitions(mock, transitionRow("parked", "hibernated", "4000"))
	mock.ExpectExec(lightClearOpenMarkerSQL).WithArgs(lightTestWS, "LIGHT_OPEN_INCOMPLETE").WillReturnResult(sqlmock.NewResult(0, 1))
	if res := m.SweepTransitions(context.Background()); res.Acted != 1 || res.Errors != 0 {
		t.Fatalf("res = %+v", res)
	}
	if !reflect.DeepEqual(api.Calls(), []string{"resume " + lightTestWS}) {
		t.Errorf("CP calls = %v", api.Calls())
	}

	api.resume = []error{&provisioner.LightCPError{HTTPStatus: 503, Code: "LIGHT_BACKEND_UNAVAILABLE"}}
	expectTransitions(mock, transitionRow("parked", "hibernated", "4000"))
	mock.ExpectExec(lightRestampOpenMarkerSQL).WithArgs(lightTestWS, "LIGHT_OPEN_INCOMPLETE").WillReturnResult(sqlmock.NewResult(0, 1))
	rec := recordSQL(t)
	if res := m.SweepTransitions(context.Background()); res.Errors != 1 {
		t.Fatalf("failed resume: %+v", res)
	}
	for _, q := range sqlWrites(rec()) {
		if !strings.HasPrefix(q, "UPDATE workspaces SET light_last_error_at = now()") {
			t.Errorf("a failed resume may only stamp the marker again; it wrote: %s", q)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// Each sweep acts on at most maxSteps agents and starts no step once its
// budget is spent; the rest wait for the next tick (review 21718 O5).
func TestSweepTransitionsBoundsItsControlPlaneWork(t *testing.T) {
	ids := []string{"00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002", "00000000-0000-4000-8000-000000000003"}
	rows := func() *sqlmock.Rows {
		r := sqlmock.NewRows([]string{"id", "light_state", "status", "age", "opened_by"})
		for _, id := range ids {
			r.AddRow(id, "parking", "hibernated", "40", "owner")
		}
		return r
	}
	mock := setupTestDB(t)
	setupTestRedis(t)
	api := &fakeLightAPI{}
	m, _ := newLightMonitorForTest(t, api)
	m.maxSteps = 1
	expectTransitions(mock, rows())
	mock.ExpectExec(lightSetStateSQL).WithArgs(ids[0], pqArrayArg{"parking"}, "parked").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(lightClearParkErrorSQL).WithArgs(ids[0], sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 0))
	res := m.SweepTransitions(context.Background())
	if res.Acted != 1 || res.Deferred != 2 || !reflect.DeepEqual(api.Calls(), []string{"resume " + ids[0]}) {
		t.Fatalf("res=%+v calls=%v, want one agent acted on and two deferred", res, api.Calls())
	}

	m.maxSteps, m.budget = 5, 0
	expectTransitions(mock, rows())
	if res := m.SweepTransitions(context.Background()); res.Acted != 0 || res.Deferred != 3 || len(api.Calls()) != 1 {
		t.Fatalf("spent budget: res=%+v calls=%v, want every agent deferred", res, api.Calls())
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
	mock.ExpectQuery(lightAutoParkSQL+`[\s\S]*make_interval\(secs => \$3\) ELSE make_interval\(mins => \$1\)`).WithArgs(30, lightSweepLimit, 60).
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

// Plan Step 3.14: an idle open agent is parked by the monitor (trigger auto,
// which keeps core's suspensions), and its background finish stops the pod
// and resumes the CronJobs.
func TestSweepAutoParkParksAnIdleOpenAgent(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	api := &fakeLightAPI{putRes: provisioner.LightPutResult{Schedules: []provisioner.LightScheduleState{{Name: "daily", CronJob: "light-x-aaaaaaaa"}}}}
	_, wh, stops := newParkTestHandler(t, api, []volumeEntry{{Name: "daily", Cron: "0 9 * * *", Timezone: "UTC", Prompt: "p", Enabled: true}})
	m := NewLightMonitor(wh)
	mock.ExpectQuery(lightAutoParkSQL).WithArgs(30, lightSweepLimit, 60).
		WillReturnRows(sqlmock.NewRows([]string{"id", "opened_by"}).AddRow(lightTestWS, "owner"))
	expectParkUpToThePutAs(t, mock, lightStoredRows().AddRow("daily", "0 9 * * *", "UTC", "p", true, "3 runs in a row failed"), "auto", 0)
	expectParkAfterThePutAs(mock, [][]driver.Value{{"daily", "0 9 * * *", "UTC", "p", true, "light-x-aaaaaaaa", false}}, []string{"daily"}, 0)
	expectHibernateThenResume(mock)

	res := m.SweepAutoPark(context.Background())
	wh.waitAsyncForTest()
	if res.Acted != 1 || res.Errors != 0 {
		t.Fatalf("res = %+v", res)
	}
	api.mu.Lock()
	req := api.putReq
	api.mu.Unlock()
	if len(req.Schedules) != 1 || *req.Schedules[0].Enabled {
		t.Errorf("PUT = %+v, want an auto park to keep the suspended schedule off", req.Schedules)
	}
	if calls := api.Calls(); !reflect.DeepEqual(calls, []string{"put " + lightTestWS, "resume " + lightTestWS}) {
		t.Errorf("CP calls = %v", calls)
	}
	if !reflect.DeepEqual(*stops, []string{lightTestWS}) {
		t.Errorf("stops = %v", *stops)
	}
	if lock := lightOpLock(lightTestWS); !lock.TryLock() {
		t.Error("the Light lock is still held after the finish")
	} else {
		lock.Unlock()
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// An auto park refused before its claim records the refusal, so the error
// back-off keeps the agent out of the next sweeps (review 21718 O2), and
// releases the agent's lock.
func TestSweepAutoParkRecordsARefusalForTheBackoff(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	api := &fakeLightAPI{}
	m, _ := newLightMonitorForTest(t, api)
	mock.ExpectQuery(lightAutoParkSQL).WithArgs(30, lightSweepLimit, 60).
		WillReturnRows(sqlmock.NewRows([]string{"id", "opened_by"}).AddRow(lightTestWS, "owner"))
	expectLightLoad(mock, "online", "claude-code", "workspace", "open", 0, nil, nil)
	mock.ExpectExec(lightRecordErrorSQL).WithArgs(lightTestWS, "LIGHT_RUNTIME_UNSUPPORTED", sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))

	res := m.SweepAutoPark(context.Background())
	if res.Acted != 0 || res.Errors != 1 || len(api.Calls()) != 0 {
		t.Fatalf("res=%+v calls=%v", res, api.Calls())
	}
	if lock := lightOpLock(lightTestWS); !lock.TryLock() {
		t.Error("a refused park left the Light lock held")
	} else {
		lock.Unlock()
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet (the refusal must be recorded): %v", err)
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
	expectLightOpenMarker(mock)
	mock.ExpectExec(lightClaimOpeningSQL).WithArgs(lightTestWS, "refresh", "opening", "parked").WillReturnResult(sqlmock.NewResult(0, 1))

	res := m.SweepRefresh(context.Background())
	wh.waitAsyncForTest()
	if res.Acted != 1 || !reflect.DeepEqual(api.Calls(), []string{"pause " + lightTestWS}) {
		t.Fatalf("res=%+v calls=%v", res, api.Calls())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// A refresh open whose pause failed and whose undoing resume failed too keeps
// the open marker (the transitions sweep needs it to resume the CronJobs): the
// refusal is recorded only over another error.
func TestSweepRefreshKeepsTheOpenMarkerOfAnOpenItCouldNotUndo(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	unavailable := &provisioner.LightCPError{HTTPStatus: 503, Code: "LIGHT_BACKEND_UNAVAILABLE"}
	api := &fakeLightAPI{pauseErr: unavailable, resume: []error{unavailable}}
	m, _ := newLightMonitorForTest(t, api)
	mock.ExpectQuery(lightInFlightSQL).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(lightRefreshSQL).WithArgs(pqArrayArg{"refresh"}, 2).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(lightTestWS))
	expectLightLoad(mock, "hibernated", "hermes", "workspace", "parked", 0, nil, nil)
	expectLightOpenMarker(mock)
	mock.ExpectQuery(lightReadRowSQL).WithArgs(lightTestWS).WillReturnRows(sqlmock.NewRows([]string{"light_state", "status"}).AddRow("parked", "hibernated"))
	mock.ExpectExec(lightRestampOpenMarkerSQL).WithArgs(lightTestWS, "LIGHT_OPEN_INCOMPLETE").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(lightKeepMarkerErrorSQL).WithArgs(lightTestWS, "LIGHT_BACKEND_UNAVAILABLE", sqlmock.AnyArg(), "LIGHT_OPEN_INCOMPLETE").
		WillReturnResult(sqlmock.NewResult(0, 0))

	if res := m.SweepRefresh(context.Background()); res.Errors != 1 || res.Acted != 0 {
		t.Fatalf("res = %+v", res)
	}
	if calls := api.Calls(); !reflect.DeepEqual(calls, []string{"pause " + lightTestWS, "resume " + lightTestWS}) {
		t.Errorf("CP calls = %v", calls)
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

	// A failed delete writes nothing: the marker stays for the next sweep.
	api.deleteErr = &provisioner.LightCPError{HTTPStatus: 503, Code: "LIGHT_BACKEND_UNAVAILABLE"}
	mock.ExpectQuery(lightRemovedSQL).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(lightTestWS))
	rec := recordSQL(t)
	if res := m.SweepRemoved(context.Background()); res.Acted != 0 || res.Errors != 1 {
		t.Fatalf("failed delete: %+v", res)
	}
	assertNoWrites(t, rec())
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
		// The run's own timeout (exit 124) is a timeout, not a failure.
		{RunID: "light-a-b-6", Schedule: "monthly", Phase: "failed", Reason: "Error", ExitCode: ptr32(124)},
	}
	mock.ExpectExec(`INSERT INTO light_runs \(workspace_id, run_id, schedule, status, source, scheduled_for, started_at\)\s+VALUES \(\$1, \$2, \$3, 'running', 'job', \$4, \$5\)`).
		WithArgs(lightTestWS, "light-a-b-2", "daily", sqlmock.AnyArg(), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	finished := `INSERT INTO light_runs \(workspace_id, run_id, schedule, status, source, scheduled_for, started_at, finished_at, exit_code, error\)[\s\S]*WHERE light_runs.status = 'running'`
	mock.ExpectExec(finished).WithArgs(lightTestWS, "light-a-b-3", "daily", "failed", nil, nil, sqlmock.AnyArg(), 137, "OOMKilled (exit 137)").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(finished).WithArgs(lightTestWS, "light-a-b-4", "weekly", "timed_out", nil, nil, sqlmock.AnyArg(), nil, "DeadlineExceeded").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(finished).WithArgs(lightTestWS, "light-a-b-5", "weekly", "succeeded", nil, nil, sqlmock.AnyArg(), 0, "").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(finished).WithArgs(lightTestWS, "light-a-b-6", "monthly", "timed_out", nil, nil, sqlmock.AnyArg(), 124, "Error (exit 124)").WillReturnResult(sqlmock.NewResult(0, 1))

	failed, unfinished := recordLightJobs(context.Background(), lightTestWS, jobs)
	if !reflect.DeepEqual(failed, []string{"daily", "weekly", "monthly"}) || !unfinished {
		t.Fatalf("failed=%v unfinished=%v", failed, unfinished)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet (pending jobs write nothing; finished ones never overwrite a reported outcome): %v", err)
	}
}

// A run whose names this core cannot store is still unfinished: it may hold
// the retired run token (review 21718 N2).
func TestRecordLightJobsCountsAnUnstorableRunAsUnfinished(t *testing.T) {
	setupTestDB(t)
	rec := recordSQL(t)
	for _, j := range []provisioner.LightJob{
		{RunID: "light-a-b-7", Schedule: "bad\tname", Phase: "running"},
		{RunID: "", Schedule: "daily", Phase: "pending"},
	} {
		if _, unfinished := recordLightJobs(context.Background(), lightTestWS, []provisioner.LightJob{j}); !unfinished {
			t.Errorf("%+v: unfinished = false", j)
		}
	}
	assertNoWrites(t, rec())
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
		WithArgs(lightTestWS, containsMatcher{`"daily"`}, containsMatcher{"OOMKilled (exit 137)", "suspended", "open the agent", "park"}).
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

// A refused suspend writes nothing but the failure count: no suspension is
// claimed and nobody is told, so the next evaluation retries.
func TestASuspendFailureWritesNothingAndIsRetried(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	api := &fakeLightAPI{suspendErr: &provisioner.LightCPError{HTTPStatus: 503, Code: "LIGHT_BACKEND_UNAVAILABLE"}}
	_, wh := newLightTestHandler(t, api)
	mock.ExpectQuery(lightStreakScheduleSQL).WillReturnRows(sqlmock.NewRows([]string{"suspended_reason", "suspended_at"}).AddRow(nil, nil))
	mock.ExpectQuery(lightStreakRunsSQL).WillReturnRows(threeFailures())
	mock.ExpectExec(lightStreakCountSQL).WillReturnResult(sqlmock.NewResult(0, 1))
	rec := recordSQL(t)

	if suspended, err := wh.evaluateLightStreak(context.Background(), lightTestWS, "daily"); err == nil || suspended {
		t.Fatalf("evaluate = %v, %v, want the CP error and no suspension", suspended, err)
	}
	writes := sqlWrites(rec())
	if len(writes) != 1 || !strings.Contains(writes[0], "SET consecutive_failures = $3") {
		t.Errorf("writes = %v, want only the failure count", writes)
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

// When the agent may not talk to the user, the suspension still reaches the
// canvas: WORKSPACE_UPDATED names the schedule (plan Step 3.15).
func TestNoticeFallsBackWhenTalkToUserIsDisabled(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	_, wh := newLightTestHandler(t, &fakeLightAPI{})
	events := &lightEventRecorder{}
	wh.broadcaster = events
	mock.ExpectQuery(`SELECT name, talk_to_user_enabled FROM workspaces`).WithArgs(lightTestWS).
		WillReturnRows(sqlmock.NewRows([]string{"name", "talk_to_user_enabled"}).AddRow("Light agent", false))
	rec := recordSQL(t)

	wh.notifyLightScheduleSuspended(context.Background(), lightTestWS, "daily", "boom")
	assertNoWrites(t, rec())
	got := events.all()
	if len(got) != 1 || !strings.HasPrefix(got[0], "WORKSPACE_UPDATED "+lightTestWS+" ") || !strings.Contains(got[0], `"light_schedule_suspended":"daily"`) {
		t.Errorf("events = %v, want one WORKSPACE_UPDATED naming the schedule", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// A streak is evaluated only under the agent's Light lock: while a transition
// holds it nothing is read or written, and the jobs poll evaluates it later
// (review 21718 O1).
func TestStreakEvaluationWaitsWhileATransitionHoldsTheAgent(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	api := &fakeLightAPI{}
	_, wh := newLightTestHandler(t, api)
	rec := recordSQL(t)
	lock := lightOpLock(lightTestWS)
	lock.Lock()
	_, err := wh.evaluateLightStreakLocked(context.Background(), lightTestWS, "daily")
	lock.Unlock()
	if !errors.Is(err, errLightLockHeld) {
		t.Fatalf("err = %v, want errLightLockHeld", err)
	}
	if got := rec(); len(got) != 0 || len(api.Calls()) != 0 {
		t.Errorf("statements=%v calls=%v, want nothing while the lock is held", got, api.Calls())
	}
}

func TestSweepRunsRevokesARetiredTokenOnlyOnceNoRunIsUnfinished(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	api := &fakeLightAPI{jobs: []provisioner.LightJob{{RunID: "light-a-b-9", Schedule: "daily", Phase: "pending"}}}
	m, _ := newLightMonitorForTest(t, api)
	mock.ExpectQuery(lightRunsSQL).WithArgs(lightRunsZeroCursor, lightRunsPerTick).WillReturnRows(sqlmock.NewRows([]string{"id", "retired"}).AddRow(lightTestWS, "tok-old"))
	rec := recordSQL(t)
	if res := m.SweepRuns(context.Background()); res.Acted != 0 {
		t.Fatalf("a pending run may still hold the retired token: %+v", res)
	}
	assertNoWrites(t, rec())
	assertNoTransaction(t, rec())

	api.jobs = []provisioner.LightJob{{RunID: "light-a-b-9", Schedule: "daily", Phase: "running"}}
	mock.ExpectQuery(lightRunsSQL).WithArgs(lightRunsZeroCursor, lightRunsPerTick).WillReturnRows(sqlmock.NewRows([]string{"id", "retired"}).AddRow(lightTestWS, "tok-old"))
	mock.ExpectExec(`INSERT INTO light_runs \(workspace_id, run_id, schedule, status, source, scheduled_for, started_at\)`).WillReturnResult(sqlmock.NewResult(0, 1))
	rec = recordSQL(t)
	if res := m.SweepRuns(context.Background()); res.Acted != 0 {
		t.Fatalf("a running run may still hold the retired token: %+v", res)
	}
	assertNoWrites(t, rec(), "light_runs")
	assertNoTransaction(t, rec())

	api.jobs = []provisioner.LightJob{}
	mock.ExpectQuery(lightRunsSQL).WithArgs(lightRunsZeroCursor, lightRunsPerTick).WillReturnRows(sqlmock.NewRows([]string{"id", "retired"}).AddRow(lightTestWS, "tok-old"))
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

// An agent re-parked with no schedules still holds its previous park's token;
// the sweep must consider it (the candidate query's retired-token arm) and
// revoke the token once its jobs listing shows no unfinished run. Before
// 2026-11-17 the query required a schedule, so such a token stayed valid.
func TestSweepRunsRevokesTheRetiredTokenOfAnAgentWithNoSchedulesLeft(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	api := &fakeLightAPI{jobs: []provisioner.LightJob{}}
	m, _ := newLightMonitorForTest(t, api)
	mock.ExpectQuery(`AND \(EXISTS \(SELECT 1 FROM light_schedules s WHERE s.workspace_id = w.id\)\s+OR w.light_retired_token_id IS NOT NULL\)`).
		WithArgs(lightRunsZeroCursor, lightRunsPerTick).
		WillReturnRows(sqlmock.NewRows([]string{"id", "retired"}).AddRow(lightTestWS, "tok-old"))
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE workspaces SET light_retired_token_id = NULL\s+WHERE id = \$1 AND light_retired_token_id = \$2`).
		WithArgs(lightTestWS, "tok-old").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE workspace_auth_tokens SET revoked_at = now\(\)`).WithArgs(lightTestWS, "tok-old").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	if res := m.SweepRuns(context.Background()); res.Considered != 1 || res.Acted != 1 {
		t.Fatalf("res = %+v, want the agent considered and its retired token revoked", res)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// The jobs poll records what the listing says, but leaves streaks and the
// retired token alone while a transition holds the agent (review 21718 O1).
func TestSweepRunsLeavesStreaksAloneWhileATransitionHoldsTheAgent(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	api := &fakeLightAPI{jobs: []provisioner.LightJob{{RunID: "light-a-b-8", Schedule: "daily", Phase: "failed", Reason: "Error", ExitCode: ptr32(1)}}}
	m, _ := newLightMonitorForTest(t, api)
	mock.ExpectQuery(lightRunsSQL).WithArgs(lightRunsZeroCursor, lightRunsPerTick).WillReturnRows(sqlmock.NewRows([]string{"id", "retired"}).AddRow(lightTestWS, "tok-old"))
	mock.ExpectExec(`INSERT INTO light_runs \(workspace_id, run_id, schedule, status, source, scheduled_for, started_at, finished_at, exit_code, error\)`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	rec := recordSQL(t)
	lock := lightOpLock(lightTestWS)
	lock.Lock()
	res := m.SweepRuns(context.Background())
	lock.Unlock()
	if res.Acted != 0 || res.Errors != 0 {
		t.Fatalf("res = %+v", res)
	}
	assertNoWrites(t, rec(), "light_runs")
	assertNoTransaction(t, rec())
	for _, q := range rec() {
		if strings.Contains(q, "suspended_reason") || strings.Contains(q, "consecutive_failures") {
			t.Errorf("the streak was evaluated under a held lock: %s", q)
		}
	}
	if calls := api.Calls(); !reflect.DeepEqual(calls, []string{"jobs " + lightTestWS}) {
		t.Errorf("CP calls = %v", calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// The jobs poll reads lightRunsPerTick agents a tick and carries on from
// where it stopped until a short page ends the round (review 21718 O5).
func TestSweepRunsReadsAPageATickAndCarriesOn(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	api := &fakeLightAPI{}
	m, _ := newLightMonitorForTest(t, api)
	page := sqlmock.NewRows([]string{"id", "retired"})
	var last string
	for i := 1; i <= lightRunsPerTick; i++ {
		last = fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
		page.AddRow(last, "")
	}
	mock.ExpectQuery(lightRunsSQL+`[\s\S]*ORDER BY w.id\s+LIMIT \$2`).WithArgs(lightRunsZeroCursor, lightRunsPerTick).WillReturnRows(page)
	if res := m.SweepRuns(context.Background()); res.Considered != lightRunsPerTick || m.runsCursor != last {
		t.Fatalf("res=%+v cursor=%q, want a full page read and the cursor at %s", res, m.runsCursor, last)
	}
	next := "00000000-0000-4000-8000-000000000099"
	mock.ExpectQuery(lightRunsSQL).WithArgs(last, lightRunsPerTick).WillReturnRows(sqlmock.NewRows([]string{"id", "retired"}).AddRow(next, ""))
	if res := m.SweepRuns(context.Background()); res.Considered != 1 || m.runsCursor != "" {
		t.Fatalf("res=%+v cursor=%q, want the round ended", res, m.runsCursor)
	}
	if n := len(api.Calls()); n != lightRunsPerTick+1 {
		t.Errorf("jobs listings = %d, want %d", n, lightRunsPerTick+1)
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

// Every tick, even one whose every query fails, ends by recording the
// monitor as alive for /admin/liveness (review 21718 O5).
func TestLightMonitorTickRecordsLiveness(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	m, _ := newLightMonitorForTest(t, &fakeLightAPI{})
	before := time.Now()
	m.Tick(context.Background())
	if last := supervised.LastTick(lightMonitorName); last.Before(before) {
		t.Errorf("LastTick(%q) = %v, want a tick recorded after %v", lightMonitorName, last, before)
	}
}

func TestLightSweepGate(t *testing.T) {
	m := &LightMonitor{budget: time.Minute}
	g := m.gate(2)
	if !g.open() {
		t.Fatal("a new gate is open")
	}
	g.took()
	g.took()
	if g.open() {
		t.Error("the gate stays open past its steps")
	}
	m.budget = 0
	if m.gate(5).open() {
		t.Error("the gate stays open past its time budget")
	}
}

// A queued action of a kind this core knows rides the refresh cycle; one it
// does not know is left queued for a newer core. A failing applier records
// its error on the row and as the agent's Light error, whose back-off spaces
// the retries (review 21718 N7). This is the seam a queued schedule edit
// will use.
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
	mock.ExpectExec(lightRecordErrorSQL).WithArgs(lightTestWS, "LIGHT_ACTION_FAILED", containsMatcher{"test_fails", "grid refused it"}).WillReturnResult(sqlmock.NewResult(0, 1))

	wh.applyLightPendingActions(context.Background(), lightTestWS)
	if !reflect.DeepEqual(got, []string{lightTestWS + ` {"name":"daily"}`}) {
		t.Errorf("applied = %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet (the unknown kind stays queued, untouched): %v", err)
	}
}
