# Workspace placement boundary

The former org-per-EC2/Railway placement RFC on this path described the May 2026
deployment. That provider-specific topology was superseded by the off-AWS,
domain-only, CI-on-merge rebuild completed in June 2026.

## Current contract

- Molecule Core owns tenant/workspace functional state and authenticated runtime
  coordination.
- The configured local or control-plane backend owns physical workload
  placement.
- Core must not assume a VM type, region, cloud vendor, database vendor, tunnel
  provider, or one-host-per-org layout.
- Tenant and workspace isolation remain required, but must be enforced by the
  active storage, authentication, and backend contracts rather than by a
  historical claim that physical EC2 separation makes application checks
  unnecessary.
- Control-plane billing/provisioning metadata is not a license to move workspace
  memory, files, secrets, or activity into an undocumented cross-tenant store.

## Sources of truth

- Core backend dispatch: `workspace-server/internal/handlers/workspace_dispatchers.go`
- Provisioner interfaces: `workspace-server/internal/provisioner/`
- Route/auth boundaries: `workspace-server/internal/router/router.go`
- Current deployment automation: `.gitea/workflows/`
- Current control-plane implementation: the live `molecule-controlplane` main
  branch and deployed environment

Any new placement invariant must be expressed in code and tests for the active
provider. Dated EC2/Railway postmortems remain historical evidence only.
