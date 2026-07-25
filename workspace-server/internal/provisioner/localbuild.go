package provisioner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Local-build mode: clone the workspace-template-<runtime> repo from Gitea
// and `docker build` it on the host so OSS contributors can run molecule-core
// end-to-end without authenticating to (or being able to reach) GHCR/ECR.
//
// The flow:
//
//  1. ensureLocalImage(runtime) is called by the provisioner before
//     ContainerCreate, but only when Resolve().Mode == RegistryModeLocal.
//  2. We compute a cache key from the Gitea repo's HEAD sha (one HTTP
//     call to https://git.moleculesai.app/api/v1/repos/.../branches/main).
//  3. If `molecule-local/workspace-template-<runtime>:<sha12>` already
//     exists in the local Docker image store, we return immediately.
//  4. Otherwise: shallow git-clone the repo into the cache dir, then
//     `docker build [-platform=<override>] -t <tag>` on it (native arch
//     unless MOLECULE_IMAGE_PLATFORM overrides). We also tag `:latest`
//     so `docker images` shows a friendly entry.
//
// Platform: NATIVE by default (core#3502). The old unconditional
// linux/amd64 pin — kept for "parity with prod images" — made every
// first build on Apple Silicon run under QEMU emulation, reliably
// exceeding the 12-minute provision-timeout sweep: a guaranteed
// cancel loop where the concierge could never come online. A local
// build has no upstream manifest to match, so parity buys nothing a
// QEMU-emulated build actually delivers; operators who want forced
// parity set MOLECULE_IMAGE_PLATFORM, which governs the build AND the
// container-create (localBuildImagePlatform keeps them in lockstep).
// The tag embeds the arch so switching platforms can never serve a
// stale-arch cache hit.
//
// Auth: clone is anonymous (templates are public). If MOLECULE_GITEA_TOKEN
// is set, we use it via the URL's userinfo — the token is masked in
// every log line by maskTokenInURL().
//
// Failure mode: fail-closed. If Gitea is unreachable we surface a clear
// error message including the repo URL; we NEVER fall back to GHCR/ECR
// silently (would be a confusing bug for an OSS contributor who
// happens to have stale ECR creds in their docker config).

// gitTemplateRepoPrefix is the prefix all workspace-template repos live
// under on Gitea. Hardcoded so an attacker who controlled cfg.Runtime
// (defence-in-depth — today the field is platform-validated upstream)
// can only ever reach a repo under molecule-ai/.
//
// Operators who want to point local-build at a fork can override the
// full prefix via MOLECULE_LOCAL_TEMPLATE_REPO_PREFIX (e.g.
// `https://git.example.com/myorg/molecule-ai-workspace-template-`).
// Default-off; opt-in only.
const gitTemplateRepoPrefix = "https://git.moleculesai.app/molecule-ai/molecule-ai-workspace-template-"

// localBuildLockMap serializes concurrent ensureLocalImage calls per
// runtime so two workspace creates that hit the cold path together don't
// race on `docker build` (Docker's daemon would serialize anyway, but
// the duplicate clone + log spam are confusing). Lock granularity is
// per-runtime, so different runtimes still build in parallel.
var (
	localBuildLockMap   = make(map[string]*sync.Mutex)
	localBuildLockMapMu sync.Mutex
)

func runtimeBuildLock(runtime string) *sync.Mutex {
	localBuildLockMapMu.Lock()
	defer localBuildLockMapMu.Unlock()
	if m, ok := localBuildLockMap[runtime]; ok {
		return m
	}
	m := &sync.Mutex{}
	localBuildLockMap[runtime] = m
	return m
}

// LocalBuildOptions controls the local-build path. Exposed so tests can
// inject fakes without standing up a real git+docker chain. Production
// uses zero-value defaults via newDefaultLocalBuildOptions().
type LocalBuildOptions struct {
	// CacheDir is the host filesystem location where cloned template
	// repos are kept between builds. Empty = use $XDG_CACHE_HOME or
	// $HOME/.cache. Override via env var MOLECULE_LOCAL_BUILD_CACHE.
	CacheDir string

	// RepoPrefix is the URL prefix all template repos hang off. Empty
	// = use gitTemplateRepoPrefix. Override via env var
	// MOLECULE_LOCAL_TEMPLATE_REPO_PREFIX.
	RepoPrefix string

	// Token, if non-empty, is sent via URL userinfo to Gitea. Default
	// empty (templates are public). Override via env var
	// MOLECULE_GITEA_TOKEN.
	Token string

	// Platform is the --platform value for the build. Empty = host
	// native (the default — core#3502); MOLECULE_IMAGE_PLATFORM
	// overrides it, in lockstep with the container-create side
	// (localBuildImagePlatform). Exposed so tests can override.
	Platform string

	// StallGrace / Ceiling govern the progress-driven runner that wraps the
	// `docker build` + `git clone` (stallrunner.go). StallGrace is the
	// primary gate — max time with ZERO output before the process is killed
	// as wedged. Ceiling is the absolute wall-clock backstop. Zero on either
	// = use the package default (buildStallGrace() / buildCeiling(),
	// env-overridable). EnsureLocalImage sets Ceiling from the provision ctx's
	// deadline (which handlers.dockerProvisionTimeout has already set to the
	// per-runtime provision_timeout_seconds, floored at 12m), so a runtime
	// declaring 30m is not capped at the 12m default. Exposed so tests can
	// drive the runner with tiny values.
	StallGrace time.Duration
	Ceiling    time.Duration

	// HTTPClient is used for the Gitea-API HEAD-sha lookup. Empty =
	// http.DefaultClient with a 30s timeout.
	HTTPClient *http.Client

	// remoteHeadSha + dockerBuild + gitClone + checkTool are seams for tests;
	// if nil, the production implementations are used.
	remoteHeadSha func(ctx context.Context, opts *LocalBuildOptions, runtime string) (string, error)
	gitClone      func(ctx context.Context, opts *LocalBuildOptions, runtime, dest string) error
	dockerBuild   func(ctx context.Context, opts *LocalBuildOptions, contextDir, tag string) error
	dockerHasTag  func(ctx context.Context, tag string) (bool, error)
	dockerTag     func(ctx context.Context, src, dst string) error
	// checkTool validates that the named binary is on PATH. nil = production
	// LookPath check; tests override to skip or mock.
	checkTool func(tool string) error
}

// stallGrace / ceiling return the effective runner gates, defaulting to the
// package (env-overridable) values when a field is unset. Keeping the
// fall-through here means a zero-valued LocalBuildOptions (e.g. a test that
// only sets the seams) still gets sane production gates unless it opts in.
func (o *LocalBuildOptions) stallGrace() time.Duration {
	if o.StallGrace > 0 {
		return o.StallGrace
	}
	return buildStallGrace()
}

func (o *LocalBuildOptions) ceiling() time.Duration {
	if o.Ceiling > 0 {
		return o.Ceiling
	}
	return buildCeiling()
}

func newDefaultLocalBuildOptions() *LocalBuildOptions {
	o := &LocalBuildOptions{
		CacheDir:   os.Getenv("MOLECULE_LOCAL_BUILD_CACHE"),
		RepoPrefix: os.Getenv("MOLECULE_LOCAL_TEMPLATE_REPO_PREFIX"),
		Token:      os.Getenv("MOLECULE_GITEA_TOKEN"),
		Platform:   localBuildImagePlatform(),
		StallGrace: buildStallGrace(),
		Ceiling:    buildCeiling(),
	}
	if o.CacheDir == "" {
		if xdg := os.Getenv("XDG_CACHE_HOME"); xdg != "" {
			o.CacheDir = filepath.Join(xdg, "molecule", "workspace-template-build")
		} else if home, err := os.UserHomeDir(); err == nil {
			o.CacheDir = filepath.Join(home, ".cache", "molecule", "workspace-template-build")
		} else {
			// Last-resort fallback: /tmp. Loses the cache between reboots
			// but at least lets the path produce builds.
			o.CacheDir = filepath.Join(os.TempDir(), "molecule", "workspace-template-build")
		}
	}
	if o.RepoPrefix == "" {
		o.RepoPrefix = gitTemplateRepoPrefix
	}
	o.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	return o
}

// LocalImageTag formats the SHA-pinned tag for a runtime. Exported for
// tests + the provisioner's image-resolution branch.
//
// The tag embeds the target ARCH (e.g. `:abc123def456-arm64`) so a machine
// that switches MOLECULE_IMAGE_PLATFORM (or a repo that changes the default)
// can never get a stale-arch cache hit — the pre-#3502 tag was arch-blind
// and the exists-check was tag-only.
func LocalImageTag(runtime, sha, platform string) string {
	short := sha
	if len(short) > 12 {
		short = short[:12]
	}
	return fmt.Sprintf("%s/workspace-template-%s:%s-%s", localImagePrefix, runtime, short, localImageArchSuffix(platform))
}

// localImageArchSuffix derives the tag's arch component from a --platform
// string ("linux/amd64" → "amd64"); empty platform = the host's native arch
// (what an unpinned docker build produces).
func localImageArchSuffix(platform string) string {
	p := strings.TrimSpace(platform)
	if p == "" {
		return goruntime.GOARCH
	}
	parts := strings.Split(p, "/")
	if len(parts) >= 2 && parts[1] != "" {
		return parts[1]
	}
	return strings.ReplaceAll(p, "/", "-")
}

// LocalImageLatestTag returns the floating `:latest` form. Used as a
// human-readable alias and as the value RuntimeImage() returns in
// local-mode.
func LocalImageLatestTag(runtime string) string {
	return fmt.Sprintf("%s/workspace-template-%s:latest", localImagePrefix, runtime)
}

// IsLocalBuildImage reports whether an image reference names a locally-built
// workspace image (the molecule-local/ namespace). The container-create path
// uses it to pick localBuildImagePlatform() over the registry-pull default —
// a locally-built image must run as the arch it was built for (core#3502).
func IsLocalBuildImage(image string) bool {
	return strings.HasPrefix(image, localImagePrefix+"/")
}

// EnsureLocalImage is the entry point the provisioner calls before
// ContainerCreate when Resolve().Mode == RegistryModeLocal. Returns the
// image tag (SHA-pinned form) the caller should hand to Docker, or an
// error if the build/clone fails.
//
// Concurrency: per-runtime lock; parallel calls for the same runtime
// share the build, parallel calls for different runtimes proceed.
//
// Idempotent: a cached SHA-pinned tag short-circuits without network
// or docker calls. The Gitea HEAD lookup is the only network call on
// the cache-hit path.
func EnsureLocalImage(ctx context.Context, runtime string) (string, error) {
	opts := newDefaultLocalBuildOptions()
	// The provision ctx already carries the per-runtime absolute deadline
	// (handlers.dockerProvisionTimeout = max(provision_timeout_seconds, 12m),
	// so hermes = 30m). Derive the stall-runner's ceiling FROM that deadline
	// so the per-runtime window actually reaches the build — otherwise the
	// runner's own 12m default backstop fires first and a 12–30m hermes build
	// is killed at 12m despite its declared 30m. Paths with no deadline (the
	// bundle importer) keep the buildCeiling() default.
	if c := ceilingFromCtxDeadline(ctx); c > 0 {
		opts.Ceiling = c
	}
	return ensureLocalImageWithOpts(ctx, runtime, opts)
}

// ceilingFromCtxDeadline returns the stall-runner ceiling implied by ctx's
// deadline: the remaining time-to-deadline when ctx has one (so the per-runtime
// provision window already on the ctx reaches the build), else 0 meaning "no
// ctx-implied ceiling — keep the option default". The ctx is the single source
// of truth for the absolute cap; the derived ceiling just gives it a clear
// typed error (errBuildCeiling) rather than a bare context-cancel.
func ceilingFromCtxDeadline(ctx context.Context) time.Duration {
	if dl, ok := ctx.Deadline(); ok {
		if remaining := time.Until(dl); remaining > 0 {
			return remaining
		}
	}
	return 0
}

// ensureLocalImageHook is the seam Start() calls into. Production code
// uses EnsureLocalImage; tests substitute a fake to exercise the
// provisioner-Start integration without standing up a real
// git+docker chain. Single-process scoped — never reassigned in
// production code.
var ensureLocalImageHook = EnsureLocalImage

// checkToolOnPath verifies tool is on PATH and returns an error with a
// descriptive message if missing. Used for pre-flight validation before the
// clone/build cold path.
func checkToolOnPath(tool string) error {
	path, err := exec.LookPath(tool)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return fmt.Errorf("%q not found on PATH — local-build mode requires both docker and git; either install them, or set MOLECULE_IMAGE_REGISTRY so local-build is bypassed", tool)
		}
		return fmt.Errorf("LookPath(%q) failed: %w", tool, err)
	}
	log.Printf("local-build: pre-flight OK (%s=%s)", tool, path)
	return nil
}

func ensureLocalImageWithOpts(ctx context.Context, runtime string, opts *LocalBuildOptions) (string, error) {
	if !IsKnownRuntime(runtime) {
		return "", fmt.Errorf("local-build: refusing to build unknown runtime %q (must be one of %v)", runtime, knownRuntimes)
	}

	lock := runtimeBuildLock(runtime)
	lock.Lock()
	defer lock.Unlock()

	// Pre-flight: both docker and git are required even on the cache-hit
	// path (docker is used for image inspect + tag). Fail fast with a clear
	// message rather than a cryptic "exec: docker: executable file not found".
	checkFn := opts.checkTool
	if checkFn == nil {
		checkFn = checkToolOnPath
	}
	if err := checkFn("docker"); err != nil {
		return "", fmt.Errorf("local-build: %w; set MOLECULE_IMAGE_REGISTRY to bypass local-build mode", err)
	}
	if err := checkFn("git"); err != nil {
		return "", fmt.Errorf("local-build: %w; set MOLECULE_IMAGE_REGISTRY to bypass local-build mode", err)
	}

	// 1. HEAD lookup → cache key.
	headFn := opts.remoteHeadSha
	if headFn == nil {
		headFn = remoteHeadShaProd
	}
	sha, err := headFn(ctx, opts, runtime)
	if err != nil {
		// Fail-closed: do not fall back to GHCR/ECR. The whole point of
		// local-build mode is that GHCR is unreachable.
		return "", fmt.Errorf("local-build: cannot determine HEAD sha for runtime %q at %s: %w", runtime, repoURL(opts, runtime), err)
	}
	if len(sha) < 12 {
		return "", fmt.Errorf("local-build: Gitea returned a short sha %q for runtime %q (expected ≥12 chars)", sha, runtime)
	}
	tag := LocalImageTag(runtime, sha, opts.Platform)
	latest := LocalImageLatestTag(runtime)

	// Cache-key note (RUNTIME_VERSION pin, #53 hardening): the tag is keyed on
	// runtime+HEAD-sha+arch — deliberately NOT on the .runtime-version pin. That
	// pin is only knowable AFTER the clone, so folding it into the tag here
	// (pre-clone, on the fast cache-hit path) would force a clone on every call
	// just to compute the key, defeating the git-free cache-hit path entirely.
	// Sha-keying is already version-correct because the pin never changes
	// independently of the sha: the propagation bot bumps .runtime-version by
	// COMMITTING it to the template repo's main branch, which moves HEAD → a new
	// sha → a new tag → a rebuild that re-reads the fresh pin. A .runtime-version
	// change at the SAME sha is not a state the SSOT flow can produce (the file
	// lives in the repo; changing it IS a commit). So the RUNTIME_VERSION
	// build-arg only needs to cache-bust the pip layer WITHIN a build (which it
	// does), not the outer image tag. If that invariant is ever broken (e.g. a
	// pin injected out-of-band without a commit), fold the resolved version into
	// the tag AFTER the clone — but that is a contortion the current flow does
	// not warrant.

	// 2. Cache hit?
	hasFn := opts.dockerHasTag
	if hasFn == nil {
		hasFn = dockerHasTagProd
	}
	exists, hasErr := hasFn(ctx, tag)
	if hasErr != nil {
		log.Printf("local-build: image inspect for %s failed (%v); will rebuild", tag, hasErr)
	}
	if exists {
		log.Printf("local-build: cache hit for %s (sha=%s) — skipping clone+build", tag, sha[:12])
		// Refresh the floating :latest alias so admins inspecting `docker
		// images` see the current sha. Best-effort.
		tagFn := opts.dockerTag
		if tagFn == nil {
			tagFn = dockerTagProd
		}
		if tErr := tagFn(ctx, tag, latest); tErr != nil {
			log.Printf("local-build: best-effort retag of %s → %s failed: %v", tag, latest, tErr)
		}
		return tag, nil
	}

	// 3. Cold path — clone + build.
	dest := filepath.Join(opts.CacheDir, runtime, sha[:12])
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", fmt.Errorf("local-build: prepare cache dir %q: %w", filepath.Dir(dest), err)
	}
	// Idempotent: if the dest exists from a previous failed run, wipe and
	// re-clone so we don't build a partial tree.
	if _, statErr := os.Stat(dest); statErr == nil {
		if rmErr := os.RemoveAll(dest); rmErr != nil {
			return "", fmt.Errorf("local-build: clean stale cache dir %q: %w", dest, rmErr)
		}
	}

	cloneFn := opts.gitClone
	if cloneFn == nil {
		cloneFn = gitCloneProd
	}
	log.Printf("local-build: cloning %s → %s (sha=%s)", redactedRepoURL(opts, runtime), dest, sha[:12])
	cloneStart := time.Now()
	if err := cloneFn(ctx, opts, runtime, dest); err != nil {
		// Best-effort cleanup so a half-cloned tree doesn't poison future runs.
		_ = os.RemoveAll(dest)
		return "", fmt.Errorf("local-build: clone %s: %w", redactedRepoURL(opts, runtime), err)
	}
	log.Printf("local-build: clone complete in %s", time.Since(cloneStart).Round(time.Millisecond))

	// 4. Sanity-check the cloned tree contains a Dockerfile at the root.
	dockerfile := filepath.Join(dest, "Dockerfile")
	info, statErr := os.Stat(dockerfile)
	if statErr != nil || info.IsDir() {
		_ = os.RemoveAll(dest)
		return "", fmt.Errorf("local-build: cloned tree at %s has no Dockerfile (template repo malformed)", dest)
	}

	// 5. Build.
	buildFn := opts.dockerBuild
	if buildFn == nil {
		buildFn = dockerBuildProd
	}
	log.Printf("local-build: docker build start for %s (platform=%s, context=%s)", tag, opts.Platform, dest)
	buildStart := time.Now()
	if err := buildFn(ctx, opts, dest, tag); err != nil {
		return "", fmt.Errorf("local-build: docker build %s: %w", tag, err)
	}
	log.Printf("local-build: docker build done for %s in %s", tag, time.Since(buildStart).Round(time.Second))

	// Tag :latest as a friendly alias.
	tagFn := opts.dockerTag
	if tagFn == nil {
		tagFn = dockerTagProd
	}
	if err := tagFn(ctx, tag, latest); err != nil {
		log.Printf("local-build: best-effort retag of %s → %s failed: %v", tag, latest, err)
	}

	return tag, nil
}

// repoURL composes the full Gitea repo URL for the given runtime. The
// prefix is hardcoded by default; operators can override via env so a
// fork can point local-build at their own Gitea instance.
func repoURL(opts *LocalBuildOptions, runtime string) string {
	return opts.RepoPrefix + runtime
}

// redactedRepoURL returns the same value with any embedded token replaced
// by "***". Use this for log lines.
func redactedRepoURL(opts *LocalBuildOptions, runtime string) string {
	return maskTokenInURL(repoURL(opts, runtime))
}

// maskTokenInURL replaces userinfo (username:password@) in a URL with
// `***@` so log lines never echo a Gitea PAT. Returns the input as-is
// on parse failures (defence: never silently corrupt the visible URL).
//
// Implementation note: net/url's URL.User stringifier percent-encodes
// the username, so `u.User = url.User("***"); u.String()` would yield
// `https://%2A%2A%2A@host/...` — unhelpful for humans grepping logs.
// We drop the userinfo via URL.User=nil, get the canonical scheme-and-
// rest, and re-insert the literal `***@` between the scheme separator
// and the host.
func maskTokenInURL(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.User == nil {
		return s
	}
	u.User = nil
	out := u.String()
	prefix := u.Scheme + "://"
	if !strings.HasPrefix(out, prefix) {
		return s
	}
	return prefix + "***@" + out[len(prefix):]
}

// remoteHeadShaProd looks up the HEAD commit sha of branch `main` for
// the workspace-template-<runtime> repo on Gitea. We use the Gitea API
// (a single HTTPS call) rather than `git ls-remote` so we don't need a
// git binary just for the HEAD lookup — we still need git for the
// clone, but the cache-hit path stays git-free.
func remoteHeadShaProd(ctx context.Context, opts *LocalBuildOptions, runtime string) (string, error) {
	// Convert a `git.example.com/org/prefix-` URL into the API form
	// `git.example.com/api/v1/repos/org/prefix-<runtime>/branches/main`.
	// Works for both git.moleculesai.app (default) and any forks that
	// share the Gitea API shape.
	apiURL, err := giteaBranchAPIURL(opts.RepoPrefix, runtime, "main")
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return "", err
	}
	if opts.Token != "" {
		// Gitea accepts "token <PAT>" in the Authorization header for
		// API calls. Userinfo is also accepted but only matters for
		// the HTTPS clone, not the JSON API.
		req.Header.Set("Authorization", "token "+opts.Token)
	}
	cli := opts.HTTPClient
	if cli == nil {
		cli = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := cli.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("repo not found at %s — runtime %q may not be mirrored to Gitea (expected one of claude-code, codex, hermes, openclaw)", apiURL, runtime)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", fmt.Errorf("auth failure (%d) at %s — verify MOLECULE_GITEA_TOKEN if private repo", resp.StatusCode, apiURL)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HEAD lookup at %s returned %d", apiURL, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", fmt.Errorf("read HEAD response body: %w", err)
	}
	// Tiny ad-hoc parser: we want commit.id, no need to drag in encoding/json
	// — actually simpler to use json. Switch to it.
	return parseGiteaBranchHeadSha(body)
}

// giteaBranchAPIURL maps a repo-prefix URL like
// `https://git.moleculesai.app/molecule-ai/molecule-ai-workspace-template-`
// + runtime "claude-code" + branch "main"
// to the API URL
// `https://git.moleculesai.app/api/v1/repos/molecule-ai/molecule-ai-workspace-template-claude-code/branches/main`.
func giteaBranchAPIURL(repoPrefix, runtime, branch string) (string, error) {
	u, err := url.Parse(repoPrefix + runtime)
	if err != nil {
		return "", fmt.Errorf("parse repo URL %q: %w", repoPrefix+runtime, err)
	}
	parts := strings.TrimPrefix(u.Path, "/")
	parts = strings.TrimSuffix(parts, "/")
	if parts == "" {
		return "", fmt.Errorf("repo URL %q has empty path", repoPrefix+runtime)
	}
	// Expect `<org>/<repo>` (single slash) — the prefix already includes
	// org+partial-repo; runtime appends the rest.
	if !strings.Contains(parts, "/") {
		return "", fmt.Errorf("repo URL %q missing org/repo path", repoPrefix+runtime)
	}
	apiURL := url.URL{
		Scheme: u.Scheme,
		Host:   u.Host,
		Path:   "/api/v1/repos/" + parts + "/branches/" + branch,
	}
	return apiURL.String(), nil
}

// parseGiteaBranchHeadSha extracts commit.id from the Gitea
// /branches/<name> response. We use a permissive substring scan so a
// missing-key in the JSON gives a clear error rather than the
// json.Decoder's somewhat opaque "missing field" message.
func parseGiteaBranchHeadSha(body []byte) (string, error) {
	// Look for `"id":"<40-hex>"` inside the commit object.
	idx := strings.Index(string(body), `"id":"`)
	if idx < 0 {
		return "", errors.New("gitea branch response missing commit.id field")
	}
	rest := string(body[idx+len(`"id":"`):])
	end := strings.IndexByte(rest, '"')
	if end < 0 {
		return "", errors.New("gitea branch response has malformed commit.id (no closing quote)")
	}
	sha := rest[:end]
	if len(sha) < 7 {
		return "", fmt.Errorf("gitea returned suspiciously short sha %q", sha)
	}
	return sha, nil
}

// gitCloneProd shallow-clones the runtime's template repo into dest.
//
// We invoke `git` rather than implementing the protocol ourselves —
// every host that runs the workspace-server already needs git available
// (it's a hard dep of go-mod for vendored repos) and the OSS contributor
// onboarding doc lists it as a prerequisite.
func gitCloneProd(ctx context.Context, opts *LocalBuildOptions, runtime, dest string) error {
	cloneURL := repoURL(opts, runtime)
	if opts.Token != "" {
		// HTTPS clone with userinfo: https://oauth2:<token>@host/...
		u, err := url.Parse(cloneURL)
		if err == nil {
			u.User = url.UserPassword("oauth2", opts.Token)
			cloneURL = u.String()
		}
		// On parse failure we silently fall through to the public URL —
		// better to attempt the anonymous clone than to refuse outright.
	}
	// --progress FORCES git to emit its "Receiving objects: NN%" meter even
	// though stderr is a pipe, not a TTY (git suppresses it on a pipe by
	// default). Without it a healthy-but-slow clone is silent for its whole
	// transfer, so the stall gate degrades to a blind wall-clock and a large
	// clone running past the grace is false-killed. The meter is \r-delimited,
	// which the runner's chunk reader handles fine (the old line scanner would
	// have accumulated it into one over-cap line).
	cmd := exec.CommandContext(ctx, "git", "clone", "--progress", "--depth=1", "--branch=main", "--single-branch", cloneURL, dest)
	// Drop git's askpass prompts so we fail-fast on auth errors instead
	// of hanging waiting for an interactive password.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/bin/echo")
	// Same progress-driven runner as the build: with --progress a
	// genuinely-progressing clone streams git's meter (resetting the no-output
	// clock), so it is never killed, while a clone hung on an unreachable host
	// emits nothing and is reaped after the stall grace (the askpass drop
	// already covers the interactive-auth hang; this covers the
	// network-black-hole hang).
	out, err := runStreamingCommand(ctx, cmd, opts.stallGrace(), opts.ceiling())
	if err != nil {
		// Mask the token in any error string git emits via stderr — git
		// occasionally echoes the URL verbatim on failure.
		errMsg := maskTokenInString(string(out), opts.Token)
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(errMsg))
	}
	return nil
}

// maskTokenInString replaces literal occurrences of the token with `***`.
// Defence against git binary or docker echoing the URL into stderr.
func maskTokenInString(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "***")
}

// dockerBuildProd invokes the docker CLI to build the workspace-template
// image. We shell out rather than use the Docker SDK's ImageBuild — the
// SDK requires hand-tarballing the build context, which adds a
// non-trivial code path with its own bug surface. The docker CLI is
// already a hard dep of the workspace-server (the provisioner needs the
// daemon), so requiring the CLI binary on PATH adds nothing.
//
// Uses the legacy `docker build` (not `docker buildx build`) because
// buildx isn't always installed by default on Linux distros and the
// legacy builder produces an image the local Docker daemon picks up
// automatically. We pass --platform=linux/amd64 directly; with Docker
// 20.10+ this works without buildx because the legacy builder
// auto-promotes to BuildKit when available, falling back to v1
// otherwise (still produces an amd64 image via QEMU).
func dockerBuildProd(ctx context.Context, opts *LocalBuildOptions, contextDir, tag string) error {
	// Read the .runtime-version pin ONCE and thread the single resolved value
	// through both the log line and the build-arg so they can never drift.
	runtimeVersion := readRuntimeVersionPin(contextDir)
	args := dockerBuildArgs(opts, contextDir, tag, runtimeVersion)
	if runtimeVersion != "" {
		log.Printf("local-build: pinning RUNTIME_VERSION=%s from .runtime-version for %s", runtimeVersion, tag)
	}
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = append(os.Environ(), "DOCKER_BUILDKIT=1")
	// Progress-driven runner (stallrunner.go): stream the build, reset a
	// no-output clock on every line, and kill on a real stall or the
	// absolute ceiling — NOT on a fixed 3-min wall clock. A cold build that
	// legitimately runs long (large pip/apt layer, QEMU cross-arch) streams
	// output the whole time and is never killed; a wedged build (network
	// black hole) is killed after opts.stallGrace() with a clear message.
	// The exemption keeps BuildKit's final export/unpack phase — silent by
	// design, minutes-long for a multi-GB image — from reading as a stall
	// (buildkitQuietPhaseExempt); the ceiling still bounds it.
	out, err := runStreamingCommandExempt(ctx, cmd, opts.stallGrace(), opts.ceiling(), buildkitQuietPhaseExempt)
	if err != nil {
		// Sanitize defensive — docker build output shouldn't contain a
		// token, but maskTokenInString is a no-op when token is empty.
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(maskTokenInString(string(out), opts.Token)))
	}
	return nil
}

// buildkitQuietPhaseExempt reports whether the tail of a `docker build`'s
// output ends INSIDE BuildKit's final image export/unpack phase. That phase
// ("#N exporting …" / "#N unpacking to …") emits nothing until it completes —
// on a multi-GB image it is minutes of silence by design, local disk I/O
// only, no network to hang on. Without this exemption the stall watchdog
// killed every first-boot build of the ~7GB hermes image mid-unpack
// ("no output within stall grace 4m0s", 2026-07-18 fresh-onboarding
// failure) — the one phase whose silence is healthy.
//
// Detection is deliberately narrow: the LAST non-empty output line must be a
// BuildKit step-status line (`#<n> `) for an export/unpack marker that has
// not yet printed its completion (`… done` suffix / `#<n> DONE`). Every
// network-bound step (pull, apt, pip, git) streams progress, so those stay
// under the normal no-output gate. A false positive merely defers the kill
// to the absolute per-runtime ceiling — bounded, never forever.
func buildkitQuietPhaseExempt(tail []byte) bool {
	trimmed := bytes.TrimRight(tail, " \t\r\n")
	if len(trimmed) == 0 {
		return false
	}
	if i := bytes.LastIndexByte(trimmed, '\n'); i >= 0 {
		trimmed = trimmed[i+1:]
	}
	last := bytes.TrimSpace(trimmed)
	// Must be a BuildKit step-status line: "#<digits> ...".
	if len(last) < 3 || last[0] != '#' {
		return false
	}
	rest := last[1:]
	d := 0
	for d < len(rest) && rest[d] >= '0' && rest[d] <= '9' {
		d++
	}
	if d == 0 || d >= len(rest) || rest[d] != ' ' {
		return false
	}
	step := rest[d+1:]
	// A completed phase prints "... done" (and the block ends with "#N DONE").
	if bytes.HasSuffix(step, []byte(" done")) || bytes.HasPrefix(step, []byte("DONE")) {
		return false
	}
	return bytes.HasPrefix(step, []byte("unpacking to ")) || bytes.HasPrefix(step, []byte("exporting "))
}

// dockerBuildArgs assembles the `docker build …` argv. Extracted from
// dockerBuildProd so the RUNTIME_VERSION SSOT-pin behavior can be asserted in a
// unit test without shelling out to docker.
//
// SSOT runtime pin: forward the template's .runtime-version as the
// RUNTIME_VERSION build-arg, exactly as the publish-image workflow's
// resolve-version step does — so a locally-built image installs the SAME runtime
// the pushed image does (one SSOT, .runtime-version, consumed identically local +
// prod). The ARG value also KEYS the pip layer's build cache, so a version change
// cache-busts a stale runtime instead of docker silently serving an old cached
// layer. Without this the local build ships whatever the cache holds — the
// RUNTIME_VERSION cache-trap (#53) that breaks any template delegating to a
// runtime-shipped helper (e.g. the mgmt-MCP prebake, #54: a delegating RUN that
// imports a script absent from the stale runtime → exit 127). Absent/unreadable
// .runtime-version falls through to the Dockerfile/requirements pin (same
// graceful behavior as publish).
//
// runtimeVersion is the ALREADY-resolved pin (from readRuntimeVersionPin),
// passed in rather than re-read here so the file is read exactly ONCE per build
// — the caller (dockerBuildProd) logs the same value it forwards, so the log
// line and the build-arg can never drift.
func dockerBuildArgs(opts *LocalBuildOptions, contextDir, tag, runtimeVersion string) []string {
	args := []string{"build"}
	if opts.Platform != "" {
		args = append(args, "--platform="+opts.Platform)
	}
	if runtimeVersion != "" {
		args = append(args, "--build-arg", "RUNTIME_VERSION="+runtimeVersion)
	}
	args = append(args,
		"-t", tag,
		"-f", filepath.Join(contextDir, "Dockerfile"),
		contextDir,
	)
	return args
}

// runtimeVersionPinRE is a deliberately permissive "version-ish" gate applied
// to the normalized pin before it is forwarded to `pip install ==${...}`. It
// accepts a leading digit followed by dot/alnum/`+`/`-`/`_`/`.` runs — enough
// to pass PEP 440 releases (0.3.115), pre/post/dev suffixes (1.2.0rc1,
// 1.2.0.post1, 1.2.0.dev3), and local versions (1.2.0+cpu). It exists only to
// REJECT obvious garbage (a stray word, a path, an unresolved `${VAR}`) that
// would hard-fail pip; a value that clears it is handed straight through, so
// the ultimate arbiter of validity stays pip/the package index, exactly as in
// prod (the resolve-version step forwards the raw first line untouched).
var runtimeVersionPinRE = regexp.MustCompile(`^[0-9][0-9A-Za-z.+_-]*$`)

// readRuntimeVersionPin resolves the RUNTIME_VERSION build-arg from
// <contextDir>/.runtime-version, or "" when there is no usable pin (falls
// through to the Dockerfile/requirements default, exactly as the publish path
// does). The .runtime-version file is the SSOT (propagation-bot–synced from each
// runtime release; #53), consumed identically by prod publish and local-build.
//
// Prod parity: the publish-image workflow's resolve-version step does
// `head -n1 .runtime-version | tr -d '[:space:]'` — take the FIRST line and
// strip ALL whitespace (not just leading/trailing). We mirror that exactly so a
// pin with internal or trailing whitespace resolves identically local vs prod.
//
// Beyond prod parity we add two guards that keep a local build resilient
// WITHOUT silently shipping a stale runtime:
//
//   - A present-but-unreadable file (permissions, IO error) is NOT conflated
//     with "absent": os.IsNotExist returns "" quietly (the legit no-pin case),
//     but any OTHER read error is logged LOUDLY with the path+error and returns
//     "". Silently swallowing it would omit the build-arg and reship the stale
//     pip layer with zero signal — the very cache-trap #53 closed.
//   - The value is normalized to a bare version so a documented SSOT form never
//     hard-fails pip: an `<runtime>@<version>` form keeps the part after the
//     LAST `@`, and a single leading `v` is stripped. If the result is not
//     version-ish (empty / not matching runtimeVersionPinRE) we log LOUDLY and
//     return "" rather than forward garbage to `pip install ==${...}`.
//
// Never returns an error: the build stays resilient (an absent/garbage pin just
// falls through to the Dockerfile default) but a genuine miss is always visible.
func readRuntimeVersionPin(contextDir string) string {
	path := filepath.Join(contextDir, ".runtime-version")
	b, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			// Present but unreadable — DO NOT conflate with absent. Log loudly
			// so the omitted pin (and the stale-layer risk it carries) is
			// visible; stay resilient by falling through to the default.
			log.Printf("local-build: WARNING: .runtime-version present but unreadable at %s: %v — RUNTIME_VERSION pin omitted, build will use the Dockerfile/requirements default (stale-runtime risk; fix the file perms/IO)", path, err)
		}
		return ""
	}
	// First line only, then strip ALL whitespace (prod: head -n1 | tr -d '[:space:]').
	line := string(b)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	raw := stripAllWhitespace(line)
	if raw == "" {
		// Empty pin = legit no-pin case (empty/blank file); quiet fall-through.
		return ""
	}
	v := normalizeRuntimeVersionPin(raw)
	if !runtimeVersionPinRE.MatchString(v) {
		log.Printf("local-build: WARNING: .runtime-version at %s holds a non-version-ish value %q (normalized %q) — RUNTIME_VERSION pin omitted rather than forwarding garbage to pip; build will use the Dockerfile/requirements default", path, raw, v)
		return ""
	}
	return v
}

// stripAllWhitespace removes every unicode-space rune, mirroring prod's
// `tr -d '[:space:]'` (which deletes spaces/tabs/newlines/CR/FF/VT). Used so a
// .runtime-version with internal or trailing whitespace normalizes identically
// local vs prod.
func stripAllWhitespace(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}

// normalizeRuntimeVersionPin coerces a documented SSOT pin form to the bare
// version pip expects:
//   - `<runtime>@<version>` → the part after the LAST `@` (so `hermes@1.2.0`
//     and even `a@b@1.2.0` resolve to `1.2.0`).
//   - a single leading `v` is stripped (`v1.2.0` → `1.2.0`).
//
// A value already bare passes through unchanged. Validation (version-ish check)
// happens in the caller AFTER this normalization.
func normalizeRuntimeVersionPin(s string) string {
	if i := strings.LastIndexByte(s, '@'); i >= 0 {
		s = s[i+1:]
	}
	if len(s) >= 2 && (s[0] == 'v' || s[0] == 'V') {
		s = s[1:]
	}
	return s
}

// dockerHasTagProd returns true iff the given tag exists in the local
// image store. Used as the fast cache-hit check.
func dockerHasTagProd(ctx context.Context, tag string) (bool, error) {
	cmd := exec.CommandContext(ctx, "docker", "image", "inspect", "--format={{.Id}}", tag)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return strings.TrimSpace(string(out)) != "", nil
	}
	// `docker image inspect` exits 1 with "Error: No such image" when
	// missing — that's a definitive false, not an error condition.
	low := strings.ToLower(string(out))
	if strings.Contains(low, "no such image") || strings.Contains(low, "not found") {
		return false, nil
	}
	return false, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
}

// dockerTagProd creates an alias from src → dst. Used to refresh the
// floating `:latest` after a build or cache hit.
func dockerTagProd(ctx context.Context, src, dst string) error {
	cmd := exec.CommandContext(ctx, "docker", "tag", src, dst)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// CacheKey is exposed for diagnostic logs / tests so the cache-key shape
// is documented in code rather than only as a string format.
//
//	cache_key = sha256(runtime || head_sha || repoPrefix)[:16]
//
// Today only the SHA is consumed, but the helper is kept for future
// extensions (e.g. include Dockerfile-content-hash to invalidate when
// only the Dockerfile changes between two runs targeting the same SHA).
func CacheKey(runtime, sha, repoPrefix string) string {
	h := sha256.Sum256([]byte(runtime + "|" + sha + "|" + repoPrefix))
	return hex.EncodeToString(h[:8])
}
