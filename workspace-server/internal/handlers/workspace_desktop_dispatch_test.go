package handlers

import (
	"context"
	"errors"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
)

type fakeSidecarProv struct {
	started, stopped, wiped int
	running                 bool
}

func (f *fakeSidecarProv) StartDesktop(_ context.Context, cfg provisioner.WorkspaceConfig) (provisioner.DesktopHandle, error) {
	f.started++
	return provisioner.DesktopHandle{Address: "wsdesk-" + cfg.WorkspaceID + ":6070", Running: true}, nil
}
func (f *fakeSidecarProv) StopDesktop(context.Context, string) error            { f.stopped++; return nil }
func (f *fakeSidecarProv) DesktopRunning(context.Context, string) (bool, error) { return f.running, nil }
func (f *fakeSidecarProv) WipeProfile(context.Context, string) error            { f.wiped++; return nil }

// With no backend wired, the desktop dispatchers gate the feature: mutating ops
// return ErrDesktopBackendUnavailable, and running-state is a clean (false,nil)
// so the feature reads as ABSENT, not broken (decision 4).
func TestDesktopAuto_NilBackendGatesFeature(t *testing.T) {
	h := &WorkspaceHandler{}
	if _, err := h.StartDesktopAuto(context.Background(), "w1"); !errors.Is(err, provisioner.ErrDesktopBackendUnavailable) {
		t.Fatalf("StartDesktopAuto nil backend: want ErrDesktopBackendUnavailable, got %v", err)
	}
	if r, err := h.DesktopRunningAuto(context.Background(), "w1"); err != nil || r {
		t.Fatalf("DesktopRunningAuto nil backend: want (false,nil), got (%v,%v)", r, err)
	}
}

func TestDesktopAuto_RoutesToWiredBackend(t *testing.T) {
	f := &fakeSidecarProv{running: true}
	h := &WorkspaceHandler{}
	h.SetSidecarProvisioner(f)

	hnd, err := h.StartDesktopAuto(context.Background(), "w1")
	if err != nil || hnd.Address != "wsdesk-w1:6070" {
		t.Fatalf("StartDesktopAuto wired: got (%+v,%v)", hnd, err)
	}
	if f.started != 1 {
		t.Fatalf("StartDesktop not routed, started=%d", f.started)
	}
	if err := h.StopDesktopAuto(context.Background(), "w1"); err != nil {
		t.Fatal(err)
	}
	if err := h.WipeDesktopProfileAuto(context.Background(), "w1"); err != nil {
		t.Fatal(err)
	}
	if f.stopped != 1 || f.wiped != 1 {
		t.Fatalf("stop/wipe not routed: stopped=%d wiped=%d", f.stopped, f.wiped)
	}
	if r, _ := h.DesktopRunningAuto(context.Background(), "w1"); !r {
		t.Fatalf("DesktopRunningAuto should reflect wired backend true")
	}
}
