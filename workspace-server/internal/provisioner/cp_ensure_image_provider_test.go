package provisioner

// cp_ensure_image_provider_test.go — core#5025 follow-up: the pre-flight must
// ask about the SAME box the provision will build.
//
// #5025 shipped the pull-before-stop guard and it was INERT in production. The
// tenant built EnsureImageRequest{WorkspaceID, Runtime, Template} and the
// provisioner back-filled only OrgID, so `provider` was ALWAYS empty on the
// ensure-image wire. Per the SDK SSOT an empty provider means AWS
// (cloudprovider.DefaultID), so the control plane answered a question about an
// AWS box — "not_applicable, nothing to pre-warm" — with 200, and the tenant
// read any 2xx as permission to destroy the container. On the local-docker
// substrate that prod actually runs on, every restart therefore still destroyed
// the container and THEN started the ~7GB pull: the core#5019 outage, while
// logging that the pre-warm had succeeded.
//
// The invariant these tests pin is not "provider is populated" but the stronger
// one that makes the guard mean anything:
//
//	The provider on the ensure-image wire EQUALS the provider on the
//	provision wire, for the same workspace, on the path where no caller
//	passes one explicitly.
//
// Asserted on the WIRE (the JSON both endpoints actually receive), never on a
// struct field a mock was handed — a pre-flight that resolves a different box
// than the provision is a guard that answers a question nobody asked, which is
// exactly the state that shipped green.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	sdkcp "go.moleculesai.app/sdk/gen/go/cloudprovider"
)

// captureCP records the decoded JSON body of every provision-family POST, keyed
// by request path. It is the wire, not a mock: the bodies it holds are the exact
// bytes the control plane would parse.
type captureCP struct {
	mu     sync.Mutex
	bodies map[string]map[string]any
}

func newCaptureCP(t *testing.T) (*captureCP, *httptest.Server) {
	t.Helper()
	c := &captureCP{bodies: map[string]map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		c.mu.Lock()
		c.bodies[r.URL.Path] = body
		c.mu.Unlock()
		switch r.URL.Path {
		case "/cp/workspaces/ensure-image":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ready","image_ref":"reg/x@sha256:abc"}`))
		default:
			// Start accepts 201 only.
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"instance_id":"mol-ws-x","state":"running"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return c, srv
}

// provider returns the `provider` field the control plane received on path, and
// whether the field was present at all. A MISSING field and an empty string are
// deliberately distinguished: `omitempty` drops the key entirely, and "the key
// was absent" is precisely the production symptom.
func (c *captureCP) provider(path string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	body, ok := c.bodies[path]
	if !ok {
		return "", false
	}
	v, ok := body["provider"]
	if !ok {
		return "", false
	}
	s, _ := v.(string)
	return s, true
}

// stubResolveProvider swaps the package's DB-backed provider lookup — the SAME
// seam Stop already routes through — and restores it afterwards.
func stubResolveProvider(t *testing.T, value string) {
	t.Helper()
	prev := resolveProvider
	resolveProvider = func(context.Context, string) (string, error) { return value, nil }
	t.Cleanup(func() { resolveProvider = prev })
}

func newProviderTestProvisioner(baseURL string) *CPProvisioner {
	p := newEnsureImageTestProvisioner(baseURL)
	p.provisionHTTPClient = p.httpClient
	return p
}

// TestEnsureImage_WireProviderMatchesTheProvisionWire is the core#5025 gate.
//
// It drives BOTH legs of a restart against one recording control plane, exactly
// as production sequences them (pre-flight, then provision), with NOBODY passing
// a provider explicitly — the production shape. The two wires must name the same
// box.
func TestEnsureImage_WireProviderMatchesTheProvisionWire(t *testing.T) {
	// The workspace's persisted compute provider. Molecules-Server is the case
	// that matters: it is what prod runs on, and it is the id an empty wire
	// value silently becomes AWS instead of.
	const workspaceProvider = sdkcp.MoleculesServer
	stubResolveProvider(t, workspaceProvider)

	cp, srv := newCaptureCP(t)
	p := newProviderTestProvisioner(srv.URL)

	// Leg 1: the pre-flight, built the way the restart paths build it — identity
	// fields only, no provider.
	if _, err := p.EnsureImage(context.Background(), EnsureImageRequest{
		WorkspaceID: "ws-5025", Runtime: "hermes", Template: "hermes",
	}); err != nil {
		t.Fatalf("EnsureImage: %v", err)
	}

	// Leg 2: the provision that follows it. Also no explicit provider — the
	// canvas compute validator drops any id outside the cloud/billable picker
	// set, and Molecules-Server is not in it, so this is the shape a real
	// local-docker workspace reaches Start with.
	if _, err := p.Start(context.Background(), WorkspaceConfig{
		WorkspaceID: "ws-5025", Runtime: "hermes", Template: "hermes",
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	ensured, ensuredPresent := cp.provider("/cp/workspaces/ensure-image")
	provisioned, provisionedPresent := cp.provider("/cp/workspaces/provision")

	if !ensuredPresent {
		t.Errorf("core#5025: the ensure-image wire carries NO provider field at all. "+
			"An absent provider is the SSOT default (%q), so the control plane pre-warms an AWS box, "+
			"answers not_applicable with 200, and the tenant reads that as permission to destroy a "+
			"%s container it never pre-warmed — core#5019 with a success log.",
			sdkcp.DefaultID, workspaceProvider)
	}
	if !provisionedPresent {
		t.Errorf("the provision wire carries no provider either, so the control plane re-guesses the "+
			"backend for a workspace whose row names %q", workspaceProvider)
	}
	if ensured != provisioned {
		t.Fatalf("core#5025: the pre-flight asked about provider %q while the provision will build on %q. "+
			"A guard that resolves a different box than the one that will run is answering a question "+
			"nobody asked.", ensured, provisioned)
	}
	if ensured != workspaceProvider {
		t.Fatalf("both wires named %q, but the workspace row says the box runs on %q", ensured, workspaceProvider)
	}
}

// TestEnsureImage_CallerProviderWins pins the direction of the back-fill.
//
// The DB lookup is a FALLBACK for callers that did not name a provider, never an
// override. The provider-switch flow stops a box on one backend and provisions
// the next on ANOTHER — there the persisted value is the OLD box, and silently
// preferring it would pre-warm the backend being abandoned.
func TestEnsureImage_CallerProviderWins(t *testing.T) {
	stubResolveProvider(t, sdkcp.MoleculesServer) // what the row still says

	cp, srv := newCaptureCP(t)
	p := newProviderTestProvisioner(srv.URL)

	if _, err := p.EnsureImage(context.Background(), EnsureImageRequest{
		WorkspaceID: "ws-switching", Runtime: "hermes", Provider: sdkcp.Hetzner, // where it is GOING
	}); err != nil {
		t.Fatalf("EnsureImage: %v", err)
	}

	got, present := cp.provider("/cp/workspaces/ensure-image")
	if !present || got != sdkcp.Hetzner {
		t.Fatalf("an explicit caller provider must reach the wire unchanged; got %q (present=%v). "+
			"Overriding it from the row pre-warms the backend being abandoned.", got, present)
	}
}

// TestEnsureImage_ProviderResolutionUsesTheSameSeamAsStop keeps the two DB reads
// from drifting into separate queries. Stop already resolves the provider so the
// CP tears the box down on its owning backend; the pre-flight must resolve it the
// same way or the two halves of one restart can disagree about which box exists.
func TestEnsureImage_ProviderResolutionUsesTheSameSeamAsStop(t *testing.T) {
	var askedFor []string
	prev := resolveProvider
	resolveProvider = func(_ context.Context, id string) (string, error) {
		askedFor = append(askedFor, id)
		return sdkcp.Hetzner, nil
	}
	t.Cleanup(func() { resolveProvider = prev })

	cp, srv := newCaptureCP(t)
	p := newProviderTestProvisioner(srv.URL)

	if _, err := p.EnsureImage(context.Background(), EnsureImageRequest{
		WorkspaceID: "ws-seam", Runtime: "hermes",
	}); err != nil {
		t.Fatalf("EnsureImage: %v", err)
	}

	if len(askedFor) != 1 || askedFor[0] != "ws-seam" {
		t.Fatalf("EnsureImage must resolve the provider through the same seam Stop uses, for THIS workspace; asked=%v", askedFor)
	}
	if got, _ := cp.provider("/cp/workspaces/ensure-image"); got != sdkcp.Hetzner {
		t.Fatalf("resolved provider did not reach the wire; got %q", got)
	}
}

// TestEnsureImage_UnresolvableProviderStillAsks is the fail-soft direction.
//
// A DB hiccup while resolving the provider must not turn into a refusal to ask
// at all: the pre-flight would then decline the restart (every non-compat error
// fails CLOSED), and a transient DB blip would wedge restarts fleet-wide — the
// larger outage this guard is not allowed to cause. The call proceeds with the
// provider unresolved, which is no worse than the pre-#5025 wire.
func TestEnsureImage_UnresolvableProviderStillAsks(t *testing.T) {
	prev := resolveProvider
	resolveProvider = func(context.Context, string) (string, error) {
		return "", context.DeadlineExceeded
	}
	t.Cleanup(func() { resolveProvider = prev })

	cp, srv := newCaptureCP(t)
	p := newProviderTestProvisioner(srv.URL)

	if _, err := p.EnsureImage(context.Background(), EnsureImageRequest{
		WorkspaceID: "ws-dbdown", Runtime: "hermes",
	}); err != nil {
		t.Fatalf("a provider-resolution failure must not become a refusal to ask: %v", err)
	}
	if _, ok := cp.bodies["/cp/workspaces/ensure-image"]; !ok {
		t.Fatal("the pre-flight must still be sent when the provider cannot be resolved")
	}
}
