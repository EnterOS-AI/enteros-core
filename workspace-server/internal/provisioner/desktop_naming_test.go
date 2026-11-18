package provisioner

import (
	"strings"
	"testing"
)

// The load-bearing invariant (design §11): a desktop sidecar name MUST NOT be
// parseable as a tenant workspace id. Every tenant-oriented parser strips the
// "ws-" prefix; "wsdesk-" shares no such prefix, so those parsers skip
// sidecars entirely — no mis-parse, no cross-reap, and (paired with the role
// label + a dedicated reap path) no leaked profile volume.
func TestDesktopNaming_DoesNotCollideWithTenantNamespace(t *testing.T) {
	const id = "abc12345-6789-4def-8123-56789abcdef0"

	name := DesktopContainerName(id)
	if want := "wsdesk-" + id; name != want {
		t.Fatalf("DesktopContainerName = %q, want %q", name, want)
	}
	if strings.HasPrefix(name, "ws-") {
		t.Fatalf("desktop name %q starts with the tenant prefix \"ws-\"; the sweeper would mis-parse it as workspace id %q", name, strings.TrimPrefix(name, "ws-"))
	}
	if strings.HasPrefix(name, containerNamePrefix) {
		t.Fatalf("desktop name %q must not carry the tenant container prefix %q", name, containerNamePrefix)
	}

	vol := DesktopProfileVolumeName(id)
	if strings.HasPrefix(vol, "ws-") {
		t.Fatalf("desktop profile volume %q must not carry the tenant \"ws-\" prefix", vol)
	}
	if !strings.HasSuffix(vol, "-profile") {
		t.Fatalf("desktop profile volume %q should end in -profile", vol)
	}

	// Round-trip + classification.
	if got := WorkspaceIDFromDesktopName(name); got != id {
		t.Fatalf("WorkspaceIDFromDesktopName(%q) = %q, want %q", name, got, id)
	}
	if got := WorkspaceIDFromDesktopName(ContainerName(id)); got != "" {
		t.Fatalf("WorkspaceIDFromDesktopName on a tenant name should be empty, got %q", got)
	}
	if !IsDesktopSidecarName(name) {
		t.Fatalf("IsDesktopSidecarName(%q) = false, want true", name)
	}
	if IsDesktopSidecarName(ContainerName(id)) {
		t.Fatalf("IsDesktopSidecarName(%q) = true, want false (tenant name)", ContainerName(id))
	}
}

// Sidecars must stay reap-able and per-instance-scoped: LabelManaged for the
// sweeper, LabelInstance for co-resident-platform ownership, plus the
// role label that is the ONLY way sidecars are found (they are outside the
// "ws-" name namespace).
func TestDesktopManagedLabels_CarriesRoleAndOwnership(t *testing.T) {
	m := desktopManagedLabels()
	if m[LabelRole] != RoleDesktop {
		t.Fatalf("desktop labels: %s = %q, want %q", LabelRole, m[LabelRole], RoleDesktop)
	}
	if m[LabelManaged] != "true" {
		t.Fatalf("desktop labels must keep %s=true so the sweeper can reap them, got %q", LabelManaged, m[LabelManaged])
	}
	if _, ok := m[LabelInstance]; !ok {
		t.Fatalf("desktop labels must carry %s for per-instance ownership on a shared daemon", LabelInstance)
	}
}
