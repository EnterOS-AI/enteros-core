package models

// Enter OS Light (contracts C3/C5). A Light agent is a parked agent whose
// schedules run as isolated copies of it in their own pods. Core keeps its
// state in workspaces.light_state (migration 20261113120000_light):
//
//	NULL     not a Light agent
//	open     awake for editing or chat; its schedules are paused
//	parking  being parked (schedules handed to the control plane, then hibernate)
//	parked   hibernated; its schedules fire as run pods
//	opening  being woken; its schedules were paused first
//
// While parking or parked nothing may wake, restart or revive the agent: every
// such write carries LightWakeAllowedSQL in its WHERE clause.

// Light states.
const (
	LightOpen    = "open"
	LightParking = "parking"
	LightParked  = "parked"
	LightOpening = "opening"
)

// LightStates lists every non-NULL light_state, in the migration's CHECK order.
var LightStates = []string{LightOpen, LightParking, LightParked, LightOpening}

// Run ledger statuses (light_runs.status). running..timed_out are the C3
// statuses a run pod reports; cancelled is core's own, for a run a pause
// deleted before its copy.
const (
	LightRunRunning   = "running"
	LightRunSucceeded = "succeeded"
	LightRunFailed    = "failed"
	LightRunTimedOut  = "timed_out"
	LightRunCancelled = "cancelled"
)

// LightRunStatuses lists every light_runs.status, in the migration's CHECK order.
var LightRunStatuses = []string{LightRunRunning, LightRunSucceeded, LightRunFailed, LightRunTimedOut, LightRunCancelled}

// LightWakeAllowedSQL is the one predicate every wake, restart and revive
// write carries: it matches no row whose Light agent is parking or parked.
const LightWakeAllowedSQL = `COALESCE(light_state, '') NOT IN ('parking', 'parked')`

// LightWakeAllowedSQLFor is LightWakeAllowedSQL on a table alias ("" = none).
func LightWakeAllowedSQLFor(alias string) string {
	if alias == "" {
		return LightWakeAllowedSQL
	}
	return "COALESCE(" + alias + ".light_state, '') NOT IN ('parking', 'parked')"
}

// LightBlocksWake reports whether a workspace in this Light state must not be
// woken, restarted, resumed, paused, drained, heartbeated or registered.
func LightBlocksWake(state string) bool {
	return state == LightParking || state == LightParked
}
