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

// DesktopProxyContainerName returns the name of a workspace's egress-proxy
// sidecar ("wsdeskproxy-<id>"). The proxy is the desktop's ONLY route off its
// internal network; it denies private/link-local destinations (see
// cmd/desktop-egress-proxy), so isolation is structural — no host firewall. The
// prefix shares "wsdesk" so both are reaped by the same label/name sweeps but is
// distinct from the desktop container's "wsdesk-" so they never collide.
func DesktopProxyContainerName(workspaceID string) string {
	return fmt.Sprintf("wsdeskproxy-%s", workspaceID)
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

// desktopSidecarPorts are the ONLY ports a resolved sidecar target may address:
// the control server (agent hands/eyes) and noVNC (human display). Any other
// port is rejected as an SSRF attempt by ValidateSidecarTarget.
var desktopSidecarPorts = map[string]struct{}{"6070": {}, "6080": {}}

// IsWellFormedWorkspaceID reports whether s uses only the hex-and-dash alphabet
// of a workspace UUID (and is non-empty). Anything else must NEVER be
// interpolated into a sidecar hostname — a value containing "/", "@", ":", or a
// dot could otherwise steer a proxy target to an attacker-chosen host.
func IsWellFormedWorkspaceID(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'F':
		case r == '-':
		default:
			return false
		}
	}
	return true
}

// ValidateSidecarTarget is the SSRF allowlist for the desktop display proxy and
// the computer-use gateway (design §13 / checklist "SSRF hostname allowlist for
// the sidecar"). It confirms host is EXACTLY a desktop-sidecar target —
// "wsdesk-<well-formed-id>:<control-or-vnc-port>" — before any request is
// forwarded to it. This is defence-in-depth: the host is normally server-built
// from an authenticated route's workspace id, but pinning the shape here means a
// malformed id, an unexpected port, or a compromised address resolver can never
// redirect the proxy to the cloud metadata IP, a backend service, or an
// arbitrary external host. Returns a non-nil error naming the rejection.
func ValidateSidecarTarget(host string) error {
	hostname, port, ok := strings.Cut(host, ":")
	if !ok || port == "" {
		return fmt.Errorf("sidecar target %q missing host:port", host)
	}
	if _, allowed := desktopSidecarPorts[port]; !allowed {
		return fmt.Errorf("sidecar target %q uses disallowed port %q (want 6070 or 6080)", host, port)
	}
	id := WorkspaceIDFromDesktopName(hostname)
	if id == "" {
		return fmt.Errorf("sidecar target %q hostname is not a %q sidecar", host, desktopContainerNamePrefix)
	}
	if !IsWellFormedWorkspaceID(id) {
		return fmt.Errorf("sidecar target %q has a malformed workspace id %q", host, id)
	}
	return nil
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
