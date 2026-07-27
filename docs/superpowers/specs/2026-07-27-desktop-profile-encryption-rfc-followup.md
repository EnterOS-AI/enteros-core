# RFC follow-up: Desktop profile-volume encryption at rest

- **Date filed:** 2026-07-27
- **Status:** Deferred — do NOT action now. Placeholder for a later cycle.
- **Parent design:** `2026-07-27-agent-desktop-sidecar-design.md` §6.4
- **Decision that created this:** Defer secrets-at-rest encryption; ship the honest
  unencrypted posture for now. (User, 2026-07-27.)

## Why deferred

No volume-encryption mechanism exists in the codebase today (grep for
`encrypt|luks|dm-crypt|SECRETS_ENCRYPTION_KEY` over the provisioner package returns
nothing; `platform_inbound_secret` is plaintext-at-rest by design). Building real
at-rest encryption is net-new infrastructure and is **not** a launch blocker for the
agent-desktop feature, because the near-term threat is not host theft:

- The profile volume is per-workspace on an isolated per-workspace network (design §6.1).
- Host-level access is already the tenant's designed privilege reality (T3/T4, design §6.2),
  so at-rest encryption would defend a threat (offline host/volume theft) that is not the
  primary near-term concern.

## What this follow-up must deliver later

- Real per-volume encryption for the `wsdesk-<id>` profile volume — either
  LUKS/dm-crypt with a KMS-held key, or application-layer encryption of the Chrome
  profile — with key custody, rotation, and a wipe/revoke path that also destroys keys.
- Extend the same treatment (or explicitly scope it out) to `platform_inbound_secret`
  and any other plaintext-at-rest secret the desktop path depends on.

## Hard gate

**Revisit and implement before any deployment that handles regulated data** (PII under
GDPR/CCPA, payment data, health data). Until then, operators must be told the profile
volume — including live login cookies — is unencrypted at rest.
