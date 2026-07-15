# Pull Request Hygiene

**Status:** Current guide. Violations are review findings unless a repository
gate enforces them.

**Audience:** Humans and agents opening Gitea pull requests in this repository.

## Current repository flow

- Canonical SCM is `https://git.moleculesai.app/molecule-ai/molecule-core`.
- Branch from current `main`; do not push directly to `main`.
- Open a Gitea pull request targeting `main`.
- Merge only after required review, mergeability, and the exact-head CI gates
  are green. Human-only checklist or product approvals cannot be self-approved
  by automation.
- A merge to `main` triggers the repository's CI/CD workflows. Do not describe
  an operator-host, GitHub, Railway, or Vercel step as part of the current
  release path.

## Keep the change reviewable

A pull request should have one coherent purpose. Large generated changes or
complete deletions can be legitimate, but explain why they belong together and
how a reviewer can verify them. If unrelated work appears in the diff, move the
intended commits onto a fresh branch from `origin/main`.

```bash
git fetch origin main
git switch -c fix/short-description origin/main
git cherry-pick <intended-commit>...
git push -u origin fix/short-description
```

When the branch is merely behind, update it without overwriting somebody
else's work. A rebase plus `--force-with-lease` is acceptable on a branch you
alone own; otherwise merge `origin/main` and let the pull request show the
resulting merge commit.

```bash
git fetch origin main
git rebase origin/main
git push --force-with-lease
```

## Write an actionable pull request

The title should name the affected surface and outcome. The body should state:

- what is broken or missing;
- why this change is the chosen fix;
- what was tested, including any live or end-to-end validation;
- what remains intentionally deferred, with a linked Gitea issue.

Reply to required review findings with either the correcting commit or a
specific technical reason the change is not needed. Do not silently resolve
threads. Optional comments may be deferred, but acknowledge them and file an
issue when the work still matters.

## Before merge

1. Confirm the pull request head SHA matches the commit whose checks you read.
2. Confirm every required job is terminal and successful; a green result for an
   older SHA is not evidence for the current head.
3. Check mergeability and required approvals in Gitea.
4. Preserve human-only gates. Do not use an admin bypass or another person's
   token to manufacture approval.
5. After merge, verify the post-merge run and the relevant staging or
   user-visible path. A merged pull request alone is not a deployment result.

## Historical context

An April 2026 audit found many pull requests whose intended fixes were obscured
by stale branch history. That incident motivated this guide, but its former
GitHub UI instructions and `staging`-first branch policy are retired.

## Related

- [Testing strategy](./testing-strategy.md)
- [Backend architecture](../architecture/backends.md)
- [Issue #1822](https://git.moleculesai.app/molecule-ai/molecule-core/issues/1822)
