package router

import "testing"

// TestLightRoutesAreRegisteredUnderWorkspaceAuth pins the Enter OS Light API
// (contracts C3/C5) to the WorkspaceAuth group of /workspaces/:id.
func TestLightRoutesAreRegisteredUnderWorkspaceAuth(t *testing.T) {
	have := map[string]bool{}
	for _, r := range registeredRoutes(t) {
		have[r.method+" "+r.path] = true
	}
	for _, want := range []string{
		"GET /workspaces/:id/light",
		"POST /workspaces/:id/light/enable",
		"POST /workspaces/:id/light/disable",
		"GET /workspaces/:id/light/runs",
		"POST /workspaces/:id/light/runs",
	} {
		if !have[want] {
			t.Errorf("route %s is not registered under the /workspaces/:id WorkspaceAuth group", want)
		}
	}
}
