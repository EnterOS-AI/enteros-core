//go:build integration
// +build integration

// wake_consolidation_integration_test.go — real-Postgres proof of the PR-D
// emitter consolidation (RFC #28) + the generalized idempotency key (RFC #27):
// the greeting, restart-context, and stall emitters all decide through the ONE
// wake-lifecycle owner (DecideWake), so three DISTINCT wake kinds minted in the
// same window each fire EXACTLY ONCE, each get their own generation + ledger row,
// and a duplicate decide of any kind is a durable no-op (no second row, no extra
// generation bump). It also pins the rule-2 cross-check that has_greeted never
// disagrees with the greet wake_intent — the greet-once CAS owner
// (claimGreetDelivery / has_greeted) and the once-per-box ledger key agree on a
// single delivery.
//
// Run (CI: Handlers Postgres Integration job):
//
//	INTEGRATION_DB_URL="postgres://postgres:test@localhost:55432/molecule?sslmode=disable" \
//	  go test -tags=integration -run TestIntegration_WakeConsolidation ./internal/handlers/
//
// Reuses the seed/count/status helpers from wake_lifecycle_integration_test.go
// and wakeIntentKind from stall_watchdog_wake_integration_test.go (same package).

package handlers

import (
	"context"
	"strconv"
	"testing"
	"time"
)

func TestIntegration_WakeConsolidation_CrossKindExactlyOnce(t *testing.T) {
	conn := integrationDB_WakeLifecycle(t)
	ws := seedWakeWorkspace(t, conn, "test-wake-crosskind")
	h := &WorkspaceHandler{}
	ctx := context.Background()

	// The exact stall dedup seed the stall probe uses: the truncated-hour bucket.
	stallSeed := strconv.FormatInt(time.Now().Truncate(time.Hour).Unix(), 10)

	// --- three DISTINCT kinds each fire once, bumping desired_generation 0→3 ---
	// Decided sequentially (no concurrency) so the generations are gap-free 1,2,3
	// in decide order — the single-decider owner is the only counter mutator.
	greet, err := h.DecideWake(ctx, ws, WakeFirstBootGreet, "")
	if err != nil {
		t.Fatalf("greet DecideWake: %v", err)
	}
	restart, err := h.DecideWake(ctx, ws, WakeRestartContext, "")
	if err != nil {
		t.Fatalf("restart DecideWake: %v", err)
	}
	stall, err := h.DecideWake(ctx, ws, WakeStall, stallSeed)
	if err != nil {
		t.Fatalf("stall DecideWake: %v", err)
	}

	type kindDecision struct {
		name string
		kind WakeKind
		dec  WakeDecision
		gen  int64
	}
	first := []kindDecision{
		{"greet", WakeFirstBootGreet, greet, 1},
		{"restart", WakeRestartContext, restart, 2},
		{"stall", WakeStall, stall, 3},
	}
	for _, kd := range first {
		if !kd.dec.Fire {
			t.Errorf("%s wake did not fire on first decide; want Fire=true", kd.name)
		}
		if kd.dec.Generation != kd.gen {
			t.Errorf("%s wake generation = %d, want %d (gap-free per distinct kind)", kd.name, kd.dec.Generation, kd.gen)
		}
		if k := wakeIntentKind(t, ws, kd.dec.IdempotencyKey); k != string(kd.kind) {
			t.Errorf("%s intent kind = %q, want %q", kd.name, k, kd.kind)
		}
	}
	// The three keys must be distinct — one identity per kind.
	if greet.IdempotencyKey == restart.IdempotencyKey ||
		restart.IdempotencyKey == stall.IdempotencyKey ||
		greet.IdempotencyKey == stall.IdempotencyKey {
		t.Errorf("wake keys collided across kinds: greet=%q restart=%q stall=%q",
			greet.IdempotencyKey, restart.IdempotencyKey, stall.IdempotencyKey)
	}
	if cur, _ := currentDesiredGeneration(ctx, ws); cur != 3 {
		t.Errorf("desired_generation = %d, want 3 (one bump per distinct kind)", cur)
	}
	if c := countWakeIntents(t, conn, ws); c != 3 {
		t.Errorf("wake_intents rows = %d, want 3", c)
	}

	// --- a duplicate decide of EACH kind in the SAME window is a no-op ---
	greetDup, err := h.DecideWake(ctx, ws, WakeFirstBootGreet, "")
	if err != nil {
		t.Fatalf("greet duplicate DecideWake: %v", err)
	}
	restartDup, err := h.DecideWake(ctx, ws, WakeRestartContext, "")
	if err != nil {
		t.Fatalf("restart duplicate DecideWake: %v", err)
	}
	stallDup, err := h.DecideWake(ctx, ws, WakeStall, stallSeed)
	if err != nil {
		t.Fatalf("stall duplicate DecideWake: %v", err)
	}
	dups := []kindDecision{
		{"greet", WakeFirstBootGreet, greetDup, 0},
		{"restart", WakeRestartContext, restartDup, 0},
		{"stall", WakeStall, stallDup, 0},
	}
	for _, kd := range dups {
		if kd.dec.Fire {
			t.Errorf("%s duplicate wake fired; want Fire=false (durable dedup)", kd.name)
		}
		if kd.dec.Generation != 0 {
			t.Errorf("%s duplicate Generation = %d, want 0", kd.name, kd.dec.Generation)
		}
	}
	// The duplicate keys must match the originals (same identity → same collision).
	if greetDup.IdempotencyKey != greet.IdempotencyKey ||
		restartDup.IdempotencyKey != restart.IdempotencyKey ||
		stallDup.IdempotencyKey != stall.IdempotencyKey {
		t.Errorf("duplicate decides derived a different key than the original — dedup identity is not stable")
	}
	if cur, _ := currentDesiredGeneration(ctx, ws); cur != 3 {
		t.Errorf("desired_generation after duplicates = %d, want 3 (a duplicate must NOT bump)", cur)
	}
	if c := countWakeIntents(t, conn, ws); c != 3 {
		t.Errorf("wake_intents rows after duplicates = %d, want 3 (no second row per kind)", c)
	}

	// --- has_greeted never disagrees with the greet wake_intent ---
	// Before any delivery: box not greeted, greet intent still pending.
	if greeted, _ := workspaceHasGreeted(ctx, ws); greeted {
		t.Errorf("has_greeted set before any greet delivery — disagrees with a pending greet intent")
	}
	if s, _ := wakeIntentStatus(t, conn, ws, greet.IdempotencyKey); s != "pending" {
		t.Errorf("greet intent status = %q before delivery, want pending", s)
	}

	// Greeter delivery path: claim the has_greeted CAS (the greet-once owner —
	// NOT the wake owner), then commit-on-delivery via MarkWakeDelivered. This is
	// exactly the FirstBootGreeter / deliverFirstBootGreeting sequence.
	won, err := claimGreetDelivery(ctx, ws)
	if err != nil {
		t.Fatalf("first greet claim: %v", err)
	}
	if !won {
		t.Fatal("first greet claim did not win on a fresh box — greet-once CAS broken")
	}
	if err := h.MarkWakeDelivered(ctx, ws, greet.IdempotencyKey); err != nil {
		t.Fatalf("mark greet delivered: %v", err)
	}

	// Now the marker is set AND the greet intent is delivered — they AGREE.
	if greeted, _ := workspaceHasGreeted(ctx, ws); !greeted {
		t.Errorf("has_greeted not set after greet delivery — disagrees with a delivered greet intent")
	}
	if s, _ := wakeIntentStatus(t, conn, ws, greet.IdempotencyKey); s != "delivered" {
		t.Errorf("greet intent status = %q after delivery, want delivered", s)
	}

	// A racing second greet delivery: the has_greeted CAS LOSES (marker already
	// true) so no second greeting fires — the greet-once owner and the once-per-box
	// ledger key agree that EXACTLY ONE greeting happened (the double-greet hole is
	// closed). A duplicate greet DecideWake likewise already no-fired above.
	wonAgain, err := claimGreetDelivery(ctx, ws)
	if err != nil {
		t.Fatalf("second greet claim: %v", err)
	}
	if wonAgain {
		t.Error("second greet claim WON — has_greeted disagreed with the delivered ledger (double-greet across wakes)")
	}

	// Deliver the restart + stall wakes too; the whole ledger is then consistent:
	// every minted intent is delivered, exactly once per kind.
	if err := h.MarkWakeDelivered(ctx, ws, restart.IdempotencyKey); err != nil {
		t.Fatalf("mark restart delivered: %v", err)
	}
	if err := h.MarkWakeDelivered(ctx, ws, stall.IdempotencyKey); err != nil {
		t.Fatalf("mark stall delivered: %v", err)
	}
	for _, kd := range first {
		if s, _ := wakeIntentStatus(t, conn, ws, kd.dec.IdempotencyKey); s != "delivered" {
			t.Errorf("%s intent status = %q after delivery, want delivered", kd.name, s)
		}
	}
	if c := countWakeIntents(t, conn, ws); c != 3 {
		t.Errorf("final wake_intents rows = %d, want 3 (exactly one per kind, cross-kind)", c)
	}
}
