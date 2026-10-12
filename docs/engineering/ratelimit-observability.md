# Rate-limit observability runbook

The workspace-server applies a configurable token bucket (`RATE_LIMIT`, default
600 requests per minute) and exposes request counters at `/metrics`.

## Current bucket identity

The key is selected in this order:

1. `X-Molecule-Org-Id`, only when it exactly matches this process's configured
   `MOLECULE_ORG_ID`;
2. a SHA-256 hash of the bearer token;
3. the direct client IP.

The limiter runs before `TenantGuard`, so arbitrary caller-supplied org headers
are never trusted as bucket keys. This prevents a caller from rotating forged
org IDs to manufacture fresh buckets. Raw bearer tokens are not stored in the
bucket map or emitted as metric labels.

Every response includes `X-RateLimit-Limit`, `X-RateLimit-Remaining`, and
`X-RateLimit-Reset`. A throttled response is JSON with status 429 and also
includes `Retry-After`.

## Metrics

The server exports route-pattern labels rather than workspace IDs:

```text
molecule_http_requests_total{method,path,status}
molecule_http_request_duration_seconds_total{method,path,status}
```

Useful PromQL queries:

```promql
sum(rate(molecule_http_requests_total{status="429"}[5m]))
```

```promql
topk(10, sum by (path) (
  rate(molecule_http_requests_total{status="429"}[5m])
))
```

A sustained rate is evidence to inspect the affected route and client behavior;
it is not, by itself, a reason to raise the global limit. Confirm that the
client honors `Retry-After`, that it is not duplicating pollers, and that the
expected WebSocket path is healthy before changing configuration.

## Distinguish application and edge throttling

The read-only probe below sends GET bursts to public endpoints and records the
response shape:

```sh
./scripts/edge-429-probe.sh <tenant>.moleculesai.app \
  --burst 80 --out /tmp/edge-429.txt
```

- Workspace-server throttling has a JSON response and `X-RateLimit-*` headers.
- Cloudflare throttling normally has a `cf-ray` header and an edge-generated
  response body.
- A response with neither signature may come from a local proxy or VPN.

The probe does not use credentials or make mutating requests. Do not run it at
high volume against a tenant you do not control.

## Changing the limit

Treat `RATE_LIMIT` as deployment configuration managed through the current
secrets/configuration path and CI-on-merge rollout. Do not edit a running host
manually. Record the metric window and affected route in the pull request, then
verify the exact merged revision and a real tenant request path after rollout.
