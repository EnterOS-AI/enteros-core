package handlers

// workspace_provision_purged_test.go — a workspace purged while its provision
// is in flight does not keep the instance that provision creates.
//
// REPRODUCED on the staging canary, 2026-11-19 06:00Z (agent c03b11b9): a
// restart, and two seconds later DELETE ?purge=true. The delete stopped what
// was on file and removed the row; the restart's own provision was still
// running and created the Deployment again. Whether that Deployment survives
// depends on which side wins: when the provision returns after the row is
// gone, its `UPDATE workspaces SET instance_id` matches no row, reports no
// error, and the instance is running with nothing left that knows about it.
// An integrator hit that ordering on 2026-11-18 (disable, then delete): a pod
// that ran until someone deleted it by hand, and re-created its own workspace
// row by registering.

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
)

// purgedDuringProvisionCPProv is a control plane whose Start succeeds and which
// can stop an instance by id; stopErrs scripts the first calls' errors.
type purgedDuringProvisionCPProv struct {
	stubInstanceIDPersistFailCPProv
	stopped  []string
	stopErrs []error
}

func (s *purgedDuringProvisionCPProv) StopPurgedInstance(_ context.Context, workspaceID, instanceID string) error {
	s.stopped = append(s.stopped, workspaceID+" "+instanceID)
	if n := len(s.stopped); n <= len(s.stopErrs) {
		return s.stopErrs[n-1]
	}
	return nil
}

var _ purgedInstanceStopper = (*purgedDuringProvisionCPProv)(nil)

// provisionCPWithPersistedRows runs provisionWorkspaceCP to the instance-id
// persist, which reports rowsAffected rows.
func provisionCPWithPersistedRows(t *testing.T, wsID string, cp provisioner.CPProvisionerAPI, rowsAffected int64) *captureBroadcaster {
	t.Helper()
	prevDelay := cpStopRetryBaseDelay
	cpStopRetryBaseDelay = time.Millisecond
	t.Cleanup(func() { cpStopRetryBaseDelay = prevDelay })

	t.Setenv("MOLECULE_LLM_BASE_URL", "https://api.example.test/api/v1/internal/llm/openai/v1")
	t.Setenv("MOLECULE_LLM_USAGE_TOKEN", "tenant-admin-token")
	t.Setenv("MOLECULE_DEPLOY_MODE", "self-hosted")

	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM global_secrets`).
		WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"}))
	mock.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM workspace_secrets`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"}))
	// mintWorkspaceSecrets: revoke + issue auth token + inbound secret
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
		WithArgs(wsID, "enteros-ws-purged").
		WillReturnResult(sqlmock.NewResult(0, rowsAffected))

	cap := &captureBroadcaster{}
	handler := NewWorkspaceHandler(cap, nil, "http://localhost:8080", t.TempDir())
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
	return cap
}

func TestProvisionWorkspaceCP_APurgeDuringTheProvisionTearsTheInstanceDown(t *testing.T) {
	cp := &purgedDuringProvisionCPProv{stubInstanceIDPersistFailCPProv: stubInstanceIDPersistFailCPProv{instanceID: "enteros-ws-purged"}}
	cap := provisionCPWithPersistedRows(t, "ws-purged", cp, 0)

	if want := []string{"ws-purged enteros-ws-purged"}; !reflect.DeepEqual(cp.stopped, want) {
		t.Errorf("instances torn down = %v, want %v — the row is gone, so nothing else will ever stop this instance", cp.stopped, want)
	}
	if cap.lastData != nil {
		t.Errorf("a provision-failed event was broadcast for a workspace that no longer exists: %v", cap.lastData)
	}
}

func TestProvisionWorkspaceCP_AWorkspaceThatStillExistsKeepsItsInstance(t *testing.T) {
	cp := &purgedDuringProvisionCPProv{stubInstanceIDPersistFailCPProv: stubInstanceIDPersistFailCPProv{instanceID: "enteros-ws-purged"}}
	cap := provisionCPWithPersistedRows(t, "ws-alive", cp, 1)

	if len(cp.stopped) != 0 {
		t.Errorf("a live workspace's new instance was torn down: %v", cp.stopped)
	}
	if cap.lastData != nil {
		t.Errorf("a failure was broadcast for a healthy provision: %v", cap.lastData)
	}
}

// The teardown is the only thing that will ever stop this instance, so a
// transient control-plane error is retried.
func TestProvisionWorkspaceCP_TheTeardownOfAPurgedWorkspaceIsRetried(t *testing.T) {
	cp := &purgedDuringProvisionCPProv{
		stubInstanceIDPersistFailCPProv: stubInstanceIDPersistFailCPProv{instanceID: "enteros-ws-purged"},
		stopErrs:                        []error{errors.New("cp provisioner: stop: 503"), errors.New("cp provisioner: stop: 503")},
	}
	provisionCPWithPersistedRows(t, "ws-purged", cp, 0)

	if len(cp.stopped) != 3 {
		t.Errorf("teardown attempts = %d (%v), want 3: two refusals, then the one that lands", len(cp.stopped), cp.stopped)
	}
}

// A control plane without the capability (every older test double) is left as
// it was: nothing is called, and nothing panics.
func TestProvisionWorkspaceCP_APurgeWithoutTheTeardownCapabilityIsUnchanged(t *testing.T) {
	cp := &stubInstanceIDPersistFailCPProv{instanceID: "enteros-ws-purged"}
	cap := provisionCPWithPersistedRows(t, "ws-purged", cp, 0)

	if cp.stopCalls != 0 {
		t.Errorf("Stop called %d times; it reads the instance from a row that is gone", cp.stopCalls)
	}
	if cap.lastData != nil {
		t.Errorf("a failure was broadcast for a workspace that no longer exists: %v", cap.lastData)
	}
}
