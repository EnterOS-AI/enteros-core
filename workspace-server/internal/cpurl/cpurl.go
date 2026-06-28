// Package cpurl centralizes resolution of the control-plane (CP) base URL.
//
// Why this exists (OSS-standalone, enteros-core mirror):
//
// molecule-core is the OSS core and must run WITHOUT the proprietary
// control plane. The CP integration is already optional at runtime — every
// caller of the CP is gated behind MOLECULE_ORG_ID / CP_UPSTREAM_URL /
// ADMIN_TOKEN and is a no-op in self-host mode. The remaining wart was the
// managed-SaaS endpoint "https://api.moleculesai.app" being hardcoded as a
// last-resort fallback in three separate places (the boot env refresher,
// the CP provisioner, and the concierge model resolver). For a public
// mirror that literal is a vendor endpoint baked into OSS: a self-host
// operator who sets MOLECULE_ORG_ID for their own org but forgets to point
// at their own platform would silently target the managed SaaS.
//
// This package gives core ONE seam for the CP endpoint:
//   - the resolution order is centralized (no more 3 copies to drift), and
//   - a deployment-wide override (MOLECULE_CP_DEFAULT_URL) lets an OSS
//     deployer redirect — or neutralize — the default without editing code.
//
// CP / managed-SaaS behavior is UNCHANGED: when MOLECULE_CP_DEFAULT_URL is
// unset, Base() returns exactly what the previous inline fallbacks returned
// (MOLECULE_CP_URL, else the managed default). Existing tenants and the CP
// see byte-identical behavior.
package cpurl

import (
	"os"
	"strings"
)

// ManagedDefault is the historical compiled-in control-plane endpoint of the
// managed MoleculesAI SaaS. It is the LAST-resort fallback only — reached
// solely when neither an explicit override, MOLECULE_CP_URL, nor
// MOLECULE_CP_DEFAULT_URL is set. Kept as the final default so already-
// provisioned managed tenants (older user-data that only sets
// MOLECULE_ORG_ID) keep working without re-provision.
const ManagedDefault = "https://api.moleculesai.app"

// Base resolves the control-plane base URL using, in order of precedence:
//
//  1. any non-empty explicit override passed by the caller (e.g. the
//     provisioner's CP_PROVISION_URL), in the order supplied;
//  2. MOLECULE_CP_URL — the standard tenant→CP pointer injected at
//     provision time;
//  3. MOLECULE_CP_DEFAULT_URL — a deployment-wide default; this is the OSS
//     seam that lets a self-host operator point core at their own platform
//     (or set it to neutralize the managed endpoint) without code edits;
//  4. ManagedDefault — the compiled-in managed-SaaS endpoint.
//
// The returned value never has a trailing slash trimmed here — callers
// append fixed paths like "/cp/tenants/config"; preserving the exact prior
// string keeps behavior identical to the inline fallbacks this replaced.
func Base(explicit ...string) string {
	for _, e := range explicit {
		if v := strings.TrimSpace(e); v != "" {
			return v
		}
	}
	if v := strings.TrimSpace(os.Getenv("MOLECULE_CP_URL")); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("MOLECULE_CP_DEFAULT_URL")); v != "" {
		return v
	}
	return ManagedDefault
}
