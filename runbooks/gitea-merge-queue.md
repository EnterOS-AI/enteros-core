# Gitea Merge Queue

Gitea 1.22.6 does not provide a real merge queue. Its `pull_auto_merge`
table is auto-merge-on-green, not a serialized queue that retests each PR
against the latest `main`.

`gitea-merge-queue` is the external queue for `molecule-core`.

## Queue Contract

**Auto-discovery (opt-OUT, default).** You do NOT need to label a PR. The bot
auto-discovers every open same-repo PR and merges any that meets the bar. The
`merge-queue` label is now optional metadata, not a gate. This removed the
historical autonomy gap: agent Gitea tokens lack `write:issue` (labels are
issue-scoped), so agents could never self-label and ready PRs stalled.

To keep a PR OUT of autonomous merging, add an opt-OUT label:
`merge-queue-hold`, `do-not-auto-merge`, or `wip`. Draft PRs are also skipped.

The bot processes one PR per tick:

1. Confirms `main`'s branch-protection-required push contexts are green.
2. Selects the oldest open same-repo PR that is NOT opt-out-labeled and NOT a
   draft (auto-discovery). With `AUTO_DISCOVER=0` it falls back to legacy
   opt-IN: only PRs carrying `merge-queue` are considered.
3. Rejects fork PRs because the queue may only update same-repo branches.
4. If the PR head does not contain current `main`, calls Gitea's
   `/pulls/{n}/update?style=merge` endpoint and waits for CI on the new head.
5. Merges only when, on the PR's CURRENT head sha:
   - `>= required_approvals` distinct genuine official `APPROVED` reviews from
     the recognised reviewer set (read from branch protection; default 2),
   - no open official `REQUEST_CHANGES`,
   - every branch-protection-required status context is green, and
   - the PR is `mergeable` (Gitea returns `True`; `None`/`False` = wait).

The merge bar is unchanged by auto-discovery — only WHICH PRs are considered
changes. The workflow is serialized with `concurrency`, so two PRs cannot be
merged against the same observed `main`.

## Operator Commands

Queue a PR (optional — auto-discovery already considers every ready PR; the
label is just visible metadata):

```bash
curl -fsS -X POST \
  -H "Authorization: token $GITEA_TOKEN" \
  -H "Content-Type: application/json" \
  "https://git.moleculesai.app/api/v1/repos/molecule-ai/molecule-core/issues/<PR>/labels" \
  -d '{"labels":["merge-queue"]}'
```

Keep a PR OUT of autonomous merging (opt-OUT — use `merge-queue-hold`,
`do-not-auto-merge`, or `wip`):

```bash
curl -fsS -X POST \
  -H "Authorization: token $GITEA_TOKEN" \
  -H "Content-Type: application/json" \
  "https://git.moleculesai.app/api/v1/repos/molecule-ai/molecule-core/issues/<PR>/labels" \
  -d '{"labels":["merge-queue-hold"]}'
```

Run the bot manually from a trusted checkout:

```bash
GITEA_TOKEN="$DEVOPS_ENGINEER_TOKEN" \
GITEA_HOST=git.moleculesai.app \
REPO=molecule-ai/molecule-core \
WATCH_BRANCH=main \
QUEUE_LABEL=merge-queue \
HOLD_LABEL=merge-queue-hold \
AUTO_DISCOVER=1 \
OPT_OUT_LABELS=do-not-auto-merge,wip \
REVIEWER_SET=agent-reviewer,agent-researcher,agent-reviewer-cr2 \
UPDATE_STYLE=merge \
python3 .gitea/scripts/gitea-merge-queue.py --dry-run
```

Dry run:

```bash
python3 .gitea/scripts/gitea-merge-queue.py --dry-run
```

## Branch Protection

`main` should keep direct merges restricted to the non-bypass merge actor
used by the queue. Normal humans and agents should not merge directly.

`block_on_outdated_branch` should be enabled as a defense in depth, but it
does not replace the queue. The queue still performs its own current-main
check immediately before merge because branch protection alone cannot
serialize two already-green PRs.

## Failure Handling

If `main` is not green, the queue pauses and does not merge anything.

If a queued PR is stale, the queue updates the PR branch and comments on the
PR. It does not merge until CI runs on the updated head.

If the queue workflow fails, treat it as a CI/CD incident. Do not bypass by
manually merging unless the human operator explicitly accepts the risk.
