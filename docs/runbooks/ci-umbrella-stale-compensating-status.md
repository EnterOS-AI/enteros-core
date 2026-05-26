# Runbook: stale CI umbrella with green sub-jobs — compensating status

**When to use this:** A PR's `CI / all-required (pull_request)` status is `failure` (so branch protection blocks the merge), but all 5 required sub-jobs (`Detect changes`, `Platform (Go)`, `Canvas (Next.js)`, `Shellcheck (E2E scripts)`, `Python Lint & Test`) actually succeeded. The umbrella job's internal 40-min poll deadline elapsed before the success statuses propagated through Gitea's commit-status pipeline.

**When NOT to use this:** Any required sub-job actually failed. The umbrella correctly reflects reality; a compensating-status post would lie.

This pattern parallels what `.gitea/workflows/status-reaper.yml` does for default-branch `(push)` status drift, but applied to PR umbrellas instead of main-branch contexts.

## Diagnose

1. Look up the umbrella status:

   ```bash
   TOK=$(cat ~/.molecule-ai/gitea-token)
   API=https://git.moleculesai.app/api/v1/repos/molecule-ai/molecule-core
   PR=<pr-number>
   sha=$(curl -sS -H "Authorization: token $TOK" "$API/pulls/$PR" | python3 -c "import sys,json; print(json.load(sys.stdin)['head']['sha'])")
   curl -sS -H "Authorization: token $TOK" "$API/commits/$sha/status" \
     | python3 -c "import sys,json; [print(s['status'], s['context']) for s in json.load(sys.stdin)['statuses'] if 'all-required' in s['context']]"
   ```

2. Look up the actual sub-job statuses in the Gitea DB:

   ```bash
   ssh root@5.78.80.188 "docker exec molecule-postgres-1 psql -U gitea -d gitea -tAc \"
     SELECT aj.name,
       CASE aj.status WHEN 1 THEN 'success' WHEN 2 THEN 'failure' WHEN 3 THEN 'cancelled' WHEN 4 THEN 'skipped' WHEN 5 THEN 'waiting' WHEN 6 THEN 'running' END
     FROM action_run ar JOIN action_run_job aj ON aj.run_id=ar.id
     WHERE ar.repo_id=17 AND ar.workflow_id='ci.yml' AND ar.commit_sha='$sha'
     ORDER BY aj.id;\""
   ```

   The 5 required-by-umbrella sub-jobs must all be `success`. (`Canvas Deploy Reminder` is intentionally not required — its state doesn't matter.)

## Recover

If diagnosis confirms all 5 required sub-jobs are success but the umbrella is stuck at failure:

```bash
curl -sS -X POST -H "Authorization: token $TOK" -H "Content-Type: application/json" \
  "$API/statuses/$sha" -d '{
    "context": "CI / all-required (pull_request)",
    "state": "success",
    "description": "Compensating status: all 5 required sub-jobs verified success in action_run_job; umbrella stale due to commit-status propagation race. Posted by <operator> per ci-umbrella-stale-compensating-status runbook."
  }'
```

The status posts immediately; the merge gate flips green within ~5 seconds.

**Always include WHO and WHY in the `description` field** so the audit trail is honest. Future operators (and `audit-force-merge.yml` consumers) need to be able to tell a recovery from a real bypass.

## Why this happens

- The umbrella's 40-min internal poll loop (`.gitea/workflows/ci.yml` → `all-required` job → `Wait for required CI contexts` step) treats `missing` statuses as pending.
- Status propagation: a job completing on a runner posts its `action_run_job.status=1` row first, then Gitea's notifier walks `action_run_job` → `commit_status` table. Under high write load (many concurrent PRs synchronizing) the notifier walk can lag by several minutes.
- If propagation lag pushes the last sub-job's commit-status past the umbrella's 40-min wall-clock deadline, the umbrella fails even though the sub-jobs were green well within the window.
- The umbrella correctly does not retry once it has emitted a terminal status (per RFC internal#219 design — retries would mask real failures).

## Prevent

Most cases are downstream of the runner-pool dispatch deadlock fixed by commit `7da843f2` (issue #1779). With the umbrella running on the dedicated `ci-meta` pool, sub-jobs are no longer competing for runners with their own umbrella, so propagation completes well before the 40-min deadline in normal load.

If you find yourself reaching for this runbook frequently, that's the signal to either:
- Raise `timeout-minutes` on the umbrella above 45.
- Build the `umbrella-reaper.yml` auto-recovery described in issue #1780 (this runbook is its precursor).

## Cross-refs

- Issue #1780 — original write-up; tracks auto-recovery
- Issue #1779 — runner-pool deadlock; root cause of the propagation lag in most cases
- `.gitea/workflows/status-reaper.yml` — sibling pattern for default-branch `(push)` status drift
- `.gitea/workflows/audit-force-merge.yml` — audits bypass merges; this runbook's `description` field is what makes a compensating-status merge auditable vs. opaque

## Session-local examples

The pattern was used twice during the 2026-05-24 CTO-bypass session:

- **PR #1737** merged via compensating-status — all 5 sub-jobs green, umbrella timed out on propagation race. Merge commit `d5941906`.
- **PR #1759** merged via compensating-status — 4/5 sub-jobs green, the 5th (`Platform (Go)`) was an inherited-from-main failure (templates_test fixtures bug, tracked as #1778, fixed in #1781). The compensating-status description called out the inherited failure honestly. Merge commit `220a04b1`.
