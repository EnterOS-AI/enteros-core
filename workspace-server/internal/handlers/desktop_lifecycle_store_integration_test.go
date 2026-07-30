package handlers

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

// Real-Postgres integration test for DesktopLifecycleStore. Runs only when
// INTEGRATION_DB_URL is set (matches the postgres_replay_integration_test
// convention). Verifies the store's SQL against a live DB — the behavior that
// compile+vet cannot prove.
func TestDesktopLifecycleStore_Integration(t *testing.T) {
	url := os.Getenv("INTEGRATION_DB_URL")
	if url == "" {
		t.Skip("INTEGRATION_DB_URL not set; skipping DesktopLifecycleStore real-PG test")
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
	// Fresh schema with just what the store touches.
	stmts := []string{
		`DROP SCHEMA public CASCADE`,
		`CREATE SCHEMA public`,
		`CREATE TABLE workspaces (id uuid PRIMARY KEY)`,
		`CREATE TABLE workspace_display_control_locks (
			workspace_id uuid PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
			controller text NOT NULL CHECK (controller IN ('user','agent')),
			controlled_by text NOT NULL,
			expires_at timestamptz NOT NULL,
			created_at timestamptz NOT NULL DEFAULT now(),
			updated_at timestamptz NOT NULL DEFAULT now())`,
		`CREATE TABLE workspace_desktop_lifecycle (
			workspace_id uuid PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
			state text NOT NULL DEFAULT 'stopped' CHECK (state IN ('stopped','starting','running','stopping')),
			last_agent_activity_at timestamptz,
			last_vnc_input_at timestamptz,
			vnc_connections int NOT NULL DEFAULT 0 CHECK (vnc_connections >= 0),
			lease_held boolean NOT NULL DEFAULT false,
			lease_expires_at timestamptz,
			profile_volume text,
			sidecar_address text,
			created_at timestamptz NOT NULL DEFAULT now(),
			updated_at timestamptz NOT NULL DEFAULT now())`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatalf("setup failed: %v\n%s", err, s)
		}
	}
	const ws = "11111111-1111-1111-1111-111111111111"
	if _, err := db.ExecContext(ctx, `INSERT INTO workspaces(id) VALUES ($1)`, ws); err != nil {
		t.Fatal(err)
	}

	store := NewDesktopLifecycleStore(db)

	// RecordAgentActivity upserts and sets last_agent_activity_at.
	if err := store.RecordAgentActivity(ctx, ws); err != nil {
		t.Fatalf("RecordAgentActivity: %v", err)
	}
	act, err := store.LoadActivity(ctx, ws)
	if err != nil {
		t.Fatalf("LoadActivity: %v", err)
	}
	if act.LastAgentActivity.IsZero() {
		t.Fatal("expected last_agent_activity to be set after RecordAgentActivity")
	}

	// AgentHoldsControl: false with no lock, true with an unexpired agent lock.
	if held, err := store.AgentHoldsControl(ctx, ws); err != nil || held {
		t.Fatalf("no lock -> (held=%v, err=%v), want (false, nil)", held, err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO workspace_display_control_locks(workspace_id, controller, controlled_by, expires_at) VALUES ($1,'agent','agent-x', now()+interval '5 minutes')`, ws); err != nil {
		t.Fatal(err)
	}
	if held, err := store.AgentHoldsControl(ctx, ws); err != nil || !held {
		t.Fatalf("agent lock -> (held=%v, err=%v), want (true, nil)", held, err)
	}
	// An EXPIRED agent lock must not count as held.
	if _, err := db.ExecContext(ctx, `UPDATE workspace_display_control_locks SET expires_at = now()-interval '1 minute' WHERE workspace_id=$1`, ws); err != nil {
		t.Fatal(err)
	}
	if held, _ := store.AgentHoldsControl(ctx, ws); held {
		t.Fatal("expired agent lock must not be held")
	}

	// SetState running -> listed by RunningDesktopWorkspaceIDs.
	if err := store.SetState(ctx, ws, "running", "wsdesk-x:6070"); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	ids, err := store.RunningDesktopWorkspaceIDs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, id := range ids {
		if id == ws {
			found = true
		}
	}
	if !found {
		t.Fatalf("running workspace not listed by RunningDesktopWorkspaceIDs: %v", ids)
	}
	// stopped -> not listed.
	if err := store.SetState(ctx, ws, "stopped", ""); err != nil {
		t.Fatal(err)
	}
	ids, _ = store.RunningDesktopWorkspaceIDs(ctx)
	for _, id := range ids {
		if id == ws {
			t.Fatal("stopped desktop must not be listed as running")
		}
	}

	// SetVNCPresence round-trips into LoadActivity.
	now := time.Now()
	if err := store.SetVNCPresence(ctx, ws, 2, now); err != nil {
		t.Fatalf("SetVNCPresence: %v", err)
	}
	act, _ = store.LoadActivity(ctx, ws)
	if act.VNCConnections != 2 {
		t.Fatalf("vnc_connections = %d, want 2", act.VNCConnections)
	}
	if act.LastVNCInput.IsZero() {
		t.Fatal("expected last_vnc_input set")
	}
}
