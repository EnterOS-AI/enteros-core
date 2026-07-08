package handlers

// platform_agent_flow_test.go — unit tests for the canonical platform-agent
// lifecycle flow (core#3496): the platform-runtime guard, the model field's
// validate→write→provision ordering, the unconditional self-host seed adapter
// (tombstone respected), and the boot provision's unconfigured-skip.

import (
	"bytes"
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// TestPlatformRuntimeAllowed pins the guard table: empty defers to the
// default, container-backed runtimes pass, external-like/mock/unknown are
// rejected (the wedged-external-concierge class, core#3496 audit finding).
func TestPlatformRuntimeAllowed(t *testing.T) {
	cases := []struct {
		runtime string
		want    bool
	}{
		{"", true},
		{"claude-code", true},
		{"codex", true},
		{"google-adk", true},
		{"hermes", true},
		{"openclaw", true},
		{"external", false},
		{"kimi", false},
		{"kimi-cli", false},
		{"mock", false},
		{"not-a-runtime", false},
		{"  external  ", false}, // trimmed before checking
	}
	for _, tc := range cases {
		got, why := platformRuntimeAllowed(tc.runtime)
		if got != tc.want {
			t.Errorf("platformRuntimeAllowed(%q) = %v (%s), want %v", tc.runtime, got, why, tc.want)
		}
		if !got && why == "" {
			t.Errorf("platformRuntimeAllowed(%q): rejection must carry a reason", tc.runtime)
		}
	}
}

// TestEnsurePlatformAgent_RejectsExternalRuntime422: the guard fires BEFORE
// any DB access or side effect — a zero-expectation sqlmock proves no query
// ran, and the install/provision seams prove no side effects.
func TestEnsurePlatformAgent_RejectsExternalRuntime422(t *testing.T) {
	for _, rt := range []string{"external", "kimi", "kimi-cli", "mock", "garbage-rt"} {
		t.Run(rt, func(t *testing.T) {
			mock := setupTestDB(t) // zero expectations: any query would error the test below
			h, cap := ensureTestHandler(t, true)
			w, body := doEnsureRequest(t, h, `{"runtime":"`+rt+`"}`)

			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("expected 422, got %d: %s", w.Code, w.Body.String())
			}
			if body["code"] != "RUNTIME_UNSUPPORTED" {
				t.Errorf("code = %v, want RUNTIME_UNSUPPORTED", body["code"])
			}
			if cap.installCalled || cap.provisionCalled {
				t.Errorf("side effects on a rejected runtime: install=%v provision=%v", cap.installCalled, cap.provisionCalled)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unexpected DB activity: %v", err)
			}
		})
	}
}

// TestInstallPlatformAgent_RejectsExternalRuntime422: the same guard protects
// the CP's row-only shim endpoint.
func TestInstallPlatformAgent_RejectsExternalRuntime422(t *testing.T) {
	setupTestDB(t)
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/admin/org/platform-agent",
		bytes.NewBufferString(`{"id":"11111111-1111-5111-8111-111111111111","runtime":"external"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	InstallPlatformAgent(c)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", w.Code, w.Body.String())
	}
}

// registryModelForRuntime returns a model id the compiled-in registry accepts
// for runtime — keeps these tests robust to catalog changes instead of
// hardcoding a model slug that may rotate out.
func registryModelForRuntime(t *testing.T, runtime string) string {
	t.Helper()
	m, err := providerRegistry()
	if err != nil || m == nil {
		t.Skip("provider registry unavailable in this build")
	}
	ids, err := m.ModelsForRuntime(runtime)
	if err != nil || len(ids) == 0 {
		t.Skipf("no registry models for runtime %q", runtime)
	}
	return ids[0]
}

// TestEnsurePlatformAgent_ModelWrittenBeforeProvision is THE ordering test
// (core#3496): the MODEL workspace_secret must be committed before the
// provision trigger fires, so the very first provision resolves the caller's
// pick instead of racing it into the platform-default fallback.
func TestEnsurePlatformAgent_ModelWrittenBeforeProvision(t *testing.T) {
	model := registryModelForRuntime(t, conciergeDefaultRuntime())

	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT id, COALESCE\(status::text, ''\) FROM workspaces WHERE kind = 'platform'`).
		WillReturnError(sql.ErrNoRows)

	var events []string
	prevSetModel := ensureSetModelFn
	ensureSetModelFn = func(_ context.Context, id, m string) error {
		events = append(events, "model:"+m)
		return nil
	}
	t.Cleanup(func() { ensureSetModelFn = prevSetModel })

	h, cap := ensureTestHandler(t, true)
	prevInstall := ensureInstallFn // ensureTestHandler installed its recorder; wrap it to record ordering too
	ensureInstallFn = func(ctx context.Context, d *sql.DB, id, name, runtime string) error {
		events = append(events, "install")
		return prevInstall(ctx, d, id, name, runtime)
	}
	t.Cleanup(func() { ensureInstallFn = prevInstall })
	h.provisionTriggerOverride = func(id string) {
		events = append(events, "provision")
		cap.provisionCalled = true
	}

	w, body := doEnsureRequest(t, h, `{"model":"`+model+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if body["status"] != "created" {
		t.Fatalf("status = %v, want created", body["status"])
	}
	want := []string{"install", "model:" + model, "provision"}
	if len(events) != len(want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("events = %v, want %v — MODEL must land after install and BEFORE the provision trigger", events, want)
		}
	}
}

// TestEnsurePlatformAgent_InvalidModel422: an unregistered (runtime, model)
// pair is rejected BEFORE any side effect.
func TestEnsurePlatformAgent_InvalidModel422(t *testing.T) {
	if m, err := providerRegistry(); err != nil || m == nil {
		t.Skip("provider registry unavailable in this build")
	}
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT id, COALESCE\(status::text, ''\) FROM workspaces WHERE kind = 'platform'`).
		WillReturnError(sql.ErrNoRows)

	h, cap := ensureTestHandler(t, true)
	w, body := doEnsureRequest(t, h, `{"model":"definitely-not-a-registered-model-xyz"}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", w.Code, w.Body.String())
	}
	if body["code"] != "UNREGISTERED_MODEL_FOR_RUNTIME" {
		t.Errorf("code = %v, want UNREGISTERED_MODEL_FOR_RUNTIME", body["code"])
	}
	if cap.installCalled || cap.provisionCalled {
		t.Errorf("side effects on a rejected model: install=%v provision=%v", cap.installCalled, cap.provisionCalled)
	}
}

// TestEnsurePlatformAgent_ModelValidatedAgainstExistingRuntime: when a root
// already exists, the model is validated against the runtime the row KEEPS
// (the install upsert preserves it), not the payload/default.
func TestEnsurePlatformAgent_ModelValidatedAgainstExistingRuntime(t *testing.T) {
	m, err := providerRegistry()
	if err != nil || m == nil {
		t.Skip("provider registry unavailable in this build")
	}
	claudeIDs, _ := m.ModelsForRuntime("claude-code")
	codexIDs, _ := m.ModelsForRuntime("codex")
	inCodex := map[string]bool{}
	for _, id := range codexIDs {
		inCodex[id] = true
	}
	var claudeOnly string
	for _, id := range claudeIDs {
		if !inCodex[id] {
			claudeOnly = id
			break
		}
	}
	if claudeOnly == "" {
		t.Skip("registry has no claude-code-only model to test the cross-runtime rejection with")
	}

	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT id, COALESCE\(status::text, ''\) FROM workspaces WHERE kind = 'platform'`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow("pa-existing", "failed"))
	mock.ExpectQuery(`SELECT COALESCE\(runtime, ''\) FROM workspaces WHERE id =`).
		WillReturnRows(sqlmock.NewRows([]string{"runtime"}).AddRow("codex"))

	h, cap := ensureTestHandler(t, true)
	w, body := doEnsureRequest(t, h, `{"model":"`+claudeOnly+`"}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 (model %q invalid for the row's codex runtime), got %d: %s", claudeOnly, w.Code, w.Body.String())
	}
	if body["code"] != "UNREGISTERED_MODEL_FOR_RUNTIME" {
		t.Errorf("code = %v, want UNREGISTERED_MODEL_FOR_RUNTIME", body["code"])
	}
	if cap.installCalled {
		t.Error("install ran despite the model rejection")
	}
}

// TestEnsurePlatformAgent_ModelStageError500: a failing MODEL secret write is
// a 500 "model failed" — and the provision trigger must NOT fire after it.
func TestEnsurePlatformAgent_ModelStageError500(t *testing.T) {
	model := registryModelForRuntime(t, conciergeDefaultRuntime())
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT id, COALESCE\(status::text, ''\) FROM workspaces WHERE kind = 'platform'`).
		WillReturnError(sql.ErrNoRows)

	prevSetModel := ensureSetModelFn
	ensureSetModelFn = func(_ context.Context, _, _ string) error { return context.DeadlineExceeded }
	t.Cleanup(func() { ensureSetModelFn = prevSetModel })

	h, cap := ensureTestHandler(t, true)
	w, body := doEnsureRequest(t, h, `{"model":"`+model+`"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
	if body["error"] != "model failed" {
		t.Errorf("error = %v, want \"model failed\"", body["error"])
	}
	if cap.provisionCalled {
		t.Error("provision fired after the model write failed — ordering contract broken")
	}
}

// TestSelfHostPlatformSeedEnabled pins the seed gate: MOLECULE_ORG_ID unset
// (or blank) ⇒ self-host ⇒ seed; set ⇒ SaaS/harness ⇒ never.
func TestSelfHostPlatformSeedEnabled(t *testing.T) {
	t.Setenv("MOLECULE_ORG_ID", "")
	if !SelfHostPlatformSeedEnabled() {
		t.Error("unset MOLECULE_ORG_ID: want seed enabled")
	}
	t.Setenv("MOLECULE_ORG_ID", "   ")
	if !SelfHostPlatformSeedEnabled() {
		t.Error("blank MOLECULE_ORG_ID: want seed enabled")
	}
	t.Setenv("MOLECULE_ORG_ID", "harness-org-alpha")
	if SelfHostPlatformSeedEnabled() {
		t.Error("set MOLECULE_ORG_ID: want seed DISABLED (CP owns creation)")
	}
}

// TestEnsureSelfHostedPlatformAgent_SeedsFreshRoot: the boot seed is a thin
// adapter over the flow — fresh DB ⇒ install with the self-hosted id and the
// platform defaults; row-only (no provision trigger exists to fire).
func TestEnsureSelfHostedPlatformAgent_SeedsFreshRoot(t *testing.T) {
	t.Setenv("MOLECULE_ORG_ID", "")
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT id, COALESCE\(status::text, ''\) FROM workspaces WHERE kind = 'platform'`).
		WillReturnError(sql.ErrNoRows)

	var gotID, gotRuntime string
	prevInstall := ensureInstallFn
	ensureInstallFn = func(_ context.Context, _ *sql.DB, id, name, runtime string) error {
		gotID, gotRuntime = id, runtime
		return nil
	}
	t.Cleanup(func() { ensureInstallFn = prevInstall })

	if err := EnsureSelfHostedPlatformAgent(context.Background(), db.DB); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if gotID != SelfHostedPlatformAgentID {
		t.Errorf("seeded id = %q, want SelfHostedPlatformAgentID %q", gotID, SelfHostedPlatformAgentID)
	}
	if gotRuntime != "" {
		t.Errorf("seeded runtime = %q, want \"\" (installPlatformAgent maps empty to the platform default)", gotRuntime)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestEnsureSelfHostedPlatformAgent_TombstoneRespected: an unattended boot
// must never silently revive a deliberately deleted concierge — the flow's
// SkipTombstoned stops before ANY side effect (no install, no revive UPDATE).
func TestEnsureSelfHostedPlatformAgent_TombstoneRespected(t *testing.T) {
	t.Setenv("MOLECULE_ORG_ID", "")
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT id, COALESCE\(status::text, ''\) FROM workspaces WHERE kind = 'platform'`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow(SelfHostedPlatformAgentID, "removed"))

	prevInstall := ensureInstallFn
	installCalled := false
	ensureInstallFn = func(_ context.Context, _ *sql.DB, _, _, _ string) error {
		installCalled = true
		return nil
	}
	t.Cleanup(func() { ensureInstallFn = prevInstall })

	if err := EnsureSelfHostedPlatformAgent(context.Background(), db.DB); err != nil {
		t.Fatalf("seed on tombstone: %v", err)
	}
	if installCalled {
		t.Error("boot seed re-installed a TOMBSTONED root — deliberate deletion must be respected")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("revive/other DB activity on a tombstoned root: %v", err)
	}
}

// TestEnsureSelfHostedPlatformAgent_ExistingOnlineNoOp: healthy root ⇒ pure
// no-op via the flow's "exists" decision.
func TestEnsureSelfHostedPlatformAgent_ExistingOnlineNoOp(t *testing.T) {
	t.Setenv("MOLECULE_ORG_ID", "")
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT id, COALESCE\(status::text, ''\) FROM workspaces WHERE kind = 'platform'`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow(SelfHostedPlatformAgentID, "online"))

	prevInstall := ensureInstallFn
	installCalled := false
	ensureInstallFn = func(_ context.Context, _ *sql.DB, _, _, _ string) error {
		installCalled = true
		return nil
	}
	t.Cleanup(func() { ensureInstallFn = prevInstall })

	if err := EnsureSelfHostedPlatformAgent(context.Background(), db.DB); err != nil {
		t.Fatalf("seed on online root: %v", err)
	}
	if installCalled {
		t.Error("install ran on a healthy online root")
	}
}

// TestMaybeProvisionPlatformAgentOnBoot_UnconfiguredSkip (core#3496 D2): a
// not-running root with NO model signal (no MODEL secret, no
// MOLECULE_LLM_DEFAULT_MODEL) parks at offline — RestartByID must NOT fire.
func TestMaybeProvisionPlatformAgentOnBoot_UnconfiguredSkip(t *testing.T) {
	t.Setenv("MOLECULE_LLM_DEFAULT_MODEL", "")
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT id, status FROM workspaces WHERE kind = 'platform'`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow("pa-boot", "offline"))
	mock.ExpectQuery(`SELECT 1 FROM workspace_secrets WHERE workspace_id = \$1 AND key = 'MODEL'`).
		WillReturnError(sql.ErrNoRows)

	called := make(chan string, 1)
	MaybeProvisionPlatformAgentOnBoot(context.Background(), db.DB, &stubBootProv{running: false}, func(id string) {
		called <- id
	})
	select {
	case id := <-called:
		t.Fatalf("RestartByID(%q) fired for an UNCONFIGURED root — must park at offline for the onboarding scene", id)
	case <-time.After(150 * time.Millisecond):
		// correct: no provision burned
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestMaybeProvisionPlatformAgentOnBoot_EnvModelConfiguredKicks: the headless
// path — MOLECULE_LLM_DEFAULT_MODEL in env counts as a model signal and the
// kick-off proceeds without touching workspace_secrets.
func TestMaybeProvisionPlatformAgentOnBoot_EnvModelConfiguredKicks(t *testing.T) {
	t.Setenv("MOLECULE_LLM_DEFAULT_MODEL", "claude-opus-4-8")
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT id, status FROM workspaces WHERE kind = 'platform'`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow("pa-boot", "offline"))

	called := make(chan string, 1)
	MaybeProvisionPlatformAgentOnBoot(context.Background(), db.DB, &stubBootProv{running: false}, func(id string) {
		called <- id
	})
	select {
	case id := <-called:
		if id != "pa-boot" {
			t.Errorf("RestartByID(%q), want pa-boot", id)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("RestartByID did not fire for an env-configured root")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestFlowErrorStrings pins the two error types' message shapes (they feed
// logs and wrapped errors).
func TestFlowErrorStrings(t *testing.T) {
	r := &flowReject{Code: "X", Message: "y"}
	if r.Error() != "X: y" {
		t.Errorf("flowReject.Error() = %q", r.Error())
	}
	s := &flowStageError{Stage: "install", Err: context.Canceled}
	if s.Error() != "install failed: context canceled" {
		t.Errorf("flowStageError.Error() = %q", s.Error())
	}
	if s.Unwrap() != context.Canceled {
		t.Error("flowStageError.Unwrap() lost the cause")
	}
}

// TestEnsurePlatformAgent_MalformedBody400: a non-empty malformed body is
// still rejected (the io.EOF tolerance is for EMPTY bodies only).
func TestEnsurePlatformAgent_MalformedBody400(t *testing.T) {
	setupTestDB(t)
	h, _ := ensureTestHandler(t, true)
	w, _ := doEnsureRequest(t, h, `{"runtime": 42`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

// TestEnsurePlatformAgent_RuntimeLookupError500: a failing platformRootRuntime
// read (needed to validate the model against the row's real runtime) is a
// lookup-stage 500.
func TestEnsurePlatformAgent_RuntimeLookupError500(t *testing.T) {
	model := registryModelForRuntime(t, conciergeDefaultRuntime())
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT id, COALESCE\(status::text, ''\) FROM workspaces WHERE kind = 'platform'`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow("pa-existing", "failed"))
	mock.ExpectQuery(`SELECT COALESCE\(runtime, ''\) FROM workspaces WHERE id =`).
		WillReturnError(context.DeadlineExceeded)

	h, _ := ensureTestHandler(t, true)
	w, body := doEnsureRequest(t, h, `{"model":"`+model+`"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
	if body["error"] != "lookup failed" {
		t.Errorf("error = %v, want \"lookup failed\"", body["error"])
	}
}

// TestEnsurePlatformAgent_RuntimeRowGoneFallsToDefault: platformRootRuntime
// hitting ErrNoRows (row raced away) falls back to the default runtime for
// model validation and the repair proceeds.
func TestEnsurePlatformAgent_RuntimeRowGoneFallsToDefault(t *testing.T) {
	model := registryModelForRuntime(t, conciergeDefaultRuntime())
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT id, COALESCE\(status::text, ''\) FROM workspaces WHERE kind = 'platform'`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow("pa-existing", "failed"))
	mock.ExpectQuery(`SELECT COALESCE\(runtime, ''\) FROM workspaces WHERE id =`).
		WillReturnError(sql.ErrNoRows)

	prevSetModel := ensureSetModelFn
	ensureSetModelFn = func(_ context.Context, _, _ string) error { return nil }
	t.Cleanup(func() { ensureSetModelFn = prevSetModel })

	h, cap := ensureTestHandler(t, true)
	w, body := doEnsureRequest(t, h, `{"model":"`+model+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if body["status"] != "repaired" {
		t.Errorf("status = %v, want repaired", body["status"])
	}
	if !cap.installCalled {
		t.Error("install did not run")
	}
}

// TestEnsureSelfHostedPlatformAgent_FlowErrorWrapped: an infrastructure error
// inside the flow surfaces wrapped (non-fatal at the boot call site).
func TestEnsureSelfHostedPlatformAgent_FlowErrorWrapped(t *testing.T) {
	t.Setenv("MOLECULE_ORG_ID", "")
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT id, COALESCE\(status::text, ''\) FROM workspaces WHERE kind = 'platform'`).
		WillReturnError(context.DeadlineExceeded)
	if err := EnsureSelfHostedPlatformAgent(context.Background(), db.DB); err == nil {
		t.Fatal("want wrapped flow error, got nil")
	}
}

// TestEnsurePlatformAgent_ExplicitNameSticksOnExistingRoot: the install upsert
// preserves name on conflict, so the flow must apply an EXPLICIT caller name
// (the scene's fixed "Enter OS Agent") to an existing root itself — before the
// provision trigger, since the {{CONCIERGE_NAME}} substitution reads the row
// name at provision. Found live in the scratch-stack e2e (core#3496).
func TestEnsurePlatformAgent_ExplicitNameSticksOnExistingRoot(t *testing.T) {
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT id, COALESCE\(status::text, ''\) FROM workspaces WHERE kind = 'platform'`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow("pa-existing", "offline"))
	mock.ExpectExec(`UPDATE workspaces SET name = \$2, updated_at = now\(\) WHERE id = \$1 AND name IS DISTINCT FROM \$2`).
		WithArgs("pa-existing", "Enter OS Agent").
		WillReturnResult(sqlmock.NewResult(0, 1))

	h, cap := ensureTestHandler(t, true)
	w, body := doEnsureRequest(t, h, `{"name":"Enter OS Agent"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if body["status"] != "repaired" {
		t.Errorf("status = %v, want repaired", body["status"])
	}
	if !cap.provisionCalled {
		t.Error("provision did not fire after the rename")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("name UPDATE did not run as expected: %v", err)
	}
}

// TestEnsurePlatformAgent_DefaultedNameNeverRenames: an empty payload name
// (boot seed / bare canvas button) must NOT touch an existing root's name.
func TestEnsurePlatformAgent_DefaultedNameNeverRenames(t *testing.T) {
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT id, COALESCE\(status::text, ''\) FROM workspaces WHERE kind = 'platform'`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow("pa-existing", "offline"))
	// NO ExpectExec: any UPDATE would be unexpected DB activity.

	h, _ := ensureTestHandler(t, true)
	w, _ := doEnsureRequest(t, h, `{}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected rename on defaulted name: %v", err)
	}
}

// TestEnsurePlatformAgent_NameStageError500: a failing rename is a 500 "name
// failed" and the provision must NOT fire after it.
func TestEnsurePlatformAgent_NameStageError500(t *testing.T) {
	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT id, COALESCE\(status::text, ''\) FROM workspaces WHERE kind = 'platform'`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).AddRow("pa-existing", "offline"))
	mock.ExpectExec(`UPDATE workspaces SET name = \$2`).
		WillReturnError(context.DeadlineExceeded)

	h, cap := ensureTestHandler(t, true)
	w, body := doEnsureRequest(t, h, `{"name":"Enter OS Agent"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
	if body["error"] != "name failed" {
		t.Errorf("error = %v, want \"name failed\"", body["error"])
	}
	if cap.provisionCalled {
		t.Error("provision fired after the rename failed")
	}
}

// TestConciergeDefaultRuntime_RejectsNonContainerBackedOverride is the
// core#3496-review regression: MOLECULE_DEFAULT_RUNTIME resolves through the
// SAME container-backed guard as an explicit runtime, so a non-container-backed
// override (external-like/mock/unknown) can NOT reach the concierge via the
// empty-runtime (default) path — it falls back to the compiled-in default
// rather than stamping an unprovisionable platform root.
func TestConciergeDefaultRuntime_RejectsNonContainerBackedOverride(t *testing.T) {
	for _, bad := range []string{"external", "kimi", "kimi-cli", "mock", "not-a-runtime"} {
		t.Run(bad, func(t *testing.T) {
			t.Setenv("MOLECULE_DEFAULT_RUNTIME", bad)
			got := conciergeDefaultRuntime()
			if got != defaultConciergeRuntime {
				t.Errorf("MOLECULE_DEFAULT_RUNTIME=%q → conciergeDefaultRuntime()=%q, want the safe fallback %q (a non-container-backed default must never seed the platform root)", bad, got, defaultConciergeRuntime)
			}
			if ok, _ := platformRuntimeAllowed(got); !ok {
				t.Errorf("resolved default %q fails platformRuntimeAllowed — the fallback itself must be container-backed", got)
			}
		})
	}
	t.Setenv("MOLECULE_DEFAULT_RUNTIME", "codex")
	if got := conciergeDefaultRuntime(); got != "codex" {
		t.Errorf("container-backed override codex not honored: got %q", got)
	}
}

// TestConciergeDefaultRuntimeAndTemplateNaming pins the operator ruling
// (core#3496, 2026-07-07: "openclaw should be the default for now") AND the
// decoupling it forced: the "-default" template suffix + system-prompt.md
// delivery are claude-code CONVENTIONS, not default-runtime behavior.
func TestConciergeDefaultRuntimeAndTemplateNaming(t *testing.T) {
	t.Setenv("MOLECULE_DEFAULT_RUNTIME", "")
	if got := conciergeDefaultRuntime(); got != "openclaw" {
		t.Errorf("conciergeDefaultRuntime() = %q, want openclaw (compiled-in fallback)", got)
	}
	cases := map[string]string{
		"":            "openclaw", // empty resolves via the env-aware default
		"openclaw":    "openclaw", // non-claude runtimes: dir == name
		"codex":       "codex",
		"claude-code": "claude-code-default", // the ONE "-default" convention
	}
	for in, want := range cases {
		if got := conciergeBaseTemplateName(in); got != want {
			t.Errorf("conciergeBaseTemplateName(%q) = %q, want %q", in, got, want)
		}
	}
	// An operator env override drives the empty-runtime template pick too.
	t.Setenv("MOLECULE_DEFAULT_RUNTIME", "claude-code")
	if got := conciergeBaseTemplateName(""); got != "claude-code-default" {
		t.Errorf(`conciergeBaseTemplateName("") with MOLECULE_DEFAULT_RUNTIME=claude-code = %q, want claude-code-default`, got)
	}
}
