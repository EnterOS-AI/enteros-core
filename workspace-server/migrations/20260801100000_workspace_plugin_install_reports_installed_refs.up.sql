-- core#5007: the box reports WHICH COMMIT it actually installed per source.
--
-- Until now `installed_sha` on workspace_plugins was a claim BY the control
-- plane, never an observation OF the box, and nothing the box sent carried a
-- commit at all: installed/skipped/failed are SOURCE STRINGS, so two commits of
-- the same `#main` pin were indistinguishable.
--
-- The cost was paid twice in one night. Throughout core#5009 the control plane
-- reported installed_sha=3d41a34 drift=0 for a workspace whose disk matched and
-- whose behaviour did not; every step of that diagnosis, and of the core#5011
-- outage after it, required shell access to a customer's container to answer
-- "what is this box actually running".
--
-- NULLABLE and defaulted to NULL rather than '{}': a runtime older than
-- molecule-ai-workspace-runtime 0.4.72 does not send the field, and a reader
-- MUST distinguish "this box cannot tell me" from "this box installed nothing".
-- Defaulting to an empty object would erase that distinction and make every
-- un-upgraded box look like an empty install.
ALTER TABLE workspace_plugin_install_reports
    ADD COLUMN IF NOT EXISTS installed_refs jsonb;

COMMENT ON COLUMN workspace_plugin_install_reports.installed_refs IS
    'core#5007: declared source -> the commit the BOX resolved and installed. '
    'NULL means the producing runtime predates the field, NOT that nothing was '
    'installed. Compare against workspace_plugins.installed_sha to detect a '
    'control-plane record that disagrees with the box.';
