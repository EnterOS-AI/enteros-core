package desktopgateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
)

// --- fakes ---

type fakeProv struct {
	addr     string
	starts   int
	startErr error
	scaledUp bool // reported as DesktopHandle.ScaledUp (the stopped->running edge)
}

func (f *fakeProv) StartDesktop(context.Context, provisioner.WorkspaceConfig) (provisioner.DesktopHandle, error) {
	f.starts++
	if f.startErr != nil {
		return provisioner.DesktopHandle{}, f.startErr
	}
	return provisioner.DesktopHandle{Address: f.addr, Running: true, ScaledUp: f.scaledUp}, nil
}
func (f *fakeProv) StopDesktop(context.Context, string) error            { return nil }
func (f *fakeProv) DesktopRunning(context.Context, string) (bool, error) { return true, nil }
func (f *fakeProv) WipeProfile(context.Context, string) error            { return nil }

type fakeLocks struct {
	held     bool
	err      error
	acquires int
}

func (f *fakeLocks) AcquireAgentControl(context.Context, string) (bool, error) {
	f.acquires++
	return f.held, f.err
}

type fakeActivity struct{ n int }

func (f *fakeActivity) RecordAgentActivity(context.Context, string) error { f.n++; return nil }

type fakeState struct {
	states []string // recorded states, in call order
	addrs  []string
}

func (f *fakeState) SetState(_ context.Context, _, state, addr string) error {
	f.states = append(f.states, state)
	f.addrs = append(f.addrs, addr)
	return nil
}

type fakeDoer struct {
	reqs []*http.Request
	code int
	body string
}

func (f *fakeDoer) Do(r *http.Request) (*http.Response, error) {
	f.reqs = append(f.reqs, r)
	return &http.Response{
		StatusCode: f.code,
		Body:       io.NopCloser(strings.NewReader(f.body)),
		Header:     make(http.Header),
	}, nil
}

func tokenFn(_ context.Context, _ string) (string, error) { return "sidecar-secret", nil }

// Input must FAIL-CLOSED when the agent does not hold the control lock: no HTTP
// call to the sidecar (§8) AND no scale-up. The control check precedes
// ensureRunning, so a human-held desktop that was reaped to zero is NOT booted
// back up just to refuse the input (verified 2026-07-28).
func TestGateway_Input_FailsClosedWhenAgentLacksControl(t *testing.T) {
	prov := &fakeProv{addr: "wsdesk-abc123:6070"}
	doer := &fakeDoer{code: http.StatusNoContent}
	g := New(prov, &fakeLocks{held: false}, &fakeActivity{}, tokenFn, doer)

	err := g.Input(context.Background(), "w1", json.RawMessage(`{"type":"click","x":1,"y":1}`))
	if !errors.Is(err, ErrHumanInControl) {
		t.Fatalf("want ErrHumanInControl, got %v", err)
	}
	if len(doer.reqs) != 0 {
		t.Fatalf("must not proxy input when the agent lacks control")
	}
	if prov.starts != 0 {
		t.Fatalf("must NOT scale up the desktop when a human holds control, got %d StartDesktop calls", prov.starts)
	}
}

// When the agent holds control but StartDesktop FAILS, Input surfaces that error
// (as a 5xx-class failure to the route). The lease WAS acquired — that is
// correct: the agent legitimately holds control and is retrying — and crucially,
// ensureRunning runs only AFTER the control check, so its failure can never be
// mistaken for a human-in-control refusal (409). See finding-1 fix (2026-07-28).
func TestGateway_Input_SurfacesStartFailureAfterAcquiringControl(t *testing.T) {
	prov := &fakeProv{addr: "wsdesk-abc123:6070", startErr: errors.New("boom")}
	doer := &fakeDoer{code: http.StatusNoContent}
	locks := &fakeLocks{held: true}
	g := New(prov, locks, &fakeActivity{}, tokenFn, doer)

	err := g.Input(context.Background(), "w1", json.RawMessage(`{"type":"click","x":1,"y":1}`))
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want StartDesktop error surfaced, got %v", err)
	}
	if errors.Is(err, ErrHumanInControl) {
		t.Fatalf("a scale-up failure must NOT masquerade as ErrHumanInControl (409), got %v", err)
	}
	if len(doer.reqs) != 0 {
		t.Fatalf("must not proxy input when the desktop failed to start")
	}
}

// On a successful scale-up (the stopped->running edge, DesktopHandle.ScaledUp)
// the gateway records state='running' with the sidecar address exactly once, so
// the idle sweeper can later find the desktop to reap (§10). It must NOT re-write
// the state on subsequent already-running forwards (finding-8 fix, 2026-07-28).
func TestGateway_RecordsRunningStateOnScaleUp(t *testing.T) {
	prov := &fakeProv{addr: "wsdesk-abc123:6070", scaledUp: true}
	doer := &fakeDoer{code: http.StatusNoContent}
	state := &fakeState{}
	g := New(prov, &fakeLocks{held: true}, &fakeActivity{}, tokenFn, doer)
	g.SetStateRecorder(state)

	if err := g.Input(context.Background(), "w1", json.RawMessage(`{"type":"click","x":1,"y":1}`)); err != nil {
		t.Fatalf("Input: %v", err)
	}
	if len(state.states) != 1 || state.states[0] != "running" {
		t.Fatalf("want one 'running' state recorded, got %v", state.states)
	}
	if state.addrs[0] != "wsdesk-abc123:6070" {
		t.Fatalf("running state must carry the sidecar address, got %q", state.addrs[0])
	}
}

// A forward on an ALREADY-running desktop (ScaledUp=false) must NOT re-record the
// 'running' lifecycle state — that was a redundant per-screenshot DB upsert on
// the hot path (finding-8 fix, 2026-07-28). recordActivity still upserts the
// activity timestamp; only the state/address write is transition-only.
func TestGateway_DoesNotRerecordStateWhenAlreadyRunning(t *testing.T) {
	prov := &fakeProv{addr: "wsdesk-abc123:6070", scaledUp: false}
	doer := &fakeDoer{code: http.StatusNoContent}
	state := &fakeState{}
	g := New(prov, &fakeLocks{held: true}, &fakeActivity{}, tokenFn, doer)
	g.SetStateRecorder(state)

	if err := g.Input(context.Background(), "w1", json.RawMessage(`{"type":"click","x":1,"y":1}`)); err != nil {
		t.Fatalf("Input: %v", err)
	}
	if len(state.states) != 0 {
		t.Fatalf("must not record state on an already-running forward, got %v", state.states)
	}
}

func TestGateway_Input_ProxiesWhenAgentHoldsControl(t *testing.T) {
	prov := &fakeProv{addr: "wsdesk-abc123:6070"}
	doer := &fakeDoer{code: http.StatusNoContent}
	act := &fakeActivity{}
	g := New(prov, &fakeLocks{held: true}, act, tokenFn, doer)

	if err := g.Input(context.Background(), "w1", json.RawMessage(`{"type":"click","x":1,"y":1}`)); err != nil {
		t.Fatalf("Input: %v", err)
	}
	if prov.starts != 1 {
		t.Fatalf("scale-from-zero: want 1 StartDesktop, got %d", prov.starts)
	}
	if act.n != 1 {
		t.Fatalf("must record agent activity, got %d", act.n)
	}
	if len(doer.reqs) != 1 {
		t.Fatalf("want 1 proxied request, got %d", len(doer.reqs))
	}
	req := doer.reqs[0]
	if req.Method != http.MethodPost || req.URL.String() != "http://wsdesk-abc123:6070/input" {
		t.Fatalf("bad proxied request: %s %s", req.Method, req.URL)
	}
	if req.Header.Get("Authorization") != "Bearer sidecar-secret" {
		t.Fatalf("missing per-sidecar auth header: %q", req.Header.Get("Authorization"))
	}
}

// Sight is never arbitrated: Screenshot does not consult the lock and works
// even without control (§8).
func TestGateway_Screenshot_NoLockCheck(t *testing.T) {
	prov := &fakeProv{addr: "wsdesk-abc123:6070"}
	doer := &fakeDoer{code: http.StatusOK, body: "PNGBYTES"}
	act := &fakeActivity{}
	// held:false — but screenshots must still work.
	g := New(prov, &fakeLocks{held: false}, act, tokenFn, doer)

	png, err := g.Screenshot(context.Background(), "w1")
	if err != nil {
		t.Fatalf("Screenshot: %v", err)
	}
	if string(png) != "PNGBYTES" {
		t.Fatalf("screenshot bytes = %q", png)
	}
	if act.n != 1 {
		t.Fatalf("screenshot must count as activity, got %d", act.n)
	}
	if doer.reqs[0].Method != http.MethodGet || !strings.HasSuffix(doer.reqs[0].URL.Path, "/screenshot") {
		t.Fatalf("bad screenshot request: %s %s", doer.reqs[0].Method, doer.reqs[0].URL)
	}
}

// On an unwired backend the availability-gate error surfaces (per-tier gate).
func TestGateway_UnavailableBackendSurfaces(t *testing.T) {
	prov := provisioner.NewUnavailableSidecarProvisioner()
	doer := &fakeDoer{code: http.StatusOK}
	g := New(prov, &fakeLocks{held: true}, &fakeActivity{}, tokenFn, doer)

	_, err := g.Screenshot(context.Background(), "w1")
	if !errors.Is(err, provisioner.ErrDesktopBackendUnavailable) {
		t.Fatalf("want ErrDesktopBackendUnavailable, got %v", err)
	}
}

// A lock-check error fails closed (deny), never open.
func TestGateway_Input_LockErrorFailsClosed(t *testing.T) {
	prov := &fakeProv{addr: "wsdesk-abc123:6070"}
	doer := &fakeDoer{code: http.StatusNoContent}
	g := New(prov, &fakeLocks{err: errors.New("db down")}, &fakeActivity{}, tokenFn, doer)
	if err := g.Input(context.Background(), "w1", json.RawMessage(`{"type":"click","x":1,"y":1}`)); err == nil {
		t.Fatalf("lock-check error must fail closed (return an error), got nil")
	}
	if len(doer.reqs) != 0 {
		t.Fatalf("must not proxy input on a lock-check error")
	}
}
