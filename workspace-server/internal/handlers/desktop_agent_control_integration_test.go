package handlers

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/lib/pq"
)

// Real-PG test for AcquireAgentControl (reviewer B2): the agent must be able to
// take/refresh a control lease so /input works, while yielding to a human who
// holds control. Runs only when INTEGRATION_DB_URL is set.
func TestAcquireAgentControl_Integration(t *testing.T) {
	url := os.Getenv("INTEGRATION_DB_URL")
	if url == "" {
		t.Skip("INTEGRATION_DB_URL not set; skipping AcquireAgentControl real-PG test")
	}
	db, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	ctx := context.Background()
	for _, s := range []string{
		`DROP SCHEMA public CASCADE`, `CREATE SCHEMA public`,
		`CREATE TABLE workspaces (id uuid PRIMARY KEY)`,
		`CREATE TABLE workspace_display_control_locks (
			workspace_id uuid PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
			controller text NOT NULL CHECK (controller IN ('user','agent')),
			controlled_by text NOT NULL,
			expires_at timestamptz NOT NULL,
			created_at timestamptz NOT NULL DEFAULT now(),
			updated_at timestamptz NOT NULL DEFAULT now())`,
	} {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatalf("setup: %v\n%s", err, s)
		}
	}
	const ws = "22222222-2222-2222-2222-222222222222"
	if _, err := db.ExecContext(ctx, `INSERT INTO workspaces(id) VALUES ($1)`, ws); err != nil {
		t.Fatal(err)
	}
	store := NewDesktopLifecycleStore(db)

	// 1) Agent acquires on an empty lock (this is what makes /input work at all).
	if held, err := store.AcquireAgentControl(ctx, ws); err != nil || !held {
		t.Fatalf("agent acquire on empty: (held=%v, err=%v), want (true, nil)", held, err)
	}
	// 2) Agent refreshes its own lease.
	if held, err := store.AcquireAgentControl(ctx, ws); err != nil || !held {
		t.Fatalf("agent refresh: (held=%v, err=%v), want (true, nil)", held, err)
	}
	// 3) A human preempts (unexpired user lock) -> agent YIELDS.
	if _, err := db.ExecContext(ctx, `UPDATE workspace_display_control_locks SET controller='user', controlled_by='user-a', expires_at=now()+interval '5 minutes' WHERE workspace_id=$1`, ws); err != nil {
		t.Fatal(err)
	}
	if held, err := store.AcquireAgentControl(ctx, ws); err != nil || held {
		t.Fatalf("agent acquire under human lock: (held=%v, err=%v), want (false, nil) — must yield", held, err)
	}
	// Confirm the human still holds it (agent didn't clobber).
	var controller, by string
	if err := db.QueryRowContext(ctx, `SELECT controller, controlled_by FROM workspace_display_control_locks WHERE workspace_id=$1`, ws).Scan(&controller, &by); err != nil {
		t.Fatal(err)
	}
	if controller != "user" || by != "user-a" {
		t.Fatalf("human lock clobbered by agent acquire: (%q,%q)", controller, by)
	}
	// 4) Human lock expires -> agent reclaims control.
	if _, err := db.ExecContext(ctx, `UPDATE workspace_display_control_locks SET expires_at=now()-interval '1 minute' WHERE workspace_id=$1`, ws); err != nil {
		t.Fatal(err)
	}
	if held, err := store.AcquireAgentControl(ctx, ws); err != nil || !held {
		t.Fatalf("agent reclaim after human lease lapsed: (held=%v, err=%v), want (true, nil)", held, err)
	}
	if c, _ := store.AgentHoldsControl(ctx, ws); !c {
		t.Fatal("AgentHoldsControl should be true after reclaim")
	}
}
