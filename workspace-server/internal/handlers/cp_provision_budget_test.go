package handlers

// cp_provision_budget_test.go — the CP-mode provision BUDGET INVERSION, the
// create-path image pre-flight, and the failure reason that reaches the
// customer.
//
// THE BUG. provisionWorkspaceCP bounded its whole provision with
// provisioner.ProvisionTimeout — 3 minutes — on the strength of a docstring
// claiming it bounded a call "which returns quickly". It does not. Downstream
// of it, in series:
//
//	core#5019   provisionHTTPClient.Timeout  = 20m  (a test floors it at 10m)
//	CP main     localRuntimePinPullAbsoluteCap = 30m, stall window 2m
//
// The 3-minute context beat all of them. It cancelled the provision POST, the
// control plane's `docker pull` was torn down WITH it (the CP derives the pull
// context from the request), and the CP logged — this line verbatim from
// molecule-cp-PROD, 2026-08-01 00:10:09 UTC:
//
//	workspace provision (local): local workspace provision: image resolution:
//	pinned workspace image "registry.enteros.ai/…/
//	workspace-template-hermes@sha256:beeceb1a…" is not available locally and
//	pull failed: context canceled
//
// That is workspace c4ebf3dc-96f9-402d-a8f5-8f248b662481, org
// 6372abfc-0673-459c-8bdd-c7864f6c9016 = reno-stars: a confirmed CUSTOMER hit,
// and the ONLY prod occurrence in the 168h Loki window (prod 1, staging 8). The
// staging line the classifier test below is built from is the same failure on
// the other plane — `workspace provision (enteros)`,
// registry.moleculesai.app/…@sha256:87e3c7d1…, 2026-08-06 06:57:23. Do not read
// the staging recurrence as ongoing prod recurrence; it is not.
//
// `context canceled` is a POSITIVE discriminator, not merely the absence of a
// timeout. dockerPullStallAware has three mutually exclusive terminal branches:
// a parent cancellation surfaces ctx.Err(), the 30-minute cap surfaces
// `pull exceeded absolute cap 30m0s`, and the watchdog surfaces `pull stalled`.
// Only the first yields `context canceled` — and a separate staging failure that
// genuinely reads `pull stalled` corroborates that the other branches do report
// themselves distinctly.
//
// workspace-template-hermes is 6.89 GB. A workspace created within a few minutes
// of a runtime-image promote could not transfer the layer delta in 3 minutes, so
// it was marked terminally `failed`.
//
// Retrying is NOT the fix and these tests do not test one: a retry re-pulls
// 6.89 GB into the same 3-minute wall.

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
	"github.com/DATA-DOG/go-sqlmock"
)

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

// budgetProbeCPProv records WHAT was called, IN WHICH ORDER, and WITH WHICH
// DEADLINE. The deadline is the load-bearing capture: the inversion is not
// visible in any return value — the handler happily reports a failure either
// way — it is visible only in the budget the handler hands to the call it is
// waiting on.
type budgetProbeCPProv struct {
	mu    sync.Mutex
	calls []string

	startCtxBudget  time.Duration
	startCtxBounded bool
	ensureCtxBudget time.Duration

	startErr  error
	startID   string
	ensureErr error
	ensureRes provisioner.EnsureImageResult
}

func (s *budgetProbeCPProv) Start(ctx context.Context, _ provisioner.WorkspaceConfig) (string, error) {
	s.mu.Lock()
	s.calls = append(s.calls, "Start")
	if dl, ok := ctx.Deadline(); ok {
		s.startCtxBounded = true
		s.startCtxBudget = time.Until(dl)
	}
	s.mu.Unlock()
	if s.startErr != nil {
		return "", s.startErr
	}
	return s.startID, nil
}

func (s *budgetProbeCPProv) EnsureImage(ctx context.Context, _ provisioner.EnsureImageRequest) (provisioner.EnsureImageResult, error) {
	s.mu.Lock()
	s.calls = append(s.calls, "EnsureImage")
	if dl, ok := ctx.Deadline(); ok {
		s.ensureCtxBudget = time.Until(dl)
	}
	s.mu.Unlock()
	return s.ensureRes, s.ensureErr
}

func (s *budgetProbeCPProv) called() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

func (s *budgetProbeCPProv) Stop(_ context.Context, _ string) error {
	panic("budgetProbeCPProv.Stop not expected on the create path")
}

func (s *budgetProbeCPProv) StopAndPrune(_ context.Context, _ string) error {
	panic("budgetProbeCPProv.StopAndPrune not expected on the create path")
}

func (s *budgetProbeCPProv) GetConsoleOutput(_ context.Context, _ string) (string, error) {
	panic("budgetProbeCPProv.GetConsoleOutput not expected on the create path")
}

func (s *budgetProbeCPProv) IsRunning(_ context.Context, _ string) (bool, error) {
	panic("budgetProbeCPProv.IsRunning not expected on the create path")
}

// captureLastSampleError records the exact string bound to $2 of
// markProvisionFailed's UPDATE — i.e. the value that lands in
// workspaces.last_sample_error and is read back by the canvas. Asserting on the
// broadcast alone would not prove the DB column, and the column is the half the
// customer's card renders.
type captureLastSampleError struct{ got *string }

func (c captureLastSampleError) Match(v driver.Value) bool {
	if s, ok := v.(string); ok {
		*c.got = s
	}
	return true
}

// runCreateProvision drives provisionWorkspaceCP over the create path with the
// DB shape the existing #1206 sibling established (two secret SELECTs, then the
// mark-failed UPDATE) and returns whatever reached last_sample_error.
//
// wantFail=false skips the UPDATE expectation for the success path.
func runCreateProvision(t *testing.T, wsID string, payload models.CreateWorkspacePayload, cp provisioner.CPProvisionerAPI, wantFail bool) (lastSampleError string, broadcast map[string]interface{}) {
	t.Helper()
	// Supply the CP proxy env so the platform-managed default does not abort
	// with MISSING_PLATFORM_PROXY (molecule-core#2162).
	t.Setenv("MOLECULE_LLM_BASE_URL", "https://api.example.test/api/v1/internal/llm/openai/v1")
	t.Setenv("MOLECULE_LLM_USAGE_TOKEN", "tenant-admin-token")

	mock := setupTestDB(t)
	mock.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM global_secrets`).
		WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"}))
	mock.ExpectQuery(`SELECT key, encrypted_value, encryption_version FROM workspace_secrets`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"key", "encrypted_value", "encryption_version"}))
	var captured string
	if wantFail {
		mock.ExpectExec(`UPDATE workspaces SET status =`).
			WithArgs(sqlmock.AnyArg(), captureLastSampleError{got: &captured}, sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
	} else {
		// Success path: instance_id persist.
		mock.ExpectExec(`UPDATE workspaces SET instance_id`).
			WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
	}

	cap := &captureBroadcaster{}
	handler := NewWorkspaceHandler(cap, nil, "http://localhost:8080", t.TempDir())
	handler.SetCPProvisioner(cp)
	handler.provisionWorkspaceCP(wsID, "/nonexistent/template", nil, payload)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations not met: %v", err)
	}
	return captured, cap.lastData
}

// createPayload is the shape a fresh org's FIRST workspace arrives in — the
// path a new customer takes, and the one that never called EnsureImage.
func createPayload(name, runtime string) models.CreateWorkspacePayload {
	p := models.CreateWorkspacePayload{
		Name:    name,
		Tier:    1,
		Runtime: runtime,
		// core#2594: a model is required — the provision gate fails closed
		// without one. The slash form derives the platform provider.
		Model:    "minimax/MiniMax-M2.7",
		Template: "workspace-template-" + runtime,
	}
	p.Compute.Provider = "enteros"
	return p
}

// ---------------------------------------------------------------------------
// 1. THE BUDGET INVERSION
// ---------------------------------------------------------------------------

// TestProvisionWorkspaceCP_ContextBudgetCoversTheProvisionClient is the direct
// regression for the inversion. It reads the deadline off the context the
// handler actually hands to cpProv.Start — not a constant, not a helper — and
// requires it to be at least the budget of the client that has to complete
// inside it.
//
// The relation, not the number, is the invariant: a context SMALLER than the
// client budget means the client's timeout can never fire, so widening the
// client (core#5019 → 20m) achieves precisely nothing and the failure surfaces
// as `context canceled` on the control plane mid-pull. Pinning `>= client
// budget` is what makes the inversion impossible to reintroduce silently — a
// future edit that re-pins this ctx to a fixed 3 minutes fails here.
//
// Shaped after provisioner.TestProvisionUsesItsOwnLongerTimeout, which pins the
// same relation one layer down (provision client > general client).
func TestProvisionWorkspaceCP_ContextBudgetCoversTheProvisionClient(t *testing.T) {
	cp := &budgetProbeCPProv{startErr: errors.New("cp provisioner: provision failed (500): upstream")}
	_, _ = runCreateProvision(t, "ws-budget-1", createPayload("ws-budget-1", "hermes"), cp, true)

	if !cp.startCtxBounded {
		t.Fatal("cpProv.Start received a context with NO deadline — the provision must stay bounded")
	}
	want := provisioner.CPProvisionCeiling()
	// Slack absorbs the wall-clock spent between WithTimeout and the deadline
	// read (secret loads, config build, the pre-flight's own leg). It is two
	// orders of magnitude below the gap this test exists to catch: the bug's
	// budget was 3m against a 20m client.
	const slack = 90 * time.Second
	if cp.startCtxBudget < want-slack {
		t.Errorf("provision ctx budget at cpProv.Start = %s, want >= %s (the provision HTTP client's own timeout).\n"+
			"A ctx SMALLER than the client budget cancels the request before the client can time out — "+
			"the control plane sees `context canceled` mid-pull and a 6.89GB post-promote image never lands. "+
			"This is the core#5019/#5020 budget inversion.",
			cp.startCtxBudget.Round(time.Second), want)
	}
}

// TestCPProvisionTimeout_NeverBelowTheProvisionClientBudget pins the resolver
// itself across the runtime shapes it must serve, so the invariant holds for
// runtimes the end-to-end test above does not enumerate.
//
// Mirrors TestDockerProvisionTimeout_* — the Docker-mode sibling made this
// exact migration first, after "capping real builds at 3 min here bricked a
// hermes concierge provision".
func TestCPProvisionTimeout_NeverBelowTheProvisionClientBudget(t *testing.T) {
	h := NewWorkspaceHandler(&captureBroadcaster{}, nil, "http://localhost:8080", t.TempDir())
	floor := provisioner.CPProvisionCeiling()
	for _, runtime := range []string{"hermes", "claude-code", "codex", "openclaw", "", "no-such-runtime"} {
		if got := h.cpProvisionTimeout(runtime); got < floor {
			t.Errorf("cpProvisionTimeout(%q) = %s, want >= %s", runtime, got, floor)
		}
	}
}

// TestCPProvisionTimeout_LongRuntimeDeclarationRaisesTheCeiling proves the
// resolver still RESPECTS a runtime that declares a window longer than the
// client budget, rather than clamping every runtime to one number. Same shape
// as TestDockerProvisionTimeout_LongRuntimeReachesBuildCeiling.
func TestCPProvisionTimeout_LongRuntimeDeclarationRaisesTheCeiling(t *testing.T) {
	long := provisioner.CPProvisionCeiling() + 10*time.Minute
	dir := t.TempDir()
	writeTemplate(t, dir, "template-slowrt", fmt.Sprintf(`
runtime: slowrt
runtime_config:
  provision_timeout_seconds: %d
`, int(long/time.Second)))
	h := &WorkspaceHandler{configsDir: dir}
	if got := h.cpProvisionTimeout("slowrt"); got != long {
		t.Errorf("cpProvisionTimeout(slowrt) = %s, want %s (the declared window wins when it exceeds the floor)", got, long)
	}
}

// TestCPProvisionCeilingLeavesRoomForTheProvisionLeg is the bounding proof for
// the pre-flight added below. The pre-warm and the provision run in SERIES on
// one context; a pre-flight allowed to consume the entire budget would starve
// the provision POST — the same class of mistake as the inversion, one level in.
func TestCPProvisionCeilingLeavesRoomForTheProvisionLeg(t *testing.T) {
	total := provisioner.CPProvisionCeiling()
	prewarm := cpCreatePrewarmBudget(total)
	if prewarm <= 0 {
		t.Fatalf("cpCreatePrewarmBudget(%s) = %s — the pre-flight would never run", total, prewarm)
	}
	if prewarm >= total {
		t.Errorf("cpCreatePrewarmBudget(%s) = %s — leaves the provision POST nothing", total, prewarm)
	}
	if got := total - prewarm; got < cpProvisionLegReserve {
		t.Errorf("provision leg reserve = %s, want >= %s", got, cpProvisionLegReserve)
	}
}

// ---------------------------------------------------------------------------
// 2. EnsureImage ON THE CREATE PATH
// ---------------------------------------------------------------------------

// TestProvisionWorkspaceCP_EnsureImageRunsBeforeStart. EnsureImage exists for
// exactly this — obtaining a newly promoted pin BEFORE the operation that needs
// it — and it was wired ONLY on the restart path
// (workspace_restart.go:ensurePinnedImageBeforeStop). The create path, which is
// what a fresh org and a new customer take, never asked. The control plane's
// own log names that path explicitly: stage=create_workspace.
func TestProvisionWorkspaceCP_EnsureImageRunsBeforeStart(t *testing.T) {
	cp := &budgetProbeCPProv{
		ensureRes: provisioner.EnsureImageResult{Status: "ready", ImageRef: "registry/x@sha256:abc"},
		startID:   "i-created",
	}
	_, broadcast := runCreateProvision(t, "ws-prewarm-1", createPayload("ws-prewarm-1", "hermes"), cp, false)

	// The other half of the negative-control pair: a pull that COMPLETES inside
	// the budget must produce a working workspace, not merely a different error.
	// Without this the pre-flight could decline everything and the decline tests
	// above would all still pass.
	if broadcast != nil {
		t.Errorf("a create whose image pre-warm SUCCEEDED still published a provision failure: %v", broadcast)
	}

	got := cp.called()
	want := []string{"EnsureImage", "Start"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("create-path call order = %v, want %v — the pre-flight must obtain the pinned image BEFORE the provision that needs it", got, want)
	}
	if cp.ensureCtxBudget <= 0 {
		t.Error("the create pre-flight ran on an unbounded context — it must carry its own budget so it cannot starve the provision leg")
	}
	if cp.ensureCtxBudget > provisioner.CPProvisionCeiling() {
		t.Errorf("pre-flight budget %s exceeds the whole provision ceiling %s", cp.ensureCtxBudget, provisioner.CPProvisionCeiling())
	}
}

// TestProvisionWorkspaceCP_PermanentRefusalSkipsStart — a control plane that
// UNDERSTOOD and refused (a digest that is not in the registry) must not be
// followed by a provision certain to fail the same way, and the workspace must
// say so.
func TestProvisionWorkspaceCP_PermanentRefusalSkipsStart(t *testing.T) {
	cp := &budgetProbeCPProv{
		ensureErr: fmt.Errorf("%w (422): digest not found", provisioner.ErrEnsureImagePermanent),
	}
	lastErr, _ := runCreateProvision(t, "ws-prewarm-2", createPayload("ws-prewarm-2", "hermes"), cp, true)

	for _, c := range cp.called() {
		if c == "Start" {
			t.Error("Start ran after a PERMANENT ensure-image refusal — the control plane already answered no")
		}
	}
	if lastErr != provisionFailedImageUnavailable {
		t.Errorf("last_sample_error = %q, want %q", lastErr, provisionFailedImageUnavailable)
	}
}

// TestProvisionWorkspaceCP_UnsupportedCPStillProvisions is the version-skew
// NEGATIVE CONTROL: it varies exactly one input against the test above — the
// error class — and the workspace must still be created. Failing closed on a
// control plane that predates the endpoint would wedge every create on the
// fleet during a rollout, a far larger outage than the one being fixed. This is
// the same deliberate fail-open the restart path documents.
func TestProvisionWorkspaceCP_UnsupportedCPStillProvisions(t *testing.T) {
	cp := &budgetProbeCPProv{
		ensureErr: provisioner.ErrEnsureImageUnsupported,
		startID:   "i-skew",
	}
	_, _ = runCreateProvision(t, "ws-prewarm-3", createPayload("ws-prewarm-3", "hermes"), cp, false)

	if got := cp.called(); len(got) != 2 || got[1] != "Start" {
		t.Errorf("call order = %v, want the provision to proceed on a CP without the endpoint", got)
	}
}

// TestProvisionWorkspaceCP_TransientPrewarmFailureStillProvisions is the second
// negative control, again varying only the error class. A momentarily
// unreachable control plane says NOTHING about the image; refusing to create a
// workspace over it would turn a 60-second CP redeploy into a fleet-wide create
// outage. On the create path there is no running container to protect, and the
// provision leg is itself a real attempt with its own bounded retry — so this
// branch proceeds and logs, rather than declining.
func TestProvisionWorkspaceCP_TransientPrewarmFailureStillProvisions(t *testing.T) {
	cp := &budgetProbeCPProv{
		ensureErr: errors.New("cp provisioner: ensure-image send: connection refused"),
		startID:   "i-transient",
	}
	_, _ = runCreateProvision(t, "ws-prewarm-4", createPayload("ws-prewarm-4", "hermes"), cp, false)

	if got := cp.called(); len(got) != 2 || got[1] != "Start" {
		t.Errorf("call order = %v, want the provision to proceed after a TRANSIENT pre-warm failure", got)
	}
}

// ---------------------------------------------------------------------------
// 3. THE FAILURE REASON THE CUSTOMER SEES
// ---------------------------------------------------------------------------

// TestProvisionWorkspaceCP_ImageFailureIsSpecificAndSanitised is the red-to-
// green for the third change, and it asserts BOTH halves at once:
//
//	SPECIFIC  — the reason names the image, so the gate and the customer's
//	            canvas can tell a pull that did not complete from a bad model id.
//	            Publishing the constant "provisioning failed" for every failure
//	            is why five weeks of this were invisible to both.
//	SANITISED — F1086 / #1206 still holds. The planted error carries a machine
//	            type, an AMI id, VPC/subnet ids and a host path; none may reach
//	            last_sample_error.
//
// The error text is the control plane's REAL prose, copied from CP stdout — the
// STAGING occurrence (2026-08-06, registry.moleculesai.app, sha256:87e3c7d1…),
// which is byte-identical in shape to the prod one and is the line the
// classifier's phrases were derived from. See the file header for the prod
// instance and for why the two must not be conflated.
func TestProvisionWorkspaceCP_ImageFailureIsSpecificAndSanitised(t *testing.T) {
	leaky := errors.New(`cp provisioner: provision failed (500): local workspace provision: image resolution: ` +
		`pinned workspace image "registry.moleculesai.app/molecule-ai/workspace-template-hermes@sha256:87e3c7d1c372418cc97b58a685ac209beb29d66edb8e6d19f97570e2c89a2a03" ` +
		`is not available locally and pull failed: context canceled ` +
		`machine_type=t3.large ami=ami-0abcd1234efgh5678 vpc=vpc-deadbeef subnet=subnet-cafef00d ` +
		`path=/var/lib/docker/overlay2/9f2c host=ip-10-0-3-27.ec2.internal`)

	cp := &budgetProbeCPProv{startErr: leaky}
	lastErr, broadcast := runCreateProvision(t, "ws-reason-1", createPayload("ws-reason-1", "hermes"), cp, true)

	if lastErr == "provisioning failed" {
		t.Errorf("last_sample_error is still the constant %q — a pull that did not complete is indistinguishable "+
			"from every other failure, to the gate and to the customer", lastErr)
	}
	if lastErr != provisionFailedImageUnavailable {
		t.Errorf("last_sample_error = %q, want %q", lastErr, provisionFailedImageUnavailable)
	}
	assertNoLeak(t, lastErr, broadcast)
}

// TestProvisionWorkspaceCP_NonImageFailureKeepsItsOwnMessage is the negative
// control for specificity, varying exactly ONE input — the CP error text. A
// provision that fails for a reason that is NOT the image must still fail, and
// must NOT be relabelled as an image problem. Without this, a classifier that
// returned the image bucket unconditionally would pass the test above.
func TestProvisionWorkspaceCP_NonImageFailureKeepsItsOwnMessage(t *testing.T) {
	cp := &budgetProbeCPProv{startErr: errors.New(
		`cp provisioner: provision failed (400): privileged_env_forbidden machine_type=t3.large vpc=vpc-deadbeef`)}
	lastErr, broadcast := runCreateProvision(t, "ws-reason-2", createPayload("ws-reason-2", "claude-code"), cp, true)

	if lastErr != provisionFailedGeneric {
		t.Errorf("last_sample_error = %q, want the unchanged generic %q — a failure that is NOT about the image "+
			"must keep its own message, or the classifier is just a relabeller",
			lastErr, provisionFailedGeneric)
	}
	assertNoLeak(t, lastErr, broadcast)
}

// TestProvisionWorkspaceCP_RuntimePinMissingIsItsOwnBucket — the CP already
// buckets this one (422 RUNTIME_PIN_MISSING) and it is ACTIONABLE in a
// completely different way from a slow pull: promote a runtime image. Collapsing
// the two into one message would hand an operator the wrong runbook.
func TestProvisionWorkspaceCP_RuntimePinMissingIsItsOwnBucket(t *testing.T) {
	cp := &budgetProbeCPProv{startErr: errors.New(`cp provisioner: provision failed (422): RUNTIME_PIN_MISSING`)}
	lastErr, broadcast := runCreateProvision(t, "ws-reason-3", createPayload("ws-reason-3", "hermes"), cp, true)

	if lastErr != provisionFailedRuntimePinMissing {
		t.Errorf("last_sample_error = %q, want %q", lastErr, provisionFailedRuntimePinMissing)
	}
	assertNoLeak(t, lastErr, broadcast)
}

// TestProvisionWorkspaceCP_CeilingBoundaryIsClassified covers the CLASSIFYING
// half of "what does the caller see at the boundary?" — the ctx, not the pull,
// is what fires, and it must land in its own bucket rather than be mistaken for
// an image problem.
//
// It proves classification ONLY. It injects context.DeadlineExceeded as a VALUE
// while the provision context is still alive, which is not the boundary: at the
// real ceiling the context is EXPIRED, and that is a materially different
// situation for everything downstream of the classifier. The DELIVERY half —
// that the classified reason actually reaches last_sample_error once the
// deadline has blown — is TestMarkProvisionFailed_DeliversThroughAnExpiredContext
// and TestCeilingReasonReachesTheColumn below. Keeping the two apart is the
// point: a test that only ever runs on a live context cannot see the gap where
// the message is computed correctly and then silently dropped.
func TestProvisionWorkspaceCP_CeilingBoundaryIsClassified(t *testing.T) {
	cp := &budgetProbeCPProv{startErr: fmt.Errorf(
		`cp provisioner: send: Post "https://cp.example/cp/workspaces/provision": %w`, context.DeadlineExceeded)}
	lastErr, broadcast := runCreateProvision(t, "ws-reason-4", createPayload("ws-reason-4", "hermes"), cp, true)

	if lastErr != provisionFailedBudgetExhausted {
		t.Errorf("last_sample_error = %q, want %q", lastErr, provisionFailedBudgetExhausted)
	}
	assertNoLeak(t, lastErr, broadcast)
}

// TestCPProvisionFailureReasonsAreClosedSet is the STRUCTURAL sanitisation
// proof, and the reason the redaction cannot rot: the classifier's entire range
// is a fixed set of compile-time constants. It cannot leak a machine type, an
// AMI id or a host path because it never copies any part of the error at all —
// as opposed to a denylist, which only redacts the markers someone remembered.
//
// It is driven with errors built to look like every hostile shape at once.
func TestCPProvisionFailureReasonsAreClosedSet(t *testing.T) {
	allowed := map[string]bool{}
	for _, r := range cpProvisionFailureReasons() {
		allowed[r] = true
	}
	if len(allowed) < 2 {
		t.Fatal("the reason set is degenerate — a single value is the constant this change removes")
	}
	secrets := []string{
		"t3.large", "ami-0abcd1234efgh5678", "vpc-deadbeef", "subnet-cafef00d",
		"/var/lib/docker/overlay2/9f2c", "ip-10-0-3-27.ec2.internal",
		"Bearer sk-live-DEADBEEF", "sha256:87e3c7d1c372418cc97b58a685ac209beb29d66edb8e6d19f97570e2c89a2a03",
	}
	blob := strings.Join(secrets, " ")
	for _, prefix := range []string{
		"cp provisioner: provision failed (500): image resolution: pull failed: context canceled",
		"cp provisioner: provision failed (422): RUNTIME_PIN_MISSING",
		"cp provisioner: provision failed (400): privileged_env_forbidden",
		"cp provisioner: send: context deadline exceeded",
		"pull stalled (no progress for 2m0s)",
		"", // an error carrying NOTHING but the secrets
	} {
		got := cpProvisionFailureReason(errors.New(prefix + " " + blob))
		if !allowed[got] {
			t.Errorf("cpProvisionFailureReason returned %q, which is not one of the declared constants — "+
				"the range must stay closed or sanitisation is a denylist again", got)
		}
		for _, s := range secrets {
			if strings.Contains(got, s) {
				t.Errorf("reason %q leaked %q", got, s)
			}
		}
	}
	if got := cpProvisionFailureReason(nil); !allowed[got] {
		t.Errorf("cpProvisionFailureReason(nil) = %q, not a declared constant", got)
	}
}

// assertNoLeak is the shared F1086 / #1206 assertion: no marker of the kind the
// leak guard exists to stop may appear in the persisted column OR in any string
// of the broadcast payload the canvas renders.
func assertNoLeak(t *testing.T, lastSampleError string, broadcast map[string]interface{}) {
	t.Helper()
	markers := []string{
		"t3.large",                      // machine type
		"ami-0abcd1234efgh5678",         // AMI id
		"vpc-deadbeef",                  // VPC id
		"subnet-cafef00d",               // subnet id
		"/var/lib/docker/overlay2/9f2c", // host path
		"ip-10-0-3-27.ec2.internal",     // internal hostname
		"cp provisioner:",               // raw error head
	}
	values := []string{lastSampleError}
	for _, v := range broadcast {
		if s, ok := v.(string); ok {
			values = append(values, s)
		}
	}
	for _, v := range values {
		for _, m := range markers {
			if strings.Contains(v, m) {
				t.Errorf("leaked %q in %q", m, v)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 4. THE BOUNDARY, FOR REAL: DELIVERY THROUGH AN EXPIRED CONTEXT
// ---------------------------------------------------------------------------

// expiredContext returns a context whose deadline has already passed — the
// state every caller of markProvisionFailed is in when the failure being
// reported IS the deadline.
func expiredContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	t.Cleanup(cancel)
	if ctx.Err() == nil {
		t.Fatal("expiredContext handed back a live context")
	}
	return ctx
}

// TestMarkProvisionFailed_DeliversThroughAnExpiredContext is the mechanism.
//
// database/sql checks the context BEFORE it reaches the driver, so an
// ExecContext on a dead context returns ctx.Err() without executing. Every
// failure whose cause is the deadline therefore arrived here holding the one
// context guaranteed to refuse the write: the row kept status='provisioning',
// last_sample_error stayed NULL, and the workspace with the most specific
// reason to report was the one that reported nothing.
//
// This is the "evidence deleted by the operation you are investigating" shape,
// and it is why a reason constant is not worth anything until the write that
// carries it survives the thing it describes.
func TestMarkProvisionFailed_DeliversThroughAnExpiredContext(t *testing.T) {
	mock := setupTestDB(t)
	var got string
	mock.ExpectExec(`UPDATE workspaces SET status =`).
		WithArgs(sqlmock.AnyArg(), captureLastSampleError{got: &got}, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	h := NewWorkspaceHandler(&captureBroadcaster{}, nil, "http://localhost:8080", t.TempDir())
	h.markProvisionFailed(expiredContext(t), "ws-expired-1", "a reason worth keeping", nil)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the failure was never written: %v", err)
	}
	if got != "a reason worth keeping" {
		t.Errorf("last_sample_error = %q, want the reason the caller passed", got)
	}
}

// TestCeilingReasonReachesTheColumn is the headline case, assembled from the two
// REAL functions rather than from mocks: at the ceiling, cpProvisionFailureReason
// classifies the deadline error and markProvisionFailed must deliver that
// classification on the expired context the ceiling leaves behind.
//
// Without it the whole third change is unreachable exactly where it was written
// to help — the improved message would be computed perfectly and then dropped,
// leaving the customer with a failed workspace and no reason. That is the same
// invisibility as the constant this PR removes, arriving by a different route.
func TestCeilingReasonReachesTheColumn(t *testing.T) {
	mock := setupTestDB(t)
	var got string
	mock.ExpectExec(`UPDATE workspaces SET status =`).
		WithArgs(sqlmock.AnyArg(), captureLastSampleError{got: &got}, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// The error shape the provision client produces when the budget is spent.
	boundaryErr := fmt.Errorf(
		`cp provisioner: send: Post "https://cp.example/cp/workspaces/provision": %w`, context.DeadlineExceeded)

	h := NewWorkspaceHandler(&captureBroadcaster{}, nil, "http://localhost:8080", t.TempDir())
	h.markProvisionFailed(expiredContext(t), "ws-ceiling-1", cpProvisionFailureReason(boundaryErr), nil)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the ceiling verdict never reached the column: %v", err)
	}
	if got != provisionFailedBudgetExhausted {
		t.Errorf("last_sample_error = %q, want %q", got, provisionFailedBudgetExhausted)
	}
}

// TestProvisionFailureWriteContext_LiveContextPassesThroughUnchanged is the
// negative control, varying exactly one input — whether the caller's context has
// failed.
//
// A LIVE context must be handed back AS IS. The recovery exists for a context
// that is already dead; a version that detached unconditionally would silently
// discard every caller's deadline and cancellation, converting bounded failure
// writes into unbounded ones on a shutting-down process. That would be a strictly
// worse bug than the one being fixed, and it would look identical in a test that
// only ever passes an expired context.
func TestProvisionFailureWriteContext_LiveContextPassesThroughUnchanged(t *testing.T) {
	type key struct{}
	want := time.Now().Add(7 * time.Minute)
	live, cancel := context.WithDeadline(context.WithValue(context.Background(), key{}, "v"), want)
	defer cancel()

	got, gotCancel := provisionFailureWriteContext(live)
	defer gotCancel()

	if got != live {
		t.Error("a live caller context was replaced — its deadline and cancellation must keep governing")
	}
	if dl, ok := got.Deadline(); !ok || !dl.Equal(want) {
		t.Errorf("deadline = %v (ok=%v), want the caller's %v", dl, ok, want)
	}
}

// TestProvisionFailureWriteContext_ExpiredIsRecoveredButStillBounded pins BOTH
// halves of the recovery, because either alone is a bug: a replacement that is
// not usable writes nothing, and a replacement with no deadline is an unbounded
// write nobody is waiting for.
//
// It also pins value propagation. context.WithoutCancel is chosen over a bare
// context.Background() precisely so request-scoped values survive; swapping it
// for Background() would pass a test that only checked liveness.
func TestProvisionFailureWriteContext_ExpiredIsRecoveredButStillBounded(t *testing.T) {
	type key struct{}
	dead, cancel := context.WithDeadline(
		context.WithValue(context.Background(), key{}, "carried"), time.Now().Add(-time.Second))
	defer cancel()
	if dead.Err() == nil {
		t.Fatal("precondition: the caller context should have failed")
	}

	got, gotCancel := provisionFailureWriteContext(dead)
	defer gotCancel()

	if got.Err() != nil {
		t.Fatalf("the recovered context is already dead (%v) — the write would still be dropped", got.Err())
	}
	dl, ok := got.Deadline()
	if !ok {
		t.Error("the recovered context has NO deadline — recovering from a dead deadline must not mean running with none")
	} else if remaining := time.Until(dl); remaining <= 0 || remaining > provisionFailureWriteBudget {
		t.Errorf("recovered budget = %s, want (0, %s]", remaining, provisionFailureWriteBudget)
	}
	if got.Value(key{}) != "carried" {
		t.Error("request-scoped values were dropped — context.WithoutCancel exists to keep them")
	}
}
