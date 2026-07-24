// Package sessionid is the ONE authority for a workspace's default
// conversation/session id.
//
// A workspace's turns are grouped — in the runtime's session/history AND in the
// Langfuse Traces tab (session_id) — by the a2a message.contextId. For every
// surface that is NOT a user-rotated "New session", that id must be the SAME
// stable per-workspace value so a restart, a plugin install, a first boot, or a
// runtime self-wake (idle / harvester / delegation-result / cron / scheduler /
// goal-nudge) all land in the user's existing conversation instead of a fresh,
// throwaway session (the Langfuse 3-session fragmentation, 2026-07-21/24).
//
// This package is that single authority. Every core producer of the id derives
// it from DefaultContextID:
//   - the a2a proxy belt + platform self-turns (handlers.canvasSessionContextID),
//   - the provisioner, which injects it into each workspace container as
//     MOLECULE_DEFAULT_SESSION_CONTEXT_ID so the shared runtime consumes core's
//     value instead of re-deriving the "canvas-" convention on its own.
//
// It is a leaf package (no internal imports) so both handlers and provisioner
// can depend on it without an import cycle. A drift test in each consumer pins
// its output against this function, so the convention can only ever move HERE.
package sessionid

// DefaultSessionContextEnv is the env var the provisioner sets on every
// workspace container to hand the runtime core's authoritative default-session
// id. The shared runtime (molecule_runtime.a2a_client.default_self_turn_context_id)
// reads it at highest precedence and falls back to "canvas-<WORKSPACE_ID>" only
// when it is absent (unit/CLI). Keep this string in sync with the runtime reader.
const DefaultSessionContextEnv = "MOLECULE_DEFAULT_SESSION_CONTEXT_ID"

// DefaultContextID returns the stable default conversation/session id for a
// workspace: "canvas-<workspaceID>". The workspace id is a UUID (dash-delimited,
// no colons), so the result survives any runtime session-id sanitisation
// unchanged. An empty workspaceID yields "canvas-" — callers that may hold an
// empty id (none on the production path) should guard before use.
func DefaultContextID(workspaceID string) string {
	return "canvas-" + workspaceID
}
