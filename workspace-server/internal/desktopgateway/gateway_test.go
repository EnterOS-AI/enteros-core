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
	addr    string
	starts  int
	startErr error
}

func (f *fakeProv) StartDesktop(context.Context, provisioner.WorkspaceConfig) (provisioner.DesktopHandle, error) {
	f.starts++
	if f.startErr != nil {
		return provisioner.DesktopHandle{}, f.startErr
	}
	return provisioner.DesktopHandle{Address: f.addr, Running: true}, nil
}
func (f *fakeProv) StopDesktop(context.Context, string) error              { return nil }
func (f *fakeProv) DesktopRunning(context.Context, string) (bool, error)   { return true, nil }
func (f *fakeProv) WipeProfile(context.Context, string) error             { return nil }

type fakeLocks struct {
	held bool
	err  error
}

func (f fakeLocks) AgentHoldsControl(context.Context, string) (bool, error) { return f.held, f.err }

type fakeActivity struct{ n int }

func (f *fakeActivity) RecordAgentActivity(context.Context, string) error { f.n++; return nil }

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
// call, no desktop start (§8).
func TestGateway_Input_FailsClosedWhenAgentLacksControl(t *testing.T) {
	prov := &fakeProv{addr: "wsdesk-abc123:6070"}
	doer := &fakeDoer{code: http.StatusNoContent}
	g := New(prov, fakeLocks{held: false}, &fakeActivity{}, tokenFn, doer)

	err := g.Input(context.Background(), "w1", json.RawMessage(`{"type":"click","x":1,"y":1}`))
	if !errors.Is(err, ErrHumanInControl) {
		t.Fatalf("want ErrHumanInControl, got %v", err)
	}
	if len(doer.reqs) != 0 {
		t.Fatalf("must not proxy input when the agent lacks control")
	}
	if prov.starts != 0 {
		t.Fatalf("must not start the desktop when input is refused")
	}
}

func TestGateway_Input_ProxiesWhenAgentHoldsControl(t *testing.T) {
	prov := &fakeProv{addr: "wsdesk-abc123:6070"}
	doer := &fakeDoer{code: http.StatusNoContent}
	act := &fakeActivity{}
	g := New(prov, fakeLocks{held: true}, act, tokenFn, doer)

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
	g := New(prov, fakeLocks{held: false}, act, tokenFn, doer)

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
	g := New(prov, fakeLocks{held: true}, &fakeActivity{}, tokenFn, doer)

	_, err := g.Screenshot(context.Background(), "w1")
	if !errors.Is(err, provisioner.ErrDesktopBackendUnavailable) {
		t.Fatalf("want ErrDesktopBackendUnavailable, got %v", err)
	}
}

// A lock-check error fails closed (deny), never open.
func TestGateway_Input_LockErrorFailsClosed(t *testing.T) {
	prov := &fakeProv{addr: "wsdesk-abc123:6070"}
	doer := &fakeDoer{code: http.StatusNoContent}
	g := New(prov, fakeLocks{err: errors.New("db down")}, &fakeActivity{}, tokenFn, doer)
	if err := g.Input(context.Background(), "w1", json.RawMessage(`{"type":"click","x":1,"y":1}`)); err == nil {
		t.Fatalf("lock-check error must fail closed (return an error), got nil")
	}
	if len(doer.reqs) != 0 {
		t.Fatalf("must not proxy input on a lock-check error")
	}
}
