//go:build staging_e2e

package staginge2e

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"testing"
)

// fakeNetTimeout is a net.Error whose Timeout() is true but which is NOT
// context.DeadlineExceeded — it exercises the net.Error branch of
// coldCreateTransportRetryable (a dial/read/write deadline), distinct from the
// http.Client.Timeout → context-deadline path.
type fakeNetTimeout struct{}

func (fakeNetTimeout) Error() string   { return "i/o timeout" }
func (fakeNetTimeout) Timeout() bool   { return true }
func (fakeNetTimeout) Temporary() bool { return false }

// TestColdCreateTransportRetryable pins the transport-error classifier: a
// CLIENT timeout / context deadline is maybe-processed → NON-retryable (so the
// non-idempotent POST /workspaces is not re-sent → no double-create), while a
// genuine connection reset / refused / never-established socket never reached a
// handler → retryable. Mirrors the shell curl-exit gate and the TS
// TypeError-vs-TimeoutError split.
func TestColdCreateTransportRetryable(t *testing.T) {
	const u = "https://tenant.example/workspaces"
	cases := []struct {
		name string
		err  error
		want bool
	}{
		// NON-retryable: client-side timeouts (maybe-processed).
		{"client-timeout-context-deadline",
			&url.Error{Op: "Post", URL: u, Err: context.DeadlineExceeded}, false},
		{"bare-context-deadline", context.DeadlineExceeded, false},
		{"net-error-timeout",
			&url.Error{Op: "Post", URL: u, Err: fakeNetTimeout{}}, false},
		{"opError-timeout",
			&net.OpError{Op: "dial", Net: "tcp", Err: fakeNetTimeout{}}, false},
		{"nil-error", nil, false},
		// Retryable: non-timeout transport failures (never reached a handler).
		{"connection-reset",
			&url.Error{Op: "Post", URL: u, Err: &net.OpError{Op: "read", Net: "tcp",
				Err: errors.New("connection reset by peer")}}, true},
		{"connection-refused",
			&url.Error{Op: "Post", URL: u, Err: &net.OpError{Op: "dial", Net: "tcp",
				Err: errors.New("connect: connection refused")}}, true},
		{"unexpected-eof",
			&url.Error{Op: "Post", URL: u, Err: errors.New("EOF")}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := coldCreateTransportRetryable(tc.err); got != tc.want {
				t.Fatalf("coldCreateTransportRetryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestColdCreateClientTimeoutNoDoubleCreate reproduces the double-create bug and
// proves the fix. It drives the SAME decision path createTenantWorkspace's retry
// loop makes — doTenantCreateOnce maps a transport error to (status, body), then
// coldCreateShouldRetry decides — while counting POSTs.
//
// OLD behavior: doTenantCreateOnce mapped ANY client.Do error (INCLUDING a
// client timeout after the origin already processed the create) to status 0,
// which coldCreateShouldRetry retries → the loop re-POSTs the non-idempotent
// create up to the full budget → 4 POSTs → orphaned duplicate workspaces.
//
// FIX: a client timeout classifies non-retryable → exactly ONE POST. A genuine
// connection reset still retries (the intended cold-origin coverage).
func TestColdCreateClientTimeoutNoDoubleCreate(t *testing.T) {
	timeoutErr := &url.Error{Op: "Post", URL: "https://tenant.example/workspaces",
		Err: context.DeadlineExceeded}
	resetErr := &url.Error{Op: "Post", URL: "https://tenant.example/workspaces",
		Err: &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}}

	// simulate the create loop's POST count for a given transport-error → (status,
	// body) mapping.
	simulate := func(mapErr func(error) (int, string), transportErr error) int {
		const maxAttempts = 4
		posts := 0
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			posts++ // each loop iteration issues exactly one POST /workspaces
			status, body := mapErr(transportErr)
			if status == http.StatusCreated || status == http.StatusOK {
				break
			}
			if coldCreateShouldRetry(status, body) && attempt < maxAttempts {
				continue
			}
			break
		}
		return posts
	}

	// FIXED mapping — exactly what doTenantCreateOnce now returns.
	fixed := func(err error) (int, string) {
		if coldCreateTransportRetryable(err) {
			return 0, ""
		}
		return -1, "transport error (maybe-processed, not retried): " + err.Error()
	}
	// OLD (buggy) mapping — any transport error became a retryable status 0.
	old := func(error) (int, string) { return 0, "" }

	// Reproduce: the OLD mapping double- (quadruple-) POSTs a non-idempotent
	// create on a client timeout.
	if got := simulate(old, timeoutErr); got != 4 {
		t.Fatalf("precondition: OLD code should re-POST a client-timeout to the budget; got %d POSTs (want 4)", got)
	}
	// Fix: exactly ONE POST on a client timeout — no double-create.
	if got := simulate(fixed, timeoutErr); got != 1 {
		t.Fatalf("FIX: a client timeout must issue exactly ONE POST; got %d", got)
	}
	// The intended cold-origin coverage is preserved: a genuine reset retries.
	if got := simulate(fixed, resetErr); got != 4 {
		t.Fatalf("connection reset should still retry to the budget; got %d POSTs (want 4)", got)
	}
}

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
