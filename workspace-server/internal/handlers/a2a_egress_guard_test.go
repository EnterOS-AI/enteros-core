package handlers

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// A2A targets are workspace-controlled. A safe URL may redirect after the
// front-door isSafeURL check, so the shared client must never chase redirects
// with the platform-inbound bearer attached.
func TestA2AClientDoesNotFollowRedirects(t *testing.T) {
	allowLoopbackForTest(t)

	var redirectTargetHits atomic.Int32
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectTargetHits.Add(1)
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("redirect target received Authorization %q", got)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(redirectTarget.Close)

	safeOrigin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTarget.URL+"/private", http.StatusFound)
	}))
	t.Cleanup(safeOrigin.Close)

	req, err := http.NewRequest(http.MethodPost, safeOrigin.URL+"/a2a", bytes.NewBufferString(`{"jsonrpc":"2.0"}`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer platform-inbound-secret")

	resp, err := a2aClient.Do(req)
	if err != nil {
		t.Fatalf("A2A request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want original 302 response", resp.StatusCode)
	}
	if hits := redirectTargetHits.Load(); hits != 0 {
		t.Fatalf("redirect target hit %d times, want 0", hits)
	}
}

// The preflight URL check and TCP dial are separate DNS resolutions. Pin the
// second boundary: the transport itself must reject a forbidden post-resolution
// address before a connection is established.
func TestA2AClientDialTimeGuardRejectsLoopback(t *testing.T) {
	t.Setenv("MOLECULE_ENV", "production")
	previousLoopback := testAllowLoopback
	previousSSRFCheck := ssrfCheckEnabled
	testAllowLoopback = false
	ssrfCheckEnabled = true
	t.Cleanup(func() {
		testAllowLoopback = previousLoopback
		ssrfCheckEnabled = previousSSRFCheck
	})

	localTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("dial-time SSRF guard allowed a request to loopback")
	}))
	t.Cleanup(localTarget.Close)

	transport, ok := a2aClient.Transport.(*http.Transport)
	if !ok || transport.DialContext == nil {
		t.Fatalf("A2A transport has no explicit DialContext guard")
	}
	address := localTarget.Listener.Addr().String()
	conn, err := transport.DialContext(t.Context(), "tcp", address)
	if conn != nil {
		_ = conn.Close()
	}
	if err == nil {
		t.Fatalf("dial to loopback %s succeeded, want SSRF rejection", address)
	}
	var blocked *ssrfDialError
	if !errors.As(err, &blocked) {
		t.Fatalf("dial error = %T %v, want *ssrfDialError", err, err)
	}
}
