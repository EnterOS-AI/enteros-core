package handlers

import (
	"encoding/json"
	"strings"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
)

// Desktop scale-to-zero idle timeout: validation bounds + omitempty round-trip
// through the compute jsonb (design §10, §12).
func TestWorkspaceCompute_DesktopIdleTimeout(t *testing.T) {
	base := func(sec int) models.WorkspaceCompute {
		return models.WorkspaceCompute{Display: models.WorkspaceComputeDisplay{
			Mode:               "desktop-control",
			IdleTimeoutSeconds: sec,
		}}
	}

	if err := validateWorkspaceCompute(base(600)); err != nil {
		t.Fatalf("valid idle timeout (600s) rejected: %v", err)
	}
	if err := validateWorkspaceCompute(base(0)); err != nil {
		t.Fatalf("unset idle timeout (0) should be allowed: %v", err)
	}
	if err := validateWorkspaceCompute(base(workspaceDesktopIdleMinSeconds - 1)); err == nil {
		t.Fatalf("idle timeout below floor must be rejected")
	}
	if err := validateWorkspaceCompute(base(workspaceDesktopIdleMaxSeconds + 1)); err == nil {
		t.Fatalf("idle timeout above ceiling must be rejected")
	}
	if err := validateWorkspaceCompute(base(-1)); err == nil {
		t.Fatalf("negative idle timeout must be rejected")
	}

	// omitempty: unset value must not appear in the serialized jsonb.
	jZero, err := workspaceComputeJSON(base(0))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(jZero, "idle_timeout_seconds") {
		t.Fatalf("unset idle timeout must be omitted, got %s", jZero)
	}

	// set value survives a full round-trip.
	jSet, err := workspaceComputeJSON(base(600))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(jSet, "idle_timeout_seconds") {
		t.Fatalf("set idle timeout must serialize, got %s", jSet)
	}
	var back models.WorkspaceCompute
	if err := json.Unmarshal([]byte(jSet), &back); err != nil {
		t.Fatal(err)
	}
	if back.Display.IdleTimeoutSeconds != 600 {
		t.Fatalf("round-trip idle timeout = %d, want 600", back.Display.IdleTimeoutSeconds)
	}
}
