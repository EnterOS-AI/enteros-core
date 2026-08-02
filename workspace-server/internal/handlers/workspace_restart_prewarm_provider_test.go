package handlers

// workspace_restart_prewarm_provider_test.go — core#5025: the pre-flight must
// ask about the box the provision is going to build.
//
// core#5025 shipped the pull-before-stop guard and it was INERT in production.
// Every restart path built EnsureImageRequest{WorkspaceID, Runtime, Template}
// and nothing ever set Provider, so the control plane resolved the SSOT default
// (AWS) for a workspace running on the local-docker substrate, answered
// "not_applicable" with 200, and the tenant read the 2xx as permission to
// destroy the container. Restarts kept destroying-then-pulling — the core#5019
// outage — while logging that the pre-warm had succeeded.
//
// Every pre-existing test of this seam supplied the identity fields only, so
// none of them could see the omission. These tests exercise the PRODUCTION
// shape: the caller passes no provider of its own, and the assertion is made on
// the JSON the control plane actually receives.
//
// The DB is deliberately stubbed to a DIFFERENT provider than the payload
// carries, so a passing test proves the CALLER handed over the provider the
// provision will use — not merely that some provider reached the wire.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
)

// prewarmWireCP is a control plane that records the ensure-image body and then
// REFUSES, so the restart declines at the pre-flight and no stop, provision or
// goroutine runs. The refusal is the containment; the recorded body is the
// assertion surface.
type prewarmWireCP struct {
	mu   sync.Mutex
	body map[string]any
	srv  *httptest.Server
}

func newPrewarmWireCP(t *testing.T) *prewarmWireCP {
	t.Helper()
	c := &prewarmWireCP{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		c.mu.Lock()
		c.body = body
		c.mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"declined so the test stops here"}`))
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *prewarmWireCP) provider(t *testing.T) (string, bool) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.body == nil {
		t.Fatal("the control plane never received an ensure-image request at all")
	}
	v, ok := c.body["provider"]
	if !ok {
		return "", false
	}
	s, _ := v.(string)
	return s, true
}

// newWireCPProvisioner builds the REAL CPProvisioner through its production
// constructor, pointed at the recording server. A stub would let the assertion
// pass on a struct field that never reached a wire — the exact class of proof
// that let core#5025 ship green.
func newWireCPProvisioner(t *testing.T, baseURL string) provisioner.CPProvisionerAPI {
	t.Helper()
	t.Setenv("MOLECULE_ORG_ID", "org-5025")
	t.Setenv("CP_PROVISION_URL", baseURL)
	p, err := provisioner.NewCPProvisioner()
	if err != nil {
		t.Fatalf("NewCPProvisioner: %v", err)
	}
	return p
}

// prewarmProviderCase is shared by both restart entry points: the payload names
// the box the provision will build, the workspace row still names an older one.
const (
	prewarmPayloadProvider = "hetzner" // where this restart is going
	prewarmRowProvider     = "gcp"     // what a bare row lookup would find
)

func stubRowProvider(t *testing.T) {
	t.Helper()
	// The pre-flight now retries a transient control plane (core#5025 finding
	// 4); these tests drive a REFUSING one, so shrink the backoff rather than
	// spend the real 1s/2s waiting for a decline we already expect.
	shrinkPrewarmRetryForTest(t)
	mock := setupTestDB(t)
	// Any row read the pre-flight makes answers with the OTHER provider, so a
	// wire carrying prewarmRowProvider proves the caller passed nothing and the
	// provisioner's row fallback filled the gap. Registered once per allowed
	// attempt: the pre-flight retries a refusing control plane, and an
	// expectation that ran out after the first call would let the later requests
	// carry no provider for a reason that has nothing to do with the caller.
	mock.MatchExpectationsInOrder(false)
	for i := 0; i < cpStopRetryAttempts; i++ {
		mock.ExpectQuery(`compute`).WillReturnRows(
			mock.NewRows([]string{"provider"}).AddRow(prewarmRowProvider))
	}
}

// TestRestartWorkspaceAutoOpts_PrewarmWireCarriesTheProvisionProvider covers the
// manual POST /workspaces/:id/restart path, which owns its own stop leg.
func TestRestartWorkspaceAutoOpts_PrewarmWireCarriesTheProvisionProvider(t *testing.T) {
	stubRowProvider(t)
	cp := newPrewarmWireCP(t)
	h := &WorkspaceHandler{cpProv: newWireCPProvisioner(t, cp.srv.URL)}

	payload := models.CreateWorkspacePayload{
		Name: "n", Tier: 4, Runtime: "hermes", Template: "hermes",
		Compute: models.WorkspaceCompute{Provider: prewarmPayloadProvider},
	}
	if h.RestartWorkspaceAutoOpts(context.Background(), "ws-manual", "", nil, payload, false) {
		t.Fatal("precondition: the refusing control plane must decline this restart")
	}
	h.waitAsyncForTest()

	got, present := cp.provider(t)
	if !present {
		t.Fatalf("core#5025: the ensure-image wire carried NO provider. The control plane then " +
			"resolves the SSOT default (aws) and answers not_applicable for a workspace that runs " +
			"somewhere else — a 200 the tenant reads as permission to destroy the container.")
	}
	if got != prewarmPayloadProvider {
		t.Fatalf("the pre-flight asked about %q but the provision this restart is about to issue "+
			"will build on %q (payload.Compute.Provider). The guard must resolve the SAME box.",
			got, prewarmPayloadProvider)
	}
}

// TestStopForRestart_PrewarmWireCarriesTheProvisionProvider covers the auto /
// programmatic path (runRestartCycle → stopForRestart), which has its own stop
// leg and therefore needs its own proof.
func TestStopForRestart_PrewarmWireCarriesTheProvisionProvider(t *testing.T) {
	stubRowProvider(t)
	cp := newPrewarmWireCP(t)
	h := &WorkspaceHandler{cpProv: newWireCPProvisioner(t, cp.srv.URL)}

	payload := models.CreateWorkspacePayload{
		Name: "n", Tier: 4, Runtime: "hermes", Template: "hermes",
		Compute: models.WorkspaceCompute{Provider: prewarmPayloadProvider},
	}
	if h.stopForRestart(context.Background(), "ws-auto", payload) {
		t.Fatal("precondition: the refusing control plane must decline this stop")
	}

	got, present := cp.provider(t)
	if !present || got != prewarmPayloadProvider {
		t.Fatalf("the auto-restart pre-flight asked about provider %q (present=%v); the provision "+
			"that follows will build on %q. core#5025: a pre-flight that resolves a different box "+
			"is not a guard.", got, present, prewarmPayloadProvider)
	}
}

// TestStopForRestart_CarriesTheRuntimeAndTemplateFromTheSamePayload keeps the
// identity fields tied to the same object as the provider. Splitting them across
// two sources is how the provider drifted away from the provision in the first
// place.
func TestStopForRestart_CarriesTheRuntimeAndTemplateFromTheSamePayload(t *testing.T) {
	stubRowProvider(t)
	cp := newPrewarmWireCP(t)
	h := &WorkspaceHandler{cpProv: newWireCPProvisioner(t, cp.srv.URL)}

	payload := models.CreateWorkspacePayload{
		Name: "n", Tier: 4, Runtime: "hermes", Template: "seo-agent",
		Compute: models.WorkspaceCompute{Provider: prewarmPayloadProvider},
	}
	h.stopForRestart(context.Background(), "ws-identity", payload)

	cp.mu.Lock()
	defer cp.mu.Unlock()
	if cp.body["runtime"] != "hermes" || cp.body["template"] != "seo-agent" {
		t.Fatalf("the pre-flight must resolve THIS workspace's pin: runtime/template on the wire = %v/%v",
			cp.body["runtime"], cp.body["template"])
	}
}
