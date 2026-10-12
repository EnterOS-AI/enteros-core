package provisioner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// These tests prove the stall runner is wired into the REAL build/clone
// production functions (dockerBuildProd / gitCloneProd) through
// LocalBuildOptions, and that the per-runtime ceiling injected via the
// options reaches the build — not the retired fixed 3-min cap.

func skipNoSh(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not on PATH")
	}
}

// The option resolvers fall back to the package defaults when a field is
// zero, and honor an explicit override otherwise — this is how the caller
// (Start) narrows the ceiling to the per-runtime provision timeout.
func TestLocalBuildOptions_GateResolvers(t *testing.T) {
	// Zero → package default.
	o := &LocalBuildOptions{}
	if o.stallGrace() != buildStallGrace() {
		t.Errorf("zero StallGrace = %s, want default %s", o.stallGrace(), buildStallGrace())
	}
	if o.ceiling() != buildCeiling() {
		t.Errorf("zero Ceiling = %s, want default %s", o.ceiling(), buildCeiling())
	}
	// Explicit → honored (e.g. a runtime declaring 30m, far above the old 3m).
	o = &LocalBuildOptions{StallGrace: 5 * time.Minute, Ceiling: 30 * time.Minute}
	if o.stallGrace() != 5*time.Minute {
		t.Errorf("explicit StallGrace not honored: %s", o.stallGrace())
	}
	if o.ceiling() != 30*time.Minute {
		t.Errorf("explicit Ceiling not honored: %s", o.ceiling())
	}
	// Regression: the resolved ceiling for a long-runtime override must beat
	// the retired 3-min cap.
	if o.ceiling() <= 3*time.Minute {
		t.Errorf("30m ceiling resolved to %s — <= retired 3-min cap", o.ceiling())
	}
}

// dockerBuildProd, with a fake `docker` on PATH that goes silent, must be
// killed by the stall runner with the stall error — proving the runner is
// actually wired into the production build function (not just the unit-level
// runStreamingCommand).
func TestDockerBuildProd_StallKilled(t *testing.T) {
	skipNoSh(t)
	// Shadow `docker` on PATH with a script that prints one line then sleeps
	// forever — a wedged build.
	bin := t.TempDir()
	fakeDocker := filepath.Join(bin, "docker")
	script := "#!/bin/sh\necho 'building...'\nsleep 30\n"
	if err := os.WriteFile(fakeDocker, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctxDir := t.TempDir()
	// dockerBuildArgs reads .runtime-version + Dockerfile path; a bare
	// context dir is fine (fake docker ignores args).
	opts := &LocalBuildOptions{
		Platform:   "",
		StallGrace: 300 * time.Millisecond,
		Ceiling:    30 * time.Second,
	}
	start := time.Now()
	err := dockerBuildProd(context.Background(), opts, ctxDir, "test:tag")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected stall kill from wedged build")
	}
	if !errors.Is(err, errBuildStalled) {
		t.Fatalf("expected errBuildStalled, got %v", err)
	}
	if elapsed > 3*time.Second {
		t.Errorf("build stall kill took %s — runner not driving the real build fn", elapsed)
	}
}

// dockerBuildProd with a fake docker that streams then exits 0 must succeed —
// the happy path is unchanged by the runner wiring.
func TestDockerBuildProd_HappyPathStreams(t *testing.T) {
	skipNoSh(t)
	bin := t.TempDir()
	fakeDocker := filepath.Join(bin, "docker")
	script := "#!/bin/sh\ni=0\nwhile [ $i -lt 5 ]; do echo \"step $i\"; i=$((i+1)); sleep 0.05; done\nexit 0\n"
	if err := os.WriteFile(fakeDocker, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	opts := &LocalBuildOptions{StallGrace: time.Second, Ceiling: 30 * time.Second}
	if err := dockerBuildProd(context.Background(), opts, t.TempDir(), "test:tag"); err != nil {
		t.Fatalf("happy-path build errored: %v", err)
	}
}

// A non-zero build exit surfaces the captured output + a non-stall error, so
// operators see the real build failure reason (token-masked).
func TestDockerBuildProd_NonZeroExitSurfacesOutput(t *testing.T) {
	skipNoSh(t)
	bin := t.TempDir()
	fakeDocker := filepath.Join(bin, "docker")
	script := "#!/bin/sh\necho 'ERROR: missing base image'\nexit 1\n"
	if err := os.WriteFile(fakeDocker, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	opts := &LocalBuildOptions{StallGrace: time.Second, Ceiling: 30 * time.Second}
	err := dockerBuildProd(context.Background(), opts, t.TempDir(), "test:tag")
	if err == nil {
		t.Fatalf("expected non-zero build error")
	}
	if errors.Is(err, errBuildStalled) || errors.Is(err, errBuildCeiling) {
		t.Fatalf("non-zero exit misclassified as stall/ceiling: %v", err)
	}
	if !strings.Contains(err.Error(), "missing base image") {
		t.Errorf("build error did not surface docker output: %v", err)
	}
}

// buildkitQuietPhaseExempt must recognize the tail of a build that is inside
// BuildKit's final export/unpack phase (legitimately silent — local I/O only,
// can run minutes on a multi-GB image with zero output) and NOTHING else.
// Pure function — runs on every platform, including Windows where the
// process-driving tests skip. 2026-07-18 fresh-onboarding regression: the
// silent unpack of the ~7GB hermes image exceeded the 4m stall grace and
// every first-boot self-host provision was killed mid-unpack.
func TestBuildkitQuietPhaseExempt(t *testing.T) {
	cases := []struct {
		name string
		tail string
		want bool
	}{
		{"unpack in progress", "#26 naming to docker.io/x done\n#26 unpacking to docker.io/molecule-local/workspace-template-hermes:4dad19390d61-amd64\n", true},
		{"export layers in progress", "#25 DONE 0.1s\n\n#26 exporting to image\n#26 exporting layers\n", true},
		{"unpack finished (done suffix)", "#26 unpacking to docker.io/x 210.4s done\n", false},
		{"phase block finished (DONE)", "#26 unpacking to docker.io/x done\n#26 DONE 212.0s\n", false},
		{"silent RUN step is NOT exempt", "#24 [18/21] RUN curl -fsSL https://example.com/install.sh | bash\n", false},
		{"pip step is NOT exempt", "#12 RUN pip install --no-cache-dir -r requirements.txt\n", false},
		{"empty tail", "", false},
		{"whitespace only", "\n\n  \n", false},
		{"unpack mentioned mid-log but later output exists", "#26 unpacking to docker.io/x\n#27 writing sbom\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildkitQuietPhaseExempt([]byte(tc.tail)); got != tc.want {
				t.Errorf("buildkitQuietPhaseExempt(%q) = %v, want %v", tc.tail, got, tc.want)
			}
		})
	}
}
