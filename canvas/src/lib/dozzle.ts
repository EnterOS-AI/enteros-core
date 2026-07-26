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
// accepts a `?filter=name%3D<container-name>` query param that pre-filters
// its container list, so this link opens Dozzle scoped to just that
// workspace's container instead of the full unfiltered log list.
export function workspaceLogsUrl(workspaceId: string): string {
  return `${DOZZLE_URL}/?filter=name%3Dws-${encodeURIComponent(workspaceId)}`;
}
