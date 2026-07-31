package plugins

import "testing"

// Reproduce the EXACT production sweep inputs observed on the reno-stars tenant.
func TestProdRepro_SpecForRealRow(t *testing.T) {
	schemes := []string{"local", "github", "gitea"}
	got := driftSpecForTrackedRef(
		"gitea://RenoStarsAI-production-client/reno-stars-coordinator#main",
		"ref:main", schemes)
	t.Logf("spec = %q", got)
	if got != "RenoStarsAI-production-client/reno-stars-coordinator#main" {
		t.Errorf("LEAK: got %q", got)
	}
}
