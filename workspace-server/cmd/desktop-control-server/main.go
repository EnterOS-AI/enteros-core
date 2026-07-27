// Command desktop-control-server is the desktop sidecar's control server. It
// runs INSIDE the sidecar container (built into the desktop image, like the
// memory-plugin sidecar binary) and exposes the authenticated screenshot/input
// API that the platform-side computer-use gateway proxies to (design RFC §9).
//
// It is the "actuator" layer of the 3-layer split: dumb execution against the
// local X display, no coordinate math, no lock awareness.
package main

import (
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

	srv := desktopcontrol.NewServer(token, geom, desktopcontrol.NewExecActuator())
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("desktop-control-server listening on %s (fixed display %dx%d)", addr, geom.Width, geom.Height)
	log.Fatal(httpSrv.ListenAndServe())
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}
