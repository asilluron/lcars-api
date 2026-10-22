import assert from "node:assert/strict";
import { once } from "node:events";
import type { AddressInfo } from "node:net";
import { Readable } from "node:stream";
import test from "node:test";

import { createServer, type CameraInfo, type CameraSource } from "./server.ts";

test("serves health, cameras, a snapshot, and a stream", async (t) => {
  const source = new Fake();
  const server = createServer(source);
  t.after(() => close(server));
  const base = await listen(server);

  const health = await fetch(`${base}/healthz`);
  assert.equal(health.status, 200);
  assert.deepEqual(await health.json(), { loggedIn: true, streaming: [] });

  const cameras = await fetch(`${base}/cameras`);
  assert.equal(cameras.status, 200);
  assert.deepEqual(await cameras.json(), [
    { sn: "CAM1", name: "Front door", camera: true },
  ]);

  const missing = await fetch(`${base}/snapshot/OTHER1`);
  assert.equal(missing.status, 404);
  const snap = await fetch(`${base}/snapshot/CAM1`);
  assert.equal(snap.status, 200);
  assert.equal(snap.headers.get("content-type"), "image/jpeg");
  assert.deepEqual(Buffer.from(await snap.arrayBuffer()), Buffer.from([0xff, 0xd8, 0xff, 0xd9]));

  const stream = await fetch(`${base}/stream/CAM1`);
  assert.equal(stream.status, 200);
  assert.equal(stream.headers.get("content-type"), "video/octet-stream");
  const live = await fetch(`${base}/healthz`);
  assert.deepEqual(await live.json(), { loggedIn: true, streaming: ["CAM1"] });

  source.end();
  assert.equal(Buffer.from(await stream.arrayBuffer()).toString(), "annex");
  const done = await fetch(`${base}/healthz`);
  assert.deepEqual(await done.json(), { loggedIn: true, streaming: [] });
});

test("rejects a bad serial and reports an unknown camera", async (t) => {
  const source = new Fake();
  const server = createServer(source);
  t.after(() => close(server));
  const base = await listen(server);

  const bad = await fetch(`${base}/stream/CAM-1`);
  assert.equal(bad.status, 400);
  assert.equal(source.opened, 0);

  const unknown = await fetch(`${base}/stream/MISSING`);
  assert.equal(unknown.status, 404);
  assert.equal(source.opened, 1);

  const post = await fetch(`${base}/cameras`, { method: "POST" });
  assert.equal(post.status, 405);
  const absent = await fetch(`${base}/nope`);
  assert.equal(absent.status, 404);
});

test("destroying the client releases the stream", async (t) => {
  const source = new Fake();
  const server = createServer(source);
  t.after(() => close(server));
  const base = await listen(server);

  const ac = new AbortController();
  const res = await fetch(`${base}/stream/CAM1`, { signal: ac.signal });
  assert.equal(res.status, 200);
  const closed = once(source.readable, "close");
  ac.abort();
  await closed;
  assert.equal(source.destroyed, true);

  const health = await fetch(`${base}/healthz`);
  assert.deepEqual(await health.json(), { loggedIn: true, streaming: [] });
});

class Fake implements CameraSource {
  opened = 0;
  destroyed = false;
  readable = new Readable({ read() {} });

  loggedIn(): boolean {
    return true;
  }

  async list(): Promise<CameraInfo[]> {
    return [{ sn: "CAM1", name: "Front door", camera: true }];
  }

  async openStream(sn: string): Promise<Readable | undefined> {
    this.opened += 1;
    if (sn !== "CAM1") return undefined;
    this.destroyed = false;
    this.readable = new Readable({ read() {} });
    this.readable.push(Buffer.from("annex"));
    this.readable.on("close", () => {
      this.destroyed = true;
    });
    return this.readable;
  }

  end(): void {
    this.readable.push(null);
  }

  async snapshot(sn: string): Promise<Buffer | undefined> {
    if (sn !== "CAM1") return undefined;
    return Buffer.from([0xff, 0xd8, 0xff, 0xd9]);
  }
}

function listen(server: ReturnType<typeof createServer>): Promise<string> {
  return new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", () => {
      const addr = server.address() as AddressInfo;
      resolve(`http://127.0.0.1:${addr.port}`);
    });
  });
}

function close(server: ReturnType<typeof createServer>): Promise<void> {
  return new Promise((resolve, reject) => {
    server.close((err) => (err ? reject(err) : resolve()));
  });
}
