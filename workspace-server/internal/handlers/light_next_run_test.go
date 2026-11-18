package handlers

// light_next_run_test.go — GET /workspaces/:id/light reports each schedule's
// next_run_at: the next fire time after now of its cron in its time zone, or
// null when the schedule is off, suspended, or does not parse.

import (
	"net/http"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// pinLightClock fixes the clock GET /light computes next_run_at with.
func pinLightClock(t *testing.T, now time.Time) {
	t.Helper()
	prev := lightNow
	lightNow = func() time.Time { return now }
	t.Cleanup(func() { lightNow = prev })
}

var lightScheduleColumns = []string{"name", "cron", "timezone", "prompt", "enabled", "suspended_reason", "suspended_at", "consecutive_failures", "cronjob"}

// expectLightGet scripts GET /light's three reads: the state row, the given
// schedule rows and no queued actions.
func expectLightGet(mock sqlmock.Sqlmock, state string, schedules *sqlmock.Rows) {
	at := time.Date(2026, 11, 13, 9, 0, 4, 0, time.UTC)
	mock.ExpectQuery(`SELECT COALESCE\(light_state, ''\), light_state_at, light_last_error, light_last_error_at\s+FROM workspaces WHERE id = \$1 AND status != 'removed'`).
		WithArgs(lightTestWS).
		WillReturnRows(sqlmock.NewRows([]string{"light_state", "light_state_at", "light_last_error", "light_last_error_at"}).
			AddRow(state, at, nil, nil))
	mock.ExpectQuery(`FROM light_schedules WHERE workspace_id = \$1 ORDER BY name`).WithArgs(lightTestWS).
		WillReturnRows(schedules)
	mock.ExpectQuery(`FROM light_pending_actions\s+WHERE workspace_id = \$1 AND applied_at IS NULL ORDER BY id`).WithArgs(lightTestWS).
		WillReturnRows(sqlmock.NewRows([]string{"kind", "source", "created_at"}))
}

// nextRunAtByName reads every schedule's next_run_at from a GET /light body,
// failing the test when a schedule lacks the key.
func nextRunAtByName(t *testing.T, body map[string]interface{}) map[string]interface{} {
	t.Helper()
	out := map[string]interface{}{}
	for _, raw := range body["schedules"].([]interface{}) {
		s := raw.(map[string]interface{})
		v, ok := s["next_run_at"]
		if !ok {
			t.Errorf("schedule %v has no next_run_at key", s["name"])
		}
		out[s["name"].(string)] = v
	}
	return out
}

func TestLightGetReportsEachSchedulesNextRunAt(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	lh, _ := newLightTestHandler(t, nil)
	// 09:30 UTC is 18:30 in Tokyo (UTC+9 all year).
	pinLightClock(t, time.Date(2026, 11, 13, 9, 30, 0, 0, time.UTC))
	suspendedAt := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	expectLightGet(mock, "parked", sqlmock.NewRows(lightScheduleColumns).
		AddRow("a-utc", "0 9 * * *", "UTC", "p", true, nil, nil, 0, "cj-a").
		AddRow("b-tokyo", "0 9 * * *", "Asia/Tokyo", "p", true, nil, nil, 0, "cj-b").
		AddRow("c-no-zone", "45 9 * * *", "", "p", true, nil, nil, 0, "cj-c").
		AddRow("d-off", "0 9 * * *", "UTC", "p", false, nil, nil, 0, "cj-d").
		AddRow("e-suspended", "0 9 * * *", "UTC", "p", true, "three failed runs in a row", suspendedAt, 3, "cj-e").
		AddRow("f-bad-cron", "61 * * * *", "UTC", "p", true, nil, nil, 0, "cj-f").
		AddRow("g-bad-zone", "0 9 * * *", "Mars/Olympus_Mons", "p", true, nil, nil, 0, "cj-g").
		AddRow("h-descriptor", "@daily", "UTC", "p", true, nil, nil, 0, "cj-h"))

	w := lightCall(lh.Get, http.MethodGet, "/", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("get = %d %s", w.Code, w.Body.String())
	}
	got := nextRunAtByName(t, lightBody(t, w))
	want := map[string]interface{}{
		"a-utc":        "2026-11-14T09:00:00Z", // 09:00 today has passed
		"b-tokyo":      "2026-11-14T00:00:00Z", // 09:00 JST tomorrow
		"c-no-zone":    "2026-11-13T09:45:00Z", // no zone is UTC, the schedule grid's default
		"d-off":        nil,
		"e-suspended":  nil,
		"f-bad-cron":   nil,
		"g-bad-zone":   nil,
		"h-descriptor": nil, // the schedules proxy's parser takes five fields, no @descriptors
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s: next_run_at = %v, want %v", name, got[name], w)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// next_run_at is the schedule's own time whatever the agent's state: an open
// agent's CronJobs are paused, but the schedule still says when it runs next.
func TestLightGetNextRunAtIgnoresTheLightState(t *testing.T) {
	for _, state := range []string{"open", "parking", "parked", "opening"} {
		t.Run(state, func(t *testing.T) {
			mock := setupTestDB(t)
			setupTestRedis(t)
			lh, _ := newLightTestHandler(t, nil)
			pinLightClock(t, time.Date(2026, 11, 13, 9, 30, 0, 0, time.UTC))
			expectLightGet(mock, state, sqlmock.NewRows(lightScheduleColumns).
				AddRow("hourly", "15 * * * *", "UTC", "p", true, nil, nil, 0, nil))

			w := lightCall(lh.Get, http.MethodGet, "/", "", nil)
			if w.Code != http.StatusOK {
				t.Fatalf("get = %d %s", w.Code, w.Body.String())
			}
			if got := nextRunAtByName(t, lightBody(t, w))["hourly"]; got != "2026-11-13T10:15:00Z" {
				t.Errorf("next_run_at = %v, want 2026-11-13T10:15:00Z", got)
			}
		})
	}
}
