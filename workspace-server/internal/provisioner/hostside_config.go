package provisioner

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Host-side config-bundle mirror (#206 docker.sock boundary).
//
// A molecules-server (local-docker) tenant runs the workspace-server WITHOUT a
// docker.sock into its runtime containers (#206 — "do NOT docker-exec / mount
// docker.sock into tenants"). The Files API therefore cannot docker-exec
// `cat`/`find` into a child workspace container to read back the delivered
// /configs, and the AWS EIC path is retired for these tenants — so a
// docker-less Files read fell through to the host-side TEMPLATE dir, which is
// empty for a user-named workspace, and returned a misleading 59-byte
// "file not found (container offline, no template)" 404. That is the true
// blocker on template-delivery-e2e assertions C (config.yaml) + D (prompts).
//
// Fix: the tenant persists the SAME rendered config bundle it hands the CP at
// provision/reprovision into a host-side mirror keyed by workspace id, and the
// Files API serves docker-less reads from that mirror. The bundle is exactly
// what molecule_runtime/config_relay.py delivers into the container's /configs
// at boot, so the mirror is a faithful copy (SSOT: ONE render, two sinks — the
// CP config-relay into the container AND this host-side mirror for read-back).
// No docker.sock, no EIC, no routing a read through the control plane
// (core-OSS-no-CP-dependency preserved — the mirror is local to the tenant).

// hostSideConfigsSubdir is the per-workspace subpath under the state base dir
// that mirrors the container's /configs tree.
const hostSideConfigsSubdir = "configs"

// sanitizeWorkspaceID guards a workspace id before it is used as a path
// segment. Workspace ids are UUIDs; strip anything that is not a safe path
// atom so a hostile/corrupt id can never traverse out of the state base dir.
func sanitizeWorkspaceID(id string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return -1
		}
	}, strings.TrimSpace(id))
}

// HostSideConfigsDir returns the host-side mirror of a workspace's /configs
// tree: <baseDir>/<wsid>/configs. Returns "" when the feature is off (empty
// baseDir) or the workspace id sanitises to empty — callers treat "" as
// "no host-side mirror; fall through".
func HostSideConfigsDir(baseDir, workspaceID string) string {
	baseDir = strings.TrimSpace(baseDir)
	wsid := sanitizeWorkspaceID(workspaceID)
	if baseDir == "" || wsid == "" {
		return ""
	}
	return filepath.Join(baseDir, wsid, hostSideConfigsSubdir)
}

// safeBundleRelPath validates a bundle-relative path stays inside the mirror
// dir (defense-in-depth; collectCPConfigFiles already rejects traversal at the
// collect boundary, but the mirror writer must not trust that invariant).
func safeBundleRelPath(name string) (string, error) {
	clean := filepath.ToSlash(filepath.Clean(name))
	if clean == "." || clean == "" || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") || strings.Contains(clean, "/../") {
		return "", fmt.Errorf("hostside: unsafe bundle path %q", name)
	}
	return filepath.FromSlash(clean), nil
}

// PersistConfigBundleHostSide writes the rendered config bundle (config.yaml +
// prompts/*) into the host-side /configs mirror for workspaceID.
//
//   - configFiles is the base64-encoded map exactly as sent to the CP in the
//     provision request's ConfigFiles field (self-host / local TemplatePath).
//   - templateAssets is the raw-bytes map exactly as sent in TemplateAssets
//     (the RFC #2843 #24 non-secret asset channel — where the SaaS Gitea
//     fetcher lands config.yaml + prompts).
//
// Both carry only template-asset paths (config.yaml / prompts/*). Files are
// written tmp+rename so a concurrent read never sees a half-written file.
// Returns an error only on a genuine write failure; the caller logs loud but
// does NOT fail the provision — config is still delivered to the container via
// the CP relay, so a mirror-write failure degrades only the docker-less
// read-back, never delivery.
func PersistConfigBundleHostSide(baseDir, workspaceID string, configFiles map[string]string, templateAssets map[string][]byte) error {
	dir := HostSideConfigsDir(baseDir, workspaceID)
	if dir == "" {
		return nil // feature disabled / no state dir
	}
	if len(configFiles) == 0 && len(templateAssets) == 0 {
		return nil // nothing rendered (external runtime); leave the mirror absent
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("hostside: mkdir mirror %s: %w", dir, err)
	}
	write := func(name string, data []byte) error {
		rel, err := safeBundleRelPath(name)
		if err != nil {
			return err
		}
		dest := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return fmt.Errorf("hostside: mkdir parent for %s: %w", name, err)
		}
		tmp := dest + ".tmp"
		if err := os.WriteFile(tmp, data, 0o644); err != nil {
			return fmt.Errorf("hostside: write %s: %w", dest, err)
		}
		if err := os.Rename(tmp, dest); err != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("hostside: rename %s: %w", dest, err)
		}
		return nil
	}
	for name, b64 := range configFiles {
		data, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return fmt.Errorf("hostside: decode config file %q: %w", name, err)
		}
		if err := write(name, data); err != nil {
			return err
		}
	}
	for name, data := range templateAssets {
		if err := write(name, data); err != nil {
			return err
		}
	}
	return nil
}

// ResolveWorkspaceStateBaseDir resolves the host-side base dir for per-workspace
// config mirrors, ONCE, so the writer (CPProvisioner) and reader (Files API)
// are handed the SAME value and can never drift.
//
// Precedence:
//  1. MOLECULE_WORKSPACE_STATE_DIR (operator override) — used if creatable+writable.
//  2. /var/lib/molecule/workspaces — the durable default when writable (root/self-host).
//  3. <tmp>/molecule-workspace-state — always-writable fallback (the tenant
//     workspace-server runs as an unprivileged uid that may not own
//     /var/lib/molecule). A tenant restart re-provisions, which re-persists the
//     mirror, so a per-container-lifetime dir is sufficient for read-back.
//
// The returned dir is guaranteed to exist and be writable.
func ResolveWorkspaceStateBaseDir() string {
	candidates := []string{
		strings.TrimSpace(os.Getenv("MOLECULE_WORKSPACE_STATE_DIR")),
		"/var/lib/molecule/workspaces",
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if dirWritable(c) {
			return c
		}
	}
	fallback := filepath.Join(os.TempDir(), "molecule-workspace-state")
	_ = os.MkdirAll(fallback, 0o755)
	return fallback
}

// dirWritable returns true if dir can be created and a file written under it.
func dirWritable(dir string) bool {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	probe := filepath.Join(dir, ".molecule-write-probe")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return false
	}
	_ = f.Close()
	_ = os.Remove(probe)
	return true
}
