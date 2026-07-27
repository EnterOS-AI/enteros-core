package handlers

import "testing"

func TestDeriveDesktopControlToken(t *testing.T) {
	a := DeriveDesktopControlToken("secret", "ws-1")
	if a == "" {
		t.Fatal("expected a token")
	}
	// Deterministic: the provisioner (sets DESKTOP_CONTROL_TOKEN) and the gateway
	// (TokenResolver) MUST derive the same value.
	if DeriveDesktopControlToken("secret", "ws-1") != a {
		t.Fatal("derivation must be deterministic")
	}
	// Per-workspace + per-secret.
	if DeriveDesktopControlToken("secret", "ws-2") == a {
		t.Fatal("different workspace must derive a different token")
	}
	if DeriveDesktopControlToken("other", "ws-1") == a {
		t.Fatal("different secret must derive a different token")
	}
	// Fail-closed.
	if DeriveDesktopControlToken("", "ws-1") != "" || DeriveDesktopControlToken("secret", "") != "" {
		t.Fatal("empty secret/workspace -> empty token")
	}
}
