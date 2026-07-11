package handlers

import (
	"testing"
	"time"
)

// dockerProvisionTimeout is the wiring that decouples the Docker-mode build
// budget from the fixed 3-min provisioner.ProvisionTimeout: a runtime that
// declares a long provision_timeout_seconds must reach the build ceiling, and
// a runtime that declares nothing (or something short) must still get the
// sane 12-min floor — never the old 3-min cap.

// A runtime declaring a LONG timeout (hermes = 30 min) must NOT be capped at
// 3 min — the whole point of the change. The ceiling reaches the build ctx.
func TestDockerProvisionTimeout_LongRuntimeReachesBuildCeiling(t *testing.T) {
	dir := t.TempDir()
	writeTemplate(t, dir, "template-hermes", `
runtime: hermes
runtime_config:
  provision_timeout_seconds: 1800
`)
	h := &WorkspaceHandler{configsDir: dir}

	got := h.dockerProvisionTimeout("hermes")
	if got != 30*time.Minute {
		t.Fatalf("hermes docker provision timeout = %s, want 30m (must not be capped at 3m)", got)
	}
	// Explicit regression guard on the old fixed cap.
	if got <= 3*time.Minute {
		t.Fatalf("hermes timeout %s is <= the retired 3-min cap — build would be killed mid-flight", got)
	}
}

// A runtime that declares NOTHING falls back to the 12-min floor, not 3 min.
func TestDockerProvisionTimeout_NoDeclarationUsesFloorNot3Min(t *testing.T) {
	dir := t.TempDir()
	writeTemplate(t, dir, "template-claude-code", `
runtime: claude-code
runtime_config:
  model: anthropic:claude-opus
`)
	h := &WorkspaceHandler{configsDir: dir}

	got := h.dockerProvisionTimeout("claude-code")
	if got != dockerProvisionCeilingFloor {
		t.Fatalf("claude-code (no declaration) = %s, want the %s floor", got, dockerProvisionCeilingFloor)
	}
	if got == 3*time.Minute {
		t.Fatalf("no-declaration path regressed to the retired fixed 3-min cap")
	}
}

// A runtime declaring a SHORT timeout (< floor) is raised to the floor — the
// floor is a lower bound so a too-eager manifest can't re-introduce the brick.
func TestDockerProvisionTimeout_ShortDeclarationRaisedToFloor(t *testing.T) {
	dir := t.TempDir()
	writeTemplate(t, dir, "template-fast", `
runtime: fast-runtime
runtime_config:
  provision_timeout_seconds: 120
`)
	h := &WorkspaceHandler{configsDir: dir}

	got := h.dockerProvisionTimeout("fast-runtime")
	if got != dockerProvisionCeilingFloor {
		t.Fatalf("short declaration = %s, want raised to the %s floor", got, dockerProvisionCeilingFloor)
	}
}

// An unknown runtime (no manifest entry) also gets the floor.
func TestDockerProvisionTimeout_UnknownRuntimeUsesFloor(t *testing.T) {
	dir := t.TempDir()
	h := &WorkspaceHandler{configsDir: dir}
	if got := h.dockerProvisionTimeout("nope"); got != dockerProvisionCeilingFloor {
		t.Fatalf("unknown runtime = %s, want the %s floor", got, dockerProvisionCeilingFloor)
	}
}
