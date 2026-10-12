package provisioner

import (
	"context"
	"errors"
	"testing"
)

// The availability-gate backend must make the desktop feature read as ABSENT,
// not BROKEN: mutating ops return ErrDesktopBackendUnavailable, but a
// running-state query is a clean (false, nil). This is the behaviour that lets
// a deployment without a wired desktop backend (CP/k8s) leave the rest of the
// platform unaffected (design decision 4).
func TestUnavailableSidecarProvisioner_GatesFeatureCleanly(t *testing.T) {
	sp := NewUnavailableSidecarProvisioner() // returns SidecarProvisioner (interface)
	ctx := context.Background()

	if _, err := sp.StartDesktop(ctx, WorkspaceConfig{WorkspaceID: "w1"}); !errors.Is(err, ErrDesktopBackendUnavailable) {
		t.Fatalf("StartDesktop: want ErrDesktopBackendUnavailable, got %v", err)
	}
	if err := sp.StopDesktop(ctx, "w1"); !errors.Is(err, ErrDesktopBackendUnavailable) {
		t.Fatalf("StopDesktop: want ErrDesktopBackendUnavailable, got %v", err)
	}
	if err := sp.WipeProfile(ctx, "w1"); !errors.Is(err, ErrDesktopBackendUnavailable) {
		t.Fatalf("WipeProfile: want ErrDesktopBackendUnavailable, got %v", err)
	}

	// Running-state must NOT error — a gated backend reports "not running",
	// so callers see "desktop absent", never "desktop failed".
	running, err := sp.DesktopRunning(ctx, "w1")
	if err != nil {
		t.Fatalf("DesktopRunning: unexpected error %v", err)
	}
	if running {
		t.Fatalf("DesktopRunning: want false on an unwired backend, got true")
	}
}
