package provisioner

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBootConfigTokenStore_IssueLookupConsume(t *testing.T) {
	s := NewBootConfigTokenStore(time.Minute)
	tok, err := s.Issue("ws-abc")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if len(tok) < 32 {
		t.Fatalf("token too short: %q", tok)
	}
	ws, ok := s.Lookup(tok)
	if !ok || ws != "ws-abc" {
		t.Fatalf("Lookup = %q,%v; want ws-abc,true", ws, ok)
	}
	// Lookup does NOT consume — a second lookup still resolves.
	if _, ok := s.Lookup(tok); !ok {
		t.Fatalf("second Lookup should still resolve before Consume")
	}
	s.Consume(tok)
	if _, ok := s.Lookup(tok); ok {
		t.Fatalf("Lookup after Consume must fail (single-use)")
	}
	// Consume is idempotent.
	s.Consume(tok)
}

func TestBootConfigTokenStore_EmptyWorkspaceRejected(t *testing.T) {
	s := NewBootConfigTokenStore(time.Minute)
	if _, err := s.Issue("  "); err == nil {
		t.Fatalf("Issue with blank workspace id must error")
	}
}

func TestBootConfigTokenStore_UnknownAndBlankTokenLookup(t *testing.T) {
	s := NewBootConfigTokenStore(time.Minute)
	if _, ok := s.Lookup("nope"); ok {
		t.Fatalf("unknown token must not resolve")
	}
	if _, ok := s.Lookup(""); ok {
		t.Fatalf("blank token must not resolve")
	}
}

func TestBootConfigTokenStore_TTLExpiry(t *testing.T) {
	s := NewBootConfigTokenStore(time.Minute)
	base := time.Now()
	s.now = func() time.Time { return base }
	tok, err := s.Issue("ws-ttl")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	// Just before expiry: resolves.
	s.now = func() time.Time { return base.Add(59 * time.Second) }
	if _, ok := s.Lookup(tok); !ok {
		t.Fatalf("token should resolve before TTL")
	}
	// After expiry: gone.
	s.now = func() time.Time { return base.Add(61 * time.Second) }
	if _, ok := s.Lookup(tok); ok {
		t.Fatalf("token must expire after TTL")
	}
}

func TestBootConfigTokenStore_SupersedesPriorTokenForWorkspace(t *testing.T) {
	s := NewBootConfigTokenStore(time.Minute)
	old, _ := s.Issue("ws-super")
	newTok, _ := s.Issue("ws-super") // reprovision
	if old == newTok {
		t.Fatalf("expected a fresh token on reissue")
	}
	if _, ok := s.Lookup(old); ok {
		t.Fatalf("old token must be superseded (invalidated) on reissue")
	}
	if _, ok := s.Lookup(newTok); !ok {
		t.Fatalf("new token must resolve")
	}
}

func TestBuildConfigBundleJSON(t *testing.T) {
	base := t.TempDir()
	mirror := HostSideConfigsDir(base, "ws-bundle")
	if err := os.MkdirAll(filepath.Join(mirror, "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mirror, "config.yaml"), []byte("name: X\nruntime: openclaw\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mirror, "prompts", "concierge.md"), []byte("# persona"), 0o644); err != nil {
		t.Fatal(err)
	}
	bundle, err := BuildConfigBundleJSON(mirror)
	if err != nil {
		t.Fatalf("BuildConfigBundleJSON: %v", err)
	}
	if len(bundle) != 2 {
		t.Fatalf("want 2 files, got %d: %v", len(bundle), bundle)
	}
	got, err := base64.StdEncoding.DecodeString(bundle["config.yaml"])
	if err != nil || string(got) != "name: X\nruntime: openclaw\n" {
		t.Fatalf("config.yaml roundtrip failed: %q err=%v", got, err)
	}
	// Forward-slash keys even on Windows (wire shape the runtime unpacks).
	if _, ok := bundle["prompts/concierge.md"]; !ok {
		t.Fatalf("expected prompts/concierge.md key, got %v", bundle)
	}
}

func TestBuildConfigBundleJSON_AbsentMirrorIsEmptyNotError(t *testing.T) {
	bundle, err := BuildConfigBundleJSON(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("absent mirror must not error: %v", err)
	}
	if len(bundle) != 0 {
		t.Fatalf("absent mirror must be empty, got %v", bundle)
	}
	if bundle, err := BuildConfigBundleJSON(""); err != nil || len(bundle) != 0 {
		t.Fatalf("empty dir arg must be empty+no-error, got %v err=%v", bundle, err)
	}
}
