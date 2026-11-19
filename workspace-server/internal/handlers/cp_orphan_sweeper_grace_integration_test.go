//go:build integration
// +build integration

// cp_orphan_sweeper_grace_integration_test.go — real-Postgres coverage of the
// control-plane orphan sweeper's grace for freshly removed workspaces. The
// sqlmock test in internal/registry pins the query's text; this one runs the
// query, so it is the predicate itself that keeps a workspace a delete has just
// marked removed away from the sweeper.
//
// Run as light_schema_integration_test.go says. NOT SAFE FOR t.Parallel().

package handlers

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/registry"
)

type recordingCPReaper struct {
	mu      sync.Mutex
	stopped []string
	seen    chan struct{}
}

func (r *recordingCPReaper) Stop(_ context.Context, workspaceID string) error {
	r.mu.Lock()
	r.stopped = append(r.stopped, workspaceID)
	r.mu.Unlock()
	select {
	case r.seen <- struct{}{}:
	default:
	}
	return nil
}

func TestIntegration_CPOrphanSweeper_LeavesAFreshlyRemovedWorkspaceToItsDelete(t *testing.T) {
	conn := integrationDB(t)
	ctx := context.Background()
	if _, err := conn.ExecContext(ctx, `DELETE FROM workspaces WHERE name LIKE 'test-sweep-grace-%'`); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	t.Cleanup(func() {
		_, _ = conn.ExecContext(context.Background(), `DELETE FROM workspaces WHERE name LIKE 'test-sweep-grace-%'`)
	})

	// One workspace a delete marked removed just now (its own stop has not run
	// yet), one whose inline stop failed long ago and left its instance behind.
	fresh := seedWorkspace(t, conn, "test-sweep-grace-fresh")
	leaked := seedWorkspace(t, conn, "test-sweep-grace-leaked")
	if _, err := conn.ExecContext(ctx,
		`UPDATE workspaces SET status = 'removed', instance_id = 'i-fresh', updated_at = now() WHERE id = $1`, fresh); err != nil {
		t.Fatalf("mark fresh removed: %v", err)
	}
	if _, err := conn.ExecContext(ctx,
		`UPDATE workspaces SET status = 'removed', instance_id = 'i-leaked', updated_at = now() - interval '1 hour' WHERE id = $1`, leaked); err != nil {
		t.Fatalf("mark leaked removed: %v", err)
	}

	reaper := &recordingCPReaper{seen: make(chan struct{}, 8)}
	sweepCtx, stop := context.WithCancel(ctx)
	finished := make(chan struct{})
	go func() { defer close(finished); registry.StartCPOrphanSweeper(sweepCtx, reaper) }()
	select {
	case <-reaper.seen:
	case <-time.After(20 * time.Second):
		stop()
		<-finished
		t.Fatal("the sweeper stopped nothing in 20 s: the leaked workspace should have been reaped on its first pass")
	}
	// The first pass runs before the ticker; give its UPDATE a moment, then end the loop.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var id sql.NullString
		if err := conn.QueryRowContext(ctx, `SELECT instance_id FROM workspaces WHERE id = $1`, leaked).Scan(&id); err == nil && !id.Valid {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	stop()
	<-finished

	reaper.mu.Lock()
	stopped := append([]string(nil), reaper.stopped...)
	reaper.mu.Unlock()
	for _, id := range stopped {
		if id == fresh {
			t.Errorf("the sweeper stopped %s, removed moments ago: it pre-empted that workspace's own delete, whose erase now finds no instance id", fresh)
		}
	}
	if len(stopped) != 1 || stopped[0] != leaked {
		t.Errorf("stopped = %v, want only the long-removed workspace %s", stopped, leaked)
	}

	var freshInstance, leakedInstance sql.NullString
	if err := conn.QueryRowContext(ctx, `SELECT instance_id FROM workspaces WHERE id = $1`, fresh).Scan(&freshInstance); err != nil {
		t.Fatalf("read fresh: %v", err)
	}
	if err := conn.QueryRowContext(ctx, `SELECT instance_id FROM workspaces WHERE id = $1`, leaked).Scan(&leakedInstance); err != nil {
		t.Fatalf("read leaked: %v", err)
	}
	if freshInstance.String != "i-fresh" {
		t.Errorf("the freshly removed workspace's instance_id = %q, want it kept (i-fresh) for its delete's erase", freshInstance.String)
	}
	if leakedInstance.Valid {
		t.Errorf("the long-removed workspace's instance_id = %q, want it cleared after the sweep", leakedInstance.String)
	}
}
