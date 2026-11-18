package handlers

// `set_by` used to be a constant.
//
// PatchPluginSettings read c.GetString("actor") and NOTHING in the server ever
// calls c.Set("actor") — so every override in the fleet was attributed to the
// literal string "operator" and the per-key provenance answered "who?" with a
// value that carried no information. These tests pin the real middleware keys
// (middleware.WorkspaceAuth, the only middleware these routes are mounted
// behind) to the actor strings they must produce.

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func actorFor(t *testing.T, keys map[string]any) string {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	for k, v := range keys {
		c.Set(k, v)
	}
	return settingsActor(c)
}

func TestSettingsActor_DerivesFromTheVerifiedCredential(t *testing.T) {
	for _, tc := range []struct {
		name string
		keys map[string]any
		want string
	}{
		{
			name: "cp session names the real user",
			keys: map[string]any{
				"cp_session_user_id":      "u-123",
				"cp_session_actor":        "session:deadbeefdeadbeef",
				"caller_credential_class": "cp-session",
			},
			want: "user:u-123",
		},
		{
			name: "cp session without a user id falls back to the session hash",
			keys: map[string]any{"cp_session_actor": "session:deadbeefdeadbeef"},
			want: "session:deadbeefdeadbeef",
		},
		{
			name: "org token contributes its PREFIX, never the token",
			keys: map[string]any{"org_token_prefix": "mol_ab12", "caller_credential_class": "org-token"},
			want: "org-token:mol_ab12",
		},
		{
			name: "admin token",
			keys: map[string]any{"caller_is_admin_token": true, "caller_credential_class": "admin-token"},
			want: "admin-token",
		},
		{
			name: "per-workspace token names its own workspace",
			keys: map[string]any{"authenticated_workspace_id": "ws-9", "caller_credential_class": "workspace-token"},
			want: "workspace:ws-9",
		},
		{
			name: "unclassified falls back to the credential class",
			keys: map[string]any{"caller_credential_class": "admin-token-tier3-fallback"},
			want: "admin-token-tier3-fallback",
		},
		{
			name: "nothing known says so rather than inventing an operator",
			keys: map[string]any{},
			want: "unknown",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := actorFor(t, tc.keys); got != tc.want {
				t.Errorf("settingsActor = %q, want %q", got, tc.want)
			}
		})
	}
}

// The regression itself: two DIFFERENT callers must not produce the SAME
// attribution. Pre-fix every one of these returned "operator".
func TestSettingsActor_DistinguishesDifferentCallers(t *testing.T) {
	seen := map[string]string{}
	for name, keys := range map[string]map[string]any{
		"alice-session": {"cp_session_user_id": "u-alice"},
		"bob-session":   {"cp_session_user_id": "u-bob"},
		"org-key":       {"org_token_prefix": "mol_ab12"},
		"admin":         {"caller_is_admin_token": true},
	} {
		actor := actorFor(t, keys)
		if prev, dup := seen[actor]; dup {
			t.Fatalf("%s and %s both attribute to %q — the provenance cannot answer 'who'",
				prev, name, actor)
		}
		seen[actor] = name
	}
	if _, constant := seen["operator"]; constant {
		t.Error(`no caller should attribute to the old hardcoded "operator"`)
	}
}
