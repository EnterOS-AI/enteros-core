# `SDK_PIN_BUMP_TOKEN` — what it is, where it lives, how to rotate it

Provisioned 2026-08-05 to arm `.gitea/workflows/sdk-pin-bump.yml`. Recorded here
so it is not a mystery credential the next time somebody finds a bot account with
push rights and no explanation.

## Why it exists

`sdk-pin-bump` moves `workspace-server/go.mod`'s `go.moleculesai.app/sdk/gen/go`
pin to `molecule-ai-sdk@main`, regenerates the provider projection, moves the
`canonicalRegistrySHA256` checkpoint and **opens a PR**. Pushing a branch and
creating a PR are the only things it needs a credential for; the SDK head read is
anonymous (the SDK repo is public).

Before this was provisioned the lane's guard warned and skipped, and the job
reported SUCCESS having done nothing — run 620174, 2026-08-05 12:19Z, dispatched
mid-release-chain, green in 19s, pin then bumped by hand four minutes later
(45a61bb). The guard now fails closed, so an unprovisioned or revoked credential
turns the lane RED instead of silently green.

## The credential

| | |
|---|---|
| Actions secret name | `SDK_PIN_BUMP_TOKEN` |
| Stored at | **repo** scope on `molecule-ai/molecule-core` (Settings → Actions → Secrets) |
| Gitea PAT name | `sdk-pin-bump-molecule-core` |
| Owning identity | `molecule-sdk-pin-bot` (Gitea user, created for this lane only) |
| PAT scopes | `write:repository` — nothing else. No `read:user`, no `write:organization`, no package scopes. |
| Repo access of that identity | **write on `molecule-ai/molecule-core` only** (repo collaborator). Read elsewhere comes from repos being public; it has write nowhere else. |

### Why repo scope and not org scope

An org-level Actions secret is readable by a workflow in **every** repo in the
org, so an org-scoped push credential is exfiltratable from any of ~110 repos'
CI. Exactly one lane in exactly one repo consumes this token, so the reader set
should be exactly that repo. Org scope is right for a credential many repos
genuinely share (`AUTO_SYNC_TOKEN`, `MOLECULE_REGISTRY_TOKEN`); it is wrong here.

### Why a dedicated identity and not `molecule-runtime-release-bot`

That bot already carries write on 13 template/runtime repos for the runtime
version-bump lanes. Minting a fourth token on it would have given this lane's
credential a 14-repo blast radius for a job that needs one repo. Gitea PAT scopes
are category-wide (`write:repository` means "every repo this user can write"), so
the only way to narrow a push credential is to narrow the **identity**. Hence a
bot whose entire write surface is this repository.

### What it deliberately cannot do

* It is **not** in `DEFAULT_REVIEWER_SET` (`.gitea/scripts/_review_policy.py`), so
  it cannot approve the PR it opens.
* `main` has `enable_push: false`, so write-collaborator does not mean it can
  push to trunk — it can only push `bump/sdk-*` branches and open PRs.
* It cannot merge: the merge bar still needs a non-author approval and the merge
  train.

## Rotation / revocation

Revoke by **name on the owning identity**, not by guessing which token a value
belongs to:

```bash
# list (names + scopes only, never values)
curl -sS -u "<admin>:<admin-pat>" \
  "https://git.moleculesai.app/api/v1/users/molecule-sdk-pin-bot/tokens?sudo=molecule-sdk-pin-bot"

# revoke
curl -sS -X DELETE -u "<admin>:<admin-pat>" \
  "https://git.moleculesai.app/api/v1/users/molecule-sdk-pin-bot/tokens/sdk-pin-bump-molecule-core?sudo=molecule-sdk-pin-bot"
```

Then mint a replacement with the same name and scope and overwrite the repo
secret. **Do not** delete the Actions secret and leave it unset expecting the
lane to skip — it will now go red on every dispatch, which is the intended
signal, but it means the pin stops moving until the secret is restored.

## How to tell the lane is actually armed

Dispatch it and read the run. A run whose steps after the guard are `skipped` is
the old broken shape and should no longer be possible; a healthy run ends with an
`Outcome` step that says one of:

* `BUMPED — opened a PR moving the pin to <sha>`
* `ALREADY CURRENT — pin is at molecule-ai-sdk@main (<sha>)`
* `FAILED before it could compare the pin`
