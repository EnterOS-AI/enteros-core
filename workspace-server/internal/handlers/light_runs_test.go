package handlers

// light_runs_test.go — the run ledger (C3) and the runs listing.

import (
	"database/sql"
	"database/sql/driver"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

const (
	lightReportStartedSQL = `INSERT INTO light_runs \(workspace_id, run_id, schedule, status, source, started_at\)\s+VALUES \(\$1, \$2, \$3, 'running', 'report', \$4\)\s+ON CONFLICT \(workspace_id, run_id\) DO UPDATE\s+SET source = 'report',\s+started_at = COALESCE\(light_runs\.started_at, EXCLUDED\.started_at\),\s+updated_at = now\(\)\s+WHERE light_runs\.status = 'running'\s+RETURNING status`
	// The SET lists are pinned whole: a finished report must set the run's
	// status (C3), and a regex that skipped the list could not see it change.
	lightReportFinishedSQL = `INSERT INTO light_runs \(workspace_id, run_id, schedule, status, source, started_at, finished_at, exit_code, output, error\)\s+VALUES \(\$1, \$2, \$3, \$4, 'report', \$5, COALESCE\(\$6, now\(\)\), \$7, \$8, \$9\)\s+ON CONFLICT \(workspace_id, run_id\) DO UPDATE\s+SET status = EXCLUDED\.status,\s+source = 'report',\s+started_at = COALESCE\(light_runs\.started_at, EXCLUDED\.started_at\),\s+finished_at = EXCLUDED\.finished_at,\s+exit_code = EXCLUDED\.exit_code,\s+output = EXCLUDED\.output,\s+error = EXCLUDED\.error,\s+updated_at = now\(\)\s+WHERE light_runs\.status = 'running' OR light_runs\.source = 'job'`
)

// asWorkspaceToken marks the request as authenticated by the workspace's own
// token, as WorkspaceAuth does for a workspace-kind bearer.
func asWorkspaceToken(c *gin.Context) { c.Set("authenticated_workspace_id", lightTestWS) }

const startedBody = `{"run_id":"light-be7540cfd160-1a2b3c4d-29384756","schedule":"daily-report","phase":"started","status":"running","started_at":"2026-11-13T04:39:52Z","finished_at":null,"exit_code":null,"output":"","error":""}`

func finishedBody(status, output, errText string, exit int) string {
	return `{"run_id":"light-be7540cfd160-1a2b3c4d-29384756","schedule":"daily-report","phase":"finished","status":"` + status +
		`","started_at":"2026-11-13T04:39:52Z","finished_at":"2026-11-13T04:41:10Z","exit_code":` + itoaForTest(exit) +
		`,"output":` + jsonStringForTest(output) + `,"error":` + jsonStringForTest(errText) + `}`
}

func itoaForTest(n int) string { return strconv.Itoa(n) }

func jsonStringForTest(s string) string {
	b := strings.Builder{}
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

var lightRunStartedAt = time.Date(2026, 11, 13, 4, 39, 52, 0, time.UTC)

func TestReportRunStartedInsertsARunningRow(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	lh, _ := newLightTestHandler(t, nil)

	expectLightState(mock, lightTestWS, "parked")
	mock.ExpectQuery(lightReportStartedSQL).
		WithArgs(lightTestWS, "light-be7540cfd160-1a2b3c4d-29384756", "daily-report", lightRunStartedAt).
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("running"))

	w := lightCall(lh.ReportRun, http.MethodPost, "/", startedBody, asWorkspaceToken)
	if w.Code != http.StatusOK || w.Body.String() != `{"ok":true}` {
		t.Fatalf("report = %d %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestReportRunFinishedRecordsTheOutcome(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	lh, _ := newLightTestHandler(t, nil)

	finishedAt := time.Date(2026, 11, 13, 4, 41, 10, 0, time.UTC)
	expectLightState(mock, lightTestWS, "parked")
	mock.ExpectExec(lightReportFinishedSQL).
		WithArgs(lightTestWS, "light-be7540cfd160-1a2b3c4d-29384756", "daily-report", "failed", lightRunStartedAt, finishedAt, 1, "partial output", "the model refused").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := lightCall(lh.ReportRun, http.MethodPost, "/", finishedBody("failed", "partial output", "the model refused", 1), asWorkspaceToken)
	if w.Code != http.StatusOK {
		t.Fatalf("report = %d %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestReportRunStartedAfterItFinishedIs409(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	lh, _ := newLightTestHandler(t, nil)

	expectLightState(mock, lightTestWS, "parked")
	// The conflict's WHERE (status = 'running') matched nothing: no row returned.
	mock.ExpectQuery(lightReportStartedSQL).WillReturnRows(sqlmock.NewRows([]string{"status"}))

	w := lightCall(lh.ReportRun, http.MethodPost, "/", startedBody, asWorkspaceToken)
	body := lightBody(t, w)
	if w.Code != http.StatusConflict || body["code"] != "LIGHT_RUN_FINISHED" || body["run_id"] != "light-be7540cfd160-1a2b3c4d-29384756" {
		t.Fatalf("report = %d %v, want 409 LIGHT_RUN_FINISHED", w.Code, body)
	}
}

func TestReportRunFinishedTwiceIsA200NoOp(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	lh, _ := newLightTestHandler(t, nil)

	expectLightState(mock, lightTestWS, "open")
	// A reported terminal row: the conflict's WHERE matches nothing, zero rows.
	mock.ExpectExec(lightReportFinishedSQL).WillReturnResult(sqlmock.NewResult(0, 0))

	w := lightCall(lh.ReportRun, http.MethodPost, "/", finishedBody("succeeded", "done", "", 0), asWorkspaceToken)
	if w.Code != http.StatusOK {
		t.Fatalf("a retried finished report = %d %s, want 200", w.Code, w.Body.String())
	}
}

func TestReportRunOnANonLightAgentIs409(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	lh, _ := newLightTestHandler(t, nil)
	expectLightState(mock, lightTestWS, "")

	w := lightCall(lh.ReportRun, http.MethodPost, "/", startedBody, asWorkspaceToken)
	if w.Code != http.StatusConflict || lightBody(t, w)["code"] != "LIGHT_NOT_ENABLED" {
		t.Fatalf("report = %d %s, want 409 LIGHT_NOT_ENABLED", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet (nothing may be written): %v", err)
	}
}

func TestReportRunNeedsTheWorkspacesOwnToken(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	lh, _ := newLightTestHandler(t, nil)
	for name, setup := range map[string]func(*gin.Context){
		"owner credential (no workspace token)": nil,
		"another workspace's token":             func(c *gin.Context) { c.Set("authenticated_workspace_id", "00000000-0000-0000-0000-000000000099") },
	} {
		w := lightCall(lh.ReportRun, http.MethodPost, "/", startedBody, setup)
		if w.Code != http.StatusForbidden || lightBody(t, w)["code"] != "LIGHT_RUN_TOKEN_REQUIRED" {
			t.Errorf("%s: %d %s, want 403 LIGHT_RUN_TOKEN_REQUIRED", name, w.Code, w.Body.String())
		}
	}
}

func TestReportRunValidatesTheBody(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)
	lh, _ := newLightTestHandler(t, nil)
	cases := map[string]struct{ body, field string }{
		"not json":         {`nope`, "body"},
		"no run_id":        {strings.Replace(startedBody, `"run_id":"light-be7540cfd160-1a2b3c4d-29384756"`, `"run_id":""`, 1), "run_id"},
		"run_id space":     {strings.Replace(startedBody, `light-be7540cfd160-1a2b3c4d-29384756`, `has space`, 1), "run_id"},
		"no schedule":      {strings.Replace(startedBody, `"schedule":"daily-report"`, `"schedule":""`, 1), "schedule"},
		"long schedule":    {strings.Replace(startedBody, `"schedule":"daily-report"`, `"schedule":"`+strings.Repeat("a", 129)+`"`, 1), "schedule"},
		"bad phase":        {strings.Replace(startedBody, `"phase":"started"`, `"phase":"halfway"`, 1), "phase"},
		"started+success":  {strings.Replace(startedBody, `"status":"running"`, `"status":"succeeded"`, 1), "status"},
		"finished+running": {strings.Replace(finishedBody("succeeded", "", "", 0), `"status":"succeeded"`, `"status":"running"`, 1), "status"},
		"no started_at":    {strings.Replace(startedBody, `"started_at":"2026-11-13T04:39:52Z"`, `"started_at":null`, 1), "started_at"},
		"bad started_at":   {strings.Replace(startedBody, `2026-11-13T04:39:52Z`, `yesterday`, 1), "started_at"},
		"bad finished_at":  {strings.Replace(finishedBody("failed", "", "", 1), `2026-11-13T04:41:10Z`, `later`, 1), "finished_at"},
		// exit_code is an INTEGER column: outside int32 is the report's fault
		// (400), not a database error (500).
		"exit above int32": {strings.Replace(finishedBody("failed", "", "", 1), `"exit_code":1`, `"exit_code":2147483648`, 1), "exit_code"},
		"exit below int32": {strings.Replace(finishedBody("failed", "", "", 1), `"exit_code":1`, `"exit_code":-2147483649`, 1), "exit_code"},
	}
	for name, tc := range cases {
		w := lightCall(lh.ReportRun, http.MethodPost, "/", tc.body, asWorkspaceToken)
		body := lightBody(t, w)
		if w.Code != http.StatusBadRequest || body["code"] != "LIGHT_INVALID_REPORT" || body["field"] != tc.field {
			t.Errorf("%s: %d %v, want 400 LIGHT_INVALID_REPORT field %s", name, w.Code, body, tc.field)
		}
	}
}

// A NUL in output or error cannot be stored in a Postgres text column; it
// becomes U+FFFD before the byte limits, so the report is never refused and
// the stored text still fits. (Real Postgres: light_runs_integration_test.go.)
func TestValidateLightRunReportReplacesNULAndKeepsTheLimits(t *testing.T) {
	start := "2026-11-13T04:39:52Z"
	exit := 2147483647
	out := "tool said a\x00b"
	errText := "bad\x00err"
	row, bad := validateLightRunReport(lightRunReport{
		RunID: "light-x-1", Schedule: "daily", Phase: "finished", Status: "failed",
		StartedAt: &start, ExitCode: &exit, Output: &out, Error: &errText,
	})
	if bad != "" || row.Output != "tool said a\uFFFDb" || row.Error != "bad\uFFFDerr" {
		t.Fatalf("row=%+v bad=%q, want NUL replaced by U+FFFD and int32 max accepted", row, bad)
	}
	big := strings.Repeat("\x00", lightOutputLimit)
	bigErr := strings.Repeat("\x00", lightErrorLimit)
	row, bad = validateLightRunReport(lightRunReport{
		RunID: "light-x-1", Schedule: "daily", Phase: "finished", Status: "failed", StartedAt: &start, Output: &big, Error: &bigErr,
	})
	if bad != "" || len(row.Output) > lightOutputLimit || len(row.Error) > lightErrorLimit ||
		strings.ContainsRune(row.Output, 0) || !utf8.ValidString(row.Output) || !utf8.ValidString(row.Error) {
		t.Fatalf("output %d bytes, error %d bytes (limits %d/%d), valid=%v/%v",
			len(row.Output), len(row.Error), lightOutputLimit, lightErrorLimit, utf8.ValidString(row.Output), utf8.ValidString(row.Error))
	}
}

// byteLenArg matches a string argument of exactly n bytes with the suffix.
type byteLenArg struct {
	n      int
	prefix string
	suffix string
}

func (a byteLenArg) Match(v driver.Value) bool {
	s, ok := v.(string)
	return ok && len(s) == a.n && strings.HasPrefix(s, a.prefix) && strings.HasSuffix(s, a.suffix)
}

func TestReportRunTruncatesOutputAndError(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	lh, _ := newLightTestHandler(t, nil)

	// 65,537 bytes of output: the last 64 KiB are kept (the "é" that straddles
	// the cut is dropped whole, never split). 5,000 bytes of error: the first
	// 4 KiB are kept.
	output := "é" + strings.Repeat("x", 65535) + "END"
	errText := "BEGIN" + strings.Repeat("e", 4995)
	expectLightState(mock, lightTestWS, "parked")
	mock.ExpectExec(lightReportFinishedSQL).
		WithArgs(lightTestWS, sqlmock.AnyArg(), "daily-report", "failed", sqlmock.AnyArg(), sqlmock.AnyArg(), 1,
			byteLenArg{n: 65536, prefix: "x", suffix: "END"}, byteLenArg{n: 4096, prefix: "BEGIN", suffix: "e"}).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := lightCall(lh.ReportRun, http.MethodPost, "/", finishedBody("failed", output, errText, 1), asWorkspaceToken)
	if w.Code != http.StatusOK {
		t.Fatalf("report = %d %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestTailAndHeadUTF8NeverSplitACharacter(t *testing.T) {
	if got := tailUTF8("aé", 1); got != "" {
		t.Errorf("tailUTF8 kept a split character: %q", got)
	}
	if got := tailUTF8("aéb", 3); got != "éb" {
		t.Errorf("tailUTF8 = %q", got)
	}
	if got := headUTF8("é", 1); got != "" {
		t.Errorf("headUTF8 kept a split character: %q", got)
	}
	if got := headUTF8("abc", 10); got != "abc" {
		t.Errorf("headUTF8 = %q", got)
	}
}

const lightRunsListSQL = `SELECT run_id, schedule, status, source, scheduled_for, started_at, finished_at, exit_code, output, error, created_at\s+FROM light_runs WHERE workspace_id = \$1`

func lightRunsRows() *sqlmock.Rows {
	newer := time.Date(2026, 11, 13, 9, 1, 0, 0, time.UTC)
	older := time.Date(2026, 11, 13, 1, 0, 30, 0, time.UTC)
	sched := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	return sqlmock.NewRows([]string{"run_id", "schedule", "status", "source", "scheduled_for", "started_at", "finished_at", "exit_code", "output", "error", "created_at"}).
		AddRow("light-a-m1", "daily", "running", "report", sched, newer, nil, nil, "", "", newer).
		AddRow("light-a-2", "daily", "failed", "job", nil, nil, older, 137, "", "OOMKilled (exit 137)", older)
}

func TestListRunsReturnsNewestFirstWithActivityCounts(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	lh, _ := newLightTestHandler(t, nil)

	mock.ExpectQuery(`SELECT 1 FROM workspaces WHERE id = \$1 AND status != 'removed'`).WithArgs(lightTestWS).
		WillReturnRows(sqlmock.NewRows([]string{"one"}).AddRow(1))
	mock.ExpectQuery(lightRunsListSQL+`\s+ORDER BY created_at DESC, run_id DESC LIMIT \$2`).WithArgs(lightTestWS, 20).
		WillReturnRows(lightRunsRows())
	mock.ExpectQuery(`SELECT run_id, COUNT\(\*\) FROM activity_logs\s+WHERE workspace_id = \$1 AND run_id = ANY\(\$2\)\s+GROUP BY run_id`).
		WithArgs(lightTestWS, pqArrayArg{"light-a-m1", "light-a-2"}).
		WillReturnRows(sqlmock.NewRows([]string{"run_id", "count"}).AddRow("light-a-m1", 3))

	w := lightCall(lh.ListRuns, http.MethodGet, "/", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d %s", w.Code, w.Body.String())
	}
	body := lightBody(t, w)
	runs := body["runs"].([]interface{})
	if len(runs) != 2 || body["next_before"] != nil {
		t.Fatalf("body = %v", body)
	}
	first, second := runs[0].(map[string]interface{}), runs[1].(map[string]interface{})
	if first["run_id"] != "light-a-m1" || first["activity_count"] != float64(3) ||
		first["activity_path"] != "/workspaces/"+lightTestWS+"/activity?run_id=light-a-m1" ||
		first["scheduled_for"] != "2026-11-13T09:00:00Z" || first["finished_at"] != nil || first["exit_code"] != nil {
		t.Errorf("first = %v", first)
	}
	if second["status"] != "failed" || second["source"] != "job" || second["exit_code"] != float64(137) ||
		second["activity_count"] != float64(0) || second["error"] != "OOMKilled (exit 137)" {
		t.Errorf("second = %v", second)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestListRunsFiltersPagesAndCaps(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	lh, _ := newLightTestHandler(t, nil)

	before := "2026-11-13T09:00:00.123456Z"
	beforeTime, _ := time.Parse(time.RFC3339Nano, before)
	mock.ExpectQuery(`SELECT 1 FROM workspaces`).WithArgs(lightTestWS).WillReturnRows(sqlmock.NewRows([]string{"one"}).AddRow(1))
	mock.ExpectQuery(lightRunsListSQL+` AND schedule = \$2 AND created_at < \$3 ORDER BY created_at DESC, run_id DESC LIMIT \$4`).
		WithArgs(lightTestWS, "daily", beforeTime, 2).
		WillReturnRows(lightRunsRows())
	mock.ExpectQuery(`FROM activity_logs`).WillReturnRows(sqlmock.NewRows([]string{"run_id", "count"}))

	w := lightCall(lh.ListRuns, http.MethodGet, "/", "", func(c *gin.Context) {
		c.Request.URL.RawQuery = "limit=2&schedule=daily&before=" + before
	})
	body := lightBody(t, w)
	next, _ := body["next_before"].(string)
	if w.Code != http.StatusOK || next != lightRunsCursor(time.Date(2026, 11, 13, 1, 0, 30, 0, time.UTC), "light-a-2") {
		t.Fatalf("list = %d %v, want a full page with next_before = the last run's cursor", w.Code, body)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}

	// The cursor resumes after the last run, breaking created_at ties by run_id.
	mock.ExpectQuery(`SELECT 1 FROM workspaces`).WithArgs(lightTestWS).WillReturnRows(sqlmock.NewRows([]string{"one"}).AddRow(1))
	mock.ExpectQuery(lightRunsListSQL+` AND \(created_at, run_id\) < \(\$2, \$3\) ORDER BY created_at DESC, run_id DESC LIMIT \$4`).
		WithArgs(lightTestWS, time.Date(2026, 11, 13, 1, 0, 30, 0, time.UTC), "light-a-2", 2).
		WillReturnRows(sqlmock.NewRows([]string{"run_id"}))
	w = lightCall(lh.ListRuns, http.MethodGet, "/", "", func(c *gin.Context) {
		c.Request.URL.RawQuery = "limit=2&before=" + next
	})
	if body := lightBody(t, w); w.Code != http.StatusOK || body["next_before"] != nil {
		t.Fatalf("next page = %d %v, want an empty last page", w.Code, body)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}

	badCursor := lightRunsCursor(time.Now(), "has space")
	for _, q := range []string{"limit=0", "limit=x", "before=yesterday", "before=" + badCursor, "schedule=" + strings.Repeat("a", 129)} {
		w := lightCall(lh.ListRuns, http.MethodGet, "/", "", func(c *gin.Context) { c.Request.URL.RawQuery = q })
		if w.Code != http.StatusBadRequest || lightBody(t, w)["code"] != "LIGHT_BAD_REQUEST" {
			t.Errorf("%s: %d %s, want 400", q, w.Code, w.Body.String())
		}
	}
}

func TestListRunsCapsTheLimitAt100(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	lh, _ := newLightTestHandler(t, nil)
	mock.ExpectQuery(`SELECT 1 FROM workspaces`).WithArgs(lightTestWS).WillReturnRows(sqlmock.NewRows([]string{"one"}).AddRow(1))
	mock.ExpectQuery(lightRunsListSQL).WithArgs(lightTestWS, 100).WillReturnRows(sqlmock.NewRows([]string{"run_id"}))

	w := lightCall(lh.ListRuns, http.MethodGet, "/", "", func(c *gin.Context) { c.Request.URL.RawQuery = "limit=500" })
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestListRunsOfAMissingWorkspaceIs404(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	lh, _ := newLightTestHandler(t, nil)
	mock.ExpectQuery(`SELECT 1 FROM workspaces`).WithArgs(lightTestWS).WillReturnError(sql.ErrNoRows)
	if w := lightCall(lh.ListRuns, http.MethodGet, "/", "", nil); w.Code != http.StatusNotFound {
		t.Fatalf("list = %d %s", w.Code, w.Body.String())
	}
}

// A failed finished report evaluates the schedule's streak at once, after the
// answer (plan Step 3.15), under the agent's Light lock.
func TestAFailedFinishedReportEvaluatesTheStreakAtOnce(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	api := &fakeLightAPI{}
	lh, wh := newLightTestHandler(t, api)
	finishedAt := time.Date(2026, 11, 13, 4, 41, 10, 0, time.UTC)
	expectLightState(mock, lightTestWS, "parked")
	mock.ExpectExec(lightReportFinishedSQL).
		WithArgs(lightTestWS, "light-be7540cfd160-1a2b3c4d-29384756", "daily-report", "timed_out", lightRunStartedAt, finishedAt, 124, "", "the run timed out").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(lightStreakScheduleSQL).WithArgs(lightTestWS, "daily-report").
		WillReturnRows(sqlmock.NewRows([]string{"suspended_reason", "suspended_at"}).AddRow(nil, nil))
	mock.ExpectQuery(lightStreakRunsSQL).WithArgs(lightTestWS, "daily-report", nil).WillReturnRows(threeFailures())
	mock.ExpectExec(lightStreakCountSQL).WithArgs(lightTestWS, "daily-report", 3).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(lightStreakClaimSQL).WithArgs(lightTestWS, "daily-report", "3 runs in a row failed").
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("daily-report"))
	mock.ExpectQuery(`SELECT name, talk_to_user_enabled FROM workspaces`).WithArgs(lightTestWS).
		WillReturnRows(sqlmock.NewRows([]string{"name", "talk_to_user_enabled"}).AddRow("Light agent", false))

	w := lightCall(lh.ReportRun, http.MethodPost, "/", finishedBody("timed_out", "", "the run timed out", 124), asWorkspaceToken)
	if w.Code != http.StatusOK {
		t.Fatalf("report = %d %s", w.Code, w.Body.String())
	}
	wh.waitAsyncForTest()
	if calls := api.Calls(); !reflect.DeepEqual(calls, []string{"suspend " + lightTestWS + " daily-report"}) {
		t.Errorf("CP calls = %v, want the schedule suspended right after the report", calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// While a transition holds the agent, a failed report's streak is left to the
// jobs poll: nothing is evaluated between a park's render and its PUT (review
// 21718 O1).
func TestAFailedReportDuringATransitionLeavesTheStreakToThePoll(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	api := &fakeLightAPI{}
	lh, wh := newLightTestHandler(t, api)
	expectLightState(mock, lightTestWS, "parked")
	mock.ExpectExec(lightReportFinishedSQL).WillReturnResult(sqlmock.NewResult(0, 1))
	rec := recordSQL(t)
	lock := lightOpLock(lightTestWS)
	lock.Lock()
	w := lightCall(lh.ReportRun, http.MethodPost, "/", finishedBody("failed", "", "boom", 1), asWorkspaceToken)
	wh.waitAsyncForTest()
	lock.Unlock()
	if w.Code != http.StatusOK {
		t.Fatalf("report = %d %s", w.Code, w.Body.String())
	}
	for _, q := range rec() {
		if strings.Contains(q, "suspended_reason") || strings.Contains(q, "consecutive_failures") {
			t.Errorf("the streak was evaluated while a transition held the agent: %s", q)
		}
	}
	if len(api.Calls()) != 0 {
		t.Errorf("CP called: %v", api.Calls())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}
