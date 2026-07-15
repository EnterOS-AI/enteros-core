BEGIN;

-- The original production-team seed described a retired operator host and
-- Neon workflow. Rewrite the existing seeded card without replacing any
-- other skills or operator-customized fields.
UPDATE workspaces
SET agent_card = jsonb_set(
        agent_card,
        '{skills}',
        COALESCE((
            SELECT jsonb_agg(
                CASE
                    WHEN skill ->> 'id' = 'ops'
                         AND skill ->> 'description' = 'Direct hands-on ops on operator host and Neon' THEN
                        jsonb_set(
                            skill,
                            '{description}',
                            to_jsonb('Direct operations through domain APIs, Gitea Actions, and CI-on-merge workflows'::text),
                            true
                        )
                    ELSE skill
                END
                ORDER BY ordinality
            )
            FROM jsonb_array_elements(agent_card -> 'skills')
                WITH ORDINALITY AS entries(skill, ordinality)
        ), '[]'::jsonb),
        true
    ),
    updated_at = now()
WHERE id = '30ba7f0b-b303-4a20-aefe-3a4a675b8aa4'::uuid
  AND jsonb_typeof(agent_card -> 'skills') = 'array'
  AND EXISTS (
      SELECT 1
      FROM jsonb_array_elements(agent_card -> 'skills') AS skills(skill)
      WHERE skill ->> 'id' = 'ops'
        AND skill ->> 'description' = 'Direct hands-on ops on operator host and Neon'
  );

COMMIT;
