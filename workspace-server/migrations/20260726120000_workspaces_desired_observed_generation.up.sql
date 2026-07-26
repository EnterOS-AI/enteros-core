BEGIN;

-- RFC concierge wake-lifecycle (PR-B): the desired/observed generation pair is
-- the Kubernetes-style convergence signal for a workspace's WAKE intents.
--
--   desired_generation  — monotonically bumped by the desired-state owner
--                         (wake_lifecycle.go bumpDesiredGeneration) every time a
--                         NEW wake intent is minted. It is the platform's "this
--                         is the state I want the box in" counter. In-SQL
--                         increment (`SET desired_generation = desired_generation
--                         + 1 RETURNING ...`) so it is atomic and gap-free even
--                         under concurrent DecideWake calls — never a
--                         read-modify-write.
--   observed_generation — the highest desired_generation the RUNTIME has
--                         acknowledged converging to, reported back through the
--                         heartbeat. The convergence loop settles every wake
--                         intent whose generation <= observed_generation.
--
-- When observed_generation < desired_generation the box has un-converged wake
-- intent(s) and the heartbeat handler re-emits/settles accordingly (later PRs).
-- When they are equal the box is fully converged and nothing is outstanding.
--
-- Both BIGINT NOT NULL DEFAULT 0 so every existing and new row starts fully
-- converged (desired == observed == 0, no phantom outstanding wake). IF NOT
-- EXISTS keeps the DDL re-entrant under a runner that re-applies ups. No
-- backfill: 0/0 is already the correct "nothing wanted, nothing outstanding"
-- state for every pre-existing workspace.
ALTER TABLE workspaces
    ADD COLUMN IF NOT EXISTS desired_generation BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS observed_generation BIGINT NOT NULL DEFAULT 0;

COMMIT;
