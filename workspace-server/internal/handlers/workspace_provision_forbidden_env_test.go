package handlers

// workspace_provision_forbidden_env_test.go — Layer 1 tests for the
// RFC#523 tenant-workspace forbidden-env guardrail (task #146).
//
// Behaviour pinned (per RFC#523 §"Acceptance criteria" Layer 1):
//   - exact-match keys (GITEA_TOKEN, CP_ADMIN_API_TOKEN, RAILWAY_TOKEN,
//     INFISICAL_OPERATOR_TOKEN, …) are flagged
//   - MOLECULE_OPERATOR_* prefix family is flagged
//   - per-agent-scope vars (GIT_HTTP_USERNAME, ANTHROPIC_API_KEY,
//     AGENT_DEV_A_TOKEN, …) are NOT flagged — guard checks key NAME
//     not value
//   - findForbiddenTenantEnvKeys returns a deterministically-sorted
//     slice (canvas Events tab needs stable rendering)
//   - formatForbiddenTenantEnvError uses singular vs plural phrasing
//     so the message reads naturally for both 1-key and N-key cases
//
// Companion: provisioner.buildContainerEnv has the older silent-
// strip guard (forensic #145). The two layers are intentionally
// redundant — this one fails closed early; that one strips late.

import (
	"strings"
	"testing"
)

func TestIsForbiddenTenantEnvKey_ExactMatches(t *testing.T) {
	cases := []struct {
		key  string
		want bool
	}{
		// SCM-write tokens — kept in sync with provisioner.scmWriteTokenKeys.
		{"GITEA_TOKEN", true},
		{"GITEA_PAT", true},
		{"GITHUB_TOKEN", true},
		{"GITHUB_PAT", true},
		{"GH_TOKEN", true},
		{"GITLAB_TOKEN", true},
		{"GL_TOKEN", true},
		{"BITBUCKET_TOKEN", true},

		// Control-plane admin tokens.
		{"CP_ADMIN_API_TOKEN", true},
		{"CP_ADMIN_TOKEN", true},

		// Secret-store operator tokens.
		{"INFISICAL_OPERATOR_TOKEN", true},
		{"INFISICAL_BOOTSTRAP_TOKEN", true},

		// Infra-platform tokens.
		{"RAILWAY_TOKEN", true},
		{"RAILWAY_PERSONAL_API_TOKEN", true},
		{"HETZNER_TOKEN", true},
		{"HETZNER_API_TOKEN", true},

		// Per-agent scoped — must NOT be flagged.
		{"GIT_HTTP_USERNAME", false},
		{"GIT_HTTP_PASSWORD", false},
		{"ANTHROPIC_API_KEY", false},
		{"ANTHROPIC_AUTH_TOKEN", false},
		{"OPENAI_API_KEY", false},
		{"KIMI_API_KEY", false},
		{"MINIMAX_API_KEY", false},
		{"AGENT_DEV_A_TOKEN", false}, // hypothetical per-agent name
		{"MOLECULE_AGENT_ROLE", false},
		{"PARENT_ID", false},
		{"WORKSPACE_ID", false},
		{"PLATFORM_URL", false},
		{"", false},
	}
	for _, c := range cases {
		got := isForbiddenTenantEnvKey(c.key)
		if got != c.want {
			t.Errorf("isForbiddenTenantEnvKey(%q) = %v; want %v", c.key, got, c.want)
		}
	}
}

func TestIsForbiddenTenantEnvKey_PrefixMatches(t *testing.T) {
	cases := []struct {
		key  string
		want bool
	}{
		{"MOLECULE_OPERATOR_HOST", true},
		{"MOLECULE_OPERATOR_SSH_KEY", true},
		{"MOLECULE_OPERATOR_BACKUP_BUCKET", true},
		{"MOLECULE_OPERATOR_", true}, // prefix itself

		// Adjacent but NOT in prefix family.
		{"MOLECULE_AGENT_ROLE", false},
		{"MOLECULE_URL", false},
		{"MOLECULE_PERSONA_ROOT", false}, // path on operator host, not tenant
		{"MOLECULE_GITEA_TOKEN", false},  // localbuild-time only; not a tenant env
	}
	for _, c := range cases {
		got := isForbiddenTenantEnvKey(c.key)
		if got != c.want {
			t.Errorf("isForbiddenTenantEnvKey(%q) = %v; want %v", c.key, got, c.want)
		}
	}
}

func TestFindForbiddenTenantEnvKeys_NoneAndEmpty(t *testing.T) {
	if got := findForbiddenTenantEnvKeys(nil); len(got) != 0 {
		t.Errorf("nil envVars: got %v; want empty", got)
	}
	if got := findForbiddenTenantEnvKeys(map[string]string{}); len(got) != 0 {
		t.Errorf("empty envVars: got %v; want empty", got)
	}
	clean := map[string]string{
		"ANTHROPIC_API_KEY":   "sk-keep",
		"GIT_HTTP_USERNAME":   "agent-dev-a",
		"GIT_HTTP_PASSWORD":   "scoped-pat",
		"MOLECULE_AGENT_ROLE": "agent-dev-a",
		"WORKSPACE_ID":        "ws-123",
	}
	if got := findForbiddenTenantEnvKeys(clean); len(got) != 0 {
		t.Errorf("clean envVars: got %v; want empty", got)
	}
}

func TestFindForbiddenTenantEnvKeys_SingleAndMultipleSorted(t *testing.T) {
	// Single key.
	single := map[string]string{
		"ANTHROPIC_API_KEY": "sk-keep",
		"GITEA_TOKEN":       "operator-scope-leak",
	}
	got := findForbiddenTenantEnvKeys(single)
	if len(got) != 1 || got[0] != "GITEA_TOKEN" {
		t.Errorf("single forbidden: got %v; want [GITEA_TOKEN]", got)
	}

	// Multiple keys — must be sorted (canvas Events tab needs stability).
	multi := map[string]string{
		"RAILWAY_TOKEN":          "z",
		"GITEA_TOKEN":            "a",
		"MOLECULE_OPERATOR_HOST": "m",
		"CP_ADMIN_API_TOKEN":     "c",
		"ANTHROPIC_API_KEY":      "ok",
	}
	got = findForbiddenTenantEnvKeys(multi)
	want := []string{"CP_ADMIN_API_TOKEN", "GITEA_TOKEN", "MOLECULE_OPERATOR_HOST", "RAILWAY_TOKEN"}
	if len(got) != len(want) {
		t.Fatalf("multi forbidden length: got %v; want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("multi forbidden[%d] = %q; want %q (full got=%v want=%v)", i, got[i], want[i], got, want)
		}
	}
}

func TestFormatForbiddenTenantEnvError_Phrasing(t *testing.T) {
	// Empty input — defensive total function.
	if msg := formatForbiddenTenantEnvError(nil); !strings.Contains(msg, "RFC#523") {
		t.Errorf("empty input: missing RFC#523 ref: %q", msg)
	}

	// Singular phrasing.
	single := formatForbiddenTenantEnvError([]string{"GITEA_TOKEN"})
	if !strings.Contains(single, `"GITEA_TOKEN"`) {
		t.Errorf("single: missing quoted key: %q", single)
	}
	if !strings.Contains(single, "operator-scope") {
		t.Errorf("single: missing operator-scope phrase: %q", single)
	}
	if !strings.Contains(single, "RFC#523") {
		t.Errorf("single: missing RFC#523 ref: %q", single)
	}
	if strings.Contains(single, "env vars ") { // plural form
		t.Errorf("single: leaked plural phrasing: %q", single)
	}

	// Plural phrasing.
	multi := formatForbiddenTenantEnvError([]string{"CP_ADMIN_API_TOKEN", "GITEA_TOKEN"})
	if !strings.Contains(multi, "CP_ADMIN_API_TOKEN, GITEA_TOKEN") {
		t.Errorf("plural: missing joined list: %q", multi)
	}
	if !strings.Contains(multi, "env vars ") {
		t.Errorf("plural: missing plural phrase: %q", multi)
	}
}
