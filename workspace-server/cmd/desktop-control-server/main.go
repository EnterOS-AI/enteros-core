// Command desktop-control-server is the desktop sidecar's control server. It
// runs INSIDE the sidecar container (built into the desktop image, like the
// memory-plugin sidecar binary) and exposes the authenticated screenshot/input
// API that the platform-side computer-use gateway proxies to (design RFC §9).
//
// It is the "actuator" layer of the 3-layer split: dumb execution against the
// local X display, no coordinate math, no lock awareness.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/desktopcontrol"
)

func main() {
	// Per-sidecar inbound secret (§6.5): the control server authenticates every
	// inbound request independently — no "internal caller" exemption — so a
	// same-network peer cannot inject synthetic input into this desktop.
	token := os.Getenv("DESKTOP_CONTROL_TOKEN")
	if token == "" {
		log.Fatal("DESKTOP_CONTROL_TOKEN is required (per-sidecar inbound secret)")
	}

	geom := desktopcontrol.Geometry{
		Width:  envInt("DESKTOP_WIDTH", 1280),
		Height: envInt("DESKTOP_HEIGHT", 800),
	}
	addr := os.Getenv("DESKTOP_CONTROL_ADDR")
	if addr == "" {
		addr = ":6070"
	}

	act := desktopcontrol.NewExecActuator()
	// Boot-time geometry assert (§3): the fixed coordinate contract only holds if
	// the real X display IS the pinned resolution. Retry briefly (Xvfb may still
	// be settling), then log LOUDLY on a persistent mismatch — every coordinate
	// would otherwise be silently off (the "wrong pixel" bug). Non-fatal: the
	// control server's out-of-bounds rejection is the runtime backstop.
	verifyDisplayGeometry(act, geom)

	srv := desktopcontrol.NewServer(token, geom, act)
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("desktop-control-server listening on %s (fixed display %dx%d)", addr, geom.Width, geom.Height)
	log.Fatal(httpSrv.ListenAndServe())
}

// verifyDisplayGeometry checks the real display matches the pinned geometry,
// retrying briefly while Xvfb settles. It logs the outcome; it never blocks
// startup (the runtime out-of-bounds guard is the backstop).
func verifyDisplayGeometry(act *desktopcontrol.ExecActuator, want desktopcontrol.Geometry) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	var lastErr error
	for attempt := 0; attempt < 8; attempt++ {
		w, h, err := act.DisplayGeometry(ctx)
		if err != nil {
			lastErr = err
		} else if w == want.Width && h == want.Height {
			log.Printf("desktop-control-server: display geometry %dx%d matches the pinned contract", w, h)
			return
		} else {
			log.Printf("desktop-control-server: ERROR display geometry %dx%d does NOT match pinned %dx%d — agent coordinates will be WRONG; check Xvfb -screen args", w, h, want.Width, want.Height)
			return
		}
		time.Sleep(1 * time.Second)
	}
	log.Printf("desktop-control-server: WARNING could not verify display geometry after retries: %v (continuing; out-of-bounds guard remains active)", lastErr)
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}
