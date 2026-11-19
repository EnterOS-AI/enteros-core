package handlers

// workspace_delete_inflight_cp_provision_test.go — the delete-vs-provision race
// on the control-plane backend.
//
// REPRODUCED on the staging canary, 2026-11-19 07:20Z (agent f46781be), with
// every other fix of that day in place: a restart, and 0.6 s later a
// DELETE ?confirm=true&purge=true.
//
//	07:20:05.18  restart: the control plane tears the Deployment down (keep-state)
//	07:20:05.20  restart: the provision goroutine starts; the control plane begins
//	07:20:05.5   delete arrives: rows marked removed, erase sent to the control plane,
//	             which finds no Deployment yet and removes the Service, Secret and claim
//	07:20:06.56  the provision creates the Deployment, the claim and the Service again
//	07:20:06.57  core: "started as machine" (the row is still there, so the
//	             instance id is written to it)
//	07:20:06.93  delete: the row is purged
//
// The pod ran on with nothing that knew about it. Three other timings (0.2 s,
// 1.0 s, 1.4 s) left nothing. The Docker backend closed this race long ago
// (workspace_delete_inflight_provision_test.go): a delete waits for an in-flight
// start to return before it tears anything down, and a start checks that its
// workspace still exists before it creates anything. These tests pin the same
// two halves for the control-plane backend.

import (
	"context"
	"reflect"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
)

// orderedCPProv records, in order, the control-plane calls a delete makes and
// the moment a scripted in-flight provision returns.
type orderedCPProv struct {
	stubInstanceIDPersistFailCPProv
	mu    sync.Mutex
	calls []string
	// startGate, when set, blocks Start until it is closed.
	startGate chan struct{}
	started   chan struct{}
}

func (p *orderedCPProv) record(s string) {
	p.mu.Lock()
	p.calls = append(p.calls, s)
	p.mu.Unlock()
}

func (p *orderedCPProv) snapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.calls...)
}

func (p *orderedCPProv) Start(_ context.Context, cfg provisioner.WorkspaceConfig) (string, error) {
	p.record("start:" + cfg.WorkspaceID)
	if p.started != nil {
		close(p.started)
	}
	if p.startGate != nil {
		<-p.startGate
	}
	return p.instanceID, nil
}

func (p *orderedCPProv) Stop(_ context.Context, id string) error {
	p.record("stop:" + id)
	return nil
}

func (p *orderedCPProv) StopAndPrune(_ context.Context, id string) error {
	p.record("stop-and-prune:" + id)
	return nil
}

// expectCascadeDeleteOfALeaf scripts the two statements CascadeDelete cannot
// proceed without; its best-effort statements fail against the mock and are
// logged.
func expectCascadeDeleteOfALeaf(mock sqlmock.Sqlmock, id string) {
	mock.MatchExpectationsInOrder(false)
	mock.ExpectQuery(`WITH RECURSIVE descendants AS`).
		WithArgs(id).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectExec(`UPDATE workspaces SET status = \$1, updated_at = now\(\) WHERE id = ANY`).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

func TestCascadeDelete_WaitsForAnInflightCPProvisionBeforeTeardown(t *testing.T) {
	const ws = "aaaaaaaa-0c0c-0000-0000-000000000001"
	mock := setupTestDB(t)
	setupTestRedis(t)
	expectCascadeDeleteOfALeaf(mock, ws)

	h := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	cp := &orderedCPProv{}
	h.SetCPProvisioner(cp)

	// A provision of this workspace is in flight and returns 60 ms from now.
	done := trackCPProvision(ws)
	go func() {
		time.Sleep(60 * time.Millisecond)
		cp.record("provision-returned:" + ws)
		done()
	}()

	if _, stopErrs, err := h.CascadeDelete(context.Background(), ws, true); err != nil || len(stopErrs) != 0 {
		t.Fatalf("CascadeDelete = (%v, %v)", stopErrs, err)
	}
	want := []string{"provision-returned:" + ws, "stop-and-prune:" + ws}
	if got := cp.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("delete call order:\n got: %v\nwant: %v\n"+
			"The in-flight provision must have RETURNED before the teardown. A teardown sent while the\n"+
			"control plane is still creating the instance finds nothing to stop, and the instance that\n"+
			"appears a moment later is tracked by a row the purge is about to delete.", got, want)
	}
}

// A provision that does not return (a multi-minute image pull) must not hold a
// delete for ever: after the bound the teardown proceeds, and the provision's
// own checks deal with what it creates.
func TestCascadeDelete_StopsWaitingForACPProvisionThatDoesNotReturn(t *testing.T) {
	const ws = "aaaaaaaa-0c0c-0000-0000-000000000002"
	prev := cpInflightProvisionWait
	cpInflightProvisionWait = 40 * time.Millisecond
	t.Cleanup(func() { cpInflightProvisionWait = prev })

	mock := setupTestDB(t)
	setupTestRedis(t)
	expectCascadeDeleteOfALeaf(mock, ws)

	h := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	cp := &orderedCPProv{}
	h.SetCPProvisioner(cp)

	done := trackCPProvision(ws)
	defer done() // never returns while the delete runs

	began := time.Now()
	if _, stopErrs, err := h.CascadeDelete(context.Background(), ws, false); err != nil || len(stopErrs) != 0 {
		t.Fatalf("CascadeDelete = (%v, %v)", stopErrs, err)
	}
	if waited := time.Since(began); waited < 40*time.Millisecond || waited > 5*time.Second {
		t.Errorf("the delete waited %s for the provision, want about the 40 ms bound", waited)
	}
	if got, want := cp.snapshot(), []string{"stop:" + ws}; !reflect.DeepEqual(got, want) {
		t.Errorf("calls = %v, want %v: after the bound the teardown still runs", got, want)
	}
}

// No provision in flight: a delete does not wait at all.
func TestCascadeDelete_DoesNotWaitWhenNoCPProvisionIsInflight(t *testing.T) {
	const ws = "aaaaaaaa-0c0c-0000-0000-000000000003"
	mock := setupTestDB(t)
	setupTestRedis(t)
	expectCascadeDeleteOfALeaf(mock, ws)

	h := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	cp := &orderedCPProv{}
	h.SetCPProvisioner(cp)

	began := time.Now()
	if _, stopErrs, err := h.CascadeDelete(context.Background(), ws, false); err != nil || len(stopErrs) != 0 {
		t.Fatalf("CascadeDelete = (%v, %v)", stopErrs, err)
	}
	if waited := time.Since(began); waited > 2*time.Second {
		t.Errorf("a delete with nothing in flight took %s", waited)
	}
	if got, want := cp.snapshot(), []string{"stop:" + ws}; !reflect.DeepEqual(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
}

// provisionCPUpToTheRemovedCheck runs provisionWorkspaceCP with the workspace's
// row in rowStatus ("" = no row) at the check made just before the control
// plane is asked to create anything.
func provisionCPUpToTheRemovedCheck(t *testing.T, wsID string, cp provisioner.CPProvisionerAPI, rowStatus string, proceeds bool) {
	t.Helper()
	t.Setenv("MOLECULE_LLM_BASE_URL", "https://api.example.test/api/v1/internal/llm/openai/v1")
	t.Setenv("MOLECULE_LLM_USAGE_TOKEN", "tenant-admin-token")
	t.Setenv("MOLECULE_DEPLOY_MODE", "self-hosted")

	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM global_secrets`).
		WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"}))
	mock.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM workspace_secrets`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"}))
	status := sqlmock.NewRows([]string{"status"})
	if rowStatus != "" {
		status.AddRow(rowStatus)
	}
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT status FROM workspaces WHERE id = $1`)).
		WithArgs(wsID).
		WillReturnRows(status)
	if proceeds {
		mock.ExpectExec(`UPDATE workspace_auth_tokens SET revoked_at`).
			WithArgs(wsID).
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec(`INSERT INTO workspace_auth_tokens`).
			WithArgs(wsID, sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(1, 1))
		mock.ExpectExec(`UPDATE workspaces SET platform_inbound_secret`).
			WithArgs(sqlmock.AnyArg(), wsID).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec(`UPDATE workspaces SET instance_id =`).
			WithArgs(wsID, "enteros-ws-x").
			WillReturnResult(sqlmock.NewResult(0, 1))
	}

	handler := NewWorkspaceHandler(&captureBroadcaster{}, nil, "http://localhost:8080", t.TempDir())
	handler.SetCPProvisioner(cp)
	handler.provisionWorkspaceCP(wsID, "/nonexistent/template", nil, models.CreateWorkspacePayload{
		Name:    wsID,
		Tier:    1,
		Runtime: "claude-code",
		Model:   "minimax/MiniMax-M2.7",
	})
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

func TestProvisionWorkspaceCP_AWorkspaceDeletedBeforeTheStartIsNotProvisioned(t *testing.T) {
	for name, rowStatus := range map[string]string{"removed": "removed", "purged": ""} {
		t.Run(name, func(t *testing.T) {
			cp := &orderedCPProv{stubInstanceIDPersistFailCPProv: stubInstanceIDPersistFailCPProv{instanceID: "enteros-ws-x"}}
			provisionCPUpToTheRemovedCheck(t, "ws-deleted-"+name, cp, rowStatus, false)
			if got := cp.snapshot(); len(got) != 0 {
				t.Errorf("control-plane calls = %v, want none: its workspace is gone, so nothing may be created for it (and no token minted)", got)
			}
		})
	}
}

func TestProvisionWorkspaceCP_AWorkspaceThatExistsIsProvisioned(t *testing.T) {
	cp := &orderedCPProv{stubInstanceIDPersistFailCPProv: stubInstanceIDPersistFailCPProv{instanceID: "enteros-ws-x"}}
	provisionCPUpToTheRemovedCheck(t, "ws-live", cp, "provisioning", true)
	if got, want := cp.snapshot(), []string{"start:ws-live"}; !reflect.DeepEqual(got, want) {
		t.Errorf("control-plane calls = %v, want %v", got, want)
	}
}

// The order of the two steps is the whole protocol. A delete marks its rows
// removed and THEN looks at the in-flight list; so a provision must be on the
// list BEFORE it checks the row. The other way round leaves a gap: the
// provision reads a live row, the delete marks it removed and finds an empty
// list, and the provision goes on to create an instance nobody waits for.
func TestProvisionWorkspaceCP_IsInflightBeforeItChecksThatItsWorkspaceExists(t *testing.T) {
	const ws = "ws-order"
	const checkTakes = 300 * time.Millisecond
	t.Setenv("MOLECULE_LLM_BASE_URL", "https://api.example.test/api/v1/internal/llm/openai/v1")
	t.Setenv("MOLECULE_LLM_USAGE_TOKEN", "tenant-admin-token")
	t.Setenv("MOLECULE_DEPLOY_MODE", "self-hosted")

	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM global_secrets`).
		WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"}))
	mock.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM workspace_secrets`).
		WithArgs(ws).
		WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"}))
	// The existence check is slow, so "before the check" is a window a test can see.
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT status FROM workspaces WHERE id = $1`)).
		WithArgs(ws).
		WillDelayFor(checkTakes).
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("removed"))

	cp := &orderedCPProv{stubInstanceIDPersistFailCPProv: stubInstanceIDPersistFailCPProv{instanceID: "enteros-ws-x"}}
	handler := NewWorkspaceHandler(&captureBroadcaster{}, nil, "http://localhost:8080", t.TempDir())
	handler.SetCPProvisioner(cp)

	began := time.Now()
	finished := make(chan time.Duration, 1)
	go func() {
		handler.provisionWorkspaceCP(ws, "/nonexistent/template", nil, models.CreateWorkspacePayload{
			Name: ws, Tier: 1, Runtime: "claude-code", Model: "minimax/MiniMax-M2.7",
		})
		finished <- time.Since(began)
	}()

	var listedAfter time.Duration = -1
	for listedAfter < 0 {
		select {
		case took := <-finished:
			t.Fatalf("the provision returned after %s without ever being on the in-flight list", took)
		default:
		}
		if len(inflightCPProvisions(ws)) == 1 {
			listedAfter = time.Since(began)
		} else {
			time.Sleep(time.Millisecond)
		}
	}
	var returnedAfter time.Duration
	select {
	case returnedAfter = <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("the provision never returned")
	}
	if returnedAfter-listedAfter < checkTakes*2/3 {
		t.Errorf("the provision was listed %s after it began and returned at %s: it was not on the in-flight list while its %s existence check ran",
			listedAfter, returnedAfter, checkTakes)
	}
	if got := cp.snapshot(); len(got) != 0 {
		t.Errorf("control-plane calls = %v, want none for a removed workspace", got)
	}
}

// A delete can only wait for a provision it can see: the provision is on the
// in-flight list from its first line until it returns.
func TestProvisionWorkspaceCP_IsInflightUntilItReturns(t *testing.T) {
	const ws = "ws-inflight"
	cp := &orderedCPProv{
		stubInstanceIDPersistFailCPProv: stubInstanceIDPersistFailCPProv{instanceID: "enteros-ws-x"},
		startGate:                       make(chan struct{}),
		started:                         make(chan struct{}),
	}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		provisionCPUpToTheRemovedCheck(t, ws, cp, "provisioning", true)
	}()

	select {
	case <-cp.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the provision never reached the control plane")
	}
	if n := len(inflightCPProvisions(ws)); n != 1 {
		t.Errorf("in-flight provisions of %s while Start runs = %d, want 1", ws, n)
	}
	close(cp.startGate)
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("the provision never returned")
	}
	if n := len(inflightCPProvisions(ws)); n != 0 {
		t.Errorf("in-flight provisions of %s after it returned = %d, want 0", ws, n)
	}
}
