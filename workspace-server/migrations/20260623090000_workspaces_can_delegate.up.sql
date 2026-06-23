-- core#2127: per-workspace `can_delegate` capability.
--
-- Default TRUE preserves the existing behaviour for every current workspace —
-- the column does not affect routing, delegation, or any other call path until
-- an operator (or the future Phase-4 governance automation) explicitly PATCHes
-- it to FALSE. Defense-in-depth: a role-locked "coding executor" agent that
-- ALSO has its prompt bypassed (jailbreak / role drift) cannot delegate; the
-- A2A layer rejects the call AND the MCP layer hides the tools. Kimi/MiniMax
-- pinning is intentionally OUT OF SCOPE for this migration — that is an
-- operator action taken AFTER this PR lands, so live delegation is not broken
-- by the schema change.

ALTER TABLE workspaces
    ADD COLUMN IF NOT EXISTS can_delegate BOOLEAN NOT NULL DEFAULT TRUE;
