# Organization API keys

Organization API keys let scripts and integrations call one tenant's
workspace-server without a browser session. Treat each key as a tenant-admin
password.

## Create a key

1. Open `https://<your-slug>.moleculesai.app`.
2. Open Settings, then **Org API Keys**.
3. Create a descriptively named key and optionally set an expiry.
4. Copy the returned token immediately. It is not shown again.

The API form accepts the same fields:

```bash
curl -X POST \
  -H "Authorization: Bearer $MOLECULE_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"ci-agent","expires_at":"2026-08-01T00:00:00Z"}' \
  "https://acme.moleculesai.app/org/tokens"
```

Bearer/agent callers may receive an approval-pending response when minting a
key. A verified human Canvas session can mint directly.

## Use a key

Send the key as a bearer token to that tenant host:

```bash
curl \
  -H "Authorization: Bearer $MOLECULE_ORG_TOKEN" \
  "https://acme.moleculesai.app/workspaces"
```

```python
import os
import requests

response = requests.get(
    "https://acme.moleculesai.app/workspaces",
    headers={"Authorization": f"Bearer {os.environ['MOLECULE_ORG_TOKEN']}"},
    timeout=30,
)
response.raise_for_status()
print(response.json())
```

## Scope

A current organization key can administer the tenant surface, including
workspaces, settings, secrets, plugins, bundles, templates, approvals, and org
keys. It can call ordinary workspace lifecycle endpoints such as restart,
pause, and resume when those routes accept tenant authentication.

It cannot:

- authenticate to control-plane admin, billing, or membership APIs;
- access another tenant;
- mutate infrastructure-only workspace fields that require a verified session
  or bootstrap admin credential.

Session and org-key access therefore overlap, but are not identical.

For calls made by one workspace to its own scoped routes, use that workspace's
token instead. It has a smaller blast radius than an organization key.

## Rotation and expiry

- Give each integration a separate, descriptive key.
- Set `expires_at` when the integration has a known lifetime.
- Revoke a leaked or unused key immediately in Settings.
- Never commit a key, paste it into logs, or embed it in source code.

Expired and revoked keys are rejected on the next request. Keys without an
expiry remain valid until revoked. Every current org key is full tenant admin;
read-only roles and per-workspace org-key scopes are not implemented.

For the implementation and threat model, see
[Organization API keys](../architecture/org-api-keys.md).
