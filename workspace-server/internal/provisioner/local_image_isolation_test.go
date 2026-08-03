package provisioner

import (
	"context"
	"strings"
	"testing"
)

// core#5031. Two CI jobs tag their OWN stub image over
// `molecule-local/workspace-template-claude-code:<sha12>-<arch>` (+ `:latest`)
// on ONE shared Docker daemon, because the tag is derived purely from the
// template repo's HEAD sha and the arch — there is no per-job component in it.
// These tests pin the property that makes concurrent jobs safe: a caller can
// name a PRIVATE tag namespace, and two different names can never collide.

func TestLocalImageIsolation_UnsetIsByteIdenticalToTheSharedTag(t *testing.T) {
	t.Setenv(LocalImageIsolationEnv, "")

	if got, want := LocalImageTag("claude-code", "abcdef0123456789", "linux/amd64"),
		"molecule-local/workspace-template-claude-code:abcdef012345-amd64"; got != want {
		t.Errorf("LocalImageTag with no isolation = %q, want %q (the pre-core#5031 tag must not move)", got, want)
	}
	if got, want := LocalImageLatestTag("claude-code"),
		"molecule-local/workspace-template-claude-code:latest"; got != want {
		t.Errorf("LocalImageLatestTag with no isolation = %q, want %q", got, want)
	}
}

func TestLocalImageIsolation_AppliesToBothTags(t *testing.T) {
	t.Setenv(LocalImageIsolationEnv, "e2e-610481-1-lifestub")

	sha := LocalImageTag("claude-code", "abcdef0123456789", "linux/amd64")
	latest := LocalImageLatestTag("claude-code")

	if want := "molecule-local/workspace-template-claude-code:abcdef012345-amd64--e2e-610481-1-lifestub"; sha != want {
		t.Errorf("LocalImageTag = %q, want %q", sha, want)
	}
	// The floating alias MUST be isolated too. It was the second shared name in
	// core#5031 and, unlike the sha tag, neither e2e script ever restored it.
	if want := "molecule-local/workspace-template-claude-code:latest--e2e-610481-1-lifestub"; latest != want {
		t.Errorf("LocalImageLatestTag = %q, want %q", latest, want)
	}
}

func TestLocalImageIsolation_DistinctTokensCannotCollide(t *testing.T) {
	const sha = "abcdef0123456789"

	t.Setenv(LocalImageIsolationEnv, "e2e-610481-1-lifestub")
	aSha, aLatest := LocalImageTag("claude-code", sha, "linux/amd64"), LocalImageLatestTag("claude-code")

	t.Setenv(LocalImageIsolationEnv, "e2e-610485-1-shsched")
	bSha, bLatest := LocalImageTag("claude-code", sha, "linux/amd64"), LocalImageLatestTag("claude-code")

	// Same runtime, same template HEAD sha, same arch — the exact inputs the two
	// concurrent jobs share. Only the isolation token differs, and that alone has
	// to be enough.
	if aSha == bSha {
		t.Errorf("two isolation tokens produced the SAME sha-pinned tag %q — the jobs would still clobber each other", aSha)
	}
	if aLatest == bLatest {
		t.Errorf("two isolation tokens produced the SAME :latest tag %q", aLatest)
	}
}

func TestLocalImageIsolation_IsolatedTagIsNeverTheSharedTag(t *testing.T) {
	const sha = "abcdef0123456789"

	t.Setenv(LocalImageIsolationEnv, "")
	sharedSha, sharedLatest := LocalImageTag("hermes", sha, ""), LocalImageLatestTag("hermes")

	t.Setenv(LocalImageIsolationEnv, "e2e-1-1-x")
	if got := LocalImageTag("hermes", sha, ""); got == sharedSha {
		t.Errorf("isolated sha tag %q equals the shared tag — an isolated job would still overwrite the real image", got)
	}
	if got := LocalImageLatestTag("hermes"); got == sharedLatest {
		t.Errorf("isolated :latest %q equals the shared :latest", got)
	}
}

func TestLocalImageIsolation_RejectsTokensThatCannotBeADockerTag(t *testing.T) {
	// Uppercase is legal in a Docker TAG but not in a repository path component;
	// keeping the token lower-only means the same string stays usable if the
	// isolation ever has to move into the namespace instead of the tag.
	for _, tok := range []string{
		"has space",
		"UPPER",
		"slash/es",
		"colon:s",
		"-leading",
		strings.Repeat("a", 49),
		"emoji-☃",
	} {
		t.Run(tok, func(t *testing.T) {
			t.Setenv(LocalImageIsolationEnv, tok)
			if _, err := LocalImageIsolation(); err == nil {
				t.Fatalf("LocalImageIsolation() accepted %q; a token that cannot appear in a Docker tag must be refused, not silently dropped back onto the SHARED tag", tok)
			}
		})
	}
}

func TestLocalImageIsolation_AcceptsTheShapesCIActuallyProduces(t *testing.T) {
	for _, tok := range []string{
		"e2e-610481-1-lifestub",
		"e2e-610485-1-shsched",
		"a",
		"run.1_2-3",
	} {
		t.Run(tok, func(t *testing.T) {
			t.Setenv(LocalImageIsolationEnv, tok)
			got, err := LocalImageIsolation()
			if err != nil {
				t.Fatalf("LocalImageIsolation() rejected %q: %v", tok, err)
			}
			if got != tok {
				t.Fatalf("LocalImageIsolation() = %q, want %q", got, tok)
			}
		})
	}
}

// An invalid token must not degrade to "no isolation" ANYWHERE on the path that
// actually resolves an image. Falling back silently is how a job that believes
// it is isolated goes back to writing the shared tag — the failure this whole
// change exists to make impossible.
func TestEnsureLocalImage_FailsClosedOnAnUnusableIsolationToken(t *testing.T) {
	t.Setenv(LocalImageIsolationEnv, "not a tag")

	opts := &LocalBuildOptions{
		checkTool: func(string) error { return nil },
		remoteHeadSha: func(context.Context, *LocalBuildOptions, string) (string, error) {
			t.Fatal("resolution continued past an invalid isolation token")
			return "", nil
		},
	}
	_, err := ensureLocalImageWithOpts(context.Background(), "claude-code", opts)
	if err == nil {
		t.Fatal("ensureLocalImageWithOpts succeeded with an invalid isolation token; want a hard error")
	}
	if !strings.Contains(err.Error(), LocalImageIsolationEnv) {
		t.Errorf("error %q does not name %s — an operator cannot fix what the message does not point at", err, LocalImageIsolationEnv)
	}
}
