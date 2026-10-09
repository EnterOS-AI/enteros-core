package provisioner

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// LOCAL IMAGE ISOLATION (core#5031)
// =================================
//
// In RegistryModeLocal the provisioner resolves a workspace image to
//
//	molecule-local/workspace-template-<runtime>:<template-HEAD-sha12>-<arch>
//
// Every component of that name is derived from the WORLD, not from the caller:
// the runtime, the template repo's HEAD sha, and the host arch. Two processes on
// one Docker daemon therefore compute the SAME name, always.
//
// That is correct — and safe — as long as everyone who writes that name writes
// the same CONTENT. The local-build path does: it builds the template at that
// exact sha, so a second builder produces the same image.
//
// CI does not. `tests/e2e/test_local_provision_lifecycle_e2e.sh` and
// `tests/e2e/test_selfhost_concierge_schedules_e2e.sh` each `docker tag` a tiny
// STUB runtime over that name so the provisioner resolves to the stub instead of
// building a 2.5GB template, and each restores the previous image id on cleanup.
// The runners are separate act_runner instances sharing ONE /var/run/docker.sock,
// so on molecule-core#5030 (head 8fd5f97) this happened:
//
//	03:04:15  concierge-schedules  tags its stub over the tag + :latest
//	03:04:16  lifecycle            tags its stub over the same tags
//	03:04:20  concierge-schedules  cleanup RESTORES the tag to the real image
//	03:04:27  lifecycle            re-provision resolves the tag -> the REAL image
//	03:04:38  lifecycle            real runtime replies "Invalid API key" -> FAIL
//
// A REQUIRED gate failed with an assertion about LLM credentials. Nothing in that
// message points at test infrastructure, which is what makes this expensive: it
// is silent, load-dependent (lifecycle normally finishes in ~44s and wins; it took
// 79s that time and lost), and it misattributes.
//
// THE FIX IS NOT A LOCK
// ---------------------
// Serialising the two jobs would remove the overlap but keep the shared mutable
// name, so the next lane that tags the same image reintroduces the defect —
// and it costs wall-clock on a required gate forever. The property worth having
// is that a job which SUBSTITUTES content can name a namespace nobody else
// resolves. Then concurrency is simply not a variable.
//
// MOLECULE_LOCAL_IMAGE_ISOLATION is that namespace. Unset (every production and
// developer path) the tags are byte-identical to before. Set, it appends
// `--<token>` to the sha-pinned tag AND to the floating `:latest` alias, so both
// shared names become private. The e2e scripts read the SAME env var to compute
// the tag they write, so there is exactly one source for the name — a script
// that computed it independently would drift the moment either side changed.
//
// FAIL-CLOSED, DELIBERATELY
// -------------------------
// A token that cannot appear in a Docker tag is an ERROR, never a fallback to
// "no isolation". A job that believes it is isolated and is silently not is
// strictly worse than today: it would write the shared tag while its cleanup and
// its logs both claim a private one.
const LocalImageIsolationEnv = "MOLECULE_LOCAL_IMAGE_ISOLATION"

// localImageIsolationSeparator is doubled on purpose. A single `-` would make
// `:<sha12>-<arch>-<token>` ambiguous with an arch that itself contains a dash
// (localImageArchSuffix emits `linux-arm-v7` shapes for a malformed platform),
// and an ambiguous tag is one a human reads wrong during an incident.
const localImageIsolationSeparator = "--"

// localImageIsolationMaxLen keeps the whole tag inside Docker's 128-char limit
// with room to spare: 12 (sha) + 1 + ~7 (arch) + 2 + 48 is 70.
const localImageIsolationMaxLen = 48

// Lower-case only. Docker tags permit uppercase, but a repository PATH component
// does not; keeping the token to the intersection means the same token stays
// valid if isolation ever has to move from the tag into the namespace.
var localImageIsolationRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]*$`)

// LocalImageIsolation returns the validated isolation token, or "" when the env
// var is unset or empty. A value that is set but unusable returns an error
// naming the variable — see the fail-closed note above.
func LocalImageIsolation() (string, error) {
	raw := os.Getenv(LocalImageIsolationEnv)
	tok := strings.TrimSpace(raw)
	if tok == "" {
		return "", nil
	}
	if len(tok) > localImageIsolationMaxLen {
		return "", fmt.Errorf("%s=%q is %d chars; max %d (the token is appended to a Docker tag)",
			LocalImageIsolationEnv, tok, len(tok), localImageIsolationMaxLen)
	}
	if !localImageIsolationRe.MatchString(tok) {
		return "", fmt.Errorf("%s=%q is not usable in a Docker tag: it must start with [a-z0-9] and contain only lower-case letters, digits, '.', '_' or '-'",
			LocalImageIsolationEnv, tok)
	}
	return tok, nil
}

// localImageIsolationSuffix is the string appended to both local tags. It is
// empty when isolation is unset AND when the token is invalid — the invalid case
// never reaches a tag because ensureLocalImageWithOpts refuses first
// (TestEnsureLocalImage_FailsClosedOnAnUnusableIsolationToken pins that
// ordering, so this cannot quietly become the fallback the doc comment forbids).
func localImageIsolationSuffix() string {
	tok, err := LocalImageIsolation()
	if err != nil || tok == "" {
		return ""
	}
	return localImageIsolationSeparator + tok
}
