//go:build staging_e2e

package staginge2e

// tenant_topology.go — derive the tenant-facing routing/CORS topology for the
// shared Go staging-e2e harness, so the SAME test binary runs unchanged against
// either
//
//	(a) real staging  — each tenant front-doored at its own slug.<suffix>, OR
//	(b) an ephemeral CP — one throwaway container whose wildcard proxy resolves
//	    the tenant by SLUG (Host + X-Molecule-Org-Slug), reached via the CP base.
//
// This is the Go sibling of tests/e2e/lib/tenant_topology.sh (the #4406
// ephemeral-port contract), so the Go go-staging-tests reuse ONE derivation
// instead of re-hardcoding https://slug.suffix + Origin=https://host in every
// request. Default (all MOLECULE_TENANT_* unset) reproduces the exact staging
// behaviour byte-for-byte: baseURL = "https://"+slug+"."+suffix, no route
// headers, Origin = baseURL — identical to the historical hardcoded values.
//
// The six tenant request-builders (doTenantJSON / doTenantJSONTimeout /
// doTenantCreateOnce / serveProbe / waitForHTTP / waitForTenantRoute) consult
// this instead of building "https://"+host by hand, so topology threads through
// the whole harness without changing any helper signature or call site.

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// tenantTopology is the resolved routing/CORS shape for one tenant identity.
type tenantTopology struct {
	baseURL   string // scheme://authority the scenario hits (no trailing slash)
	routeHost string // Host header under ephemeral slug-routing; "" on staging
	slug      string // X-Molecule-Org-Slug value under ephemeral slug-routing
	origin    string // exact Origin the tenant's gin-contrib/cors allows
}

// deriveTenantTopology mirrors derive_tenant_topology (tenant_topology.sh).
//
// subdomainSuffix is the historical staging suffix (cfg.subdomainSuffix); it is
// the default tenant domain so the derived baseURL is byte-identical to the old
// host = slug + "." + subdomainSuffix. cpBase is accepted for parity with the
// shell contract's cp_url arg and used only as a defensive domain fallback when
// the suffix is empty (never, in practice).
//
// Env inputs (all optional; unset ⇒ exact staging behaviour):
//
//	MOLECULE_TENANT_URL             base URL override (ephemeral ⇒ the CP base URL)
//	MOLECULE_TENANT_DOMAIN          override the derived staging tenant domain
//	MOLECULE_TENANT_ROUTE_HOST      explicit Host (else <slug>.<ROUTE_DOMAIN>)
//	MOLECULE_TENANT_ROUTE_DOMAIN    ephemeral route domain (e.g. lvh.me)
//	MOLECULE_TENANT_ROUTE_PORT      explicit origin port (else taken from baseURL)
//	MOLECULE_TENANT_ORIGIN_TEMPLATE the CP's LOCAL_TENANT_URL_TEMPLATE with {slug}
//
// Returns an error ONLY in the ephemeral case where slug-routing is active but a
// tenant CORS Origin cannot be derived (no scheme/route host and no template) —
// the exact fail-closed twin of the shell's rc=1 path. It never errors on the
// default (unset) staging path.
func deriveTenantTopology(slug, subdomainSuffix, cpBase string) (tenantTopology, error) {
	tenantDomain := envOr("MOLECULE_TENANT_DOMAIN", subdomainSuffix)
	if tenantDomain == "" {
		tenantDomain = deriveDomainFromCPBase(cpBase)
	}

	top := tenantTopology{slug: slug}
	top.baseURL = strings.TrimRight(
		envOr("MOLECULE_TENANT_URL", "https://"+slug+"."+tenantDomain), "/")

	// Ephemeral slug-routing headers: carry the routing slug via Host +
	// X-Molecule-Org-Slug so the CP wildcard proxy resolves the tenant. Default
	// unset ⇒ no route host ⇒ no extra headers ⇒ exact staging behaviour.
	routeHost := strings.TrimSpace(os.Getenv("MOLECULE_TENANT_ROUTE_HOST"))
	if routeHost == "" {
		if rd := strings.TrimSpace(os.Getenv("MOLECULE_TENANT_ROUTE_DOMAIN")); rd != "" {
			routeHost = slug + "." + rd
		}
	}
	top.routeHost = routeHost

	// Origin precedence (mirrors the shell contract):
	//   1. MOLECULE_TENANT_ORIGIN_TEMPLATE → the SAME template the CP turns into
	//      the tenant's CORS_ORIGINS, substituted with this slug. Always wins.
	//   2. ephemeral slug-routing active but template unset → baseURL is the CP
	//      base URL (NOT a tenant origin), so derive a tenant-scoped origin from
	//      the route host + the scheme/port of baseURL.
	//   3. staging (no routing) → Origin = baseURL (the tenant's own subdomain,
	//      which IS its CORS_ORIGINS) ⇒ exact staging behaviour.
	if tmpl := strings.TrimSpace(os.Getenv("MOLECULE_TENANT_ORIGIN_TEMPLATE")); tmpl != "" {
		top.origin = strings.ReplaceAll(tmpl, "{slug}", slug)
		return top, nil
	}
	if routeHost != "" {
		u, err := url.Parse(top.baseURL)
		scheme := ""
		if err == nil {
			scheme = u.Scheme
		}
		if scheme != "http" && scheme != "https" {
			return top, fmt.Errorf(
				"cannot derive a tenant CORS Origin for ephemeral slug-routing "+
					"(scheme=%q route_host=%q): set MOLECULE_TENANT_ORIGIN_TEMPLATE to the "+
					"CP's LOCAL_TENANT_URL_TEMPLATE (with {slug})", scheme, routeHost)
		}
		// A route host that already carries a port is used verbatim (mirrors the
		// shell's *:* case); otherwise take the port from ROUTE_PORT or baseURL.
		if strings.Contains(routeHost, ":") {
			top.origin = scheme + "://" + routeHost
		} else {
			port := strings.TrimSpace(os.Getenv("MOLECULE_TENANT_ROUTE_PORT"))
			if port == "" {
				port = u.Port()
			}
			if port != "" {
				top.origin = scheme + "://" + routeHost + ":" + port
			} else {
				top.origin = scheme + "://" + routeHost
			}
		}
		return top, nil
	}
	top.origin = top.baseURL
	return top, nil
}

// deriveDomainFromCPBase reproduces the shell's cp_host→domain derivation. It is
// only a defensive fallback (the suffix is non-empty on every real run), so the
// default staging path never depends on it.
func deriveDomainFromCPBase(cpBase string) string {
	host := cpBase
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	if i := strings.IndexByte(host, '/'); i >= 0 {
		host = host[:i]
	}
	switch {
	case strings.HasPrefix(host, "api."):
		return strings.TrimPrefix(host, "api.")
	case strings.HasPrefix(host, "staging-api."):
		return "staging." + strings.TrimPrefix(host, "staging-api.")
	default:
		return host
	}
}

// tenantTopoFromHost derives the topology for a tenant IDENTITY host of the form
// "<slug>.<suffix>" — the historical host string the harness threads through its
// helpers. slug is the first DNS label; the remainder is the staging suffix.
func tenantTopoFromHost(host string) (tenantTopology, error) {
	slug, suffix := host, ""
	if i := strings.IndexByte(host, '.'); i >= 0 {
		slug, suffix = host[:i], host[i+1:]
	}
	return deriveTenantTopology(slug, suffix, "")
}

// tenantTopoFromURL parses a historical tenant URL ("https://<slug>.<suffix><path>")
// and returns the topology plus the URL rewritten onto the topology baseURL
// (baseURL + path?query). Under the default (unset) staging path the rewritten
// URL is byte-identical to the input.
func tenantTopoFromURL(rawURL string) (rewritten string, top tenantTopology, err error) {
	u, perr := url.Parse(rawURL)
	if perr != nil || u.Host == "" {
		return rawURL, tenantTopology{}, fmt.Errorf("parse tenant url %q: %v", rawURL, perr)
	}
	top, err = tenantTopoFromHost(u.Host)
	if err != nil {
		return rawURL, top, err
	}
	return top.baseURL + u.RequestURI(), top, nil
}

// applyTenantRouting sets the ephemeral slug-routing headers + the CORS Origin on
// req. Under the staging default (routeHost==""), it sets ONLY Origin to the
// tenant's own subdomain — byte-identical to the historical
// `req.Header.Set("Origin", "https://"+host)`; no Host/X-Molecule-Org-Slug are
// added.
func applyTenantRouting(req *http.Request, top tenantTopology) {
	applyTenantHostOnly(req, top)
	if top.origin != "" {
		req.Header.Set("Origin", top.origin)
	}
}

// applyTenantHostOnly sets ONLY the slug-routing Host + X-Molecule-Org-Slug
// headers (no Origin) — for probes like /health that carry no CORS Origin today.
// Under the staging default (routeHost=="") it is a no-op, so the request is
// byte-identical to the historical one.
func applyTenantHostOnly(req *http.Request, top tenantTopology) {
	if top.routeHost != "" {
		// In net/http the Host header is set via req.Host (Header.Set("Host",…) is
		// ignored on the wire); this is what makes the CP wildcard proxy route.
		req.Host = top.routeHost
		req.Header.Set("X-Molecule-Org-Slug", top.slug)
	}
}
