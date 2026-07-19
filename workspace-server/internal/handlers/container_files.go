package handlers

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
)

// maxExecOutput limits container exec output to 5MB to prevent OOM.
const maxExecOutput = 5 * 1024 * 1024

// Docker-less config sink (molecules-server / local-docker tenant, #206): when
// this process runs INSIDE the workspace container with NO docker.sock, the
// Files API can neither docker-exec into a sibling runtime container nor write
// to `/configs` at its OWN filesystem root — the workspace-server runs as the
// non-root `canvas` user, so `mkdir /configs` fails with "permission denied"
// (the earlier assumption that `/configs` is a writable local mount was wrong;
// Dockerfile.tenant never creates it). The docker-less SINK is instead the
// per-workspace HOST-SIDE MIRROR `<hostStateDir>/<wsid>/configs` — the SAME dir
// the docker-less Files-API READ path serves from (hostSideConfigsRoot /
// bootconfig.go / provisioner.HostSideConfigsDir). Writing there keeps PUT/GET
// consistent and persists the bundle for re-delivery. See writeViaEphemeral /
// deleteViaEphemeral below.

// containedJoin joins relPath under base and refuses anything that escapes
// base after cleaning (absolute paths, ".." traversal). It is the os.*
// counterpart of the "escapes destPath" guard in copyFilesToContainer and the
// validateRelPath check in deleteViaEphemeral: the joined, cleaned absolute
// path MUST stay inside base. Returns the safe absolute path or an error.
func containedJoin(base, relPath string) (string, error) {
	clean := filepath.Clean(relPath)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes %s: %s", base, relPath)
	}
	joined := filepath.Join(base, clean)
	// Defence-in-depth: after the join, the resolved path must remain within
	// base. Guards against platform-specific filepath.Join behaviour where a
	// relative name containing ".." could still climb out of base.
	baseClean := filepath.Clean(base)
	if joined != baseClean && !strings.HasPrefix(joined, baseClean+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes %s: %s", base, relPath)
	}
	return joined, nil
}

// writeFilesLocal writes each file directly to disk under base, creating
// parent directories as needed. Every destination is run through containedJoin
// so a ".." or absolute name can never escape base (typically /configs).
// Used on the docker-less local-docker path (writeViaEphemeral fallback).
func writeFilesLocal(base string, files map[string]string) error {
	for name, content := range files {
		dst, err := containedJoin(base, name)
		if err != nil {
			return fmt.Errorf("unsafe file path: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("failed to create parent dir for %s: %w", name, err)
		}
		if err := os.WriteFile(dst, []byte(content), 0o644); err != nil {
			return fmt.Errorf("failed to write %s: %w", name, err)
		}
	}
	return nil
}

// deleteFileLocal removes a single file under base, refusing any path that
// escapes base. Mirrors `rm -f`: a missing file is not an error, matching the
// docker/EIC delete paths' "deleted or didn't exist" semantics. Used on the
// docker-less local-docker path (deleteViaEphemeral fallback).
func deleteFileLocal(base, relPath string) error {
	dst, err := containedJoin(base, relPath)
	if err != nil {
		return err
	}
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete %s: %w", relPath, err)
	}
	return nil
}

// findContainer finds a running container for the workspace.
// Checks provisioner name, full ID, and DB workspace name (same candidates as terminal handler).
func (h *TemplatesHandler) findContainer(ctx context.Context, workspaceID string) string {
	if h.docker == nil {
		return ""
	}
	name := provisioner.ContainerName(workspaceID)
	candidates := []string{name}
	if name != "ws-"+workspaceID {
		candidates = append(candidates, "ws-"+workspaceID)
	}
	// Also check by workspace name from DB
	var wsName string
	db.DB.QueryRowContext(ctx, `SELECT LOWER(REPLACE(name, ' ', '-')) FROM workspaces WHERE id = $1`, workspaceID).Scan(&wsName)
	if wsName != "" {
		candidates = append(candidates, wsName)
	}
	for _, c := range candidates {
		info, err := h.docker.ContainerInspect(ctx, c)
		if err == nil && info.State.Running {
			return c
		}
	}
	return ""
}

// execInContainer runs a command in a container and returns stdout (capped at maxExecOutput).
func (h *TemplatesHandler) execInContainer(ctx context.Context, containerName string, cmd []string) (string, error) {
	execCfg := container.ExecOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
	}
	execID, err := h.docker.ContainerExecCreate(ctx, containerName, execCfg)
	if err != nil {
		return "", err
	}
	resp, err := h.docker.ContainerExecAttach(ctx, execID.ID, container.ExecAttachOptions{})
	if err != nil {
		return "", err
	}
	defer resp.Close()
	var stdout, stderr bytes.Buffer
	// Use stdcopy to correctly demux Docker multiplexed stream (stdout/stderr)
	stdcopy.StdCopy(&stdout, &stderr, io.LimitReader(resp.Reader, maxExecOutput))
	// Surface the exit code — callers assume "non-zero exit → non-nil error"
	// (template_import.go branches its config-regeneration on `test -f`, and
	// DELETE /files reports rm failures), but without ContainerExecInspect a
	// failed exec looked identical to success (same masking that hid the
	// plugin-delivery corruption; see PluginsHandler.execInContainerAs).
	inspect, ierr := h.docker.ContainerExecInspect(ctx, execID.ID)
	if ierr != nil {
		return strings.TrimSpace(stdout.String()), fmt.Errorf("exec inspect %v: %w", cmd, ierr)
	}
	if inspect.ExitCode != 0 {
		errText := strings.TrimSpace(stderr.String())
		if errText == "" {
			errText = strings.TrimSpace(stdout.String())
		}
		return strings.TrimSpace(stdout.String()), fmt.Errorf("exec %v: exit %d: %s", cmd, inspect.ExitCode, errText)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// copyFilesToContainer creates a tar archive from a map of files and copies it into a container.
// The destPath is prepended to each file name. File names must be relative and must not escape
// destPath via ".." segments — otherwise the tar header name could escape the mounted volume.
func (h *TemplatesHandler) copyFilesToContainer(ctx context.Context, containerName, destPath string, files map[string]string) error {
	buf, err := buildContainerFilesTar(destPath, files)
	if err != nil {
		return err
	}
	return h.docker.CopyToContainer(ctx, containerName, destPath, buf, container.CopyToContainerOptions{})
}

// buildContainerFilesTar builds the archive copyFilesToContainer streams into
// the container. Pulled out as a pure function so the entry-name contract
// (slash-only, destPath-rooted, no escapes) is unit-testable without a daemon.
func buildContainerFilesTar(destPath string, files map[string]string) (*bytes.Buffer, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	// Tar entry names and destPath are CONTAINER (Linux) paths — build them
	// with path.* on ToSlash'd input, never filepath.* (on a Windows host
	// filepath.Join produces backslash entry names, which the Linux daemon
	// extracts as flat literal-backslash files; and the escape guard below
	// would false-positive because `\configs\x` has no `/configs` prefix).
	destSlash := path.Clean(filepath.ToSlash(destPath))
	createdDirs := map[string]bool{}
	for name, content := range files {
		// Block absolute paths and traversal attempts at the archive-write boundary.
		// Files are written inside destPath (typically /configs); anything that escapes
		// via ".." or an absolute name could reach other volumes or system paths.
		clean := path.Clean(filepath.ToSlash(name))
		if path.IsAbs(clean) || strings.HasPrefix(clean, "..") {
			return nil, fmt.Errorf("unsafe file path in archive: %s", name)
		}
		// Prepend destPath so relative paths land inside the volume mount.
		// Use cleaned name so validation (which checks clean) and usage stay consistent.
		archiveName := path.Join(destSlash, clean)
		// Defence-in-depth: ensure the joined path doesn't escape destPath.
		// This guards against a relative name containing ".." that survives
		// the check above yet still climbs out of the destination once joined.
		if !strings.HasPrefix(archiveName, destSlash+"/") && archiveName != destSlash {
			return nil, fmt.Errorf("path escapes destination: %s", name)
		}

		// Create parent directories in tar (deduplicated)
		dir := path.Dir(archiveName)
		if dir != destSlash && !createdDirs[dir] {
			tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeDir,
				Name:     dir + "/",
				Mode:     0755,
			})
			createdDirs[dir] = true
		}

		data := []byte(content)
		header := &tar.Header{
			Name: archiveName,
			Mode: 0644,
			Size: int64(len(data)),
		}
		if err := tw.WriteHeader(header); err != nil {
			return nil, fmt.Errorf("failed to write tar header for %s: %w", name, err)
		}
		if _, err := tw.Write(data); err != nil {
			return nil, fmt.Errorf("failed to write tar data for %s: %w", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("failed to close tar writer: %w", err)
	}

	return &buf, nil
}

// writeViaEphemeral writes files to a named volume using an ephemeral Alpine container.
// Used when the workspace container is offline (e.g., during provisioning).
func (h *TemplatesHandler) writeViaEphemeral(ctx context.Context, volumeName, workspaceID string, files map[string]string) error {
	if h.docker == nil {
		// No docker client → molecules-server (local-docker) tenant. Write to the
		// per-workspace HOST-SIDE MIRROR (the same dir the docker-less READ path
		// serves from), NOT `/configs` at the fs root (unwritable to `canvas` and
		// inconsistent with reads). This is what makes config.yaml Save&Restart
		// work on the default provider (no docker socket).
		mirror := h.hostSideConfigsRoot("/configs", workspaceID)
		if mirror == "" {
			return fmt.Errorf("docker-less config write unavailable: no host-side configs mirror for workspace %q (hostStateDir empty)", workspaceID)
		}
		return writeFilesLocal(mirror, files)
	}

	// Create ephemeral container mounting the volume
	resp, err := h.docker.ContainerCreate(ctx, &container.Config{
		Image: "alpine:latest",
		Cmd:   []string{"sleep", "10"},
	}, &container.HostConfig{
		Binds: []string{volumeName + ":/configs"},
	}, nil, nil, "")
	if err != nil {
		return fmt.Errorf("failed to create ephemeral container: %w", err)
	}
	defer h.docker.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})

	if err := h.docker.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("failed to start ephemeral container: %w", err)
	}

	// Copy files via tar, then stop container cleanly
	if err := h.copyFilesToContainer(ctx, resp.ID, "/configs", files); err != nil {
		return err
	}
	// Wait for container to be ready for removal (copy is synchronous, but be safe)
	timeout := 5
	h.docker.ContainerStop(ctx, resp.ID, container.StopOptions{Timeout: &timeout})
	return nil
}

// deleteViaEphemeral deletes a file from a named volume using an ephemeral container.
func (h *TemplatesHandler) deleteViaEphemeral(ctx context.Context, volumeName, workspaceID, filePath string) error {
	// CWE-78/CWE-22: exec form binds rm to the /configs volume regardless
	// of path traversal in filePath. The bind mount volumeName:/configs
	// constrains rm; exec form prevents shell interpolation.
	// validateRelPath is defense-in-depth (blocks ".." in raw input).
	// The concat form is the critical fix: rm receives ONE path argument
	// so ".." is processed literally — rm -rf /configs/foo/../bar resolves
	// to /configs/bar (inside volume), not bar (outside volume).
	//
	// Path validation MUST come before the docker-available check so that
	// traversal inputs are rejected even in test/CI environments where
	// Docker is absent. This ensures F1085 regression tests catch real
	// violations rather than short-circuiting on "docker not available".
	if err := validateRelPath(filePath); err != nil {
		return err
	}
	if h.docker == nil {
		// No docker client → local-docker tenant. Delete from the per-workspace
		// host-side mirror (same sink as the write path), not `/configs`. The
		// validateRelPath guard above already blocks ".." / absolute paths;
		// containedJoin re-verifies the resolved path stays under the mirror.
		mirror := h.hostSideConfigsRoot("/configs", workspaceID)
		if mirror == "" {
			return fmt.Errorf("docker-less config delete unavailable: no host-side configs mirror for workspace %q (hostStateDir empty)", workspaceID)
		}
		return deleteFileLocal(mirror, filePath)
	}

	resp, err := h.docker.ContainerCreate(ctx, &container.Config{
		Image: "alpine:latest",
		Cmd:   []string{"rm", "-rf", "/configs/" + filePath},
	}, &container.HostConfig{
		Binds: []string{volumeName + ":/configs"},
	}, nil, nil, "")
	if err != nil {
		return fmt.Errorf("failed to create ephemeral container: %w", err)
	}
	defer h.docker.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})

	if err := h.docker.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return err
	}
	// Wait for the rm command to finish before removing the container
	statusCh, errCh := h.docker.ContainerWait(ctx, resp.ID, container.WaitConditionNotRunning)
	select {
	case <-statusCh:
		return nil
	case err := <-errCh:
		return err
	}
}
