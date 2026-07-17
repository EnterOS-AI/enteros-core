package handlers

// schedules_inheritance_capture_test.go — the CAPTURE half of the P4b volume-side
// org-re-import schedule inheritance path (core#4435). captureRuntimeSchedulesForCarryover
// runs inside CascadeDelete, BEFORE the predecessor container is torn down, and
// buffers only the user-created (source='runtime') grid entries into
// workspaces.carryover_runtime_schedules.
//
// NEGATIVE CONTROL:
//   - the "buffers only runtime" test fails on pre-change main (stub capture to a
//     no-op → the UPDATE never fires → sqlmock unmet expectation), and its
//     carryoverJSONMatcher additionally fails the test if the template entry T is
//     captured or the [A,B] order drifts.
//   - the "unreachable" and "non-volume" NO-WRITE tests assert the ABSENCE of a
//     write. A subtlety makes the naive form vacuous: production SWALLOWS DB errors
//     (best-effort teardown), so an absent ExpectExec would NOT fail on a spurious
//     write — sqlmock returns its "call was not expected" error to the swallowing
//     caller, and ExpectationsWereMet() cannot detect EXTRA (unexpected) calls. So
//     instead we register a TRIPWIRE: the carryover UPDATE (resp. the fanout URL
//     lookup) with an execCallSpy arg-matcher that flips a bool if the statement is
//     ever issued, turning "a write happened" into an OBSERVABLE flag the test
//     asserts is false. Discriminating-ness is re-verified by injecting a spurious
//     UPDATE on the unreachable path → the test goes RED (see PR #4453).

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// execCallSpy is a driver.Value arg-matcher whose only job is a side effect: it
// flips its shared *bool the moment sqlmock evaluates it (i.e. the statement it is
// attached to was actually issued), then returns true so the call is consumed
// cleanly. It is the tripwire that makes a spurious best-effort write — which
// production swallows and ExpectationsWereMet() cannot see — observable to a test.
type execCallSpy struct{ hit *bool }

func (s execCallSpy) Match(driver.Value) bool { *s.hit = true; return true }

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
// carryover NULL: teardown must NEVER block on a schedule capture. Discriminating:
// a carryover UPDATE tripwire (execCallSpy) flips `wrote` if the buffer is written
// despite the unreachable runtime; an absent expectation would be VACUOUS because
// production swallows the resulting sqlmock error (see file header).
func TestCaptureCarryover_UnreachablePredecessor_NoWrite(t *testing.T) {
	allowLoopbackForTest(t)
	mock := setupTestDB(t)
	setupTestRedis(t)
	// Unordered: the tripwire must catch a spurious UPDATE wherever in the
	// sequence a bug would issue it, not only in registration order.
	mock.MatchExpectationsInOrder(false)

	wsID := "22e59f01-0002-4000-8000-000000000002"
	markVolumeScheduler(t, wsID)

	// A dead loopback endpoint: nothing listens on 127.0.0.1:1 → connection
	// refused. resolve still succeeds (URL + secret below), so the code reaches
	// the grid GET and fails THERE — the exact point a buggy capture might still
	// write the buffer. (Non-vacuity: if resolve failed early there'd be no write
	// trivially; providing both rows drives the code to the reachable-write site.)
	deadURL := "http://127.0.0.1:1"
	expectFanoutURL(mock, wsID, deadURL)
	expectInboundSecret(mock, wsID, "cap-secret-2")

	// Tripwire: register the carryover UPDATE with a spy that flips `wrote` if it
	// is ever issued. On the correct (unreachable) path it never fires, so the
	// tripwire is left unfulfilled ON PURPOSE and we assert on `wrote` rather than
	// ExpectationsWereMet() (v1.5.2 has no .Maybe() to mark it optional).
	var wrote bool
	mock.ExpectExec(`UPDATE workspaces SET carryover_runtime_schedules`).
		WithArgs(execCallSpy{&wrote}, execCallSpy{&wrote}).
		WillReturnResult(sqlmock.NewResult(0, 1))

	captureRuntimeSchedulesForCarryover(context.Background(), []string{wsID})

	if wrote {
		t.Error("carryover UPDATE was issued on the unreachable path — teardown-time capture must leave carryover NULL when the predecessor runtime is unreachable")
	}
}

// TestCaptureCarryover_NonVolumeWorkspace_Skipped — a legacy DB-backed workspace
// (no scheduler capability) is skipped entirely: no runtime forward, no UPDATE.
// Its schedules already persist in workspace_schedules and are re-pointed by the
// DB-world path, so capturing here would be redundant. Discriminating: a tripwire
// on the FIRST DB op the volume path performs (the fanout URL lookup) flips
// `touched` if the skip ever regresses; an absent expectation would be vacuous
// because production swallows a stray query's error (see file header).
func TestCaptureCarryover_NonVolumeWorkspace_Skipped(t *testing.T) {
	allowLoopbackForTest(t)
	mock := setupTestDB(t)
	setupTestRedis(t)
	mock.MatchExpectationsInOrder(false)

	wsID := "22e59f01-0003-4000-8000-000000000003"
	// Intentionally NOT markVolumeScheduler → scheduleBackendIsVolume == false, so
	// the capture must skip this id BEFORE any DB or runtime work.

	// Tripwire on the fanout URL lookup — the first DB statement the volume path
	// would issue. The non-volume path must never reach it, so `touched` stays
	// false; if it flips, the volume-skip guard regressed.
	var touched bool
	mock.ExpectQuery(`SELECT COALESCE\(url, ''\) FROM workspaces WHERE id = \$1`).
		WithArgs(execCallSpy{&touched}).
		WillReturnRows(sqlmock.NewRows([]string{"url"}).AddRow("http://unused"))

	captureRuntimeSchedulesForCarryover(context.Background(), []string{wsID})

	if touched {
		t.Error("a non-volume workspace must be skipped with zero DB/runtime work — the fanout URL lookup fired")
	}
}
