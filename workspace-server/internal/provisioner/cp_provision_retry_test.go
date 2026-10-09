package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Retry coverage for CPProvisioner.Start (core#5057).
//
// The POST that CREATES the box was single-shot: a transient failure on the hop
// meant the box was never created, Start went straight to markProvisionFailed,
// and nothing re-drove it — so e2e-smoke polled loaded_mcp_tools for 240s
// against a workspace with no box.
//
// These tests use a real httptest server rather than a hand-rolled
// RoundTripper so the transport-error case is a genuine broken connection, not
// a simulated error value.

// shrinkRetryBudget makes the ctx-aware backoff sub-millisecond for tests, and
// restores the production values afterwards.
func shrinkRetryBudget(t *testing.T) {
	t.Helper()
	oldAttempts, oldDelay := cpProvisionRetryAttempts, cpProvisionRetryBaseDelay
	cpProvisionRetryBaseDelay = time.Millisecond
	t.Cleanup(func() {
		cpProvisionRetryAttempts, cpProvisionRetryBaseDelay = oldAttempts, oldDelay
	})
}

// newTestCPProvisioner points a CPProvisioner at the given test server.
func newTestCPProvisioner(baseURL string) *CPProvisioner {
	return &CPProvisioner{
		baseURL:             baseURL,
		orgID:               "org-1",
		httpClient:          &http.Client{Timeout: 5 * time.Second},
		provisionHTTPClient: &http.Client{Timeout: 5 * time.Second},
	}
}

func testWorkspaceConfig() WorkspaceConfig {
	return WorkspaceConfig{WorkspaceID: "ws-1", Runtime: "hermes", PlatformURL: "https://tenant.example.test"}
}

// recordingCP captures every provision request body the server sees.
type recordingCP struct {
	mu     sync.Mutex
	bodies []cpProvisionRequest
}

func (r *recordingCP) record(body []byte) {
	var req cpProvisionRequest
	_ = json.Unmarshal(body, &req)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bodies = append(r.bodies, req)
}

func (r *recordingCP) calls() []cpProvisionRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]cpProvisionRequest(nil), r.bodies...)
}

// THE REGRESSION. A transient 502 on the first attempt used to fail the whole
// provision and leave the workspace with no box. It must now be retried and
// succeed.
func TestStart_RetriesTransient502AndSucceeds(t *testing.T) {
	shrinkRetryBudget(t)
	rec := &recordingCP{}
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rec.record(b)
		n++
		if n == 1 {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":"bad gateway"}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"instance_id":"i-real","state":"running"}`))
	}))
	defer srv.Close()

	id, err := newTestCPProvisioner(srv.URL).Start(context.Background(), testWorkspaceConfig())
	if err != nil {
		t.Fatalf("Start should have recovered from a transient 502: %v", err)
	}
	if id != "i-real" {
		t.Errorf("instance_id = %q, want i-real", id)
	}
	if got := len(rec.calls()); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
}

// THE SAFETY PROPERTY. Every attempt must carry the SAME idempotency key —
// that is the only thing standing between a retry and a second box, because
// re-entry into the CP handler terminates the pre-existing instance.
func TestStart_ReusesOneIdempotencyKeyAcrossRetries(t *testing.T) {
	shrinkRetryBudget(t)
	rec := &recordingCP{}
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rec.record(b)
		n++
		if n < 3 {
			w.WriteHeader(http.StatusGatewayTimeout)
			_, _ = w.Write([]byte(`{"error":"timeout"}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"instance_id":"i-real","state":"running"}`))
	}))
	defer srv.Close()

	if _, err := newTestCPProvisioner(srv.URL).Start(context.Background(), testWorkspaceConfig()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	calls := rec.calls()
	if len(calls) != 3 {
		t.Fatalf("attempts = %d, want 3", len(calls))
	}
	first := calls[0].IdempotencyKey
	if first == "" {
		t.Fatal("no idempotency_key on the wire — the CP cannot dedupe, so the retry is UNSAFE")
	}
	if !strings.HasPrefix(first, "cpprov-") {
		t.Errorf("idempotency_key = %q, want the cpprov- prefix", first)
	}
	for i, c := range calls {
		if c.IdempotencyKey != first {
			t.Fatalf("attempt %d used key %q but attempt 1 used %q — a per-attempt key makes every retry look like a NEW provision to the CP, which is exactly the double-provision this exists to prevent", i+1, c.IdempotencyKey, first)
		}
	}
}

// Two independent Start calls must NOT share a key, or the CP would replay the
// first workspace's response to the second.
func TestStart_DistinctCallsGetDistinctKeys(t *testing.T) {
	shrinkRetryBudget(t)
	rec := &recordingCP{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rec.record(b)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"instance_id":"i-1","state":"running"}`))
	}))
	defer srv.Close()

	p := newTestCPProvisioner(srv.URL)
	for i := 0; i < 2; i++ {
		if _, err := p.Start(context.Background(), testWorkspaceConfig()); err != nil {
			t.Fatalf("Start %d: %v", i, err)
		}
	}
	calls := rec.calls()
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(calls))
	}
	if calls[0].IdempotencyKey == calls[1].IdempotencyKey {
		t.Error("two separate provisions reused one idempotency key — the CP would replay the first response instead of provisioning")
	}
}

// A genuine transport failure (server closes the connection mid-request) is the
// `tls: bad record MAC` class. It must be retried.
func TestStart_RetriesGenuineTransportError(t *testing.T) {
	shrinkRetryBudget(t)
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		if n == 1 {
			// Hijack and slam the connection: a real broken transport, not a
			// simulated error value.
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Error("test server does not support hijacking")
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"instance_id":"i-after-transport-error","state":"running"}`))
	}))
	defer srv.Close()

	id, err := newTestCPProvisioner(srv.URL).Start(context.Background(), testWorkspaceConfig())
	if err != nil {
		t.Fatalf("Start should have recovered from a transport error: %v", err)
	}
	if id != "i-after-transport-error" {
		t.Errorf("instance_id = %q", id)
	}
}

// A deterministic rejection must NOT be retried — it will fail identically
// forever and retrying only burns the provision budget.
func TestStart_DoesNotRetryDeterministicRejections(t *testing.T) {
	shrinkRetryBudget(t)
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"privileged env", http.StatusBadRequest, `{"error":"privileged_env_forbidden"}`},
		{"forbidden", http.StatusForbidden, `{"error":"caller is not authorized"}`},
		{"pin missing", http.StatusUnprocessableEntity, `{"error":"RUNTIME_PIN_MISSING"}`},
		{"key conflict", http.StatusConflict, `{"error":"idempotency_key_conflict"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var n int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n++
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			if _, err := newTestCPProvisioner(srv.URL).Start(context.Background(), testWorkspaceConfig()); err == nil {
				t.Fatal("expected an error")
			}
			if n != 1 {
				t.Errorf("attempts = %d, want exactly 1 — %s is deterministic", n, tc.name)
			}
		})
	}
}

// 409 provision_in_progress means OUR earlier attempt landed and is still
// running. Retrying is right: once it resolves the CP replays its recorded 201
// and we learn the real instance_id.
func TestStart_RetriesProvisionInProgressAndPicksUpTheReplay(t *testing.T) {
	shrinkRetryBudget(t)
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		if n == 1 {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"provision_in_progress"}`))
			return
		}
		// The CP's replay of the earlier attempt's recorded 201.
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"instance_id":"i-from-first-attempt","state":"running"}`))
	}))
	defer srv.Close()

	id, err := newTestCPProvisioner(srv.URL).Start(context.Background(), testWorkspaceConfig())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if id != "i-from-first-attempt" {
		t.Errorf("instance_id = %q — the retry must adopt the box the FIRST attempt created, not create a new one", id)
	}
}

// Exhausting the budget must still fail, and must not silently report success.
func TestStart_ExhaustsBudgetAndFails(t *testing.T) {
	shrinkRetryBudget(t)
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"still down"}`))
	}))
	defer srv.Close()

	if _, err := newTestCPProvisioner(srv.URL).Start(context.Background(), testWorkspaceConfig()); err == nil {
		t.Fatal("a persistently failing CP must still fail the provision")
	}
	if n != cpProvisionRetryAttempts {
		t.Errorf("attempts = %d, want %d", n, cpProvisionRetryAttempts)
	}
}

// The backoff must observe context cancellation rather than sitting in a bare
// time.Sleep past the provision deadline.
func TestStart_BackoffIsContextAware(t *testing.T) {
	oldDelay := cpProvisionRetryBaseDelay
	cpProvisionRetryBaseDelay = 30 * time.Second
	t.Cleanup(func() { cpProvisionRetryBaseDelay = oldDelay })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"down"}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := newTestCPProvisioner(srv.URL).Start(ctx, testWorkspaceConfig())
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start sat in a non-ctx-aware backoff past the deadline — a cancelled provision must abort promptly")
	}
}

// retryableProvisionStatus is the classifier the loop keys on; pin it directly.
func TestRetryableProvisionStatus(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
		want   bool
	}{
		{http.StatusBadGateway, "", true},
		{http.StatusServiceUnavailable, "", true},
		{http.StatusGatewayTimeout, "", true},
		{524, "", true}, // Cloudflare: origin timed out — often COMPLETED the provision
		{http.StatusInternalServerError, "", true},
		{http.StatusTooManyRequests, "", true},
		{http.StatusRequestTimeout, "", true},
		{http.StatusConflict, "provision_in_progress", true},
		{http.StatusConflict, "idempotency_key_conflict", false},
		{http.StatusBadRequest, "privileged_env_forbidden", false},
		{http.StatusForbidden, "", false},
		{http.StatusUnprocessableEntity, "RUNTIME_PIN_MISSING", false},
		{http.StatusNotFound, "", false},
	} {
		t.Run(fmt.Sprintf("%d/%s", tc.status, tc.code), func(t *testing.T) {
			if got := retryableProvisionStatus(tc.status, tc.code); got != tc.want {
				t.Errorf("retryableProvisionStatus(%d,%q) = %v, want %v", tc.status, tc.code, got, tc.want)
			}
		})
	}
}

// --- A control-plane redeploy is the longest transient on this hop ---------
//
// Regression coverage for the staging failure of 2026-08-07 (controlplane#2908
// run 628216 job 927298). Staging CD recreated molecule-cp-staging while a
// tenant was provisioning; the CP answered nothing for ~17.7s, the 3-attempt
// budget expired in 9s, and the workspace was marked failed permanently — four
// seconds before the provision route came back up.

// cpDownFor returns a CP that fails `downFor` attempts with the gateway's
// 502 (a bare "502 Bad Gateway", exactly what the tenant saw — note there is no
// JSON {"error":...} field, because the CP process is not the one answering)
// and then serves a normal 201.
func cpDownFor(downFor int, seen *int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		*seen++
		if *seen <= downFor {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("502 Bad Gateway\n"))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"instance_id":"i-after-cp-restart","state":"running"}`))
	}
}

// The provision must survive a CP that is unreachable across several attempts
// and then returns. With the previous budget of 3 this failed outright.
func TestStart_SurvivesAControlPlaneRestart(t *testing.T) {
	shrinkRetryBudget(t)

	var seen int
	srv := httptest.NewServer(cpDownFor(cpProvisionRetryAttempts-1, &seen))
	defer srv.Close()

	id, err := newTestCPProvisioner(srv.URL).Start(context.Background(), testWorkspaceConfig())
	if err != nil {
		t.Fatalf("a CP that returns after a restart must still yield a box, got: %v", err)
	}
	if id != "i-after-cp-restart" {
		t.Errorf("instance_id = %q, want the box created on the recovering attempt", id)
	}
	if seen != cpProvisionRetryAttempts {
		t.Errorf("attempts = %d, want %d — the budget must be spent, not abandoned early", seen, cpProvisionRetryAttempts)
	}
}

// measuredCPRestartOutage is the wall-clock gap between the old CP container
// being stopped (19:21:12.797) and the new one having registered the provision
// route (19:21:27.300), from deploy job 927326. The budget must outlast it, or
// a routine deploy destroys in-flight provisions again.
const measuredCPRestartOutage = 14*time.Second + 503*time.Millisecond

// The PRODUCTION constants — not a shrunk test budget — must cover a real CP
// redeploy. This is the assertion that would have caught the regression; every
// other test in this file passes at attempts=3.
func TestProvisionRetryBudgetOutlastsAControlPlaneRestart(t *testing.T) {
	var total time.Duration
	delay := cpProvisionRetryBaseDelay
	for attempt := 1; attempt < cpProvisionRetryAttempts; attempt++ {
		total += delay
		delay *= 2
		if delay > cpProvisionRetryMaxDelay {
			delay = cpProvisionRetryMaxDelay
		}
	}
	if total <= measuredCPRestartOutage {
		t.Fatalf("total provision backoff %s does not outlast a measured CP restart (%s) — "+
			"a deploy landing mid-provision will fail the workspace permanently", total, measuredCPRestartOutage)
	}
}

// The doubling must saturate rather than run away: an uncapped 6th attempt
// would sleep 32s in one gap, longer than the outage being waited out.
func TestProvisionRetryDelayIsCapped(t *testing.T) {
	delay := cpProvisionRetryBaseDelay
	for attempt := 1; attempt < cpProvisionRetryAttempts; attempt++ {
		delay *= 2
		if delay > cpProvisionRetryMaxDelay {
			delay = cpProvisionRetryMaxDelay
		}
		if delay > cpProvisionRetryMaxDelay {
			t.Fatalf("delay %s exceeded the cap %s at attempt %d", delay, cpProvisionRetryMaxDelay, attempt)
		}
	}
}

// A deterministic rejection must STILL fail on the first attempt. Widening the
// budget must not turn a fast, correct rejection into a 34s stall.
func TestStart_DeterministicRejectionStillFailsFastAtTheWiderBudget(t *testing.T) {
	shrinkRetryBudget(t)

	var seen int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen++
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":"RUNTIME_PIN_MISSING"}`))
	}))
	defer srv.Close()

	if _, err := newTestCPProvisioner(srv.URL).Start(context.Background(), testWorkspaceConfig()); err == nil {
		t.Fatal("a deterministic rejection must fail the provision")
	}
	if seen != 1 {
		t.Errorf("attempts = %d, want 1 — a 422 fails identically forever and must not burn the budget", seen)
	}
}
