//go:build integration
// +build integration

// stall_watchdog_wake_integration_test.go — real-Postgres proof that the stall
// watchdog's live desired-generation bump point (PR-C) behaves correctly through
// the wake owner: a fired stall probe mints exactly one wake_intent (kind=stall)
// and bumps workspaces.desired_generation, and a duplicate probe within the same
// dedup window does NOT double-bump. This is the emitter-shaped counterpart to
// wake_lifecycle_integration_test.go (which proves the owner in isolation): it
// pins the exact (kind, seed) the stall probe hands to DecideWake.
//
// Run (CI: Handlers Postgres Integration job):
//
//	INTEGRATION_DB_URL="postgres://postgres:test@localhost:55432/molecule?sslmode=disable" \
//	  go test -tags=integration -run TestIntegration_StallWake ./internal/handlers/

package handlers

import (
	"context"
	"strconv"
	"testing"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
)

// wakeIntentKind reads the kind stamped on a wake_intents row (proves the stall
// probe minted a STALL intent, not some other kind).
func wakeIntentKind(t *testing.T, workspaceID, key string) string {
	t.Helper()
	var kind string
	if err := db.DB.QueryRowContext(context.Background(),
		`SELECT kind FROM wake_intents WHERE workspace_id = $1 AND idempotency_key = $2`,
		workspaceID, key).Scan(&kind); err != nil {
		t.Fatalf("wakeIntentKind %q: %v", key, err)
	}
	return kind
}

func TestIntegration_StallWake_BumpsOnceAndDedups(t *testing.T) {
	conn := integrationDB_WakeLifecycle(t)
	ws := seedWakeWorkspace(t, conn, "test-wake-stall")
	h := &WorkspaceHandler{}
	ctx := context.Background()

	// The exact seed the stall probe uses: the truncated-hour bucket, as a string.
	bucket := time.Now().Truncate(time.Hour).Unix()
	seed := strconv.FormatInt(bucket, 10)

	// First probe of the window → fires, bumps desired 0 → 1, mints one intent.
	first, err := h.DecideWake(ctx, ws, WakeStall, seed)
	if err != nil {
		t.Fatalf("first DecideWake(WakeStall): %v", err)
	}
	if !first.Fire || first.Generation != 1 {
		t.Fatalf("first stall decision = %+v, want Fire=true Generation=1", first)
	}
	if cur, _ := currentDesiredGeneration(ctx, ws); cur != 1 {
		t.Errorf("desired_generation after first probe = %d, want 1", cur)
	}
	if c := countWakeIntents(t, conn, ws); c != 1 {
		t.Errorf("wake_intents rows after first probe = %d, want 1", c)
	}
	if k := wakeIntentKind(t, ws, first.IdempotencyKey); k != string(WakeStall) {
		t.Errorf("minted intent kind = %q, want %q", k, WakeStall)
	}

	// Duplicate probe in the SAME window → no fire, no double-bump, no new row.
	dup, err := h.DecideWake(ctx, ws, WakeStall, seed)
	if err != nil {
		t.Fatalf("duplicate DecideWake(WakeStall): %v", err)
	}
	if dup.Fire {
		t.Errorf("duplicate stall probe fired; want Fire=false")
	}
	if cur, _ := currentDesiredGeneration(ctx, ws); cur != 1 {
		t.Errorf("desired_generation after duplicate = %d, want 1 (a duplicate must NOT bump)", cur)
	}
	if c := countWakeIntents(t, conn, ws); c != 1 {
		t.Errorf("wake_intents rows after duplicate = %d, want 1", c)
	}

	// A probe in the NEXT window (distinct seed) is a fresh occurrence → fires
	// again, bumping desired 1 → 2.
	next, err := h.DecideWake(ctx, ws, WakeStall, strconv.FormatInt(bucket+3600, 10))
	if err != nil {
		t.Fatalf("next-window DecideWake(WakeStall): %v", err)
	}
	if !next.Fire || next.Generation != 2 {
		t.Fatalf("next-window stall decision = %+v, want Fire=true Generation=2", next)
	}
	if c := countWakeIntents(t, conn, ws); c != 2 {
		t.Errorf("wake_intents rows after next window = %d, want 2", c)
	}
}
