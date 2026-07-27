package provisioner

import (
	"context"
	_ "embed"
	"fmt"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// defaultDesktopSeccompProfile is the Chromium-tuned seccomp profile applied to
// every sidecar (SecurityOpt "seccomp=<content>"). It is the upstream Docker
// default (deny-by-default) with the namespace-creation + chroot-setup syscalls
// the Chromium unprivileged-userns sandbox needs allowed — so the browser keeps
// its OWN sandbox (no --no-sandbox) WITHOUT the container holding CAP_SYS_ADMIN.
// Empirically verified against Docker 29.5.3 (2026-07-27). See seccomp/README.md.
//
//go:embed seccomp/desktop-sidecar.json
var defaultDesktopSeccompProfile string

// sidecarDocker is the narrow slice of the Docker SDK the local desktop-sidecar
// provisioner needs. It is deliberately NOT the package's dockerClient
// interface: the sidecar needs ContainerStop (graceful SIGTERM teardown — see
// StopDesktop) which the tenant provisioner never calls, and a separate narrow
// interface avoids widening dockerClient (and every fake that implements it)
// for a method only this path uses. The real *client.Client satisfies it.
type sidecarDocker interface {
	ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkingConfig *network.NetworkingConfig, platform *ocispec.Platform, containerName string) (container.CreateResponse, error)
	ContainerInspect(ctx context.Context, name string) (container.InspectResponse, error)
	ContainerStart(ctx context.Context, name string, options container.StartOptions) error
	ContainerStop(ctx context.Context, name string, options container.StopOptions) error
	ContainerRemove(ctx context.Context, name string, options container.RemoveOptions) error
	VolumeCreate(ctx context.Context, options volume.CreateOptions) (volume.Volume, error)
	VolumeRemove(ctx context.Context, volumeID string, force bool) error
	NetworkCreate(ctx context.Context, name string, options network.CreateOptions) (network.CreateResponse, error)
	NetworkRemove(ctx context.Context, networkID string) error
}

// LocalSidecarProvisioner is the Docker (self-host) backend of
// SidecarProvisioner. It runs the desktop as a lifecycle-coupled sibling
// container "wsdesk-<id>" on the workspace's network — design decision 1
// (co-located) / §7. It reuses the naming/label helpers (desktop_naming.go) so
// the container is reap-able by LabelRole=desktop and never mis-parsed by the
// tenant "ws-" name parsers.
type LocalSidecarProvisioner struct {
	cli sidecarDocker

	// image is the desktop-sidecar image (Xorg + WM + Chrome + x11vnc +
	// control server). Digest-pinned in production.
	image string

	// networkPrefix names a DEDICATED PER-WORKSPACE network the sidecar is
	// created on: "<networkPrefix>-<workspaceID>" (§6.1, decision 1 — security
	// first). Each workspace's desktop gets its OWN network. StartDesktop
	// creates it; StopDesktop removes it best-effort. Empty = no network attach
	// (tests only; NOT production).
	//
	// VERIFIED against real Docker (2026-07-27): a per-workspace network blocks
	// reaching other containers BY NAME (Docker DNS is per-network) but NOT BY
	// IP — bridge networks on one host still route to each other's subnets. So
	// this is NECESSARY BUT NOT SUFFICIENT: it MUST be paired with egress
	// control that denies RFC-1918 (the §6.1 egress proxy / a DOCKER-USER
	// firewall rule), or a sidecar can still hit postgres/redis/litellm by IP.
	// Do NOT treat "sidecar is on a per-workspace network" as "sidecar is
	// isolated" until the egress-deny is in place.
	networkPrefix string

	// controlPort is the sidecar control-server port the computer-use gateway
	// and the display proxy dial.
	controlPort int

	// stopTimeout is the graceful-shutdown window: SIGTERM, wait up to this,
	// then the daemon SIGKILLs. Long enough for Chrome/Xorg to flush their
	// SQLite profile so the "logins persist" guarantee holds (§10).
	stopTimeout time.Duration

	// memoryLimitBytes caps the sidecar's RAM (§10 admission/OOM control). 0 =
	// unbounded (not recommended in production — a runaway Chrome must not
	// compete unbounded with the tenant).
	memoryLimitBytes int64

	// seccompProfile is the seccomp profile JSON applied to the sidecar via
	// SecurityOpt "seccomp=<content>". Empty is replaced by the embedded
	// Chromium-tuned default in the constructor. The literal "unconfined"
	// disables seccomp (debugging ONLY — never production). See
	// seccomp/README.md for why a custom profile is required: the stock Docker
	// profile blocks the Chromium userns sandbox under --cap-drop ALL.
	seccompProfile string
}

// NewLocalSidecarProvisioner constructs the Docker desktop backend. See the
// networkName doc for the production security requirement. seccompProfile is
// the seccomp JSON to apply ("" → the embedded Chromium-tuned default;
// "unconfined" → seccomp disabled, debugging only).
func NewLocalSidecarProvisioner(cli sidecarDocker, image, networkPrefix string, controlPort int, stopTimeout time.Duration, memoryLimitBytes int64, seccompProfile string) *LocalSidecarProvisioner {
	if stopTimeout <= 0 {
		stopTimeout = 10 * time.Second
	}
	if seccompProfile == "" {
		seccompProfile = defaultDesktopSeccompProfile
	}
	return &LocalSidecarProvisioner{
		cli:              cli,
		image:            image,
		networkPrefix:    networkPrefix,
		controlPort:      controlPort,
		stopTimeout:      stopTimeout,
		memoryLimitBytes: memoryLimitBytes,
		seccompProfile:   seccompProfile,
	}
}

// securityOpt builds the container SecurityOpt list: no-new-privileges always,
// plus the seccomp profile (the embedded Chromium-tuned default unless an
// operator supplied one, or "unconfined" to disable). Combined with CapDrop
// ALL in StartDesktop, this is the B2 hardening — verified to keep the Chromium
// userns sandbox active with no CAP_SYS_ADMIN (seccomp/README.md).
func (p *LocalSidecarProvisioner) securityOpt() []string {
	opts := []string{"no-new-privileges"}
	if p.seccompProfile == "unconfined" {
		opts = append(opts, "seccomp=unconfined")
	} else {
		opts = append(opts, "seccomp="+p.seccompProfile)
	}
	return opts
}

// perWorkspaceNetwork is the dedicated network name for a workspace's desktop
// ("<prefix>-<id>"), or "" when no prefix is configured.
func (p *LocalSidecarProvisioner) perWorkspaceNetwork(workspaceID string) string {
	if p.networkPrefix == "" {
		return ""
	}
	return p.networkPrefix + "-" + workspaceID
}

// Compile-time assertion: *LocalSidecarProvisioner satisfies SidecarProvisioner.
var _ SidecarProvisioner = (*LocalSidecarProvisioner)(nil)

func (p *LocalSidecarProvisioner) handle(workspaceID string, running bool) DesktopHandle {
	return DesktopHandle{
		Address: fmt.Sprintf("%s:%d", DesktopContainerName(workspaceID), p.controlPort),
		Running: running,
	}
}

// StartDesktop brings up the workspace's desktop sidecar, idempotently: if it
// is already running, it returns the handle without creating a second one (the
// agent's first tool call and a human opening the display can race, §10).
func (p *LocalSidecarProvisioner) StartDesktop(ctx context.Context, cfg WorkspaceConfig) (DesktopHandle, error) {
	if running, _ := p.DesktopRunning(ctx, cfg.WorkspaceID); running {
		return p.handle(cfg.WorkspaceID, true), nil
	}

	name := DesktopContainerName(cfg.WorkspaceID)

	// Persistent profile volume (cookies / live logins) — survives
	// scale-to-zero; only WipeProfile deletes it. Idempotent create.
	if _, err := p.cli.VolumeCreate(ctx, volume.CreateOptions{
		Name:   DesktopProfileVolumeName(cfg.WorkspaceID),
		Labels: desktopManagedLabels(),
	}); err != nil {
		return DesktopHandle{}, fmt.Errorf("create desktop profile volume: %w", err)
	}

	// Clear any stale exited same-name container so ContainerCreate doesn't
	// 409. Best-effort; a missing container is fine.
	if err := p.cli.ContainerRemove(ctx, name, container.RemoveOptions{Force: true}); err != nil && !isNoSuchContainer(err) {
		// Non-fatal: log-and-continue semantics — the create below will
		// surface a real conflict if the stale container truly blocks it.
		_ = err
	}

	hostCfg := &container.HostConfig{
		Binds: []string{DesktopProfileVolumeName(cfg.WorkspaceID) + ":/home/desktop/profile"},
		// "no": an on-demand sidecar must NOT resurrect after a graceful stop.
		// "on-failure" would restart on the SIGTERM exit-143 the graceful stop
		// produces; crash recovery is the platform re-calling StartDesktop
		// (§10). Never "unless-stopped".
		RestartPolicy: container.RestartPolicy{Name: "no"},
		// B2 hardening (verified 2026-07-27, Docker 29.5.3): the sidecar runs
		// untrusted web content, so it is locked to the minimum the browser's
		// own userns sandbox needs. CapDrop ALL + no-new-privileges + the
		// Chromium-tuned seccomp profile keep Chromium's sandbox ACTIVE (no
		// --no-sandbox) WITHOUT granting the container CAP_SYS_ADMIN. Empirically
		// confirmed: Chromium renders under exactly this config. See
		// securityOpt() and seccomp/README.md.
		CapDrop:     []string{"ALL"},
		SecurityOpt: p.securityOpt(),
	}
	if p.memoryLimitBytes > 0 {
		hostCfg.Resources.Memory = p.memoryLimitBytes
		// Pin swap to the memory cap so the container cannot use ~2× the limit
		// via swap — otherwise the OOM-shed guarantee below is only half real
		// (reviewer B-nit). Equal Memory/MemorySwap = swap disabled for the
		// container.
		hostCfg.Resources.MemorySwap = p.memoryLimitBytes
		// Shed the DESKTOP under memory pressure, never the tenant/agent (§10):
		// a positive oom_score_adj makes the kernel prefer killing the sidecar.
		hostCfg.OomScoreAdj = 500
	}

	netCfg := &network.NetworkingConfig{}
	if net := p.perWorkspaceNetwork(cfg.WorkspaceID); net != "" {
		// Create the DEDICATED per-workspace network (idempotent: a re-create
		// after a stale sidecar reuses it). This is the isolation boundary —
		// the sidecar is on ONLY this network, never the shared molecule-core-
		// net, so it cannot reach other tenants or the backend infra (§6.1).
		if _, err := p.cli.NetworkCreate(ctx, net, network.CreateOptions{Labels: desktopManagedLabels()}); err != nil && !isNetworkExists(err) {
			return DesktopHandle{}, fmt.Errorf("create per-workspace network: %w", err)
		}
		netCfg.EndpointsConfig = map[string]*network.EndpointSettings{
			net: {Aliases: []string{name}},
		}
	}

	if _, err := p.cli.ContainerCreate(ctx, &container.Config{
		Image:  p.image,
		Labels: desktopManagedLabels(),
	}, hostCfg, netCfg, nil, name); err != nil {
		return DesktopHandle{}, fmt.Errorf("create desktop sidecar: %w", err)
	}
	if err := p.cli.ContainerStart(ctx, name, container.StartOptions{}); err != nil {
		return DesktopHandle{}, fmt.Errorf("start desktop sidecar: %w", err)
	}
	return p.handle(cfg.WorkspaceID, true), nil
}

// StopDesktop gracefully tears the sidecar down: SIGTERM + a flush window so
// Chrome/Xorg close their SQLite profile cleanly, THEN remove. It MUST NOT copy
// the tenant's force-remove — a SIGKILL mid-write corrupts the profile and
// silently breaks "logins persist" (§10). Safe because the sidecar is
// RestartPolicy "no" (no resurrection race to beat). The profile volume
// survives; only WipeProfile deletes it. Idempotent.
func (p *LocalSidecarProvisioner) StopDesktop(ctx context.Context, workspaceID string) error {
	name := DesktopContainerName(workspaceID)
	secs := int(p.stopTimeout.Seconds())
	if err := p.cli.ContainerStop(ctx, name, container.StopOptions{Timeout: &secs}); err != nil {
		if isNoSuchContainer(err) {
			return nil
		}
		return fmt.Errorf("graceful stop desktop sidecar: %w", err)
	}
	// Not Force: the container is already stopped, so a plain remove suffices
	// and there is no SIGKILL involved.
	if err := p.cli.ContainerRemove(ctx, name, container.RemoveOptions{}); err != nil && !isNoSuchContainer(err) {
		return fmt.Errorf("remove desktop sidecar: %w", err)
	}
	// Best-effort: remove the now-empty per-workspace network so it doesn't
	// leak. A network still holding an endpoint (slow teardown) is left for a
	// later sweep; don't fail the stop over it.
	if net := p.perWorkspaceNetwork(workspaceID); net != "" {
		if err := p.cli.NetworkRemove(ctx, net); err != nil && !isNoSuchNetwork(err) {
			_ = err
		}
	}
	return nil
}

// DesktopRunning reports whether the sidecar container is currently up.
func (p *LocalSidecarProvisioner) DesktopRunning(ctx context.Context, workspaceID string) (bool, error) {
	insp, err := p.cli.ContainerInspect(ctx, DesktopContainerName(workspaceID))
	if err != nil {
		if isNoSuchContainer(err) {
			return false, nil
		}
		return false, err
	}
	return insp.State != nil && insp.State.Running, nil
}

// WipeProfile destroys the persistent profile volume (cookies / live logins) —
// the revoke/wipe path (§11). The sidecar should be stopped first so the
// volume is not in use. Idempotent.
func (p *LocalSidecarProvisioner) WipeProfile(ctx context.Context, workspaceID string) error {
	if err := p.cli.VolumeRemove(ctx, DesktopProfileVolumeName(workspaceID), true); err != nil {
		if isNoSuchVolume(err) {
			return nil
		}
		return fmt.Errorf("wipe desktop profile volume: %w", err)
	}
	return nil
}

// isNoSuchContainer / isNoSuchVolume match both the real Docker SDK not-found
// error text and the in-memory test fake ("No such container/volume: ...").
func isNoSuchContainer(err error) bool {
	return err != nil && strings.Contains(err.Error(), "No such container")
}

func isNoSuchVolume(err error) bool {
	return err != nil && strings.Contains(err.Error(), "No such volume")
}

func isNetworkExists(err error) bool {
	return err != nil && strings.Contains(err.Error(), "already exists")
}

func isNoSuchNetwork(err error) bool {
	return err != nil && strings.Contains(err.Error(), "No such network")
}
