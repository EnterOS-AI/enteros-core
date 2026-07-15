# Secrets key custody

Core returns metadata—not plaintext values—from normal workspace/global secret
list surfaces. At-rest behavior depends on the checked server environment and
`SECRETS_ENCRYPTION_KEY`; it is not an unconditional encryption guarantee.

## Current boundary

- With a valid 32-byte `SECRETS_ENCRYPTION_KEY`, new values are stored as
  AES-256-GCM (`encryption_version=1`). The key comes directly from the
  workspace-server environment and is not stored beside ciphertext.
- In `MOLECULE_ENV=prod` or `production`, `InitStrict` refuses to boot when the
  key is missing or invalid.
- In non-production environments, the implementation deliberately warns and
  permits version-0 plaintext storage when no valid key is configured. Local
  developers who need an encrypted-at-rest test must set the key explicitly.
- Historical version-0 rows remain readable for migration compatibility; the
  schema column name `encrypted_value` does not prove that a row is encrypted.
- Workspace and global secret routes are protected by the router's current auth
  middleware.
- Runtime injection happens through the authenticated provisioning/lifecycle
  boundary. Logs and API responses must not echo values.
- Physical host, VM, database, and cloud-vendor placement are provider-specific.
  Isolation cannot be documented as guaranteed merely because an older design
  used one EC2 instance per tenant.

## Threat model

Compromise of an authorized running workspace-server or its key source can
expose the secrets within that server's authority. A database-only compromise
does not reveal AES-GCM values without the external key, but it does reveal any
non-production or historical version-0 rows. Cross-tenant protection must be
enforced by current identity, storage, key-scope, and provider controls; verify
those controls in the deployed architecture.

Do not copy credentials into chat, commits, fixtures, workflow logs, or local
credential bundles. Retrieve the narrow value from the configured secrets SSOT
only when needed.

The encryption implementation, route wiring, and secret-handler tests are the
authoritative contract.
