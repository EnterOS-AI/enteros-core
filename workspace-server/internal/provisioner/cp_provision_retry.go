package provisioner

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"time"
)

// Retry support for CPProvisioner.Start (core#5057).
//
// Knobs are package-level vars, not consts, so tests can shrink the budget
// without sleeping for real — matching cpStopRetryAttempts /
// cpStopRetryBaseDelay and instanceIDPersistRetryAttempts, the existing
// precedents in this tree.
var (
	// cpProvisionRetryAttempts caps TOTAL attempts (initial + retries) at the
	// provision POST.
	//
	// This was 3 (2s + 4s = 6s of backoff), sized for the transient classes then
	// on record: a TLS record-MAC blip and a Cloudflare 524/502 under load. Both
	// resolve in a second or two, so 6s looked generous.
	//
	// It is not, because it missed the LONGEST transient the CP actually has: a
	// redeploy of the control plane itself. MEASURED 2026-08-07, staging CD
	// recreating molecule-cp-staging (job 927326, controlplane#2908):
	//
	//	19:21:12.797  old container stopped + renamed
	//	19:21:19.105  new container started
	//	19:21:21.630  tenant networks re-attached (alias controlplane-staging)
	//	19:21:27.300  POST /cp/workspaces/provision route registered — first
	//	              instant the endpoint can answer at all
	//	19:21:30.499  HEALTHY
	//
	// ~17.7s of outage. A tenant provisioning across it (workspace
	// 25215f53-dd1b-4d96-ad6f-72e508cfe3eb) burned all 3 attempts between
	// 19:21:14 and 19:21:23 and gave up FOUR SECONDS before the route existed:
	//
	//	PROVISION-EXHAUSTED cpProv.Start workspace_id=25215f53-… attempts=3
	//	  last_err=cp provisioner: provision failed (502): <unstructured body, 16 bytes>
	//
	// The workspace was then marked failed permanently — a routine, successful CP
	// deploy silently destroys any provision in flight, in prod as well as
	// staging. 6 attempts with the cap below spend at most 2+4+8+10+10 = 34s of
	// backoff, ~2x the measured outage, against a provision budget of 12m
	// (30m hermes). A genuinely wedged CP still fails, just 34s later.
	cpProvisionRetryAttempts = 6

	// cpProvisionRetryBaseDelay is the first-retry backoff; it doubles each
	// attempt up to cpProvisionRetryMaxDelay. Deliberately longer than the
	// instance_id persist's 100ms: that one races a local DB blip, this one
	// waits out an edge/tunnel hiccup or a CP restart, and a sub-second retry
	// into a 502-ing edge is just a second failure.
	cpProvisionRetryBaseDelay = 2 * time.Second

	// cpProvisionRetryMaxDelay caps the doubling. Without it, 6 attempts would
	// back off 2+4+8+16+32 = 62s, and the last gap alone (32s) would exceed the
	// whole outage it exists to cover — the retry would spend most of its budget
	// asleep past a CP that came back long ago. Capping at 10s keeps the polls
	// dense enough to catch the CP within a few seconds of it starting to serve.
	cpProvisionRetryMaxDelay = 10 * time.Second
)

// newProvisionIdempotencyKey mints the key that makes retrying this POST safe:
// the CP records the terminal response against (org_id, key) and replays it
// instead of provisioning a second box.
//
// ONE key per Start CALL, reused across that call's retries. A key minted
// per-attempt would defeat the entire mechanism — every attempt would look like
// a brand-new provision to the CP, which is exactly the double-provision this
// exists to prevent.
//
// crypto/rand, not math/rand: a guessable key would let one tenant pre-claim
// another's (the CP scopes keys per-org, so this is defence in depth) and would
// risk accidental collisions across concurrent provisions in the same org.
// Returns an error rather than a weak key so the caller can fall back to a
// single un-retried attempt.
func newProvisionIdempotencyKey() (string, error) {
	buf := make([]byte, 16) // 128 bits
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "cpprov-" + hex.EncodeToString(buf), nil
}

// retryableProvisionStatus reports whether a non-201 provision response is
// worth another attempt.
//
// Retryable = the request plausibly failed in transit or transiently at the
// edge/origin, so the SAME request can still succeed:
//
//   - 408 / 429                     — client timeout, rate limit
//   - 500, 502, 503, 504           — origin or gateway hiccup
//   - 520-527                       — Cloudflare-origin family; 524 (origin
//     timed out) is the one that motivated this, because the origin often
//     COMPLETES the provision after CF has given up on the response
//   - 409 provision_in_progress     — our own earlier attempt DID land and is
//     still running. Backing off and asking again is exactly right: once it
//     resolves, the CP replays its recorded 201 and we learn the real
//     instance_id.
//
// Everything else is deterministic — 400 privileged_env_forbidden, 401/403 auth,
// 422 RUNTIME_PIN_MISSING / PLATFORM_AGENT_RUNTIME_UNSUPPORTED — and will fail
// identically on every attempt, so retrying only burns the provision budget.
//
// 409 idempotency_key_conflict is deliberately NOT retryable: it means the key
// was reused across workspaces, which is a client bug that no amount of
// retrying fixes.
func retryableProvisionStatus(status int, errCode string) bool {
	if status == http.StatusConflict {
		return errCode == "provision_in_progress"
	}
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		return true
	}
	if status >= 500 && status <= 527 {
		return true
	}
	return false
}
