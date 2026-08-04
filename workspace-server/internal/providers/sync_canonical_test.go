package providers

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// sync_canonical_test.go — hermetic drift backstop pinning the SSOT registry
// the binary embeds (internal#718 P2-A).
//
// The provider/model/runtime registry SSOT is the SDK: core no longer keeps its
// own providers.yaml copy — `embeddedYAML` is sourced from
// go.moleculesai.app/sdk/gen/go/llmregistry (embedded from the SDK's
// contracts/llm-registry/llm-registry.yaml). This test pins the sha256 of that
// embedded registry so an UNEXPECTED change to the SDK content the binary links
// (a bad module bump, a local replace pointing at drifted content) flips red
// locally and in `go test ./...` — a hermetic check that needs no network.
//
// When the SDK registry legitimately changes, the procedure is:
//  1. Bump the go.moleculesai.app/sdk/gen/go dependency to the version carrying
//     the new registry (or update the local replace).
//  2. Update canonicalRegistrySHA256 below to the new sha (the failure message
//     prints the observed sha to paste in).
// The deliberate constant bump is the human checkpoint that a registry change
// was consciously adopted from the SDK SSOT, not silently pulled in.

// canonicalRegistrySHA256 is the sha256 of the SDK llm-registry.yaml the binary
// embeds via llmregistry.RawYAML. Bumped deliberately on each SDK registry
// adoption (see file doc).
// 2026-08-04 adoption: molecule-ai-sdk#203 (merged 1426a986) narrowed every
// runtime's `platform` arm to minimax-only after the platform's Moonshot vendor
// account was suspended for non-payment (moonshot/* platform calls 429 in
// ~200ms, 100% of the time). Adopted here so core's create-time
// validateRegisteredModelForRuntime and the canvas menu see the withdrawal.
const canonicalRegistrySHA256 = "42f891573505c410dcef459bae20d615036226076544ae9e6fd58eb52d716254"

func TestSyncedYAMLMatchesCanonicalSHA(t *testing.T) {
	sum := sha256.Sum256(embeddedYAML)
	got := hex.EncodeToString(sum[:])
	if got != canonicalRegistrySHA256 {
		t.Fatalf("embedded SDK registry sha256 = %s, pinned = %s\n"+
			"If you intentionally adopted a new SDK registry (bumped "+
			"go.moleculesai.app/sdk/gen/go or its replace), update "+
			"canonicalRegistrySHA256 to %s.\n"+
			"If you did NOT expect the embedded registry to change, check the SDK "+
			"dependency — the canonical SSOT is the SDK's llm-registry.yaml, which "+
			"core derives from (it keeps no local copy).",
			got, canonicalRegistrySHA256, got)
	}
}
