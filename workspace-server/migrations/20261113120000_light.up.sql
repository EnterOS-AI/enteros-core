-- Enter OS Light (contracts C3/C5, docs/api-protocol/light-api.md): a Light
-- agent is a parked agent whose schedules run as isolated, short-lived copies
-- of it in their own pods. Core keeps the Light state of each agent, a ledger
-- of its runs, the schedule set rendered at its last park, and the work queued
-- for the next open/park cycle.
--
-- Additive and idempotent: every statement is IF NOT EXISTS, no default that
-- rewrites a table, no backfill. NULL light_state = not a Light agent, which is
-- every existing row.

-- workspaces.light_state (C5): NULL | open | parking | parked | opening.
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS light_state TEXT
    CONSTRAINT workspaces_light_state_check CHECK (light_state IN ('open', 'parking', 'parked', 'opening'));
-- When light_state last changed: the idle clock of auto-park and the age of a
-- park or open the monitor may have to finish.
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS light_state_at TIMESTAMPTZ;
-- Who opened the agent last: 'owner' (POST /light/open) or 'refresh' (the
-- monitor, to apply queued work). A refresh-opened agent parks again as soon as
-- it is online and idle; an owner-opened one after 30 idle minutes.
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS light_opened_by TEXT
    CONSTRAINT workspaces_light_opened_by_check CHECK (light_opened_by IN ('owner', 'refresh'));
-- The run tokens (API-kind workspace_auth_tokens ids). light_run_token_id is
-- the one in the control plane's Light Secret since the last PUT;
-- light_retired_token_id is the previous park's, kept until its runs finish.
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS light_run_token_id UUID;
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS light_retired_token_id UUID;
-- The last Light operation that failed in the background: {"code","message"}.
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS light_last_error JSONB;
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS light_last_error_at TIMESTAMPTZ;

-- The monitors scan Light agents only.
CREATE INDEX IF NOT EXISTS idx_workspaces_light_state
    ON workspaces (light_state) WHERE light_state IS NOT NULL;

-- The run ledger (C3). One row per run, keyed by the Job name the run pod sees
-- as MOLECULE_RUN_ID. source 'report' = the run pod reported it; 'job' = core
-- recorded it from the control plane's jobs listing (a run that never
-- reported) or from a pause that cancelled it before its copy ('cancelled').
CREATE TABLE IF NOT EXISTS light_runs (
    workspace_id  UUID        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    run_id        TEXT        NOT NULL CHECK (char_length(run_id) BETWEEN 1 AND 128),
    schedule      TEXT        NOT NULL,
    status        TEXT        NOT NULL CHECK (status IN ('running', 'succeeded', 'failed', 'timed_out', 'cancelled')),
    source        TEXT        NOT NULL CHECK (source IN ('report', 'job')),
    scheduled_for TIMESTAMPTZ,
    started_at    TIMESTAMPTZ,
    finished_at   TIMESTAMPTZ,
    exit_code     INTEGER,
    output        TEXT        NOT NULL DEFAULT '' CHECK (octet_length(output) <= 65536),
    error         TEXT        NOT NULL DEFAULT '' CHECK (octet_length(error) <= 4096),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, run_id)
);
CREATE INDEX IF NOT EXISTS idx_light_runs_ws_created
    ON light_runs (workspace_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_light_runs_ws_schedule_created
    ON light_runs (workspace_id, schedule, created_at DESC);

-- The schedule set rendered at the last park, as the agent's grid held it, plus
-- core's own record of a schedule it suspended after failed runs.
CREATE TABLE IF NOT EXISTS light_schedules (
    workspace_id         UUID        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name                 TEXT        NOT NULL,
    cron                 TEXT        NOT NULL,
    timezone             TEXT        NOT NULL DEFAULT '',
    prompt               TEXT        NOT NULL,
    enabled              BOOLEAN     NOT NULL DEFAULT true,
    suspended_reason     TEXT,
    suspended_at         TIMESTAMPTZ,
    consecutive_failures INTEGER     NOT NULL DEFAULT 0,
    cronjob              TEXT,
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, name)
);

-- Work queued for a parked agent: the monitor opens the agent, applies each
-- pending action once it is online and idle, and parks it again. 'refresh'
-- (a secret, plugin or config change the control plane's env snapshot must
-- pick up) needs nothing applied beyond the open itself. kind is free text so a
-- later kind (e.g. a queued schedule edit) needs code, not a migration.
CREATE TABLE IF NOT EXISTS light_pending_actions (
    id           BIGSERIAL   PRIMARY KEY,
    workspace_id UUID        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    kind         TEXT        NOT NULL,
    payload      JSONB       NOT NULL DEFAULT '{}'::jsonb,
    source       TEXT        NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    applied_at   TIMESTAMPTZ,
    error        TEXT
);
CREATE INDEX IF NOT EXISTS idx_light_pending_actions_open
    ON light_pending_actions (workspace_id, id) WHERE applied_at IS NULL;

-- C3b: the run a row was written by, from the X-Molecule-Run-Id header of a
-- run pod's request. NULL for every row not written by a Light run.
ALTER TABLE activity_logs ADD COLUMN IF NOT EXISTS run_id TEXT;
-- Partial, so it indexes only run rows (none at migration time). Plain CREATE
-- INDEX, not CONCURRENTLY, as 20260809140000 does: this runner wraps each file
-- in one transaction.
CREATE INDEX IF NOT EXISTS idx_activity_logs_ws_run
    ON activity_logs (workspace_id, run_id) WHERE run_id IS NOT NULL;
