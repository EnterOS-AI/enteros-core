-- Retire legacy workflow checkpoint API/storage.
DROP INDEX IF EXISTS idx_wf_checkpoints_ws;
DROP TABLE IF EXISTS workflow_checkpoints;
