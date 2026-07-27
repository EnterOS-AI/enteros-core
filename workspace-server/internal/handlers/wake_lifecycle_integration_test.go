//go:build integration
// +build integration

// wake_lifecycle_integration_test.go — real-Postgres proof of the wake
// desired-state owner. sqlmock (wake_lifecycle_test.go) pins the SQL shapes but
// CANNOT model what actually matters here: the workspaces row-lock that makes
// the in-SQL `+1 RETURNING` gap-free under concurrent DecideWake, and the
// ON CONFLICT dedup that must NOT bump the counter. Those are behaviours of the
// live engine, so they are proven end-to-end against a real database here.
//
// Run (CI does this in the Handlers Postgres Integration job; the -race variant
// is a local proof — the CI job does not pass -race):
//
//	INTEGRATION_DB_URL="postgres://postgres:test@localhost:55432/molecule?sslmode=disable" \
//	  go test -tags=integration -race -run TestIntegration_WakeLifecycle ./internal/handlers/
//
// NOT SAFE FOR t.Parallel() at the package level — each test owns its own
// freshly-seeded workspace rows (unique ids), but they share the global db.DB.

package handlers

import (
	"context"
	"database/sql"
	"os"
	"sort"
	"strconv"
	"sync"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	_ "github.com/lib/pq"
)

// integrationDB_WakeLifecycle connects the package-global db.DB to the real test
// Postgres and cleans up any leftover test-wake workspaces (cascades to
// wake_intents). Mirrors integrationDB_PhantomBusy.
func integrationDB_WakeLifecycle(t *testing.T) *sql.DB {
	t.Helper()
	url := os.Getenv("INTEGRATION_DB_URL")
	if url == "" {
		t.Fatal("INTEGRATION_DB_URL not set; failing (local devs: see file header)")
	}
	conn, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := conn.PingContext(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
	// wake_intents rows cascade when the owning workspace is deleted.
	if _, err := conn.ExecContext(context.Background(),
		`DELETE FROM workspaces WHERE name LIKE 'test-wake-%'`); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	prev := db.DB
	db.DB = conn
	t.Cleanup(func() {
		conn.ExecContext(context.Background(), `DELETE FROM workspaces WHERE name LIKE 'test-wake-%'`)
		db.DB = prev
		conn.Close()
	})
	return conn
}

// seedWakeWorkspace inserts a workspace and returns its id. desired_generation
// and observed_generation default to 0 (migration 20260726120000).
func seedWakeWorkspace(t *testing.T, conn *sql.DB, name string) string {
	t.Helper()
	var id string
	if err := conn.QueryRowContext(context.Background(),
		`INSERT INTO workspaces (id, name, status) VALUES (gen_random_uuid(), $1, 'online') RETURNING id`,
		name).Scan(&id); err != nil {
		t.Fatalf("seedWakeWorkspace %q: %v", name, err)
	}
	return id
}

// insertWakeIntent seeds a wake_intents row directly at an explicit generation
// and status — used to set up the convergence/transition tests without going
// through DecideWake.
func insertWakeIntent(t *testing.T, conn *sql.DB, workspaceID, key string, generation int64, status string) {
	t.Helper()
	if _, err := conn.ExecContext(context.Background(),
		`INSERT INTO wake_intents (workspace_id, kind, idempotency_key, generation, status)
		 VALUES ($1, 'lifecycle', $2, $3, $4)`,
		workspaceID, key, generation, status); err != nil {
		t.Fatalf("insertWakeIntent %q: %v", key, err)
	}
}

func wakeIntentStatus(t *testing.T, conn *sql.DB, workspaceID, key string) (status string, settledAtSet bool) {
	t.Helper()
	var settledAt sql.NullTime
	if err := conn.QueryRowContext(context.Background(),
		`SELECT status, settled_at FROM wake_intents WHERE workspace_id = $1 AND idempotency_key = $2`,
		workspaceID, key).Scan(&status, &settledAt); err != nil {
		t.Fatalf("wakeIntentStatus %q: %v", key, err)
	}
	return status, settledAt.Valid
}

func countWakeIntents(t *testing.T, conn *sql.DB, workspaceID string) int {
	t.Helper()
	var n int
	if err := conn.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM wake_intents WHERE workspace_id = $1`, workspaceID).Scan(&n); err != nil {
		t.Fatalf("countWakeIntents: %v", err)
	}
	return n
}

// TestIntegration_WakeLifecycle_MonotonicGapFreeConcurrent fires N concurrent
// DecideWake calls (each a DISTINCT recurring wake, so each must fire) and
// asserts the desired_generation advanced exactly 1..N with no gaps and no
// duplicates. The gap-free property is enforced by the workspaces row lock the
// in-SQL `+1 RETURNING` takes; run with `-race` to also prove the harness has no
// data race.
func TestIntegration_WakeLifecycle_MonotonicGapFreeConcurrent(t *testing.T) {
	conn := integrationDB_WakeLifecycle(t)
	ws := seedWakeWorkspace(t, conn, "test-wake-monotonic")
	h := &WorkspaceHandler{}

	const n = 25
	gens := make([]int64, n)
	fired := make([]bool, n)
	errs := make([]error, n)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // maximize contention: release all goroutines at once
			// DISTINCT seed per goroutine → each is a unique wake that must fire.
			dec, err := h.DecideWake(context.Background(), ws, WakeIdle, seedFor(i))
			errs[i] = err
			fired[i] = dec.Fire
			gens[i] = dec.Generation
		}(i)
	}
	close(start)
	wg.Wait()

	got := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("DecideWake[%d]: %v", i, errs[i])
		}
		if !fired[i] {
			t.Fatalf("DecideWake[%d] did not fire — every distinct wake must fire", i)
		}
		got = append(got, gens[i])
	}
	sort.Slice(got, func(a, b int) bool { return got[a] < got[b] })
	for i := 0; i < n; i++ {
		if want := int64(i + 1); got[i] != want {
			t.Fatalf("generation set has a gap/dup: sorted[%d] = %d, want %d (full: %v)", i, got[i], want, got)
		}
	}

	cur, err := currentDesiredGeneration(context.Background(), ws)
	if err != nil {
		t.Fatalf("currentDesiredGeneration: %v", err)
	}
	if cur != n {
		t.Errorf("final desired_generation = %d, want %d", cur, n)
	}
	if c := countWakeIntents(t, conn, ws); c != n {
		t.Errorf("wake_intents rows = %d, want %d", c, n)
	}
}

// TestIntegration_WakeLifecycle_DuplicateNoBump proves a duplicate wake does not
// fire, does not bump the counter, and leaves exactly one ledger row.
func TestIntegration_WakeLifecycle_DuplicateNoBump(t *testing.T) {
	conn := integrationDB_WakeLifecycle(t)
	ws := seedWakeWorkspace(t, conn, "test-wake-dup")
	h := &WorkspaceHandler{}
	ctx := context.Background()

	first, err := h.DecideWake(ctx, ws, WakeFirstBootGreet, "")
	if err != nil {
		t.Fatalf("first DecideWake: %v", err)
	}
	if !first.Fire || first.Generation != 1 {
		t.Fatalf("first decision = %+v, want Fire=true Generation=1", first)
	}

	// Same kind (greet is once-per-box) → duplicate key → no fire, no bump.
	second, err := h.DecideWake(ctx, ws, WakeFirstBootGreet, "")
	if err != nil {
		t.Fatalf("second DecideWake: %v", err)
	}
	if second.Fire {
		t.Errorf("duplicate decision fired; want Fire=false")
	}
	if second.Generation != 0 {
		t.Errorf("duplicate Generation = %d, want 0", second.Generation)
	}

	if cur, _ := currentDesiredGeneration(ctx, ws); cur != 1 {
		t.Errorf("desired_generation after duplicate = %d, want 1 (a duplicate must NOT bump)", cur)
	}
	if c := countWakeIntents(t, conn, ws); c != 1 {
		t.Errorf("wake_intents rows = %d, want 1 (duplicate must not mint a second row)", c)
	}
}

// TestIntegration_WakeLifecycle_MarkSettled proves MarkWakeSettled flips
// DELIVERED intents at/below observed to settled (stamping settled_at) and
// leaves both pending-below-observed and delivered-above-observed alone.
func TestIntegration_WakeLifecycle_MarkSettled(t *testing.T) {
	conn := integrationDB_WakeLifecycle(t)
	ws := seedWakeWorkspace(t, conn, "test-wake-settle")
	h := &WorkspaceHandler{}
	ctx := context.Background()

	insertWakeIntent(t, conn, ws, "delivered-below", 1, "delivered")
	insertWakeIntent(t, conn, ws, "pending-below", 1, "pending")
	insertWakeIntent(t, conn, ws, "delivered-above", 3, "delivered")

	if err := h.MarkWakeSettled(ctx, ws, 2); err != nil {
		t.Fatalf("MarkWakeSettled: %v", err)
	}

	if s, settledAt := wakeIntentStatus(t, conn, ws, "delivered-below"); s != "settled" || !settledAt {
		t.Errorf("delivered-below: status=%q settled_at_set=%v, want settled + settled_at set", s, settledAt)
	}
	if s, _ := wakeIntentStatus(t, conn, ws, "pending-below"); s != "pending" {
		t.Errorf("pending-below: status=%q, want pending (a not-yet-delivered wake is not converged)", s)
	}
	if s, settledAt := wakeIntentStatus(t, conn, ws, "delivered-above"); s != "delivered" || settledAt {
		t.Errorf("delivered-above: status=%q settled_at_set=%v, want delivered untouched (generation > observed)", s, settledAt)
	}
}

// TestIntegration_WakeLifecycle_MarkDelivered proves the delivered transition:
// pending → delivered, dispatched → delivered, and already-settled is a no-op.
func TestIntegration_WakeLifecycle_MarkDelivered(t *testing.T) {
	conn := integrationDB_WakeLifecycle(t)
	ws := seedWakeWorkspace(t, conn, "test-wake-delivered")
	h := &WorkspaceHandler{}
	ctx := context.Background()

	insertWakeIntent(t, conn, ws, "was-pending", 1, "pending")
	insertWakeIntent(t, conn, ws, "was-dispatched", 2, "dispatched")
	insertWakeIntent(t, conn, ws, "was-settled", 3, "settled")

	for _, key := range []string{"was-pending", "was-dispatched", "was-settled"} {
		if err := h.MarkWakeDelivered(ctx, ws, key); err != nil {
			t.Fatalf("MarkWakeDelivered %q: %v", key, err)
		}
	}

	if s, _ := wakeIntentStatus(t, conn, ws, "was-pending"); s != "delivered" {
		t.Errorf("was-pending: status=%q, want delivered", s)
	}
	if s, _ := wakeIntentStatus(t, conn, ws, "was-dispatched"); s != "delivered" {
		t.Errorf("was-dispatched: status=%q, want delivered", s)
	}
	// A terminal 'settled' row is NOT in (pending,dispatched), so MarkWakeDelivered
	// must leave it settled — never resurrect a converged wake.
	if s, _ := wakeIntentStatus(t, conn, ws, "was-settled"); s != "settled" {
		t.Errorf("was-settled: status=%q, want settled (no-op — must not resurrect)", s)
	}
}

func seedFor(i int) string {
	// Stable distinct seed per goroutine index.
	return "occurrence-" + strconv.Itoa(i)
}
