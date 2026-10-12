package staginge2e

import (
	"net/http"
	"testing"
)

func TestClassifyConciergeConfigTabResponse(t *testing.T) {
	tests := []struct {
		name       string
		tab        string
		statusCode int
		body       string
		want       conciergeConfigTabVerdict
	}{
		{
			name:       "ordinary success is reachable",
			tab:        "traces",
			statusCode: http.StatusOK,
			body:       `[]`,
			want:       conciergeConfigTabReachable,
		},
		{
			name:       "non-auth client response remains reachable",
			tab:        "plugins",
			statusCode: http.StatusNotFound,
			body:       `{"error":"not found"}`,
			want:       conciergeConfigTabReachable,
		},
		{
			name:       "exact schedules offline response is expected while unbooted",
			tab:        "schedules",
			statusCode: http.StatusServiceUnavailable,
			body:       `{"error":"workspace url not registered yet"}`,
			want:       conciergeConfigTabExpectedOffline,
		},
		{
			name:       "insignificant JSON whitespace preserves exact offline response",
			tab:        "schedules",
			statusCode: http.StatusServiceUnavailable,
			body:       "  { \n \t\"error\" : \"workspace url not registered yet\" }\n",
			want:       conciergeConfigTabExpectedOffline,
		},
		{
			name:       "unauthorized always fails closed",
			tab:        "schedules",
			statusCode: http.StatusUnauthorized,
			body:       `{"error":"unauthorized"}`,
			want:       conciergeConfigTabRejected,
		},
		{
			name:       "forbidden always fails closed",
			tab:        "model",
			statusCode: http.StatusForbidden,
			body:       `{"error":"forbidden"}`,
			want:       conciergeConfigTabRejected,
		},
		{
			name:       "arbitrary schedules 503 remains a server fault",
			tab:        "schedules",
			statusCode: http.StatusServiceUnavailable,
			body:       `{"error":"database unavailable"}`,
			want:       conciergeConfigTabRejected,
		},
		{
			name:       "offline response with extra data is not exact",
			tab:        "schedules",
			statusCode: http.StatusServiceUnavailable,
			body:       `{"error":"workspace url not registered yet","detail":"unexpected"}`,
			want:       conciergeConfigTabRejected,
		},
		{
			name:       "malformed offline response is rejected",
			tab:        "schedules",
			statusCode: http.StatusServiceUnavailable,
			body:       `{"error":"workspace url not registered yet"`,
			want:       conciergeConfigTabRejected,
		},
		{
			name:       "offline body with wrong status is rejected",
			tab:        "schedules",
			statusCode: http.StatusBadGateway,
			body:       `{"error":"workspace url not registered yet"}`,
			want:       conciergeConfigTabRejected,
		},
		{
			name:       "offline body with successful status is rejected",
			tab:        "schedules",
			statusCode: http.StatusOK,
			body:       `{"error":"workspace url not registered yet"}`,
			want:       conciergeConfigTabRejected,
		},
		{
			name:       "offline body from a non-proxy tab is rejected",
			tab:        "plugins",
			statusCode: http.StatusServiceUnavailable,
			body:       `{"error":"workspace url not registered yet"}`,
			want:       conciergeConfigTabRejected,
		},
		{
			name:       "offline body from a non-proxy tab with client status is rejected",
			tab:        "plugins",
			statusCode: http.StatusNotFound,
			body:       `{"error":"workspace url not registered yet"}`,
			want:       conciergeConfigTabRejected,
		},
		{
			name:       "generic internal server error is rejected",
			tab:        "channels",
			statusCode: http.StatusInternalServerError,
			body:       `{"error":"boom"}`,
			want:       conciergeConfigTabRejected,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := classifyConciergeConfigTabResponse(tt.tab, tt.statusCode, tt.body)
			if got != tt.want {
				t.Fatalf("classifyConciergeConfigTabResponse(%q, %d, %q) = %v, want %v (reason=%q)",
					tt.tab, tt.statusCode, tt.body, got, tt.want, reason)
			}
			if reason == "" {
				t.Fatal("classification reason must be non-empty")
			}
		})
	}
}
