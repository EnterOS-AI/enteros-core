package provisioner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// The delete-vs-provision race (RC09), driven through the REAL Start against an
// in-memory daemon. The shape that leaked on the CI hosts: provisioning is in
// flight in its own goroutine (stuck in a slow image step), a DELETE lands —
// marks the row removed, finds no container, removes the config volume — and the
// provision then created ws-<id> anyway, which crash-looped on an empty config
// volume for months. These tests pin that a delete landing at any point of a
// Start leaves no container and no volume behind.

const raceWorkspaceID = "11111111-2222-3333-4444-555555555555"

// raceDocker is fakeDockerClient plus what Start needs end-to-end: an image step
// the test can hold open (a cold build / slow pull), containers that exist once
// created, calls that honour a cancelled ctx like the real client does, and an
// ordered event log.
type raceDocker struct {
	*fakeDockerClient

	inspectGate chan struct{} // nil = image step returns at once
	honourCtx   bool          // image step returns on ctx cancel (the real client does)
	inspecting  chan struct{} // closed when Start reaches the image step
	onStart     func()        // runs inside ContainerStart (a delete landing mid-start)

	events      []string
	createdHost []*container.HostConfig
	removeOpts  map[string]container.RemoveOptions
}

func newRaceDocker() *raceDocker {
	return &raceDocker{
		fakeDockerClient: newFakeDockerClient(),
		honourCtx:        true,
		inspecting:       make(chan struct{}),
		removeOpts:       map[string]container.RemoveOptions{},
	}
}

func (f *raceDocker) log(e string) {
	f.mu.Lock()
	f.events = append(f.events, e)
	f.mu.Unlock()
}

func (f *raceDocker) ImageInspect(ctx context.Context, img string, _ ...client.ImageInspectOption) (image.InspectResponse, error) {
	f.log("image-inspect")
	select {
	case <-f.inspecting:
	default:
		close(f.inspecting)
	}
	if f.inspectGate != nil {
		if f.honourCtx {
			select {
			case <-f.inspectGate:
			case <-ctx.Done():
				return image.InspectResponse{}, ctx.Err()
			}
		} else {
			<-f.inspectGate
		}
	}
	return image.InspectResponse{ID: "sha256:" + strings.Repeat("ab", 32)}, nil
}

func (f *raceDocker) VolumeCreate(ctx context.Context, o volume.CreateOptions) (volume.Volume, error) {
	f.log("volume-create:" + o.Name)
	return f.fakeDockerClient.VolumeCreate(ctx, o)
}

func (f *raceDocker) ContainerCreate(ctx context.Context, cfg *container.Config, host *container.HostConfig, nw *network.NetworkingConfig, pl *ocispec.Platform, name string) (container.CreateResponse, error) {
	if err := ctx.Err(); err != nil {
		return container.CreateResponse{}, err
	}
	f.log("container-create:" + name)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.containerCreateCalls = append(f.containerCreateCalls, name)
	f.createdHost = append(f.createdHost, host)
	f.containers[name] = container.InspectResponse{ContainerJSONBase: &container.ContainerJSONBase{Name: name, State: &container.State{}}}
	return container.CreateResponse{ID: name}, nil
}

func (f *raceDocker) ContainerStart(ctx context.Context, id string, _ container.StartOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.log("container-start:" + id)
	if f.onStart != nil {
		f.onStart()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.containerStartCalls = append(f.containerStartCalls, id)
	if c, ok := f.containers[id]; ok {
		c.State.Running = true
	}
	return nil
}

// Never answer an inspect of the new container: Start then skips its
// port-binding probe, which is not what these tests are about.
func (f *raceDocker) ContainerInspect(_ context.Context, name string) (container.InspectResponse, error) {
	return container.InspectResponse{}, errors.New("No such container: " + name)
}

func (f *raceDocker) ContainerRemove(ctx context.Context, name string, o container.RemoveOptions) error {
	f.log("container-remove:" + name)
	f.mu.Lock()
	f.removeOpts[name] = o
	f.mu.Unlock()
	return f.fakeDockerClient.ContainerRemove(ctx, name, o)
}

func (f *raceDocker) liveContainers() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for n := range f.containers {
		out = append(out, n)
	}
	return out
}

func (f *raceDocker) hasVolume(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.volumes[name]
	return ok
}

func raceConfig() WorkspaceConfig {
	// A pinned image (no local build, no re-pull) so the only slow step is the
	// one the test holds open.
	return WorkspaceConfig{WorkspaceID: raceWorkspaceID, Image: "registry.example/ws-template:1.2.3", Tier: 2}
}

func startAsync(p *Provisioner) <-chan error {
	errCh := make(chan error, 1)
	go func() {
		_, err := p.Start(context.Background(), raceConfig())
		errCh <- err
	}()
	return errCh
}

func waitErr(t *testing.T, errCh <-chan error) error {
	t.Helper()
	select {
	case err := <-errCh:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("Start did not return within 10s")
		return nil
	}
}

func assertNothingLeft(t *testing.T, f *raceDocker) {
	t.Helper()
	if live := f.liveContainers(); len(live) != 0 {
		t.Errorf("containers left behind after the delete: %v", live)
	}
	if f.hasVolume(ConfigVolumeName(raceWorkspaceID)) {
		t.Errorf("config volume %s left behind after the delete", ConfigVolumeName(raceWorkspaceID))
	}
}

// The CI shape: DELETE lands while Start is stuck in a slow image step. The
// delete marks the row removed and then does exactly what CascadeDelete does —
// CancelInflightStart, Stop, RemoveVolume. No container may exist afterwards.
func TestStart_DeleteDuringSlowImageStep_LeavesNoContainer(t *testing.T) {
	f := newRaceDocker()
	f.inspectGate = make(chan struct{}) // only the delete's cancel releases it
	p := &Provisioner{cli: f, alpineImage: "alpine"}
	var removed atomic.Bool
	p.SetWorkspaceRemovedCheck(func(context.Context, string) bool { return removed.Load() })

	errCh := startAsync(p)
	<-f.inspecting

	removed.Store(true) // CascadeDelete's first UPDATE
	if !p.CancelInflightStart(raceWorkspaceID, 5*time.Second) {
		t.Fatal("CancelInflightStart reported the start still running: the delete would tear down under a live provision")
	}
	if err := p.Stop(context.Background(), raceWorkspaceID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	_ = p.RemoveVolume(context.Background(), raceWorkspaceID)

	err := waitErr(t, errCh)
	if !errors.Is(err, ErrWorkspaceRemoved) {
		t.Fatalf("Start returned %v, want ErrWorkspaceRemoved (the caller must not mark a deleted workspace failed)", err)
	}
	if len(f.containerCreateCalls) != 0 {
		t.Errorf("ContainerCreate ran for a deleted workspace: %v", f.containerCreateCalls)
	}
	assertNothingLeft(t, f)
}

// The delete could not cancel the start (it began after the delete marked the
// row, or the delete's wait ran out): the pre-create re-check alone must stop it.
func TestStart_RemovedBeforeCreate_CreatesNothing(t *testing.T) {
	f := newRaceDocker()
	f.inspectGate = make(chan struct{})
	p := &Provisioner{cli: f, alpineImage: "alpine"}
	var removed atomic.Bool
	p.SetWorkspaceRemovedCheck(func(context.Context, string) bool { return removed.Load() })

	errCh := startAsync(p)
	<-f.inspecting
	removed.Store(true) // deleted — but nobody cancels this start
	close(f.inspectGate)

	if err := waitErr(t, errCh); !errors.Is(err, ErrWorkspaceRemoved) {
		t.Fatalf("Start returned %v, want ErrWorkspaceRemoved", err)
	}
	if len(f.containerCreateCalls) != 0 {
		t.Fatalf("ContainerCreate ran for a removed workspace: %v", f.containerCreateCalls)
	}
	assertNothingLeft(t, f)
}

// The delete lands between ContainerCreate and the end of ContainerStart: the
// post-start re-check must tear the new container down, volumes included.
func TestStart_RemovedDuringContainerStart_TearsItDown(t *testing.T) {
	f := newRaceDocker()
	p := &Provisioner{cli: f, alpineImage: "alpine"}
	var removed atomic.Bool
	p.SetWorkspaceRemovedCheck(func(context.Context, string) bool { return removed.Load() })
	f.onStart = func() { removed.Store(true) }

	if _, err := p.Start(context.Background(), raceConfig()); !errors.Is(err, ErrWorkspaceRemoved) {
		t.Fatalf("Start returned %v, want ErrWorkspaceRemoved", err)
	}
	name := ContainerName(raceWorkspaceID)
	if len(f.containerCreateCalls) != 1 {
		t.Fatalf("expected the container to have been created once, got %v", f.containerCreateCalls)
	}
	if o, ok := f.removeOpts[name]; !ok || !o.Force || !o.RemoveVolumes {
		t.Errorf("the started container was not force-removed WITH its volumes: %+v (removed=%v)", o, ok)
	}
	assertNothingLeft(t, f)
}

// The normal path is untouched: not removed → created, started, URL returned;
// the labelled config volume is re-asserted right before the create; the restart
// policy defaults to unless-stopped.
func TestStart_NotRemoved_StartsNormally(t *testing.T) {
	t.Setenv("MOLECULE_WORKSPACE_RESTART_POLICY", "")
	f := newRaceDocker()
	p := &Provisioner{cli: f, alpineImage: "alpine"}
	p.SetWorkspaceRemovedCheck(func(context.Context, string) bool { return false })

	url, err := p.Start(context.Background(), raceConfig())
	if err != nil || url == "" {
		t.Fatalf("Start = (%q, %v), want a URL and no error", url, err)
	}
	if live := f.liveContainers(); len(live) != 1 || live[0] != ContainerName(raceWorkspaceID) {
		t.Fatalf("expected exactly the workspace container, got %v", live)
	}
	if got := f.createdHost[0].RestartPolicy.Name; got != container.RestartPolicyUnlessStopped {
		t.Errorf("default restart policy = %q, want unless-stopped", got)
	}
	// The LAST thing before the create is the labelled config volume (re)assert.
	cfgVol := ConfigVolumeName(raceWorkspaceID)
	var lastBeforeCreate string
	for _, e := range f.events {
		if strings.HasPrefix(e, "container-create:") {
			break
		}
		lastBeforeCreate = e
	}
	if lastBeforeCreate != "volume-create:"+cfgVol {
		t.Errorf("event before ContainerCreate = %q, want volume-create:%s (events: %v)", lastBeforeCreate, cfgVol, f.events)
	}
	if v := f.volumes[cfgVol]; v.Labels[LabelManaged] != "true" || v.Labels[LabelInstance] == "" {
		t.Errorf("config volume labels = %v, want the managed + instance labels", v.Labels)
	}
	// Start deregistered itself: nothing left to cancel.
	if !p.CancelInflightStart(raceWorkspaceID, 0) {
		t.Error("a finished Start is still registered as in flight")
	}
}

// A delete never waits unboundedly on a start that ignores cancellation; the
// start then still unwinds as removed (its next ctx-aware call fails).
func TestCancelInflightStart_WaitIsBounded(t *testing.T) {
	f := newRaceDocker()
	f.inspectGate = make(chan struct{})
	f.honourCtx = false // a step that does not watch ctx
	p := &Provisioner{cli: f, alpineImage: "alpine"}

	errCh := startAsync(p)
	<-f.inspecting
	t0 := time.Now()
	if p.CancelInflightStart(raceWorkspaceID, 150*time.Millisecond) {
		t.Fatal("reported the start finished while it was still blocked")
	}
	if waited := time.Since(t0); waited > 2*time.Second {
		t.Fatalf("CancelInflightStart waited %s, want ~150ms", waited)
	}
	close(f.inspectGate)
	if err := waitErr(t, errCh); !errors.Is(err, ErrWorkspaceRemoved) {
		t.Fatalf("Start returned %v, want ErrWorkspaceRemoved after being cancelled for delete", err)
	}
	assertNothingLeft(t, f)
}

func TestCancelInflightStart_NothingInFlight(t *testing.T) {
	p := &Provisioner{cli: newRaceDocker(), alpineImage: "alpine"}
	if !p.CancelInflightStart("no-such-workspace", time.Second) {
		t.Fatal("nothing in flight must report done")
	}
	var nilProv *Provisioner
	if !nilProv.CancelInflightStart("x", time.Second) {
		t.Fatal("nil provisioner must report done")
	}
}

func TestWorkspaceRestartPolicy(t *testing.T) {
	for env, want := range map[string]container.RestartPolicyMode{
		"":               container.RestartPolicyUnlessStopped,
		"no":             container.RestartPolicyDisabled,
		" no ":           container.RestartPolicyDisabled,
		"always":         container.RestartPolicyAlways,
		"on-failure":     container.RestartPolicyOnFailure,
		"unless-stopped": container.RestartPolicyUnlessStopped,
		"never":          container.RestartPolicyUnlessStopped, // invalid → default
	} {
		t.Run(fmt.Sprintf("%q", env), func(t *testing.T) {
			t.Setenv("MOLECULE_WORKSPACE_RESTART_POLICY", env)
			if got := workspaceRestartPolicy().Name; got != want {
				t.Errorf("MOLECULE_WORKSPACE_RESTART_POLICY=%q → %q, want %q", env, got, want)
			}
		})
	}
}

// CI runs the platform with MOLECULE_WORKSPACE_RESTART_POLICY=no; the policy
// must reach the container's HostConfig.
func TestStart_RestartPolicyFromEnvReachesContainer(t *testing.T) {
	t.Setenv("MOLECULE_WORKSPACE_RESTART_POLICY", "no")
	f := newRaceDocker()
	p := &Provisioner{cli: f, alpineImage: "alpine"}
	if _, err := p.Start(context.Background(), raceConfig()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := f.createdHost[0].RestartPolicy.Name; got != container.RestartPolicyDisabled {
		t.Errorf("container restart policy = %q, want no", got)
	}
}

// Two starts racing for one workspace (e.g. a bundle import and a restart) are
// BOTH cancelled by one delete.
func TestCancelInflightStart_CancelsEveryStartForTheWorkspace(t *testing.T) {
	p := &Provisioner{cli: newRaceDocker(), alpineImage: "alpine"}
	var wg sync.WaitGroup
	ctxs := make([]context.Context, 2)
	dones := make([]func(), 2)
	for i := range ctxs {
		ctxs[i], dones[i] = p.trackStart(context.Background(), raceWorkspaceID)
	}
	for i := range ctxs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-ctxs[i].Done()
			dones[i]()
		}(i)
	}
	if !p.CancelInflightStart(raceWorkspaceID, 5*time.Second) {
		t.Fatal("not every start was cancelled")
	}
	wg.Wait()
	for i, c := range ctxs {
		if !errors.Is(context.Cause(c), ErrWorkspaceRemoved) {
			t.Errorf("start %d cancelled with cause %v, want ErrWorkspaceRemoved", i, context.Cause(c))
		}
	}
}
