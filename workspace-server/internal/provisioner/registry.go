package provisioner

import (
	"fmt"
	"os"
	"strings"
)

// defaultRegistryPrefix is the upstream OSS face for all workspace template
// images. Self-hosted Molecule deployments without the MOLECULE_IMAGE_REGISTRY
// override pull from here.
const defaultRegistryPrefix = "ghcr.io/molecule-ai"

// knownRuntimes is the canonical list of workspace template runtimes shipped
// in main. Any runtime added here MUST also have a standalone template repo
// (Molecule-AI/molecule-ai-workspace-template-<name>) and an entry in the
// publish-template-image workflow that builds it.
//
// Order matters for deterministic test snapshots; keep alphabetical.
var knownRuntimes = []string{
	"claude-code",
	"codex",
	"google-adk",
	"hermes",
	"openclaw",
}

// defaultRuntimeFallback is the compiled-in fallback when a workspace's config
// doesn't specify a runtime AND the MOLECULE_DEFAULT_RUNTIME env override is
// unset/empty/unknown. It is also the OSS default for self-hosted operators who
// run without the platform KMS-injected override.
const defaultRuntimeFallback = "claude-code"

// defaultRuntime resolves the platform default runtime, honoring the
// MOLECULE_DEFAULT_RUNTIME env override (KMS SSOT, injected at deploy time)
// over the compiled-in defaultRuntimeFallback const.
//
// De-hardcode (behavior-neutral): the override's staging KMS value equals the
// old literal ("claude-code"), and unset/local falls back to the same literal,
// so no behavior changes today. A later platform-default flip is a separate,
// deliberate KMS edit — not a code change.
//
// FAIL CLOSED on an unknown override: the resolved runtime MUST be in the
// canonical knownRuntimes allowlist (IsKnownRuntime). An override that names a
// runtime with no template repo / publish entry would otherwise resolve to a
// DefaultImage with the wrong deps; we refuse it and fall back to the known
// compiled-in default rather than provision an unknown runtime.
func defaultRuntime() string {
	if v := os.Getenv("MOLECULE_DEFAULT_RUNTIME"); v != "" {
		if IsKnownRuntime(v) {
			return v
		}
		// Unknown override — do NOT provision an unknown runtime. Fall back to
		// the compiled-in known default (which always passes IsKnownRuntime).
		fmt.Fprintf(os.Stderr,
			"provisioner: MOLECULE_DEFAULT_RUNTIME=%q is not a known runtime; falling back to %q\n",
			v, defaultRuntimeFallback)
	}
	return defaultRuntimeFallback
}

// DefaultRuntime is the exported SSOT accessor for the platform default runtime.
// It resolves the MOLECULE_DEFAULT_RUNTIME env override (KMS SSOT, injected at
// deploy time) over the compiled-in defaultRuntimeFallback, fail-closed on an
// unknown override (see defaultRuntime). Lower-level callers outside this
// package (e.g. internal/bundle) use this to FOLLOW the one platform-runtime
// SSOT instead of baking a second "claude-code" literal. Behavior-neutral today:
// the prod/staging KMS value equals the fallback, and unset/local resolves to it.
func DefaultRuntime() string {
	return defaultRuntime()
}

// RegistryPrefix returns the registry prefix all workspace-template image
// references should use. Defaults to ghcr.io/molecule-ai (the upstream OSS
// face) and is overridden by the MOLECULE_IMAGE_REGISTRY env var in
// production tenants where we mirror images to a private registry.
//
// The override is set at deploy time (Railway env, EC2 user-data) — never
// from user-supplied input — so the value is trusted by the time it reaches
// this code. Validation is deliberately minimal: an operator-supplied
// prefix that points at a registry the EC2 can't authenticate to will fail
// loudly at docker-pull time, which is the right blast radius.
//
// Example values:
//
//	(unset)                                              → ghcr.io/molecule-ai (OSS default)
//	"123456789012.dkr.ecr.us-east-2.amazonaws.com/molecule-ai"  → AWS ECR mirror
//	"git.moleculesai.app/molecule-ai"                    → self-hosted Gitea Container Registry (future)
//
// Auth is registry-specific and configured outside this function:
//   - GHCR: GHCR_USER/GHCR_TOKEN env vars consumed by ghcrAuthHeader()
//   - ECR: docker credential helper (amazon-ecr-credential-helper) configured
//     in EC2 user-data; ~/.docker/config.json has credHelpers entry; the
//     daemon resolves auth automatically on every pull.
func RegistryPrefix() string {
	if v := os.Getenv("MOLECULE_IMAGE_REGISTRY"); v != "" {
		return v
	}
	return defaultRegistryPrefix
}

// RegistryHost returns just the registry host portion of RegistryPrefix() —
// i.e. everything before the first "/" separator. This is the value that
// belongs in:
//
//   - Docker Engine PullOptions.RegistryAuth payloads (`serveraddress` field)
//     — the engine matches credentials against host, not host+org-path.
//   - Docker Registry V2 HTTP API base URLs (e.g. `https://<host>/v2/...`)
//     — the V2 API is host-rooted; the org-path lives in the manifest path.
//
// Examples:
//
//	"ghcr.io/molecule-ai"                                    → "ghcr.io"
//	"123456789012.dkr.ecr.us-east-2.amazonaws.com/molecule-ai" → "123456789012.dkr.ecr.us-east-2.amazonaws.com"
//	"git.moleculesai.app/molecule-ai"                        → "git.moleculesai.app"
//
// If RegistryPrefix() ever returns a bare host (no `/`), we return it as-is
// rather than letting strings.SplitN produce an empty string — defensive
// against a misconfiguration where the operator sets just the host.
func RegistryHost() string {
	prefix := RegistryPrefix()
	if i := strings.IndexByte(prefix, '/'); i > 0 {
		return prefix[:i]
	}
	return prefix
}

// RuntimeImage returns the canonical image reference for the given runtime,
// using the current RegistryPrefix() and the moving `:latest` tag.
//
// SHA-pinned references for production thin-AMI launches are applied by CP
// (molecule-controlplane) at its provisioner layer using CP's
// migrations/027_runtime_image_pins table, which is the single SSOT for
// runtime image pins. The local digest-pin reader that previously lived at
// handlers/runtime_image_pin.go was retired by RFC internal#617 / task #335
// (it never had a writer; the table was always empty so the reader hit
// sql.ErrNoRows and fell through to :latest on every provision).
//
// Returns the empty string for unknown runtimes; callers should fall through
// to DefaultImage in that case (matching legacy behavior).
func RuntimeImage(runtime string) string {
	for _, r := range knownRuntimes {
		if r == runtime {
			return fmt.Sprintf("%s/workspace-template-%s:latest", RegistryPrefix(), runtime)
		}
	}
	return ""
}

// computeRuntimeImages returns the {runtime: image-ref} map evaluated against
// the current RegistryPrefix(). Called at package init to populate the
// exported RuntimeImages var. Tests that flip MOLECULE_IMAGE_REGISTRY between
// expected values use this helper to rebuild the map mid-run.
func computeRuntimeImages() map[string]string {
	out := make(map[string]string, len(knownRuntimes))
	for _, r := range knownRuntimes {
		out[r] = RuntimeImage(r)
	}
	return out
}
