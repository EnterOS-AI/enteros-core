package db

// kept_state.go — the two ways a stopped, removed workspace's instance_id is
// cleared, shared by every writer of that column on a removed row (the delete
// path in handlers/workspace_crud.go and the CP orphan sweeper in
// registry/cp_orphan_sweeper.go) so the kept-state reaper
// (registry/cp_kept_state_reaper.go) sees one consistent record
// (molecule-core#5193).
//
// A delete that does not erase stops the workspace KEEP-STATE: on Kubernetes the
// Deployment goes, while the identity claim, the config seed Secret and the
// Service stay. The control plane can erase them later, but only through the
// workspace teardown, which requires the id of the instance that was stopped
// (DELETE /cp/workspaces/:id?instance_id=...&prune=true). instance_id itself
// must be cleared once the compute is stopped (the orphan sweeper keys on it),
// so the id moves to kept_state_instance_id, and kept_state_since records when.
// A teardown that erased keeps nothing.
//
// Both statements set every column from the row's OLD values (Postgres
// evaluates all SET expressions against the row before the update), so the
// order of the assignments does not matter.

// ClearInstanceIDKeepingStateSQL clears instance_id after a keep-state stop and
// remembers the stopped instance for the kept-state reaper. A row with no
// instance id (nothing was ever started, or it was already cleared) keeps
// whatever it remembered before. $1 = workspace id.
const ClearInstanceIDKeepingStateSQL = `UPDATE workspaces SET instance_id = NULL, updated_at = now(), kept_state_instance_id = COALESCE(NULLIF(instance_id, ''), kept_state_instance_id), kept_state_since = CASE WHEN COALESCE(instance_id, '') = '' THEN kept_state_since ELSE now() END WHERE id = $1`

// ClearInstanceIDStateErasedSQL clears instance_id after a teardown that erased
// the workspace's state: there is nothing left for the reaper. Only a teardown
// that had an instance to name reached the control plane's erase, so a row with
// no instance id keeps what it remembered (the reaper still finishes that one).
// $1 = workspace id.
const ClearInstanceIDStateErasedSQL = `UPDATE workspaces SET instance_id = NULL, updated_at = now(), kept_state_instance_id = CASE WHEN COALESCE(instance_id, '') = '' THEN kept_state_instance_id END, kept_state_since = CASE WHEN COALESCE(instance_id, '') = '' THEN kept_state_since END WHERE id = $1`
