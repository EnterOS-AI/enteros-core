package handlers

import (
	"testing"
	"time"
)

// TestDesktopSigningRoot_OverrideWins: an explicit DISPLAY_SESSION_SIGNING_SECRET
// is returned verbatim and takes precedence over any derivable source, so an
// existing deployment that sets it is byte-identical (zero behavior change).
func TestDesktopSigningRoot_OverrideWins(t *testing.T) {
	clearDesktopSigningSources(t)
	t.Setenv("SECRETS_ENCRYPTION_KEY", "enc-key-value")
	t.Setenv("DISPLAY_SESSION_SIGNING_SECRET", "explicit-override")
	if got := DesktopSigningRoot(); got != "explicit-override" {
		t.Fatalf("override must win verbatim, got %q", got)
	}
}

// TestDesktopSigningRoot_DerivesAndFallsBack: with no override, the root is
// DERIVED (not the raw source) from the first present fallback, deterministically,
// and different sources yield different roots.
func TestDesktopSigningRoot_DerivesAndFallsBack(t *testing.T) {
	clearDesktopSigningSources(t)
	t.Setenv("SECRETS_ENCRYPTION_KEY", "enc-key-value")
	fromEnc := DesktopSigningRoot()
	if fromEnc == "" || fromEnc == "enc-key-value" {
		t.Fatalf("root must be a non-empty DERIVED value, not the raw source: %q", fromEnc)
	}
	if again := DesktopSigningRoot(); again != fromEnc {
		t.Fatalf("derivation must be deterministic: %q vs %q", fromEnc, again)
	}

	// With the encryption key gone, it falls back to the CP shared secret, and the
	// derived root differs from the encryption-key-derived one.
	t.Setenv("SECRETS_ENCRYPTION_KEY", "")
	t.Setenv("MOLECULE_CP_SHARED_SECRET", "cp-shared")
	fromCP := DesktopSigningRoot()
	if fromCP == "" || fromCP == fromEnc {
		t.Fatalf("CP-derived root must be present and distinct from enc-derived: %q vs %q", fromCP, fromEnc)
	}
}

// TestDesktopSigningRoot_EmptyWhenNothingSet: the fail-closed contract — no
// override and no derivable source → "" (desktop stays disabled).
func TestDesktopSigningRoot_EmptyWhenNothingSet(t *testing.T) {
	clearDesktopSigningSources(t)
	if got := DesktopSigningRoot(); got != "" {
		t.Fatalf("no source set must yield empty root, got %q", got)
	}
}

// TestDisplayToken_WorksWithoutDedicatedSecret: the whole point of the change —
// with ONLY SECRETS_ENCRYPTION_KEY present (no DISPLAY_SESSION_SIGNING_SECRET),
// display tokens sign and validate end-to-end. The dedicated-secret footgun is gone.
func TestDisplayToken_WorksWithoutDedicatedSecret(t *testing.T) {
	clearDesktopSigningSources(t)
	t.Setenv("SECRETS_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	exp := time.Now().Add(5 * time.Minute)

	tok := signDisplaySessionToken("ws-1", "user-a", exp)
	if tok == "" {
		t.Fatal("expected a session token derived from SECRETS_ENCRYPTION_KEY alone")
	}
	if !validateDisplaySessionToken(tok, "ws-1", "user-a", exp) {
		t.Fatal("token derived from the encryption key must validate")
	}
	// Per-workspace key: a ws-1 token must not validate for ws-2 even with the
	// same controller/expiry (the derived KEY differs, not just the payload).
	if validateDisplaySessionToken(tok, "ws-2", "user-a", exp) {
		t.Fatal("per-workspace derivation must reject a token replayed to another workspace")
	}
}
