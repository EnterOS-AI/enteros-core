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

	events        []string
	createdHost   []*container.HostConfig
	createdImages []string
	removeOpts    map[string]container.RemoveOptions
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
	f.createdImages = append(f.createdImages, cfg.Image)
	f.containers[name] = container.InspectResponse{ContainerJSONBase: &container.ContainerJSONBase{Name: name, State: &container.State{}}}
	// Like dockerd: a bind naming a volume that does not exist creates it, with
	// NO labels.
	for _, b := range host.Binds {
		if src, _, ok := strings.Cut(b, ":"); ok && !strings.HasPrefix(src, "/") {
			if _, exists := f.volumes[src]; !exists {
				f.volumes[src] = volume.Volume{Name: src}
			}
		}
	}
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
	if err := ctx.Err(); err != nil {
		return err // the real client never sends a request on a cancelled ctx
	}
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
	// The /workspace volume outlives the discard by design (a delete keeps it
	// too). It must carry the instance label, or nothing could attribute it once
	// its container is gone (the CI instance teardown lists volumes by it).
	if v := f.volumes[WorkspaceVolumeName(raceWorkspaceID)]; v.Labels[LabelInstance] != PlatformInstanceID() {
		t.Errorf("the discarded workspace's /workspace volume has labels %v, want its instance label", v.Labels)
	}
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

// The /workspace volume is created LABELLED before the container whenever the
// tier keeps that bind, so it stays attributable to its platform instance after
// the container is gone (a delete keeps it by design; left to the bind, Docker
// would create it with no labels at all). Tier 1 strips the mount, and a
// host-path workspace is no volume: neither gets one.
func TestStart_WorkspaceVolumeIsCreatedLabelled(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*WorkspaceConfig)
		want bool
	}{
		{"tier2", func(c *WorkspaceConfig) { c.Tier = 2 }, true},
		{"tier3", func(c *WorkspaceConfig) { c.Tier = 3 }, true},
		{"tier1", func(c *WorkspaceConfig) { c.Tier = 1 }, false},
		{"host-path", func(c *WorkspaceConfig) {
			c.WorkspacePath, c.WorkspaceAccess = "/srv/agent-workspace", WorkspaceAccessReadWrite
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRaceDocker()
			p := &Provisioner{cli: f, alpineImage: "alpine"}
			cfg := raceConfig()
			tc.edit(&cfg)
			if _, err := p.Start(context.Background(), cfg); err != nil {
				t.Fatalf("Start: %v", err)
			}
			v, ok := f.volumes[WorkspaceVolumeName(raceWorkspaceID)]
			if ok != tc.want {
				t.Fatalf("workspace volume exists=%v, want %v (volumes: %v)", ok, tc.want, f.volumes)
			}
			if ok && (v.Labels[LabelManaged] != "true" || v.Labels[LabelInstance] != PlatformInstanceID()) {
				t.Errorf("workspace volume labels = %v, want the managed + instance labels", v.Labels)
			}
		})
	}
}

// migrationRaceDocker adds a legacy (KI-013) volume migration slow enough for a
// delete to land mid-copy: the copy container runs until its ctx is cancelled
// (the real ContainerWait then reports the ctx error), and a volume that a live
// container mounts cannot be removed, as on a real daemon.
type migrationRaceDocker struct {
	*raceDocker
	migrating chan struct{} // closed once Start is waiting on the copy container
	binds     map[string][]string
}

func (f *migrationRaceDocker) ContainerCreate(ctx context.Context, cfg *container.Config, host *container.HostConfig, nw *network.NetworkingConfig, pl *ocispec.Platform, name string) (container.CreateResponse, error) {
	if name != "" {
		return f.raceDocker.ContainerCreate(ctx, cfg, host, nw, pl, name)
	}
	if err := ctx.Err(); err != nil {
		return container.CreateResponse{}, err
	}
	const id = "volume-migration"
	f.log("container-create:" + id)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.containers[id] = container.InspectResponse{ContainerJSONBase: &container.ContainerJSONBase{Name: id, State: &container.State{}}}
	for _, b := range host.Binds {
		src, _, _ := strings.Cut(b, ":")
		f.binds[id] = append(f.binds[id], src)
	}
	return container.CreateResponse{ID: id}, nil
}

func (f *migrationRaceDocker) ContainerWait(ctx context.Context, _ string, _ container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
	select {
	case <-f.migrating:
	default:
		close(f.migrating)
	}
	errCh := make(chan error, 1)
	go func() { <-ctx.Done(); errCh <- ctx.Err() }()
	return make(chan container.WaitResponse), errCh
}

func (f *migrationRaceDocker) ContainerRemove(ctx context.Context, name string, o container.RemoveOptions) error {
	err := f.raceDocker.ContainerRemove(ctx, name, o)
	if err == nil {
		f.mu.Lock()
		delete(f.binds, name)
		f.mu.Unlock()
	}
	return err
}

func (f *migrationRaceDocker) VolumeRemove(ctx context.Context, name string, force bool) error {
	f.mu.Lock()
	for c, vols := range f.binds {
		for _, v := range vols {
			if v == name {
				f.mu.Unlock()
				return fmt.Errorf("remove %s: volume is in use - [%s]", name, c)
			}
		}
	}
	f.mu.Unlock()
	return f.raceDocker.VolumeRemove(ctx, name, force)
}

// A delete landing while Start migrates a legacy config volume must not strand
// the copy container. Its deferred remove used the Start's ctx, which the delete
// has just cancelled, so the request never reached the daemon: the copy
// container outlived the Start and kept both volumes in use, so the discard
// could not remove them either.
func TestStart_DeleteDuringLegacyVolumeMigration_RemovesTheCopyContainer(t *testing.T) {
	f := &migrationRaceDocker{raceDocker: newRaceDocker(), migrating: make(chan struct{}), binds: map[string][]string{}}
	legacy := legacyConfigVolumeName(raceWorkspaceID)
	f.volumes[legacy] = volume.Volume{Name: legacy}
	p := &Provisioner{cli: f, alpineImage: "alpine"}
	var removed atomic.Bool
	p.SetWorkspaceRemovedCheck(func(context.Context, string) bool { return removed.Load() })

	errCh := startAsync(p)
	<-f.migrating
	removed.Store(true)
	if !p.CancelInflightStart(raceWorkspaceID, 5*time.Second) {
		t.Fatal("CancelInflightStart reported the start still running")
	}
	if err := waitErr(t, errCh); !errors.Is(err, ErrWorkspaceRemoved) {
		t.Fatalf("Start returned %v, want ErrWorkspaceRemoved", err)
	}
	if live := f.liveContainers(); len(live) != 0 {
		t.Errorf("containers left behind after the delete: %v (the volume-migration copy container must not outlive the Start)", live)
	}
	for _, v := range []string{ConfigVolumeName(raceWorkspaceID), legacy} {
		if f.hasVolume(v) {
			t.Errorf("volume %s left behind after the delete", v)
		}
	}
}

func localBuildConfig() WorkspaceConfig {
	return WorkspaceConfig{WorkspaceID: raceWorkspaceID, Runtime: "hermes", Tier: 2}
}

// fakeLocalBuild stands in for the local image build (`git clone` + `docker
// build`): it runs until released or until its ctx is cancelled — as a real
// build under exec.CommandContext is killed — and records how it ended.
type fakeLocalBuild struct {
	entered, release, finished chan struct{}
	deadline                   time.Time
	hasDeadline                bool
	killed                     atomic.Bool
}

func installFakeLocalBuild(t *testing.T) *fakeLocalBuild {
	t.Helper()
	t.Setenv("MOLECULE_IMAGE_REGISTRY", "") // local-build mode
	b := &fakeLocalBuild{entered: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{})}
	orig := ensureLocalImageHook
	t.Cleanup(func() { ensureLocalImageHook = orig })
	ensureLocalImageHook = func(ctx context.Context, runtime string) (string, error) {
		defer close(b.finished)
		b.deadline, b.hasDeadline = ctx.Deadline()
		close(b.entered)
		select {
		case <-b.release:
			return "molecule-local/workspace-template-" + runtime + ":0123abcd", nil
		case <-ctx.Done():
			b.killed.Store(true)
			return "", ctx.Err()
		}
	}
	return b
}

// A delete landing during a cold local image build ABANDONS the build rather
// than killing it: Start unwinds at once (the delete does not wait out a
// minutes-long build), nothing is created, and the build itself runs on to
// completion. A `docker build` killed mid-RUN pins its partial BuildKit
// snapshot on Docker >= 29.6 (RC06), and before deletes could cancel a Start
// they never reached the build at all.
func TestStart_DeleteDuringLocalImageBuild_AbandonsTheBuildWithoutKillingIt(t *testing.T) {
	b := installFakeLocalBuild(t)
	f := newRaceDocker()
	p := &Provisioner{cli: f, alpineImage: "alpine"}
	var removed atomic.Bool
	p.SetWorkspaceRemovedCheck(func(context.Context, string) bool { return removed.Load() })

	errCh := make(chan error, 1)
	go func() {
		_, err := p.Start(context.Background(), localBuildConfig())
		errCh <- err
	}()
	<-b.entered

	removed.Store(true)
	t0 := time.Now()
	if !p.CancelInflightStart(raceWorkspaceID, 5*time.Second) {
		t.Fatal("the delete had to wait out the image build: Start did not unwind when cancelled")
	}
	if waited := time.Since(t0); waited > 2*time.Second {
		t.Errorf("CancelInflightStart waited %s on a start that was in its image build", waited)
	}
	if err := waitErr(t, errCh); !errors.Is(err, ErrWorkspaceRemoved) {
		t.Fatalf("Start returned %v, want ErrWorkspaceRemoved", err)
	}
	if len(f.containerCreateCalls) != 0 {
		t.Errorf("ContainerCreate ran for a deleted workspace: %v", f.containerCreateCalls)
	}
	assertNothingLeft(t, f)

	select {
	case <-b.finished:
		t.Fatal("the delete killed the image build — it must be abandoned, not cancelled")
	case <-time.After(100 * time.Millisecond):
	}
	close(b.release)
	select {
	case <-b.finished:
	case <-time.After(5 * time.Second):
		t.Fatal("the abandoned build never finished")
	}
	if b.killed.Load() {
		t.Error("the delete cancelled the image build's ctx")
	}
}

// Only the delete is cut off from the build. The caller's deadline — the
// per-runtime provision window, which also sizes the build's ceiling — still
// reaches it, and the caller's own cancellation still stops it.
func TestStart_LocalImageBuild_KeepsTheCallersDeadlineAndCancel(t *testing.T) {
	b := installFakeLocalBuild(t)
	f := newRaceDocker()
	p := &Provisioner{cli: f, alpineImage: "alpine"}
	p.SetWorkspaceRemovedCheck(func(context.Context, string) bool { return false })

	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	want, _ := ctx.Deadline()
	errCh := make(chan error, 1)
	go func() {
		_, err := p.Start(ctx, localBuildConfig())
		errCh <- err
	}()
	<-b.entered
	if !b.hasDeadline || !b.deadline.Equal(want) {
		t.Errorf("build deadline = %v (set=%v), want the caller's %v", b.deadline, b.hasDeadline, want)
	}
	cancel() // the caller gives up: not a delete
	err := waitErr(t, errCh)
	if err == nil || errors.Is(err, ErrWorkspaceRemoved) {
		t.Fatalf("Start returned %v, want the build's own cancellation error", err)
	}
	if !b.killed.Load() {
		t.Error("the caller's cancellation did not stop the build")
	}
}

// Not deleted: Start waits for the build and creates the container from the
// image it built.
func TestStart_LocalImageBuild_NotRemovedUsesTheBuiltImage(t *testing.T) {
	b := installFakeLocalBuild(t)
	close(b.release)
	f := newRaceDocker()
	p := &Provisioner{cli: f, alpineImage: "alpine"}
	p.SetWorkspaceRemovedCheck(func(context.Context, string) bool { return false })
	if _, err := p.Start(context.Background(), localBuildConfig()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if want := "molecule-local/workspace-template-hermes:0123abcd"; len(f.createdImages) != 1 || f.createdImages[0] != want {
		t.Fatalf("created from %v, want [%s]", f.createdImages, want)
	}
	if b.killed.Load() {
		t.Error("a build that was not abandoned was cancelled")
	}
}
