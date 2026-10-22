import { mkdirSync, writeFileSync } from "node:fs";
import path from "node:path";
import type { Readable, Writable } from "node:stream";

import { EufyMega, FileSessionStore, StoredSnapshotUnavailableError } from "@mega-yfue/eufy-sdk";

import { annexB } from "./annex.ts";
import type { Config } from "./config.ts";
import { askLine, finishLogin } from "./login.ts";
import type { CameraInfo, CameraSource } from "./server.ts";

// Log in, saving the session so the next start skips captcha and 2FA.
// A missing terminal exits before any prompt: a code belongs to this login.
export async function connect(
  cfg: Config,
  stdin: Readable = process.stdin,
  stdout: Writable = process.stdout,
): Promise<EufyMega> {
  mkdirSync(cfg.sessionDir, { recursive: true });
  const eufy = new EufyMega({
    email: cfg.email,
    password: cfg.password,
    countryCode: cfg.country,
    store: new FileSessionStore(path.join(cfg.sessionDir, "session.json")),
  });
  const captchaPath = path.join(cfg.sessionDir, "captcha.png");
  await finishLogin(
    eufy,
    (message) => askLine(stdin, stdout, message),
    (dataURL) => {
      const png = Buffer.from(dataURL.replace(/^data:image\/\w+;base64,/, ""), "base64");
      writeFileSync(captchaPath, png);
      return captchaPath;
    },
  );
  return eufy;
}

export class EufyCameras implements CameraSource {
  private readonly client: EufyMega;
  private sessionOk = true;

  constructor(client: EufyMega) {
    this.client = client;
    client.on("sessionExpired", () => {
      this.sessionOk = false;
      console.error("eufy proxy: session expired");
    });
  }

  loggedIn(): boolean {
    return this.sessionOk;
  }

  async list(): Promise<CameraInfo[]> {
    const records = await this.client.getDevices();
    const devices = await Promise.all(records.map((record) => this.client.getDevice(record.sn)));
    return devices.map((device) => ({
      sn: device.sn,
      name: device.name,
      camera: device.has("camera"),
    }));
  }

  async openStream(sn: string): Promise<Readable | undefined> {
    const device = await this.device(sn);
    const live = device?.camera?.()?.live;
    if (!live) return undefined;
    const stream = await live();
    return annexB({
      onVideo: (listener) => {
        stream.on("video", listener);
      },
      onBudget: (listener) => {
        stream.on("budget", listener);
      },
      onError: (listener) => {
        stream.on("error", listener);
      },
      onStop: (listener) => {
        stream.on("stop", listener);
      },
      stop: () => {
        stream.stop();
      },
      pause: () => {
        stream.pause();
      },
      resume: () => {
        stream.resume();
      },
    });
  }

  async snapshot(sn: string): Promise<Buffer | undefined> {
    const device = await this.device(sn);
    const take = device?.camera?.()?.snapshotStored;
    if (!take) return undefined;
    try {
      const jpeg = await take();
      return jpeg.length > 0 ? jpeg : undefined;
    } catch (err) {
      if (err instanceof StoredSnapshotUnavailableError) return undefined;
      throw err;
    }
  }

  private async device(sn: string) {
    const records = await this.client.getDevices();
    if (!records.some((record) => record.sn === sn)) return undefined;
    return this.client.getDevice(sn);
  }
}
