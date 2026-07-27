package handlers

import (
	"testing"
	"time"
)

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
