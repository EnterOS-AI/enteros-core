package handlers

import (
	"context"
	"database/sql"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/desktopgateway"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
)

// DesktopLifecycleStore reads/writes the workspace_desktop_lifecycle side-table
// and the display-control lock. It backs the computer-use gateway's LockChecker
// + ActivityRecorder and the idle sweeper (design §8, §10, §12).
//
// VERIFICATION NOTE: the SQL/DB behavior here is NOT unit-verified in this
// environment (no running Postgres). Only compile + vet. The queries mirror the
// migration 20260727130000_workspace_desktop_lifecycle and the existing
// workspace_display_control_locks schema; validate against a stack.
type DesktopLifecycleStore struct {
	db *sql.DB
}

// NewDesktopLifecycleStore builds the store over the platform DB pool.
func NewDesktopLifecycleStore(db *sql.DB) *DesktopLifecycleStore {
	return &DesktopLifecycleStore{db: db}
}

// RecordAgentActivity bumps last_agent_activity_at — the authoritative liveness
// signal (§10) — upserting the lifecycle row.
func (s *DesktopLifecycleStore) RecordAgentActivity(ctx context.Context, workspaceID string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO workspace_desktop_lifecycle (workspace_id, last_agent_activity_at, updated_at)
		VALUES ($1, now(), now())
		ON CONFLICT (workspace_id)
		DO UPDATE SET last_agent_activity_at = now(), updated_at = now()
	`, workspaceID)
	return err
}

// AgentHoldsControl reports whether the workspace's AGENT currently holds the
// display-control lock (§8: /input is gated on this; view is not).
func (s *DesktopLifecycleStore) AgentHoldsControl(ctx context.Context, workspaceID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT count(*) FROM workspace_display_control_locks
		WHERE workspace_id = $1 AND controller = 'agent' AND expires_at > now()
	`, workspaceID).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// SetVNCPresence records live human VNC viewer count + last-input time (the
// human side of the §10 liveness signal), upserting the row.
func (s *DesktopLifecycleStore) SetVNCPresence(ctx context.Context, workspaceID string, connections int, lastInput time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO workspace_desktop_lifecycle (workspace_id, vnc_connections, last_vnc_input_at, updated_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (workspace_id)
		DO UPDATE SET vnc_connections = $2, last_vnc_input_at = $3, updated_at = now()
	`, workspaceID, connections, lastInput)
	return err
}

// LoadActivity loads the liveness signals the idle decision (§10) needs. A
// missing row reads as all-cold (no activity, no viewers).
func (s *DesktopLifecycleStore) LoadActivity(ctx context.Context, workspaceID string) (provisioner.DesktopActivity, error) {
	var (
		lastAgent, lastVNC sql.NullTime
		vncConns           int
		leaseHeld          bool
		leaseExp           sql.NullTime
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT last_agent_activity_at, last_vnc_input_at, vnc_connections, lease_held, lease_expires_at
		FROM workspace_desktop_lifecycle WHERE workspace_id = $1
	`, workspaceID).Scan(&lastAgent, &lastVNC, &vncConns, &leaseHeld, &leaseExp)
	if err == sql.ErrNoRows {
		return provisioner.DesktopActivity{}, nil
	}
	if err != nil {
		return provisioner.DesktopActivity{}, err
	}
	act := provisioner.DesktopActivity{
		VNCConnections: vncConns,
		// A held lease only counts while unexpired (a stale lease must not pin
		// the desktop up forever).
		AgentLeaseHeld: leaseHeld && (!leaseExp.Valid || leaseExp.Time.After(time.Now())),
	}
	if lastAgent.Valid {
		act.LastAgentActivity = lastAgent.Time
	}
	if lastVNC.Valid {
		act.LastVNCInput = lastVNC.Time
	}
	return act, nil
}

// SetState updates the lifecycle state-machine column (stopped/starting/running/
// stopping) and, when transitioning to running, the reachable sidecar address.
func (s *DesktopLifecycleStore) SetState(ctx context.Context, workspaceID, state, sidecarAddress string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO workspace_desktop_lifecycle (workspace_id, state, sidecar_address, updated_at)
		VALUES ($1, $2, NULLIF($3, ''), now())
		ON CONFLICT (workspace_id)
		DO UPDATE SET state = $2, sidecar_address = NULLIF($3, ''), updated_at = now()
	`, workspaceID, state, sidecarAddress)
	return err
}

// RunningDesktopWorkspaceIDs returns the workspaces whose desktop is 'running',
// for the idle sweeper to evaluate for teardown (§10). Uses the partial index.
func (s *DesktopLifecycleStore) RunningDesktopWorkspaceIDs(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT workspace_id::text FROM workspace_desktop_lifecycle WHERE state = 'running'
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// Compile-time: the store satisfies the gateway's dependency interfaces.
var (
	_ desktopgateway.LockChecker      = (*DesktopLifecycleStore)(nil)
	_ desktopgateway.ActivityRecorder = (*DesktopLifecycleStore)(nil)
)
