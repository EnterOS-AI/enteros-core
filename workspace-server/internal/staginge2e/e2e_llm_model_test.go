package staginge2e

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestE2ELLMModelFrom(t *testing.T) {
	if m, why := e2eLLMModelFrom(""); m != "google/gemini-3.1-flash-lite" || why != "" {
		t.Fatalf("unset = %q (%q), want the Flash-Lite default", m, why)
	}
	if m, why := e2eLLMModelFrom("  minimax/MiniMax-M3 "); m != "minimax/MiniMax-M3" || why != "" {
		t.Fatalf("override = %q (%q)", m, why)
	}
	for _, pricey := range []string{"google/gemini-4-hydrogen", "google/Gemini-4-Carbon", "gemini-4-hydrogen"} {
		if _, why := e2eLLMModelFrom(pricey); why == "" {
			t.Fatalf("%q was accepted as an e2e model", pricey)
		}
	}
	if _, why := e2eLLMModelFrom(e2eLLMModelDefault); why != "" {
		t.Fatalf("the default fails its own guard: %s", why)
	}
}

func TestAdminCreateOrgBody(t *testing.T) {
	raw, err := adminCreateOrgBody("e2e-mcp-1", `E2E "q"`, "molecules-server", []string{"google/gemini-3.1-flash-lite"})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"slug": "e2e-mcp-1", "name": `E2E "q"`, "owner_user_id": "e2e-runner:e2e-mcp-1",
		"provider": "molecules-server", "llm_allowed_models": []any{"google/gemini-3.1-flash-lite"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("body = %v, want %v", got, want)
	}
	raw, _ = adminCreateOrgBody("e2e-life-1", "E2E", "molecules-server", nil)
	var plain map[string]any
	if err := json.Unmarshal([]byte(raw), &plain); err != nil {
		t.Fatal(err)
	}
	if _, ok := plain["llm_allowed_models"]; ok {
		t.Fatalf("an unrestricted org must not send llm_allowed_models: %s", raw)
	}
}

func TestWorkspaceModelFromBody(t *testing.T) {
	if m, s := workspaceModelFromBody(`{"model":"google/gemini-3.1-flash-lite","source":"workspace_secrets"}`); m != "google/gemini-3.1-flash-lite" || s != "workspace_secrets" {
		t.Fatalf("got %q %q", m, s)
	}
	if m, s := workspaceModelFromBody(`{"model":"","source":"unresolved"}`); m != "" || s != "unresolved" {
		t.Fatalf("got %q %q", m, s)
	}
	if m, s := workspaceModelFromBody(`<html>`); m != "" || s != "" {
		t.Fatalf("got %q %q", m, s)
	}
}
