import { existsSync } from "node:fs";
import { fileURLToPath } from "node:url";

import { loadConfig } from "./config.ts";
import { loadDotEnv } from "./env.ts";
import { connect, EufyCameras } from "./eufy.ts";
import { createServer } from "./server.ts";

const defaultSessionDir = fileURLToPath(new URL("../.session", import.meta.url));
const defaultEnvFile = fileURLToPath(new URL("../.env", import.meta.url));

async function main(): Promise<void> {
  loadDotEnv(defaultEnvFile);
  const cfg = readConfig();
  const eufy = await connect(cfg);
  const server = createServer(new EufyCameras(eufy));

  await new Promise<void>((resolve, reject) => {
    server.once("error", reject);
    server.listen(cfg.port, cfg.host, () => resolve());
  });
  console.log(`eufy proxy listening on http://${cfg.host}:${cfg.port}`);

  await new Promise<void>((resolve) => {
    process.once("SIGINT", resolve);
    process.once("SIGTERM", resolve);
  });
  await new Promise<void>((resolve, reject) => {
    server.close((err) => (err ? reject(err) : resolve()));
  });
  await eufy.disconnect();
}

function readConfig() {
  try {
    return loadConfig(process.env, defaultSessionDir);
  } catch (err) {
    if (!process.env["EUFY_ENV_FILE"] && !existsSync(defaultEnvFile) && err instanceof Error) {
      throw new Error(`${err.message}. Copy .env.example to .env and edit it.`);
    }
    throw err;
  }
}

main().catch((err: unknown) => {
  const message = err instanceof Error ? err.message : "eufy proxy failed";
  console.error("eufy proxy:", message);
  process.exitCode = 1;
});
