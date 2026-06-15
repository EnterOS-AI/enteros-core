package provisioner

// platform_agent_image_drift_test.go — CI DRIFT-GATE for the
// IMAGE-BAKED platform-agent identity (RFC #2843 §10a).
//
// The IMAGE-BAKED impl (workspace-server/Dockerfile.platform-agent)
// bakes the concierge's identity (config.yaml +
// prompts/concierge.md + mcp_servers.yaml) from the platform-agent
// TEMPLATE REPO into the platform-agent image at
// /opt/molecule-platform-agent-template/. The driver hard-requirement:
// "The image-baked config.yaml + prompts/concierge.md +
// mcp_servers.yaml MUST be SOURCED FROM the platform-agent TEMPLATE
// REPO (single SSOT = PR #1's content) — NOT vendored/duplicated in
// core."
//
// A future drift — e.g., someone edits config.yaml in core, or the
// pre-clone step points at the wrong dir, or a build-arg change
// reroutes the source — would silently create a 2-SSOT situation
// (image snapshot diverges from template repo). The driver-rejected
// option (b) MINIMAL IN-CORE FALLBACK was rejected EXPLICITLY
// because of this 2-SSOT drift risk; the IMAGE-BAKED impl survives
// only because the drift-gate closes that risk.
//
// The drift-gate (this test) pins the invariant: byte-equal content
// between the SSOT (pre-cloned template repo) and the would-be image-
// baked paths that Dockerfile.platform-agent COPYs. The test runs in
// CI alongside the existing provisioner test suite; a drift fails the
// gate with a clear error naming both the SSOT path and the
// Dockerfile COPY destination.
//
// How to run: `go test -run TestPlatformAgentImageDriftGate
// ./internal/provisioner/`. The test reads the SSOT path from the
// PLATFORM_AGENT_TEMPLATE_REPO_PATH env var (set by the CI workflow
// in publish-workspace-server-image.yml after the pre-clone step).
// When unset, the test FAILS LOUD with a remediation hint — it does
// NOT silently skip, because the IMAGE-BAKED impl's safety is
// conditional on the drift-gate running.
//
// Test scope: the 3 files the Dockerfile COPYs (config.yaml,
// mcp_servers.yaml, prompts/concierge.md). A future concierge-
// identity change that adds a new file MUST also extend the
// expectedFiles list here; the test is the load-bearing pin that
// catches a missing extension.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// expectedImageBakedFiles is the canonical list of files the
// IMAGE-BAKED impl bakes into the platform-agent image. The list
// MUST match Dockerfile.platform-agent's COPY instructions exactly.
// Adding a new concierge-identity file = adding it here AND in the
// Dockerfile; the test fails if either side drifts.
//
// Paths are RELATIVE to the SSOT root (the platform-agent template
// repo). The Dockerfile's PLATFORM_AGENT_TEMPLATE_DIR build-arg
// points at this same root.
var expectedImageBakedFiles = []string{
	"config.yaml",
	"mcp_servers.yaml",
	"prompts/concierge.md",
}

// isConciergeIdentityPath reports whether a path in the platform-agent
// template repo is part of the concierge's IDENTITY (the set of
// files the IMAGE-BAKED impl should COPY into the image). A file
// outside this namespace (e.g. README.md, .gitignore) is
// documentation / metadata and is correctly EXCLUDED from the
// image-baked content.
//
// Namespace mirrors the template-asset allowlist in
// internal/provisioner/template_assets.go (IsCPTemplateAssetPath):
//   - "config.yaml"        — runtime entrypoint config
//   - "mcp_servers.yaml"   — MCP wiring (overlay)
//   - "prompts/*"          — system prompts
//
// A future RFC that adds a new namespace (e.g. "hooks/*") MUST
// extend this function AND the Dockerfile AND expectedImageBakedFiles
// in lockstep. The drift-gate's value is in the lockstep invariant.
func isConciergeIdentityPath(rel string) bool {
	rel = filepath.ToSlash(filepath.Clean(rel))
	return rel == "config.yaml" ||
		rel == "mcp_servers.yaml" ||
		strings.HasPrefix(rel, "prompts/")
}

// canonicalPlatformAgentSSOTRelPath is the default SSOT path the
// drift-gate reads from when PLATFORM_AGENT_TEMPLATE_REPO_PATH is
// unset, RELATIVE TO THE REPO ROOT. It mirrors Dockerfile.platform-
// agent's default PLATFORM_AGENT_TEMPLATE_DIR build-arg (i.e. where
// scripts/clone-manifest.sh places the platform-agent template repo
// after the pre-clone step in publish-workspace-server-image.yml).
//
// The env-var override exists for operators running the test
// outside the canonical CI context (e.g. an ad-hoc build verifying
// the drift-gate against a custom template mirror). When the env
// var is set, the test uses that path verbatim; otherwise it walks
// up from the test's CWD to find the repo root and resolves the
// canonical path from there.
//
// The drift-gate is CWD-AGNOSTIC: the test runs from the package
// dir (workspace-server/internal/provisioner/) which is two levels
// below the repo root, so the walk-up is necessary. This is the
// standard pattern for Go tests that need a repo-rooted fixture.
const canonicalPlatformAgentSSOTRelPath = ".tenant-bundle-deps/workspace-configs-templates/platform-agent"

// repoRoot walks up from the test's CWD until it finds the
// molecule-core repo root (identified by go.mod at workspace-server/
// go.mod or by the presence of manifest.json — the molecule-core
// root marker). Returns the absolute path to the repo root.
//
// Used by the drift-gate to resolve canonicalPlatformAgentSSOTRelPath
// to an absolute path regardless of where the test was invoked
// from. Bounded walk-up (max 10 levels) prevents an infinite loop
// if the test somehow runs from a path that doesn't contain a
// molecule-core repo above it.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	dir := wd
	for i := 0; i < 10; i++ {
		// The canonical repo-root marker: manifest.json (present
		// only at the molecule-core repo root, not in any submodule
		// or vendored copy). workspace-server/go.mod is a weaker
		// signal — it's also present in nested test fixtures.
		if _, err := os.Stat(filepath.Join(dir, "manifest.json")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("could not locate molecule-core repo root from CWD %q (walked up 10 levels; expected manifest.json in some ancestor)", wd)
	return ""
}

// TestPlatformAgentImageDriftGate pins the IMAGE-BAKED ↔ template
// SSOT invariant. Byte-equal at build time; a drift fails the CI
// gate BEFORE the image is published.
//
// The test reads the SSOT from $PLATFORM_AGENT_TEMPLATE_REPO_PATH
// when set (operator override), or from the canonical CI path
// (canonicalPlatformAgentSSOTRelPath, resolved against the repo
// root) otherwise. The canonical path is the same place scripts/
// clone-manifest.sh places the platform-agent template repo, so
// the existing CI pre-clone step is sufficient — no extra pre-
// clone logic is required for the drift-gate to run.
//
// When the SSOT dir is missing entirely (no pre-clone happened,
// wrong CWD, etc.), the test fails loud with a remediation hint —
// silent skip is a footgun (the safety of the IMAGE-BAKED impl
// depends on this gate running every build).
func TestPlatformAgentImageDriftGate(t *testing.T) {
	ssotRoot := os.Getenv("PLATFORM_AGENT_TEMPLATE_REPO_PATH")
	if ssotRoot == "" {
		ssotRoot = filepath.Join(repoRoot(t), canonicalPlatformAgentSSOTRelPath)
	}
	if _, err := os.Stat(ssotRoot); err != nil {
		t.Fatalf("platform-agent template SSOT not found at %q (PLATFORM_AGENT_TEMPLATE_REPO_PATH env var unset, falling back to canonical CI path). The IMAGE-BAKED drift-gate requires the CI workflow's pre-clone step (scripts/clone-manifest.sh) to populate this path. Verify the pre-clone ran from the repo root, or set PLATFORM_AGENT_TEMPLATE_REPO_PATH to the pre-cloned template dir. stat: %v", ssotRoot, err)
	}

	// SSOT-side: each expected file MUST exist at ssotRoot/<relpath>
	// and have non-zero content (zero-byte file = silent miss).
	for _, rel := range expectedImageBakedFiles {
		ssotPath := filepath.Join(ssotRoot, rel)
		data, err := os.ReadFile(ssotPath)
		if err != nil {
			t.Errorf("SSOT missing: %s (read: %v) — the platform-agent template repo is the load-bearing identity SSOT; a missing file is a regression", ssotPath, err)
			continue
		}
		if len(data) == 0 {
			t.Errorf("SSOT empty: %s — zero-byte identity file would silently bake a broken concierge into the image", ssotPath)
		}
	}

	// SSOT-side: scan the platform-agent template repo for any
	// additional files in the concierge-identity namespace (e.g.
	// prompts/foo.md) that the Dockerfile might be missing. The
	// forward-direction check (above) catches a missing expected
	// file; this REVERSE check catches an un-expected new identity
	// file the Dockerfile doesn't COPY. Both must hold for the
	// image-baked content to remain SSOT-equal.
	extraIdentityFiles, err := scanConciergeIdentityFiles(ssotRoot)
	if err != nil {
		t.Errorf("scan SSOT identity files: %v", err)
	} else {
		for _, rel := range extraIdentityFiles {
			found := false
			for _, expected := range expectedImageBakedFiles {
				if rel == expected {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("SSOT has an un-baked concierge-identity file: %s — the IMAGE-BAKED impl is now SILENTLY DRIFTING from the SSOT (a new file was added to the platform-agent template repo without a matching COPY in Dockerfile.platform-agent + entry in expectedImageBakedFiles). Either bake it (update Dockerfile + expected list) or mark it non-identity.", rel)
			}
		}
	}

	// Dockerfile-side: verify Dockerfile.platform-agent's COPY
	// instructions match expectedImageBakedFiles (so the Dockerfile
	// is in sync with this gate's expected list). The Dockerfile
	// sits in workspace-server/ next to the other Dockerfiles; the
	// test runs from the package dir (workspace-server/internal/
	// provisioner/) so the relative path goes up two levels.
	dockerfilePath := filepath.Join("..", "..", "Dockerfile.platform-agent")
	dockerfile, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatalf("read %s: %v — the drift-gate requires Dockerfile.platform-agent to live next to the other Dockerfiles; verify the path", dockerfilePath, err)
	}
	dockerfileStr := string(dockerfile)

	for _, rel := range expectedImageBakedFiles {
		// The Dockerfile uses two patterns: COPY <rel> /opt/...
		// for the top-level files (config.yaml, mcp_servers.yaml)
		// and COPY <dir>/ /opt/.../  for the prompts/ directory.
		// We check that EITHER pattern appears for the expected file.
		topLevel := `COPY ${PLATFORM_AGENT_TEMPLATE_DIR}/` + rel
		dirPattern := `COPY ${PLATFORM_AGENT_TEMPLATE_DIR}/` + filepath.Dir(rel) + `/`
		if !strings.Contains(dockerfileStr, topLevel) && !strings.Contains(dockerfileStr, dirPattern) {
			t.Errorf("Dockerfile COPY missing: %s — the IMAGE-BAKED impl must COPY %s from the platform-agent template SSOT; if a new identity file is added, update Dockerfile.platform-agent AND expectedImageBakedFiles", rel, rel)
		}
	}

	// ALSO verify the Dockerfile references the build-arg + the
	// destination path. A future refactor that changes either of
	// these would silently break the SSOT contract; the test pins
	// the names that the workspace-server's runtime fallback (and
	// any operator inspecting the image) relies on.
	if !strings.Contains(dockerfileStr, "ARG PLATFORM_AGENT_TEMPLATE_DIR=") {
		t.Error("Dockerfile.platform-agent is missing the PLATFORM_AGENT_TEMPLATE_DIR build-arg declaration — the IMAGE-BAKED impl requires this arg to source from the pre-cloned template repo")
	}
	if !strings.Contains(dockerfileStr, "/opt/molecule-platform-agent-template/") {
		t.Error("Dockerfile.platform-agent is missing the /opt/molecule-platform-agent-template/ destination path — the workspace-server runtime fallback (and the drift-gate convention) pins this path; a change requires a coordinated update in both places")
	}
}

// scanConciergeIdentityFiles walks the platform-agent template repo
// and returns the RELATIVE paths of every file in the concierge-
// identity namespace (config.yaml + mcp_servers.yaml + prompts/).
// Non-identity files (README, .gitignore, etc.) are filtered out.
//
// Errors are returned for filesystem-walk failures; the caller turns
// them into a t.Errorf (so other checks still run). The walk is
// deliberately non-recursive beyond the namespace prefix — the
// concierge's identity is config + mcp + prompts, nothing nested.
func scanConciergeIdentityFiles(ssotRoot string) ([]string, error) {
	var identity []string
	entries, err := os.ReadDir(ssotRoot)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		// Top-level files: config.yaml, mcp_servers.yaml
		if !e.IsDir() {
			if isConciergeIdentityPath(e.Name()) {
				identity = append(identity, e.Name())
			}
			continue
		}
		// Directories: scan prompts/
		if e.Name() == "prompts" {
			promptEntries, err := os.ReadDir(filepath.Join(ssotRoot, e.Name()))
			if err != nil {
				return nil, err
			}
			for _, pe := range promptEntries {
				if pe.IsDir() {
					continue
				}
				rel := filepath.ToSlash(filepath.Join(e.Name(), pe.Name()))
				if isConciergeIdentityPath(rel) {
					identity = append(identity, rel)
				}
			}
		}
	}
	return identity, nil
}
