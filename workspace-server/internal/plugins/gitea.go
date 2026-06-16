package plugins

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// GiteaResolver fetches plugins from a (typically private) Gitea repository
// by shallow-cloning at the specified ref and extracting an optional
// subpath. It exists so a declared plugin can resolve to a *subdirectory*
// of a larger repo — e.g. the `agent-skills/seo-all/` skill package inside
// the private seo-agent template repo — which the GitHub resolver cannot do
// (it copies the whole repo root).
//
// Source-contract string (the value a template puts in `plugins:`):
//
//	gitea://<owner>/<repo>[/<subpath...>]#<ref>
//
// Examples:
//
//	gitea://molecule-ai/molecule-ai-workspace-template-seo-agent/agent-skills/seo-all#main
//	gitea://molecule-ai/some-repo#v1.2.0          (whole-repo, no subpath)
//	gitea://molecule-ai/some-repo/plugins/foo#sha:abc123
//
// Parsing: the FIRST two path segments are <owner>/<repo>; everything after
// (up to the optional `#<ref>`) is the in-repo subpath. The resolved plugin
// name is the last subpath segment (or <repo> when no subpath is given).
//
// Authentication: private Gitea repos need a PAT. The resolver reads the
// token from the environment (MOLECULE_TEMPLATE_REPO_TOKEN by default — the
// same read-only Gitea PAT CP PR#850 already places on every tenant box).
// The token is injected into the clone URL's userinfo and never logged.
//
// Pinned-ref enforcement mirrors GithubResolver: an unpinned spec (no
// `#<ref>`) is rejected unless PLUGIN_ALLOW_UNPINNED=true.
type GiteaResolver struct {
	// GitRunner runs git commands. Defaults to defaultGitRunner (shells out
	// to the system `git`). Overridable in tests.
	GitRunner func(ctx context.Context, dir string, args ...string) error

	// BaseURL is the Gitea instance origin, e.g. "https://git.moleculesai.app".
	// Tests point it at a local file:// bare repo (in which case TokenEnv is
	// ignored — file:// has no userinfo auth).
	BaseURL string

	// TokenEnv is the environment variable the PAT is read from at Fetch
	// time. Read lazily (not at construction) so a token rotated into the
	// process env after startup is picked up. Empty disables auth injection
	// (anonymous clone — works for public repos and file:// test repos).
	TokenEnv string

	// LastFetchSHA holds the commit SHA checked out by the last successful
	// Fetch. Mirrors GithubResolver so the install pipeline's drift-seed
	// type-switch can pick it up. Reset on each Fetch.
	LastFetchSHA string
}

// NewGiteaResolver constructs a resolver with platform defaults: the
// canonical Gitea origin and the MOLECULE_TEMPLATE_REPO_TOKEN PAT env.
func NewGiteaResolver() *GiteaResolver {
	base := os.Getenv("MOLECULE_GITEA_BASE_URL")
	if base == "" {
		base = "https://git.moleculesai.app"
	}
	return &GiteaResolver{
		GitRunner: defaultGitRunner,
		BaseURL:   base,
		TokenEnv:  "MOLECULE_TEMPLATE_REPO_TOKEN",
	}
}

// Scheme returns "gitea".
func (r *GiteaResolver) Scheme() string { return "gitea" }

// LastSHA returns the SHA of the last successful Fetch, or "" if Fetch has
// not been called or the last call failed.
func (r *GiteaResolver) LastSHA() string { return r.LastFetchSHA }

// giteaPathRE constrains owner / repo segments. Same grammar as the GitHub
// resolver's owner/repo: start alphanumeric, then [a-zA-Z0-9_.-].
var giteaPathSegRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.\-]{0,99}$`)

// giteaSubpathSegRE constrains each subpath segment. Disallows "..", leading
// "-" (flag injection), and shell metacharacters — only path-safe names.
var giteaSubpathSegRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.\-]{0,127}$`)

// giteaRefRE mirrors GithubResolver's ref grammar: must not start with "-",
// then a bounded set of ref-safe characters.
var giteaRefRE = regexp.MustCompile(`^[a-zA-Z0-9_.][a-zA-Z0-9_./:\-]{0,254}$`)

// parsedGiteaSpec is the decomposition of a gitea spec body.
type parsedGiteaSpec struct {
	owner   string
	repo    string
	subpath string // cleaned, relative, slash-joined; "" when whole-repo
	ref     string // "" when unpinned
}

// parseGiteaSpec decomposes "<owner>/<repo>[/<subpath...>][#<ref>]".
func parseGiteaSpec(spec string) (parsedGiteaSpec, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return parsedGiteaSpec{}, fmt.Errorf("gitea resolver: empty spec")
	}

	var ref string
	if i := strings.Index(spec, "#"); i >= 0 {
		ref = strings.TrimSpace(spec[i+1:])
		spec = spec[:i]
		if ref != "" && !giteaRefRE.MatchString(ref) {
			return parsedGiteaSpec{}, fmt.Errorf("gitea resolver: invalid ref %q", ref)
		}
	}

	segs := strings.Split(strings.Trim(spec, "/"), "/")
	if len(segs) < 2 {
		return parsedGiteaSpec{}, fmt.Errorf(
			"gitea resolver: spec %q must be <owner>/<repo>[/<subpath>][#<ref>]", spec)
	}
	owner, repo := segs[0], segs[1]
	if !giteaPathSegRE.MatchString(owner) || !giteaPathSegRE.MatchString(repo) {
		return parsedGiteaSpec{}, fmt.Errorf("gitea resolver: invalid owner/repo in %q", spec)
	}

	subSegs := segs[2:]
	for _, s := range subSegs {
		if s == "" || s == "." || s == ".." || !giteaSubpathSegRE.MatchString(s) {
			return parsedGiteaSpec{}, fmt.Errorf("gitea resolver: invalid subpath segment %q", s)
		}
	}
	subpath := strings.Join(subSegs, "/")

	return parsedGiteaSpec{owner: owner, repo: repo, subpath: subpath, ref: ref}, nil
}

// cloneURL builds the authenticated clone URL. The PAT (if present) is
// injected into the userinfo component. file:// base URLs are returned as-is
// (no userinfo — local test repos).
func (r *GiteaResolver) cloneURL(owner, repo string) (string, error) {
	base := r.BaseURL
	if base == "" {
		base = "https://git.moleculesai.app"
	}
	path := fmt.Sprintf("/%s/%s.git", owner, repo)

	if strings.HasPrefix(base, "file://") || strings.HasPrefix(base, "/") {
		// Local test repo: BaseURL points at a directory of bare repos.
		return strings.TrimSuffix(base, "/") + path, nil
	}

	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("gitea resolver: invalid base url: %w", err)
	}
	if r.TokenEnv != "" {
		if tok := strings.TrimSpace(os.Getenv(r.TokenEnv)); tok != "" {
			// Gitea accepts the PAT as the username with an empty (or any)
			// password over HTTPS basic auth. url.UserPassword URL-encodes
			// the credential so a token with reserved chars stays valid.
			u.User = url.UserPassword(tok, "x-oauth-basic")
		}
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + path
	return u.String(), nil
}

// Fetch clones the repo at the pinned ref and copies the (optional) subpath
// into dst. Returns the resolved plugin name.
func (r *GiteaResolver) Fetch(ctx context.Context, spec string, dst string) (string, error) {
	p, err := parseGiteaSpec(spec)
	if err != nil {
		return "", err
	}

	if p.ref == "" && os.Getenv("PLUGIN_ALLOW_UNPINNED") != "true" {
		return "", fmt.Errorf("gitea resolver: spec %q requires a pinned ref "+
			"(e.g. %s/%s#main or #v1.0.0); set PLUGIN_ALLOW_UNPINNED=true for local dev",
			spec, p.owner, p.repo)
	}

	runner := r.GitRunner
	if runner == nil {
		runner = defaultGitRunner
	}

	cloneURL, err := r.cloneURL(p.owner, p.repo)
	if err != nil {
		return "", err
	}

	workDir, err := os.MkdirTemp("", "molecule-gitea-clone-*")
	if err != nil {
		return "", fmt.Errorf("gitea resolver: tempdir: %w", err)
	}
	defer os.RemoveAll(workDir)

	cloneTarget := filepath.Join(workDir, "repo")
	args := []string{"clone", "--depth=1"}
	if p.ref != "" {
		args = append(args, "--branch", p.ref)
	}
	args = append(args, "--", cloneURL, cloneTarget)
	if err := runner(ctx, workDir, args...); err != nil {
		// Map "repo/ref doesn't exist" to ErrPluginNotFound (handler → 404).
		// NOTE: the error string may contain the tokenized URL; callers MUST
		// NOT surface resolver errors to clients (the install pipeline logs
		// them server-side and returns a sanitized body). Errors are wrapped
		// with a non-tokenized URL form for the log line.
		safeURL := fmt.Sprintf("%s/%s/%s.git", r.BaseURL, p.owner, p.repo)
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "repository not found") ||
			strings.Contains(msg, "could not find remote branch") ||
			(strings.Contains(msg, "remote branch") && strings.Contains(msg, "not found")) ||
			strings.Contains(msg, "not found") && strings.Contains(msg, "did you run git update-server-info") {
			return "", fmt.Errorf("gitea resolver: %s: %w", safeURL, ErrPluginNotFound)
		}
		return "", fmt.Errorf("gitea resolver: clone %s failed: %w", safeURL, err)
	}

	// Capture the installed SHA before stripping .git (drift-seed parity).
	if shaOut, shaErr := runGitOneLine(ctx, cloneTarget, "rev-parse", "--verify", "HEAD"); shaErr == nil {
		r.LastFetchSHA = strings.TrimSpace(shaOut)
	}

	// The source tree to copy: repo root, or the subpath within it.
	srcTree := cloneTarget
	pluginName := p.repo
	if p.subpath != "" {
		// filepath.Join cleans the path; reject any attempt to escape the
		// clone (defence in depth — parseGiteaSpec already rejected "..").
		joined := filepath.Join(cloneTarget, filepath.FromSlash(p.subpath))
		relCheck, relErr := filepath.Rel(cloneTarget, joined)
		if relErr != nil || strings.HasPrefix(relCheck, "..") {
			return "", fmt.Errorf("gitea resolver: subpath %q escapes repo", p.subpath)
		}
		info, statErr := os.Stat(joined)
		if statErr != nil {
			if os.IsNotExist(statErr) {
				return "", fmt.Errorf("gitea resolver: subpath %q in %s/%s: %w",
					p.subpath, p.owner, p.repo, ErrPluginNotFound)
			}
			return "", fmt.Errorf("gitea resolver: stat subpath: %w", statErr)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("gitea resolver: subpath %q is not a directory", p.subpath)
		}
		srcTree = joined
		// Plugin name is the last subpath segment.
		parts := strings.Split(p.subpath, "/")
		pluginName = parts[len(parts)-1]
	} else {
		// Whole-repo install: strip .git so the plugin dir isn't a nested repo.
		if err := os.RemoveAll(filepath.Join(cloneTarget, ".git")); err != nil {
			return "", fmt.Errorf("gitea resolver: remove .git: %w", err)
		}
	}

	if err := copyTree(ctx, srcTree, dst); err != nil {
		return "", fmt.Errorf("gitea resolver: copy to dst: %w", err)
	}

	return pluginName, nil
}

// ResolveRef resolves a gitea spec's ref to a full commit SHA, so the drift
// sweeper can compare installed vs upstream for gitea:// sources too.
//
// Accepts the same ref shapes as GithubResolver.ResolveRef:
//
//	"<owner>/<repo>[/<subpath>]#tag:vX.Y.Z"
//	"<owner>/<repo>[/<subpath>]#sha:<full>"
//	"<owner>/<repo>[/<subpath>]#<branch>"
func (r *GiteaResolver) ResolveRef(ctx context.Context, spec string) (string, error) {
	p, err := parseGiteaSpec(spec)
	if err != nil {
		return "", err
	}
	if p.ref == "" {
		return "", fmt.Errorf("gitea resolver: ResolveRef requires a ref (got bare %q)", spec)
	}

	runner := r.GitRunner
	if runner == nil {
		runner = defaultGitRunner
	}
	cloneURL, err := r.cloneURL(p.owner, p.repo)
	if err != nil {
		return "", err
	}

	workDir, err := os.MkdirTemp("", "molecule-gitea-resolve-*")
	if err != nil {
		return "", fmt.Errorf("gitea resolver: tempdir: %w", err)
	}
	defer os.RemoveAll(workDir)

	// Normalize the ref into a fetchable git ref.
	fetchRef := p.ref
	switch {
	case strings.HasPrefix(p.ref, "tag:"):
		fetchRef = strings.TrimPrefix(p.ref, "tag:")
	case strings.HasPrefix(p.ref, "sha:"):
		fetchRef = strings.TrimPrefix(p.ref, "sha:")
	}

	// `git -C <workDir> init` then fetch the single ref shallowly.
	if err := runner(ctx, workDir, "init", "-q"); err != nil {
		return "", fmt.Errorf("gitea resolver: git init: %w", err)
	}
	if err := runner(ctx, workDir, "fetch", "--depth=1", "--", cloneURL, fetchRef); err != nil {
		safeURL := fmt.Sprintf("%s/%s/%s.git", r.BaseURL, p.owner, p.repo)
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "repository not found") ||
			strings.Contains(msg, "couldn't find remote ref") ||
			strings.Contains(msg, "could not find remote ref") {
			return "", fmt.Errorf("gitea resolver: %s ref %q: %w", safeURL, fetchRef, ErrPluginNotFound)
		}
		return "", fmt.Errorf("gitea resolver: fetch %s %s failed: %w", safeURL, fetchRef, err)
	}

	sha, err := runGitOneLine(ctx, workDir, "rev-parse", "--verify", "FETCH_HEAD")
	if err != nil {
		return "", fmt.Errorf("gitea resolver: rev-parse FETCH_HEAD: %w", err)
	}
	return strings.TrimSpace(sha), nil
}
