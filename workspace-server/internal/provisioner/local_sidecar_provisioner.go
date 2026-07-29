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
	NetworkConnect(ctx context.Context, networkID, containerID string, config *network.EndpointSettings) error
	NetworkDisconnect(ctx context.Context, networkID, containerID string, force bool) error
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
	// first). Each workspace's desktop gets its OWN network, created INTERNAL
	// (no external route). StartDesktop creates it; StopDesktop removes it
	// best-effort. Empty = no network attach (tests only; NOT production).
	//
	// ISOLATION IS STRUCTURAL (verified 2026-07-28): the network is internal, so
	// the sidecar has NO egress of its own — it can reach nothing but the
	// per-workspace egress proxy (wsdeskproxy-<id>) on the same net. The proxy is
	// the sidecar's only route out and DENIES private/link-local destinations
	// (cmd/desktop-egress-proxy), so a compromised browser cannot reach backend
	// infra, the host, other tenants, or the cloud metadata service — with NO
	// host firewall and NO operator step. This is what makes the feature safe to
	// run on by default.
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

	// controlTokenSecret is the shared signing secret used to derive the
	// per-sidecar DESKTOP_CONTROL_TOKEN the control server authenticates against
	// (§6.5). The gateway's TokenResolver derives the SAME token from the SAME
	// secret, so they agree without storing anything. Empty means the sidecar
	// gets no token and its control server refuses to boot (fail-closed) — set
	// it via SetControlTokenSecret in production wiring.
	controlTokenSecret string

	// selfContainerID identifies the platform's OWN container so StartDesktop can
	// attach it to each per-workspace network — otherwise the platform (on the
	// shared net) cannot resolve/reach the sidecar's "wsdesk-<id>" name on the
	// isolated network (reviewer B3). Empty disables the attach (single-network
	// dev/test).
	selfContainerID string

	// egressNetwork is the network the per-workspace egress PROXY joins as its
	// second leg — its only route to the internet. The proxy denies
	// private/link-local destinations, so this network merely needs internet;
	// the desktop sidecar is NEVER on it. Empty defaults to the Docker "bridge"
	// network (always present, has internet NAT, does not carry molecule infra).
	egressNetwork string
}

// SetControlTokenSecret wires the shared secret used to derive each sidecar's
// DESKTOP_CONTROL_TOKEN. Must match the secret the gateway's TokenResolver uses.
func (p *LocalSidecarProvisioner) SetControlTokenSecret(secret string) { p.controlTokenSecret = secret }

// SetEgressNetwork overrides the network the egress proxy uses for internet
// egress (default "bridge").
func (p *LocalSidecarProvisioner) SetEgressNetwork(name string) { p.egressNetwork = name }

// desktopProxyPort is the port the per-workspace egress proxy listens on and the
// sidecar's DESKTOP_PROXY points at.
const desktopProxyPort = 3128

// proxyMemoryLimitBytes caps the egress proxy's RAM (256MB) — a stream-copy
// forward proxy needs little, and a runaway one must not pressure the host.
const proxyMemoryLimitBytes = 256 << 20

// SetSelfContainerID tells the provisioner which container is the platform, so it
// can join each per-workspace network and reach the sidecar by name (B3).
func (p *LocalSidecarProvisioner) SetSelfContainerID(id string) { p.selfContainerID = id }

// desktopGeometry is the fixed resolution the sidecar and control server pin
// (§3). Matches the image defaults + the coordinate contract.
const desktopWidth, desktopHeight = 1280, 800

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
		// Even on the already-running fast path, RECONCILE the egress plumbing
		// before returning: the egress proxy is RestartPolicy "no", so it can die
		// while the sidecar keeps running, and the platform's network attachment
		// can be lost — either leaves the sidecar wedged with no route out. Both
		// operations are idempotent and cheap (ensureEgressProxy recreates a dead
		// proxy; NetworkConnect is isAlreadyConnected-guarded), so a re-called
		// StartDesktop self-heals the egress path instead of shortcutting past it.
		if net := p.perWorkspaceNetwork(cfg.WorkspaceID); net != "" {
			if err := p.ensureEgressProxy(ctx, cfg.WorkspaceID, net); err != nil {
				return DesktopHandle{}, err
			}
			if p.selfContainerID != "" {
				if err := p.cli.NetworkConnect(ctx, net, p.selfContainerID, nil); err != nil && !isAlreadyConnected(err) {
					return DesktopHandle{}, fmt.Errorf("attach platform to per-workspace network: %w", err)
				}
			}
		}
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
		hostCfg.Memory = p.memoryLimitBytes
		// Pin swap to the memory cap so the container cannot use ~2× the limit
		// via swap — otherwise the OOM-shed guarantee below is only half real
		// (reviewer B-nit). Equal Memory/MemorySwap = swap disabled for the
		// container.
		hostCfg.MemorySwap = p.memoryLimitBytes
		// Shed the DESKTOP under memory pressure, never the tenant/agent (§10):
		// a positive oom_score_adj makes the kernel prefer killing the sidecar.
		hostCfg.OomScoreAdj = 500
	}

	netCfg := &network.NetworkingConfig{}
	if net := p.perWorkspaceNetwork(cfg.WorkspaceID); net != "" {
		// Create the DEDICATED per-workspace network as INTERNAL (no external
		// route). Idempotent. This is the isolation boundary — the sidecar is on
		// ONLY this network, so it has NO egress of its own and can reach nothing
		// but the egress proxy below (§6.1, structural isolation).
		if _, err := p.cli.NetworkCreate(ctx, net, network.CreateOptions{Labels: desktopManagedLabels(), Internal: true}); err != nil && !isNetworkExists(err) {
			return DesktopHandle{}, fmt.Errorf("create per-workspace network: %w", err)
		}
		netCfg.EndpointsConfig = map[string]*network.EndpointSettings{
			net: {Aliases: []string{name}},
		}
		// Bring up the per-workspace egress proxy on this internal net (its only
		// route out is the egress network; it DENIES private/link-local dsts). The
		// sidecar's browser reaches the internet ONLY through it — structural
		// isolation, no host firewall.
		if err := p.ensureEgressProxy(ctx, cfg.WorkspaceID, net); err != nil {
			return DesktopHandle{}, err
		}
	}

	// Env the sidecar's control server + entrypoint REQUIRE (reviewer B1): the
	// control server log.Fatals without DESKTOP_CONTROL_TOKEN, so an env-less
	// container boots and dies immediately. The token is derived from the shared
	// secret — identical to the gateway's TokenResolver — so the two agree with
	// nothing stored. Geometry is pinned to the coordinate contract. DESKTOP_PROXY
	// points Chromium at the per-workspace egress proxy (the sidecar's only way
	// out; empty when no per-workspace network, i.e. tests).
	env := []string{
		"DESKTOP_CONTROL_TOKEN=" + DeriveDesktopControlToken(p.controlTokenSecret, cfg.WorkspaceID),
		fmt.Sprintf("DESKTOP_WIDTH=%d", desktopWidth),
		fmt.Sprintf("DESKTOP_HEIGHT=%d", desktopHeight),
	}
	if p.perWorkspaceNetwork(cfg.WorkspaceID) != "" {
		env = append(env, fmt.Sprintf("DESKTOP_PROXY=http://%s:%d", DesktopProxyContainerName(cfg.WorkspaceID), desktopProxyPort))
	}
	if _, err := p.cli.ContainerCreate(ctx, &container.Config{
		Image:  p.image,
		Env:    env,
		Labels: desktopManagedLabels(),
	}, hostCfg, netCfg, nil, name); err != nil {
		return DesktopHandle{}, fmt.Errorf("create desktop sidecar: %w", err)
	}
	if err := p.cli.ContainerStart(ctx, name, container.StartOptions{}); err != nil {
		return DesktopHandle{}, fmt.Errorf("start desktop sidecar: %w", err)
	}
	// Attach the PLATFORM to the per-workspace network so the gateway + display
	// proxy can reach the sidecar by its "wsdesk-<id>" name (reviewer B3). The
	// sidecar stays OFF molecule-core-net — only the trusted platform joins the
	// isolated net, so tenant/backend isolation is preserved while the platform
	// gains reachability. Idempotent: an already-connected platform is fine.
	if net := p.perWorkspaceNetwork(cfg.WorkspaceID); net != "" && p.selfContainerID != "" {
		if err := p.cli.NetworkConnect(ctx, net, p.selfContainerID, nil); err != nil && !isAlreadyConnected(err) {
			return DesktopHandle{}, fmt.Errorf("attach platform to per-workspace network: %w", err)
		}
	}
	return p.handle(cfg.WorkspaceID, true), nil
}

// egressNetworkName is the network the proxy uses for internet egress.
func (p *LocalSidecarProvisioner) egressNetworkName() string {
	if p.egressNetwork == "" {
		return "bridge"
	}
	return p.egressNetwork
}

// ensureEgressProxy starts the per-workspace egress proxy (wsdeskproxy-<id>),
// idempotently. The proxy is created ON the internal per-workspace net (aliased
// so the sidecar reaches it by name) and ALSO attached to the egress network —
// its only route to the internet. It runs the SAME desktop image with the
// egress-proxy command (no separate image / supply-chain artifact), and is
// hardened like the sidecar (cap-drop, no-new-privileges). Its deny-list
// (cmd/desktop-egress-proxy) is what makes the sidecar's isolation structural.
func (p *LocalSidecarProvisioner) ensureEgressProxy(ctx context.Context, workspaceID, internalNet string) error {
	proxyName := DesktopProxyContainerName(workspaceID)
	if insp, err := p.cli.ContainerInspect(ctx, proxyName); err == nil {
		if insp.State != nil && insp.State.Running {
			return nil // already up
		}
		// Stale exited proxy — clear it so create doesn't 409.
		_ = p.cli.ContainerRemove(ctx, proxyName, container.RemoveOptions{Force: true})
	}

	hostCfg := &container.HostConfig{
		RestartPolicy: container.RestartPolicy{Name: "no"},
		CapDrop:       []string{"ALL"},
		SecurityOpt:   []string{"no-new-privileges"},
		// Cap the proxy's RAM (reviewer note): it is a tiny stream-copy forward
		// proxy, so 256MB is generous, and a compromised/runaway proxy must not
		// pressure the host. Swap pinned to the cap; shed the proxy first.
		Resources:   container.Resources{Memory: proxyMemoryLimitBytes, MemorySwap: proxyMemoryLimitBytes},
		OomScoreAdj: 500,
	}
	netCfg := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			internalNet: {Aliases: []string{proxyName}},
		},
	}
	if _, err := p.cli.ContainerCreate(ctx, &container.Config{
		Image: p.image,
		// Override the desktop ENTRYPOINT: this container runs the proxy binary
		// (under tini for signal/child handling), NOT the Xorg/Chromium desktop.
		Entrypoint: []string{"/usr/bin/tini", "--", "/usr/local/bin/desktop-egress-proxy"},
		Labels:     desktopManagedLabels(),
	}, hostCfg, netCfg, nil, proxyName); err != nil {
		return fmt.Errorf("create egress proxy: %w", err)
	}
	// Attach the egress leg (internet route) BEFORE start so the proxy has a
	// route out the moment it comes up.
	if err := p.cli.NetworkConnect(ctx, p.egressNetworkName(), proxyName, nil); err != nil && !isAlreadyConnected(err) {
		return fmt.Errorf("attach egress proxy to egress network: %w", err)
	}
	if err := p.cli.ContainerStart(ctx, proxyName, container.StartOptions{}); err != nil {
		return fmt.Errorf("start egress proxy: %w", err)
	}
	return nil
}

// stopEgressProxy tears down a workspace's egress proxy (best-effort). Called
// from StopDesktop before the network is removed.
func (p *LocalSidecarProvisioner) stopEgressProxy(ctx context.Context, workspaceID string) {
	proxyName := DesktopProxyContainerName(workspaceID)
	secs := int(p.stopTimeout.Seconds())
	if err := p.cli.ContainerStop(ctx, proxyName, container.StopOptions{Timeout: &secs}); err != nil && !isNoSuchContainer(err) {
		_ = err
	}
	if err := p.cli.ContainerRemove(ctx, proxyName, container.RemoveOptions{Force: true}); err != nil && !isNoSuchContainer(err) {
		_ = err
	}
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
	// Capture (do NOT early-return on) the sidecar stop/remove error: the egress
	// proxy + per-workspace network must still be torn down even when the sidecar
	// stop fails, or wsdeskproxy-<id> and the per-workspace network leak. Run the
	// proxy/network cleanup below unconditionally, then surface the captured error.
	var sidecarErr error
	if err := p.cli.ContainerStop(ctx, name, container.StopOptions{Timeout: &secs}); err != nil {
		if !isNoSuchContainer(err) {
			sidecarErr = fmt.Errorf("graceful stop desktop sidecar: %w", err)
		}
	} else if err := p.cli.ContainerRemove(ctx, name, container.RemoveOptions{}); err != nil && !isNoSuchContainer(err) {
		// Not Force: the container is already stopped, so a plain remove suffices
		// and there is no SIGKILL involved.
		sidecarErr = fmt.Errorf("remove desktop sidecar: %w", err)
	}
	// Tear down the egress proxy too (it holds an endpoint on the per-workspace
	// net, so it MUST go before NetworkRemove).
	p.stopEgressProxy(ctx, workspaceID)
	// Best-effort: detach the platform, then remove the now-empty per-workspace
	// network so it doesn't leak. The platform must leave first or NetworkRemove
	// fails on a network that still has an endpoint. A network still holding an
	// endpoint (slow teardown) is left for a later sweep; don't fail the stop.
	if net := p.perWorkspaceNetwork(workspaceID); net != "" {
		if p.selfContainerID != "" {
			if err := p.cli.NetworkDisconnect(ctx, net, p.selfContainerID, true); err != nil && !isNoSuchNetwork(err) {
				_ = err
			}
		}
		if err := p.cli.NetworkRemove(ctx, net); err != nil && !isNoSuchNetwork(err) {
			_ = err
		}
	}
	return sidecarErr
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

// isAlreadyConnected matches Docker's error when a container is already an
// endpoint of a network — a benign no-op for the idempotent platform attach.
func isAlreadyConnected(err error) bool {
	return err != nil && strings.Contains(err.Error(), "already exists in network")
}
