package handlers

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// The credential decision for outbound A2A dispatches.
//
// Every "no credential" assertion here is paired with a "credential
// attached" assertion on the same branch, in both flag positions. That
// pairing is the point: a resolver hardwired to return "" passes the
// send-nothing half and fails the attach half, and a resolver hardwired to
// always return the secret fails the reverse. Neither mutation can leave
// this file green.

const testInboundSecret = "inbound-secret-for-test"

// externalURL is classified external by isExternalAgentURL (public
// hostname, not the workspace's container DNS, not a platform tunnel).
const externalURL = "https://agent.example.com/a2a/inbound"

// tunnelURL is the per-workspace tunnel shape. isPlatformTunnelHostname
// classifies it INTERNAL under the default appDomain — this is the case the
// whole change exists for.
const tunnelURL = "https://ws-abc123.moleculesai.app/"

const a2aAuthTestWorkspaceID = "ws-test-0001"

// containerURL is the workspace's own container DNS name — unambiguously
// internal on every substrate.
func containerURL(t *testing.T) string {
	t.Helper()
	return "http://" + containerNameForWorkspace(a2aAuthTestWorkspaceID) + ":8000/"
}

func okReader(secret string) inboundSecretReader {
	return func(context.Context, string, string) (string, bool, error) {
		return secret, false, nil
	}
}

func errReader(err error) inboundSecretReader {
	return func(context.Context, string, string) (string, bool, error) {
		return "", false, err
	}
}

func healedReader(secret string) inboundSecretReader {
	return func(context.Context, string, string) (string, bool, error) {
		return secret, true, nil
	}
}

// countingReader records whether the secret was read at all — the flag-off
// internal path must not even touch the datastore.
func countingReader(secret string, calls *int) inboundSecretReader {
	return func(context.Context, string, string) (string, bool, error) {
		*calls++
		return secret, false, nil
	}
}

// ---------- pre-flip: flag OFF must be byte-identical to today ----------

func TestResolveA2AInboundSecret_FlagOff_InternalSendsNothing(t *testing.T) {
	calls := 0
	secret, perr := resolveA2AInboundSecret(
		context.Background(), a2aAuthTestWorkspaceID, containerURL(t),
		countingReader(testInboundSecret, &calls), false,
	)
	if perr != nil {
		t.Fatalf("internal dispatch must never error pre-flip, got %+v", perr)
	}
	if secret != "" {
		t.Errorf("flag off must attach no credential to an internal URL, got %q", secret)
	}
	if calls != 0 {
		t.Errorf("flag off must not read the secret for internal URLs; read %d times", calls)
	}
}

func TestResolveA2AInboundSecret_FlagOff_TunnelSendsNothing(t *testing.T) {
	// Documents the BUG this change closes: with the flag off, the tunnel
	// — the intended architecture — carries no credential. If this ever
	// starts returning the secret with alwaysAuth=false, the flag has
	// stopped being the thing that controls the rollout.
	secret, perr := resolveA2AInboundSecret(
		context.Background(), a2aAuthTestWorkspaceID, tunnelURL, okReader(testInboundSecret), false,
	)
	if perr != nil {
		t.Fatalf("unexpected error: %+v", perr)
	}
	if secret != "" {
		t.Errorf("flag off must leave tunnel behaviour unchanged, got %q", secret)
	}
}

func TestResolveA2AInboundSecret_FlagOff_ExternalStillAuthenticates(t *testing.T) {
	// The flag must not regress core#3319. External callers were already
	// authenticating and must continue to, flag or no flag.
	secret, perr := resolveA2AInboundSecret(
		context.Background(), a2aAuthTestWorkspaceID, externalURL, okReader(testInboundSecret), false,
	)
	if perr != nil {
		t.Fatalf("unexpected error: %+v", perr)
	}
	if secret != testInboundSecret {
		t.Errorf("external URL must carry the bearer even pre-flip, got %q", secret)
	}
}

// ---------- post-flip: flag ON attaches everywhere ----------

func TestResolveA2AInboundSecret_FlagOn_TunnelAuthenticates(t *testing.T) {
	// The point of the whole change.
	secret, perr := resolveA2AInboundSecret(
		context.Background(), a2aAuthTestWorkspaceID, tunnelURL, okReader(testInboundSecret), true,
	)
	if perr != nil {
		t.Fatalf("unexpected error: %+v", perr)
	}
	if secret != testInboundSecret {
		t.Errorf("tunnel URL must carry the bearer when always-auth is on, got %q", secret)
	}
}

func TestResolveA2AInboundSecret_FlagOn_ContainerAuthenticates(t *testing.T) {
	secret, perr := resolveA2AInboundSecret(
		context.Background(), a2aAuthTestWorkspaceID, containerURL(t), okReader(testInboundSecret), true,
	)
	if perr != nil {
		t.Fatalf("unexpected error: %+v", perr)
	}
	if secret != testInboundSecret {
		t.Errorf("container URL must carry the bearer when always-auth is on, got %q", secret)
	}
}

func TestResolveA2AInboundSecret_FlagOn_ExternalUnchanged(t *testing.T) {
	secret, perr := resolveA2AInboundSecret(
		context.Background(), a2aAuthTestWorkspaceID, externalURL, okReader(testInboundSecret), true,
	)
	if perr != nil {
		t.Fatalf("unexpected error: %+v", perr)
	}
	if secret != testInboundSecret {
		t.Errorf("external URL behaviour must be unchanged, got %q", secret)
	}
}

// ---------- the live-fleet safety property ----------

func TestResolveA2AInboundSecret_FlagOn_InternalReadErrorDoesNotFailDispatch(t *testing.T) {
	// reno-stars is serving on container-local URLs right now. A secret-read
	// hiccup must degrade to "send nothing", never to a 503.
	secret, perr := resolveA2AInboundSecret(
		context.Background(), a2aAuthTestWorkspaceID, containerURL(t),
		errReader(errors.New("datastore unavailable")), true,
	)
	if perr != nil {
		t.Fatalf("internal dispatch must NOT fail on a secret-read error, got %+v", perr)
	}
	if secret != "" {
		t.Errorf("unreadable secret must yield no credential, got %q", secret)
	}
}

func TestResolveA2AInboundSecret_FlagOn_InternalJustMintedDoesNotFailDispatch(t *testing.T) {
	// A freshly minted secret is one the workspace has not received yet.
	// Sending it would be a bearer the workspace cannot match; 503ing would
	// break a live agent. Send nothing and let the heartbeat converge.
	secret, perr := resolveA2AInboundSecret(
		context.Background(), a2aAuthTestWorkspaceID, containerURL(t),
		healedReader(testInboundSecret), true,
	)
	if perr != nil {
		t.Fatalf("internal dispatch must NOT fail when the secret was just minted, got %+v", perr)
	}
	if secret != "" {
		t.Errorf("just-minted secret must not be sent, got %q", secret)
	}
}

// ---------- the external path's pre-existing 503 contract ----------

func TestResolveA2AInboundSecret_ExternalReadErrorStill503s(t *testing.T) {
	// Deliberate asymmetry with the internal path above. Preserved exactly.
	for _, always := range []bool{false, true} {
		secret, perr := resolveA2AInboundSecret(
			context.Background(), a2aAuthTestWorkspaceID, externalURL,
			errReader(errors.New("datastore unavailable")), always,
		)
		if perr == nil {
			t.Fatalf("alwaysAuth=%v: external read error must 503, got nil error", always)
		}
		if perr.Status != http.StatusServiceUnavailable {
			t.Errorf("alwaysAuth=%v: want 503, got %d", always, perr.Status)
		}
		if secret != "" {
			t.Errorf("alwaysAuth=%v: must not return a secret alongside an error", always)
		}
	}
}

func TestResolveA2AInboundSecret_ExternalJustMintedStill503s(t *testing.T) {
	for _, always := range []bool{false, true} {
		_, perr := resolveA2AInboundSecret(
			context.Background(), a2aAuthTestWorkspaceID, externalURL,
			healedReader(testInboundSecret), always,
		)
		if perr == nil {
			t.Fatalf("alwaysAuth=%v: just-minted external must 503, got nil", always)
		}
		if perr.Status != http.StatusServiceUnavailable {
			t.Errorf("alwaysAuth=%v: want 503, got %d", always, perr.Status)
		}
	}
}

// ---------- the flag predicate itself ----------

func TestA2AAlwaysAuthEnabled_DefaultsOff(t *testing.T) {
	t.Setenv(a2aAlwaysAuthEnv, "")
	if a2aAlwaysAuthEnabled() {
		t.Fatal("MOLECULE_A2A_ALWAYS_AUTH must default to OFF — the fleet ships dark")
	}
}

func TestA2AAlwaysAuthEnabled_TruthyEnables(t *testing.T) {
	// Paired with the default test: without this, a predicate hardwired to
	// false would pass the test above and never enforce anything.
	for _, v := range []string{"1", "true", "TRUE", "t"} {
		t.Setenv(a2aAlwaysAuthEnv, v)
		if !a2aAlwaysAuthEnabled() {
			t.Errorf("value %q must enable always-auth", v)
		}
	}
}

func TestA2AAlwaysAuthEnabled_UnparseableFallsBackToOff(t *testing.T) {
	// A typo'd flag must not silently start changing outbound bytes.
	for _, v := range []string{"yes", "on", "maybe", "2"} {
		t.Setenv(a2aAlwaysAuthEnv, v)
		if a2aAlwaysAuthEnabled() {
			t.Errorf("unparseable value %q must fall back to OFF", v)
		}
	}
}
