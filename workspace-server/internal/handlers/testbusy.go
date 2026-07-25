package handlers

// Test-only busy-inject seam (task #92 / core#4293).
//
// The ephemeral CP happy-path gate's step 10b must force-hibernate a workspace
// that is GENUINELY BUSY (active_tasks>0), because force-hibernate is a no-op on
// an idle workspace and can therefore never prove the #4293 force-on-busy fix.
// Today active_tasks/is_busy is fed EXCLUSIVELY by the agent heartbeat, which
// self-reports 0 unless a real LLM turn is in flight — so the gate had to drive
// a real long turn and poll, an inherently flaky, non-deterministic mechanism.
//
// This seam lets a build-tagged tenant image (`-tags e2e_busy_inject`, built
// ONLY for the throwaway ephemeral gate) expose a test route that pins a
// workspace's active_tasks to a chosen floor WITHOUT a real turn. See
// testbusy_enabled.go for the implementation and testbusy_disabled.go for the
// production no-op.
//
// SAFETY / prod-unreachability: the two hook points below are the ONLY
// references to test-busy logic in code that ships. Both are inert in every
// normal build:
//
//   - testBusyActiveTasksHook is nil (never assigned) unless the e2e_busy_inject
//     build tag is present, so the heartbeat handler's active_tasks bind is
//     byte-for-byte the runtime-reported value in production.
//   - RegisterTestBusyRoutes is the no-op stub in testbusy_disabled.go, so NO
//     test route is registered — the endpoint returns Gin's standard 404 in
//     every shipped image, exactly as if this code did not exist.
//
// The mutating route + the non-nil hook exist ONLY in a binary compiled with the
// tag, and that binary is produced ONLY by the gate's `--build-arg
// BUILD_TAGS=e2e_busy_inject` tenant build. No production or staging image is
// ever built with the tag, so the lever is unreachable there at COMPILE time —
// a stronger guarantee than a runtime env flag, whose code always ships.

// testBusyActiveTasksHook, when non-nil, rewrites the active_tasks value the
// heartbeat handler persists (used to keep an injected busy floor alive across
// heartbeats). It is assigned ONLY by testbusy_enabled.go under the
// e2e_busy_inject build tag; it is nil in every shipped build, so the heartbeat
// path is behaviour-identical in production.
var testBusyActiveTasksHook func(workspaceID string, reported int) int
