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

func (a *ExecActuator) Type(ctx context.Context, text string) error {
	if _, err := a.run(ctx, "xdotool", "type", "--clearmodifiers", "--", text); err != nil {
		return fmt.Errorf("xdotool type: %w", err)
	}
	return nil
}

func (a *ExecActuator) Key(ctx context.Context, keys string) error {
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
