# External Agent Registration Guide

External and BYO-compute workspaces run outside platform-managed compute while
participating in the authenticated workspace hierarchy. The platform owns their
identity, authorization, discovery, inbox, and activity records; the operator
owns the external process and its lifecycle.

## Supported setup path

Use **Canvas → Create workspace → external/remote compute** and run the setup
material returned by that server. The payload is assembled by
`workspace-server/internal/handlers/external_connection.go`, which is the Core
source of truth for runtime-specific commands and reviewed version pins.

The create response contains a `connection` object when the workspace is
created without a pre-existing external URL. It includes the workspace ID,
public platform URL, one-time workspace credential, registry and heartbeat
endpoints, and the snippets appropriate to supported external runtimes.

Do not rebuild those snippets from this page. The target server may use a push,
poll, MCP, or native channel path depending on runtime and release.

## Credential lifecycle

- The plaintext workspace credential is returned on create or rotation only.
  Core stores a verifier, not a recoverable copy.
- `GET /workspaces/:id/external/connection` re-shows the connection material
  without minting a token. Credential sites contain a deliberately
  non-runnable placeholder.
- `POST /workspaces/:id/external/rotate` revokes prior live workspace tokens,
  mints a replacement, and returns newly runnable connection material.
- Both endpoints require authorization for that workspace (or a verified
  control-plane session) and reject non-external-like runtimes.

If a token is lost or suspected to be exposed, rotate it. Do not delete and
recreate the workspace; that would discard stable hierarchy and workspace
identity unnecessarily.

## Registration and liveness

The generated runtime setup registers the issued workspace identity and sends
authenticated heartbeats. Registration may also establish the runtime-specific
delivery mode. Avoid hard-coding heartbeat intervals, offline thresholds, or an
inbound `/a2a` path in operator documentation; those contracts are owned by the
current runtime and Core implementation.

For a generic Python integration, use the SDK classes named in the generated
Python snippet (`RemoteAgentClient` and `A2AServer`) and persist the issued token
through the SDK's supported token-storage path. Do not use removed helpers such
as `fetch_inbound()` from older tutorials.

Push delivery must fail closed. Registration and heartbeats deliver a separate,
non-empty `platform_inbound_secret`; Core sends it on each push as an
`Authorization: Bearer` credential. The generated Python snippet attaches the
server, registers, verifies that this secret was captured, and only then starts
`A2AServer` on its literal `/a2a/inbound` route. A hand-written push server must
persist the secret, compare the bearer in constant time, and reject missing or
incorrect auth before reading or dispatching the request body. Never start a
public listener when the secret is empty.

## Secrets

Workspace-authenticated external processes read merged plaintext values from:

```text
GET /workspaces/:id/secrets/values
Authorization: Bearer <workspace token>
```

`GET /workspaces/:id/secrets` is the metadata/listing surface and must not be
documented as returning decrypted values. Treat the values response as secret
material and keep it out of logs.

## End-to-end verification

After applying the generated setup, verify all of the following against the
target tenant:

1. the process registers as the expected workspace ID;
2. heartbeats move the workspace online in Canvas;
3. a Canvas message reaches the running external session;
4. the reply returns to the originating conversation;
5. peer discovery/delegation obeys the stored `parent_id` hierarchy; and
6. credential rotation invalidates the old token and the replacement setup can
   reconnect.

See [External Workspace Quickstart](./external-workspace-quickstart.md) for the
operator flow and [Workspace Runtime](../agent-runtime/workspace-runtime.md) for
the repository ownership boundary.
