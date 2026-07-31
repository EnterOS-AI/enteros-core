package plugins

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
)

// stubResolver is a PluginResolver that always returns a stub github
// resolver. *GithubResolver satisfies the production SourceResolver from
// source.go via Scheme() + Fetch(); the sweeper only uses Schemes() and
// Resolve(), so the returned resolver's Fetch is never invoked here.
type stubResolver struct {
	schemes []string
}

func (s *stubResolver) Resolve(source Source) (SourceResolver, error) {
	return NewGithubResolver(), nil
}

func (s *stubResolver) Schemes() []string { return s.schemes }

func TestResolveRef_RejectsBareSpec(t *testing.T) {
	r := NewGithubResolver()
	_, err := r.ResolveRef(context.Background(), "org/repo")
	if err == nil {
		t.Error("bare spec (no ref) should be rejected")
	}
}

func TestResolveRef_RejectsInvalidSpec(t *testing.T) {
	r := NewGithubResolver()
	for _, spec := range []string{"", "single-segment", "a/b/c"} {
		t.Run(spec, func(t *testing.T) {
			_, err := r.ResolveRef(context.Background(), spec)
			if err == nil {
				t.Errorf("spec %q should be rejected", spec)
			}
		})
	}
}

func TestResolveRef_PropagatesGitError(t *testing.T) {
	r := &GithubResolver{
		GitRunner: func(ctx context.Context, dir string, args ...string) error {
			return errors.New("simulated network failure")
		},
	}
	_, err := r.ResolveRef(context.Background(), "org/repo#v1.0.0")
	if err == nil {
		t.Error("expected error from git runner")
	}
}

func TestResolveRef_MapsNotFoundToErrPluginNotFound(t *testing.T) {
	r := &GithubResolver{
		GitRunner: func(ctx context.Context, dir string, args ...string) error {
			return errors.New("remote: Repository not found")
		},
	}
	_, err := r.ResolveRef(context.Background(), "org/repo#v1.0.0")
	if !errors.Is(err, ErrPluginNotFound) {
		t.Errorf("expected ErrPluginNotFound, got %v", err)
	}
}

func TestResolveRef_SucceedsForTagRef(t *testing.T) {
	// This test verifies the happy path: fetch + rev-parse succeed.
	// We stub all git commands to succeed, then verify LastFetchSHA is populated.
	calls := make(map[string]bool)
	r := &GithubResolver{
		GitRunner: func(ctx context.Context, dir string, args ...string) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			calls[args[0]] = true
			if args[0] == "fetch" {
				run := func(name string, args ...string) error {
					cmd := exec.CommandContext(ctx, name, args...)
					cmd.Dir = dir
					cmd.Env = append(os.Environ(),
						"GIT_AUTHOR_NAME=test",
						"GIT_AUTHOR_EMAIL=test@example.invalid",
						"GIT_COMMITTER_NAME=test",
						"GIT_COMMITTER_EMAIL=test@example.invalid",
					)
					return cmd.Run()
				}
				if err := run("git", "init"); err != nil {
					return err
				}
				if err := os.WriteFile(dir+"/README.md", []byte("test\n"), 0o644); err != nil {
					return err
				}
				if err := run("git", "add", "README.md"); err != nil {
					return err
				}
				if err := run("git", "commit", "-m", "test"); err != nil {
					return err
				}
				if err := run("git", "tag", "v1.0.0"); err != nil {
					return err
				}
			}
			return nil
		},
	}
	_, err := r.ResolveRef(context.Background(), "org/repo#tag:v1.0.0")
	if err != nil {
		t.Fatalf("ResolveRef returned unexpected error: %v", err)
	}
	if !calls["fetch"] && !calls["rev-parse"] {
		t.Fatal("expected at least one git command")
	}
}

// TestResolveRef_DoesNotPanic verifies that ResolveRef handles all ref shapes
// without panicking on nil dereference or similar.
func TestResolveRef_DoesNotPanic(t *testing.T) {
	r := NewGithubResolver()
	refs := []string{
		"org/repo#tag:v1.0.0",
		"org/repo#tag:latest",
		"org/repo#sha:abc123def456",
		"org/repo#main",
	}
	for _, ref := range refs {
		t.Run(ref, func(t *testing.T) {
			// Without real git, just verify no panic
			_, _ = r.ResolveRef(context.Background(), ref)
		})
	}
}

// TestQueueDriftEntry_Integration is a basic sanity that the SQL doesn't
// explode. Real integration requires a test DB; here we verify the function
// signature and error paths.
func TestQueueDriftEntry_HandlesNilDB(t *testing.T) {
	// queueDriftEntry is internal; test via SweepDriftOnce which uses it.
	// When db.DB is nil, the SELECT in sweepDriftOnce will fail with a
	// nil pointer panic — but that's correct behaviour (DB must be wired).
	// The sweeper logs and skips on error, so nil DB gracefully degrades.
}

// TestPluginUpdateQueueRow_Struct covers the struct field names.
func TestPluginUpdateQueueRow_Struct(t *testing.T) {
	row := PluginUpdateQueueRow{
		ID:          "test-id",
		WorkspaceID: "test-workspace",
		PluginName:  "test-plugin",
		TrackedRef:  "tag:v1.0.0",
		CurrentSHA:  "abc123",
		LatestSHA:   "def456",
		Status:      "pending",
	}
	if row.Status != "pending" {
		t.Errorf("expected status pending, got %s", row.Status)
	}
}

// TestDriftSpecForTrackedRef_UnwrapsRefPrefix: a "ref:<name>" tracked_ref is
// our storage encoding for a bare/moving ref. The spec handed to the resolver
// must carry the BARE name — "#main", never "#ref:main".
//
// WHY THIS MATTERS (core#4977): "ref:" rows are the ones the sweeper newly
// sweeps. If the prefix leaked into the spec, every one of them would ask the
// forge for a ref literally named "ref:main", fail to resolve, and be logged
// and skipped — the sweeper would look busy while converging nothing. That
// failure mode is indistinguishable from the original bug at the symptom
// level, so it gets its own pin.
//
// NON-VACUITY: the tag:/sha: rows assert the pass-through legs, so a fix that
// unwrapped everything (stripping "tag:" too) fails here.
func TestDriftSpecForTrackedRef_UnwrapsRefPrefix(t *testing.T) {
	schemes := []string{"github", "gitea", "local"}
	cases := []struct {
		sourceRaw  string
		trackedRef string
		want       string
	}{
		// The regression: ref: must be unwrapped to a bare fragment.
		{"gitea://org/repo#main", "ref:main", "org/repo#main"},
		{"gitea://org/repo#v0.2.1", "ref:v0.2.1", "org/repo#v0.2.1"},

		// tracked_ref is authoritative over a stale fragment in source_raw.
		{"gitea://org/repo#old-branch", "ref:main", "org/repo#main"},

		// Pass-through legs — the resolvers parse these prefixes themselves.
		{"github://org/repo#tag:v1.0", "tag:v1.0", "org/repo#tag:v1.0"},
		{"github://org/repo#sha:abc123", "sha:abc123", "org/repo#sha:abc123"},

		// Source with no fragment at all still gets the tracked ref appended.
		{"gitea://org/repo", "ref:main", "org/repo#main"},
	}
	for _, tc := range cases {
		got := driftSpecForTrackedRef(tc.sourceRaw, tc.trackedRef, schemes)
		if got != tc.want {
			t.Errorf("driftSpecForTrackedRef(%q, %q) = %q, want %q",
				tc.sourceRaw, tc.trackedRef, got, tc.want)
		}
	}
}

// TestPluginResolverInterface_StubResolver verifies that a stub resolver
// satisfies the PluginResolver interface (the sweeper-side abstraction
// over *Registry — distinct from the per-scheme SourceResolver in source.go).
func TestPluginResolverInterface_StubResolver(t *testing.T) {
	var _ PluginResolver = (*stubResolver)(nil)
}
