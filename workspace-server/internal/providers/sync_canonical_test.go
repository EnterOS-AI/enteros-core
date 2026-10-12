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
// 2026-08-05 adoption: molecule-ai-sdk#204 (merged fb755048) CORRECTS the
// 2026-08-04 #203 narrowing. #203 had collapsed every runtime's `platform` arm
// to minimax-only after the platform's Moonshot vendor account was suspended
// for non-payment (moonshot/* platform calls 429 in ~200ms, 100% of the time),
// but it took five HEALTHY ids down with the two dead ones. #204 restores them
// to `models` — anthropic/claude-opus-4-7, anthropic/claude-opus-4-8 and
// anthropic/claude-sonnet-4-6 on the claude-code arm; openai/gpt-5.4 and
// openai/gpt-5.4-mini on the codex arm (all five probed HTTP 200 through the
// metered proxy) — and moves ONLY moonshot/kimi-k2.6 and moonshot/kimi-k2.5
// into the new `withdrawn_models` key. Adopted here so core's create-time
// validateRegisteredModelForRuntime and the canvas menu offer the restored ids
// again: under #203 they were healed for EXISTING pins but not re-selectable
// for new workspaces.
//
// NOTE on `withdrawn_models`: it is a NEW key in the SDK registry schema that
// this loader does not model. parseManifest uses non-strict yaml.Unmarshal, so
// the key is silently IGNORED here — adopting #204 is therefore safe (no parse
// break), but core does NOT yet implement #204's "unselectable but still OWNED"
// semantic for withdrawn ids. Under this pin a workspace already pinned to a
// moonshot/* platform id behaves exactly as it did under #203. Closing that gap
// is a separate change to internal/providers (see the SDK #204 arm comment).
const canonicalRegistrySHA256 = "3fbf55ca1417392edcf87b01f6e5eec309ea46ed1734c532dc802596964bd2c4"

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
