package handlers

// light_park_test.go — park, open and run now of an Enter OS Light agent.

import (
	"context"
	"database/sql/driver"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
)

const (
	lightSetStateSQL        = `UPDATE workspaces\s+SET light_state = NULLIF\(\$3, ''\), light_state_at = now\(\), updated_at = now\(\)\s+WHERE id = \$1 AND COALESCE\(light_state, ''\) = ANY\(\$2\)`
	lightWatermarkSQL       = `SELECT COALESCE\(MAX\(id\), 0\) FROM light_pending_actions`
	lightStoredSchedulesSQL = `SELECT name, cron, timezone, prompt, enabled, suspended_reason\s+FROM light_schedules WHERE workspace_id = \$1`
	lightRecordErrorSQL     = `SET light_last_error = jsonb_build_object`
	lightClearErrorSQL      = `UPDATE workspaces SET light_last_error = NULL, light_last_error_at = NULL`
	lightReadRowSQL         = `SELECT COALESCE\(light_state, ''\), status FROM workspaces WHERE id = \$1`
	lightHibernateClaimSQL  = `UPDATE workspaces\s+SET\s+status = \$2, updated_at = now\(\)\s+WHERE\s+id = \$1\s+AND\s+status IN \('online', 'degraded'\)\s+AND\s+active_tasks = 0`
	lightRevokeTokenSQL     = `UPDATE workspace_auth_tokens\s+SET revoked_at = now\(\)\s+WHERE id = \$1 AND workspace_id = \$2 AND revoked_at IS NULL`
)

func expectLightSetState(mock sqlmock.Sqlmock, from []string, to string, n int64) {
	mock.ExpectExec(lightSetStateSQL).WithArgs(lightTestWS, pqArrayArg(from), to).WillReturnResult(sqlmock.NewResult(0, n))
}

// expectLightAudit scripts one audit_events append (the run token mint).
func expectLightAudit(mock sqlmock.Sqlmock) {
	mock.ExpectBegin()
	mock.ExpectExec(`pg_advisory_xact_lock`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT hmac, timestamp`).WillReturnRows(sqlmock.NewRows([]string{"hmac", "timestamp"}))
	mock.ExpectExec(`INSERT INTO audit_events`).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
}

func lightStoredRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"name", "cron", "timezone", "prompt", "enabled", "suspended_reason"})
}

// expectParkUpToThePut scripts a park from open up to the C4 PUT.
func expectParkUpToThePut(t *testing.T, mock sqlmock.Sqlmock, stored *sqlmock.Rows) {
	t.Helper()
	expectLightLoad(mock, "online", "hermes", "workspace", "open", 0, "tok-cur", "tok-old")
	mock.ExpectQuery(lightSecretReadSQL).WithArgs(lightTestWS, "MOLECULE_LIGHT").WillReturnRows(encryptedRow(t, "1"))
	mock.ExpectQuery(lightWatermarkSQL).WithArgs(lightTestWS, "refresh").WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(7))
	expectLightSetState(mock, []string{"open"}, "parking", 1)
	mock.ExpectQuery(lightStoredSchedulesSQL).WithArgs(lightTestWS).WillReturnRows(stored)
	mock.ExpectQuery(`INSERT INTO workspace_auth_tokens \(workspace_id, token_hash, prefix, kind\)`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("tok-new"))
	expectLightAudit(mock)
}

// expectParkAfterThePut scripts the token commit (from open), the stored set
// and the refresh watermark.
func expectParkAfterThePut(mock sqlmock.Sqlmock, schedules [][]driver.Value, names []string) {
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT w.light_run_token_id, w.light_retired_token_id`).WithArgs(lightTestWS).
		WillReturnRows(sqlmock.NewRows([]string{"cur", "ret", "this_park"}).AddRow("tok-cur", "tok-old", false))
	mock.ExpectExec(`UPDATE workspaces SET light_retired_token_id = light_run_token_id, light_run_token_id = \$2 WHERE id = \$1`).
		WithArgs(lightTestWS, "tok-new").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE workspace_auth_tokens SET revoked_at = now\(\)\s+WHERE workspace_id = \$1 AND id = \$2 AND revoked_at IS NULL`).
		WithArgs(lightTestWS, "tok-old").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectBegin()
	for _, s := range schedules {
		mock.ExpectExec(`INSERT INTO light_schedules`).WithArgs(append([]driver.Value{lightTestWS}, s...)...).WillReturnResult(sqlmock.NewResult(0, 1))
	}
	mock.ExpectExec(`DELETE FROM light_schedules WHERE workspace_id = \$1 AND NOT \(name = ANY\(\$2\)\)`).
		WithArgs(lightTestWS, pqArrayArg(names)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()
	mock.ExpectExec(`UPDATE light_pending_actions SET applied_at = now\(\)`).WithArgs(lightTestWS, "refresh", int64(7)).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

// expectHibernateThenResume scripts finishLightPark of an online agent.
func expectHibernateThenResume(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(lightReadRowSQL).WithArgs(lightTestWS).WillReturnRows(sqlmock.NewRows([]string{"light_state", "status"}).AddRow("parking", "online"))
	mock.ExpectExec(lightHibernateClaimSQL).WithArgs(lightTestWS, "hibernating").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT name, tier FROM workspaces WHERE id = \$1`).WillReturnRows(sqlmock.NewRows([]string{"name", "tier"}).AddRow("Light agent", 2))
	mock.ExpectExec(`UPDATE workspaces SET status = \$1, url = '', active_tasks = 0, updated_at = now\(\) WHERE id = \$2`).
		WithArgs("hibernated", lightTestWS).WillReturnResult(sqlmock.NewResult(0, 1))
	expectLightSetState(mock, []string{"parking"}, "parked", 1)
	mock.ExpectExec(lightClearErrorSQL).WithArgs(lightTestWS).WillReturnResult(sqlmock.NewResult(0, 0))
}

func lightGrid() []volumeEntry {
	return []volumeEntry{
		{Name: "weekly", Cron: "0 8 * * 1", Timezone: "UTC", Prompt: "Weekly review.", Enabled: false},
		{Name: "daily", Cron: "0 9 * * *", Timezone: "America/Vancouver", Prompt: "Write the daily report.", Enabled: true},
	}
}

// newParkTestHandler wires a Light handler with a scripted grid and a
// recorded stop.
func newParkTestHandler(t *testing.T, api *fakeLightAPI, grid []volumeEntry) (*LightHandler, *WorkspaceHandler, *[]string) {
	t.Helper()
	lh, wh := newLightTestHandler(t, api)
	wh.lightScheduleReader = func(context.Context, string) ([]volumeEntry, error) { return grid, nil }
	var mu sync.Mutex
	stops := []string{}
	wh.stopFnOverride = func(_ context.Context, id string) {
		mu.Lock()
		stops = append(stops, id)
		mu.Unlock()
	}
	return lh, wh, &stops
}

func TestParkReadsSchedulesMintsATokenPutsAndHibernates(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	api := &fakeLightAPI{putRes: provisioner.LightPutResult{
		Schedules: []provisioner.LightScheduleState{
			{Name: "daily", CronJob: "light-be7540cfd160-aaaaaaaa", Enabled: true, Suspended: true},
			{Name: "weekly", CronJob: "light-be7540cfd160-bbbbbbbb", Enabled: false, Suspended: true},
		},
		Removed: []string{"old"},
	}}
	lh, wh, stops := newParkTestHandler(t, api, lightGrid())

	expectParkUpToThePut(t, mock, lightStoredRows())
	expectParkAfterThePut(mock, [][]driver.Value{
		{"daily", "0 9 * * *", "America/Vancouver", "Write the daily report.", true, "light-be7540cfd160-aaaaaaaa", true},
		{"weekly", "0 8 * * 1", "UTC", "Weekly review.", false, "light-be7540cfd160-bbbbbbbb", true},
	}, []string{"daily", "weekly"})
	expectHibernateThenResume(mock)

	w := lightCall(lh.Park, http.MethodPost, "/", "", nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("park = %d %s", w.Code, w.Body.String())
	}
	body := lightBody(t, w)
	if body["light_state"] != "parking" || !reflect.DeepEqual(body["removed"], []interface{}{"old"}) {
		t.Errorf("body = %v", body)
	}
	scheds := body["schedules"].([]interface{})
	if len(scheds) != 2 || scheds[0].(map[string]interface{})["name"] != "daily" || scheds[1].(map[string]interface{})["enabled"] != false ||
		scheds[0].(map[string]interface{})["cronjob"] != "light-be7540cfd160-aaaaaaaa" {
		t.Errorf("schedules = %v", scheds)
	}

	wh.waitAsyncForTest()
	api.mu.Lock()
	req := api.putReq
	api.mu.Unlock()
	if req.DeadlineSeconds != 3600 || req.RunToken == "" || len(req.Schedules) != 2 ||
		req.Schedules[0].Name != "daily" || !*req.Schedules[0].Enabled || *req.Schedules[1].Enabled {
		t.Errorf("PUT = %+v", req)
	}
	if calls := api.Calls(); !reflect.DeepEqual(calls, []string{"put " + lightTestWS, "resume " + lightTestWS}) {
		t.Errorf("CP calls = %v, want put then resume", calls)
	}
	if !reflect.DeepEqual(*stops, []string{lightTestWS}) {
		t.Errorf("stops = %v, want the agent's pod stopped once", *stops)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestParkRefusesBeforeWritingAnything(t *testing.T) {
	cases := []struct {
		name, status, runtime, state string
		active                       int
		secret                       string // "" = missing
		wantStatus                   int
		wantCode                     string
	}{
		{"busy", "online", "hermes", "open", 2, "1", http.StatusConflict, "LIGHT_AGENT_BUSY"},
		{"not awake", "hibernated", "hermes", "open", 0, "1", http.StatusConflict, "LIGHT_AGENT_NOT_AWAKE"},
		{"secret gone", "online", "hermes", "open", 0, "", http.StatusConflict, "LIGHT_RESTART_REQUIRED"},
		{"other runtime", "online", "claude-code", "open", 0, "1", http.StatusUnprocessableEntity, "LIGHT_RUNTIME_UNSUPPORTED"},
		{"not Light", "online", "hermes", "", 0, "1", http.StatusConflict, "LIGHT_NOT_ENABLED"},
		{"opening", "provisioning", "hermes", "opening", 0, "1", http.StatusConflict, "LIGHT_TRANSITION_IN_PROGRESS"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := setupTestDB(t)
			setupTestRedis(t)
			api := &fakeLightAPI{}
			lh, _, _ := newParkTestHandler(t, api, lightGrid())
			expectLightLoad(mock, tc.status, tc.runtime, "workspace", tc.state, tc.active, nil, nil)
			if tc.state == "open" && tc.runtime == "hermes" && tc.status == "online" && tc.active == 0 {
				q := mock.ExpectQuery(lightSecretReadSQL).WithArgs(lightTestWS, "MOLECULE_LIGHT")
				if tc.secret == "" {
					q.WillReturnRows(sqlmock.NewRows([]string{"encrypted_value", "encryption_version"}))
				} else {
					q.WillReturnRows(encryptedRow(t, tc.secret))
				}
			}
			w := lightCall(lh.Park, http.MethodPost, "/", "", nil)
			if w.Code != tc.wantStatus || lightBody(t, w)["code"] != tc.wantCode {
				t.Fatalf("park = %d %s, want %d %s", w.Code, w.Body.String(), tc.wantStatus, tc.wantCode)
			}
			if len(api.Calls()) != 0 {
				t.Errorf("CP called: %v", api.Calls())
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet (nothing may be claimed or written): %v", err)
			}
		})
	}
}

// expectParkUndone scripts the token revoke (when one was minted) and the
// return to open with the error recorded.
func expectParkUndone(mock sqlmock.Sqlmock, revoked bool) {
	if revoked {
		mock.ExpectExec(lightRevokeTokenSQL).WithArgs("tok-new", lightTestWS).WillReturnResult(sqlmock.NewResult(0, 1))
	}
	expectLightSetState(mock, []string{"parking"}, "open", 1)
	mock.ExpectExec(lightRecordErrorSQL).WillReturnResult(sqlmock.NewResult(0, 1))
}

func TestParkPassesTheCPRefusalByScheduleNameAndStaysOpen(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	invalid := []provisioner.LightFieldError{
		{Schedule: "nightly", Field: "cron", Reason: "6 fields"},
		{Schedule: "tz", Field: "timezone", Reason: "unknown time zone Mars/Base"},
	}
	api := &fakeLightAPI{putErr: &provisioner.LightCPError{HTTPStatus: 422, Code: "LIGHT_INVALID", Detail: "2 schedules are invalid", Invalid: invalid}}
	lh, wh, stops := newParkTestHandler(t, api, lightGrid())
	expectParkUpToThePut(t, mock, lightStoredRows())
	expectParkUndone(mock, true)

	w := lightCall(lh.Park, http.MethodPost, "/", "", nil)
	body := lightBody(t, w)
	if w.Code != http.StatusUnprocessableEntity || body["code"] != "LIGHT_INVALID" {
		t.Fatalf("park = %d %v, want 422 LIGHT_INVALID", w.Code, body)
	}
	got := body["invalid"].([]interface{})
	if len(got) != 2 || got[0].(map[string]interface{})["schedule"] != "nightly" || got[1].(map[string]interface{})["field"] != "timezone" {
		t.Errorf("invalid = %v, want each entry naming its schedule", got)
	}
	wh.waitAsyncForTest()
	if len(*stops) != 0 {
		t.Errorf("a refused park stopped the agent: %v", *stops)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet (back to open, token revoked): %v", err)
	}
}

func TestParkMapsCPConflictsAndStaysOpen(t *testing.T) {
	cases := []struct {
		err        error
		wantStatus int
		wantCode   string
		retryAfter string
	}{
		{&provisioner.LightCPError{HTTPStatus: 409, Code: "LIGHT_ENV_UNSUPPORTED", Key: "OPENAI_API_KEY"}, http.StatusConflict, "LIGHT_ENV_UNSUPPORTED", ""},
		{&provisioner.LightCPError{HTTPStatus: 409, Code: "LIGHT_NOT_ENABLED"}, http.StatusConflict, "LIGHT_RESTART_REQUIRED", ""},
		{&provisioner.LightCPError{HTTPStatus: 409, Code: "LIGHT_RUNTIME_UNSUPPORTED"}, http.StatusUnprocessableEntity, "LIGHT_RUNTIME_UNSUPPORTED", ""},
		{&provisioner.LightCPError{HTTPStatus: 413, Code: "LIGHT_TOO_LARGE"}, http.StatusRequestEntityTooLarge, "LIGHT_TOO_LARGE", ""},
		{&provisioner.LightCPError{HTTPStatus: 501, Code: "LIGHT_UNSUPPORTED"}, http.StatusNotImplemented, "LIGHT_UNSUPPORTED", ""},
		{&provisioner.LightCPError{HTTPStatus: 503, Code: "LIGHT_BACKEND_UNAVAILABLE", RetryAfter: "10"}, http.StatusServiceUnavailable, "LIGHT_BACKEND_UNAVAILABLE", "10"},
	}
	for _, tc := range cases {
		t.Run(tc.wantCode, func(t *testing.T) {
			mock := setupTestDB(t)
			setupTestRedis(t)
			lh, _, _ := newParkTestHandler(t, &fakeLightAPI{putErr: tc.err}, lightGrid())
			expectParkUpToThePut(t, mock, lightStoredRows())
			expectParkUndone(mock, true)
			w := lightCall(lh.Park, http.MethodPost, "/", "", nil)
			if w.Code != tc.wantStatus || lightBody(t, w)["code"] != tc.wantCode || w.Header().Get("Retry-After") != tc.retryAfter {
				t.Fatalf("park = %d %s (Retry-After %q)", w.Code, w.Body.String(), w.Header().Get("Retry-After"))
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet: %v", err)
			}
		})
	}
}

func TestParkSchedulesUnreadableIs502AndStaysOpen(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	api := &fakeLightAPI{}
	lh, wh, _ := newParkTestHandler(t, api, nil)
	wh.lightScheduleReader = func(context.Context, string) ([]volumeEntry, error) { return nil, errors.New("dial tcp: refused") }
	expectLightLoad(mock, "online", "hermes", "workspace", "open", 0, nil, nil)
	mock.ExpectQuery(lightSecretReadSQL).WithArgs(lightTestWS, "MOLECULE_LIGHT").WillReturnRows(encryptedRow(t, "1"))
	mock.ExpectQuery(lightWatermarkSQL).WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(0))
	expectLightSetState(mock, []string{"open"}, "parking", 1)
	expectParkUndone(mock, false)

	w := lightCall(lh.Park, http.MethodPost, "/", "", nil)
	if w.Code != http.StatusBadGateway || lightBody(t, w)["code"] != "LIGHT_SCHEDULES_UNREADABLE" {
		t.Fatalf("park = %d %s", w.Code, w.Body.String())
	}
	if len(api.Calls()) != 0 {
		t.Errorf("CP called: %v", api.Calls())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestRenderLightSchedulesKeepsCoresSuspensions(t *testing.T) {
	reason := "3 runs in a row failed"
	grid := []volumeEntry{
		{Name: "b-edited", Cron: "0 10 * * *", Prompt: "new prompt", Enabled: true},
		{Name: "a-suspended", Cron: "0 9 * * *", Prompt: "p", Enabled: true},
		{Name: "c-off", Cron: "0 9 * * *", Prompt: "p", Enabled: false},
	}
	stored := map[string]lightScheduleRow{
		"a-suspended": {Name: "a-suspended", Cron: "0 9 * * *", Prompt: "p", SuspendedReason: &reason},
		"b-edited":    {Name: "b-edited", Cron: "0 10 * * *", Prompt: "old prompt", SuspendedReason: &reason},
	}
	enabled := func(rs []lightRendered) map[string]bool {
		out := map[string]bool{}
		for _, r := range rs {
			out[r.Schedule.Name] = *r.Schedule.Enabled
		}
		return out
	}
	auto := renderLightSchedules(grid, stored, lightTriggerAuto)
	if auto[0].Schedule.Name != "a-suspended" || !auto[0].KeptSuspended {
		t.Errorf("auto = %+v, want sorted with a-suspended kept suspended", auto)
	}
	if got := enabled(auto); !reflect.DeepEqual(got, map[string]bool{"a-suspended": false, "b-edited": true, "c-off": false}) {
		t.Errorf("auto/refresh park enabled = %v (an unchanged suspended schedule goes out enabled:false; an edited one is cleared)", got)
	}
	if got := enabled(renderLightSchedules(grid, stored, lightTriggerOwner)); !reflect.DeepEqual(got, map[string]bool{"a-suspended": true, "b-edited": true, "c-off": false}) {
		t.Errorf("owner park enabled = %v (an owner park clears suspensions)", got)
	}
}

func TestCommitLightRunTokenRetiresOrRevokes(t *testing.T) {
	t.Run("from open: retire the current, revoke the retired", func(t *testing.T) {
		mock := setupTestDB(t)
		mock.ExpectBegin()
		mock.ExpectQuery(`FOR UPDATE OF w`).WithArgs(lightTestWS).
			WillReturnRows(sqlmock.NewRows([]string{"cur", "ret", "this_park"}).AddRow("tok-cur", "tok-old", false))
		mock.ExpectExec(`SET light_retired_token_id = light_run_token_id, light_run_token_id = \$2`).WithArgs(lightTestWS, "tok-new").WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec(`UPDATE workspace_auth_tokens SET revoked_at`).WithArgs(lightTestWS, "tok-old").WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()
		if err := commitLightRunToken(context.Background(), lightTestWS, "tok-new"); err != nil {
			t.Fatal(err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
	})
	t.Run("continuing: revoke the unused token this park minted", func(t *testing.T) {
		mock := setupTestDB(t)
		mock.ExpectBegin()
		mock.ExpectQuery(`FOR UPDATE OF w`).WithArgs(lightTestWS).
			WillReturnRows(sqlmock.NewRows([]string{"cur", "ret", "this_park"}).AddRow("tok-attempt", "tok-old", true))
		mock.ExpectExec(`UPDATE workspaces SET light_run_token_id = \$2 WHERE id = \$1`).WithArgs(lightTestWS, "tok-new").WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec(`UPDATE workspace_auth_tokens SET revoked_at`).WithArgs(lightTestWS, "tok-attempt").WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()
		if err := commitLightRunToken(context.Background(), lightTestWS, "tok-new"); err != nil {
			t.Fatal(err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
	})
}

func TestParkAfterHibernateGoesStraightToResume(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	api := &fakeLightAPI{}
	lh, wh, _ := newParkTestHandler(t, api, nil)
	wh.lightScheduleReader = func(context.Context, string) ([]volumeEntry, error) {
		t.Error("a park whose pod already stopped must not read the grid")
		return nil, nil
	}
	expectLightLoad(mock, "hibernated", "hermes", "workspace", "parking", 0, nil, nil)
	mock.ExpectQuery(lightReadRowSQL).WithArgs(lightTestWS).WillReturnRows(sqlmock.NewRows([]string{"light_state", "status"}).AddRow("parking", "hibernated"))
	expectLightSetState(mock, []string{"parking"}, "parked", 1)
	mock.ExpectExec(lightClearErrorSQL).WillReturnResult(sqlmock.NewResult(0, 0))

	w := lightCall(lh.Park, http.MethodPost, "/", "", nil)
	if w.Code != http.StatusAccepted || lightBody(t, w)["light_state"] != "parking" {
		t.Fatalf("park = %d %s", w.Code, w.Body.String())
	}
	wh.waitAsyncForTest()
	if calls := api.Calls(); !reflect.DeepEqual(calls, []string{"resume " + lightTestWS}) {
		t.Errorf("CP calls = %v, want the resume only (PUT needs the Deployment)", calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestParkRetriesResumeWhileTheAgentPodStops(t *testing.T) {
	prev := lightResumeRetryEvery
	lightResumeRetryEvery = time.Millisecond
	t.Cleanup(func() { lightResumeRetryEvery = prev })
	mock := setupTestDB(t)
	setupTestRedis(t)
	awake := &provisioner.LightCPError{HTTPStatus: 409, Code: "LIGHT_AGENT_AWAKE"}
	api := &fakeLightAPI{resume: []error{awake, awake, nil}}
	_, wh := newLightTestHandler(t, api)
	expectLightSetState(mock, []string{"parking"}, "parked", 1)
	mock.ExpectExec(lightClearErrorSQL).WillReturnResult(sqlmock.NewResult(0, 0))

	if err := wh.resumeLightAfterPark(context.Background(), lightTestWS, lightTriggerOwner, time.Minute); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if api.resumed != 3 {
		t.Errorf("resume calls = %d, want 3 (two LIGHT_AGENT_AWAKE, then success)", api.resumed)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestParkThatLosesTheHibernateClaimReturnsToOpen(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	api := &fakeLightAPI{}
	_, wh := newLightTestHandler(t, api)
	mock.ExpectQuery(lightReadRowSQL).WithArgs(lightTestWS).WillReturnRows(sqlmock.NewRows([]string{"light_state", "status"}).AddRow("parking", "online"))
	mock.ExpectExec(lightHibernateClaimSQL).WithArgs(lightTestWS, "hibernating").WillReturnResult(sqlmock.NewResult(0, 0))
	expectLightSetState(mock, []string{"parking"}, "open", 1)
	mock.ExpectExec(lightRecordErrorSQL).WithArgs(lightTestWS, "LIGHT_AGENT_BUSY", sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))

	if err := wh.finishLightPark(context.Background(), lightTestWS, lightTriggerOwner, 0); !errors.Is(err, errLightParkAbandoned) {
		t.Fatalf("finish = %v, want errLightParkAbandoned", err)
	}
	if len(api.Calls()) != 0 {
		t.Errorf("CP called: %v (a busy agent's CronJobs stay paused)", api.Calls())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestParkOfAParkedAgentIsUnchanged(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	api := &fakeLightAPI{}
	lh, _, _ := newParkTestHandler(t, api, nil)
	expectLightLoad(mock, "hibernated", "hermes", "workspace", "parked", 0, nil, nil)
	w := lightCall(lh.Park, http.MethodPost, "/", "", nil)
	if w.Code != http.StatusOK || lightBody(t, w)["status"] != "unchanged" || len(api.Calls()) != 0 {
		t.Fatalf("park = %d %s calls=%v", w.Code, w.Body.String(), api.Calls())
	}
}

func TestLightTransitionsWhileAnotherOperationRunsAre409(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	lh, _, _ := newParkTestHandler(t, &fakeLightAPI{}, nil)
	lock := lightOpLock(lightTestWS)
	lock.Lock()
	defer lock.Unlock()
	for name, handle := range map[string]func(*gin.Context){"park": lh.Park, "open": lh.Open} {
		w := lightCall(handle, http.MethodPost, "/", "", nil)
		if w.Code != http.StatusConflict || lightBody(t, w)["code"] != "LIGHT_TRANSITION_IN_PROGRESS" {
			t.Errorf("%s = %d %s, want 409 LIGHT_TRANSITION_IN_PROGRESS", name, w.Code, w.Body.String())
		}
	}
	w := lightCall(lh.RunNow, http.MethodPost, "/", `{"schedule":"daily"}`, nil)
	if w.Code != http.StatusConflict || lightBody(t, w)["code"] != "LIGHT_TRANSITION_IN_PROGRESS" {
		t.Errorf("run = %d %s, want 409", w.Code, w.Body.String())
	}
}

const lightClaimOpeningSQL = `UPDATE workspaces\s+SET light_state = 'opening', light_state_at = now\(\), light_opened_by = \$2, updated_at = now\(\)\s+WHERE id = \$1 AND light_state = 'parked'`

func TestOpenPausesBeforeItWakes(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	api := &fakeLightAPI{pause: provisioner.LightStateResult{Cancelled: []string{"light-be7540cfd160-aaaaaaaa-29384756"}}}
	lh, wh := newLightTestHandler(t, api)
	expectLightLoad(mock, "hibernated", "hermes", "workspace", "parked", 0, nil, nil)
	mock.ExpectExec(`INSERT INTO light_runs \(workspace_id, run_id, schedule, status, source, finished_at\)`).
		WithArgs(lightTestWS, "light-be7540cfd160-aaaaaaaa-29384756", "light-be7540cfd160-aaaaaaaa").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(lightClaimOpeningSQL).WithArgs(lightTestWS, "owner").WillReturnResult(sqlmock.NewResult(0, 1))

	w := lightCall(lh.Open, http.MethodPost, "/", "", nil)
	wh.waitAsyncForTest()
	want := map[string]interface{}{"light_state": "opening", "cancelled": []interface{}{"light-be7540cfd160-aaaaaaaa-29384756"}}
	if got := lightBody(t, w); w.Code != http.StatusAccepted || !reflect.DeepEqual(got, want) {
		t.Fatalf("open = %d %v, want 202 %v", w.Code, got, want)
	}
	if calls := api.Calls(); !reflect.DeepEqual(calls, []string{"pause " + lightTestWS}) {
		t.Errorf("CP calls = %v", calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet (pause, cancelled rows, then the opening claim): %v", err)
	}
}

func TestOpenWhosePauseFailsLeavesTheAgentParked(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	api := &fakeLightAPI{pauseErr: &provisioner.LightCPError{HTTPStatus: 503, Code: "LIGHT_BACKEND_UNAVAILABLE"}}
	lh, _ := newLightTestHandler(t, api)
	expectLightLoad(mock, "hibernated", "hermes", "workspace", "parked", 0, nil, nil)
	w := lightCall(lh.Open, http.MethodPost, "/", "", nil)
	if w.Code != http.StatusServiceUnavailable || lightBody(t, w)["code"] != "LIGHT_BACKEND_UNAVAILABLE" || w.Header().Get("Retry-After") == "" {
		t.Fatalf("open = %d %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet (no claim may follow a failed pause): %v", err)
	}
}

func TestRefreshOpenRecordsWhoOpenedIt(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	_, wh := newLightTestHandler(t, &fakeLightAPI{})
	expectLightLoad(mock, "hibernated", "hermes", "workspace", "parked", 0, nil, nil)
	mock.ExpectExec(lightClaimOpeningSQL).WithArgs(lightTestWS, "refresh").WillReturnResult(sqlmock.NewResult(0, 1))
	cancelled, unchanged, state, ref := wh.openLight(context.Background(), lightTestWS, lightTriggerRefresh)
	if ref != nil || unchanged || state != "opening" || len(cancelled) != 0 {
		t.Fatalf("open = %v %v %q %v", cancelled, unchanged, state, ref)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestOpenOfAnOpenAgentIsUnchangedAndOfAParkingOneIs409(t *testing.T) {
	for state, want := range map[string]int{"open": http.StatusOK, "opening": http.StatusOK, "parking": http.StatusConflict, "": http.StatusConflict} {
		mock := setupTestDB(t)
		setupTestRedis(t)
		api := &fakeLightAPI{}
		lh, _ := newLightTestHandler(t, api)
		expectLightLoad(mock, "online", "hermes", "workspace", state, 0, nil, nil)
		w := lightCall(lh.Open, http.MethodPost, "/", "", nil)
		if w.Code != want || len(api.Calls()) != 0 {
			t.Errorf("open of %q = %d %s calls=%v, want %d", state, w.Code, w.Body.String(), api.Calls(), want)
		}
	}
}

func TestRunNowOnlyWhileParked(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	api := &fakeLightAPI{runID: "light-be7540cfd160-aaaaaaaa-m1763000000"}
	lh, _ := newLightTestHandler(t, api)

	expectLightLoad(mock, "online", "hermes", "workspace", "open", 0, nil, nil)
	w := lightCall(lh.RunNow, http.MethodPost, "/", `{"schedule":"daily"}`, nil)
	if w.Code != http.StatusConflict || lightBody(t, w)["code"] != "LIGHT_NOT_PARKED" || len(api.Calls()) != 0 {
		t.Fatalf("run on an open agent = %d %s calls=%v", w.Code, w.Body.String(), api.Calls())
	}

	expectLightLoad(mock, "hibernated", "hermes", "workspace", "parked", 0, nil, nil)
	w = lightCall(lh.RunNow, http.MethodPost, "/", `{"schedule":"daily"}`, nil)
	if w.Code != http.StatusCreated || lightBody(t, w)["run_id"] != "light-be7540cfd160-aaaaaaaa-m1763000000" {
		t.Fatalf("run = %d %s", w.Code, w.Body.String())
	}
	if calls := api.Calls(); !reflect.DeepEqual(calls, []string{"run " + lightTestWS + " daily"}) {
		t.Errorf("CP calls = %v", calls)
	}
}

func TestRunNowValidatesTheBodyAndMapsCPRefusals(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	lh, _ := newLightTestHandler(t, &fakeLightAPI{})
	for body, want := range map[string]int{`{}`: http.StatusUnprocessableEntity, `{"schedule":"  "}`: http.StatusUnprocessableEntity, `{"x":1}`: http.StatusBadRequest, ``: http.StatusBadRequest} {
		if w := lightCall(lh.RunNow, http.MethodPost, "/", body, nil); w.Code != want {
			t.Errorf("body %q = %d %s, want %d", body, w.Code, w.Body.String(), want)
		}
	}

	for _, tc := range []struct {
		err      error
		status   int
		code     string
		hasRunID bool
	}{
		{&provisioner.LightCPError{HTTPStatus: 409, Code: "LIGHT_RUN_ACTIVE", RunID: "light-x-1"}, http.StatusConflict, "LIGHT_RUN_ACTIVE", true},
		{&provisioner.LightCPError{HTTPStatus: 409, Code: "LIGHT_AGENT_AWAKE"}, http.StatusConflict, "LIGHT_AGENT_AWAKE", false},
		{&provisioner.LightCPError{HTTPStatus: 409, Code: "LIGHT_TENANT_SUSPENDED"}, http.StatusConflict, "LIGHT_TENANT_SUSPENDED", false},
		{&provisioner.LightCPError{HTTPStatus: 404, Code: "LIGHT_SCHEDULE_NOT_FOUND"}, http.StatusNotFound, "LIGHT_SCHEDULE_NOT_FOUND", false},
	} {
		mock := setupTestDB(t)
		lh, _ := newLightTestHandler(t, &fakeLightAPI{runErr: tc.err})
		expectLightLoad(mock, "hibernated", "hermes", "workspace", "parked", 0, nil, nil)
		w := lightCall(lh.RunNow, http.MethodPost, "/", `{"schedule":"daily"}`, nil)
		body := lightBody(t, w)
		if w.Code != tc.status || body["code"] != tc.code || (body["run_id"] != nil) != tc.hasRunID {
			t.Errorf("%s: %d %v", tc.code, w.Code, body)
		}
	}
}

func TestLightCronJobOfRun(t *testing.T) {
	for run, want := range map[string]string{
		"light-be7540cfd160-aaaaaaaa-29384756":  "light-be7540cfd160-aaaaaaaa",
		"light-be7540cfd160-aaaaaaaa-m17630000": "light-be7540cfd160-aaaaaaaa",
		"nodash":                                "nodash",
	} {
		if got := lightCronJobOfRun(run); got != want {
			t.Errorf("lightCronJobOfRun(%q) = %q, want %q", run, got, want)
		}
	}
	if !strings.HasPrefix(lightCronJobOfRun("light-a-b-c"), "light-a-b") {
		t.Error("only the last segment is the Job suffix")
	}
}
