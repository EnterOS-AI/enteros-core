package sessionid

import "testing"

// TestDefaultContextID pins the derivation and its stability — the property the
// runtime relies on for session resumption: the id is a pure function of the
// workspace id (same in → same out), so it never drifts across turns / restarts.
func TestDefaultContextID(t *testing.T) {
	const ws = "ea3cfcf1-cb9c-53b4-90fd-c53123569c4a"
	if got := DefaultContextID(ws); got != "canvas-"+ws {
		t.Fatalf("DefaultContextID(%q) = %q, want %q", ws, got, "canvas-"+ws)
	}
	// Pure function of the workspace id: distinct ids never collide (the property
	// runtime session resumption relies on — one stable id per workspace).
	if DefaultContextID("a") == DefaultContextID("b") {
		t.Fatal("DefaultContextID must depend on the workspace id")
	}
}

// TestDefaultSessionContextEnv pins the env-var name the provisioner sets and
// the shared runtime reads. It is a cross-repo contract string — changing it
// here without changing the runtime reader silently breaks convergence.
func TestDefaultSessionContextEnv(t *testing.T) {
	if DefaultSessionContextEnv != "MOLECULE_DEFAULT_SESSION_CONTEXT_ID" {
		t.Fatalf("env var name changed to %q — update molecule_runtime "+
			"a2a_client.default_self_turn_context_id to match", DefaultSessionContextEnv)
	}
}
