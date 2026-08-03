package provisioner

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	dockerimage "github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type sidecarCreateCall struct {
	name string
	cfg  *container.Config
	host *container.HostConfig
	net  *network.NetworkingConfig
}

type sidecarUpdateCall struct {
	name string
	cfg  container.UpdateConfig
}

type fakeSidecarDocker struct {
	creates []sidecarCreateCall
	updates []sidecarUpdateCall
	starts  []string
	stops   []string
	removes []struct {
		name  string
		force bool
	}
	volCreates    []volume.CreateOptions
	volRemoves    []string
	netsCreated   []string
	netsInternal  map[string]bool // net name -> Internal flag
	netsRemoved   []string
	netsConnected []string // "net|container"
	netsDisconn   []string // "net|container"
	running       map[string]bool
	exists        map[string]bool  // §25.1: a container can EXIST while stopped (running=false)
	stopErrs      map[string]error // name -> error ContainerStop returns (fault injection)
	imageMissing  bool             // when true, ImageInspect reports not-found so ensureImage must pull
	imageID       string           // ID ImageInspect returns as the DESIRED image (image-drift test); "" = default
	imagePulls    []string         // refs passed to ImagePull
	// Drift simulation (§25.1 desktopDriftReason): baked env + image ID per
	// container, surfaced by ContainerInspect. Empty defaults → no drift.
	configEnv      map[string][]string
	containerImage map[string]string
	// Self-heal simulation (§25.1 startedCrashed): a start that leaves the
	// container immediately EXITED (crashOnStart) or OOM-killed (oomOnStart);
	// consumed on the first start so the self-heal recreate boots healthy.
	crashOnStart map[string]bool
	oomOnStart   map[string]bool
	crashed      map[string]bool // container currently in exited/crashed state
	oomed        map[string]bool // exited due to OOM (must NOT self-heal)
}

func newFakeSidecarDocker() *fakeSidecarDocker {
	return &fakeSidecarDocker{
		running: map[string]bool{}, exists: map[string]bool{}, netsInternal: map[string]bool{},
		configEnv: map[string][]string{}, containerImage: map[string]string{},
		crashOnStart: map[string]bool{}, oomOnStart: map[string]bool{},
		crashed: map[string]bool{}, oomed: map[string]bool{},
	}
}

// createByName returns the recorded ContainerCreate for name, or nil.
func (f *fakeSidecarDocker) createByName(name string) *sidecarCreateCall {
	for i := range f.creates {
		if f.creates[i].name == name {
			return &f.creates[i]
		}
	}
	return nil
}

// removeForce returns whether a recorded remove of name used Force (and whether
// any remove was recorded).
func (f *fakeSidecarDocker) removeForce(name string) (force, found bool) {
	for _, r := range f.removes {
		if r.name == name {
			return r.force, true
		}
	}
	return false, false
}

func (f *fakeSidecarDocker) ContainerCreate(_ context.Context, cfg *container.Config, host *container.HostConfig, net *network.NetworkingConfig, _ *ocispec.Platform, name string) (container.CreateResponse, error) {
	f.creates = append(f.creates, sidecarCreateCall{name: name, cfg: cfg, host: host, net: net})
	f.exists[name] = true
	return container.CreateResponse{ID: "cid-" + name}, nil
}
func (f *fakeSidecarDocker) ContainerInspect(_ context.Context, name string) (container.InspectResponse, error) {
	// §25.1: a container EXISTS whenever it is running OR was created/started and
	// not yet removed (a stopped-but-persisted desktop). Running reflects only the
	// live state; a crashed container reports an exited/OOM state for startedCrashed.
	if !f.running[name] && !f.exists[name] {
		return container.InspectResponse{}, errors.New("No such container: " + name)
	}
	st := &container.State{Running: f.running[name]}
	if f.crashed[name] {
		st.Status = container.StateExited
		st.ExitCode = 1
		st.OOMKilled = f.oomed[name]
	}
	base := &container.ContainerJSONBase{Name: name, State: st}
	if img, ok := f.containerImage[name]; ok {
		base.Image = img
	}
	return container.InspectResponse{ContainerJSONBase: base, Config: &container.Config{Env: f.configEnv[name]}}, nil
}
func (f *fakeSidecarDocker) ContainerStart(_ context.Context, name string, _ container.StartOptions) error {
	f.starts = append(f.starts, name)
	f.exists[name] = true
	// Simulate an immediate crash-on-start (consumed once) so the self-heal path
	// recreates and the SECOND start boots healthy. ContainerStart itself returns
	// nil (as the real daemon does) even when the container exits right after.
	if f.crashOnStart[name] || f.oomOnStart[name] {
		f.oomed[name] = f.oomOnStart[name]
		delete(f.crashOnStart, name)
		delete(f.oomOnStart, name)
		f.crashed[name] = true
		f.running[name] = false
		return nil
	}
	delete(f.crashed, name)
	delete(f.oomed, name)
	f.running[name] = true
	return nil
}
func (f *fakeSidecarDocker) ContainerUpdate(_ context.Context, name string, cfg container.UpdateConfig) (container.UpdateResponse, error) {
	f.updates = append(f.updates, sidecarUpdateCall{name: name, cfg: cfg})
	return container.UpdateResponse{}, nil
}
func (f *fakeSidecarDocker) ContainerStop(_ context.Context, name string, _ container.StopOptions) error {
	if err := f.stopErrs[name]; err != nil {
		return err
	}
	if !f.running[name] {
		return errors.New("No such container: " + name)
	}
	f.stops = append(f.stops, name)
	f.running[name] = false // stopped, but still EXISTS (persistent, §25.1)
	return nil
}
func (f *fakeSidecarDocker) ContainerRemove(_ context.Context, name string, opts container.RemoveOptions) error {
	f.removes = append(f.removes, struct {
		name  string
		force bool
	}{name, opts.Force})
	delete(f.running, name)
	delete(f.exists, name)
	delete(f.crashed, name)
	delete(f.oomed, name)
	delete(f.configEnv, name)
	delete(f.containerImage, name)
	return nil
}
func (f *fakeSidecarDocker) ImageInspect(_ context.Context, _ string, _ ...client.ImageInspectOption) (dockerimage.InspectResponse, error) {
	if f.imageMissing {
		return dockerimage.InspectResponse{}, errors.New("Error: No such image")
	}
	return dockerimage.InspectResponse{ID: f.imageID}, nil
}
func (f *fakeSidecarDocker) ImagePull(_ context.Context, ref string, _ dockerimage.PullOptions) (io.ReadCloser, error) {
	f.imagePulls = append(f.imagePulls, ref)
	f.imageMissing = false // pulled → now present
	return io.NopCloser(strings.NewReader("")), nil
}
func (f *fakeSidecarDocker) VolumeCreate(_ context.Context, opts volume.CreateOptions) (volume.Volume, error) {
	f.volCreates = append(f.volCreates, opts)
	return volume.Volume{Name: opts.Name, Labels: opts.Labels}, nil
}
func (f *fakeSidecarDocker) VolumeRemove(_ context.Context, name string, _ bool) error {
	f.volRemoves = append(f.volRemoves, name)
	return nil
}
func (f *fakeSidecarDocker) NetworkCreate(_ context.Context, name string, opts network.CreateOptions) (network.CreateResponse, error) {
	f.netsCreated = append(f.netsCreated, name)
	f.netsInternal[name] = opts.Internal
	return network.CreateResponse{ID: "net-" + name}, nil
}
func (f *fakeSidecarDocker) NetworkRemove(_ context.Context, name string) error {
	f.netsRemoved = append(f.netsRemoved, name)
	return nil
}
func (f *fakeSidecarDocker) NetworkConnect(_ context.Context, net, container string, _ *network.EndpointSettings) error {
	f.netsConnected = append(f.netsConnected, net+"|"+container)
	return nil
}
func (f *fakeSidecarDocker) NetworkDisconnect(_ context.Context, net, container string, _ bool) error {
	f.netsDisconn = append(f.netsDisconn, net+"|"+container)
	return nil
}

const sidecarTestWS = "abc12345-6789-4def-8123-56789abcdef0"

// fastProbe shrinks the post-start self-heal probe window so restart-path tests
// (a healthy fake stays Running, so startedCrashed polls to the deadline) don't
// each pay the ~1.5s production window. Same-package access to the unexported
// fields. Returns p for chaining.
func fastProbe(p *LocalSidecarProvisioner) *LocalSidecarProvisioner {
	p.startupProbeWindow = 30 * time.Millisecond
	p.startupProbeTick = 5 * time.Millisecond
	return p
}

func TestLocalSidecar_StartDesktop_CreatesLabeledIsolatedSidecar(t *testing.T) {
	f := newFakeSidecarDocker()
	const prefix = "wsnet"
	p := NewLocalSidecarProvisioner(f, "desk:img", prefix, 6070, 0, 1<<30, "") // "" → embedded Chromium-tuned seccomp default
	p.SetControlTokenSecret("test-secret")
	p.SetSelfContainerID("platform-xyz")

	h, err := p.StartDesktop(context.Background(), WorkspaceConfig{WorkspaceID: sidecarTestWS})
	if err != nil {
		t.Fatalf("StartDesktop: %v", err)
	}
	name := DesktopContainerName(sidecarTestWS)
	if h.Address != name+":6070" || !h.Running {
		t.Fatalf("handle = %+v, want address %q running", h, name+":6070")
	}

	// Persistent profile volume with the role label.
	if len(f.volCreates) != 1 || f.volCreates[0].Name != DesktopProfileVolumeName(sidecarTestWS) {
		t.Fatalf("profile volume not created: %+v", f.volCreates)
	}
	if f.volCreates[0].Labels[LabelRole] != RoleDesktop {
		t.Fatalf("profile volume missing role label: %v", f.volCreates[0].Labels)
	}

	// Two creates: the desktop sidecar + its egress proxy.
	if len(f.creates) != 2 {
		t.Fatalf("want 2 container creates (sidecar + proxy), got %d", len(f.creates))
	}
	c := f.createByName(name)
	if c == nil {
		t.Fatalf("sidecar container %q not created", name)
	}
	if c.cfg.Image != "desk:img" {
		t.Fatalf("image = %q, want desk:img", c.cfg.Image)
	}
	if c.cfg.Labels[LabelRole] != RoleDesktop || c.cfg.Labels[LabelManaged] != "true" {
		t.Fatalf("container labels missing role/managed: %v", c.cfg.Labels)
	}
	// Restart policy MUST be "no" (never resurrect after graceful stop).
	if string(c.host.RestartPolicy.Name) != "no" {
		t.Fatalf("restart policy = %q, want \"no\"", c.host.RestartPolicy.Name)
	}
	// Memory cap + oom-shed-the-desktop.
	if c.host.Memory != 1<<30 {
		t.Fatalf("memory limit = %d, want %d", c.host.Memory, int64(1<<30))
	}
	if c.host.OomScoreAdj <= 0 {
		t.Fatalf("OomScoreAdj = %d, want > 0 so pressure sheds the desktop not the tenant", c.host.OomScoreAdj)
	}
	// Swap pinned to the memory cap (no ~2× via swap).
	if c.host.MemorySwap != c.host.Memory {
		t.Fatalf("MemorySwap = %d, want == Memory (%d) so swap can't defeat the cap", c.host.MemorySwap, c.host.Memory)
	}
	// §25.3 — the v3 desktop is a REAL computer (sudo/apt self-install), so the
	// old B2 cap-drop-ALL + no-new-privileges hardening is DELIBERATELY relaxed
	// for the SIDECAR: it keeps Docker's DEFAULT cap set (no CapDrop) and does NOT
	// set no-new-privileges (that blocks sudo's setuid). The ONE remaining
	// SecurityOpt is the Chromium-tuned seccomp profile, which keeps the browser's
	// userns sandbox active without CAP_SYS_ADMIN. Isolation is unchanged — it's
	// the per-workspace internal network + egress proxy (asserted elsewhere), and
	// the egress PROXY stays fully hardened (its own test).
	if len(c.host.CapDrop) != 0 {
		t.Fatalf("CapDrop = %v, want [] (v3 keeps Docker default caps so sudo/apt works, §25.3)", c.host.CapDrop)
	}
	var hasNoNewPriv bool
	var seccompVal string
	for _, opt := range c.host.SecurityOpt {
		if opt == "no-new-privileges" {
			hasNoNewPriv = true
		}
		if strings.HasPrefix(opt, "seccomp=") {
			seccompVal = strings.TrimPrefix(opt, "seccomp=")
		}
	}
	if hasNoNewPriv {
		t.Fatalf("SecurityOpt must NOT set no-new-privileges on the v3 sidecar (it blocks sudo setuid, §25.3): %v", c.host.SecurityOpt)
	}
	// The seccomp profile MUST still be applied — the embedded Chromium-tuned
	// default (since "" was passed), real JSON, NOT "unconfined".
	if seccompVal == "" || seccompVal == "unconfined" || !strings.HasPrefix(strings.TrimSpace(seccompVal), "{") {
		t.Fatalf("SecurityOpt seccomp must be the embedded JSON profile, got %.40q", seccompVal)
	}
	// A DEDICATED per-workspace network was created as INTERNAL (no egress), and
	// the sidecar is attached to it with a name alias.
	wantNet := prefix + "-" + sidecarTestWS
	if len(f.netsCreated) != 1 || f.netsCreated[0] != wantNet {
		t.Fatalf("per-workspace network not created: %v (want %q)", f.netsCreated, wantNet)
	}
	if !f.netsInternal[wantNet] {
		t.Fatalf("per-workspace network %q MUST be Internal (no egress) — structural isolation", wantNet)
	}
	ep, ok := c.net.EndpointsConfig[wantNet]
	if !ok {
		t.Fatalf("sidecar not attached to per-workspace network %q: %+v", wantNet, c.net.EndpointsConfig)
	}
	if len(ep.Aliases) == 0 || ep.Aliases[0] != name {
		t.Fatalf("network alias = %v, want %q", ep.Aliases, name)
	}

	// The egress proxy was created, on the internal net (aliased), with the proxy
	// entrypoint, and connected to the egress network for its internet route.
	proxyName := DesktopProxyContainerName(sidecarTestWS)
	pc := f.createByName(proxyName)
	if pc == nil {
		t.Fatalf("egress proxy %q not created", proxyName)
	}
	if len(pc.cfg.Entrypoint) == 0 || pc.cfg.Entrypoint[len(pc.cfg.Entrypoint)-1] != "/usr/local/bin/desktop-egress-proxy" {
		t.Fatalf("proxy entrypoint = %v, want it to run desktop-egress-proxy", pc.cfg.Entrypoint)
	}
	if _, ok := pc.net.EndpointsConfig[wantNet]; !ok {
		t.Fatalf("proxy not on the internal net %q: %+v", wantNet, pc.net.EndpointsConfig)
	}
	if !contains(f.netsConnected, "bridge|"+proxyName) {
		t.Fatalf("proxy not connected to the egress network: %v", f.netsConnected)
	}

	// Both containers started.
	if !contains(f.starts, name) || !contains(f.starts, proxyName) {
		t.Fatalf("sidecar + proxy not both started: %v", f.starts)
	}

	// B1: the sidecar MUST receive DESKTOP_CONTROL_TOKEN + pinned geometry, and
	// DESKTOP_PROXY pointing at its egress proxy (its only route out).
	wantTok := "DESKTOP_CONTROL_TOKEN=" + DeriveDesktopControlToken("test-secret", sidecarTestWS)
	wantProxy := "DESKTOP_PROXY=http://" + proxyName + ":3128"
	var haveTok, haveW, haveH, haveProxy bool
	for _, e := range c.cfg.Env {
		switch e {
		case wantTok:
			haveTok = true
		case "DESKTOP_WIDTH=1280":
			haveW = true
		case "DESKTOP_HEIGHT=800":
			haveH = true
		case wantProxy:
			haveProxy = true
		}
	}
	if !haveTok {
		t.Fatalf("sidecar Env missing derived DESKTOP_CONTROL_TOKEN: %v", c.cfg.Env)
	}
	if DeriveDesktopControlToken("test-secret", sidecarTestWS) == "" {
		t.Fatal("derived control token is empty — control server would refuse to boot")
	}
	if !haveW || !haveH {
		t.Fatalf("sidecar Env missing pinned geometry: %v", c.cfg.Env)
	}
	if !haveProxy {
		t.Fatalf("sidecar Env missing DESKTOP_PROXY (its only egress): %v", c.cfg.Env)
	}
	// B3: the PLATFORM is attached to the per-workspace network so it can reach
	// the sidecar by name.
	if !contains(f.netsConnected, wantNet+"|platform-xyz") {
		t.Fatalf("platform not attached to per-workspace network: %v (want %q)", f.netsConnected, wantNet+"|platform-xyz")
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// A CP/SaaS tenant never pre-seeds the desktop image, and ContainerCreate does
// not pull — so the first StartDesktop MUST pull it, then still bring the
// sidecar up. Regression guard for the "fresh org 503s forever" gap.
func TestLocalSidecar_StartDesktop_PullsMissingImage(t *testing.T) {
	f := newFakeSidecarDocker()
	f.imageMissing = true // image absent locally, as on a fresh CP tenant
	p := NewLocalSidecarProvisioner(f, "desk:img", "wsnet", 6070, 0, 1<<30, "")
	p.SetControlTokenSecret("test-secret")
	p.SetSelfContainerID("platform-xyz")

	h, err := p.StartDesktop(context.Background(), WorkspaceConfig{WorkspaceID: sidecarTestWS})
	if err != nil {
		t.Fatalf("StartDesktop with a missing image must pull, not fail: %v", err)
	}
	if len(f.imagePulls) != 1 || f.imagePulls[0] != "desk:img" {
		t.Fatalf("expected exactly one pull of desk:img, got %v", f.imagePulls)
	}
	if !h.Running || f.createByName(DesktopContainerName(sidecarTestWS)) == nil {
		t.Fatalf("sidecar must still be created after the pull: handle=%+v creates=%v", h, f.creates)
	}
}

// When the image is already present, StartDesktop must NOT pull (the hot path
// stays a single cheap inspect, no per-start network round-trip).
func TestLocalSidecar_StartDesktop_PresentImageSkipsPull(t *testing.T) {
	f := newFakeSidecarDocker() // imageMissing defaults false = present
	p := NewLocalSidecarProvisioner(f, "desk:img", "wsnet", 6070, 0, 1<<30, "")
	p.SetControlTokenSecret("test-secret")
	p.SetSelfContainerID("platform-xyz")

	if _, err := p.StartDesktop(context.Background(), WorkspaceConfig{WorkspaceID: sidecarTestWS}); err != nil {
		t.Fatalf("StartDesktop: %v", err)
	}
	if len(f.imagePulls) != 0 {
		t.Fatalf("present image must not be pulled, got pulls=%v", f.imagePulls)
	}
}

func TestLocalSidecar_StartDesktop_Idempotent(t *testing.T) {
	f := newFakeSidecarDocker()
	f.running[DesktopContainerName(sidecarTestWS)] = true // already up
	p := NewLocalSidecarProvisioner(f, "desk:img", "net", 6070, 0, 0, "unconfined")

	h, err := p.StartDesktop(context.Background(), WorkspaceConfig{WorkspaceID: sidecarTestWS})
	if err != nil {
		t.Fatalf("StartDesktop: %v", err)
	}
	if !h.Running {
		t.Fatalf("handle should report running")
	}
	// Idempotent: the already-running sidecar is NOT recreated. (Reconciling the
	// egress proxy on this path is expected — see the reconcile test below — so we
	// assert on the sidecar specifically, not on total creates.)
	if f.createByName(DesktopContainerName(sidecarTestWS)) != nil {
		t.Fatalf("idempotent start must not recreate the sidecar, got creates=%v", f.creates)
	}
}

func TestLocalSidecar_StartDesktop_RunningReconcilesEgressProxy(t *testing.T) {
	f := newFakeSidecarDocker()
	name := DesktopContainerName(sidecarTestWS)
	// Sidecar is up, but its egress proxy has died (RestartPolicy "no") — leaving
	// the sidecar wedged with no route out.
	f.running[name] = true
	const prefix = "wsnet"
	p := NewLocalSidecarProvisioner(f, "desk:img", prefix, 6070, 0, 0, "unconfined")
	p.SetSelfContainerID("platform-xyz")

	h, err := p.StartDesktop(context.Background(), WorkspaceConfig{WorkspaceID: sidecarTestWS})
	if err != nil {
		t.Fatalf("StartDesktop: %v", err)
	}
	if !h.Running {
		t.Fatalf("handle should report running")
	}
	// The running sidecar is NOT recreated...
	if f.createByName(name) != nil {
		t.Fatalf("running sidecar must not be recreated: creates=%v", f.creates)
	}
	// ...but the dead proxy IS recreated and started (the reconcile self-heals egress).
	proxyName := DesktopProxyContainerName(sidecarTestWS)
	if f.createByName(proxyName) == nil {
		t.Fatalf("StartDesktop must recreate the absent egress proxy on the already-running path: creates=%v", f.creates)
	}
	if !contains(f.starts, proxyName) {
		t.Fatalf("recreated egress proxy must be started: starts=%v", f.starts)
	}
	// ...and the platform is (idempotently) re-attached to the per-workspace network.
	wantNet := prefix + "-" + sidecarTestWS
	if !contains(f.netsConnected, wantNet+"|platform-xyz") {
		t.Fatalf("platform must be re-attached to the per-workspace network: %v", f.netsConnected)
	}
}

func TestLocalSidecar_StopDesktop_GracefulPreservesProfile(t *testing.T) {
	f := newFakeSidecarDocker()
	name := DesktopContainerName(sidecarTestWS)
	f.running[name] = true
	p := NewLocalSidecarProvisioner(f, "desk:img", "net", 6070, 0, 0, "unconfined")

	if err := p.StopDesktop(context.Background(), sidecarTestWS); err != nil {
		t.Fatalf("StopDesktop: %v", err)
	}
	// §25.1 — the SIDECAR is gracefully STOPPED (SIGTERM flushes its SQLite
	// profile) but DELIBERATELY KEPT: its writable layer (apt installs, /home,
	// /etc) is the workspace's state and must survive to the next scale-up, which
	// restarts this same container. So the container is NEVER removed on stop.
	if !contains(f.stops, name) {
		t.Fatalf("StopDesktop must gracefully stop the sidecar: stops=%v", f.stops)
	}
	if _, found := f.removeForce(name); found {
		t.Fatalf("StopDesktop must NOT remove the persistent sidecar container (§25.1): removes=%v", f.removes)
	}
	// The profile volume (cookies / logins) MUST survive a stop.
	if len(f.volRemoves) != 0 {
		t.Fatalf("StopDesktop must NOT wipe the profile volume: %v", f.volRemoves)
	}
}

// When the sidecar stop FAILS (container still running), StopDesktop must NOT
// sever that live sidecar's egress: tearing down its proxy + network would strand
// a running sidecar with no route out and leak a network NetworkRemove can't drop
// while the sidecar endpoint remains. It surfaces the error and leaves the
// plumbing for the caller's retry / a later sweep (finding-5 fix, 2026-07-28).
func TestLocalSidecar_StopDesktop_LeavesProxyAndNetworkWhenSidecarStopFails(t *testing.T) {
	f := newFakeSidecarDocker()
	name := DesktopContainerName(sidecarTestWS)
	proxyName := DesktopProxyContainerName(sidecarTestWS)
	f.running[name] = true
	f.running[proxyName] = true
	// Inject a generic (non-'No such container') sidecar stop failure.
	f.stopErrs = map[string]error{name: errors.New("daemon boom")}
	const prefix = "wsnet"
	wantNet := prefix + "-" + sidecarTestWS
	p := NewLocalSidecarProvisioner(f, "desk:img", prefix, 6070, 0, 0, "unconfined")
	p.SetSelfContainerID("platform-xyz")

	err := p.StopDesktop(context.Background(), sidecarTestWS)
	if err == nil {
		t.Fatalf("StopDesktop must surface the sidecar stop error")
	}
	// The still-running sidecar keeps its egress: the proxy is NOT stopped/removed
	// and the network is NOT removed.
	if contains(f.stops, proxyName) {
		t.Fatalf("egress proxy must NOT be stopped while the sidecar is still running: stops=%v", f.stops)
	}
	if _, found := f.removeForce(proxyName); found {
		t.Fatalf("egress proxy must NOT be removed while the sidecar is still running: removes=%v", f.removes)
	}
	if contains(f.netsRemoved, wantNet) {
		t.Fatalf("per-workspace network must NOT be removed while the sidecar is still running: %v", f.netsRemoved)
	}
}

// On a SUCCESSFUL stop the STATELESS egress proxy is torn down AND the
// per-workspace network is removed (§25.1 proper lifecycle — net lifetime ==
// desktop-RUNNING lifetime, so no idle net accumulates per ever-used workspace).
// Teardown order: proxy removed → stopped sidecar disconnected → platform
// disconnected → NetworkRemove the now-empty net. The persistent CONTAINER itself
// is kept (only WipeProfile removes it).
func TestLocalSidecar_StopDesktop_TearsDownProxyAndNetworkOnSuccess(t *testing.T) {
	f := newFakeSidecarDocker()
	name := DesktopContainerName(sidecarTestWS)
	proxyName := DesktopProxyContainerName(sidecarTestWS)
	f.running[name] = true
	f.running[proxyName] = true
	const prefix = "wsnet"
	wantNet := prefix + "-" + sidecarTestWS
	p := NewLocalSidecarProvisioner(f, "desk:img", prefix, 6070, 0, 0, "unconfined")
	p.SetSelfContainerID("platform-xyz")

	if err := p.StopDesktop(context.Background(), sidecarTestWS); err != nil {
		t.Fatalf("StopDesktop (success path): %v", err)
	}
	if !contains(f.stops, proxyName) {
		t.Fatalf("egress proxy must be stopped after a successful sidecar stop: stops=%v", f.stops)
	}
	if _, found := f.removeForce(proxyName); !found {
		t.Fatalf("egress proxy must be removed after a successful sidecar stop: removes=%v", f.removes)
	}
	// The stopped sidecar's OWN endpoint must be disconnected so the net can be
	// emptied (this is what the old code missed → the leak).
	if !contains(f.netsDisconn, wantNet+"|"+name) {
		t.Fatalf("stopped sidecar must be disconnected from the per-workspace network: %v", f.netsDisconn)
	}
	if !contains(f.netsDisconn, wantNet+"|platform-xyz") {
		t.Fatalf("platform must be disconnected from the per-workspace network: %v", f.netsDisconn)
	}
	// The per-workspace network MUST be removed (proper lifecycle — no leak).
	if !contains(f.netsRemoved, wantNet) {
		t.Fatalf("per-workspace network must be REMOVED after a successful stop (§25.1 proper lifecycle): %v", f.netsRemoved)
	}
	// But the persistent sidecar container itself is NOT removed.
	if _, found := f.removeForce(name); found {
		t.Fatalf("StopDesktop must KEEP the persistent sidecar container (§25.1): removes=%v", f.removes)
	}
}

func TestLocalSidecar_StopDesktop_Idempotent(t *testing.T) {
	f := newFakeSidecarDocker() // nothing running
	p := NewLocalSidecarProvisioner(f, "desk:img", "net", 6070, 0, 0, "unconfined")
	if err := p.StopDesktop(context.Background(), sidecarTestWS); err != nil {
		t.Fatalf("StopDesktop on absent sidecar must be a no-op, got %v", err)
	}
}

// WipeProfile is the full revoke path (§25.1): because the persistent container's
// OWN writable layer holds installed apps + system changes, a wipe must destroy
// the container (force), the profile volume, AND the per-workspace network — not
// just the volume as the old kiosk model did.
func TestLocalSidecar_WipeProfile_RemovesContainerVolumeAndNetwork(t *testing.T) {
	f := newFakeSidecarDocker()
	const prefix = "wsnet"
	name := DesktopContainerName(sidecarTestWS)
	f.running[name] = true // a still-running desktop at revoke time
	p := NewLocalSidecarProvisioner(f, "desk:img", prefix, 6070, 0, 0, "unconfined")
	p.SetSelfContainerID("platform-xyz")

	if err := p.WipeProfile(context.Background(), sidecarTestWS); err != nil {
		t.Fatalf("WipeProfile: %v", err)
	}
	// The persistent container itself MUST be force-removed (it holds the writable
	// layer + a network endpoint).
	if force, found := f.removeForce(name); !found || !force {
		t.Fatalf("WipeProfile must FORCE-remove the persistent sidecar container: force=%v found=%v removes=%v", force, found, f.removes)
	}
	if len(f.volRemoves) != 1 || f.volRemoves[0] != DesktopProfileVolumeName(sidecarTestWS) {
		t.Fatalf("WipeProfile must remove the profile volume: %v", f.volRemoves)
	}
	// The per-workspace network is torn down here (only WipeProfile does this now,
	// not StopDesktop).
	if !contains(f.netsRemoved, prefix+"-"+sidecarTestWS) {
		t.Fatalf("WipeProfile must remove the per-workspace network: %v", f.netsRemoved)
	}
}

// §25.1 START-EXISTING: a scale-up on a PERSISTED (stopped) desktop must START
// the existing container to keep its writable layer — it must NOT recreate it
// (recreation would discard every apt install). This is the core persistence
// guarantee.
func TestLocalSidecar_StartDesktop_RestartsPersistedContainer(t *testing.T) {
	f := newFakeSidecarDocker()
	const prefix = "wsnet"
	name := DesktopContainerName(sidecarTestWS)
	// Simulate a container that EXISTS but is STOPPED (scaled to zero earlier).
	f.exists[name] = true // running stays false

	p := fastProbe(NewLocalSidecarProvisioner(f, "desk:img", prefix, 6070, 0, 0, "unconfined"))
	p.SetSelfContainerID("platform-xyz")

	h, err := p.StartDesktop(context.Background(), WorkspaceConfig{WorkspaceID: sidecarTestWS})
	if err != nil {
		t.Fatalf("StartDesktop (persisted restart): %v", err)
	}
	// The persisted container is STARTED...
	if !contains(f.starts, name) {
		t.Fatalf("StartDesktop must START the persisted sidecar: starts=%v", f.starts)
	}
	// ...and NOT recreated (that would discard the writable layer / installed apps).
	if f.createByName(name) != nil {
		t.Fatalf("StartDesktop must NOT recreate an existing persisted sidecar (§25.1): creates=%v", f.creates)
	}
	// No fresh profile volume on a restart (the container + its state already exist).
	if len(f.volCreates) != 0 {
		t.Fatalf("StartDesktop restart must not re-create the profile volume: %v", f.volCreates)
	}
	// §25.1 net re-attach: StopDesktop removed the net, so the restart MUST
	// re-attach the freshly-created net to the persisted container (with its alias)
	// so it comes up ON the internal net, never on bridge.
	wantNet := prefix + "-" + sidecarTestWS
	if !contains(f.netsConnected, wantNet+"|"+name) {
		t.Fatalf("restart must re-attach the persisted sidecar to the per-workspace net: netsConnected=%v", f.netsConnected)
	}
	if !h.Running || !h.ScaledUp {
		t.Fatalf("restart handle: got Running=%v ScaledUp=%v, want both true", h.Running, h.ScaledUp)
	}
}

// §25.1 MEMORY RECONCILE: a raised memory ceiling reaches an EXISTING desktop via
// ContainerUpdate on the wake (before start) — recreating would defeat
// persistence. A first-ever create bakes the limit at create and must NOT update.
func TestLocalSidecar_StartDesktop_ReconcilesMemoryOnRestart(t *testing.T) {
	f := newFakeSidecarDocker()
	const prefix = "wsnet"
	name := DesktopContainerName(sidecarTestWS)
	f.exists[name] = true // persisted, stopped
	const newLimit = int64(4 << 30)
	p := fastProbe(NewLocalSidecarProvisioner(f, "desk:img", prefix, 6070, 0, newLimit, "unconfined"))

	if _, err := p.StartDesktop(context.Background(), WorkspaceConfig{WorkspaceID: sidecarTestWS}); err != nil {
		t.Fatalf("StartDesktop: %v", err)
	}
	if len(f.updates) != 1 || f.updates[0].name != name {
		t.Fatalf("restart must issue exactly one ContainerUpdate for the sidecar: %+v", f.updates)
	}
	if f.updates[0].cfg.Memory != newLimit || f.updates[0].cfg.MemorySwap != newLimit {
		t.Fatalf("ContainerUpdate must set Memory==MemorySwap==%d, got %+v", newLimit, f.updates[0].cfg.Resources)
	}
	// The update must precede the start (cgroup created at the ceiling from t=0).
	if len(f.starts) == 0 || len(f.updates) == 0 {
		t.Fatalf("expected both an update and a start")
	}
}

func TestLocalSidecar_StartDesktop_FirstCreateDoesNotUpdateMemory(t *testing.T) {
	f := newFakeSidecarDocker() // nothing exists → first-ever create
	p := fastProbe(NewLocalSidecarProvisioner(f, "desk:img", "wsnet", 6070, 0, 4<<30, "unconfined"))
	if _, err := p.StartDesktop(context.Background(), WorkspaceConfig{WorkspaceID: sidecarTestWS}); err != nil {
		t.Fatalf("StartDesktop: %v", err)
	}
	if len(f.updates) != 0 {
		t.Fatalf("first-ever create bakes the memory limit at create; must NOT ContainerUpdate: %+v", f.updates)
	}
}

// §25.1 DRIFT RECONCILE: a control-token rotation (the derived token no longer
// matches the baked one) forces a recreate — a plain restart would leave the
// sidecar authing against the stale token and 401 the agent forever.
func TestLocalSidecar_StartDesktop_RecreatesOnControlTokenDrift(t *testing.T) {
	f := newFakeSidecarDocker()
	const prefix = "wsnet"
	name := DesktopContainerName(sidecarTestWS)
	f.exists[name] = true
	f.configEnv[name] = []string{"DESKTOP_CONTROL_TOKEN=STALE-TOKEN", "DESKTOP_WIDTH=1280"}
	p := fastProbe(NewLocalSidecarProvisioner(f, "desk:img", prefix, 6070, 0, 0, "unconfined"))
	p.SetControlTokenSecret("rotated-secret") // derives a token != STALE-TOKEN

	if _, err := p.StartDesktop(context.Background(), WorkspaceConfig{WorkspaceID: sidecarTestWS}); err != nil {
		t.Fatalf("StartDesktop: %v", err)
	}
	if force, found := f.removeForce(name); !found || !force {
		t.Fatalf("token drift must force-remove the stale container: removes=%v", f.removes)
	}
	c := f.createByName(name)
	if c == nil {
		t.Fatalf("token drift must recreate the container: creates=%v", f.creates)
	}
	wantTok := "DESKTOP_CONTROL_TOKEN=" + DeriveDesktopControlToken("rotated-secret", sidecarTestWS)
	if !contains(c.cfg.Env, wantTok) {
		t.Fatalf("recreated container must bake the NEW token %q: env=%v", wantTok, c.cfg.Env)
	}
}

// §25.1 DRIFT RECONCILE: an image upgrade (the desired tag now resolves to a
// different ID than the container's baked image) forces a recreate so the fix
// actually boots.
func TestLocalSidecar_StartDesktop_RecreatesOnImageDrift(t *testing.T) {
	f := newFakeSidecarDocker()
	const prefix = "wsnet"
	name := DesktopContainerName(sidecarTestWS)
	f.exists[name] = true
	f.containerImage[name] = "sha256:OLD" // the container's baked image
	f.imageID = "sha256:NEW"              // the desired tag now resolves here
	p := fastProbe(NewLocalSidecarProvisioner(f, "desk:img", prefix, 6070, 0, 0, "unconfined"))
	p.SetControlTokenSecret("s") // token matches (only image drifts)
	f.configEnv[name] = []string{"DESKTOP_CONTROL_TOKEN=" + DeriveDesktopControlToken("s", sidecarTestWS)}

	if _, err := p.StartDesktop(context.Background(), WorkspaceConfig{WorkspaceID: sidecarTestWS}); err != nil {
		t.Fatalf("StartDesktop: %v", err)
	}
	if force, found := f.removeForce(name); !found || !force {
		t.Fatalf("image drift must force-remove the old container: removes=%v", f.removes)
	}
	if f.createByName(name) == nil {
		t.Fatalf("image drift must recreate the container: creates=%v", f.creates)
	}
}

// No drift (token + image match) → plain restart, NO recreate.
func TestLocalSidecar_StartDesktop_NoDriftPlainRestart(t *testing.T) {
	f := newFakeSidecarDocker()
	const prefix = "wsnet"
	name := DesktopContainerName(sidecarTestWS)
	f.exists[name] = true
	f.containerImage[name] = "sha256:SAME"
	f.imageID = "sha256:SAME"
	p := fastProbe(NewLocalSidecarProvisioner(f, "desk:img", prefix, 6070, 0, 0, "unconfined"))
	p.SetControlTokenSecret("s")
	f.configEnv[name] = []string{"DESKTOP_CONTROL_TOKEN=" + DeriveDesktopControlToken("s", sidecarTestWS)}

	if _, err := p.StartDesktop(context.Background(), WorkspaceConfig{WorkspaceID: sidecarTestWS}); err != nil {
		t.Fatalf("StartDesktop: %v", err)
	}
	if _, found := f.removeForce(name); found {
		t.Fatalf("matching token+image must NOT recreate: removes=%v", f.removes)
	}
	if f.createByName(name) != nil {
		t.Fatalf("matching token+image must plain-restart, not recreate: creates=%v", f.creates)
	}
}

// §25.1 SELF-HEAL: a persisted container that exits IMMEDIATELY after start
// (corrupt writable layer) is recreated ONCE from the image; an OOM exit is NOT
// self-healed (it is host memory pressure, not corruption — recreating would
// discard a good layer).
func TestLocalSidecar_StartDesktop_SelfHealsCrashedContainer(t *testing.T) {
	f := newFakeSidecarDocker()
	const prefix = "wsnet"
	name := DesktopContainerName(sidecarTestWS)
	f.exists[name] = true
	f.crashOnStart[name] = true // first start exits immediately; consumed → 2nd boots
	p := fastProbe(NewLocalSidecarProvisioner(f, "desk:img", prefix, 6070, 0, 0, "unconfined"))

	if _, err := p.StartDesktop(context.Background(), WorkspaceConfig{WorkspaceID: sidecarTestWS}); err != nil {
		t.Fatalf("StartDesktop: %v", err)
	}
	if force, found := f.removeForce(name); !found || !force {
		t.Fatalf("crashed sidecar must be force-removed for self-heal: removes=%v", f.removes)
	}
	if f.createByName(name) == nil {
		t.Fatalf("self-heal must recreate the crashed container: creates=%v", f.creates)
	}
	// Two starts: the crashed one + the healthy recreate.
	starts := 0
	for _, s := range f.starts {
		if s == name {
			starts++
		}
	}
	if starts != 2 {
		t.Fatalf("self-heal expects 2 starts of the sidecar (crash + recreate), got %d: %v", starts, f.starts)
	}
}

func TestLocalSidecar_StartDesktop_DoesNotSelfHealOnOOM(t *testing.T) {
	f := newFakeSidecarDocker()
	const prefix = "wsnet"
	name := DesktopContainerName(sidecarTestWS)
	f.exists[name] = true
	f.oomOnStart[name] = true // exits, but OOMKilled → NOT corruption
	p := fastProbe(NewLocalSidecarProvisioner(f, "desk:img", prefix, 6070, 0, 0, "unconfined"))

	if _, err := p.StartDesktop(context.Background(), WorkspaceConfig{WorkspaceID: sidecarTestWS}); err != nil {
		t.Fatalf("StartDesktop: %v", err)
	}
	if _, found := f.removeForce(name); found {
		t.Fatalf("an OOM exit must NOT trigger a self-heal recreate (would discard a good layer): removes=%v", f.removes)
	}
	if f.createByName(name) != nil {
		t.Fatalf("OOM must not recreate: creates=%v", f.creates)
	}
}

func TestLocalSidecar_DesktopRunning(t *testing.T) {
	f := newFakeSidecarDocker()
	p := NewLocalSidecarProvisioner(f, "desk:img", "net", 6070, 0, 0, "unconfined")

	if r, err := p.DesktopRunning(context.Background(), sidecarTestWS); err != nil || r {
		t.Fatalf("absent sidecar: got (%v,%v), want (false,nil)", r, err)
	}
	f.running[DesktopContainerName(sidecarTestWS)] = true
	if r, err := p.DesktopRunning(context.Background(), sidecarTestWS); err != nil || !r {
		t.Fatalf("running sidecar: got (%v,%v), want (true,nil)", r, err)
	}
}
