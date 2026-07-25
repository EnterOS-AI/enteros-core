// Package sessionid is the ONE authority for a workspace's default
// conversation/session id.
//
// A workspace's canvas / restart-context / first-boot turns are grouped — in the
// runtime's session/history AND in the Langfuse Traces tab (session_id) — by the
// a2a message.contextId. For every surface that is NOT a user-rotated "New
// session", that id must be the SAME stable per-workspace value so a restart, a
// plugin install, or a first boot lands in the user's existing conversation
// instead of a fresh, throwaway session (the Langfuse 3-session fragmentation,
// 2026-07-21/24).
//
// This package is that single authority for the PLATFORM side: the a2a proxy
// belt + platform self-turns (handlers.canvasSessionContextID) derive their
// contextId from DefaultContextID. Runtime-internal self-wakes (idle / harvester
// / delegation-result / cron / scheduler / goal-nudge / reprovision) keep their
// OWN routing context_id and converge only their Langfuse session_id, done
// runtime-side in tracing.TracingExecutor; that convergence mirrors this same
// "canvas-<ws>" convention (molecule_runtime/tracing.py). The two are kept equal
// by the shared literal — there is deliberately no cross-container env handshake
// (an earlier MOLECULE_DEFAULT_SESSION_CONTEXT_ID injection was removed: it let
// the runtime's id diverge from this platform id, re-opening fragmentation).
//
// It is a leaf package (no internal imports) so both handlers and provisioner
// can depend on it without an import cycle. A drift test in handlers pins the
// belt's output against this function, so the platform convention moves only HERE.
package sessionid

// DefaultContextID returns the stable default conversation/session id for a
// workspace: "canvas-<workspaceID>". The workspace id is a UUID (dash-delimited,
// no colons), so the result survives any runtime session-id sanitisation
// unchanged. An empty workspaceID yields "canvas-" — callers that may hold an
// empty id (none on the production path) should guard before use.
func DefaultContextID(workspaceID string) string {
	return "canvas-" + workspaceID
}
