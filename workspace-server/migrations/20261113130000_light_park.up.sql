-- Enter OS Light, part 2 (park, open, the Light monitor): what a park in
-- flight needs so that an interruption (a core restart, a control-plane
-- outage) loses nothing. All three are written when a park claims 'parking'
-- or mints its run token, and read when the park is continued.
--
-- Additive and idempotent: nullable columns, no default, no backfill.

-- Who asked for the park in flight: 'owner' (POST /light/park clears core's
-- schedule suspensions), 'auto' (idle window), 'refresh' (queued work), or
-- 'monitor'. A continued park keeps the original trigger.
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS light_park_trigger TEXT
    CONSTRAINT workspaces_light_park_trigger_check CHECK (light_park_trigger IN ('owner', 'auto', 'refresh', 'monitor'));
-- The newest unapplied refresh when the park claimed 'parking'. The park's env
-- snapshot covers exactly the refreshes up to it; one queued while the park
-- runs stays queued (its change may postdate the snapshot).
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS light_park_watermark BIGINT;
-- The run token a park minted and has not recorded as light_run_token_id yet
-- (the PUT is in between). An interrupted park leaves it here, so the next
-- park, an open or a disable revokes it instead of leaving it live.
ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS light_pending_token_id UUID;
