package handlers

// workspace_cp_inflight_provision.go — the delete-vs-provision race on the
// control-plane backend.
//
// provisionWorkspaceCP runs in its own goroutine (create, restart, wake, a
// Light enable or disable) and spends most of its time inside the control
// plane's provision call. A delete that tears the workspace down while that
// call is in flight tears down nothing: the instance does not exist yet. It
// appears a moment later, its id is written to a row the delete is about to
// purge, and the pod runs on with nothing that knows about it (reproduced on
// the staging canary 2026-11-19; workspace_delete_inflight_cp_provision_test.go
// has the timeline).
//
// The Docker backend closed this race with a cancel-and-wait plus a
// removed-check inside the start (provisioner/start_delete_guard.go). The
// control plane's provision cannot be cancelled from here — the request is
// already being served — so this is the same protocol without the cancel:
//
//   - a provision is on the in-flight list from its first line until it returns,
//     and checks that its workspace still exists before it asks the control
//     plane to create anything;
//   - a delete marks its rows removed FIRST and then waits, bounded, for the
//     in-flight provisions of those rows to return before any teardown.
//
// The order closes the gap between the two: a provision either sees the removed
// row and creates nothing, or it was already on the list when the delete looked,
// and the delete waits for it. The teardown that follows then finds the
// instance, with its id on the row.
//
// What the bound leaves open: a provision still running after
// cpInflightProvisionWait. The delete proceeds; when that provision returns to
// a purged row, provisionWorkspaceCP tears its instance down by id
// (stopInstanceOfPurgedWorkspace), and for a row that is only 'removed' the
// orphan sweeper stops it.

import (
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
)

// cpInflightProvisionWait bounds, in total, how long a delete waits for
// in-flight control-plane provisions. The same bound the Docker path uses
// (inflightProvisionWait): a provision on a warm image returns in a few
// seconds, and a delete must not hang on one that is pulling an image for
// minutes. A package var so tests can shorten it.
var cpInflightProvisionWait = inflightProvisionWait

// cpInflight is the in-flight list: per workspace, one channel per
// provisionWorkspaceCP call that has not returned; a call closes its channel
// when it does. Process-wide, like restartStates: the provisions and the
// deletes of one tenant run in this one process.
var cpInflight = struct {
	sync.Mutex
	byWorkspace map[string][]chan struct{}
}{byWorkspace: map[string][]chan struct{}{}}

// inflightKey is the form of a workspace id the in-flight list is keyed by. A
// request may spell one id several ways (upper case, braces, urn:uuid:, 32 hex
// digits: validateWorkspaceID accepts whatever uuid.Parse does, and Postgres
// compares them as equal), while a provision carries the canonical text the
// database returned. Keyed by the raw string, a delete spelled differently
// would not find the provision it is racing and would not wait for it.
func inflightKey(workspaceID string) string {
	if u, err := uuid.Parse(workspaceID); err == nil {
		return u.String()
	}
	return workspaceID
}

// trackCPProvision puts a control-plane provision of workspaceID on the
// in-flight list. The returned func takes it off again and must be called
// exactly when the provision returns (defer it).
func trackCPProvision(workspaceID string) (done func()) {
	workspaceID = inflightKey(workspaceID)
	ch := make(chan struct{})
	cpInflight.Lock()
	cpInflight.byWorkspace[workspaceID] = append(cpInflight.byWorkspace[workspaceID], ch)
	cpInflight.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			cpInflight.Lock()
			list := cpInflight.byWorkspace[workspaceID]
			for i := range list {
				if list[i] == ch {
					list = append(list[:i], list[i+1:]...)
					break
				}
			}
			if len(list) == 0 {
				delete(cpInflight.byWorkspace, workspaceID)
			} else {
				cpInflight.byWorkspace[workspaceID] = list
			}
			cpInflight.Unlock()
			close(ch)
		})
	}
}

// inflightCPProvisions is a snapshot of workspaceID's in-flight provisions:
// each channel closes when its provision returns.
func inflightCPProvisions(workspaceID string) []<-chan struct{} {
	workspaceID = inflightKey(workspaceID)
	cpInflight.Lock()
	defer cpInflight.Unlock()
	out := make([]<-chan struct{}, 0, len(cpInflight.byWorkspace[workspaceID]))
	for _, ch := range cpInflight.byWorkspace[workspaceID] {
		out = append(out, ch)
	}
	return out
}

// waitInflightCPProvisions waits for the control-plane provisions in flight for
// ids to return, for at most cpInflightProvisionWait in total. Called by
// CascadeDelete after the rows are marked removed, so a provision that starts
// later sees that and creates nothing. Control-plane backend only.
func (h *WorkspaceHandler) waitInflightCPProvisions(ids []string) {
	if h.cpProv == nil {
		return
	}
	deadline := time.NewTimer(cpInflightProvisionWait)
	defer deadline.Stop()
	for _, id := range ids {
		for _, returned := range inflightCPProvisions(id) {
			select {
			case <-returned:
			case <-deadline.C:
				log.Printf("CascadeDelete %s: a control-plane provision is still in flight after %s — tearing down without it; the provision removes what it creates when it returns to a purged row, and the orphan sweeper stops it for a removed one",
					id, cpInflightProvisionWait)
				return
			}
		}
	}
}
