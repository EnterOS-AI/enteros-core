-- Per-install plugin settings, split by WHO OWNS each half.
--
-- The whole point of this table is the two-column split:
--
--   config     layers 2-5 — template `plugins[].config`, org defaults, org node.
--              Core REWRITES this wholesale on every (re-)provision, because it
--              is derived from the template and must track it.
--
--   overrides  layer 6 — an operator's live edit. Core NEVER writes this during
--              provisioning. It is the only reason an edit survives a
--              re-provision, and four previous attempts at layer 6 failed by
--              trying to keep both in one column and then losing the edit the
--              next time the template was applied.
--
-- Effective value for a key = overrides[key] if present, else config[key].
--
-- PROVENANCE. Both columns store {value, layer, set_by, set_at} PER KEY, not a
-- bare value. `config` needs it too: without it a GET can name the winning
-- layer only for overridden keys and has nothing to say about the rest, which
-- is exactly the question an operator asks ("why is it this?").
--
-- `set_at` is stamped from a CONTENT HASH of the value, not wall-clock time, so
-- a re-provision that produces the same value does not churn the timestamp and
-- make every provision look like an edit.
CREATE TABLE IF NOT EXISTS workspace_plugin_settings (
  workspace_id       UUID        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  -- The INSTALL name: the /configs/plugins/<name>/ directory, derived from the
  -- plugin source repo. NOT the manifest's own `name:`. Core writes the
  -- settings FILE under this key and the runtime reads it back with the same
  -- one; keying on the manifest name would address a file nothing reads, and a
  -- missing settings file is a clean no-op on the runtime side, so that
  -- mismatch would fail SILENTLY.
  plugin_name        TEXT        NOT NULL,
  config             JSONB       NOT NULL DEFAULT '{}'::jsonb,
  overrides          JSONB       NOT NULL DEFAULT '{}'::jsonb,
  -- Compare-and-set token for concurrent operator edits. A PATCH carries the
  -- version it read; a mismatch is a 409 rather than a lost update. Deliberately
  -- NOT the per-workspace provision gate, which is a documented non-reentrant
  -- deadlock — an edit must never be able to block or be blocked by a provision.
  overrides_version  BIGINT      NOT NULL DEFAULT 0,
  created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY (workspace_id, plugin_name)
);

-- Provision rewrites every row for a workspace at once; the plugin tab reads
-- one workspace at a time. Both are covered by the primary key's leading column.
CREATE INDEX IF NOT EXISTS workspace_plugin_settings_ws
  ON workspace_plugin_settings(workspace_id);
