#!/usr/bin/env bash
# Entrypoint for the per-workspace desktop sidecar (design RFC §3, §9, §25).
#
# Brings up ONE fixed-resolution X display and, on it: a FULL XFCE desktop
# (panel, app menu, taskbar, file manager, terminal — §25.2, not a kiosk
# browser), x11vnc+noVNC for the human view path, and the control server (the
# agent's hands/eyes) on :6070. The desktop can self-install software (sudo/apt
# through the egress proxy, §25.3) and its whole rootfs persists across
# scale-to-zero because the provisioner stops-not-removes the container (§25.1).
set -euo pipefail

: "${DESKTOP_WIDTH:=1280}"
: "${DESKTOP_HEIGHT:=800}"
: "${DISPLAY:=:99}"
export DISPLAY
export HOME="${HOME:-/home/desktop}"

# ── Stale X-server state cleanup (§25.1, persistence). The whole rootfs — /tmp
# INCLUDED — now PERSISTS across scale-to-zero (the provisioner stops-not-removes
# the container). So the PREVIOUS boot's Xvfb lock (/tmp/.X<n>-lock) and socket
# (/tmp/.X11-unix/X<n>) survive into this boot. A FRESH container never has them;
# a RECYCLED one does, and Xvfb then refuses to start ("X server already running
# on display :99") — which kills startxfce4 (no display) and leaves the desktop
# DEAD after a single recycle. The process that held them is long gone (this
# container was just (re)started), so clearing them is safe and required. Same
# class of bug as the Chromium Singleton guards below (#4989), one layer down.
_dpynum="${DISPLAY#:}"; _dpynum="${_dpynum%%.*}"
rm -f "/tmp/.X${_dpynum}-lock" "/tmp/.X11-unix/X${_dpynum}" 2>/dev/null || true

# ── Coordinate contract (§3): ONE resolution, pinned in the X screen, the
# control server (DESKTOP_WIDTH/HEIGHT), and capture. DPI 96, no HiDPI, no
# scaling anywhere — a mis-scaled click is impossible by construction.
Xvfb "$DISPLAY" -screen 0 "${DESKTOP_WIDTH}x${DESKTOP_HEIGHT}x24" -dpi 96 -nolisten tcp &
for _ in $(seq 1 50); do
	xdpyinfo -display "$DISPLAY" >/dev/null 2>&1 && break
	sleep 0.1
done

# ── Human view path (§8): x11vnc streams the framebuffer, bound to localhost so
# only the in-container websockify bridge reaches it; noVNC serves :6080, which
# the platform's same-origin proxy + token gate front. --heartbeat sends a WS
# ping every 25s so an IDLE viewer (a static screen produces no framebuffer
# traffic) is not dropped by an upstream idle timeout (§25 / #4989). The agent
# does NOT use this — its screenshots are a direct framebuffer grab.
x11vnc -display "$DISPLAY" -forever -shared -localhost -nopw -rfbport 5900 -quiet &
websockify --heartbeat=25 --web=/usr/share/novnc 6080 localhost:5900 &

# ── Egress through the per-workspace proxy (§25.3 / §6.1). The sidecar sits on
# an INTERNAL network with NO direct route out — its ONLY path to the public
# internet is DESKTOP_PROXY. So apt (self-install) AND the browser MUST be
# pointed at it, or they have no network at all. The proxy denies private/
# link-local/loopback, so isolation is unchanged. Written with sudo (the desktop
# user has passwordless sudo, §25.3); best-effort so a dev run with no proxy
# still boots.
if [ -n "${DESKTOP_PROXY:-}" ]; then
	export http_proxy="${DESKTOP_PROXY}" https_proxy="${DESKTOP_PROXY}" no_proxy="localhost,127.0.0.1"
	printf 'Acquire::http::Proxy "%s";\nAcquire::https::Proxy "%s";\n' "${DESKTOP_PROXY}" "${DESKTOP_PROXY}" \
		| sudo tee /etc/apt/apt.conf.d/01proxy >/dev/null 2>&1 || true
	# Chromium reads its proxy from a managed policy, not http_proxy — pin the
	# same proxy (host:port form) so the browser's egress is proxied too.
	sudo mkdir -p /etc/chromium/policies/managed 2>/dev/null || true
	printf '{"ProxyMode":"fixed_servers","ProxyServer":"%s"}\n' "${DESKTOP_PROXY#http://}" \
		| sudo tee /etc/chromium/policies/managed/proxy.json >/dev/null 2>&1 || true
fi

# ── Stale Chromium singleton cleanup (§25 / #4989). The profile is PERSISTENT
# (survives scale-to-zero), so a recycled container can re-mount a profile still
# holding the previous container's SingletonLock; Chromium then REFUSES TO START
# ("profile in use by another Chromium on another computer"). Exactly one
# Chromium ever uses each profile, so clearing the guards on boot — for the
# default profile the menu launcher uses AND the legacy kiosk --user-data-dir —
# is safe and required for the browser to start after a recycle.
rm -f "$HOME"/.config/chromium/Singleton{Lock,Cookie,Socket} \
      /home/desktop/profile/Singleton{Lock,Cookie,Socket} 2>/dev/null || true

# XFCE + dbus need a runtime dir. It lives under the now-persistent /tmp, so a
# recycled container would inherit the previous boot's stale dbus/session sockets
# — recreate it FRESH each boot (§25.1) so dbus-launch starts a clean session bus.
export XDG_RUNTIME_DIR="/tmp/xdg-$(id -u)"
rm -rf "$XDG_RUNTIME_DIR" 2>/dev/null || true
mkdir -p "$XDG_RUNTIME_DIR" && chmod 700 "$XDG_RUNTIME_DIR" || true

# ── Full XFCE desktop session (§25.2). startxfce4 launches its own dbus, the
# window manager, panel, desktop, and settings daemons. Agent + human both see a
# real desktop; apps (browser, terminal, files, and anything apt-installed) are
# launched from it — no more single kiosk browser.
startxfce4 >/tmp/xfce.log 2>&1 &

# ── Control server (agent hands/eyes) on :6070, authed per-sidecar. exec so it
# is PID 1's foreground and signals (graceful SIGTERM stop, §10) reach it.
exec desktop-control-server
