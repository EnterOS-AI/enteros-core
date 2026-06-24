package handlers

import (
	"context"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
)

// execReadStubProv is a LocalProvisionerAPI whose ExecRead returns a
// configurable config.yaml body, so we can simulate a container whose baked
// /configs/config.yaml runtime has drifted from the DB runtime column.
// Every other method panics — restartRuntimeFromConfig only calls ExecRead.
type execReadStubProv struct {
	configYAML string
	err        error
}

func (s *execReadStubProv) Start(_ context.Context, _ provisioner.WorkspaceConfig) (string, error) {
	panic("execReadStubProv.Start not implemented in test")
}
func (s *execReadStubProv) Stop(_ context.Context, _ string) error {
	panic("execReadStubProv.Stop not implemented in test")
}
func (s *execReadStubProv) IsRunning(_ context.Context, _ string) (bool, error) {
	panic("execReadStubProv.IsRunning not implemented in test")
}
func (s *execReadStubProv) ExecRead(_ context.Context, _, _ string) ([]byte, error) {
	if s.err != nil {
		return nil, s.err
	}
	return []byte(s.configYAML), nil
}
func (s *execReadStubProv) RemoveVolume(_ context.Context, _ string) error {
	panic("execReadStubProv.RemoveVolume not implemented in test")
}
func (s *execReadStubProv) VolumeHasFile(_ context.Context, _, _ string) (bool, error) {
	panic("execReadStubProv.VolumeHasFile not implemented in test")
}
func (s *execReadStubProv) WriteAuthTokenToVolume(_ context.Context, _, _ string) error {
	panic("execReadStubProv.WriteAuthTokenToVolume not implemented in test")
}

var _ provisioner.LocalProvisionerAPI = (*execReadStubProv)(nil)

// TestRestartRuntimeFromConfig_DBRuntimeWinsOverStaleConfig is the regression
// test for the runtime-switch-then-restart bug: the runtime-switch PATCH writes
// only the workspaces.runtime DB column (not the running container's
// /configs/config.yaml), so on restart the DB column is the SSOT. Pre-fix, the
// container's stale template-default config.yaml ("claude-code") won over the
// switched DB runtime ("google-adk") AND stomped the DB column — so a switched
// runtime box was never re-provisioned.
//
// Here the DB runtime is "google-adk" while the container config.yaml still
// declares "claude-code"; the restart must re-provision with "google-adk".
func TestRestartRuntimeFromConfig_DBRuntimeWinsOverStaleConfig(t *testing.T) {
	h := &WorkspaceHandler{
		provisioner: &execReadStubProv{configYAML: "runtime: claude-code\nmodel: x\n"},
	}

	got := h.restartRuntimeFromConfig(context.Background(), "ws-1", "adk-test", "google-adk", false)

	if got != "google-adk" {
		t.Fatalf("restartRuntimeFromConfig returned %q; want the DB SSOT runtime %q (the stale config.yaml=claude-code must NOT win)", got, "google-adk")
	}
}

// TestRestartRuntimeFromConfig_ApplyTemplateShortCircuits verifies that
// apply_template=true bypasses the container read entirely and returns the DB
// runtime (the existing fast path).
func TestRestartRuntimeFromConfig_ApplyTemplateShortCircuits(t *testing.T) {
	// ExecRead would panic if reached; apply_template=true must short-circuit.
	h := &WorkspaceHandler{provisioner: &execReadStubProv{}}

	got := h.restartRuntimeFromConfig(context.Background(), "ws-1", "adk-test", "google-adk", true)

	if got != "google-adk" {
		t.Fatalf("restartRuntimeFromConfig (apply_template) returned %q; want %q", got, "google-adk")
	}
}

// TestRestartRuntimeFromConfig_NilProvisionerReturnsDB verifies the SaaS path
// (no local Docker provisioner) trusts the DB runtime.
func TestRestartRuntimeFromConfig_NilProvisionerReturnsDB(t *testing.T) {
	h := &WorkspaceHandler{provisioner: nil}

	got := h.restartRuntimeFromConfig(context.Background(), "ws-1", "adk-test", "google-adk", false)

	if got != "google-adk" {
		t.Fatalf("restartRuntimeFromConfig (nil provisioner) returned %q; want %q", got, "google-adk")
	}
}

// TestRestartRuntimeFromConfig_MatchingConfigReturnsDB verifies the no-drift
// case: when config.yaml agrees with the DB, the DB runtime is returned.
func TestRestartRuntimeFromConfig_MatchingConfigReturnsDB(t *testing.T) {
	h := &WorkspaceHandler{
		provisioner: &execReadStubProv{configYAML: "runtime: google-adk\n"},
	}

	got := h.restartRuntimeFromConfig(context.Background(), "ws-1", "adk-test", "google-adk", false)

	if got != "google-adk" {
		t.Fatalf("restartRuntimeFromConfig returned %q; want %q", got, "google-adk")
	}
}
