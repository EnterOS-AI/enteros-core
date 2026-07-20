// Package approvals holds the single source of truth for which destructive
// org operations require a human approval before they execute.
//
// (RFC docs/design/rfc-platform-agent.md — Phase 4)
//
// The org-level platform agent is driven by end-user chat and holds an org-admin
// token, so destructive/irreversible operations it can trigger are gated: the
// handler creates a pending approval and returns it instead of executing, and a
// human decides via the existing approvals subsystem. Keeping the gated-action
// list in ONE map makes the blast-radius boundary auditable in a single place —
// a handler not listed here behaves exactly as before.
package approvals

// Action is the canonical identifier of a gated destructive operation. The same
// string is stored in approval_requests.action so the gate can match a pending/
// approved request to the operation being retried.
type Action string

const (
	ActionDeleteWorkspace Action = "delete_workspace"
	ActionDeprovision     Action = "deprovision_workspace"
	ActionSecretWrite     Action = "secret_write"
	ActionOrgTokenMint    Action = "org_token_mint"
	// ActionPrivilegedDelegate is the single-use grant class for a
	// PRIVILEGED / boundary-crossing delegation (a handoff dispatched by an
	// admin-token or org-token caller). It is NOT a routine intra-org sibling
	// A2A message — those carry no grant and remain fluid. The grant is minted
	// out-of-band as an APPROVED approval_requests row and CONSUMED (single-use)
	// at delegation execution, mirroring the destructive-op gate's
	// verify-before-act consumption. See handlers/approval_gate.go
	// gatePrivilegedDelegation.
	ActionPrivilegedDelegate Action = "privileged_delegate"
)

// gated is the set of actions that require a human approval. Add an entry here
// (and gate the corresponding handler with requireApproval) to expand the
// boundary; remove one to drop a gate. This is the only place the policy lives.
var gated = map[Action]bool{
	ActionSecretWrite:        true,
	ActionOrgTokenMint:       true,
	ActionPrivilegedDelegate: true,
}

// IsGated reports whether action requires a human approval before executing.
func IsGated(action Action) bool {
	return gated[action]
}

// SetGatedForTest overrides whether an action is gated and returns a restore
// func. TEST-ONLY — exported so consumers in OTHER packages (e.g. the
// privileged-delegation gate in handlers/) can negative-control that their gate
// decision is wired through IsGated, the single source of truth, rather than a
// divergent local classifier. Never call from production code.
func SetGatedForTest(action Action, value bool) func() {
	prev, had := gated[action]
	gated[action] = value
	return func() {
		if had {
			gated[action] = prev
		} else {
			delete(gated, action)
		}
	}
}
