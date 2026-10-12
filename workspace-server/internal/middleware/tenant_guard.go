package middleware

import (
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
)

// flyReplaySrcHeader is retained for deployments using the legacy Fly replay
// mechanism. Its format is a semicolon-
// separated list of k=v pairs, e.g.
//
//	instance=91854...;region=ord;t=1700000000000;state=<uuid>
//
// Those deployments put the bare UUID in state (no prefix) because Fly's
// proxy returns 502 "replay malformed" on any second `=` in the value.
// We read the whole state= segment as the org id.
const flyReplaySrcHeader = "Fly-Replay-Src"

// Tenant-mode guard — public repo's only SaaS hook.
//
// The SaaS control plane (private `molecule-controlplane` repo) provisions one
// tenant platform per customer org on the configured backend and sets
// MOLECULE_ORG_ID=<uuid>. The domain router forwards
// X-Molecule-Org-Id=<uuid> to the tenant platform.
//
// TenantGuard wraps every non-allowlisted route so a mis-routed request from
// another org bounces with 404 (not 403 — don't leak existence).
//
// When MOLECULE_ORG_ID is unset (self-hosted / dev / CI), the guard is a
// passthrough — self-hosters see no behavior change.
//
// The guard intentionally knows nothing about orgs, signup, billing, or
// provisioning. Those live in the private control-plane repo. All this code
// does is: "am I the tenant for this request? if not, reject it."
// This is routing validation, not authentication: protected handlers must still
// verify a CP session, org/admin token, or workspace token. Missing tenant
// identity is an actionable client error. Wrong tenant identity still returns
// 404 so cross-tenant probes cannot distinguish "wrong tenant" from "no such
// route".

// tenantOrgIDHeader is the HTTP header the control-plane/domain router sets when
// routing a request to a tenant platform. Case-insensitive at the HTTP layer
// (Gin normalizes).
const tenantOrgIDHeader = "X-Molecule-Org-Id"

// tenantGuardAllowlist is the set of paths that MUST remain accessible even in
// tenant mode without the org header (health checks, Prometheus scrapes,
// workspace → platform boot signals).
// Exact-match — no prefix semantics — to avoid accidentally exposing admin
// routes via e.g. "/health/debug/admin".
//
// /registry/register and /registry/heartbeat are workspace-initiated boot
// signals. Workspace hosts are provisioned by the control plane with
// PLATFORM_URL but no MOLECULE_ORG_ID env var, so the runtime's httpx
// calls can't attach X-Molecule-Org-Id. The registry handlers themselves
// enforce workspace-scoped bearer auth via wsauth.HasAnyLiveToken. Allowlisting
// here only bypasses the cross-org routing check, not auth.
var tenantGuardAllowlist = map[string]struct{}{
	"/health":             {},
	"/buildinfo":          {},
	"/metrics":            {},
	"/registry/register":  {},
	"/registry/heartbeat": {},
}

// TenantGuard returns a Gin middleware configured from the MOLECULE_ORG_ID env
// var. Reads env once at construction — changing the env at runtime requires
// a restart (matches every other platform env var). Pass the orgID directly to
// TenantGuardWithOrgID if you need to test a specific configuration without
// mutating the process environment.
func TenantGuard() gin.HandlerFunc {
	return TenantGuardWithOrgID(strings.TrimSpace(os.Getenv("MOLECULE_ORG_ID")))
}

// TenantGuardWithOrgID is the constructor used by tests; ordinary callers use
// TenantGuard. When configuredOrgID is empty the guard is a no-op.
func TenantGuardWithOrgID(configuredOrgID string) gin.HandlerFunc {
	if configuredOrgID == "" {
		return func(c *gin.Context) { c.Next() }
	}
	return func(c *gin.Context) {
		if _, ok := tenantGuardAllowlist[c.Request.URL.Path]; ok {
			c.Next()
			return
		}
		// /cp/* is reverse-proxied to the control plane. The CP has its
		// own auth (WorkOS session cookie + admin bearer) so the tenant
		// doesn't need to attach org identity here. Bypassing the guard
		// avoids blocking the proxy with a 404 that would then look
		// like the CP is down.
		//
		// SECURITY NOTE: this pass-through is routing-only. cp_proxy enforces
		// its own explicit path allowlist (see router/cp_proxy.go
		// cpProxyAllowedPrefixes), and the CP endpoints enforce their own auth.
		if strings.HasPrefix(c.Request.URL.Path, "/cp/") {
			c.Next()
			return
		}
		// Primary: explicit X-Molecule-Org-Id header from the domain router
		// (or from authenticated API tooling that supplies it directly).
		if c.GetHeader(tenantOrgIDHeader) == configuredOrgID {
			c.Next()
			return
		}
		// Secondary: legacy Fly deployments encode the org id in
		// Fly-Replay-Src state because response headers do not travel to the
		// replayed tenant.
		if orgIDFromReplaySrc(c.GetHeader(flyReplaySrcHeader)) == configuredOrgID {
			c.Next()
			return
		}
		// Tertiary: same-origin Canvas requests on tenant platforms where the
		// UI and API are served under the same domain.
		// CANVAS_PROXY_URL is set → Referer/Origin matches Host. This bypasses
		// only routing validation; it must never replace handler authentication.
		if isSameOriginCanvas(c) {
			c.Next()
			return
		}
		// Missing identity is an actionable API client error. This is the
		// common operator/molecli failure mode: a valid bearer reaches the right
		// hostname but omits the required SaaS routing header.
		if c.GetHeader(tenantOrgIDHeader) == "" && c.GetHeader(flyReplaySrcHeader) == "" {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"error":           "missing tenant routing header",
				"code":            "TENANT_ORG_HEADER_REQUIRED",
				"required_header": tenantOrgIDHeader,
				"detail":          "SaaS tenant API requests must include X-Molecule-Org-Id matching the organization UUID.",
			})
			return
		}
		// Wrong identity remains 404, not 403 — existence of this tenant must
		// not be inferable by probing other orgs' machines.
		c.AbortWithStatus(404)
	}
}

// orgIDFromReplaySrc extracts the org id from a legacy Fly replay state=
// segment. Its value is a bare UUID because Fly rejects another `=` in the
// state value. Returns "" if the header is missing or has no state segment.
// Separated from TenantGuardWithOrgID so tests can round-trip header →
// id without spinning a full Gin context.
func orgIDFromReplaySrc(header string) string {
	if header == "" {
		return ""
	}
	for _, seg := range strings.Split(header, ";") {
		seg = strings.TrimSpace(seg)
		const statePrefix = "state="
		if strings.HasPrefix(seg, statePrefix) {
			return seg[len(statePrefix):]
		}
	}
	return ""
}
