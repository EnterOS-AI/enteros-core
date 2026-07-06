package handlers

// public_url_guard.go — the SSOT predicate that decides whether a candidate
// base URL is safe to serve VERBATIM in a customer-facing connect snippet.
//
// This lives in a non-`external_*.go` file on purpose: the leak-lint
// (external_leak_lint_test.go, TestLeakLint_ExternalBuildersNoInternalAddrLiteral)
// forbids internal-address string LITERALS inside external_*.go builders, and
// the banned-host list necessarily contains those literals. Keeping the list +
// predicate here keeps the builders literal-free while still giving them a
// single, tested guard to call.

import (
	"net/url"
	"strings"
)

// bannedInternalHosts are internal-address hostnames that must never appear in
// a customer-facing connect-snippet URL. A real external agent off the docker
// host cannot reach any of them, and exposing one leaks internal topology.
// Production SSOT for the list; the connect-snippet leak tests reference it so
// the guard and the assertion can never drift.
var bannedInternalHosts = []string{
	"host.docker.internal",
	"127.0.0.1",
	"0.0.0.0",
	"::1",
	"localhost",
}

// isPublicExternalURL reports whether s is safe to serve verbatim as the
// customer-facing platform base: a parseable, portless, HTTPS URL free of any
// internal-address host. It mirrors the invariant assertPublicSnippetURL
// enforces on the OUTPUT, applied to a candidate env value BEFORE we trust it.
//
// molecule-core does NOT guarantee PLATFORM_URL is public — cmd/server/main.go
// defaults it to http://host.docker.internal:<port>, and provisioner/platform
// paths carry in-cluster values (e.g. http://platform:8080) — so an
// unvalidated PLATFORM_URL / EXTERNAL_PLATFORM_URL could leak an internal /
// non-HTTPS / ported address into the snippet, the exact class this endpoint's
// leak-lint closes. Reject here and let the caller fall through to the next
// resolution source.
func isPublicExternalURL(s string) bool {
	low := strings.ToLower(s)
	for _, bad := range bannedInternalHosts {
		if strings.Contains(low, bad) {
			return false
		}
	}
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	// Portless HTTPS with a real host. url.Parse("https://") yields an empty
	// Host; a raw published port (e.g. :8080) must never survive.
	return u.Scheme == "https" && u.Host != "" && u.Port() == ""
}
