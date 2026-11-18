-- Backfill workspace_plugins.tracked_ref for rows that predate the "ref:<name>"
-- form (core#4977).
--
-- WHY THIS IS NEEDED
-- ------------------
-- trackFromSource used to return 'none' for any ref without a literal
-- 'tag:'/'sha:' prefix. Nothing in production writes that prefixed form — real
-- sources look like '#main' and '#v0.2.1' — so EVERY existing row sits at
-- 'none'. The drift sweeper selects `WHERE tracked_ref != 'none'`, so it has
-- been selecting zero rows and reporting no drift: a guard covering nothing.
--
-- Shipping the Go fix alone only helps plugins installed AFTER the deploy.
-- Existing installs would stay invisible to the sweeper forever, because
-- nothing rewrites tracked_ref on an already-installed row. Hence this
-- backfill: re-derive tracked_ref from the source_raw each row already stores.
--
-- The derivation mirrors handlers.trackFromSource exactly:
--   '#tag:v1.0' -> 'tag:v1.0'   (immutable pin, passed through)
--   '#sha:abc'  -> 'sha:abc'    (immutable pin, passed through)
--   '#main'     -> 'ref:main'   (moving ref, now sweepable)
--   '#v0.2.1'   -> 'ref:v0.2.1' (bare tag; resolves to a constant SHA, so it
--                                simply never reports drift)
--   no fragment -> unchanged ('none' — nothing upstream to chase)
--   local://    -> unchanged (no upstream at all)
--
-- Everything after the FIRST '#' is the fragment, matching the Go
-- strings.Index(spec, "#") split — NOT split_part, which would truncate a
-- fragment that itself contains '#'.
--
-- IDEMPOTENT: the WHERE clause only matches tracked_ref = 'none', and this
-- statement never produces 'none', so a re-run is a no-op.
--
-- SAFETY: this only widens what the sweeper considers. It cannot cause a
-- downgrade or a re-install by itself — the sweeper compares the resolved
-- upstream SHA against installed_sha and enqueues drift only when they
-- differ, and rows with a NULL installed_sha stay excluded by its second
-- predicate.

UPDATE workspace_plugins
   SET tracked_ref = CASE
         WHEN substring(source_raw from position('#' in source_raw) + 1) LIKE 'tag:_%'
           THEN substring(source_raw from position('#' in source_raw) + 1)
         WHEN substring(source_raw from position('#' in source_raw) + 1) LIKE 'sha:_%'
           THEN substring(source_raw from position('#' in source_raw) + 1)
         ELSE 'ref:' || substring(source_raw from position('#' in source_raw) + 1)
       END,
       updated_at = NOW()
 WHERE tracked_ref = 'none'
   AND position('#' in source_raw) > 0
   AND substring(source_raw from position('#' in source_raw) + 1) <> ''
   AND source_raw NOT LIKE 'local://%';
