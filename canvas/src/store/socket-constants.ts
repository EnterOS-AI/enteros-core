/** Cadence for the HTTP fallback rehydrate that runs while the WS is
 *  in connecting/disconnected limbo. Shared with delete tombstones so
 *  stale GET /workspaces responses cannot resurrect nodes removed within
 *  one fallback polling window.
 *
 *  Keep this in a leaf module: deleteTombstones is imported by the canvas
 *  store, and socket imports the canvas store. Importing this constant from
 *  socket recreates a production-bundle circular import. */
export const FALLBACK_POLL_MS = 10_000;
