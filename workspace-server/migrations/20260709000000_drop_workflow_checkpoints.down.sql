-- Restore legacy workflow checkpoint storage when rolling back this removal.
CREATE TABLE IF NOT EXISTS workflow_checkpoints (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  workflow_id TEXT NOT NULL,
  step_name TEXT NOT NULL,
  step_index INT NOT NULL,
  completed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  payload JSONB,
  UNIQUE(workspace_id, workflow_id, step_name)
);

CREATE INDEX IF NOT EXISTS idx_wf_checkpoints_ws
  ON workflow_checkpoints(workspace_id, workflow_id, completed_at DESC);
