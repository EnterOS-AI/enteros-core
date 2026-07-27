package provisioner

import (
	"fmt"
	"strings"
)

// desktopContainerNamePrefix is the prefix every desktop-sidecar container +
// profile volume carries. It is DELIBERATELY "wsdesk-", NOT "ws-<id>-desktop".
//
// The tenant container namespace is "ws-<id>", and several tenant-oriented
// parsers derive a workspace id by stripping the "ws-" prefix
// (ContainerName / ListWorkspaceContainerIDPrefixes and the orphan sweeper).
// A "ws-<id>-desktop" name would be mis-parsed as the workspace id
// "<id>-desktop" — polluting queries — or, because it never matches a real
// row, silently never reaped, leaking the profile volume (a live-credential
// store) forever. "wsdesk-" shares no prefix with "ws-", so the tenant
// parsers never touch a sidecar; sidecars are found EXCLUSIVELY by
// LabelRole == RoleDesktop. See the design RFC:
// docs/superpowers/specs/2026-07-27-agent-desktop-sidecar-design.md §11.
const desktopContainerNamePrefix = "wsdesk-"

// LabelRole distinguishes a desktop sidecar container/volume from a tenant
// workspace container on the shared daemon. Tenant containers do NOT set it
// (absent == the implicit "workspace" role); desktop sidecars set it to
// RoleDesktop, so lifecycle + sweep code find and reap sidecars by label
// rather than by a name parse.
const LabelRole = "molecule.platform.role"

// RoleDesktop is the LabelRole value stamped on desktop-sidecar containers +
// their profile volumes.
const RoleDesktop = "desktop"

// DesktopContainerName returns the Docker container name for a workspace's
// desktop sidecar ("wsdesk-<id>"). See desktopContainerNamePrefix for why the
// prefix is not "ws-".
func DesktopContainerName(workspaceID string) string {
	return fmt.Sprintf("%s%s", desktopContainerNamePrefix, workspaceID)
}

// DesktopProfileVolumeName returns the Docker named volume holding the
// desktop's persistent browser profile (cookies / live logins). It survives
// scale-to-zero (StopDesktop) and is destroyed only by WipeProfile / prune.
func DesktopProfileVolumeName(workspaceID string) string {
	return fmt.Sprintf("%s%s-profile", desktopContainerNamePrefix, workspaceID)
}

// IsDesktopSidecarName reports whether a Docker container name is a desktop
// sidecar (carries the "wsdesk-" prefix). The LabelRole label is the
// authoritative signal; this is a cheap defensive name check.
func IsDesktopSidecarName(name string) bool {
	return strings.HasPrefix(name, desktopContainerNamePrefix)
}

// WorkspaceIDFromDesktopName extracts the workspace id from a desktop-sidecar
// container name, or "" if the name is not a sidecar name.
func WorkspaceIDFromDesktopName(name string) string {
	if !strings.HasPrefix(name, desktopContainerNamePrefix) {
		return ""
	}
	return strings.TrimPrefix(name, desktopContainerNamePrefix)
}

// desktopManagedLabels is the label map stamped on every desktop sidecar
// container + profile volume: the standard managed/instance ownership labels
// PLUS LabelRole=RoleDesktop, so the container is found and reaped by label,
// never by the "ws-" name parse.
func desktopManagedLabels() map[string]string {
	m := managedLabels()
	m[LabelRole] = RoleDesktop
	return m
}
