//go:build integration
// +build integration

// wake_redrive_integration_test.go — real-Postgres proof of the generation-loop
// re-drive (wake_redrive.go). sqlmock (wake_redrive_test.go) pins the SQL shapes
// and the in-Go selection/bounding branches, but CANNOT model what actually
// matters here against the live engine: the status filter genuinely excluding
// settled/dropped rows, the age + throttle predicates keyed on real timestamps,
// and the attempt counter crossing the cap over successive passes against a live
// redrive_attempts column. Those are proven end-to-end here.
//
// Run (mirrors wake_lifecycle_integration_test.go):
//
//	INTEGRATION_DB_URL="postgres://postgres:test@localhost:55440/molecule?sslmode=disable" \
//	  go test -tags=integration -run TestIntegration_WakeRedrive ./internal/handlers/
//
// NOT SAFE for t.Parallel() — shares the global db.DB; each test owns its own
// freshly-seeded workspace rows (unique names) that cascade-clean on teardown.

package handlers

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

// recordingReEmitter records the keys/kinds it was asked to re-emit and never
// touches the DB — so a re-driven intent's status stays exactly as seeded and
// only the owner's own bookkeeping (redrive_attempts / last_redriven_at /
// dropped) changes. Safe for concurrent calls (the owner is single-goroutine
// per pass, but be defensive).
type recordingReEmitter struct {
	mu    sync.Mutex
	calls []reEmitCall
}

func (r *recordingReEmitter) emit(_ context.Context, workspaceID string, kind WakeKind, key string, gen int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, reEmitCall{workspaceID, kind, key, gen})
	return nil
}

func (r *recordingReEmitter) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// seedAgedWakeIntent seeds a wake_intents row with an explicit age (created_at =
// now() - age), status, and redrive_attempts so the re-drive selection and
// bounding can be driven deterministically. last_redriven_at is left NULL.
func seedAgedWakeIntent(t *testing.T, conn *sql.DB, workspaceID, key, kind string, generation int64, status string, age time.Duration, attempts int) {
	t.Helper()
	if _, err := conn.ExecContext(context.Background(),
		`INSERT INTO wake_intents (workspace_id, kind, idempotency_key, generation, status, created_at, redrive_attempts)
		 VALUES ($1, $2, $3, $4, $5, now() - ($6 * INTERVAL '1 second'), $7)`,
		workspaceID, kind, key, generation, status, int(age.Seconds()), attempts); err != nil {
		t.Fatalf("seedAgedWakeIntent %q: %v", key, err)
	}
}

// readWakeRedrive returns the redrive_attempts and status for an intent.
func readWakeRedrive(t *testing.T, conn *sql.DB, ws, key string) (attempts int, status string) {
	t.Helper()
	if err := conn.QueryRowContext(context.Background(),
		`SELECT redrive_attempts, status FROM wake_intents WHERE workspace_id = $1 AND idempotency_key = $2`,
		ws, key).Scan(&attempts, &status); err != nil {
		t.Fatalf("readWakeRedrive %q: %v", key, err)
	}
	return attempts, status
}

// TestIntegration_WakeRedrive_DeliveredUnsettledReEmittedOnce proves a stuck
// delivered-but-unsettled intent (old enough, below the attempt cap) is
// re-emitted exactly once through its EXISTING idempotency key: the recorder is
// invoked once with that key, redrive_attempts advances 0→1, and the intent
// itself is untouched (still 'delivered' — the re-emit path owns delivery, not
// the owner). A SECOND immediate pass is a no-op: last_redriven_at throttles it.
func TestIntegration_WakeRedrive_DeliveredUnsettledReEmittedOnce(t *testing.T) {
	conn := integrationDB_WakeLifecycle(t)
	ws := seedWakeWorkspace(t, conn, "test-wake-redrive-once")
	rec := &recordingReEmitter{}
	h := &WorkspaceHandler{}
	h.SetWakeReEmitter(rec.emit)
	ctx := context.Background()

	key := "first-boot-greet:" + ws
	// Delivered 20 min ago, never settled, never re-driven → stuck & eligible.
	seedAgedWakeIntent(t, conn, ws, key, "first-boot-greet", 1, "delivered", 20*time.Minute, 0)

	res, err := h.ReDriveStuckWakes(ctx, ws)
	if err != nil {
		t.Fatalf("ReDriveStuckWakes: %v", err)
	}
	if res.Redriven != 1 || res.Dropped != 0 {
		t.Fatalf("first pass result = %+v, want Redriven=1 Dropped=0", res)
	}
	if rec.count() != 1 {
		t.Fatalf("re-emit invoked %d times, want 1", rec.count())
	}
	if got := rec.calls[0]; got.key != key || got.kind != WakeFirstBootGreet {
		t.Errorf("re-emit call = %+v, want the intent's own key/kind", got)
	}
	if attempts, status := readWakeRedrive(t, conn, ws, key); attempts != 1 || status != "delivered" {
		t.Errorf("after re-drive: attempts=%d status=%q, want attempts=1 status=delivered (owner must not mutate delivery status)", attempts, status)
	}

	// Immediate second pass: last_redriven_at was just stamped, so the throttle
	// (redriveMinInterval) excludes it — no double re-emit in the same window.
	res2, err := h.ReDriveStuckWakes(ctx, ws)
	if err != nil {
		t.Fatalf("second ReDriveStuckWakes: %v", err)
	}
	if res2.Redriven != 0 || rec.count() != 1 {
		t.Errorf("second immediate pass re-emitted again (res=%+v, calls=%d); the throttle must suppress it", res2, rec.count())
	}
}

// TestIntegration_WakeRedrive_AttemptCapDrops proves the bound: an intent that
// stays stuck is re-emitted at most redriveMaxAttempts times and is then marked
// 'dropped' — never re-emitted again. The throttle is bypassed between passes
// (reset last_redriven_at) to simulate the passage of redriveMinInterval so the
// cap crossing is exercised without real waiting.
func TestIntegration_WakeRedrive_AttemptCapDrops(t *testing.T) {
	conn := integrationDB_WakeLifecycle(t)
	ws := seedWakeWorkspace(t, conn, "test-wake-redrive-cap")
	rec := &recordingReEmitter{}
	h := &WorkspaceHandler{}
	h.SetWakeReEmitter(rec.emit)
	ctx := context.Background()

	key := "restart-context:" + ws
	seedAgedWakeIntent(t, conn, ws, key, "restart-context", 1, "delivered", 30*time.Minute, 0)

	// Run more passes than the cap; between passes clear last_redriven_at so the
	// throttle never blocks (simulating redriveMinInterval elapsing each time).
	for i := 0; i < redriveMaxAttempts+3; i++ {
		if _, err := h.ReDriveStuckWakes(ctx, ws); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
		if _, err := conn.ExecContext(ctx,
			`UPDATE wake_intents SET last_redriven_at = NULL WHERE workspace_id = $1 AND status <> 'dropped'`, ws); err != nil {
			t.Fatalf("reset throttle: %v", err)
		}
	}

	if rec.count() != redriveMaxAttempts {
		t.Errorf("re-emitted %d times, want exactly redriveMaxAttempts=%d (bounded)", rec.count(), redriveMaxAttempts)
	}
	if attempts, status := readWakeRedrive(t, conn, ws, key); status != "dropped" || attempts != redriveMaxAttempts {
		t.Errorf("after cap: attempts=%d status=%q, want attempts=%d status=dropped", attempts, status, redriveMaxAttempts)
	}

	// One more pass now that it is dropped: terminal, never selected again.
	before := rec.count()
	if _, err := h.ReDriveStuckWakes(ctx, ws); err != nil {
		t.Fatalf("post-drop pass: %v", err)
	}
	if rec.count() != before {
		t.Errorf("a dropped intent was re-emitted again (calls %d→%d); dropped must be terminal", before, rec.count())
	}
}

// TestIntegration_WakeRedrive_SettledNeverReDriven proves a SETTLED intent is
// never selected (the non-terminal status filter excludes it), and a too-YOUNG
// intent is never selected (the age gate excludes it) — so neither is ever
// re-emitted or has its attempt counter touched.
func TestIntegration_WakeRedrive_SettledNeverReDriven(t *testing.T) {
	conn := integrationDB_WakeLifecycle(t)
	ws := seedWakeWorkspace(t, conn, "test-wake-redrive-settled")
	rec := &recordingReEmitter{}
	h := &WorkspaceHandler{}
	h.SetWakeReEmitter(rec.emit)
	ctx := context.Background()

	settledKey := "lifecycle:" + ws + ":settled"
	youngKey := "lifecycle:" + ws + ":young"
	seedAgedWakeIntent(t, conn, ws, settledKey, "lifecycle", 1, "settled", 30*time.Minute, 0) // terminal → excluded
	seedAgedWakeIntent(t, conn, ws, youngKey, "lifecycle", 2, "delivered", 1*time.Minute, 0)  // too young → excluded

	res, err := h.ReDriveStuckWakes(ctx, ws)
	if err != nil {
		t.Fatalf("ReDriveStuckWakes: %v", err)
	}
	if res.Redriven != 0 || res.Dropped != 0 {
		t.Errorf("result = %+v, want zero (settled excluded by status, young excluded by age)", res)
	}
	if rec.count() != 0 {
		t.Errorf("re-emit fired %d times; neither a settled nor a too-young intent may be re-driven", rec.count())
	}
	if a, s := readWakeRedrive(t, conn, ws, settledKey); a != 0 || s != "settled" {
		t.Errorf("settled intent mutated: attempts=%d status=%q, want attempts=0 status=settled", a, s)
	}
	if a, s := readWakeRedrive(t, conn, ws, youngKey); a != 0 || s != "delivered" {
		t.Errorf("young intent mutated: attempts=%d status=%q, want attempts=0 status=delivered", a, s)
	}
}
