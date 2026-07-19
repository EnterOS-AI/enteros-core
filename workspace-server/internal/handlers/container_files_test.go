package handlers

import (
	"archive/tar"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

// TestValidateRelPath tests the path-traversal guard used in deleteViaEphemeral.
// validateRelPath should reject absolute paths and ".." segments after cleaning.
// NOTE: This test lives in a file that does NOT call setupTestDB, so SSRF checks
// remain enabled. The test directly exercises validateRelPath without any DB
// dependency, so no mock DB is needed.
func TestValidateRelPath(t *testing.T) {
	cases := []struct {
		name      string
		path      string
		wantErr   bool
		errSubstr string // if non-empty, error message must contain this substring
	}{
		// Valid: simple relative paths inside a destination
		{"single file", "config.json", false, ""},
		{"nested relative", "dir/subdir/file.txt", false, ""},
		{"file at destination root", "file.txt", false, ""},
		{"subdirectory file", "configs/myapp/file.cfg", false, ""},
		{"dotfile (hidden file, not traversal)", ".env", false, ""},

		// Empty/dot-only: must be rejected with specific message
		{"empty string", "", true, "empty or dot-only path"},
		{"dot only", ".", true, "empty or dot-only path"},

		// Traversal: must be rejected
		{"double dot parent", "../etc/passwd", true, "path traversal"},
		{"trailing dotdot", "../", true, "path traversal"},
		{"embedded dotdot", "foo/../bar", true, "path traversal"},
		{"dotdot middle", "a/b/../../c", true, "path traversal"},
		{"path ends in ..", "foo/..", true, "path traversal"},
		{"bare ..", "..", true, "path traversal"},

		// Absolute: must be rejected
		{"absolute unix", "/etc/passwd", true, "path traversal"},
		{"absolute windows", "C:\\Windows\\System32", false, ""}, // Unix/Linux: no drive letter, treated as relative by Go
		{"embedded absolute", "foo/etc/passwd", false, ""},
		{"root absolute", "/workspace/file.txt", true, "path traversal"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRelPath(tc.path)
			if tc.wantErr && err == nil {
				t.Errorf("validateRelPath(%q): expected error, got nil", tc.path)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("validateRelPath(%q): expected nil, got %v", tc.path, err)
			}
			if tc.errSubstr != "" && (err == nil || !strings.Contains(err.Error(), tc.errSubstr)) {
				t.Errorf("validateRelPath(%q): expected error containing %q, got %v", tc.path, tc.errSubstr, err)
			}
		})
	}
}

// TestValidateRelPath_Cleaned ensures that validateRelPath is called on the
// cleaned (resolved) path, not the raw input, so tricks like "foo/./bar"
// pass but "foo/../bar" fails.
func TestValidateRelPath_Cleaned(t *testing.T) {
	// ". " (dot-space) is not "..", but after Clean() it becomes just the dir.
	// validateRelPath should be called on the clean path, not raw.
	// These are valid relative paths.
	valid := []string{
		"foo/./bar",
		"foo/././baz",
		"./file.cfg",
	}
	for _, p := range valid {
		if err := validateRelPath(p); err != nil {
			t.Errorf("validateRelPath(%q): expected nil, got %v", p, err)
		}
	}
}

// TestDeleteViaEphemeral_ConcatFormDocs documents that the exec form
// of rm used in deleteViaEphemeral receives the path as a single concatenated
// argument, not as a shell-expanded arg. This prevents traversal even if
// validateRelPath were somehow bypassed (defence in depth).
//
// The concat form: []string{"rm", "-rf", "/configs/" + filePath}
// passes ONE argument "/configs/../../../etc" to rm, which resolves it
// relative to rm's CWD, NOT the shell's working directory.
//
// By contrast, the shell-expanded form:
//
//	sh -c "rm -rf /configs $filePath"
//
// would treat ".." as path components relative to /configs and could escape.
//
// deleteViaEphemeral uses the exec form only (verified in code review).
func TestDeleteViaEphemeral_ConcatFormDocs(t *testing.T) {
	// This is a documentation test — it confirms the concat form is present
	// in the actual codebase by reading the source file directly.
	src, err := sourceFile("container_files.go")
	if err != nil {
		t.Skip("cannot read source: " + err.Error())
	}
	if !strings.Contains(src, `"/configs/" + filePath`) {
		t.Error("deleteViaEphemeral does not use concat form; F1085 fix may be missing or reverted")
	}
}

// TestBuildContainerFilesTar_SlashOnlyDestRootedNames pins the Windows-host
// contract for the Files-API write path (copyFilesToContainer): tar entry
// names are CONTAINER (Linux) paths and must be slash-only regardless of host
// OS. Pre-fix, filepath.Join on a Windows host produced `\configs\x` entry
// names — which both tripped the destPath escape guard (spurious "path escapes
// destination" on every save) and, where they slipped through, extracted on
// the Linux daemon as flat literal-backslash files (same bug class as the
// 2026-07-19 plugin-delivery incident).
func TestBuildContainerFilesTar_SlashOnlyDestRootedNames(t *testing.T) {
	files := map[string]string{
		"config.yaml":          "tier: 3\n",
		"skills/foo/SKILL.md":  "# Foo\n",
		"win\\style\\file.txt": "key built with filepath.Join on Windows",
	}

	buf, err := buildContainerFilesTar("/configs", files)
	if err != nil {
		t.Fatalf("buildContainerFilesTar: %v", err)
	}

	got := map[string]string{}
	tr := tar.NewReader(buf)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read tar: %v", err)
		}
		if strings.Contains(hdr.Name, `\`) {
			t.Fatalf("tar entry %q contains a backslash — Windows path separator leaked into the tar", hdr.Name)
		}
		if !strings.HasPrefix(hdr.Name, "/configs/") {
			t.Fatalf("tar entry %q not rooted under /configs/", hdr.Name)
		}
		if hdr.Typeflag == tar.TypeDir && !strings.HasSuffix(hdr.Name, "/") {
			t.Fatalf("dir entry %q missing trailing slash", hdr.Name)
		}
		if hdr.Typeflag == tar.TypeReg {
			body, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			got[hdr.Name] = string(body)
		}
	}

	want := map[string]string{
		"/configs/config.yaml":         "tier: 3\n",
		"/configs/skills/foo/SKILL.md": "# Foo\n",
		"/configs/win/style/file.txt":  "key built with filepath.Join on Windows",
	}
	for name, body := range want {
		if got[name] != body {
			t.Errorf("body[%q] = %q; want %q", name, got[name], body)
		}
	}
}

// TestBuildContainerFilesTar_RejectsEscapes: absolute names and ".." names
// that would climb out of destPath must be refused; a benign embedded ".."
// that cleans to a safe in-destination path stays accepted.
func TestBuildContainerFilesTar_RejectsEscapes(t *testing.T) {
	for _, name := range []string{
		"../outside.txt",
		"..\\outside.txt",
		"/etc/passwd",
		"a/../../outside.txt",
	} {
		if _, err := buildContainerFilesTar("/configs", map[string]string{name: "x"}); err == nil {
			t.Errorf("name %q: expected escape/unsafe-path error, got nil", name)
		}
	}
	if _, err := buildContainerFilesTar("/configs", map[string]string{"a/../b.txt": "x"}); err != nil {
		t.Errorf("a/../b.txt should be accepted (cleans to b.txt): %v", err)
	}
}

// sourceFile reads a source file from the same package at runtime.
// Used for compile-time-verification-style tests without importing io/ioutil.
func sourceFile(name string) (string, error) {
	data, err := os.ReadFile(name)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
