package provisioner

import (
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
)

// TestWorkspaceKindPlatform_MatchesModels guards the duplicated constant: the
// provisioner-local WorkspaceKindPlatform MUST equal models.KindPlatform, else a
// kind='platform' concierge would be mis-identified on the provision path (Kind
// is forwarded to the CP for the concierge's config/identity overlay).
func TestWorkspaceKindPlatform_MatchesModels(t *testing.T) {
	if WorkspaceKindPlatform != models.KindPlatform {
		t.Fatalf("WorkspaceKindPlatform=%q != models.KindPlatform=%q — keep them in sync", WorkspaceKindPlatform, models.KindPlatform)
	}
}
