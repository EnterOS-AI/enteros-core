package provisioner

import (
	"archive/tar"
	"errors"
	"io"
	"runtime"
	"strings"
	"testing"
)

// These tests pin the P0 fix for the fleet-wide list_peers 401 (Hermes and
// every other template): the workspace-server token-injection paths wrote
// /configs/.auth_token (and /configs/.platform_inbound_secret) as root:root
// AFTER the template entrypoint's `chown -R agent:agent /configs` ran, so the
// agent-uid (1000) MCP server (a2a_mcp_server, running via `gosu agent`) hit
// `[Errno 13] Permission denied` reading the bearer → empty bearer → platform
// 401 on /registry/{id}/peers (the literal tool_list_peers path).
//
// The agent uid is 1000:1000, verified from the templates:
//   - workspace-configs-templates/claude-code-default/Dockerfile: `useradd -u 1000 ... agent`
//   - workspace-configs-templates/hermes/Dockerfile:               `useradd -u 1000 ... agent`
//   - workspace/entrypoint.sh / claude-code-default/entrypoint.sh:  `exec gosu agent` ("uid 1000")
//
// Both tests assert the real artifact (the tar headers Docker's CopyToContainer
// honours for ownership, and the literal shell command the throwaway alpine
// container runs), not a mock that bypasses ownership. They FAIL on pre-fix
// code (no Uid/Gid in tar headers; no chown in the alpine command → root:root)
// and PASS post-fix (agent-owned).

// TestWriteFilesToContainerTar_FilesAreAgentOwned covers the issue #418
// post-start re-injection path (WriteFilesToContainer): the tar it streams
// into /configs via CopyToContainer must carry Uid/Gid = agent (1000) so the
// extracted files land agent-readable, not root:root. This is the path that
// (re)writes BOTH .auth_token and .platform_inbound_secret on a cadence.
func TestWriteFilesToContainerTar_FilesAreAgentOwned(t *testing.T) {
	files := map[string][]byte{
		".auth_token":              []byte("tok-abc123"),
		".platform_inbound_secret": []byte("inbound-secret-xyz"),
		"nested/dir/file.txt":      []byte("data"),
	}

	buf, err := buildConfigFilesTar(files)
	if err != nil {
		t.Fatalf("buildConfigFilesTar: %v", err)
	}

	tr := tar.NewReader(buf)
	seen := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read tar: %v", err)
		}
		if _, err := io.Copy(io.Discard, tr); err != nil {
			t.Fatalf("drain %s: %v", hdr.Name, err)
		}
		seen[hdr.Name] = true
		if hdr.Uid != AgentUID {
			t.Fatalf("tar entry %q Uid = %d, want %d (agent) — root-owned injection causes the list_peers 401",
				hdr.Name, hdr.Uid, AgentUID)
		}
		if hdr.Gid != AgentGID {
			t.Fatalf("tar entry %q Gid = %d, want %d (agent)", hdr.Name, hdr.Gid, AgentGID)
		}
	}

	for _, want := range []string{".auth_token", ".platform_inbound_secret"} {
		if !seen[want] {
			t.Fatalf("tar missing %q (seen: %v)", want, seen)
		}
	}
}

// TestBuildConfigFilesTar_SlashOnlyEntryNames pins the Windows-host fix: tar
// entry names (including parent-dir headers) must be slash-only regardless of
// host OS and even when a caller built the map key with filepath.Join on
// Windows. A backslash dir header ("nested\dir/") extracts on the Linux
// daemon as ONE flat literal-backslash filename — same bug class as the
// 2026-07-19 plugin-delivery incident.
func TestBuildConfigFilesTar_SlashOnlyEntryNames(t *testing.T) {
	files := map[string][]byte{
		"nested/dir/file.txt": []byte("data"),
		".auth_token":         []byte("tok"),
	}
	// Literal-backslash key exercises the normalization only on Windows:
	// filepath.ToSlash is a no-op on Linux, where a backslash is a legal
	// filename character.
	wantNames := []string{"nested/dir/", "nested/dir/file.txt", ".auth_token"}
	if runtime.GOOS == "windows" {
		files["win\\style\\key.txt"] = []byte("from filepath.Join on Windows")
		wantNames = append(wantNames, "win/style/key.txt")
	}

	buf, err := buildConfigFilesTar(files)
	if err != nil {
		t.Fatalf("buildConfigFilesTar: %v", err)
	}

	tr := tar.NewReader(buf)
	seen := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read tar: %v", err)
		}
		if _, err := io.Copy(io.Discard, tr); err != nil {
			t.Fatalf("drain %s: %v", hdr.Name, err)
		}
		if strings.Contains(hdr.Name, `\`) {
			t.Fatalf("tar entry %q contains a backslash — Windows path separator leaked into the tar", hdr.Name)
		}
		if hdr.Typeflag == tar.TypeDir && !strings.HasSuffix(hdr.Name, "/") {
			t.Fatalf("dir entry %q missing trailing slash", hdr.Name)
		}
		seen[hdr.Name] = true
	}

	for _, want := range wantNames {
		if !seen[want] {
			t.Fatalf("tar missing %q (seen: %v)", want, seen)
		}
	}
}

// TestWriteAuthTokenVolumeCmd_ChownsToAgent covers the issue #1877 pre-start
// volume-write path (WriteAuthTokenToVolume): the throwaway alpine container
// writes /vol/.auth_token then chmod 0600 but, pre-fix, never chowns it, so it
// stays root:root (alpine runs the command as root). The literal command must
// chown the file to the agent uid:gid so the agent-uid MCP server can read it.
func TestWriteAuthTokenVolumeCmd_ChownsToAgent(t *testing.T) {
	cmd := writeAuthTokenVolumeCmd()

	if !strings.Contains(cmd, "chmod 0600 /vol/.auth_token") {
		t.Fatalf("alpine cmd lost the 0600 chmod (regression): %q", cmd)
	}

	wantChown := "chown 1000:1000 /vol/.auth_token"
	if !strings.Contains(cmd, wantChown) {
		t.Fatalf("alpine cmd = %q, missing %q — without it .auth_token stays root:root "+
			"and the agent-uid MCP server gets EACCES → empty bearer → list_peers 401",
			cmd, wantChown)
	}
}
