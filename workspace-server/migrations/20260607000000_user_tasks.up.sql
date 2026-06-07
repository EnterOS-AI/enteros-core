-- user_tasks: structured agent→user action requests ("things an agent asks
-- the user to do"). Mirrors the approval_requests shape but is a worklist
-- signal, not a destructive-action gate. See docs/design/rfc-user-tasks.md.
CREATE TABLE IF NOT EXISTS user_tasks (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    title        TEXT NOT NULL,
    detail       TEXT,
    status       TEXT NOT NULL DEFAULT 'pending'
                 CHECK (status IN ('pending', 'done', 'dismissed')),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at  TIMESTAMPTZ,
    resolved_by  TEXT
);

CREATE INDEX IF NOT EXISTS idx_user_tasks_pending
    ON user_tasks (status, created_at DESC);
