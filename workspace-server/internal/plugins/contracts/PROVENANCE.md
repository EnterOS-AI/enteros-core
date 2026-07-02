# Provenance: vendored plugin-manifest SSOT schema

`plugin-manifest.schema.json` in this directory is a byte-for-byte VENDORED
copy of the plugin-manifest SSOT contract. The SSOT is canonical — align this
copy to it, never the reverse.

| Field | Value |
| --- | --- |
| Source repo | `molecule-ai/molecule-ai-sdk` |
| Source path | `contracts/plugin-manifest/plugin-manifest.schema.json` |
| Source commit (last change to the schema) | `20d47232a9f7ee95a56f1014e3ac06cc5ccf6dcc` |
| molecule-ai-sdk `main` HEAD at vendoring | `8aedf2bc8f820d79de8534e07e0d482ac1c43f4e` |
| Vendored | 2026-07-02 (core#3383 PR-1, advisory install-time validation) |

Consumers:

- `workspace-server/internal/plugins/manifest_ssot.go` embeds this copy
  (`//go:embed`) and validates staged `plugin.yaml` manifests against it at
  install time (advisory phase of core#3383).
- `.gitea/scripts/check-plugin-manifest-ssot-sync.sh` (run by
  `.gitea/workflows/contract-ssot-sync.yml`) byte-compares this copy against
  the live SSOT and reds on drift.

Re-vendor (from the repo root):

```bash
curl -fsS -A "curl/8.4.0" \
  https://git.moleculesai.app/molecule-ai/molecule-ai-sdk/raw/branch/main/contracts/plugin-manifest/plugin-manifest.schema.json \
  -o workspace-server/internal/plugins/contracts/plugin-manifest.schema.json
```

The explicit `-A "curl/8.4.0"` User-Agent is REQUIRED: the Cloudflare edge in
front of git.moleculesai.app 403s default non-browser UAs (e.g. Python
urllib) — a 403 here is a UA problem, not a token/visibility problem. Update
the commit pins above after re-vendoring.
