package provisioner

import (
	"context"
	"errors"
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
	creates    []sidecarCreateCall
	starts     []string
	stops      []string
	removes    []struct {
		name  string
		force bool
	}
	volCreates  []volume.CreateOptions
	volRemoves  []string
	netsCreated []string
	netsRemoved []string
	running     map[string]bool
}

func newFakeSidecarDocker() *fakeSidecarDocker { return &fakeSidecarDocker{running: map[string]bool{}} }

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
func (f *fakeSidecarDocker) NetworkCreate(_ context.Context, name string, _ network.CreateOptions) (network.CreateResponse, error) {
	f.netsCreated = append(f.netsCreated, name)
	return network.CreateResponse{ID: "net-" + name}, nil
}
func (f *fakeSidecarDocker) NetworkRemove(_ context.Context, name string) error {
	f.netsRemoved = append(f.netsRemoved, name)
	return nil
}

const sidecarTestWS = "abc12345-6789-4def-8123-56789abcdef0"

func TestLocalSidecar_StartDesktop_CreatesLabeledIsolatedSidecar(t *testing.T) {
	f := newFakeSidecarDocker()
	const prefix = "wsnet"
	p := NewLocalSidecarProvisioner(f, "desk:img", prefix, 6070, 0, 1<<30)

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

	if len(f.creates) != 1 {
		t.Fatalf("want exactly 1 container create, got %d", len(f.creates))
	}
	c := f.creates[0]
	if c.name != name {
		t.Fatalf("container name = %q, want %q", c.name, name)
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
	if c.host.Resources.Memory != 1<<30 {
		t.Fatalf("memory limit = %d, want %d", c.host.Resources.Memory, int64(1<<30))
	}
	if c.host.OomScoreAdj <= 0 {
		t.Fatalf("OomScoreAdj = %d, want > 0 so pressure sheds the desktop not the tenant", c.host.OomScoreAdj)
	}
	// A DEDICATED per-workspace network was created, and the sidecar is
	// attached to it (isolation boundary), with a name alias.
	wantNet := prefix + "-" + sidecarTestWS
	if len(f.netsCreated) != 1 || f.netsCreated[0] != wantNet {
		t.Fatalf("per-workspace network not created: %v (want %q)", f.netsCreated, wantNet)
	}
	ep, ok := c.net.EndpointsConfig[wantNet]
	if !ok {
		t.Fatalf("sidecar not attached to per-workspace network %q: %+v", wantNet, c.net.EndpointsConfig)
	}
	if len(ep.Aliases) == 0 || ep.Aliases[0] != name {
		t.Fatalf("network alias = %v, want %q", ep.Aliases, name)
	}

	if len(f.starts) != 1 || f.starts[0] != name {
		t.Fatalf("container not started: %v", f.starts)
	}
}

func TestLocalSidecar_StartDesktop_Idempotent(t *testing.T) {
	f := newFakeSidecarDocker()
	f.running[DesktopContainerName(sidecarTestWS)] = true // already up
	p := NewLocalSidecarProvisioner(f, "desk:img", "net", 6070, 0, 0)

	h, err := p.StartDesktop(context.Background(), WorkspaceConfig{WorkspaceID: sidecarTestWS})
	if err != nil {
		t.Fatalf("StartDesktop: %v", err)
	}
	if !h.Running {
		t.Fatalf("handle should report running")
	}
	if len(f.creates) != 0 {
		t.Fatalf("idempotent start must not create a second sidecar, got %d creates", len(f.creates))
	}
}

func TestLocalSidecar_StopDesktop_GracefulPreservesProfile(t *testing.T) {
	f := newFakeSidecarDocker()
	name := DesktopContainerName(sidecarTestWS)
	f.running[name] = true
	p := NewLocalSidecarProvisioner(f, "desk:img", "net", 6070, 0, 0)

	if err := p.StopDesktop(context.Background(), sidecarTestWS); err != nil {
		t.Fatalf("StopDesktop: %v", err)
	}
	// Graceful stop (SIGTERM), not force-remove.
	if len(f.stops) != 1 || f.stops[0] != name {
		t.Fatalf("StopDesktop must gracefully stop the container: stops=%v", f.stops)
	}
	if len(f.removes) != 1 || f.removes[0].force {
		t.Fatalf("StopDesktop remove must be non-force (graceful): removes=%+v", f.removes)
	}
	// The profile volume (cookies / logins) MUST survive a stop.
	if len(f.volRemoves) != 0 {
		t.Fatalf("StopDesktop must NOT wipe the profile volume: %v", f.volRemoves)
	}
}

func TestLocalSidecar_StopDesktop_Idempotent(t *testing.T) {
	f := newFakeSidecarDocker() // nothing running
	p := NewLocalSidecarProvisioner(f, "desk:img", "net", 6070, 0, 0)
	if err := p.StopDesktop(context.Background(), sidecarTestWS); err != nil {
		t.Fatalf("StopDesktop on absent sidecar must be a no-op, got %v", err)
	}
}

func TestLocalSidecar_WipeProfile_RemovesVolume(t *testing.T) {
	f := newFakeSidecarDocker()
	p := NewLocalSidecarProvisioner(f, "desk:img", "net", 6070, 0, 0)
	if err := p.WipeProfile(context.Background(), sidecarTestWS); err != nil {
		t.Fatalf("WipeProfile: %v", err)
	}
	if len(f.volRemoves) != 1 || f.volRemoves[0] != DesktopProfileVolumeName(sidecarTestWS) {
		t.Fatalf("WipeProfile must remove the profile volume: %v", f.volRemoves)
	}
}

func TestLocalSidecar_DesktopRunning(t *testing.T) {
	f := newFakeSidecarDocker()
	p := NewLocalSidecarProvisioner(f, "desk:img", "net", 6070, 0, 0)

	if r, err := p.DesktopRunning(context.Background(), sidecarTestWS); err != nil || r {
		t.Fatalf("absent sidecar: got (%v,%v), want (false,nil)", r, err)
	}
	f.running[DesktopContainerName(sidecarTestWS)] = true
	if r, err := p.DesktopRunning(context.Background(), sidecarTestWS); err != nil || !r {
		t.Fatalf("running sidecar: got (%v,%v), want (true,nil)", r, err)
	}
}
