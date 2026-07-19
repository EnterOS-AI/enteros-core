/**
 * Minimal A2A echo runtime for E2E tests.
 *
 * Listens on an ephemeral port, receives A2A JSON-RPC `message/send`
 * requests, and returns a response with the original text echoed back.
 * Also implements the workspace-side chat upload ingest endpoint so
 * file-attachment E2E can exercise the full upload → send → echo
 * round-trip.
 *
 * Usage (inside test fixture):
 *   const echo = await startEchoRuntime();
 *   // ... seed workspace with agent_url pointing to echo.baseURL ...
 *   echo.stop();
 */

import { createServer, type Server } from "node:http";

export interface EchoRuntime {
  baseURL: string;
  stop: () => Promise<void>;
  lastRequest: { method: string; text: string; files: unknown[] } | null;
}

/** One schedule as the runtime's volume grid contract names its fields. */
interface GridEntry {
  name: string;
  cron: string;
  timezone: string;
  prompt: string;
  enabled: boolean;
  source: string;
}

/**
 * Minimal 5-field cron validation, standing in for the runtime schedule_store's
 * validate_entry. It accepts the common numeric AND named forms a user can type
 * in the free-text ScheduleTab cron input (steps, ranges, lists, and named
 * day/month tokens like MON-FRI or JAN) and rejects free prose ("not a cron"), so
 * the invalid-cron alert path (validated host-side post-P4b, not in core) stays
 * covered without rejecting valid exprs — full cron semantics are the runtime's
 * concern, tested there.
 */
function isValidCron(expr: string): boolean {
  const fields = expr.trim().split(/\s+/);
  if (fields.length !== 5) return false;
  // A field is *, or a comma-list of terms, each: a value/name, an optional
  // range (a-b), and an optional /step. Values are digits or 3-letter names
  // (MON, JAN, …) so day-of-week/month names validate.
  const term = "(\\*|\\d+|[A-Za-z]{3})(-(\\d+|[A-Za-z]{3}))?(\\/\\d+)?";
  const field = new RegExp(`^(\\*(\\/\\d+)?|${term}(,${term})*)$`);
  return fields.every((f) => field.test(f));
}

/** Parse a minimal multipart body and extract the first file's name + content. */
function parseMultipart(body: Buffer): { name: string; mimeType: string; content: Buffer } | null {
  // Find the boundary line (first line starting with "--").
  const str = body.toString("binary");
  const firstDash = str.indexOf("--");
  if (firstDash === -1) return null;
  const eol = str.indexOf("\r\n", firstDash);
  if (eol === -1) return null;
  const boundary = str.slice(firstDash + 2, eol);
  const boundaryMarker = "\r\n--" + boundary;

  // Find the first part that has a filename in Content-Disposition.
  let pos = eol + 2;
  while (pos < str.length) {
    const nextBoundary = str.indexOf(boundaryMarker, pos);
    if (nextBoundary === -1) break;
    const part = str.slice(pos, nextBoundary);

    const cdMatch = part.match(/Content-Disposition:[^\r\n]*filename="([^"]+)"/i);
    if (cdMatch) {
      const name = cdMatch[1];
      const ctMatch = part.match(/Content-Type:\s*([^\r\n]+)/i);
      const mimeType = ctMatch ? ctMatch[1].trim() : "application/octet-stream";
      // Body starts after the first double-CRLF in the part.
      const bodyStart = part.indexOf("\r\n\r\n");
      if (bodyStart !== -1) {
        // Extract the raw bytes (not the string) so binary is safe.
        const headerBytes = Buffer.byteLength(part.slice(0, bodyStart + 4), "binary");
        const partStartInBody = Buffer.byteLength(str.slice(0, pos + bodyStart + 4), "binary");
        const partEndInBody = Buffer.byteLength(str.slice(0, nextBoundary), "binary");
        const content = body.subarray(partStartInBody, partEndInBody);
        return { name, mimeType, content };
      }
    }
    pos = nextBoundary + boundaryMarker.length;
    // Skip trailing "--" (end marker) or CRLF.
    if (str.slice(pos, pos + 2) === "--") break;
    if (str.slice(pos, pos + 2) === "\r\n") pos += 2;
  }
  return null;
}

export async function startEchoRuntime(): Promise<EchoRuntime> {
  let lastRequest: EchoRuntime["lastRequest"] = null;
  // In-process, name-keyed schedule grid (the volume backend the Canvas schedule
  // surface forwards to post-P4b, once the legacy core workspace_schedules store
  // was retired). Persists across requests for one echo-runtime instance.
  const schedules = new Map<string, GridEntry>();

  const server = createServer((req, res) => {
    // CORS: allow the canvas origin (localhost:3000) to call us.
    res.setHeader("Access-Control-Allow-Origin", "*");
    res.setHeader("Access-Control-Allow-Methods", "POST, GET, OPTIONS");
    res.setHeader("Access-Control-Allow-Headers", "Content-Type, Authorization");

    if (req.method === "OPTIONS") {
      res.writeHead(204);
      res.end();
      return;
    }

    const url = req.url ?? "/";

    // Workspace-side chat upload ingest (RFC #2312).
    if (url === "/internal/chat/uploads/ingest" && req.method === "POST") {
      const chunks: Buffer[] = [];
      req.on("data", (chunk: Buffer) => chunks.push(chunk));
      req.on("end", () => {
        const body = Buffer.concat(chunks);
        const file = parseMultipart(body);
        if (!file) {
          res.writeHead(400);
          res.end(JSON.stringify({ error: "no files field" }));
          return;
        }
        const sanitized = file.name.replace(/[^a-zA-Z0-9._\-]/g, "_").replace(/ /g, "_");
        const prefix = Array.from({ length: 32 }, () =>
          Math.floor(Math.random() * 16).toString(16),
        ).join("");
        const response = {
          files: [
            {
              uri: `workspace:/workspace/.molecule/chat-uploads/${prefix}-${sanitized}`,
              name: sanitized,
              mimeType: file.mimeType,
              size: file.content.length,
            },
          ],
        };
        res.setHeader("Content-Type", "application/json");
        res.writeHead(200);
        res.end(JSON.stringify(response));
      });
      return;
    }

    // ── Volume schedule API (post-P4b) ──────────────────────────────────────
    // The Canvas schedule surface forwards to the runtime's /internal/schedules*
    // grid and hot-arms the daemon via /internal/daemons/reload. Serve both as a
    // faithful in-process volume backend so schedule-tab.spec exercises the real
    // path (the legacy core-DB store was retired in P4b). Name-keyed; mirrors the
    // runtime schedule_store contract (validate cron, reject duplicate name).
    const path = url.split("?")[0];

    if (path === "/internal/daemons/reload" && req.method === "POST") {
      // A 2xx here signals the grid API is already serving — the create-race
      // readiness probe the platform's synchronous arm relies on.
      res.setHeader("Content-Type", "application/json");
      res.writeHead(200);
      res.end(JSON.stringify({ armed: schedules.size, added: ["molecule-scheduler"], trigger: true }));
      return;
    }
    if (path === "/internal/schedules" && req.method === "GET") {
      res.setHeader("Content-Type", "application/json");
      res.writeHead(200);
      res.end(JSON.stringify({ schedules: [...schedules.values()] }));
      return;
    }
    if (path === "/internal/schedules/health" && req.method === "GET") {
      res.setHeader("Content-Type", "application/json");
      res.writeHead(200);
      res.end(JSON.stringify({ last_tick: null, armed: schedules.size, errors: {} }));
      return;
    }
    if (path === "/internal/schedules" && req.method === "POST") {
      let raw = "";
      req.setEncoding("utf8");
      req.on("data", (chunk: string) => {
        raw += chunk;
      });
      req.on("end", () => {
        res.setHeader("Content-Type", "application/json");
        let e: Record<string, unknown>;
        try {
          e = JSON.parse(raw);
        } catch {
          res.writeHead(400);
          res.end(JSON.stringify({ error: "invalid json" }));
          return;
        }
        const cron = String(e.cron ?? "");
        if (!isValidCron(cron)) {
          res.writeHead(400);
          res.end(JSON.stringify({ error: `invalid cron expression: ${cron}` }));
          return;
        }
        const name = String(e.name ?? "");
        if (schedules.has(name)) {
          res.writeHead(400);
          res.end(JSON.stringify({ error: `schedule already exists: ${name}` }));
          return;
        }
        const entry: GridEntry = {
          name,
          cron,
          timezone: String(e.timezone || "UTC"),
          prompt: String(e.prompt ?? ""),
          enabled: e.enabled !== false,
          source: "runtime",
        };
        schedules.set(name, entry);
        res.writeHead(201);
        res.end(JSON.stringify(entry));
      });
      return;
    }
    const schedMatch = path.match(/^\/internal\/schedules\/([^/]+)(\/run|\/history)?$/);
    if (schedMatch) {
      const name = decodeURIComponent(schedMatch[1]);
      const suffix = schedMatch[2];
      res.setHeader("Content-Type", "application/json");
      if (suffix === "/history" && req.method === "GET") {
        res.writeHead(200);
        res.end(JSON.stringify({ history: [] }));
        return;
      }
      if (suffix === "/run" && req.method === "POST") {
        res.writeHead(202);
        res.end(JSON.stringify({ poked: name }));
        return;
      }
      if (!suffix && req.method === "DELETE") {
        schedules.delete(name);
        res.writeHead(200);
        res.end(JSON.stringify({ status: "deleted" }));
        return;
      }
      if (!suffix && req.method === "PATCH") {
        let raw = "";
        req.setEncoding("utf8");
        req.on("data", (chunk: string) => {
          raw += chunk;
        });
        req.on("end", () => {
          const cur = schedules.get(name);
          if (!cur) {
            res.writeHead(404);
            res.end(JSON.stringify({ error: `not found: ${name}` }));
            return;
          }
          let patch: Record<string, unknown>;
          try {
            patch = JSON.parse(raw);
          } catch {
            res.writeHead(400);
            res.end(JSON.stringify({ error: "invalid json" }));
            return;
          }
          if (patch.cron !== undefined) {
            if (!isValidCron(String(patch.cron))) {
              res.writeHead(400);
              res.end(JSON.stringify({ error: `invalid cron expression: ${String(patch.cron)}` }));
              return;
            }
            cur.cron = String(patch.cron);
          }
          if (patch.timezone !== undefined) cur.timezone = String(patch.timezone);
          if (patch.prompt !== undefined) cur.prompt = String(patch.prompt);
          if (patch.enabled !== undefined) cur.enabled = patch.enabled !== false;
          // Rename re-keys the grid (name is the primary key). Reject a collision
          // with an existing entry, mirroring the real store's name guard.
          if (patch.name !== undefined && String(patch.name) !== name) {
            const newName = String(patch.name);
            if (schedules.has(newName)) {
              res.writeHead(400);
              res.end(JSON.stringify({ error: `schedule already exists: ${newName}` }));
              return;
            }
            schedules.delete(name);
            cur.name = newName;
          }
          schedules.set(cur.name, cur);
          res.writeHead(200);
          res.end(JSON.stringify(cur));
        });
        return;
      }
    }

    // Default: A2A JSON-RPC handler.
    let body = "";
    req.setEncoding("utf8");
    req.on("data", (chunk: string) => {
      body += chunk;
    });
    req.on("end", () => {
      res.setHeader("Content-Type", "application/json");
      try {
        const rpc = JSON.parse(body);
        const msg = rpc.params?.message;
        const textParts =
          msg?.parts
            ?.filter((p: { kind?: string; text?: string }) => p.kind === "text")
            .map((p: { text?: string }) => p.text)
            .filter(Boolean) ?? [];
        const fileParts =
          msg?.parts?.filter((p: { kind?: string }) => p.kind === "file") ?? [];
        const text = textParts.join("\n");

        lastRequest = {
          method: rpc.method ?? "unknown",
          text,
          files: fileParts,
        };

        const replyText = text
          ? `Echo: ${text}`
          : fileParts.length > 0
            ? "Echo: received your file(s)."
            : "Echo: hello";

        const response = {
          jsonrpc: "2.0",
          id: rpc.id ?? null,
          result: {
            parts: [{ kind: "text", text: replyText }],
          },
        };

        res.writeHead(200);
        res.end(JSON.stringify(response));
      } catch {
        res.writeHead(400);
        res.end(JSON.stringify({ error: "invalid json" }));
      }
    });
  });

  await new Promise<void>((resolve) => server.listen(0, resolve));
  const address = server.address();
  const port = typeof address === "object" && address ? address.port : 0;
  const baseURL = `http://localhost:${port}`;

  return {
    baseURL,
    stop: () =>
      new Promise((resolve) => {
        server.close(() => resolve(undefined));
      }),
    get lastRequest() {
      return lastRequest;
    },
  };
}
