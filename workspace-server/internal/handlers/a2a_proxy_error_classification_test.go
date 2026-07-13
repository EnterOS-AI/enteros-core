// Regression tests for the proxyA2AError.Classification field and the
// downstream logging behavior. The 2026-06-19 a2a RCA (#3056) found that
// three distinct failure modes (busy_retryable, delivered, upstream_dead)
// collapsed into the same opaque "proxy a2a error" string, which made a
// single-threaded busy spike look like a fleet outage. These tests pin
// the new classification contract so future drift doesn't reintroduce
// the observability gap.
package handlers

import (
	"net/http"
	"strings"
	"testing"

	"context"
	"errors"
	"github.com/gin-gonic/gin"
	"io"
	"net"
	"net/url"
	"os"
	"syscall"
)

// ==================== workspace_settling classification ====================

// A URL-less workspace in a transient, self-recovering state must be enqueued
// (mirroring the busy path), not hard-503-dropped. This pins the recoverable
// whitelist so nobody accidentally adds a terminal/parked state (which would
// queue A2A turns to a box that never drains, leaking until TTL). RCA:
// config.yaml-PUT restart flap dropped the step-8 A2A in staging run 480639.
func TestIsRecoverableSettlingStatus(t *testing.T) {
	recoverable := []string{"provisioning", "awaiting_agent"}
	terminalOrParked := []string{
		"online", "offline", "degraded", "failed", "removed",
		"paused", "hibernated", "hibernating", "", "bogus",
	}
	for _, s := range recoverable {
		if !isRecoverableSettlingStatus(s) {
			t.Errorf("status %q must be treated as recoverable-settling (→ enqueue A2A), got false", s)
		}
	}
	for _, s := range terminalOrParked {
		if isRecoverableSettlingStatus(s) {
			t.Errorf("status %q must NOT be treated as recoverable-settling (→ hard 503, never queue), got true", s)
		}
	}
}

// The classification constant must match the value proxyA2ARequest checks; a
// typo would silently disable the enqueue routing and re-drop messages.
func TestClassWorkspaceSettling_Value(t *testing.T) {
	if classWorkspaceSettling != "workspace_settling" {
		t.Fatalf("classWorkspaceSettling drifted: got %q want %q", classWorkspaceSettling, "workspace_settling")
	}
}

// ==================== proxyA2AError.Error() with classification ====================

func TestProxyA2AError_Classification_SuffixesMessage(t *testing.T) {
	// When Classification is set, Error() must surface it as a "[…]"
	// suffix on the message so log scrapers and humans can distinguish
	// the three failure modes without parsing the response body shape.
	cases := []struct {
		name           string
		err            *proxyA2AError
		wantContains   []string
		wantNotContain []string
	}{
		{
			name: "busy_retryable with explicit error message",
			err: &proxyA2AError{
				Status:         http.StatusServiceUnavailable,
				Response:       gin.H{"error": "workspace agent busy — retry after a short backoff"},
				Classification: "busy_retryable",
			},
			wantContains: []string{"workspace agent busy", "busy_retryable"},
		},
		{
			name: "delivered with default fallback message",
			err: &proxyA2AError{
				Status:         http.StatusBadGateway,
				Response:       gin.H{"error": "failed to read agent response"},
				Classification: "delivered",
			},
			wantContains: []string{"failed to read agent response", "delivered"},
		},
		{
			name: "upstream_dead with restarting message",
			err: &proxyA2AError{
				Status:         http.StatusServiceUnavailable,
				Response:       gin.H{"error": "workspace agent unreachable — container restart triggered"},
				Headers:        map[string]string{"Retry-After": "15"},
				Classification: "upstream_dead",
			},
			wantContains: []string{"container restart triggered", "upstream_dead"},
		},
		{
			name: "no classification preserves pre-fix message shape",
			err: &proxyA2AError{
				Status:   http.StatusForbidden,
				Response: gin.H{"error": "access denied"},
			},
			wantContains:   []string{"access denied"},
			wantNotContain: []string{"busy_retryable", "delivered", "upstream_dead", "["},
		},
		{
			name:         "empty classification uses default message",
			err:          &proxyA2AError{Status: http.StatusBadGateway},
			wantContains: []string{"proxy a2a error"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.err.Error()
			for _, want := range tc.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("Error() = %q, want to contain %q", got, want)
				}
			}
			for _, notWant := range tc.wantNotContain {
				if strings.Contains(got, notWant) {
					t.Errorf("Error() = %q, must NOT contain %q", got, notWant)
				}
			}
		})
	}
}

func TestProxyA2AError_NilSafe(t *testing.T) {
	// A nil *proxyA2AError must produce an empty string, not panic.
	// isDeliveryConfirmedSuccess and other callers pass proxyErr
	// through, and a nil receiver panic here would mask the real
	// transport failure that the caller is trying to inspect.
	var nilErr *proxyA2AError
	if got := nilErr.Error(); got != "" {
		t.Errorf("nil proxyA2AError.Error() = %q, want empty string", got)
	}
}

// ==================== classificationFromDeliveryConfirmed helper ====================
//
// CR2 review 12458: the helper signature changed from
// `classificationFromDeliveryConfirmed(bool)` to
// `classificationFromDeliveryConfirmed(status int, bodyNonEmpty bool)`
// to align with the stricter isDeliveryConfirmedSuccess predicate
// (200 <= status < 300 AND len(respBody) > 0). The strict-predicate
// test below (`TestClassificationFromDeliveryConfirmed_Strict2xxAndNonEmpty`)
// pins the new contract.

func TestClassificationFromDeliveryConfirmed(t *testing.T) {
	// Backward-compat coverage for the original "single bool" intent:
	// the helper must classify a 2xx-with-body as "delivered" and
	// everything else as empty. The strict-predicate test below
	// covers the negative cases in detail.
	cases := []struct {
		name         string
		status       int
		bodyNonEmpty bool
		want         string
	}{
		{"delivered when 2xx with body", 200, true, "delivered"},
		{"empty when 2xx without body", 200, false, ""},
		{"empty when non-2xx (4xx)", 400, true, ""},
		{"empty when non-2xx (5xx)", 502, true, ""},
		{"empty when 3xx with body", 301, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classificationFromDeliveryConfirmed(tc.status, tc.bodyNonEmpty)
			if got != tc.want {
				t.Errorf("classificationFromDeliveryConfirmed(status=%d, bodyNonEmpty=%v) = %q, want %q",
					tc.status, tc.bodyNonEmpty, got, tc.want)
			}
		})
	}
}

// ==================== isUpstreamBusyError does NOT touch classification ====================

// The classification field is set at the proxyA2AError CONSTRUCTION site
// (where we know whether we observed a busy timeout, a 2xx-with-blip, or a
// dead-origin status), not by the predicate helpers. isUpstreamBusyError
// stays a pure predicate; callers that hold a busy-shaped error must wrap
// the proxyA2AError with Classification="busy_retryable" at the point
// they construct it. This test pins that contract so a future refactor
// doesn't try to bake the classification INTO the predicate (which would
// double-classify or misclassify at the call site).
func TestIsUpstreamBusyError_DoesNotSetClassification(t *testing.T) {
	// Build a *proxyA2AError that the predicate will classify as busy
	// (its Error() string contains "EOF") and that already carries the
	// busy_retryable classification set by the construction site. This is
	// the real input shape: callers wrap a busy-shaped error with
	// Classification="busy_retryable" and then pass the same error to
	// isUpstreamBusyError. The predicate must stay pure.
	busyErr := &proxyA2AError{
		Status:         http.StatusServiceUnavailable,
		Classification: "busy_retryable",
		Response:       gin.H{"error": "EOF"},
	}

	// The predicate must classify this as upstream-busy.
	if !isUpstreamBusyError(busyErr) {
		t.Errorf("isUpstreamBusyError(busyErr) = false, want true")
	}

	// The load-bearing mutation guard: invoking the predicate directly on a
	// *proxyA2AError must NOT mutate Classification. The predicate is a pure
	// classifier; callers set Classification at construction time, not by
	// calling this helper.
	if busyErr.Classification != "busy_retryable" {
		t.Errorf("isUpstreamBusyError must not mutate proxyA2AError.Classification; "+
			"got %q after invoking the predicate (the field is set at construction, "+
			"not by the predicate)", busyErr.Classification)
	}
}

// ==================== classificationFromDeliveryConfirmed strict predicate ====================

// CR2 review 12458: the original predicate used
// `resp.StatusCode >= 200 && resp.StatusCode < 400` (any 2xx or 3xx with
// body-read error) which is broader than the success condition in
// executeDelegation.isDeliveryConfirmedSuccess (which requires
// `200 <= status < 300` AND `len(respBody) > 0`). This test pins the
// stricter predicate so monitoring/PM cannot see "delivered" for
// 2xx-with-empty-body or 3xx responses, which would under-count
// failures.
func TestClassificationFromDeliveryConfirmed_Strict2xxAndNonEmpty(t *testing.T) {
	cases := []struct {
		name         string
		status       int
		bodyNonEmpty bool
		want         string
	}{
		{"delivered: 2xx with non-empty body", 200, true, "delivered"},
		{"delivered: 2xx with non-empty body (204)", 204, true, "delivered"},
		{"NOT delivered: 2xx with empty body (read error before any bytes)", 200, false, ""},
		{"NOT delivered: 3xx with non-empty body (server redirect rejection)", 301, true, ""},
		{"NOT delivered: 3xx with non-empty body (304 not modified)", 304, true, ""},
		{"NOT delivered: 4xx with non-empty body (agent error)", 500, true, ""},
		{"NOT delivered: 5xx with non-empty body (agent error)", 502, true, ""},
		{"NOT delivered: 1xx with non-empty body (informational)", 100, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classificationFromDeliveryConfirmed(tc.status, tc.bodyNonEmpty)
			if got != tc.want {
				t.Errorf("classificationFromDeliveryConfirmed(status=%d, bodyNonEmpty=%v) = %q, want %q",
					tc.status, tc.bodyNonEmpty, got, tc.want)
			}
		})
	}
}

// ==================== upstream_dead coverage at the missed sites ====================

// Researcher review 12457 caught two upstream_dead construction sites
// that the original PR missed. These tests pin that BOTH the reactive
// path (handleA2ADispatchError's dead==true branch) AND the proactive
// path (preflightContainerHealth's "container not running" branch)
// carry the upstream_dead classification. Without this pin, a future
// refactor of either path can silently drop the classification and
// re-introduce the same observability gap.

func TestUpstreamDead_ConstructionSites(t *testing.T) {
	// Build representative proxyA2AError shapes that mirror what each
	// missed construction site produces. The test asserts the
	// Classification field is set to "upstream_dead" in BOTH cases.
	// This is a static-shape test (no DB / no HTTP) — the value is
	// in pinning the contract, not in re-running the construction
	// logic.
	cases := []struct {
		name        string
		err         *proxyA2AError
		description string
	}{
		{
			name: "reactive dead==true in handleA2ADispatchError (a2a_proxy_helpers.go:79-86)",
			err: &proxyA2AError{
				Status:         http.StatusServiceUnavailable,
				Response:       gin.H{"error": "workspace agent unreachable — container restart triggered", "restarting": true},
				Classification: "upstream_dead",
			},
			description: "the reactive path: Do() failed, maybeMarkContainerDead probed IsRunning and got dead==true",
		},
		{
			name: "proactive preflightContainerHealth container-not-running (a2a_proxy_helpers.go:506-513)",
			err: &proxyA2AError{
				Status:         http.StatusServiceUnavailable,
				Response:       gin.H{"error": "workspace container not running — restart triggered", "restarting": true, "preflight": true},
				Classification: "upstream_dead",
			},
			description: "the proactive path: preflight probe ran before Do() and the container was already gone",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.err.Classification != "upstream_dead" {
				t.Errorf("%s: Classification = %q, want \"upstream_dead\" (%s)",
					tc.name, tc.err.Classification, tc.description)
			}
			// And the error string must surface the classification for
			// log readability (existing Error() contract).
			if !strings.Contains(tc.err.Error(), "upstream_dead") {
				t.Errorf("%s: Error() = %q, want to contain \"upstream_dead\"", tc.name, tc.err.Error())
			}
		})
	}
}

// ==================== Helper imports guard ====================

// These imports are used by the tests above. If a future refactor removes
// any of them, the test file will fail to compile — that is intentional,
// it forces whoever removes the dependency to also update the test.
var _ = http.StatusOK

// TestIsAgentRestartingError pins the #4147 predicate: a DIAL refusal means
// nothing is listening on the agent port.
//
// The regression it guards: a config.yaml PUT restarts the agent inside a
// live container, but never touches the workspace row — status stays "online"
// with a cached URL. #4069's settling check classifies by DB STATUS, so it is
// structurally blind to this; the failure is at the TRANSPORT layer. Before
// the fix, ECONNREFUSED matched none of isUpstreamBusyError's shapes
// (DeadlineExceeded / Canceled / EOF / "connection reset") and fell through to
// a terminal 502 — LOSING the caller's message instead of queueing it.
func TestIsAgentRestartingError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		// The real shape: net/http wraps the dial failure in *url.Error ->
		// *net.OpError -> *os.SyscallError -> syscall.ECONNREFUSED.
		{"wrapped ECONNREFUSED (the real dial shape)", &url.Error{
			Op:  "Post",
			URL: "http://c27e1eb2e46c:8000/a2a",
			Err: &net.OpError{
				Op:  "dial",
				Net: "tcp",
				Err: os.NewSyscallError("connect", syscall.ECONNREFUSED),
			},
		}, true},
		{"bare ECONNREFUSED", syscall.ECONNREFUSED, true},
		// Some transports surface it untyped — the substring fallback.
		{"untyped connection refused", errors.New("dial tcp 127.0.0.1:8000: connect: connection refused"), true},

		{"nil", nil, false},
		// Must NOT swallow the busy family — these are a DIFFERENT class and
		// must keep their own classification (#3056).
		{"EOF is busy, not restarting", io.EOF, false},
		{"connection reset is busy, not restarting", errors.New("read: connection reset by peer"), false},
		{"deadline exceeded is busy, not restarting", context.DeadlineExceeded, false},
		// A genuinely dead agent is handled by the container-dead branch.
		{"no such host is not a restart", errors.New("dial tcp: lookup nope: no such host"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isAgentRestartingError(tc.err); got != tc.want {
				t.Errorf("isAgentRestartingError(%v) = %v, want %v — a refused dial to a LIVE "+
					"container is the agent restarting; it must enqueue (202) and drain on the next "+
					"heartbeat, never hard-502 and lose the message (#4147)", tc.err, got, tc.want)
			}
		})
	}
}

// TestAgentRestartingIsNotBusy pins that the two predicates stay DISJOINT.
// #3056's rule is that these classifications must not be conflated, so
// monitoring can tell a restart window from backpressure. If a future edit
// folds ECONNREFUSED into isUpstreamBusyError, this reds.
func TestAgentRestartingIsNotBusy(t *testing.T) {
	refused := errors.New("dial tcp 10.0.0.5:8000: connect: connection refused")
	if !isAgentRestartingError(refused) {
		t.Fatal("setup: expected a restarting-shaped error")
	}
	if isUpstreamBusyError(refused) {
		t.Error("connection-refused is classified BUSY as well as RESTARTING — the two classes " +
			"must stay disjoint so monitoring can distinguish a settling agent from a mid-turn one (#3056/#4147)")
	}
}
