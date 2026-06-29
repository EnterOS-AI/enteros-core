package handlers

// external_connect_snippet_e2e_test.go — CI-E2E for the "Connect your
// external agent" flow.
//
// THE FLOW THIS GUARDS (end to end):
//
//	control plane provisions a local-docker tenant
//	  → injects EXTERNAL_PLATFORM_URL = the tenant's PUBLIC tunnel URL
//	    (molecule-controlplane #1050, tenantPublicURLEnvArgs →
//	    "https://<slug>.<public-base-domain>")
//	  → operator opens the workspace's "Connect your external agent" modal
//	  → the modal calls GET /workspaces/:id/external/connection
//	  → the workspace-server returns the connect-snippet payload, whose
//	    base is externalPlatformURL() and whose registry_endpoint /
//	    heartbeat_endpoint are derived from it.
//
// THE BUG CLASS: before #1050, a local-docker tenant had NO public base
// env, so externalPlatformURL() fell through to the request Host header,
// which in the published-port posture is host.docker.internal:<port> —
// an internal address that (a) LEAKS docker topology in a customer-facing
// response and (b) is UNREACHABLE by a real external agent off the docker
// host. The copy-paste snippet was therefore wrong from anywhere.
//
// A full live provision (real docker tenant boot) is not feasible per-PR,
// so this is the HERMETIC equivalent: it drives the REAL endpoint through
// the REAL gin router with the EXACT env the CP injects, and asserts the
// customer-facing URL fields (platform_url, registry_endpoint,
// heartbeat_endpoint) are the PUBLIC https://<slug>.moleculesai.app and
// carry NO host.docker.internal / 127.0.0.1 / localhost / raw :<port>.
//
// The CP half of the chain (that EXTERNAL_PLATFORM_URL/PLATFORM_URL are
// pinned to the public URL, never the in-box docker address) is asserted
// CP-side by TestTenantPublicURLEnvArgs_* in
// molecule-controlplane/internal/provisioner/local_docker_external_url_test.go.
// Together they cover the full inject→serve chain.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// bannedInternalHosts are the internal-address hostnames a customer-facing
// connect-snippet URL must NEVER contain. A real external agent off the
// docker host cannot reach any of them, and exposing one leaks internal
// topology in a customer-facing response.
var bannedInternalHosts = []string{
	"host.docker.internal",
	"127.0.0.1",
	"0.0.0.0",
	"::1",
	"localhost",
}

// assertPublicSnippetURL fails if value is not a public, portless HTTPS URL
// free of any internal-address hostname. This is the single assertion the
// whole connect-snippet leak class reduces to: every customer-facing URL
// field must satisfy it.
func assertPublicSnippetURL(t *testing.T, field, value string) {
	t.Helper()
	if value == "" {
		t.Errorf("%s is empty", field)
		return
	}
	// Substring sweep first — catches an internal host embedded anywhere
	// (even in a malformed URL the parser would otherwise mangle).
	low := strings.ToLower(value)
	for _, bad := range bannedInternalHosts {
		if strings.Contains(low, bad) {
			t.Errorf("%s = %q leaks internal address %q (unreachable by an external agent; topology leak)", field, value, bad)
		}
	}
	u, err := url.Parse(value)
	if err != nil {
		t.Errorf("%s = %q is not a parseable URL: %v", field, value, err)
		return
	}
	if u.Scheme != "https" {
		t.Errorf("%s = %q is not HTTPS — an external agent needs proper TLS (and the public tunnel terminates :443)", field, value)
	}
	// The public tunnel URL is portless (Cloudflare terminates :443). A raw
	// published port (e.g. host.docker.internal:50389, or any :<port>) must
	// never survive into a customer-facing field.
	if p := u.Port(); p != "" {
		t.Errorf("%s = %q carries an explicit port %q — the public URL must be portless", field, value, p)
	}
}

// newConnectSnippetRouter wires the REAL route the canvas calls
// (GET /workspaces/:id/external/connection) onto a bare engine so the test
// exercises routing + handler + JSON marshalling end-to-end. The wsAuth
// middleware is intentionally omitted — auth is not what this e2e pins;
// the URL correctness of the served payload is.
func newConnectSnippetRouter(wh *WorkspaceHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/workspaces/:id/external/connection", wh.GetExternalConnection)
	return r
}

// callConnectSnippet drives the endpoint and returns the decoded
// connection sub-object. hostHeader is set deliberately to an INTERNAL
// address so a regression that reads the request Host (the pre-#1050 leak
// path) would surface here.
func callConnectSnippet(t *testing.T, wh *WorkspaceHandler, wsID, hostHeader string) map[string]interface{} {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/workspaces/"+wsID+"/external/connection", nil)
	// Internal host + forwarded host: if the handler ever falls back to the
	// request Host, the leak would appear in the response and red the test.
	req.Host = hostHeader
	req.Header.Set("X-Forwarded-Host", hostHeader)
	newConnectSnippetRouter(wh).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET external/connection: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Connection map[string]interface{} `json:"connection"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode connection payload: %v (body=%s)", err, w.Body.String())
	}
	if body.Connection == nil {
		t.Fatalf("response had no 'connection' object: %s", w.Body.String())
	}
	return body.Connection
}

// expectExternalRuntimeLookup primes the single SELECT GetExternalConnection runs
// (runtime + name) so the handler proceeds to build the payload.
func expectExternalRuntimeLookup(mock sqlmock.Sqlmock, wsID, runtime, name string) {
	mock.ExpectQuery(`SELECT COALESCE\(runtime, ''\), COALESCE\(name, ''\) FROM workspaces WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"runtime", "name"}).AddRow(runtime, name))
}

// TestE2E_ExternalConnectSnippet_PublicHTTPSNoInternal is the production
// posture: the CP injected EXTERNAL_PLATFORM_URL = the public tunnel URL.
// The served snippet's platform_url + registry_endpoint + heartbeat_endpoint
// must be exactly that public HTTPS base, regardless of the (internal)
// request Host.
func TestE2E_ExternalConnectSnippet_PublicHTTPSNoInternal(t *testing.T) {
	const publicURL = "https://acme.moleculesai.app"
	t.Setenv("EXTERNAL_PLATFORM_URL", publicURL)
	// Ensure no PLATFORM_URL bleed-through decides the result — EXTERNAL_ wins.
	t.Setenv("PLATFORM_URL", "")

	mock := setupTestDB(t)
	setupTestRedis(t)
	wh := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	expectExternalRuntimeLookup(mock, "ws-ext", "external", "my-bot")

	// host.docker.internal:50389 is the EXACT pre-#1050 leak the request-Host
	// fallback produced for a local-docker tenant. It must NOT win.
	conn := callConnectSnippet(t, wh, "ws-ext", "host.docker.internal:50389")

	if got := conn["platform_url"]; got != publicURL {
		t.Errorf("platform_url = %v; want %q (the CP-injected public base)", got, publicURL)
	}
	if got := conn["registry_endpoint"]; got != publicURL+"/registry/register" {
		t.Errorf("registry_endpoint = %v; want %q", got, publicURL+"/registry/register")
	}
	if got := conn["heartbeat_endpoint"]; got != publicURL+"/registry/heartbeat" {
		t.Errorf("heartbeat_endpoint = %v; want %q", got, publicURL+"/registry/heartbeat")
	}
	// The core assertion: every customer-facing URL field is public + clean.
	for _, field := range []string{"platform_url", "registry_endpoint", "heartbeat_endpoint"} {
		assertPublicSnippetURL(t, field, asString(t, conn, field))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock: %v", err)
	}
}

// TestE2E_ExternalConnectSnippet_DefenseInDepthPlatformURL is the
// defense-in-depth posture: a deploy shipped the tenant WITHOUT
// EXTERNAL_PLATFORM_URL (the EC2 prod posture today, and the "ops forgot
// the env" case). PLATFORM_URL still carries the public tunnel URL, so the
// snippet stays public and never falls through to the internal Host.
func TestE2E_ExternalConnectSnippet_DefenseInDepthPlatformURL(t *testing.T) {
	const publicURL = "https://acme.moleculesai.app"
	t.Setenv("EXTERNAL_PLATFORM_URL", "") // simulate the missing env
	t.Setenv("PLATFORM_URL", publicURL)   // CP sets this on every backend

	mock := setupTestDB(t)
	setupTestRedis(t)
	wh := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	expectExternalRuntimeLookup(mock, "ws-ext", "external", "my-bot")

	conn := callConnectSnippet(t, wh, "ws-ext", "host.docker.internal:50389")

	if got := conn["platform_url"]; got != publicURL {
		t.Errorf("platform_url = %v; want %q (PLATFORM_URL fallback must win over the internal Host)", got, publicURL)
	}
	for _, field := range []string{"platform_url", "registry_endpoint", "heartbeat_endpoint"} {
		assertPublicSnippetURL(t, field, asString(t, conn, field))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock: %v", err)
	}
}

// asString extracts a string field from the decoded connection map, failing
// the test if it is missing or not a string.
func asString(t *testing.T, m map[string]interface{}, key string) string {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("connection payload missing field %q", key)
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("connection field %q is %T, want string", key, v)
	}
	return s
}
