package provisioner

import (
	"context"
	"errors"
)

// DesktopHandle identifies a running desktop sidecar and how to reach it.
// Returned by StartDesktop. Address is the reachable base host:port of the
// sidecar's control server on the workspace's own per-workspace network
// (e.g. "wsdesk-<id>:<port>" on the local Docker backend); the display proxy
// and the computer-use gateway both dial it from there.
type DesktopHandle struct {
	Address string
	Running bool
	// ScaledUp is true only when THIS StartDesktop call transitioned the desktop
	// from stopped to running (a fresh start), and false when it was already
	// running. Callers use it to do transition-only work — e.g. recording the
	// 'running' lifecycle state exactly once per scale-up instead of on every
	// hot-path screenshot/input forward.
	ScaledUp bool
}

// SidecarProvisioner is the backend-neutral contract for the per-workspace
// desktop-sidecar lifecycle. See the design RFC:
// docs/superpowers/specs/2026-07-27-agent-desktop-sidecar-design.md §5.
//
// It mirrors the LocalProvisionerAPI / CPProvisionerAPI split: one interface,
// a Local (Docker) implementation now, a CP implementation deferred, and a
// k8s implementation at migration. The active backend is selected per
// deployment by a …Auto dispatcher, exactly like the tenant provisioner
// backends — so nothing above this interface knows or cares whether the
// desktop is a local Docker sibling, a cloud VM co-tenant, or a k8s pod
// sidecar (design decision 1: co-located; decision 4: backend-neutral).
//
// Naming note: the local backend names sidecar containers "wsdesk-<id>", NOT
// "ws-<id>-desktop", so the orphan sweeper's TrimPrefix("ws-") name parse can
// never mis-read a sidecar as a workspace id (design §11).
type SidecarProvisioner interface {
	// StartDesktop brings up the workspace's desktop sidecar (or no-ops if it
	// is already up) and returns a handle to reach it. MUST be idempotent —
	// the agent's first computer-use tool call and a human opening the display
	// can both trigger it concurrently (design §10).
	StartDesktop(ctx context.Context, cfg WorkspaceConfig) (DesktopHandle, error)

	// StopDesktop gracefully tears the sidecar down: SIGTERM + a flush window
	// so Chrome/Xorg close their SQLite profile cleanly, THEN remove. It must
	// NOT copy the tenant's force-remove — a SIGKILL mid-write corrupts the
	// Chrome profile and silently breaks the "logins persist" guarantee
	// (design §10). The persistent profile volume survives StopDesktop; only
	// WipeProfile deletes it.
	StopDesktop(ctx context.Context, workspaceID string) error

	// DesktopRunning reports whether the sidecar is currently up. On a backend
	// that is not wired (see ErrDesktopBackendUnavailable) this returns
	// (false, nil) — a clean "no", so the feature reads as absent, not broken.
	DesktopRunning(ctx context.Context, workspaceID string) (bool, error)

	// WipeProfile destroys the persistent profile volume (cookies / live
	// logins) — the revoke/wipe path, and part of permanent-delete-with-erase.
	// Must actually target the sidecar's profile volume, which the tenant
	// RemoveVolume path does not (design §11).
	WipeProfile(ctx context.Context, workspaceID string) error
}

// ErrDesktopBackendUnavailable is returned by StartDesktop/StopDesktop/
// WipeProfile on a SidecarProvisioner backend that is not wired on this
// deployment — e.g. the CP/k8s backend before the control plane implements
// desktop scheduling (design §5, §14; decision 4).
//
// Callers surface it as a per-tier AVAILABILITY GATE
// ("desktop_backend_unavailable"), NOT a failure: the desktop feature is
// simply absent on that tier, and the rest of the platform is unaffected.
// Self-host (the local Docker backend) is fully functional regardless.
var ErrDesktopBackendUnavailable = errors.New("desktop sidecar backend not available on this deployment")

// unavailableSidecarProvisioner is the availability-gate backend: every
// mutating operation reports ErrDesktopBackendUnavailable, and DesktopRunning
// reports a clean "not running". Used on deployments whose desktop backend
// (CP/k8s) is not yet wired, so the feature degrades to "absent", never
// "broken" (design decision 4 — "CP not wired won't hurt function").
type unavailableSidecarProvisioner struct{}

// NewUnavailableSidecarProvisioner returns a SidecarProvisioner that gates the
// desktop feature off cleanly (per-tier availability). It is the default
// backend on a deployment that has no desktop provisioner wired yet.
func NewUnavailableSidecarProvisioner() SidecarProvisioner { return unavailableSidecarProvisioner{} }

func (unavailableSidecarProvisioner) StartDesktop(context.Context, WorkspaceConfig) (DesktopHandle, error) {
	return DesktopHandle{}, ErrDesktopBackendUnavailable
}

func (unavailableSidecarProvisioner) StopDesktop(context.Context, string) error {
	return ErrDesktopBackendUnavailable
}

func (unavailableSidecarProvisioner) DesktopRunning(context.Context, string) (bool, error) {
	return false, nil
}

func (unavailableSidecarProvisioner) WipeProfile(context.Context, string) error {
	return ErrDesktopBackendUnavailable
}

// Compile-time assertion, mirroring the LocalProvisionerAPI / CPProvisionerAPI
// pattern: catches a signature drift at build time.
var _ SidecarProvisioner = unavailableSidecarProvisioner{}
