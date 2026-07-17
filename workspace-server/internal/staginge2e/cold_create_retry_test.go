//go:build staging_e2e

package staginge2e

import "testing"

// These are PURE-helper unit tests for the cold-origin create retry classifier
// (#91, RCA run 527280). They do NOT touch the network and run without staging
// creds, unlike the live suites that t.Skip. They pin the SAME non-masking
// decision table the shell (tests/e2e/lib/workspace_create_retry.sh) and TS
// (canvas workspaceCreateRetry.ts) siblings encode, so all three create seams
// share ONE rule: retry ONLY a "never reached a handler" signature (empty-body
// 503 or a transport error surfaced as status 0), never a non-empty body and
// never a 502/504.

func TestColdCreateShouldRetry(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		// Retryable: the cold-origin "never reached a handler" signatures.
		{"empty-503", 503, "", true},
		{"whitespace-503", 503, "  \n\t \r\n", true},
		{"status0-transport", 0, "", true},
		// NOT retryable.
		{"empty-502", 502, "", false},                     // maybe-processed non-idempotent POST
		{"empty-504", 504, "", false},                     // maybe-processed non-idempotent POST
		{"json-body-503", 503, `{"error":"boom"}`, false}, // real handler response
		{"json-422", 422, `{"error":"bad request"}`, false},
		{"empty-404", 404, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := coldCreateShouldRetry(tc.status, tc.body); got != tc.want {
				t.Fatalf("coldCreateShouldRetry(%d, %q) = %v, want %v",
					tc.status, tc.body, got, tc.want)
			}
		})
	}
}

func TestParseColdRetryAfter(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int
	}{
		{"integer", "3", 3},
		{"padded", "  5  ", 5},
		{"empty-default", "", 2},
		{"cap-at-10", "900", 10},
		{"exactly-10", "10", 10},
		{"http-date-default", "Wed, 21 Oct 2015 07:28:00 GMT", 2},
		{"garbage-default", "soon", 2},
		{"negative-default", "-5", 2}, // '-' is not all-digits → default, never negative sleep
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseColdRetryAfter(tc.raw); got != tc.want {
				t.Fatalf("parseColdRetryAfter(%q) = %d, want %d", tc.raw, got, tc.want)
			}
		})
	}
}
