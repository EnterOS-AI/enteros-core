package provisioner

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	dimage "github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
)

// ensurePullFake embeds the package fakeDockerClient (for the full dockerClient
// interface) and overrides ImageInspect / ImagePull to control local presence
// and record pull calls, so we can assert ensureImagePresent's acquire policy.
type ensurePullFake struct {
	*fakeDockerClient
	present bool     // ImageInspect returns "present" (nil err) when true
	pulls   []string // every ImagePull ref, in order
}

func (f *ensurePullFake) ImageInspect(ctx context.Context, img string, opts ...client.ImageInspectOption) (dimage.InspectResponse, error) {
	if f.present {
		// ID/Created must be long enough for ensureImagePresent's [:19] logging.
		return dimage.InspectResponse{ID: "sha256:0123456789abcdef0123456789abcdef", Created: "2026-07-11T00:00:00Z"}, nil
	}
	return dimage.InspectResponse{}, errors.New("no such image")
}

func (f *ensurePullFake) ImagePull(ctx context.Context, ref string, opts dimage.PullOptions) (io.ReadCloser, error) {
	f.pulls = append(f.pulls, ref)
	return io.NopCloser(strings.NewReader("")), nil
}

// TestEnsureImagePresent pins the acquire policy: pull-on-miss for any ref,
// re-pull a MOVING tag that is already present (refresh a stale snapshot), and
// — the whole point of "ensure-present, not pull-latest" — NEVER re-pull a
// pinned (@sha256) image that is already present, so deliberate pinning holds.
func TestEnsureImagePresent(t *testing.T) {
	pinned := "alpine:3.20@sha256:c64c687cbea9300178b30c95835354e34c4e4febc4badfe27102879de0483b5e"
	moving := "molecule-local/workspace-template-hermes:latest"

	cases := []struct {
		name      string
		image     string
		present   bool
		wantPulls int
	}{
		{"absent pinned → pull-on-miss (self-heal)", pinned, false, 1},
		{"present pinned → NO re-pull (immutable digest)", pinned, true, 0},
		{"present moving tag → re-pull to refresh", moving, true, 1},
		{"absent moving → pull", moving, false, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &ensurePullFake{fakeDockerClient: newFakeDockerClient(), present: tc.present}
			p := &Provisioner{cli: f, alpineImage: pinned}
			p.ensureImagePresent(context.Background(), tc.image, "")
			if len(f.pulls) != tc.wantPulls {
				t.Fatalf("pulls=%d want %d (image=%s present=%v)", len(f.pulls), tc.wantPulls, tc.image, tc.present)
			}
			if tc.wantPulls > 0 && f.pulls[0] != tc.image {
				t.Fatalf("pulled %q, want %q", f.pulls[0], tc.image)
			}
		})
	}
}

// TestEnsureImagePresent_PullFailureIsBestEffort — a pull error must not panic
// or block; ensureImagePresent logs and returns so the caller's ContainerCreate
// surfaces the actionable error (the throwaway-helper fallback contract).
func TestEnsureImagePresent_PullFailureIsBestEffort(t *testing.T) {
	f := &failingPullFake{fakeDockerClient: newFakeDockerClient()}
	p := &Provisioner{cli: f, alpineImage: "alpine:3.20@sha256:deadbeef"}
	// absent image + failing pull: must return (not panic), reporting the inspect error.
	if _, _, err := p.ensureImagePresent(context.Background(), p.alpineImage, ""); err == nil {
		t.Fatal("expected the pre-pull inspect error to be returned for an absent image")
	}
}

// TestEnsureAlpineImage_OncePerProcess — the throwaway-helper image must be
// inspected+pulled AT MOST once no matter how many helper calls happen, so hot
// paths (and registry-unreachable hosts) don't re-pay the pull each time.
func TestEnsureAlpineImage_OncePerProcess(t *testing.T) {
	f := &ensurePullFake{fakeDockerClient: newFakeDockerClient(), present: false} // absent → would pull every call if not gated
	p := &Provisioner{cli: f, alpineImage: "alpine:3.20@sha256:deadbeef0123456789abcdef"}
	for i := 0; i < 5; i++ {
		p.ensureAlpineImage(context.Background())
	}
	if len(f.pulls) != 1 {
		t.Fatalf("ensureAlpineImage pulled %d times across 5 calls, want exactly 1 (once/process)", len(f.pulls))
	}
}

type failingPullFake struct {
	*fakeDockerClient
}

func (f *failingPullFake) ImageInspect(ctx context.Context, img string, opts ...client.ImageInspectOption) (dimage.InspectResponse, error) {
	return dimage.InspectResponse{}, errors.New("no such image")
}

func (f *failingPullFake) ImagePull(ctx context.Context, ref string, opts dimage.PullOptions) (io.ReadCloser, error) {
	return nil, errors.New("network unreachable")
}
