package handlers

// wake_lifecycle_test.go — fast, DB-less unit coverage of wake_lifecycle.go.
//
// These use sqlmock (no build tag) so they run in the default `go test ./...`
// and, crucially, keep every unexported owner symbol (wakeIdempotencyKey,
// currentDesiredGeneration, MarkWake* SQL) referenced under the DEFAULT lint
// build — the dormant owner would otherwise trip staticcheck U1000, since its
// real-behaviour tests live behind the `integration` build tag (see
// wake_lifecycle_integration_test.go). The tx-heavy concurrency, dedup, and
// convergence SEMANTICS are proven against a real Postgres there; sqlmock cannot
// model row-lock serialization or a -race monotonic increment.

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestWakeIdempotencyKey_PerKindDerivation pins the cross-wake dedup identity
// per kind: greet and restart-context are ONCE-per-box (workspace id is the
// whole key, seed ignored); the recurring kinds fold in the caller's seed.
func TestWakeIdempotencyKey_PerKindDerivation(t *testing.T) {
	const ws = "ws-abc"

	// Once-per-box kinds: the seed must NOT change the key.
	if got, want := wakeIdempotencyKey(WakeFirstBootGreet, ws, "seed-ignored"), "first-boot-greet:ws-abc"; got != want {
		t.Errorf("first-boot-greet key = %q, want %q", got, want)
	}
	if got, want := wakeIdempotencyKey(WakeRestartContext, ws, "seed-ignored"), "restart-context:ws-abc"; got != want {
		t.Errorf("restart-context key = %q, want %q", got, want)
	}

	// Recurring kinds: the seed scopes each distinct occurrence.
	for _, kind := range []WakeKind{WakeIdle, WakeStall, WakeNudge, WakeLifecycle} {
		a := wakeIdempotencyKey(kind, ws, "seed-1")
		b := wakeIdempotencyKey(kind, ws, "seed-2")
		if a == b {
			t.Errorf("kind %q: distinct seeds produced the same key %q — recurring wakes would collapse to one", kind, a)
		}
		if want := string(kind) + ":" + ws + ":seed-1"; a != want {
			t.Errorf("kind %q key = %q, want %q", kind, a, want)
		}
	}
}

// TestCurrentDesiredGeneration_Reads pins the read SQL and scan.
func TestCurrentDesiredGeneration_Reads(t *testing.T) {
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT desired_generation FROM workspaces WHERE id = \$1`).
		WithArgs("ws-1").
		WillReturnRows(sqlmock.NewRows([]string{"desired_generation"}).AddRow(int64(7)))

	got, err := currentDesiredGeneration(context.Background(), "ws-1")
	if err != nil {
		t.Fatalf("currentDesiredGeneration: %v", err)
	}
	if got != 7 {
		t.Errorf("desired_generation = %d, want 7", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestMarkWakeDelivered_UpdatesPendingOrDispatched pins the delivered transition
// SQL (pending/dispatched → delivered, keyed by workspace+idempotency_key).
func TestMarkWakeDelivered_UpdatesPendingOrDispatched(t *testing.T) {
	mock := setupTestDB(t)
	h := &WorkspaceHandler{}

	mock.ExpectExec(`UPDATE wake_intents SET status = 'delivered'`).
		WithArgs("ws-1", "first-boot-greet:ws-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := h.MarkWakeDelivered(context.Background(), "ws-1", "first-boot-greet:ws-1"); err != nil {
		t.Fatalf("MarkWakeDelivered: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestMarkWakeSettled_UpdatesDeliveredBelowObserved pins the convergence SQL
// (delivered AND generation <= observed → settled).
func TestMarkWakeSettled_UpdatesDeliveredBelowObserved(t *testing.T) {
	mock := setupTestDB(t)
	h := &WorkspaceHandler{}

	mock.ExpectExec(`UPDATE wake_intents SET status = 'settled', settled_at = now\(\)`).
		WithArgs("ws-1", int64(5)).
		WillReturnResult(sqlmock.NewResult(0, 2))

	if err := h.MarkWakeSettled(context.Background(), "ws-1", 5); err != nil {
		t.Fatalf("MarkWakeSettled: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
