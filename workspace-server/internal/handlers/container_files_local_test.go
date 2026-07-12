package handlers

// container_files_local_test.go — coverage for the docker-less local
// filesystem path (FIX #2). On a molecules-server (local-docker) tenant the
// workspace-server runs INSIDE the workspace container with no docker socket,
// so the Files API WriteFile / DeleteFile fall back to writing directly under
// the container's /configs mount. These tests exercise that path against a
// temp base dir and pin the path-traversal containment.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestContainedJoin_ContainmentMatrix pins the base-containment guard shared by
// the local write and delete paths: legitimate relative names resolve under
// base; absolute names and ".." traversal are refused.
func TestContainedJoin_ContainmentMatrix(t *testing.T) {
	base := "/configs"
	cases := []struct {
		name    string
		in      string
		wantErr bool
		wantAbs string // expected resolved path when no error
	}{
		{"simple file", "config.yaml", false, "/configs/config.yaml"},
		{"nested file", "prompts/system.md", false, "/configs/prompts/system.md"},
		{"dot-normalized", "prompts/./system.md", false, "/configs/prompts/system.md"},
		{"hidden file", ".env", false, "/configs/.env"},
		{"absolute rejected", "/etc/passwd", true, ""},
		{"leading dotdot rejected", "../etc/passwd", true, ""},
		{"deep leading dotdot rejected", "../../root/.ssh/authorized_keys", true, ""},
		{"mid traversal rejected", "foo/../../../etc/cron.d", true, ""},
		{"bare dotdot rejected", "..", true, ""},
		{"dotdot slash rejected", "../..", true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := containedJoin(base, tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("containedJoin(%q, %q): want error, got %q", base, tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("containedJoin(%q, %q): unexpected error: %v", base, tc.in, err)
			}
			if got != tc.wantAbs {
				t.Errorf("containedJoin(%q, %q) = %q, want %q", base, tc.in, got, tc.wantAbs)
			}
			if !strings.HasPrefix(got, filepath.Clean(base)) {
				t.Errorf("resolved path %q escaped base %q", got, base)
			}
		})
	}
}

// TestWriteFilesLocal_WritesUnderBaseAndCreatesDirs verifies the local write
// path writes each file (creating parent dirs) under the base, and reads back
// the exact content — the config.yaml Save case on a molecules-server tenant.
func TestWriteFilesLocal_WritesUnderBaseAndCreatesDirs(t *testing.T) {
	base := t.TempDir()
	files := map[string]string{
		"config.yaml":       "tier: saas\n",
		"prompts/system.md": "you are helpful\n",
	}
	if err := writeFilesLocal(base, files); err != nil {
		t.Fatalf("writeFilesLocal: %v", err)
	}
	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(base, name))
		if err != nil {
			t.Fatalf("read back %s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", name, string(got), want)
		}
	}
}

// TestWriteFilesLocal_RejectsTraversal ensures a ".." name never escapes base,
// and — critically — that no file is created outside base when a batch mixes a
// safe file with a traversing one.
func TestWriteFilesLocal_RejectsTraversal(t *testing.T) {
	base := t.TempDir()
	// Sibling dir that the traversal would try to reach.
	outside := filepath.Join(filepath.Dir(base), "escaped-secret.txt")
	_ = os.Remove(outside)

	err := writeFilesLocal(base, map[string]string{"../escaped-secret.txt": "pwned"})
	if err == nil {
		t.Fatalf("writeFilesLocal accepted a traversal path")
	}
	if !strings.Contains(err.Error(), "escapes") && !strings.Contains(err.Error(), "unsafe") {
		t.Errorf("expected containment error, got %v", err)
	}
	if _, statErr := os.Stat(outside); statErr == nil {
		t.Errorf("traversal wrote a file OUTSIDE base at %s", outside)
		_ = os.Remove(outside)
	}
}

// TestDeleteFileLocal_ContainedAndTolerant verifies local delete removes a file
// under base, treats a missing file as success (rm -f semantics), and refuses
// to delete anything outside base.
func TestDeleteFileLocal_ContainedAndTolerant(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "sub", "gone.txt")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := deleteFileLocal(base, "sub/gone.txt"); err != nil {
		t.Fatalf("deleteFileLocal(existing): %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("file still present after delete: %v", err)
	}

	// Missing file is not an error (rm -f semantics).
	if err := deleteFileLocal(base, "sub/gone.txt"); err != nil {
		t.Errorf("deleteFileLocal(missing) should be nil, got %v", err)
	}

	// Traversal outside base must be refused. Plant a decoy sibling and confirm
	// it survives.
	decoy := filepath.Join(filepath.Dir(base), "decoy.txt")
	if err := os.WriteFile(decoy, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(decoy)
	if err := deleteFileLocal(base, "../decoy.txt"); err == nil {
		t.Errorf("deleteFileLocal accepted a traversal path")
	}
	if _, err := os.Stat(decoy); err != nil {
		t.Errorf("traversal deleted a file OUTSIDE base: %v", err)
	}
}

// TestWriteViaEphemeral_NilDocker_UsesLocalPath confirms writeViaEphemeral no
// longer errors "docker not available" when h.docker is nil, and that traversal
// is still rejected on that path (defence in depth via containedJoin).
func TestWriteViaEphemeral_NilDocker_UsesLocalPath(t *testing.T) {
	h := &TemplatesHandler{docker: nil}
	// A traversal name must be rejected (not silently written, not "docker
	// not available"). We can't assert a successful write to /configs in a
	// unit test (not writable / not present), but the rejection proves we
	// took the local path rather than the old docker-nil early return.
	err := h.writeViaEphemeral(context.Background(), "ws-vol", map[string]string{"../escape": "x"})
	if err == nil {
		t.Fatalf("expected traversal rejection on local path, got nil")
	}
	if strings.Contains(err.Error(), "docker not available") {
		t.Errorf("writeViaEphemeral still returns 'docker not available' — FIX #2 regressed: %v", err)
	}
	if !strings.Contains(err.Error(), "escapes") && !strings.Contains(err.Error(), "unsafe") {
		t.Errorf("expected containment error, got %v", err)
	}
}

// TestDeleteViaEphemeral_NilDocker_UsesLocalPath confirms the docker-nil delete
// path is contained (validateRelPath + containedJoin) and no longer short-
// circuits on "docker not available" for a safe path.
func TestDeleteViaEphemeral_NilDocker_UsesLocalPath(t *testing.T) {
	h := &TemplatesHandler{docker: nil}

	// Traversal → rejected by validateRelPath before any filesystem touch.
	if err := h.deleteViaEphemeral(context.Background(), "ws-vol", "../../etc/passwd"); err == nil {
		t.Errorf("expected traversal rejection")
	} else if strings.Contains(err.Error(), "docker not available") {
		t.Errorf("traversal returned 'docker not available' instead of a path error: %v", err)
	}

	// Safe path against the real /configs base: either succeeds (rm -f on a
	// missing file is nil) or fails for a filesystem reason — but must NOT be
	// the old "docker not available" short-circuit.
	err := h.deleteViaEphemeral(context.Background(), "ws-vol", "definitely-absent-file.tmp")
	if err != nil && strings.Contains(err.Error(), "docker not available") {
		t.Errorf("safe delete still returns 'docker not available' — FIX #2 regressed: %v", err)
	}
}
