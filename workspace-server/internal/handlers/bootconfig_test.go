package handlers

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
	"github.com/gin-gonic/gin"
)

func init() { gin.SetMode(gin.TestMode) }

// seedBootMirror writes a rendered /configs bundle into the host-side mirror for
// wsID and returns (baseDir, store, token) ready for the endpoint.
func seedBootMirror(t *testing.T, wsID string, files map[string]string) (string, *provisioner.BootConfigTokenStore, string) {
	t.Helper()
	base := t.TempDir()
	mirror := provisioner.HostSideConfigsDir(base, wsID)
	for rel, content := range files {
		dest := filepath.Join(mirror, rel)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dest, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	store := provisioner.NewBootConfigTokenStore(time.Minute)
	tok, err := store.Issue(wsID)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	return base, store, tok
}

func doBootReq(h *BootConfigHandler, authz string) *httptest.ResponseRecorder {
	r := gin.New()
	r.GET("/internal/workspaces/boot-config", h.Serve)
	req := httptest.NewRequest(http.MethodGet, "/internal/workspaces/boot-config", nil)
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestBootConfig_ServesRealBundleOnceThenInvalidates(t *testing.T) {
	base, store, tok := seedBootMirror(t, "ws-serve", map[string]string{
		"config.yaml":          "name: OpenClaw\nruntime: openclaw\nmodel: minimax:MiniMax-M2.7\n",
		"prompts/concierge.md": "# concierge persona",
	})
	h := NewBootConfigHandler(store, base)

	// First fetch: real bundle, 200, unpackable {relpath: base64} shape.
	w := doBootReq(h, "Bearer "+tok)
	if w.Code != http.StatusOK {
		t.Fatalf("first fetch: want 200, got %d body=%s", w.Code, w.Body.String())
	}
	var bundle map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &bundle); err != nil {
		t.Fatalf("bundle not JSON object: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(bundle["config.yaml"])
	if err != nil || len(raw) < 20 {
		t.Fatalf("config.yaml not real: %q err=%v", raw, err)
	}
	if _, ok := bundle["prompts/concierge.md"]; !ok {
		t.Fatalf("prompts missing from bundle: %v", bundle)
	}

	// Second fetch with the SAME token: 401 (single-use, token invalidated).
	w2 := doBootReq(h, "Bearer "+tok)
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("replay: want 401, got %d", w2.Code)
	}
}

func TestBootConfig_MissingOrBadTokenIs401(t *testing.T) {
	base, store, _ := seedBootMirror(t, "ws-401", map[string]string{"config.yaml": "name: X\n"})
	h := NewBootConfigHandler(store, base)

	if w := doBootReq(h, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("no auth header: want 401, got %d", w.Code)
	}
	if w := doBootReq(h, "Bearer bogus-token"); w.Code != http.StatusUnauthorized {
		t.Fatalf("bad token: want 401, got %d", w.Code)
	}
	if w := doBootReq(h, "Basic abc"); w.Code != http.StatusUnauthorized {
		t.Fatalf("non-bearer scheme: want 401, got %d", w.Code)
	}
}

func TestBootConfig_DisabledFeatureIs404(t *testing.T) {
	// nil store → feature off → 404 (as if unrouted), even with a plausible token.
	h := NewBootConfigHandler(nil, t.TempDir())
	if w := doBootReq(h, "Bearer anything"); w.Code != http.StatusNotFound {
		t.Fatalf("disabled (nil store): want 404, got %d", w.Code)
	}
	// empty host dir → also disabled.
	store := provisioner.NewBootConfigTokenStore(time.Minute)
	h2 := NewBootConfigHandler(store, "")
	if w := doBootReq(h2, "Bearer anything"); w.Code != http.StatusNotFound {
		t.Fatalf("disabled (empty dir): want 404, got %d", w.Code)
	}
}

func TestBootConfig_ValidTokenButEmptyMirrorIs404AndKeepsToken(t *testing.T) {
	// A valid token whose workspace has NO mirror on disk → 404, and the token is
	// NOT consumed (a mirror-read miss must not burn the token; the caller retries).
	base := t.TempDir()
	store := provisioner.NewBootConfigTokenStore(time.Minute)
	tok, _ := store.Issue("ws-empty")
	h := NewBootConfigHandler(store, base)
	if w := doBootReq(h, "Bearer "+tok); w.Code != http.StatusNotFound {
		t.Fatalf("empty mirror: want 404, got %d", w.Code)
	}
	if _, ok := store.Lookup(tok); !ok {
		t.Fatalf("token must survive an empty-mirror read (retryable)")
	}
}
