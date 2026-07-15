# Temporary native-channel cutover inventory

`GET /admin/cutovers/native-channels/inventory` is a temporary,
`AdminAuth`-only endpoint used to prove that the legacy
`workspace_channels` table is empty before native channels are replaced by
channel plugins. It is a prerequisite surface, not the cutover itself.

## Two-step deployment and removal sequence

1. Merge and deploy this prerequisite endpoint from the current native-channel
   Core line. Keep the old routes and writers intact until the endpoint is
   present on every tenant and the control-plane binder can query it.
2. Freeze lifecycle and old writers, collect and accept the full fleet proof,
   then update and land molecule-core#4267. That retirement change removes this
   endpoint, its tests, this runbook, and the rest of the native subsystem
   together.

Adding the endpoint only to #4267 is invalid: the same deployment would remove
the subsystem before the prerequisite evidence could be collected. Do not keep
this endpoint as a general reporting API after the cutover.

## Response contract

A successful read returns only identifiers and numeric counts:

```json
{
  "contract_version": 1,
  "table_state": "present",
  "total_rows": 0,
  "orphan_rows": 0,
  "workspace_row_sum": 0,
  "workspaces": [
    {"workspace_id": "<uuid>", "row_count": 0}
  ]
}
```

The database reads use one read-only, repeatable-read transaction. Every active
workspace appears exactly once, including workspaces with zero rows.
`orphan_rows` includes rows attached to removed or missing workspaces, so the
following identity must always hold:

```text
workspace_row_sum + orphan_rows == total_rows
```

Any query, scan, iteration, duplicate-ID, missing-ID, overflow, accounting, or
transaction error returns a generic `500` and no partial inventory. A missing
`workspace_channels` table returns `409` with `table_state: "absent"`; it is
never normalized into a numeric zero during this pre-cutover phase.

The SQL never selects `channel_config`, provider credentials, allowed-user
lists, or other channel content. Do not replace this endpoint with
`GET /workspaces/:id/channels`: that route returns channel configuration and is
not an appropriate fleet-evidence surface.

## Control-plane binding contract

The control plane should proxy this endpoint with the same trusted per-tenant
admin token, `Origin`, `X-Molecule-Org-Id`, and `User-Agent: curl/8.4.0` headers
used by its workspace-roster proxy. The token stays inside the control plane;
fleet scripts must never receive or log it.

For each organization in every page of the CP admin roster, the preflight must:

1. Reject zero organizations, pagination errors, duplicate or missing org IDs,
   non-running organizations without an explicit disposition, and any
   `409`/`502`/`503` or empty response body.
2. Fetch the authenticated tenant `GET /workspaces` roster and this inventory.
3. Require exact set equality between the roster workspace IDs and the
   inventory workspace IDs. Missing, duplicate, or extra IDs abort the run.
   This equality check also catches an old `GET /workspaces` implementation
   silently skipping a row after a scan failure: the count query still includes
   that active workspace, so the two ID sets cannot match.
4. Require contract version `1`, table state `present`, the accounting identity
   above, `total_rows == 0`, `orphan_rows == 0`, and every workspace count `0`.
5. Repeat the roster/inventory snapshot while org and workspace lifecycle and
   old channel writers are frozen. Any mismatch restarts the preflight.

Nonzero counts are safe numeric migration evidence, but they block the cutover
until the affected configuration is exported to its plugin, the provider
credential is revoked or rotated as required, and the legacy rows are removed.
This endpoint alone does not prove writers are quiesced or credentials revoked.
