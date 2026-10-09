package desktopcontrol

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type recordedCmd struct {
	name string
	args []string
}

func newRecordingActuator(t *testing.T) (*ExecActuator, *[]recordedCmd) {
	t.Helper()
	var got []recordedCmd
	dir := t.TempDir()
	a := &ExecActuator{
		tmpDir:       dir,
		sleep:        func(time.Duration) {},
		moveSettleMs: 0,
		run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			got = append(got, recordedCmd{name: name, args: args})
			// Make Screenshot's os.ReadFile succeed: scrot "writes" the PNG.
			if name == "scrot" && len(args) == 2 {
				_ = os.WriteFile(args[1], []byte("\x89PNG"), 0o600)
			}
			return nil, nil
		},
	}
	return a, &got
}

func joined(c recordedCmd) string { return c.name + " " + strings.Join(c.args, " ") }

func TestExecActuator_Click_MovesThenClicks(t *testing.T) {
	a, got := newRecordingActuator(t)
	a.moveSettleMs = 0 // don't actually sleep
	if err := a.Click(context.Background(), 100, 200, "right"); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 2 {
		t.Fatalf("want move then click (2 cmds), got %d: %+v", len(*got), *got)
	}
	if j := joined((*got)[0]); j != "xdotool mousemove --sync 100 200" {
		t.Fatalf("move cmd = %q", j)
	}
	// right button = 3
	if j := joined((*got)[1]); j != "xdotool click 3" {
		t.Fatalf("click cmd = %q", j)
	}
}

func TestExecActuator_Click_DefaultLeftButton(t *testing.T) {
	a, got := newRecordingActuator(t)
	if err := a.Click(context.Background(), 5, 5, ""); err != nil {
		t.Fatal(err)
	}
	if j := joined((*got)[1]); j != "xdotool click 1" {
		t.Fatalf("default click cmd = %q, want button 1 (left)", j)
	}
}

func TestExecActuator_TypeKeyScroll(t *testing.T) {
	a, got := newRecordingActuator(t)
	if err := a.Type(context.Background(), "hello world"); err != nil {
		t.Fatal(err)
	}
	// Focus is verified BEFORE typing (input hardening), then a delayed type.
	if j := joined((*got)[0]); j != "xdotool getactivewindow" {
		t.Fatalf("expected focus verify first, got %q", j)
	}
	if j := joined((*got)[1]); j != "xdotool type --clearmodifiers --delay 12 -- hello world" {
		t.Fatalf("type cmd = %q", j)
	}

	a, got = newRecordingActuator(t)
	if err := a.Key(context.Background(), "ctrl+v"); err != nil {
		t.Fatal(err)
	}
	if j := joined((*got)[0]); j != "xdotool getactivewindow" {
		t.Fatalf("expected focus verify first, got %q", j)
	}
	if j := joined((*got)[1]); j != "xdotool key --clearmodifiers ctrl+v" {
		t.Fatalf("key cmd = %q", j)
	}

	// scroll up 3 -> button 4 thrice.
	a, got = newRecordingActuator(t)
	if err := a.Scroll(context.Background(), -3); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 3 {
		t.Fatalf("scroll up 3 -> 3 clicks, got %d", len(*got))
	}
	for _, c := range *got {
		if joined(c) != "xdotool click 4" {
			t.Fatalf("scroll-up cmd = %q, want button 4", joined(c))
		}
	}
}

func TestExecActuator_TypePasteFallback(t *testing.T) {
	// Long text -> clipboard paste path (getactivewindow, xclip, ctrl+v).
	a, got := newRecordingActuator(t)
	long := strings.Repeat("a", 50) // > typePasteThreshold, ASCII
	if err := a.Type(context.Background(), long); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 3 {
		t.Fatalf("paste path wants 3 cmds (focus, xclip, paste), got %d: %+v", len(*got), *got)
	}
	if (*got)[0].name != "xdotool" || (*got)[0].args[0] != "getactivewindow" {
		t.Fatalf("first cmd = %q, want focus verify", joined((*got)[0]))
	}
	if (*got)[1].name != "xclip" || (*got)[1].args[0] != "-selection" || (*got)[1].args[1] != "clipboard" {
		t.Fatalf("second cmd = %q, want xclip set clipboard", joined((*got)[1]))
	}
	if j := joined((*got)[2]); j != "xdotool key --clearmodifiers ctrl+v" {
		t.Fatalf("third cmd = %q, want ctrl+v paste", j)
	}

	// Non-ASCII (even if short) also uses the paste path.
	a, got = newRecordingActuator(t)
	if err := a.Type(context.Background(), "café ☕"); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 3 || (*got)[1].name != "xclip" {
		t.Fatalf("non-ASCII text must use paste path: %+v", *got)
	}

	// Short ASCII stays on the direct type path (no clipboard side effect).
	a, got = newRecordingActuator(t)
	if err := a.Type(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 2 || (*got)[1].name != "xdotool" || (*got)[1].args[0] != "type" {
		t.Fatalf("short ASCII must use direct type path: %+v", *got)
	}
}

func TestExecActuator_TypeRefusesWhenNoFocus(t *testing.T) {
	dir := t.TempDir()
	a := &ExecActuator{
		tmpDir: dir,
		sleep:  func(time.Duration) {},
		run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			if name == "xdotool" && len(args) > 0 && args[0] == "getactivewindow" {
				return nil, errors.New("no active window")
			}
			t.Fatalf("keystroke sent despite no focus: %s %v", name, args)
			return nil, nil
		},
	}
	if err := a.Type(context.Background(), "secret"); err == nil {
		t.Fatal("Type must error when no window is focused (keystrokes would land in the void)")
	}
	if err := a.Key(context.Background(), "Return"); err == nil {
		t.Fatal("Key must error when no window is focused")
	}
}

func TestExecActuator_Navigate_PrimaryAliveHandsOffFast(t *testing.T) {
	a, got := newRecordingActuator(t)
	spawned := false
	a.spawn = func(string, ...string) error { spawned = true; return nil }
	if err := a.Navigate(context.Background(), "https://example.com"); err != nil {
		t.Fatal(err)
	}
	// A live primary: exactly the single hand-off invocation, no kiosk relaunch.
	if len(*got) != 1 {
		t.Fatalf("healthy hand-off wants 1 cmd, got %d: %+v", len(*got), *got)
	}
	if j := joined((*got)[0]); j != "chromium --user-data-dir=/home/desktop/profile https://example.com" {
		t.Fatalf("hand-off cmd = %q", j)
	}
	if spawned {
		t.Fatal("must NOT relaunch kiosk when the primary is alive")
	}
}

func TestExecActuator_Navigate_DeadPrimaryRelaunchesKiosk(t *testing.T) {
	// Geometry + proxy come from the same env the entrypoint honors.
	t.Setenv("DESKTOP_WIDTH", "1920")
	t.Setenv("DESKTOP_HEIGHT", "1080")
	t.Setenv("DESKTOP_PROXY", "http://egress:8080")

	var spawns []recordedCmd
	a := &ExecActuator{
		tmpDir: t.TempDir(),
		sleep:  func(time.Duration) {},
		// Simulate a dead primary: the hand-off spawned a new foreground instance
		// that blocked and got SIGKILLed at the timeout — surfaces as an error.
		run: func(_ context.Context, _ string, _ ...string) ([]byte, error) {
			return nil, context.DeadlineExceeded
		},
		spawn: func(name string, args ...string) error {
			spawns = append(spawns, recordedCmd{name: name, args: args})
			return nil
		},
	}
	if err := a.Navigate(context.Background(), "https://example.com"); err != nil {
		t.Fatalf("Navigate must self-heal, not error: %v", err)
	}
	if len(spawns) != 1 {
		t.Fatalf("dead primary must trigger exactly one detached relaunch, got %d", len(spawns))
	}
	want := "chromium --user-data-dir=/home/desktop/profile --kiosk " +
		"--force-device-scale-factor=1 --window-position=0,0 --window-size=1920,1080 " +
		"--no-first-run --no-default-browser-check --proxy-server=http://egress:8080 " +
		"https://example.com"
	if j := joined(spawns[0]); j != want {
		t.Fatalf("kiosk relaunch argv =\n  %q\nwant\n  %q", j, want)
	}
}

func TestExecActuator_Navigate_DeadPrimaryDefaultsGeometryNoProxy(t *testing.T) {
	// No env: geometry falls back to the 1280x800 contract and --proxy-server is
	// omitted (matching the entrypoint's empty PROXY_ARGS branch).
	t.Setenv("DESKTOP_WIDTH", "")
	t.Setenv("DESKTOP_HEIGHT", "")
	t.Setenv("DESKTOP_PROXY", "")

	var spawns []recordedCmd
	a := &ExecActuator{
		tmpDir: t.TempDir(),
		sleep:  func(time.Duration) {},
		run:    func(_ context.Context, _ string, _ ...string) ([]byte, error) { return nil, errors.New("dead") },
		spawn:  func(name string, args ...string) error { spawns = append(spawns, recordedCmd{name, args}); return nil },
	}
	if err := a.Navigate(context.Background(), "https://ex.com"); err != nil {
		t.Fatal(err)
	}
	want := "chromium --user-data-dir=/home/desktop/profile --kiosk " +
		"--force-device-scale-factor=1 --window-position=0,0 --window-size=1280,800 " +
		"--no-first-run --no-default-browser-check https://ex.com"
	if j := joined(spawns[0]); j != want {
		t.Fatalf("default kiosk relaunch argv =\n  %q\nwant\n  %q", j, want)
	}
}

func TestExecActuator_DisplayGeometry(t *testing.T) {
	a := &ExecActuator{
		run: func(_ context.Context, _ string, _ ...string) ([]byte, error) {
			return []byte("1280 800\n"), nil
		},
	}
	w, h, err := a.DisplayGeometry(context.Background())
	if err != nil || w != 1280 || h != 800 {
		t.Fatalf("DisplayGeometry = (%d,%d,%v), want (1280,800,nil)", w, h, err)
	}

	// Malformed output is an error (not a silent 0x0 that would pass a bad
	// geometry check).
	for _, bad := range []string{"", "1280", "1280 800 96", "wide tall"} {
		a.run = func(_ context.Context, _ string, _ ...string) ([]byte, error) { return []byte(bad), nil }
		if _, _, err := a.DisplayGeometry(context.Background()); err == nil {
			t.Errorf("DisplayGeometry(%q) = nil error, want failure", bad)
		}
	}
}

func TestExecActuator_Screenshot_ReadsFramebufferFile(t *testing.T) {
	a, got := newRecordingActuator(t)
	png, err := a.Screenshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if string(png) != "\x89PNG" {
		t.Fatalf("screenshot bytes = %q", png)
	}
	if (*got)[0].name != "scrot" || (*got)[0].args[0] != "-o" {
		t.Fatalf("expected scrot -o <path>, got %+v", (*got)[0])
	}
	if filepath.Dir((*got)[0].args[1]) != a.tmpDir {
		t.Fatalf("scrot path not in tmpDir: %s", (*got)[0].args[1])
	}
}
