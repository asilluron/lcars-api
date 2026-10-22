import { createServer as createHTTPServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import type { Readable } from "node:stream";

export interface CameraInfo {
  sn: string;
  name: string;
  camera: boolean;
}

// Implemented by the Eufy adapter and by tests. Tests never import the SDK.
export interface CameraSource {
  loggedIn(): boolean;
  list(): Promise<readonly CameraInfo[]>;
  openStream(sn: string): Promise<Readable | undefined>;
  snapshot(sn: string): Promise<Buffer | undefined>;
}

const serialPattern = /^[A-Za-z0-9]{1,32}$/;

export function createServer(source: CameraSource): Server {
  const viewers = new Map<string, number>();
  return createHTTPServer((req, res) => {
    void handle(source, viewers, req, res).catch((err: unknown) => {
      logError(err);
      if (!res.headersSent) sendJSON(res, 500, { error: "internal error" });
      else res.destroy();
    });
  });
}

async function handle(
  source: CameraSource,
  viewers: Map<string, number>,
  req: IncomingMessage,
  res: ServerResponse,
): Promise<void> {
  const path = new URL(req.url ?? "/", "http://127.0.0.1").pathname;

  if (path === "/healthz") {
    if (req.method !== "GET") return sendJSON(res, 405, { error: "method not allowed" });
    return sendJSON(res, 200, {
      loggedIn: source.loggedIn(),
      streaming: [...viewers.keys()].sort(),
    });
  }

  if (path === "/cameras") {
    if (req.method !== "GET") return sendJSON(res, 405, { error: "method not allowed" });
    return sendJSON(res, 200, await source.list());
  }

  const stream = /^\/stream\/([^/]+)$/.exec(path);
  if (stream) {
    if (req.method !== "GET") return sendJSON(res, 405, { error: "method not allowed" });
    const sn = stream[1] ?? "";
    if (!serialPattern.test(sn)) return sendJSON(res, 400, { error: "invalid serial" });
    return pipeStream(source, viewers, sn, res);
  }

  const snapshot = /^\/snapshot\/([^/]+)$/.exec(path);
  if (snapshot) {
    if (req.method !== "GET") return sendJSON(res, 405, { error: "method not allowed" });
    const sn = snapshot[1] ?? "";
    if (!serialPattern.test(sn)) return sendJSON(res, 400, { error: "invalid serial" });
    const jpeg = await source.snapshot(sn);
    if (!jpeg || jpeg.length === 0) return sendJSON(res, 404, { error: "not found" });
    res.writeHead(200, {
      "content-type": "image/jpeg",
      "content-length": jpeg.length,
      "cache-control": "no-store",
    });
    res.end(jpeg);
    return;
  }

  sendJSON(res, 404, { error: "not found" });
}

async function pipeStream(
  source: CameraSource,
  viewers: Map<string, number>,
  sn: string,
  res: ServerResponse,
): Promise<void> {
  const readable = await source.openStream(sn);
  if (!readable) {
    sendJSON(res, 404, { error: "not found" });
    return;
  }

  viewers.set(sn, (viewers.get(sn) ?? 0) + 1);
  let released = false;
  const release = () => {
    if (released) return;
    released = true;
    const left = (viewers.get(sn) ?? 1) - 1;
    if (left <= 0) viewers.delete(sn);
    else viewers.set(sn, left);
    readable.destroy();
  };
  // A client hang-up closes the response. That is the viewer leaving, not a
  // failed request, so the error is consumed here.
  readable.on("error", (err: Error) => {
    logError(err);
    release();
    if (!res.writableEnded) res.destroy();
  });
  res.on("error", release);
  res.on("close", release);
  res.writeHead(200, {
    "content-type": "video/octet-stream",
    "cache-control": "no-store",
  });
  readable.pipe(res);
}

function sendJSON(res: ServerResponse, status: number, body: unknown): void {
  const payload = JSON.stringify(body);
  res.writeHead(status, {
    "content-type": "application/json; charset=utf-8",
    "cache-control": "no-store",
    "content-length": Buffer.byteLength(payload),
  });
  res.end(payload);
}

function logError(err: unknown): void {
  const message = err instanceof Error ? err.message : "request failed";
  console.error("eufy proxy:", message);
}
