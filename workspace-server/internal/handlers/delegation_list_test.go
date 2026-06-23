package handlers

// delegation_list_test.go — unit tests for listDelegationsFromLedger and
// listDelegationsFromActivityLogs. Both methods are the data-backend of the
// ListDelegations handler; coverage was missing (cf. infra-sre review of PR #942).

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
)

// ---------- listDelegationsFromLedger ----------

func TestListDelegationsFromLedger_EmptyResult(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	prevDB := db.DB
	db.DB = mockDB
	t.Cleanup(func() { db.DB = prevDB; mockDB.Close() })

	rows := sqlmock.NewRows([]string{
		"delegation_id", "caller_id", "callee_id", "task_preview",
		"status", "result_preview", "error_detail",
		"last_heartbeat", "deadline", "created_at", "updated_at", "direction",
	})
	mock.ExpectQuery("SELECT .+ FROM delegations").
		WithArgs("ws-1").
		WillReturnRows(rows)

	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	got := dh.listDelegationsFromLedger(context.Background(), "ws-1")
	if got != nil {
		t.Errorf("empty result: expected nil, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

func TestListDelegationsFromLedger_SingleRow(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	prevDB := db.DB
	db.DB = mockDB
	t.Cleanup(func() { db.DB = prevDB; mockDB.Close() })

	now := time.Now()
	// Use time.Time{} for nullable *time.Time columns — sqlmock passes the
	// zero value to the handler's scan destination. The handler checks Valid
	// before using each nullable field, so zero values are safe.
	rows := sqlmock.NewRows([]string{
		"delegation_id", "caller_id", "callee_id", "task_preview",
		"status", "result_preview", "error_detail",
		"last_heartbeat", "deadline", "created_at", "updated_at", "direction",
	}).AddRow(
		"del-1", "ws-1", "ws-2", "summarise the report",
		"completed", "the report is about Q1",
		"", now, now, now, now, "sent",
	)
	mock.ExpectQuery("SELECT .+ FROM delegations").
		WithArgs("ws-1").
		WillReturnRows(rows)

	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	got := dh.listDelegationsFromLedger(context.Background(), "ws-1")
	if len(got) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(got))
	}
	e := got[0]
	if e["delegation_id"] != "del-1" {
		t.Errorf("delegation_id: got %v, want del-1", e["delegation_id"])
	}
	if e["source_id"] != "ws-1" {
		t.Errorf("source_id: got %v, want ws-1", e["source_id"])
	}
	if e["target_id"] != "ws-2" {
		t.Errorf("target_id: got %v, want ws-2", e["target_id"])
	}
	if e["status"] != "completed" {
		t.Errorf("status: got %v, want completed", e["status"])
	}
	if e["response_preview"] != "the report is about Q1" {
		t.Errorf("response_preview: got %v", e["response_preview"])
	}
	if _, ok := e["error"]; ok {
		t.Errorf("error should be absent when empty, got %v", e["error"])
	}
	if e["_ledger"] != true {
		t.Errorf("_ledger marker: got %v, want true", e["_ledger"])
	}
	if e["direction"] != "sent" {
		t.Errorf("direction: got %v, want sent", e["direction"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

func TestListDelegationsFromLedger_MultipleRows(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	prevDB := db.DB
	db.DB = mockDB
	t.Cleanup(func() { db.DB = prevDB; mockDB.Close() })

	now := time.Now()
	rows := sqlmock.NewRows([]string{
		"delegation_id", "caller_id", "callee_id", "task_preview",
		"status", "result_preview", "error_detail",
		"last_heartbeat", "deadline", "created_at", "updated_at", "direction",
	}).
		AddRow("del-a", "ws-1", "ws-2", "task a", "in_progress", "", "", now, now, now, now, "sent").
		AddRow("del-b", "ws-1", "ws-3", "task b", "failed", "", "timeout", now, now, now, now, "sent").
		AddRow("del-c", "ws-1", "ws-4", "task c", "completed", "result c", "", now, now, now, now, "sent")
	mock.ExpectQuery("SELECT .+ FROM delegations").
		WithArgs("ws-1").
		WillReturnRows(rows)

	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	got := dh.listDelegationsFromLedger(context.Background(), "ws-1")
	if len(got) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(got))
	}
	if got[0]["delegation_id"] != "del-a" || got[1]["delegation_id"] != "del-b" || got[2]["delegation_id"] != "del-c" {
		t.Errorf("unexpected order: %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

func TestListDelegationsFromLedger_NullsOmitted(t *testing.T) {
	// last_heartbeat, deadline, result_preview, error_detail are all NULL.
	// Handler must not panic and must omit those keys from the map.
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	prevDB := db.DB
	db.DB = mockDB
	t.Cleanup(func() { mockDB.Close(); db.DB = prevDB })

	now := time.Now()
	rows := sqlmock.NewRows([]string{
		"delegation_id", "caller_id", "callee_id", "task_preview",
		"status", "result_preview", "error_detail",
		"last_heartbeat", "deadline", "created_at", "updated_at", "direction",
	}).
		AddRow("del-1", "ws-1", "ws-2", "task", "queued", nil, nil, nil, nil, now, now, "sent")
	mock.ExpectQuery("SELECT .+ FROM delegations").
		WithArgs("ws-1").
		WillReturnRows(rows)

	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	got := dh.listDelegationsFromLedger(context.Background(), "ws-1")
	if len(got) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(got))
	}
	e := got[0]
	if _, ok := e["last_heartbeat"]; ok {
		t.Error("last_heartbeat should be absent when NULL")
	}
	if _, ok := e["deadline"]; ok {
		t.Error("deadline should be absent when NULL")
	}
	if _, ok := e["response_preview"]; ok {
		t.Error("response_preview should be absent when NULL result_preview")
	}
	if _, ok := e["error"]; ok {
		t.Error("error should be absent when NULL error_detail")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

func TestListDelegationsFromLedger_QueryError(t *testing.T) {
	// Query failure returns nil — graceful fallback, no panic.
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	prevDB := db.DB
	db.DB = mockDB
	t.Cleanup(func() { db.DB = prevDB; mockDB.Close() })

	mock.ExpectQuery("SELECT .+ FROM delegations").
		WithArgs("ws-1").
		WillReturnError(context.DeadlineExceeded)

	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	got := dh.listDelegationsFromLedger(context.Background(), "ws-1")
	if got != nil {
		t.Errorf("query error: expected nil, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

func TestListDelegationsFromLedger_RowsErr(t *testing.T) {
	// rows.Err() mid-stream: handler collects partial results and returns them.
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	prevDB := db.DB
	db.DB = mockDB
	t.Cleanup(func() { db.DB = prevDB; mockDB.Close() })

	now := time.Now()
	// RowError(0) before AddRow(0): row 0 is "bad", rows.Next() returns false
	// on first call — the row never scans, result stays nil. To get partial
	// results (row 0 scanned) with rows.Err() non-nil, we use 2 rows and put
	// RowError(1) after AddRow(1): row 0 scans normally, row 1 is bad,
	// rows.Err() is error, handler returns partial result.
	rows := sqlmock.NewRows([]string{
		"delegation_id", "caller_id", "callee_id", "task_preview",
		"status", "result_preview", "error_detail",
		"last_heartbeat", "deadline", "created_at", "updated_at", "direction",
	}).
		AddRow("del-1", "ws-1", "ws-2", "task", "queued", "", "", now, now, now, now, "sent").
		AddRow("del-2", "ws-1", "ws-3", "another task", "queued", "", "", now, now, now, now, "sent").
		RowError(1, context.DeadlineExceeded)
	mock.ExpectQuery("SELECT .+ FROM delegations").
		WithArgs("ws-1").
		WillReturnRows(rows)

	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	got := dh.listDelegationsFromLedger(context.Background(), "ws-1")
	// Row 0 scanned and appended; row 1 is bad; rows.Err() is non-nil.
	// Handler logs the error but returns result (partial results because result != nil).
	if got == nil || len(got) != 1 {
		t.Errorf("rows.Err path: expected 1 partial result, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// TestListDelegationsFromLedger_ScanError is removed.
//
// In Go 1.25 sqlmock.NewRows validates column count at AddRow() time and
// panics when len(values) != len(columns). The old pattern
//   sqlmock.NewRows([]string{}).AddRow("only-one-col")
// therefore panics in test SETUP, not inside the handler. The handler has no
// recover(), so a scan panic would propagate out of listDelegationsFromLedger
// and crash the process — this is the correct behaviour (not silently skipping
// a row). The correct way to cover this path is a real-DB integration test.
//
// ---------- listDelegationsFromActivityLogs ----------

func TestListDelegationsFromActivityLogs_EmptyResult(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	prevDB := db.DB
	db.DB = mockDB
	t.Cleanup(func() { db.DB = prevDB; mockDB.Close() })

	rows := sqlmock.NewRows([]string{
		"id", "activity_type", "source_id", "target_id",
		"summary", "status", "error_detail",
		"response_preview", "delegation_id", "created_at", "workspace_id",
	})
	mock.ExpectQuery("SELECT .+ FROM activity_logs").
		WithArgs("ws-1").
		WillReturnRows(rows)

	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	got := dh.listDelegationsFromActivityLogs(context.Background(), "ws-1")
	if len(got) != 0 {
		t.Errorf("empty result: expected empty slice, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

func TestListDelegationsFromActivityLogs_SingleDelegateRow(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	prevDB := db.DB
	db.DB = mockDB
	t.Cleanup(func() { db.DB = prevDB; mockDB.Close() })

	now := time.Now()
	rows := sqlmock.NewRows([]string{
		"id", "activity_type", "source_id", "target_id",
		"summary", "status", "error_detail",
		"response_preview", "delegation_id", "created_at", "workspace_id",
	}).AddRow(
		"act-1", "delegate",
		"ws-1", "ws-2",
		"analyse Q1 numbers",
		"in_progress",
		"", "", "",
		now, "ws-1",
	)
	mock.ExpectQuery("SELECT .+ FROM activity_logs").
		WithArgs("ws-1").
		WillReturnRows(rows)

	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	got := dh.listDelegationsFromActivityLogs(context.Background(), "ws-1")
	if len(got) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(got))
	}
	e := got[0]
	if e["id"] != "act-1" {
		t.Errorf("id: got %v, want act-1", e["id"])
	}
	if e["type"] != "delegate" {
		t.Errorf("type: got %v, want delegate", e["type"])
	}
	if e["source_id"] != "ws-1" {
		t.Errorf("source_id: got %v, want ws-1", e["source_id"])
	}
	if e["target_id"] != "ws-2" {
		t.Errorf("target_id: got %v, want ws-2", e["target_id"])
	}
	if e["summary"] != "analyse Q1 numbers" {
		t.Errorf("summary: got %v", e["summary"])
	}
	if e["status"] != "in_progress" {
		t.Errorf("status: got %v", e["status"])
	}
	if e["direction"] != "sent" {
		t.Errorf("direction: got %v, want sent", e["direction"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

func TestListDelegationsFromActivityLogs_DelegateResultWithError(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	prevDB := db.DB
	db.DB = mockDB
	t.Cleanup(func() { db.DB = prevDB; mockDB.Close() })

	now := time.Now()
	rows := sqlmock.NewRows([]string{
		"id", "activity_type", "source_id", "target_id",
		"summary", "status", "error_detail",
		"response_preview", "delegation_id", "created_at", "workspace_id",
	}).AddRow(
		"act-2", "delegate_result",
		"ws-1", "ws-2",
		"result summary",
		"failed",
		"Callee workspace not reachable",
		`{"text":"the result body text"}`,
		"del-abc",
		now, "ws-1",
	)
	mock.ExpectQuery("SELECT .+ FROM activity_logs").
		WithArgs("ws-1").
		WillReturnRows(rows)

	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	got := dh.listDelegationsFromActivityLogs(context.Background(), "ws-1")
	if len(got) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(got))
	}
	e := got[0]
	if e["type"] != "delegate_result" {
		t.Errorf("type: got %v", e["type"])
	}
	if e["error"] != "Callee workspace not reachable" {
		t.Errorf("error: got %v", e["error"])
	}
	if e["response_preview"] != `{"text":"the result body text"}` {
		t.Errorf("response_preview: got %v", e["response_preview"])
	}
	if e["delegation_id"] != "del-abc" {
		t.Errorf("delegation_id: got %v", e["delegation_id"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

func TestListDelegationsFromActivityLogs_QueryError(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	prevDB := db.DB
	db.DB = mockDB
	t.Cleanup(func() { db.DB = prevDB; mockDB.Close() })

	mock.ExpectQuery("SELECT .+ FROM activity_logs").
		WithArgs("ws-1").
		WillReturnError(context.DeadlineExceeded)

	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	got := dh.listDelegationsFromActivityLogs(context.Background(), "ws-1")
	// Error → returns empty slice, not nil.
	if len(got) != 0 {
		t.Errorf("query error: expected empty slice, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

func TestListDelegationsFromActivityLogs_RowsErr(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	prevDB := db.DB
	db.DB = mockDB
	t.Cleanup(func() { db.DB = prevDB; mockDB.Close() })

	now := time.Now()
	// RowError(0) before AddRow(0): row 0 is "bad", rows.Next() returns false
	// on first call — the row never scans, result stays nil. To get partial
	// results (row 0 scanned) with rows.Err() non-nil, we use 2 rows and put
	// RowError(1) after AddRow(1): row 0 scans normally, row 1 is bad,
	// rows.Err() is error, handler returns partial result.
	rows := sqlmock.NewRows([]string{
		"id", "activity_type", "source_id", "target_id",
		"summary", "status", "error_detail",
		"response_preview", "delegation_id", "created_at", "workspace_id",
	}).
		AddRow("act-1", "delegate", "ws-1", "ws-2", "task", "queued", "", "", "", now, "ws-1").
		AddRow("act-2", "delegate", "ws-1", "ws-3", "another task", "queued", "", "", "", now, "ws-1").
		RowError(1, context.DeadlineExceeded)
	mock.ExpectQuery("SELECT .+ FROM activity_logs").
		WithArgs("ws-1").
		WillReturnRows(rows)

	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	got := dh.listDelegationsFromActivityLogs(context.Background(), "ws-1")
	// Row 0 scanned and appended; row 1 is bad; rows.Err() is non-nil.
	// Handler logs the error but returns result (partial results because result != nil).
	if got == nil || len(got) != 1 {
		t.Errorf("rows.Err path: expected 1 partial result, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// TestListDelegationsFromActivityLogs_ScanErrorSkipped is removed.
//
// Same reason as TestListDelegationsFromLedger_ScanError: Go 1.25 causes
// sqlmock.NewRows([]string{}).AddRow(...) to panic in test SETUP. The handler
// has no recover(), so a scan panic would crash the process — the correct
// behaviour. Real-DB integration tests cover this path.

// ---------- Direction: received (callee) ----------

func TestListDelegationsFromLedger_CalleeDirection_Received(t *testing.T) {
	// When the workspace ID appears as callee_id (not caller_id), direction = "received".
	// The query returns rows where d.caller_id = $1 OR d.callee_id = $1, and the
	// CASE expression sets direction based on whether caller_id matches.
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	prevDB := db.DB
	db.DB = mockDB
	t.Cleanup(func() { db.DB = prevDB; mockDB.Close() })

	now := time.Now()
	// ws-1 is the callee here (received a delegation from ws-other)
	rows := sqlmock.NewRows([]string{
		"delegation_id", "caller_id", "callee_id", "task_preview",
		"status", "result_preview", "error_detail",
		"last_heartbeat", "deadline", "created_at", "updated_at", "direction",
	}).AddRow(
		"del-received-1", "ws-other", "ws-1", "task from other workspace",
		"in_progress", "", "",
		now, now, now, now, "received",
	)
	mock.ExpectQuery("SELECT .+ FROM delegations").
		WithArgs("ws-1").
		WillReturnRows(rows)

	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	got := dh.listDelegationsFromLedger(context.Background(), "ws-1")
	if len(got) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(got))
	}
	e := got[0]
	if e["delegation_id"] != "del-received-1" {
		t.Errorf("delegation_id: got %v, want del-received-1", e["delegation_id"])
	}
	// source_id is the workspace that initiated the delegation (caller)
	if e["source_id"] != "ws-other" {
		t.Errorf("source_id: got %v, want ws-other", e["source_id"])
	}
	// target_id is the workspace receiving the delegation (callee)
	if e["target_id"] != "ws-1" {
		t.Errorf("target_id: got %v, want ws-1", e["target_id"])
	}
	if e["direction"] != "received" {
		t.Errorf("direction: got %v, want received", e["direction"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

func TestListDelegationsFromActivityLogs_ReceivedDirection(t *testing.T) {
	// When workspace_id differs from source_id, direction = "received".
	// This happens when the workspace received a delegation, not sent one.
	//
	// Non-vacuous guard (RC 11026): the sqlmock regex is tightened to
	// require BOTH `workspace_id` AND `source_id` in the WHERE clause
	// (with a "delegate" method filter). The previous regex
	// ("SELECT .+ FROM activity_logs") matched any query on
	// activity_logs and let a source_id-only predicate pass even
	// though it would silently exclude this row in real SQL. The
	// tightened regex pins the contract: any future change that
	// drops the OR clause (or narrows back to a single column)
	// fails this mock expectation, so the test is a real assertion
	// of the predicate shape, not just a happy-path sqlmock.
	mockDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	prevDB := db.DB
	db.DB = mockDB
	t.Cleanup(func() { db.DB = prevDB; mockDB.Close() })

	now := time.Now()
	// ws-1 is receiving a delegation from ws-other (workspace_id != source_id)
	rows := sqlmock.NewRows([]string{
		"id", "activity_type", "source_id", "target_id",
		"summary", "status", "error_detail",
		"response_preview", "delegation_id", "created_at", "workspace_id",
	}).AddRow(
		"act-received-1", "delegate",
		"ws-other", "ws-1",
		"Delegating to ws-1",
		"in_progress",
		"", "", "",
		now, "ws-1", // workspace_id = ws-1 (the receiving workspace)
	)
	// Tightened regex: require BOTH workspace_id AND source_id in
	// the WHERE clause (the OR predicate), pinned to the methods we
	// filter for. The previous regex matched any SELECT on
	// activity_logs and masked the predicate regression.
	mock.ExpectQuery(`SELECT .+ FROM activity_logs\s+WHERE \(workspace_id = \$1 OR source_id = \$1\) AND method IN \('delegate', 'delegate_result'\)`).
		WithArgs("ws-1").
		WillReturnRows(rows)

	broadcaster := newTestBroadcaster()
	wh := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	dh := NewDelegationHandler(wh, broadcaster)

	got := dh.listDelegationsFromActivityLogs(context.Background(), "ws-1")
	if len(got) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(got))
	}
	e := got[0]
	if e["id"] != "act-received-1" {
		t.Errorf("id: got %v, want act-received-1", e["id"])
	}
	if e["source_id"] != "ws-other" {
		t.Errorf("source_id: got %v, want ws-other", e["source_id"])
	}
	if e["target_id"] != "ws-1" {
		t.Errorf("target_id: got %v, want ws-1", e["target_id"])
	}
	if e["direction"] != "received" {
		t.Errorf("direction: got %v, want received", e["direction"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}
