package handlers

// workspace_restart_pull_before_stop_test.go — core#5019, the STRUCTURAL half.
//
// The defect this file pins is an ORDERING one, not a timeout one. PR #5020
// gave the provision POST its own 20m budget (cpProvisionTimeout) so a cold
// ~7GB pull no longer blows a 120s deadline. That removed the failure that
// fired; it did NOT remove the reason it was fatal:
//
//	12:35:30  evt: restart.pre_stop
//	12:35:30  evt: provision.ec2_stopped   <- container destroyed HERE
//	12:35:30  evt: provision.start          <- only NOW is the image needed
//
// The container is destroyed before anything has tried to obtain the pinned
// image. A longer budget means a cold pull usually FINISHES. It does not
// protect against a pull that CANNOT succeed — a bad digest, a registry
// outage, a full disk — because the old container is already gone. The window
// is widest exactly when the operation is most wanted: right after a template
// promote, when by definition no host holds the new digest.
//
// The contract enforced here:
//
//	A cold adoption must be SLOW, never DESTRUCTIVE.
//
// Concretely: on the CP (SaaS) path, no restart may issue the CP stop until
// the control plane has confirmed it can obtain the pinned image for this
// workspace. When it cannot, the restart is DECLINED and the running
// container is left alone.
//
// Three layers, deliberately:
//
//  1. Behaviour — ensurePinnedImageBeforeStop's decision table, and the two
//     restart entry points actually honouring it (no Stop on a declined
//     prewarm; Stop on an allowed one).
//  2. Ordering — an AST gate proving the ensure call PRECEDES the CP stop in
//     every function that issues one, so a future refactor cannot re-order
//     them back into the #5019 shape while every behaviour test stays green.
//  3. Return-honouring — an AST gate proving stopForRestart's decline signal
//     is never discarded (a bare `h.stopForRestart(...)` statement would
//     silently restore the destructive ordering).
//
// Layer 2 exists because layer 1 alone cannot see ordering: a stub that
// records only "was Stop called" passes whether the ensure ran before or
// after it. Same family as TestRestart_CPStopOnlyInsideRetryHelper.

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
)

// --- layer 2: ordering gate -------------------------------------------------

// restartStopSourceFiles are the files that own the restart stop legs.
var restartStopSourceFiles = []string{"workspace_restart.go", "workspace_dispatchers.go"}

// TestRestart_EnsureImagePrecedesCPStop is the #5019 ordering gate.
//
// Every function that issues the CP stop for a RESTART must first call
// ensurePinnedImageBeforeStop, and the call must appear EARLIER in the
// function body than the stop. Position-based rather than
// presence-based on purpose: "both calls exist somewhere in this function"
// is exactly the state #5019 would reach if the ensure were appended after
// the stop, which is the bug.
//
// Exempt: cpStopWithRetry / cpStopWithRetryErr (the retry helpers themselves —
// they are the destination, not a call site) and stopWorkspaceForDelete (the
// DELETE path, whose whole purpose IS to destroy the workspace; pre-warming an
// image for a box that is being torn down would be nonsense).
func TestRestart_EnsureImagePrecedesCPStop(t *testing.T) {
	t.Parallel()

	exempt := map[string]bool{
		"cpStopWithRetry":        true,
		"cpStopWithRetryErr":     true,
		"stopWorkspaceForDelete": true,
	}

	for _, file := range restartStopSourceFiles {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, filepath.Join(".", file), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}

		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || exempt[fn.Name.Name] {
				continue
			}

			stopPos := token.NoPos
			ensurePos := token.NoPos
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch sel.Sel.Name {
				case "cpStopWithRetry", "cpStopWithRetryErr":
					if !stopPos.IsValid() || call.Pos() < stopPos {
						stopPos = call.Pos()
					}
				case "ensurePinnedImageBeforeStop":
					if !ensurePos.IsValid() || call.Pos() < ensurePos {
						ensurePos = call.Pos()
					}
				}
				return true
			})

			if !stopPos.IsValid() {
				continue // this function issues no CP stop
			}
			if !ensurePos.IsValid() {
				t.Errorf(
					"%s:%d %s issues the CP stop with NO ensurePinnedImageBeforeStop guard. "+
						"core#5019: destroying the container before anything has tried to obtain the "+
						"pinned image leaves the workspace with NOTHING when the image cannot be "+
						"obtained (bad digest / registry outage / full disk). A cold adoption must be "+
						"SLOW, never DESTRUCTIVE.",
					file, fset.Position(stopPos).Line, fn.Name.Name,
				)
				continue
			}
			if ensurePos > stopPos {
				t.Errorf(
					"%s:%d %s calls ensurePinnedImageBeforeStop AFTER the CP stop "+
						"(ensure at line %d, stop at line %d). core#5019: the guard only guards when it "+
						"runs FIRST — checking image availability after the container is already gone "+
						"is exactly the sequence that left the workspace down for ~10 minutes.",
					file, fset.Position(stopPos).Line, fn.Name.Name,
					fset.Position(ensurePos).Line, fset.Position(stopPos).Line,
				)
			}
		}
	}
}

// TestRestart_StopForRestartDeclineIsNeverDiscarded pins layer 3.
//
// stopForRestart reports whether it actually stopped. A caller that ignores
// that bool proceeds to re-provision a workspace it never stopped — and, worse,
// re-introduces the #5019 shape from the caller's side even while
// stopForRestart itself is correct. A bare `h.stopForRestart(...)` expression
// statement is therefore a hard failure; the value must be consumed (if, assign,
// return, …).
func TestRestart_StopForRestartDeclineIsNeverDiscarded(t *testing.T) {
	t.Parallel()

	for _, file := range restartStopSourceFiles {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, filepath.Join(".", file), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			stmt, ok := n.(*ast.ExprStmt)
			if !ok {
				return true
			}
			call, ok := stmt.X.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "stopForRestart" {
				return true
			}
			t.Errorf(
				"%s:%d discards stopForRestart's return value. It reports whether the "+
					"container was ACTUALLY stopped — false means the pinned image could not be "+
					"obtained and the still-running container was deliberately left alone "+
					"(core#5019). Ignoring it re-provisions a workspace that was never stopped.",
				file, fset.Position(stmt.Pos()).Line,
			)
			return true
		})
	}
}

// --- layer 1: behaviour -----------------------------------------------------

// prewarmCPProv is a CPProvisionerAPI stub that records the ORDER of the calls
// it receives, so the behaviour tests can assert "Stop was never reached"
// rather than merely "Stop returned an error".
type prewarmCPProv struct {
	calls       []string
	ensureErr   error
	ensureRes   provisioner.EnsureImageResult
	lastRequest provisioner.EnsureImageRequest
}

func (p *prewarmCPProv) EnsureImage(_ context.Context, req provisioner.EnsureImageRequest) (provisioner.EnsureImageResult, error) {
	p.calls = append(p.calls, "EnsureImage")
	p.lastRequest = req
	return p.ensureRes, p.ensureErr
}
func (p *prewarmCPProv) Stop(_ context.Context, _ string) error {
	p.calls = append(p.calls, "Stop")
	return nil
}
func (p *prewarmCPProv) StopAndPrune(_ context.Context, _ string) error {
	p.calls = append(p.calls, "StopAndPrune")
	return nil
}
func (p *prewarmCPProv) Start(_ context.Context, _ provisioner.WorkspaceConfig) (string, error) {
	p.calls = append(p.calls, "Start")
	return "i-prewarm", nil
}
func (p *prewarmCPProv) IsRunning(_ context.Context, _ string) (bool, error) { return true, nil }
func (p *prewarmCPProv) GetConsoleOutput(_ context.Context, _ string) (string, error) {
	return "", nil
}

func TestEnsurePinnedImageBeforeStop_AllowsWhenCPConfirmsImage(t *testing.T) {
	stub := &prewarmCPProv{ensureRes: provisioner.EnsureImageResult{Status: "ready", ImageRef: "reg/x@sha256:abc"}}
	h := &WorkspaceHandler{cpProv: stub}
	if !h.ensurePinnedImageBeforeStop(context.Background(), "ws-ok", models.CreateWorkspacePayload{Runtime: "hermes", Template: "hermes"}) {
		t.Fatal("a CP that confirms the pinned image is ready must ALLOW the stop")
	}
	if len(stub.calls) != 1 || stub.calls[0] != "EnsureImage" {
		t.Fatalf("expected exactly one EnsureImage call; got %v", stub.calls)
	}
	if stub.lastRequest.WorkspaceID != "ws-ok" || stub.lastRequest.Runtime != "hermes" || stub.lastRequest.Template != "hermes" {
		t.Errorf("EnsureImage must carry the workspace's own runtime+template so CP resolves THIS workspace's pin; got %+v", stub.lastRequest)
	}
}

func TestEnsurePinnedImageBeforeStop_DeclinesWhenImageCannotBeObtained(t *testing.T) {
	// The whole point of #5019: a pull that CANNOT succeed must not cost the
	// workspace its container. This is the bad-digest / registry-outage /
	// disk-full class the longer timeout does nothing for.
	stub := &prewarmCPProv{ensureErr: errors.New("manifest unknown: sha256:93dfaf12… not found in registry")}
	h := &WorkspaceHandler{cpProv: stub}
	if h.ensurePinnedImageBeforeStop(context.Background(), "ws-bad-digest", models.CreateWorkspacePayload{Runtime: "hermes", Template: "hermes"}) {
		t.Fatal("an unobtainable pinned image must DECLINE the stop — the running container is the only thing the user still has")
	}
}

func TestEnsurePinnedImageBeforeStop_AllowsOnOlderControlPlane(t *testing.T) {
	// Compatibility, and deliberately fail-OPEN for exactly this one error:
	// a CP that predates the ensure-image endpoint answers 404. Failing closed
	// there would wedge every restart on the fleet during a rollout — a much
	// larger outage than the one being fixed. Any OTHER error still declines.
	stub := &prewarmCPProv{ensureErr: provisioner.ErrEnsureImageUnsupported}
	h := &WorkspaceHandler{cpProv: stub}
	if !h.ensurePinnedImageBeforeStop(context.Background(), "ws-old-cp", models.CreateWorkspacePayload{Runtime: "hermes", Template: "hermes"}) {
		t.Fatal("a control plane without the ensure-image endpoint must not block restarts (pre-#5019 behaviour)")
	}
}

func TestEnsurePinnedImageBeforeStop_AllowsWhenNoCPProvisioner(t *testing.T) {
	// Self-hosted / Docker path: there is no CP seam to ask, and the local
	// provisioner resolves its own image. Nothing to guard, nothing to block.
	h := &WorkspaceHandler{}
	if !h.ensurePinnedImageBeforeStop(context.Background(), "ws-selfhost", models.CreateWorkspacePayload{Runtime: "claude-code"}) {
		t.Fatal("a handler with no CP provisioner must not block the Docker restart path")
	}
}

func TestStopForRestart_DeclinedPrewarmNeverReachesStop(t *testing.T) {
	read := captureProvLog(t)
	stub := &prewarmCPProv{ensureErr: errors.New("registry unreachable")}
	h := &WorkspaceHandler{cpProv: stub}

	stopped := h.stopForRestart(context.Background(), "ws-decline", models.CreateWorkspacePayload{Runtime: "hermes", Template: "hermes"})

	if stopped {
		t.Fatal("stopForRestart must report false when the prewarm declined")
	}
	for _, c := range stub.calls {
		if c == "Stop" || c == "StopAndPrune" {
			t.Fatalf("core#5019 REGRESSION: the container was destroyed despite an unobtainable image; calls=%v", stub.calls)
		}
	}
	got := read()
	// provision.ec2_stopped is emitted by the provisioner on a real stop; the
	// pre_stop marker is emitted by this helper. Neither may appear when the
	// stop was declined — ops reading the wire log must not see a stop that
	// never happened.
	if strings.Contains(got, "evt: restart.pre_stop ") {
		t.Errorf("a declined restart must not emit restart.pre_stop; log:\n%s", got)
	}
	if !strings.Contains(got, "evt: restart.image_prewarm_declined ") {
		t.Errorf("a declined restart must emit restart.image_prewarm_declined so the decision is visible in the wire log; log:\n%s", got)
	}
}

func TestStopForRestart_AllowedPrewarmStopsAfterEnsuring(t *testing.T) {
	// RED CONTROL for the test above: the same helper, same stub type, only the
	// prewarm outcome differs — proving the decline test is discriminating and
	// not merely observing a code path that never stops anything.
	stub := &prewarmCPProv{ensureRes: provisioner.EnsureImageResult{Status: "ready"}}
	h := &WorkspaceHandler{cpProv: stub}
	defer func() { _ = recover() }() // db.ClearWorkspaceKeys touches a nil Redis client under test

	stopped := h.stopForRestart(context.Background(), "ws-allow", models.CreateWorkspacePayload{Runtime: "hermes", Template: "hermes"})

	if !stopped {
		t.Fatal("stopForRestart must report true when the prewarm allowed the stop")
	}
	if len(stub.calls) < 2 || stub.calls[0] != "EnsureImage" || stub.calls[1] != "Stop" {
		t.Fatalf("expected EnsureImage strictly BEFORE Stop; got %v", stub.calls)
	}
}

func TestRestartWorkspaceAutoOpts_DeclinedPrewarmLeavesContainerAlone(t *testing.T) {
	// The manual HTTP Restart path (POST /workspaces/:id/restart) dispatches
	// through this helper. It has its OWN stop leg — it does not go through
	// stopForRestart — so it needs its own proof.
	stub := &prewarmCPProv{ensureErr: errors.New("no space left on device")}
	h := &WorkspaceHandler{cpProv: stub}

	ok := h.RestartWorkspaceAutoOpts(context.Background(), "ws-nospace", "", nil,
		models.CreateWorkspacePayload{Name: "n", Tier: 4, Runtime: "hermes", Template: "hermes"}, false)
	h.waitAsyncForTest()

	if ok {
		t.Error("a declined restart must not report that a backend was kicked off")
	}
	for _, c := range stub.calls {
		if c == "Stop" || c == "StopAndPrune" {
			t.Fatalf("core#5019 REGRESSION: manual restart destroyed the container despite an unobtainable image; calls=%v", stub.calls)
		}
		if c == "Start" {
			t.Fatalf("a declined restart must not re-provision either; calls=%v", stub.calls)
		}
	}
}

// TestRestartProvisionGate_ReleasedOnDeclinedPrewarm proves the decline path
// does not strand the per-workspace restart/provision gate. A leaked gate would
// wedge the workspace out of EVERY future restart — turning a recoverable
// "image not ready yet" into a permanent one, which is strictly worse than the
// bug being fixed.
func TestRestartProvisionGate_ReleasedOnDeclinedPrewarm(t *testing.T) {
	stub := &prewarmCPProv{ensureErr: errors.New("registry outage")}
	h := &WorkspaceHandler{cpProv: stub}

	h.RestartWorkspaceAutoOpts(context.Background(), "ws-gate", "", nil,
		models.CreateWorkspacePayload{Name: "n", Tier: 4, Runtime: "hermes"}, false)
	h.waitAsyncForTest()

	gate := acquireRestartProvisionGate("ws-gate")
	locked := make(chan struct{})
	go func() {
		// Signal from INSIDE the critical section: acquiring the gate is the
		// proof it was released, and closing the channel while holding it keeps
		// staticcheck's SA2001 (empty critical section) honest rather than
		// suppressed — an empty Lock/Unlock pair reads as a mistake even when
		// it is deliberate.
		gate.Lock()
		close(locked)
		gate.Unlock()
	}()
	select {
	case <-locked:
	case <-time.After(5 * time.Second):
		t.Fatal("the restart/provision gate was never released on the declined-prewarm path — the workspace is now permanently unrestartable")
	}
}
