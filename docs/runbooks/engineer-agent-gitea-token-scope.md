# Engineer-Agent Gitea Token Scope Runbook

## Symptom

Engineer-class agents (e.g. `agent-dev-a`, `agent-dev-b`) fail swarm-pull issue discovery or receive HTTP 403 when calling Gitea issue-list APIs, while PR review and repository API operations continue to work.

Typical failing call:
```bash
GET /api/v1/repos/molecule-ai/molecule-core/issues?state=open&labels=approved&limit=50
# => 403 Forbidden
```

Typical working calls (same token):
```bash
GET /api/v1/repos/molecule-ai/molecule-core/pulls?state=open&limit=50
POST /api/v1/repos/molecule-ai/molecule-core/pulls/1666/comments
# => 200 OK
```

## Root Cause

Gitea v1.22.6 routes issue-list under the `Issue` scope category (`routers/api/v1/api.go:1379-1491`), while PR routes live under repository/pull routing (`api.go:1278-1305`). The scope gate derives required read/write level from HTTP method (`api.go:309-313`), so `GET /issues?...` requires `read:issue`.

Engineer-class agent PATs were provisioned with repository and PR scopes but without `read:issue`, causing the asymmetric 403.

## Detection

1. **Agent-side**: swarm-pull workflow logs show `403 Forbidden` on issue enumeration but not on PR list/review.
2. **Platform-side**: Gitea access logs show `GET /repos/{owner}/{repo}/issues` returning 403 for the affected token.
3. **Reproduction** (from any workspace with a suspected token):
   ```bash
   TOKEN=$(cat /configs/secrets.d/GITEA_TOKEN)
   PLATFORM="https://git.moleculesai.app"

   # Should succeed — confirms token is live
   curl -s -o /dev/null -w "%{http_code}" \
     -H "Authorization: token $TOKEN" \
     "$PLATFORM/api/v1/user"

   # Will 403 if the token lacks read:issue
   curl -s -o /dev/null -w "%{http_code}" \
     -H "Authorization: token $TOKEN" \
     "$PLATFORM/api/v1/repos/molecule-ai/molecule-core/issues?state=open&limit=1"
   ```

## Immediate Fix

### Step 1: Issue fresh PATs with correct scopes

From a Gitea site-admin account (or via the Gitea web UI → Settings → Applications):

1. Navigate to the affected user's profile (e.g. `agent-dev-a`).
2. Go to **Settings → Applications → Generate New Token**.
3. Select scopes:
   - `read:repository` (existing)
   - `write:repository` (existing, if push is required)
   - `read:issue` (**add this**)
   - `write:issue` (add only if agents must comment/edit issues)
   - `read:pull-request` / `write:pull-request` (existing)
   - `read:comment` / `write:comment` (existing, if PR review is required)
4. Copy the plaintext token immediately — it is shown only once.

### Step 2: Update workspace secrets

For each affected engineer workspace, update the Gitea token secret:

```bash
# Via the platform API (admin auth required)
PLATFORM="https://agents-team.moleculesai.app"
ADMIN_TOKEN="<your-admin-token>"
WORKSPACE_ID="<affected-workspace-id>"
NEW_GITEA_TOKEN="<fresh-token-from-step-1>"

curl -X POST "$PLATFORM/workspaces/$WORKSPACE_ID/secrets" \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d "{
    \"GITEA_TOKEN\": \"$NEW_GITEA_TOKEN\"
  }"
```

Restart the workspace so the runtime re-reads secrets:
```bash
curl -X POST "$PLATFORM/workspaces/$WORKSPACE_ID/restart" \
  -H "Authorization: Bearer $ADMIN_TOKEN"
```

### Step 3: Smoke-test

From the restarted workspace, verify all three paths:

```bash
# 1. Issue list (the previously failing path)
curl -s -H "Authorization: token $GITEA_TOKEN" \
  "https://git.moleculesai.app/api/v1/repos/molecule-ai/molecule-core/issues?state=open&labels=approved&limit=1" | jq '.[0].number'

# 2. PR list (should still work)
curl -s -H "Authorization: token $GITEA_TOKEN" \
  "https://git.moleculesai.app/api/v1/repos/molecule-ai/molecule-core/pulls?state=open&limit=1" | jq '.[0].number'

# 3. Swarm-pull discovery (end-to-end)
# Trigger the agent's autonomous tick or delegate a task that enumerates open issues.
```

## Long-Term Fix

Update the **workspace secret injection path** that writes `/configs/secrets.d/GITEA_TOKEN` for engineer-class agents. The provisioning template or secret-distribution job should request `read:issue` (and optionally `write:issue`) at token-creation time.

File locations to audit:
- `.gitea/scripts/` — any token-provisioning automation
- `infra/terraform/` or equivalent — IAM/secret-manager templates
- `workspace-configs-templates/` — engineer-class workspace templates that declare required secrets

## Prevention

1. **Token scope checklist**: when provisioning new engineer-class agent tokens, verify the scope set includes `read:issue` before distributing the secret.
2. **Monitoring**: add an agent health-check that probes `GET /repos/molecule-ai/molecule-core/issues?limit=1` and surfaces a non-fatal warning if it returns 403.
3. **Documentation**: update the onboarding runbook for new engineer agents to include the full required scope list.

## References

- Gitea issue #1750: [RCA: engineer-token read:issue scope gap blocks swarm-pull workflow](https://git.moleculesai.app/molecule-ai/molecule-core/issues/1750)
- Gitea source: `routers/api/v1/api.go:309-313` (scope gate), `api.go:1278-1305` (PR routing), `api.go:1379-1491` (issue routing)
- Related: PR #1542 (provisioner git-creds injection), PR #1669 (auth_token inline mint)
