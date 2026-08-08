"use strict";
/**
 * Server-side half of the core#5106 instrument. Loaded into the E2E Chat canvas
 * server with `node --require`, so it observes the REAL standalone server:
 *
 *   NODE_OPTIONS="--require ../../e2e/support/server-probe.cjs" node server.js
 *
 * Why this exists. On the one captured recurrence (run 632484) the canvas
 * server's whole log was:
 *
 *     ▲ Next.js 15.5.20
 *     - Local: http://127.0.0.1:60685
 *     ✓ Starting...
 *     ✓ Ready in 85ms
 *
 * ...and nothing else, for the entire run. So "the server never crashed" was
 * the only thing the log could establish, while the two questions that decide
 * where the bug lives were both unanswerable:
 *
 *   1. Did the request for the dead stylesheet ever REACH this process?
 *      If the server never saw it, the failure is on the client/socket side.
 *      If it saw it and answered, the transfer was cut after the fact.
 *   2. Was this process able to run at all at that moment? A single-threaded
 *      Node server starved of CPU on a host running ~37 concurrent CI jobs
 *      cannot answer anything, and would leave exactly this log.
 *
 * The probe answers both and stays silent otherwise: it logs one line per
 * static asset request, and a line whenever the event loop was blocked. It adds
 * no middleware, no dependency, and does not touch the response path — it only
 * attaches listeners.
 *
 * Deliberately quiet on the happy path (a couple of hundred lines per run at
 * most, all `[probe]`-prefixed) so `Dump canvas log on failure` stays readable.
 */

const http = require("node:http");

const t0 = Date.now();
const stamp = () => `+${((Date.now() - t0) / 1000).toFixed(3)}s`;
const log = (msg) => {
  // stdout is redirected to canvas.log by the workflow.
  process.stdout.write(`[probe ${stamp()}] ${msg}\n`);
};

// ---------------------------------------------------------------------------
// Event-loop liveness. A 250ms interval that reports whenever it was late,
// which is the only direct evidence that this process was starved rather than
// merely idle. The threshold is deliberately low (250ms of lateness) because
// the failure being chased leaves a request unanswered for 17 SECONDS — if the
// loop is the cause, this will be screaming, and if it stays quiet through a
// failure the loop is exonerated.
// ---------------------------------------------------------------------------
const TICK_MS = 250;
let lastTick = Date.now();
const ticker = setInterval(() => {
  const now = Date.now();
  const lateBy = now - lastTick - TICK_MS;
  lastTick = now;
  if (lateBy > 250) log(`EVENT LOOP BLOCKED for ~${lateBy}ms — this process could not serve anything during that window`);
}, TICK_MS);
// Do not hold the process open on our account; the server does that.
if (typeof ticker.unref === "function") ticker.unref();

// ---------------------------------------------------------------------------
// Per-request record for static assets. `res.on("close")` fires for BOTH a
// completed response and a connection torn down mid-flight, and
// `res.writableFinished` is what tells them apart — which is precisely the
// distinction the browser-side Resource Timing evidence is being matched
// against (see canvas/e2e/helpers/canvas.ts).
// ---------------------------------------------------------------------------
const STATIC = /^\/_next\/static\//;
const originalCreateServer = http.createServer;

http.createServer = function patchedCreateServer(...args) {
  const server = originalCreateServer.apply(this, args);
  try {
    log(
      `http server created (keepAliveTimeout=${server.keepAliveTimeout}ms ` +
        `headersTimeout=${server.headersTimeout}ms requestTimeout=${server.requestTimeout}ms ` +
        `maxRequestsPerSocket=${server.maxRequestsPerSocket})`,
    );
    server.on("request", (req, res) => {
      if (!STATIC.test(req.url || "")) return;
      const started = Date.now();
      res.on("close", () => {
        const ms = Date.now() - started;
        // Quiet on the happy path: a fast, fully-written response is the
        // expected case and there are ~100 of them per run.
        if (res.writableFinished && ms < 1000) return;
        log(
          `${req.method} ${req.url} -> status=${res.statusCode} ` +
            `writableFinished=${res.writableFinished} bytesWritten=${req.socket ? req.socket.bytesWritten : "?"} ` +
            `after=${ms}ms` +
            (res.writableFinished
              ? "  <-- SLOW but complete"
              : "  <-- CONNECTION DIED BEFORE THE RESPONSE WAS FULLY WRITTEN"),
        );
      });
    });
    // A malformed/aborted request that never becomes a `request` event.
    server.on("clientError", (err, socket) => {
      log(`clientError ${err && err.code ? err.code : err} (bytesRead=${socket ? socket.bytesRead : "?"})`);
    });
    server.on("dropRequest", (req) => {
      log(`dropRequest ${req.method} ${req.url} — server refused it (maxRequestsPerSocket)`);
    });
  } catch (e) {
    // A probe must never be able to break the server it is watching.
    log(`probe attach failed: ${e && e.message ? e.message : e}`);
  }
  return server;
};

log("attached");
