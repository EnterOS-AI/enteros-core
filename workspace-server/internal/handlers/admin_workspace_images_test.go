package handlers

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/providers"
)

func TestRegistryAuthHeader_NoEnvReturnsEmpty(t *testing.T) {
	t.Setenv("GHCR_USER", "")
	t.Setenv("GHCR_TOKEN", "")
	t.Setenv("MOLECULE_IMAGE_REGISTRY", "")
	if got := registryAuthHeader(); got != "" {
		t.Errorf("expected empty (no auth → public-only), got %q", got)
	}
}

func TestRegistryAuthHeader_PartialEnvReturnsEmpty(t *testing.T) {
	// Both must be set — defensive against half-configured env.
	t.Setenv("GHCR_USER", "alice")
	t.Setenv("GHCR_TOKEN", "")
	if got := registryAuthHeader(); got != "" {
		t.Errorf("user-only env should disable auth, got %q", got)
	}
	t.Setenv("GHCR_USER", "")
	t.Setenv("GHCR_TOKEN", "fake-tok-xxx")
	if got := registryAuthHeader(); got != "" {
		t.Errorf("token-only env should disable auth, got %q", got)
	}
}

func TestRegistryAuthHeader_EncodesDockerEnginePayload(t *testing.T) {
	// Default registry env uses the current Gitea registry, so the
	// serveraddress field should resolve to registry.moleculesai.app. Pin both env vars so the
	// test is hermetic regardless of the host's MOLECULE_IMAGE_REGISTRY.
	t.Setenv("MOLECULE_IMAGE_REGISTRY", "")
	t.Setenv("GHCR_USER", "alice")
	t.Setenv("GHCR_TOKEN", "fake-tok-value")
	got := registryAuthHeader()
	if got == "" {
		t.Fatal("expected non-empty auth header")
	}
	raw, err := base64.StdEncoding.DecodeString(got)
	if err != nil {
		t.Fatalf("auth header is not valid base64: %v", err)
	}
	var payload map[string]string
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decoded auth is not valid JSON: %v (raw=%s)", err, raw)
	}
	if payload["username"] != "alice" {
		t.Errorf("username: got %q, want alice", payload["username"])
	}
	if payload["password"] != "fake-tok-value" {
		t.Errorf("password: got %q, want fake-tok-value", payload["password"])
	}
	if payload["serveraddress"] != "registry.moleculesai.app" {
		t.Errorf("serveraddress: got %q, want registry.moleculesai.app", payload["serveraddress"])
	}
}

// TestRegistryAuthHeader_RespectsRegistryEnv pins the registry override: when
// MOLECULE_IMAGE_REGISTRY points at a private mirror, the Docker engine auth
// payload's serveraddress must reflect that mirror's host.
func TestRegistryAuthHeader_RespectsRegistryEnv(t *testing.T) {
	t.Setenv("GHCR_USER", "alice")
	t.Setenv("GHCR_TOKEN", "fake-tok-value")
	t.Setenv("MOLECULE_IMAGE_REGISTRY", "registry.example.com/molecule-ai")

	got := registryAuthHeader()
	if got == "" {
		t.Fatal("expected non-empty auth header")
	}
	raw, err := base64.StdEncoding.DecodeString(got)
	if err != nil {
		t.Fatalf("auth header is not valid base64: %v", err)
	}
	var payload map[string]string
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decoded auth is not valid JSON: %v (raw=%s)", err, raw)
	}
	want := "registry.example.com"
	if payload["serveraddress"] != want {
		t.Errorf("serveraddress: got %q, want %q (must follow MOLECULE_IMAGE_REGISTRY host)",
			payload["serveraddress"], want)
	}
	// Sanity: the org-path portion must NOT leak into serveraddress.
	if payload["serveraddress"] == "registry.example.com/molecule-ai" {
		t.Error("serveraddress must be host-only, not host+org-path")
	}
}

// runtimeListContains is a tiny membership helper for the runtime-allowlist tests.
func runtimeListContains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// TestAllRuntimes_MatchesProvidersSSOT is the drift guard. AllRuntimes is
// derived from providers.LoadManifest().Runtimes — assert it equals exactly the
// runtime keys the providers manifest (mirrored from CP's providers.yaml)
// declares. If CP adds/removes a runtime, this test fails RED until the tenant
// re-derives, so the tenant image-refresh allowlist can never silently drift
// from the CP pin-promote allowlist again.
func TestAllRuntimes_MatchesProvidersSSOT(t *testing.T) {
	m, err := providers.LoadManifest()
	if err != nil {
		t.Fatalf("providers.LoadManifest: %v", err)
	}
	want := make([]string, 0, len(m.Runtimes))
	for rt := range m.Runtimes {
		want = append(want, rt)
	}
	sort.Strings(want)

	got := append([]string(nil), AllRuntimes...)
	sort.Strings(got)

	if len(got) != len(want) {
		t.Fatalf("AllRuntimes drift: got %v, want %v (providers SSOT)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("AllRuntimes drift at %d: got %v, want %v (providers SSOT)", i, got, want)
		}
	}
}

// TestImageRefreshFallbackMatchesSSOT pins the static fallback (used only when
// the embedded manifest fails to load) to the providers SSOT. If a runtime is
// added to providers.yaml but not to imageRefreshFallbackRuntimes, this fails
// RED — so a manifest-load failure can't silently drop a supported runtime.
func TestImageRefreshFallbackMatchesSSOT(t *testing.T) {
	m, err := providers.LoadManifest()
	if err != nil {
		t.Fatalf("providers.LoadManifest: %v", err)
	}
	want := make([]string, 0, len(m.Runtimes))
	for rt := range m.Runtimes {
		want = append(want, rt)
	}
	sort.Strings(want)

	got := append([]string(nil), imageRefreshFallbackRuntimes...)
	sort.Strings(got)

	if len(got) != len(want) {
		t.Fatalf("fallback drift: got %v, want %v (providers SSOT)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("fallback drift at %d: got %v, want %v (providers SSOT)", i, got, want)
		}
	}
}

// TestRefresh_RejectsUnknownRuntime asserts a genuinely unknown runtime still
// 400s and that the 400 body lists the supported runtime set. This exercises
// the gin handler's reject branch, which runs entirely before any Docker call.
func TestRefresh_RejectsUnknownRuntime(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// nil docker client is safe: the unknown-runtime branch returns 400
	// before svc.Refresh (which is the only path that touches Docker).
	h := &AdminWorkspaceImagesHandler{svc: &WorkspaceImageService{}}

	r := gin.New()
	r.POST("/admin/workspace-images/refresh", h.Refresh)

	req := httptest.NewRequest(http.MethodPost, "/admin/workspace-images/refresh?runtime=not-a-real-runtime", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown runtime: got status %d, want 400; body=%s", rec.Code, rec.Body.String())
	}

	var body struct {
		Error         string   `json:"error"`
		KnownRuntimes []string `json:"known_runtimes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode 400 body: %v (raw=%s)", err, rec.Body.String())
	}
	if runtimeListContains(body.KnownRuntimes, "google-adk") {
		t.Errorf("400 known_runtimes must not advertise retired google-adk; got %v", body.KnownRuntimes)
	}
	if !runtimeListContains(body.KnownRuntimes, "hermes") {
		t.Errorf("400 known_runtimes must advertise hermes; got %v", body.KnownRuntimes)
	}
}

func TestRefresh_RejectsRemotePullInLocalBuildMode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("MOLECULE_IMAGE_REGISTRY", "")

	// A nil Docker client is intentional: local-build mode must be rejected
	// before the handler can attempt a registry pull or container mutation.
	h := &AdminWorkspaceImagesHandler{svc: &WorkspaceImageService{}}
	r := gin.New()
	r.POST("/admin/workspace-images/refresh", h.Refresh)

	req := httptest.NewRequest(http.MethodPost, "/admin/workspace-images/refresh?runtime=hermes", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("local-build refresh: got status %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "local-build mode") {
		t.Fatalf("local-build refresh error must explain the mode mismatch; body=%s", rec.Body.String())
	}
}

func TestRegistryAuthHeader_TrimsWhitespace(t *testing.T) {
	t.Setenv("MOLECULE_IMAGE_REGISTRY", "")
	// .env lines often have trailing newlines or accidental spaces. Without
	// trimming, a stray space would produce an auth payload the engine
	// rejects with a confusing 401.
	t.Setenv("GHCR_USER", "  alice  ")
	t.Setenv("GHCR_TOKEN", "\tfake-tok-value\n")
	got := registryAuthHeader()
	raw, _ := base64.StdEncoding.DecodeString(got)
	var payload map[string]string
	_ = json.Unmarshal(raw, &payload)
	if payload["username"] != "alice" {
		t.Errorf("username not trimmed: got %q", payload["username"])
	}
	if payload["password"] != "fake-tok-value" {
		t.Errorf("password not trimmed: got %q", payload["password"])
	}
}

func TestTemplateImageRef(t *testing.T) {
	cases := []struct {
		name        string
		registryEnv string
		runtime     string
		want        string
	}{
		{
			name:        "default registry prefix",
			registryEnv: "",
			runtime:     "claude-code",
			want:        "registry.moleculesai.app/molecule-ai/workspace-template-claude-code:latest",
		},
		{
			name:        "custom mirror prefix",
			registryEnv: "registry.example.com/molecule-ai",
			runtime:     "claude-code",
			want:        "registry.example.com/molecule-ai/workspace-template-claude-code:latest",
		},
		{
			name:        "self-hosted Gitea registry",
			registryEnv: "git.moleculesai.app/molecule-ai",
			runtime:     "hermes",
			want:        "git.moleculesai.app/molecule-ai/workspace-template-hermes:latest",
		},
		{
			name:        "runtime with hyphen",
			registryEnv: "",
			runtime:     "openclaw-runtime",
			want:        "registry.moleculesai.app/molecule-ai/workspace-template-openclaw-runtime:latest",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MOLECULE_IMAGE_REGISTRY", tc.registryEnv)
			got := TemplateImageRef(tc.runtime)
			if got != tc.want {
				t.Errorf("TemplateImageRef(%q) = %q, want %q", tc.runtime, got, tc.want)
			}
		})
	}
}
