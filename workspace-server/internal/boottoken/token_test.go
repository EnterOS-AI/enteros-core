package boottoken

import (
	"testing"
	"time"
)

// --- Golden vector: the SSOT anti-drift anchor. -----------------------------
// These bytes are the molecule-ai-sdk contracts/boot-token golden test_vector.
// molecule-controlplane's minter produced this exact token; asserting BOTH that
// our Verify accepts it AND that our Mint reproduces it byte-for-byte proves this
// package's construction is identical to the controlplane minter's. If either
// side's HMAC/encoding/field-order drifts, this test fails. Keep in lockstep with
// molecule-ai-sdk contracts/boot-token/boot-token.contract.json.
const (
	goldenKey   = "golden-tenant-admin-key-v1"
	goldenToken = "eyJ3c2lkIjoid3MtZ29sZGVuLTAwMDEiLCJvcmciOiJvcmctZ29sZGVuLTAwMDEiLCJleHAiOjE4OTM0NTYwMDAsInNjb3BlIjpbImJvb3QtZXZlbnQiLCJyZXN0b3JlIl19.bI345Nc3Hp8ASXQoBhlbiXiunHghZJcBQk81YXXQZm0"
)

func goldenClaims() Claims {
	return Claims{
		WorkspaceID: "ws-golden-0001",
		OrgID:       "org-golden-0001",
		Expiry:      1893456000,
		Scopes:      []string{ScopeBootEvent, ScopeRestore},
	}
}

// beforeExpiry is a fixed instant < the golden exp, so Verify is deterministic.
var beforeExpiry = time.Unix(1893455999, 0)

func TestGoldenVector_Verify(t *testing.T) {
	got, err := Verify(goldenToken, []byte(goldenKey), beforeExpiry)
	if err != nil {
		t.Fatalf("Verify(golden): %v — core construction has DRIFTED from the SDK SSOT / controlplane minter", err)
	}
	want := goldenClaims()
	if got.WorkspaceID != want.WorkspaceID || got.OrgID != want.OrgID || got.Expiry != want.Expiry ||
		len(got.Scopes) != len(want.Scopes) || got.Scopes[0] != want.Scopes[0] || got.Scopes[1] != want.Scopes[1] {
		t.Fatalf("golden claims mismatch: got %+v want %+v", got, want)
	}
}

func TestGoldenVector_MintReproduces(t *testing.T) {
	tok, err := Mint([]byte(goldenKey), goldenClaims())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if tok != goldenToken {
		t.Fatalf("Mint did NOT reproduce the golden token — construction drift.\n got: %s\nwant: %s", tok, goldenToken)
	}
}

// --- Core Verify matrix -----------------------------------------------------

func TestVerify_WrongKey(t *testing.T) {
	if _, err := Verify(goldenToken, []byte("other-key"), beforeExpiry); err != ErrBadSig {
		t.Fatalf("wrong key: want ErrBadSig, got %v", err)
	}
}

func TestVerify_Expired(t *testing.T) {
	after := time.Unix(1893456001, 0) // exp + 1
	if _, err := Verify(goldenToken, []byte(goldenKey), after); err != ErrExpired {
		t.Fatalf("expired: want ErrExpired, got %v", err)
	}
}

func TestVerify_Malformed(t *testing.T) {
	for _, in := range []string{"", "nodot", "a.b.c", "!!!.###"} {
		if _, err := Verify(in, []byte(goldenKey), beforeExpiry); err == nil {
			t.Errorf("Verify(%q) must error", in)
		}
	}
}

// --- VerifyRestore (route usage) --------------------------------------------

func TestVerifyRestore_OK(t *testing.T) {
	got, err := VerifyRestore(goldenToken, "ws-golden-0001", beforeExpiry, goldenKey)
	if err != nil {
		t.Fatalf("VerifyRestore: %v", err)
	}
	if got.WorkspaceID != "ws-golden-0001" {
		t.Fatalf("wrong claims: %+v", got)
	}
}

func TestVerifyRestore_WorkspaceMismatch(t *testing.T) {
	if _, err := VerifyRestore(goldenToken, "ws-OTHER", beforeExpiry, goldenKey); err != ErrWorkspaceMismatch {
		t.Fatalf("workspace mismatch: want ErrWorkspaceMismatch, got %v", err)
	}
}

func TestVerifyRestore_MissingRestoreScope(t *testing.T) {
	// A boot-event-only token must not authorize restore.
	c := goldenClaims()
	c.Scopes = []string{ScopeBootEvent}
	tok, _ := Mint([]byte(goldenKey), c)
	if _, err := VerifyRestore(tok, "ws-golden-0001", beforeExpiry, goldenKey); err != ErrScope {
		t.Fatalf("missing restore scope: want ErrScope, got %v", err)
	}
}

func TestVerifyRestore_NoKeyFailsClosed(t *testing.T) {
	t.Setenv("ADMIN_TOKEN", "")
	if _, err := VerifyRestore(goldenToken, "ws-golden-0001", beforeExpiry, ""); err != ErrNoKey {
		t.Fatalf("no key: want ErrNoKey, got %v", err)
	}
}

func TestVerifyRestore_WrongKey(t *testing.T) {
	if _, err := VerifyRestore(goldenToken, "ws-golden-0001", beforeExpiry, "attacker-key"); err != ErrBadSig {
		t.Fatalf("wrong key: want ErrBadSig, got %v", err)
	}
}
