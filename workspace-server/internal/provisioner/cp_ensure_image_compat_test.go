package provisioner

// cp_ensure_image_compat_test.go — core#5025 finding 5: which 404 means
// "this control plane is old", and which means "you are not talking to the
// control plane".
//
// The compat branch is the ONE deliberate fail-open in the whole pull-before-stop
// guard, and it was triggered by a bare status code. Every 404 read as version
// skew — including the ones a misconfigured base URL, a stale ingress rule or a
// proxy that lost the route produce. In that state the entire fleet silently
// reverts to the pre-core#5019 destroy-then-pull ordering while the log reports a
// reassuring compatibility skip. It is the most expensive way for this guard to
// be wrong precisely because it looks fine.
//
// The fix is POSITIVE detection: a 404 is version skew only when the control
// plane demonstrably serves /cp/workspaces/* and merely lacks this one route. A
// CP old enough to lack ensure-image cannot emit a marker for a route it does not
// have, so the evidence has to come from a route it DOES have.
//
// Both directions are pinned here. A test that only proved the old CP still gets
// its fail-open would pass on the original bare-status-code code.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// cpRoutes builds a control plane whose ensure-image route answers 404 and whose
// /cp/workspaces/:id/status route answers with statusRouteCode (0 = not routed,
// i.e. 404).
func cpWithMissingEnsureImage(t *testing.T, statusRouteCode int) *CPProvisioner {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/status") && statusRouteCode != 0 {
			w.WriteHeader(statusRouteCode)
			_, _ = w.Write([]byte(`{"state":"running"}`))
			return
		}
		// Gin's own answer for a route it does not have.
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("404 page not found"))
	}))
	t.Cleanup(srv.Close)
	return newEnsureImageTestProvisioner(srv.URL)
}

// TestEnsureImage_404FromAnOldControlPlaneStillFailsOpen keeps the deliberate
// exception working. A tenant that rolls ahead of its control plane during a
// deploy must keep restarting; failing closed there would wedge every restart on
// the fleet, which is a much larger outage than the one being fixed.
func TestEnsureImage_404FromAnOldControlPlaneStillFailsOpen(t *testing.T) {
	stubResolveProvider(t, "")
	p := cpWithMissingEnsureImage(t, http.StatusOK)

	_, err := p.EnsureImage(context.Background(), EnsureImageRequest{WorkspaceID: "ws-old", Runtime: "hermes"})
	if !errors.Is(err, ErrEnsureImageUnsupported) {
		t.Fatalf("a control plane that serves /cp/workspaces/* but not ensure-image is a VERSION SKEW "+
			"and must fail open; got %v", err)
	}
}

// TestEnsureImage_404FromAnOldControlPlaneThatErrorsOnStatus — the probe asks
// "does this URL reach the control-plane route family", not "is this workspace
// healthy". A 4xx/5xx on the status route still proves something is listening
// and routing, so it must not be mistaken for a dead URL.
func TestEnsureImage_404FromAnOldControlPlaneThatErrorsOnStatus(t *testing.T) {
	stubResolveProvider(t, "")
	for _, code := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusInternalServerError} {
		p := cpWithMissingEnsureImage(t, code)
		_, err := p.EnsureImage(context.Background(), EnsureImageRequest{WorkspaceID: "ws-old", Runtime: "hermes"})
		if !errors.Is(err, ErrEnsureImageUnsupported) {
			t.Errorf("status route answering %d proves the route family is live; got %v", code, err)
		}
	}
}

// TestEnsureImage_404FromAMisroutedURLFailsClosed is the case the bare status
// code could not see, and the reason this finding exists.
func TestEnsureImage_404FromAMisroutedURLFailsClosed(t *testing.T) {
	stubResolveProvider(t, "")
	p := cpWithMissingEnsureImage(t, 0) // nothing under /cp/workspaces/* routes

	_, err := p.EnsureImage(context.Background(), EnsureImageRequest{WorkspaceID: "ws-misrouted", Runtime: "hermes"})
	if errors.Is(err, ErrEnsureImageUnsupported) {
		t.Fatal("a URL where NOTHING under /cp/workspaces/* routes is a misconfiguration, not an old " +
			"control plane. Reading it as version skew degrades the whole fleet to the pre-core#5019 " +
			"destroy-then-pull ordering while logging a compatibility skip — silent, fleet-wide, and " +
			"indistinguishable from working.")
	}
	if err == nil {
		t.Fatal("a misrouted control plane must not read as permission to destroy the container")
	}
	if !errors.Is(err, ErrEnsureImagePermanent) {
		t.Errorf("a misrouted URL will 404 again on every retry; it should be classified permanent, got %v", err)
	}
}

// TestEnsureImage_UnreachableHostIsNotACompatSkip — if the probe itself cannot
// connect, we have learned nothing that justifies failing open.
func TestEnsureImage_UnreachableHostIsNotACompatSkip(t *testing.T) {
	stubResolveProvider(t, "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	p := newEnsureImageTestProvisioner(srv.URL)
	srv.Close() // the 404 is delivered by nobody; both calls now fail at transport

	_, err := p.EnsureImage(context.Background(), EnsureImageRequest{WorkspaceID: "ws-gone", Runtime: "hermes"})
	if errors.Is(err, ErrEnsureImageUnsupported) {
		t.Fatal("an unreachable control plane is not a control plane without the endpoint")
	}
	if err == nil {
		t.Fatal("an unreachable control plane must not read as permission")
	}
}

// TestEnsureImage_501IsACompatSignalWithoutAProbe — 501 needs no corroboration:
// the request was ROUTED and the control plane answered "not implemented". That
// is already the positive signal, and spending a second round trip on it would
// only add a way to get it wrong.
func TestEnsureImage_501IsACompatSignalWithoutAProbe(t *testing.T) {
	stubResolveProvider(t, "")
	var probed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/status") {
			probed = true
		}
		w.WriteHeader(http.StatusNotImplemented)
		_, _ = w.Write([]byte(`{"error":"not implemented"}`))
	}))
	defer srv.Close()

	_, err := newEnsureImageTestProvisioner(srv.URL).
		EnsureImage(context.Background(), EnsureImageRequest{WorkspaceID: "ws-501", Runtime: "hermes"})
	if !errors.Is(err, ErrEnsureImageUnsupported) {
		t.Fatalf("501 is the control plane declining a route it HAS; got %v", err)
	}
	if probed {
		t.Error("501 needs no capability probe — it is already a routed, CP-emitted answer")
	}
}

// TestEnsureImage_RefusalsAreClassifiedPermanent pins the retry classification
// (core#5025 finding 4's other half): a control plane that ANSWERED must not be
// asked the same question twice while the restart gate is held.
func TestEnsureImage_RefusalsAreClassifiedPermanent(t *testing.T) {
	stubResolveProvider(t, "")
	cases := []struct {
		status    int
		permanent bool
	}{
		{http.StatusUnprocessableEntity, true}, // the core#5019 unobtainable digest
		{http.StatusBadRequest, true},
		{http.StatusUnauthorized, true},
		{http.StatusForbidden, true},
		{http.StatusRequestTimeout, false},  // explicitly "try again"
		{http.StatusTooManyRequests, false}, // explicitly "try again"
		{http.StatusInternalServerError, false},
		{http.StatusBadGateway, false},
		{http.StatusServiceUnavailable, false},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(`{"error":"nope"}`))
		}))
		_, err := newEnsureImageTestProvisioner(srv.URL).
			EnsureImage(context.Background(), EnsureImageRequest{WorkspaceID: "ws", Runtime: "hermes"})
		srv.Close()

		if err == nil {
			t.Fatalf("HTTP %d must not read as permission", tc.status)
		}
		if got := errors.Is(err, ErrEnsureImagePermanent); got != tc.permanent {
			t.Errorf("HTTP %d classified permanent=%v, want %v — retrying an answered refusal only "+
				"lengthens the window the restart gate is held; not retrying an unavailable control "+
				"plane refuses restarts fleet-wide during a redeploy", tc.status, got, tc.permanent)
		}
	}
}

// TestEnsureImage_404WithAnUnANSWERABLEProbeFailsClosed covers the probe's own
// failure, which is a DIFFERENT situation from the probe answering 404.
//
// A half-broken path — the ensure-image route answers 404 while the probe can
// not complete at all (connection dropped, TLS reset, a proxy that hangs up on
// the second request) — tells us nothing about whether this deployment has the
// endpoint. Reading "I could not check" as "old control plane" would restore the
// fail-open on exactly the transient conditions that produce spurious 404s.
func TestEnsureImage_404WithAnUnanswerableProbeFailsClosed(t *testing.T) {
	stubResolveProvider(t, "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/status") {
			// Hang up mid-request: the client gets a transport error, not a
			// status code.
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Error("test server does not support hijack")
				return
			}
			conn, _, err := hj.Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("404 page not found"))
	}))
	defer srv.Close()

	_, err := newEnsureImageTestProvisioner(srv.URL).
		EnsureImage(context.Background(), EnsureImageRequest{WorkspaceID: "ws-halfbroken", Runtime: "hermes"})
	if errors.Is(err, ErrEnsureImageUnsupported) {
		t.Fatal("a probe that could not complete is not evidence of version skew. \"I could not check\" " +
			"must never fail OPEN — that is the whole shape of core#5025 finding 5.")
	}
	if err == nil {
		t.Fatal("a 404 we could not corroborate must not read as permission to destroy the container")
	}
}

// TestEnsureImage_404WithNoProbeClientFailsClosed — an unwired HTTP client means
// the capability probe cannot run at all.
//
// The direction matters more than it looks: this is the one branch where the
// evidence is missing for a reason INSIDE this process, and a wiring bug that
// silently restored the fail-open would degrade the whole fleet with nothing
// external to blame it on. No evidence is not evidence of an old control plane.
func TestEnsureImage_404WithNoProbeClientFailsClosed(t *testing.T) {
	stubResolveProvider(t, "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("404 page not found"))
	}))
	defer srv.Close()

	p := newEnsureImageTestProvisioner(srv.URL)
	p.httpClient = nil // the probe has nothing to call with

	_, err := p.EnsureImage(context.Background(), EnsureImageRequest{WorkspaceID: "ws-unwired", Runtime: "hermes"})
	if errors.Is(err, ErrEnsureImageUnsupported) {
		t.Fatal("with no client to probe with, the 404 is uncorroborated and must fail CLOSED")
	}
	if err == nil {
		t.Fatal("an uncorroborated 404 must not read as permission")
	}
}
