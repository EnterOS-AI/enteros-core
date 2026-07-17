package handlers

// schedules_inheritance_capture_test.go — the CAPTURE half of the P4b volume-side
// org-re-import schedule inheritance path (core#4435). captureRuntimeSchedulesForCarryover
// runs inside CascadeDelete, BEFORE the predecessor container is torn down, and
// buffers only the user-created (source='runtime') grid entries into
// workspaces.carryover_runtime_schedules.
//
// NEGATIVE CONTROL: both tests here fail on pre-change main (verified by stubbing
// captureRuntimeSchedulesForCarryover to a no-op and re-running):
//   - the "buffers only runtime" test fails because a no-op body never issues the
//     UPDATE (sqlmock unmet expectation), and the carryoverJSONMatcher additionally
//     fails the test if the template entry T is captured or the [A,B] order drifts;
//   - the "unreachable" test fails if any UPDATE is issued despite the runtime being
//     unreachable (sqlmock errors on the unexpected query).

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// carryoverJSONMatcher asserts the captured JSON is EXACTLY the expected ordered
// set of runtime-source entry names (and nothing else) — the built-in negative
// control that template-source entries are never captured.
type carryoverJSONMatcher struct {
	wantNames []string
}

func (m carryoverJSONMatcher) Match(v driver.Value) bool {
	var b []byte
	switch t := v.(type) {
	case []byte:
		b = t
	case string:
		b = []byte(t)
	default:
		return false
	}
	var entries []struct {
		Name   string `json:"name"`
		Source string `json:"source"`
	}
	if json.Unmarshal(b, &entries) != nil {
		return false
	}
	if len(entries) != len(m.wantNames) {
		return false
	}
	for i, e := range entries {
		if e.Name != m.wantNames[i] || e.Source != "runtime" {
			return false
		}
	}
	return true
}

// TestCaptureCarryover_BuffersOnlyRuntimeSchedules — grid A(runtime), B(runtime),
// T(template): the UPDATE that buffers the carryover must carry EXACTLY [A,B]
// (both runtime-source, in grid order). Negative control: the template entry T is
// NOT captured (the org-template reconcile re-seeds it on the successor volume, so
// carrying it would duplicate; template wins on collision).
func TestCaptureCarryover_BuffersOnlyRuntimeSchedules(t *testing.T) {
	allowLoopbackForTest(t)
	mock := setupTestDB(t)
	setupTestRedis(t)

	wsID := "22e59f01-0001-4000-8000-000000000001"
	markVolumeScheduler(t, wsID)

	stub := newVolumeRuntimeStub(t,
		`{"schedules":[
			{"name":"digest A","cron":"*/30 * * * *","timezone":"UTC","prompt":"pa","enabled":true,"source":"runtime"},
			{"name":"digest B","cron":"0 9 * * *","timezone":"UTC","prompt":"pb","enabled":false,"source":"runtime"},
			{"name":"seeded T","cron":"0 0 * * *","timezone":"UTC","prompt":"pt","enabled":true,"source":"template"}
		]}`,
		`{}`, `{}`,
	)

	// resolveScheduleFanoutTarget: url + inbound secret for the doomed workspace.
	expectFanoutURL(mock, wsID, stub.srv.URL)
	expectInboundSecret(mock, wsID, "cap-secret-1")

	// The capture UPDATE must buffer EXACTLY [A,B] (runtime-source), NOT T.
	mock.ExpectExec(`UPDATE workspaces SET carryover_runtime_schedules`).
		WithArgs(carryoverJSONMatcher{wantNames: []string{"digest A", "digest B"}}, wsID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	captureRuntimeSchedulesForCarryover(context.Background(), []string{wsID})

	// The runtime saw exactly one grid GET (capture never mutates the grid).
	if reqs := stub.got(); len(reqs) != 1 || reqs[0] != "GET /internal/schedules" {
		t.Errorf("want exactly one grid GET, got %v", reqs)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met (buffer must be exactly [A,B], template T excluded): %v", err)
	}
}

// TestCaptureCarryover_UnreachablePredecessor_NoWrite — when the predecessor's
// runtime is unreachable (already gone) the capture must log-and-skip and leave
// carryover NULL: teardown must NEVER block on a schedule capture. Proven by the
// ABSENCE of any UPDATE expectation — sqlmock errors on an unexpected query.
func TestCaptureCarryover_UnreachablePredecessor_NoWrite(t *testing.T) {
	allowLoopbackForTest(t)
	mock := setupTestDB(t)
	setupTestRedis(t)

	wsID := "22e59f01-0002-4000-8000-000000000002"
	markVolumeScheduler(t, wsID)

	// A dead loopback endpoint: nothing listens on 127.0.0.1:1 → connection refused.
	deadURL := "http://127.0.0.1:1"
	expectFanoutURL(mock, wsID, deadURL)
	expectInboundSecret(mock, wsID, "cap-secret-2")
	// No ExpectExec(UPDATE ...): a carryover write here would be an unexpected
	// query and fail the test — proving the buffer is left NULL on unreachable.

	captureRuntimeSchedulesForCarryover(context.Background(), []string{wsID})

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("carryover must be left NULL (no UPDATE) when the predecessor is unreachable: %v", err)
	}
}

// TestCaptureCarryover_NonVolumeWorkspace_Skipped — a legacy DB-backed workspace
// (no scheduler capability) is skipped entirely: no runtime forward, no UPDATE.
// Its schedules already persist in workspace_schedules and are re-pointed by the
// DB-world path, so capturing here would be redundant.
func TestCaptureCarryover_NonVolumeWorkspace_Skipped(t *testing.T) {
	allowLoopbackForTest(t)
	mock := setupTestDB(t)
	setupTestRedis(t)

	wsID := "22e59f01-0003-4000-8000-000000000003"
	// Intentionally NOT markVolumeScheduler → scheduleBackendIsVolume == false.
	// No sqlmock expectations at all: any DB touch would fail the test.

	captureRuntimeSchedulesForCarryover(context.Background(), []string{wsID})

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a non-volume workspace must be skipped with zero DB/runtime work: %v", err)
	}
}
