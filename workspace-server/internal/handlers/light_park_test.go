package handlers

// light_park_test.go — park, open and run now of an Enter OS Light agent.

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
	lightClaimParkingSQL    = `UPDATE workspaces\s+SET light_state = 'parking', light_state_at = now\(\), updated_at = now\(\),\s+light_park_trigger = \$2,\s+light_park_watermark = \(SELECT COALESCE\(MAX\(a\.id\), 0\) FROM light_pending_actions a\s+WHERE a\.workspace_id = \$1 AND a\.kind = \$3 AND a\.applied_at IS NULL\)\s+WHERE id = \$1 AND light_state = 'open'\s+RETURNING light_park_watermark`
	lightPendingTokenSQL    = `UPDATE workspaces SET light_pending_token_id = \$2 WHERE id = \$1`
	lightDropPendingSQL     = `UPDATE workspaces SET light_pending_token_id = NULL WHERE id = \$1 AND light_pending_token_id = \$2`
	lightBusySQL            = `SELECT COALESCE\(active_tasks, 0\) > 0 OR COALESCE\(is_busy, false\)\s+OR COALESCE\(last_activity_at > light_state_at, false\)\s+FROM workspaces WHERE id = \$1`
	lightCommitSQL          = `SELECT w\.status, w\.light_run_token_id, w\.light_retired_token_id,[\s\S]*FOR UPDATE OF w`
	lightClearParkErrorSQL  = `UPDATE workspaces SET light_last_error = NULL, light_last_error_at = NULL\s+WHERE id = \$1 AND light_last_error IS NOT NULL AND NOT \(COALESCE\(light_last_error->>'code', ''\) = ANY\(\$2\)\)`
	lightClearOpenMarkerSQL = `UPDATE workspaces SET light_last_error = NULL, light_last_error_at = NULL\s+WHERE id = \$1 AND light_state = 'parked' AND light_last_error->>'code' = \$2`
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

// expectLightClaimParking scripts the parking claim, which records the
// trigger and returns the refresh watermark.
func expectLightClaimParking(mock sqlmock.Sqlmock, trig string, watermark int64) {
	mock.ExpectQuery(lightClaimParkingSQL).WithArgs(lightTestWS, trig, "refresh").
		WillReturnRows(sqlmock.NewRows([]string{"light_park_watermark"}).AddRow(watermark))
}

// expectLightClearParkError scripts clearLightParkError (it keeps the
// refresh back-off errors).
func expectLightClearParkError(mock sqlmock.Sqlmock) {
	mock.ExpectExec(lightClearParkErrorSQL).WithArgs(lightTestWS, pqArrayArg{"LIGHT_REFRESH_OPEN_FAILED", "LIGHT_ACTION_FAILED"}).
		WillReturnResult(sqlmock.NewResult(0, 0))
}

// expectLightBusy scripts lightAgentBusy.
func expectLightBusy(mock sqlmock.Sqlmock, busy bool) {
	mock.ExpectQuery(lightBusySQL).WithArgs(lightTestWS).WillReturnRows(sqlmock.NewRows([]string{"busy"}).AddRow(busy))
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

// expectParkUpToThePut scripts an owner's park from open up to the C4 PUT.
func expectParkUpToThePut(t *testing.T, mock sqlmock.Sqlmock, stored *sqlmock.Rows) {
	t.Helper()
	expectParkUpToThePutAs(t, mock, stored, "owner", 7)
}

// expectParkUpToThePutAs scripts a park from open up to the C4 PUT with the
// given trigger, whose claim returns watermark.
func expectParkUpToThePutAs(t *testing.T, mock sqlmock.Sqlmock, stored *sqlmock.Rows, trig string, watermark int64) {
	t.Helper()
	expectLightLoad(mock, "online", "hermes", "workspace", "open", 0, "tok-cur", "tok-old")
	mock.ExpectQuery(lightSecretReadSQL).WithArgs(lightTestWS, "MOLECULE_LIGHT").WillReturnRows(encryptedRow(t, "1"))
	expectLightClaimParking(mock, trig, watermark)
	mock.ExpectQuery(lightStoredSchedulesSQL).WithArgs(lightTestWS).WillReturnRows(stored)
	mock.ExpectQuery(`INSERT INTO workspace_auth_tokens \(workspace_id, token_hash, prefix, kind\)`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("tok-new"))
	expectLightAudit(mock)
	mock.ExpectExec(lightPendingTokenSQL).WithArgs(lightTestWS, "tok-new").WillReturnResult(sqlmock.NewResult(0, 1))
}

// expectParkAfterThePut scripts the token commit (from open), the stored set
// and the refresh watermark (7).
func expectParkAfterThePut(mock sqlmock.Sqlmock, schedules [][]driver.Value, names []string) {
	expectParkAfterThePutAs(mock, schedules, names, 7)
}

// expectParkAfterThePutAs is expectParkAfterThePut for a claim that returned
// watermark (0 marks nothing applied).
func expectParkAfterThePutAs(mock sqlmock.Sqlmock, schedules [][]driver.Value, names []string, watermark int64) {
	mock.ExpectBegin()
	mock.ExpectQuery(lightCommitSQL).WithArgs(lightTestWS).
		WillReturnRows(sqlmock.NewRows([]string{"status", "cur", "ret", "this_park"}).AddRow("online", "tok-cur", "tok-old", false))
	mock.ExpectExec(`UPDATE workspaces SET light_retired_token_id = light_run_token_id, light_run_token_id = \$2, light_pending_token_id = NULL WHERE id = \$1`).
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
	if watermark > 0 {
		mock.ExpectExec(`UPDATE light_pending_actions SET applied_at = now\(\)`).WithArgs(lightTestWS, "refresh", watermark).
			WillReturnResult(sqlmock.NewResult(0, 1))
	}
}

// expectHibernateThenResume scripts finishLightPark of an online agent.
func expectHibernateThenResume(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(lightReadRowSQL).WithArgs(lightTestWS).WillReturnRows(sqlmock.NewRows([]string{"light_state", "status"}).AddRow("parking", "online"))
	expectLightBusy(mock, false)
	mock.ExpectExec(lightHibernateClaimSQL).WithArgs(lightTestWS, "hibernating").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT name, tier FROM workspaces WHERE id = \$1`).WillReturnRows(sqlmock.NewRows([]string{"name", "tier"}).AddRow("Light agent", 2))
	mock.ExpectExec(`UPDATE workspaces SET status = \$1, url = '', active_tasks = 0, updated_at = now\(\) WHERE id = \$2`).
		WithArgs("hibernated", lightTestWS).WillReturnResult(sqlmock.NewResult(0, 1))
	expectLightSetState(mock, []string{"parking"}, "parked", 1)
	expectLightClearParkError(mock)
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
			rec := recordSQL(t)
			w := lightCall(lh.Park, http.MethodPost, "/", "", nil)
			assertNoWrites(t, rec())
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
		mock.ExpectExec(lightDropPendingSQL).WithArgs(lightTestWS, "tok-new").WillReturnResult(sqlmock.NewResult(0, 1))
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
	expectLightClaimParking(mock, "owner", 0)
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
		mock.ExpectQuery(lightCommitSQL).WithArgs(lightTestWS).
			WillReturnRows(sqlmock.NewRows([]string{"status", "cur", "ret", "this_park"}).AddRow("online", "tok-cur", "tok-old", false))
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
		mock.ExpectQuery(lightCommitSQL).WithArgs(lightTestWS).
			WillReturnRows(sqlmock.NewRows([]string{"status", "cur", "ret", "this_park"}).AddRow("online", "tok-attempt", "tok-old", true))
		mock.ExpectExec(`UPDATE workspaces SET light_run_token_id = \$2, light_pending_token_id = NULL WHERE id = \$1`).WithArgs(lightTestWS, "tok-new").WillReturnResult(sqlmock.NewResult(0, 1))
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
	expectLightClearParkError(mock)

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
	expectLightClearParkError(mock)

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
	expectLightBusy(mock, false)
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

const lightClaimOpeningSQL = `UPDATE workspaces\s+SET light_state = \$3, light_state_at = now\(\), light_opened_by = \$2, updated_at = now\(\),\s+light_last_error = NULL, light_last_error_at = NULL\s+WHERE id = \$1 AND light_state = \$4`

// expectLightOpenMarker scripts the open marker an open records before its
// pause.
func expectLightOpenMarker(mock sqlmock.Sqlmock) {
	mock.ExpectExec(lightRecordErrorSQL).WithArgs(lightTestWS, "LIGHT_OPEN_INCOMPLETE", sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
}

func TestOpenPausesBeforeItWakes(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	api := &fakeLightAPI{pause: provisioner.LightStateResult{Cancelled: []string{"light-be7540cfd160-aaaaaaaa-29384756"}}}
	lh, wh := newLightTestHandler(t, api)
	expectLightLoad(mock, "hibernated", "hermes", "workspace", "parked", 0, nil, nil)
	expectLightOpenMarker(mock)
	mock.ExpectExec(`INSERT INTO light_runs \(workspace_id, run_id, schedule, status, source, finished_at\)`).
		WithArgs(lightTestWS, "light-be7540cfd160-aaaaaaaa-29384756", "light-be7540cfd160-aaaaaaaa").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(lightClaimOpeningSQL).WithArgs(lightTestWS, "owner", "opening", "parked").WillReturnResult(sqlmock.NewResult(0, 1))

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

// A failed pause claims nothing: the agent stays parked, a best-effort resume
// undoes whatever the pause may have done, and the open marker goes with it.
func TestOpenWhosePauseFailsLeavesTheAgentParked(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	api := &fakeLightAPI{pauseErr: &provisioner.LightCPError{HTTPStatus: 503, Code: "LIGHT_BACKEND_UNAVAILABLE"}}
	lh, _ := newLightTestHandler(t, api)
	expectLightLoad(mock, "hibernated", "hermes", "workspace", "parked", 0, nil, nil)
	expectLightOpenMarker(mock)
	mock.ExpectQuery(lightReadRowSQL).WithArgs(lightTestWS).WillReturnRows(sqlmock.NewRows([]string{"light_state", "status"}).AddRow("parked", "hibernated"))
	mock.ExpectExec(lightClearOpenMarkerSQL).WithArgs(lightTestWS, "LIGHT_OPEN_INCOMPLETE").WillReturnResult(sqlmock.NewResult(0, 1))
	rec := recordSQL(t)
	w := lightCall(lh.Open, http.MethodPost, "/", "", nil)
	for _, q := range sqlWrites(rec()) {
		if !strings.HasPrefix(q, "UPDATE workspaces SET light_last_error = ") {
			t.Errorf("a failed pause may only set and clear the open marker; it wrote: %s", q)
		}
	}
	if w.Code != http.StatusServiceUnavailable || lightBody(t, w)["code"] != "LIGHT_BACKEND_UNAVAILABLE" || w.Header().Get("Retry-After") == "" {
		t.Fatalf("open = %d %s", w.Code, w.Body.String())
	}
	if calls := api.Calls(); !reflect.DeepEqual(calls, []string{"pause " + lightTestWS, "resume " + lightTestWS}) {
		t.Errorf("CP calls = %v, want the pause then the undoing resume", calls)
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
	expectLightOpenMarker(mock)
	mock.ExpectExec(lightClaimOpeningSQL).WithArgs(lightTestWS, "refresh", "opening", "parked").WillReturnResult(sqlmock.NewResult(0, 1))
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
	for body, want := range map[string]int{`{}`: http.StatusUnprocessableEntity, `{"schedule":"  "}`: http.StatusUnprocessableEntity, `{"x":1}`: http.StatusBadRequest, ``: http.StatusBadRequest,
		`{"schedule":"daily"}{"schedule":"weekly"}`: http.StatusBadRequest, `{"schedule":"daily"} x`: http.StatusBadRequest} {
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

// A turn that started around the parking claim shows up after it: its
// heartbeat still records the task while 'parking', and anything it writes
// stamps last_activity_at. The park is then undone before the hibernate claim,
// so the pod is never stopped mid-turn, and LIGHT_AGENT_BUSY is recorded
// (review 21718 R3).
func TestParkFinishUndoesTheParkOfABusyAgent(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	api := &fakeLightAPI{}
	_, wh, stops := newParkTestHandler(t, api, nil)
	mock.ExpectQuery(lightReadRowSQL).WithArgs(lightTestWS).WillReturnRows(sqlmock.NewRows([]string{"light_state", "status"}).AddRow("parking", "online"))
	expectLightBusy(mock, true)
	expectLightSetState(mock, []string{"parking"}, "open", 1)
	mock.ExpectExec(lightRecordErrorSQL).WithArgs(lightTestWS, "LIGHT_AGENT_BUSY", sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	rec := recordSQL(t)

	if err := wh.finishLightPark(context.Background(), lightTestWS, lightTriggerRefresh, 0); !errors.Is(err, errLightParkAbandoned) {
		t.Fatalf("finish = %v, want errLightParkAbandoned", err)
	}
	for _, q := range rec() {
		if strings.Contains(q, "active_tasks = 0") {
			t.Errorf("the hibernate claim ran for a busy agent: %s", q)
		}
	}
	if len(*stops) != 0 || len(api.Calls()) != 0 {
		t.Errorf("stops=%v calls=%v, want the pod left running and the CronJobs still paused", *stops, api.Calls())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// The owner's open of an agent that is already open or opening takes it over:
// light_opened_by becomes owner and the idle window restarts, so the refresh
// cycle that opened it does not park it at the next idle moment (review 21718
// R2). A refresh's open of an open agent writes nothing.
func TestOwnerOpenOfAnOpenAgentTakesItOver(t *testing.T) {
	for _, state := range []string{"open", "opening"} {
		t.Run(state, func(t *testing.T) {
			mock := setupTestDB(t)
			setupTestRedis(t)
			api := &fakeLightAPI{}
			lh, wh := newLightTestHandler(t, api)
			expectLightLoad(mock, "online", "hermes", "workspace", state, 0, nil, nil)
			mock.ExpectExec(`UPDATE workspaces SET light_opened_by = 'owner', light_state_at = now\(\), updated_at = now\(\)\s+WHERE id = \$1 AND light_state IN \('open', 'opening'\)`).
				WithArgs(lightTestWS).WillReturnResult(sqlmock.NewResult(0, 1))
			w := lightCall(lh.Open, http.MethodPost, "/", "", nil)
			if body := lightBody(t, w); w.Code != http.StatusOK || body["status"] != "unchanged" || body["light_state"] != state {
				t.Fatalf("open = %d %v", w.Code, body)
			}

			expectLightLoad(mock, "online", "hermes", "workspace", state, 0, nil, nil)
			rec := recordSQL(t)
			if _, unchanged, _, ref := wh.openLight(context.Background(), lightTestWS, lightTriggerRefresh); ref != nil || !unchanged {
				t.Fatalf("refresh open = %v %v", unchanged, ref)
			}
			assertNoWrites(t, rec())
			if len(api.Calls()) != 0 {
				t.Errorf("CP called: %v", api.Calls())
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet: %v", err)
			}
		})
	}
}

// A park stuck in 'parking' (its resume keeps failing: a billing suspend, a
// lost Light object) is recovered by an open once it has held three minutes:
// a stopped pod is woken (opening), a pod still up is open at once, and the
// run token the stuck attempt never recorded is revoked. A younger park is
// still 409 (review 21718 R5).
func TestOpenRecoversAParkStuckInParking(t *testing.T) {
	stuck := time.Now().Add(-4 * time.Minute)
	t.Run("pod stopped: opening, and woken", func(t *testing.T) {
		mock := setupTestDB(t)
		setupTestRedis(t)
		api := &fakeLightAPI{}
		lh, wh := newLightTestHandler(t, api)
		wh.cpProv = &trackingCPProv{}
		expectLightLoadWith(mock, "hibernated", "hermes", "workspace", "parking", 0, "tok-cur", nil,
			lightLoadExtra{stateAt: stuck, pendingTokenID: "tok-lost"})
		expectLightOpenMarker(mock)
		mock.ExpectExec(lightRevokeTokenSQL).WithArgs("tok-lost", lightTestWS).WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec(lightDropPendingSQL).WithArgs(lightTestWS, "tok-lost").WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec(lightClaimOpeningSQL).WithArgs(lightTestWS, "owner", "opening", "parking").WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec(lightWakeClaimSQL).WithArgs("provisioning", lightTestWS).WillReturnResult(sqlmock.NewResult(0, 0))
		w := lightCall(lh.Open, http.MethodPost, "/", "", nil)
		wh.waitAsyncForTest()
		if w.Code != http.StatusAccepted || lightBody(t, w)["light_state"] != "opening" {
			t.Fatalf("open = %d %s", w.Code, w.Body.String())
		}
		if calls := api.Calls(); !reflect.DeepEqual(calls, []string{"pause " + lightTestWS}) {
			t.Errorf("CP calls = %v", calls)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet: %v", err)
		}
	})
	t.Run("pod still up: open at once", func(t *testing.T) {
		mock := setupTestDB(t)
		setupTestRedis(t)
		api := &fakeLightAPI{}
		lh, wh := newLightTestHandler(t, api)
		wh.cpProv = &trackingCPProv{}
		expectLightLoadWith(mock, "online", "hermes", "workspace", "parking", 0, "tok-cur", nil, lightLoadExtra{stateAt: stuck})
		expectLightOpenMarker(mock)
		mock.ExpectExec(lightClaimOpeningSQL).WithArgs(lightTestWS, "owner", "open", "parking").WillReturnResult(sqlmock.NewResult(0, 1))
		w := lightCall(lh.Open, http.MethodPost, "/", "", nil)
		wh.waitAsyncForTest()
		if w.Code != http.StatusAccepted || lightBody(t, w)["light_state"] != "open" {
			t.Fatalf("open = %d %s", w.Code, w.Body.String())
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet (no wake for a pod that is up): %v", err)
		}
	})
	t.Run("a younger park is still in progress", func(t *testing.T) {
		mock := setupTestDB(t)
		setupTestRedis(t)
		api := &fakeLightAPI{}
		lh, _ := newLightTestHandler(t, api)
		expectLightLoadWith(mock, "hibernated", "hermes", "workspace", "parking", 0, "tok-cur", nil,
			lightLoadExtra{stateAt: time.Now().Add(-time.Minute)})
		rec := recordSQL(t)
		w := lightCall(lh.Open, http.MethodPost, "/", "", nil)
		assertNoWrites(t, rec())
		if w.Code != http.StatusConflict || lightBody(t, w)["code"] != "LIGHT_TRANSITION_IN_PROGRESS" || len(api.Calls()) != 0 {
			t.Fatalf("open = %d %s calls=%v", w.Code, w.Body.String(), api.Calls())
		}
	})
}

// The owner's park keeps the agent's Light lock until its background finish
// (the pod stop and the resume) is done, so no other Light operation can run
// in between (plan Step 3.12).
func TestParkKeepsTheLightLockUntilItsFinishEnds(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	api := &fakeLightAPI{putRes: provisioner.LightPutResult{Schedules: []provisioner.LightScheduleState{
		{Name: "daily", CronJob: "light-be7540cfd160-aaaaaaaa"}, {Name: "weekly", CronJob: "light-be7540cfd160-bbbbbbbb"},
	}}}
	lh, wh, _ := newParkTestHandler(t, api, lightGrid())
	inStop := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	releaseStop := func() { once.Do(func() { close(release) }) }
	defer releaseStop()
	wh.stopFnOverride = func(context.Context, string) {
		close(inStop)
		<-release
	}
	expectParkUpToThePut(t, mock, lightStoredRows())
	expectParkAfterThePut(mock, [][]driver.Value{
		{"daily", "0 9 * * *", "America/Vancouver", "Write the daily report.", true, "light-be7540cfd160-aaaaaaaa", true},
		{"weekly", "0 8 * * 1", "UTC", "Weekly review.", false, "light-be7540cfd160-bbbbbbbb", true},
	}, []string{"daily", "weekly"})
	expectHibernateThenResume(mock)

	if w := lightCall(lh.Park, http.MethodPost, "/", "", nil); w.Code != http.StatusAccepted {
		t.Fatalf("park = %d %s", w.Code, w.Body.String())
	}
	select {
	case <-inStop:
	case <-time.After(10 * time.Second):
		t.Fatal("the background finish never stopped the pod")
	}
	lock := lightOpLock(lightTestWS)
	if lock.TryLock() {
		lock.Unlock()
		t.Fatal("the Light lock was released before the background finish ended")
	}
	releaseStop()
	wh.waitAsyncForTest()
	if !lock.TryLock() {
		t.Fatal("the Light lock is still held after the finish")
	}
	lock.Unlock()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// The park reads the agent's grid through the runtime's /internal/schedules
// with the platform's inbound secret (the schedules proxy's SSRF-safe path),
// up to the control plane's 8 MiB body limit rather than the proxy's 1 MiB. A
// grid without the "schedules" list is refused, never read as empty, which
// would delete every CronJob (plan Step 3.11; review 21718 N8).
func TestReadAgentSchedulesUsesTheSchedulesProxy(t *testing.T) {
	allowLoopbackForTest(t)
	var mu sync.Mutex
	var gotAuth, gotPath, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		b := body
		mu.Unlock()
		_, _ = io.WriteString(w, b)
	}))
	defer srv.Close()
	setBody := func(s string) {
		mu.Lock()
		body = s
		mu.Unlock()
	}
	read := func() ([]volumeEntry, error) {
		mock := setupTestDB(t)
		_, wh := newLightTestHandler(t, &fakeLightAPI{})
		mock.ExpectQuery(`SELECT COALESCE\(url, ''\) FROM workspaces WHERE id = \$1`).WithArgs(lightTestWS).
			WillReturnRows(sqlmock.NewRows([]string{"url"}).AddRow(srv.URL))
		expectInboundSecret(mock, lightTestWS, "inbound-secret")
		return wh.readAgentSchedules(context.Background(), lightTestWS)
	}

	prompt := strings.Repeat("p", 60<<10)
	var b strings.Builder
	b.WriteString(`{"schedules":[{"name":"daily","cron":"0 9 * * *","timezone":"UTC","prompt":"Write the report.","enabled":true}`)
	for i := 0; i < 30; i++ {
		fmt.Fprintf(&b, `,{"name":"s%d","cron":"0 8 * * 1","timezone":"UTC","prompt":%q,"enabled":false}`, i, prompt)
	}
	b.WriteString(`]}`)
	if b.Len() <= 1<<20 {
		t.Fatalf("test grid is %d bytes; it must exceed the proxy's 1 MiB cap", b.Len())
	}
	setBody(b.String())
	grid, err := read()
	if err != nil || len(grid) != 31 || grid[0].Name != "daily" || grid[0].Prompt != "Write the report." || !grid[0].Enabled || grid[1].Prompt != prompt {
		t.Fatalf("grid of %d entries, err=%v", len(grid), err)
	}
	mu.Lock()
	if gotAuth != "Bearer inbound-secret" || gotPath != "/internal/schedules" {
		t.Errorf("request auth=%q path=%q, want the inbound secret on /internal/schedules", gotAuth, gotPath)
	}
	mu.Unlock()

	setBody(`{"items":[]}`)
	if grid, err := read(); err == nil {
		t.Errorf("a grid without the schedules list was read as %v", grid)
	}
	setBody(`{"schedules":[{"name":"x","prompt":"` + strings.Repeat("y", 8<<20) + `"}]}`)
	if _, err := read(); !errors.Is(err, errLightGridTooLarge) {
		t.Errorf("err = %v, want errLightGridTooLarge for a grid over 8 MiB", err)
	}
}

// A grid over 8 MiB is refused 413 before anything is minted or sent, and the
// agent goes back to open with the error recorded.
func TestParkOfAGridOver8MiBIs413AndStaysOpen(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	api := &fakeLightAPI{}
	lh, wh, _ := newParkTestHandler(t, api, nil)
	wh.lightScheduleReader = func(context.Context, string) ([]volumeEntry, error) { return nil, errLightGridTooLarge }
	expectLightLoad(mock, "online", "hermes", "workspace", "open", 0, nil, nil)
	mock.ExpectQuery(lightSecretReadSQL).WithArgs(lightTestWS, "MOLECULE_LIGHT").WillReturnRows(encryptedRow(t, "1"))
	expectLightClaimParking(mock, "owner", 0)
	expectLightSetState(mock, []string{"parking"}, "open", 1)
	mock.ExpectExec(lightRecordErrorSQL).WithArgs(lightTestWS, "LIGHT_TOO_LARGE", sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))

	w := lightCall(lh.Park, http.MethodPost, "/", "", nil)
	if w.Code != http.StatusRequestEntityTooLarge || lightBody(t, w)["code"] != "LIGHT_TOO_LARGE" || len(api.Calls()) != 0 {
		t.Fatalf("park = %d %s calls=%v", w.Code, w.Body.String(), api.Calls())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// A workspace deleted while the park's PUT ran: the delete may have missed
// what the PUT wrote, so the park deletes the Light objects again, revokes the
// token it minted and answers 404 (review 21718 O4).
func TestParkOfAWorkspaceDeletedDuringThePutDeletesItsObjectsAgain(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	api := &fakeLightAPI{putRes: provisioner.LightPutResult{Schedules: []provisioner.LightScheduleState{{Name: "daily", CronJob: "light-be7540cfd160-aaaaaaaa"}}}}
	lh, wh, stops := newParkTestHandler(t, api, lightGrid())
	expectParkUpToThePut(t, mock, lightStoredRows())
	mock.ExpectBegin()
	mock.ExpectQuery(lightCommitSQL).WithArgs(lightTestWS).
		WillReturnRows(sqlmock.NewRows([]string{"status", "cur", "ret", "this_park"}).AddRow("removed", "tok-cur", "tok-old", false))
	mock.ExpectRollback()
	mock.ExpectExec(lightRevokeTokenSQL).WithArgs("tok-new", lightTestWS).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(lightDropPendingSQL).WithArgs(lightTestWS, "tok-new").WillReturnResult(sqlmock.NewResult(0, 1))

	w := lightCall(lh.Park, http.MethodPost, "/", "", nil)
	wh.waitAsyncForTest()
	if w.Code != http.StatusNotFound || lightBody(t, w)["code"] != "WORKSPACE_NOT_FOUND" {
		t.Fatalf("park = %d %s", w.Code, w.Body.String())
	}
	if calls := api.Calls(); !reflect.DeepEqual(calls, []string{"put " + lightTestWS, "delete " + lightTestWS}) {
		t.Errorf("CP calls = %v, want the PUT's objects deleted again", calls)
	}
	if len(*stops) != 0 {
		t.Errorf("stops = %v", *stops)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}
