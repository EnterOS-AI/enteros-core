package handlers

// Tests for the core#4548 docker-DNS-settle residual on the A2A routability
// gate. The bug: on the local-docker (ephemeral e2e) topology, the workspace
// URL is rewritten to the container hostname ws-<id>:8000; during a config-PUT
// restart flap net.LookupHost races docker-DNS (re)registration, isSafeURL
// returns a transient "DNS resolution blocked" error, and the A2A turn is
// dropped with a spurious 502 "workspace URL is not publicly routable".
//
// The fix (settleDockerInternalDNS) re-runs isSafeURL a bounded number of
// times to let docker-DNS settle. It is NOT a routability relaxation:
//
//   - It only re-attempts while the failure remains a *net.DNSError (a
//     resolution failure). A routability *rejection* (private/metadata/
//     link-local classification) is a plain error, does not match, and fails
//     closed immediately.
//   - It re-runs the REAL isSafeURL each attempt, so it can never fabricate
//     success — a blocked-classification URL stays blocked in every mode.
//   - The whole branch in resolveAgentURL is gated on
//     devModeAllowsLoopback() (MOLECULE_ENV=development|dev). Prod tenants run
//     MOLECULE_ENV=production and register by VPC-private IP (never the
//     127.0.0.1→ws-<id> rewrite), so the production SSRF/routability guard is
//     byte-for-byte unchanged (isSafeURL called exactly once, no retry).

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// shortenSettleForTest shrinks the DNS-settle bound so tests that exercise the
// bounded-failure path don't sleep the full ~1.5s production ceiling.
func shortenSettleForTest(t *testing.T, attempts int, delay time.Duration) {
	t.Helper()
	prevA, prevD := dockerDNSSettleAttempts, dockerDNSSettleDelay
	dockerDNSSettleAttempts = attempts
	dockerDNSSettleDelay = delay
	t.Cleanup(func() {
		dockerDNSSettleAttempts = prevA
		dockerDNSSettleDelay = prevD
	})
}

// TestIsDNSResolutionError_DistinguishesResolutionFromRejection is the
// linchpin of the security argument: the settle retry keys off this predicate,
// so it MUST return true only for transient resolution failures and false for
// every routability rejection. If it ever returned true for a rejection, the
// settle loop would re-attempt (and could mask) a real SSRF block.
func TestIsDNSResolutionError_DistinguishesResolutionFromRejection(t *testing.T) {
	// production => loopback/private/metadata all strict.
	t.Setenv("MOLECULE_ENV", "production")
	t.Setenv("MOLECULE_DEPLOY_MODE", "")
	t.Setenv("MOLECULE_ORG_ID", "")

	// Genuine resolution failure (unresolvable host, .invalid fails fast).
	dnsErr := isSafeURL("http://ws-nonexistent-4548.invalid:8000")
	if dnsErr == nil {
		t.Fatal("expected DNS resolution to fail for .invalid host")
	}
	if !isDNSResolutionError(dnsErr) {
		t.Errorf("expected a DNS-resolution error to be classified as such; got %v", dnsErr)
	}

	// Routability REJECTIONS must NOT be classified as resolution errors —
	// otherwise the settle loop would retry them. (negative control)
	for _, rawURL := range []string{
		"http://169.254.169.254/latest/meta-data/", // cloud metadata
		"http://10.0.0.1:8000",                      // RFC-1918 (strict)
		"http://192.168.1.10:8000",                  // RFC-1918 (strict)
		"http://127.0.0.1:8000",                     // loopback (strict)
		"http://100.64.0.1:8000",                    // CGNAT
		"http://192.0.2.5:8000",                     // TEST-NET
	} {
		rejErr := isSafeURL(rawURL)
		if rejErr == nil {
			t.Fatalf("expected %s to be REJECTED by isSafeURL in production", rawURL)
		}
		if isDNSResolutionError(rejErr) {
			t.Errorf("routability rejection for %s must NOT be classified as a DNS error (would enable retry-masking); got %v", rawURL, rejErr)
		}
	}
}

// TestSettleDockerInternalDNS_NeverMasksRoutabilityRejection is the security
// negative control: even in the MOST permissive env (MOLECULE_ENV=development,
// where loopback + RFC-1918 are relaxed), the settle wrapper must still return
// the rejection for always-blocked classes (metadata / TEST-NET / CGNAT),
// because it re-runs the real isSafeURL. This test MUST fail if someone
// weakens the settle path into blindly returning nil after retries.
func TestSettleDockerInternalDNS_NeverMasksRoutabilityRejection(t *testing.T) {
	t.Setenv("MOLECULE_ENV", "development") // most permissive posture
	shortenSettleForTest(t, 3, time.Millisecond)

	for _, rawURL := range []string{
		"http://169.254.169.254:8000", // cloud metadata — blocked in ALL modes
		"http://192.0.2.7:8000",       // TEST-NET — blocked in ALL modes
		"http://100.64.0.9:8000",      // CGNAT — blocked in ALL modes
	} {
		if err := settleDockerInternalDNS(rawURL); err == nil {
			t.Errorf("settleDockerInternalDNS(%s) must return a rejection even in dev mode; got nil (routability guard weakened!)", rawURL)
		}
	}
}

// TestSettleDockerInternalDNS_BoundedOnPersistentDNSFailure proves the retry
// is bounded (returns, does not hang) and does not fabricate success when the
// name never resolves.
func TestSettleDockerInternalDNS_BoundedOnPersistentDNSFailure(t *testing.T) {
	t.Setenv("MOLECULE_ENV", "development")
	shortenSettleForTest(t, 3, time.Millisecond)

	err := settleDockerInternalDNS("http://ws-doesnotexist-4548.invalid:8000")
	if err == nil {
		t.Fatal("expected persistent DNS failure to remain an error after bounded retries; got nil")
	}
	if !isDNSResolutionError(err) {
		t.Errorf("expected the terminal error to still be a DNS-resolution error; got %v", err)
	}
}

// TestSettleDockerInternalDNS_AcceptsResolvableDevTopologyURL is the flake-fix
// positive case: once the name resolves to the docker-bridge RFC-1918 address
// (modelled here by a literal 172.18.x.x, which is what ws-<id> resolves to on
// the docker network), the settle path accepts it in dev mode. The prod
// counterpart is the negative control: the identical URL is REJECTED under
// MOLECULE_ENV=production, proving the acceptance is scoped to the dev
// topology and the prod guard is intact.
func TestSettleDockerInternalDNS_AcceptsResolvableDevTopologyURL(t *testing.T) {
	const dockerBridgeURL = "http://172.18.0.9:8000" // RFC-1918, docker bridge

	t.Run("dev topology accepts", func(t *testing.T) {
		t.Setenv("MOLECULE_ENV", "development")
		shortenSettleForTest(t, 3, time.Millisecond)
		if err := settleDockerInternalDNS(dockerBridgeURL); err != nil {
			t.Errorf("dev topology must accept resolved docker-bridge URL %s; got %v", dockerBridgeURL, err)
		}
	})

	t.Run("prod REJECTS same URL (negative control)", func(t *testing.T) {
		t.Setenv("MOLECULE_ENV", "production")
		t.Setenv("MOLECULE_DEPLOY_MODE", "")
		t.Setenv("MOLECULE_ORG_ID", "")
		shortenSettleForTest(t, 3, time.Millisecond)
		if err := settleDockerInternalDNS(dockerBridgeURL); err == nil {
			t.Errorf("production must REJECT RFC-1918 URL %s; got nil (prod guard weakened!)", dockerBridgeURL)
		}
	})
}

// TestResolveAgentURL_ProdGuardIntact_NonRoutableStillRejected is the
// integration-level negative control: in production, the docker-hostname
// rewrite path (were it to occur) must NOT be rescued by the settle retry —
// devModeAllowsLoopback() is false, so resolveAgentURL calls isSafeURL exactly
// once and returns the same 502 it does today. Uses a .invalid workspace id so
// the single real DNS lookup fails fast.
func TestResolveAgentURL_ProdGuardIntact_NonRoutableStillRejected(t *testing.T) {
	// Assert the gate signal itself: prod can never satisfy devModeAllowsLoopback.
	t.Setenv("MOLECULE_ENV", "production")
	t.Setenv("MOLECULE_DEPLOY_MODE", "")
	t.Setenv("MOLECULE_ORG_ID", "")
	if devModeAllowsLoopback() {
		t.Fatal("devModeAllowsLoopback() must be false in production — the settle branch would otherwise be reachable in prod")
	}

	mock := setupTestDB(t)
	// setupTestDB DISABLES the SSRF check; re-enable it so this test drives the
	// REAL production routability guard (that is the whole point of the control).
	setSSRFCheckForTest(true)
	t.Cleanup(func() { setSSRFCheckForTest(false)() })
	mr := setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	waitForHandlerAsyncBeforeDBCleanup(t, handler)
	handler.provisioner = &stubLocalProv{}

	restore := setPlatformInDockerForTest(true)
	defer restore()

	// If the settle branch were (wrongly) reachable in prod this large delay
	// would blow the test's time budget; because it is gated out, isSafeURL is
	// called exactly once and this returns immediately.
	shortenSettleForTest(t, 5, 10*time.Second)

	const wsID = "settle4548prod.invalid" // ws-<id> => ws-settle4548prod.invalid (fast NXDOMAIN)
	mr.Set("ws:"+wsID+":url", "http://127.0.0.1:55555")
	mock.ExpectQuery(`SELECT COALESCE\(runtime`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"runtime"}).AddRow("claude-code"))

	start := time.Now()
	_, perr := handler.resolveAgentURL(context.Background(), wsID)
	elapsed := time.Since(start)

	if perr == nil {
		t.Fatal("expected 502 for non-routable rewritten URL in production; got nil")
	}
	if perr.Status != http.StatusBadGateway {
		t.Errorf("got status %d, want 502", perr.Status)
	}
	if elapsed > 3*time.Second {
		t.Errorf("production path took %v — the settle retry appears to have run in prod (must be gated out)", elapsed)
	}
}

// TestResolveAgentURL_DevTopologySettleWired proves the settle branch IS wired
// into resolveAgentURL on the dev-docker topology: with MOLECULE_ENV=development
// the branch is entered (bounded retries) and, when the container name never
// resolves, still fails closed with a 502 — i.e. the retry reduces flakiness
// without ever fabricating routability. (In the real ephemeral e2e the name
// resolves within the window and the call succeeds; that success path needs a
// live docker network and is covered by the e2e itself.)
func TestResolveAgentURL_DevTopologySettleWired(t *testing.T) {
	t.Setenv("MOLECULE_ENV", "development")
	if !devModeAllowsLoopback() {
		t.Fatal("precondition: devModeAllowsLoopback() must be true under MOLECULE_ENV=development")
	}

	mock := setupTestDB(t)
	// setupTestDB DISABLES the SSRF check; re-enable it so the settle branch
	// actually runs against the real routability guard.
	setSSRFCheckForTest(true)
	t.Cleanup(func() { setSSRFCheckForTest(false)() })
	mr := setupTestRedis(t)
	handler := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	waitForHandlerAsyncBeforeDBCleanup(t, handler)
	handler.provisioner = &stubLocalProv{}

	restore := setPlatformInDockerForTest(true)
	defer restore()

	shortenSettleForTest(t, 3, time.Millisecond)

	const wsID = "settle4548dev.invalid"
	mr.Set("ws:"+wsID+":url", "http://127.0.0.1:55556")
	mock.ExpectQuery(`SELECT COALESCE\(runtime`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"runtime"}).AddRow("claude-code"))

	_, perr := handler.resolveAgentURL(context.Background(), wsID)
	if perr == nil {
		t.Fatal("expected 502 when the docker hostname never resolves; got nil (settle must not fabricate success)")
	}
	if perr.Status != http.StatusBadGateway {
		t.Errorf("got status %d, want 502", perr.Status)
	}
}
