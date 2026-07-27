package desktopcontrol

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

// commandRunner runs an external command and returns combined output.
// Injectable so the xdotool/scrot argument construction is unit-testable
// without the binaries or a real X display.
type commandRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

func execRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// ExecActuator drives the real X display: scrot for screenshots (a direct
// framebuffer grab — NOT through VNC, so the agent's eyes are lossless; VNC is
// for humans, design §9) and xdotool for input. It targets the sidecar's single
// fixed display via the DISPLAY env the process inherits.
type ExecActuator struct {
	run          commandRunner
	tmpDir       string
	sleep        func(time.Duration)
	moveSettleMs int
}

// NewExecActuator builds the production actuator.
func NewExecActuator() *ExecActuator {
	return &ExecActuator{
		run:          execRunner,
		tmpDir:       os.TempDir(),
		sleep:        time.Sleep,
		moveSettleMs: 40, // §9: let hover/focus handlers fire between move and click
	}
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

func (a *ExecActuator) Type(ctx context.Context, text string) error {
	if err := a.ensureFocus(ctx); err != nil {
		return err
	}
	// --delay spaces keystrokes (ms) so fast injection doesn't outrun an input
	// field's async JS handlers and drop characters (§ input hardening). NOTE:
	// a clipboard "paste" fallback for very long / non-ASCII (IME) text is a
	// follow-up — it needs xclip/xsel added to the sidecar image.
	if _, err := a.run(ctx, "xdotool", "type", "--clearmodifiers", "--delay", "12", "--", text); err != nil {
		return fmt.Errorf("xdotool type: %w", err)
	}
	return nil
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
