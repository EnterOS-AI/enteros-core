package plugins

// host_credentials.go — molecule-core#4997: resolve a PER-HOST forge credential
// for the on-demand (API) plugin-install path and the drift sweeper.
//
// # Why this exists
//
// A plugin whose source repo is PRIVATE and owned by a CUSTOMER cannot be read
// by the platform's own PAT, and must not be: template_repo_creds.go states the
// constraint outright — MOLECULE_TEMPLATE_REPO_TOKEN is legitimate ONLY for
// delivering templates the platform itself owns, and "do not extend this token
// to read third-party sellers' private repos". Widening it would make one
// platform-wide credential, already present on every tenant box, a reader of
// every customer's private source.
//
// The access model instead mints a RESTRICTED, read-only Gitea machine user PER
// ORG, holding EXPLICIT COLLABORATOR grants on exactly the private repos that
// org declares as plugin sources. Gitea token scopes are capability-scoped
// (read:repository), not repo-scoped, so blast radius follows the USER — which
// is precisely why collaborator grants beat org/team membership: the grant set
// IS the scope, and revoking one grant revokes exactly one repo.
//
// The control plane resolves that credential server-side from the per-tenant
// secret store and stamps ONLY the per-host key into the workspace env (see
// controlplane internal/provisioner/plugin_git_creds.go). The container holds no
// secret-manager credential and no org-wide PAT.
//
// # What was still missing
//
// molecule_runtime's BOOT-install path already consumed that per-host key. This
// Go path — POST /workspaces/:id/plugins and the drift sweeper — did not: it
// read MOLECULE_TEMPLATE_REPO_TOKEN and nothing else, so an on-demand install of
// a customer-owned private repo failed even on a box that had been handed a
// perfectly good per-org credential. This file closes that half.
//
// # Precedence is a MIRROR, not a reinvention
//
// The SSOT is molecule_runtime plugin_sources._host_token_map. This file
// reproduces it exactly, including its unfolding direction:
//
//	1. MOLECULE_GIT_TOKENS        — JSON {"<host>": "<token>"}; host is a netloc,
//	                                so it is the only form that can carry a port.
//	2. MOLECULE_GIT_TOKEN__<HOST> — flat per-host form. Recovered the way the
//	                                runtime recovers it: suffix.lower() with '_'
//	                                unfolded to '.'. Does NOT clobber (1).
//	3. the gitea read token       — seeded for the base host only. Does NOT
//	                                clobber (1) or (2).
//
// The unfolding direction matters. Folding a host FORWARD ('.' and '-' → '_')
// would let this code match a key that the runtime unfolds to a DIFFERENT host,
// so the Go path would offer a credential the Python path would not. Mirroring
// the runtime's direction makes the two agree by construction: a host containing
// '-' is simply not expressible in the flat form, and such an operator must use
// the JSON map — exactly as on the runtime side.
//
// # Scoping
//
// A token is only ever returned for the host it is keyed to. That is the
// isolation property #4997 is about: the credential layer is keyed by HOST, so
// one org's reader is never handed to another org's forge lookup, and a
// github.com credential is never offered to our gitea.

import (
	"encoding/json"
	"log"
	"strings"
)

// perHostTokenPrefix is the flat per-host credential env prefix. Mirrors
// molecule_runtime plugin_sources._PER_HOST_TOKEN_PREFIX and the control
// plane's provisioner.PluginGitTokenEnvPrefix — the three must agree.
const perHostTokenPrefix = "MOLECULE_GIT_TOKEN__"

// gitTokensJSONEnv is the general N-provider form: a JSON host→token object.
const gitTokensJSONEnv = "MOLECULE_GIT_TOKENS"

// HostTokenFor returns the credential to present to the forge named by
// baseURLOrHost, or "" when the box holds none for that host.
//
// environ is an os.Environ()-shaped "KEY=VALUE" slice (injected so the
// resolution is a pure function and testable without mutating the process).
// giteaTokenEnv is the legacy single-forge PAT variable, kept as a last-resort
// SEED for the base host so platform-owned private repos do not regress; pass
// "" to disable it.
//
// file:// and local-path bases never carry a credential — there is no HTTP
// challenge to answer, and userinfo on a local path is meaningless.
func HostTokenFor(environ []string, baseURLOrHost, giteaTokenEnv string) string {
	if strings.HasPrefix(baseURLOrHost, "file://") || strings.HasPrefix(baseURLOrHost, "/") {
		return ""
	}
	host := netlocOf(baseURLOrHost)
	if host == "" {
		return ""
	}

	// 1. JSON map — the general form, and the only one that can carry a port.
	if raw := strings.TrimSpace(envLookup(environ, gitTokensJSONEnv)); raw != "" {
		var parsed map[string]any
		if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
			// Never fail an install on a malformed credential blob; a source
			// that needs the token will fail loudly on its own 401.
			log.Printf("[plugins] %s is not valid JSON (%v) — ignored", gitTokensJSONEnv, err)
		} else {
			for k, v := range parsed {
				token, ok := v.(string)
				if !ok || token == "" {
					continue
				}
				if netlocOf(k) == host {
					return token
				}
			}
		}
	}

	// 2. Flat per-host form. Unfolded the way the runtime unfolds it, so both
	//    sides agree on which host a given key names.
	for _, kv := range environ {
		key, value, found := strings.Cut(kv, "=")
		if !found || value == "" || !strings.HasPrefix(key, perHostTokenPrefix) {
			continue
		}
		suffix := key[len(perHostTokenPrefix):]
		if suffix == "" {
			continue
		}
		if strings.ToLower(strings.ReplaceAll(suffix, "_", ".")) == host {
			return value
		}
	}

	// 3. Legacy single-forge seed, for the base host only.
	if giteaTokenEnv != "" {
		if tok := strings.TrimSpace(envLookup(environ, giteaTokenEnv)); tok != "" {
			return tok
		}
	}

	return ""
}

// envLookup reads one key out of an os.Environ()-shaped slice. Last occurrence
// wins, matching how the OS resolves a duplicated variable.
func envLookup(environ []string, want string) string {
	out := ""
	for _, kv := range environ {
		if key, value, found := strings.Cut(kv, "="); found && key == want {
			out = value
		}
	}
	return out
}

// netlocOf reduces a URL to its netloc (host, INCLUDING any port), or returns a
// bare host unchanged. The port is deliberately retained, matching
// molecule_runtime plugin_sources._netloc: a forge on a non-default port is a
// distinct credential domain, and collapsing :8443 onto :443 would offer a
// dev-forge credential to whatever answers on the default port.
//
// ONE DELIBERATE DIVERGENCE from that mirror: userinfo is stripped. Python's
// urlsplit().netloc KEEPS it, so a base URL carrying userinfo matches no host
// there and silently offers no credential. Stripping is the more correct
// reading of RFC 3986 (the host is what follows the LAST '@' in the authority)
// and is why LastIndex is used here — with the FIRST '@', a multi-'@' authority
// leaves an '@' in the result, which then matches no key and drops the
// credential silently. Both are fail-closed; only LastIndex is correct.
func netlocOf(urlOrHost string) string {
	v := strings.TrimSpace(urlOrHost)
	if v == "" {
		return ""
	}
	if i := strings.Index(v, "//"); i >= 0 {
		v = v[i+2:]
	}
	if i := strings.IndexAny(v, "/?#"); i >= 0 {
		v = v[:i]
	}
	if i := strings.LastIndex(v, "@"); i >= 0 {
		v = v[i+1:]
	}
	return strings.ToLower(strings.TrimSpace(v))
}
