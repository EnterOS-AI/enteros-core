# Read a Workspace Runtime Transcript

`GET /workspaces/:id/transcript` is a bounded proxy to the selected
workspace's own `GET /transcript` endpoint. It does not build or normalize a
transcript in Core.

## Current support

- Claude Code exposes raw events from the most recently modified session
  JSONL file and returns `supported: true`.
- Runtimes that do not implement transcript access return
  `supported: false` with an empty `lines` array.
- `lines` are runtime-owned objects. Do not assume every entry has a
  `type`/`content` pair.
- `more` only says that more file entries remain after the requested page. It
  is not a session-running or session-complete signal.

The runtime package defines the shared response envelope:

```json
{
  "runtime": "claude-code",
  "supported": true,
  "lines": [],
  "cursor": 0,
  "more": false,
  "source": "/home/agent/.claude/projects/-workspace/session.jsonl"
}
```

`source` is runtime diagnostic metadata and its path is container-local.

## Authentication

Use the live bearer token for the exact workspace:

```bash
curl -fsS \
  "https://<tenant>.moleculesai.app/workspaces/${WORKSPACE_ID}/transcript?since=0&limit=100" \
  -H "Authorization: Bearer ${WORKSPACE_TOKEN}" | jq .
```

Core validates the bearer against `:id` and forwards it to the runtime, which
independently validates the same workspace token. A control-plane session,
org token, or `ADMIN_TOKEN` can pass Core's general workspace middleware but
is not converted into a workspace token for this proxy; the runtime will
reject it. This endpoint is therefore currently a workspace-token surface,
not a Canvas operator dashboard API.

## Pagination

Pass the returned `cursor` as the next `since` value. `limit` defaults to 100;
the runtime clamps it to its own maximum.

```bash
cursor=0
while :; do
  page=$(curl -fsS \
    "https://<tenant>.moleculesai.app/workspaces/${WORKSPACE_ID}/transcript?since=${cursor}&limit=100" \
    -H "Authorization: Bearer ${WORKSPACE_TOKEN}")

  jq -c '.lines[]?' <<<"$page"
  cursor=$(jq -r '.cursor' <<<"$page")

  # This drains the entries available now. Poll again later if you want to
  # tail a still-running session; `more=false` does not mean the session ended.
  [[ $(jq -r '.more' <<<"$page") == true ]] || break
done
```

Core forwards only `since` and `limit`, caps the upstream response at 1 MiB,
does not follow redirects, and applies the same SSRF policy at URL validation
and socket-dial time. Upstream status codes and JSON are relayed to the caller.

Implementation references:

- `workspace-server/internal/handlers/transcript.go` — authenticated,
  size-bounded Core proxy
- `molecule-ai-workspace-runtime/molecule_runtime/main.py` — runtime route and
  workspace-token check
- `molecule-ai-workspace-runtime/molecule_runtime/adapter_base.py` — default
  `supported: false` contract
- `molecule-ai-workspace-template-claude-code/adapter.py` — Claude Code JSONL
  reader
