-- migration: 20260730190000_workspace_plugin_install_reports_outcome_comments.up.sql
--
-- CORRECTS THE DOCUMENTED SEMANTICS of workspace_plugin_install_reports.failed
-- and .swapped. Schema-neutral: no column, index or row is touched. The only
-- thing wrong with that table was what it SAID about itself, and the thing it
-- said was load-bearing — it is the text an operator reads out of `\d+` when
-- deciding whether a workspace is broken.
--
-- 20260730060000_workspace_plugin_install_reports.up.sql (core#4958, merged
-- 2af1f3989d39) says of `failed`: "Non-empty ⇒ never promoted", and of `swapped`:
-- "A partial build is never promoted", concluding "Liveness is `declared AND
-- swapped AND failed = []`". That is not what the runtime does, and was not what
-- it did when the table was written.
--
-- molecule-ai-workspace-runtime, molecule_runtime/plugin_sources.py:
--
--     # A failed source fails THAT SOURCE — not the whole tree.
--     ...
--     if report.failed:
--         <carry every live dir the successful sources are not replacing
--          into staging>
--         log.warning("[plugins] %d of %d source(s) failed — promoting the %d "
--                     "that succeeded; failed sources retry next boot", ...)
--     _atomic_swap_dir(staging_dir, target_dir)
--     report.swapped = True
--
-- The veto this table's comment describes DID exist, and was removed on purpose
-- after staging test5, 2026-07-13: one third-party plugin pinned to an
-- unfetchable SHA vetoed the whole swap and took the concierge's own management
-- MCP down with it, and on a first boot the "keep the existing tree" safety net
-- is vacuous — there is no previous tree, so the box booted with an EMPTY
-- plugins dir. Only an all-sources-failed boot, or a failure to carry a previous
-- dir forward, still declines the swap.
--
-- So `failed` non-empty says nothing about promotion, and folding it into
-- liveness reports a workspace with 5 of 6 plugins live and one flaky gitea
-- fetch as not-live. This table exists to stop core lying about plugin liveness;
-- a false alarm on a healthy box is the same lie with the sign flipped.
--
-- LIVENESS IS `declared AND swapped`. Non-empty `failed` on a promoted tree is a
-- separate, weaker signal — degraded — and both are derived on read by the API
-- (handlers.reportIsLive / reportIsDegraded), never stored, so there is still no
-- `live` column here to drift.
--
-- The partial index workspace_plugin_install_reports_not_live is UNCHANGED and
-- needs no change: `WHERE declared AND NOT swapped` is the exact complement of
-- the corrected rule. It was already right; only the prose around it was wrong.
--
-- WHY A NEW MIGRATION rather than an edit. 20260730060000 has been APPLIED —
-- staging-tenant-cd deployed it — and the runner records applied filenames in
-- schema_migrations and skips them, so an edit to that file would change what
-- the repo claims and nothing about any live database. Every statement below is
-- idempotent regardless: COMMENT ON COLUMN is a set, not an append.

COMMENT ON COLUMN workspace_plugin_install_reports.failed IS
    'Sources that could not be fetched or staged this boot. NON-EMPTY DOES NOT
     MEAN NOT PROMOTED: the runtime promotes partial builds by design
     (molecule_runtime/plugin_sources.py, "A failed source fails THAT SOURCE —
     not the whole tree"), carrying every live dir the successful sources are
     not replacing into staging and swapping anyway. swapped=true with a
     non-empty failed is the normal partial-promotion outcome — plugins ARE
     live, and a source needs looking at. That is degraded, not down. Read with
     swapped, never instead of it.';

COMMENT ON COLUMN workspace_plugin_install_reports.swapped IS
    'THE load-bearing column. Whether the freshly built staging tree was
     atomically swapped into plugins_dir. installed=[6 sources] with
     swapped=false means NOTHING went live — the state that was
     indistinguishable from success in every signal core had before this table
     existed. LIVENESS IS `declared AND swapped`, and nothing else: `failed`
     subtracts from WHAT is live without making the promotion not have happened,
     and reading `installed` alone is the mistake the rule exists to prevent.
     Derived live/degraded are computed on read by the API
     (handlers.reportIsLive / reportIsDegraded) and deliberately not stored — a
     derived column would be one more place for the rule to drift.';
