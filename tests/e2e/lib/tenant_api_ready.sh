#!/usr/bin/env bash
# tenant_api_ready.sh — gate on the tenant's REAL proxied API, not the shallow
# /health.
#
# The break this closes (controlplane#1012 / cp#576): the control plane publishes
# org_instances.status='running' once the prod/tunnel canary sees a sustained
# /health 2xx (internal/provisioner/canary.go probes ONLY /health). /health is
# allowlisted past the tenant guard, so it goes green while the tenant app's real
# routes (/workspaces, /plugins) are still coming up. Under concurrent load the
# app finishes booting a few seconds AFTER 'running' is published, so the first
# proxied API call transiently 502/503s. A single-shot assert the instant /health
# is up therefore flakes.
#
# This is NOT a retry-until-green mask: provisioning is inherently asynchronous,
# so "declared running" does not imply "app already serving". We poll the actual
# API contract to a STABLE consecutive-200 streak, bounded by a deadline. A
# genuinely half-wired tenant (app never serves the route) never reaches the
# streak and this fails loudly — the caller's strict shape assertions still run
# afterwards, so a "200-but-canvas-HTML" half-wire is still caught downstream.
#
# wait_tenant_api_ready <tenant_url> <path> <token> <org_id> [label]
#   tenant_url : https://<slug>.<domain>   (no trailing slash)
#   path       : proxied API path that requires the app to be serving, e.g. /workspaces
#   token      : tenant admin bearer
#   org_id     : X-Molecule-Org-Id value
#   label      : context string for log/error messages
#
# Tunables (env):
#   TENANT_API_READY_DEADLINE  total wall-clock seconds (default 180)
#   TENANT_API_READY_POLL      seconds between polls    (default 3)
#   TENANT_API_READY_STREAK    consecutive 200s required (default 2)
#   TENANT_API_READY_TIMEOUT   per-attempt curl --max-time (default 10)
#   TENANT_API_READY_CURL      curl binary override
wait_tenant_api_ready() {
  local turl="$1" path="$2" token="$3" org_id="$4" label="${5:-tenant-api}"
  local deadline_s="${TENANT_API_READY_DEADLINE:-180}"
  local poll_s="${TENANT_API_READY_POLL:-3}"
  local need_streak="${TENANT_API_READY_STREAK:-2}"
  local per_timeout="${TENANT_API_READY_TIMEOUT:-10}"
  local curl_bin="${TENANT_API_READY_CURL:-curl}"

  if [ -z "$turl" ] || [ -z "$path" ]; then
    echo "::error::wait_tenant_api_ready: tenant_url and path are required" >&2
    return 2
  fi

  local start now elapsed code last="000" streak=0
  start=$(date +%s)
  while true; do
    # `|| code=000`: a transport timeout / connection refused during app boot must
    # NOT abort the step under `bash -e` — it is a retryable not-ready signal.
    code=$("$curl_bin" -sS -o /dev/null -w '%{http_code}' --max-time "$per_timeout" \
      -H "Authorization: Bearer $token" \
      -H "X-Molecule-Org-Id: $org_id" \
      -H "Origin: $turl" \
      "$turl$path" 2>/dev/null) || code="000"
    last="$code"

    if [ "$code" = "200" ]; then
      streak=$((streak + 1))
      if [ "$streak" -ge "$need_streak" ]; then
        echo "[tenant-ready] $label serving (stable ${need_streak}x200 at $path)"
        return 0
      fi
    else
      # 502/503/000/404 = app still wiring up under load → reset streak, keep polling.
      if [ "$streak" -gt 0 ]; then
        echo "::notice::[tenant-ready] $label streak reset (HTTP ${code} at $path)"
      fi
      streak=0
    fi

    now=$(date +%s)
    elapsed=$((now - start))
    if [ "$elapsed" -ge "$deadline_s" ]; then
      echo "::error::[tenant-ready] $label ($path) never served a stable ${need_streak}x200 within ${deadline_s}s (last HTTP ${last}) — persistent half-wired tenant (controlplane#1012), not a transient boot window." >&2
      return 1
    fi
    sleep "$poll_s"
  done
}
