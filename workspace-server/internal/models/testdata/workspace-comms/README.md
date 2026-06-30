# Vendored workspace-comms SSOT schemas (synced copy)

These `*.schema.json` files are a **VENDORED, READ-ONLY MIRROR** of the
workspace-comms SSOT schemas that live in the public `molecule-contracts`
repo at `workspace-comms/`:

- https://git.moleculesai.app/molecule-ai/molecule-contracts/raw/branch/main/workspace-comms/register.schema.json
- https://git.moleculesai.app/molecule-ai/molecule-contracts/raw/branch/main/workspace-comms/heartbeat.schema.json
- https://git.moleculesai.app/molecule-ai/molecule-contracts/raw/branch/main/workspace-comms/agent-card.schema.json

## Why vendored (not fetched at test time)

`workspace_comms_ssot_test.go` loads these copies via `//go:embed` so the
gate is **offline / hermetic** — it runs in `go test ./...` on every CI
context (incl. fork PRs) with no network and no token. The schema↔struct
gate must red on a *real* struct/SSOT divergence, not flake on a network
blip.

## The two halves that keep this honest

This is the same transitional "vendored copy + sync-check" shape that core
already uses for the MCP-plugin-delivery mirror (`contracts/`):

1. **struct ↔ vendored-schema** — `workspace_comms_ssot_test.go` (this
   package). Asserts `models.RegisterPayload` / `HeartbeatPayload` /
   `RuntimeMetadata` (the WIRE AUTHORITY the schemas were derived from) stay
   field-compatible with these schemas. Reds if someone edits
   `workspace.go` and silently drifts the contract.
2. **vendored-schema ↔ molecule-contracts SSOT** — the
   `contract-ssot-sync` workflow (`.gitea/workflows/contract-ssot-sync.yml`
   + `.gitea/scripts/check-workspace-comms-ssot-sync.sh`). Canonical-JSON
   compares each file here against the molecule-contracts SSOT over the
   public raw endpoint. Reds if this vendored copy drifts from the SSOT.

Together: a change to `workspace.go` can't drift the SSOT without one of the
two reds firing.

## When the contract changes

Re-sync this directory from molecule-contracts (the SSOT is canonical —
align these copies to it, never the reverse):

```sh
DEST=workspace-server/internal/models/testdata/workspace-comms
for f in register heartbeat agent-card; do
  curl -fsS -A "curl/8.4.0" \
    "https://git.moleculesai.app/molecule-ai/molecule-contracts/raw/branch/main/workspace-comms/$f.schema.json" \
    -o "$DEST/$f.schema.json"
done
```

Then update `workspace.go` if the struct shape moved, and run
`go test ./internal/models/...`.
