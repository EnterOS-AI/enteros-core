package desktopcontrol_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// TestDesktopSidecar_E2E is the local-Docker display e2e (design §15.1 / checklist
// "Local-Docker display e2e" P2 gate). It runs the REAL desktop-sidecar image
// under the SAME hardening StartDesktop applies (--cap-drop ALL,
// no-new-privileges, the Chromium-tuned seccomp profile) and asserts the
// control-server contract end-to-end: healthz, bearer auth (401/200), a live PNG
// screenshot off the framebuffer, and input coordinate/scroll bounds.
//
// Guarded by DESKTOP_E2E_IMAGE (the image tag, e.g. "molecule-desktop:test") so
// it is a no-op wherever Docker or the image is absent — CI opts in by building
// the image and setting the var. This codifies the manual verification done on
// 2026-07-27 as a repeatable regression gate.
func TestDesktopSidecar_E2E(t *testing.T) {
	image := os.Getenv("DESKTOP_E2E_IMAGE")
	if image == "" {
		t.Skip("DESKTOP_E2E_IMAGE not set; skipping local-Docker desktop e2e")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH; skipping desktop e2e")
	}

	const token = "e2e-token"
	const hostPort = "16090"
	name := "desktop-e2e-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	seccomp, err := filepath.Abs(filepath.Join("..", "provisioner", "seccomp", "desktop-sidecar.json"))
	if err != nil {
		t.Fatalf("resolve seccomp profile: %v", err)
	}

	runArgs := []string{
		"run", "-d", "--name", name,
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--security-opt", "seccomp=" + seccomp,
		"--memory", "2g", "--memory-swap", "2g",
		"-e", "DESKTOP_CONTROL_TOKEN=" + token,
		"-p", hostPort + ":6070",
		image,
	}
	if out, err := exec.Command("docker", runArgs...).CombinedOutput(); err != nil {
		t.Fatalf("docker run failed: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })

	base := "http://localhost:" + hostPort
	// Per-request timeout so a stalled connection fails fast instead of hanging
	// the whole test. DisableKeepAlives gives every request a fresh connection:
	// otherwise, reading only part of the ~15KB screenshot body before the next
	// request leaves the transport trying to drain it for reuse, which stalls the
	// following POST (verified: the server itself answers /input in ~0.2s).
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{DisableKeepAlives: true},
	}

	// Wait for the control server (exec'd after Xvfb is up).
	deadline := time.Now().Add(45 * time.Second)
	for {
		resp, err := client.Get(base + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			out, _ := exec.Command("docker", "logs", name).CombinedOutput()
			t.Fatalf("control server never became healthy\n%s", out)
		}
		time.Sleep(500 * time.Millisecond)
	}

	do := func(method, path, tok string, body []byte) *http.Response {
		var r *http.Request
		if body != nil {
			r, _ = http.NewRequestWithContext(context.Background(), method, base+path, bytes.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
		} else {
			r, _ = http.NewRequestWithContext(context.Background(), method, base+path, nil)
		}
		if tok != "" {
			r.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := client.Do(r)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		return resp
	}

	// Auth: screenshot without a token is 401.
	if resp := do("GET", "/screenshot", "", nil); resp.StatusCode != 401 {
		resp.Body.Close()
		t.Fatalf("screenshot without token: got %d, want 401", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	// Screenshot with the token is a real PNG (drain the full body).
	resp := do("GET", "/screenshot", token, nil)
	png, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("screenshot with token: got %d, want 200", resp.StatusCode)
	}
	if len(png) < 8 || !bytes.Equal(png[:8], []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}) {
		t.Fatalf("screenshot is not a PNG (%d bytes): % x", len(png), png[:min(8, len(png))])
	}

	// Input: valid click 204; out-of-bounds click 400; over-range scroll 400.
	cases := []struct {
		body string
		want int
	}{
		{`{"type":"click","x":100,"y":100}`, http.StatusNoContent},
		{`{"type":"click","x":5000,"y":5000}`, http.StatusBadRequest},
		{`{"type":"scroll","amount":2000000000}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		resp := do("POST", "/input", token, []byte(tc.body))
		got := resp.StatusCode
		resp.Body.Close()
		if got != tc.want {
			t.Fatalf("input %s: got %d, want %d", tc.body, got, tc.want)
		}
	}
	fmt.Fprintln(os.Stderr, "desktop e2e: healthz, auth, live PNG screenshot, and input bounds all verified against the real image")
}
