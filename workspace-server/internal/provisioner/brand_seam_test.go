package provisioner

// Brand-seam safety tests for the workspace-container name matcher.
//
// EVERY fixture below is a LITERAL string. None may be derived from the
// package constants or from branding.*: a derived fixture silently flips with
// the brand token and the test proves nothing (the exact vacuity these tests
// exist to prevent).
//
// The *_FlippedWorld tests drive the pure `…In` matcher body with a simulated
// brand list:
//
//	{"enteros"}        — the BROKEN world: a matcher keyed on the current brand
//	                     alone. The mol-* fixtures MUST go unrecognized, proving
//	                     the failure mode is real (negative control). A suite
//	                     that only shows "enteros matches enteros" says nothing
//	                     about the union.
//	{"enteros","mol"}  — the union branding.AllResourcePrefixes() returns. The
//	                     same fixtures MUST be recognized.
//
// Separately, TestGolden_WorkspaceContainerPrefixes pins the DECLARED DEFAULT —
// what production runs with when nobody injects a brand list. Injected-argument
// tests prove the mechanism; only the golden proves the value.

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"

	"go.moleculesai.app/sdk/gen/go/branding"
)

// Literal fixtures — the shapes that actually appear on a shared daemon.
const (
	// Minted by THIS package: ContainerName(wsID) = "ws-" + full dashed UUID.
	fxOwnFull = "ws-abc123de-f456-7890-abcd-ef0123456789"
	// Minted by this package pre-KI-013: first 12 chars of the DASHED uuid.
	fxOwnLegacy = "ws-abc123de-f45"
	// Minted by the control plane's local-docker backend, legacy brand.
	fxMolWS = "mol-ws-acme-abc123def456"
	// Same, current brand. Both generations co-exist on one host.
	fxEnterosWS = "enteros-ws-acme-abc123def456"
	// Tenant-less control-plane form.
	fxMolWSNoTenant = "mol-ws-abc123def456"
	// Hyphenated tenant slug — the short is still the trailing dash-free run.
	fxEnterosWSHyphenTenant = "enteros-ws-reno-stars-abc123def456"
	// Never a workspace container, but the Docker name filter is a SUBSTRING
	// match so it DOES reach the parser. Must be rejected in every world.
	fxSubstringDecoy = "my-ws-thing"
	// Other resource families that share the brand token but are not workspaces.
	fxMolTenant  = "mol-tenant-acme-x"
	fxMolNetwork = "mol-acme-abc123def456"
	fxUnrelated  = "postgres"

	// The workspace row all the abc123… fixtures above belong to.
	fxWorkspaceUUID = "abc123de-f456-7890-abcd-ef0123456789"
)

var (
	brandsFlippedOnly = []string{"enteros"}        // broken world
	brandsUnion       = []string{"enteros", "mol"} // AllResourcePrefixes()
)

// TestGolden_WorkspaceContainerPrefixes pins the DECLARED DEFAULT — the value
// production computes with no argument injected. Literals on purpose: deriving
// them from the constants would make this golden vacuous.
func TestGolden_WorkspaceContainerPrefixes(t *testing.T) {
	if containerNamePrefix != "ws-" {
		t.Errorf("containerNamePrefix = %q, want the brand-free literal %q", containerNamePrefix, "ws-")
	}
	if wsInfix != "-ws-" {
		t.Errorf("wsInfix = %q, want the brand-free literal %q", wsInfix, "-ws-")
	}
	if got, want := branding.AllResourcePrefixes(), []string{"enteros", "mol"}; !reflect.DeepEqual(got, want) {
		t.Errorf("branding.AllResourcePrefixes() = %v, want %v (current brand first, outgoing \"mol\" recognised forever)", got, want)
	}
	if got, want := branding.LegacyResourcePrefixes(), []string{"mol"}; !reflect.DeepEqual(got, want) {
		t.Errorf("branding.LegacyResourcePrefixes() = %v, want %v — \"mol\" must be recognised forever or reapers false-orphan the running mol-* fleet", got, want)
	}
	if got, want := WorkspaceContainerNamePrefixes(), []string{"ws-", "enteros-ws-", "mol-ws-"}; !reflect.DeepEqual(got, want) {
		t.Errorf("WorkspaceContainerNamePrefixes() = %v, want %v", got, want)
	}
}

// TestDefaultParse_RecognisesLegacyBrandWithoutInjection is the companion to
// the golden: it drives the PRODUCTION entry point (no brand list argument) and
// asserts the legacy-brand container is recognised. Mutating
// LegacyResourcePrefixes() to drop "mol", or rebinding
// parseWorkspaceContainerID to anything narrower than the union, fails here —
// an injected-argument test would not.
func TestDefaultParse_RecognisesLegacyBrandWithoutInjection(t *testing.T) {
	for _, name := range []string{fxOwnFull, fxOwnLegacy, fxMolWS, fxEnterosWS} {
		if _, ok := parseWorkspaceContainerID(name); !ok {
			t.Errorf("parseWorkspaceContainerID(%q) = not-ok; production defaults must recognise every live workspace container shape", name)
		}
	}
	for _, name := range []string{fxSubstringDecoy, fxMolTenant, fxMolNetwork, fxUnrelated} {
		if id, ok := parseWorkspaceContainerID(name); ok {
			t.Errorf("parseWorkspaceContainerID(%q) = (%q,true); want not a workspace container", name, id)
		}
	}
}

// TestParseWorkspaceContainerIDIn_FlippedWorld is the core reaper-blindness
// negative control.
func TestParseWorkspaceContainerIDIn_FlippedWorld(t *testing.T) {
	molNames := []string{fxMolWS, fxMolWSNoTenant}

	// NEGATIVE CONTROL — broken world: every mol-* container vanishes.
	for _, name := range molNames {
		if id, ok := parseWorkspaceContainerIDIn(name, brandsFlippedOnly); ok {
			t.Errorf("negative control violated: parseWorkspaceContainerIDIn(%q, {enteros}) = (%q,true) — the flipped-world blindness this suite guards against is not being simulated", name, id)
		}
	}

	// FIX — union world: same fixtures resolve to the workspace-id prefix.
	cases := []struct {
		name string
		want string
	}{
		{fxMolWS, "abc123de-f456"},
		{fxMolWSNoTenant, "abc123de-f456"},
		{fxEnterosWS, "abc123de-f456"},
		{fxEnterosWSHyphenTenant, "abc123de-f456"},
		// Brand-free arm: unchanged, byte-for-byte, in BOTH worlds.
		{fxOwnFull, "abc123de-f456-7890-abcd-ef0123456789"},
		{fxOwnLegacy, "abc123de-f45"},
	}
	for _, tc := range cases {
		got, ok := parseWorkspaceContainerIDIn(tc.name, brandsUnion)
		if !ok || got != tc.want {
			t.Errorf("parseWorkspaceContainerIDIn(%q, union) = (%q,%v), want (%q,true)", tc.name, got, ok, tc.want)
		}
	}

	// The brand-free arm must survive ANY brand list, including an empty one —
	// containers this package mints carry no brand token at all.
	for _, brands := range [][]string{nil, {}, brandsFlippedOnly, brandsUnion} {
		if got, ok := parseWorkspaceContainerIDIn(fxOwnFull, brands); !ok || got != "abc123de-f456-7890-abcd-ef0123456789" {
			t.Errorf("parseWorkspaceContainerIDIn(%q, %v) = (%q,%v); the brand-free ws- arm must never depend on a brand token", fxOwnFull, brands, got, ok)
		}
	}

	// Non-workspace names stay unrecognized in BOTH worlds.
	for _, name := range []string{fxSubstringDecoy, fxMolTenant, fxMolNetwork, fxUnrelated, "ws-", "mol-ws-", "enteros-ws-", "enteros-ws-acme-"} {
		for _, brands := range [][]string{brandsFlippedOnly, brandsUnion} {
			if id, ok := parseWorkspaceContainerIDIn(name, brands); ok {
				t.Errorf("parseWorkspaceContainerIDIn(%q, %v) = (%q,true); want not a workspace container", name, brands, id)
			}
		}
	}
}

// TestParsedPrefixIsALikePrefixOfTheRow is the assertion that keeps the union
// from being cosmetic. Every caller turns the parse result into
// `id::text LIKE '<prefix>%'`; the control plane's short is DASH-STRIPPED, so a
// naive TrimPrefix would hand the sweeper "abc123def456", which never matches
// the dashed "abc123de-f456-…" row. Recognised-but-unmatchable is the same
// false-orphan outcome as unrecognised.
func TestParsedPrefixIsALikePrefixOfTheRow(t *testing.T) {
	for _, name := range []string{fxOwnFull, fxOwnLegacy, fxMolWS, fxMolWSNoTenant, fxEnterosWS, fxEnterosWSHyphenTenant} {
		got, ok := parseWorkspaceContainerIDIn(name, brandsUnion)
		if !ok {
			t.Fatalf("parseWorkspaceContainerIDIn(%q, union) = not-ok", name)
		}
		if !strings.HasPrefix(fxWorkspaceUUID, got) {
			t.Errorf("parseWorkspaceContainerIDIn(%q, union) = %q, which is NOT a leading substring of the row id %q — the LIKE pattern can never match, so the container is still invisible to the sweeper", name, got, fxWorkspaceUUID)
		}
		if !isLikelyWorkspaceIDLocal(got) {
			t.Errorf("parseWorkspaceContainerIDIn(%q, union) = %q, which the sweeper's hex-and-dash guard would drop", name, got)
		}
	}
}

// isLikelyWorkspaceIDLocal mirrors registry.isLikelyWorkspaceID (unexported in
// a package this one must not import — provisioner sits BELOW registry). Kept
// literal so a divergence in either direction surfaces here.
func isLikelyWorkspaceIDLocal(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'F':
		case r == '-':
		default:
			return false
		}
	}
	return true
}

// TestWorkspaceNamePrefixesFor_UnionShape proves the generic ws- arm survives
// any flip and the brand arms grow with the token list.
func TestWorkspaceNamePrefixesFor_UnionShape(t *testing.T) {
	if got, want := workspaceNamePrefixesFor(brandsFlippedOnly), []string{"ws-", "enteros-ws-"}; !reflect.DeepEqual(got, want) {
		t.Errorf("workspaceNamePrefixesFor({enteros}) = %v, want %v", got, want)
	}
	if got, want := workspaceNamePrefixesFor(brandsUnion), []string{"ws-", "enteros-ws-", "mol-ws-"}; !reflect.DeepEqual(got, want) {
		t.Errorf("workspaceNamePrefixesFor({enteros,mol}) = %v, want %v", got, want)
	}
	if got, want := workspaceNamePrefixesFor(nil), []string{"ws-"}; !reflect.DeepEqual(got, want) {
		t.Errorf("workspaceNamePrefixesFor(nil) = %v, want %v", got, want)
	}
}

// TestDockerNameFilterReachesEveryUnionPrefix pins the load-bearing coupling
// between the single-token Docker name filter and the multi-token classifier:
// the filter is a SUBSTRING match on containerNamePrefix, so it only delivers a
// candidate to the parser if every union prefix CONTAINS that token. A future
// brand shape that broke this would leave the classifier correct and the daemon
// query blind — the worst kind of silent gap.
func TestDockerNameFilterReachesEveryUnionPrefix(t *testing.T) {
	for _, p := range WorkspaceContainerNamePrefixes() {
		if !strings.Contains(p, containerNamePrefix) {
			t.Errorf("union prefix %q does not contain the Docker name filter token %q — ContainerList would never return such a container for the classifier to see", p, containerNamePrefix)
		}
	}
}

// TestDashedUUIDPrefix covers the dash-reinsertion helper directly, including
// the pass-through cases.
func TestDashedUUIDPrefix(t *testing.T) {
	cases := []struct{ in, want string }{
		{"abc123def456", "abc123de-f456"},
		{"abc123de", "abc123de"},                                     // exactly the first offset: nothing to insert
		{"abc123d", "abc123d"},                                       // shorter than the first offset
		{"abc123def4567890", "abc123de-f456-7890"},                   // second offset
		{"abc123def456789012345678", "abc123de-f456-7890-1234-5678"}, // all four
		{"abc123de-f456", "abc123de-f456"},                           // already dashed: untouched
		{"", ""},
	}
	for _, tc := range cases {
		if got := dashedUUIDPrefix(tc.in); got != tc.want {
			t.Errorf("dashedUUIDPrefix(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// End-to-end: the two list methods the orphan sweeper consumes.
// ---------------------------------------------------------------------------

// listFakeDocker adds ContainerList support to the package's fakeDockerClient.
type listFakeDocker struct {
	*fakeDockerClient
	summaries []container.Summary
	lastOpts  container.ListOptions
}

func (f *listFakeDocker) ContainerList(_ context.Context, options container.ListOptions) ([]container.Summary, error) {
	f.lastOpts = options
	return f.summaries, nil
}

func namedSummaries(names ...string) []container.Summary {
	out := make([]container.Summary, 0, len(names))
	for _, n := range names {
		// The Docker API returns names with a leading slash.
		out = append(out, container.Summary{Names: []string{"/" + n}})
	}
	return out
}

// TestListWorkspaceContainerIDPrefixes_SeesEveryBrand is the end-to-end proof
// that the seam is actually wired into the method the sweeper calls. Before the
// union, only fxOwnFull came back and the two brand-prefixed live workspaces
// were reported as having no container — which is what makes
// sweepStaleTokensWithoutContainer revoke a live workspace's auth token.
func TestListWorkspaceContainerIDPrefixes_SeesEveryBrand(t *testing.T) {
	f := &listFakeDocker{
		fakeDockerClient: newFakeDockerClient(),
		summaries: namedSummaries(
			fxOwnFull, fxMolWS, fxEnterosWS, fxEnterosWSHyphenTenant,
			fxSubstringDecoy, fxMolTenant, fxUnrelated,
		),
	}
	p := &Provisioner{cli: f}

	got, err := p.ListWorkspaceContainerIDPrefixes(context.Background())
	if err != nil {
		t.Fatalf("ListWorkspaceContainerIDPrefixes: %v", err)
	}
	want := []string{
		"abc123de-f456-7890-abcd-ef0123456789",
		"abc123de-f456",
		"abc123de-f456",
		"abc123de-f456",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ListWorkspaceContainerIDPrefixes() = %v, want %v (decoy/tenant/unrelated names dropped, every brand generation kept)", got, want)
	}
	if !f.lastOpts.All {
		t.Error("ContainerList must be called with All=true so stopped-but-present containers still count as live")
	}
}

// TestListManagedContainerIDPrefixes_SeesEveryBrand covers the wiped-DB pass's
// source. Its candidate set is label-scoped, so a brand-named container here is
// definitively ours; a `ws-`-only check would drop it and leak it forever.
func TestListManagedContainerIDPrefixes_SeesEveryBrand(t *testing.T) {
	f := &listFakeDocker{
		fakeDockerClient: newFakeDockerClient(),
		summaries:        namedSummaries(fxOwnFull, fxMolWS, fxEnterosWS, fxUnrelated),
	}
	p := &Provisioner{cli: f}

	got, err := p.ListManagedContainerIDPrefixes(context.Background())
	if err != nil {
		t.Fatalf("ListManagedContainerIDPrefixes: %v", err)
	}
	want := []string{
		"abc123de-f456-7890-abcd-ef0123456789",
		"abc123de-f456",
		"abc123de-f456",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ListManagedContainerIDPrefixes() = %v, want %v", got, want)
	}
}
