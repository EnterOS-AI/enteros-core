#!/usr/bin/env bash
# workspace_env_presence.sh — shared PRESENCE-ONLY probe for the platform
# CP-proxy LLM usage token (MOLECULE_LLM_USAGE_TOKEN) on a workspace.
#
# Used by BOTH directions of the same underlying signal:
#   - test_staging_concierge_creates_workspace_e2e.sh — a platform member MUST
#     carry the token (present == ok).
#   - test_staging_full_saas.sh step 8c — a BYOK parent must NOT carry it
#     (present == the #1994 misroute: routed through the platform proxy, draining
#     the platform key).
#
# The token lands in a workspace's container env ONLY on the platform-managed
# route (workspace-server workspace_provision.go). We read its env-key PRESENCE,
# NEVER its value. The presence itself is exposed by GET /workspaces/:id once
# core's a06a52eb env-presence observability lands; until then this returns
# `unobservable` and callers treat that as advisory (tracked: core #4042).
#
# Reuses the host script's tenant_call(); source this AFTER tenant_call exists.
#
# CONTRACT-HARDENING (code-review #4032 findings 3-5): a06a52eb's field shape is
# not yet frozen, so this probe:
#   - trusts ONLY token-SPECIFIC boolean flags (llm_usage_token_present /
#     has_llm_usage_token) — NOT generic "has any LLM auth" flags, which are true
#     for a healthy BYOK workspace holding its OWN key and would false-fail 8c;
#   - accepts an env-key-NAME array as list-of-strings OR list-of-{key:...}
#     objects, and treats any other element shape as `unobservable` (never a
#     silent `absent` that would pass a real misroute);
#   - requires an ACTUAL boolean in an env_key_presence map (no truthy-coercion
#     of "false"/objects, which would false-fail).

# workspace_platform_llm_token_presence <workspace_id> — echoes present|absent|unobservable
workspace_platform_llm_token_presence() {
  local _body
  # Capture the body defensively: a host whose CURL_COMMON carries --fail-with-body
  # makes curl exit non-zero (22) on an HTTP error WHILE still writing the body, and
  # a bare `tenant_call | python3` would propagate that 22 under `set -o pipefail`
  # and ABORT the caller (opaque exit-22, no diagnostic). `|| true` neutralizes it;
  # a non-JSON / empty / error body then falls through to `unobservable` — never a
  # false present/absent. (code-review #4032 follow-up finding 1.)
  _body=$(tenant_call GET "/workspaces/$1" 2>/dev/null) || true
  printf '%s' "$_body" | python3 -c '
import sys, json
KEY = "MOLECULE_LLM_USAGE_TOKEN"
try:
    d = json.load(sys.stdin)
except Exception:
    print("unobservable"); sys.exit(0)
if not isinstance(d, dict):
    print("unobservable"); sys.exit(0)
# (a) token-SPECIFIC boolean presence flags ONLY.
for f in ("llm_usage_token_present", "has_llm_usage_token"):
    v = d.get(f)
    if isinstance(v, bool):
        print("present" if v else "absent"); sys.exit(0)
# (b) env-key NAME arrays — UNION across all synonym fields (a token enumerated in
#     ANY populated field == present); only a NON-EMPTY, fully-recognizable name
#     set can assert absence. An empty / heterogeneous / unknown-shape array ->
#     unobservable, never a false absent that would pass a real misroute.
lists = [d.get(f) for f in ("provisioned_env_keys", "container_env_keys", "env_keys", "llm_env_keys")]
lists = [v for v in lists if isinstance(v, list)]
if lists:
    names = set()
    unknown = False
    for v in lists:
        for e in v:
            if isinstance(e, str):
                names.add(e)
            elif isinstance(e, dict) and isinstance(e.get("key"), str):
                names.add(e["key"])
            else:
                unknown = True
    if KEY in names:
        print("present"); sys.exit(0)
    if unknown or not names:
        print("unobservable"); sys.exit(0)
    print("absent"); sys.exit(0)
# (c) env_key_presence map — require an actual boolean value.
ep = d.get("env_key_presence")
if isinstance(ep, dict) and KEY in ep:
    val = ep[KEY]
    if isinstance(val, bool):
        print("present" if val else "absent"); sys.exit(0)
    print("unobservable"); sys.exit(0)
print("unobservable")' || echo "unobservable"
}
