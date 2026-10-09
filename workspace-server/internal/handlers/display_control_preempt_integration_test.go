package handlers

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/lib/pq"
)

// Real-Postgres test for the human-preempts-agent arbitration policy embedded in
// acquireDisplayControlQuery (checklist line 269). Runs only when
// INTEGRATION_DB_URL is set. Exercises the EXACT statement the handler runs, so
// it proves the WHERE-clause policy against a live DB (which compile+vet cannot).
func TestAcquireDisplayControl_HumanPreemptsAgent_Integration(t *testing.T) {
	url := os.Getenv("INTEGRATION_DB_URL")
	if url == "" {
		t.Skip("INTEGRATION_DB_URL not set; skipping display-control preemption real-PG test")
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
	} {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatalf("setup failed: %v\n%s", err, s)
		}
	}
	const ws = "11111111-1111-1111-1111-111111111111"
	if _, err := db.ExecContext(ctx, `INSERT INTO workspaces(id) VALUES ($1)`, ws); err != nil {
		t.Fatal(err)
	}

	acquire := func(controller, by string, ttl int) (gotController, gotBy string, err error) {
		err = db.QueryRowContext(ctx, acquireDisplayControlQuery, ws, controller, by, ttl).
			Scan(&gotController, &gotBy, new(any))
		return
	}

	// 1) Agent takes control on an empty lock.
	if c, b, err := acquire("agent", "agent-x", 300); err != nil || c != "agent" || b != "agent-x" {
		t.Fatalf("agent initial acquire: (%q,%q,%v), want (agent, agent-x, nil)", c, b, err)
	}

	// 2) A USER preempts the active agent lock — NO admin token, NO force.
	c, b, err := acquire("user", "user-a", 300)
	if err != nil {
		t.Fatalf("user preempt of agent lock must succeed, got err %v (409-equivalent)", err)
	}
	if c != "user" || b != "user-a" {
		t.Fatalf("after preempt: controller=%q controlled_by=%q, want (user, user-a)", c, b)
	}

	// 3) A DIFFERENT user CANNOT steal user-a's active lock (that path must fail;
	//    stealing another human's control still needs an explicit force release).
	if _, _, err := acquire("user", "user-b", 300); err != sql.ErrNoRows {
		t.Fatalf("user-b stealing user-a's active lock: err=%v, want sql.ErrNoRows (blocked)", err)
	}
	// Confirm user-a still holds it.
	var stillController, stillBy string
	if err := db.QueryRowContext(ctx, `SELECT controller, controlled_by FROM workspace_display_control_locks WHERE workspace_id=$1`, ws).Scan(&stillController, &stillBy); err != nil {
		t.Fatal(err)
	}
	if stillController != "user" || stillBy != "user-a" {
		t.Fatalf("lock changed under a blocked steal: (%q,%q), want (user, user-a)", stillController, stillBy)
	}

	// 4) The same user re-acquiring (renew) still works.
	if c, b, err := acquire("user", "user-a", 600); err != nil || c != "user" || b != "user-a" {
		t.Fatalf("user-a renew: (%q,%q,%v), want (user, user-a, nil)", c, b, err)
	}
}
