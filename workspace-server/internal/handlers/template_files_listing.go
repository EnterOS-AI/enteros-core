package handlers

// Files-API listing: path resolution, containment, and honest "absent" vs
// "empty" reporting (core#4341).
//
// BACKGROUND. `GET /workspaces/:id/files` answers from one of four backends:
// the EIC tunnel (SaaS EC2), a docker-exec `find` in the workspace container,
// the docker-less HOST-SIDE MIRROR (#206 molecules-server), or the host-side
// template dir. Only the first two see the live container filesystem. The
// mirror carries ONLY the CP-delivered config bundle (config.yaml + prompts/*)
// — `plugins/`, `skills/` and the runtime dotfiles are created by the runtime
// INSIDE the container and are never mirrored.
//
// Pre-fix, every "I cannot see that here" condition was reported as `200 []`,
// which is wire-identical to "this directory exists and is empty". An operator
// listing a live tenant read `path=plugins -> []` as "no plugins installed"
// when the container in fact had 8. The empty array is the defect: it is a
// FALSE NEGATIVE, not a cosmetic one.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Provenance header. A listing served from the partial host-side mirror is
// otherwise wire-identical to one served from the live container, so a caller
// has no way to tell a complete listing from a config-bundle-only one. The
// header is additive — the body stays a plain JSON array, so existing clients
// (canvas useFilesApi) are unaffected.
const (
	filesSourceHeader     = "X-Molecule-Files-Source"
	filesSourceEIC        = "eic"
	filesSourceContainer  = "container"
	filesSourceHostMirror = "host-mirror"
	filesSourceTemplate   = "template-dir"
	filesSourceNone       = "none"
)

// errListPathAbsent signals the requested ?path= does not exist under the
// resolved backend root — a 404, distinct from an escape attempt (400).
var errListPathAbsent = errors.New("list path absent")

// errListPathEscapes signals the requested ?path= resolved outside the root,
// lexically or after symlink evaluation — always a 400, never downgraded to
// the 404 above, so an escape attempt cannot hide as an ordinary miss.
var errListPathEscapes = errors.New("list path escapes root")

// errListPathIsFile signals ?path= names a regular file. Walking a file yields
// zero entries, so pre-fix this was another `200 []`.
var errListPathIsFile = errors.New("list path is a file")

// isAbsPathPortable reports whether p would be absolute on ANY supported
// platform, not just the one this binary was built for.
//
// WHY NOT filepath.IsAbs. filepath.IsAbs is GOOS-dependent: on Windows
// `IsAbs("/etc/passwd")` is FALSE, so the shared validateRelPath guard silently
// admits a POSIX-absolute path when the test suite (or a dev box) runs on
// Windows, while production — Linux — rejects it. That divergence makes every
// absolute-path containment test vacuous on Windows: it asserts a rejection
// that the platform, not the code, was responsible for. Judge the path by its
// shape instead so the guard and its tests mean the same thing everywhere.
func isAbsPathPortable(p string) bool {
	if p == "" {
		return false
	}
	if p[0] == '/' || p[0] == '\\' {
		return true
	}
	// Drive-letter forms: C:\dir, C:/dir, C:dir.
	if len(p) >= 2 && p[1] == ':' {
		c := p[0]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			return true
		}
	}
	return filepath.IsAbs(p)
}

// validateListSubPath validates the ?path= query for a listing.
//
// DECISION — absolute ?path= stays REJECTED. Two reasons:
//
//  1. `?root=` already selects the root (/configs, /workspace, ...), so an
//     absolute `?path=` is ambiguous by construction: `?root=/workspace
//     &path=/configs` states two different roots and there is no non-arbitrary
//     reading of it. Silently resolving `/configs/plugins` against root
//     /configs would also have to decide whether a leading `/configs` is the
//     root repeated or a literal child directory named `configs`.
//  2. validateRelPath is the SHARED guard for ListFiles, ReadFile, WriteFile
//     and DeleteFile (docs/pages/api/workspace-files.mdx). Loosening it for
//     listing alone would leave two different path grammars on one endpoint
//     family — which is precisely where path-escape bugs are introduced.
//
// What was wrong was the MESSAGE, not the rejection: a bare "invalid path"
// does not tell the caller that the API is already rooted at /configs, so
// `/configs/plugins` is a doubled root rather than a typo. The returned
// message names the root and the relative form expected.
func validateListSubPath(rootPath, subPath string) error {
	if subPath == "" {
		return nil
	}
	if isAbsPathPortable(subPath) {
		return fmt.Errorf(
			"invalid path %q: ?path= must be RELATIVE to the workspace root, which is already %s — drop the leading separator (e.g. path=plugins or path=prompts/concierge.md, not path=%s/plugins). Use ?root= to select a different root",
			subPath, rootPath, strings.TrimSuffix(rootPath, "/"))
	}
	if err := validateRelPath(subPath); err != nil {
		// validateRelPath distinguishes traversal, dot-only and the .hermes
		// private-state denial; surface which one applied instead of
		// collapsing all three into "invalid path".
		return fmt.Errorf("invalid path %q under root %s: %v", subPath, rootPath, err)
	}
	return nil
}

// resolveListWalkRoot resolves subPath under dir and returns the directory to
// walk, guaranteeing containment both lexically and physically.
//
// CONTAINMENT PRESERVED. The lexical half delegates to the existing
// resolveInsideRoot helper (org_helpers.go) — the repo's established
// resolve-inside-root primitive, covered by the TestResolveInsideRoot_* suites
// — so this does not introduce a second, divergent containment rule.
//
// CONTAINMENT ADDED. resolveInsideRoot is purely lexical, and the walk root is
// then handed to filepath.Walk. A symlink INSIDE the root whose target is
// outside passes every lexical check, and filepath.Walk resolves the walk root
// through it: the walker then lists the TARGET's contents. The pre-existing
// OFFSEC-010 skip in walkConfigTree only drops symlink ENTRIES encountered
// DURING the walk — it never stops the walk ROOT itself from being (or
// traversing) a symlink. Template dirs are fetched from template repos and git
// preserves symlinks, so a template carrying `data -> /` made the host
// filesystem listable. EvalSymlinks on both sides closes that, and because it
// evaluates the whole path it also covers a symlinked INTERMEDIATE component
// (`?path=escape/sub`), which an Lstat of only the final element would miss.
func resolveListWalkRoot(dir, subPath string) (string, error) {
	if subPath == "" {
		return dir, nil
	}
	joined, err := resolveInsideRoot(dir, subPath)
	if err != nil {
		return "", fmt.Errorf("%w: %v", errListPathEscapes, err)
	}

	// Physical containment. Evaluate the root first: it must exist for the
	// comparison to mean anything.
	realRoot, err := filepath.EvalSymlinks(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", errListPathAbsent
		}
		return "", fmt.Errorf("%w: resolve root: %v", errListPathEscapes, err)
	}
	realTarget, err := filepath.EvalSymlinks(joined)
	if err != nil {
		if os.IsNotExist(err) {
			// Genuinely missing — a 404, not an escape.
			return "", errListPathAbsent
		}
		return "", fmt.Errorf("%w: resolve target: %v", errListPathEscapes, err)
	}
	if realTarget != realRoot && !strings.HasPrefix(realTarget, realRoot+string(filepath.Separator)) {
		return "", errListPathEscapes
	}

	// A regular file has no children; walking one returns zero entries, which
	// pre-fix surfaced as yet another indistinguishable `200 []`.
	fi, err := os.Lstat(realTarget)
	if err != nil {
		if os.IsNotExist(err) {
			return "", errListPathAbsent
		}
		return "", fmt.Errorf("%w: stat target: %v", errListPathEscapes, err)
	}
	if !fi.IsDir() {
		return "", errListPathIsFile
	}
	return realTarget, nil
}

// listPathNotFoundMessage explains an absent ?path= without leaking host
// paths. It names the ROOT and the RELATIVE subpath the caller asked for, plus
// the backend consulted, so "not found in the mirror" is distinguishable from
// "does not exist in the workspace" — the distinction whose absence caused the
// operator's wrong conclusion.
func listPathNotFoundMessage(rootPath, subPath, source string) string {
	switch source {
	case filesSourceHostMirror:
		return fmt.Sprintf(
			"path %q not found under %s. This workspace is served from the host-side config mirror, which carries ONLY the delivered config bundle (config.yaml, prompts/*) — runtime-created directories such as plugins/, skills/ and .molecule/ exist in the container but are NOT mirrored, so their absence here does not mean they are absent from the workspace",
			subPath, rootPath)
	case filesSourceTemplate:
		return fmt.Sprintf(
			"path %q not found under %s. This workspace is served from the host-side TEMPLATE dir (no container and no config mirror available), which reflects the seed template, not the workspace's live state",
			subPath, rootPath)
	default:
		return fmt.Sprintf(
			"path %q not found under %s: no file backend is available for this workspace (no container, no config mirror, no matching template)",
			subPath, rootPath)
	}
}
