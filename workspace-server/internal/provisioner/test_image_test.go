package provisioner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	dimage "github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
)

// testImageClient is what ensureTestImage needs of a Docker client: the real
// *client.Client in the daemon tests, a fake in its own test below.
type testImageClient interface {
	dockerImageClient
	ImageInspect(ctx context.Context, img string, opts ...client.ImageInspectOption) (dimage.InspectResponse, error)
}

// ensureTestImage makes ref present on the daemon a test drives, pulling it
// only when it is absent (a present image is never re-pulled, so a test does
// not depend on the registry when the daemon already has it).
//
// Why (operator-config#1116): the daemon tests here create containers from
// `alpine` with ContainerCreate, which never pulls. They passed only where
// something else had left the image behind. On a CI VM that is not true after
// its weekly reset (a fresh disk with the job image alone), nor after the VM's
// disk-pressure GC: TestMigrateVolumeIfNeeded_ExistingTruncatedVolume then
// failed with "No such image: alpine:latest".
func ensureTestImage(ctx context.Context, cli testImageClient, ref string) error {
	if _, err := cli.ImageInspect(ctx, ref); err == nil {
		return nil
	}
	if err := pullImageAndDrain(ctx, cli, ref, ""); err != nil {
		return fmt.Errorf("image %s is not on the daemon and could not be pulled: %w", ref, err)
	}
	if _, err := cli.ImageInspect(ctx, ref); err != nil {
		return fmt.Errorf("image %s was pulled but the daemon still does not have it: %w", ref, err)
	}
	return nil
}

// requireTestImage is ensureTestImage for a test: a missing image that cannot
// be had fails the test here, saying so, instead of as a ContainerCreate error.
func requireTestImage(t *testing.T, ctx context.Context, cli testImageClient, ref string) {
	t.Helper()
	if err := ensureTestImage(ctx, cli, ref); err != nil {
		t.Fatalf("test image: %v", err)
	}
}

// testImageFake: an image that is absent until pulled (or present from the
// start), and an optional pull failure.
type testImageFake struct {
	*ensurePullFake
	pullErr error
}

func (f *testImageFake) ImageInspect(ctx context.Context, img string, opts ...client.ImageInspectOption) (dimage.InspectResponse, error) {
	if f.present || (len(f.pulls) > 0 && f.pullErr == nil) {
		return dimage.InspectResponse{ID: "sha256:0123456789abcdef"}, nil
	}
	return dimage.InspectResponse{}, errors.New("No such image: " + img)
}

func TestEnsureTestImage(t *testing.T) {
	ctx := context.Background()
	newFake := func(present bool, pullErr error) *testImageFake {
		return &testImageFake{ensurePullFake: &ensurePullFake{fakeDockerClient: newFakeDockerClient(), present: present}, pullErr: pullErr}
	}

	t.Run("absent: pulled once, then present", func(t *testing.T) {
		f := newFake(false, nil)
		if err := ensureTestImage(ctx, f, "alpine"); err != nil {
			t.Fatalf("ensureTestImage: %v", err)
		}
		if len(f.pulls) != 1 || f.pulls[0] != "alpine" {
			t.Fatalf("pulls = %v, want exactly [alpine]", f.pulls)
		}
	})

	t.Run("present: never pulled (no registry dependency)", func(t *testing.T) {
		f := newFake(true, nil)
		if err := ensureTestImage(ctx, f, "alpine"); err != nil {
			t.Fatalf("ensureTestImage: %v", err)
		}
		if len(f.pulls) != 0 {
			t.Fatalf("pulls = %v, want none for a present image", f.pulls)
		}
	})

	t.Run("absent and the pull fails: an error naming the image", func(t *testing.T) {
		f := newFake(false, errors.New("registry unreachable"))
		err := ensureTestImage(ctx, &failingPullTestImage{f}, "alpine")
		if err == nil {
			t.Fatal("ensureTestImage returned nil for an image it could not get")
		}
		if want := "image alpine is not on the daemon and could not be pulled"; !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not say %q", err, want)
		}
	})
}

// failingPullTestImage: the pull itself errors.
type failingPullTestImage struct{ *testImageFake }

func (f *failingPullTestImage) ImagePull(ctx context.Context, ref string, opts dimage.PullOptions) (io.ReadCloser, error) {
	f.pulls = append(f.pulls, ref)
	return nil, f.pullErr
}
