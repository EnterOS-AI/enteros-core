// Dozzle (https://dozzle.dev) is the dev-only container log viewer that
// runs alongside Enter OS. This module is the single place that knows its
// base URL and how to deep-link into a specific workspace's container logs,
// so the canvas UI never hardcodes 127.0.0.1:3110 in more than one spot.
//
// Base URL is configurable via NEXT_PUBLIC_DOZZLE_URL (e.g. when Dozzle is
// exposed on a different host/port, or behind a proxy in a shared/staging
// environment) and defaults to the local dev instance.
export const DOZZLE_URL =
  process.env.NEXT_PUBLIC_DOZZLE_URL ?? "http://127.0.0.1:3110";

// Workspace containers are named `ws-<workspaceId>` by the provisioner (see
// workspace-server internal/provisioner/provisioner.go ContainerName). Dozzle
// exposes a real name→container deep-link at `/show?name=<container-name>`:
// it resolves the name to the container's id and forwards to
// `/container/<id>`, opening scoped straight to that workspace's logs.
// (Dozzle's web UI has NO client-side `?filter=` query param — that only
// exists as the server-side DOZZLE_FILTER env/CLI setting — so an earlier
// `/?filter=name%3D…` link silently landed on the full unfiltered list.
// See https://dozzle.dev/guide/faq — "Can I deep link to a container?")
export function workspaceLogsUrl(workspaceId: string): string {
  return `${DOZZLE_URL}/show?name=ws-${encodeURIComponent(workspaceId)}`;
}
