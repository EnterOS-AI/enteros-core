# Workspace lifecycle backends

Core supports a local provisioner and a control-plane provisioner behind shared
workspace lifecycle dispatchers. The control-plane backend is a platform API
boundary; Core must not document its implementation as a fixed EC2, VM, or cloud
vendor contract.

## Dispatcher contract

Handlers use the shared methods on `WorkspaceHandler` rather than calling a
backend directly:

| Operation | Dispatcher |
|---|---|
| Provision | `provisionWorkspaceAuto` / synchronous variant where required |
| Stop | `StopWorkspaceAuto` |
| Restart | `RestartWorkspaceAuto` |
| Backend availability | `HasProvisioner` |

The implementation and source-level architecture gates live in:

- `workspace-server/internal/handlers/workspace_dispatchers.go`
- `workspace-server/internal/handlers/workspace_provision_shared.go`
- `workspace-server/internal/handlers/workspace_provision_auto_test.go`
- `workspace-server/internal/handlers/workspace_provision_shared_test.go`
- `workspace-server/internal/provisioner/`

Do not call the local or control-plane `Start`/`Stop` methods directly from a
new handler. The dispatcher is responsible for selecting the configured
backend and for the no-backend failure contract.

## Capability differences

Substrate-specific features can differ. For example, local container file or
log operations do not imply that the control-plane backend exposes the same
transport. A caller must either use a shared capability surface or return an
explicit unsupported result; it must not silently choose a local-only path.

Tier describes workspace capability/isolation. It does not select local versus
control-plane provisioning and must not be used as a backend discriminator.

## Verification

When changing lifecycle behavior:

1. exercise the dispatcher contract, including no-backend behavior;
2. run the source-level direct-call guards;
3. test every configured backend contract affected by the change;
4. follow the exact post-merge workflow to a terminal result; and
5. validate the relevant staging/runtime health surface rather than treating a
   created row or merged PR as proof of a healthy workspace.

Historical EC2-specific incident notes may remain in dated postmortems, but they
are not the current backend architecture.
