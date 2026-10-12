package provisioner

import (
	"fmt"
	"os"
	"strings"
)

// defaultRegistryPrefix is the current Molecule Gitea container registry.
// Named runtimes in an unset-registry self-host still use Resolve()'s
// local-build path; this fallback covers legacy callers and explicit image
// references without sending them to the retired GitHub registry.
const defaultRegistryPrefix = "registry.moleculesai.app/molecule-ai"

// knownRuntimes is the canonical list of workspace template runtimes shipped
// in main. Any runtime added here MUST also have a standalone template repo
// (molecule-ai/molecule-ai-workspace-template-<name>) and an entry in the
// publish-template-image workflow that builds it.
//
// Order matters for deterministic test snapshots; keep alphabetical.
var knownRuntimes = []string{
	"claude-code",
	"codex",
	"hermes",
	"openclaw",
}

// defaultRuntimeFallback is the compiled-in fallback when a workspace's config
// doesn't specify a runtime AND the MOLECULE_DEFAULT_RUNTIME env override is
// unset/empty/unknown. It is also the OSS default for self-hosted operators who
// run without the platform KMS-injected override.
const defaultRuntimeFallback = "hermes"

// defaultRuntime resolves the platform default runtime, honoring the
// MOLECULE_DEFAULT_RUNTIME env override (KMS SSOT, injected at deploy time)
// over the compiled-in defaultRuntimeFallback const.
//
// The env override remains the SSOT for managed deployments; this compiled
// fallback is the local/self-host default when no operator value is present.
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
// SSOT instead of baking a second runtime literal.
func DefaultRuntime() string {
	return defaultRuntime()
}

// RegistryPrefix returns the registry prefix for workspace-template image
// references. It defaults to Molecule's Gitea registry and is overridden by
// MOLECULE_IMAGE_REGISTRY for an operator-controlled mirror.
//
// The override is deployment configuration, never request input, so the value
// is trusted by the time it reaches this code. Validation is deliberately
// minimal: a prefix that points at a registry the host cannot authenticate to
// will fail
// loudly at docker-pull time, which is the right blast radius.
//
// Example values:
//
//	(unset)                                     → registry.moleculesai.app/molecule-ai
//	"registry.example.com/acme"                → operator-controlled mirror
//
// Auth is registry-specific and configured outside this function.
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
//	"registry.moleculesai.app/molecule-ai" → "registry.moleculesai.app"
//	"registry.example.com/acme"            → "registry.example.com"
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
// SHA-pinned references for managed launches are applied by CP
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
