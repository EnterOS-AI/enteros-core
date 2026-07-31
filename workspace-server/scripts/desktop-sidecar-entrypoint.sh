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
# --heartbeat: send a WebSocket ping every 25s so an IDLE viewer (a static screen
# produces no framebuffer traffic) is not dropped by an upstream idle timeout
# (the tunnel/CP proxy closed silent sockets at ~34s — "take over, idle, lose the
# connection"). 25s < that window with margin; noVNC answers pings transparently.
websockify --heartbeat=25 --web=/usr/share/novnc 6080 localhost:5900 &

# ── Chromium: kiosk pins the WINDOW to the full fixed screen (§3 — pinning the
# screen alone leaks a toolbar offset). Profile persists on the mounted volume
# (cookies/logins survive scale-to-zero). device-scale-factor=1 keeps DPR=1.
# Sandbox stays ON (the runtime provides the needed user namespaces); NEVER
# pass --no-sandbox (§6.2).
#
# DESKTOP_PROXY (set by the provisioner) routes ALL browser egress through the
# per-workspace egress proxy — the sidecar's only route off its internal network
# (§6.1 structural isolation). --proxy-server applies it; the proxy denies
# private/link-local dsts, so the browser cannot reach infra/host/metadata. When
# unset (dev/no-network), Chromium goes direct.
PROXY_ARGS=""
if [ -n "${DESKTOP_PROXY:-}" ]; then
	PROXY_ARGS="--proxy-server=${DESKTOP_PROXY}"
fi
# The profile volume is PERSISTENT (cookies/logins survive scale-to-zero). When a
# reaped sidecar is recreated, the profile can still carry the previous
# container's Chromium singleton guards (SingletonLock/Cookie/Socket) — it was
# SIGKILLed on teardown, never cleaning them. The new Chromium then sees the lock
# owned by "another Chromium process on another computer" (the old container's
# hostname) and REFUSES TO START — the browser never comes up (black screen, no
# egress; reads to a human as "no internet"). Exactly ONE Chromium ever uses this
# profile, so clearing the stale guards on boot is safe and required for the
# browser to survive a scale-to-zero/scale-up cycle.
rm -f /home/desktop/profile/SingletonLock \
      /home/desktop/profile/SingletonCookie \
      /home/desktop/profile/SingletonSocket 2>/dev/null || true
chromium \
	--user-data-dir=/home/desktop/profile \
	--kiosk \
	--force-device-scale-factor=1 \
	--window-position=0,0 \
	--window-size="${DESKTOP_WIDTH},${DESKTOP_HEIGHT}" \
	--no-first-run --no-default-browser-check \
	${PROXY_ARGS} \
	about:blank &

# ── Control server (agent hands/eyes) on :6070, authed per-sidecar. exec so it
# is PID 1's foreground and signals (graceful SIGTERM stop, §10) reach it.
exec desktop-control-server
