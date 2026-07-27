-- Per-workspace desktop-sidecar lifecycle state (design RFC §10, §12).
--
-- A side table keyed by workspace_id, DELIBERATELY off the hot workspaces row
-- (same rationale as workspace_stall_state / workspace_display_control_locks):
-- the scale-to-zero bookkeeping writes (last_agent_activity_at, lease) must
-- never contend with the heartbeat UPDATE on workspaces.
CREATE TABLE IF NOT EXISTS workspace_desktop_lifecycle (
    workspace_id uuid PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
    state text NOT NULL DEFAULT 'stopped'
        CHECK (state IN ('stopped', 'starting', 'running', 'stopping')),
    -- Authoritative liveness signal: agent control-server activity. The agent
    -- opens ZERO VNC connections, so VNC/X-idle alone would reap a working
    -- desktop (§10). Screenshots count as activity.
    last_agent_activity_at timestamptz,
    last_vnc_input_at timestamptz,
    vnc_connections int NOT NULL DEFAULT 0 CHECK (vnc_connections >= 0),
    -- Explicit computer-use lease: suppresses teardown for the life of the
    -- agent loop regardless of input cadence.
    lease_held boolean NOT NULL DEFAULT false,
    lease_expires_at timestamptz,
    -- Realized-resource handle: the persistent profile volume (cookies/logins).
    profile_volume text,
    -- Where the sidecar control server / VNC is reachable this run.
    sidecar_address text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Partial index for the idle sweeper: it scans only RUNNING desktops to decide
-- teardown, so keep that set cheap to find (mirrors the hibernation-monitor
-- partial-index pattern on workspaces).
CREATE INDEX IF NOT EXISTS idx_workspace_desktop_lifecycle_running
    ON workspace_desktop_lifecycle (workspace_id)
    WHERE state = 'running';
