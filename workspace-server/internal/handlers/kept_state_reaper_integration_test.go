//go:build integration
// +build integration

// kept_state_reaper_integration_test.go — real-Postgres coverage of
// molecule-core#5193: a plain delete remembers the instance it stopped, and the
// kept-state reaper erases that state once the retention has passed, never for
// a workspace that is not removed. The sqlmock tests pin the statements' text;
// this one runs them against the migrated schema.
//
// Run as light_schema_integration_test.go says. NOT SAFE FOR t.Parallel().

package handlers

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/registry"
)

type recordingKeptStateEraser struct {
	mu     sync.Mutex
	erased map[string]string
	seen   chan struct{}
}

func (r *recordingKeptStateEraser) EraseKeptState(_ context.Context, workspaceID, instanceID string) error {
	r.mu.Lock()
	r.erased[workspaceID] = instanceID
	r.mu.Unlock()
	select {
	case r.seen <- struct{}{}:
	default:
	}
	return nil
}

func readKeptState(t *testing.T, conn *sql.DB, id string) (instance sql.NullString, kept sql.NullString, since sql.NullTime) {
	t.Helper()
	if err := conn.QueryRowContext(context.Background(),
		`SELECT instance_id, kept_state_instance_id, kept_state_since FROM workspaces WHERE id = $1`, id,
	).Scan(&instance, &kept, &since); err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	return
}

func TestIntegration_KeptState_PlainDeleteIsErasedAfterTheRetention(t *testing.T) {
	conn := integrationDB(t)
	ctx := context.Background()
	const prefix = "test-kept-state-%"
	if _, err := conn.ExecContext(ctx, `DELETE FROM workspaces WHERE name LIKE $1`, prefix); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	t.Cleanup(func() {
		_, _ = conn.ExecContext(context.Background(), `DELETE FROM workspaces WHERE name LIKE $1`, prefix)
	})

	kept := seedWorkspace(t, conn, "test-kept-state-plain")
	erased := seedWorkspace(t, conn, "test-kept-state-erased")
	revived := seedWorkspace(t, conn, "test-kept-state-revived")
	recent := seedWorkspace(t, conn, "test-kept-state-recent")
	for id, inst := range map[string]string{kept: "i-kept", erased: "i-erased", revived: "i-revived", recent: "i-recent"} {
		if _, err := conn.ExecContext(ctx,
			`UPDATE workspaces SET status = 'removed', instance_id = $2 WHERE id = $1`, id, inst); err != nil {
			t.Fatalf("mark %s removed: %v", id, err)
		}
	}

	// The delete path's two writes, as CascadeDelete issues them.
	for _, id := range []string{kept, revived, recent} {
		if _, err := conn.ExecContext(ctx, db.ClearInstanceIDKeepingStateSQL, id); err != nil {
			t.Fatalf("keep-state clear %s: %v", id, err)
		}
	}
	if _, err := conn.ExecContext(ctx, db.ClearInstanceIDStateErasedSQL, erased); err != nil {
		t.Fatalf("erased clear: %v", err)
	}

	inst, k, since := readKeptState(t, conn, kept)
	if inst.Valid || k.String != "i-kept" || !since.Valid {
		t.Fatalf("after a plain delete: instance_id=%v kept=%v since=%v, want NULL / i-kept / set", inst, k, since)
	}
	// A second keep-state clear (the orphan sweeper after the delete) finds no
	// instance id and must not forget the kept one or restart its clock.
	if _, err := conn.ExecContext(ctx, db.ClearInstanceIDKeepingStateSQL, kept); err != nil {
		t.Fatalf("second keep-state clear: %v", err)
	}
	_, k2, since2 := readKeptState(t, conn, kept)
	if k2.String != "i-kept" || !since2.Valid || !since2.Time.Equal(since.Time) {
		t.Fatalf("a repeated clear changed the kept state: kept=%v since=%v (was %v)", k2, since2, since)
	}
	if _, k, s := readKeptState(t, conn, erased); k.Valid || s.Valid {
		t.Fatalf("after an erasing delete: kept=%v since=%v, want both NULL", k, s)
	}

	// Past the retention: kept and revived. The revived one is live again.
	if _, err := conn.ExecContext(ctx,
		`UPDATE workspaces SET kept_state_since = now() - interval '25 hours' WHERE id = ANY($1::uuid[])`,
		"{"+kept+","+revived+"}"); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `UPDATE workspaces SET status = 'online' WHERE id = $1`, revived); err != nil {
		t.Fatalf("revive: %v", err)
	}

	eraser := &recordingKeptStateEraser{erased: map[string]string{}, seen: make(chan struct{}, 8)}
	loopCtx, stop := context.WithCancel(ctx)
	finished := make(chan struct{})
	go func() { defer close(finished); registry.StartCPKeptStateReaper(loopCtx, eraser) }()
	select {
	case <-eraser.seen:
	case <-time.After(20 * time.Second):
		stop()
		<-finished
		t.Fatal("the reaper erased nothing in 20 s")
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, k, _ := readKeptState(t, conn, kept); !k.Valid {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	stop()
	<-finished

	eraser.mu.Lock()
	got := map[string]string{}
	for k, v := range eraser.erased {
		got[k] = v
	}
	eraser.mu.Unlock()
	if len(got) != 1 || got[kept] != "i-kept" {
		t.Errorf("erased = %v, want only %s with i-kept (not the revived %s, not the recent %s)", got, kept, revived, recent)
	}
	if _, k, s := readKeptState(t, conn, kept); k.Valid || s.Valid {
		t.Errorf("after the erase: kept=%v since=%v, want both NULL", k, s)
	}
	if _, k, _ := readKeptState(t, conn, recent); k.String != "i-recent" {
		t.Errorf("the recently deleted workspace's kept state = %v, want it kept until the retention passes", k)
	}
}
