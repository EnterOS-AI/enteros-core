#!/usr/bin/env bash
# Entrypoint for the per-workspace desktop sidecar (design RFC §3, §9).
#
# Brings up ONE fixed-resolution X display and, on it: a window manager,
# Chromium in kiosk mode with the persistent profile, x11vnc+noVNC for the
# human view path, and the control server (the agent's hands/eyes) on :6070.
set -euo pipefail

: "${DESKTOP_WIDTH:=1280}"
: "${DESKTOP_HEIGHT:=800}"
: "${DISPLAY:=:99}"
export DISPLAY

# ── Coordinate contract (§3): ONE resolution, pinned in the X screen, the
# control server (DESKTOP_WIDTH/HEIGHT), and capture. DPI 96, no HiDPI, no
# scaling anywhere — a mis-scaled click is impossible by construction.
Xvfb "$DISPLAY" -screen 0 "${DESKTOP_WIDTH}x${DESKTOP_HEIGHT}x24" -dpi 96 -nolisten tcp &
for _ in $(seq 1 50); do
	xdpyinfo -display "$DISPLAY" >/dev/null 2>&1 && break
	sleep 0.1
done

# Lightweight WM (needed so Chromium gets focus/placement).
openbox &

# ── Human view path (§8): x11vnc streams the framebuffer, bound to localhost so
# only the in-container websockify bridge reaches it; noVNC serves :6080, which
# the platform's same-origin proxy + token gate front. The agent does NOT use
# this — its screenshots are a direct framebuffer grab in the control server.
x11vnc -display "$DISPLAY" -forever -shared -localhost -nopw -rfbport 5900 -quiet &
websockify --web=/usr/share/novnc 6080 localhost:5900 &

# ── Chromium: kiosk pins the WINDOW to the full fixed screen (§3 — pinning the
# screen alone leaks a toolbar offset). Profile persists on the mounted volume
# (cookies/logins survive scale-to-zero). device-scale-factor=1 keeps DPR=1.
# Sandbox stays ON (the runtime provides the needed user namespaces); NEVER
# pass --no-sandbox (§6.2).
chromium \
	--user-data-dir=/home/desktop/profile \
	--kiosk \
	--force-device-scale-factor=1 \
	--window-position=0,0 \
	--window-size="${DESKTOP_WIDTH},${DESKTOP_HEIGHT}" \
	--no-first-run --no-default-browser-check \
	about:blank &

# ── Control server (agent hands/eyes) on :6070, authed per-sidecar. exec so it
# is PID 1's foreground and signals (graceful SIGTERM stop, §10) reach it.
exec desktop-control-server
