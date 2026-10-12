package handlers

// READING /configs BACK OFF A WORKSPACE — the missing counterpart of
// writePluginSettingsToWorkspace.
//
// Two separate defects came from the same root cause: layer 6 could WRITE to a
// workspace on three backends but could only READ from one.
//
//   - GetPluginDeclaration's readPluginManifestFromWorkspace had a single
//     `findContainer` → `docker exec` branch. findContainer returns "" whenever
//     h.docker == nil — the docker-less CP tenant shape and EVERY SaaS/EC2
//     workspace — so a declared AND installed plugin returned 404 there. Its own
//     comment claimed it "Mirrors ReadFile's backend dispatch"; it did not.
//   - PatchPluginSettings delivered the effective settings WHOLESALE without
//     ever reading the file it was replacing, so with an unpopulated `config`
//     column it deleted every template-supplied key (see plugin_settings_api.go).
//
// So the dispatch is written ONCE, here, in the same three-way shape the writer
// uses (plugin_settings_writer.go: EIC → live container → host-side mirror), and
// both callers go through it.
//
// FAILURE MODES ARE NOT INTERCHANGEABLE. "the file is not there", "the box is
// unreachable" and "the backend errored" are three different answers to an
// operator and must not collapse into one status. They are three distinct
// sentinels here, and GetPluginDeclaration maps them to 404 / 503 / 502.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
)

var (
	// errWorkspaceFileAbsent — a backend WAS reached and the file is genuinely
	// not in the workspace's /configs tree. For a plugin manifest this means
	// "not installed", which is a legitimate 404.
	errWorkspaceFileAbsent = errors.New("file is not present in the workspace's /configs")
	// errWorkspaceUnreachable — no backend could answer for this workspace
	// (no docker socket, no EC2 instance, no host-side mirror; or the mirror
	// cannot hold this path at all). We know NOTHING about the file. This is a
	// 503, never a 404: reporting "not installed" because we could not look
	// would be a fabricated answer.
	errWorkspaceUnreachable = errors.New("cannot read the workspace's /configs")
	// errWorkspaceReadFailed — a backend was reached and FAILED (docker exec
	// error, EIC tunnel error, unreadable mirror file). A 502.
	errWorkspaceReadFailed = errors.New("the workspace's /configs backend returned an error")
)

// fileAbsentSentinel is emitted on STDOUT by the container probe when the file
// does not exist, so a missing file (exit 0, sentinel) is distinguishable from
// a failed exec (non-zero exit / docker error) without parsing an error string.
// A `cat` that simply fails gives the two cases the same shape, which is how
// "the box is down" and "the plugin is not installed" became one 404.
//
// PRINTABLE ASCII ONLY, and matched on the WHOLE trimmed output. It travels in
// the exec argv, and a NUL byte there makes the argv itself invalid — docker
// answers `exec /bin/sh: invalid argument` and every read fails as a backend
// error. (Found by running it against a real container; no unit test reaches
// this branch.) Matching the entire output rather than a substring means a file
// that merely CONTAINS the marker is still returned as content.
const fileAbsentSentinel = "__MOLECULE_CONFIGS_FILE_ABSENT_7f3a1c__"

// hostSideMirrorCanServe reports whether a /configs-relative path is inside the
// bundle the host-side mirror actually holds.
//
// PersistConfigBundleHostSide writes configFiles + templateAssets — config.yaml,
// prompts/*, and the core-generated plugin-settings/*.json. It NEVER writes
// plugins/<name>/: that tree is staged INSIDE the box by the post-online plugin
// reconcile (the EIC leg literally `rm -rf`s and re-stages it), so it has no
// host-side copy by construction.
//
// A miss under plugins/ therefore means "we cannot see the box", NOT "the
// plugin is not installed" — and those two must not return the same status.
// Returning false here routes such a read to errWorkspaceUnreachable (503).
func hostSideMirrorCanServe(rel string) bool {
	return !strings.HasPrefix(path.Clean(rel), "plugins/")
}

// readWorkspaceConfigFile fetches one /configs-relative file off a workspace,
// mirroring writePluginSettingsToWorkspace's backend dispatch exactly.
//
// rel must be relative and must not escape /configs; callers pass paths they
// built themselves from a validated install name.
func (h *TemplatesHandler) readWorkspaceConfigFile(
	ctx context.Context, workspaceID, rel string,
) ([]byte, error) {
	if err := validateRelPath(rel); err != nil {
		return nil, fmt.Errorf("read %s from %s: %w", rel, workspaceID, err)
	}

	// Same lookup, same isEC2InstanceID gate, as the writer: a local-docker
	// workspace persists its container NAME in instance_id and must NOT be
	// routed to the AWS-only EIC tunnel.
	var instanceID, runtime string
	if db.DB != nil {
		if err := db.DB.QueryRowContext(ctx,
			`SELECT COALESCE(instance_id, ''), COALESCE(runtime, '') FROM workspaces WHERE id = $1`,
			workspaceID,
		).Scan(&instanceID, &runtime); err != nil && err != sql.ErrNoRows {
			return nil, fmt.Errorf("%w: workspace lookup for %s: %v", errWorkspaceReadFailed, workspaceID, err)
		}
	}

	// 1. EC2 — read through the EIC tunnel.
	if isEC2InstanceID(instanceID) {
		content, err := readFileViaEIC(ctx, instanceID, runtime, "/configs", rel)
		switch {
		case err == nil:
			return content, nil
		case errors.Is(err, os.ErrNotExist):
			return nil, fmt.Errorf("%w: %s on %s", errWorkspaceFileAbsent, rel, workspaceID)
		default:
			return nil, fmt.Errorf("%w: EIC read %s from %s: %v", errWorkspaceReadFailed, rel, workspaceID, err)
		}
	}

	// 2. Local docker, container UP — probe and cat in one exec so a missing
	//    file is reported on stdout rather than as an indistinguishable error.
	if containerName := h.findContainer(ctx, workspaceID); containerName != "" {
		abs := path.Join("/configs", rel)
		out, err := h.execInContainer(ctx, containerName, []string{
			"sh", "-c", `if [ -f "$0" ]; then cat -- "$0"; else printf %s "` + fileAbsentSentinel + `"; fi`, abs,
		})
		if err != nil {
			return nil, fmt.Errorf("%w: container read %s from %s: %v", errWorkspaceReadFailed, rel, workspaceID, err)
		}
		if strings.TrimSpace(out) == fileAbsentSentinel {
			return nil, fmt.Errorf("%w: %s on %s", errWorkspaceFileAbsent, rel, workspaceID)
		}
		return []byte(out), nil
	}

	// 3. Host-side /configs mirror — the docker-less molecules-server / CP
	//    tenant shape, and the read-back half of the writer's ephemeral leg.
	mirror := h.hostSideConfigsRoot("/configs", workspaceID)
	if mirror == "" {
		return nil, fmt.Errorf("%w: %s has no EC2 instance, no running container and no host-side mirror",
			errWorkspaceUnreachable, workspaceID)
	}
	if !hostSideMirrorCanServe(rel) {
		// The mirror never carries this subtree, so its absence is not evidence.
		return nil, fmt.Errorf("%w: %s is staged inside the box by the plugin reconcile and has no "+
			"host-side copy; this workspace has no container or EIC leg to read it from",
			errWorkspaceUnreachable, rel)
	}
	if fi, statErr := os.Stat(mirror); statErr != nil || !fi.IsDir() {
		return nil, fmt.Errorf("%w: no host-side /configs mirror at %s (workspace not provisioned through it)",
			errWorkspaceUnreachable, mirror)
	}
	full, jerr := containedJoin(mirror, rel)
	if jerr != nil {
		return nil, fmt.Errorf("read %s from %s: %w", rel, workspaceID, jerr)
	}
	data, rerr := os.ReadFile(full)
	switch {
	case rerr == nil:
		return data, nil
	case os.IsNotExist(rerr):
		return nil, fmt.Errorf("%w: %s on %s", errWorkspaceFileAbsent, rel, workspaceID)
	default:
		return nil, fmt.Errorf("%w: mirror read %s: %v", errWorkspaceReadFailed, filepath.Base(full), rerr)
	}
}
