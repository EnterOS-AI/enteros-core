package handlers

import (
	"strings"
	"testing"
	"time"
)

// TestDisplayViewerURL_MintsAcceptableToken proves the issuance→acceptance loop
// (reviewer N1): the URL DisplayControl hands out carries a viewer token that the
// DisplaySession path (validateDisplayViewerToken) accepts, so a non-lock-holder
// can actually watch.
func TestDisplayViewerURL_MintsAcceptableToken(t *testing.T) {
	t.Setenv("DISPLAY_SESSION_SIGNING_SECRET", "test-secret")
	u := signedDisplayViewerURL("ws-1")
	if u == "" || !strings.Contains(u, "#token=") {
		t.Fatalf("expected a viewer session URL with a token, got %q", u)
	}
	tok := u[strings.Index(u, "#token=")+len("#token="):]
	if !validateDisplayViewerToken(tok, "ws-1") {
		t.Fatal("token minted into the viewer URL must validate for its workspace")
	}
	if validateDisplayViewerToken(tok, "ws-2") {
		t.Fatal("viewer URL token must not validate for another workspace")
	}
	// No secret -> no URL (fail-closed).
	t.Setenv("DISPLAY_SESSION_SIGNING_SECRET", "")
	if signedDisplayViewerURL("ws-1") != "" {
		t.Fatal("no secret -> empty viewer URL")
	}
}

func TestDisplayViewerToken_DecoupledFromLockHolder(t *testing.T) {
	t.Setenv("DISPLAY_SESSION_SIGNING_SECRET", "test-secret")
	exp := time.Now().Add(5 * time.Minute)

	tok := signDisplayViewerToken("ws-1", exp)
	if tok == "" {
		t.Fatal("expected a viewer token")
	}
	// The core review fix: a viewer token validates for its workspace WITHOUT
	// any knowledge of who holds the control lock — sight is not arbitrated.
	if !validateDisplayViewerToken(tok, "ws-1") {
		t.Fatal("valid viewer token rejected")
	}

	// Wrong workspace -> rejected.
	if validateDisplayViewerToken(tok, "ws-2") {
		t.Fatal("viewer token must not validate for a different workspace")
	}
	// Tampered token -> rejected.
	if validateDisplayViewerToken(tok+"x", "ws-1") {
		t.Fatal("tampered viewer token must be rejected")
	}
	// A CONTROL token is not a viewer token (different payload prefix).
	ctrl := signDisplaySessionToken("ws-1", "someuser", exp)
	if validateDisplayViewerToken(ctrl, "ws-1") {
		t.Fatal("a control token must not pass viewer validation")
	}
}

func TestDisplayViewerToken_Expired(t *testing.T) {
	t.Setenv("DISPLAY_SESSION_SIGNING_SECRET", "test-secret")
	tok := signDisplayViewerToken("ws-1", time.Now().Add(-1*time.Minute))
	if validateDisplayViewerToken(tok, "ws-1") {
		t.Fatal("expired viewer token must be rejected")
	}
}

func TestDisplayViewerToken_NoSecret(t *testing.T) {
	t.Setenv("DISPLAY_SESSION_SIGNING_SECRET", "")
	if signDisplayViewerToken("ws-1", time.Now().Add(time.Minute)) != "" {
		t.Fatal("no secret -> empty token")
	}
	if validateDisplayViewerToken("anything", "ws-1") {
		t.Fatal("no secret -> validation must fail closed")
	}
}
