package provisioner

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type sidecarCreateCall struct {
	name string
	cfg  *container.Config
	host *container.HostConfig
	net  *network.NetworkingConfig
}

type fakeSidecarDocker struct {
	creates []sidecarCreateCall
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
	stopErrs      map[string]error // name -> error ContainerStop returns (fault injection)
}

func newFakeSidecarDocker() *fakeSidecarDocker {
	return &fakeSidecarDocker{running: map[string]bool{}, netsInternal: map[string]bool{}}
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
	return container.CreateResponse{ID: "cid-" + name}, nil
}
func (f *fakeSidecarDocker) ContainerInspect(_ context.Context, name string) (container.InspectResponse, error) {
	if f.running[name] {
		return container.InspectResponse{ContainerJSONBase: &container.ContainerJSONBase{Name: name, State: &container.State{Running: true}}}, nil
	}
	return container.InspectResponse{}, errors.New("No such container: " + name)
}
func (f *fakeSidecarDocker) ContainerStart(_ context.Context, name string, _ container.StartOptions) error {
	f.starts = append(f.starts, name)
	f.running[name] = true
	return nil
}
func (f *fakeSidecarDocker) ContainerStop(_ context.Context, name string, _ container.StopOptions) error {
	if err := f.stopErrs[name]; err != nil {
		return err
	}
	if !f.running[name] {
		return errors.New("No such container: " + name)
	}
	f.stops = append(f.stops, name)
	f.running[name] = false
	return nil
}
func (f *fakeSidecarDocker) ContainerRemove(_ context.Context, name string, opts container.RemoveOptions) error {
	f.removes = append(f.removes, struct {
		name  string
		force bool
	}{name, opts.Force})
	delete(f.running, name)
	return nil
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
	// B2 hardening: CapDrop ALL + no-new-privileges + a NON-empty seccomp
	// profile (the embedded Chromium-tuned default, since "" was passed). This
	// is what keeps the browser's userns sandbox active with no CAP_SYS_ADMIN.
	if len(c.host.CapDrop) != 1 || c.host.CapDrop[0] != "ALL" {
		t.Fatalf("CapDrop = %v, want [ALL]", c.host.CapDrop)
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
	if !hasNoNewPriv {
		t.Fatalf("SecurityOpt missing no-new-privileges: %v", c.host.SecurityOpt)
	}
	// Embedded default must be real JSON, NOT "unconfined".
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
	// The SIDECAR is gracefully stopped (SIGTERM) and removed NON-force, so its
	// SQLite profile flushes cleanly (the proxy is stateless — its force-remove
	// doesn't matter).
	if !contains(f.stops, name) {
		t.Fatalf("StopDesktop must gracefully stop the sidecar: stops=%v", f.stops)
	}
	if force, found := f.removeForce(name); !found || force {
		t.Fatalf("sidecar remove must be non-force (graceful): force=%v found=%v", force, found)
	}
	// The profile volume (cookies / logins) MUST survive a stop.
	if len(f.volRemoves) != 0 {
		t.Fatalf("StopDesktop must NOT wipe the profile volume: %v", f.volRemoves)
	}
}

func TestLocalSidecar_StopDesktop_CleansUpProxyAndNetworkWhenSidecarStopFails(t *testing.T) {
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
	// Despite the sidecar stop failing, the egress proxy is still stopped + removed
	// so wsdeskproxy-<id> does not leak.
	if !contains(f.stops, proxyName) {
		t.Fatalf("egress proxy must still be stopped when the sidecar stop fails: stops=%v", f.stops)
	}
	if _, found := f.removeForce(proxyName); !found {
		t.Fatalf("egress proxy must still be removed when the sidecar stop fails: removes=%v", f.removes)
	}
	// ...and the per-workspace network is still disconnected + removed so it does
	// not leak either.
	if !contains(f.netsDisconn, wantNet+"|platform-xyz") {
		t.Fatalf("platform must still be disconnected from the per-workspace network: %v", f.netsDisconn)
	}
	if !contains(f.netsRemoved, wantNet) {
		t.Fatalf("per-workspace network must still be removed when the sidecar stop fails: %v", f.netsRemoved)
	}
}

func TestLocalSidecar_StopDesktop_Idempotent(t *testing.T) {
	f := newFakeSidecarDocker() // nothing running
	p := NewLocalSidecarProvisioner(f, "desk:img", "net", 6070, 0, 0, "unconfined")
	if err := p.StopDesktop(context.Background(), sidecarTestWS); err != nil {
		t.Fatalf("StopDesktop on absent sidecar must be a no-op, got %v", err)
	}
}

func TestLocalSidecar_WipeProfile_RemovesVolume(t *testing.T) {
	f := newFakeSidecarDocker()
	p := NewLocalSidecarProvisioner(f, "desk:img", "net", 6070, 0, 0, "unconfined")
	if err := p.WipeProfile(context.Background(), sidecarTestWS); err != nil {
		t.Fatalf("WipeProfile: %v", err)
	}
	if len(f.volRemoves) != 1 || f.volRemoves[0] != DesktopProfileVolumeName(sidecarTestWS) {
		t.Fatalf("WipeProfile must remove the profile volume: %v", f.volRemoves)
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
