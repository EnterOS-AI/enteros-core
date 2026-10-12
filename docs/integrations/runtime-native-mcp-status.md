# Runtime delivery integration ownership

This Core repository does not maintain a static “done/blocked” table for native
runtime delivery. That status changes independently in the runtime package,
channel bridges, and workspace-template repositories, so the old May 2026 table
was removed after its patched-fork, package, and implementation claims drifted.

Use these sources for current behavior:

| Concern | Source of truth |
|---|---|
| shared registration, heartbeat, inbox, MCP tools, and A2A client behavior | `molecule-ai-workspace-runtime` repository and published `molecules-workspace-runtime` package |
| external setup commands and version/source pins | `workspace-server/internal/handlers/external_connection.go` in this repository |
| Claude Code inbound bridge | `claude-channel-molecule` repository and Claude Code workspace template |
| Codex inbound bridge | `codex-channel-molecule` repository and Codex workspace template |
| Hermes platform channel | `hermes-channel-molecule` repository and Hermes workspace template |
| OpenClaw boot and delivery behavior | OpenClaw workspace template; the Core setup payload explicitly states whether the external path is outbound-only |

A runtime stream is not complete merely because code or an image merged. Close
it only after observing the target release boot, register, receive a real
Canvas or peer message in the intended session, return a reply, and survive the
repository's post-merge staging verification.

See [CLI Runtime Boundary](../agent-runtime/cli-runtime.md) and
[External Agent Registration](../guides/external-agent-registration.md).
