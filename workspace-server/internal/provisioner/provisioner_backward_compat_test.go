package provisioner

import (
	"context"
	"fmt"
	"io"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	dockerimage "github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// fakeDockerClient is a test double that implements dockerClient.
// It records every call and holds lightweight in-memory state for volumes
// and containers so tests can assert behaviour without a real daemon.
type fakeDockerClient struct {
	volumes    map[string]volume.Volume
	containers map[string]container.InspectResponse
	removeErr  map[string]error
	inspectErr map[string]error
	calls      []string
}

func newFakeDockerClient() *fakeDockerClient {
	return &fakeDockerClient{
		volumes:    make(map[string]volume.Volume),
		containers: make(map[string]container.InspectResponse),
		removeErr:  make(map[string]error),
		inspectErr: make(map[string]error),
	}
}

func (f *fakeDockerClient) record(format string, args ...interface{}) {
	f.calls = append(f.calls, fmt.Sprintf(format, args...))
}

func (f *fakeDockerClient) Close() error { return nil }

func (f *fakeDockerClient) ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkingConfig *network.NetworkingConfig, platform *ocispec.Platform, containerName string) (container.CreateResponse, error) {
	f.record("ContainerCreate:%s", containerName)
	return container.CreateResponse{}, nil
}

func (f *fakeDockerClient) ContainerExecAttach(ctx context.Context, execID string, config container.ExecAttachOptions) (types.HijackedResponse, error) {
	panic("ContainerExecAttach not expected")
}

func (f *fakeDockerClient) ContainerExecCreate(ctx context.Context, container string, config container.ExecOptions) (container.ExecCreateResponse, error) {
	panic("ContainerExecCreate not expected")
}

func (f *fakeDockerClient) ContainerInspect(ctx context.Context, name string) (container.InspectResponse, error) {
	f.record("ContainerInspect:%s", name)
	if err, ok := f.inspectErr[name]; ok {
		return container.InspectResponse{}, err
	}
	c, ok := f.containers[name]
	if !ok {
		return container.InspectResponse{}, fmt.Errorf("No such container: %s", name)
	}
	return c, nil
}

func (f *fakeDockerClient) ContainerList(ctx context.Context, options container.ListOptions) ([]container.Summary, error) {
	panic("ContainerList not expected")
}

func (f *fakeDockerClient) ContainerLogs(ctx context.Context, container string, options container.LogsOptions) (io.ReadCloser, error) {
	panic("ContainerLogs not expected")
}

func (f *fakeDockerClient) ContainerRemove(ctx context.Context, name string, options container.RemoveOptions) error {
	f.record("ContainerRemove:%s", name)
	if err, ok := f.removeErr[name]; ok {
		return err
	}
	if _, ok := f.containers[name]; !ok {
		return fmt.Errorf("No such container: %s", name)
	}
	delete(f.containers, name)
	return nil
}

func (f *fakeDockerClient) ContainerStart(ctx context.Context, name string, options container.StartOptions) error {
	f.record("ContainerStart:%s", name)
	return nil
}

func (f *fakeDockerClient) ContainerWait(ctx context.Context, name string, condition container.WaitCondition) (
	<-chan container.WaitResponse, <-chan error) {
	f.record("ContainerWait:%s", name)
	done := make(chan container.WaitResponse, 1)
	errCh := make(chan error, 1)
	done <- container.WaitResponse{StatusCode: 0}
	close(done)
	close(errCh)
	return done, errCh
}

func (f *fakeDockerClient) CopyToContainer(ctx context.Context, container, path string, content io.Reader, options container.CopyToContainerOptions) error {
	panic("CopyToContainer not expected")
}

func (f *fakeDockerClient) ImageInspect(ctx context.Context, image string, opts ...client.ImageInspectOption) (dockerimage.InspectResponse, error) {
	panic("ImageInspect not expected")
}

func (f *fakeDockerClient) ImagePull(ctx context.Context, ref string, opts dockerimage.PullOptions) (io.ReadCloser, error) {
	panic("ImagePull not expected")
}

func (f *fakeDockerClient) VolumeCreate(ctx context.Context, options volume.CreateOptions) (volume.Volume, error) {
	f.record("VolumeCreate:%s", options.Name)
	f.volumes[options.Name] = volume.Volume{Name: options.Name}
	return f.volumes[options.Name], nil
}

func (f *fakeDockerClient) VolumeInspect(ctx context.Context, name string) (volume.Volume, error) {
	f.record("VolumeInspect:%s", name)
	v, ok := f.volumes[name]
	if !ok {
		return volume.Volume{}, fmt.Errorf("No such volume: %s", name)
	}
	return v, nil
}

func (f *fakeDockerClient) VolumeRemove(ctx context.Context, name string, force bool) error {
	f.record("VolumeRemove:%s", name)
	if _, ok := f.volumes[name]; !ok {
		return fmt.Errorf("No such volume: %s", name)
	}
	delete(f.volumes, name)
	return nil
}

// Helper: return true if a call matching prefix was recorded.
func (f *fakeDockerClient) called(prefix string) bool {
	for _, c := range f.calls {
		if c == prefix {
			return true
		}
	}
	return false
}

// (a) resolveConfigVolumeName prefers existing legacy volume.
func TestResolveConfigVolumeName_LegacyExists(t *testing.T) {
	ctx := context.Background()
	f := newFakeDockerClient()
	f.volumes[legacyConfigVolumeName("ws-abc123")] = volume.Volume{Name: legacyConfigVolumeName("ws-abc123")}

	p := &Provisioner{cli: f}
	got := p.resolveConfigVolumeName(ctx, "ws-abc123")
	want := legacyConfigVolumeName("ws-abc123")
	if got != want {
		t.Errorf("resolveConfigVolumeName legacy exists: got %q, want %q", got, want)
	}
	if !f.called("VolumeInspect:" + legacyConfigVolumeName("ws-abc123")) {
		t.Errorf("expected VolumeInspect on legacy volume")
	}
}

// (a) resolveConfigVolumeName returns full-UUID when legacy is absent.
func TestResolveConfigVolumeName_LegacyAbsent(t *testing.T) {
	ctx := context.Background()
	f := newFakeDockerClient()

	p := &Provisioner{cli: f}
	got := p.resolveConfigVolumeName(ctx, "ws-abc123")
	want := ConfigVolumeName("ws-abc123")
	if got != want {
		t.Errorf("resolveConfigVolumeName legacy absent: got %q, want %q", got, want)
	}
	if !f.called("VolumeInspect:" + legacyConfigVolumeName("ws-abc123")) {
		t.Errorf("expected VolumeInspect on legacy volume")
	}
}

// (a) resolveClaudeSessionVolumeName prefers existing legacy volume.
func TestResolveClaudeSessionVolumeName_LegacyExists(t *testing.T) {
	ctx := context.Background()
	f := newFakeDockerClient()
	f.volumes[legacyClaudeSessionVolumeName("ws-abc123")] = volume.Volume{Name: legacyClaudeSessionVolumeName("ws-abc123")}

	p := &Provisioner{cli: f}
	got := p.resolveClaudeSessionVolumeName(ctx, "ws-abc123")
	want := legacyClaudeSessionVolumeName("ws-abc123")
	if got != want {
		t.Errorf("resolveClaudeSessionVolumeName legacy exists: got %q, want %q", got, want)
	}
}

// (a) resolveClaudeSessionVolumeName returns full-UUID when legacy is absent.
func TestResolveClaudeSessionVolumeName_LegacyAbsent(t *testing.T) {
	ctx := context.Background()
	f := newFakeDockerClient()

	p := &Provisioner{cli: f}
	got := p.resolveClaudeSessionVolumeName(ctx, "ws-abc123")
	want := ClaudeSessionVolumeName("ws-abc123")
	if got != want {
		t.Errorf("resolveClaudeSessionVolumeName legacy absent: got %q, want %q", got, want)
	}
}

// (c) Stop falls back to legacy container when full-ID is absent.
func TestStop_FallbackToLegacy(t *testing.T) {
	ctx := context.Background()
	f := newFakeDockerClient()
	f.containers[legacyContainerName("ws-abc123")] = container.InspectResponse{}

	p := &Provisioner{cli: f}
	if err := p.Stop(ctx, "ws-abc123"); err != nil {
		t.Fatalf("Stop fallback: unexpected error: %v", err)
	}
	if !f.called("ContainerRemove:" + ContainerName("ws-abc123")) {
		t.Errorf("expected ContainerRemove on full-ID name first")
	}
	if !f.called("ContainerRemove:" + legacyContainerName("ws-abc123")) {
		t.Errorf("expected ContainerRemove on legacy name as fallback")
	}
}

// (c) Stop returns nil when new name removal is already in progress.
func TestStop_RemovalInProgress(t *testing.T) {
	ctx := context.Background()
	f := newFakeDockerClient()
	f.containers[ContainerName("abcdefghijklmnopqrstuvwxyz")] = container.InspectResponse{}
	f.removeErr[ContainerName("abcdefghijklmnopqrstuvwxyz")] = fmt.Errorf("removal of container %s is already in progress", ContainerName("abcdefghijklmnopqrstuvwxyz"))

	p := &Provisioner{cli: f}
	if err := p.Stop(ctx, "abcdefghijklmnopqrstuvwxyz"); err != nil {
		t.Fatalf("Stop removal-in-progress: unexpected error: %v", err)
	}
	if f.called("ContainerRemove:" + legacyContainerName("abcdefghijklmnopqrstuvwxyz")) {
		t.Errorf("did not expect fallback ContainerRemove when removal is in progress")
	}
}

// (c) Stop surfaces real daemon errors (not not-found, not in-progress).
func TestStop_RealError(t *testing.T) {
	ctx := context.Background()
	f := newFakeDockerClient()
	f.containers[ContainerName("ws-abc123")] = container.InspectResponse{}
	f.removeErr[ContainerName("ws-abc123")] = fmt.Errorf("daemon timeout")

	p := &Provisioner{cli: f}
	err := p.Stop(ctx, "ws-abc123")
	if err == nil {
		t.Fatalf("Stop real error: expected error, got nil")
	}
}

// (c) RunningContainerName falls back to legacy when full-ID is absent.
func TestRunningContainerName_FallbackToLegacy(t *testing.T) {
	ctx := context.Background()
	f := newFakeDockerClient()
	f.containers[legacyContainerName("ws-abc123")] = container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{State: &container.State{Running: true}},
	}

	got, err := RunningContainerName(ctx, f, "ws-abc123")
	if err != nil {
		t.Fatalf("RunningContainerName fallback: unexpected error: %v", err)
	}
	want := legacyContainerName("ws-abc123")
	if got != want {
		t.Errorf("RunningContainerName fallback: got %q, want %q", got, want)
	}
	if !f.called("ContainerInspect:" + ContainerName("ws-abc123")) {
		t.Errorf("expected ContainerInspect on full-ID name")
	}
	if !f.called("ContainerInspect:" + legacyContainerName("ws-abc123")) {
		t.Errorf("expected ContainerInspect on legacy name")
	}
}

// (c) RunningContainerName returns full-ID when it is running.
func TestRunningContainerName_FullIDRunning(t *testing.T) {
	ctx := context.Background()
	f := newFakeDockerClient()
	f.containers[ContainerName("abcdefghijklmnopqrstuvwxyz")] = container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{State: &container.State{Running: true}},
	}

	got, err := RunningContainerName(ctx, f, "abcdefghijklmnopqrstuvwxyz")
	if err != nil {
		t.Fatalf("RunningContainerName full-id: unexpected error: %v", err)
	}
	want := ContainerName("abcdefghijklmnopqrstuvwxyz")
	if got != want {
		t.Errorf("RunningContainerName full-id: got %q, want %q", got, want)
	}
	if f.called("ContainerInspect:" + legacyContainerName("abcdefghijklmnopqrstuvwxyz")) {
		t.Errorf("did not expect legacy inspect when full-id is running")
	}
}

// (c) RunningContainerName surfaces transient errors (not not-found).
func TestRunningContainerName_TransientError(t *testing.T) {
	ctx := context.Background()
	f := newFakeDockerClient()
	f.inspectErr[ContainerName("ws-abc123")] = fmt.Errorf("daemon socket EOF")

	_, err := RunningContainerName(ctx, f, "ws-abc123")
	if err == nil {
		t.Fatalf("RunningContainerName transient error: expected error, got nil")
	}
}

// (d) RemoveVolume removes only the target workspace's volumes.
func TestRemoveVolume_WorkspaceScoped(t *testing.T) {
	ctx := context.Background()
	f := newFakeDockerClient()
	wsA := "ws-abc123"
	wsB := "ws-def456"
	f.volumes[ConfigVolumeName(wsA)] = volume.Volume{Name: ConfigVolumeName(wsA)}
	f.volumes[legacyConfigVolumeName(wsA)] = volume.Volume{Name: legacyConfigVolumeName(wsA)}
	f.volumes[ConfigVolumeName(wsB)] = volume.Volume{Name: ConfigVolumeName(wsB)}
	f.volumes[legacyConfigVolumeName(wsB)] = volume.Volume{Name: legacyConfigVolumeName(wsB)}
	f.volumes[ClaudeSessionVolumeName(wsA)] = volume.Volume{Name: ClaudeSessionVolumeName(wsA)}
	f.volumes[legacyClaudeSessionVolumeName(wsA)] = volume.Volume{Name: legacyClaudeSessionVolumeName(wsA)}
	f.volumes[ClaudeSessionVolumeName(wsB)] = volume.Volume{Name: ClaudeSessionVolumeName(wsB)}
	f.volumes[legacyClaudeSessionVolumeName(wsB)] = volume.Volume{Name: legacyClaudeSessionVolumeName(wsB)}

	p := &Provisioner{cli: f}
	if err := p.RemoveVolume(ctx, wsA); err != nil {
		t.Fatalf("RemoveVolume scoped: unexpected error: %v", err)
	}

	// wsA volumes must be gone.
	for _, v := range []string{ConfigVolumeName(wsA), legacyConfigVolumeName(wsA), ClaudeSessionVolumeName(wsA), legacyClaudeSessionVolumeName(wsA)} {
		if _, ok := f.volumes[v]; ok {
			t.Errorf("RemoveVolume scoped: expected %s to be removed", v)
		}
	}
	// wsB volumes must remain.
	for _, v := range []string{ConfigVolumeName(wsB), legacyConfigVolumeName(wsB), ClaudeSessionVolumeName(wsB), legacyClaudeSessionVolumeName(wsB)} {
		if _, ok := f.volumes[v]; !ok {
			t.Errorf("RemoveVolume scoped: expected %s to remain", v)
		}
	}
}

// (d) RemoveVolume returns error when neither new nor legacy config volume exists.
func TestRemoveVolume_BothMissing(t *testing.T) {
	ctx := context.Background()
	f := newFakeDockerClient()

	p := &Provisioner{cli: f}
	err := p.RemoveVolume(ctx, "abcdefghijklmnopqrstuvwxyz")
	if err == nil {
		t.Fatalf("RemoveVolume both missing: expected error, got nil")
	}
}
