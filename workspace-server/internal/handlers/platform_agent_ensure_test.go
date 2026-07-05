package handlers

import (
	"bytes"
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// cpDeterministicPlatformAgentID is a verbatim, independent re-implementation of
// the control plane's deterministicPlatformAgentID
// (molecule-controlplane/internal/provisioner/ec2.go) — manual SHA1(namespace ||
// data) + v5/variant bit twiddling + lowercase-hex formatting. It exists ONLY so
// the cross-impl test below can prove core's DeterministicPlatformAgentID
// produces the EXACT same wire id without importing the proprietary CP (core MUST
// NOT depend on the CP). If the CP ever changes its derivation, this golden
// replica is the tripwire that the two have drifted.
func cpDeterministicPlatformAgentID(orgID string) string {
	ns := [16]byte{0x6b, 0xa7, 0xb8, 0x11, 0x9d, 0xad, 0x11, 0xd1, 0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8}
	h := sha1.New()
	h.Write(ns[:])
	h.Write([]byte("molecule-platform-agent:" + orgID))
	sum := h.Sum(nil)
	var u [16]byte
	copy(u[:], sum[:16])
	u[6] = (u[6] & 0x0f) | 0x50 // version 5
	u[8] = (u[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}

var uuidV5Re = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-5[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// TestDeterministicPlatformAgentID_MatchesCPAlgorithm pins the SSOT contract:
// core's id derivation reproduces the CP's byte-for-byte, so a concierge the CP
// installed and one core create/repairs resolve to the same workspace id.
func TestDeterministicPlatformAgentID_MatchesCPAlgorithm(t *testing.T) {
	for _, orgID := range []string{
		"",
		"01ab9bec-0000-0000-0000-000000000000",
		"org-test-123",
		"550e8400-e29b-41d4-a716-446655440000",
	} {
		got := DeterministicPlatformAgentID(orgID)
		want := cpDeterministicPlatformAgentID(orgID)
		if got != want {
			t.Errorf("DeterministicPlatformAgentID(%q) = %q, want (CP algo) %q", orgID, got, want)
		}
		if !uuidV5Re.MatchString(got) {
			t.Errorf("DeterministicPlatformAgentID(%q) = %q is not a lowercase RFC-4122 v5 UUID", orgID, got)
		}
		// Deterministic across calls.
		if again := DeterministicPlatformAgentID(orgID); again != got {
			t.Errorf("DeterministicPlatformAgentID(%q) not stable: %q != %q", orgID, again, got)
		}
	}
}

// TestPlatformAgentID_OrgVsSelfHost: MOLECULE_ORG_ID set -> org-scoped derived id
// (matches the CP install); unset -> the fixed self-host id used by the boot-seed.
func TestPlatformAgentID_OrgVsSelfHost(t *testing.T) {
	t.Run("saas org id set", func(t *testing.T) {
		t.Setenv("MOLECULE_ORG_ID", "org-abc-999")
		if got, want := PlatformAgentID(), DeterministicPlatformAgentID("org-abc-999"); got != want {
			t.Errorf("PlatformAgentID() = %q, want %q", got, want)
		}
	})
	t.Run("self-host org id unset", func(t *testing.T) {
		t.Setenv("MOLECULE_ORG_ID", "")
		if got, want := PlatformAgentID(), SelfHostedPlatformAgentID; got != want {
			t.Errorf("PlatformAgentID() = %q, want SelfHostedPlatformAgentID %q", got, want)
		}
	})
	t.Run("whitespace org id treated as unset", func(t *testing.T) {
		t.Setenv("MOLECULE_ORG_ID", "   ")
		if got, want := PlatformAgentID(), SelfHostedPlatformAgentID; got != want {
			t.Errorf("PlatformAgentID() = %q, want SelfHostedPlatformAgentID %q", got, want)
		}
	})
}

// TestDecideEnsureAction covers the pure create/repair/no-op decision matrix.
func TestDecideEnsureAction(t *testing.T) {
	const derived = "derived-id"
	const existing = "existing-id"
	cases := []struct {
		name                       string
		existingID, existingStatus string
		found, force, hasProv      bool
		wantTarget, wantStatus     string
		wantProvision              bool
		wantRevive                 bool
	}{
		{"missing -> create + provision", "", "", false, false, true, derived, "created", true, false},
		{"missing, no provisioner -> create, no provision", "", "", false, false, false, derived, "created", false, false},
		{"online -> exists no-op", existing, "online", true, false, true, existing, "exists", false, false},
		{"online + force -> repair", existing, "online", true, true, true, existing, "repaired", true, false},
		{"failed -> repair", existing, "failed", true, false, true, existing, "repaired", true, false},
		{"offline -> repair", existing, "offline", true, false, true, existing, "repaired", true, false},
		{"degraded -> repair", existing, "degraded", true, false, true, existing, "repaired", true, false},
		{"degraded, no provisioner -> repair, no provision", existing, "degraded", true, false, false, existing, "repaired", false, false},
		{"online uppercase still healthy", existing, "ONLINE", true, false, true, existing, "exists", false, false},
		// CR2 RC 14676 — a REMOVED (tombstoned) concierge is never 'online', so it
		// always lands on the repair path AND is flagged for an explicit revive
		// (clear the removed flag before provisioning). Force is irrelevant: a
		// removed row is repaired+revived with or without it. Case-insensitive.
		{"removed -> repair + revive", existing, "removed", true, false, true, existing, "repaired", true, true},
		{"removed + force -> repair + revive", existing, "removed", true, true, true, existing, "repaired", true, true},
		{"removed uppercase -> repair + revive", existing, "REMOVED", true, false, true, existing, "repaired", true, true},
		{"removed, no provisioner -> repair + revive, no provision", existing, "removed", true, false, false, existing, "repaired", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decideEnsureAction(derived, tc.existingID, tc.existingStatus, tc.found, tc.force, tc.hasProv)
			if got.targetID != tc.wantTarget || got.status != tc.wantStatus || got.provision != tc.wantProvision || got.revive != tc.wantRevive {
				t.Errorf("decideEnsureAction = %+v, want target=%q status=%q provision=%v revive=%v",
					got, tc.wantTarget, tc.wantStatus, tc.wantProvision, tc.wantRevive)
			}
		})
	}
}

// ensureTestHandler builds a WorkspaceHandler whose install + provision are
// captured (no real Postgres / provisioner). hasProv toggles HasProvisioner via a
// non-nil cpProv sentinel (the established pattern in saas_default_tier_test.go).
func ensureTestHandler(t *testing.T, hasProv bool) (*WorkspaceHandler, *ensureCapture) {
	t.Helper()
	cap := &ensureCapture{}
	prevInstall := ensureInstallFn
	ensureInstallFn = func(_ context.Context, _ *sql.DB, id, name, runtime string) error {
		cap.installCalled = true
		cap.installID = id
		cap.installName = name
		cap.installRuntime = runtime
		return cap.installErr
	}
	t.Cleanup(func() { ensureInstallFn = prevInstall })

	h := &WorkspaceHandler{
		provisionTriggerOverride: func(id string) {
			cap.provisionCalled = true
			cap.provisionID = id
		},
	}
	if hasProv {
		h.cpProv = &trackingCPProv{}
	}
	return h, cap
}

type ensureCapture struct {
	installCalled                          bool
	installID, installName, installRuntime string
	installErr                             error
	provisionCalled                        bool
	provisionID                            string
}

func doEnsureRequest(t *testing.T, h *WorkspaceHandler, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	var reader *bytes.Buffer
	if body != "" {
		reader = bytes.NewBufferString(body)
	} else {
		reader = bytes.NewBuffer(nil)
	}
	c.Request = httptest.NewRequest("POST", "/admin/org/platform-agent/ensure", reader)
	if body != "" {
		c.Request.Header.Set("Content-Type", "application/json")
	}
	h.EnsurePlatformAgent(c)
	var parsed map[string]any
	if w.Body.Len() > 0 {
		_ = json.Unmarshal(w.Body.Bytes(), &parsed)
	}
	return w, parsed
}

// TestEnsurePlatformAgent_HealthyNoOp: an online platform root is a no-op —
// 200 "exists", NO install, NO provision.
func TestEnsurePlatformAgent_HealthyNoOp(t *testing.T) {
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT id, COALESCE\(status, ''\) FROM workspaces WHERE kind = 'platform'`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow("pa-existing", "online"))

	h, cap := ensureTestHandler(t, true)
	w, body := doEnsureRequest(t, h, `{}`)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if body["status"] != "exists" {
		t.Errorf("status = %v, want exists", body["status"])
	}
	if body["platform_agent_id"] != "pa-existing" {
		t.Errorf("platform_agent_id = %v, want pa-existing", body["platform_agent_id"])
	}
	if body["provisioning"] != false {
		t.Errorf("provisioning = %v, want false", body["provisioning"])
	}
	if cap.installCalled {
		t.Error("install must NOT be called for a healthy concierge")
	}
	if cap.provisionCalled {
		t.Error("provision must NOT be triggered for a healthy concierge")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestEnsurePlatformAgent_CreatesWhenMissing: no platform root -> install +
// provision against the CORE-derived (org-scoped) id, with ZERO CP calls.
func TestEnsurePlatformAgent_CreatesWhenMissing(t *testing.T) {
	t.Setenv("MOLECULE_ORG_ID", "org-create-test")
	wantID := DeterministicPlatformAgentID("org-create-test")

	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT id, COALESCE\(status, ''\) FROM workspaces WHERE kind = 'platform'`).
		WillReturnError(sql.ErrNoRows)

	h, cap := ensureTestHandler(t, true)
	w, body := doEnsureRequest(t, h, `{}`)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if body["status"] != "created" {
		t.Errorf("status = %v, want created", body["status"])
	}
	if body["platform_agent_id"] != wantID {
		t.Errorf("platform_agent_id = %v, want derived %q", body["platform_agent_id"], wantID)
	}
	if body["provisioning"] != true {
		t.Errorf("provisioning = %v, want true", body["provisioning"])
	}
	if !cap.installCalled || cap.installID != wantID {
		t.Errorf("install: called=%v id=%q, want called=true id=%q", cap.installCalled, cap.installID, wantID)
	}
	if !cap.provisionCalled || cap.provisionID != wantID {
		t.Errorf("provision: called=%v id=%q, want called=true id=%q", cap.provisionCalled, cap.provisionID, wantID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestEnsurePlatformAgent_RepairsDegraded: a degraded platform root is repaired
// IN PLACE (install + provision against the EXISTING id, never a duplicate).
func TestEnsurePlatformAgent_RepairsDegraded(t *testing.T) {
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT id, COALESCE\(status, ''\) FROM workspaces WHERE kind = 'platform'`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow("pa-existing", "failed"))

	h, cap := ensureTestHandler(t, true)
	w, body := doEnsureRequest(t, h, `{}`)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if body["status"] != "repaired" {
		t.Errorf("status = %v, want repaired", body["status"])
	}
	if cap.installID != "pa-existing" || cap.provisionID != "pa-existing" {
		t.Errorf("repair must target existing id: install=%q provision=%q", cap.installID, cap.provisionID)
	}
	if !cap.installCalled || !cap.provisionCalled {
		t.Errorf("repair must install (%v) and provision (%v)", cap.installCalled, cap.provisionCalled)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestEnsurePlatformAgent_RepairsRemovedConciergeRevives is the CR2 RC 14676 fix:
// a REMOVED (tombstoned) platform root is SELECTED by the ensure lookup (it is not
// filtered out), repaired IN PLACE, and EXPLICITLY revived — the handler runs the
// `UPDATE workspaces SET status='offline' ... AND status='removed'` un-tombstone
// before triggering the provision. Without the revive the install would preserve
// status='removed' and RestartByID would skip the row, so repair would silently
// no-op for exactly the case it exists to handle.
func TestEnsurePlatformAgent_RepairsRemovedConciergeRevives(t *testing.T) {
	mock := setupTestDB(t)
	// The lookup INCLUDES removed roots — it returns the tombstoned concierge.
	mock.ExpectQuery(`SELECT id, COALESCE\(status, ''\) FROM workspaces WHERE kind = 'platform'`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow("pa-removed", "removed"))
	// The explicit revive: clear the removed flag (scoped to status='removed').
	mock.ExpectExec(`UPDATE workspaces SET status = \$2, updated_at = now\(\) WHERE id = \$1 AND status = 'removed'`).
		WithArgs("pa-removed", string(models.StatusOffline)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	h, cap := ensureTestHandler(t, true)
	w, body := doEnsureRequest(t, h, `{}`)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if body["status"] != "repaired" {
		t.Errorf("status = %v, want repaired (removed concierge is repaired+revived)", body["status"])
	}
	if cap.installID != "pa-removed" {
		t.Errorf("repair must target the EXISTING removed id, got install id=%q", cap.installID)
	}
	if !cap.installCalled {
		t.Error("repair must reinstall the removed concierge")
	}
	if !cap.provisionCalled || cap.provisionID != "pa-removed" {
		t.Errorf("revived concierge must be provisioned: called=%v id=%q", cap.provisionCalled, cap.provisionID)
	}
	if body["provisioning"] != true {
		t.Errorf("provisioning = %v, want true", body["provisioning"])
	}
	// The revive UPDATE expectation being met is the assertion that the flag was
	// cleared deliberately.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations (revive UPDATE must run): %v", err)
	}
}

// TestEnsurePlatformAgent_NonRemovedRepairDoesNotRevive guards the inverse: a
// degraded/failed (NOT removed) concierge is repaired WITHOUT any revive UPDATE —
// the un-tombstone is scoped to genuinely-removed rows only. If the handler ran a
// revive here, sqlmock would flag the unexpected UPDATE.
func TestEnsurePlatformAgent_NonRemovedRepairDoesNotRevive(t *testing.T) {
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT id, COALESCE\(status, ''\) FROM workspaces WHERE kind = 'platform'`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow("pa-degraded", "degraded"))
	// NOTE: no ExpectExec for a revive — a non-removed repair must NOT issue one.

	h, cap := ensureTestHandler(t, true)
	w, body := doEnsureRequest(t, h, `{}`)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if body["status"] != "repaired" {
		t.Errorf("status = %v, want repaired", body["status"])
	}
	if !cap.installCalled || !cap.provisionCalled {
		t.Errorf("degraded repair must install (%v) + provision (%v)", cap.installCalled, cap.provisionCalled)
	}
	// ExpectationsWereMet passes iff NO unexpected revive UPDATE was executed.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a non-removed repair must not run a revive UPDATE: %v", err)
	}
}

// TestEnsurePlatformAgent_ReviveFailureIs500 — if the revive UPDATE errors, the
// handler surfaces a 500 and does NOT trigger a provision (a concierge that could
// not be un-tombstoned must not be reported as provisioning).
func TestEnsurePlatformAgent_ReviveFailureIs500(t *testing.T) {
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT id, COALESCE\(status, ''\) FROM workspaces WHERE kind = 'platform'`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow("pa-removed", "removed"))
	mock.ExpectExec(`UPDATE workspaces SET status = \$2, updated_at = now\(\) WHERE id = \$1 AND status = 'removed'`).
		WillReturnError(fmt.Errorf("db down"))

	h, cap := ensureTestHandler(t, true)
	w, _ := doEnsureRequest(t, h, `{}`)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on revive failure, got %d: %s", w.Code, w.Body.String())
	}
	if cap.provisionCalled {
		t.Error("provision must NOT fire when the revive failed")
	}
}

// TestInstallPlatformAgent_PreservesRemovedStatusOnConflict drives the real
// installPlatformAgent transaction through sqlmock and proves the upsert PRESERVES
// the removed flag: the modeled statements are Begin, the kind-downgrade, the
// INSERT…ON CONFLICT upsert, the old-roots SELECT, and Commit — and crucially NO
// `UPDATE … SET status …` statement. sqlmock runs ordered expectations and fails
// on any unexpected query, so the test passing is the proof that install never
// un-tombstones a row as a side-effect (the deliberate revive lives in the ensure
// handler, not here). The post-transaction ROW state is additionally proven on
// real Postgres by TestIntegration_PlatformAgentInstall_PreservesRemovedFlag.
func TestInstallPlatformAgent_PreservesRemovedStatusOnConflict(t *testing.T) {
	mock := setupTestDB(t)

	mock.ExpectBegin()
	// 0. downgrade any other platform root (no-op here, still issued).
	mock.ExpectExec(`UPDATE workspaces SET kind = 'workspace'.*WHERE kind = 'platform' AND parent_id IS NULL AND id <> \$1`).
		WithArgs("pa-removed").
		WillReturnResult(sqlmock.NewResult(0, 0))
	// 1. upsert — the ON CONFLICT clause sets kind/parent_id/template and must NOT
	//    touch status. Matching the DO UPDATE clause here documents that status is
	//    absent from it.
	mock.ExpectExec(`INSERT INTO workspaces .*ON CONFLICT \(id\) DO UPDATE SET\s+kind = 'platform',\s+parent_id = NULL,\s+template = 'platform-agent'`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// 2. capture old roots — none.
	mock.ExpectQuery(`SELECT id FROM workspaces WHERE parent_id IS NULL AND id <> \$1 FOR UPDATE`).
		WithArgs("pa-removed").
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectCommit()

	if err := installPlatformAgent(context.Background(), db.DB, "pa-removed", "Org Concierge", "claude-code"); err != nil {
		t.Fatalf("installPlatformAgent: %v", err)
	}
	// If install had issued any `UPDATE ... SET status` (un-tombstoning the row),
	// sqlmock would have failed on the unexpected query above. Reaching here with
	// all expectations met proves the upsert preserved the removed flag.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet/unexpected sqlmock expectations (install must not mutate status): %v", err)
	}
}

// TestRestartByID_RemovedConciergeSkipped proves fix 3: with a provisioner wired,
// RestartByID of a REMOVED concierge is a no-op. runRestartCycle's lookup filters
// `status NOT IN ('removed','paused','hibernated')`, so a removed row scans to
// ErrNoRows and the cycle returns BEFORE any Stop/provision — the tombstone is
// never restarted. (The earlier provisioner-nil test only covered the early
// return; this one exercises the DB filter with a live provisioner.)
func TestRestartByID_RemovedConciergeSkipped(t *testing.T) {
	mock := setupTestDB(t)
	cp := &trackingCPProv{}
	h := &WorkspaceHandler{cpProv: cp}

	// The removed row is filtered out by the status guard → no rows.
	mock.ExpectQuery(`SELECT name, status, tier,.*WHERE id = \$1 AND status NOT IN \('removed', 'paused', 'hibernated'\)`).
		WithArgs("ws-removed-concierge").
		WillReturnError(sql.ErrNoRows)

	h.RestartByID("ws-removed-concierge")
	drainTestAsync()

	if stops := cp.stoppedSnapshot(); len(stops) != 0 {
		t.Errorf("removed concierge must not be stopped/restarted, got stops: %v", stops)
	}
	if started := cp.startedSnapshot(); len(started) != 0 {
		t.Errorf("removed concierge must not be (re)started, got starts: %v", started)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestEnsurePlatformAgent_ForceRepairsHealthy: force=true repairs even an online
// concierge (the explicit repair-tool path).
func TestEnsurePlatformAgent_ForceRepairsHealthy(t *testing.T) {
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT id, COALESCE\(status, ''\) FROM workspaces WHERE kind = 'platform'`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow("pa-existing", "online"))

	h, cap := ensureTestHandler(t, true)
	w, body := doEnsureRequest(t, h, `{"force":true}`)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if body["status"] != "repaired" {
		t.Errorf("status = %v, want repaired (force)", body["status"])
	}
	if !cap.installCalled || !cap.provisionCalled {
		t.Errorf("force repair must install (%v) and provision (%v)", cap.installCalled, cap.provisionCalled)
	}
}

// TestEnsurePlatformAgent_NoProvisionerStillInstalls: with no backend wired the
// row is still installed but no provision is triggered (provisioning=false).
func TestEnsurePlatformAgent_NoProvisionerStillInstalls(t *testing.T) {
	t.Setenv("MOLECULE_ORG_ID", "")
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT id, COALESCE\(status, ''\) FROM workspaces WHERE kind = 'platform'`).
		WillReturnError(sql.ErrNoRows)

	h, cap := ensureTestHandler(t, false) // no provisioner
	w, body := doEnsureRequest(t, h, `{}`)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if body["status"] != "created" {
		t.Errorf("status = %v, want created", body["status"])
	}
	if body["provisioning"] != false {
		t.Errorf("provisioning = %v, want false (no provisioner)", body["provisioning"])
	}
	if !cap.installCalled {
		t.Error("install must still run with no provisioner")
	}
	if cap.provisionCalled {
		t.Error("provision must NOT be triggered with no provisioner")
	}
	// Self-host create targets the fixed self-host id.
	if cap.installID != SelfHostedPlatformAgentID {
		t.Errorf("self-host create install id = %q, want %q", cap.installID, SelfHostedPlatformAgentID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestEnsurePlatformAgent_InstallErrorIs500: an install failure surfaces as 500
// and does NOT trigger a provision.
func TestEnsurePlatformAgent_InstallErrorIs500(t *testing.T) {
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT id, COALESCE\(status, ''\) FROM workspaces WHERE kind = 'platform'`).
		WillReturnError(sql.ErrNoRows)

	h, cap := ensureTestHandler(t, true)
	cap.installErr = fmt.Errorf("boom")
	w, _ := doEnsureRequest(t, h, `{}`)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
	if cap.provisionCalled {
		t.Error("provision must NOT fire when install failed")
	}
}

// TestEnsurePlatformAgent_EmptyBodyTolerated: the canvas may POST no body — the
// handler must treat it as defaults (not a 400).
func TestEnsurePlatformAgent_EmptyBodyTolerated(t *testing.T) {
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT id, COALESCE\(status, ''\) FROM workspaces WHERE kind = 'platform'`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow("pa-existing", "online"))

	h, _ := ensureTestHandler(t, true)
	w, body := doEnsureRequest(t, h, "") // no body, no Content-Type

	if w.Code != http.StatusOK {
		t.Fatalf("empty body should be 200, got %d: %s", w.Code, w.Body.String())
	}
	if body["status"] != "exists" {
		t.Errorf("status = %v, want exists", body["status"])
	}
}

// TestEnsurePlatformAgent_LookupErrorIs500: a non-ErrNoRows lookup error is a 500
// (no install, no provision).
func TestEnsurePlatformAgent_LookupErrorIs500(t *testing.T) {
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT id, COALESCE\(status, ''\) FROM workspaces WHERE kind = 'platform'`).
		WillReturnError(fmt.Errorf("db down"))

	h, cap := ensureTestHandler(t, true)
	w, _ := doEnsureRequest(t, h, `{}`)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
	if cap.installCalled || cap.provisionCalled {
		t.Error("no install/provision on lookup error")
	}
}
