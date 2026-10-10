package provisioner

// brand_seam.go — the ONE place this package turns a Docker container name
// into a workspace-id prefix, and the ONE place it decides which name shapes
// count as "a workspace container".
//
// # WHY THIS FILE EXISTS
//
// The orphan sweeper's three passes all key on the container-name → workspace-id
// derivation below. Until this seam existed the derivation was a bare
// `strings.HasPrefix(name, "ws-")` inlined at three call sites, keyed on a
// SINGLE token. That is a false-orphan hazard, because `ws-<id>` is not the only
// workspace container shape that shows up on the daemon this provisioner talks
// to:
//
//   - This package mints `ws-<workspace-uuid>` (see ContainerName).
//   - The control plane's local-docker backend mints
//     `<brand>-ws-<tenant>-<short>` on the SAME host daemon — historically
//     `mol-ws-…`, and, after the Enter OS rebrand flipped
//     branding.ResourcePrefix, `enteros-ws-…`. Both generations co-exist: a
//     freshly-minted `enteros-ws-*` workspace container routinely sits inside a
//     `mol-*` per-tenant network, so "one prefix per org" is NOT true and no
//     matcher may assume it.
//
// A `ws-`-only matcher goes blind to every brand-prefixed container. The
// sharpest consequence is in sweepStaleTokensWithoutContainer: a live workspace
// whose container is named `enteros-ws-…` looks like "workspace with no live
// container", so its auth token is revoked and the workspace wedges on 401 —
// a live tenant classified as an orphan. The wiped-DB pass has the mirror bug
// (a genuine orphan stays invisible and leaks forever).
//
// # THE SEAM
//
// The brand token is NOT owned here. It is owned by the neutral SDK SSOT
// go.moleculesai.app/sdk/gen/go/branding, which molecule-core (OSS) and the
// proprietary control plane both import — so neither repo depends on the other
// and neither can drift. Constructors mint with the current brand;
// MATCHERS must accept the UNION branding.AllResourcePrefixes() returns
// (current brand first, then every legacy brand, forever), or a rebrand
// false-orphans the still-running previous generation.
//
// Every matcher below is a pure `…For(brands)` / `…In(name, brands)` builder
// plus a thin wrapper bound to branding.AllResourcePrefixes(). The pure form
// exists so tests can drive a single-brand list and PROVE the blindness failure
// mode is real (negative control) rather than merely observing that the current
// brand matches itself. See brand_seam_test.go.

import (
	"strings"

	"go.moleculesai.app/sdk/gen/go/branding"
)

// wsInfix is the brand-free descriptor that separates the brand token from the
// workspace body in a control-plane-minted container name
// (`<brand>-ws-<tenant>-<short>`). It is generic, not branded, so it survives
// any rebrand untouched — only the token in front of it changes.
const wsInfix = "-ws-"

// workspaceNamePrefixesFor is the pure builder behind
// WorkspaceContainerNamePrefixes: the brand-free `ws-` prefix this package
// mints with, followed by `<brand>-ws-` for every supplied brand token, in the
// order given.
//
// The brand-free arm is listed FIRST and is never derived from a brand token:
// containers this package creates carry no brand at all, and must keep matching
// no matter what happens to branding.ResourcePrefix.
func workspaceNamePrefixesFor(brands []string) []string {
	out := make([]string, 0, len(brands)+1)
	out = append(out, containerNamePrefix)
	for _, b := range brands {
		out = append(out, b+wsInfix)
	}
	return out
}

// WorkspaceContainerNamePrefixes returns every container-name prefix that
// identifies a workspace container on this daemon: the brand-free `ws-` form
// this package mints, plus `<brand>-ws-` for the current brand and EVERY legacy
// brand (branding.AllResourcePrefixes()).
//
// Today that is exactly {"ws-", "enteros-ws-", "mol-ws-"} — pinned as literals
// in TestGolden_WorkspaceContainerPrefixes.
func WorkspaceContainerNamePrefixes() []string {
	return workspaceNamePrefixesFor(branding.AllResourcePrefixes())
}

// parseWorkspaceContainerIDIn extracts the workspace-id prefix carried by a
// container name, for any of the supplied brand tokens. ok is false when the
// name is not a workspace container of ANY recognised shape.
//
// The returned string is contractually a LEADING SUBSTRING OF `workspaces.id::text`
// (the dashed UUID), because every caller turns it straight into a SQL LIKE
// pattern (`prefix || '%'`) against that column. The two name families reach
// that contract differently:
//
//   - `ws-<body>`: <body> is this package's own ContainerName payload — the
//     full dashed UUID (or, for pre-KI-013 containers, its first 12 characters,
//     dashes included). Returned VERBATIM, byte-for-byte as before this seam
//     existed.
//   - `<brand>-ws-[<tenant>-]<short>`: <short> is the control plane's
//     dash-STRIPPED 12-hex id (wsname.Short). Returned dashes-reinserted at the
//     canonical UUID offsets, since `abc123def456` is NOT a prefix of
//     `abc123de-f456-…` but `abc123de-f456` is. Skipping this step would make
//     the whole union cosmetic: the name would be recognised and then never
//     match a row.
//
// Matching is case-sensitive and the name is used as given (minus the Docker
// API's leading slash), preserving the previous behaviour exactly — Docker
// lower-cases nothing for us and normalising here would silently change the
// LIKE patterns the sweeper builds.
func parseWorkspaceContainerIDIn(name string, brands []string) (string, bool) {
	n := strings.TrimPrefix(strings.TrimSpace(name), "/")

	// Brand arms first. They cannot collide with the brand-free arm — a name
	// starting with `<brand>-ws-` never starts with `ws-` — but the specific
	// shapes are tested first so the ordering stays obviously safe if a future
	// brand token ever ends in something that changes that.
	for _, b := range brands {
		p := b + wsInfix
		if !strings.HasPrefix(n, p) {
			continue
		}
		body := n[len(p):]
		if body == "" {
			return "", false
		}
		// `<tenant>-<short>` or a bare `<short>`: the short is always the
		// trailing dash-free run (tenant slugs may themselves be hyphenated).
		short := body
		if i := strings.LastIndex(body, "-"); i >= 0 {
			short = body[i+1:]
		}
		if short == "" {
			return "", false
		}
		return dashedUUIDPrefix(short), true
	}

	if !strings.HasPrefix(n, containerNamePrefix) {
		return "", false
	}
	id := n[len(containerNamePrefix):]
	if id == "" {
		return "", false
	}
	return id, true
}

// parseWorkspaceContainerID is parseWorkspaceContainerIDIn bound to the live
// brand union — what production runs with.
func parseWorkspaceContainerID(name string) (string, bool) {
	return parseWorkspaceContainerIDIn(name, branding.AllResourcePrefixes())
}

// uuidDashOffsets are the character positions of the four hyphens in a
// canonical 8-4-4-4-12 UUID, counted in the DASH-STRIPPED string.
var uuidDashOffsets = [...]int{8, 12, 16, 20}

// dashedUUIDPrefix re-inserts the canonical UUID hyphens into a dash-stripped
// id prefix, so the result is a leading substring of the dashed `id::text`
// stored in Postgres. Input shorter than the first offset is returned
// unchanged; an input that already contains a dash is returned unchanged
// (nothing to reconstruct, and rewriting it could only corrupt it).
func dashedUUIDPrefix(short string) string {
	if len(short) <= uuidDashOffsets[0] || strings.Contains(short, "-") {
		return short
	}
	var b strings.Builder
	b.Grow(len(short) + len(uuidDashOffsets))
	prev := 0
	for _, off := range uuidDashOffsets {
		if len(short) <= off {
			break
		}
		b.WriteString(short[prev:off])
		b.WriteByte('-')
		prev = off
	}
	b.WriteString(short[prev:])
	return b.String()
}
