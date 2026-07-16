// Package staginge2e holds live, against-real-staging-infra end-to-end tests
// for molecule-core's workspace-server. The live tests are excluded from the
// normal `go test ./...` run; untagged harness contract tests still run there.
//
// Every live test here is guarded by the `staging_e2e` build tag and skips at
// runtime unless the required staging credentials are present (see
// requireStagingEnv). Hermetic harness contracts run without live credentials.
// So:
//
//	go test ./...                      # runs untagged harness contracts only
//	go test -tags=staging_e2e ./...    # runs tagged contracts; live tests skip LOUD without creds
//	STAGING_E2E=1 CP_BASE_URL=... CP_ADMIN_API_TOKEN=... \
//	  go test -tags=staging_e2e -run TestWorkspaceLifecycle_Staging \
//	  -timeout 40m ./internal/staginge2e/
//
// These tests provision a real throwaway tenant through staging's configured
// control-plane backend, drive the workspace lifecycle endpoints against the
// live tenant workspace-server, and assert observable runtime-state
// transitions (status + serve reachability) — not just HTTP 200. Teardown is
// t.Cleanup-driven (admin DELETE /cp/admin/tenants).
//
// Run them in CI or from an authenticated workstation where the staging
// control-plane admin surface and tenant DNS are reachable.
//
// The tagged suite is not part of ordinary pull-request `go test ./...`.
// `staging-tenant-cd.yml` runs the selected lifecycle tests as a hard post-roll
// staging gate before that workflow reports success.
package staginge2e
