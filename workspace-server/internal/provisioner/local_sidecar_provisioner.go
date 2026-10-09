package provisioner

import (
	"context"
	_ "embed"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	dockerimage "github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// desktopImagePullTimeout bounds the ONE-TIME pull of the desktop image when a
// tenant that has never run the desktop starts it (see ensureImage). Generous
// because the image is ~GB and a cold registry is slow, but bounded so a wedged
// registry can never leak the pull goroutine.
const desktopImagePullTimeout = 10 * time.Minute

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
	// ContainerUpdate reconciles a persisted (stopped) container's resource limits
	// on wake — §25.1: the memory ceiling is set at create, so a raised
	// MOLECULE_DESKTOP_MEMORY_BYTES only reaches an existing desktop by updating it
	// here (recreating would defeat persistence). Verified: an update to a STOPPED
	// container persists to the next start's memory.max.
	ContainerUpdate(ctx context.Context, containerID string, updateConfig container.UpdateConfig) (container.UpdateResponse, error)
	VolumeCreate(ctx context.Context, options volume.CreateOptions) (volume.Volume, error)
	VolumeRemove(ctx context.Context, volumeID string, force bool) error
	NetworkCreate(ctx context.Context, name string, options network.CreateOptions) (network.CreateResponse, error)
	NetworkRemove(ctx context.Context, networkID string) error
	NetworkConnect(ctx context.Context, networkID, containerID string, config *network.EndpointSettings) error
	NetworkDisconnect(ctx context.Context, networkID, containerID string, force bool) error
	// ImageInspect + ImagePull back ensureImage: ContainerCreate does NOT pull,
	// so a CP/SaaS tenant that never pre-seeded the desktop image must fetch it
	// on first StartDesktop. Signatures match the *client.Client SDK (v28).
	ImageInspect(ctx context.Context, image string, opts ...client.ImageInspectOption) (dockerimage.InspectResponse, error)
	ImagePull(ctx context.Context, ref string, opts dockerimage.PullOptions) (io.ReadCloser, error)
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

	// reconcileMu guards lastEgressReconcile. StartDesktop runs on the gateway's
	// hot path (every screenshot/input), so the already-running fast path only
	// reconciles egress plumbing at most once per egressReconcileInterval per
	// workspace — see reconcileEgressBestEffort.
	reconcileMu         sync.Mutex
	lastEgressReconcile map[string]time.Time

	// startupProbeWindow/startupProbeTick bound the post-start liveness probe
	// (§25.1 self-heal): after a persisted container is restarted, poll for up to
	// startupProbeWindow (every startupProbeTick) to catch a container that exits
	// IMMEDIATELY — the signature of a corrupt writable layer / a boot-precondition
	// failure — and recreate it once. Overridable by tests to keep them fast.
	startupProbeWindow time.Duration
	startupProbeTick   time.Duration
}

// desktopStartupProbeWindow/Tick default the post-start self-heal probe. A
// HEALTHY cold wake pays up to the full window once per scale-up (never on the
// screenshot/input hot loop); a corrupt-layer crash is caught within a tick or
// two. Kept modest so cold-wake latency stays low.
const (
	desktopStartupProbeWindow = 1500 * time.Millisecond
	desktopStartupProbeTick   = 150 * time.Millisecond
)

// egressReconcileInterval throttles the best-effort egress reconcile on
// StartDesktop's already-running fast path. StartDesktop is called on every
// screenshot/input, but a dead egress proxy (RestartPolicy "no") is rare, so
// reconciling once per this window per workspace keeps the interactive loop
// cheap while still self-healing egress within the interval.
const egressReconcileInterval = 30 * time.Second

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
		cli:                 cli,
		image:               image,
		networkPrefix:       networkPrefix,
		controlPort:         controlPort,
		stopTimeout:         stopTimeout,
		memoryLimitBytes:    memoryLimitBytes,
		seccompProfile:      seccompProfile,
		lastEgressReconcile: make(map[string]time.Time),
		startupProbeWindow:  desktopStartupProbeWindow,
		startupProbeTick:    desktopStartupProbeTick,
	}
}

// desktopSecurityOpt is the SIDECAR's security options (§25.3): the seccomp
// profile ONLY (the embedded Chromium-tuned default unless an operator supplied
// one, or "unconfined" to disable). The v3 desktop must allow sudo/apt, so —
// unlike the egress proxy, which stays hardened with no-new-privileges + cap-drop
// ALL (hardcoded in ensureEgressProxy) — no-new-privileges is omitted here (it
// blocks sudo's setuid) and the default cap set is kept (cap-drop ALL is NOT
// applied). The Chromium-tuned seccomp profile stays so the browser's
// unprivileged userns sandbox works without CAP_SYS_ADMIN. See the hostCfg
// comment in StartDesktop and seccomp/README.md.
func (p *LocalSidecarProvisioner) desktopSecurityOpt() []string {
	if p.seccompProfile == "unconfined" {
		return []string{"seccomp=unconfined"}
	}
	return []string{"seccomp=" + p.seccompProfile}
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

// reconcileEgressBestEffort re-establishes the sidecar's egress plumbing on the
// already-running fast path WITHOUT ever failing the caller and WITHOUT taxing
// the interactive loop. Screenshots/inputs on a healthy running desktop must
// never break because egress (which they don't need) hiccuped, so errors are
// logged and swallowed. StartDesktop is called on every screenshot/input, so a
// per-workspace throttle keeps real Docker work to at most once per
// egressReconcileInterval — a dead egress proxy self-heals within the interval
// instead of on every forward. The timestamp is stamped before the work so
// concurrent hot-path callers within the window skip the reconcile entirely.
func (p *LocalSidecarProvisioner) reconcileEgressBestEffort(ctx context.Context, workspaceID, net string) {
	p.reconcileMu.Lock()
	if p.lastEgressReconcile == nil {
		p.lastEgressReconcile = make(map[string]time.Time)
	}
	if last, ok := p.lastEgressReconcile[workspaceID]; ok && time.Since(last) < egressReconcileInterval {
		p.reconcileMu.Unlock()
		return
	}
	p.lastEgressReconcile[workspaceID] = time.Now()
	p.reconcileMu.Unlock()

	if err := p.ensureEgressProxy(ctx, workspaceID, net); err != nil {
		log.Printf("desktop: best-effort egress-proxy reconcile for %s failed (non-fatal, running desktop still serves): %v", workspaceID, err)
	}
	if p.selfContainerID != "" {
		if err := p.cli.NetworkConnect(ctx, net, p.selfContainerID, nil); err != nil && !isAlreadyConnected(err) {
			log.Printf("desktop: best-effort platform network attach for %s failed (non-fatal): %v", workspaceID, err)
		}
	}
}

// StartDesktop brings up the workspace's desktop sidecar, idempotently: if it
// is already running, it returns the handle without creating a second one (the
// agent's first tool call and a human opening the display can race, §10).
// ensureImage guarantees the desktop sidecar image is present locally before
// ContainerCreate (which never pulls). Present-image is the common case and
// costs one cheap local inspect. When absent — a CP/SaaS tenant on first use —
// it pulls, DETACHED from the caller's context: StartDesktop sits on the
// gateway hot path, so the first (necessarily slow, ~GB) screenshot's client
// timeout must not cancel the pull mid-stream and strand a half-image that
// every subsequent call re-pulls from zero. The pull is instead bounded by
// desktopImagePullTimeout; the first few starts return "not running yet" until
// it lands, exactly like a normal cold boot. A transient (non-not-found)
// inspect error is left for ContainerCreate to surface rather than forcing a
// needless pull.
func (p *LocalSidecarProvisioner) ensureImage(ctx context.Context) error {
	if _, err := p.cli.ImageInspect(ctx, p.image); err == nil {
		return nil
	} else if !isImageNotFound(err) {
		return nil
	}
	log.Printf("desktop: image %q not present — pulling (one-time, up to %s)", p.image, desktopImagePullTimeout)
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), desktopImagePullTimeout)
	defer cancel()
	rc, err := p.cli.ImagePull(pctx, p.image, dockerimage.PullOptions{})
	if err != nil {
		return fmt.Errorf("pull desktop image %q: %w", p.image, err)
	}
	defer func() { _ = rc.Close() }()
	// Drain the progress stream to completion — the pull is not finished until
	// the reader hits EOF; discarding is fine, we only need the image local.
	if _, err := io.Copy(io.Discard, rc); err != nil {
		return fmt.Errorf("pull desktop image %q (stream): %w", p.image, err)
	}
	log.Printf("desktop: image %q pulled", p.image)
	return nil
}

// isImageNotFound reports whether a Docker error means the image genuinely is
// not present locally, versus a transient daemon error. Same string-match
// rationale as isContainerNotFound (avoids an errdefs dependency; the message
// is stable across daemon versions and is what the Docker CLI itself matches).
func isImageNotFound(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "No such image") || strings.Contains(s, "not found")
}

func (p *LocalSidecarProvisioner) StartDesktop(ctx context.Context, cfg WorkspaceConfig) (DesktopHandle, error) {
	name := DesktopContainerName(cfg.WorkspaceID)

	// ONE up-front inspect yields BOTH existence and liveness (previously
	// DesktopRunning + containerExists each inspected — two round-trips on the hot
	// loop; now one).
	insp, inspErr := p.cli.ContainerInspect(ctx, name)
	if inspErr != nil && !isNoSuchContainer(inspErr) {
		return DesktopHandle{}, fmt.Errorf("inspect desktop sidecar: %w", inspErr)
	}
	exists := inspErr == nil
	running := exists && insp.State != nil && insp.State.Running

	if running {
		// Already-running fast path. RECONCILE the egress plumbing best-effort —
		// the egress proxy is RestartPolicy "no", so it can die while the sidecar
		// keeps running, and the platform's network attachment can be lost. But
		// this path is on the gateway's hot loop (Screenshot/Input call
		// ensureRunning -> StartDesktop on every forward), and a screenshot needs
		// NO egress, so the reconcile MUST NOT be able to fail the handle return:
		// a transient egress/Docker hiccup must never blind the agent on a healthy
		// running desktop. reconcileEgressBestEffort logs-and-continues and is
		// throttled to egressReconcileInterval so we don't pay 2 Docker round-trips
		// per screenshot. (Verified 2026-07-28: a fatal reconcile here regressed
		// the "agent can ALWAYS see" invariant.) Config reconciles (token/image
		// drift, memory ceiling, self-heal) are DELIBERATELY not done here — they
		// land on the next real cold-wake edge, never on the screenshot/input loop.
		if net := p.perWorkspaceNetwork(cfg.WorkspaceID); net != "" {
			p.reconcileEgressBestEffort(ctx, cfg.WorkspaceID, net)
		}
		return p.handle(cfg.WorkspaceID, true), nil
	}

	// §25.1 DRIFT RECONCILE (existing STOPPED container only): the control token
	// and the image are baked at create and IMMUTABLE across a plain restart, so
	// if either has drifted a ContainerStart can never deliver the fix — the
	// rotated-secret sidecar would 401 forever, the patched image would never boot.
	// Recreate on drift instead: the /home/desktop/profile volume (live logins) is
	// preserved; only the writable-layer apt installs are discarded, which is the
	// correct trade for a deliberate, rare token rotation / image upgrade.
	if exists {
		if reason := p.desktopDriftReason(ctx, insp, cfg.WorkspaceID); reason != "" {
			log.Printf("desktop: recreating sidecar %s due to drift (%s) — /home/desktop/profile (logins) preserved on the profile volume; writable-layer apt installs discarded", name, reason)
			if err := p.cli.ContainerRemove(ctx, name, container.RemoveOptions{Force: true}); err != nil && !isNoSuchContainer(err) {
				return DesktopHandle{}, fmt.Errorf("recreate desktop sidecar (remove drifted): %w", err)
			}
			exists = false // route through the create branch below → created=true
		}
	}

	// ContainerCreate does NOT pull. On self-host the desktop image is seeded by
	// the deploy; on a CP/SaaS tenant it is not, so the FIRST create must pull it
	// (design §5). Only gate this on the create path — a persisted restart runs the
	// image the container was already built from, so it needs no ImageInspect/pull.
	if !exists {
		if err := p.ensureImage(ctx); err != nil {
			return DesktopHandle{}, err
		}
	}

	// Per-workspace INTERNAL network + egress proxy — required whether we create or
	// restart. Both are idempotent and are rebuilt each wake: StopDesktop tears the
	// network + proxy down (§25.1 proper-lifecycle — the net's lifetime equals the
	// desktop's RUNNING lifetime, never accumulating one idle net per ever-used
	// workspace).
	var netCfg *network.NetworkingConfig
	if net := p.perWorkspaceNetwork(cfg.WorkspaceID); net != "" {
		// The DEDICATED per-workspace network is INTERNAL (no external route) —
		// the isolation boundary (§6.1): the sidecar is on ONLY this net, so it
		// has NO egress of its own and can reach nothing but the egress proxy.
		if _, err := p.cli.NetworkCreate(ctx, net, network.CreateOptions{Labels: desktopManagedLabels(), Internal: true}); err != nil && !isNetworkExists(err) {
			return DesktopHandle{}, fmt.Errorf("create per-workspace network: %w", err)
		}
		// Bring up the per-workspace egress proxy on this internal net (its only
		// route out is the egress network; it DENIES private/link-local dsts). The
		// sidecar's browser + apt reach the internet ONLY through it — structural
		// isolation, no host firewall.
		if err := p.ensureEgressProxy(ctx, cfg.WorkspaceID, net); err != nil {
			return DesktopHandle{}, err
		}
		netCfg = &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
			net: {Aliases: []string{name}},
		}}
	}

	// First-ever start OR drift-recreate: build the container (+ its profile
	// volume) from the current image/env via the shared helper, guaranteeing an
	// identical container on every create path. `created` is the sole coupling
	// flag: a freshly-created container already has the net (via netCfg), the
	// current memory ceiling, and a fresh image, so it SKIPS the memory reconcile,
	// the restart net re-attach, and the self-heal probe below.
	created := false
	if !exists {
		if err := p.createDesktopContainer(ctx, cfg, name, netCfg); err != nil {
			return DesktopHandle{}, err
		}
		created = true
	}

	// §25.1 MEMORY RECONCILE (persisted restart only): the ceiling is baked at
	// create, so a raised MOLECULE_DESKTOP_MEMORY_BYTES only reaches an existing
	// desktop by updating it here — BEFORE ContainerStart, so the cgroup is created
	// at the new ceiling from t=0 (verified: an update to a STOPPED container
	// persists to the next start's memory.max). Set BOTH Memory and MemorySwap
	// (equal = swap disabled). OomScoreAdj is create-only (not an UpdateConfig
	// field) — it never changes, so leaving it at its create value is correct.
	if !created && p.memoryLimitBytes > 0 {
		if _, err := p.cli.ContainerUpdate(ctx, name, container.UpdateConfig{
			Resources: container.Resources{Memory: p.memoryLimitBytes, MemorySwap: p.memoryLimitBytes},
		}); err != nil {
			return DesktopHandle{}, fmt.Errorf("reconcile desktop memory ceiling: %w", err)
		}
	}

	// §25.1 RESTART NET RE-ATTACH (persisted restart only): StopDesktop removed the
	// per-workspace net and with it this container's endpoint, so re-attach the
	// freshly-created net while the container is still STOPPED — it then comes up
	// already ON the internal net with its resolvable alias. (Verified: a container
	// created with an endpoint but restarted with ZERO endpoints comes up
	// loopback-only, NEVER on bridge — so even if this connect were skipped the
	// sidecar can never leak onto bridge; isolation is structural.) A
	// created/recreated container already got the net via netCfg at create.
	if net := p.perWorkspaceNetwork(cfg.WorkspaceID); net != "" && !created {
		if err := p.cli.NetworkConnect(ctx, net, name, &network.EndpointSettings{Aliases: []string{name}}); err != nil && !isAlreadyConnected(err) {
			return DesktopHandle{}, fmt.Errorf("attach desktop sidecar to per-workspace network: %w", err)
		}
	}

	// Start the container — the one just created, OR the persisted stopped one
	// (§25.1). ContainerStart re-runs the entrypoint, so its boot invariants
	// (stale-Singleton/X-lock cleanup, websockify heartbeat, proxy wiring) apply on
	// every wake.
	if err := p.cli.ContainerStart(ctx, name, container.StartOptions{}); err != nil {
		return DesktopHandle{}, fmt.Errorf("start desktop sidecar: %w", err)
	}

	// §25.1 SELF-HEAL (persisted restart only): a persisted container whose
	// writable layer is corrupt (half-finished apt, broken /etc) — or that hits a
	// boot-precondition failure — exits IMMEDIATELY. Without recreation every wake
	// restarts the same broken container and StartDesktop returns a healthy handle
	// over a dead desktop; only a destructive manual WipeProfile recovered. Detect
	// the immediate exit and recreate ONCE from the image. A just-created container
	// (created==true) that crashes is an image bug, not a stale layer, so it is not
	// re-healed here (the crash surfaces to the caller). Known limit: this cannot
	// distinguish a corrupt layer from a transient boot-precondition failure, so a
	// rare transient failure costs the (good) writable layer once — bounded, and
	// the profile volume is always kept.
	if !created && p.startedCrashed(ctx, name) {
		log.Printf("desktop: sidecar %s exited immediately after start — recreating once (corrupt-layer self-heal; /home/desktop/profile kept)", name)
		if err := p.cli.ContainerRemove(ctx, name, container.RemoveOptions{Force: true}); err != nil && !isNoSuchContainer(err) {
			return DesktopHandle{}, fmt.Errorf("self-heal remove corrupt desktop sidecar: %w", err)
		}
		if err := p.ensureImage(ctx); err != nil {
			return DesktopHandle{}, err
		}
		if err := p.createDesktopContainer(ctx, cfg, name, netCfg); err != nil {
			return DesktopHandle{}, err
		}
		if err := p.cli.ContainerStart(ctx, name, container.StartOptions{}); err != nil {
			return DesktopHandle{}, fmt.Errorf("start recreated desktop sidecar: %w", err)
		}
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
	// This call transitioned the desktop stopped -> running, so mark ScaledUp: the
	// gateway records the 'running' lifecycle state on exactly this edge rather
	// than on every subsequent hot-path forward.
	h := p.handle(cfg.WorkspaceID, true)
	h.ScaledUp = true
	return h, nil
}

// createDesktopContainer creates the persistent profile volume + the sidecar
// container from the CURRENT image/env/limits. It is the single create path —
// used by the first-ever start, the drift-recreate, and the self-heal recreate —
// so every create produces an identical container. netCfg attaches the
// per-workspace internal network at create time (nil in tests without a network).
func (p *LocalSidecarProvisioner) createDesktopContainer(ctx context.Context, cfg WorkspaceConfig, name string, netCfg *network.NetworkingConfig) error {
	// The profile volume (cookies / live logins) is bind-mounted separately and
	// SURVIVES a container recreate; only WipeProfile deletes it. Idempotent.
	if _, err := p.cli.VolumeCreate(ctx, volume.CreateOptions{
		Name:   DesktopProfileVolumeName(cfg.WorkspaceID),
		Labels: desktopManagedLabels(),
	}); err != nil {
		return fmt.Errorf("create desktop profile volume: %w", err)
	}

	hostCfg := &container.HostConfig{
		Binds: []string{DesktopProfileVolumeName(cfg.WorkspaceID) + ":/home/desktop/profile"},
		// "no": an on-demand sidecar must NOT resurrect after a graceful stop.
		// "on-failure" would restart on the SIGTERM exit-143 the graceful stop
		// produces; crash recovery is the platform re-calling StartDesktop
		// (§10). Never "unless-stopped".
		RestartPolicy: container.RestartPolicy{Name: "no"},
		// §25.3 — the desktop is a REAL computer: the agent/user can `sudo apt
		// install` anything (persists on the writable layer). That needs Docker's
		// DEFAULT cap set (dpkg wants CHOWN/DAC_OVERRIDE/FOWNER/SETUID/SETGID —
		// cap-drop ALL breaks it) and NO no-new-privileges (it blocks sudo's
		// setuid). We KEEP the Chromium-tuned seccomp profile so the browser's
		// unprivileged userns sandbox stays active (no --no-sandbox, no
		// CAP_SYS_ADMIN). The isolation boundary is UNCHANGED — the per-workspace
		// INTERNAL network + egress proxy (§6.1); relaxing the in-container caps
		// only lets the desktop modify its OWN rootfs, and daemon userns-remap
		// (verified at startup, cmd/server/main.go) keeps container-root != host
		// root. (The egress PROXY stays fully hardened — no-new-privileges +
		// cap-drop ALL, see ensureEgressProxy — it never needs sudo.)
		SecurityOpt: p.desktopSecurityOpt(),
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

	// Env the sidecar's control server + entrypoint REQUIRE (reviewer B1): the
	// control server log.Fatals without DESKTOP_CONTROL_TOKEN, so an env-less
	// container boots and dies immediately. The token is derived from the shared
	// secret — identical to the gateway's TokenResolver — so the two agree with
	// nothing stored. Geometry is pinned to the coordinate contract. DESKTOP_PROXY
	// points Chromium+apt at the per-workspace egress proxy (the sidecar's only
	// way out; empty when no per-workspace network, i.e. tests). Env is baked at
	// create time; a control-token rotation is picked up by the drift-recreate in
	// StartDesktop, not by mutating a running container.
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
		return fmt.Errorf("create desktop sidecar: %w", err)
	}
	return nil
}

// desktopDriftReason reports why a persisted sidecar must be RECREATED rather
// than plain-restarted, or "" when a plain restart is correct (§25.1). Drift is
// checked against config that is BAKED at create and cannot change on a restart:
//   - control-token: a rotated shared secret changes the derived token; the old
//     baked token would 401 the agent forever.
//   - image: an operator re-tag/upgrade (security or entrypoint fix) never lands
//     on a restart of the old image.
// It is FAIL-SAFE: any missing field or transient inspect error returns "" so a
// hiccup never destroys a good writable layer.
func (p *LocalSidecarProvisioner) desktopDriftReason(ctx context.Context, insp container.InspectResponse, workspaceID string) string {
	if insp.Config != nil {
		if baked, ok := envValue(insp.Config.Env, "DESKTOP_CONTROL_TOKEN"); ok {
			if want := DeriveDesktopControlToken(p.controlTokenSecret, workspaceID); baked != want {
				return "control-token rotated"
			}
		}
	}
	// Compare the container's image ID to the ID the desired tag resolves to
	// NOW. ImageInspect is a cheap LOCAL inspect (never a pull); a not-found /
	// error → "" (fail-safe), so a manually-deleted image just plain-restarts.
	if insp.ContainerJSONBase != nil && insp.Image != "" {
		if desired, err := p.cli.ImageInspect(ctx, p.image); err == nil && desired.ID != "" && desired.ID != insp.Image {
			return "image upgraded"
		}
	}
	return ""
}

// envValue returns the value of the first "KEY=..." entry in a container env
// slice, and whether it was present. nil-safe.
func envValue(env []string, key string) (string, bool) {
	prefix := key + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return strings.TrimPrefix(e, prefix), true
		}
	}
	return "", false
}

// startedCrashed reports whether the just-started sidecar exited IMMEDIATELY (the
// corrupt-layer / boot-precondition signature, §25.1 self-heal). It polls the
// container state up to startupProbeWindow; a healthy container that stays
// Running (or is merely slow to boot) returns false. OOMKilled is EXCLUDED: an
// OOM is host memory pressure (the sidecar carries OomScoreAdj 500 to be shed
// first), NOT layer corruption — recreating would discard a perfectly good layer.
// A transient inspect error or ctx cancellation is treated as healthy (never
// nuke a layer on a hiccup).
func (p *LocalSidecarProvisioner) startedCrashed(ctx context.Context, name string) bool {
	deadline := time.Now().Add(p.startupProbeWindow)
	for {
		insp, err := p.cli.ContainerInspect(ctx, name)
		if err == nil && insp.State != nil && !insp.State.Running && !insp.State.OOMKilled {
			switch {
			case insp.State.Status == container.StateExited || insp.State.Status == container.StateDead:
				return true
			case insp.State.Status == container.StateCreated && insp.State.ExitCode != 0:
				return true
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(p.startupProbeTick):
		}
	}
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

// StopDesktop is scale-to-zero for the PERSISTENT desktop (§25.1): it gracefully
// STOPS the sidecar (SIGTERM + a flush window so Chrome/Xorg close their SQLite
// profile cleanly) but DELIBERATELY KEEPS the container. Its writable layer
// (apt-installed apps, /home, /etc changes) is the workspace's state and must
// survive to the next scale-up, which RESTARTS this same container. So StopDesktop
// MUST NOT force-remove (a SIGKILL mid-write corrupts the profile) and MUST NOT
// plain-remove either (that would discard the persistent rootfs — the whole point
// of v3). Safe because the sidecar is RestartPolicy "no" (no resurrection race).
// The stateless egress proxy AND the per-workspace network are torn down here
// (proper lifecycle — net lifetime == desktop-RUNNING lifetime, no leaked idle
// nets) and rebuilt + re-attached by StartDesktop on the next wake. Only
// WipeProfile removes the container / volume. Idempotent.
func (p *LocalSidecarProvisioner) StopDesktop(ctx context.Context, workspaceID string) error {
	name := DesktopContainerName(workspaceID)
	secs := int(p.stopTimeout.Seconds())
	// Capture (do NOT early-return on) the sidecar stop outcome. isGone is true
	// only when the sidecar is actually down — the stop succeeded, or the
	// container was already absent. A genuine stop FAILURE leaves the sidecar
	// RUNNING, and it still needs its egress proxy: tearing that down under a live
	// sidecar would strand it egress-less. So the proxy teardown below is gated on
	// isGone; on a stop failure we surface the error and leave the plumbing for
	// the caller's retry or a later sweep to reclaim.
	var sidecarErr error
	isGone := true
	if err := p.cli.ContainerStop(ctx, name, container.StopOptions{Timeout: &secs}); err != nil {
		if !isNoSuchContainer(err) {
			sidecarErr = fmt.Errorf("graceful stop desktop sidecar: %w", err)
			isGone = false
		}
	}
	if !isGone {
		// Sidecar still running after a failed stop — do NOT sever its egress.
		return sidecarErr
	}
	// Sidecar is down (STOPPED, container KEPT — §25.1). Tear the per-workspace
	// network down as PROPER LIFECYCLE (not a background reaper): its lifetime
	// equals the desktop's RUNNING lifetime, so at most one idle net exists per
	// RUNNING desktop — never one per ever-used workspace (which would exhaust
	// Docker's address pool and block NEW desktops). Order is load-bearing:
	// stopEgressProxy drops the proxy's endpoint, then disconnect the stopped
	// sidecar's endpoint AND the platform's, then NetworkRemove the now-empty net.
	// Verified: force-disconnecting a STOPPED sidecar and removing the emptied
	// internal net both succeed. StartDesktop recreates net + proxy and re-attaches
	// the persisted container on the next wake. A transient teardown failure is
	// logged non-fatal and reclaimed on the next stop/WipeProfile lifecycle edge,
	// never by a sweep.
	p.stopEgressProxy(ctx, workspaceID)
	if net := p.perWorkspaceNetwork(workspaceID); net != "" {
		if err := p.cli.NetworkDisconnect(ctx, net, name, true); err != nil && !isNoSuchNetwork(err) && !isNoSuchContainer(err) {
			_ = err
		}
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

// WipeProfile is the full revoke/wipe path (§11, §25.1). Under the persistent-
// container model the desktop's OWN writable layer holds installed software and
// system changes, so a wipe must destroy the CONTAINER (not just the profile
// volume) — otherwise apt-installed apps and any tampering survive the revoke.
// It removes: the container (force — it may still be running at revoke time), the
// profile volume (cookies / live logins), the stateless egress proxy, and the
// now-empty per-workspace network. Idempotent: any already-absent piece is fine.
func (p *LocalSidecarProvisioner) WipeProfile(ctx context.Context, workspaceID string) error {
	// Force-remove the persistent container FIRST — it holds the writable layer
	// AND an endpoint on the per-workspace network (which must be empty before
	// NetworkRemove). Force so a still-running desktop is torn down in one call.
	if err := p.cli.ContainerRemove(ctx, DesktopContainerName(workspaceID), container.RemoveOptions{Force: true}); err != nil && !isNoSuchContainer(err) {
		return fmt.Errorf("wipe desktop container: %w", err)
	}
	if err := p.cli.VolumeRemove(ctx, DesktopProfileVolumeName(workspaceID), true); err != nil && !isNoSuchVolume(err) {
		return fmt.Errorf("wipe desktop profile volume: %w", err)
	}
	// Best-effort per-workspace infra teardown: the egress proxy (also holds a
	// network endpoint) then the network. Leaving these on a wipe would leak an
	// idle proxy + net per revoked workspace; a slow-teardown endpoint is left for
	// a later sweep rather than failing the wipe.
	p.stopEgressProxy(ctx, workspaceID)
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
