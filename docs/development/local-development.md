# Local development

The supported entry point is:

```bash
git clone https://git.moleculesai.app/molecule-ai/molecule-core.git
cd molecule-core
./scripts/dev-start.sh
```

Open `http://localhost:3000`. The [quick-start guide](../quickstart.md) owns the
current prerequisites, manual startup path, first-run flow, and troubleshooting.

## Current boundaries

- Canonical SCM is `git.moleculesai.app`; do not use the suspended Molecule-AI
  GitHub organization as a template or package source.
- `scripts/dev-start.sh`, `.env.example`, the compose files, and
  `infra/scripts/` are the executable local setup contract.
- `manifest.json` contains immutable template/plugin source pins. Setup and
  template-cache helpers resolve those entries through the configured provider;
  documentation must not maintain a separate list of mirrored runtimes.
- Local development does not require a retired operator host, AWS ECR, Railway,
  or a production credential bundle.
- `ADMIN_TOKEN` authentication remains fail-closed locally. The supported setup
  path provisions the matching local configuration; see the [admin-auth
  runbook](../runbooks/admin-auth.md).
- `PARENT_ID` is a legacy compatibility environment field. Current checked-in
  runtimes discover hierarchy through platform rows and peer APIs.

## Manual component work

For focused changes, use the component's own dependency and test commands:

```bash
cd workspace-server
go test ./...

cd ../canvas
npm ci
npm test
npm run build
```

Some integration tests require sibling repositories or local services. Follow
the corresponding workflow and test comments instead of substituting a stale
global machine path.

## Before opening a PR

- run the focused tests for the changed behavior;
- run the component build and formatting/lint checks;
- run `git diff --check` and the repository secret scan;
- verify docs/comments against current source; and
- leave production deployment and promotion to the normal reviewed Gitea
  workflow.
