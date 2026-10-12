package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
	"github.com/DATA-DOG/go-sqlmock"
)

// The delete-vs-provision race (RC09), handler side. provisioner.Start is pinned
// in internal/provisioner/start_delete_guard_test.go (a delete landing at any
// point of a Start leaves no container); these tests pin the handler's half of
// the protocol: CascadeDelete must cancel the workspaces' in-flight starts and
// WAIT for them before it tears anything down, and a deleted workspace must
// never be flipped back to 'failed' by the provision it cancelled.

// inflightRecordingProv is a LocalProvisionerAPI that also implements the
// optional CancelInflightStart, records every call in order, and models one
// provision in flight per workspace that needs `unwind` to stop once cancelled.
type inflightRecordingProv struct {
	mu     sync.Mutex
	calls  []string
	unwind time.Duration
}

func (p *inflightRecordingProv) record(s string) {
	p.mu.Lock()
	p.calls = append(p.calls, s)
	p.mu.Unlock()
}

func (p *inflightRecordingProv) CancelInflightStart(workspaceID string, wait time.Duration) bool {
	if wait <= 0 {
		p.record("cancel:" + workspaceID)
		return false // cancelled, still unwinding
	}
	time.Sleep(p.unwind) // the delete WAITS here for the start to return
	p.record("unwound:" + workspaceID)
	return true
}
func (p *inflightRecordingProv) Stop(_ context.Context, id string) error {
	p.record("stop:" + id)
	return nil
}
func (p *inflightRecordingProv) RemoveVolume(_ context.Context, id string) error {
	p.record("remove-volume:" + id)
	return nil
}
func (p *inflightRecordingProv) Start(context.Context, provisioner.WorkspaceConfig) (string, error) {
	panic("inflightRecordingProv.Start not expected")
}
func (p *inflightRecordingProv) IsRunning(context.Context, string) (bool, error) {
	panic("inflightRecordingProv.IsRunning not expected")
}
func (p *inflightRecordingProv) ExecRead(context.Context, string, string) ([]byte, error) {
	panic("inflightRecordingProv.ExecRead not expected")
}
func (p *inflightRecordingProv) VolumeHasFile(context.Context, string, string) (bool, error) {
	panic("inflightRecordingProv.VolumeHasFile not expected")
}
func (p *inflightRecordingProv) WriteAuthTokenToVolume(context.Context, string, string) error {
	panic("inflightRecordingProv.WriteAuthTokenToVolume not expected")
}

var _ provisioner.LocalProvisionerAPI = (*inflightRecordingProv)(nil)

func TestCascadeDelete_WaitsForInflightProvisionsBeforeTeardown(t *testing.T) {
	const root = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	const child = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	mock := setupTestDB(t)
	setupTestRedis(t)
	mock.MatchExpectationsInOrder(false)
	mock.ExpectQuery(`WITH RECURSIVE descendants AS`).
		WithArgs(root).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(child))
	// The rows are marked removed BEFORE any start is cancelled: a start that
	// unwinds earlier would otherwise mark its workspace failed.
	mock.ExpectExec(`UPDATE workspaces SET status = \$1, updated_at = now\(\) WHERE id = ANY`).
		WillReturnResult(sqlmock.NewResult(0, 2))

	h := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	prov := &inflightRecordingProv{unwind: 50 * time.Millisecond}
	h.provisioner = prov

	if _, stopErrs, err := h.CascadeDelete(context.Background(), root, false); err != nil || len(stopErrs) != 0 {
		t.Fatalf("CascadeDelete = (%v, %v)", stopErrs, err)
	}

	got := strings.Join(prov.calls, " ")
	want := fmt.Sprintf("cancel:%[1]s cancel:%[2]s unwound:%[1]s unwound:%[2]s stop:%[2]s remove-volume:%[2]s stop:%[1]s remove-volume:%[1]s", root, child)
	if got != want {
		t.Fatalf("delete call order:\n got: %s\nwant: %s\n"+
			"Every in-flight start must be cancelled (all first, so they unwind concurrently) and have\n"+
			"RETURNED before the first Stop/RemoveVolume — a start still running after the teardown\n"+
			"creates ws-<id> anyway (the RC09 crash-looping orphan).", got, want)
	}
}

// The CP backend (and test stubs) have no local start to cancel: the hook is an
// optional interface, so their deletes are unchanged.
func TestCancelInflightProvisions_SkipsBackendsWithoutTheHook(t *testing.T) {
	h := &WorkspaceHandler{provisioner: &stoppingLocalProv{}}
	h.cancelInflightProvisions([]string{"x"}) // must not panic: no CancelInflightStart method
	(&WorkspaceHandler{}).cancelInflightProvisions([]string{"x"})
}

func TestWorkspaceIsRemoved(t *testing.T) {
	const id = "cccccccc-cccc-cccc-cccc-cccccccccccc"
	q := regexp.QuoteMeta(`SELECT status FROM workspaces WHERE id = $1`)
	for _, tc := range []struct {
		name string
		rows *sqlmock.Rows
		err  error
		want bool
	}{
		{"removed row", sqlmock.NewRows([]string{"status"}).AddRow("removed"), nil, true},
		{"purged row (no row at all)", sqlmock.NewRows([]string{"status"}), nil, true},
		{"live row", sqlmock.NewRows([]string{"status"}).AddRow("provisioning"), nil, false},
		{"failed row", sqlmock.NewRows([]string{"status"}).AddRow("failed"), nil, false},
		// A DB blip must not kill a legitimate provision.
		{"query error", nil, sql.ErrConnDone, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := setupTestDB(t)
			e := mock.ExpectQuery(q).WithArgs(id)
			if tc.err != nil {
				e.WillReturnError(tc.err)
			} else {
				e.WillReturnRows(tc.rows)
			}
			if got := WorkspaceIsRemoved(context.Background(), id); got != tc.want {
				t.Errorf("WorkspaceIsRemoved = %v, want %v", got, tc.want)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Error(err)
			}
		})
	}
}

// markProvisionFailed must never flip a deleted workspace back to 'failed'.
func TestMarkProvisionFailed_NeverResurrectsARemovedWorkspace(t *testing.T) {
	mock := setupTestDB(t)
	mock.ExpectExec(regexp.QuoteMeta(
		`UPDATE workspaces SET status = $3, last_sample_error = $2, updated_at = now() WHERE id = $1 AND status != 'removed'`)).
		WithArgs("ws-gone", "workspace start failed", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	h := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	h.markProvisionFailed(context.Background(), "ws-gone", "workspace start failed", nil)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the failed-status UPDATE is not guarded by status != 'removed': %v", err)
	}
}
