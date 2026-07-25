#!/usr/bin/env python3
"""Minimal stub runtime for the local Docker-provisioner lifecycle e2e.

This is NOT a real agent — it carries no LLM, no claude-code SDK, no plugin
host. Its only job is to satisfy the platform's runtime<->platform contract so
the `test_local_provision_lifecycle_e2e.sh` harness can prove the LOCAL Docker
provisioner can provision a workspace, bring it online, SURVIVE A RESTART
(reusing the config volume), and route an A2A `message/send` through the
platform proxy — all WITHOUT building/booting the 2.5GB real claude-code image.

Contract it replicates (discovered from workspace-server):

  * registration is done BY the runtime container on boot (NOT the provisioner).
    The provisioner only sets status=provisioning + pre-stores the host URL; the
    container must POST /registry/register itself, and the heartbeat loop is what
    transitions provisioning -> online (registry.go evaluateStatus, #1784).

  * env vars the real entrypoint reads, injected by buildContainerEnv():
        WORKSPACE_ID   - this workspace's UUID
        PLATFORM_URL   - canonical platform base URL (e.g. http://platform:8080)
    We read exactly those (with WORKSPACE_CONFIG_PATH for the config.yaml probe).

  * POST {PLATFORM_URL}/registry/register
        body: {"id", "url", "agent_card":{"name","skills":[]}}
        - url MUST be push-routable. The provisioner runs the platform inside
          Docker, so it rewrites a stored http://127.0.0.1:<port> URL to the
          container-DNS form http://ws-<id[:12]>:8000 before proxying
          (a2a_proxy.go resolveAgentURL). We register our OWN container-DNS URL
          (http://<hostname>:8000) so SSRF validation passes in SaaS mode AND the
          proxy can reach us; in self-hosted (non-saas) mode RFC-1918 is blocked,
          so we fall back to registering by the ws-<id> alias hostname which
          resolves on molecule-core-net.
        - first register returns {"auth_token": ...}; we keep it for heartbeats.

  * POST {PLATFORM_URL}/registry/heartbeat   (every ~10s)
        header: Authorization: Bearer <auth_token>
        body: {"workspace_id","error_rate","sample_error","active_tasks",
               "uptime_seconds","current_task"}
        This is what lifts the workspace provisioning -> online and keeps the
        Redis liveness TTL fresh (so the restart re-online assertion can pass).

  * listen on :8000 and answer the A2A JSON-RPC the proxy forwards:
        POST /  {"jsonrpc","id","method":"message/send","params":{...}}
        -> 200 {"jsonrpc":"2.0","id":<echoed>,
                "result":{"kind":"message","role":"agent",
                          "parts":[{"kind":"text","text":"STUB OK"}],
                          "messageId":<uuid>}}
    The result envelope matches what test_a2a_e2e.sh asserts on
    (result.parts[0].text, role=agent, kind=text). A health path (/health and
    GET /) returns 200 so any probe sees the container as alive.
"""

import json
import os
import sys
import threading
import time
import urllib.request
import urllib.error
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

try:
    import yaml  # PyYAML — same parser the real runtime seeds config.yaml with.
except Exception:  # pragma: no cover — import guard so a missing dep is diagnosable.
    yaml = None

PORT = 8000

WORKSPACE_ID = os.environ.get("WORKSPACE_ID", "").strip()
PLATFORM_URL = (os.environ.get("PLATFORM_URL") or os.environ.get("MOLECULE_URL") or "").rstrip("/")
HOSTNAME = os.environ.get("HOSTNAME", "").strip()  # docker sets this to the container id; ws-<id> alias also resolves

# URL we register with. Two hard constraints, discovered from workspace-server:
#
#   * validateAgentURL (registry.go) blocks RFC-1918 ranges in NON-saas mode
#     (this dev stack sets neither MOLECULE_DEPLOY_MODE=saas nor MOLECULE_ORG_ID
#     -> strict mode). The molecule-core-net bridge is 172.18.0.0/16, INSIDE the
#     blocked 172.16/12 — so registering our own ws-<id>:8000 DNS name (which
#     resolves to a 172.18.x bridge IP) would be REJECTED and we'd never get an
#     auth_token. "localhost" is explicitly allowed BY NAME (no DNS lookup).
#
#   * the proxy doesn't use the URL we register anyway: the provisioner
#     pre-stores http://127.0.0.1:<host-port>, the register upsert PRESERVES any
#     existing 127.0.0.1 URL (CASE WHEN url LIKE 'http://127.0.0.1%'), and when
#     the platform runs in Docker resolveAgentURL rewrites that to the container
#     -DNS form http://ws-<id[:12]>:8000 before forwarding. So our listen
#     address (0.0.0.0:8000, reachable as ws-<id>:8000 on the bridge) is what
#     the proxy actually hits — independent of the URL string we register.
#
# Net: register a host-reachable localhost URL. The provisioner injects the
# correct host-mapped port via MOLECULE_WORKSPACE_URL (#2851); prefer that and
# fall back to the legacy STUB_REGISTER_URL or the internal listen port.
_short = WORKSPACE_ID[:12] if len(WORKSPACE_ID) > 12 else WORKSPACE_ID
SELF_URL = (
    os.environ.get("STUB_REGISTER_URL")
    or os.environ.get("MOLECULE_WORKSPACE_URL")
    or f"http://localhost:{PORT}"
)

CONFIG_PATH = (os.environ.get("WORKSPACE_CONFIG_PATH") or "/configs").rstrip("/")
AUTH_TOKEN_FILE = f"{CONFIG_PATH}/.auth_token"

AUTH_TOKEN = None
_started = time.time()


def _log(msg):
    print(f"[stub-runtime {_short}] {msg}", flush=True)


def _plugin_name_from_source(src):
    """Derive the on-box plugin directory name from a declared source, mirroring
    workspace-server/internal/plugins.PluginNameFromSource: strip the scheme and
    any #ref, then take the LAST path segment (the repo, or a subpath leaf). E.g.
    `gitea://molecule-ai/molecule-ai-plugin-scheduler#v0.2.0` -> `molecule-ai-plugin-scheduler`.
    Returns "" for an unparseable entry (skipped)."""
    s = (src or "").strip()
    if "://" in s:
        s = s.split("://", 1)[1]
    s = s.split("#", 1)[0].rstrip("/")
    return s.split("/")[-1] if s else ""


def materialize_declared_plugins():
    """Boot-materialize the workspace's DECLARED plugins, mirroring the real
    runtime's boot materializer (molecule_runtime.plugin_sources.install_declared_plugins):
    for every source in MOLECULE_DECLARED_PLUGINS, create
    /configs/plugins/<name>/plugin.yaml so the plugin reads as boot-installed.

    WHY THE STUB NEEDS THIS: the platform's post-online plugin reconcile skips
    delivery+restart for a plugin already present on the box
    (plugins_reconcile.pluginPresentOnBox cats /configs/plugins/<name>/plugin.yaml).
    The REAL runtime pulls declared plugins at boot, so a new workspace is online
    WITH them and the reconcile is a no-op. This stub carried no materializer, so a
    default-on plugin (e.g. molecule-scheduler, now declared on every workspace)
    read as "not on box" -> the reconcile docker-cp'd it into the running container
    and AUTO-RESTARTED to load it — which races (and, with AutoRemove containers,
    tears down) the restart-survival + proxy assertions. Materializing here makes
    the stub faithful to production's present-at-first-boot contract. Placeholder
    content is sufficient: the stub runs no plugin host, and pluginPresentOnBox only
    checks that plugin.yaml exists and is non-empty. Idempotent (config volume
    persists across restart)."""
    raw = os.environ.get("MOLECULE_DECLARED_PLUGINS", "").strip()
    if not raw:
        return
    plugins_dir = os.path.join(CONFIG_PATH, "plugins")
    for src in raw.split(","):
        name = _plugin_name_from_source(src)
        if not name:
            continue
        try:
            pdir = os.path.join(plugins_dir, name)
            os.makedirs(pdir, exist_ok=True)
            manifest = os.path.join(pdir, "plugin.yaml")
            if not os.path.exists(manifest) or os.path.getsize(manifest) == 0:
                with open(manifest, "w") as f:
                    f.write(
                        f"name: {name}\n"
                        "kind: stub\n"
                        "# boot-materialized placeholder (e2e stub runtime) — see "
                        "materialize_declared_plugins()\n"
                    )
            _log(f"materialized declared plugin: {name}")
        except Exception as e:
            _log(f"materialize {name!r} failed (non-fatal): {e}")


def _config_schedule_entries():
    """Read the delivered /configs/config.yaml top-level `schedules:` block and
    return it as a list of grid entries in the shape core's schedules_proxy
    expects (volumeEntry: name/cron/timezone/prompt/enabled/source). Returns []
    when there is no config, no schedules node, or the config can't be parsed.

    WHY: on self-host the platform GRAFTS the platform-agent template's
    `schedules:` node onto the concierge's composed config.yaml
    (graftConciergeSchedules, workspace-server platform_agent.go). The REAL
    runtime's seed_schedules_from_workspace_config then materializes that block
    into the runtime schedule grid; core reads the grid via
    GET /workspaces/:id/schedules -> proxy -> GET /internal/schedules. This stub
    carries no runtime, so it mirrors that seed: parse the config schedules and
    serve them as the grid. Faithful to the runtime seeder's config path (the
    `cron` key is passed through as-is — it is already the grid's field name;
    schedules_proxy.volumeEntry maps `cron`, not core's `cron_expr`)."""
    cfg = os.path.join(CONFIG_PATH, "config.yaml")
    if yaml is None:
        _log("PyYAML unavailable — cannot parse config.yaml schedules")
        return []
    try:
        with open(cfg, "r") as f:
            doc = yaml.safe_load(f) or {}
    except FileNotFoundError:
        return []
    except Exception as e:
        _log(f"config.yaml parse failed (non-fatal): {e}")
        return []
    raw = doc.get("schedules") if isinstance(doc, dict) else None
    if not isinstance(raw, list):
        return []
    entries = []
    for s in raw:
        if not isinstance(s, dict):
            continue
        name = str(s.get("name", "")).strip()
        if not name:
            continue
        entries.append({
            "name": name,
            "cron": str(s.get("cron", "")),
            "timezone": str(s.get("timezone", "")),
            "prompt": str(s.get("prompt", "")),
            "enabled": bool(s.get("enabled", True)),
            # Config.yaml-seeded schedules are template-origin (schedules.go:102).
            "source": str(s.get("source", "") or "template"),
        })
    return entries


def materialize_schedules_from_config():
    """Boot-materialize the config.yaml `schedules:` block onto
    /configs/schedules/schedules.yaml — mirroring the real runtime's
    seed_schedules_from_workspace_config, which writes the grid file the
    ephemeral SaaS e2e reads via `docker exec cat /configs/schedules/schedules.yaml`
    (test_staging_full_saas.sh). The live grid served by GET /internal/schedules is
    read fresh from config.yaml on each request (below); this file write is the
    on-disk mirror the docker-exec assertion inspects. Best-effort + idempotent."""
    entries = _config_schedule_entries()
    if not entries:
        return
    try:
        sched_dir = os.path.join(CONFIG_PATH, "schedules")
        os.makedirs(sched_dir, exist_ok=True)
        out = os.path.join(sched_dir, "schedules.yaml")
        payload = {"schedules": entries}
        with open(out, "w") as f:
            if yaml is not None:
                yaml.safe_dump(payload, f, sort_keys=False, default_flow_style=False)
            else:
                json.dump(payload, f)
        _log(f"materialized {len(entries)} schedule(s) onto {out}: "
             f"{[e['name'] for e in entries]}")
    except Exception as e:
        _log(f"materialize schedules failed (non-fatal): {e}")


def read_volume_token():
    """The provisioner pre-writes the CURRENT workspace bearer to
    /configs/.auth_token before every container start (issueAndInjectToken,
    #1877), and ROTATES it on every (re)provision (RevokeAllForWorkspace +
    IssueToken). So the volume file — NOT the register-response token — is the
    authoritative, rotation-proof bearer. Reading it on each heartbeat means a
    provision-time token rotation never wedges our heartbeat at 401 (which is
    what kept the workspace stuck in 'provisioning' instead of flipping online).
    """
    try:
        with open(AUTH_TOKEN_FILE, "r") as f:
            tok = f.read().strip()
            return tok or None
    except Exception:
        return None


def _post_json(path, payload, token=None):
    url = f"{PLATFORM_URL}{path}"
    data = json.dumps(payload).encode()
    req = urllib.request.Request(url, data=data, method="POST")
    req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", f"Bearer {token}")
    with urllib.request.urlopen(req, timeout=15) as resp:
        body = resp.read().decode()
        return resp.status, body


def register():
    """POST /registry/register. Returns the issued auth_token (first register).

    C18 hijack guard: once the workspace has ANY live token on file (the
    provisioner mints+injects one into /configs/.auth_token before start), a
    register MUST carry that workspace's bearer or it 401s. So we send the
    volume token (if present). First-ever boot has no live token yet → bootstrap
    register (no bearer) is allowed and returns the freshly-issued auth_token.
    """
    global AUTH_TOKEN
    payload = {
        "id": WORKSPACE_ID,
        "url": SELF_URL,
        "delivery_mode": "push",
        "agent_card": {
            "name": WORKSPACE_ID,
            "description": "stub runtime (e2e lifecycle)",
            "skills": [],
        },
    }
    status, body = _post_json("/registry/register", payload, token=read_volume_token())
    _log(f"register -> {status} {body[:200]}")
    try:
        parsed = json.loads(body)
    except Exception:
        parsed = {}
    tok = parsed.get("auth_token")
    if tok:
        AUTH_TOKEN = tok
        _log("captured auth_token from register response")
    return status


def current_token():
    # Volume file is authoritative (rotation-proof); fall back to the token we
    # captured from the register response if the file isn't there yet.
    return read_volume_token() or AUTH_TOKEN


def heartbeat():
    payload = {
        "workspace_id": WORKSPACE_ID,
        "error_rate": 0.0,
        "sample_error": "",
        "active_tasks": 0,
        "uptime_seconds": int(time.time() - _started),
        "current_task": "",
    }
    status, body = _post_json("/registry/heartbeat", payload, token=current_token())
    return status, body


def register_with_retry():
    # The platform may still be wiring the row when we boot; retry a few times.
    # Register is best-effort for the e2e (heartbeat drives online); a sticky
    # 401 just means the workspace already has a live token and our volume token
    # is momentarily stale — the heartbeat path re-reads the volume each beat.
    for attempt in range(1, 11):
        try:
            status = register()
            if status == 200:
                return True
            _log(f"register attempt {attempt}: HTTP {status}, retrying")
        except urllib.error.HTTPError as e:
            _log(f"register attempt {attempt}: HTTPError {e.code} {e.read().decode()[:200]}")
        except Exception as e:
            _log(f"register attempt {attempt}: {e}")
        time.sleep(2)
    return False


def heartbeat_loop():
    # Fire the FIRST heartbeat immediately (no initial 5s wait) — the
    # provisioning->online transition is driven by the heartbeat handler
    # (registry.go evaluateStatus, #1784), so an eager first beat minimises the
    # provision->online latency the e2e polls on.
    while True:
        try:
            status, body = heartbeat()
            if status != 200:
                _log(f"heartbeat -> {status} {body[:160]}")
                # A 401 means our token was rotated (every provision rotates the
                # workspace token, issueAndInjectToken -> RevokeAllForWorkspace).
                # Re-register to mint a fresh one. This is what lets the SAME
                # container process survive a platform-side token rotation.
                if status == 401:
                    _log("heartbeat 401 — re-registering to refresh token")
                    register_with_retry()
        except urllib.error.HTTPError as e:
            if e.code == 401:
                _log("heartbeat 401 (HTTPError) — re-registering")
                register_with_retry()
            else:
                _log(f"heartbeat HTTPError {e.code}")
        except Exception as e:
            _log(f"heartbeat error: {e}")
        time.sleep(5)


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):  # silence default access logging
        pass

    def _send(self, code, obj):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        # Schedule grid: core's GET /workspaces/:id/schedules proxies to
        # <wsURL>/internal/schedules (schedules_proxy.forwardScheduleAPI) and
        # expects 200 {"schedules":[volumeEntry...]}. Serve the config.yaml-seeded
        # grid (read fresh so a config delivered slightly after boot is still
        # picked up). The proxy attaches the inbound-secret bearer; the stub does
        # no LLM work, so it does not re-validate it (same posture as its other
        # routes) — the e2e only exercises the grid contract, not auth.
        path = self.path.split("?", 1)[0].rstrip("/")
        if path == "/internal/schedules":
            self._send(200, {"schedules": _config_schedule_entries()})
            return
        # Health: any other GET returns 200 so probes see us as alive.
        self._send(200, {"status": "ok", "stub": True, "workspace_id": WORKSPACE_ID})

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0") or "0")
        raw = self.rfile.read(length) if length else b"{}"
        try:
            req = json.loads(raw or b"{}")
        except Exception:
            req = {}

        method = req.get("method", "")
        req_id = req.get("id", str(uuid.uuid4()))

        if method and method != "message/send":
            # Match the proxy's -32601 method-not-found contract for unknowns.
            self._send(200, {
                "jsonrpc": "2.0",
                "id": req_id,
                "error": {"code": -32601, "message": f"method not found: {method}"},
            })
            return

        # Canned A2A reply — exact envelope the canvas/proxy + test_a2a_e2e.sh
        # assert on: result.role=agent, result.parts[0].kind=text/text.
        self._send(200, {
            "jsonrpc": "2.0",
            "id": req_id,
            "result": {
                "kind": "message",
                "role": "agent",
                "parts": [{"kind": "text", "text": "STUB OK"}],
                "messageId": str(uuid.uuid4()),
            },
        })


def main():
    if not WORKSPACE_ID or not PLATFORM_URL:
        _log(f"FATAL: WORKSPACE_ID={WORKSPACE_ID!r} PLATFORM_URL={PLATFORM_URL!r} — both required")
        sys.exit(1)

    _log(f"booting: platform={PLATFORM_URL} self_url={SELF_URL} hostname={HOSTNAME}")

    # Boot-materialize declared plugins BEFORE the first heartbeat flips us online,
    # so the platform's transition-to-online plugin reconcile sees them already on
    # the box (present-at-first-boot) and records-only instead of docker-cp'ing +
    # auto-restarting them mid-lifecycle. Mirrors the real runtime's boot materializer.
    materialize_declared_plugins()

    # Boot-materialize the config.yaml schedules block onto
    # /configs/schedules/schedules.yaml (the on-disk grid mirror the docker-exec
    # assertion reads), mirroring the real runtime's config-schedule seeder. The
    # live GET /internal/schedules grid is served fresh from config.yaml.
    materialize_schedules_from_config()

    # Start the HTTP server FIRST so the platform can reach us the instant we
    # register (avoids a race where the proxy forwards before we're listening).
    server = ThreadingHTTPServer(("0.0.0.0", PORT), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    _log(f"listening on :{PORT}")

    # Try to register, but do NOT make heartbeating contingent on it. The
    # provisioning->online transition is driven by the HEARTBEAT handler
    # (registry.go evaluateStatus, #1784), and heartbeats authenticate with the
    # volume token (rotation-proof). If register transiently 401s (e.g. a token
    # rotation mid-boot), we must still heartbeat so the workspace can come
    # online — blocking the heartbeat loop on register success is exactly what
    # kept the workspace stuck in 'provisioning'. register_with_retry runs in a
    # background thread; the foreground heartbeat loop starts immediately.
    threading.Thread(target=register_with_retry, daemon=True).start()
    heartbeat_loop()


if __name__ == "__main__":
    main()
