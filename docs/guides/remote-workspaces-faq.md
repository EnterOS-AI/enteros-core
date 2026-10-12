# External workspaces FAQ

This FAQ describes the current external-workspace contract. The running tenant
generates the authoritative runtime-specific setup material.

## What is an external workspace?

It is a Molecule workspace identity whose agent process runs on compute that the
platform does not provision or manage. It still appears in Canvas and can use
authenticated registration, heartbeat, peer discovery, chat, and A2A delivery.

## How do I create one?

Create an external/remote workspace in Canvas, then run the returned connection
snippet on the external host. The snippet includes the current tenant URL,
workspace ID, credential shape, and runtime-specific instructions. There is no
universal `get.moleculesai.app` installer, `molecule login`, or platform-managed
`molecule update` command in this contract.

## What does the platform manage?

The platform owns the workspace record, issued credential, registry presence,
activity history, and routed messages. It does not install, restart, patch,
back up, or erase the external process or its host.

## How is it authenticated?

The setup material contains a workspace-scoped credential. Registration,
heartbeat, polling, and workspace API calls present that credential as a bearer
token. Treat the generated snippet as secret material: store it with mode 0600,
do not paste it into tickets or chat, and rotate the workspace credential if it
is exposed.

## Does the host need an inbound port?

That depends on delivery mode:

- Poll mode needs outbound HTTPS only. The external process registers without
  a callback URL, heartbeats, and polls its authenticated activity inbox.
- Push mode needs a platform-reachable HTTPS A2A endpoint. Use a correctly
  authenticated tunnel or reverse proxy; do not expose an unauthenticated local
  agent endpoint.

The server-generated instructions choose the supported mode for that runtime.

## Why does Canvas say `queued`?

For a poll-mode target, `queued` means the platform accepted and persisted the
message for the external agent to collect. It is not proof that the agent has
processed or answered it. Verify the external bridge is running, heartbeating,
and polling; completion is demonstrated by the reply returning to Canvas.

## What happens when the laptop sleeps or the process stops?

Heartbeat freshness expires and Canvas reports the workspace offline. Messages
already accepted into a poll-mode inbox remain subject to the server's queue and
retention policy. Restarting the external process should register and heartbeat
again; the platform does not restart it for you.

## Can one host connect more than one workspace?

Yes when the generated runtime integration supports it. Keep each workspace ID
paired with its own credential and tenant URL. Do not reuse one workspace token
for another identity, and do not start duplicate polling bridges for the same
workspace unless the integration explicitly coordinates consumers.

## Does an external workspace inherit the host's files and credentials?

Only the external process can answer that. Molecule does not automatically copy
SSH keys, Git configuration, VPN access, files, or environment variables from a
host. Review the generated integration and the runtime's own sandbox before
granting access.

## How do I verify the setup end to end?

1. Confirm the external process starts without a credential or config error.
2. Confirm registration and recurring heartbeats are successful.
3. Confirm Canvas shows the intended workspace online.
4. Send a small message from Canvas.
5. Confirm the external runtime receives it and a reply returns to Canvas.
6. Restart the bridge once and repeat the exchange to verify durable local
   configuration and clean reconnection.

For the operator flow, see [External Workspace Quickstart](./external-workspace-quickstart.md).
For the full credential and protocol contract, see
[External Agent Registration](./external-agent-registration.md).
