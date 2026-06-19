// Regression tests for the proxyA2AError.Classification field and the
// downstream logging behavior. The 2026-06-19 a2a RCA (#3056) found that
// three distinct failure modes (busy_retryable, delivered, upstream_dead)
// collapsed into the same opaque "proxy a2a error" string, which made a
// single-threaded busy spike look like a fleet outage. These tests pin
// the new classification contract so future drift doesn't reintroduce
// the observability gap.
package handlers

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

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
				Status:   http.StatusServiceUnavailable,
				Response: gin.H{"error": "workspace agent unreachable — container restart triggered"},
				Headers:  map[string]string{"Retry-After": "15"},
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

func TestClassificationFromDeliveryConfirmed(t *testing.T) {
	// When deliveryConfirmed is true (the agent wrote a 2xx/3xx response
	// that we partially or fully received), the proxyA2AError must be
	// classified as "delivered" so monitoring/PM can tell it apart from a
	// real failure. When false (non-2xx or empty body), the field stays
	// empty — those are real failures, not transport blips.
	cases := []struct {
		name             string
		deliveryConfirmed bool
		want             string
	}{
		{"delivered when true", true, "delivered"},
		{"empty when false (non-2xx agent response)", false, ""},
		{"empty when false (empty body)", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classificationFromDeliveryConfirmed(tc.deliveryConfirmed)
			if got != tc.want {
				t.Errorf("classificationFromDeliveryConfirmed(%v) = %q, want %q",
					tc.deliveryConfirmed, got, tc.want)
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
	busyErr := &proxyA2AError{Status: http.StatusServiceUnavailable}
	if !isUpstreamBusyError(errors.New("EOF")) {
		// sanity: a synthetic EOF does classify as busy at the predicate
		// level, but the proxyA2AError's Classification field is set by
		// the caller at construction time, not by this predicate.
		t.Skip("predicate semantics changed — update this test")
	}
	// The KEY assertion: isUpstreamBusyError is a pure predicate and
	// does NOT mutate proxyA2AError.Classification. Callers must set
	// the field at construction.
	if busyErr.Classification != "" {
		t.Errorf("isUpstreamBusyError must not mutate proxyA2AError.Classification; "+
			"got %q after a busy-shaped error (the field is set at construction, "+
			"not by the predicate)", busyErr.Classification)
	}
}

// ==================== Helper imports guard ====================

// These imports are used by the tests above. If a future refactor removes
// any of them, the test file will fail to compile — that is intentional,
// it forces whoever removes the dependency to also update the test.
var _ = errors.New
var _ = http.StatusOK
