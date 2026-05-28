package providers

import (
	"fmt"
	"sort"
	"strings"
)

// PlatformProviderName is the single, closed, core-only provider key that
// denotes Molecule-managed billing (no tenant key; the platform owns the
// upstream credential + the bill). It is a CLOSED set BY CONSTRUCTION: a
// third-party / contributed runtime manifest can introduce its own providers
// (BYOK by definition), but it can never name one `platform` and thereby
// forge platform billing — the merge/validation layer reserves this key for
// the core catalog (internal#718 federation refinement, CTO 2026-05-27).
// DeriveProvider treats it like any other native provider for resolution;
// the closed-set guarantee is enforced at manifest registration/merge, not
// here. isPlatformProvider is the single predicate billing/credential
// emission keys off the DERIVED provider (P2; not wired in P0).
const PlatformProviderName = "platform"

// IsPlatform reports whether this provider is the closed, core-only
// platform-managed provider. Billing + credential-emission decisions key off
// this predicate applied to a DERIVED provider (P2), so a model can never be
// platform-billed unless DeriveProvider resolves it to the closed platform
// entry. Any BYOK / third-party provider returns false -> fail-closed
// without the tenant's own key.
func (p Provider) IsPlatform() bool {
	return p.Name == PlatformProviderName
}

// DeriveProvider resolves the SINGLE owning Provider for a (runtime, model)
// pair against the merged registry Manifest. It is the P0 foundation of
// internal#718: every model->provider decision point will eventually derive
// through this one function instead of one of the ~9 hardcoded, disagreeing
// vocabularies. In P0 NOTHING in production calls it (additive, zero behavior
// change) — it is exercised only by tests + the codegen artifact.
//
// It is written as a method on Manifest (a pure function of the merged
// registry) so a future FEDERATED registry — core catalog UNION validated
// per-runtime contributed manifests — works through the identical code path:
// DeriveProvider neither knows nor cares whether a runtime/provider is
// first-party or contributed; it only sees the merged Manifest.
//
// Resolution (fail-closed at every step — never silently default):
//
//  1. The runtime must be known. An unknown runtime errors (it never falls
//     through to "any provider in the catalog").
//  2. The candidate set is the runtime's NATIVE provider set ONLY (the
//     `runtimes:` block). A provider absent from the runtime's native set is
//     never selectable for that runtime, even if its catalog regex matches.
//  3. EXACT model-id match is authoritative (CTO 2026-05-27 "disambiguate by
//     exact model id"): if the model id appears verbatim in exactly one
//     native provider ref's Models list, that provider wins outright — this
//     resolves the kimi namespace split (moonshot/kimi-k2.6 -> platform vs
//     bare kimi-for-coding -> kimi-coding) deterministically and overrides
//     any broader prefix match.
//  4. Otherwise, fall back to model_prefix_match among the native providers.
//  5. If >1 native provider still matches, disambiguate by auth env: keep
//     only the providers whose auth_env intersects availableAuthEnv. If
//     exactly one survives, it wins.
//  6. If still >1 (or 0) -> error. Overlap is an ambiguity the registry data
//     must resolve; none is an unregistered (unselectable) model. Both
//     fail-closed with a zero-value Provider.
//
// availableAuthEnv is the set of auth-env-var NAMES (never secret values)
// present for the workspace — exactly the disambiguation input the canvas
// uses today to split anthropic-oauth (CLAUDE_CODE_OAUTH_TOKEN) from
// anthropic-api (ANTHROPIC_API_KEY). It may be nil; nil simply means the
// auth-env tie-break cannot fire (an overlap then errors rather than guesses).
func (m *Manifest) DeriveProvider(runtime, model string, availableAuthEnv []string) (Provider, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return Provider{}, fmt.Errorf("providers: model is required")
	}

	native, ok := m.Runtimes[runtime]
	if !ok {
		return Provider{}, fmt.Errorf("providers: unknown runtime %q", runtime)
	}

	byName := make(map[string]Provider, len(m.Providers))
	for _, p := range m.Providers {
		byName[p.Name] = p
	}

	// Step 3: exact model-id match against each native provider ref's Models.
	// Authoritative — a verbatim id beats any prefix. If two native refs both
	// list the same id, that is a manifest ambiguity we surface rather than
	// silently pick (LoadManifest already forbids a provider ref appearing
	// twice in one runtime, but two DIFFERENT providers listing the same id
	// is not load-rejected, so guard it here).
	var exact []Provider
	for _, ref := range native.Providers {
		for _, mid := range ref.Models {
			if mid == model {
				if p, ok := byName[ref.Name]; ok {
					exact = append(exact, p)
				}
				break
			}
		}
	}
	if len(exact) == 1 {
		return exact[0], nil
	}
	if len(exact) > 1 {
		return Provider{}, fmt.Errorf(
			"providers: model %q for runtime %q is exact-listed by %d native providers (%s) — manifest ambiguity",
			model, runtime, len(exact), strings.Join(providerNames(exact), ", "))
	}

	// Step 4: prefix match among native providers only.
	var matched []Provider
	for _, ref := range native.Providers {
		p, ok := byName[ref.Name]
		if !ok {
			continue
		}
		if p.MatchesModel(model) {
			matched = append(matched, p)
		}
	}

	switch len(matched) {
	case 1:
		return matched[0], nil
	case 0:
		return Provider{}, fmt.Errorf(
			"providers: no native provider for runtime %q owns model %q (unregistered/unselectable)",
			runtime, model)
	}

	// Step 5: >1 prefix match — disambiguate by available auth env.
	if len(availableAuthEnv) > 0 {
		avail := make(map[string]struct{}, len(availableAuthEnv))
		for _, e := range availableAuthEnv {
			avail[e] = struct{}{}
		}
		var byAuth []Provider
		for _, p := range matched {
			for _, want := range p.AuthEnv {
				if _, ok := avail[want]; ok {
					byAuth = append(byAuth, p)
					break
				}
			}
		}
		if len(byAuth) == 1 {
			return byAuth[0], nil
		}
		if len(byAuth) > 1 {
			matched = byAuth // narrowed but still ambiguous; report the narrowed set
		}
	}

	// Step 6: still ambiguous -> error (never silently pick).
	return Provider{}, fmt.Errorf(
		"providers: model %q for runtime %q overlaps %d providers (%s) and auth env did not disambiguate — resolve in the registry",
		model, runtime, len(matched), strings.Join(providerNames(matched), ", "))
}

// Upstream is the result of ResolveUpstream: the proxy's upstream-vendor key
// (the 4-name vocabulary {openai, moonshot, anthropic, minimax} the proxy's
// resolveLLMProviderTarget switch dispatches on to pick the upstream base URL +
// key) plus the model id to send upstream (the namespace SUFFIX). Provider is
// the catalog entry the namespace resolved to (its base_url_template /
// base_url_anthropic / auth_env are the SINGLE source for the upstream target).
type Upstream struct {
	// Vendor is the proxy upstream-vendor key (Provider.UpstreamVendor). It is
	// the axis resolveLLMProviderTarget dispatches on; for "anthropic-api" it is
	// "anthropic" (the entry NAME and the upstream VENDOR legitimately differ).
	Vendor string
	// Model is the id to send upstream — the namespace suffix (e.g. the
	// "kimi-k2.6" of "moonshot/kimi-k2.6").
	Model string
	// Provider is the resolved catalog entry. Its base_url_* / auth_env are the
	// one source for the upstream target — there is no parallel routing block.
	Provider Provider
}

// ResolveUpstream is the SINGLE registry resolution the LLM proxy uses to pick
// the upstream vendor + base URL + auth for a wire model id (internal#718 P1,
// CONVERGED 2026-05-27). It replaces the proxy's hardcoded inferLLMProvider
// switch AND the earlier two-derivation shape (DeriveUpstreamForModel + a
// separate proxy_routing data block): there is now ONE resolution over the
// EXISTING vendor provider entries — no duplicate routing vocabulary.
//
// Resolution = the platform model id's NAMESPACE. A platform model id is
// `vendor/model` (or the BYOK colon form `vendor:model`); the namespace token
// NAMES the backing provider, whose catalog entry carries the upstream
// base_url_* + auth_env. The upstream vendor key is the entry's UpstreamVendor
// (a property of the entry, recorded once on the entry — NOT a parallel
// routing block). VERIFIED FACT (internal#718, 2026-05-27): all platform model
// ids in providers.yaml are namespaced; ZERO are bare — so namespace
// resolution covers 100% of live proxy traffic.
//
// It is DELIBERATELY separate from DeriveProvider:
//   - DeriveProvider is runtime-SCOPED and speaks the REGISTRY vocabulary
//     (platform/anthropic-api/kimi-coding/…); for a platform model it returns
//     `platform` (the proxy ITSELF), which is useless for upstream routing.
//   - ResolveUpstream is runtime-AGNOSTIC (the proxy serves platform models
//     across runtimes, with no single runtime) and speaks the proxy's 4-name
//     UPSTREAM vocabulary — exactly what selects the upstream base URL + key.
//
// Resolution (fail-closed; never a silent default):
//
//  1. Namespace split: for each separator "/" then ":" (the proxy's loop
//     order), cut the id. If the prefix token EQUALS some provider entry's
//     UpstreamVendor, that entry wins: Vendor = its UpstreamVendor, Model = the
//     SUFFIX. The first separator that yields a known vendor wins ("/" before
//     ":"), matching the proxy verbatim.
//  2. Otherwise the id is BARE. Bare ids are VESTIGIAL at the proxy: zero live
//     platform traffic is bare (every platform model id is namespaced), so the
//     converged path does NOT resolve them — it returns an error and the proxy
//     falls back to its documented, retained legacy switch (inferLLMProviderLegacy).
//     This is INTENTIONAL: P0 tightened bare `kimi-*` to the kimi-coding
//     gateway in the registry, which is NOT a valid proxy upstream, so routing
//     bare ids through the shared registry matcher would misroute. Namespace-
//     only resolution sidesteps that without a moonshot special-case or a new
//     bare→vendor data block.
//
// Callers that need the legacy bare behavior keep the legacy switch as a
// documented vestigial fallback (see internal/handlers/llm_proxy.go).
func (m *Manifest) ResolveUpstream(model string) (Upstream, error) {
	// NOTE: model is pre-trimmed by every production caller
	// (resolveLLMProviderTargetForProtocol trims + rejects empty before calling
	// inferLLMProvider). No TrimSpace here — the prior copy was unreachable in
	// prod and is the review nit being dropped in the convergence.
	if model == "" {
		return Upstream{}, fmt.Errorf("providers: model is required")
	}

	for _, sep := range []string{"/", ":"} {
		before, after, found := strings.Cut(model, sep)
		if !found {
			continue
		}
		for _, p := range m.Providers {
			if v := p.UpstreamVendor; v != "" && v == before {
				return Upstream{Vendor: v, Model: after, Provider: p}, nil
			}
		}
	}

	return Upstream{}, fmt.Errorf(
		"providers: %q is not an upstream-namespaced model id (vendor/model); bare ids are vestigial at the proxy and resolve via the legacy fallback", model)
}

// providerNames returns the sorted names of a provider slice for stable,
// deterministic error messages (test assertions + operator readability).
func providerNames(ps []Provider) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Name)
	}
	sort.Strings(out)
	return out
}
