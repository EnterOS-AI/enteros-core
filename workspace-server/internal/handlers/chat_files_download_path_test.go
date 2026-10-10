package handlers

import "testing"

// TestContainerDownloadPathError guards the 2026-07-24 Windows-host regression:
// the chat-attachment download validated a LINUX CONTAINER path with os-specific
// path/filepath, so on a native Windows-host platform (`go run`) every download
// 400'd (filepath.IsAbs("/workspace/x")==false; filepath.Clean rewrites "/"→"\").
// These checks are pure forward-slash logic, so this Linux-CI test proves the
// Windows behavior — the whole point of internal/wirepath.
func TestContainerDownloadPathError(t *testing.T) {
	cases := []struct {
		name string
		path string
		want string // "" means valid (forwardable)
	}{
		{
			"real chat-upload attachment (the QR that 404'd on Windows)",
			"/workspace/.molecule/chat-uploads/e7911edc301cfcace8faa206877c6c29-connect-qr.png",
			"",
		},
		{"bare /workspace root", "/workspace", ""},
		{"config file", "/configs/config.yaml", ""},
		{"home path", "/home/agent/report.pdf", ""},
		{"plugins path", "/plugins/foo/bar.txt", ""},
		{"agent-home path", "/agent-home/x", ""},
		{"relative path rejected", "workspace/x", "path must be absolute"},
		{"empty rejected", "", "path must be absolute"},
		{"non-allowed root rejected", "/etc/passwd", "path must be under /configs, /workspace, /home, or /plugins"},
		{"traversal rejected", "/workspace/../etc/passwd", "invalid path"},
		{"double slash non-canonical rejected", "/workspace//x", "invalid path"},
		{"trailing slash non-canonical rejected", "/workspace/x/", "invalid path"},
		// A backslash path is caught by the rooted check (`\x` != `/x`, so not
		// under `/workspace/`) — still rejected, just a different message. The
		// point is it never forwards.
		{"backslash separator rejected", `/workspace\x`, "path must be under /configs, /workspace, /home, or /plugins"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := containerDownloadPathError(tc.path); got != tc.want {
				t.Errorf("containerDownloadPathError(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}
