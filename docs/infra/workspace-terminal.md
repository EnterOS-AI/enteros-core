# Workspace terminal boundary

Canvas exposes the workspace terminal through the authenticated
`/workspaces/:id/terminal` WebSocket route. The backend implementation depends
on the configured lifecycle provider.

- A local provider may attach to a local container/process.
- A control-plane provider may proxy a provider-specific remote console.
- An external workspace may not expose a managed terminal at all.

Core must not describe the control-plane terminal as universally using SSH,
EC2 Instance Connect, a particular VM, or a fixed security-group topology.
Provider-specific implementations belong in the current control-plane code and
their dated operational runbooks.

The terminal route remains subject to the router's authentication and workspace
authorization. Terminal access is privileged execution, not merely an
observability view; never expose it through a fail-open or cosmetic Canvas auth
path.

Verify a terminal change against the exact configured provider and an actual
interactive command/exit flow, not only WebSocket upgrade success.
