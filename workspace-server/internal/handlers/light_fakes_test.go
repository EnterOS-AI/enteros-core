package handlers

// light_fakes_test.go — shared fakes and sqlmock helpers for the Enter OS Light
// tests.

import (
	"context"
	"database/sql/driver"
	"fmt"
	"sync"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
)

// lightTestWS is a canonical workspace id (the Light routes validate UUIDs).
const lightTestWS = "be7540cf-d160-4e2a-9b1c-0123456789ab"

// lightStateQuery is the guard read every Light guard issues.
const lightStateQuery = `SELECT COALESCE\(light_state, ''\) FROM workspaces WHERE id = \$1`

// expectLightState scripts the guard read of workspaceID.
func expectLightState(mock sqlmock.Sqlmock, workspaceID, state string) {
	mock.ExpectQuery(lightStateQuery).
		WithArgs(workspaceID).
		WillReturnRows(sqlmock.NewRows([]string{"light_state"}).AddRow(state))
}

// pqArrayArg matches a pq.Array([]string) argument by its driver value.
type pqArrayArg []string

func (a pqArrayArg) Match(v driver.Value) bool {
	want, err := pq.Array([]string(a)).Value()
	if err != nil {
		return false
	}
	switch got := v.(type) {
	case string:
		return got == fmt.Sprint(want)
	case []byte:
		return string(got) == fmt.Sprint(want)
	}
	return false
}

// fakeLightAPI is a scripted provisioner.LightAPI that records every call.
type fakeLightAPI struct {
	mu    sync.Mutex
	calls []string

	putReq     provisioner.LightPutRequest
	putRes     provisioner.LightPutResult
	putErr     error
	resume     []error // consumed in order; the last one repeats
	resumed    int
	pause      provisioner.LightStateResult
	pauseErr   error
	suspend    provisioner.LightScheduleState
	suspendErr error
	suspended  []string
	runID      string
	runErr     error
	jobs       []provisioner.LightJob
	jobsErr    error
	deleted    provisioner.LightDeleteResult
	deleteErr  error
}

func (f *fakeLightAPI) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeLightAPI) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeLightAPI) PutLight(_ context.Context, workspaceID string, req provisioner.LightPutRequest) (provisioner.LightPutResult, error) {
	f.record("put " + workspaceID)
	f.mu.Lock()
	f.putReq = req
	f.mu.Unlock()
	return f.putRes, f.putErr
}

func (f *fakeLightAPI) ResumeLight(_ context.Context, workspaceID string) (provisioner.LightStateResult, error) {
	f.record("resume " + workspaceID)
	f.mu.Lock()
	defer f.mu.Unlock()
	var err error
	if len(f.resume) > 0 {
		i := f.resumed
		if i >= len(f.resume) {
			i = len(f.resume) - 1
		}
		err = f.resume[i]
	}
	f.resumed++
	return provisioner.LightStateResult{}, err
}

func (f *fakeLightAPI) PauseLight(_ context.Context, workspaceID string) (provisioner.LightStateResult, error) {
	f.record("pause " + workspaceID)
	return f.pause, f.pauseErr
}

func (f *fakeLightAPI) SuspendLightSchedule(_ context.Context, workspaceID, schedule string) (provisioner.LightScheduleState, error) {
	f.record("suspend " + workspaceID + " " + schedule)
	f.mu.Lock()
	f.suspended = append(f.suspended, schedule)
	f.mu.Unlock()
	return f.suspend, f.suspendErr
}

func (f *fakeLightAPI) RunLightNow(_ context.Context, workspaceID, schedule string) (string, error) {
	f.record("run " + workspaceID + " " + schedule)
	return f.runID, f.runErr
}

func (f *fakeLightAPI) ListLightJobs(_ context.Context, workspaceID string) ([]provisioner.LightJob, error) {
	f.record("jobs " + workspaceID)
	if f.jobsErr != nil {
		return nil, f.jobsErr
	}
	if f.jobs == nil {
		return []provisioner.LightJob{}, nil
	}
	return f.jobs, nil
}

func (f *fakeLightAPI) DeleteLight(_ context.Context, workspaceID string) (provisioner.LightDeleteResult, error) {
	f.record("delete " + workspaceID)
	return f.deleted, f.deleteErr
}

var _ provisioner.LightAPI = (*fakeLightAPI)(nil)

// trackedCPCalls lists what a trackingCPProv was asked to do (start, stop,
// prune): empty proves nothing was provisioned or torn down.
func trackedCPCalls(r *trackingCPProv) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, s := range r.started {
		out = append(out, "start "+s)
	}
	for _, s := range r.stopped {
		out = append(out, "stop "+s)
	}
	for _, s := range r.pruned {
		out = append(out, "prune "+s)
	}
	return out
}
