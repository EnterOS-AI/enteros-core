package desktopcontrol

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// commandRunner runs an external command and returns combined output.
// Injectable so the xdotool/scrot argument construction is unit-testable
// without the binaries or a real X display.
type commandRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

func execRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// detachedSpawner starts an external command in a new session and returns
// immediately WITHOUT waiting for it to exit. Injectable so the kiosk-relaunch
// argument construction is unit-testable without a real chromium or X display.
type detachedSpawner func(name string, args ...string) error

// spawnDetached launches name via setsid(1) so the child runs in a NEW session,
// detached from the control server's process group (a graceful SIGTERM to the
// server won't reap it, §10) and outliving the request goroutine. It returns as
// soon as the process is started — it never blocks on the browser staying up.
func spawnDetached(name string, args ...string) error {
	return exec.Command("setsid", append([]string{name}, args...)...).Start()
}

// ExecActuator drives the real X display: scrot for screenshots (a direct
// framebuffer grab — NOT through VNC, so the agent's eyes are lossless; VNC is
// for humans, design §9) and xdotool for input. It targets the sidecar's single
// fixed display via the DISPLAY env the process inherits.
type ExecActuator struct {
	run          commandRunner
	spawn        detachedSpawner
	tmpDir       string
	sleep        func(time.Duration)
	moveSettleMs int
}

// NewExecActuator builds the production actuator.
func NewExecActuator() *ExecActuator {
	return &ExecActuator{
		run:          execRunner,
		spawn:        spawnDetached,
		tmpDir:       os.TempDir(),
		sleep:        time.Sleep,
		moveSettleMs: 40, // §9: let hover/focus handlers fire between move and click
	}
}

// DisplayGeometry reports the actual pixel size of the X display via
// `xdotool getdisplaygeometry` (output "WIDTH HEIGHT"). Used by the boot-time
// geometry assert: if the real display is not the pinned resolution, every
// coordinate the agent sends is off — the exact "wrong pixel" failure the fixed
// coordinate contract (§3) exists to prevent — so a mismatch must surface.
func (a *ExecActuator) DisplayGeometry(ctx context.Context) (width, height int, err error) {
	out, err := a.run(ctx, "xdotool", "getdisplaygeometry")
	if err != nil {
		return 0, 0, fmt.Errorf("xdotool getdisplaygeometry: %w", err)
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("unexpected getdisplaygeometry output %q", string(out))
	}
	w, werr := strconv.Atoi(fields[0])
	h, herr := strconv.Atoi(fields[1])
	if werr != nil || herr != nil || w <= 0 || h <= 0 {
		return 0, 0, fmt.Errorf("non-numeric getdisplaygeometry output %q", string(out))
	}
	return w, h, nil
}

func (a *ExecActuator) Screenshot(ctx context.Context) ([]byte, error) {
	path := filepath.Join(a.tmpDir, "desktop-shot.png")
	if _, err := a.run(ctx, "scrot", "-o", path); err != nil {
		return nil, fmt.Errorf("scrot: %w", err)
	}
	return os.ReadFile(path)
}

func (a *ExecActuator) Click(ctx context.Context, x, y int, button string) error {
	btn := map[string]string{"left": "1", "middle": "2", "right": "3"}[button]
	if btn == "" {
		btn = "1"
	}
	// move -> settle -> click (§9): a fused move+click gives Chrome no time to
	// fire hover/focus handlers, so menus and hover-activated controls miss.
	if _, err := a.run(ctx, "xdotool", "mousemove", "--sync", strconv.Itoa(x), strconv.Itoa(y)); err != nil {
		return fmt.Errorf("xdotool mousemove: %w", err)
	}
	if a.moveSettleMs > 0 && a.sleep != nil {
		a.sleep(time.Duration(a.moveSettleMs) * time.Millisecond)
	}
	if _, err := a.run(ctx, "xdotool", "click", btn); err != nil {
		return fmt.Errorf("xdotool click: %w", err)
	}
	return nil
}

// ensureFocus verifies an input target actually has focus before keystroke
// injection (§ input hardening). Without it, xdotool type/key silently sends
// keystrokes into the void when no window is focused, and the agent believes it
// typed. In the kiosk sidecar there is always a focused window, so a failure
// here means the display is wedged — surface it rather than pretend success.
func (a *ExecActuator) ensureFocus(ctx context.Context) error {
	if _, err := a.run(ctx, "xdotool", "getactivewindow"); err != nil {
		return fmt.Errorf("no focused window for keystroke input: %w", err)
	}
	return nil
}

// typePasteThreshold is the byte length above which Type() pastes via the
// clipboard instead of injecting keystrokes. Key injection is O(n) xdotool work
// and drops characters on fields with async handlers; paste is one Ctrl+V.
const typePasteThreshold = 40

func (a *ExecActuator) Type(ctx context.Context, text string) error {
	if err := a.ensureFocus(ctx); err != nil {
		return err
	}
	// IME paste fallback (§ input hardening): key injection is slow and
	// IME-fragile for long or non-ASCII text (accented chars, CJK, emoji), which
	// xdotool type may drop or mistranslate. For those, stage the text on the
	// clipboard and Ctrl+V it — atomic and IME-safe. Short ASCII stays on the
	// direct type path (no clipboard side effects for the common case).
	if len(text) > typePasteThreshold || !isASCII(text) {
		return a.typeViaPaste(ctx, text)
	}
	// --delay spaces keystrokes (ms) so fast injection doesn't outrun an input
	// field's async JS handlers and drop characters.
	if _, err := a.run(ctx, "xdotool", "type", "--clearmodifiers", "--delay", "12", "--", text); err != nil {
		return fmt.Errorf("xdotool type: %w", err)
	}
	return nil
}

// typeViaPaste stages text on the X clipboard (via xclip, reading a short-lived
// temp file so no stdin plumbing is needed) then pastes it with Ctrl+V. The temp
// file lives only inside the isolated sidecar and is removed immediately, matching
// the screenshot temp-file pattern.
func (a *ExecActuator) typeViaPaste(ctx context.Context, text string) error {
	f := filepath.Join(a.tmpDir, "desktop-paste.txt")
	if err := os.WriteFile(f, []byte(text), 0o600); err != nil {
		return fmt.Errorf("stage paste text: %w", err)
	}
	defer func() { _ = os.Remove(f) }()
	if _, err := a.run(ctx, "xclip", "-selection", "clipboard", "-i", f); err != nil {
		return fmt.Errorf("xclip set clipboard: %w", err)
	}
	if _, err := a.run(ctx, "xdotool", "key", "--clearmodifiers", "ctrl+v"); err != nil {
		return fmt.Errorf("xdotool paste: %w", err)
	}
	return nil
}

// isASCII reports whether every rune in s is a 7-bit ASCII character.
func isASCII(s string) bool {
	for _, r := range s {
		if r > 127 {
			return false
		}
	}
	return true
}

func (a *ExecActuator) Key(ctx context.Context, keys string) error {
	if err := a.ensureFocus(ctx); err != nil {
		return err
	}
	if _, err := a.run(ctx, "xdotool", "key", "--clearmodifiers", keys); err != nil {
		return fmt.Errorf("xdotool key: %w", err)
	}
	return nil
}

// desktopProfileDir is the Chromium user-data-dir the entrypoint launches the
// kiosk instance with. Navigate MUST reuse it so a second chromium invocation is
// recognized as the same single instance and hands off the URL (navigating the
// pinned window) instead of spawning a competing instance.
const desktopProfileDir = "/home/desktop/profile"

// Navigate points the kiosk browser at url. Chromium runs single-instance per
// user-data-dir: a second `chromium <url>` invocation with the SAME profile dir
// forwards the URL to the running instance, which navigates the pinned kiosk
// window, then this invocation exits (verified 2026-07-27). --kiosk hides the
// omnibox so keyboard navigation is unavailable; this is the open_url path.
func (a *ExecActuator) Navigate(ctx context.Context, url string) error {
	// Bound the hand-off: a healthy running kiosk instance accepts the URL and
	// this invocation exits in well under a second. If the primary instance has
	// died (crash/OOM) while the sidecar stays up, `chromium <url>` does NOT
	// forward — it becomes a NEW foreground (non-kiosk) instance that blocks until
	// the browser exits. The timeout SIGKILLs that blocked spawn so a dead primary
	// fails fast instead of hanging the request goroutine.
	nctx, cancel := context.WithTimeout(ctx, desktopNavigateTimeout)
	defer cancel()
	if _, err := a.run(nctx, "chromium", "--user-data-dir="+desktopProfileDir, url); err == nil {
		// Primary alive: the hand-off forwarded the URL to the pinned kiosk window
		// and exited. This is the common, fast path.
		return nil
	}
	// The hand-off did not succeed. If the CALLER's context (not merely our bounded
	// hand-off timeout) is already done, the request was cancelled/deadlined
	// upstream — report that honestly instead of spawning a detached browser and
	// returning nil, which would tell the agent open_url SUCCEEDED for a navigation
	// nobody awaited (verified 2026-07-28: the self-heal branch used to swallow
	// this into a false SUCCESS).
	if ctx.Err() != nil {
		return fmt.Errorf("navigate hand-off aborted before completing: %w", ctx.Err())
	}
	// Otherwise the kiosk primary is gone/unresponsive (a dead primary makes the
	// bounded hand-off block until SIGKILL). Self-heal by relaunching the kiosk
	// instance ourselves — same flags the entrypoint uses, with the target URL as
	// the start page so the relaunch IS the navigation — detached so it outlives
	// this request and restores the pinned kiosk window. The agent's next
	// screenshot verifies the result. Without this, open_url would stay permanently
	// broken for the desktop after a browser crash.
	if err := a.spawn("chromium", kioskRelaunchArgs(url)...); err != nil {
		return fmt.Errorf("chromium kiosk relaunch after failed hand-off: %w", err)
	}
	return nil
}

// kioskRelaunchArgs builds the chromium argv for restoring a dead kiosk primary,
// mirroring the sidecar entrypoint: the same profile dir (so it is the single
// instance), --kiosk to pin the window to the full fixed screen, DPR=1, geometry
// from DESKTOP_WIDTH/HEIGHT (default 1280x800), and the egress --proxy-server
// from DESKTOP_PROXY when set. The target URL is passed last as the start page.
func kioskRelaunchArgs(url string) []string {
	w, h := desktopGeometry()
	args := []string{
		"--user-data-dir=" + desktopProfileDir,
		"--kiosk",
		"--force-device-scale-factor=1",
		"--window-position=0,0",
		fmt.Sprintf("--window-size=%d,%d", w, h),
		"--no-first-run",
		"--no-default-browser-check",
	}
	if proxy := os.Getenv("DESKTOP_PROXY"); proxy != "" {
		args = append(args, "--proxy-server="+proxy)
	}
	return append(args, url)
}

// desktopGeometry reads the pinned resolution from the same env the entrypoint
// honors, defaulting to the 1280x800 coordinate contract when unset/invalid.
func desktopGeometry() (width, height int) {
	return envInt("DESKTOP_WIDTH", 1280), envInt("DESKTOP_HEIGHT", 800)
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// desktopNavigateTimeout caps a single navigate hand-off. Far longer than a
// healthy hand-off needs, short enough that a dead-primary spawn can't hang.
const desktopNavigateTimeout = 10 * time.Second

func (a *ExecActuator) Scroll(ctx context.Context, amount int) error {
	// xdotool: button 4 = scroll up, 5 = scroll down; one click per notch.
	btn := "5"
	n := amount
	if n < 0 {
		btn = "4"
		n = -n
	}
	for i := 0; i < n; i++ {
		if _, err := a.run(ctx, "xdotool", "click", btn); err != nil {
			return fmt.Errorf("xdotool scroll: %w", err)
		}
	}
	return nil
}

var _ Actuator = (*ExecActuator)(nil)
